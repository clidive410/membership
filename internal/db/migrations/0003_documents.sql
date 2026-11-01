-- Document library (replaces the WordPress WP-Filebase plugin). Files live
-- on disk under DOCUMENTS_DIR as <stored_name>; only metadata is here.

-- Categories form a tree. access is public, members (any signed-in user)
-- or committee (admin, membership_manager); a category is never more open
-- than its parent (resolved in Go).
CREATE TABLE document_categories (
  id           INT UNSIGNED  NOT NULL AUTO_INCREMENT,
  parent_id    INT UNSIGNED  NULL,
  name         VARCHAR(255)  NOT NULL,
  slug         VARCHAR(255)  NOT NULL,
  description  VARCHAR(2000) NOT NULL DEFAULT '',
  sort_order   INT           NOT NULL DEFAULT 0,
  access       VARCHAR(10)   NOT NULL DEFAULT 'committee' CHECK (access IN ('public', 'members', 'committee')),
  legacy_key   VARCHAR(64)   NULL COMMENT 'import key, e.g. wpfb-cat:16 or archive',
  created_at   DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
  archived_at  DATETIME      NULL COMMENT 'soft delete: hides the category, its subcategories and documents',
  PRIMARY KEY (id),
  UNIQUE KEY uq_document_categories_slug (slug),
  UNIQUE KEY uq_document_categories_legacy (legacy_key),
  KEY ix_document_categories_parent (parent_id),
  CONSTRAINT fk_document_categories_parent FOREIGN KEY (parent_id) REFERENCES document_categories (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE documents (
  id           INT UNSIGNED    NOT NULL AUTO_INCREMENT,
  category_id  INT UNSIGNED    NULL COMMENT 'NULL: top level of the library',
  title        VARCHAR(255)    NOT NULL,
  description  VARCHAR(2000)   NOT NULL DEFAULT '',
  file_name    VARCHAR(255)    NOT NULL COMMENT 'original file name, used for downloads',
  stored_name  VARCHAR(64)     NOT NULL COMMENT 'random <hex>.bin under DOCUMENTS_DIR',
  mime_type    VARCHAR(100)    NOT NULL DEFAULT 'application/octet-stream',
  size_bytes   BIGINT UNSIGNED NOT NULL DEFAULT 0,
  sha256       CHAR(64)        NOT NULL DEFAULT '' COMMENT 'empty when the file is missing',
  access       VARCHAR(10)     NOT NULL DEFAULT 'inherit' CHECK (access IN ('public', 'members', 'committee', 'inherit')),
  uploaded_by  INT UNSIGNED    NULL,
  uploaded_at  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  legacy_id    BIGINT UNSIGNED NULL COMMENT 'dbt_wpfb_files.file_id',
  legacy_path  VARCHAR(2000)   NOT NULL DEFAULT '',
  downloads    INT UNSIGNED    NOT NULL DEFAULT 0,
  archived_at  DATETIME        NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_documents_stored (stored_name),
  UNIQUE KEY uq_documents_legacy (legacy_id),
  KEY ix_documents_category (category_id),
  CONSTRAINT fk_documents_category FOREIGN KEY (category_id) REFERENCES document_categories (id),
  CONSTRAINT fk_documents_uploaded_by FOREIGN KEY (uploaded_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
