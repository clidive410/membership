# clidive410/membership

The Clidive (BSAC branch) membership portal, written in Go. It replaces the
membership admin pages of the old PHP site (`old.clidive.org/admin/membership`).

Infrastructure lives in [`clidive410/infra`](https://github.com/clidive410/infra).

- Committee admin UI: member list with search and type/status filters,
  create/edit/archive/restore, payment history, bulk payment management, CSV
  export and mailing-list email lists.
- Roles: `admin` (webmaster/committee), `membership_manager`, `editor`
  (website editor) and `member`.
- Members sign in with a single-use link emailed to their primary address
  and see their own record at `/me` (read-only). The committee signs in with
  a password (argon2id), set from an emailed invitation or reset link, so
  nobody has to share a password. Admins manage logins and roles in the UI
  (`/admin/users`) or with `membership user`. Server-side sessions, CSRF
  protection and an audit log of every member, payment and access change
  and of sign-ins.
- OpenID Connect provider (authorization code + PKCE): the WordPress site
  and later other club tools sign members in through the portal (see
  [Using the portal as an identity provider](#using-the-portal-as-an-identity-provider)).
- `GET /auth/verify` lets Caddy `forward_auth` tell the WordPress site
  whether the visitor is a signed-in club member, and whether their
  membership has expired.
- Document library at `/documents` (public, members and committee
  documents in a category tree), managed at `/admin/documents`; replaces the
  old WordPress WP-Filebase library (see [Documents](#documents)).
- `migrate-legacy` imports the legacy `membership` tables idempotently.
- `seed-dev` fills an empty scratch database with invented members for UI work.

`net/http` with [`gorilla/mux`](https://github.com/gorilla/mux) for routing
(method matching, an `/admin` subrouter whose middleware enforces sign-in and
each route's permission), `html/template`, `database/sql` with
`go-sql-driver/mysql`, `golang.org/x/crypto` (argon2) plus
`golang.org/x/term` (password prompt), and
[`zitadel/oidc`](https://github.com/zitadel/oidc) (`op` package) for the
OpenID Connect provider. The UI uses the
[Tabler](https://tabler.io) CSS kit, vendored and embedded in the binary
(see `internal/web/static/vendor/README.md`), themed to the club's colours
(black, logo yellow `#fff200`, Open Sans Condensed headings) in
`internal/web/static/style.css`. No JavaScript, no npm, no build step.

## Layout

```
cmd/membership/         CLI entrypoint: serve, migrate, migrate-legacy, user, oidc, documents, healthcheck
internal/config/        environment configuration (NAME or NAME_FILE)
internal/db/            connection setup and embedded SQL migrations (migrations/*.sql)
internal/members/       domain model, field groups, diffs, MariaDB repository, audit log
internal/auth/          roles and permissions, argon2id, sessions, CSRF, login tokens
internal/idp/           OpenID Connect provider: storage for zitadel/oidc, signing keys, clients, claims
internal/legacy/        legacy field mapping and idempotent importer
internal/documents/     document library: access rules, file storage, repository, WP-Filebase import
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
| `SESSION_SECRET` | (required)               | At least 32 random characters. Keys the CSRF tokens, and (through HKDF) the encryption of the OIDC signing keys and access tokens: changing it invalidates them (run `membership oidc rotate-keys` afterwards). |
| `BASE_URL`       | `http://localhost:8080`  | Public URL, e.g. `https://members.clidive.org`. A path such as `https://clidive.org/membership` mounts the app under that prefix. |
| `COOKIE_SECURE`  | `true` if BASE_URL is https | Mark cookies `Secure`. |
| `COOKIE_DOMAIN`  | (host-only)              | Session cookie domain, e.g. `clidive.org`, so the cookie also reaches the WordPress site for `/auth/verify`. |
| `LISTEN_ADDR`    | `:8080`                  | HTTP listen address. The healthcheck probes its port on 127.0.0.1. |
| `SESSION_TTL`    | `12h`                    | Lifetime of sessions started with a password (Go duration). |
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
| `DOCUMENTS_DIR`  | `/srv/clidive/data/documents` (`./data/documents` with `LOG_FORMAT=text`) | Where document files are stored. Must not be under a web root; must be writable by the service user. Created at start if possible; if not, the error is logged and the portal still serves (uploads and downloads fail). |
| `DOCUMENTS_MAX_UPLOAD_MB` | `50`            | Upload size limit for documents (1 to 2048). |
| `OIDC_ISSUER`    | (= `BASE_URL`)           | Optional. The OIDC issuer is always `BASE_URL`; a different value is logged and ignored. |
| `OIDC_WORDPRESS_CLIENT_ID` | (none)         | When set, `serve` creates or updates this OIDC client after migrations (`membership oidc client ensure`); failures are logged, never fatal. |
| `OIDC_WORDPRESS_CLIENT_SECRET` | (none)     | Its secret (at least 32 characters), usually `OIDC_WORDPRESS_CLIENT_SECRET_FILE` (Swarm secret). |
| `OIDC_WORDPRESS_REDIRECT_URI` | (none)      | Its redirect URI, e.g. `https://www.clidive.org/wp-admin/admin-ajax.php?action=openid-connect-authorize`. |
| `OIDC_WORDPRESS_POST_LOGOUT_URI` | (origin of the redirect URI + `/`) | Optional post-logout redirect URI. |
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
| `member`             | Sign in with an emailed link and view their own record at `/me` (read-only), and members documents. No access to `/admin` (403). Granted automatically on the first link request. Self-service editing (own contact, address and emergency details only) is planned. |
| `editor`             | Website editor: no portal permission beyond their own record and members documents. It exists for the `roles` claim, which the WordPress site maps to its editor role. Granted with the "Website editor" checkbox in Portal access, or `membership user grant -role editor`. |

A user's roles do not depend on how they signed in: a session started
from an emailed link and a password session carry the same, full set of
roles. The sign-in method is recorded on the session (`auth_method`) for
the audit log only.

Every route declares its permission (`internal/web/server.go`). Field-level
rules use field groups (`internal/members/fields.go`): the server applies only
the groups the user may edit, whatever the form contains.

```sh
membership user invite -email someone@example.org -role admin|membership_manager|editor [-member-id 123]  # emails a set-password link
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

Both offer one role select, a **Website editor** checkbox (the `editor`
role, independent of the level) and two buttons:

- **Save** sets the access level:
  - `none` revokes every role, **disables** the login, ends all its
    sessions, spends its outstanding links and revokes its OpenID Connect
    tokens (relying parties lose access at once). The row is kept (audit
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
3. Later they sign in at `/login` with "Sign in with a password"
   ("Forgotten your password?" sends a new link) or, if they are a
   member, with an emailed sign-in link: both give the same roles.
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

## Documents

The document library replaces the WP-Filebase plugin of the old WordPress
site. Categories form a tree; each document has a title, description, file
(original name, size, SHA-256), uploader, date and download count.

| Route | Who | What |
|-------|-----|------|
| `GET /documents` | everyone | Category tree (collapsible), subcategories, documents with size and date, search over titles and descriptions (`?q=`), one category with `?cat=<slug>`. Each item is shown or not by its access. |
| `GET /documents/{id}/download` | per document | The file as an attachment. A visitor asking for a members or committee document is sent to sign in; anything else they may not see is 404. |
| `/admin/documents` | `documents.manage` | Upload; create, edit (name, description, parent, order, access) and archive/restore categories; edit (title, description, category, access), replace the file of, and archive/restore documents; filters and archived view. |

**Access levels.** `public` (no sign-in), `members` (any signed-in user:
members, expired members too, with a renewal note, and website editors) and
`committee` (roles `admin` and `membership_manager`). A category is never
more open than its parent (the most restrictive level on the way up
applies). A document is `inherit` (its category's level, the default) or has
its own level, which may be more open than the category: a public document
in a committee category is listed to visitors under that category. A
top-level document that inherits, an unknown level or a broken parent chain
counts as committee. Archiving a category hides it, its subcategories and
their documents; archived items are never downloadable, even by the
committee (restore first). Permissions: `documents.view` (all roles) and
`documents.manage` (`admin`, `membership_manager`).

**Files.** Stored under `DOCUMENTS_DIR` as `<random hex>.bin`, never under a
web root, and only served by the download handler: `Content-Disposition:
attachment` with the sanitised original name, the stored `Content-Type`
(fallback `application/octet-stream`), `X-Content-Type-Options: nosniff`.
Uploads accept `pdf doc docx xls xlsx ppt pptx odt ods csv txt jpg jpeg png
gif zip` up to `DOCUMENTS_MAX_UPLOAD_MB`; the type is taken from the
extension, never from the browser. Uploads and downloads bypass the 30 second
handler timeout (they get 15 minutes); uploads beyond 8 MB are buffered in a
temporary file under `os.TempDir()` until the request ends, so `/tmp` must
be writable. Replacing a file deletes the old one; archiving keeps it.

**Audit.** Every category and document change (`doccat.*`, `document.*`,
with the changed fields) and every download (`document.download`, user id,
or `anonymous` for public documents) is written to `audit_log` with
`entity = document|document_category`. Downloads also increment
`documents.downloads` (range requests are not counted).

### Importing the WP-Filebase library

```sh
DOCUMENTS_DIR=/srv/clidive/data/documents membership documents import-legacy \
  -catalog-dsn 'user:pw@tcp(host:3306)/wordpress' \
  -files-dir /path/to/wp-content/uploads/filebase [-dry-run] [-v]
```

- Reads `dbt_wpfb_cats` and `dbt_wpfb_files` (SELECTs only) and copies each
  file, found at `-files-dir/<file_path>`, into `DOCUMENTS_DIR` under a new
  name; the copy is re-read and its SHA-256 and size checked, and size and md5
  are compared with the catalogue (differences are reported, the file is
  imported as found).
- Categories keep their parents, names, descriptions and order; files map
  to documents with title `file_display_name` (or the file name),
  description, type from the extension (anything outside the upload
  allow-list, such as `.html`, is served as `application/octet-stream`),
  size, date `file_date`, `legacy_id = file_id` and `legacy_path`.
- A file missing on disk is still imported, archived, with
  "(file missing in migration)" appended to its description; the import
  carries on. A later run copies it if it has turned up and restores it.
- **Access mapping** (club decision, 2026-11-01; `legacyCategoryAccess`,
  `legacyFileAccess` and `movedToArchive` in `internal/documents/legacy.go`):
  every category is `committee` and every file `inherit` (so committee);
  the one file whose WordPress role list is empty becomes `public`; the
  top-level category "BSAC Courses (not updated)" moves under a new
  top-level category "Archive" (committee). The summary prints the number of
  documents per effective access level.
- Idempotent: categories are matched on `legacy_key` (`wpfb-cat:<cat_id>`,
  `archive`) and documents on `legacy_id`. A re-run updates metadata and
  re-applies the mapping (overwriting portal edits to imported titles,
  descriptions, categories and access), and never copies a file twice. One
  transaction; files copied by a failed run are removed. `-dry-run` checks
  everything, rolls back and copies nothing. Output is counts only; `-v`
  adds the legacy file ids that are missing or differ.

On the 2026 catalogue (171 categories, 654 files, 831 MB): 172 categories
(with Archive), 654 documents, 652 files copied, 2 missing (two `.html`
pages of the course material, one of them empty in WordPress already),
none differing; 1 public document, 653 committee.

### Infrastructure

The service needs a bind mount for `DOCUMENTS_DIR`, e.g. the host directory
`/srv/clidive/data/documents` mounted at the same path, writable by the
image's user (`nonroot`, uid/gid 65532: `chown 65532:65532`), and included in
the instance backups. Without it the portal works, but uploads fail and
files would not survive a redeploy.

## Member sign-in (email links)

Members have no password. They sign in with a link sent to their primary
email (Email 1), which identifies them:

1. `GET /login` shows "Members: email me a sign-in link" (an email field)
   and, collapsed below it, "Sign in with a password".
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

A session started from a link carries all of the user's roles, exactly as
a password session does: a committee user who is also a member gets the
admin UI, and a website editor gets `editor` in the `roles` claim, after
either sign-in. Only the session lifetime differs (`MEMBER_SESSION_TTL`
for links, `SESSION_TTL` for passwords). Members see a top bar with only "My membership" and "Sign out";
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

1. Under the password form, `GET /login` links to "Forgotten
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

## Using the portal as an identity provider

The portal is an OpenID Connect provider (`internal/idp`, on
[`zitadel/oidc`](https://github.com/zitadel/oidc)'s `op` package with
storage in MariaDB). The issuer is `BASE_URL`
(`https://members.clidive.org`). It is inert until a client is registered:
the endpoints answer, but no authorization can succeed and no signing key is
generated before the first token.

| Endpoint | |
|----------|-|
| `/.well-known/openid-configuration` | Discovery. |
| `/oauth/authorize` | Authorization endpoint (GET or POST). |
| `/oauth/token` | Token endpoint: `authorization_code` and `refresh_token` grants; client authentication `client_secret_basic` or `client_secret_post`. Rate limited per client and address. |
| `/oauth/userinfo` | Userinfo (Bearer access token). |
| `/oauth/jwks` | Signing keys (RS256): the active key and keys retired less than 24 h ago. |
| `/oauth/revoke` | Token revocation (RFC 7009). Revoking a refresh token revokes its whole grant. |
| `/oauth/end_session` | RP-initiated logout (`id_token_hint`, `post_logout_redirect_uri`, `state`). |

**Flow.** Authorization code with PKCE only: `response_type=code`,
`code_challenge_method=S256`, `state` and `nonce` are required, and the
response mode is `query`. Implicit, hybrid, `form_post`, client credentials,
JWT bearer, token exchange, device flow, introspection and dynamic
registration are not offered. Redirect URIs match exactly (query string
included).

At `/oauth/authorize` the portal session is reused. Without one the browser
goes to the normal sign-in page (email link or password) with
`next=/oauth/authorize?...` and comes back afterwards. `prompt=none` answers
`login_required`/`consent_required` instead; `prompt=login`, or a session
older than `max_age`, signs the portal session out and asks for a new
sign-in. The request is bound to the browser's portal session. Disabled
users, users without a role and users linked to an archived member get
`access_denied`; members whose membership has expired can sign in (see
`membership_status`). First-party clients skip consent; other clients get a
consent page once per user and client (`oidc_consents`).

**Scopes and claims** (in the ID token and at userinfo):

| Scope | Claims |
|-------|--------|
| `openid` | `sub`: `user:<users.id>`, stable. |
| `email` | `email` (the login email), `email_verified: true` (signing in proves control of the mailbox). |
| `profile` | `name`, `given_name`, `family_name` from the linked member record (`name` is the email when there is none). |
| `clidive` | `member_id` (or `null`), `membership_status` (`active`, `expired` or `none` without a current member record), `membership_type` (or `null`), `membership_expiry` (`YYYY-MM-DD` or `null`), `roles` (the user's portal roles: `admin`, `membership_manager`, `editor`, `member`; the same whether the session was started with an email link or a password), `bsac_member_id` (only when present). |
| `offline_access` | A refresh token. |

Addresses, phone numbers, dates of birth and emergency contacts are never
released.

**Tokens.** Codes are valid 60 s and single use (a second use revokes what
the first produced). Access tokens are opaque and valid 10 minutes; ID
tokens 10 minutes; refresh tokens 30 days from their issue, rotated on every
use (reusing a rotated one revokes the grant). Codes, access token ids and
refresh tokens are stored as SHA-256 hashes, client secrets as argon2id.
Tokens are revoked on portal logout (those issued during that session), on
`end_session`, on `/oauth/revoke`, when a login is disabled or set to no
access, and when its password is changed. Signing keys are RSA 2048,
generated on first use and stored encrypted (AES-256-GCM, key derived from
`SESSION_SECRET` with HKDF). Client authorizations (`oidc.authorize`) and
token issuance (`oidc.token`) are audit-logged by user id with the client id.

**Registering a client.**

```sh
membership oidc client create -id ID -name "Name" -redirect-uri URI [-redirect-uri URI ...] \
    [-post-logout-uri URI ...] [-scopes "openid email profile offline_access clidive"] [-first-party] [-secret-file PATH]
                                            # prints a generated secret once unless -secret-file is given
membership oidc client list
membership oidc client rotate-secret -id ID [-secret-file PATH]
membership oidc client disable -id ID      # also deletes its tokens, codes and consents
membership oidc client ensure              # idempotent create-or-update, from flags or OIDC_WORDPRESS_* (first party)
membership oidc rotate-keys                # new signing key; the old one stays in the JWKS for 24 h
```

Redirect and post-logout URIs must be https (http only on localhost);
post-logout URIs default to the origin of each redirect URI (`https://host/`).
`ensure` never re-enables a disabled client and re-hashes the secret only
when it changed. `serve` runs it at start when `OIDC_WORDPRESS_CLIENT_ID` is
set.

**WordPress** (OpenID Connect Generic Client, see `docs/sso-plan.md`):

| Plugin setting | Value |
|----------------|-------|
| Client ID | `wordpress` (`OIDC_WORDPRESS_CLIENT_ID`) |
| Client secret | the Swarm secret `OIDC_WORDPRESS_CLIENT_SECRET` |
| Scope | `openid email profile clidive offline_access` |
| Login endpoint | `https://members.clidive.org/oauth/authorize` |
| Userinfo endpoint | `https://members.clidive.org/oauth/userinfo` |
| Token endpoint | `https://members.clidive.org/oauth/token` |
| End session endpoint | `https://members.clidive.org/oauth/end_session` |
| JWKS / issuer | `https://members.clidive.org/oauth/jwks`, `https://members.clidive.org` |
| PKCE | enabled (S256), required by the portal |
| Identity key / nickname key | `email` / `name` |
| Redirect URI (registered here) | `https://<site host>/wp-admin/admin-ajax.php?action=openid-connect-authorize` |

## `/auth/verify` (Caddy forward_auth)

`GET /auth/verify` (or `HEAD`) always returns `200` with an empty body. It
never sets a cookie and never redirects. It reads the `clidive_session`
cookie and answers with one database query and no template, and sends
`Cache-Control: no-store` and `Vary: Cookie`.

The headers below are set only when the cookie is a valid, unexpired
session of an enabled user who either

- has the `member` role and is linked to a non-archived member, or
- has a committee role (`admin`, `membership_manager`), whichever way it
  signed in.

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
| `COOKIE_DOMAIN` | `clidive.org`, so `/auth/verify` sees the session on the WordPress site. Once the site uses OIDC, leave it unset (host-only cookie). |
| `OIDC_WORDPRESS_CLIENT_ID`, `OIDC_WORDPRESS_CLIENT_SECRET_FILE`, `OIDC_WORDPRESS_REDIRECT_URI` | Register the WordPress OIDC client at start-up (see [Using the portal as an identity provider](#using-the-portal-as-an-identity-provider)). |
| `SMTP_HOST`, `SMTP_PORT` | SMTP submission server and port (`587`, STARTTLS). |
| `SMTP_USERNAME_FILE`, `SMTP_PASSWORD_FILE` | Swarm secrets with the SMTP credentials. Or `SMTP_USERNAME`/`SMTP_PASSWORD`. |
| `MAIL_FROM` | e.g. `Clidive Membership <membership@clidive.org>`; the SMTP account must be allowed to send as it. |
| `LOGIN_RETURN_URL` | Optional, e.g. `https://www.clidive.org/`. |
| `TRUSTED_PROXIES` | CIDR of the overlay network Caddy reaches the service on, e.g. `10.0.0.0/8`. |
| `LOG_FORMAT` | `json` (default). |
| `LOG_LEVEL` | `info` (default). |
| `MIGRATE_ON_START` | `true`, or run the command `serve --migrate`. |
| `DOCUMENTS_DIR` | `/srv/clidive/data/documents` (default), on a bind mount writable by uid 65532 (see [Documents](#infrastructure)). |
| `DOCUMENTS_MAX_UPLOAD_MB` | `50` (default). |

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
