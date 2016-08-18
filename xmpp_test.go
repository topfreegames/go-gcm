package gcm

import (
	"errors"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("GCM XMPP Client", func() {
	Context("initialized", func() {
		var (
			xm *xmppClientMock
			c  *gcmXMPP
		)

		BeforeEach(func() {
			xm = new(xmppClientMock)
			c = &gcmXMPP{xmppClient: xm, pongs: make(chan struct{}, 10)}
		})

		AfterEach(func() {
			xm.AssertExpectations(GinkgoT())
		})

		Describe("pinging", func() {
			It("should fail on ping error", func() {
				xm.On("JID").Return("jid")
				xm.On("PingC2S", "", "").Return(errors.New("Ping"))
				err := c.Ping(time.Second)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError("Ping"))
			})

			It("should fail with ping timeout", func() {
				xm.On("JID").Return("jid")
				xm.On("PingC2S", "", "").Return(nil)
				err := c.Ping(100 * time.Millisecond)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError("gcm xmpp pong timed out after 100ms"))
			})

			It("should succeed with pong", func() {
				xm.On("JID").Return("jid")
				c.pongs <- struct{}{}
				xm.On("PingC2S", "", "").Return(nil)
				err := c.Ping(100 * time.Millisecond)
				Expect(err).To(Succeed())
			})
		})

		Describe("sending", func() {
			var (
				ack, msg               XMPPMessage
				ackPayload, msgPayload string
			)

			BeforeEach(func() {
				c.messages = struct {
					sync.RWMutex
					m map[string]*messageLogEntry
				}{
					m: make(map[string]*messageLogEntry),
				}
				ack = XMPPMessage{MessageType: CCSAck, MessageID: "id1"}
				ackPayload = `<message id="id1"><gcm xmlns="google:mobile:data">{"message_id":"id1","message_type":"ack"}</gcm></message>`
				var data Data = map[string]interface{}{"key": "value"}
				msg = XMPPMessage{MessageID: "id2", Data: data}
				msgPayload = `<message id="id2"><gcm xmlns="google:mobile:data">{"message_id":"id2","data":{"key":"value"}}</gcm></message>`
			})

			It("should send ack successfully", func() {
				xm.On("SendOrg", ackPayload).Return(100, nil)
				id, bytes, err := c.Send(ack)
				Expect(err).To(Succeed())
				Expect(id).To(Equal("id1"))
				Expect(bytes).To(Equal(100))
			})

			It("should fail sending ack", func() {
				xm.On("SendOrg", ackPayload).Return(0, errors.New("SendOrg"))
				id, bytes, err := c.Send(ack)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError("SendOrg"))
				Expect(id).To(Equal("id1"))
				Expect(bytes).To(Equal(0))
			})

			It("should send successfully", func() {
				xm.On("SendOrg", msgPayload).Return(100, nil)
				id, bytes, err := c.Send(msg)
				Expect(err).To(Succeed())
				Expect(id).To(Equal("id2"))
				Expect(bytes).To(Equal(100))
				Expect(len(c.messages.m)).To(Equal(1))
			})

			It("should fail to send", func() {
				xm.On("SendOrg", msgPayload).Return(0, errors.New("Send"))
				id, bytes, err := c.Send(msg)
				Expect(err).To(HaveOccurred())
				Expect(id).To(Equal("id2"))
				Expect(err).To(MatchError("Send"))
				Expect(bytes).To(Equal(0))
				Expect(len(c.messages.m)).To(Equal(0))
			})
		})

		Describe("listening", func() {
			Context("recv error", func() {
				BeforeEach(func() {
					xm.On("Recv").Return("", errors.New("Recv"))
				})

				It("should return nil when the client is closed", func() {
					c.closed = true
					err := c.Listen(nil)
					Expect(err).To(Succeed())
				})

				It("should return error when the client is not closed", func() {
					err := c.Listen(nil)
					Expect(err).To(HaveOccurred())
					Expect(err).To(MatchError("error on Recv: Recv"))
				})
			})
		})

		Describe("closing", func() {
			Context("not graceful close", func() {
				BeforeEach(func() {
					xm.On("JID").Return("jid")
				})

				It("should succeed when internal is successful", func() {
					Expect(c.IsClosed()).To(BeFalse())
					xm.On("Close").Return(nil)
					err := c.Close(false)
					Expect(err).To(Succeed())
					Expect(c.closed).To(BeTrue())
				})

				It("should fail when internal fails", func() {
					Expect(c.IsClosed()).To(BeFalse())
					xm.On("Close").Return(errors.New("Close"))
					err := c.Close(false)
					Expect(err).To(HaveOccurred())
					Expect(err).To(MatchError("Close"))
				})
			})

			Context("graceful close", func() {
				BeforeEach(func() {
					c.messages = struct {
						sync.RWMutex
						m map[string]*messageLogEntry
					}{
						m: make(map[string]*messageLogEntry),
					}
					xm.On("JID").Return("jid")
					xm.On("Close").Return(nil)
				})

				It("should succeed when all done", func() {
					err := c.Close(true)
					Expect(err).To(Succeed())
				})

				It("should succeed with timeout", func() {
					c.messages.m["id"] = &messageLogEntry{}
					err := c.Close(true)
					Expect(err).To(Succeed())
				})
			})
		})

		Describe("misc", func() {
			It("should show if closed", func() {
				c := &gcmXMPP{closed: true}
				Expect(c.IsClosed()).To(BeTrue())
			})

			It("should return internal client jid", func() {
				c := &gcmXMPP{xmppClient: xm}
				xm.On("JID").Return("jid")
				Expect(c.ID()).To(Equal("jid"))
			})

			It("should return valid xmpp user", func() {
				Expect(xmppUser("host", "sender")).To(Equal("sender@host"))
			})
		})
	})
})
