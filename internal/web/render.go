package web

import (
	"bytes"
	"database/sql"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
)

type pageTemplate struct{ t *template.Template }

var pageNames = []string{"login", "members", "member", "payments", "emails", "error"}

// typeColors maps the legacy membership type ids to badge colours; unknown
// types fall back to the default badge.
var typeColors = map[int64]string{
	members.TypeAnnual: "green", members.TypeStandingOrder: "blue", members.TypeSocial: "purple",
	members.TypeHonorary: "yellow", members.TypeThreeMonth: "cyan",
	members.TypeLapsed: "secondary",
}

// icons holds the vendored Tabler icons, ready to inline (see icon).
var icons = loadIcons()

func loadIcons() map[string]template.HTML {
	const dir = "static/vendor/tabler-icons/outline"
	entries, err := fs.ReadDir(staticFS, dir)
	if err != nil {
		panic(err)
	}
	out := map[string]template.HTML{}
	for _, e := range entries {
		b, err := fs.ReadFile(staticFS, dir+"/"+e.Name())
		if err != nil {
			panic(err)
		}
		svg := strings.Join(strings.Fields(string(b)), " ")
		svg = strings.Replace(svg, "<svg ", `<svg aria-hidden="true" focusable="false" `, 1)
		svg = strings.ReplaceAll(svg, "> <", "><")
		out[strings.TrimSuffix(e.Name(), ".svg")] = template.HTML(svg) // vendored, trusted markup
	}
	return out
}

// icon returns the inline SVG of a vendored icon. It panics on an unknown
// name so that a typo fails the template tests rather than rendering nothing.
func icon(name string) template.HTML {
	svg, ok := icons[name]
	if !ok {
		panic("unknown icon " + name)
	}
	return svg
}

// dict builds a map for passing several values to a sub-template.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

func mustParseTemplates(base string) map[string]*pageTemplate {
	funcs := template.FuncMap{
		"url":  func(p string) string { return base + p },
		"date": members.FormatDate,
		"dateTime": func(t sql.NullTime) string {
			if !t.Valid {
				return ""
			}
			return t.Time.Format("2006-01-02 15:04")
		},
		"ukdate": func(t sql.NullTime) string {
			if !t.Valid {
				return ""
			}
			return t.Time.Format("02/01/2006")
		},
		"query": func(kv ...string) template.URL {
			v := url.Values{}
			for i := 0; i+1 < len(kv); i += 2 {
				if kv[i+1] != "" {
					v.Set(kv[i], kv[i+1])
				}
			}
			if len(v) == 0 {
				return ""
			}
			return template.URL("?" + v.Encode())
		},
		"itoa":   func(i int64) string { return strconv.FormatInt(i, 10) },
		"permit": func(p *auth.Principal, perm string) bool { return p.Can(auth.Permission(perm)) },
		"icon":   icon,
		"dict":   dict,
		// typeColor picks a Tabler badge colour for a membership type.
		"typeColor": func(id sql.NullInt64) string {
			if !id.Valid {
				return "secondary"
			}
			return typeColors[id.Int64]
		},
		"roleLabel": func(p *auth.Principal) string {
			switch {
			case p.HasRole(auth.RoleAdmin):
				return "Admin"
			case p.HasRole(auth.RoleMembershipManager):
				return "Membership manager"
			}
			return "Member"
		},
		"initials": func(email string) string {
			if email == "" {
				return "?"
			}
			return strings.ToUpper(email[:1])
		},
	}
	pages := map[string]*pageTemplate{}
	for _, name := range pageNames {
		t := template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/"+name+".html"))
		pages[name] = &pageTemplate{t: t}
	}
	return pages
}

// page is the data passed to every template.
type page struct {
	Title     string
	Nav       string // active sidebar entry
	Status    int
	P         *auth.Principal
	CSRF      string
	Flash     string
	Base      string
	Data      any
	FormError string
}

// Shell reports whether to show the admin layout (sidebar, top bar). Pages
// for visitors without admin access are shown centred instead.
func (p page) Shell() bool { return p.P.Can(auth.PermViewMembers) }

// flashes are fixed messages selected by a ?flash= key, so no user input is
// ever reflected.
var flashes = map[string]string{
	"created":         "Member created.",
	"saved":           "Member saved.",
	"unchanged":       "No changes to save.",
	"archived":        "Member archived. They no longer receive mailing-list emails; you can restore them below.",
	"restored":        "Member restored.",
	"payment":         "Payment recorded.",
	"payment-deleted": "Payment deleted.",
	"loggedout":       "You have been signed out.",
}

// navFor maps a page template to its sidebar entry.
var navFor = map[string]string{"members": "members", "member": "members", "payments": "payments", "emails": "emails"}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name, title string, data any) {
	pg := page{
		Title:  title,
		Nav:    navFor[name],
		Status: status,
		P:      principalFrom(r.Context()),
		CSRF:   s.csrfToken(w, r),
		Flash:  flashes[r.URL.Query().Get("flash")],
		Base:   s.opt.BasePath,
		Data:   data,
	}
	if d, ok := data.(memberData); ok && d.New {
		pg.Nav = "new"
	}
	if name == "payments" {
		if n, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && r.URL.Query().Get("flash") == "bulk" {
			pg.Flash = fmt.Sprintf("Payment recorded for %d member(s).", n)
		}
	}
	var buf bytes.Buffer
	if err := s.pages[name].t.Execute(&buf, pg); err != nil {
		s.opt.Log.Error("rendering template", "template", name, "err", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.render(w, r, status, "error", http.StatusText(status), msg)
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.opt.Log.Error("internal error", "path", r.URL.Path, "err", err)
	s.renderError(w, r, http.StatusInternalServerError, "Something went wrong. Please try again.")
}
