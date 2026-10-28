# clidive410/membership

The Clidive (BSAC branch) membership portal, written in Go. It replaces the
membership admin pages of the old PHP site (`old.clidive.org/admin/membership`).

Infrastructure lives in [`clidive410/infra`](https://github.com/clidive410/infra).

- Committee admin UI: member list with search and type/status filters,
  create/edit/archive/restore, payment history, bulk payment management, CSV
  export and mailing-list email lists.
- Roles: `admin` (webmaster/committee), `membership_manager`, and `member`.
- Members sign in with a single-use link emailed to their primary address
  and see their own record at `/me` (read-only). The committee signs in with
  a password (argon2id), set from an emailed invitation or reset link, so
  nobody has to share a password. Admins manage logins and roles in the UI
  (`/admin/users`) or with `membership user`. Server-side sessions, CSRF
  protection and an audit log of every member, payment and access change
  and of sign-ins.
- `GET /auth/verify` lets Caddy `forward_auth` tell the WordPress site
  whether the visitor is a signed-in club member, and whether their
  membership has expired.
- `migrate-legacy` imports the legacy `membership` tables idempotently.
- `seed-dev` fills an empty scratch database with invented members for UI work.

`net/http` with [`gorilla/mux`](https://github.com/gorilla/mux) for routing
(method matching, an `/admin` subrouter whose middleware enforces sign-in and
each route's permission), `html/template`, `database/sql` with
`go-sql-driver/mysql`, and `golang.org/x/crypto` (argon2) plus
`golang.org/x/term` (password prompt). The UI uses the
[Tabler](https://tabler.io) CSS kit, vendored and embedded in the binary
(see `internal/web/static/vendor/README.md`), themed to the club's colours
(black, logo yellow `#fff200`, Open Sans Condensed headings) in
`internal/web/static/style.css`. No JavaScript, no npm, no build step.

## Layout

```
cmd/membership/         CLI entrypoint: serve, migrate, migrate-legacy, user, healthcheck
internal/config/        environment configuration (NAME or NAME_FILE)
internal/db/            connection setup and embedded SQL migrations (migrations/*.sql)
internal/members/       domain model, field groups, diffs, MariaDB repository, audit log
internal/auth/          roles and permissions, argon2id, sessions, CSRF, login tokens
internal/legacy/        legacy field mapping and idempotent importer
internal/export/        CSV export and email-list formatting
internal/mail/          SMTP sending (STARTTLS) of the sign-in emails
internal/web/           handlers, middleware, page and email templates, Tabler CSS and overrides (embedded)
internal/devseed/       invented demo members for `seed-dev`
internal/integration/   MariaDB integration tests (opt-in)
```

## Running locally

You need Go (see `go.mod`) and Docker for the development database.

```sh
docker compose up -d db                    # MariaDB 11 on 127.0.0.1:3307 (dev only)

export DB_DSN='membership:devpassword@tcp(127.0.0.1:3307)/membership'
export SESSION_SECRET='dev-only-session-secret-change-me-0123456789'
export BASE_URL='http://localhost:8080' COOKIE_SECURE=false LOG_FORMAT=text

go run ./cmd/membership migrate
go run ./cmd/membership user set-password -email you@example.org -role admin   # prompts for a password
# or: go run ./cmd/membership user invite -email you@example.org -role admin   # the set-password link is logged
go run ./cmd/membership serve            # add --migrate to apply migrations first
```

Then open <http://localhost:8080/> and sign in. Without `SMTP_HOST`, and
with `LOG_FORMAT=text`, emails (sign-in, password reset and invitation
links included) are written to the log (of the server, or of the `user
invite` command) instead of being sent.

For UI work without real member data, use a scratch database and invented
members (`seed-dev` refuses to run on a database that already has members):

```sh
docker compose exec db mariadb -uroot -pdevroot -e "CREATE DATABASE membership_ui CHARACTER SET utf8mb4; GRANT ALL ON membership_ui.* TO 'membership'@'%'"
export DB_DSN='membership:devpassword@tcp(127.0.0.1:3307)/membership_ui'
go run ./cmd/membership migrate
go run ./cmd/membership seed-dev -n 40      # deterministic; -seed picks another set
```

To run the whole stack in containers: `docker compose --profile app up --build`
(the compose app runs `serve --migrate`).

### Tests

```sh
go vet ./...
go test ./...
```

Unit and handler tests need no database. The MariaDB integration tests
(importer idempotency, repositories, sessions) run only when
`MEMBERSHIP_TEST_MYSQL_DSN` points at a server. They create scratch databases
named `legacy_test_<n>` and `membership_test_<n>`, fill them with invented data,
and drop them afterwards:

```sh
MEMBERSHIP_TEST_MYSQL_DSN='root:devroot@tcp(127.0.0.1:3307)/' go test ./internal/integration
```

CI runs them against a MariaDB 11 service container.

## Configuration

Every variable can also be given as a file by setting `NAME_FILE` instead of
`NAME` (for example `DB_DSN_FILE=/run/secrets/membership_db_dsn`). The file's
contents are trimmed of surrounding whitespace. If both are set, `NAME` wins.

| Variable         | Default                  | Description |
|------------------|--------------------------|-------------|
| `DB_DSN`         | (required)               | [go-sql-driver DSN](https://github.com/go-sql-driver/mysql#dsn-data-source-name) for the app database, e.g. `membership:pw@tcp(clidive_db:3306)/membership`. `parseTime`, UTC and `utf8mb4` are forced. |
| `SESSION_SECRET` | (required)               | At least 32 random characters. Keys the CSRF tokens. |
| `BASE_URL`       | `http://localhost:8080`  | Public URL, e.g. `https://members.clidive.org`. A path such as `https://clidive.org/membership` mounts the app under that prefix. |
| `COOKIE_SECURE`  | `true` if BASE_URL is https | Mark cookies `Secure`. |
| `COOKIE_DOMAIN`  | (host-only)              | Session cookie domain, e.g. `clidive.org`, so the cookie also reaches the WordPress site for `/auth/verify`. |
| `LISTEN_ADDR`    | `:8080`                  | HTTP listen address. The healthcheck probes its port on 127.0.0.1. |
| `SESSION_TTL`    | `12h`                    | Lifetime of password (committee) sessions (Go duration). |
| `MEMBER_SESSION_TTL` | `720h` (30 days)     | Lifetime of sessions started from an emailed sign-in link. |
| `LOGIN_RETURN_URL` | (none)                 | Where members land after signing in when the link has no `next`, e.g. `https://www.clidive.org/`. Without it: `/me`. Absolute `next` URLs are only followed on this URL's scheme and host. |
| `MIGRATE_ON_START` | `false`                | `true` makes `serve` apply pending migrations before listening, like `serve --migrate`. `AUTO_MIGRATE` is a deprecated alias, used only when `MIGRATE_ON_START` is unset. |
| `HEALTHCHECK_URL` | (derived)               | Only for `membership healthcheck`: URL to probe instead of `http://127.0.0.1:<port of LISTEN_ADDR><BASE_URL path>/healthz`. |
| `LOG_LEVEL`      | `info`                   | `debug`, `info`, `warn`, `error`. |
| `LOG_FORMAT`     | `json`                   | `json` or `text` (log/slog). |
| `MAIL_FROM`      | `membership@clidive.org` | Sender of all emails (sign-in, password reset, invitation): `addr@example.org` or `Name <addr@example.org>`. |
| `SMTP_HOST`      | (none)                   | SMTP submission server. Unset: emails are logged when `LOG_FORMAT=text` (development); otherwise every send fails with an error saying SMTP is not configured (and `serve` logs an error at start). |
| `SMTP_PORT`      | `587`                    | `587` uses STARTTLS, `465` implicit TLS. TLS is required (except to a loopback server); the app never falls back to clear text. |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | (none)   | SMTP AUTH PLAIN credentials (set both or neither). Usually given as `SMTP_USERNAME_FILE`/`SMTP_PASSWORD_FILE`. |
| `LEGACY_DB_DSN`  |                          | Default for `migrate-legacy -legacy-dsn`. |
| `TRUSTED_PROXIES`| (none)                   | Comma-separated CIDRs (or addresses) of reverse proxies, e.g. the Swarm overlay network `10.0.0.0/8`. Only requests whose TCP peer is in this list have `X-Forwarded-For` read, from the right, to find the client address used for login rate limiting. Without it the TCP peer is the client. |

Logs never include query strings or member data: only method, path, status,
user ids and counts. Every response carries an `X-Request-Id` that also
appears in its log line. Handlers time out after 30 seconds (503).

## Users and roles

Login identities (`users`) are keyed by lower-cased email and kept separate
from member records. A user may link to a member row (`users.member_id`);
admins need not have one.

| Role                 | Can |
|----------------------|-----|
| `admin`              | Everything: create, edit all fields, archive/restore, payments, CSV export, email lists, history, and portal access (logins and roles, `/admin/users`). |
| `membership_manager` | View members; edit membership fields (Clidive type/expiry/join date, BSAC details, query/notes); record and delete payments; bulk payments; view history. Personal and club fields are shown read-only. |
| `member`             | Sign in with an emailed link and view their own record at `/me` (read-only). No access to `/admin` (403). Granted automatically on the first link request. Self-service editing (own contact, address and emergency details only) is planned. |

Every route declares its permission (`internal/web/server.go`). Field-level
rules use field groups (`internal/members/fields.go`): the server applies only
the groups the user may edit, whatever the form contains.

```sh
membership user invite -email someone@example.org -role admin|membership_manager [-member-id 123]  # emails a set-password link
membership user set-password -email someone@example.org [-role admin]  # create/reset; reads stdin if not a terminal
membership user grant  -email someone@example.org -role membership_manager
membership user revoke -email someone@example.org -role admin
membership user link   -email someone@example.org -member-id 123
membership user disable|enable -email someone@example.org
membership user list
```

In the container: `docker exec -it <container> membership user invite -email ... -role admin`
(the container has the `SMTP_*`, `BASE_URL` and `SESSION_SECRET` settings
`user invite` needs).

### Managing access

Admins see **Portal access** in the sidebar (`/admin/users`, admin only):
every login with its linked member, roles, enabled/disabled, whether a
password is set and the last sign-in. Each member page also has a
**Portal access** card for the login linked to the member, or else the
login with the member's primary email (read-only for membership managers).

Both offer one role select and two buttons:

- **Save** sets the access level:
  - `none` revokes every role, **disables** the login, ends all its
    sessions and spends its outstanding links. The row is kept (audit
    history); choosing a role again re-enables it.
  - `member`, `membership_manager`, `admin` create the login if there is
    none (without a password), enable it, set that role and remove the
    other committee role. The `member` role is kept, and added for a login
    linked to a member, so that committee members can still use email
    sign-in links.
- **Send invite / reset email** applies the chosen role, then emails a
  link to set a password (valid 7 days), as `membership user invite` does.
  It is also how an admin resends a reset link for someone else.

"Invite by email" at the top of `/admin/users` is for committee members
without a member record (email + role). An admin cannot demote or disable
their own login (the page says so); another admin must do it. Every
change writes an `auth.access` or `auth.invite` audit entry, shown in the
member's history when the login is linked to a member. All forms are
CSRF-protected. The UI and the CLI call the same functions
(`auth.Store.SetAccess`, `auth.Store.InviteUser`).

### Onboarding a new committee admin

1. If they are a member, open their member page, choose `admin` (or
   `membership_manager`) under **Portal access** and press **Send invite /
   reset email**. Otherwise use **Invite by email** on `/admin/users`, or
   on the server:
   `docker exec <container> membership user invite -email new.admin@example.org -role admin`
   (`-member-id N` links a member record whose primary email differs).
2. They receive "You've been given access to the Clidive members portal"
   with a link to `/password/reset?token=...`, valid 7 days, choose a
   password (at least 12 characters) and are signed in.
3. Later they sign in at `/login` with "Committee: sign in with a
   password"; "Forgotten your password?" sends a new link.
4. To remove access, set their role to `none` (or
   `membership user revoke` / `disable`).

**Amazon SES sandbox:** while the SES account is in the sandbox, SES only
delivers to verified addresses. Verify the new admin's address in SES
(or move the account out of the sandbox) before inviting them, otherwise
the send fails (`user invite` exits with an error; the UI says the email
could not be sent) and you have to re-send it.

## Importing the legacy data

```sh
membership migrate-legacy -legacy-dsn 'user:pw@tcp(legacy-host:3306)/ironmonger' [-dry-run] [-v]
```

- Reads `membership`, `membershipclidivetype` and, if present,
  `membershiprenewaltype`. All records are imported, archived ones too
  (`DeletedOn` becomes `deleted_at`). Member ids are kept.
- **Not imported:** `email`, `surname`, `password` (plain text in the legacy
  database), `passhint`, `admin`, `session`, `LastLogin`. These columns are
  never selected. Legacy logins are not migrated; committee users are
  invited with `membership user invite` (or the admin UI).
- `last_paid` becomes one payment row (`source = legacy`, no amount).
- Zero dates (`0000-00-00`) become NULL. The free-text `ClidiveJoinDate` is
  parsed (`Y-M-D`, `DD/MM/YY`, `DD.MM.YY`). Anything it cannot parse is kept in
  `join_date_raw` and shown on the member page. Emails are trimmed and
  lower-cased.
- Text is repaired on import: the legacy tables are declared latin1 but
  hold UTF-8 bytes, which arrive double-encoded (`Ã©` for `é`, `â€“` for an
  en dash). A value whose characters all map back to latin1/cp1252 bytes
  that form valid UTF-8 is decoded again; correct text is left alone.
- Idempotent: members are matched on `legacy_member_id` and updated in
  place, and the legacy payment row follows `last_paid`. Each run is one
  transaction and records `legacy.import` audit entries. Re-running after
  go-live **overwrites edits made in the new app** to imported members, so run
  it only during cutover.
- The output contains only counts. With `-v` it also lists warnings, each
  giving a member id and column, never the value.

## Member sign-in (email links)

Members have no password. They sign in with a link sent to their primary
email (Email 1), which identifies them:

1. `GET /login` shows "Members: email me a sign-in link" (an email field)
   and, collapsed below it, the committee password form.
2. `POST /login/link` (CSRF-protected) always answers with the same page:
   "If that address belongs to a current member, we've emailed a link". The
   lookup and the email happen in the background, so neither the page nor
   the response time reveals whether the address is a member's. Requests are
   limited to 20 per IP address and 5 per email address per 15 minutes (429
   beyond that).
   If the trimmed, lower-cased address is the primary email of exactly one
   non-archived member, the app, in one transaction: finds or creates the
   `users` row with that email, links it to the member, grants the `member`
   role, stores the SHA-256 of a random 32-byte token in `login_tokens`
   (valid 15 minutes, single use) and writes an `auth.login_link` audit
   entry (by user id only). It then emails
   `BASE_URL/login/verify?token=...` (plus `&next=...` when the form carried
   a safe `next`). Members whose membership has expired can still sign in;
   archived members cannot. No link is issued for a disabled user, or when
   the login identity is linked inconsistently (the user is linked to a
   different member, or the member to a different user): such requests are
   logged as warnings for the committee to resolve with `membership user`.
3. `GET /login/verify?token=...` spends the token (even if the sign-in is
   then refused), checks that the user is enabled, still has the `member`
   role and is linked to a non-archived member, writes an `auth.login` audit
   entry, starts a session of `MEMBER_SESSION_TTL` (30 days) and sets the
   `clidive_session` cookie (with `COOKIE_DOMAIN` when set). It redirects to
   `next` if that is an in-app path (such as `/me`) or a URL on the scheme
   and host of `LOGIN_RETURN_URL`; otherwise to `LOGIN_RETURN_URL`, or
   `/me`. An unknown, used or expired link shows the sign-in page with
   "This sign-in link has expired or has already been used". The route
   answers `GET` only, so a `HEAD` request cannot spend a link.

A session started from a link carries the `member` role only, even for a
committee user who is also a member: the admin UI always needs a password
sign-in. Members see a top bar with only "My membership" and "Sign out";
`/admin` pages answer 403. `POST /logout` ends any session.

`GET /me` shows the member's own record read-only: name, membership type,
Clidive expiry (with an "expired" badge when in the past), join date, last
payment, BSAC number and renewal date, contact details, emergency contact,
qualifications, mailing-list flags and payment history (date, amount,
method; payment notes and the committee's query/notes fields are not
shown). Changes are requested by email to membership@clidive.org.

Logs carry user ids and request ids only: never the email address or the
token. The SMTP sender logs the recipient's domain.

## Password reset (committee logins)

1. Under the committee password form, `GET /login` links to "Forgotten
   your password?" (`GET /password/forgot`, an email field).
2. `POST /password/forgot` (CSRF-protected) always answers "If that
   address belongs to a committee login or a login with a password, we've
   emailed a link". As for sign-in links, the lookup and email happen in
   the background, and requests are limited to 20 per IP address and 5
   per email address per 15 minutes (429 beyond; separate counters from
   the sign-in link). A link is issued only for an **enabled** user with a
   committee role (`admin`, `membership_manager`) or a password: a
   single-use token with purpose `password_reset`, valid 30 minutes, only
   its SHA-256 stored in `login_tokens`, and an `auth.password_reset_link`
   audit entry. The email links to `BASE_URL/password/reset?token=...`.
3. `GET /password/reset?token=...` shows "Set your password" (new password
   twice) without spending the token; an unknown, used, expired or
   wrong-purpose token shows "This link has expired or has already been
   used". Invitations use the same page.
4. `POST /password/reset` requires matching passwords of at least 12
   characters (otherwise the form is shown again and the token stays
   valid). It then, in one transaction, spends the token (purpose
   checked), stores the argon2id hash, spends any other outstanding reset
   or invitation tokens of the user, deletes all their sessions and writes
   an `auth.password_reset` audit entry. It starts a password session
   (`SESSION_TTL`, full roles) and redirects to `/admin/members` (`/me`
   for a member without committee roles).

Login tokens carry a purpose (`magic_link` or `password_reset`) that is
checked when they are spent, so a sign-in link cannot reset a password
and a reset link cannot start a member session. The reset flow never
grants roles and never re-enables a disabled login: it only changes the
password of an existing user.

## `/auth/verify` (Caddy forward_auth)

`GET /auth/verify` (or `HEAD`) always returns `200` with an empty body. It
never sets a cookie and never redirects. It reads the `clidive_session`
cookie and answers with one database query and no template, and sends
`Cache-Control: no-store` and `Vary: Cookie`.

The headers below are set only when the cookie is a valid, unexpired
session of an enabled user who either

- has the `member` role and is linked to a non-archived member, or
- has a committee role (`admin`, `membership_manager`) and signed in with a
  password.

| Header | Value |
|--------|-------|
| `X-Clidive-Member` | `1` |
| `X-Clidive-Member-Status` | `expired` when the linked member's Clidive expiry date is before today, otherwise `active`. Committee users without a linked member row are `active`. No other rule is applied (membership type, including Lapsed, is not considered). |
| `X-Clidive-Member-Id` | The linked member's id (`members.id`). Absent for committee users without a member row. |

In every other case (no cookie, unknown or expired session, disabled user,
archived member, a user without either role) none of the three headers is
set.

With `COOKIE_DOMAIN=clidive.org` the session cookie set by
`members.clidive.org` is also sent to `www.clidive.org`. Caddy calls
`/auth/verify` for each WordPress request and copies the headers onto it:

```caddyfile
www.clidive.org {
	# route keeps this order; outside it Caddy runs forward_auth before
	# request_header.
	route {
		# Never trust these headers from the client: strip them before
		# forward_auth adds the real ones.
		request_header -X-Clidive-Member
		request_header -X-Clidive-Member-Status
		request_header -X-Clidive-Member-Id
		forward_auth membership:8080 {
			uri /auth/verify
			copy_headers X-Clidive-Member X-Clidive-Member-Status X-Clidive-Member-Id
		}
		reverse_proxy wordpress:80
	}
}
```

Notes for the WordPress side:

- **The site must strip incoming `X-Clidive-*` headers before
  `forward_auth` copies them** (the `request_header` lines above). Otherwise
  a visitor could send `X-Clidive-Member: 1` themselves and pass as a member
  whenever the app adds no header.
- In PHP the headers appear as `$_SERVER['HTTP_X_CLIDIVE_MEMBER']`,
  `HTTP_X_CLIDIVE_MEMBER_STATUS` and `HTTP_X_CLIDIVE_MEMBER_ID`. Treat a
  missing header as "not a signed-in member".
- Any page cache must vary on these headers or bypass the cache for
  requests that carry the `clidive_session` cookie.
- To send a visitor to sign in and back, link to
  `https://members.clidive.org/login?next=<url-encoded page URL>` with
  `LOGIN_RETURN_URL=https://www.clidive.org/` set on the app.
- If the app is mounted under a path (`BASE_URL` with a path), prefix the
  `uri` with it.

## Deployment

The service runs as a Docker Swarm service defined in the
[`clidive410/infra`](https://github.com/clidive410/infra) repository
(`terraform/blog`). This repository only builds the image and tells infra
which one to run.

### Release flow

1. A push to `main` runs CI: gofmt, vet, `go test -race` (with the MariaDB
   integration tests), then builds the image and pushes
   `ghcr.io/clidive410/membership:main` and `:sha-<short sha>`.
2. The `deploy` job clones `clidive410/infra` with the `INFRA_PUSH_TOKEN`
   secret and writes the pushed digest to
   `terraform/blog/membership-image.json`:
   ```json
   {
     "image": "ghcr.io/clidive410/membership@sha256:<digest>"
   }
   ```
   It commits that as `membership: deploy <short sha>` (author
   `clidive-ci <ci@clidive.org>`) straight to infra `main`, and skips the
   commit when the digest is already pinned. Runs on `main` are serialised,
   so pins arrive in commit order.
3. Infra's Terraform reads the file and rolls the Swarm service to the
   pinned digest.

Pull requests run the tests and build the image without pushing it.

**Owner setup:** add a repository secret `INFRA_PUSH_TOKEN` to
`clidive410/membership`: a fine-grained personal access token, resource owner
`clidive410`, repository access only `clidive410/infra`, repository
permission **Contents: Read and write** (Metadata: Read is added
automatically). If infra `main` is branch-protected, the token's owner must
be allowed to push to it directly. Without the secret the deploy job fails
with an explicit error. Images in ghcr.io must be pullable by the Swarm
nodes (public package or registry credentials on the nodes).

### Image

- Static binary on `gcr.io/distroless/static-debian12:nonroot`: no shell, no
  curl, runs as `nonroot` (65532).
- The binary is **`/usr/local/bin/membership`**. It is the entrypoint and
  the default command is `serve`.
- Health check, for the Dockerfile `HEALTHCHECK` (built in) and the Swarm
  service definition:
  ```json
  ["CMD", "/usr/local/bin/membership", "healthcheck"]
  ```
  It GETs `http://127.0.0.1:<port of LISTEN_ADDR><BASE_URL path>/healthz`
  (or `HEALTHCHECK_URL`) with a 3 second timeout and exits 0 on `200`, 1
  otherwise. `/healthz` returns 503 when the database is unreachable. It needs
  no secrets.

### Migrations on start

Run the service as `serve --migrate` (or set `MIGRATE_ON_START=true`). Before
listening, each task takes the MariaDB named lock `membership_migrate`
(`GET_LOCK`, 60 second timeout), applies pending embedded migrations, and
releases the lock. Tasks that start together therefore migrate one at a time;
the second finds nothing to do. A task that cannot get the lock within 60
seconds logs `could not obtain the migration lock` and exits non-zero, and
Swarm restarts it. A failed migration also exits non-zero without serving.
`membership migrate` still applies migrations as a one-off command.

**Rule: every migration must be backward compatible with the previous
release (expand/contract).** With a start-first rolling update the old task
keeps serving on the new schema while the new task migrates and starts. So
add nullable or defaulted columns and new tables first; ship code that stops
using a column; drop or rename it in a later release. Never rename or drop
something the running release still reads, and never add a `NOT NULL` column
without a default.

### Environment

| Variable | Notes for the Swarm service |
|----------|-----------------------------|
| `LISTEN_ADDR` | `:8080` (default). |
| `DB_DSN_FILE` | Swarm secret, e.g. `/run/secrets/membership_db_dsn`, holding `membership:<pw>@tcp(clidive_db:3306)/membership`. Or `DB_DSN`. |
| `SESSION_SECRET_FILE` | Swarm secret, at least 32 random characters. Or `SESSION_SECRET`. |
| `BASE_URL` | `https://members.clidive.org` |
| `COOKIE_SECURE` | Defaults to `true` for an https `BASE_URL`. |
| `COOKIE_DOMAIN` | `clidive.org`, so `/auth/verify` sees the session on the WordPress site. |
| `SMTP_HOST`, `SMTP_PORT` | SMTP submission server and port (`587`, STARTTLS). |
| `SMTP_USERNAME_FILE`, `SMTP_PASSWORD_FILE` | Swarm secrets with the SMTP credentials. Or `SMTP_USERNAME`/`SMTP_PASSWORD`. |
| `MAIL_FROM` | e.g. `Clidive Membership <membership@clidive.org>`; the SMTP account must be allowed to send as it. |
| `LOGIN_RETURN_URL` | Optional, e.g. `https://www.clidive.org/`. |
| `TRUSTED_PROXIES` | CIDR of the overlay network Caddy reaches the service on, e.g. `10.0.0.0/8`. |
| `LOG_FORMAT` | `json` (default). |
| `LOG_LEVEL` | `info` (default). |
| `MIGRATE_ON_START` | `true`, or run the command `serve --migrate`. |

All variables are described under [Configuration](#configuration).

### Routing, database, shutdown

- **Routing:** Caddy reverse-proxies `https://members.clidive.org` to the
  service on port 8080. Hosting under a path prefix instead also works: set
  `BASE_URL` with the path and use Caddy `handle /membership/*` (not
  `handle_path`), because the app strips the prefix itself.
- **Database:** a dedicated database and user on the shared `clidive_db`
  MariaDB 11 service, for example:
  ```sql
  CREATE DATABASE membership CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
  CREATE USER 'membership'@'%' IDENTIFIED BY '...';
  GRANT ALL PRIVILEGES ON membership.* TO 'membership'@'%';
  ```
- **Shutdown:** SIGTERM drains in-flight requests for up to 15 seconds; give
  the service a `stop_grace_period` of at least 20 seconds.

## Behaviour compared with the legacy app

Replicated:

- The "current" list shows non-archived members, including the Lapsed type.
  "All" also includes archived members. Columns are sortable, default sort is
  last name.
- Archive is a soft delete. Archived members drop off mailing lists and stay
  viewable.
- Payment management works as before: filter by membership type (Standing
  Order by default), all members ticked by default, one date applied to every
  selected member.
- Full export of current or all members, sorted by last name, with the legacy
  column order.
- The "all members" email list follows the `email-all-auto` view rules: email N
  is used only when "use email N" is ticked, the address is longer than 5
  characters, and the member is not archived. There is one list per legacy flag
  (committee, instructors, boat handlers, lifeguards, drivers).
- New members default to "use email 1 for mailing list".

Changed on purpose:

- Authentication moved from Apache basic auth to app logins with roles, CSRF
  protection and an audit log. Plain-text legacy passwords are dropped.
- `last_paid` became a payment history (date required; amount, method and
  note optional). Bulk payment can also set a new Clidive expiry date.
- The export is a UTF-8 CSV instead of HTML pretending to be `.xls`. Dates are
  ISO `YYYY-MM-DD`, and it adds the columns the legacy export left out. Cells
  that could act as spreadsheet formulas are escaped.
- Email 1 is required and identifies the member: it is stored lower-cased and
  must be unique among non-archived members. Address fields are optional.
- Gender can be left unstated; the legacy form forced Male.
- Dates use native date inputs. Archived members can be restored. A status
  filter "current, excluding Lapsed type" and a type filter were added.
- The email lists can optionally exclude the Lapsed type. The legacy view
  included it.
- Every SQL statement is parameterised; sort columns come from an allow-list.

## Extension points

- **Member self-service:** members already sign in (see
  [Member sign-in](#member-sign-in-email-links)) and view `/me`. Editing
  would add `POST /me`, guarded by `auth.PermEditOwnPersonal` and applying
  only `Principal.SelfEditableGroups()` (personal, contact and emergency
  details). `users.totp_secret` is reserved for optional TOTP.
- **Member directory:** `members.share_contact_details` (default false) and
  the `members.DirectoryEntry` read model (`Member.Directory()`) define what
  one member may see about another. No schema change is needed.
- **Trips and bookings:** planned as a separate `internal/trips` package with
  a public JSON route for the WordPress site.
- **Mailing lists:** for now these are the legacy flags, exported as lists.
  Real list management (subscriptions, sending) is a later step.
