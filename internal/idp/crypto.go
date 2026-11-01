package idp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// HKDF info strings: one independent key per purpose, all derived from
// SESSION_SECRET, so that the secret itself is never used as a key.
const (
	infoKeyEncryption = "clidive-membership oidc signing-key encryption v1"
	infoTokenCrypto   = "clidive-membership oidc token crypto v1"
)

// deriveKey returns a 256-bit key for purpose info from the session secret.
func deriveKey(secret []byte, info string) ([]byte, error) {
	if len(secret) < 32 {
		return nil, errors.New("idp: the session secret must be at least 32 bytes")
	}
	return hkdf.Key(sha256.New, secret, nil, info, 32)
}

// aead is AES-256-GCM with a random 96-bit nonce prepended to the sealed
// data.
type aead struct{ gcm cipher.AEAD }

func newAEAD(key []byte) (*aead, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &aead{gcm: gcm}, nil
}

func (a *aead) seal(plain, ad []byte) []byte {
	nonce := make([]byte, a.gcm.NonceSize(), a.gcm.NonceSize()+len(plain)+a.gcm.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return a.gcm.Seal(nonce, nonce, plain, ad)
}

func (a *aead) open(sealed, ad []byte) ([]byte, error) {
	n := a.gcm.NonceSize()
	if len(sealed) < n+a.gcm.Overhead() {
		return nil, errors.New("idp: sealed data too short")
	}
	return a.gcm.Open(nil, sealed[:n], sealed[n:], ad)
}

// tokenCrypto implements op.Crypto. The library encrypts "<token id>:<subject>"
// into opaque access tokens and decrypts them at the userinfo and revocation
// endpoints, where it expects exactly one colon. Our subjects ("user:42")
// contain one, so Decrypt escapes colons in the subject; the storage never
// trusts that subject anyway and reads the user from the token row.
type tokenCrypto struct{ a *aead }

var tokenAD = []byte("oidc-token")

func (c tokenCrypto) Encrypt(s string) (string, error) {
	return base64.RawURLEncoding.EncodeToString(c.a.seal([]byte(s), tokenAD)), nil
}

func (c tokenCrypto) Decrypt(s string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	plain, err := c.a.open(raw, tokenAD)
	if err != nil {
		return "", err
	}
	id, sub, ok := strings.Cut(string(plain), ":")
	if !ok {
		return string(plain), nil
	}
	return id + ":" + strings.ReplaceAll(sub, ":", "%3A"), nil
}
