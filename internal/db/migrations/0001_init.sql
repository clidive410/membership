-- Membership types (replaces legacy membershipclidivetype). IDs are kept
-- identical to the legacy IDs so that imported members map 1:1.
CREATE TABLE membership_types (
  id          INT UNSIGNED NOT NULL,
  name        VARCHAR(45)  NOT NULL,
  sort_order  INT          NOT NULL DEFAULT 0,
  PRIMARY KEY (id),
  UNIQUE KEY uq_membership_types_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT INTO membership_types (id, name, sort_order) VALUES
  (1, 'Annual', 10),
  (2, 'Standing Order', 20),
  (3, 'Social', 30),
  (4, 'Honorary', 40),
  (5, '3 Month (eg OD)', 50),
  (6, 'Lapsed', 60);

-- One row per member (replaces legacy `membership`). The legacy login
-- columns (email, surname, password, passhint, admin, session, LastLogin)
-- are intentionally not carried over.
CREATE TABLE members (
  id                       INT UNSIGNED NOT NULL AUTO_INCREMENT,
  legacy_member_id         INT UNSIGNED NULL,
  first_name               VARCHAR(100) NOT NULL DEFAULT '',
  last_name                VARCHAR(100) NOT NULL DEFAULT '',
  gender                   CHAR(1)      NOT NULL DEFAULT '' COMMENT 'M, F or empty (not stated)',
  date_of_birth            DATE         NULL,
  occupation               VARCHAR(100) NOT NULL DEFAULT '',
  address1                 VARCHAR(100) NOT NULL DEFAULT '',
  address2                 VARCHAR(100) NOT NULL DEFAULT '',
  address3                 VARCHAR(100) NOT NULL DEFAULT '',
  town                     VARCHAR(100) NOT NULL DEFAULT '',
  postcode                 VARCHAR(10)  NOT NULL DEFAULT '',
  phone1                   VARCHAR(30)  NOT NULL DEFAULT '',
  phone2                   VARCHAR(30)  NOT NULL DEFAULT '',
  phone3                   VARCHAR(30)  NOT NULL DEFAULT '',
  email1                   VARCHAR(254) NOT NULL DEFAULT '' COMMENT 'primary email: the member identity, lower-cased',
  email2                   VARCHAR(254) NOT NULL DEFAULT '',
  email1_use               BOOLEAN      NOT NULL DEFAULT TRUE,
  email2_use               BOOLEAN      NOT NULL DEFAULT FALSE,
  ice_name                 VARCHAR(100) NOT NULL DEFAULT '',
  ice_relation             VARCHAR(100) NOT NULL DEFAULT '',
  ice_phones               VARCHAR(100) NOT NULL DEFAULT '',
  ice_address              VARCHAR(255) NOT NULL DEFAULT '',
  bsac_member_id           VARCHAR(20)  NOT NULL DEFAULT '' COMMENT 'confirmed BSAC membership number',
  bsac_member_id_if_known  VARCHAR(20)  NOT NULL DEFAULT '' COMMENT 'as provided by the member',
  bsac_renewal_due         DATE         NULL,
  bsac_annual_fee          DECIMAL(10,2) NULL,
  bsac_abated              BOOLEAN      NOT NULL DEFAULT FALSE,
  bsac_direct_debit        BOOLEAN      NOT NULL DEFAULT FALSE,
  join_date                DATE         NULL,
  join_date_raw            VARCHAR(45)  NOT NULL DEFAULT '' COMMENT 'legacy free-text join date that could not be parsed',
  membership_type_id       INT UNSIGNED NULL,
  membership_expiry        DATE         NULL,
  query_text               VARCHAR(255) NOT NULL DEFAULT '',
  notes                    VARCHAR(1000) NOT NULL DEFAULT '',
  diver_grades             VARCHAR(100) NOT NULL DEFAULT '',
  instructor_grades        VARCHAR(100) NOT NULL DEFAULT '',
  other_qualifications     VARCHAR(255) NOT NULL DEFAULT '',
  previous_club            VARCHAR(100) NOT NULL DEFAULT '',
  committee_mailing        BOOLEAN      NOT NULL DEFAULT FALSE,
  instructor_mailing       BOOLEAN      NOT NULL DEFAULT FALSE,
  boat_handler_mailing     BOOLEAN      NOT NULL DEFAULT FALSE,
  lifeguard_mailing        BOOLEAN      NOT NULL DEFAULT FALSE,
  driver                   BOOLEAN      NOT NULL DEFAULT FALSE,
  created_at               DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at               DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  deleted_at               DATETIME     NULL COMMENT 'soft delete ("archived")',
  PRIMARY KEY (id),
  UNIQUE KEY uq_members_legacy (legacy_member_id),
  KEY ix_members_name (last_name, first_name),
  KEY ix_members_type (membership_type_id),
  KEY ix_members_deleted (deleted_at),
  KEY ix_members_email1 (email1),
  CONSTRAINT fk_members_type FOREIGN KEY (membership_type_id) REFERENCES membership_types (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Payment history (replaces the single legacy last_paid column).
CREATE TABLE payments (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  member_id   INT UNSIGNED    NOT NULL,
  paid_on     DATE            NOT NULL,
  amount      DECIMAL(10,2)   NULL COMMENT 'NULL when unknown (e.g. imported from legacy last_paid)',
  method      VARCHAR(30)     NOT NULL DEFAULT '',
  note        VARCHAR(255)    NOT NULL DEFAULT '',
  source      VARCHAR(20)     NOT NULL DEFAULT 'manual' COMMENT 'manual, bulk or legacy',
  created_by  VARCHAR(100)    NOT NULL DEFAULT '',
  created_at  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY ix_payments_member (member_id, paid_on),
  CONSTRAINT fk_payments_member FOREIGN KEY (member_id) REFERENCES members (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Login identities, keyed by lower-cased email. Authentication is kept
-- separate from the member record: a user optionally links to a member row
-- (members get one when self-service arrives), admins may have none.
-- password_hash is nullable because members will sign in with email magic
-- links; admins use a password (argon2id).
CREATE TABLE users (
  id             INT UNSIGNED NOT NULL AUTO_INCREMENT,
  email          VARCHAR(254) NOT NULL,
  member_id      INT UNSIGNED NULL,
  password_hash  VARCHAR(255) NULL,
  totp_secret    VARCHAR(255) NULL COMMENT 'reserved for optional TOTP (not used yet)',
  disabled       BOOLEAN      NOT NULL DEFAULT FALSE,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  last_login_at  DATETIME     NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_users_email (email),
  UNIQUE KEY uq_users_member (member_id),
  CONSTRAINT fk_users_member FOREIGN KEY (member_id) REFERENCES members (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE roles (
  name         VARCHAR(40)  NOT NULL,
  description  VARCHAR(255) NOT NULL DEFAULT '',
  PRIMARY KEY (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT INTO roles (name, description) VALUES
  ('admin', 'Committee: full access to the admin UI'),
  ('membership_manager', 'May view members and edit membership type, expiry, BSAC details and payments'),
  ('member', 'Club member: future self-service (own contact, address and emergency details only)');

CREATE TABLE user_roles (
  user_id  INT UNSIGNED NOT NULL,
  role     VARCHAR(40)  NOT NULL,
  PRIMARY KEY (user_id, role),
  CONSTRAINT fk_user_roles_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
  CONSTRAINT fk_user_roles_role FOREIGN KEY (role) REFERENCES roles (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Single-use, expiring login tokens (email magic links). Migrated but not
-- used yet. Only a SHA-256 of the token is stored.
CREATE TABLE login_tokens (
  token_hash  CHAR(64)     NOT NULL,
  user_id     INT UNSIGNED NOT NULL,
  purpose     VARCHAR(20)  NOT NULL DEFAULT 'magic_link',
  created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at  DATETIME     NOT NULL,
  used_at     DATETIME     NULL,
  PRIMARY KEY (token_hash),
  KEY ix_login_tokens_user (user_id),
  KEY ix_login_tokens_expires (expires_at),
  CONSTRAINT fk_login_tokens_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Server-side sessions; only a SHA-256 of the cookie token is stored.
CREATE TABLE sessions (
  token_hash   CHAR(64)     NOT NULL,
  user_id      INT UNSIGNED NOT NULL,
  auth_method  VARCHAR(20)  NOT NULL DEFAULT 'password',
  created_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at   DATETIME     NOT NULL,
  PRIMARY KEY (token_hash),
  KEY ix_sessions_user (user_id),
  KEY ix_sessions_expires (expires_at),
  CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Who changed what, when. Written in the same transaction as every member
-- and payment mutation. No foreign keys so the trail survives deletions.
CREATE TABLE audit_log (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  at             DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  actor_user_id  INT UNSIGNED    NULL,
  actor          VARCHAR(254)    NOT NULL COMMENT 'user email or system label such as migrate-legacy',
  action         VARCHAR(40)     NOT NULL,
  member_id      INT UNSIGNED    NULL,
  entity         VARCHAR(20)     NOT NULL,
  entity_id      BIGINT UNSIGNED NULL,
  changes        LONGTEXT        NULL COMMENT 'JSON: {"field": ["old", "new"], ...}' CHECK (changes IS NULL OR JSON_VALID(changes)),
  PRIMARY KEY (id),
  KEY ix_audit_member (member_id, at),
  KEY ix_audit_at (at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
