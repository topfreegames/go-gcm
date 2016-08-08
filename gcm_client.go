package gcm

import (
	"errors"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
)

// gcmClient is a container for http and xmpp GCM clients.
type gcmClient struct {
	sync.Mutex
	senderID   string
	apiKey     string
	mh         MessageHandler
	xmppClient xmppClient
	httpClient httpClient
	cerr       chan error
	sandbox    bool
	debug      bool
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

	c := &gcmClient{
		senderID: config.SenderID,
		apiKey:   config.APIKey,
		mh:       h,
		debug:    config.Debug,
		sandbox:  config.Sandbox,
	}

	if config.Debug {
		log.SetLevel(log.DebugLevel)
	}

	// Create HTTP client.
	c.httpClient = newHTTPClient(config.APIKey, config.Debug)

	// Create and monitor XMPP client.
	cerr := make(chan error)
	go c.createAndMonitorXMPP(config.MonitorConnection, cerr)

	// Wait a bit to see if the newly created client is ok.
	select {
	case err := <-cerr:
		return nil, err
	case <-time.After(time.Second):
		// Looks good.
	}

	return c, nil
}

// ID returns XMPP JID of this client.
func (c *gcmClient) ID() string {
	return c.xmppClient.ID()
}

// Send a message using the HTTP GCM connection server.
func (c *gcmClient) SendHTTP(m HTTPMessage) (*HTTPResponse, error) {
	return c.httpClient.send(m)
}

// SendXmpp sends a message using the XMPP GCM connection server.
func (c *gcmClient) SendXMPP(m XMPPMessage) (string, int, error) {
	return c.xmppClient.send(m)
}

// Close will stop and close the corresponding client.
func (c *gcmClient) Close() error {
	c.Lock()
	defer c.Unlock()
	if c.xmppClient != nil {
		return c.xmppClient.close(true)
	}
	return nil
}

// createAndMonitorXMPP creates a new GCM XMPP client, replaces the active client,
// closes the old client and starts monitoring the new connection.
func (c *gcmClient) createAndMonitorXMPP(activeMonitor bool, xcerr chan error) {
	firstRun := true
	for {
		// On the first run, send the error upstream.
		var cerr chan error
		if firstRun {
			cerr = xcerr
		} else {
			cerr = make(chan error)
		}

		// Create XMPP client.
		newc, err := connectXMPP(c.sandbox, c.senderID, c.apiKey, c.onCCSMessage, cerr, c.debug)
		if err != nil {
			if firstRun {
				cerr <- err
				break
			}
			log.WithFields(log.Fields{"sender id": c.senderID, "error": err}).Error("connect gcm xmpp")
			// Wait and try again.
			time.Sleep(DefaultPingTimeout)
			continue
		}

		// Replace the active client.
		c.Lock()
		oldc := c.xmppClient
		c.xmppClient = newc
		c.cerr = cerr
		c.Unlock()

		if firstRun {
			log.WithField("client id", newc.ID()).Debug("created gcm xmpp client")
		} else {
			log.WithFields(log.Fields{"new client id": newc.ID(), "old client id": oldc.ID()}).
				Debug("replaced gcm xmpp client")
		}

		// If active monitoring is enabled, start pinging routine.
		if activeMonitor {
			go func() {
				cerr <- pingPeriodically(newc, DefaultPingTimeout, DefaultPingInterval)
			}()
		}

		// Wait for an error to occur (from listen or ping).
		err = <-cerr
		if err == nil {
			// Active close.
			break
		}
		log.WithFields(log.Fields{"client id": newc.ID(), "error": err}).Error("gcm xmpp connection")

		// Close the old client.
		go newc.close(true)

		firstRun = false
	}
	log.WithField("sender id", c.senderID).Debug("gcm xmpp connection monitor finished")
}

// Creates a new xmpp client, connects to the server and starts listening.
func connectXMPP(isSandbox bool, senderID string, apiKey string, h MessageHandler, cerr chan error, debug bool) (xmppClient, error) {
	newc, err := newXMPPClient(isSandbox, senderID, apiKey, debug)
	if err != nil {
		return nil, err
	}
	l := log.WithField("client id", newc.ID())

	// Start listening on this connection.
	go func() {
		if err := newc.listen(h); err != nil {
			l.WithField("error", err).Error("gcm xmpp listen")
			cerr <- err
		}
		l.Debug("gcm xmpp listen finished")
	}()

	return newc, nil
}

// pingPeriodically sends periodic pings. If pong is received, the timer is reset.
func pingPeriodically(xm xmppClient, timeout, interval time.Duration) error {
	t := time.NewTimer(interval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			if xm.isClosed() {
				return nil
			}
			if err := xm.ping(timeout); err != nil {
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
	// Bubble up.
	return c.mh(cm)
}
