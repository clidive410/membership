package web

import (
	"bytes"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
)

type pageTemplate struct{ t *template.Template }

var pageNames = []string{"login", "members", "member", "payments", "emails", "error"}

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
	}
	pages := map[string]*pageTemplate{}
	for _, name := range pageNames {
		t := template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html"))
		pages[name] = &pageTemplate{t: t}
	}
	return pages
}

// page is the data passed to every template.
type page struct {
	Title     string
	P         *auth.Principal
	CSRF      string
	Flash     string
	Base      string
	Data      any
	FormError string
}

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

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name, title string, data any) {
	pg := page{
		Title: title,
		P:     principalFrom(r.Context()),
		CSRF:  s.csrfToken(w, r),
		Flash: flashes[r.URL.Query().Get("flash")],
		Base:  s.opt.BasePath,
		Data:  data,
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
