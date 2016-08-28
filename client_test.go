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

package gcm

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
)

func TestClient(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GCM Client")
}

var _ = Describe("GCM Client", func() {
	Describe("initializing", func() {
		DescribeTable("wrong initialization parameters",
			func(config *Config, h MessageHandler, errStr string) {
				c, err := NewClient(config, h)
				Expect(c).To(BeNil())
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(errStr))
			},
			Entry("it should fail on nil config", nil, nil, "config is nil"),
			Entry("it should fail on nil message handler",
				&Config{}, nil, "message handler is nil"),
			Entry("it should fail on empty sender id",
				&Config{}, func(cm CCSMessage) error { return nil }, "empty sender id"),
			Entry("it should fail on empty api key",
				&Config{SenderID: "123"}, func(cm CCSMessage) error { return nil }, "empty api key"),
		)

		Context("good config", func() {
			var (
				xm *xmppCMock
				hm *httpCMock
			)

			BeforeEach(func() {
				xm = new(xmppCMock)
				hm = new(httpCMock)
				xm.On("ID").Return("id1")
			})

			AfterEach(func() {
				gt := GinkgoT()
				xm.AssertExpectations(gt)
				hm.AssertExpectations(gt)
			})

			It("should fail on xmpp connection error", func() {
				xm.On("Listen", mock.AnythingOfType("gcm.MessageHandler")).
					Return(errors.New("Connect"))
				c, err := newGCMClient(xm, hm, &Config{}, nil)
				Expect(err).To(HaveOccurred())
				Expect(c).To(BeNil())
				Expect(err).To(MatchError("Connect"))
			})

			It("should succeed", func() {
				xm.On("Listen", mock.AnythingOfType("gcm.MessageHandler")).
					Return(nil)
				c, err := newGCMClient(xm, hm, &Config{}, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(c).To(BeAssignableToTypeOf(&gcmClient{}))
				Expect(c.pingInterval).To(Equal(DefaultPingInterval))
				Expect(c.pingTimeout).To(Equal(DefaultPingTimeout))
				c.Lock()
				Expect(c.xmppClient).To(Equal(xm))
				c.Unlock()
				Expect(c.cerr).NotTo(BeClosed())
			})
		})
	})

	Describe("listening", func() {
		var (
			xm *xmppCMock
			c  *gcmClient
		)

		BeforeEach(func() {
			xm = new(xmppCMock)
			c = &gcmClient{
				xmppClient: xm,
				senderID:   "sender id",
				apiKey:     "api key",
				mh:         nil,
			}
		})

		AfterEach(func() {
			xm.AssertExpectations(GinkgoT())
		})

		It("should fail on listen error", func() {
			xm.On("ID").Return("id1")
			xm.On("Listen", mock.AnythingOfType("gcm.MessageHandler")).
				Return(errors.New("Listen"))
			cerr := make(chan error)
			go c.monitorXMPP(false, cerr)
			err := <-cerr
			Expect(err).To(MatchError("Listen"))
		})
	})

	Describe("sending", func() {
		var (
			xm XMPPMessage
			hm HTTPMessage
			h  *httpCMock
			x  *xmppCMock
			c  Client
		)

		BeforeEach(func() {
			xm = XMPPMessage{To: "me", MessageID: "id1"}
			hm = HTTPMessage{To: "me"}
			h = new(httpCMock)
			x = new(xmppCMock)
			c = &gcmClient{httpClient: h, xmppClient: x}
		})

		AfterEach(func() {
			gt := GinkgoT()
			h.AssertExpectations(gt)
			x.AssertExpectations(gt)
		})

		It("should send http message", func() {
			r := &HTTPResponse{MulticastID: 100}
			h.On("Send", hm).Return(r, nil)
			resp, err := c.SendHTTP(hm)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).To(Equal(r))
		})

		It("should fail sending http message", func() {
			h.On("Send", hm).Return(nil, errors.New("send error"))
			resp, err := c.SendHTTP(hm)
			Expect(err).To(HaveOccurred())
			Expect(resp).To(BeNil())
			Expect(err).To(MatchError("send error"))
		})

		It("should send xmpp message", func() {
			x.On("Send", xm).Return("id1", 100, nil)
			id, bytes, err := c.SendXMPP(xm)
			Expect(err).NotTo(HaveOccurred())
			Expect(id).To(Equal("id1"))
			Expect(bytes).To(Equal(100))
		})

		It("should fail on send error", func() {
			x.On("Send", xm).Return("", 0, errors.New("send error"))
			id, bytes, err := c.SendXMPP(xm)
			Expect(err).To(HaveOccurred())
			Expect(id).To(BeEmpty())
			Expect(bytes).To(Equal(0))
			Expect(err).To(MatchError("send error"))
		})
	})

	Describe("closing", func() {
		var (
			x *xmppCMock
			c *gcmClient
		)

		BeforeEach(func() {
			x = new(xmppCMock)
			c = &gcmClient{xmppClient: x}
		})

		It("should close successfully", func() {
			x.On("Close", true).Return(nil)
			x.On("IsClosed").Return(true)
			err := c.Close()
			Expect(err).NotTo(HaveOccurred())
			Expect(x.IsClosed()).To(BeTrue())
		})

		It("should return close error from xmpp", func() {
			x.On("Close", true).Return(errors.New("close error"))
			x.On("IsClosed").Return(true)
			err := c.Close()
			Expect(err).To(MatchError("close error"))
			Expect(x.IsClosed()).To(BeTrue())
		})
	})

	Describe("periodic ping", func() {
		var (
			xm *xmppCMock
			c  *gcmClient
		)

		BeforeEach(func() {
			xm = new(xmppCMock)
			c = &gcmClient{xmppClient: xm}
		})

		It("should exit when xmpp client is closed", func() {
			xm.On("IsClosed").Return(true)
			err := pingPeriodically(xm, time.Millisecond, time.Millisecond)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should exit when xmpp ping errors", func() {
			xm.On("IsClosed").Return(false)
			xm.On("Ping", time.Millisecond).Return(errors.New("Ping"))
			err := pingPeriodically(xm, time.Millisecond, time.Millisecond)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError("Ping"))
		})
	})

	Describe("handling upstream messages", func() {
		var c *gcmClient

		BeforeEach(func() {
			c = &gcmClient{cerr: make(chan error, 1)}
		})

		It("should handle connection drainging request", func() {
			cm := CCSMessage{
				MessageType: CCSControl,
				ControlType: "CONNECTION_DRAINING",
			}
			err := c.onCCSMessage(cm)
			Expect(err).NotTo(HaveOccurred())
			Expect(c.cerr).To(HaveLen(1))
			err = <-c.cerr
			Expect(err).To(MatchError("connection draining"))
		})

		It("should bubble up everything else", func() {
			c.mh = func(cm CCSMessage) error {
				return errors.New("Bubble")
			}
			cm := CCSMessage{
				MessageType: CCSReceipt,
			}
			err := c.onCCSMessage(cm)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError("Bubble"))
		})
	})

	Describe("misc", func() {
		var (
			x *xmppCMock
			c *gcmClient
		)

		BeforeEach(func() {
			x = new(xmppCMock)
			c = &gcmClient{xmppClient: x}
		})

		It("should return client id", func() {
			x.On("ID").Return("id1")
			Expect(c.ID()).To(Equal("id1"))
		})
	})
})
