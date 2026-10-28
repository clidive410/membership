package web

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
)

// Sign-in link limits.
const (
	loginLinkTTL         = 15 * time.Minute
	linkRequestsPerIP    = 20 // per 15 minutes
	linkRequestsPerEmail = 5  // per 15 minutes
	maxPendingLinks      = 16 // background lookups/sends in flight
	linkSendTimeout      = 45 * time.Second
)

type loginData struct {
	Email     string // password form
	Error     string // password form
	LinkEmail string // sign-in link form
	LinkError string // sign-in link form
	Next      string // carried through both forms, validated when used
}

// safeNext returns where to send the browser after signing in: next, if it
// is an in-app path ("/me", never "//host") or an absolute URL on the
// scheme and host of LOGIN_RETURN_URL; otherwise "".
func (s *Server) safeNext(next string) string {
	if next == "" || len(next) > 2048 || strings.ContainsAny(next, "\\\x00\r\n\t") {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil || u.User != nil || u.Opaque != "" {
		return ""
	}
	if u.Scheme == "" && u.Host == "" {
		if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
			return ""
		}
		return s.url(next)
	}
	ret := s.opt.LoginReturnURL
	if ret != nil && strings.EqualFold(u.Scheme, ret.Scheme) && strings.EqualFold(u.Host, ret.Host) {
		return u.String()
	}
	return ""
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	p := principalFrom(r.Context())
	switch {
	case p.Can(auth.PermViewMembers):
		http.Redirect(w, r, cmp.Or(s.safeNext(next), s.url("/admin/members")), http.StatusSeeOther)
		return
	case p != nil && p.MemberID != 0:
		http.Redirect(w, r, cmp.Or(s.safeNext(next), s.url("/me")), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", "Sign in", loginData{Next: next})
}

// startSession rotates the browser's session: it drops any session the
// browser had and sets a new one.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64, method string, ttl time.Duration) error {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_ = s.opt.Auth.DeleteSession(r.Context(), c.Value)
	}
	token, err := s.opt.Auth.CreateSession(r.Context(), userID, method, ttl)
	if err != nil {
		return err
	}
	s.setSessionCookie(w, token, ttl)
	s.opt.Log.Info("login", "user_id", userID, "method", method)
	return nil
}

// handleLogin is the committee password sign-in.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	email := members.NormalizeEmail(r.PostForm.Get("email"))
	password := r.PostForm.Get("password")
	next := r.PostForm.Get("next")
	now := s.opt.Now()
	keys := []string{"ip:" + clientIP(r), "email:" + email}

	if s.limiter.Blocked(now, keys...) {
		s.opt.Log.Warn("login rate limited", "ip", clientIP(r))
		s.render(w, r, http.StatusTooManyRequests, "login", "Sign in",
			loginData{Email: email, Next: next, Error: "Too many failed attempts. Please wait 15 minutes and try again."})
		return
	}
	p, err := s.opt.Auth.Authenticate(r.Context(), email, password)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		s.limiter.Fail(now, keys...)
		s.opt.Log.Info("login failed", "ip", clientIP(r))
		s.render(w, r, http.StatusUnauthorized, "login", "Sign in", loginData{Email: email, Next: next, Error: "Incorrect email or password."})
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.limiter.Reset(keys...)
	if err := s.startSession(w, r, p.UserID, auth.LoginMethodPassword, s.opt.SessionTTL); err != nil {
		s.serverError(w, r, err)
		return
	}
	dest := s.url("/admin/members")
	switch {
	case !p.Can(auth.PermViewMembers) && p.MemberID != 0:
		dest = s.url("/me")
	case !p.Can(auth.PermViewMembers):
		dest = s.url("/login") // signed in, but nothing to show
	}
	http.Redirect(w, r, cmp.Or(s.safeNext(next), dest), http.StatusSeeOther)
}

// handleLoginLink asks for a sign-in link. The answer is the same whether
// or not the address belongs to a member, and the lookup and email happen
// in the background so that the response time does not tell either.
func (s *Server) handleLoginLink(w http.ResponseWriter, r *http.Request) {
	email := members.NormalizeEmail(r.PostForm.Get("email"))
	next := r.PostForm.Get("next")
	if !members.LooksLikeEmail(email) || len(email) > 254 {
		s.render(w, r, http.StatusBadRequest, "login", "Sign in",
			loginData{LinkEmail: email, Next: next, LinkError: "Please enter a valid email address."})
		return
	}
	now := s.opt.Now()
	ipKey, emailKey := "ip:"+clientIP(r), "email:"+email
	if s.linkByIP.Blocked(now, ipKey) || s.linkByEmail.Blocked(now, emailKey) {
		s.opt.Log.Warn("login link rate limited", "ip", clientIP(r))
		s.render(w, r, http.StatusTooManyRequests, "login", "Sign in",
			loginData{LinkEmail: email, Next: next, LinkError: "Too many sign-in link requests. Please wait 15 minutes and try again."})
		return
	}
	s.linkByIP.Fail(now, ipKey)
	s.linkByEmail.Fail(now, emailKey)
	if s.safeNext(next) == "" {
		next = "" // not worth carrying into the email
	}
	s.sendLoginLink(email, next, requestIDFrom(r.Context()))
	s.render(w, r, http.StatusOK, "linksent", "Check your email", nil)
}

// sendLoginLink looks up the member, issues a token and emails the link,
// in the background. Outcomes are only logged, by user id.
func (s *Server) sendLoginLink(email, next, reqID string) {
	select {
	case s.sendSem <- struct{}{}:
	default:
		s.opt.Log.Warn("login link dropped: too many requests in flight", "request_id", reqID)
		return
	}
	s.bg.Go(func() {
		defer func() { <-s.sendSem }()
		defer func() {
			if v := recover(); v != nil {
				s.opt.Log.Error("panic sending login link", "value", v, "request_id", reqID)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), linkSendTimeout)
		defer cancel()
		link, err := s.opt.Auth.RequestLoginLink(ctx, email, loginLinkTTL)
		switch {
		case errors.Is(err, auth.ErrNoMember):
			s.opt.Log.Info("login link requested for an address that is not a current member", "request_id", reqID)
			return
		case errors.Is(err, auth.ErrLoginNotAllowed):
			s.opt.Log.Warn("login link refused: user disabled or linked to a different member", "request_id", reqID)
			return
		case err != nil:
			s.opt.Log.Error("issuing login link", "err", err, "request_id", reqID)
			return
		}
		msg, err := loginLinkEmail.message(email, loginLinkData{Link: s.loginLinkURL(link.Token, next), Minutes: int(loginLinkTTL.Minutes())})
		if err != nil {
			s.opt.Log.Error("rendering login link email", "err", err, "request_id", reqID)
			return
		}
		if err := s.opt.Mailer.Send(ctx, msg); err != nil {
			s.opt.Log.Error("emailing login link failed", "user_id", link.UserID, "err", err, "request_id", reqID)
			return
		}
		s.opt.Log.Info("login link emailed", "user_id", link.UserID, "request_id", reqID)
	})
}

func (s *Server) loginLinkURL(token, next string) string {
	q := url.Values{"token": {token}}
	if next != "" {
		q.Set("next", next)
	}
	return strings.TrimRight(s.opt.PublicURL.String(), "/") + "/login/verify?" + q.Encode()
}

// handleLoginVerify signs a member in from an emailed link.
func (s *Server) handleLoginVerify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	p, err := s.opt.Auth.LoginWithToken(r.Context(), q.Get("token"))
	if errors.Is(err, auth.ErrInvalidToken) {
		s.opt.Log.Info("login link invalid or expired", "ip", clientIP(r))
		s.render(w, r, http.StatusBadRequest, "login", "Sign in", loginData{Next: q.Get("next"),
			LinkError: "This sign-in link has expired or has already been used. Enter your email address to get a new one."})
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.startSession(w, r, p.UserID, auth.LoginMethodMagicLink, s.opt.MemberSessionTTL); err != nil {
		s.serverError(w, r, err)
		return
	}
	dest := s.url("/me")
	if s.opt.LoginReturnURL != nil {
		dest = s.opt.LoginReturnURL.String()
	}
	http.Redirect(w, r, cmp.Or(s.safeNext(q.Get("next")), dest), http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.opt.Auth.DeleteSession(r.Context(), c.Value); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.setSessionCookie(w, "", 0)
	http.Redirect(w, r, s.url("/login?flash=loggedout"), http.StatusSeeOther)
}
