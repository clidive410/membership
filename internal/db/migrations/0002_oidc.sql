-- The portal as an OpenID Connect provider (docs/sso-plan.md section 1).
-- Every token, code and client secret is stored hashed: SHA-256 for random
-- tokens and codes, argon2id for client secrets. Nothing here is used until
-- a client is registered with `membership oidc client create` or `ensure`.

-- Website editors: no portal permission beyond their own record, mapped to
-- the WordPress editor role by the WordPress client.
INSERT IGNORE INTO roles (name, description) VALUES
  ('editor', 'Website editor: WordPress editor via single sign-on, no extra portal permissions');

-- Registered relying parties. No dynamic registration.
CREATE TABLE oidc_clients (
  client_id                  VARCHAR(100) NOT NULL,
  secret_hash                VARCHAR(255) NOT NULL COMMENT 'argon2id (PHC string)',
  name                       VARCHAR(100) NOT NULL,
  redirect_uris              LONGTEXT     NOT NULL COMMENT 'JSON array, exact match' CHECK (JSON_VALID(redirect_uris)),
  post_logout_redirect_uris  LONGTEXT     NOT NULL COMMENT 'JSON array, exact match' CHECK (JSON_VALID(post_logout_redirect_uris)),
  scopes                     VARCHAR(255) NOT NULL DEFAULT 'openid email profile offline_access clidive' COMMENT 'space-separated scopes the client may request',
  first_party                BOOLEAN      NOT NULL DEFAULT FALSE COMMENT 'no consent screen',
  created_at                 DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at                 DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  disabled_at                DATETIME     NULL,
  PRIMARY KEY (client_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Signing keys (RS256). The PKCS#8 private key is sealed with AES-256-GCM
-- under a key derived from SESSION_SECRET (HKDF-SHA256). active_slot is 1
-- for the one key that signs and NULL otherwise (the unique key makes a
-- second concurrent "first key" fail instead of creating two).
CREATE TABLE oidc_keys (
  kid             VARCHAR(64)  NOT NULL,
  alg             VARCHAR(10)  NOT NULL DEFAULT 'RS256',
  private_key_enc BLOB         NOT NULL COMMENT 'nonce and AES-256-GCM sealed PKCS#8 private key',
  public_key      BLOB         NOT NULL COMMENT 'PKIX public key (DER), published in JWKS',
  active_slot     TINYINT      NULL,
  created_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  retired_at      DATETIME     NULL,
  PRIMARY KEY (kid),
  UNIQUE KEY uq_oidc_keys_active (active_slot)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Authorization requests and their codes. A request is bound to the portal
-- session that started it. code_hash is the SHA-256 of the code, valid 60 s
-- and spent on first use (code_used_at).
CREATE TABLE oidc_authorizations (
  id                     CHAR(43)     NOT NULL,
  client_id              VARCHAR(100) NOT NULL,
  user_id                INT UNSIGNED NULL,
  session_hash           CHAR(64)     NULL,
  auth_method            VARCHAR(20)  NOT NULL DEFAULT '',
  auth_time              DATETIME     NULL,
  redirect_uri           VARCHAR(2048) NOT NULL,
  scopes                 VARCHAR(512) NOT NULL,
  state                  VARCHAR(1024) NOT NULL DEFAULT '',
  nonce                  VARCHAR(1024) NOT NULL DEFAULT '',
  code_challenge         VARCHAR(128) NOT NULL,
  code_challenge_method  VARCHAR(10)  NOT NULL,
  response_mode          VARCHAR(20)  NOT NULL DEFAULT '',
  prompt                 VARCHAR(100) NOT NULL DEFAULT '',
  done                   BOOLEAN      NOT NULL DEFAULT FALSE,
  code_hash              CHAR(64)     NULL,
  code_expires_at        DATETIME     NULL,
  code_used_at           DATETIME     NULL,
  created_at             DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at             DATETIME     NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_oidc_authorizations_code (code_hash),
  KEY ix_oidc_authorizations_user (user_id),
  KEY ix_oidc_authorizations_expires (expires_at),
  CONSTRAINT fk_oidc_authorizations_client FOREIGN KEY (client_id) REFERENCES oidc_clients (client_id) ON DELETE CASCADE,
  CONSTRAINT fk_oidc_authorizations_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Access and refresh tokens. token_hash is the SHA-256 of the access token
-- id or of the refresh token. grant_id ties together every token issued
-- from one authorization (rotation keeps it), so that reusing a code or a
-- rotated refresh token revokes the whole grant. session_hash binds tokens
-- to the portal session, so signing out of the portal revokes them.
CREATE TABLE oidc_tokens (
  token_hash    CHAR(64)     NOT NULL,
  kind          VARCHAR(10)  NOT NULL COMMENT 'access or refresh',
  grant_id      CHAR(43)     NOT NULL,
  client_id     VARCHAR(100) NOT NULL,
  user_id       INT UNSIGNED NOT NULL,
  session_hash  CHAR(64)     NULL,
  auth_method   VARCHAR(20)  NOT NULL,
  auth_time     DATETIME     NULL,
  scopes        VARCHAR(512) NOT NULL,
  created_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at    DATETIME     NOT NULL,
  rotated_at    DATETIME     NULL COMMENT 'refresh token already exchanged',
  PRIMARY KEY (token_hash),
  KEY ix_oidc_tokens_grant (grant_id),
  KEY ix_oidc_tokens_user (user_id, client_id),
  KEY ix_oidc_tokens_session (session_hash),
  KEY ix_oidc_tokens_expires (expires_at),
  CONSTRAINT fk_oidc_tokens_client FOREIGN KEY (client_id) REFERENCES oidc_clients (client_id) ON DELETE CASCADE,
  CONSTRAINT fk_oidc_tokens_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Consent given to clients that are not first party, per user and client.
CREATE TABLE oidc_consents (
  user_id     INT UNSIGNED NOT NULL,
  client_id   VARCHAR(100) NOT NULL,
  scopes      VARCHAR(512) NOT NULL,
  created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (user_id, client_id),
  CONSTRAINT fk_oidc_consents_client FOREIGN KEY (client_id) REFERENCES oidc_clients (client_id) ON DELETE CASCADE,
  CONSTRAINT fk_oidc_consents_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
