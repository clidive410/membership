package documents

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clidive410/membership/internal/auth"
)

func id(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }

var archived = sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}

func testLibrary() *Library {
	cats := []Category{
		{ID: 1, Name: "Public root", Access: Public},
		{ID: 2, ParentID: id(1), Name: "Members child", Access: Members},
		{ID: 3, ParentID: id(2), Name: "Public grandchild", Access: Public}, // capped by its parent
		{ID: 4, Name: "Committee", Access: Committee},
		{ID: 5, ParentID: id(4), Name: "Archived", Access: Committee, ArchivedAt: archived},
		{ID: 6, ParentID: id(5), Name: "Under archived", Access: Committee},
		{ID: 7, ParentID: id(99), Name: "Orphan", Access: Public},
		{ID: 8, ParentID: id(9), Name: "Cycle a", Access: Public},
		{ID: 9, ParentID: id(8), Name: "Cycle b", Access: Public},
	}
	return NewLibrary(cats, nil)
}

func TestEffectiveAccess(t *testing.T) {
	l := testLibrary()
	cases := []struct {
		doc  Document
		want Access
	}{
		{Document{CategoryID: id(1), Access: Inherit}, Public},
		{Document{CategoryID: id(2), Access: Inherit}, Members},
		{Document{CategoryID: id(3), Access: Inherit}, Members},
		{Document{CategoryID: id(4), Access: Inherit}, Committee},
		{Document{CategoryID: id(4), Access: Public}, Public}, // explicit beats the category
		{Document{CategoryID: id(1), Access: Committee}, Committee},
		{Document{Access: Inherit}, Committee}, // top level: fail closed
		{Document{CategoryID: id(7), Access: Inherit}, Committee},
		{Document{CategoryID: id(8), Access: Inherit}, Committee},
		{Document{CategoryID: id(42), Access: Inherit}, Committee},
		{Document{CategoryID: id(1), Access: "bogus"}, Committee},
	}
	for i, c := range cases {
		if got := l.EffectiveAccess(&c.doc); got != c.want {
			t.Errorf("case %d: got %s, want %s", i, got, c.want)
		}
	}
	if !l.CategoryArchived(6) || !l.CategoryArchived(5) || l.CategoryArchived(4) {
		t.Error("archived state should be inherited by subcategories only")
	}
	if !l.Hidden(&Document{CategoryID: id(6)}) || !l.Hidden(&Document{CategoryID: id(1), ArchivedAt: archived}) || l.Hidden(&Document{CategoryID: id(1)}) {
		t.Error("Hidden")
	}
}

func TestViewerLevelAndCanView(t *testing.T) {
	l := testLibrary()
	member := &auth.Principal{UserID: 1, Roles: []auth.Role{auth.RoleMember}}
	editor := &auth.Principal{UserID: 2, Roles: []auth.Role{auth.RoleEditor}}
	mgr := &auth.Principal{UserID: 3, Roles: []auth.Role{auth.RoleMembershipManager}}
	admin := &auth.Principal{UserID: 4, Roles: []auth.Role{auth.RoleAdmin}}
	for p, want := range map[*auth.Principal]Access{nil: Public, member: Members, editor: Members, mgr: Committee, admin: Committee} {
		if got := ViewerLevel(p); got != want {
			t.Errorf("ViewerLevel(%v) = %s, want %s", p, got, want)
		}
	}
	pub := &Document{CategoryID: id(1), Access: Inherit}
	mem := &Document{CategoryID: id(2), Access: Inherit}
	com := &Document{CategoryID: id(4), Access: Inherit}
	gone := &Document{CategoryID: id(6), Access: Public}
	for _, c := range []struct {
		level Access
		doc   *Document
		want  bool
	}{
		{Public, pub, true}, {Public, mem, false}, {Public, com, false},
		{Members, pub, true}, {Members, mem, true}, {Members, com, false},
		{Committee, com, true}, {Committee, gone, false},
	} {
		if got := l.CanView(c.level, c.doc); got != c.want {
			t.Errorf("CanView(%s, cat %d) = %v", c.level, c.doc.CategoryID.Int64, got)
		}
	}
}

func TestParseAccess(t *testing.T) {
	if _, ok := ParseAccess("inherit", false); ok {
		t.Error("categories cannot inherit")
	}
	if a, ok := ParseAccess("inherit", true); !ok || a != Inherit {
		t.Error("documents can inherit")
	}
	if _, ok := ParseAccess("everyone", true); ok {
		t.Error("unknown level accepted")
	}
}

func TestAllowList(t *testing.T) {
	for _, n := range strings.Fields(AllowedExtensions) {
		if _, ok := AllowedUpload("file." + strings.ToUpper(n)); !ok {
			t.Errorf("%s should be allowed", n)
		}
	}
	for _, n := range []string{"page.html", "x.svg", "run.exe", "noext", "a.pdf.php", "movie.mp4", ".pdf "} {
		if _, ok := AllowedUpload(n); ok {
			t.Errorf("%q should be rejected", n)
		}
	}
	if MimeFor("Minutes.PDF") != "application/pdf" || MimeFor("old.html") != FallbackMime || MimeFor("x") != FallbackMime {
		t.Error("MimeFor")
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"AGM minutes 2013.pdf":            "AGM minutes 2013.pdf",
		`C:\Users\x\report.docx`:          "report.docx",
		"../../etc/passwd":                "passwd",
		"a\"b;c\r\nd.pdf":                 "a_b_c__d.pdf",
		"":                                "document",
		"...":                             "document",
		"Café plan.pdf":                   "Café plan.pdf",
		strings.Repeat("x", 300) + ".pdf": strings.Repeat("x", 146) + ".pdf",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilesSaveOpen(t *testing.T) {
	f := Files{Dir: filepath.Join(t.TempDir(), "docs")}
	s, err := f.Save(strings.NewReader("hello"), 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Size != 5 || s.SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" || !storedNameRE.MatchString(s.StoredName) {
		t.Fatalf("saved %+v", s)
	}
	fh, err := f.Open(s.StoredName)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.ReadFrom(fh)
	fh.Close()
	if b.String() != "hello" {
		t.Fatal(b.String())
	}
	if _, err := f.Save(strings.NewReader("too long!!!"), 10, nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	if _, err := f.Open("../secret"); err == nil {
		t.Fatal("traversal accepted")
	}
	entries, _ := os.ReadDir(f.Dir)
	if len(entries) != 1 {
		t.Fatalf("left %d files, want 1 (temporary files removed)", len(entries))
	}
	if err := f.Remove(s.StoredName); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyMapping(t *testing.T) {
	committee := "administrator|editor|author|contributor|subscriber|instructor|committee"
	if legacyCategoryAccess(LegacyCategory{Roles: ""}) != Committee || legacyCategoryAccess(LegacyCategory{Roles: committee}) != Committee {
		t.Error("categories are committee only")
	}
	if legacyFileAccess(LegacyFile{Roles: " "}) != Public || legacyFileAccess(LegacyFile{Roles: "instructor"}) != Inherit {
		t.Error("file access mapping")
	}
	date := time.Date(2014, 11, 1, 16, 3, 52, 0, time.UTC)
	d := mapFile(LegacyFile{ID: 2, Name: "2012.02 Newsplash.pdf", Path: "newsplash/2012/2012.02 Newsplash.pdf", Size: 10,
		Date: date, DisplayName: " ", Description: " Feb ", Roles: committee}, id(4))
	if d.Title != "2012.02 Newsplash.pdf" || d.Description != "Feb" || d.MimeType != "application/pdf" || d.Access != Inherit ||
		d.LegacyID.Int64 != 2 || d.LegacyPath != "newsplash/2012/2012.02 Newsplash.pdf" || !d.UploadedAt.Equal(date) || d.CategoryID.Int64 != 4 {
		t.Errorf("mapFile: %+v", d)
	}
	if d := mapFile(LegacyFile{Name: "x.html", DisplayName: "Shown"}, sql.NullInt64{}); d.Title != "Shown" || d.MimeType != FallbackMime {
		t.Errorf("mapFile title/mime: %+v", d)
	}
	c := mapCategory(LegacyCategory{ID: 16, Name: "BSAC Courses (not updated)", Order: 3}, id(1))
	if c.LegacyKey.String != "wpfb-cat:16" || c.Access != Committee || c.SortOrder != 3 || c.ParentID.Int64 != 1 {
		t.Errorf("mapCategory: %+v", c)
	}
	if !movedToArchive["BSAC Courses (not updated)"] {
		t.Error("the course category moves to Archive")
	}
	if got := withMissingNote(withMissingNote("Slides")); got != "Slides (file missing in migration)" {
		t.Errorf("withMissingNote: %q", got)
	}
	order := orderCategories([]LegacyCategory{{ID: 3, Parent: 2}, {ID: 2, Parent: 1}, {ID: 1}, {ID: 5, Parent: 6}, {ID: 6, Parent: 5}, {ID: 7, Parent: 99}})
	var ids []int64
	for _, c := range order {
		ids = append(ids, c.ID)
	}
	if len(ids) != 6 || ids[0] != 1 || ids[1] != 2 || ids[2] != 3 {
		t.Errorf("orderCategories: %v", ids)
	}
}

func TestImporterSource(t *testing.T) {
	im := &Importer{FilesDir: "/legacy"}
	if p, ok := im.source("agm/2013/minutes.pdf"); !ok || p != "/legacy/agm/2013/minutes.pdf" {
		t.Errorf("source: %q %v", p, ok)
	}
	for _, bad := range []string{"../etc/passwd", "", "agm/../../x"} {
		if _, ok := im.source(bad); ok {
			t.Errorf("source(%q) accepted", bad)
		}
	}
}

func TestSlugify(t *testing.T) {
	if Slugify("AGM Documents - 2013!") != "agm-documents-2013" || Slugify("***") != "category" {
		t.Error("Slugify")
	}
}
