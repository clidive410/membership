// Package web serves the admin (committee) UI, the login flow, the health
// endpoint and the /auth/verify endpoint used by Caddy forward_auth.
package web

import (
	"context"
	"embed"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/gorilla/mux"

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
	// TrustedProxies are CIDR prefixes of reverse proxies (e.g. Caddy)
	// whose X-Forwarded-For entries are believed. Empty: the TCP peer is
	// the client.
	TrustedProxies []netip.Prefix
	SessionTTL     time.Duration
	Now            func() time.Time
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

// requestTimeout bounds the time a handler may take; the request context
// (and with it any database query) is cancelled after it. It is shorter
// than the http.Server WriteTimeout so the client still gets an answer.
const requestTimeout = 30 * time.Second

// Handler returns the root HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	r := mux.NewRouter()
	get := func(path string, h http.HandlerFunc) *mux.Route {
		// net/http "GET" patterns also answered HEAD; keep that.
		return r.HandleFunc(path, h).Methods(http.MethodGet, http.MethodHead)
	}

	get("/healthz", s.handleHealth)
	get("/auth/verify", s.handleVerify)
	r.PathPrefix("/static/").Methods(http.MethodGet, http.MethodHead).Handler(staticHandler())

	get("/login", s.handleLoginForm)
	r.HandleFunc("/login", s.handleLogin).Methods(http.MethodPost)
	r.HandleFunc("/logout", s.handleLogout).Methods(http.MethodPost)
	get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.url("/admin/members"), http.StatusSeeOther)
	})

	// Admin UI (committee and membership managers). Every route names the
	// permission it needs (see internal/auth/roles.go); the subrouter's
	// middleware enforces sign-in and that permission.
	admin := r.PathPrefix("/admin").Subrouter()
	perms := map[*mux.Route]auth.Permission{}
	route := func(method, path string, perm auth.Permission, h http.HandlerFunc) {
		methods := []string{method}
		if method == http.MethodGet {
			methods = append(methods, http.MethodHead)
		}
		perms[admin.HandleFunc(path, h).Methods(methods...)] = perm
	}
	route("GET", "/members", auth.PermViewMembers, s.handleMemberList)
	route("GET", "/members/new", auth.PermCreateMembers, s.handleMemberNew) // before /members/{id}
	route("POST", "/members", auth.PermCreateMembers, s.handleMemberCreate)
	route("GET", "/members/{id}", auth.PermViewMembers, s.handleMemberShow)
	route("POST", "/members/{id}", auth.PermViewMembers, s.handleMemberUpdate)
	route("POST", "/members/{id}/archive", auth.PermArchiveMembers, s.handleMemberArchive)
	route("POST", "/members/{id}/restore", auth.PermArchiveMembers, s.handleMemberRestore)
	route("POST", "/members/{id}/payments", auth.PermRecordPayments, s.handlePaymentAdd)
	route("POST", "/members/{id}/payments/{pid}/delete", auth.PermRecordPayments, s.handlePaymentDelete)
	route("GET", "/payments", auth.PermRecordPayments, s.handleBulkPaymentForm)
	route("POST", "/payments", auth.PermRecordPayments, s.handleBulkPayment)
	route("GET", "/export.csv", auth.PermExport, s.handleExportCSV)
	route("GET", "/emails", auth.PermEmailLists, s.handleEmailLists)
	route("GET", "/emails.txt", auth.PermEmailLists, s.handleEmailListText)
	admin.Use(s.requireSignIn, s.requirePermission(perms))

	// Extension point: member self-service routes (e.g. GET/POST /me) will be
	// registered here, guarded by auth.PermEditOwnPersonal and applying only
	// Principal.SelfEditableGroups().

	// The chain below wraps the whole router (not router.Use), so it also
	// applies to 404 and 405 responses. Outermost first.
	return chain(r,
		requestID,
		s.logRequests,
		s.recoverPanics,
		s.clientIP,
		timeout(requestTimeout),
		s.stripBase,
		securityHeaders,
		s.loadSession,
		http.NewCrossOriginProtection().Handler,
		s.checkCSRF,
	)
}

// chain wraps h in middleware, the first being the outermost.
func chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// staticHandler serves the embedded stylesheets. Vendored files are
// immutable for a given version, so browsers may cache them for a day.
func staticHandler() http.Handler {
	fs := http.FileServerFS(staticFS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		fs.ServeHTTP(w, r)
	})
}

// url prefixes an absolute in-app path with the base path.
func (s *Server) url(p string) string { return s.opt.BasePath + p }

func (s *Server) stripBase(next http.Handler) http.Handler {
	base := s.opt.BasePath
	if base == "" {
		return next
	}
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
