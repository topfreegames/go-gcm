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

// XmppMessage is a container for GCM XMPP message.
type XmppMessage struct {
	To                       string        `json:"to,omitempty"`
	MessageId                string        `json:"message_id"`
	MessageType              string        `json:"message_type,omitempty"`
	CollapseKey              string        `json:"collapse_key,omitempty"`
	Priority                 string        `json:"priority,omitempty"`
	ContentAvailable         bool          `json:"content_available,omitempty"`
	DelayWhileIdle           bool          `json:"delay_while_idle,omitempty"`
	TimeToLive               *uint         `json:"time_to_live,omitempty"`
	DeliveryReceiptRequested bool          `json:"delivery_receipt_requested,omitempty"`
	DryRun                   bool          `json:"dry_run,omitempty"`
	Data                     Data          `json:"data,omitempty"`
	Notification             *Notification `json:"notification,omitempty"`
}

// CcsMessage is an XMPP message sent from CCS.
type CcsMessage struct {
	From             string `json:"from, omitempty"`
	MessageId        string `json:"message_id, omitempty"`
	MessageType      string `json:"message_type, omitempty"`
	RegistrationId   string `json:"registration_id,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
	Category         string `json:"category, omitempty"`
	Data             Data   `json:"data,omitempty"`
	ControlType      string `json:"control_type,omitempty"`
}

// MessageHandler is the type for a function that handles a CCS message.
// The CCS message can be an upstream message (device to server) or a
// message from CCS (e.g. a delivery receipt).
type MessageHandler func(cm CcsMessage) error

// xmppClient is an interface to stub the xmpp client in tests.
type xmppClient interface {
	Listen(h MessageHandler) error
	Send(m XmppMessage) (string, int, error)
	Ping(timeout time.Duration) error
	Close(graceful bool) error
	IsClosed() bool
	ID() string
}

// xmppGcmClient is a client for the GCM XMPP Connection Server (CCS).
type xmppGcmClient struct {
	sync.RWMutex
	xmppClient xmpp.Client
	xmppHost   string
	messages   struct {
		sync.RWMutex
		m map[string]*messageLogEntry
	}
	pongs      chan struct{}
	senderID   string
	isClosed   bool
	destructor sync.Once
	debug      bool
}

// An entry in the messages log, used to keep track of messages pending ack and
// retries for failed messages.
type messageLogEntry struct {
	body    *XmppMessage
	backoff *exponentialBackoff
}

// newXmppGcmClient creates a new client for GCM XMPP Server (CCS).
func newXmppGcmClient(isSandbox bool, senderID string, apiKey string, debug bool) (*xmppGcmClient, error) {
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

	xc := &xmppGcmClient{
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

// IsClosed reports if the client is already closed.
func (c *xmppGcmClient) IsClosed() bool {
	c.RLock()
	closed := c.isClosed
	c.RUnlock()
	return closed
}

// Ping sends a c2s ping message and blocks until s2c pong is received.
//
// Returns error if timeout time passes before pong.
func (c *xmppGcmClient) Ping(timeout time.Duration) error {
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

// waitAllDone waits until the message store is empty.
func (c *xmppGcmClient) waitAllDone() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for {
			// Exit if closed.
			if c.IsClosed() {
				break
			}
			// Exit if done.
			c.messages.RLock()
			nm := len(c.messages.m)
			c.messages.RUnlock()
			log.WithField("client id", c.ID()).Debugf("waitAllDone messages %d", nm)
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

// Close sets the closing flag and (if graceful) waits until either all messages are
// processed or a timeout is reached.
func (c *xmppGcmClient) Close(graceful bool) error {
	var err error
	l := log.WithFields(log.Fields{"client id": c.ID(), "graceful": graceful})
	c.destructor.Do(func() {
		l.Debug("xmppGcmClient close started")
		if c.IsClosed() {
			return
		}
		c.Lock()
		c.isClosed = true
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
		l.Debug("xmppGcmClient close finished")
	})

	return err
}

// ID returns the identifier of this XMPP client.
func (c *xmppGcmClient) ID() string {
	return c.xmppClient.JID()
}

// xmppGcmClient implementation of listening for messages from CCS; the messages can be
// acks or nacks for messages sent through XMPP, control messages, upstream messages.
func (c *xmppGcmClient) Listen(h MessageHandler) error {
	for {
		stanza, err := c.xmppClient.Recv()
		if err != nil {
			if c.IsClosed() {
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
			cm := &CcsMessage{}
			err = json.Unmarshal([]byte(v.Other[0]), cm)
			if err != nil {
				log.WithField("error", err).Error("unmarshaling ccs message")
				continue
			}
			switch cm.MessageType {
			case CCSAck:
				c.messages.Lock()
				// ack for a sent message, delete it from log.
				if _, ok := c.messages.m[cm.MessageId]; ok {
					go h(*cm)
					delete(c.messages.m, cm.MessageId)
				}
				c.messages.Unlock()
			case CCSNack:
				// nack for a sent message, retry if retryable error, bubble up otherwise.
				if retryableErrors[cm.Error] {
					c.retryMessage(*cm, h)
				} else {
					c.messages.Lock()
					if _, ok := c.messages.m[cm.MessageId]; ok {
						go h(*cm)
						delete(c.messages.m, cm.MessageId)
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
			cm := &CcsMessage{}
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
				ack := XmppMessage{To: cm.From, MessageId: cm.MessageId, MessageType: CCSAck}
				c.Send(ack)
				go h(*cm)
			default:
				log.WithField("ccs message", cm).Warn("uknown ccs message type")
				// Upstream message: send ack and pass to listener.
				ack := XmppMessage{To: cm.From, MessageId: cm.MessageId, MessageType: CCSAck}
				c.Send(ack)
				go h(*cm)
			}
		case "error":
			log.WithField("stanza", v).Warn("error stanza")
			cm := &CcsMessage{}
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
func (c *xmppGcmClient) Send(m XmppMessage) (string, int, error) {
	if m.MessageId == "" {
		m.MessageId = uuid.New()
	}

	stanza := `<message id=""><gcm xmlns="google:mobile:data">%v</gcm></message>`
	body, err := json.Marshal(m)
	if err != nil {
		return m.MessageId, 0, fmt.Errorf("could not unmarshal body of xmpp message: %v", err)
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
		return m.MessageId, bytes, err
	}

	// For payload messages, keep them until Ack/Nack is received.
	c.messages.Lock()
	if _, ok := c.messages.m[m.MessageId]; !ok {
		b := newExponentialBackoff()
		if b.b.Min < ccsMinBackoff {
			b.setMin(ccsMinBackoff)
		}
		c.messages.m[m.MessageId] = &messageLogEntry{body: &m, backoff: b}
	}
	c.messages.Unlock()

	// Serialize wire access for thread safety.
	c.Lock()
	bytes, err := c.xmppClient.SendOrg(payload)
	c.Unlock()

	// If send is not successful, remove the message from the store immediately.
	if err != nil {
		c.messages.Lock()
		delete(c.messages.m, m.MessageId)
		c.messages.Unlock()
	}
	return m.MessageId, bytes, err
}

// Retry sending an xmpp message with exponential backoff; if over limit, bubble up the failed message.
func (c *xmppGcmClient) retryMessage(cm CcsMessage, h MessageHandler) {
	c.messages.RLock()
	defer c.messages.RUnlock()
	if me, ok := c.messages.m[cm.MessageId]; ok {
		if me.backoff.sendAnother() {
			go func(m *messageLogEntry) {
				m.backoff.wait()
				c.Send(*m.body)
			}(me)
		} else {
			log.WithField("xmpp payload", me).Warn("exponential backoff failed over limit")
			go h(cm)
		}
	}
}

// xmppUser generates an xmpp username from a sender ID.
func xmppUser(xmppHost, senderId string) string {
	return senderId + "@" + xmppHost
}
