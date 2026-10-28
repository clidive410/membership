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
	LoginMethodMagicLink = "magic_link" // emailed sign-in link (members)
)

// Audit actions written by the sign-in flow. Entries identify the user by
// id only (actor_user_id and entity_id), never by email.
const (
	ActionLoginLink = "auth.login_link" // a sign-in link was emailed
	ActionLoginVia  = "auth.login"      // a sign-in link was used
)

// auditActor labels sign-in audit entries; the user id is in actor_user_id.
const auditActor = "member-login"

// ErrNoMember is returned by RequestLoginLink when the email does not
// identify exactly one current (non-archived) member.
var ErrNoMember = errors.New("no single current member with this email")

// ErrLoginNotAllowed is returned by RequestLoginLink when the email belongs
// to a member but no link may be issued: the user is disabled, or the login
// identity is linked inconsistently (another user is linked to the member,
// or this user to another member). The committee must resolve it.
var ErrLoginNotAllowed = errors.New("sign-in link not allowed for this user")

// ErrInvalidToken is returned for an unknown, used or expired sign-in link,
// or when its user may no longer sign in.
var ErrInvalidToken = errors.New("invalid or expired sign-in link")

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
	MemberName  string // linked member's name ("" if none)
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
	rows, err := s.DB.QueryContext(ctx, "SELECT "+userColumns+" FROM users u LEFT JOIN members m ON m.id = u.member_id ORDER BY u.email")
	if err != nil {
		return nil, err
	}
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.MemberID, &u.HasPassword, &u.Disabled, &u.LastLoginAt, &u.MemberName); err != nil {
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
	return s.principal(ctx, id, LoginMethodPassword)
}

// principal loads a user's identity and roles. Sessions started with an
// emailed link carry the member role only: committee roles need a password
// sign-in, so that access to the admin UI never rests on email alone.
func (s *Store) principal(ctx context.Context, userID int64, method string) (*Principal, error) {
	p := &Principal{UserID: userID, AuthMethod: method}
	var memberID sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, "SELECT email, member_id FROM users WHERE id = ? AND NOT disabled", userID).Scan(&p.Email, &memberID); err != nil {
		return nil, err
	}
	p.MemberID = memberID.Int64
	roles, err := s.rolesOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, r := range roles {
		if method != LoginMethodMagicLink || r == RoleMember {
			p.Roles = append(p.Roles, r)
		}
	}
	return p, nil
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
	var (
		userID int64
		method string
	)
	err := s.DB.QueryRowContext(ctx, `SELECT s.user_id, s.auth_method FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > UTC_TIMESTAMP() AND NOT u.disabled`, HashToken(token)).Scan(&userID, &method)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	p, err := s.principal(ctx, userID, method)
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

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Purposes of login_tokens rows. A token is only accepted for the purpose
// it was issued for, so a sign-in link cannot reset a password and a
// password-reset link cannot start a member session.
const (
	PurposeMagicLink     = "magic_link"     // emailed member sign-in link
	PurposePasswordReset = "password_reset" // password reset or invitation link
)

func createLoginToken(ctx context.Context, db execer, userID int64, purpose string, ttl time.Duration) (string, error) {
	token := NewToken()
	_, err := db.ExecContext(ctx, "INSERT INTO login_tokens (token_hash, user_id, purpose, expires_at) VALUES (?, ?, ?, ?)",
		HashToken(token), userID, purpose, time.Now().UTC().Add(ttl))
	return token, err
}

// consumeLoginToken atomically marks a token of the given purpose used and
// returns its user id.
func consumeLoginToken(ctx context.Context, db execer, token, purpose string) (int64, error) {
	h := HashToken(strings.TrimSpace(token))
	res, err := db.ExecContext(ctx, `UPDATE login_tokens SET used_at = UTC_TIMESTAMP()
		WHERE token_hash = ? AND purpose = ? AND used_at IS NULL AND expires_at > UTC_TIMESTAMP()`, h, purpose)
	if err != nil {
		return 0, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, err
	} else if n != 1 {
		return 0, ErrInvalidToken
	}
	var userID int64
	err = db.QueryRowContext(ctx, "SELECT user_id FROM login_tokens WHERE token_hash = ?", h).Scan(&userID)
	return userID, err
}

// CreateLoginToken issues a single-use login token for userID. Only its
// SHA-256 is stored. RequestLoginLink is the normal entry point.
func (s *Store) CreateLoginToken(ctx context.Context, userID int64, ttl time.Duration) (string, error) {
	return createLoginToken(ctx, s.DB, userID, PurposeMagicLink, ttl)
}

// ConsumeLoginToken atomically marks a token used and returns its user id
// (ErrInvalidToken if it is unknown, used or expired). LoginWithToken is
// the normal entry point.
func (s *Store) ConsumeLoginToken(ctx context.Context, token string) (int64, error) {
	return consumeLoginToken(ctx, s.DB, token, PurposeMagicLink)
}

func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// LoginLink is an issued sign-in link token.
type LoginLink struct {
	Token    string // raw token for the emailed URL; never log or store it
	UserID   int64
	MemberID int64
}

// RequestLoginLink issues a sign-in link for the current member whose
// primary email is email (trimmed, lower-cased). In one transaction it
// finds or creates the user with that email, links it to the member,
// grants the member role, stores the token hash and writes an audit entry.
//
// It returns ErrNoMember unless exactly one non-archived member has this
// primary email (an expired membership is fine), and ErrLoginNotAllowed for
// a disabled or inconsistently linked user.
func (s *Store) RequestLoginLink(ctx context.Context, email string, ttl time.Duration) (*LoginLink, error) {
	email = members.NormalizeEmail(email)
	if !members.LooksLikeEmail(email) {
		return nil, ErrNoMember
	}
	var link *LoginLink
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT id FROM members WHERE email1 = ? AND deleted_at IS NULL LIMIT 2", email)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) != 1 {
			return ErrNoMember
		}
		memberID := ids[0]

		if _, err := tx.ExecContext(ctx, "INSERT IGNORE INTO users (email) VALUES (?)", email); err != nil {
			return err
		}
		var (
			userID   int64
			linked   sql.NullInt64
			disabled bool
		)
		if err := tx.QueryRowContext(ctx, "SELECT id, member_id, disabled FROM users WHERE email = ? FOR UPDATE", email).
			Scan(&userID, &linked, &disabled); err != nil {
			return err
		}
		if disabled || (linked.Valid && linked.Int64 != memberID) {
			return ErrLoginNotAllowed
		}
		if !linked.Valid {
			var other int64
			err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE member_id = ? AND id <> ?", memberID, userID).Scan(&other)
			if err == nil {
				return ErrLoginNotAllowed // the member is linked to another login email
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE users SET member_id = ? WHERE id = ?", memberID, userID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT IGNORE INTO user_roles (user_id, role) VALUES (?, ?)", userID, string(RoleMember)); err != nil {
			return err
		}
		token, err := createLoginToken(ctx, tx, userID, PurposeMagicLink, ttl)
		if err != nil {
			return err
		}
		if err := members.WriteAudit(ctx, tx, members.Actor{UserID: userID, Label: auditActor}, ActionLoginLink, 0, "user", userID, nil); err != nil {
			return err
		}
		link = &LoginLink{Token: token, UserID: userID, MemberID: memberID}
		return nil
	})
	return link, err
}

// LoginWithToken consumes a sign-in link token and returns the member
// principal to start a magic-link session for. The token is spent even
// when the sign-in is then refused (user disabled, member archived or
// unlinked, member role revoked): every such case is ErrInvalidToken.
func (s *Store) LoginWithToken(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, ErrInvalidToken
	}
	var userID int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		userID, err = consumeLoginToken(ctx, tx, token, PurposeMagicLink)
		return err
	})
	if err != nil {
		return nil, err
	}
	var ok bool
	err = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) = 1 FROM users u
		JOIN members m ON m.id = u.member_id AND m.deleted_at IS NULL
		JOIN user_roles r ON r.user_id = u.id AND r.role = ?
		WHERE u.id = ? AND NOT u.disabled`, string(RoleMember), userID).Scan(&ok)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrInvalidToken
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET last_login_at = UTC_TIMESTAMP() WHERE id = ?", userID); err != nil {
			return err
		}
		return members.WriteAudit(ctx, tx, members.Actor{UserID: userID, Label: auditActor}, ActionLoginVia, 0, "user", userID, nil)
	})
	if err != nil {
		return nil, err
	}
	return s.principal(ctx, userID, LoginMethodMagicLink)
}

// SessionStatus is what /auth/verify needs to know about a session.
type SessionStatus struct {
	UserID    int64
	MemberID  int64        // linked non-archived member, 0 if none
	Expiry    sql.NullTime // that member's Clidive membership expiry
	Member    bool         // the user has the member role
	Committee bool         // a committee role, in a password session
}

// VerifySession resolves a session token for forward_auth in a single
// query. It returns (nil, nil) for an unknown or expired session or a
// disabled user.
func (s *Store) VerifySession(ctx context.Context, token string) (*SessionStatus, error) {
	if token == "" {
		return nil, nil
	}
	var (
		st       SessionStatus
		memberID sql.NullInt64
		method   string
	)
	err := s.DB.QueryRowContext(ctx, `SELECT u.id, m.id, m.membership_expiry, s.auth_method,
			EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = ?),
			EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role IN (?, ?))
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		LEFT JOIN members m ON m.id = u.member_id AND m.deleted_at IS NULL
		WHERE s.token_hash = ? AND s.expires_at > UTC_TIMESTAMP() AND NOT u.disabled`,
		string(RoleMember), string(RoleAdmin), string(RoleMembershipManager), HashToken(token)).
		Scan(&st.UserID, &memberID, &st.Expiry, &method, &st.Member, &st.Committee)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	st.MemberID = memberID.Int64
	st.Committee = st.Committee && method != LoginMethodMagicLink
	return &st, nil
}
