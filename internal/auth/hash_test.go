package auth

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestHashSecret_MatchesKnownVector locks the HMAC-SHA256 output for
// a fixed (key, message) pair. The fixture digest was produced by
// openssl and cross-verified with python's hmac module, so any
// drift in the algorithm or its byte encoding will fail this test.
//
//	printf 'c0ffee' | openssl dgst -sha256 \
//	  -hmac 'test-encryption-key-32-bytes-long' -binary | xxd -p -c 64
//	python3 -c 'import hmac, hashlib; \
//	  print(hmac.new(b"test-encryption-key-32-bytes-long", \
//	  b"c0ffee", hashlib.sha256).hexdigest())'
//
//	-> 0996712ac834605a74e9c331ab48784572ec5afe364d19df08e4d48031e8521b
//
// Both tools agree on the same digest — that is the binding
// expectation here.
func TestHashSecret_MatchesKnownVector(t *testing.T) {
	const (
		key    = "test-encryption-key-32-bytes-long"
		secret = "c0ffee"
		want   = "0996712ac834605a74e9c331ab48784572ec5afe364d19df08e4d48031e8521b"
	)
	got := HashSecret(key, secret)
	if got != want {
		t.Fatalf("HashSecret(%q, %q):\n  got  %s\n  want %s", key, secret, got, want)
	}
}

// TestHashSecret_OutputIsHexLowercase64 locks the output shape:
// 64 hex chars, lowercase. The digest is compared verbatim against
// the ApiKey.hash column, which stores lowercase hex.
func TestHashSecret_OutputIsHexLowercase64(t *testing.T) {
	out := HashSecret("k", "v")
	if len(out) != 64 {
		t.Errorf("expected 64 hex chars, got %d (%q)", len(out), out)
	}
	if out != strings.ToLower(out) {
		t.Errorf("expected lowercase hex, got %q", out)
	}
	if _, err := hex.DecodeString(out); err != nil {
		t.Errorf("output is not valid hex: %v (%q)", err, out)
	}
}

// TestHashSecret_DeterministicAcrossCalls confirms the same input
// always produces the same hash — no per-call randomness was
// accidentally introduced.
func TestHashSecret_DeterministicAcrossCalls(t *testing.T) {
	a := HashSecret("k", "v")
	b := HashSecret("k", "v")
	if a != b {
		t.Fatalf("HashSecret should be deterministic: %q vs %q", a, b)
	}
}

// TestHashSecret_KeyAffectsOutput is a basic distinguishability check:
// changing the key (with message held constant) must change the
// output. If this fails, the key argument was silently ignored — a
// security regression.
func TestHashSecret_KeyAffectsOutput(t *testing.T) {
	a := HashSecret("key1", "v")
	b := HashSecret("key2", "v")
	if a == b {
		t.Fatalf("key argument must affect HMAC output: %q == %q", a, b)
	}
}

// TestHashSecret_MessageAffectsOutput is the dual of KeyAffectsOutput:
// changing the message (key held constant) must change the output.
func TestHashSecret_MessageAffectsOutput(t *testing.T) {
	a := HashSecret("k", "v1")
	b := HashSecret("k", "v2")
	if a == b {
		t.Fatalf("message argument must affect HMAC output: %q == %q", a, b)
	}
}

// TestHashSecret_EmptyInputsDoNotPanic asserts that empty key or empty
// message return a deterministic hex string without panic. (HMAC with
// an empty key is defined; we just need the boundary not to crash.)
func TestHashSecret_EmptyInputsDoNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("HashSecret panicked on empty inputs: %v", r)
		}
	}()
	if got := HashSecret("", ""); len(got) != 64 {
		t.Fatalf("expected 64-char output for empty inputs, got %q", got)
	}
}
