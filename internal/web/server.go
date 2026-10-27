// Package web serves the admin (committee) UI, the login flow, the health
// endpoint and the /auth/verify endpoint used by Caddy forward_auth.
package web

import (
	"context"
	"embed"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// MemberStore is the subset of *members.Store used by the handlers.
type MemberStore interface {
	ListTypes(ctx context.Context) ([]members.MembershipType, error)
	ListMembers(ctx context.Context, f members.Filter) ([]members.Member, error)
	GetMember(ctx context.Context, id int64) (*members.Member, error)
	CreateMember(ctx context.Context, a members.Actor, m *members.Member) (int64, error)
	UpdateMember(ctx context.Context, a members.Actor, m *members.Member) error
	ArchiveMember(ctx context.Context, a members.Actor, id int64) error
	RestoreMember(ctx context.Context, a members.Actor, id int64) error
	ListPayments(ctx context.Context, memberID int64) ([]members.Payment, error)
	AddPayment(ctx context.Context, a members.Actor, p *members.Payment) error
	DeletePayment(ctx context.Context, a members.Actor, memberID, paymentID int64) error
	RecordBulkPayment(ctx context.Context, a members.Actor, ids []int64, bp members.BulkPayment) (int, error)
	EmailList(ctx context.Context, list members.MailingList, excludeLapsed bool) ([]string, error)
	ListAudit(ctx context.Context, memberID int64, limit int) ([]members.AuditEntry, error)
	Ping(ctx context.Context) error
}

// AuthStore is the subset of *auth.Store used by the handlers.
type AuthStore interface {
	Authenticate(ctx context.Context, email, password string) (*auth.Principal, error)
	CreateSession(ctx context.Context, userID int64, method string, ttl time.Duration) (string, error)
	LookupSession(ctx context.Context, token string) (*auth.Principal, error)
	DeleteSession(ctx context.Context, token string) error
}

// Options configures a Server.
type Options struct {
	Members       MemberStore
	Auth          AuthStore
	Log           *slog.Logger
	SessionSecret []byte
	BasePath      string // e.g. "/membership", "" for root
	CookieSecure  bool
	CookieDomain  string
	SessionTTL    time.Duration
	Now           func() time.Time
}

// Server holds the HTTP handlers.
type Server struct {
	opt     Options
	csrf    auth.CSRF
	limiter *limiter
	pages   map[string]*pageTemplate
}

// New builds a Server; it panics if the embedded templates do not parse.
func New(opt Options) *Server {
	if opt.Log == nil {
		opt.Log = slog.New(slog.DiscardHandler)
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.SessionTTL == 0 {
		opt.SessionTTL = 12 * time.Hour
	}
	opt.BasePath = strings.TrimRight(opt.BasePath, "/")
	s := &Server{
		opt:     opt,
		csrf:    auth.CSRF{Secret: opt.SessionSecret},
		limiter: newLimiter(10, 15*time.Minute),
	}
	s.pages = mustParseTemplates(opt.BasePath)
	return s
}

// Handler returns the root HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /auth/verify", s.handleVerify)
	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.url("/admin/members"), http.StatusSeeOther)
	})

	// Admin UI (committee and membership managers). Every route names the
	// permission it needs; see internal/auth/roles.go.
	mux.Handle("GET /admin/members", s.require(auth.PermViewMembers, s.handleMemberList))
	mux.Handle("GET /admin/members/new", s.require(auth.PermCreateMembers, s.handleMemberNew))
	mux.Handle("POST /admin/members", s.require(auth.PermCreateMembers, s.handleMemberCreate))
	mux.Handle("GET /admin/members/{id}", s.require(auth.PermViewMembers, s.handleMemberShow))
	mux.Handle("POST /admin/members/{id}", s.require(auth.PermViewMembers, s.handleMemberUpdate))
	mux.Handle("POST /admin/members/{id}/archive", s.require(auth.PermArchiveMembers, s.handleMemberArchive))
	mux.Handle("POST /admin/members/{id}/restore", s.require(auth.PermArchiveMembers, s.handleMemberRestore))
	mux.Handle("POST /admin/members/{id}/payments", s.require(auth.PermRecordPayments, s.handlePaymentAdd))
	mux.Handle("POST /admin/members/{id}/payments/{pid}/delete", s.require(auth.PermRecordPayments, s.handlePaymentDelete))
	mux.Handle("GET /admin/payments", s.require(auth.PermRecordPayments, s.handleBulkPaymentForm))
	mux.Handle("POST /admin/payments", s.require(auth.PermRecordPayments, s.handleBulkPayment))
	mux.Handle("GET /admin/export.csv", s.require(auth.PermExport, s.handleExportCSV))
	mux.Handle("GET /admin/emails", s.require(auth.PermEmailLists, s.handleEmailLists))
	mux.Handle("GET /admin/emails.txt", s.require(auth.PermEmailLists, s.handleEmailListText))

	// Extension point: member self-service routes (e.g. GET/POST /me) will be
	// registered here, guarded by auth.PermEditOwnPersonal and applying only
	// Principal.SelfEditableGroups().

	var h http.Handler = mux
	h = s.checkCSRF(h)
	h = http.NewCrossOriginProtection().Handler(h)
	h = s.loadSession(h)
	h = securityHeaders(h)
	if s.opt.BasePath != "" {
		h = s.stripBase(h)
	}
	h = s.logRequests(h)
	h = s.recoverPanics(h)
	return h
}

// url prefixes an absolute in-app path with the base path.
func (s *Server) url(p string) string { return s.opt.BasePath + p }

func (s *Server) stripBase(next http.Handler) http.Handler {
	base := s.opt.BasePath
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == base {
			http.Redirect(w, r, base+"/", http.StatusMovedPermanently)
			return
		}
		if !strings.HasPrefix(r.URL.Path, base+"/") {
			http.NotFound(w, r)
			return
		}
		http.StripPrefix(base, next).ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.opt.Members.Ping(ctx); err != nil {
		s.opt.Log.Warn("health check failed", "err", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("database unavailable\n"))
		return
	}
	w.Write([]byte("ok\n"))
}

// handleVerify is for Caddy forward_auth: it always answers 200, and adds
// X-Clidive-Member (and X-Clidive-Member-Status) only when the request
// carries a valid session of a user linked to a non-archived member. The
// status is "expired" exactly when the Clidive expiry date is before today,
// otherwise "active"; no other rule is applied.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Cookie")
	p := principalFrom(r.Context())
	if p != nil && p.MemberID != 0 {
		m, err := s.opt.Members.GetMember(r.Context(), p.MemberID)
		if err != nil && err != members.ErrNotFound {
			s.opt.Log.Error("verify: loading member", "err", err)
		}
		if err == nil && !m.Archived() {
			status := "active"
			if m.Expired(s.opt.Now()) {
				status = "expired"
			}
			w.Header().Set("X-Clidive-Member", "1")
			w.Header().Set("X-Clidive-Member-Status", status)
		}
	}
	w.WriteHeader(http.StatusOK)
}
