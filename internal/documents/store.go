package documents

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"

	"github.com/clidive410/membership/internal/members"
)

// Audit actions (audit_log.action) and entities.
const (
	ActionCategoryCreate  = "doccat.create"
	ActionCategoryUpdate  = "doccat.update"
	ActionCategoryArchive = "doccat.archive"
	ActionCategoryRestore = "doccat.restore"
	ActionCreate          = "document.create"
	ActionUpdate          = "document.update"
	ActionReplaceFile     = "document.replace_file"
	ActionArchive         = "document.archive"
	ActionRestore         = "document.restore"
	ActionDownload        = "document.download"
	ActionLegacyImport    = "documents.import"

	entityCategory = "document_category"
	entityDocument = "document"
)

// ErrNotFound is returned for a missing category or document.
var ErrNotFound = errors.New("not found")

// Store is the MariaDB repository of the library. Every mutation writes an
// audit_log row in the same transaction.
type Store struct{ DB *sql.DB }

// NewStore returns a Store on db.
func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

const categoryCols = `id, parent_id, name, slug, description, sort_order, access, legacy_key, created_at, archived_at`

func scanCategory(sc interface{ Scan(...any) error }) (Category, error) {
	var c Category
	err := sc.Scan(&c.ID, &c.ParentID, &c.Name, &c.Slug, &c.Description, &c.SortOrder, &c.Access, &c.LegacyKey, &c.CreatedAt, &c.ArchivedAt)
	return c, err
}

const documentCols = `id, category_id, title, description, file_name, stored_name, mime_type, size_bytes, sha256, access,
	uploaded_by, uploaded_at, legacy_id, legacy_path, downloads, archived_at`

func scanDocument(sc interface{ Scan(...any) error }) (Document, error) {
	var d Document
	err := sc.Scan(&d.ID, &d.CategoryID, &d.Title, &d.Description, &d.FileName, &d.StoredName, &d.MimeType, &d.SizeBytes,
		&d.SHA256, &d.Access, &d.UploadedBy, &d.UploadedAt, &d.LegacyID, &d.LegacyPath, &d.Downloads, &d.ArchivedAt)
	return d, err
}

func listCategories(ctx context.Context, q querier) ([]Category, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+categoryCols+` FROM document_categories ORDER BY sort_order, name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		c, err := scanCategory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func listDocuments(ctx context.Context, q querier) ([]Document, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+documentCols+` FROM documents ORDER BY title, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Document
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Library loads all categories and documents (archived included).
func (s *Store) Library(ctx context.Context) (*Library, error) {
	cats, err := listCategories(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	docs, err := listDocuments(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	return NewLibrary(cats, docs), nil
}

// Document returns one document.
func (s *Store) Document(ctx context.Context, id int64) (*Document, error) {
	d, err := scanDocument(s.DB.QueryRowContext(ctx, `SELECT `+documentCols+` FROM documents WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &d, err
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns a name into a URL slug ("AGM 2013" -> "agm-2013").
func Slugify(s string) string {
	s = strings.Trim(slugRE.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 200 {
		s = strings.TrimRight(s[:200], "-")
	}
	if s == "" {
		return "category"
	}
	return s
}

// uniqueSlug returns base, or base-2, base-3... not used by another
// category than except.
func uniqueSlug(ctx context.Context, q querier, base string, except int64) (string, error) {
	for i := 1; ; i++ {
		slug := base
		if i > 1 {
			slug = base + "-" + strconv.Itoa(i)
		}
		var n int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_categories WHERE slug = ? AND id <> ?`, slug, except).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return slug, nil
		}
	}
}

// categorySlug derives a slug from the parent's slug and the name, so that
// the many "2013" subcategories become "agm-2013", "newsplash-2013"...
func categorySlug(ctx context.Context, q querier, c *Category) (string, error) {
	base := Slugify(c.Name)
	if c.ParentID.Valid {
		var ps string
		if err := q.QueryRowContext(ctx, `SELECT slug FROM document_categories WHERE id = ?`, c.ParentID.Int64).Scan(&ps); err != nil {
			return "", err
		}
		base = Slugify(ps + "-" + c.Name)
	}
	return uniqueSlug(ctx, q, base, c.ID)
}

func audit(ctx context.Context, q querier, a members.Actor, action, entity string, id int64, changes any) error {
	return members.WriteAudit(ctx, q, a, action, 0, entity, id, changes)
}

func insertCategory(ctx context.Context, q querier, c *Category) error {
	slug, err := categorySlug(ctx, q, c)
	if err != nil {
		return err
	}
	c.Slug = slug
	res, err := q.ExecContext(ctx, `INSERT INTO document_categories (parent_id, name, slug, description, sort_order, access, legacy_key)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, c.ParentID, c.Name, c.Slug, c.Description, c.SortOrder, c.Access, c.LegacyKey)
	if err != nil {
		return err
	}
	c.ID, err = res.LastInsertId()
	return err
}

// CreateCategory inserts c (its slug is derived from the name) and sets c.ID.
func (s *Store) CreateCategory(ctx context.Context, a members.Actor, c *Category) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := insertCategory(ctx, tx, c); err != nil {
			return err
		}
		return audit(ctx, tx, a, ActionCategoryCreate, entityCategory, c.ID, map[string]any{"name": c.Name, "access": c.Access})
	})
}

func getCategory(ctx context.Context, q querier, id int64) (*Category, error) {
	c, err := scanCategory(q.QueryRowContext(ctx, `SELECT `+categoryCols+` FROM document_categories WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// diff records changed fields as {"field": [old, new]}.
type diff map[string][2]any

func (d diff) add(field string, old, new any) {
	if fmt.Sprint(old) != fmt.Sprint(new) {
		d[field] = [2]any{old, new}
	}
}

func nullStr(n sql.NullInt64) string {
	if !n.Valid {
		return ""
	}
	return strconv.FormatInt(n.Int64, 10)
}

func updateCategory(ctx context.Context, q querier, c *Category) (diff, error) {
	old, err := getCategory(ctx, q, c.ID)
	if err != nil {
		return nil, err
	}
	d := diff{}
	d.add("name", old.Name, c.Name)
	d.add("description", old.Description, c.Description)
	d.add("parent_id", nullStr(old.ParentID), nullStr(c.ParentID))
	d.add("sort_order", old.SortOrder, c.SortOrder)
	d.add("access", old.Access, c.Access)
	c.Slug = old.Slug
	if old.Name != c.Name || old.ParentID != c.ParentID {
		if c.Slug, err = categorySlug(ctx, q, c); err != nil {
			return nil, err
		}
	}
	if len(d) == 0 {
		return d, nil
	}
	_, err = q.ExecContext(ctx, `UPDATE document_categories SET parent_id = ?, name = ?, slug = ?, description = ?, sort_order = ?, access = ?
		WHERE id = ?`, c.ParentID, c.Name, c.Slug, c.Description, c.SortOrder, c.Access, c.ID)
	return d, err
}

// UpdateCategory saves name, description, parent, order and access. The
// caller checks that the new parent is not the category or a descendant.
func (s *Store) UpdateCategory(ctx context.Context, a members.Actor, c *Category) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		d, err := updateCategory(ctx, tx, c)
		if err != nil || len(d) == 0 {
			return err
		}
		return audit(ctx, tx, a, ActionCategoryUpdate, entityCategory, c.ID, d)
	})
}

func setArchived(ctx context.Context, q querier, table string, id int64, archive bool) error {
	cond := "archived_at IS NOT NULL"
	set := "NULL"
	if archive {
		cond, set = "archived_at IS NULL", "CURRENT_TIMESTAMP"
	}
	res, err := q.ExecContext(ctx, `UPDATE `+table+` SET archived_at = `+set+` WHERE id = ? AND `+cond, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE id = ?`, id).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
	}
	return nil
}

// SetCategoryArchived archives (hides) or restores a category.
func (s *Store) SetCategoryArchived(ctx context.Context, a members.Actor, id int64, archive bool) error {
	action := ActionCategoryRestore
	if archive {
		action = ActionCategoryArchive
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := setArchived(ctx, tx, "document_categories", id, archive); err != nil {
			return err
		}
		return audit(ctx, tx, a, action, entityCategory, id, nil)
	})
}

func insertDocument(ctx context.Context, q querier, d *Document) error {
	res, err := q.ExecContext(ctx, `INSERT INTO documents (category_id, title, description, file_name, stored_name, mime_type,
		size_bytes, sha256, access, uploaded_by, uploaded_at, legacy_id, legacy_path, archived_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.CategoryID, d.Title, d.Description, d.FileName, d.StoredName, d.MimeType, d.SizeBytes, d.SHA256, d.Access,
		d.UploadedBy, d.UploadedAt.UTC(), d.LegacyID, d.LegacyPath, d.ArchivedAt)
	if err != nil {
		return err
	}
	d.ID, err = res.LastInsertId()
	return err
}

// CreateDocument inserts d (whose file is already stored) and sets d.ID.
func (s *Store) CreateDocument(ctx context.Context, a members.Actor, d *Document) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := insertDocument(ctx, tx, d); err != nil {
			return err
		}
		return audit(ctx, tx, a, ActionCreate, entityDocument, d.ID, map[string]any{
			"title": d.Title, "file_name": d.FileName, "size_bytes": d.SizeBytes, "sha256": d.SHA256, "access": d.Access})
	})
}

// UpdateDocument saves title, description, category and access.
func (s *Store) UpdateDocument(ctx context.Context, a members.Actor, d *Document) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		old, err := scanDocument(tx.QueryRowContext(ctx, `SELECT `+documentCols+` FROM documents WHERE id = ? FOR UPDATE`, d.ID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		ch := diff{}
		ch.add("title", old.Title, d.Title)
		ch.add("description", old.Description, d.Description)
		ch.add("category_id", nullStr(old.CategoryID), nullStr(d.CategoryID))
		ch.add("access", old.Access, d.Access)
		if len(ch) == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET title = ?, description = ?, category_id = ?, access = ? WHERE id = ?`,
			d.Title, d.Description, d.CategoryID, d.Access, d.ID); err != nil {
			return err
		}
		return audit(ctx, tx, a, ActionUpdate, entityDocument, d.ID, ch)
	})
}

// ReplaceFile points a document at a newly stored file and returns the
// previous stored name, for the caller to delete.
func (s *Store) ReplaceFile(ctx context.Context, a members.Actor, id int64, fileName, mimeType string, saved Saved, by int64) (string, error) {
	var oldStored string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		old, err := scanDocument(tx.QueryRowContext(ctx, `SELECT `+documentCols+` FROM documents WHERE id = ? FOR UPDATE`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		oldStored = old.StoredName
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET file_name = ?, stored_name = ?, mime_type = ?, size_bytes = ?, sha256 = ?,
			uploaded_by = ?, uploaded_at = CURRENT_TIMESTAMP WHERE id = ?`,
			fileName, saved.StoredName, mimeType, saved.Size, saved.SHA256, sql.NullInt64{Int64: by, Valid: by != 0}, id); err != nil {
			return err
		}
		ch := diff{}
		ch.add("file_name", old.FileName, fileName)
		ch.add("size_bytes", old.SizeBytes, saved.Size)
		ch.add("sha256", old.SHA256, saved.SHA256)
		return audit(ctx, tx, a, ActionReplaceFile, entityDocument, id, ch)
	})
	return oldStored, err
}

// SetDocumentArchived archives (hides) or restores a document; its file is
// kept.
func (s *Store) SetDocumentArchived(ctx context.Context, a members.Actor, id int64, archive bool) error {
	action := ActionRestore
	if archive {
		action = ActionArchive
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := setArchived(ctx, tx, "documents", id, archive); err != nil {
			return err
		}
		return audit(ctx, tx, a, action, entityDocument, id, nil)
	})
}

// RecordDownload increments the download counter and logs who downloaded
// the document (user id, or the "anonymous" label for public downloads).
func (s *Store) RecordDownload(ctx context.Context, a members.Actor, id int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET downloads = downloads + 1 WHERE id = ?`, id); err != nil {
			return err
		}
		return audit(ctx, tx, a, ActionDownload, entityDocument, id, nil)
	})
}

// IsDuplicate reports a unique-key violation.
func IsDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
