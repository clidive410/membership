package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
)

// passwordResetTTL is the validity of a "Forgotten your password?" link.
// Invitations (InviteTTL) use the same reset page with a longer validity.
const passwordResetTTL = 30 * time.Minute

// InviteTTL is the validity of an invitation's set-password link.
const InviteTTL = 7 * 24 * time.Hour

type forgotData struct {
	Email string
	Error string
	Sent  bool
}

type resetFormData struct {
	Token   string
	Email   string // the login, for password managers
	Error   string
	Invalid bool // unknown, used or expired link
}

func (s *Server) handleForgotForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "forgot", "Forgotten password", forgotData{})
}

// handleForgot asks for a password-reset link. Like handleLoginLink it
// always gives the same answer and does the work in the background.
func (s *Server) handleForgot(w http.ResponseWriter, r *http.Request) {
	email := members.NormalizeEmail(r.PostForm.Get("email"))
	if !members.LooksLikeEmail(email) || len(email) > 254 {
		s.render(w, r, http.StatusBadRequest, "forgot", "Forgotten password", forgotData{Email: email, Error: "Please enter a valid email address."})
		return
	}
	now := s.opt.Now()
	ipKey, emailKey := "ip:"+clientIP(r), "email:"+email
	if s.resetByIP.Blocked(now, ipKey) || s.resetByEmail.Blocked(now, emailKey) {
		s.opt.Log.Warn("password reset rate limited", "ip", clientIP(r))
		s.render(w, r, http.StatusTooManyRequests, "forgot", "Forgotten password",
			forgotData{Email: email, Error: "Too many password reset requests. Please wait 15 minutes and try again."})
		return
	}
	s.resetByIP.Fail(now, ipKey)
	s.resetByEmail.Fail(now, emailKey)
	reqID := requestIDFrom(r.Context())
	s.background("password reset", reqID, func(ctx context.Context) {
		link, err := s.opt.Auth.RequestPasswordReset(ctx, email, passwordResetTTL)
		switch {
		case errors.Is(err, auth.ErrNoResetUser):
			s.opt.Log.Info("password reset requested for an address without a committee or password login", "request_id", reqID)
			return
		case err != nil:
			s.opt.Log.Error("issuing password reset link", "err", err, "request_id", reqID)
			return
		}
		msg, err := passwordResetEmail.message(email, resetData{Link: PasswordResetURL(s.opt.PublicURL, link.Token), Minutes: int(passwordResetTTL.Minutes())})
		if err != nil {
			s.opt.Log.Error("rendering password reset email", "err", err, "request_id", reqID)
			return
		}
		if err := s.opt.Mailer.Send(ctx, msg); err != nil {
			s.opt.Log.Error("emailing password reset link failed", "user_id", link.UserID, "err", err, "request_id", reqID)
			return
		}
		s.opt.Log.Info("password reset link emailed", "user_id", link.UserID, "request_id", reqID)
	})
	s.render(w, r, http.StatusOK, "forgot", "Check your email", forgotData{Sent: true})
}

func (s *Server) renderResetInvalid(w http.ResponseWriter, r *http.Request) {
	s.opt.Log.Info("password reset link invalid or expired", "ip", clientIP(r))
	s.render(w, r, http.StatusBadRequest, "reset", "Set your password", resetFormData{Invalid: true})
}

// handleResetForm shows the set-password form for a reset or invitation
// link. It checks the token without spending it.
func (s *Server) handleResetForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	token := r.URL.Query().Get("token")
	email, err := s.opt.Auth.PasswordResetEmail(r.Context(), token)
	if errors.Is(err, auth.ErrInvalidToken) {
		s.renderResetInvalid(w, r)
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "reset", "Set your password", resetFormData{Token: token, Email: email})
}

// handleReset sets the password, revokes the user's sessions and signs
// them in with a password session (their roles are unchanged).
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	token := r.PostForm.Get("token")
	pw, pw2 := r.PostForm.Get("password"), r.PostForm.Get("password2")
	email, err := s.opt.Auth.PasswordResetEmail(r.Context(), token)
	if errors.Is(err, auth.ErrInvalidToken) {
		s.renderResetInvalid(w, r)
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	retry := func(msg string) {
		s.render(w, r, http.StatusBadRequest, "reset", "Set your password", resetFormData{Token: token, Email: email, Error: msg})
	}
	if pw != pw2 {
		retry("The two passwords do not match.")
		return
	}
	p, err := s.opt.Auth.ResetPassword(r.Context(), token, pw)
	switch {
	case errors.Is(err, auth.ErrWeakPassword):
		retry("Please choose a password of at least 12 characters.")
		return
	case errors.Is(err, auth.ErrInvalidToken):
		s.renderResetInvalid(w, r)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.opt.Log.Info("password set from a reset link", "user_id", p.UserID)
	if err := s.startSession(w, r, p.UserID, auth.LoginMethodPassword, s.opt.SessionTTL); err != nil {
		s.serverError(w, r, err)
		return
	}
	dest := "/login"
	switch {
	case p.Can(auth.PermViewMembers):
		dest = "/admin/members"
	case p.MemberID != 0:
		dest = "/me"
	}
	http.Redirect(w, r, s.url(dest)+"?flash=password-set", http.StatusSeeOther)
}
