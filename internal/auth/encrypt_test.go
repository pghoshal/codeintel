package auth

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestEncryptDecrypt_Roundtrip locks the basic correctness contract:
// Decrypt(Encrypt(x)) == x. Both halves of the port must agree on
// algorithm + key derivation or migration breaks every existing
// OrgSecret row.
func TestEncryptDecrypt_Roundtrip(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef" // 32 ASCII bytes
	plaintext := "hunter2!@#"
	iv, ciphertext, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if iv == "" || ciphertext == "" {
		t.Fatalf("Encrypt returned empty outputs: iv=%q ciphertext=%q", iv, ciphertext)
	}
	if !isHex(iv) {
		t.Errorf("iv is not hex: %q", iv)
	}
	if !isHex(ciphertext) {
		t.Errorf("ciphertext is not hex: %q", ciphertext)
	}
	got, err := Decrypt(key, iv, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plaintext {
		t.Fatalf("roundtrip: got %q, want %q", got, plaintext)
	}
}

// TestEncrypt_ShapeMatchesLegacy locks the wire-format contract: a
// 16-byte IV emitted as 32 hex chars (lowercase). Legacy
// crypto.ts:encrypt uses `iv.toString('hex')` with a 16-byte random
// IV; OrgSecret.iv columns in production store the exact same shape.
func TestEncrypt_ShapeMatchesLegacy(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	iv, ciphertext, err := Encrypt(key, "x")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(iv) != 32 {
		t.Errorf("iv length: got %d, want 32 (16 bytes hex)", len(iv))
	}
	if iv != strings.ToLower(iv) {
		t.Errorf("iv must be lowercase hex: %q", iv)
	}
	// AES-CBC padded block length is multiple of 16 bytes = 32 hex chars.
	if len(ciphertext)%32 != 0 {
		t.Errorf("ciphertext length not a multiple of 32 hex chars (1 AES block): %d", len(ciphertext))
	}
}

// TestEncrypt_IVIsRandomPerCall confirms two calls with the same
// plaintext produce different ciphertexts (because the IV is random).
// CBC with a fixed IV would leak whether two stored secrets are
// equal — a security regression we must catch.
func TestEncrypt_IVIsRandomPerCall(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	iv1, c1, err := Encrypt(key, "samevalue")
	if err != nil {
		t.Fatalf("Encrypt #1: %v", err)
	}
	iv2, c2, err := Encrypt(key, "samevalue")
	if err != nil {
		t.Fatalf("Encrypt #2: %v", err)
	}
	if iv1 == iv2 {
		t.Fatalf("IVs must differ between calls: both %q", iv1)
	}
	if c1 == c2 {
		t.Fatalf("ciphertexts must differ between calls: both %q", c1)
	}
}

// TestEncrypt_RejectsBadKeyLength asserts the key-length guard.
// ASCII bytes so a misconfigured key produces a crypto error rather
// than a silent truncation.
func TestEncrypt_RejectsBadKeyLength(t *testing.T) {
	for _, k := range []string{"", "short", strings.Repeat("a", 31), strings.Repeat("a", 33)} {
		if _, _, err := Encrypt(k, "x"); err == nil {
			t.Errorf("Encrypt with key of length %d must error", len(k))
		}
	}
}

// TestDecrypt_KnownVector confirms the byte-equal decrypt path
// against a fixture produced by openssl using AES-256-CBC + PKCS#7
// padding — the canonical algorithm pairing:
//
//   printf 'hello' | openssl enc -aes-256-cbc \
//     -K '3031323334353637383961626364656630313233343536373839616263646566' \
//     -iv '00112233445566778899aabbccddeeff' -nosalt | xxd -p -c 256
//
//   → bec9eaa408e659897944a7c82d7d8875
//
// Drift here means a row encrypted with this key/algorithm pair
// will not decrypt — a production data outage.
func TestDecrypt_KnownVector(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	const iv = "00112233445566778899aabbccddeeff"
	const ciphertext = "bec9eaa408e659897944a7c82d7d8875"
	got, err := Decrypt(key, iv, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != "hello" {
		t.Fatalf("Decrypt(known vector): got %q, want %q", got, "hello")
	}
}

// TestDecrypt_RejectsMalformed asserts hostile inputs (non-hex IV,
// non-hex ciphertext, wrong-length IV) return errors rather than
// panicking.
func TestDecrypt_RejectsMalformed(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name, iv, ct string
	}{
		{"non_hex_iv", "ZZ112233445566778899aabbccddeeff", "fc586d6ad2c812ff66de14fbedfd5d6c"},
		{"short_iv", "00", "fc586d6ad2c812ff66de14fbedfd5d6c"},
		{"non_hex_ct", "00112233445566778899aabbccddeeff", "ZZ586d6ad2c812ff66de14fbedfd5d6c"},
		{"empty_ct", "00112233445566778899aabbccddeeff", ""},
		{"not_block_aligned_ct", "00112233445566778899aabbccddeeff", "fc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on malformed input: %v", r)
				}
			}()
			if _, err := Decrypt(key, c.iv, c.ct); err == nil {
				t.Errorf("expected an error for input %q/%q", c.iv, c.ct)
			}
		})
	}
}

// isHex reports whether s decodes cleanly as lowercase hex.
func isHex(s string) bool {
	if _, err := hex.DecodeString(s); err != nil {
		return false
	}
	return s == strings.ToLower(s)
}
