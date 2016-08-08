package gcm

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("XMPP Client", func() {
	Context("infrastructure", func() {
		It("should return valid xmpp user", func() {
			Expect(xmppUser("host", "sender")).To(Equal("sender@host"))
		})

		It("should show if it is closed", func() {
			c := xmppGCMClient{closed: true}
			Expect(c.isClosed()).To(Equal(true))
		})
	})
})
