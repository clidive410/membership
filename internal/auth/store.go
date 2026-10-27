package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/clidive410/membership/internal/members"
)

// Login methods recorded on sessions.
const (
	LoginMethodPassword  = "password"
	LoginMethodMagicLink = "magic_link" // reserved: member email links (next iteration)
)

// ErrInvalidCredentials is returned for any failed password login.
var ErrInvalidCredentials = errors.New("invalid email or password")

// Store persists users, roles, sessions and login tokens.
type Store struct{ DB *sql.DB }

// NewStore returns a Store using db.
func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

// User is a login identity.
type User struct {
	ID          int64
	Email       string
	MemberID    sql.NullInt64
	HasPassword bool
	Disabled    bool
	Roles       []Role
	LastLoginAt sql.NullTime
}

func (s *Store) rolesOf(ctx context.Context, userID int64) ([]Role, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT role FROM user_roles WHERE user_id = ? ORDER BY role", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Role
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EnsureUser returns the id of the user with this email, creating it if needed.
func (s *Store) EnsureUser(ctx context.Context, email string) (int64, error) {
	email = members.NormalizeEmail(email)
	if !members.LooksLikeEmail(email) {
		return 0, fmt.Errorf("invalid email %q", email)
	}
	if _, err := s.DB.ExecContext(ctx, "INSERT IGNORE INTO users (email) VALUES (?)", email); err != nil {
		return 0, err
	}
	var id int64
	err := s.DB.QueryRowContext(ctx, "SELECT id FROM users WHERE email = ?", email).Scan(&id)
	return id, err
}

// SetPassword sets (or replaces) a user's password and revokes their sessions.
func (s *Store) SetPassword(ctx context.Context, userID int64, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, "UPDATE users SET password_hash = ?, disabled = FALSE WHERE id = ?", hash, userID); err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID)
	return err
}

// SetDisabled enables or disables a user (disabling revokes sessions).
func (s *Store) SetDisabled(ctx context.Context, userID int64, disabled bool) error {
	if _, err := s.DB.ExecContext(ctx, "UPDATE users SET disabled = ? WHERE id = ?", disabled, userID); err != nil {
		return err
	}
	if disabled {
		_, err := s.DB.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID)
		return err
	}
	return nil
}

// LinkMember links a user to a member row (0 unlinks).
func (s *Store) LinkMember(ctx context.Context, userID, memberID int64) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE users SET member_id = ? WHERE id = ?", sql.NullInt64{Int64: memberID, Valid: memberID != 0}, userID)
	return err
}

// GrantRole adds a role to a user.
func (s *Store) GrantRole(ctx context.Context, userID int64, r Role) error {
	if !ValidRole(r) {
		return fmt.Errorf("unknown role %q", r)
	}
	_, err := s.DB.ExecContext(ctx, "INSERT IGNORE INTO user_roles (user_id, role) VALUES (?, ?)", userID, string(r))
	return err
}

// RevokeRole removes a role from a user.
func (s *Store) RevokeRole(ctx context.Context, userID int64, r Role) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM user_roles WHERE user_id = ? AND role = ?", userID, string(r))
	return err
}

// ListUsers returns all users with their roles.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT id, email, member_id, password_hash IS NOT NULL, disabled, last_login_at FROM users ORDER BY email")
	if err != nil {
		return nil, err
	}
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.MemberID, &u.HasPassword, &u.Disabled, &u.LastLoginAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Roles, err = s.rolesOf(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UserIDByEmail looks up a user id.
func (s *Store) UserIDByEmail(ctx context.Context, email string) (int64, error) {
	var id int64
	err := s.DB.QueryRowContext(ctx, "SELECT id FROM users WHERE email = ?", members.NormalizeEmail(email)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, members.ErrNotFound
	}
	return id, err
}

// Authenticate verifies an email/password pair.
func (s *Store) Authenticate(ctx context.Context, email, password string) (*Principal, error) {
	var (
		id       int64
		hash     sql.NullString
		disabled bool
	)
	err := s.DB.QueryRowContext(ctx, "SELECT id, password_hash, disabled FROM users WHERE email = ?",
		members.NormalizeEmail(email)).Scan(&id, &hash, &disabled)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !hash.Valid) {
		VerifyPassword(dummyHash, password) // equalise timing
		return nil, ErrInvalidCredentials
	} else if err != nil {
		return nil, err
	}
	ok, err := VerifyPassword(hash.String, password)
	if err != nil || !ok || disabled {
		return nil, ErrInvalidCredentials
	}
	if _, err := s.DB.ExecContext(ctx, "UPDATE users SET last_login_at = UTC_TIMESTAMP() WHERE id = ?", id); err != nil {
		return nil, err
	}
	return s.principal(ctx, id)
}

func (s *Store) principal(ctx context.Context, userID int64) (*Principal, error) {
	p := &Principal{UserID: userID}
	var memberID sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, "SELECT email, member_id FROM users WHERE id = ? AND NOT disabled", userID).Scan(&p.Email, &memberID); err != nil {
		return nil, err
	}
	p.MemberID = memberID.Int64
	var err error
	p.Roles, err = s.rolesOf(ctx, userID)
	return p, err
}

// CreateSession starts a session and returns the cookie token.
func (s *Store) CreateSession(ctx context.Context, userID int64, method string, ttl time.Duration) (string, error) {
	token := NewToken()
	_, err := s.DB.ExecContext(ctx, "INSERT INTO sessions (token_hash, user_id, auth_method, expires_at) VALUES (?, ?, ?, ?)",
		HashToken(token), userID, method, time.Now().UTC().Add(ttl))
	return token, err
}

// LookupSession returns the principal for a session token, or (nil, nil)
// when the token is unknown, expired or the user disabled.
func (s *Store) LookupSession(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, nil
	}
	var userID int64
	err := s.DB.QueryRowContext(ctx, `SELECT s.user_id FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > UTC_TIMESTAMP() AND NOT u.disabled`, HashToken(token)).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	p, err := s.principal(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// DeleteSession ends a session.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", HashToken(token))
	return err
}

// PurgeExpired deletes expired sessions and login tokens.
func (s *Store) PurgeExpired(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= UTC_TIMESTAMP()"); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, "DELETE FROM login_tokens WHERE expires_at <= UTC_TIMESTAMP()")
	return err
}

// CreateLoginToken issues a single-use login token (for email magic links).
// Extension point: not wired to any route yet. The next iteration will look
// up the non-archived member by primary email, EnsureUser + LinkMember +
// GrantRole(RoleMember), call this, and email the link via AWS SES.
func (s *Store) CreateLoginToken(ctx context.Context, userID int64, ttl time.Duration) (string, error) {
	token := NewToken()
	_, err := s.DB.ExecContext(ctx, "INSERT INTO login_tokens (token_hash, user_id, purpose, expires_at) VALUES (?, ?, 'magic_link', ?)",
		HashToken(token), userID, time.Now().UTC().Add(ttl))
	return token, err
}

// ConsumeLoginToken atomically marks a token used and returns its user id.
func (s *Store) ConsumeLoginToken(ctx context.Context, token string) (int64, error) {
	h := HashToken(strings.TrimSpace(token))
	res, err := s.DB.ExecContext(ctx, "UPDATE login_tokens SET used_at = UTC_TIMESTAMP() WHERE token_hash = ? AND used_at IS NULL AND expires_at > UTC_TIMESTAMP()", h)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, ErrInvalidCredentials
	}
	var userID int64
	err = s.DB.QueryRowContext(ctx, "SELECT user_id FROM login_tokens WHERE token_hash = ?", h).Scan(&userID)
	return userID, err
}
