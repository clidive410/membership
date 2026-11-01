package idp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-sql-driver/mysql"
)

// Signing key policy.
const (
	keyBits = 2048
	// RetiredKeyGrace is how long a retired key stays in the JWKS, so that
	// tokens signed just before a rotation still verify.
	RetiredKeyGrace = 24 * time.Hour
	// keyCacheTTL bounds how long another instance's rotation goes unseen.
	keyCacheTTL = time.Minute
)

// signingKey implements op.SigningKey.
type signingKey struct {
	kid string
	key *rsa.PrivateKey
}

func (k *signingKey) SignatureAlgorithm() jose.SignatureAlgorithm { return jose.RS256 }
func (k *signingKey) Key() any                                    { return k.key }
func (k *signingKey) ID() string                                  { return k.kid }

// publicKey implements op.Key.
type publicKey struct {
	kid string
	key *rsa.PublicKey
}

func (k *publicKey) ID() string                         { return k.kid }
func (k *publicKey) Algorithm() jose.SignatureAlgorithm { return jose.RS256 }
func (k *publicKey) Use() string                        { return "sig" }
func (k *publicKey) Key() any                           { return k.key }

// KeyStore keeps the RS256 signing keys in oidc_keys. The private key is
// sealed with AES-256-GCM under a key derived from SESSION_SECRET, with the
// kid as additional data. The first key is generated on first use.
type KeyStore struct {
	db  *sql.DB
	enc *aead
	now func() time.Time

	mu       sync.Mutex
	active   *signingKey
	cachedAt time.Time
}

// NewKeyStore returns a KeyStore sealing keys with a key derived from secret.
func NewKeyStore(db *sql.DB, secret []byte, now func() time.Time) (*KeyStore, error) {
	k, err := deriveKey(secret, infoKeyEncryption)
	if err != nil {
		return nil, err
	}
	a, err := newAEAD(k)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &KeyStore{db: db, enc: a, now: now}, nil
}

// Active returns the current signing key, generating the first one if
// there is none.
func (ks *KeyStore) Active(ctx context.Context) (*signingKey, error) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if ks.active != nil && ks.now().Sub(ks.cachedAt) < keyCacheTTL {
		return ks.active, nil
	}
	for range 3 {
		k, err := ks.loadActive(ctx)
		if err == nil {
			ks.active, ks.cachedAt = k, ks.now()
			return k, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		// No key yet. The unique active_slot makes a concurrent first
		// insert fail, in which case the other instance's key is loaded.
		if _, err := ks.insert(ctx, ks.db); err != nil && !isDuplicate(err) {
			return nil, err
		}
	}
	return nil, errors.New("idp: no active signing key")
}

func (ks *KeyStore) loadActive(ctx context.Context) (*signingKey, error) {
	var (
		kid    string
		sealed []byte
	)
	if err := ks.db.QueryRowContext(ctx, "SELECT kid, private_key_enc FROM oidc_keys WHERE active_slot = 1").Scan(&kid, &sealed); err != nil {
		return nil, err
	}
	der, err := ks.enc.open(sealed, []byte(kid))
	if err != nil {
		return nil, fmt.Errorf("idp: decrypting signing key %s (was SESSION_SECRET changed? run `membership oidc rotate-keys`): %w", kid, err)
	}
	priv, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	rk, ok := priv.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("idp: signing key %s is not RSA", kid)
	}
	return &signingKey{kid: kid, key: rk}, nil
}

type dbExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (ks *KeyStore) insert(ctx context.Context, db dbExec) (string, error) {
	priv, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", err
	}
	pub, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	kid := hex.EncodeToString(b)
	_, err = db.ExecContext(ctx, "INSERT INTO oidc_keys (kid, alg, private_key_enc, public_key, active_slot, created_at) VALUES (?, 'RS256', ?, ?, 1, ?)",
		kid, ks.enc.seal(der, []byte(kid)), pub, ks.now().UTC())
	return kid, err
}

// Rotate retires the active key (it stays published for RetiredKeyGrace)
// and makes a new one active. It returns the new kid.
func (ks *KeyStore) Rotate(ctx context.Context) (string, error) {
	tx, err := ks.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE oidc_keys SET active_slot = NULL, retired_at = ? WHERE active_slot = 1", ks.now().UTC()); err != nil {
		return "", err
	}
	kid, err := ks.insert(ctx, tx)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	ks.mu.Lock()
	ks.active = nil
	ks.mu.Unlock()
	return kid, nil
}

// Published returns the keys for the JWKS: the active key and keys retired
// less than RetiredKeyGrace ago. It never generates a key.
func (ks *KeyStore) Published(ctx context.Context) ([]*publicKey, error) {
	rows, err := ks.db.QueryContext(ctx, "SELECT kid, public_key FROM oidc_keys WHERE active_slot = 1 OR retired_at > ? ORDER BY created_at DESC",
		ks.now().UTC().Add(-RetiredKeyGrace))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*publicKey
	for rows.Next() {
		var (
			kid string
			der []byte
		)
		if err := rows.Scan(&kid, &der); err != nil {
			return nil, err
		}
		pub, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			return nil, err
		}
		rk, ok := pub.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("idp: key %s is not RSA", kid)
		}
		out = append(out, &publicKey{kid: kid, key: rk})
	}
	return out, rows.Err()
}

// isDuplicate reports a MariaDB duplicate-key error (1062).
func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
