package gcm

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/mattn/go-xmpp"

	"github.com/rounds/go-gcm/mocks"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("GCM XMPP Client", func() {
	Context("initialized", func() {
		var (
			xm *mocks.XMPPClient
			c  *gcmXMPP
		)

		BeforeEach(func() {
			xm = new(mocks.XMPPClient)
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

			Context("recv sucessful", func() {
				var (
					c1, c2 int
					ret    interface{}
					errCm  = CCSMessage{
						From:             "from",
						MessageID:        "id1",
						MessageType:      "error",
						Error:            "ccs error",
						ErrorDescription: "description",
					}
					errCmStr, ackXmStr string
				)

				f1 := func() interface{} {
					if c1 == 0 {
						c1++
						return ret
					}
					return nil
				}
				f2 := func() error {
					if c2 == 0 {
						c2++
						return nil
					}
					return errors.New("Recv")
				}

				BeforeEach(func() {
					// Recv will be called twice, first time with our payload
					// and the second time with error to exit listen routine.
					c1 = 0
					c2 = 0
					xm.On("Recv").Return(f1, f2).Twice()
					c.closed = true
					data, _ := json.Marshal(&errCm)
					errCmStr = string(data)
					ackXmStr = "<message id=\"id1\"><gcm xmlns=\"google:mobile:data\">{\"message_id\":\"id1\",\"message_type\":\"ack\"}</gcm></message>"
				})

				Context("iq stanza", func() {
					It("should handle xmpp iq stanza when ping", func() {
						ret = xmpp.IQ{Type: "result", ID: "c2s1"}
						err := c.Listen(nil)
						Expect(err).To(Succeed())
						Expect(c.pongs).To(HaveLen(1))
					})

					It("should skip xmpp iq stanza when not ping", func() {
						ret = xmpp.IQ{}
						err := c.Listen(nil)
						Expect(err).To(Succeed())
					})
				})

				Context("presence stanza", func() {
					It("should skip presence stanza", func() {
						ret = xmpp.Presence{}
						err := c.Listen(nil)
						Expect(err).To(Succeed())
					})
				})

				Context("chat stanza", func() {
					It("should skip chat stanza with unknown type", func() {
						ret = xmpp.Chat{Type: "bogus"}
						err := c.Listen(nil)
						Expect(err).To(Succeed())
					})

					Context("empty message type", func() {
						It("should skip stanza if ccs message not decoded", func() {
							ret = xmpp.Chat{Type: "", Other: []string{"bogus"}}
							h := func(cm CCSMessage) error {
								defer GinkgoRecover()
								Fail("should not be called")
								return nil
							}
							err := c.Listen(h)
							Expect(err).To(Succeed())
						})

						Context("ccs ack", func() {
							var (
								um = CCSMessage{
									MessageID:   "id1",
									MessageType: CCSAck,
								}
								umStr string
							)

							BeforeEach(func() {
								d, _ := json.Marshal(&um)
								umStr = string(d)
								ret = xmpp.Chat{Type: "", Other: []string{string(umStr)}}
							})

							It("should ignore ack if no such message in the log", func() {
								h := func(cm CCSMessage) error {
									defer GinkgoRecover()
									Fail("should not be called")
									return nil
								}
								err := c.Listen(h)
								Expect(err).To(Succeed())
							})

							It("should handle ack if message is found in the log", func() {
								xxm := XMPPMessage{MessageID: "id1"}
								c.messages.m = make(map[string]*messageLogEntry)
								c.messages.m[um.MessageID] = &messageLogEntry{body: &xxm, backoff: newExponentialBackoff()}
								h := func(cm CCSMessage) error {
									defer GinkgoRecover()
									Expect(cm).To(Equal(um))
									return nil
								}
								err := c.Listen(h)
								Expect(err).To(Succeed())
								Expect(c.messages.m).To(BeEmpty())
							})
						})

						Context("ccs nack", func() {
							var (
								um = CCSMessage{
									MessageID:   "id1",
									MessageType: CCSNack,
								}
								umStr string
							)

							BeforeEach(func() {
								d, _ := json.Marshal(&um)
								umStr = string(d)
								ret = xmpp.Chat{Type: "", Other: []string{string(umStr)}}
							})

							It("should ignore nack if no such message in the log", func() {
								h := func(cm CCSMessage) error {
									defer GinkgoRecover()
									Fail("should not be called")
									return nil
								}
								err := c.Listen(h)
								Expect(err).To(Succeed())
							})

							It("should handle nack if message is found in the log", func() {
								xxm := XMPPMessage{MessageID: "id1"}
								c.messages.m = make(map[string]*messageLogEntry)
								c.messages.m[um.MessageID] = &messageLogEntry{body: &xxm, backoff: newExponentialBackoff()}
								h := func(cm CCSMessage) error {
									defer GinkgoRecover()
									Expect(cm).To(Equal(um))
									return nil
								}
								err := c.Listen(h)
								Expect(err).To(Succeed())
								Expect(c.messages.m).To(BeEmpty())
							})
						})

						It("should process control message type", func() {
							um := CCSMessage{
								MessageID:   "id1",
								MessageType: CCSControl,
							}
							umStr, _ := json.Marshal(&um)
							ret = xmpp.Chat{Type: "", Other: []string{string(umStr)}}
							h := func(cm CCSMessage) error {
								defer GinkgoRecover()
								Expect(cm).To(Equal(um))
								return nil
							}
							err := c.Listen(h)
							Expect(err).To(Succeed())
						})

						It("should process unknown message type", func() {
							um := CCSMessage{
								MessageID:   "id1",
								MessageType: "unknown",
							}
							umStr, _ := json.Marshal(&um)
							ret = xmpp.Chat{Type: "", Other: []string{string(umStr)}}
							h := func(cm CCSMessage) error {
								defer GinkgoRecover()
								Expect(cm).To(Equal(um))
								return nil
							}
							err := c.Listen(h)
							Expect(err).To(Succeed())
						})
					})

					Context("normal message type", func() {
						It("should skip stanza if ccs message not decoded", func() {
							ret = xmpp.Chat{Type: "normal", Other: []string{"bogus"}}
							h := func(cm CCSMessage) error {
								defer GinkgoRecover()
								Fail("should not be called")
								return nil
							}
							err := c.Listen(h)
							Expect(err).To(Succeed())
						})

						It("should process receipt", func() {
							um := CCSMessage{
								MessageID:   "id1",
								MessageType: CCSReceipt,
							}
							umStr, _ := json.Marshal(&um)
							ret = xmpp.Chat{Type: "normal", Other: []string{string(umStr)}}
							h := func(cm CCSMessage) error {
								defer GinkgoRecover()
								Expect(cm).To(Equal(um))
								return nil
							}
							payload := "<message id=\"id1\"><gcm xmlns=\"google:mobile:data\">{\"message_id\":\"id1\",\"message_type\":\"ack\"}</gcm></message>"
							xm.On("SendOrg", payload).Return(0, nil)
							err := c.Listen(h)
							Expect(err).To(Succeed())
						})

						It("should process unknown message type", func() {
							um := CCSMessage{
								MessageID:   "id1",
								MessageType: "unknown",
							}
							umStr, _ := json.Marshal(&um)
							ret = xmpp.Chat{Type: "normal", Other: []string{string(umStr)}}
							h := func(cm CCSMessage) error {
								defer GinkgoRecover()
								Expect(cm).To(Equal(um))
								return nil
							}
							payload := "<message id=\"id1\"><gcm xmlns=\"google:mobile:data\">{\"message_id\":\"id1\",\"message_type\":\"ack\"}</gcm></message>"
							xm.On("SendOrg", payload).Return(0, nil)
							err := c.Listen(h)
							Expect(err).To(Succeed())
						})

					})

					Context("error message type", func() {
						It("should skip stanza errors if ccs message not decoded", func() {
							ret = xmpp.Chat{Type: "error"}
							h := func(cm CCSMessage) error {
								defer GinkgoRecover()
								Fail("should not be called")
								return nil
							}
							err := c.Listen(h)
							Expect(err).To(Succeed())
						})

						It("should handle stanza errors if ccs message is decoded", func() {
							ret = xmpp.Chat{Type: "error", Other: []string{errCmStr}}
							h := func(cm CCSMessage) error {
								defer GinkgoRecover()
								Expect(cm).To(Equal(errCm))
								return nil
							}
							err := c.Listen(h)
							Expect(err).To(Succeed())
						})
					})
				})
			})
		})

		Describe("closing", func() {
			It("should show if closed", func() {
				c := &gcmXMPP{closed: true}
				Expect(c.IsClosed()).To(BeTrue())
			})

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
				})

				It("should succeed when already closed", func() {
					c.closed = true
					err := c.Close(true)
					Expect(err).To(Succeed())
				})

				It("should succeed when all done", func() {
					xm.On("Close").Return(nil)
					err := c.Close(true)
					Expect(err).To(Succeed())
				})

				It("should succeed with timeout", func() {
					xm.On("Close").Return(nil)
					c.messages.m["id"] = &messageLogEntry{}
					err := c.Close(true)
					Expect(err).To(Succeed())
				})
			})
		})

		Describe("misc", func() {
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
