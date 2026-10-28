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

// Audit actions written by password resets, invitations and access changes.
const (
	ActionPasswordResetLink = "auth.password_reset_link" // a reset link was emailed
	ActionPasswordReset     = "auth.password_reset"      // a password was set from a reset or invite link
	ActionInvite            = "auth.invite"              // an invitation (set-password link) was issued
	ActionAccess            = "auth.access"              // portal access level changed
)

// auditResetActor labels self-service password reset audit entries.
const auditResetActor = "password-reset"

// ErrNoResetUser is returned by RequestPasswordReset when the email does not
// belong to an enabled user with a committee role or a password.
var ErrNoResetUser = errors.New("no enabled committee or password user with this email")

// ErrSelfLockout is returned when an admin tries to demote or disable their
// own login.
var ErrSelfLockout = errors.New("you cannot remove your own admin access")

// ErrUserDisabled is returned when inviting a disabled user.
var ErrUserDisabled = errors.New("this login is disabled; give it a role first (or run `membership user enable`)")

// ErrNoRole is returned when inviting a user that has no role at all.
var ErrNoRole = errors.New("this login has no role; choose a role first")

// ErrLinkConflict is returned when the requested member link contradicts an
// existing one (the login is linked to another member, or the member to
// another login). Resolve it with `membership user link`.
var ErrLinkConflict = errors.New("the login and the member record are linked to others")

// Access is the single portal access level an admin chooses for a login.
type Access string

// Access levels, from none to admin. AccessNone disables the login.
const (
	AccessNone              Access = "none"
	AccessMember            Access = "member"
	AccessMembershipManager Access = "membership_manager"
	AccessAdmin             Access = "admin"
)

// AccessLevels lists the levels in the order the UI offers them.
var AccessLevels = []Access{AccessNone, AccessMember, AccessMembershipManager, AccessAdmin}

// ValidAccess reports whether a is a known level.
func ValidAccess(a Access) bool {
	for _, l := range AccessLevels {
		if a == l {
			return true
		}
	}
	return false
}

// AccessOf returns the level that describes a login: none when disabled or
// without roles, otherwise its highest role.
func AccessOf(roles []Role, disabled bool) Access {
	has := func(r Role) bool {
		for _, x := range roles {
			if x == r {
				return true
			}
		}
		return false
	}
	switch {
	case disabled:
		return AccessNone
	case has(RoleAdmin):
		return AccessAdmin
	case has(RoleMembershipManager):
		return AccessMembershipManager
	case has(RoleMember):
		return AccessMember
	}
	return AccessNone
}

// Access returns the user's level.
func (u *User) Access() Access { return AccessOf(u.Roles, u.Disabled) }

// RequestPasswordReset issues a password-reset token (purpose
// password_reset) for the enabled user with this email, if that user has a
// committee role or a password. It writes an audit entry by user id. It
// never grants roles.
func (s *Store) RequestPasswordReset(ctx context.Context, email string, ttl time.Duration) (*LoginLink, error) {
	email = members.NormalizeEmail(email)
	if !members.LooksLikeEmail(email) {
		return nil, ErrNoResetUser
	}
	var link *LoginLink
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var (
			userID   int64
			memberID sql.NullInt64
		)
		err := tx.QueryRowContext(ctx, `SELECT u.id, u.member_id FROM users u
			WHERE u.email = ? AND NOT u.disabled AND (u.password_hash IS NOT NULL
				OR EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role IN (?, ?)))`,
			email, string(RoleAdmin), string(RoleMembershipManager)).Scan(&userID, &memberID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoResetUser
		} else if err != nil {
			return err
		}
		token, err := createLoginToken(ctx, tx, userID, PurposePasswordReset, ttl)
		if err != nil {
			return err
		}
		if err := members.WriteAudit(ctx, tx, members.Actor{UserID: userID, Label: auditResetActor}, ActionPasswordResetLink,
			memberID.Int64, "user", userID, nil); err != nil {
			return err
		}
		link = &LoginLink{Token: token, UserID: userID, MemberID: memberID.Int64}
		return nil
	})
	return link, err
}

// PasswordResetEmail returns the email of the user a valid (unused,
// unexpired, password_reset purpose) token belongs to, without spending it.
// It returns ErrInvalidToken otherwise, or when the user is disabled.
func (s *Store) PasswordResetEmail(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", ErrInvalidToken
	}
	var email string
	err := s.DB.QueryRowContext(ctx, `SELECT u.email FROM login_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ? AND t.purpose = ? AND t.used_at IS NULL AND t.expires_at > UTC_TIMESTAMP() AND NOT u.disabled`,
		HashToken(strings.TrimSpace(token)), PurposePasswordReset).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInvalidToken
	}
	return email, err
}

// ResetPassword spends a password_reset token and sets the user's password.
// In one transaction it stores the argon2id hash, spends every other
// outstanding reset token of the user, revokes all their sessions and
// writes an audit entry. It returns the principal for a new password
// session. It never changes roles and never re-enables a disabled user.
//
// A password shorter than MinPasswordLength is ErrWeakPassword and leaves
// the token unspent.
func (s *Store) ResetPassword(ctx context.Context, token, password string) (*Principal, error) {
	if len(password) < MinPasswordLength {
		return nil, ErrWeakPassword
	}
	if _, err := s.PasswordResetEmail(ctx, token); err != nil {
		return nil, err // cheap check before the costly hash
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	var (
		userID  int64
		refused bool
	)
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		if userID, err = consumeLoginToken(ctx, tx, token, PurposePasswordReset); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ?, last_login_at = UTC_TIMESTAMP() WHERE id = ? AND NOT disabled", hash, userID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			refused = true // disabled meanwhile: keep the token spent
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE login_tokens SET used_at = UTC_TIMESTAMP() WHERE user_id = ? AND purpose = ? AND used_at IS NULL",
			userID, PurposePasswordReset); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID); err != nil {
			return err
		}
		var memberID sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT member_id FROM users WHERE id = ?", userID).Scan(&memberID); err != nil {
			return err
		}
		return members.WriteAudit(ctx, tx, members.Actor{UserID: userID, Label: auditResetActor}, ActionPasswordReset, memberID.Int64, "user", userID, nil)
	})
	if err != nil {
		return nil, err
	}
	if refused {
		return nil, ErrInvalidToken
	}
	return s.principal(ctx, userID, LoginMethodPassword)
}

// lockedUser is a users row read FOR UPDATE.
type lockedUser struct {
	id       int64
	memberID sql.NullInt64
	disabled bool
	created  bool
}

// lockUser reads the user with email FOR UPDATE, creating it (without a
// password) when create is set. It returns (nil, nil) if absent and not
// created.
func lockUser(ctx context.Context, tx *sql.Tx, email string, create bool) (*lockedUser, error) {
	u := &lockedUser{}
	if create {
		res, err := tx.ExecContext(ctx, "INSERT IGNORE INTO users (email) VALUES (?)", email)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		u.created = n == 1
	}
	err := tx.QueryRowContext(ctx, "SELECT id, member_id, disabled FROM users WHERE email = ? FOR UPDATE", email).
		Scan(&u.id, &u.memberID, &u.disabled)
	if errors.Is(err, sql.ErrNoRows) && !create {
		return nil, nil
	}
	return u, err
}

// linkMember links the user to a member: to want if non-zero (which must
// be a current member not linked to another login), otherwise to the one
// current member whose primary email is email, if there is exactly one and
// it is free. It returns the linked member id (0 if none) and, when no
// automatic link was made, a note saying why.
func linkMember(ctx context.Context, tx *sql.Tx, u *lockedUser, want int64, email string) (int64, string, error) {
	otherUser := func(memberID int64) (bool, error) {
		var other int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE member_id = ? AND id <> ?", memberID, u.id).Scan(&other)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	link := func(memberID int64) error {
		_, err := tx.ExecContext(ctx, "UPDATE users SET member_id = ? WHERE id = ?", memberID, u.id)
		u.memberID = sql.NullInt64{Int64: memberID, Valid: true}
		return err
	}
	if want != 0 {
		var current bool
		err := tx.QueryRowContext(ctx, "SELECT deleted_at IS NULL FROM members WHERE id = ?", want).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", fmt.Errorf("member %d: %w", want, members.ErrNotFound)
		} else if err != nil {
			return 0, "", err
		}
		if !current {
			return 0, "", fmt.Errorf("member %d is archived", want)
		}
		if u.memberID.Valid {
			if u.memberID.Int64 != want {
				return 0, "", fmt.Errorf("%w: the login is linked to member %d", ErrLinkConflict, u.memberID.Int64)
			}
			return want, "", nil
		}
		if taken, err := otherUser(want); err != nil {
			return 0, "", err
		} else if taken {
			return 0, "", fmt.Errorf("%w: member %d is linked to another login", ErrLinkConflict, want)
		}
		return want, "", link(want)
	}
	if u.memberID.Valid {
		return u.memberID.Int64, "", nil
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM members WHERE email1 = ? AND deleted_at IS NULL LIMIT 2", email)
	if err != nil {
		return 0, "", err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, "", err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	switch len(ids) {
	case 0:
		return 0, "no current member has this primary email: not linked to a member record", nil
	case 2:
		return 0, "several current members have this primary email: not linked (use -member-id)", nil
	}
	if taken, err := otherUser(ids[0]); err != nil {
		return 0, "", err
	} else if taken {
		return 0, fmt.Sprintf("member %d has this email but is linked to another login: not linked", ids[0]), nil
	}
	return ids[0], "", link(ids[0])
}

// InviteRequest asks for a set-password link.
type InviteRequest struct {
	Email string
	// Role, if set, is granted (added) first. Empty leaves roles alone.
	Role Role
	// MemberID links the login to this member; 0 links automatically when
	// exactly one current member has Email as primary email.
	MemberID int64
	TTL      time.Duration
}

// Invite is the outcome of InviteUser.
type Invite struct {
	Token    string // raw password_reset token for the emailed URL; never log or print it
	UserID   int64
	Email    string
	Created  bool  // the login was created
	MemberID int64 // linked member, 0 if none
	Roles    []Role
	Note     string // why no member was linked automatically, if relevant
}

// InviteUser prepares an invitation, shared by `membership user invite`
// and the admin UI. In one transaction it creates the user if absent (with
// no password), grants req.Role if set, links the member record, issues a
// password_reset token valid for req.TTL and writes an auth.invite audit
// entry. Re-running it for an existing user only ensures the role and
// issues a new token. The caller emails the link.
func (s *Store) InviteUser(ctx context.Context, actor members.Actor, req InviteRequest) (*Invite, error) {
	email := members.NormalizeEmail(req.Email)
	if !members.LooksLikeEmail(email) || len(email) > 254 {
		return nil, fmt.Errorf("invalid email %q", email)
	}
	if req.Role != "" && !ValidRole(req.Role) {
		return nil, fmt.Errorf("unknown role %q", req.Role)
	}
	var inv *Invite
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		u, err := lockUser(ctx, tx, email, true)
		if err != nil {
			return err
		}
		if u.disabled {
			return ErrUserDisabled
		}
		if req.Role != "" {
			if _, err := tx.ExecContext(ctx, "INSERT IGNORE INTO user_roles (user_id, role) VALUES (?, ?)", u.id, string(req.Role)); err != nil {
				return err
			}
		}
		memberID, note, err := linkMember(ctx, tx, u, req.MemberID, email)
		if err != nil {
			return err
		}
		roles, err := rolesIn(ctx, tx, u.id)
		if err != nil {
			return err
		}
		if len(roles) == 0 {
			return ErrNoRole
		}
		token, err := createLoginToken(ctx, tx, u.id, PurposePasswordReset, req.TTL)
		if err != nil {
			return err
		}
		changes := map[string]any{"roles": roles, "created": u.created}
		if err := members.WriteAudit(ctx, tx, actor, ActionInvite, memberID, "user", u.id, changes); err != nil {
			return err
		}
		inv = &Invite{Token: token, UserID: u.id, Email: email, Created: u.created, MemberID: memberID, Roles: roles, Note: note}
		return nil
	})
	return inv, err
}

func rolesIn(ctx context.Context, tx *sql.Tx, userID int64) ([]Role, error) {
	rows, err := tx.QueryContext(ctx, "SELECT role FROM user_roles WHERE user_id = ? ORDER BY role", userID)
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

// AccessRequest changes a login's portal access level.
type AccessRequest struct {
	Email    string
	MemberID int64 // member to link when creating or linking (0: by email)
	Access   Access
}

// AccessResult is the outcome of SetAccess.
type AccessResult struct {
	UserID  int64 // 0 when there was no login and Access was none
	Created bool
	From    Access
	To      Access
	Note    string
}

// SetAccess sets the access level of the login with this email, as chosen
// by an admin in the UI:
//
//   - none: revokes every role, disables the login, revokes its sessions
//     and spends its outstanding links. The row is kept for the audit trail.
//   - member, membership_manager, admin: creates the login if needed (no
//     password), enables it, sets that role, removes the other committee
//     role(s), and keeps (or, for a login linked to a member, adds) the
//     member role so that the person can still sign in with an email link.
//
// An actor cannot change their own login to anything but admin
// (ErrSelfLockout). The change is audit-logged with the old and new level.
func (s *Store) SetAccess(ctx context.Context, actor members.Actor, req AccessRequest) (*AccessResult, error) {
	email := members.NormalizeEmail(req.Email)
	if !members.LooksLikeEmail(email) || len(email) > 254 {
		return nil, fmt.Errorf("invalid email %q", email)
	}
	if !ValidAccess(req.Access) {
		return nil, fmt.Errorf("unknown access level %q", req.Access)
	}
	res := &AccessResult{To: req.Access}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		u, err := lockUser(ctx, tx, email, req.Access != AccessNone)
		if err != nil {
			return err
		}
		if u == nil {
			res.From = AccessNone
			return nil // no login, nothing to revoke
		}
		res.UserID, res.Created = u.id, u.created
		if actor.UserID != 0 && actor.UserID == u.id && req.Access != AccessAdmin {
			return ErrSelfLockout
		}
		roles, err := rolesIn(ctx, tx, u.id)
		if err != nil {
			return err
		}
		res.From = AccessOf(roles, u.disabled)
		exec := func(q string, args ...any) error {
			_, err := tx.ExecContext(ctx, q, args...)
			return err
		}
		if req.Access == AccessNone {
			for _, q := range []string{
				"DELETE FROM user_roles WHERE user_id = ?",
				"UPDATE users SET disabled = TRUE WHERE id = ?",
				"DELETE FROM sessions WHERE user_id = ?",
				"UPDATE login_tokens SET used_at = UTC_TIMESTAMP() WHERE user_id = ? AND used_at IS NULL",
			} {
				if err := exec(q, u.id); err != nil {
					return err
				}
			}
		} else {
			memberID, note, err := linkMember(ctx, tx, u, req.MemberID, email)
			if err != nil {
				return err
			}
			res.Note = note
			// Committee roles other than the chosen one go; the member role
			// is never removed here (it is what email-link sign-in needs).
			for _, r := range []Role{RoleAdmin, RoleMembershipManager} {
				if r != Role(req.Access) {
					if err := exec("DELETE FROM user_roles WHERE user_id = ? AND role = ?", u.id, string(r)); err != nil {
						return err
					}
				}
			}
			grant := []Role{Role(req.Access)}
			if memberID != 0 {
				grant = append(grant, RoleMember)
			}
			for _, r := range grant {
				if err := exec("INSERT IGNORE INTO user_roles (user_id, role) VALUES (?, ?)", u.id, string(r)); err != nil {
					return err
				}
			}
			if u.disabled {
				if err := exec("UPDATE users SET disabled = FALSE WHERE id = ?", u.id); err != nil {
					return err
				}
			}
		}
		var memberID sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT member_id FROM users WHERE id = ?", u.id).Scan(&memberID); err != nil {
			return err
		}
		changes := map[string][2]string{"access": {string(res.From), string(res.To)}}
		return members.WriteAudit(ctx, tx, actor, ActionAccess, memberID.Int64, "user", u.id, changes)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// GetUser returns one login with its roles and linked member's name.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	return s.oneUser(ctx, "u.id = ?", id)
}

// UserForMember returns the login linked to memberID or, failing that, the
// login whose email is email (the member's primary address); nil if none.
func (s *Store) UserForMember(ctx context.Context, memberID int64, email string) (*User, error) {
	u, err := s.oneUser(ctx, "u.member_id = ?", memberID)
	if errors.Is(err, members.ErrNotFound) && email != "" {
		u, err = s.oneUser(ctx, "u.email = ?", members.NormalizeEmail(email))
	}
	if errors.Is(err, members.ErrNotFound) {
		return nil, nil
	}
	return u, err
}

const userColumns = `u.id, u.email, u.member_id, u.password_hash IS NOT NULL, u.disabled, u.last_login_at,
	COALESCE(TRIM(CONCAT(m.first_name, ' ', m.last_name)), '')`

func (s *Store) oneUser(ctx context.Context, where string, arg any) (*User, error) {
	var u User
	err := s.DB.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users u LEFT JOIN members m ON m.id = u.member_id WHERE "+where, arg).
		Scan(&u.ID, &u.Email, &u.MemberID, &u.HasPassword, &u.Disabled, &u.LastLoginAt, &u.MemberName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, members.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if u.Roles, err = s.rolesOf(ctx, u.ID); err != nil {
		return nil, err
	}
	return &u, nil
}
