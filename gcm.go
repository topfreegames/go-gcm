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
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/jpillora/backoff"
)

var (
	// Default Min and Max delay for backoff.
	DefaultMinBackoff = 1 * time.Second
	DefaultMaxBackoff = 10 * time.Second
	// Ping interval and timeout.
	DefaultPingInterval = 20 * time.Second
	DefaultPingTimeout  = 30 * time.Second
)

// The data payload of a GCM message.
type Data map[string]interface{}

// The notification payload of a GCM message.
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
	SendHTTP(m HttpMessage) (*HttpResponse, error)
	SendXMPP(m XmppMessage) (string, int, error)
	Close() error
}

// gcmClient is a container for http and xmpp GCM clients.
type gcmClient struct {
	sync.Mutex
	senderID    string
	apiKey      string
	mh          MessageHandler
	xmppClient  xmppClient
	httpClient  httpClient
	sandbox     bool
	monitorConn bool
	debug       bool
}

// Config is a container for gcm configuration data.
type Config struct {
	SenderID          string
	APIKey            string
	Sandbox           bool
	MonitorConnection bool
	Debug             bool
}

func init() {
	log.SetLevel(log.DebugLevel)
}

// NewClient creates a new GCM client for this senderID.
func NewClient(config *Config, h MessageHandler) (Client, error) {
	c := &gcmClient{
		senderID:    config.SenderID,
		apiKey:      config.APIKey,
		mh:          h,
		debug:       config.Debug,
		sandbox:     config.Sandbox,
		monitorConn: config.MonitorConnection,
	}

	// Create HTTP client.
	c.httpClient = newHttpGcmClient(config.APIKey, config.Debug)

	// Create XMPP client.
	cerr := make(chan error)
	xm, err := connectXmpp(config.Sandbox, config.SenderID, config.APIKey, c.onCCSMessage, cerr, config.Debug)
	if err != nil {
		return nil, err
	}
	c.xmppClient = xm

	l := log.WithField("gcm xmpp client id", c.ID())

	if config.MonitorConnection {
		go func() {
			cerr <- pingPeriodically(c.xmppClient, DefaultPingTimeout, DefaultPingInterval)
		}()
	}

	go func() {
		for {
			err = <-cerr
			close(cerr)
			l.WithField("error", err).Warn("gcm xmpp connection error")

			// Create a new client.
			cerr = make(chan error)
			newc, err := connectXmpp(c.sandbox, c.senderID, c.apiKey, c.onCCSMessage, cerr, c.debug)
			if err != nil {
				return
			}
			// Replace the active client.
			log.WithField("gcm xmpp client id", c.xmppClient.ID()).Warn("replacing xmpp client")
			c.Lock()
			oldc := c.xmppClient
			c.xmppClient = newc
			c.Unlock()
			log.WithFields(log.Fields{"new client id": newc.ID(), "old client id": oldc.ID()}).
				Warn("replaced gcm xmpp client")

			// Close the old client.
			go oldc.Close(false)

			if config.MonitorConnection {
				go func() {
					cerr <- pingPeriodically(c.xmppClient, DefaultPingTimeout, DefaultPingInterval)
				}()
			}
		}
	}()

	log.WithFields(log.Fields{"gcm xmpp client id": c.ID(), "sender id": c.senderID}).
		Debug("gcm xmpp client created")
	return c, nil
}

// ID returns XMPP JID of this client.
func (c *gcmClient) ID() string {
	return c.xmppClient.ID()
}

// Send a message using the HTTP GCM connection server.
func (c *gcmClient) SendHTTP(m HttpMessage) (*HttpResponse, error) {
	return c.httpClient.Send(m)
}

// SendXmpp sends a message using the XMPP GCM connection server.
func (c *gcmClient) SendXMPP(m XmppMessage) (string, int, error) {
	return c.xmppClient.Send(m)
}

// Close will stop and close the corresponding client.
func (c *gcmClient) Close() error {
	return c.xmppClient.Close(true)
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
	return nil
}

// CCS upstream message callback.
// Tries to handle what it can here, before bubbling up.
func (c *gcmClient) onCCSMessage(cm CcsMessage) error {
	l := log.WithField("gcm xmpp client id", c.ID())
	switch cm.MessageType {
	case CCSControl:
		// Handle connection drainging request.
		if cm.ControlType == "CONNECTION_DRAINING" {
			l.Warn("connection draining requested")
			// Server will close the current connection, so create a new one.
			//c.cerr <- "drain"
		}
		// Don't bubble up control messages.
		return nil
	default:
	}
	// Bubble up.
	return c.mh(cm)
}

// Creates a new xmpp client, connects to the server and starts listening.
func connectXmpp(isSandbox bool, senderID string, apiKey string, h MessageHandler, cerr chan error, debug bool) (*xmppGcmClient, error) {
	newc, err := newXmppGcmClient(isSandbox, senderID, apiKey, debug)
	if err != nil {
		return nil, err
	}
	l := log.WithField("gcm xmpp client id", newc.ID())
	l.Debug("created new gcm xmpp client")

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
