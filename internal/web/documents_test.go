package web

import (
	"bytes"
	"context"
	"database/sql"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/documents"
	"github.com/clidive410/membership/internal/members"
)

// fakeDocs is an in-memory DocumentStore.
type fakeDocs struct {
	mu    sync.Mutex
	cats  []documents.Category
	docs  []documents.Document
	audit []string // "actor action id"
	next  int64
}

func (f *fakeDocs) log(a members.Actor, action string, id int64) {
	f.audit = append(f.audit, a.Label+" "+action+" "+itoa(id))
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func (f *fakeDocs) Library(context.Context) (*documents.Library, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return documents.NewLibrary(append([]documents.Category(nil), f.cats...), append([]documents.Document(nil), f.docs...)), nil
}

func (f *fakeDocs) CreateCategory(_ context.Context, a members.Actor, c *documents.Category) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	c.ID, c.Slug = f.next, documents.Slugify(c.Name)
	f.cats = append(f.cats, *c)
	f.log(a, documents.ActionCategoryCreate, c.ID)
	return nil
}

func (f *fakeDocs) cat(id int64) *documents.Category {
	for i := range f.cats {
		if f.cats[i].ID == id {
			return &f.cats[i]
		}
	}
	return nil
}

func (f *fakeDocs) doc(id int64) *documents.Document {
	for i := range f.docs {
		if f.docs[i].ID == id {
			return &f.docs[i]
		}
	}
	return nil
}

func (f *fakeDocs) UpdateCategory(_ context.Context, a members.Actor, c *documents.Category) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	old := f.cat(c.ID)
	if old == nil {
		return documents.ErrNotFound
	}
	c.Slug, c.ArchivedAt = old.Slug, old.ArchivedAt
	*old = *c
	f.log(a, documents.ActionCategoryUpdate, c.ID)
	return nil
}

func (f *fakeDocs) SetCategoryArchived(_ context.Context, a members.Actor, id int64, archive bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cat(id)
	if c == nil {
		return documents.ErrNotFound
	}
	c.ArchivedAt = sql.NullTime{Time: fixedNow, Valid: archive}
	f.log(a, documents.ActionCategoryArchive, id)
	return nil
}

func (f *fakeDocs) CreateDocument(_ context.Context, a members.Actor, d *documents.Document) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	d.ID = f.next
	f.docs = append(f.docs, *d)
	f.log(a, documents.ActionCreate, d.ID)
	return nil
}

func (f *fakeDocs) UpdateDocument(_ context.Context, a members.Actor, d *documents.Document) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	old := f.doc(d.ID)
	if old == nil {
		return documents.ErrNotFound
	}
	old.Title, old.Description, old.CategoryID, old.Access = d.Title, d.Description, d.CategoryID, d.Access
	f.log(a, documents.ActionUpdate, d.ID)
	return nil
}

func (f *fakeDocs) ReplaceFile(_ context.Context, a members.Actor, id int64, name, mimeType string, s documents.Saved, by int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.doc(id)
	if d == nil {
		return "", documents.ErrNotFound
	}
	old := d.StoredName
	d.FileName, d.MimeType, d.StoredName, d.SizeBytes, d.SHA256 = name, mimeType, s.StoredName, s.Size, s.SHA256
	f.log(a, documents.ActionReplaceFile, id)
	return old, nil
}

func (f *fakeDocs) SetDocumentArchived(_ context.Context, a members.Actor, id int64, archive bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.doc(id)
	if d == nil {
		return documents.ErrNotFound
	}
	d.ArchivedAt = sql.NullTime{Time: fixedNow, Valid: archive}
	f.log(a, documents.ActionArchive, id)
	return nil
}

func (f *fakeDocs) RecordDownload(_ context.Context, a members.Actor, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.doc(id).Downloads++
	f.log(a, documents.ActionDownload, id)
	return nil
}

type docEnv struct {
	*env
	fd    *fakeDocs
	files documents.Files
}

func nid(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }

// newDocEnv adds a small library: a public, a members and a committee
// category, one document in each, an archived one and a public document in
// the committee category.
func newDocEnv(t *testing.T, mods ...func(*Options)) *docEnv {
	t.Helper()
	fd := &fakeDocs{next: 100}
	files := documents.Files{Dir: t.TempDir()}
	store := func(content string) documents.Saved {
		s, err := files.Save(strings.NewReader(content), 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	fd.cats = []documents.Category{
		{ID: 1, Name: "Club info", Slug: "club-info", Access: documents.Public},
		{ID: 2, Name: "Newsletters", Slug: "newsletters", Access: documents.Members},
		{ID: 3, Name: "Committee papers", Slug: "committee-papers", Access: documents.Committee},
	}
	add := func(id, cat int64, title string, access documents.Access, archived bool) {
		s := store("content of " + title)
		fd.docs = append(fd.docs, documents.Document{ID: id, CategoryID: nid(cat), Title: title, FileName: title + ".pdf",
			StoredName: s.StoredName, MimeType: "application/pdf", SizeBytes: s.Size, SHA256: s.SHA256, Access: access,
			UploadedAt: fixedNow, ArchivedAt: sql.NullTime{Time: fixedNow, Valid: archived}})
	}
	add(11, 1, "Welcome pack", documents.Inherit, false)
	add(12, 2, "Newsplash June", documents.Inherit, false)
	add(13, 3, "Minutes secret", documents.Inherit, false)
	add(14, 1, "Old leaflet", documents.Inherit, true)
	add(15, 3, "Public flyer", documents.Public, false)
	e := newEnv(t, "", append([]func(*Options){func(o *Options) { o.Documents, o.DocumentFiles = fd, files }}, mods...)...)
	e.fa.addUser("ed@example.org", "editor-password-12", 0, auth.RoleEditor)
	return &docEnv{env: e, fd: fd, files: files}
}

func TestDocumentsEmptyLibrary(t *testing.T) {
	e := newEnv(t, "", func(o *Options) { o.Documents, o.DocumentFiles = &fakeDocs{}, documents.Files{Dir: t.TempDir()} })
	for _, session := range []string{"", e.fa.session("ada@example.org"), e.fa.session("admin@example.org")} {
		w := e.do(t, "GET", "/documents", session, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "No documents yet") {
			t.Errorf("empty library: %d", w.Code)
		}
	}
	if w := e.do(t, "GET", "/admin/documents", e.fa.session("admin@example.org"), nil); w.Code != 200 {
		t.Errorf("empty manage page: %d", w.Code)
	}
}

func TestDocumentsVisibility(t *testing.T) {
	e := newDocEnv(t)
	cases := []struct {
		who       string
		see, hide []string
	}{
		{"", []string{"Welcome pack", "Public flyer", "Club info", "Committee papers"}, []string{"Newsplash June", "Minutes secret", "Old leaflet", "Newsletters"}},
		{"ada@example.org", []string{"Welcome pack", "Newsplash June", "Public flyer"}, []string{"Minutes secret", "Old leaflet"}},
		{"bob@example.org", []string{"Newsplash June", "expired on"}, []string{"Minutes secret"}},
		{"ed@example.org", []string{"Newsplash June"}, []string{"Minutes secret"}},
		{"manager@example.org", []string{"Welcome pack", "Newsplash June", "Minutes secret", "Public flyer"}, []string{"Old leaflet"}},
	}
	for _, c := range cases {
		session := ""
		if c.who != "" {
			session = e.fa.session(c.who)
		}
		// Search lists every visible document; the tree shows categories.
		w := e.do(t, "GET", "/documents?q=e", session, nil)
		if w.Code != 200 {
			t.Fatalf("%s: %d", c.who, w.Code)
		}
		body := w.Body.String()
		for _, s := range c.see {
			if !strings.Contains(body, s) {
				t.Errorf("%q should see %q", c.who, s)
			}
		}
		for _, s := range c.hide {
			if strings.Contains(body, s) {
				t.Errorf("%q should not see %q", c.who, s)
			}
		}
	}
	if w := e.do(t, "GET", "/documents?cat=newsletters", "", nil); w.Code != 404 {
		t.Errorf("visitor opening a members category: %d", w.Code)
	}
	w := e.do(t, "GET", "/documents?cat=newsletters", e.fa.session("ada@example.org"), nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Newsplash June") || strings.Contains(w.Body.String(), "Welcome pack") {
		t.Errorf("member in a category: %d", w.Code)
	}
}

func TestDocumentDownload(t *testing.T) {
	e := newDocEnv(t)
	member, mgr := e.fa.session("ada@example.org"), e.fa.session("manager@example.org")
	w := e.do(t, "GET", "/documents/11/download", "", nil)
	if w.Code != 200 || w.Body.String() != "content of Welcome pack" {
		t.Fatalf("public download: %d %q", w.Code, w.Body.String())
	}
	h := w.Header()
	if h.Get("Content-Disposition") != `attachment; filename="Welcome pack.pdf"` || h.Get("Content-Type") != "application/pdf" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", h)
	}
	cases := []struct {
		session, path string
		want          int
	}{
		{"", "/documents/12/download", 303},                              // members: sign in first
		{"", "/documents/13/download", 303},                              // committee: sign in first
		{"", "/documents/15/download", 200},                              // public document in a committee category
		{"", "/documents/14/download", 404},                              // archived
		{"", "/documents/999/download", 404},                             // unknown
		{member, "/documents/12/download", 200},                          // members
		{member, "/documents/13/download", 404},                          // committee
		{e.fa.session("bob@example.org"), "/documents/12/download", 200}, // expired member
		{mgr, "/documents/13/download", 200},
		{mgr, "/documents/14/download", 404}, // archived, even for the committee
	}
	for _, c := range cases {
		if w := e.do(t, "GET", c.path, c.session, nil); w.Code != c.want {
			t.Errorf("GET %s (%q): %d, want %d", c.path, c.session, w.Code, c.want)
		}
	}
	if d := e.fd.doc(12); d.Downloads != 2 {
		t.Errorf("downloads of 12: %d", d.Downloads)
	}
	if !strings.Contains(strings.Join(e.fd.audit, "\n"), "ada@example.org document.download 12") || !strings.Contains(strings.Join(e.fd.audit, "\n"), "anonymous document.download 11") {
		t.Errorf("audit: %v", e.fd.audit)
	}
	// A file missing on disk is reported, not a server error.
	e.fd.doc(11).StoredName = documents.NewStoredName()
	if w := e.do(t, "GET", "/documents/11/download", "", nil); w.Code != 404 {
		t.Errorf("missing file: %d", w.Code)
	}
}

func TestDocumentsManagePermissions(t *testing.T) {
	e := newDocEnv(t)
	for who, want := range map[string]int{"ada@example.org": 403, "ed@example.org": 403, "manager@example.org": 200, "admin@example.org": 200} {
		s := e.fa.session(who)
		for _, path := range []string{"/admin/documents", "/admin/documents/13", "/admin/documents/categories/3"} {
			if w := e.do(t, "GET", path, s, nil); w.Code != want {
				t.Errorf("%s GET %s: %d, want %d", who, path, w.Code, want)
			}
		}
	}
	member := e.fa.session("ada@example.org")
	for _, path := range []string{"/admin/documents/13", "/admin/documents/13/archive", "/admin/documents/categories", "/admin/documents/categories/3/archive"} {
		if w := e.do(t, "POST", path, member, url.Values{"name": {"x"}}); w.Code != 403 {
			t.Errorf("member POST %s: %d", path, w.Code)
		}
	}
	w := e.do(t, "GET", "/documents?cat=committee-papers", e.fa.session("manager@example.org"), nil)
	if !strings.Contains(w.Body.String(), `href="/admin/documents?cat=3"`) {
		t.Error("a category page should link to managing that category")
	}
	w = e.do(t, "GET", "/admin/documents?cat=3", e.fa.session("manager@example.org"), nil)
	if !strings.Contains(w.Body.String(), `href="/admin/documents/categories/3"`) || !strings.Contains(w.Body.String(), "Minutes secret") || strings.Contains(w.Body.String(), "Welcome pack") {
		t.Error("filtered manage list")
	}
	if w := e.do(t, "GET", "/admin/documents", "", nil); w.Code != 303 {
		t.Errorf("visitor: %d", w.Code)
	}
	if len(e.fd.audit) != 0 {
		t.Errorf("no mutation expected: %v", e.fd.audit)
	}
}

// upload posts a multipart form with a file.
func (e *docEnv) upload(t *testing.T, path, session, fileName, content string, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if _, ok := fields["csrf_token"]; !ok {
		mw.WriteField("csrf_token", auth.CSRF{Secret: secret}.Token(browser))
	}
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	if fileName != "" {
		fw, _ := mw.CreateFormFile("file", fileName)
		fw.Write([]byte(content))
	}
	mw.Close()
	r := httptest.NewRequest("POST", path, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: browser})
	if session != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func TestDocumentUpload(t *testing.T) {
	e := newDocEnv(t, func(o *Options) { o.MaxUploadBytes = 1 << 10 })
	mgr := e.fa.session("manager@example.org")
	w := e.upload(t, "/admin/documents", mgr, `..\AGM "2026".PDF`, "%PDF-1.4 minutes", map[string]string{"category": "3", "access": "inherit", "description": "Draft"})
	if w.Code != 303 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	d := e.fd.docs[len(e.fd.docs)-1]
	if d.Title != "AGM _2026_" || d.FileName != "AGM _2026_.PDF" || d.MimeType != "application/pdf" || d.SizeBytes != 16 || len(d.SHA256) != 64 ||
		d.CategoryID.Int64 != 3 || d.Access != documents.Inherit || d.Description != "Draft" || !d.UploadedBy.Valid {
		t.Errorf("uploaded: %+v", d)
	}
	if !strings.HasSuffix(e.fd.audit[len(e.fd.audit)-1], "document.create "+itoa(d.ID)) {
		t.Errorf("audit: %v", e.fd.audit)
	}
	if w := e.upload(t, "/admin/documents", mgr, "page.html", "<script>", nil); w.Code != 422 || !strings.Contains(w.Body.String(), "not accepted") {
		t.Errorf("html upload: %d", w.Code)
	}
	if w := e.upload(t, "/admin/documents", mgr, "big.pdf", strings.Repeat("x", 3<<10), nil); w.Code != 413 {
		t.Errorf("too large: %d", w.Code)
	}
	if w := e.upload(t, "/admin/documents", mgr, "a.pdf", "x", map[string]string{"csrf_token": "forged"}); w.Code != 403 {
		t.Errorf("forged csrf: %d", w.Code)
	}
	if w := e.upload(t, "/admin/documents", e.fa.session("ada@example.org"), "a.pdf", "x", nil); w.Code != 403 {
		t.Errorf("member upload: %d", w.Code)
	}
	n := len(e.fd.docs)
	if w := e.upload(t, "/admin/documents", mgr, "a.pdf", "x", map[string]string{"access": "everyone"}); w.Code != 422 || len(e.fd.docs) != n {
		t.Errorf("bad access: %d", w.Code)
	}

	// Replace the file: the old one is deleted.
	old := e.fd.doc(13).StoredName
	if w := e.upload(t, "/admin/documents/13/file", mgr, "minutes-v2.docx", "v2", nil); w.Code != 303 {
		t.Fatalf("replace: %d", w.Code)
	}
	if d := e.fd.doc(13); d.FileName != "minutes-v2.docx" || d.SizeBytes != 2 || d.StoredName == old {
		t.Errorf("replaced: %+v", d)
	}
	if _, err := e.files.Open(old); err == nil {
		t.Error("old file kept")
	}
}

func TestDocumentEditAndArchive(t *testing.T) {
	e := newDocEnv(t)
	admin := e.fa.session("admin@example.org")
	w := e.do(t, "POST", "/admin/documents/12", admin, url.Values{"title": {"June news"}, "category": {"1"}, "access": {"committee"}})
	if w.Code != 303 {
		t.Fatalf("update: %d", w.Code)
	}
	if d := e.fd.doc(12); d.Title != "June news" || d.CategoryID.Int64 != 1 || d.Access != documents.Committee {
		t.Errorf("updated: %+v", d)
	}
	if w := e.do(t, "POST", "/admin/documents/12", admin, url.Values{"title": {""}, "access": {"inherit"}}); w.Code != 422 {
		t.Errorf("empty title: %d", w.Code)
	}
	if w := e.do(t, "POST", "/admin/documents/11/archive", admin, url.Values{}); w.Code != 303 || !e.fd.doc(11).ArchivedAt.Valid {
		t.Errorf("archive: %d", w.Code)
	}
	if w := e.do(t, "GET", "/documents/11/download", "", nil); w.Code != 404 {
		t.Errorf("archived download: %d", w.Code)
	}
	if w := e.do(t, "POST", "/admin/documents/11/restore", admin, url.Values{}); w.Code != 303 || e.fd.doc(11).ArchivedAt.Valid {
		t.Errorf("restore: %d", w.Code)
	}
}

func TestCategoryManage(t *testing.T) {
	e := newDocEnv(t)
	mgr := e.fa.session("manager@example.org")
	if w := e.do(t, "POST", "/admin/documents/categories", mgr, url.Values{"name": {"AGM 2026"}, "parent": {"3"}, "access": {"committee"}, "sort_order": {"5"}}); w.Code != 303 || w.Header().Get("Location") != "/admin/documents?cat=3&flash=cat-created" {
		t.Fatalf("create: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := e.do(t, "GET", "/admin/documents?cat=3", mgr, nil); !strings.Contains(w.Body.String(), `<input type="hidden" name="parent" value="3">`) || strings.Contains(w.Body.String(), `id="nc-parent"`) {
		t.Error("a new folder on a filtered page goes inside that folder")
	}
	c := e.fd.cats[len(e.fd.cats)-1]
	// The manage page browses folders: subfolders, then documents.
	body := e.do(t, "GET", "/admin/documents?cat=3", mgr, nil).Body.String()
	if !strings.Contains(body, `href="/admin/documents?cat=`+itoa(c.ID)+`">`) || !strings.Contains(body, "Minutes secret") ||
		strings.Contains(body, "Welcome pack") || strings.Contains(body, `href="/admin/documents?cat=1">`) {
		t.Error("a folder lists its subfolders and documents only")
	}
	if strings.Index(body, "AGM 2026") > strings.Index(body, "Minutes secret") {
		t.Error("subfolders come before documents")
	}
	body = e.do(t, "GET", "/admin/documents", mgr, nil).Body.String()
	if !strings.Contains(body, `href="/admin/documents?cat=1">`) || strings.Contains(body, "Minutes secret") || strings.Contains(body, `href="/admin/documents?cat=`+itoa(c.ID)+`">`) {
		t.Error("the top level lists top-level folders only")
	}
	body = e.do(t, "GET", "/admin/documents?q=minutes", mgr, nil).Body.String()
	if !strings.Contains(body, "Minutes secret") || !strings.Contains(body, "Committee papers") {
		t.Error("search lists matches from every folder, with their folder")
	}
	if c.Name != "AGM 2026" || c.ParentID.Int64 != 3 || c.SortOrder != 5 {
		t.Errorf("created: %+v", c)
	}
	// Moving a category under its own child is refused.
	if w := e.do(t, "POST", "/admin/documents/categories/3", mgr, url.Values{"name": {"Committee papers"}, "parent": {itoa(c.ID)}, "access": {"committee"}}); w.Code != 422 {
		t.Errorf("cycle: %d", w.Code)
	}
	if w := e.do(t, "POST", "/admin/documents/categories/3", mgr, url.Values{"name": {"x"}, "access": {"inherit"}}); w.Code != 422 {
		t.Errorf("inherit category: %d", w.Code)
	}
	if w := e.do(t, "POST", "/admin/documents/categories/1/archive", mgr, url.Values{}); w.Code != 303 {
		t.Fatalf("archive: %d", w.Code)
	}
	if w := e.do(t, "GET", "/documents/11/download", "", nil); w.Code != 404 {
		t.Errorf("document of an archived category: %d", w.Code)
	}
	if w := e.do(t, "POST", "/admin/documents/categories/99/archive", mgr, url.Values{}); w.Code != 404 {
		t.Errorf("unknown category: %d", w.Code)
	}
}

// Both pages render the same folder-and-document list.
func TestDocumentsSharedBrowser(t *testing.T) {
	e := newDocEnv(t)
	mgr := e.fa.session("manager@example.org")
	member := e.do(t, "GET", "/documents", mgr, nil).Body.String()
	admin := e.do(t, "GET", "/admin/documents", mgr, nil).Body.String()
	for name, body := range map[string]string{"library": member, "manage": admin} {
		if !strings.Contains(body, `aria-labelledby="browse-h"`) || !strings.Contains(body, "All folders") || !strings.Contains(body, "Committee papers") {
			t.Errorf("%s page does not use the shared list", name)
		}
	}
	if !strings.Contains(member, `href="/documents?cat=committee-papers"`) || !strings.Contains(admin, `href="/admin/documents?cat=3"`) {
		t.Error("folder rows link within their own page")
	}
	inside := e.do(t, "GET", "/documents?cat=committee-papers", mgr, nil).Body.String()
	if !strings.Contains(inside, `href="/documents/13/download"`) || !strings.Contains(inside, `<a href="/documents">All folders</a> › Committee papers`) {
		t.Error("folder view: breadcrumb and download links")
	}
}
