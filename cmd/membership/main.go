// Command membership runs the Clidive membership portal and its admin tasks.
//
//	membership [serve] [--migrate]     run the HTTP server (default)
//	membership migrate                 apply database migrations
//	membership migrate-legacy          import from the legacy PHP database
//	membership user <subcommand>       manage login users and roles
//	membership healthcheck             probe /healthz (Docker HEALTHCHECK)
//	membership seed-dev                fill an empty scratch database with invented members
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/config"
	"github.com/clidive410/membership/internal/db"
	"github.com/clidive410/membership/internal/devseed"
	"github.com/clidive410/membership/internal/legacy"
	"github.com/clidive410/membership/internal/mail"
	"github.com/clidive410/membership/internal/members"
	"github.com/clidive410/membership/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: membership <command> [flags]

Commands:
  serve            Run the HTTP server (default); --migrate (or MIGRATE_ON_START=true)
                   applies pending migrations first, under a MariaDB named lock
  migrate          Apply database migrations and exit
  migrate-legacy   Import members from the legacy database (-legacy-dsn, -dry-run)
  user             Manage users: invite, set-password, grant, revoke, link, disable, enable, list
  healthcheck      Exit 0 if the local server's /healthz answers 200 (HEALTHCHECK_URL overrides)
  seed-dev         Insert invented members into an EMPTY database (-n 40, -seed 1)
  version          Print the version

Configuration is read from environment variables; see README.md.
`)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(ctx, args)
	case "migrate":
		return migrateCmd(ctx)
	case "migrate-legacy":
		return migrateLegacy(ctx, args, stdout)
	case "user":
		return userCmd(ctx, args, stdin, stdout)
	case "healthcheck":
		return healthcheck(ctx, os.Getenv)
	case "seed-dev":
		return seedDev(ctx, args, stdout)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	}
	usage(stderr)
	return fmt.Errorf("unknown command %q", cmd)
}

func newLogger(level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler = slog.NewJSONHandler(os.Stderr, opts)
	if format == "text" {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h).With("app", "membership")
}

// cliLogger is used by subcommands that only need the database.
func cliLogger() *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(os.Getenv("LOG_LEVEL")))
	return newLogger(lvl, "text")
}

func openAppDB(ctx context.Context, log *slog.Logger) (*members.Store, *auth.Store, func(), error) {
	dsn, err := config.LoadDB(os.Getenv)
	if err != nil {
		return nil, nil, nil, err
	}
	conn, err := db.Open(ctx, dsn)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := db.Migrate(ctx, conn, log); err != nil {
		conn.Close()
		return nil, nil, nil, err
	}
	return members.NewStore(conn), auth.NewStore(conn), func() { conn.Close() }, nil
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	migrate := fs.Bool("migrate", false, "apply pending migrations before listening (also MIGRATE_ON_START=true)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if *migrate {
		cfg.MigrateOnStart = true
	}
	log := newLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := db.Open(ctx, cfg.DBDSN)
	if err != nil {
		return err
	}
	defer conn.Close()
	if cfg.MigrateOnStart {
		if err := db.Migrate(ctx, conn, log); err != nil {
			if errors.Is(err, db.ErrLockTimeout) {
				log.Error("could not obtain the migration lock; another instance is probably still migrating. Exiting without serving.",
					"lock", db.MigrationLock, "timeout", db.DefaultLockTimeout.String())
			} else {
				log.Error("migrations failed; exiting without serving", "err", err)
			}
			return err
		}
	}
	memberStore, authStore := members.NewStore(conn), auth.NewStore(conn)
	mailer, err := newMailer(cfg, log)
	if err != nil {
		return err
	}

	srv := web.New(web.Options{
		Members:          memberStore,
		Auth:             authStore,
		Log:              log,
		SessionSecret:    cfg.SessionSecret,
		BasePath:         cfg.BasePath(),
		CookieSecure:     cfg.CookieSecure,
		CookieDomain:     cfg.CookieDomain,
		SessionTTL:       cfg.SessionTTL,
		MemberSessionTTL: cfg.MemberSessionTTL,
		PublicURL:        cfg.BaseURL,
		LoginReturnURL:   cfg.LoginReturnURL,
		Mailer:           mailer,
		TrustedProxies:   cfg.TrustedProxies,
	})
	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	// Housekeeping: purge expired sessions and login tokens hourly.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if err := authStore.PurgeExpired(ctx); err != nil && ctx.Err() == nil {
				log.Warn("purging expired sessions", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr, "base_url", cfg.BaseURL.String(), "version", version)
		errc <- httpSrv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := srv.Wait(shutdownCtx); err != nil {
		log.Warn("stopped before all sign-in emails were sent", "err", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("stopped")
	return nil
}

// newMailer picks how sign-in links are delivered: SMTP when SMTP_HOST is
// set; otherwise the log in development (LOG_FORMAT=text), and in
// production a sender that fails every send with a clear error.
func newMailer(cfg *config.Config, log *slog.Logger) (mail.Sender, error) {
	from, err := mail.ParseFrom(cfg.MailFrom)
	if err != nil {
		return nil, err
	}
	switch {
	case cfg.SMTPHost != "":
		log.Info("email via SMTP", "host", cfg.SMTPHost, "port", cfg.SMTPPort, "from", from.Address)
		return &mail.SMTP{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword,
			From: from, Log: log}, nil
	case cfg.Development():
		log.Warn("SMTP_HOST is not set: sign-in emails are written to this log instead of being sent (development only)")
		return mail.LogSender{Log: log}, nil
	default:
		log.Error("SMTP_HOST is not set: members cannot be emailed sign-in links")
		return mail.Unconfigured{}, nil
	}
}

func migrateCmd(ctx context.Context) error {
	_, _, closeDB, err := openAppDB(ctx, cliLogger())
	if err != nil {
		return err
	}
	closeDB()
	fmt.Println("migrations applied")
	return nil
}

func migrateLegacy(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("migrate-legacy", flag.ContinueOnError)
	legacyDSN := fs.String("legacy-dsn", "", "DSN of the legacy database (default $LEGACY_DB_DSN)")
	dryRun := fs.Bool("dry-run", false, "run the import in a transaction and roll it back")
	verbose := fs.Bool("v", false, "list every warning (member id and column only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *legacyDSN == "" {
		*legacyDSN = os.Getenv("LEGACY_DB_DSN")
	}
	if *legacyDSN == "" {
		return errors.New("legacy DSN required: -legacy-dsn or LEGACY_DB_DSN")
	}
	log := cliLogger()
	mstore, _, closeDB, err := openAppDB(ctx, log)
	if err != nil {
		return err
	}
	defer closeDB()
	legacyDB, err := legacy.OpenLegacy(ctx, *legacyDSN)
	if err != nil {
		return err
	}
	defer legacyDB.Close()

	res, err := (&legacy.Importer{Legacy: legacyDB, App: mstore.DB, Log: log, DryRun: *dryRun}).Run(ctx)
	if err != nil {
		return err
	}
	renewal := fmt.Sprint(res.RenewalTypes)
	if res.RenewalTypes < 0 {
		renewal = "table not present"
	}
	fmt.Fprintf(out, "membership types: %d\nrenewal types:    %s\nlegacy members:   %d\ncreated:          %d\nupdated:          %d\nunchanged:        %d\n",
		res.Types, renewal, res.Read, res.Created, res.Updated, res.Unchanged)
	fmt.Fprintf(out, "payments created: %d, updated: %d, removed: %d\n", res.PaymentsCreated, res.PaymentsUpdated, res.PaymentsRemoved)
	fmt.Fprintf(out, "duplicate primary emails among current members: %d\nwarnings: %d\n", res.DuplicateEmails, len(res.Warnings))
	if *verbose {
		for _, w := range res.Warnings {
			fmt.Fprintln(out, "  "+w.String())
		}
	}
	if *dryRun {
		fmt.Fprintln(out, "dry run: no changes were committed")
	}
	return nil
}

func userCmd(ctx context.Context, args []string, stdin io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: membership user invite|set-password|grant|revoke|link|disable|enable|list [flags]")
	}
	sub, args := args[0], args[1:]
	if sub == "invite" {
		return inviteCmd(ctx, args, out)
	}
	fs := flag.NewFlagSet("user "+sub, flag.ContinueOnError)
	email := fs.String("email", "", "user email (login identity)")
	role := fs.String("role", "", "role: admin, membership_manager or member")
	memberID := fs.Int64("member-id", 0, "member id to link (0 unlinks)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, store, closeDB, err := openAppDB(ctx, cliLogger())
	if err != nil {
		return err
	}
	defer closeDB()

	if sub == "list" {
		users, err := store.ListUsers(ctx)
		if err != nil {
			return err
		}
		for _, u := range users {
			roles := make([]string, len(u.Roles))
			for i, r := range u.Roles {
				roles[i] = string(r)
			}
			fmt.Fprintf(out, "%-40s roles=%-30s member=%-6v password=%-5v disabled=%v\n", u.Email, strings.Join(roles, ","),
				u.MemberID.Int64, u.HasPassword, u.Disabled)
		}
		return nil
	}
	if *email == "" {
		return errors.New("-email is required")
	}
	switch sub {
	case "set-password":
		id, err := store.EnsureUser(ctx, *email)
		if err != nil {
			return err
		}
		pw, err := readPassword(stdin, out)
		if err != nil {
			return err
		}
		if err := store.SetPassword(ctx, id, pw); err != nil {
			return err
		}
		if *role != "" {
			if err := store.GrantRole(ctx, id, auth.Role(*role)); err != nil {
				return err
			}
		}
		fmt.Fprintln(out, "password set; existing sessions revoked")
		return nil
	case "grant", "revoke":
		if *role == "" {
			return errors.New("-role is required")
		}
		id, err := store.EnsureUser(ctx, *email)
		if err != nil {
			return err
		}
		if sub == "grant" {
			err = store.GrantRole(ctx, id, auth.Role(*role))
		} else {
			err = store.RevokeRole(ctx, id, auth.Role(*role))
		}
		if err == nil {
			fmt.Fprintf(out, "%s %s\n", sub, *role)
		}
		return err
	case "link":
		id, err := store.UserIDByEmail(ctx, *email)
		if err != nil {
			return err
		}
		return store.LinkMember(ctx, id, *memberID)
	case "disable", "enable":
		id, err := store.UserIDByEmail(ctx, *email)
		if err != nil {
			return err
		}
		return store.SetDisabled(ctx, id, sub == "disable")
	}
	return fmt.Errorf("unknown user subcommand %q", sub)
}

// seedDev fills a fresh scratch database with deterministic, invented
// members for UI work. It refuses to run against a database that already
// has members.
func seedDev(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("seed-dev", flag.ContinueOnError)
	n := fs.Int("n", 40, "number of members")
	seed := fs.Uint64("seed", 1, "generator seed (same seed, same members)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	mstore, _, closeDB, err := openAppDB(ctx, cliLogger())
	if err != nil {
		return err
	}
	defer closeDB()
	res, err := devseed.Seed(ctx, mstore, *n, *seed, time.Now().UTC())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "invented members: %d (archived: %d), payments: %d\n", res.Members, res.Archived, res.Payments)
	return nil
}

// healthcheckURL is HEALTHCHECK_URL, or /healthz on the loopback address of
// LISTEN_ADDR under the BASE_URL path prefix.
func healthcheckURL(getenv config.Getenv) (string, error) {
	if u := getenv("HEALTHCHECK_URL"); u != "" {
		return u, nil
	}
	addr := getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("LISTEN_ADDR: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	base := ""
	if b := getenv("BASE_URL"); b != "" {
		if u, err := url.Parse(b); err == nil {
			base = strings.TrimRight(u.Path, "/")
		}
	}
	return "http://" + net.JoinHostPort(host, port) + base + "/healthz", nil
}

// healthcheck is the container HEALTHCHECK: the image has no shell or curl,
// so the binary probes itself. A non-nil error exits 1.
func healthcheck(ctx context.Context, getenv config.Getenv) error {
	u, err := healthcheckURL(getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s returned %d", u, resp.StatusCode)
	}
	return nil
}
