package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/clidive410/membership/internal/config"
	"github.com/clidive410/membership/internal/idp"
)

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, " ") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

const oidcUsage = `usage:
  membership oidc rotate-keys
  membership oidc client create -id ID -name NAME -redirect-uri URI [-redirect-uri URI ...] [-post-logout-uri URI ...]
                                [-scopes "openid email profile offline_access clidive"] [-first-party] [-secret-file PATH]
  membership oidc client list
  membership oidc client rotate-secret -id ID [-secret-file PATH]
  membership oidc client disable -id ID
  membership oidc client ensure [-id ID -name NAME -redirect-uri URI -secret-file PATH -first-party=true]
                                (defaults from OIDC_WORDPRESS_CLIENT_ID, OIDC_WORDPRESS_CLIENT_SECRET(_FILE),
                                 OIDC_WORDPRESS_REDIRECT_URI, OIDC_WORDPRESS_POST_LOGOUT_URI)`

func oidcCmd(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(oidcUsage)
	}
	switch args[0] {
	case "rotate-keys":
		secret, err := config.LoadSessionSecret(os.Getenv)
		if err != nil {
			return err
		}
		mstore, _, closeDB, err := openAppDB(ctx, cliLogger())
		if err != nil {
			return err
		}
		defer closeDB()
		ks, err := idp.NewKeyStore(mstore.DB, secret, nil)
		if err != nil {
			return err
		}
		kid, err := ks.Rotate(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "new signing key %s is active; the previous key stays published for %s\n", kid, idp.RetiredKeyGrace)
		return nil
	case "client":
		if len(args) < 2 {
			return errors.New(oidcUsage)
		}
		return oidcClientCmd(ctx, args[1], args[2:], out)
	}
	return fmt.Errorf("unknown oidc subcommand %q\n%s", args[0], oidcUsage)
}

// readSecretFile returns the trimmed contents of path ("" for no path).
func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func oidcClientCmd(ctx context.Context, sub string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("oidc client "+sub, flag.ContinueOnError)
	id := fs.String("id", "", "client id")
	name := fs.String("name", "", "display name (shown on the consent page)")
	var redirects, logouts stringList
	fs.Var(&redirects, "redirect-uri", "redirect URI, matched exactly (repeatable)")
	fs.Var(&logouts, "post-logout-uri", "post-logout redirect URI (repeatable; default: the origin of each redirect URI)")
	scopes := fs.String("scopes", "", "space-separated scopes the client may request (default: "+strings.Join(idp.DefaultClientScopes, " ")+")")
	firstParty := fs.Bool("first-party", sub == "ensure", "first-party client: no consent screen")
	secretFile := fs.String("secret-file", "", "read the client secret from this file instead of generating one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	mstore, _, closeDB, err := openAppDB(ctx, cliLogger())
	if err != nil {
		return err
	}
	defer closeDB()
	cs := &idp.ClientStore{DB: mstore.DB}
	secret, err := readSecretFile(*secretFile)
	if err != nil {
		return err
	}
	spec := idp.ClientSpec{ID: *id, Name: *name, RedirectURIs: redirects, PostLogoutRedirectURIs: logouts,
		Scopes: strings.Fields(*scopes), FirstParty: *firstParty, Secret: secret}

	switch sub {
	case "create":
		got, err := cs.Create(ctx, cliActor, spec)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "client %s created\n", strings.TrimSpace(*id))
		if secret == "" {
			fmt.Fprintf(out, "client secret (shown once, store it now): %s\n", got)
		}
		return nil
	case "list":
		list, err := cs.List(ctx)
		if err != nil {
			return err
		}
		for _, c := range list {
			state := "enabled"
			if c.Disabled() {
				state = "disabled " + c.DisabledAt.Time.Format("2006-01-02")
			}
			fmt.Fprintf(out, "%s\tname=%q\tfirst_party=%v\t%s\tscopes=%q\n", c.ID, c.Name, c.FirstParty, state, strings.Join(c.Scopes, " "))
			for _, u := range c.RedirectURIs {
				fmt.Fprintf(out, "\tredirect_uri=%s\n", u)
			}
			for _, u := range c.PostLogoutRedirectURIs {
				fmt.Fprintf(out, "\tpost_logout_redirect_uri=%s\n", u)
			}
		}
		if len(list) == 0 {
			fmt.Fprintln(out, "no clients")
		}
		return nil
	case "rotate-secret":
		if *id == "" {
			return errors.New("-id is required")
		}
		got, err := cs.RotateSecret(ctx, cliActor, *id, secret)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "secret of client %s replaced\n", *id)
		if secret == "" {
			fmt.Fprintf(out, "client secret (shown once, store it now): %s\n", got)
		}
		return nil
	case "disable":
		if *id == "" {
			return errors.New("-id is required")
		}
		if err := cs.Disable(ctx, cliActor, *id); err != nil {
			return err
		}
		fmt.Fprintf(out, "client %s disabled; its tokens, codes and consents were deleted\n", *id)
		return nil
	case "ensure":
		if err := fillFromEnv(&spec, os.Getenv); err != nil {
			return err
		}
		res, err := cs.Ensure(ctx, cliActor, spec)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, describeEnsure(spec.ID, res))
		return nil
	}
	return fmt.Errorf("unknown oidc client subcommand %q\n%s", sub, oidcUsage)
}

// fillFromEnv completes an ensure spec from the OIDC_WORDPRESS_* variables.
func fillFromEnv(spec *idp.ClientSpec, getenv config.Getenv) error {
	if spec.ID != "" && spec.Secret != "" && len(spec.RedirectURIs) > 0 {
		return nil
	}
	c, err := config.LoadOIDCClient(getenv)
	if err != nil {
		return err
	}
	if c == nil {
		return errors.New("ensure needs -id, -redirect-uri and -secret-file, or OIDC_WORDPRESS_CLIENT_ID and friends")
	}
	if spec.ID == "" {
		spec.ID = c.ID
		if spec.Name == "" {
			spec.Name = wordPressClientName // as at server start-up
		}
	}
	if spec.Secret == "" {
		spec.Secret = c.Secret
	}
	if len(spec.RedirectURIs) == 0 {
		spec.RedirectURIs = []string{c.RedirectURI}
	}
	if len(spec.PostLogoutRedirectURIs) == 0 && c.PostLogoutRedirectURI != "" {
		spec.PostLogoutRedirectURIs = []string{c.PostLogoutRedirectURI}
	}
	return nil
}

// wordPressClientName is the display name of the client ensured at start-up.
const wordPressClientName = "Clidive website"

func describeEnsure(id string, res *idp.EnsureResult) string {
	var parts []string
	switch {
	case res.Created:
		parts = append(parts, "created")
	case res.Updated:
		parts = append(parts, "updated")
	default:
		parts = append(parts, "unchanged")
	}
	if res.SecretChanged {
		parts = append(parts, "secret replaced")
	}
	if res.Disabled {
		parts = append(parts, "client is DISABLED and was left disabled")
	}
	return "oidc client " + id + ": " + strings.Join(parts, ", ")
}

// ensureWordPressClient runs at server start when OIDC_WORDPRESS_CLIENT_ID
// is set. Failures are logged, never fatal: the portal must keep serving.
func ensureWordPressClient(ctx context.Context, cs *idp.ClientStore, getenv config.Getenv, log *slog.Logger) {
	c, err := config.LoadOIDCClient(getenv)
	if err != nil {
		log.Error("oidc: WordPress client configuration incomplete; not registered", "err", err)
		return
	}
	if c == nil {
		return
	}
	spec := idp.ClientSpec{ID: c.ID, Name: wordPressClientName, RedirectURIs: []string{c.RedirectURI}, FirstParty: true, Secret: c.Secret}
	if c.PostLogoutRedirectURI != "" {
		spec.PostLogoutRedirectURIs = []string{c.PostLogoutRedirectURI}
	}
	res, err := cs.Ensure(ctx, cliActor, spec)
	if err != nil {
		log.Error("oidc: ensuring the WordPress client failed", "client_id", c.ID, "err", err)
		return
	}
	level := slog.LevelInfo
	if res.Disabled {
		level = slog.LevelWarn
	}
	log.Log(ctx, level, describeEnsure(c.ID, res))
}
