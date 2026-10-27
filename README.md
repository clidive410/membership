# clidive410/membership

The Clidive (BSAC branch) membership portal, written in Go. It replaces the
membership admin pages of the old PHP site (`old.clidive.org/admin/membership`).

Infrastructure lives in [`clidive410/infra`](https://github.com/clidive410/infra).

- Committee admin UI: member list with search and type/status filters,
  create/edit/archive/restore, payment history, bulk payment management, CSV
  export and mailing-list email lists.
- Roles: `admin` (webmaster/committee), `membership_manager`, and `member`
  (reserved for future self-service).
- Password login (argon2id) with server-side sessions, CSRF protection and an
  audit log of every member and payment change.
- `GET /auth/verify` lets Caddy `forward_auth` tell the WordPress site
  whether the visitor is a signed-in club member.
- `migrate-legacy` imports the legacy `membership` tables idempotently.

Standard library HTTP server (`net/http` pattern routing, `html/template`),
`database/sql` with `go-sql-driver/mysql`, and `golang.org/x/crypto` (argon2)
plus `golang.org/x/term` (password prompt). No JavaScript, no build step.

## Layout

```
cmd/membership/         CLI entrypoint: serve, migrate, migrate-legacy, user, healthcheck
internal/config/        environment configuration (NAME or NAME_FILE)
internal/db/            connection setup and embedded SQL migrations (migrations/*.sql)
internal/members/       domain model, field groups, diffs, MariaDB repository, audit log
internal/auth/          roles and permissions, argon2id, sessions, CSRF, login tokens
internal/legacy/        legacy field mapping and idempotent importer
internal/export/        CSV export and email-list formatting
internal/web/           handlers, middleware, templates and stylesheet (embedded)
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
go run ./cmd/membership serve
```

Then open <http://localhost:8080/> and sign in. To run the whole stack in
containers: `docker compose --profile app up --build`.

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
`NAME` (for example `DB_DSN_FILE=/run/secrets/membership_db_dsn`).

| Variable         | Default                  | Description |
|------------------|--------------------------|-------------|
| `DB_DSN`         | (required)               | [go-sql-driver DSN](https://github.com/go-sql-driver/mysql#dsn-data-source-name) for the app database, e.g. `membership:pw@tcp(clidive_db:3306)/membership`. `parseTime`, UTC and `utf8mb4` are forced. |
| `SESSION_SECRET` | (required)               | At least 32 random characters. Keys the CSRF tokens. |
| `BASE_URL`       | `http://localhost:8080`  | Public URL, e.g. `https://members.clidive.org`. A path such as `https://clidive.org/membership` mounts the app under that prefix. |
| `COOKIE_SECURE`  | `true` if BASE_URL is https | Mark cookies `Secure`. |
| `COOKIE_DOMAIN`  | (host-only)              | Session cookie domain, e.g. `clidive.org`, so the cookie also reaches the WordPress site for `/auth/verify`. |
| `LISTEN_ADDR`    | `:8080`                  | HTTP listen address. |
| `SESSION_TTL`    | `12h`                    | Session lifetime (Go duration). |
| `AUTO_MIGRATE`   | `true`                   | Apply migrations at startup. |
| `LOG_LEVEL`      | `info`                   | `debug`, `info`, `warn`, `error`. |
| `LOG_FORMAT`     | `json`                   | `json` or `text` (log/slog). |
| `MAIL_FROM`      | `membership@clidive.org` | Sender for future emails. Not used yet. |
| `LEGACY_DB_DSN`  |                          | Default for `migrate-legacy -legacy-dsn`. |

Logs never include query strings or member data: only method, path, status,
user ids and counts.

## Users and roles

Login identities (`users`) are keyed by lower-cased email and kept separate
from member records. A user may link to a member row (`users.member_id`);
admins need not have one.

| Role                 | Can |
|----------------------|-----|
| `admin`              | Everything: create, edit all fields, archive/restore, payments, CSV export, email lists, history. |
| `membership_manager` | View members; edit membership fields (Clidive type/expiry/join date, BSAC details, query/notes); record and delete payments; bulk payments; view history. Personal and club fields are shown read-only. |
| `member`             | Nothing in the admin UI yet. Reserved for self-service, which will allow editing own contact, address and emergency details only. |

Every route declares its permission (`internal/web/server.go`). Field-level
rules use field groups (`internal/members/fields.go`): the server applies only
the groups the user may edit, whatever the form contains.

```sh
membership user set-password -email someone@example.org [-role admin]  # create/reset; reads stdin if not a terminal
membership user grant  -email someone@example.org -role membership_manager
membership user revoke -email someone@example.org -role admin
membership user link   -email someone@example.org -member-id 123
membership user disable|enable -email someone@example.org
membership user list
```

In the container: `docker exec -it <container> membership user set-password -email ... -role admin`.

## Importing the legacy data

```sh
membership migrate-legacy -legacy-dsn 'user:pw@tcp(legacy-host:3306)/ironmonger' [-dry-run] [-v]
```

- Reads `membership`, `membershipclidivetype` and, if present,
  `membershiprenewaltype`. All records are imported, archived ones too
  (`DeletedOn` becomes `deleted_at`). Member ids are kept.
- **Not imported:** `email`, `surname`, `password` (plain text in the legacy
  database), `passhint`, `admin`, `session`, `LastLogin`. These columns are
  never selected. Legacy logins are not migrated; committee users are created
  with `membership user set-password`.
- `last_paid` becomes one payment row (`source = legacy`, no amount).
- Zero dates (`0000-00-00`) become NULL. The free-text `ClidiveJoinDate` is
  parsed (`Y-M-D`, `DD/MM/YY`, `DD.MM.YY`). Anything it cannot parse is kept in
  `join_date_raw` and shown on the member page. Emails are trimmed and
  lower-cased.
- Idempotent: members are matched on `legacy_member_id` and updated in
  place, and the legacy payment row follows `last_paid`. Each run is one
  transaction and records `legacy.import` audit entries. Re-running after
  go-live **overwrites edits made in the new app** to imported members, so run
  it only during cutover.
- The output contains only counts. With `-v` it also lists warnings, each
  giving a member id and column, never the value.

## `/auth/verify` (Caddy forward_auth)

`GET /auth/verify` always returns `200`. When the request carries a valid
session cookie of a user linked to a non-archived member, it adds:

- `X-Clidive-Member: 1`
- `X-Clidive-Member-Status: active` or `expired`. `expired` means the Clidive
  membership expiry date is before today; no other rule is applied.

With `COOKIE_DOMAIN=clidive.org` the session cookie set by
`members.clidive.org` is also sent to `clidive.org`. The WordPress site can
then show member-only prices:

```caddyfile
clidive.org {
	# Never trust these headers from the client.
	request_header -X-Clidive-Member
	request_header -X-Clidive-Member-Status
	forward_auth membership:8080 {
		uri /auth/verify
		copy_headers X-Clidive-Member X-Clidive-Member-Status
	}
	reverse_proxy wordpress:80
}
```

Any WordPress page cache must vary on these headers or bypass the cache for
requests that carry the `clidive_session` cookie. Members cannot sign in yet:
that arrives with magic links (see below). Until then only users linked to a
member with `user link` get the header.

## Deployment

The service runs as a Docker Swarm service defined in the
[`clidive410/infra`](https://github.com/clidive410/infra) repository
(`terraform/blog`). This repository does not deploy anything itself.

- **Image:** CI builds on every push and pull request and pushes
  `ghcr.io/clidive410/membership:latest` and `:sha-<commit>` on pushes to `main`.
  It is a static binary on distroless, running as a non-root user, with a
  built-in `HEALTHCHECK` (`membership healthcheck`).
- **Routing:** Caddy reverse-proxies `https://members.clidive.org` to the
  service on port 8080 (`BASE_URL=https://members.clidive.org`,
  `COOKIE_DOMAIN=clidive.org`). Hosting under a path prefix instead also works:
  set `BASE_URL` with the path and use Caddy `handle /membership/*` (not
  `handle_path`), because the app strips the prefix itself.
- **Database:** a dedicated database and user on the shared `clidive_db`
  MariaDB 11 service, for example:
  ```sql
  CREATE DATABASE membership CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
  CREATE USER 'membership'@'%' IDENTIFIED BY '...';
  GRANT ALL PRIVILEGES ON membership.* TO 'membership'@'%';
  ```
  Migrations run at startup and are serialised with a MariaDB named lock, so
  several replicas can start safely.
- **Secrets:** pass `DB_DSN_FILE` and `SESSION_SECRET_FILE` as Swarm secrets.
- **Shutdown:** SIGTERM drains in-flight requests for up to 15 seconds.

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

- **Member self-service and magic links:** `users.password_hash` is
  nullable, and `login_tokens` (single-use, expiring, hashed) is already
  migrated. `auth.Store.CreateLoginToken`/`ConsumeLoginToken` exist. The plan:
  look up the non-archived member by primary email, create or link the user
  with role `member`, and email the link via AWS SES from `MAIL_FROM`. A `/me`
  route then applies only `Principal.SelfEditableGroups()`. `users.totp_secret`
  is reserved for optional TOTP.
- **Member directory:** `members.share_contact_details` (default false) and
  the `members.DirectoryEntry` read model (`Member.Directory()`) define what
  one member may see about another. No schema change is needed.
- **Trips and bookings:** planned as a separate `internal/trips` package with
  a public JSON route for the WordPress site.
- **Mailing lists:** for now these are the legacy flags, exported as lists.
  Real list management (subscriptions, sending) is a later step.
