// Package gcm provides send and receive GCM functionality.
package gcm

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/mattn/go-xmpp"
	"github.com/pborman/uuid"
)

const (
	// XMPP message types.
	CCSAck     = "ack"
	CCSNack    = "nack"
	CCSControl = "control"
	CCSReceipt = "receipt"
	// GCM service constants.
	ccsHostProd = "gcm.googleapis.com"
	ccsPortProd = "5235"
	ccsHostDev  = "gcm-preprod.googleapis.com"
	ccsPortDev  = "5236"
	// For CCS the min for exponential backoff has to be 1 sec
	ccsMinBackoff = 1 * time.Second
)

var (
	retryableErrors = map[string]bool{
		"Unavailable":           true,
		"SERVICE_UNAVAILABLE":   true,
		"InternalServerError":   true,
		"INTERNAL_SERVER_ERROR": true,
		// TODO(silvano): should we backoff with the same strategy on
		// DeviceMessageRateExceeded and TopicsMessageRateExceeded.
	}
)

// xmppGcmClient is a client for the GCM XMPP Connection Server (CCS).
type xmppGCMClient struct {
	sync.RWMutex
	xmppClient xmpp.Client
	xmppHost   string
	messages   struct {
		sync.RWMutex
		m map[string]*messageLogEntry
	}
	pongs      chan struct{}
	senderID   string
	closed     bool
	destructor sync.Once
	debug      bool
}

// An entry in the messages log, used to keep track of messages pending ack and
// retries for failed messages.
type messageLogEntry struct {
	body    *XMPPMessage
	backoff backoffProvider
}

// newXMPPClient creates a new client for GCM XMPP Server (CCS).
func newXMPPClient(isSandbox bool, senderID string, apiKey string, debug bool) (xmppClient, error) {
	var xmppHost, xmppAddress string
	if isSandbox {
		xmppHost = ccsHostDev
		xmppAddress = net.JoinHostPort(ccsHostDev, ccsPortDev)
	} else {
		xmppHost = ccsHostProd
		xmppAddress = net.JoinHostPort(ccsHostProd, ccsPortProd)
	}

	nc, err := xmpp.NewClient(xmppAddress, xmppUser(xmppHost, senderID), apiKey, debug)
	if err != nil {
		return nil, fmt.Errorf("error connecting gcm xmpp client: %v", err)
	}

	xc := &xmppGCMClient{
		xmppClient: *nc,
		messages: struct {
			sync.RWMutex
			m map[string]*messageLogEntry
		}{
			m: make(map[string]*messageLogEntry),
		},
		xmppHost: xmppHost,
		senderID: senderID,
		pongs:    make(chan struct{}, 100),
		debug:    debug,
	}

	return xc, nil
}

// isClosed reports if the client is already closed.
func (c *xmppGCMClient) isClosed() bool {
	c.RLock()
	closed := c.closed
	c.RUnlock()
	return closed
}

// ping sends a c2s ping message and blocks until s2c pong is received.
//
// Returns error if timeout time passes before pong.
func (c *xmppGCMClient) ping(timeout time.Duration) error {
	l := log.WithField("id", c.ID())
	l.Debug("------- ping")
	if err := c.xmppClient.PingC2S("", c.xmppHost); err != nil {
		return err
	}
	select {
	case <-c.pongs:
		// Ping successful.
		l.Debug("-- pong")
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("gcm xmpp pong timed out after %s", timeout.String())
	}
}

// close sets the closing flag and (if graceful) waits until either all messages are
// processed or a timeout is reached.
func (c *xmppGCMClient) close(graceful bool) error {
	var err error
	l := log.WithFields(log.Fields{"client id": c.ID(), "graceful": graceful})
	c.destructor.Do(func() {
		l.Debug("xmppGcmClient close started")
		defer l.Debug("xmppGcmClient close finished")
		if c.isClosed() {
			return
		}
		c.Lock()
		c.closed = true
		c.Unlock()

		if graceful {
			// Wait until all is done, or timed out.
			select {
			case <-c.waitAllDone():
			case <-time.After(DefaultPingTimeout):
				l.Debug("gcm xmpp taking a while to close, so giving up")
			}
		}
		err = c.xmppClient.Close()
	})

	return err
}

// ID returns the identifier of this XMPP client.
func (c *xmppGCMClient) ID() string {
	return c.xmppClient.JID()
}

// xmppGcmClient implementation of listening for messages from CCS; the messages can be
// acks or nacks for messages sent through XMPP, control messages, upstream messages.
func (c *xmppGCMClient) listen(h MessageHandler) error {
	for {
		stanza, err := c.xmppClient.Recv()
		if err != nil {
			if c.isClosed() {
				// Client is closed, return without error.
				break
			}
			// This is likely fatal, so return.
			return fmt.Errorf("error on Recv: %v", err)
		}
		switch v := stanza.(type) {
		case xmpp.Chat:
		case xmpp.IQ:
			// See if it's a pong and do not add more than one.
			if v.Type == "result" && v.ID == "c2s1" && len(c.pongs) == 0 {
				c.pongs <- struct{}{}
			}
			continue
		case xmpp.Presence:
			continue
		}

		v := stanza.(xmpp.Chat)
		switch v.Type {
		case "":
			cm := &CCSMessage{}
			err = json.Unmarshal([]byte(v.Other[0]), cm)
			if err != nil {
				log.WithField("error", err).Error("unmarshaling ccs message")
				continue
			}
			switch cm.MessageType {
			case CCSAck:
				c.messages.Lock()
				// ack for a sent message, delete it from log.
				if _, ok := c.messages.m[cm.MessageID]; ok {
					go h(*cm)
					delete(c.messages.m, cm.MessageID)
				}
				c.messages.Unlock()
			case CCSNack:
				// nack for a sent message, retry if retryable error, bubble up otherwise.
				if retryableErrors[cm.Error] {
					c.retryMessage(*cm, h)
				} else {
					c.messages.Lock()
					if _, ok := c.messages.m[cm.MessageID]; ok {
						go h(*cm)
						delete(c.messages.m, cm.MessageID)
					}
					c.messages.Unlock()
				}
			case CCSControl:
				log.WithField("ccs message", cm).Warn("control message")
				go h(*cm)
			default:
				log.WithField("ccs message", cm).Warn("unknown ccs message type")
			}
		case "normal":
			cm := &CCSMessage{}
			err = json.Unmarshal([]byte(v.Other[0]), cm)
			if err != nil {
				log.WithField("error", err).Error("unmarshaling ccs message")
				continue
			}
			switch cm.MessageType {
			case CCSReceipt:
				if c.debug {
					log.WithField("ccs message", cm).Debug("message receipt")
				}
				// Receipt message: send ack and pass to listener.
				ack := XMPPMessage{To: cm.From, MessageID: cm.MessageID, MessageType: CCSAck}
				c.send(ack)
				go h(*cm)
			default:
				log.WithField("ccs message", cm).Warn("uknown ccs message type")
				// Upstream message: send ack and pass to listener.
				ack := XMPPMessage{To: cm.From, MessageID: cm.MessageID, MessageType: CCSAck}
				c.send(ack)
				go h(*cm)
			}
		case "error":
			log.WithField("stanza", v).Warn("error stanza")
			cm := &CCSMessage{}
			err = json.Unmarshal([]byte(v.Other[0]), cm)
			if err != nil {
				log.WithField("error", err).Error("unmarshaling ccs message")
				continue
			}
			go h(*cm)
		default:
			log.WithField("stanza", v).Warn("unknown stanza")
		}
	}
	return nil
}

// TODO(silvano): add flow control (max 100 pending messages at one time)
// xmppGcmClient implementation to send a message through Gcm Xmpp server (ccs).
func (c *xmppGCMClient) send(m XMPPMessage) (string, int, error) {
	if m.MessageID == "" {
		m.MessageID = uuid.New()
	}

	stanza := `<message id=""><gcm xmlns="google:mobile:data">%v</gcm></message>`
	body, err := json.Marshal(m)
	if err != nil {
		return m.MessageID, 0, fmt.Errorf("could not unmarshal body of xmpp message: %v", err)
	}
	bs := string(body)

	payload := fmt.Sprintf(stanza, bs)

	if c.debug {
		log.WithField("xmpp payload", payload).Debug("sending gcm xmpp")
	}

	// For Acks, just send the message.
	if m.MessageType == CCSAck {
		// Serialize wire access for thread safety.
		c.Lock()
		bytes, err := c.xmppClient.SendOrg(payload)
		c.Unlock()
		return m.MessageID, bytes, err
	}

	// For payload messages, keep them until Ack/Nack is received.
	c.messages.Lock()
	if _, ok := c.messages.m[m.MessageID]; !ok {
		b := newExponentialBackoff()
		if b.getMin() < ccsMinBackoff {
			b.setMin(ccsMinBackoff)
		}
		c.messages.m[m.MessageID] = &messageLogEntry{body: &m, backoff: b}
	}
	c.messages.Unlock()

	// Serialize wire access for thread safety.
	c.Lock()
	bytes, err := c.xmppClient.SendOrg(payload)
	c.Unlock()

	// If send is not successful, remove the message from the store immediately.
	if err != nil {
		c.messages.Lock()
		delete(c.messages.m, m.MessageID)
		c.messages.Unlock()
	}
	return m.MessageID, bytes, err
}

// waitAllDone waits until the message store is empty.
func (c *xmppGCMClient) waitAllDone() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for {
			// Exit if closed.
			if c.isClosed() {
				break
			}
			// Exit if done.
			c.messages.RLock()
			nm := len(c.messages.m)
			c.messages.RUnlock()
			if nm == 0 {
				break
			}
			// Otherwise sleep and retry.
			time.Sleep(100 * time.Millisecond)
		}
		close(ch)
	}()
	return ch
}

// Retry sending an xmpp message with exponential backoff; if over limit, bubble up the failed message.
func (c *xmppGCMClient) retryMessage(cm CCSMessage, h MessageHandler) {
	c.messages.RLock()
	defer c.messages.RUnlock()
	if me, ok := c.messages.m[cm.MessageID]; ok {
		if me.backoff.sendAnother() {
			go func(m *messageLogEntry) {
				m.backoff.wait()
				c.send(*m.body)
			}(me)
		} else {
			log.WithField("xmpp payload", me).Warn("exponential backoff failed over limit")
			go h(cm)
		}
	}
}

// xmppUser generates an xmpp username from a sender ID.
func xmppUser(xmppHost, senderID string) string {
	return senderID + "@" + xmppHost
}
