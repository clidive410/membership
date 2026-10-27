package web

import (
	"errors"
	"net/http"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
)

type loginData struct {
	Email string
	Error string
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if p := principalFrom(r.Context()); p.Can(auth.PermViewMembers) {
		http.Redirect(w, r, s.url("/admin/members"), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", "Sign in", loginData{})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	email := members.NormalizeEmail(r.PostForm.Get("email"))
	password := r.PostForm.Get("password")
	now := s.opt.Now()
	keys := []string{"ip:" + clientIP(r), "email:" + email}

	if s.limiter.Blocked(now, keys...) {
		s.opt.Log.Warn("login rate limited", "ip", clientIP(r))
		s.render(w, r, http.StatusTooManyRequests, "login", "Sign in",
			loginData{Email: email, Error: "Too many failed attempts. Please wait 15 minutes and try again."})
		return
	}
	p, err := s.opt.Auth.Authenticate(r.Context(), email, password)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		s.limiter.Fail(now, keys...)
		s.opt.Log.Info("login failed", "ip", clientIP(r))
		s.render(w, r, http.StatusUnauthorized, "login", "Sign in", loginData{Email: email, Error: "Incorrect email or password."})
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.limiter.Reset(keys...)

	// Rotate: drop any session the browser had before.
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_ = s.opt.Auth.DeleteSession(r.Context(), c.Value)
	}
	token, err := s.opt.Auth.CreateSession(r.Context(), p.UserID, auth.LoginMethodPassword, s.opt.SessionTTL)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.setSessionCookie(w, token)
	s.opt.Log.Info("login", "user_id", p.UserID)

	dest := s.url("/admin/members")
	if !p.Can(auth.PermViewMembers) {
		dest = s.url("/login") // signed in, but no admin access (e.g. a member)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.opt.Auth.DeleteSession(r.Context(), c.Value); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.setSessionCookie(w, "")
	http.Redirect(w, r, s.url("/login?flash=loggedout"), http.StatusSeeOther)
}
