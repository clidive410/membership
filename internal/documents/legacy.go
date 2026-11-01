package documents

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/clidive410/membership/internal/logging"
	"github.com/clidive410/membership/internal/members"
	"github.com/sirupsen/logrus"
)

// ImportActor is the audit-log actor of the legacy import.
var ImportActor = members.Actor{Label: "import-legacy-documents"}

// LegacyCategory is a row of the WP-Filebase table dbt_wpfb_cats.
type LegacyCategory struct {
	ID          int64
	Parent      int64 // 0: top level
	Name        string
	Description string
	Order       int
	Roles       string // "administrator|editor|..." (empty: everyone)
}

// LegacyFile is a row of dbt_wpfb_files.
type LegacyFile struct {
	ID          int64
	Name        string // file_name
	Path        string // file_path, relative to wp-content/uploads/filebase/
	Size        int64
	Date        time.Time
	MD5         string // file_hash
	DisplayName string
	Description string
	Roles       string
	Category    int64 // 0: top level
}

// Legacy mapping, decided by the club (2026-11-01). Everything is committee
// only, as on the old site where nearly every file required a committee or
// instructor role; the single file whose role list is empty (readable by
// anyone there) becomes public; the stale course material moves under a new
// top-level "Archive" category. Adjust these functions and re-run the import
// to change it: a re-run re-applies the mapping to imported rows.

// archiveKey is the legacy_key of the "Archive" category the import creates.
const archiveKey = "archive"

// movedToArchive names the top-level legacy categories moved under Archive.
var movedToArchive = map[string]bool{"BSAC Courses (not updated)": true}

// legacyCategoryAccess maps a legacy category to an access level.
func legacyCategoryAccess(LegacyCategory) Access { return Committee }

// legacyFileAccess maps a legacy file: an empty role list was public on
// the old site; anything else follows its category (committee).
func legacyFileAccess(f LegacyFile) Access {
	if strings.TrimSpace(f.Roles) == "" {
		return Public
	}
	return Inherit
}

func legacyCategoryKey(id int64) string { return "wpfb-cat:" + strconv.FormatInt(id, 10) }

// missingNote is appended to the description of a file not found on disk.
const missingNote = "(file missing in migration)"

// mapCategory converts a legacy category; parent is the new parent id.
func mapCategory(c LegacyCategory, parent sql.NullInt64) Category {
	return Category{
		ParentID:    parent,
		Name:        strings.TrimSpace(c.Name),
		Description: strings.TrimSpace(c.Description),
		SortOrder:   c.Order,
		Access:      legacyCategoryAccess(c),
		LegacyKey:   sql.NullString{String: legacyCategoryKey(c.ID), Valid: true},
	}
}

// mapFile converts a legacy file's metadata (not its content).
func mapFile(f LegacyFile, category sql.NullInt64) Document {
	title := strings.TrimSpace(f.DisplayName)
	if title == "" {
		title = strings.TrimSpace(f.Name)
	}
	name := f.Name
	if name == "" {
		name = path.Base(f.Path)
	}
	return Document{
		CategoryID:  category,
		Title:       title,
		Description: strings.TrimSpace(f.Description),
		FileName:    SanitizeFilename(name),
		MimeType:    MimeFor(name),
		SizeBytes:   f.Size,
		Access:      legacyFileAccess(f),
		UploadedAt:  f.Date,
		LegacyID:    sql.NullInt64{Int64: f.ID, Valid: true},
		LegacyPath:  f.Path,
	}
}

// withMissingNote appends missingNote once.
func withMissingNote(desc string) string {
	if strings.Contains(desc, missingNote) {
		return desc
	}
	return strings.TrimSpace(desc + " " + missingNote)
}

// orderCategories returns the categories parents first; categories whose
// parent is missing (or in a cycle) come last and are imported at the top
// level.
func orderCategories(cats []LegacyCategory) []LegacyCategory {
	byID := map[int64]LegacyCategory{}
	for _, c := range cats {
		byID[c.ID] = c
	}
	sorted := append([]LegacyCategory(nil), cats...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var out []LegacyCategory
	done := map[int64]bool{}
	var visit func(c LegacyCategory, depth int)
	visit = func(c LegacyCategory, depth int) {
		if done[c.ID] || depth > len(cats) {
			return
		}
		if p, ok := byID[c.Parent]; ok && c.Parent != 0 {
			visit(p, depth+1)
		}
		if !done[c.ID] {
			done[c.ID] = true
			out = append(out, c)
		}
	}
	for _, c := range sorted {
		visit(c, 0)
	}
	return out
}

// ReadLegacy reads the WP-Filebase catalogue (SELECTs only).
func ReadLegacy(ctx context.Context, db *sql.DB) ([]LegacyCategory, []LegacyFile, error) {
	rows, err := db.QueryContext(ctx, `SELECT cat_id, cat_parent, cat_name, COALESCE(cat_description, ''), cat_order, cat_user_roles
		FROM dbt_wpfb_cats ORDER BY cat_id`)
	if err != nil {
		return nil, nil, fmt.Errorf("reading dbt_wpfb_cats: %w", err)
	}
	defer rows.Close()
	var cats []LegacyCategory
	for rows.Next() {
		var c LegacyCategory
		if err := rows.Scan(&c.ID, &c.Parent, &c.Name, &c.Description, &c.Order, &c.Roles); err != nil {
			return nil, nil, err
		}
		cats = append(cats, c)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	frows, err := db.QueryContext(ctx, `SELECT file_id, file_name, file_path, file_size, CAST(file_date AS CHAR), file_hash,
		file_display_name, COALESCE(file_description, ''), file_user_roles, file_category
		FROM dbt_wpfb_files ORDER BY file_id`)
	if err != nil {
		return nil, nil, fmt.Errorf("reading dbt_wpfb_files: %w", err)
	}
	defer frows.Close()
	var files []LegacyFile
	for frows.Next() {
		var f LegacyFile
		var date string
		if err := frows.Scan(&f.ID, &f.Name, &f.Path, &f.Size, &date, &f.MD5, &f.DisplayName, &f.Description, &f.Roles, &f.Category); err != nil {
			return nil, nil, err
		}
		f.Date, _ = time.Parse(time.DateTime, date) // zero dates stay zero
		files = append(files, f)
	}
	return cats, files, frows.Err()
}

// Importer copies the legacy catalogue and files into the library.
type Importer struct {
	DB       *sql.DB // app database
	Files    Files   // DOCUMENTS_DIR
	FilesDir string  // legacy wp-content/uploads/filebase
	DryRun   bool    // check everything, write nothing
	Log      logrus.FieldLogger
}

// ImportResult summarises a run (counts and legacy ids only).
type ImportResult struct {
	CategoriesCreated, CategoriesUpdated int
	DocumentsCreated, DocumentsUpdated   int
	FilesCopied                          int
	BytesCopied                          int64
	Missing                              []int64 // legacy file ids not found on disk
	Mismatched                           []int64 // legacy file ids whose size or md5 differs from the catalogue
	ByAccess                             map[Access]int
}

// Run imports cats and files in one transaction. Files are copied before
// the commit and removed again if it fails; a dry run copies nothing.
func (im *Importer) Run(ctx context.Context, cats []LegacyCategory, files []LegacyFile) (res ImportResult, err error) {
	if im.Log == nil {
		im.Log = logging.Discard()
	}
	res.ByAccess = map[Access]int{}
	var copied []string
	defer func() {
		if err != nil {
			for _, s := range copied {
				_ = im.Files.Remove(s)
			}
		}
	}()
	tx, err := im.DB.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	catIDs, err := im.importCategories(ctx, tx, cats, &res)
	if err != nil {
		return res, err
	}
	all, err := listCategories(ctx, tx)
	if err != nil {
		return res, err
	}
	lib := NewLibrary(all, nil)
	for _, f := range files {
		cat := sql.NullInt64{}
		if id, ok := catIDs[f.Category]; ok {
			cat = sql.NullInt64{Int64: id, Valid: true}
		} else if f.Category != 0 {
			im.Log.WithFields(logrus.Fields{"file_id": f.ID, "cat_id": f.Category}).Warn("legacy file in an unknown category: imported at the top level")
		}
		d, stored, err := im.importFile(ctx, tx, f, cat, &res)
		if err != nil {
			return res, fmt.Errorf("legacy file %d: %w", f.ID, err)
		}
		if stored != "" {
			copied = append(copied, stored)
		}
		res.ByAccess[lib.EffectiveAccess(&d)]++
	}
	if err := audit(ctx, tx, ImportActor, ActionLegacyImport, entityDocument, 0, map[string]any{
		"categories_created": res.CategoriesCreated, "categories_updated": res.CategoriesUpdated,
		"documents_created": res.DocumentsCreated, "documents_updated": res.DocumentsUpdated,
		"files_copied": res.FilesCopied, "missing": len(res.Missing)}); err != nil {
		return res, err
	}
	if im.DryRun {
		return res, nil // rolled back
	}
	return res, tx.Commit()
}

func (im *Importer) importCategories(ctx context.Context, tx *sql.Tx, cats []LegacyCategory, res *ImportResult) (map[int64]int64, error) {
	archive, err := im.upsertCategory(ctx, tx, Category{Name: "Archive", Description: "Older material kept for reference.",
		SortOrder: 1000, Access: Committee, LegacyKey: sql.NullString{String: archiveKey, Valid: true}}, res)
	if err != nil {
		return nil, err
	}
	ids := map[int64]int64{}
	for _, c := range orderCategories(cats) {
		parent := sql.NullInt64{}
		if id, ok := ids[c.Parent]; ok && c.Parent != 0 {
			parent = sql.NullInt64{Int64: id, Valid: true}
		} else if c.Parent == 0 && movedToArchive[strings.TrimSpace(c.Name)] {
			parent = sql.NullInt64{Int64: archive, Valid: true}
		}
		id, err := im.upsertCategory(ctx, tx, mapCategory(c, parent), res)
		if err != nil {
			return nil, fmt.Errorf("legacy category %d: %w", c.ID, err)
		}
		ids[c.ID] = id
	}
	return ids, nil
}

func (im *Importer) upsertCategory(ctx context.Context, tx *sql.Tx, c Category, res *ImportResult) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM document_categories WHERE legacy_key = ?`, c.LegacyKey).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := insertCategory(ctx, tx, &c); err != nil {
			return 0, err
		}
		res.CategoriesCreated++
		return c.ID, nil
	case err != nil:
		return 0, err
	}
	c.ID = id
	d, err := updateCategory(ctx, tx, &c)
	if err != nil {
		return 0, err
	}
	if len(d) > 0 {
		res.CategoriesUpdated++
	}
	return id, nil
}

// source resolves a legacy path inside FilesDir (never outside it).
func (im *Importer) source(p string) (string, bool) {
	rel := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(p, "/")))
	if rel == "." || !filepath.IsLocal(rel) {
		return "", false
	}
	return filepath.Join(im.FilesDir, rel), true
}

// importFile creates or updates one document. It returns the stored name of
// a file it copied, so that a failed run can remove it.
func (im *Importer) importFile(ctx context.Context, tx *sql.Tx, f LegacyFile, cat sql.NullInt64, res *ImportResult) (Document, string, error) {
	d := mapFile(f, cat)
	existing, err := scanDocument(tx.QueryRowContext(ctx, `SELECT `+documentCols+` FROM documents WHERE legacy_id = ?`, f.ID))
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return d, "", err
	}
	// Copy the content only for a new document, or one whose file was
	// missing last time: a re-run never duplicates files.
	var stored string
	haveFile := found && existing.SHA256 != ""
	if !haveFile {
		saved, ok, err := im.copyFile(f, res)
		if err != nil {
			return d, "", err
		}
		if ok {
			d.StoredName, d.SizeBytes, d.SHA256 = saved.StoredName, saved.Size, saved.SHA256
			stored = saved.StoredName
		} else {
			res.Missing = append(res.Missing, f.ID)
			d.StoredName = NewStoredName()
			d.Description = withMissingNote(d.Description)
			d.ArchivedAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
		}
	}
	if !found {
		if d.UploadedAt.IsZero() {
			d.UploadedAt = time.Now().UTC()
		}
		if err := insertDocument(ctx, tx, &d); err != nil {
			return d, stored, err
		}
		res.DocumentsCreated++
		return d, stored, nil
	}
	d.ID = existing.ID
	if haveFile {
		d.StoredName, d.SizeBytes, d.SHA256, d.ArchivedAt = existing.StoredName, existing.SizeBytes, existing.SHA256, existing.ArchivedAt
	} else if stored == "" { // still missing
		d.StoredName = existing.StoredName
	}
	if d.UploadedAt.IsZero() {
		d.UploadedAt = existing.UploadedAt
	}
	changed := existing.CategoryID != d.CategoryID || existing.Title != d.Title || existing.Description != d.Description ||
		existing.Access != d.Access || existing.FileName != d.FileName || existing.MimeType != d.MimeType ||
		existing.LegacyPath != d.LegacyPath || existing.StoredName != d.StoredName || existing.SHA256 != d.SHA256 ||
		existing.ArchivedAt.Valid != d.ArchivedAt.Valid || !existing.UploadedAt.Equal(d.UploadedAt.UTC().Truncate(time.Second))
	if !changed {
		return d, stored, nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE documents SET category_id = ?, title = ?, description = ?, file_name = ?, stored_name = ?,
		mime_type = ?, size_bytes = ?, sha256 = ?, access = ?, uploaded_at = ?, legacy_path = ?, archived_at = ? WHERE id = ?`,
		d.CategoryID, d.Title, d.Description, d.FileName, d.StoredName, d.MimeType, d.SizeBytes, d.SHA256, d.Access,
		d.UploadedAt.UTC(), d.LegacyPath, d.ArchivedAt, d.ID)
	if err == nil {
		res.DocumentsUpdated++
	}
	return d, stored, err
}

// copyFile copies a legacy file into DOCUMENTS_DIR and verifies the copy.
// ok is false when the file is missing. A dry run only checks it exists.
func (im *Importer) copyFile(f LegacyFile, res *ImportResult) (Saved, bool, error) {
	src, ok := im.source(f.Path)
	if !ok {
		im.Log.WithField("file_id", f.ID).Warn("legacy file path outside the files directory")
		return Saved{}, false, nil
	}
	st, err := os.Stat(src)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !st.Mode().IsRegular()) {
		im.Log.WithField("file_id", f.ID).Warn("legacy file missing")
		return Saved{}, false, nil
	} else if err != nil {
		return Saved{}, false, err
	}
	if im.DryRun {
		if st.Size() != f.Size {
			res.Mismatched = append(res.Mismatched, f.ID)
		}
		res.FilesCopied++
		res.BytesCopied += st.Size()
		return Saved{StoredName: NewStoredName(), Size: st.Size(), SHA256: "dry-run"}, true, nil
	}
	in, err := os.Open(src)
	if err != nil {
		return Saved{}, false, err
	}
	defer in.Close()
	sum := md5.New()
	saved, err := im.Files.Save(in, 0, sum)
	if err != nil {
		return Saved{}, false, err
	}
	// Verify the copy against what was read, and the catalogue.
	if h, n, err := im.Files.Hash(saved.StoredName); err != nil || h != saved.SHA256 || n != saved.Size || n != st.Size() {
		_ = im.Files.Remove(saved.StoredName)
		return Saved{}, false, fmt.Errorf("copy verification failed (err=%v)", err)
	}
	if saved.Size != f.Size || (f.MD5 != "" && !strings.EqualFold(hex.EncodeToString(sum.Sum(nil)), f.MD5)) {
		im.Log.WithField("file_id", f.ID).Warn("legacy file differs from the catalogue (size or md5); imported as found")
		res.Mismatched = append(res.Mismatched, f.ID)
	}
	res.FilesCopied++
	res.BytesCopied += saved.Size
	return saved, true, nil
}
