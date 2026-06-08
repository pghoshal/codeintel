package auth

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// AES-256-CBC encryption with PKCS#7 padding. The 32-ASCII-byte key
// is read from CODEINTEL_ENCRYPTION_KEY at startup and applies to
// every Encrypt/Decrypt site. IV is a 16-byte CSPRNG value from
// crypto/rand on each Encrypt call so two ciphertexts of the same
// plaintext do not collide.

const aesBlockSize = aes.BlockSize // 16

// ErrInvalidEncryptionKey indicates the supplied key was not exactly
// 32 ASCII bytes. Surfaced cleanly so a misconfiguration produces a
// clear startup or boundary error rather than a runtime panic deep
// inside the crypto stack.
var ErrInvalidEncryptionKey = errors.New("auth: encryption key must be exactly 32 ASCII bytes (AES-256)")

// Encrypt returns (iv, ciphertext) as lowercase hex strings. The
// caller persists both columns; Decrypt reverses the operation.
func Encrypt(key, plaintext string) (string, string, error) {
	if len(key) != 32 {
		return "", "", ErrInvalidEncryptionKey
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", "", fmt.Errorf("auth: NewCipher: %w", err)
	}
	iv := make([]byte, aesBlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", "", fmt.Errorf("auth: rand.Read: %w", err)
	}
	padded := pkcs7Pad([]byte(plaintext), aesBlockSize)
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(encrypted, padded)
	return hex.EncodeToString(iv), hex.EncodeToString(encrypted), nil
}

// Decrypt is the inverse of Encrypt. Takes hex-encoded iv +
// ciphertext (the wire format of the encrypted-secret columns) and
// returns the original plaintext.
func Decrypt(key, ivHex, ciphertextHex string) (string, error) {
	if len(key) != 32 {
		return "", ErrInvalidEncryptionKey
	}
	iv, err := hex.DecodeString(ivHex)
	if err != nil {
		return "", fmt.Errorf("auth: decode iv: %w", err)
	}
	if len(iv) != aesBlockSize {
		return "", fmt.Errorf("auth: iv must be %d bytes, got %d", aesBlockSize, len(iv))
	}
	ciphertext, err := hex.DecodeString(ciphertextHex)
	if err != nil {
		return "", fmt.Errorf("auth: decode ciphertext: %w", err)
	}
	if len(ciphertext) == 0 || len(ciphertext)%aesBlockSize != 0 {
		return "", fmt.Errorf("auth: ciphertext length %d is not a positive multiple of the block size", len(ciphertext))
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", fmt.Errorf("auth: NewCipher: %w", err)
	}
	decrypted := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(decrypted, ciphertext)
	unpadded, err := pkcs7Unpad(decrypted, aesBlockSize)
	if err != nil {
		return "", err
	}
	return string(unpadded), nil
}

// pkcs7Pad appends (blockSize - len%blockSize) bytes to in, each
// equal to the pad length. Always appends at least one block —
// pure PKCS#7 / RFC 5652 semantics.
func pkcs7Pad(in []byte, blockSize int) []byte {
	padLen := blockSize - len(in)%blockSize
	pad := bytes.Repeat([]byte{byte(padLen)}, padLen)
	return append(in, pad...)
}

// pkcs7Unpad is the inverse of pkcs7Pad and verifies the padding
// bytes before trimming. Returns an error on malformed padding so
// tamper attempts surface as decrypt failures.
func pkcs7Unpad(in []byte, blockSize int) ([]byte, error) {
	if len(in) == 0 || len(in)%blockSize != 0 {
		return nil, errors.New("auth: invalid padded length")
	}
	padLen := int(in[len(in)-1])
	if padLen == 0 || padLen > blockSize {
		return nil, errors.New("auth: invalid pkcs7 pad length")
	}
	pad := in[len(in)-padLen:]
	for _, b := range pad {
		if int(b) != padLen {
			return nil, errors.New("auth: pkcs7 padding bytes do not match length")
		}
	}
	return in[:len(in)-padLen], nil
}
