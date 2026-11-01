package idp

import (
	"strings"
	"testing"
)

var testSecret = []byte(strings.Repeat("k", 40))

func TestTokenCryptoEscapesSubjectColons(t *testing.T) {
	k, err := deriveKey(testSecret, infoTokenCrypto)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := newAEAD(k)
	c := tokenCrypto{a: a}
	tok, err := c.Encrypt("tokenid:user:42")
	if err != nil || strings.Contains(tok, "user") {
		t.Fatalf("encrypt: %q %v", tok, err)
	}
	got, err := c.Decrypt(tok)
	if err != nil || got != "tokenid:user%3A42" || strings.Count(got, ":") != 1 {
		t.Errorf("decrypt = %q, %v", got, err)
	}
	if _, err := c.Decrypt(tok[:len(tok)-2] + "AA"); err == nil {
		t.Error("tampered token decrypted")
	}
	other, _ := deriveKey(testSecret, infoKeyEncryption)
	if string(other) == string(k) {
		t.Error("keys for different purposes must differ")
	}
	if _, err := deriveKey([]byte("short"), infoTokenCrypto); err == nil {
		t.Error("short secret accepted")
	}
}

func TestSubject(t *testing.T) {
	if Subject(42) != "user:42" {
		t.Error(Subject(42))
	}
	for in, want := range map[string]int64{"user:42": 42, "user:": 0, "42": 0, "user:-1": 0, "user:x": 0} {
		if got, _ := ParseSubject(in); got != want {
			t.Errorf("ParseSubject(%q) = %d", in, got)
		}
	}
}

func TestClientSpecValidation(t *testing.T) {
	good := ClientSpec{ID: "wordpress", RedirectURIs: []string{"https://www.example.org/wp-admin/admin-ajax.php?action=openid-connect-authorize"}}
	if err := good.normalize(); err != nil {
		t.Fatal(err)
	}
	if good.Name != "wordpress" || len(good.PostLogoutRedirectURIs) != 1 || good.PostLogoutRedirectURIs[0] != "https://www.example.org/" ||
		strings.Join(good.Scopes, " ") != strings.Join(DefaultClientScopes, " ") {
		t.Errorf("defaults: %+v", good)
	}
	for _, uri := range []string{"http://www.example.org/cb", "https://example.org/cb#x", "https://*.example.org/cb", "/relative",
		"https://user@example.org/cb", "javascript:alert(1)"} {
		s := ClientSpec{ID: "x", RedirectURIs: []string{uri}}
		if err := s.normalize(); err == nil {
			t.Errorf("accepted redirect URI %q", uri)
		}
	}
	for _, uri := range []string{"http://localhost:8081/cb", "http://127.0.0.1/cb"} {
		s := ClientSpec{ID: "x", RedirectURIs: []string{uri}}
		if err := s.normalize(); err != nil {
			t.Errorf("refused development URI %q: %v", uri, err)
		}
	}
	for _, id := range []string{"", "a b", "a:b", strings.Repeat("x", 101)} {
		s := ClientSpec{ID: id, RedirectURIs: good.RedirectURIs}
		if err := s.normalize(); err == nil {
			t.Errorf("accepted id %q", id)
		}
	}
}
