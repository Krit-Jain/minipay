package webhooks

import "testing"

func TestSignVerifyRoundtrip(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`{"id":"abc","type":"payment.captured"}`)

	sig := Sign(secret, body)
	if len(sig) != 64 {
		t.Fatalf("signature length = %d, want 64 hex chars", len(sig))
	}

	if !Verify(secret, body, sig) {
		t.Fatal("valid signature rejected")
	}

	if Verify("wrong-secret", body, sig) {
		t.Fatal("wrong secret accepted")
	}

	if Verify(secret, []byte(`{"id":"abc","type":"payment.failed"}`), sig) {
		t.Fatal("tampered body accepted")
	}

	if Verify(secret, body, "not-hex!!") {
		t.Fatal("malformed signature accepted")
	}
}
