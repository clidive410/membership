// Command membership runs the Clidive membership portal and its admin tasks.
//
//	membership [serve]                 run the HTTP server (default)
//	membership migrate                 apply database migrations
//	membership migrate-legacy          import from the legacy PHP database
//	membership user <subcommand>       manage login users and roles
//	membership healthcheck             probe /healthz (Docker HEALTHCHECK)
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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/config"
	"github.com/clidive410/membership/internal/db"
	"github.com/clidive410/membership/internal/legacy"
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
  serve            Run the HTTP server (default)
  migrate          Apply database migrations and exit
  migrate-legacy   Import members from the legacy database (-legacy-dsn, -dry-run)
  user             Manage users: set-password, grant, revoke, link, disable, enable, list
  healthcheck      Exit 0 if the local server's /healthz answers 200
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
		return serve(ctx)
	case "migrate":
		return migrateCmd(ctx)
	case "migrate-legacy":
		return migrateLegacy(ctx, args, stdout)
	case "user":
		return userCmd(ctx, args, stdin, stdout)
	case "healthcheck":
		return healthcheck()
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

func serve(ctx context.Context) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
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
	if cfg.AutoMigrate {
		if err := db.Migrate(ctx, conn, log); err != nil {
			return err
		}
	}
	memberStore, authStore := members.NewStore(conn), auth.NewStore(conn)

	srv := web.New(web.Options{
		Members:        memberStore,
		Auth:           authStore,
		Log:            log,
		SessionSecret:  cfg.SessionSecret,
		BasePath:       cfg.BasePath(),
		CookieSecure:   cfg.CookieSecure,
		CookieDomain:   cfg.CookieDomain,
		SessionTTL:     cfg.SessionTTL,
		TrustedProxies: cfg.TrustedProxies,
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
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("stopped")
	return nil
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
		return errors.New("usage: membership user set-password|grant|revoke|link|disable|enable|list [flags]")
	}
	sub, args := args[0], args[1:]
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

func healthcheck() error {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	base := ""
	if c, err := config.Load(os.Getenv); err == nil {
		base = c.BasePath()
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + base + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}
