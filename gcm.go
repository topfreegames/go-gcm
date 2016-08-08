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
	"time"

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

// HTTPMessage defines a downstream GCM HTTP message.
type HTTPMessage struct {
	To                    string        `json:"to,omitempty"`
	RegistrationIDs       []string      `json:"registration_ids,omitempty"`
	CollapseKey           string        `json:"collapse_key,omitempty"`
	Priority              string        `json:"priority,omitempty"`
	ContentAvailable      bool          `json:"content_available,omitempty"`
	DelayWhileIdle        bool          `json:"delay_while_idle,omitempty"`
	TimeToLive            *uint         `json:"time_to_live,omitempty"`
	RestrictedPackageName string        `json:"restricted_package_name,omitempty"`
	DryRun                bool          `json:"dry_run,omitempty"`
	Data                  Data          `json:"data,omitempty"`
	Notification          *Notification `json:"notification,omitempty"`
}

// HTTPResponse is the GCM connection server response to an HTTP downstream message.
type HTTPResponse struct {
	MulticastID  int64        `json:"multicast_id,omitempty"`
	Success      uint         `json:"success,omitempty"`
	Failure      uint         `json:"failure,omitempty"`
	CanonicalIds uint         `json:"canonical_ids,omitempty"`
	Results      []httpResult `json:"results,omitempty"`
	MessageID    uint         `json:"message_id,omitempty"`
	Error        string       `json:"error,omitempty"`
}

// XMPPMessage defines a downstream GCM XMPP message.
type XMPPMessage struct {
	To                       string        `json:"to,omitempty"`
	MessageID                string        `json:"message_id"`
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

// Data defines the custom payload of a GCM message.
type Data map[string]interface{}

// Notification defines the notification payload of a GCM message.
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

// Config is a container for gcm configuration data.
type Config struct {
	SenderID          string
	APIKey            string
	Sandbox           bool
	MonitorConnection bool
	Debug             bool
}

// CCSMessage is an XMPP message sent from CCS.
// The CCS message can be an upstream message (device to server) or a
// message from CCS (e.g. a delivery receipt, a control, etc).
type CCSMessage struct {
	From             string `json:"from, omitempty"`
	MessageID        string `json:"message_id, omitempty"`
	MessageType      string `json:"message_type, omitempty"`
	RegistrationID   string `json:"registration_id,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
	Category         string `json:"category, omitempty"`
	Data             Data   `json:"data,omitempty"`
	ControlType      string `json:"control_type,omitempty"`
}

// MessageHandler is the type for a function that handles a CCS message.
type MessageHandler func(cm CCSMessage) error

// Client defines an interface for GCM client.
type Client interface {
	ID() string
	SendHTTP(m HTTPMessage) (*HTTPResponse, error)
	SendXMPP(m XMPPMessage) (string, int, error)
	Close() error
}

// httpClient is an interface to stub the internal http client in tests.
type httpClient interface {
	send(m HTTPMessage) (*HTTPResponse, error)
	getRetryAfter() string
}

// xmppClient is an interface to stub the internal xmpp client in tests.
type xmppClient interface {
	listen(h MessageHandler) error
	send(m XMPPMessage) (string, int, error)
	ping(timeout time.Duration) error
	close(graceful bool) error
	isClosed() bool
	ID() string
}

// backoffProvider defines an interface for backoff.
type backoffProvider interface {
	sendAnother() bool
	getMin() time.Duration
	setMin(min time.Duration)
	wait()
}

// Implementation of backoff provider using exponential backoff.
type exponentialBackoff struct {
	b            backoff.Backoff
	currentDelay time.Duration
}

// Factory method for exponential backoff, uses default values for Min and Max and
// adds Jitter.
func newExponentialBackoff() backoffProvider {
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

func (eb *exponentialBackoff) getMin() time.Duration {
	return eb.b.Min
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
