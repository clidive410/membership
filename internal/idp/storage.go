package idp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
)

// Audit actions written by the provider. Entries identify the user by id
// (actor_user_id, entity_id) and name the client in changes.
const (
	ActionAuthorize = "oidc.authorize" // a user authorized a client (code issued)
	ActionToken     = "oidc.token"     // tokens issued to a client for a user
)

// auditActor labels provider audit entries.
const auditActor = "oidc"

// SubjectPrefix prefixes the user id in the sub claim ("user:42").
const SubjectPrefix = "user:"

// Subject returns the sub claim of a user.
func Subject(userID int64) string { return SubjectPrefix + strconv.FormatInt(userID, 10) }

// ParseSubject is the inverse of Subject.
func ParseSubject(sub string) (int64, bool) {
	s, ok := strings.CutPrefix(sub, SubjectPrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// errNotAllowed: the user may not use relying parties (disabled, no role,
// or linked to an archived member).
var errNotAllowed = errors.New("user not allowed to sign in to relying parties")

var errNotFound = errors.New("not found")

// serverErr hides internal errors from clients (the library would
// otherwise put err.Error() into error_description).
func serverErr(err error) error {
	if err == nil {
		return nil
	}
	var oe *oidc.Error
	if errors.As(err, &oe) {
		return err
	}
	return oidc.ErrServerError().WithParent(err)
}

// Session context: the web layer puts the portal session of the browser
// on the request context before handing /oauth/authorize and
// /oauth/end_session to the library.
type sessionKey struct{}

type browserSession struct {
	hash string
	end  func(context.Context) error
}

// WithSession returns ctx carrying the portal session (its token hash) and
// a function that ends it (portal logout), used by end_session.
func WithSession(ctx context.Context, sessionHash string, end func(context.Context) error) context.Context {
	return context.WithValue(ctx, sessionKey{}, &browserSession{hash: sessionHash, end: end})
}

func sessionFrom(ctx context.Context) *browserSession {
	s, _ := ctx.Value(sessionKey{}).(*browserSession)
	return s
}

// AuthRequest is a stored authorization request (op.AuthRequest).
type AuthRequest struct {
	ID                  string
	ClientID            string
	UserID              int64 // 0 until bound to a signed-in user
	SessionHash         string
	AuthMethod          string
	AuthTime            time.Time
	RedirectURI         string
	Scopes              []string
	State               string
	Nonce               string
	CodeChallenge       string
	CodeChallengeMethod string
	ResponseMode        string
	Prompt              []string
	IsDone              bool
}

var _ op.AuthRequest = (*AuthRequest)(nil)

func (a *AuthRequest) GetID() string          { return a.ID }
func (a *AuthRequest) GetACR() string         { return "" }
func (a *AuthRequest) GetAMR() []string       { return amr(a.AuthMethod) }
func (a *AuthRequest) GetAudience() []string  { return []string{a.ClientID} }
func (a *AuthRequest) GetAuthTime() time.Time { return a.AuthTime }
func (a *AuthRequest) GetClientID() string    { return a.ClientID }
func (a *AuthRequest) GetCodeChallenge() *oidc.CodeChallenge {
	return &oidc.CodeChallenge{Challenge: a.CodeChallenge, Method: oidc.CodeChallengeMethod(a.CodeChallengeMethod)}
}
func (a *AuthRequest) GetNonce() string                   { return a.Nonce }
func (a *AuthRequest) GetRedirectURI() string             { return a.RedirectURI }
func (a *AuthRequest) GetResponseType() oidc.ResponseType { return oidc.ResponseTypeCode }
func (a *AuthRequest) GetResponseMode() oidc.ResponseMode { return oidc.ResponseMode(a.ResponseMode) }
func (a *AuthRequest) GetScopes() []string                { return a.Scopes }
func (a *AuthRequest) GetState() string                   { return a.State }
func (a *AuthRequest) Done() bool                         { return a.IsDone }
func (a *AuthRequest) GetSubject() string {
	if a.UserID == 0 {
		return ""
	}
	return Subject(a.UserID)
}

// HasPrompt reports whether the request carried prompt=p.
func (a *AuthRequest) HasPrompt(p string) bool { return slices.Contains(a.Prompt, p) }

func amr(method string) []string {
	if method == auth.LoginMethodPassword {
		return []string{"pwd"}
	}
	return nil
}

// refreshRequest is the grant behind a refresh token (op.RefreshTokenRequest).
type refreshRequest struct {
	grantID     string
	clientID    string
	userID      int64
	sessionHash string
	authMethod  string
	authTime    time.Time
	scopes      []string
}

func (r *refreshRequest) GetAMR() []string                 { return amr(r.authMethod) }
func (r *refreshRequest) GetAudience() []string            { return []string{r.clientID} }
func (r *refreshRequest) GetAuthTime() time.Time           { return r.authTime }
func (r *refreshRequest) GetClientID() string              { return r.clientID }
func (r *refreshRequest) GetScopes() []string              { return r.scopes }
func (r *refreshRequest) GetSubject() string               { return Subject(r.userID) }
func (r *refreshRequest) SetCurrentScopes(scopes []string) { r.scopes = scopes }

// Storage implements op.Storage on MariaDB.
type Storage struct {
	db       *sql.DB
	clients  *ClientStore
	keys     *KeyStore
	auth     *auth.Store
	now      func() time.Time
	loginURL func(id string) string
	log      *slog.Logger
}

var (
	_ op.Storage                        = (*Storage)(nil)
	_ op.CanSetUserinfoFromRequest      = (*Storage)(nil)
	_ op.CanTerminateSessionFromRequest = (*Storage)(nil)
)

func (s *Storage) utcNow() time.Time { return s.now().UTC().Truncate(time.Second) }

// --- clients ---

// GetClientByClientID returns an enabled client.
func (s *Storage) GetClientByClientID(ctx context.Context, id string) (op.Client, error) {
	info, err := s.clients.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if info.Disabled() {
		return nil, ErrClientNotFound
	}
	return &client{info: info, loginURL: s.loginURL}, nil
}

// AuthorizeClientIDSecret checks a client secret (argon2id).
func (s *Storage) AuthorizeClientIDSecret(ctx context.Context, id, secret string) error {
	if _, err := s.clients.authenticate(ctx, id, secret); err != nil {
		if errors.Is(err, ErrClientNotFound) {
			return oidc.ErrInvalidClient().WithDescription("invalid client credentials")
		}
		return serverErr(err)
	}
	return nil
}

// --- authorization requests and codes ---

const authReqColumns = `id, client_id, COALESCE(user_id, 0), COALESCE(session_hash, ''), auth_method, auth_time, redirect_uri, scopes,
	state, nonce, code_challenge, code_challenge_method, response_mode, prompt, done`

func scanAuthRequest(sc rowScanner) (*AuthRequest, error) {
	var (
		a              AuthRequest
		authTime       sql.NullTime
		scopes, prompt string
	)
	if err := sc.Scan(&a.ID, &a.ClientID, &a.UserID, &a.SessionHash, &a.AuthMethod, &authTime, &a.RedirectURI, &scopes,
		&a.State, &a.Nonce, &a.CodeChallenge, &a.CodeChallengeMethod, &a.ResponseMode, &prompt, &a.IsDone); err != nil {
		return nil, err
	}
	a.AuthTime = authTime.Time
	a.Scopes = strings.Fields(scopes)
	a.Prompt = strings.Fields(prompt)
	return &a, nil
}

// CreateAuthRequest stores a validated authorization request, bound to the
// browser's portal session when there is one.
func (s *Storage) CreateAuthRequest(ctx context.Context, req *oidc.AuthRequest, _ string) (op.AuthRequest, error) {
	a := &AuthRequest{
		ID: auth.NewToken(), ClientID: req.ClientID, RedirectURI: req.RedirectURI, Scopes: req.Scopes,
		State: req.State, Nonce: req.Nonce, CodeChallenge: req.CodeChallenge, CodeChallengeMethod: string(req.CodeChallengeMethod),
		ResponseMode: string(req.ResponseMode), Prompt: req.Prompt,
	}
	var (
		userID   sql.NullInt64
		sessHash sql.NullString
		authTime sql.NullTime
	)
	if bs := sessionFrom(ctx); bs != nil && bs.hash != "" {
		var created time.Time
		err := s.db.QueryRowContext(ctx, `SELECT s.user_id, s.auth_method, s.created_at FROM sessions s JOIN users u ON u.id = s.user_id
			WHERE s.token_hash = ? AND s.expires_at > UTC_TIMESTAMP() AND NOT u.disabled`, bs.hash).Scan(&a.UserID, &a.AuthMethod, &created)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, serverErr(err)
		}
		if err == nil {
			a.SessionHash, a.AuthTime = bs.hash, created.UTC()
			userID = sql.NullInt64{Int64: a.UserID, Valid: true}
			sessHash = sql.NullString{String: bs.hash, Valid: true}
			authTime = sql.NullTime{Time: created, Valid: true}
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO oidc_authorizations (id, client_id, user_id, session_hash, auth_method, auth_time, redirect_uri,
		scopes, state, nonce, code_challenge, code_challenge_method, response_mode, prompt, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ClientID, userID, sessHash, a.AuthMethod, authTime, a.RedirectURI, strings.Join(a.Scopes, " "), a.State, a.Nonce,
		a.CodeChallenge, a.CodeChallengeMethod, a.ResponseMode, strings.Join(a.Prompt, " "), s.utcNow().Add(AuthRequestTTL))
	if err != nil {
		return nil, serverErr(err)
	}
	return a, nil
}

// AuthRequestByID returns an unexpired request that has no code yet.
func (s *Storage) AuthRequestByID(ctx context.Context, id string) (op.AuthRequest, error) {
	a, err := s.AuthRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// AuthRequest returns an unexpired authorization request that has no code
// yet (the consent step works on these).
func (s *Storage) AuthRequest(ctx context.Context, id string) (*AuthRequest, error) {
	a, err := scanAuthRequest(s.db.QueryRowContext(ctx, "SELECT "+authReqColumns+
		" FROM oidc_authorizations WHERE id = ? AND code_hash IS NULL AND expires_at > ?", id, s.utcNow()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return a, serverErr(err)
}

// Approve marks a request bound to userID as authorized and audits it.
func (s *Storage) Approve(ctx context.Context, a *AuthRequest) error {
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE oidc_authorizations SET done = TRUE WHERE id = ? AND user_id = ? AND code_hash IS NULL", a.ID, a.UserID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errNotFound
		}
		return members.WriteAudit(ctx, tx, members.Actor{UserID: a.UserID, Label: auditActor}, ActionAuthorize, 0, "user", a.UserID,
			map[string]any{"client_id": a.ClientID, "scopes": strings.Join(a.Scopes, " ")})
	})
	if err != nil {
		return err
	}
	a.IsDone = true
	return nil
}

// SaveAuthCode stores the hash of the code issued for an approved request;
// the code is valid for CodeTTL.
func (s *Storage) SaveAuthCode(ctx context.Context, id, code string) error {
	now := s.utcNow()
	res, err := s.db.ExecContext(ctx, `UPDATE oidc_authorizations SET code_hash = ?, code_expires_at = ?, expires_at = ?
		WHERE id = ? AND done AND user_id IS NOT NULL AND code_hash IS NULL`, auth.HashToken(code), now.Add(CodeTTL), now.Add(AuthRequestTTL), id)
	if err != nil {
		return serverErr(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return oidc.ErrServerError().WithDescription("authorization request not approved")
	}
	return nil
}

// AuthRequestByCode spends a code: it succeeds once, within CodeTTL. A
// second use revokes every token issued from the code (RFC 6749 4.1.2).
func (s *Storage) AuthRequestByCode(ctx context.Context, code string) (op.AuthRequest, error) {
	var a *AuthRequest
	reused := false
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		var (
			expires sql.NullTime
			used    sql.NullTime
		)
		row := tx.QueryRowContext(ctx, "SELECT "+authReqColumns+", code_expires_at, code_used_at FROM oidc_authorizations WHERE code_hash = ? FOR UPDATE",
			auth.HashToken(code))
		var err error
		a, err = scanAuthRequest(scanFunc(func(dest ...any) error { return row.Scan(append(dest, &expires, &used)...) }))
		if err != nil {
			return err
		}
		if used.Valid {
			reused = true
			_, err := tx.ExecContext(ctx, "DELETE FROM oidc_tokens WHERE grant_id = ?", a.ID)
			return err
		}
		if !expires.Valid || !expires.Time.After(s.utcNow()) {
			return errNotFound
		}
		_, err = tx.ExecContext(ctx, "UPDATE oidc_authorizations SET code_used_at = ? WHERE id = ?", s.utcNow(), a.ID)
		return err
	})
	switch {
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, errNotFound):
		return nil, errNotFound
	case err != nil:
		return nil, serverErr(err)
	case reused:
		s.log.Warn("oidc: authorization code reused; tokens of the grant revoked", "client_id", a.ClientID, "user_id", a.UserID)
		return nil, errNotFound
	}
	return a, nil
}

// DeleteAuthRequest removes a request without a code (a request with a
// code is kept until it expires, to detect code reuse).
func (s *Storage) DeleteAuthRequest(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM oidc_authorizations WHERE id = ? AND code_hash IS NULL", id)
	return serverErr(err)
}

// --- consents ---

// HasConsent reports whether the user already consented to the client
// for all the scopes.
func (s *Storage) HasConsent(ctx context.Context, userID int64, clientID string, scopes []string) (bool, error) {
	var granted string
	err := s.db.QueryRowContext(ctx, "SELECT scopes FROM oidc_consents WHERE user_id = ? AND client_id = ?", userID, clientID).Scan(&granted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	have := strings.Fields(granted)
	for _, sc := range scopes {
		if !slices.Contains(have, sc) {
			return false, nil
		}
	}
	return true, nil
}

// SaveConsent remembers the user's consent to the client for scopes.
func (s *Storage) SaveConsent(ctx context.Context, userID int64, clientID string, scopes []string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO oidc_consents (user_id, client_id, scopes) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE scopes = VALUES(scopes)`, userID, clientID, strings.Join(scopes, " "))
	return err
}

// --- users ---

// Allowed returns the principal a session of userID started with method
// carries, or errNotAllowed when the user is disabled, has no role for
// that kind of session, or is linked to an archived member.
func (s *Storage) Allowed(ctx context.Context, userID int64, method string) (*auth.Principal, error) {
	p, err := s.auth.PrincipalFor(ctx, userID, method)
	if err != nil {
		return nil, err
	}
	if p == nil || len(p.Roles) == 0 {
		return nil, errNotAllowed
	}
	var archived bool
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) > 0 FROM users u JOIN members m ON m.id = u.member_id
		WHERE u.id = ? AND m.deleted_at IS NOT NULL`, userID).Scan(&archived); err != nil {
		return nil, err
	}
	if archived {
		return nil, errNotAllowed
	}
	return p, nil
}

// IsNotAllowed reports whether err means the user may not sign in.
func IsNotAllowed(err error) bool { return errors.Is(err, errNotAllowed) }

// --- tokens ---

type tokenRow struct {
	grantID, clientID string
	userID            int64
	sessionHash       string
	authMethod        string
	authTime          time.Time
	scopes            []string
}

func (s *Storage) insertToken(ctx context.Context, tx *sql.Tx, hash, kind string, t *tokenRow, expires time.Time) error {
	sess := sql.NullString{String: t.sessionHash, Valid: t.sessionHash != ""}
	authTime := sql.NullTime{Time: t.authTime, Valid: !t.authTime.IsZero()}
	_, err := tx.ExecContext(ctx, `INSERT INTO oidc_tokens (token_hash, kind, grant_id, client_id, user_id, session_hash, auth_method, auth_time, scopes, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, hash, kind, t.grantID, t.clientID, t.userID, sess, t.authMethod, authTime,
		strings.Join(t.scopes, " "), s.utcNow(), expires)
	return err
}

func (s *Storage) grantOf(req op.TokenRequest) (*tokenRow, string, error) {
	switch r := req.(type) {
	case *AuthRequest:
		return &tokenRow{grantID: r.ID, clientID: r.ClientID, userID: r.UserID, sessionHash: r.SessionHash, authMethod: r.AuthMethod,
			authTime: r.AuthTime, scopes: r.Scopes}, string(oidc.GrantTypeCode), nil
	case *refreshRequest:
		return &tokenRow{grantID: r.grantID, clientID: r.clientID, userID: r.userID, sessionHash: r.sessionHash, authMethod: r.authMethod,
			authTime: r.authTime, scopes: r.scopes}, string(oidc.GrantTypeRefreshToken), nil
	}
	return nil, "", oidc.ErrUnsupportedGrantType()
}

func (s *Storage) auditToken(ctx context.Context, tx *sql.Tx, t *tokenRow, grant string, refresh bool) error {
	return members.WriteAudit(ctx, tx, members.Actor{UserID: t.userID, Label: auditActor}, ActionToken, 0, "user", t.userID,
		map[string]any{"client_id": t.clientID, "grant_type": grant, "refresh_token": refresh})
}

// CreateAccessToken issues an access token (no refresh token). The
// returned id is encrypted into the opaque token by the library; only its
// hash is stored.
func (s *Storage) CreateAccessToken(ctx context.Context, req op.TokenRequest) (string, time.Time, error) {
	t, grant, err := s.grantOf(req)
	if err != nil {
		return "", time.Time{}, err
	}
	if _, err := s.Allowed(ctx, t.userID, t.authMethod); err != nil {
		return "", time.Time{}, notAllowedErr(err)
	}
	id := auth.NewToken()
	exp := s.utcNow().Add(AccessTokenTTL)
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := s.insertToken(ctx, tx, auth.HashToken(id), "access", t, exp); err != nil {
			return err
		}
		return s.auditToken(ctx, tx, t, grant, false)
	})
	if err != nil {
		return "", time.Time{}, serverErr(err)
	}
	return id, exp, nil
}

func notAllowedErr(err error) error {
	if IsNotAllowed(err) {
		return oidc.ErrInvalidGrant().WithDescription("the user may no longer sign in")
	}
	return serverErr(err)
}

// CreateAccessAndRefreshTokens issues an access token and a refresh token.
// For a refresh grant the presented refresh token is rotated: it is
// marked used in the same transaction, and presenting it again revokes the
// grant (see TokenRequestByRefreshToken).
func (s *Storage) CreateAccessAndRefreshTokens(ctx context.Context, req op.TokenRequest, current string) (string, string, time.Time, error) {
	t, grant, err := s.grantOf(req)
	if err != nil {
		return "", "", time.Time{}, err
	}
	if _, err := s.Allowed(ctx, t.userID, t.authMethod); err != nil {
		return "", "", time.Time{}, notAllowedErr(err)
	}
	id, refresh := auth.NewToken(), auth.NewToken()
	now := s.utcNow()
	exp := now.Add(AccessTokenTTL)
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		if current != "" {
			res, err := tx.ExecContext(ctx, `UPDATE oidc_tokens SET rotated_at = ? WHERE token_hash = ? AND kind = 'refresh'
				AND grant_id = ? AND rotated_at IS NULL AND expires_at > ?`, now, auth.HashToken(current), t.grantID, now)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return oidc.ErrInvalidGrant().WithDescription("refresh token already used")
			}
		}
		if err := s.insertToken(ctx, tx, auth.HashToken(id), "access", t, exp); err != nil {
			return err
		}
		if err := s.insertToken(ctx, tx, auth.HashToken(refresh), "refresh", t, now.Add(RefreshTokenTTL)); err != nil {
			return err
		}
		return s.auditToken(ctx, tx, t, grant, true)
	})
	if err != nil {
		return "", "", time.Time{}, serverErr(err)
	}
	return id, refresh, exp, nil
}

// TokenRequestByRefreshToken returns the grant of a valid refresh token.
// A refresh token that was already rotated revokes the whole grant
// (refresh token reuse detection).
func (s *Storage) TokenRequestByRefreshToken(ctx context.Context, token string) (op.RefreshTokenRequest, error) {
	var (
		r        refreshRequest
		sess     sql.NullString
		authTime sql.NullTime
		scopes   string
		expires  time.Time
		rotated  sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `SELECT grant_id, client_id, user_id, session_hash, auth_method, auth_time, scopes, expires_at, rotated_at
		FROM oidc_tokens WHERE token_hash = ? AND kind = 'refresh'`, auth.HashToken(token)).
		Scan(&r.grantID, &r.clientID, &r.userID, &sess, &r.authMethod, &authTime, &scopes, &expires, &rotated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, op.ErrInvalidRefreshToken
	} else if err != nil {
		return nil, serverErr(err)
	}
	if rotated.Valid {
		s.log.Warn("oidc: rotated refresh token reused; grant revoked", "client_id", r.clientID, "user_id", r.userID)
		if _, err := s.db.ExecContext(ctx, "DELETE FROM oidc_tokens WHERE grant_id = ?", r.grantID); err != nil {
			return nil, serverErr(err)
		}
		return nil, op.ErrInvalidRefreshToken
	}
	if !expires.After(s.utcNow()) {
		return nil, op.ErrInvalidRefreshToken
	}
	if _, err := s.Allowed(ctx, r.userID, r.authMethod); err != nil {
		return nil, notAllowedErr(err)
	}
	r.sessionHash, r.authTime, r.scopes = sess.String, authTime.Time, strings.Fields(scopes)
	return &r, nil
}

// refreshIDPrefix marks the token id GetRefreshTokenInfo hands to RevokeToken.
const refreshIDPrefix = "refresh:"

// GetRefreshTokenInfo identifies a refresh token for revocation.
func (s *Storage) GetRefreshTokenInfo(ctx context.Context, clientID, token string) (string, string, error) {
	h := auth.HashToken(token)
	var userID int64
	err := s.db.QueryRowContext(ctx, "SELECT user_id FROM oidc_tokens WHERE token_hash = ? AND kind = 'refresh' AND client_id = ?", h, clientID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", op.ErrInvalidRefreshToken
	} else if err != nil {
		return "", "", err
	}
	return Subject(userID), refreshIDPrefix + h, nil
}

// RevokeToken revokes a refresh token (and with it its whole grant) or a
// single access token, if it belongs to clientID. Unknown tokens are not
// an error (RFC 7009).
func (s *Storage) RevokeToken(ctx context.Context, tokenOrID, _ string, clientID string) *oidc.Error {
	var err error
	if h, ok := strings.CutPrefix(tokenOrID, refreshIDPrefix); ok {
		var grant string
		err = s.db.QueryRowContext(ctx, "SELECT grant_id FROM oidc_tokens WHERE token_hash = ? AND client_id = ?", h, clientID).Scan(&grant)
		if err == nil {
			_, err = s.db.ExecContext(ctx, "DELETE FROM oidc_tokens WHERE grant_id = ? AND client_id = ?", grant, clientID)
		}
	} else {
		_, err = s.db.ExecContext(ctx, "DELETE FROM oidc_tokens WHERE token_hash = ? AND kind = 'access' AND client_id = ?", auth.HashToken(tokenOrID), clientID)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.Error("oidc: revoking token", "err", err)
		return oidc.ErrServerError()
	}
	return nil
}

// TerminateSession revokes the user's tokens for the client.
func (s *Storage) TerminateSession(ctx context.Context, userID, clientID string) error {
	_, err := s.TerminateSessionFromRequest(ctx, &op.EndSessionRequest{UserID: userID, ClientID: clientID})
	return err
}

// TerminateSessionFromRequest handles end_session: it revokes the user's
// tokens for the client and, when the browser's portal session belongs to
// the user named by the id_token_hint, signs the portal out too.
func (s *Storage) TerminateSessionFromRequest(ctx context.Context, req *op.EndSessionRequest) (string, error) {
	uid, ok := ParseSubject(req.UserID)
	if !ok {
		return req.RedirectURI, nil
	}
	if req.ClientID != "" {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM oidc_tokens WHERE user_id = ? AND client_id = ?", uid, req.ClientID); err != nil {
			return "", serverErr(err)
		}
	}
	if bs := sessionFrom(ctx); bs != nil && bs.hash != "" && bs.end != nil {
		var owner int64
		err := s.db.QueryRowContext(ctx, "SELECT user_id FROM sessions WHERE token_hash = ?", bs.hash).Scan(&owner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", serverErr(err)
		}
		if owner == uid {
			if err := bs.end(ctx); err != nil {
				return "", serverErr(err)
			}
		}
	}
	return req.RedirectURI, nil
}

// --- keys ---

func (s *Storage) SigningKey(ctx context.Context) (op.SigningKey, error) {
	k, err := s.keys.Active(ctx)
	if err != nil {
		return nil, serverErr(err)
	}
	return k, nil
}

func (s *Storage) SignatureAlgorithms(context.Context) ([]jose.SignatureAlgorithm, error) {
	return []jose.SignatureAlgorithm{jose.RS256}, nil
}

func (s *Storage) KeySet(ctx context.Context) ([]op.Key, error) {
	keys, err := s.keys.Published(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]op.Key, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out, nil
}

// --- claims ---

// SetUserinfoFromScopes is deprecated in the library; claims are set by
// SetUserinfoFromRequest and SetUserinfoFromToken.
func (s *Storage) SetUserinfoFromScopes(context.Context, *oidc.UserInfo, string, string, []string) error {
	return nil
}

// SetUserinfoFromRequest fills the ID token claims.
func (s *Storage) SetUserinfoFromRequest(ctx context.Context, info *oidc.UserInfo, req op.IDTokenRequest, scopes []string) error {
	var (
		userID int64
		method string
	)
	switch r := req.(type) {
	case *AuthRequest:
		userID, method = r.UserID, r.AuthMethod
	case *refreshRequest:
		userID, method = r.userID, r.authMethod
	default:
		return oidc.ErrServerError()
	}
	return notAllowedErr(s.setClaims(ctx, info, userID, method, scopes))
}

// SetUserinfoFromToken fills the userinfo response for an access token.
func (s *Storage) SetUserinfoFromToken(ctx context.Context, info *oidc.UserInfo, tokenID, _, _ string) error {
	var (
		userID  int64
		method  string
		scopes  string
		expires time.Time
	)
	err := s.db.QueryRowContext(ctx, "SELECT user_id, auth_method, scopes, expires_at FROM oidc_tokens WHERE token_hash = ? AND kind = 'access'",
		auth.HashToken(tokenID)).Scan(&userID, &method, &scopes, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !expires.After(s.utcNow())) {
		return oidc.ErrAccessDenied().WithDescription("access token invalid or expired")
	} else if err != nil {
		return serverErr(err)
	}
	if err := s.setClaims(ctx, info, userID, method, strings.Fields(scopes)); err != nil {
		if IsNotAllowed(err) {
			return oidc.ErrAccessDenied().WithDescription("the user may no longer sign in")
		}
		return serverErr(err)
	}
	return nil
}

// Membership statuses in the clidive scope.
const (
	StatusActive  = "active"
	StatusExpired = "expired"
	StatusNone    = "none"
)

func (s *Storage) setClaims(ctx context.Context, info *oidc.UserInfo, userID int64, method string, scopes []string) error {
	p, err := s.Allowed(ctx, userID, method)
	if err != nil {
		return err
	}
	var (
		memberID          sql.NullInt64
		first, last, bsac sql.NullString
		expiry            sql.NullTime
		typeName          sql.NullString
	)
	err = s.db.QueryRowContext(ctx, `SELECT m.id, m.first_name, m.last_name, m.bsac_member_id, m.membership_expiry, t.name
		FROM users u LEFT JOIN members m ON m.id = u.member_id AND m.deleted_at IS NULL
		LEFT JOIN membership_types t ON t.id = m.membership_type_id WHERE u.id = ?`, userID).
		Scan(&memberID, &first, &last, &bsac, &expiry, &typeName)
	if err != nil {
		return err
	}
	info.Subject = Subject(userID)
	if slices.Contains(scopes, oidc.ScopeEmail) {
		info.Email, info.EmailVerified = p.Email, true
	}
	if slices.Contains(scopes, oidc.ScopeProfile) {
		info.GivenName, info.FamilyName = strings.TrimSpace(first.String), strings.TrimSpace(last.String)
		info.Name = strings.TrimSpace(info.GivenName + " " + info.FamilyName)
		if info.Name == "" {
			info.Name = p.Email
		}
	}
	if slices.Contains(scopes, ScopeClidive) {
		status := StatusNone
		var (
			mid    any
			mtype  any
			mexp   any
			roles  = make([]string, 0, len(p.Roles))
			member = members.Member{MembershipExpiry: expiry}
		)
		if memberID.Valid {
			mid = memberID.Int64
			status = StatusActive
			if member.Expired(s.now().UTC()) {
				status = StatusExpired
			}
			if typeName.Valid {
				mtype = typeName.String
			}
			if expiry.Valid {
				mexp = expiry.Time.Format(time.DateOnly)
			}
		}
		for _, r := range p.Roles {
			roles = append(roles, string(r))
		}
		info.AppendClaims("member_id", mid)
		info.AppendClaims("membership_status", status)
		info.AppendClaims("membership_type", mtype)
		info.AppendClaims("membership_expiry", mexp)
		info.AppendClaims("roles", roles)
		if b := strings.TrimSpace(bsac.String); memberID.Valid && b != "" {
			info.AppendClaims("bsac_member_id", b)
		}
	}
	return nil
}

// --- unsupported grants and introspection ---

func (s *Storage) SetIntrospectionFromToken(context.Context, *oidc.IntrospectionResponse, string, string, string) error {
	return oidc.ErrRequestNotSupported()
}

func (s *Storage) GetPrivateClaimsFromScopes(context.Context, string, string, []string) (map[string]any, error) {
	return nil, nil
}

func (s *Storage) GetKeyByIDAndClientID(context.Context, string, string) (*jose.JSONWebKey, error) {
	return nil, fmt.Errorf("private_key_jwt is not supported")
}

func (s *Storage) ValidateJWTProfileScopes(context.Context, string, []string) ([]string, error) {
	return nil, fmt.Errorf("the JWT profile grant is not supported")
}

// Health pings the database.
func (s *Storage) Health(ctx context.Context) error { return s.db.PingContext(ctx) }

// Purge deletes expired authorization requests and tokens.
func (s *Storage) Purge(ctx context.Context) error {
	now := s.utcNow()
	if _, err := s.db.ExecContext(ctx, "DELETE FROM oidc_authorizations WHERE expires_at <= ?", now); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM oidc_tokens WHERE expires_at <= ?", now)
	return err
}

// SessionAuthTime returns when the portal session with this token hash
// signed in (sessions.created_at), for max_age.
func (s *Storage) SessionAuthTime(ctx context.Context, sessionHash string) (time.Time, error) {
	var t time.Time
	err := s.db.QueryRowContext(ctx, "SELECT created_at FROM sessions WHERE token_hash = ?", sessionHash).Scan(&t)
	return t, err
}
