// Package documents is the club's document library: categories, files on
// disk, access levels and the import from the old WordPress WP-Filebase
// plugin.
package documents

import (
	"database/sql"
	"strings"
	"time"

	"github.com/clidive410/membership/internal/auth"
)

// Access is who may see a category or document.
type Access string

// Access levels, from most open to most restricted. Inherit is valid for
// documents only: they then take their category's effective access.
const (
	Public    Access = "public"    // anyone, without signing in
	Members   Access = "members"   // any signed-in user (expired members too)
	Committee Access = "committee" // roles admin and membership_manager
	Inherit   Access = "inherit"
)

// Levels lists the concrete access levels, most open first.
var Levels = []Access{Public, Members, Committee}

func (a Access) rank() int {
	switch a {
	case Public:
		return 0
	case Members:
		return 1
	}
	return 2 // committee, and anything unknown: fail closed
}

// Permits reports whether a viewer at level may see something with access a.
func (a Access) Permits(level Access) bool { return a.rank() <= level.rank() }

// ParseAccess validates a posted access level; inherit only when allowed.
func ParseAccess(s string, allowInherit bool) (Access, bool) {
	a := Access(s)
	switch a {
	case Public, Members, Committee:
		return a, true
	case Inherit:
		return a, allowInherit
	}
	return "", false
}

// stricter returns the more restricted of a and b.
func stricter(a, b Access) Access {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// ViewerLevel is the most restricted level the principal may see: public
// for visitors, members for anyone signed in, committee for admins and
// membership managers.
func ViewerLevel(p *auth.Principal) Access {
	switch {
	case p.IsCommittee():
		return Committee
	case p.Can(auth.PermViewDocuments):
		return Members
	}
	return Public
}

// Category is a folder of the library.
type Category struct {
	ID          int64
	ParentID    sql.NullInt64
	Name        string
	Slug        string
	Description string
	SortOrder   int
	Access      Access // never Inherit
	LegacyKey   sql.NullString
	CreatedAt   time.Time
	ArchivedAt  sql.NullTime
}

// Document is one file of the library.
type Document struct {
	ID          int64
	CategoryID  sql.NullInt64 // NULL: top level
	Title       string
	Description string
	FileName    string // original name, shown and used for downloads
	StoredName  string // name under DOCUMENTS_DIR
	MimeType    string
	SizeBytes   int64
	SHA256      string // empty when the file is missing
	Access      Access
	UploadedBy  sql.NullInt64
	UploadedAt  time.Time
	LegacyID    sql.NullInt64
	LegacyPath  string
	Downloads   int64
	ArchivedAt  sql.NullTime
}

// Library is the whole catalogue in memory (a few hundred rows), with the
// effective access and archived state of every category resolved.
type Library struct {
	Categories []Category
	Documents  []Document
	byID       map[int64]*Category
	access     map[int64]Access
	archived   map[int64]bool
}

// NewLibrary indexes categories and documents.
func NewLibrary(cats []Category, docs []Document) *Library {
	l := &Library{Categories: cats, Documents: docs, byID: map[int64]*Category{},
		access: map[int64]Access{}, archived: map[int64]bool{}}
	for i := range cats {
		l.byID[cats[i].ID] = &cats[i]
	}
	for _, c := range cats {
		l.resolve(c.ID)
	}
	return l
}

// resolve walks up the parents: a category is as restricted as its most
// restricted ancestor, and archived when any ancestor is. A cycle or a
// missing parent resolves to committee (fail closed).
func (l *Library) resolve(id int64) {
	acc, arch := Access(""), false
	seen := map[int64]bool{}
	for cur := id; ; {
		c, ok := l.byID[cur]
		if !ok || seen[cur] {
			acc = Committee
			break
		}
		seen[cur] = true
		if acc == "" {
			acc = c.Access
		}
		acc = stricter(acc, c.Access)
		arch = arch || c.ArchivedAt.Valid
		if !c.ParentID.Valid {
			break
		}
		cur = c.ParentID.Int64
	}
	l.access[id], l.archived[id] = acc, arch
}

// Category returns the category with id, or nil.
func (l *Library) Category(id int64) *Category { return l.byID[id] }

// CategoryAccess is the effective access of a category.
func (l *Library) CategoryAccess(id int64) Access {
	if a, ok := l.access[id]; ok {
		return a
	}
	return Committee
}

// CategoryArchived reports whether the category or an ancestor is archived.
func (l *Library) CategoryArchived(id int64) bool { return l.archived[id] }

// EffectiveAccess is the document's own access unless it is inherit; then
// its category's effective access. A top-level document that inherits is
// committee only.
func (l *Library) EffectiveAccess(d *Document) Access {
	if d.Access != Inherit {
		if _, ok := ParseAccess(string(d.Access), false); ok {
			return d.Access
		}
		return Committee
	}
	if !d.CategoryID.Valid {
		return Committee
	}
	return l.CategoryAccess(d.CategoryID.Int64)
}

// Hidden reports whether the document is archived, directly or through
// its category.
func (l *Library) Hidden(d *Document) bool {
	return d.ArchivedAt.Valid || (d.CategoryID.Valid && l.CategoryArchived(d.CategoryID.Int64))
}

// CanView reports whether a viewer at level may see and download d.
func (l *Library) CanView(level Access, d *Document) bool {
	return !l.Hidden(d) && l.EffectiveAccess(d).Permits(level)
}

// Path returns the category's ancestors and itself, top first.
func (l *Library) Path(id int64) []*Category {
	var out []*Category
	seen := map[int64]bool{}
	for c := l.byID[id]; c != nil && !seen[c.ID]; {
		seen[c.ID] = true
		out = append([]*Category{c}, out...)
		if !c.ParentID.Valid {
			break
		}
		c = l.byID[c.ParentID.Int64]
	}
	return out
}

// Matches reports whether d's title or description contains every word of
// q (case-insensitive).
func Matches(d *Document, q string) bool {
	hay := strings.ToLower(d.Title + " " + d.Description + " " + d.FileName)
	for _, w := range strings.Fields(strings.ToLower(q)) {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}
