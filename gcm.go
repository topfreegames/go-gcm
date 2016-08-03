// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package gcm provides send and receive GCM functionality.
package gcm

import (
	"errors"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/jpillora/backoff"
)

var (
	// DefaultMinBackoff is a default min delay for backoff.
	DefaultMinBackoff = 1 * time.Second
	// DefaultMaxBackoff is the default max delay for backoff.
	DefaultMaxBackoff = 10 * time.Second
	// DefaultPingInterval is a default interval between pings.
	DefaultPingInterval = 20 * time.Second
	// DefaultPingTimeout is a default time to wait for ping replies.
	DefaultPingTimeout = 30 * time.Second
)

// Data defines the custom payload of a GCM message.
type Data map[string]interface{}

// Notification defines the general of a GCM message.
type Notification struct {
	Title        string `json:"title,omitempty"`
	Body         string `json:"body,omitempty"`
	Icon         string `json:"icon,omitempty"`
	Sound        string `json:"sound,omitempty"`
	Badge        string `json:"badge,omitempty"`
	Tag          string `json:"tag,omitempty"`
	Color        string `json:"color,omitempty"`
	ClickAction  string `json:"click_action,omitempty"`
	BodyLocKey   string `json:"body_loc_key,omitempty"`
	BodyLocArgs  string `json:"body_loc_args,omitempty"`
	TitleLocArgs string `json:"title_loc_args,omitempty"`
	TitleLocKey  string `json:"title_loc_key,omitempty"`
}

// Client defines an interface for GCM client.
type Client interface {
	ID() string
	SendHTTP(m HTTPMessage) (*HTTPResponse, error)
	SendXMPP(m XMPPMessage) (string, int, error)
	Close() error
}

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

// Config is a container for gcm configuration data.
type Config struct {
	SenderID          string
	APIKey            string
	Sandbox           bool
	MonitorConnection bool
	Debug             bool
}

// NewClient creates a new GCM client for this senderID.
func NewClient(config *Config, h MessageHandler) (Client, error) {
	c := &gcmClient{
		senderID: config.SenderID,
		apiKey:   config.APIKey,
		mh:       h,
		debug:    config.Debug,
		sandbox:  config.Sandbox,
	}

	if c.debug {
		log.SetLevel(log.DebugLevel)
	}

	// Create HTTP client.
	c.httpClient = newHTTPGCMClient(config.APIKey, config.Debug)

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
	return c.httpClient.Send(m)
}

// SendXmpp sends a message using the XMPP GCM connection server.
func (c *gcmClient) SendXMPP(m XMPPMessage) (string, int, error) {
	return c.xmppClient.Send(m)
}

// Close will stop and close the corresponding client.
func (c *gcmClient) Close() error {
	c.Lock()
	defer c.Unlock()
	if c.xmppClient != nil {
		return c.xmppClient.Close(true)
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
		go newc.Close(true)

		firstRun = false
	}
	log.WithField("sender id", c.senderID).Debug("gcm xmpp connection monitor finished")
}

// pingPeriodically sends periodic pings. If pong is received, the timer is reset.
func pingPeriodically(xm xmppClient, timeout, interval time.Duration) error {
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
	// Bubble up.
	return c.mh(cm)
}

// Creates a new xmpp client, connects to the server and starts listening.
func connectXMPP(isSandbox bool, senderID string, apiKey string, h MessageHandler, cerr chan error, debug bool) (*xmppGCMClient, error) {
	newc, err := newXMPPGCMClient(isSandbox, senderID, apiKey, debug)
	if err != nil {
		return nil, err
	}
	l := log.WithField("client id", newc.ID())

	// Start listening on this connection.
	go func() {
		if err := newc.Listen(h); err != nil {
			l.WithField("error", err).Error("gcm xmpp listen")
			cerr <- err
		}
		l.Debug("gcm xmpp listen finished")
	}()

	return newc, nil
}

// Implementation of backoff provider using exponential backoff.
type exponentialBackoff struct {
	b            backoff.Backoff
	currentDelay time.Duration
}

// Factory method for exponential backoff, uses default values for Min and Max and
// adds Jitter.
func newExponentialBackoff() *exponentialBackoff {
	b := &backoff.Backoff{
		Min:    DefaultMinBackoff,
		Max:    DefaultMaxBackoff,
		Jitter: true,
	}
	return &exponentialBackoff{b: *b, currentDelay: b.Duration()}
}

// Returns true if not over the retries limit
func (eb exponentialBackoff) sendAnother() bool {
	return eb.currentDelay <= eb.b.Max
}

// Set the minumim delay for backoff
func (eb *exponentialBackoff) setMin(min time.Duration) {
	eb.b.Min = min
	if (eb.currentDelay) < min {
		eb.currentDelay = min
	}
}

// Wait for the current value of backoff
func (eb exponentialBackoff) wait() {
	time.Sleep(eb.currentDelay)
	eb.currentDelay = eb.b.Duration()
}
