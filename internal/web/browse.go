package web

import (
	"database/sql"
	"fmt"
	"net/url"

	"github.com/clidive410/membership/internal/documents"
)

// docBrowser is the folder-and-document list shared by /documents and
// /admin/documents (template "browser" in partials.html): a breadcrumb, a
// search box, then one table of folders followed by documents.
type docBrowser struct {
	Crumbs []crumb // "All folders" first; the last one is the open folder
	Title  string  // instead of the breadcrumb: search results, archived
	Count  int
	Action string            // search form action
	Hidden map[string]string // extra search form fields (the open folder)
	Query  string
	// ArchivedToggle shows the "Archived" checkbox (manage page).
	ArchivedToggle, Archived bool
	AccessCol, DownloadsCol  bool
	Rows                     []browseRow
	Pager                    pager
	Empty                    string
}

type crumb struct {
	Name string
	Href string // empty for the current folder
}

// browseRow is a folder or a document.
type browseRow struct {
	Folder    bool
	Name      string
	Href      string
	Path      string // folder path, in search results
	Note      string // description
	Badge     string // e.g. "archived", "file missing"
	Access    documents.Access
	Inherited bool
	Size      string // "1.6 MB", or "3 documents" for a folder
	Date      string
	Downloads int64
}

// crumbs builds the breadcrumb of the open folder; href makes a folder link.
func crumbs(lib *documents.Library, current int64, href func(*documents.Category) string, top string) []crumb {
	out := []crumb{{Name: "All folders", Href: top}}
	for _, c := range lib.Path(current) {
		h := href(c)
		if c.ID == current {
			h = ""
		}
		out = append(out, crumb{Name: c.Name, Href: h})
	}
	if current == 0 {
		out[0].Href = ""
	}
	return out
}

func folderRow(c *documents.Category, name, href string, access documents.Access, docs int) browseRow {
	r := browseRow{Folder: true, Name: name, Href: href, Note: c.Description, Access: access,
		Size: fmt.Sprintf("%d document%s", docs, map[bool]string{true: "", false: "s"}[docs == 1])}
	if c.ArchivedAt.Valid {
		r.Badge = "archived"
	}
	return r
}

func documentRow(lib *documents.Library, d *documents.Document, href string, withPath bool) browseRow {
	r := browseRow{Name: d.Title, Href: href, Note: d.Description, Access: lib.EffectiveAccess(d),
		Inherited: d.Access == documents.Inherit, Size: formatBytes(d.SizeBytes),
		Date: d.UploadedAt.Format("02/01/2006"), Downloads: d.Downloads}
	if withPath {
		r.Path = categoryLabel(lib, d.CategoryID)
		if r.Path == "" {
			r.Path = "(top level)"
		}
	}
	if d.SHA256 == "" {
		r.Badge = "file missing"
	}
	return r
}

// inFolder reports whether a category or document parent is the open folder
// (0: the top level).
func inFolder(parent sql.NullInt64, current int64) bool {
	return parent.Valid == (current != 0) && parent.Int64 == current
}

// paginateRows pages the rows of b with the request query.
func (b *docBrowser) paginateRows(path string, q url.Values, rows []browseRow) {
	requested := 0
	fmt.Sscan(q.Get("page"), &requested)
	lo, hi, pg := paginate(path, q, requested, len(rows), pageSize)
	b.Rows, b.Pager, b.Count = rows[lo:hi], pg, len(rows)
}

// subtreeCounts counts, per folder, the documents in it or any subfolder
// for which include is true.
func subtreeCounts(lib *documents.Library, include func(*documents.Document) bool) map[int64]int {
	n := map[int64]int{}
	for i := range lib.Documents {
		d := &lib.Documents[i]
		if !d.CategoryID.Valid || !include(d) {
			continue
		}
		for _, c := range lib.Path(d.CategoryID.Int64) {
			n[c.ID]++
		}
	}
	return n
}
