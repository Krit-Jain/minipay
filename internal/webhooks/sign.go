// Package webhooks holds the cryptography for webhook delivery: HMAC-SHA256
// request signing. It is dependency-free and imports nothing internal, so
// both the relay (sender) and merchant-side verifiers can reason about it
// in isolation. Unit-tested with a fixed RFC-style vector.
package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// Sign returns the hex HMAC-SHA256 of body under secret. The relay sends it
// as X-Minipay-Signature; merchants recompute it over the raw request bytes
// they received.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)

	return hex.EncodeToString(mac.Sum(nil))
}

// Verify recomputes the signature and compares in constant time, so an
// attacker cannot use timing differences to forge one byte at a time.
func Verify(secret string, body []byte, signature string) bool {
	expected, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)

	return hmac.Equal(mac.Sum(nil), expected)
}
