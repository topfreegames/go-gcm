package gcm

import (
	"errors"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
)

// httpC is an interface to stub the internal HTTP client.
type httpC interface {
	Send(m HTTPMessage) (*HTTPResponse, error)
}

// xmppC is an interface to stub the internal XMPP client.
type xmppC interface {
	Listen(h MessageHandler) error
	Send(m XMPPMessage) (string, int, error)
	Ping(timeout time.Duration) error
	Close(graceful bool) error
	IsClosed() bool
	ID() string
}

// gcmClient is a container for http and xmpp GCM clients.
type gcmClient struct {
	sync.Mutex
	mh      MessageHandler
	cerr    chan error
	sandbox bool
	debug   bool
	// Clients.
	xmppClient xmppC
	httpClient httpC
	// GCM auth.
	senderID string
	apiKey   string
	// XMPP config.
	pingInterval time.Duration
	pingTimeout  time.Duration
}

// NewClient creates a new GCM client for this senderID.
func NewClient(config *Config, h MessageHandler) (Client, error) {
	switch {
	case config == nil:
		return nil, errors.New("config is nil")
	case h == nil:
		return nil, errors.New("message handler is nil")
	case config.SenderID == "":
		return nil, errors.New("empty sender id")
	case config.APIKey == "":
		return nil, errors.New("empty api key")
	}

	// Create GCM XMPP client.
	xmppc, err := newXMPPClient(config.Sandbox, config.SenderID, config.APIKey, config.Debug)
	if err != nil {
		return nil, err
	}

	// Create GCM HTTP client.
	httpc := newHTTPClient(config.APIKey, config.Debug)

	// Construct GCM client.
	return newGCMClient(xmppc, httpc, config, h)
}

// ID returns client identification (XMPP JID).
func (c *gcmClient) ID() string {
	return c.xmppClient.ID()
}

// Send a message using the HTTP GCM connection server (blocking).
func (c *gcmClient) SendHTTP(m HTTPMessage) (*HTTPResponse, error) {
	return c.httpClient.Send(m)
}

// SendXmpp sends a message using the XMPP GCM connection server (blocking).
func (c *gcmClient) SendXMPP(m XMPPMessage) (string, int, error) {
	return c.xmppClient.Send(m)
}

// Close will stop and close the corresponding client, releasing all resources (blocking).
func (c *gcmClient) Close() error {
	c.Lock()
	defer c.Unlock()
	if c.xmppClient != nil {
		return c.xmppClient.Close(true)
	}
	return nil
}

// newGCMClient creates an instance of gcmClient.
func newGCMClient(xmppc xmppC, httpc httpC, config *Config, h MessageHandler) (*gcmClient, error) {
	c := &gcmClient{
		httpClient:   httpc,
		xmppClient:   xmppc,
		senderID:     config.SenderID,
		apiKey:       config.APIKey,
		mh:           h,
		debug:        config.Debug,
		sandbox:      config.Sandbox,
		pingInterval: time.Duration(config.PingInterval) * time.Second,
		pingTimeout:  time.Duration(config.PingTimeout) * time.Second,
	}
	if c.pingInterval <= 0 {
		c.pingInterval = DefaultPingInterval
	}
	if c.pingTimeout <= 0 {
		c.pingTimeout = DefaultPingTimeout
	}

	// Create and monitor XMPP client.
	cerr := make(chan error)
	go c.monitorXMPP(config.MonitorConnection, cerr)

	// Wait a bit to see if the newly created client is ok.
	// TODO: find a better way (success notification, etc).
	select {
	case err := <-cerr:
		return nil, err
	case <-time.After(time.Second): // TODO: configurable
		// Looks good.
	}

	return c, nil

}

// monitorXMPP creates a new GCM XMPP client (if not provided), replaces the active client,
// closes the old client and starts monitoring the new connection.
func (c *gcmClient) monitorXMPP(activeMonitor bool, xcerr chan error) {
	firstRun := true
	for {
		// On the first run, send the error upstream.
		// TODO: channel recreating is ugly.
		var cerr chan error
		if firstRun {
			cerr = xcerr
		} else {
			cerr = make(chan error)
		}

		// Create XMPP client.
		xmppc, err := connectXMPP(c.xmppClient, c.sandbox, c.senderID, c.apiKey,
			c.onCCSMessage, cerr, c.debug)
		if err != nil {
			if firstRun {
				cerr <- err
				break
			}
			log.WithFields(log.Fields{"sender id": c.senderID, "error": err}).
				Error("connect gcm xmpp")
			// Wait and try again.
			// TODO: remove infinite loop.
			time.Sleep(c.pingTimeout)
			continue
		}

		// Replace the active client.
		c.Lock()
		oldc := c.xmppClient
		c.xmppClient = xmppc
		c.cerr = cerr
		c.Unlock()

		if firstRun {
			log.WithField("client id", xmppc.ID()).Debug("created gcm xmpp client")
		} else {
			log.WithFields(log.Fields{"new client id": xmppc.ID(), "old client id": oldc.ID()}).
				Debug("replaced gcm xmpp client")
		}

		// If active monitoring is enabled, start pinging routine.
		if activeMonitor {
			go func() {
				cerr <- pingPeriodically(xmppc, c.pingTimeout, c.pingInterval)
			}()
		}

		// Wait for an error to occur (from listen or ping).
		err = <-cerr
		if err == nil {
			// Active close.
			break
		}
		log.WithFields(log.Fields{"client id": xmppc.ID(), "error": err}).
			Error("gcm xmpp connection")

		// Close the old client.
		go xmppc.Close(true)

		firstRun = false
	}
	log.WithField("sender id", c.senderID).
		Debug("gcm xmpp connection monitor finished")
}

// Creates a new xmpp client (if not provided), connects to the server and starts listening.
func connectXMPP(c xmppC, isSandbox bool, senderID string, apiKey string,
	h MessageHandler, cerr chan error, debug bool) (xmppC, error) {
	var xmppc xmppC
	if c != nil {
		// Use the provided client.
		xmppc = c
	} else {
		// Create new.
		var err error
		xmppc, err = newXMPPClient(isSandbox, senderID, apiKey, debug)
		if err != nil {
			return nil, err
		}
	}

	l := log.WithField("client id", xmppc.ID())

	// Start listening on this connection.
	go func() {
		if err := xmppc.Listen(h); err != nil {
			l.WithField("error", err).Error("gcm xmpp listen")
			cerr <- err
		}
		l.Debug("gcm xmpp listen finished")
	}()

	return xmppc, nil
}

// pingPeriodically sends periodic pings. If pong is received, the timer is reset.
func pingPeriodically(xm xmppC, timeout, interval time.Duration) error {
	t := time.NewTimer(interval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			if xm.IsClosed() {
				return nil
			}
			if err := xm.Ping(timeout); err != nil {
				return err
			}
			t.Reset(interval)
		}
	}
}

// CCS upstream message callback.
// Tries to handle what it can here, before bubbling up.
func (c *gcmClient) onCCSMessage(cm CCSMessage) error {
	switch cm.MessageType {
	case CCSControl:
		// Handle connection drainging request.
		if cm.ControlType == "CONNECTION_DRAINING" {
			// Server should close the current connection.
			c.cerr <- errors.New("connection draining")
		}
		// Don't bubble up control messages.
		return nil
	}
	// Bubble up everything else.
	return c.mh(cm)
}
