package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/documents"
	"github.com/clidive410/membership/internal/members"
)

// DocumentStore is the subset of *documents.Store used by the handlers.
type DocumentStore interface {
	Library(ctx context.Context) (*documents.Library, error)
	CreateCategory(ctx context.Context, a members.Actor, c *documents.Category) error
	UpdateCategory(ctx context.Context, a members.Actor, c *documents.Category) error
	SetCategoryArchived(ctx context.Context, a members.Actor, id int64, archive bool) error
	CreateDocument(ctx context.Context, a members.Actor, d *documents.Document) error
	UpdateDocument(ctx context.Context, a members.Actor, d *documents.Document) error
	ReplaceFile(ctx context.Context, a members.Actor, id int64, fileName, mimeType string, saved documents.Saved, by int64) (string, error)
	SetDocumentArchived(ctx context.Context, a members.Actor, id int64, archive bool) error
	RecordDownload(ctx context.Context, a members.Actor, id int64) error
}

// longRequestTimeout bounds uploads and downloads, which bypass the
// buffering http.TimeoutHandler and extend the connection deadlines.
const longRequestTimeout = 15 * time.Minute

// Paths of uploads (multipart, large bodies) and downloads (streamed).
var (
	downloadPathRE = regexp.MustCompile(`^/documents/[0-9]+/download$`)
	uploadPathRE   = regexp.MustCompile(`^/admin/documents(/[0-9]+/file)?$`)
)

// longRequest reports whether r is an upload or a download; path is
// without the base path.
func longRequest(r *http.Request, path string) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return downloadPathRE.MatchString(path)
	case http.MethodPost:
		return uploadPathRE.MatchString(path)
	}
	return false
}

func (s *Server) documentRoutes(r *mux.Router, route func(method, path string, perm auth.Permission, h http.HandlerFunc)) {
	// Public: the handlers decide per category and document.
	r.HandleFunc("/documents", s.handleDocuments).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/documents/{id:[0-9]+}/download", s.handleDocumentDownload).Methods(http.MethodGet, http.MethodHead)

	m := auth.PermManageDocuments
	route("GET", "/documents", m, s.handleDocAdmin)
	route("POST", "/documents", m, s.handleDocUpload)
	route("POST", "/documents/categories", m, s.handleCategoryCreate)
	route("GET", "/documents/categories/{id:[0-9]+}", m, s.handleCategoryEdit)
	route("POST", "/documents/categories/{id:[0-9]+}", m, s.handleCategoryUpdate)
	route("POST", "/documents/categories/{id:[0-9]+}/archive", m, s.handleCategoryArchive)
	route("POST", "/documents/categories/{id:[0-9]+}/restore", m, s.handleCategoryRestore)
	route("GET", "/documents/{id:[0-9]+}", m, s.handleDocEdit)
	route("POST", "/documents/{id:[0-9]+}", m, s.handleDocUpdate)
	route("POST", "/documents/{id:[0-9]+}/file", m, s.handleDocReplace)
	route("POST", "/documents/{id:[0-9]+}/archive", m, s.handleDocArchive)
	route("POST", "/documents/{id:[0-9]+}/restore", m, s.handleDocRestore)
}

func (s *Server) maxUpload() int64 {
	if s.opt.MaxUploadBytes > 0 {
		return s.opt.MaxUploadBytes
	}
	return 50 << 20
}

// ---- Member side ----

// catNode is a category in the browsing tree.
type catNode struct {
	C        *documents.Category
	Children []*catNode
	Open     bool // on the path to the current category
	Current  bool
}

// docRow is a document in a list, with its effective access and category.
type docRow struct {
	D        *documents.Document
	Access   documents.Access
	Category string // category path, "" at the top level
	Hidden   bool   // archived directly or through its category (manage list)
}

type documentsData struct {
	Tree      []*catNode
	Current   *documents.Category
	Path      []*documents.Category
	Children  []*catNode // subcategories of the current one (or top level)
	Docs      []docRow
	Query     string
	Level     documents.Access
	Expired   bool
	ExpiredOn sql.NullTime
	Empty     bool // nothing visible at all
}

// visibleCategories returns the categories a viewer may browse: those open
// to their level, and the ancestors of any document they may see (a public
// document can sit in a committee category).
func visibleCategories(lib *documents.Library, level documents.Access) map[int64]bool {
	vis := map[int64]bool{}
	for _, c := range lib.Categories {
		if !lib.CategoryArchived(c.ID) && lib.CategoryAccess(c.ID).Permits(level) {
			vis[c.ID] = true
		}
	}
	for i := range lib.Documents {
		d := &lib.Documents[i]
		if d.CategoryID.Valid && lib.CanView(level, d) {
			for _, c := range lib.Path(d.CategoryID.Int64) {
				vis[c.ID] = true
			}
		}
	}
	return vis
}

// buildTree arranges the categories in include into a tree, in the
// library's order (sort_order, name).
func buildTree(lib *documents.Library, include func(*documents.Category) bool, current int64) []*catNode {
	nodes := map[int64]*catNode{}
	for i := range lib.Categories {
		c := &lib.Categories[i]
		if include(c) {
			nodes[c.ID] = &catNode{C: c, Current: c.ID == current}
		}
	}
	var roots []*catNode
	for i := range lib.Categories {
		n := nodes[lib.Categories[i].ID]
		if n == nil {
			continue
		}
		if p := n.C.ParentID; p.Valid && nodes[p.Int64] != nil {
			nodes[p.Int64].Children = append(nodes[p.Int64].Children, n)
		} else if !p.Valid || lib.Category(p.Int64) == nil {
			roots = append(roots, n)
		}
	}
	for _, c := range lib.Path(current) {
		if n := nodes[c.ID]; n != nil {
			n.Open = true
		}
	}
	return roots
}

func findNode(nodes []*catNode, id int64) *catNode {
	for _, n := range nodes {
		if n.C.ID == id {
			return n
		}
		if f := findNode(n.Children, id); f != nil {
			return f
		}
	}
	return nil
}

func categoryLabel(lib *documents.Library, id sql.NullInt64) string {
	if !id.Valid {
		return ""
	}
	var names []string
	for _, c := range lib.Path(id.Int64) {
		names = append(names, c.Name)
	}
	return strings.Join(names, " › ")
}

// handleDocuments is the library for everyone: visitors see public items,
// signed-in users members items, the committee everything not archived.
func (s *Server) handleDocuments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	lib, err := s.opt.Documents.Library(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	level := documents.ViewerLevel(p)
	vis := visibleCategories(lib, level)
	d := documentsData{Level: level, Query: strings.TrimSpace(r.URL.Query().Get("q"))}
	var current int64
	if slug := r.URL.Query().Get("cat"); slug != "" {
		for i := range lib.Categories {
			if c := &lib.Categories[i]; c.Slug == slug && vis[c.ID] {
				d.Current, current = c, c.ID
			}
		}
		if d.Current == nil {
			s.renderError(w, r, http.StatusNotFound, "No such folder.")
			return
		}
		d.Path = lib.Path(current)
	}
	d.Tree = buildTree(lib, func(c *documents.Category) bool { return vis[c.ID] }, current)
	d.Children = d.Tree
	if n := findNode(d.Tree, current); n != nil {
		d.Children = n.Children
	}
	any := len(d.Tree) > 0
	for i := range lib.Documents {
		doc := &lib.Documents[i]
		if !lib.CanView(level, doc) {
			continue
		}
		any = true
		if d.Query != "" {
			if !documents.Matches(doc, d.Query) {
				continue
			}
		} else if doc.CategoryID.Int64 != current || doc.CategoryID.Valid != (current != 0) {
			continue
		}
		d.Docs = append(d.Docs, docRow{D: doc, Access: lib.EffectiveAccess(doc), Category: categoryLabel(lib, doc.CategoryID)})
	}
	d.Empty = !any
	if p != nil && p.MemberID != 0 {
		if m, err := s.opt.Members.GetMember(ctx, p.MemberID); err == nil && m.Expired(s.opt.Now()) {
			d.Expired, d.ExpiredOn = true, m.MembershipExpiry
		}
	}
	title := "Documents"
	if d.Current != nil {
		title = d.Current.Name
	}
	s.render(w, r, http.StatusOK, "documents", title, d)
}

// handleDocumentDownload serves a file as an attachment to whoever may see
// it. Visitors asking for a members or committee document are sent to sign
// in; anything else they may not see is "not found".
func (s *Server) handleDocumentDownload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	id, _ := pathID(r, "id")
	lib, err := s.opt.Documents.Library(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var doc *documents.Document
	for i := range lib.Documents {
		if lib.Documents[i].ID == id {
			doc = &lib.Documents[i]
		}
	}
	if doc == nil || !lib.CanView(documents.ViewerLevel(p), doc) {
		if doc != nil && p == nil && !lib.Hidden(doc) {
			http.Redirect(w, r, s.url("/login?"+url.Values{"next": {r.URL.Path}}.Encode()), http.StatusSeeOther)
			return
		}
		s.renderError(w, r, http.StatusNotFound, "No such document.")
		return
	}
	f, err := s.opt.DocumentFiles.Open(doc.StoredName)
	if err != nil {
		s.opt.Log.Error("opening document file", "document_id", doc.ID, "err", err)
		s.renderError(w, r, http.StatusNotFound, "This file is not available. Please tell the committee.")
		return
	}
	defer f.Close()
	if r.Method == http.MethodGet && r.Header.Get("Range") == "" {
		a := members.Actor{Label: "anonymous"}
		if p != nil {
			a = p.Actor()
		}
		if err := s.opt.Documents.RecordDownload(ctx, a, doc.ID); err != nil {
			s.opt.Log.Warn("recording download", "document_id", doc.ID, "err", err)
		}
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(longRequestTimeout))
	h := w.Header()
	ct := doc.MimeType
	if ct == "" {
		ct = documents.FallbackMime
	}
	h.Set("Content-Type", ct)
	disp := mime.FormatMediaType("attachment", map[string]string{"filename": documents.SanitizeFilename(doc.FileName)})
	if disp == "" {
		disp = "attachment"
	}
	h.Set("Content-Disposition", disp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, "", doc.UploadedAt, f)
}

// ---- Manage side ----

// catOption is a category in a select, indented by depth.
type catOption struct {
	ID       int64
	Label    string
	Depth    int
	C        *documents.Category
	Access   documents.Access // effective
	Archived bool             // directly or through a parent
	Docs     int
}

func flatten(lib *documents.Library) []catOption {
	counts := map[int64]int{}
	for _, d := range lib.Documents {
		if d.CategoryID.Valid {
			counts[d.CategoryID.Int64]++
		}
	}
	var out []catOption
	var walk func(ns []*catNode, depth int)
	walk = func(ns []*catNode, depth int) {
		for _, n := range ns {
			out = append(out, catOption{ID: n.C.ID, Label: strings.Repeat("— ", depth) + n.C.Name, Depth: depth, C: n.C,
				Access: lib.CategoryAccess(n.C.ID), Archived: lib.CategoryArchived(n.C.ID), Docs: counts[n.C.ID]})
			walk(n.Children, depth+1)
		}
	}
	walk(buildTree(lib, func(*documents.Category) bool { return true }, 0), 0)
	return out
}

type docAdminData struct {
	Categories []catOption
	Docs       []docRow
	Pager      pager
	Count      int
	Query      string
	Cat        string // filter: category id, "none" or ""
	CatID      int64  // the filtered category, when Cat names one
	CatName    string
	Archived   bool // show archived documents only
	MaxMB      int64
	Extensions string
	Error      string
	Levels     []documents.Access
	Form       url.Values // values of a rejected form
}

func (s *Server) docAdminPage(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	lib, err := s.opt.Documents.Library(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	q := r.URL.Query()
	d := docAdminData{Categories: flatten(lib), Query: strings.TrimSpace(q.Get("q")), Cat: q.Get("cat"),
		Archived: q.Get("show") == "archived", MaxMB: s.maxUpload() >> 20, Extensions: documents.AllowedExtensions,
		Error: errMsg, Levels: append(slices.Clone(documents.Levels), documents.Inherit)}
	if errMsg != "" {
		d.Form = r.PostForm
	}
	if id, err := strconv.ParseInt(d.Cat, 10, 64); err == nil && lib.Category(id) != nil {
		d.CatID, d.CatName = id, lib.Category(id).Name
	}
	var rows []docRow
	for i := range lib.Documents {
		doc := &lib.Documents[i]
		if lib.Hidden(doc) != d.Archived {
			continue
		}
		switch d.Cat {
		case "":
		case "none":
			if doc.CategoryID.Valid {
				continue
			}
		default:
			if strconv.FormatInt(doc.CategoryID.Int64, 10) != d.Cat {
				continue
			}
		}
		if d.Query != "" && !documents.Matches(doc, d.Query) {
			continue
		}
		rows = append(rows, docRow{D: doc, Access: lib.EffectiveAccess(doc), Category: categoryLabel(lib, doc.CategoryID), Hidden: lib.Hidden(doc)})
	}
	requested, _ := strconv.Atoi(q.Get("page"))
	lo, hi, pg := paginate(s.url("/admin/documents"), q, requested, len(rows), pageSize)
	d.Docs, d.Pager, d.Count = rows[lo:hi], pg, len(rows)
	s.render(w, r, status, "docadmin", "Manage documents", d)
}

func (s *Server) handleDocAdmin(w http.ResponseWriter, r *http.Request) {
	s.docAdminPage(w, r, http.StatusOK, "")
}

// parseCategoryRef reads a category select: "" is the top level.
func parseCategoryRef(lib *documents.Library, v string) (sql.NullInt64, bool) {
	if v == "" {
		return sql.NullInt64{}, true
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || lib.Category(id) == nil {
		return sql.NullInt64{}, false
	}
	return sql.NullInt64{Int64: id, Valid: true}, true
}

func trimmed(r *http.Request, name string, max int) (string, bool) {
	v := strings.TrimSpace(r.PostForm.Get(name))
	return v, len([]rune(v)) <= max
}

// parseCategoryForm validates a category form against the library; id is
// the category being edited (0 for a new one).
func parseCategoryForm(r *http.Request, lib *documents.Library, id int64) (*documents.Category, string) {
	c := &documents.Category{ID: id}
	var ok bool
	if c.Name, ok = trimmed(r, "name", 255); !ok || c.Name == "" {
		return c, "Enter a name of at most 255 characters."
	}
	if c.Description, ok = trimmed(r, "description", 2000); !ok {
		return c, "The description is limited to 2000 characters."
	}
	if c.ParentID, ok = parseCategoryRef(lib, r.PostForm.Get("parent")); !ok {
		return c, "Choose a parent folder."
	}
	if id != 0 && c.ParentID.Valid {
		for _, a := range lib.Path(c.ParentID.Int64) {
			if a.ID == id {
				return c, "A folder cannot be moved under itself or one of its subfolders."
			}
		}
	}
	if v := strings.TrimSpace(r.PostForm.Get("sort_order")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < -100000 || n > 100000 {
			return c, "The order must be a whole number."
		}
		c.SortOrder = n
	}
	if c.Access, ok = documents.ParseAccess(r.PostForm.Get("access"), false); !ok {
		return c, "Choose who can see the folder."
	}
	return c, ""
}

func (s *Server) handleCategoryCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lib, err := s.opt.Documents.Library(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	c, msg := parseCategoryForm(r, lib, 0)
	if msg != "" {
		s.docAdminPage(w, r, http.StatusUnprocessableEntity, msg)
		return
	}
	if err := s.opt.Documents.CreateCategory(ctx, principalFrom(ctx).Actor(), c); err != nil {
		s.serverError(w, r, err)
		return
	}
	back := "/admin/documents?flash=cat-created"
	if c.ParentID.Valid {
		back = fmt.Sprintf("/admin/documents?cat=%d&flash=cat-created", c.ParentID.Int64)
	}
	http.Redirect(w, r, s.url(back), http.StatusSeeOther)
}

type categoryEditData struct {
	C         *documents.Category
	Parents   []catOption
	Levels    []documents.Access
	Error     string
	Effective documents.Access
	Archived  bool
}

func (s *Server) loadCategory(w http.ResponseWriter, r *http.Request) (*documents.Library, *documents.Category, bool) {
	lib, err := s.opt.Documents.Library(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return nil, nil, false
	}
	id, _ := pathID(r, "id")
	c := lib.Category(id)
	if c == nil {
		s.renderError(w, r, http.StatusNotFound, "No such folder.")
		return nil, nil, false
	}
	return lib, c, true
}

func (s *Server) categoryPage(w http.ResponseWriter, r *http.Request, status int, lib *documents.Library, c *documents.Category, msg string) {
	var parents []catOption
	for _, o := range flatten(lib) {
		inside := false
		for _, a := range lib.Path(o.ID) {
			inside = inside || a.ID == c.ID
		}
		if !inside {
			parents = append(parents, o)
		}
	}
	s.render(w, r, status, "doccategory", "Folder: "+c.Name, categoryEditData{C: c, Parents: parents,
		Levels: documents.Levels, Error: msg, Effective: lib.CategoryAccess(c.ID), Archived: lib.CategoryArchived(c.ID)})
}

func (s *Server) handleCategoryEdit(w http.ResponseWriter, r *http.Request) {
	if lib, c, ok := s.loadCategory(w, r); ok {
		s.categoryPage(w, r, http.StatusOK, lib, c, "")
	}
}

func (s *Server) handleCategoryUpdate(w http.ResponseWriter, r *http.Request) {
	lib, existing, ok := s.loadCategory(w, r)
	if !ok {
		return
	}
	c, msg := parseCategoryForm(r, lib, existing.ID)
	if msg != "" {
		c.Slug, c.ArchivedAt = existing.Slug, existing.ArchivedAt
		s.categoryPage(w, r, http.StatusUnprocessableEntity, lib, c, msg)
		return
	}
	if err := s.opt.Documents.UpdateCategory(r.Context(), principalFrom(r.Context()).Actor(), c); err != nil {
		s.docError(w, r, err)
		return
	}
	http.Redirect(w, r, s.url(fmt.Sprintf("/admin/documents/categories/%d?flash=cat-saved", c.ID)), http.StatusSeeOther)
}

func (s *Server) handleCategoryArchive(w http.ResponseWriter, r *http.Request) {
	s.archiveCategory(w, r, true)
}
func (s *Server) handleCategoryRestore(w http.ResponseWriter, r *http.Request) {
	s.archiveCategory(w, r, false)
}

func (s *Server) archiveCategory(w http.ResponseWriter, r *http.Request, archive bool) {
	id, _ := pathID(r, "id")
	if err := s.opt.Documents.SetCategoryArchived(r.Context(), principalFrom(r.Context()).Actor(), id, archive); err != nil {
		s.docError(w, r, err)
		return
	}
	flash := "cat-restored"
	if archive {
		flash = "cat-archived"
	}
	http.Redirect(w, r, s.url(fmt.Sprintf("/admin/documents/categories/%d?flash=%s", id, flash)), http.StatusSeeOther)
}

func (s *Server) docError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, documents.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Not found.")
		return
	}
	s.serverError(w, r, err)
}

// upload is a stored file from the posted "file" field.
type upload struct {
	Name, Mime string
	Saved      documents.Saved
}

// storeUpload saves the posted "file" after checking its extension. A
// non-empty msg (with its status) is a problem for the user to fix.
func (s *Server) storeUpload(r *http.Request) (u upload, status int, msg string, err error) {
	if r.MultipartForm == nil {
		return u, http.StatusUnprocessableEntity, "Choose a file to upload.", nil
	}
	f, hdr, ferr := r.FormFile("file")
	if ferr != nil {
		return u, http.StatusUnprocessableEntity, "Choose a file to upload.", nil
	}
	defer f.Close()
	u.Name = documents.SanitizeFilename(hdr.Filename)
	var ok bool
	if u.Mime, ok = documents.AllowedUpload(u.Name); !ok {
		return u, http.StatusUnprocessableEntity, "This type of file is not accepted. Allowed: " + documents.AllowedExtensions + ".", nil
	}
	u.Saved, err = s.opt.DocumentFiles.Save(f, s.maxUpload(), nil)
	if errors.Is(err, documents.ErrTooLarge) {
		return u, http.StatusRequestEntityTooLarge, fmt.Sprintf("The file is too large: the limit is %d MB.", s.maxUpload()>>20), nil
	}
	return u, 0, "", err
}

// parseDocumentForm reads title, description, category and access.
func parseDocumentForm(r *http.Request, lib *documents.Library, d *documents.Document) string {
	var ok bool
	if d.Title, ok = trimmed(r, "title", 255); !ok {
		return "The title is limited to 255 characters."
	}
	if d.Description, ok = trimmed(r, "description", 2000); !ok {
		return "The description is limited to 2000 characters."
	}
	if d.CategoryID, ok = parseCategoryRef(lib, r.PostForm.Get("category")); !ok {
		return "Choose a folder."
	}
	access := r.PostForm.Get("access")
	if access == "" {
		access = string(documents.Inherit)
	}
	if d.Access, ok = documents.ParseAccess(access, true); !ok {
		return "Choose who can see the document."
	}
	return ""
}

func (s *Server) handleDocUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	lib, err := s.opt.Documents.Library(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := &documents.Document{UploadedBy: sql.NullInt64{Int64: p.UserID, Valid: p.UserID != 0}, UploadedAt: s.opt.Now().UTC()}
	if msg := parseDocumentForm(r, lib, d); msg != "" {
		s.docAdminPage(w, r, http.StatusUnprocessableEntity, msg)
		return
	}
	u, status, msg, err := s.storeUpload(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	} else if msg != "" {
		s.docAdminPage(w, r, status, msg)
		return
	}
	d.FileName, d.MimeType, d.StoredName, d.SizeBytes, d.SHA256 = u.Name, u.Mime, u.Saved.StoredName, u.Saved.Size, u.Saved.SHA256
	if d.Title == "" {
		d.Title = strings.TrimSuffix(u.Name, path.Ext(u.Name))
	}
	if err := s.opt.Documents.CreateDocument(ctx, p.Actor(), d); err != nil {
		_ = s.opt.DocumentFiles.Remove(u.Saved.StoredName)
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, s.url(fmt.Sprintf("/admin/documents/%d?flash=doc-uploaded", d.ID)), http.StatusSeeOther)
}

type docEditData struct {
	D          *documents.Document
	Categories []catOption
	Levels     []documents.Access
	Effective  documents.Access
	Hidden     bool
	MaxMB      int64
	Extensions string
	Error      string
}

func (s *Server) loadDocument(w http.ResponseWriter, r *http.Request) (*documents.Library, *documents.Document, bool) {
	lib, err := s.opt.Documents.Library(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return nil, nil, false
	}
	id, _ := pathID(r, "id")
	for i := range lib.Documents {
		if lib.Documents[i].ID == id {
			return lib, &lib.Documents[i], true
		}
	}
	s.renderError(w, r, http.StatusNotFound, "No such document.")
	return nil, nil, false
}

func (s *Server) docEditPage(w http.ResponseWriter, r *http.Request, status int, lib *documents.Library, d *documents.Document, msg string) {
	s.render(w, r, status, "document", d.Title, docEditData{D: d, Categories: flatten(lib),
		Levels: append(slices.Clone(documents.Levels), documents.Inherit), Effective: lib.EffectiveAccess(d), Hidden: lib.Hidden(d),
		MaxMB: s.maxUpload() >> 20, Extensions: documents.AllowedExtensions, Error: msg})
}

func (s *Server) handleDocEdit(w http.ResponseWriter, r *http.Request) {
	if lib, d, ok := s.loadDocument(w, r); ok {
		s.docEditPage(w, r, http.StatusOK, lib, d, "")
	}
}

func (s *Server) handleDocUpdate(w http.ResponseWriter, r *http.Request) {
	lib, d, ok := s.loadDocument(w, r)
	if !ok {
		return
	}
	if msg := parseDocumentForm(r, lib, d); msg != "" || d.Title == "" {
		if msg == "" {
			msg = "Enter a title."
		}
		s.docEditPage(w, r, http.StatusUnprocessableEntity, lib, d, msg)
		return
	}
	if err := s.opt.Documents.UpdateDocument(r.Context(), principalFrom(r.Context()).Actor(), d); err != nil {
		s.docError(w, r, err)
		return
	}
	http.Redirect(w, r, s.url(fmt.Sprintf("/admin/documents/%d?flash=doc-saved", d.ID)), http.StatusSeeOther)
}

func (s *Server) handleDocReplace(w http.ResponseWriter, r *http.Request) {
	lib, d, ok := s.loadDocument(w, r)
	if !ok {
		return
	}
	p := principalFrom(r.Context())
	u, status, msg, err := s.storeUpload(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	} else if msg != "" {
		s.docEditPage(w, r, status, lib, d, msg)
		return
	}
	old, err := s.opt.Documents.ReplaceFile(r.Context(), p.Actor(), d.ID, u.Name, u.Mime, u.Saved, p.UserID)
	if err != nil {
		_ = s.opt.DocumentFiles.Remove(u.Saved.StoredName)
		s.docError(w, r, err)
		return
	}
	if err := s.opt.DocumentFiles.Remove(old); err != nil {
		s.opt.Log.Warn("removing replaced document file", "document_id", d.ID, "err", err)
	}
	http.Redirect(w, r, s.url(fmt.Sprintf("/admin/documents/%d?flash=doc-replaced", d.ID)), http.StatusSeeOther)
}

func (s *Server) handleDocArchive(w http.ResponseWriter, r *http.Request) {
	s.archiveDocument(w, r, true)
}
func (s *Server) handleDocRestore(w http.ResponseWriter, r *http.Request) {
	s.archiveDocument(w, r, false)
}

func (s *Server) archiveDocument(w http.ResponseWriter, r *http.Request, archive bool) {
	id, _ := pathID(r, "id")
	if err := s.opt.Documents.SetDocumentArchived(r.Context(), principalFrom(r.Context()).Actor(), id, archive); err != nil {
		s.docError(w, r, err)
		return
	}
	flash := "doc-restored"
	if archive {
		flash = "doc-archived"
	}
	http.Redirect(w, r, s.url(fmt.Sprintf("/admin/documents/%d?flash=%s", id, flash)), http.StatusSeeOther)
}
