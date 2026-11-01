# Plan: the membership portal as the identity provider for WordPress (OpenID Connect)

Status: proposed 2026-11-01. Replaces the Caddy `forward_auth` header mechanism.

## Goal
Members and committee sign in once at the portal (`members.clidive.org`) and are recognised on the
WordPress site on any hostname (today `clidive.capella.dev`, later `www.clidive.org`) through a standard
OpenID Connect (OIDC) login. The portal stays the source of truth; WordPress holds only minimal users.

## Design

### 1. The portal becomes an OIDC provider
- Library: `github.com/zitadel/oidc/v3` (server side, actively maintained, certified flows) with our own
  storage adapter backed by MariaDB. No hand-rolled token code.
- Endpoints (all under `BASE_URL`): `/.well-known/openid-configuration`, `/oauth/authorize`,
  `/oauth/token`, `/oauth/userinfo`, `/oauth/jwks`, `/oauth/end_session` (optional).
- Flow: authorization code with PKCE required; `response_type=code` only; no implicit flow.
- Signing: RS256 with a key pair generated at first start and stored encrypted in the database
  (table `oidc_keys`), rotated by a CLI command; the previous key stays published in JWKS for 24 h.
- Clients: table `oidc_clients` (client_id, bcrypt-hashed secret, redirect URIs exact-match, allowed
  scopes, name). Managed by CLI `membership oidc client create|list|rotate-secret|delete`; the WordPress
  client is created at deploy with its secret from a Swarm secret. No dynamic registration.
- Sessions: the authorize endpoint reuses the existing portal session. Not signed in → the normal login
  page (magic link or password) with `next` back to the authorize URL. Members with expired membership
  can still sign in (status is a claim, see below). Archived/disabled users are refused.
- Consent: none for first-party clients (the WordPress client is marked `first_party`).
- Claims (scopes `openid email profile clidive`):
  - `sub`: stable `user:<id>`; `email`, `email_verified: true` (login proves mailbox control);
    `name`, `given_name`, `family_name` from the member record (or user email when no member row).
  - `clidive` scope: `member_id`, `membership_status` (`active|expired|none`), `membership_type`,
    `membership_expiry` (date), `roles` (portal roles incl. the new `editor`), `bsac_member_id`
    (only when present).
- Tokens: ID token 10 min, access token 10 min (opaque, for `/oauth/userinfo`), refresh token
  30 days rotating, revoked on portal logout and when the user is disabled. Authorization codes 60 s,
  single use. All stored hashed.
- Audit: client authorizations and token issuance logged by user id and client id.
- Rate limiting and CSRF as elsewhere; `state` and `nonce` enforced.

### 2. WordPress as an OIDC client
- Plugin: **OpenID Connect Generic Client** (daggerhart, wordpress.org, `daggerhart-openid-connect-generic`),
  configured by constants in a must-use plugin (`clidive-sso.php`) so no settings live only in the
  database: client id/secret (from env: `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET_FILE`), endpoints from the
  portal's discovery URL, scopes `openid email profile clidive`, identity key `email`, nickname `name`.
- Minimal users: created on first login with role `clidive_member` (custom role: `read` only), a random
  unusable password, display name from the `name` claim, and user meta `clidive_member_id`,
  `clidive_membership_status`, `clidive_roles`, `clidive_last_sso` updated on every login.
  Role mapping, applied on every login (so a portal revocation takes effect at the next sign-in):
  portal `admin` → WordPress `administrator`; portal `editor` (new portal role for website editors,
  grantable from the Portal access screens) → WordPress `editor`; anything else → `clidive_member`.
  Existing WordPress editor accounts are matched by email and taken over by SSO on first login
  (their password is replaced by an unusable one). No other profile fields are stored.
- Gating: `clidive_can_view_members_content()` becomes true when the current WordPress user has
  `clidive_membership_status = active` or is a WordPress editor/admin. `clidive_member_status()` reads
  the meta. The header-based path is removed from the plugin. Expired members can log in but see the
  public view with a renewal note.
- Login/logout: the LOGIN menu item and `/login/` start the OIDC flow (plugin's login URL) with a
  redirect back to the current page (existing `clidive_login_url()` wiring). Logout ends the WordPress
  session and calls the portal's `end_session` so both sides agree.
- Housekeeping: a daily WP-Cron job marks users whose last SSO login is older than 400 days as
  `clidive_membership_status=none`; nothing is deleted automatically.
- Password login is removed for everyone: `wp-login.php` shows only "Sign in with Clidive",
  password reset and registration are disabled, and application passwords stay off. Break-glass is
  WP-CLI on the instance (`/srv/clidive/wp.sh user ...`), documented in the infra README.

### 3. Infrastructure
- Portal: new Swarm secret `OIDC_WORDPRESS_CLIENT_SECRET` (GitHub secret, like the others); env
  `OIDC_ISSUER` = `BASE_URL`. The deploy creates/updates the WordPress client on start (idempotent,
  `membership oidc client ensure`) from `OIDC_WORDPRESS_CLIENT_ID` + the secret + redirect URI
  `https://<site_host>/wp-admin/admin-ajax.php?action=openid-connect-authorize`.
- WordPress: env `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET_FILE`, `OIDC_ISSUER` (`https://members.clidive.org`).
- Caddy: remove `forward_auth` and the header stripping (two small deletions); nothing else changes.
- Cookie domain: `COOKIE_DOMAIN` no longer needs to span the site; set it to the portal host only.

### 4. Delivery
- Membership repo, branch `feat/oidc-provider`, PR with: migrations (`oidc_clients`, `oidc_keys`,
  `oidc_authorizations`, `oidc_tokens`), provider wiring, CLI, tests (conformance-style tests for the
  code flow with PKCE, token rotation, revocation on logout/disable, claims per status) and README.
- Blog repo: `clidive-sso.php` must-use plugin, theme gating change, plugin installed from wordpress.org
  in the deploy (version pinned in `scripts/remote-install.sh`).
- Infra repo: secrets wiring, Caddy cleanup, README (login flow diagram in words).
- Order: 1) portal PR merged and deployed (provider live, unused); 2) infra PR (secret, client ensure);
  3) blog PR (plugin + gating), tested on `clidive.capella.dev` with a real member; 4) Caddy cleanup.
- Rollback at any step: the site keeps working for visitors; only member recognition is affected.

### 5. Out of scope now (later, same provider)
Discussion list / document tool SSO (just more clients), passkeys/TOTP at the portal, member
self-service edits, trips. Member data stays in the portal; WordPress never receives addresses, phone
numbers, dates of birth or emergency contacts.

## Decisions taken
- Minimal WordPress users are acceptable (owner, 2026-11-01).
- Authorization code + PKCE only; RS256; no consent screen for the first-party WordPress client.
- Expired members may sign in; content gating uses the `membership_status` claim.
- Portal `admin` → WordPress administrator, portal `editor` → WordPress editor (owner, 2026-11-01).
- No password login in WordPress at all; WP-CLI is the break-glass (owner, 2026-11-01).

## Open questions
None outstanding.
