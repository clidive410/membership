package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// NewToken returns a random 256-bit URL-safe token.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken returns the hex SHA-256 of a token; only hashes are stored.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CSRF derives and checks form tokens bound to a per-browser random value
// (kept in an HttpOnly cookie), keyed with the session secret.
type CSRF struct{ Secret []byte }

// Token returns the form token for the given browser value.
func (c CSRF) Token(browserValue string) string {
	mac := hmac.New(sha256.New, c.Secret)
	mac.Write([]byte("csrf:" + browserValue))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Valid reports whether formToken matches browserValue.
func (c CSRF) Valid(browserValue, formToken string) bool {
	if browserValue == "" || formToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Token(browserValue)), []byte(formToken)) == 1
}
