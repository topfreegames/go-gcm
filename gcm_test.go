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
			Entry("it should fail on nil message handler", &Config{}, nil, "message handler is nil"),
			Entry("it should fail on empty sender id", &Config{}, func(cm CCSMessage) error { return nil }, "empty sender id"),
			Entry("it should fail on empty sender id", &Config{SenderID: "123"}, func(cm CCSMessage) error { return nil }, "empty api key"),
		)
	})

	Describe("interface implementation", func() {
		var h *httpClientMock
		var x *xmppClientMock
		var c Client
		BeforeEach(func() {
			h = new(httpClientMock)
			x = new(xmppClientMock)
			c = &gcmClient{httpClient: h, xmppClient: x}
		})

		AfterEach(func() {
			gt := GinkgoT()
			h.AssertExpectations(gt)
			x.AssertExpectations(gt)
		})

		It("should send http message", func() {
			m := HTTPMessage{To: "me"}
			r := &HTTPResponse{MulticastID: 100}
			h.On("send", m).Return(r, nil)
			resp, err := c.SendHTTP(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).To(Equal(r))
		})

		It("should fail sending http message", func() {
			m := HTTPMessage{}
			h.On("send", m).Return(nil, errors.New("send error"))
			resp, err := c.SendHTTP(m)
			Expect(err).To(HaveOccurred())
			Expect(resp).To(BeNil())
			Expect(err).To(MatchError("send error"))
		})

		It("should return client id", func() {
			x.On("ID").Return("my id")
			Expect(c.ID()).To(Equal("my id"))
		})

		It("should send xmpp message", func() {
			m := XMPPMessage{To: "me", MessageID: "my id"}
			x.On("send", m).Return("my id", 100, nil)
			id, bytes, err := c.SendXMPP(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(id).To(Equal("my id"))
			Expect(bytes).To(Equal(100))
		})

		It("should fail sending xmpp message", func() {
			m := XMPPMessage{To: "me", MessageID: "my id"}
			x.On("send", m).Return("", 0, errors.New("send error"))
			id, bytes, err := c.SendXMPP(m)
			Expect(err).To(HaveOccurred())
			Expect(id).To(BeEmpty())
			Expect(bytes).To(Equal(0))
			Expect(err).To(MatchError("send error"))
		})

		It("should close successfully", func() {
			x.On("close", true).Return(nil)
			err := c.Close()
			Expect(err).NotTo(HaveOccurred())
		})

		It("should return close error from xmpp", func() {
			x.On("close", true).Return(errors.New("close error"))
			err := c.Close()
			Expect(err).To(MatchError("close error"))
		})
	})
})
