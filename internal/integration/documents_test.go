package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clidive410/membership/internal/documents"
	"github.com/clidive410/membership/internal/members"
)

// TestDocumentsAgainstMariaDB runs the store and the legacy import (twice)
// on invented catalogue rows and files.
func TestDocumentsAgainstMariaDB(t *testing.T) {
	f := setup(t)
	ctx := f.ctx
	legacyDir, docsDir := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(legacyDir, "agm", "2013"), 0o755)
	os.WriteFile(filepath.Join(legacyDir, "agm", "2013", "minutes.pdf"), []byte("minutes"), 0o644)
	os.WriteFile(filepath.Join(legacyDir, "welcome.pdf"), []byte("welcome"), 0o644)
	date := time.Date(2014, 11, 1, 16, 3, 52, 0, time.UTC)
	cats := []documents.LegacyCategory{
		{ID: 10, Parent: 9, Name: "2013", Roles: "committee"}, // child listed before its parent
		{ID: 9, Name: "AGM Documents", Roles: "committee"},
		{ID: 16, Name: "BSAC Courses (not updated)", Roles: "instructor"},
	}
	files := []documents.LegacyFile{
		{ID: 1, Name: "minutes.pdf", Path: "agm/2013/minutes.pdf", Size: 7, Date: date, MD5: "", DisplayName: "AGM minutes", Roles: "committee", Category: 10},
		{ID: 2, Name: "welcome.pdf", Path: "welcome.pdf", Size: 7, Date: date, Roles: "", Category: 16},
		{ID: 3, Name: "gone.ppt", Path: "courses/gone.ppt", Size: 99, Date: date, Roles: "instructor", Category: 16},
	}
	run := func(dry bool) documents.ImportResult {
		t.Helper()
		res, err := (&documents.Importer{DB: f.app, Files: documents.Files{Dir: docsDir}, FilesDir: legacyDir, DryRun: dry}).Run(ctx, cats, files)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run(true); res.DocumentsCreated != 3 || res.FilesCopied != 2 {
		t.Fatalf("dry run: %+v", res)
	}
	store := documents.NewStore(f.app)
	if lib, _ := store.Library(ctx); len(lib.Documents) != 0 || len(lib.Categories) != 0 {
		t.Fatal("dry run wrote rows")
	}
	if entries, _ := os.ReadDir(docsDir); len(entries) != 0 {
		t.Fatal("dry run copied files")
	}
	res := run(false)
	if res.CategoriesCreated != 4 || res.DocumentsCreated != 3 || res.FilesCopied != 2 || len(res.Missing) != 1 ||
		res.ByAccess[documents.Public] != 1 || res.ByAccess[documents.Committee] != 2 {
		t.Fatalf("import: %+v", res)
	}
	again := run(false)
	if again.CategoriesCreated != 0 || again.DocumentsCreated != 0 || again.FilesCopied != 0 || again.DocumentsUpdated != 0 ||
		again.CategoriesUpdated != 0 || len(again.Missing) != 1 {
		t.Fatalf("re-run: %+v", again)
	}
	if entries, _ := os.ReadDir(docsDir); len(entries) != 2 {
		t.Fatalf("files in DOCUMENTS_DIR: %d", len(entries))
	}
	lib, err := store.Library(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var courses, archive *documents.Category
	for i := range lib.Categories {
		switch lib.Categories[i].Name {
		case "BSAC Courses (not updated)":
			courses = &lib.Categories[i]
		case "Archive":
			archive = &lib.Categories[i]
		}
	}
	if courses == nil || archive == nil || courses.ParentID.Int64 != archive.ID || archive.ParentID.Valid {
		t.Fatalf("archive move: %+v %+v", courses, archive)
	}
	for _, d := range lib.Documents {
		switch d.LegacyID.Int64 {
		case 1:
			if d.Title != "AGM minutes" || lib.EffectiveAccess(&d) != documents.Committee || d.SizeBytes != 7 || !d.UploadedAt.Equal(date) ||
				lib.Path(d.CategoryID.Int64)[0].Slug != "agm-documents" || lib.Category(d.CategoryID.Int64).Slug != "agm-documents-2013" {
				t.Errorf("doc 1: %+v", d)
			}
			if h, _, err := (documents.Files{Dir: docsDir}).Hash(d.StoredName); err != nil || h != d.SHA256 {
				t.Errorf("stored file hash: %v", err)
			}
		case 2:
			if lib.EffectiveAccess(&d) != documents.Public || !lib.CanView(documents.Public, &d) {
				t.Errorf("doc 2 should be public: %+v", d)
			}
		case 3:
			if !d.ArchivedAt.Valid || d.SHA256 != "" || !strings.Contains(d.Description, "(file missing in migration)") {
				t.Errorf("missing doc: %+v", d)
			}
		}
	}

	// Store mutations write the audit log.
	a := members.Actor{Label: "tester"}
	c := &documents.Category{Name: "AGM Documents", Access: documents.Members}
	if err := store.CreateCategory(ctx, a, c); err != nil || c.Slug != "agm-documents-2" {
		t.Fatalf("create category: %v %q", err, c.Slug)
	}
	c.SortOrder = 3
	if err := store.UpdateCategory(ctx, a, c); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCategoryArchived(ctx, a, c.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCategoryArchived(ctx, a, 999999, true); err != documents.ErrNotFound {
		t.Fatalf("archive unknown: %v", err)
	}
	doc := lib.Documents[0]
	doc.Title, doc.Access = "Renamed", documents.Members
	if err := store.UpdateDocument(ctx, a, &doc); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDownload(ctx, a, doc.ID); err != nil {
		t.Fatal(err)
	}
	saved, _ := (documents.Files{Dir: docsDir}).Save(strings.NewReader("v2"), 0, nil)
	if old, err := store.ReplaceFile(ctx, a, doc.ID, "v2.pdf", "application/pdf", saved, 0); err != nil || old != doc.StoredName {
		t.Fatalf("replace: %v", err)
	}
	got, _ := store.Document(ctx, doc.ID)
	if got.Title != "Renamed" || got.Downloads != 1 || got.SizeBytes != 2 || got.StoredName != saved.StoredName {
		t.Errorf("after edits: %+v", got)
	}
	var n int
	f.app.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE actor = 'tester' AND entity IN ('document', 'document_category')`).Scan(&n)
	if n != 6 {
		t.Errorf("audit rows: %d, want 6", n)
	}
}
