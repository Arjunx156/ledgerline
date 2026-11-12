// Package ids makes the prefixed identifiers every object carries, such as
// pi_ followed by 24 random letters and digits (spec 3.8).
package ids

import (
	"crypto/rand"
	"strings"
)

// Prefixes in use. The prefix tells a reader what an id is at a glance.
const (
	Merchant        = "acct"
	Customer        = "cus"
	PaymentIntent   = "pi"
	Refund          = "re"
	Event           = "evt"
	LedgerTxn       = "txn"
	WebhookEndpoint = "we"
	Delivery        = "whd"
	Request         = "req"
	Authorization   = "auth"
)

const (
	alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	length   = 24
)

// New returns prefix_ followed by 24 characters from a cryptographically
// secure source, about 143 bits: guessing an id is not a way in.
func New(prefix string) string {
	var b strings.Builder
	b.Grow(len(prefix) + 1 + length)
	b.WriteString(prefix)
	b.WriteByte('_')
	b.WriteString(Random(length))
	return b.String()
}

// Random returns n characters drawn uniformly from letters and digits.
// Bytes at or above 248 are
// rejected because 248 is the largest multiple of 62 that fits in a byte;
// keeping them would favour the first eight characters.
func Random(n int) string {
	out := make([]byte, 0, n)
	buf := make([]byte, n*2)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			panic("ids: crypto/rand failed: " + err.Error()) // documented never to fail
		}
		for _, c := range buf {
			if c < 248 && len(out) < n {
				out = append(out, alphabet[int(c)%len(alphabet)])
			}
		}
	}
	return string(out)
}
