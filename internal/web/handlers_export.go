package web

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/clidive410/membership/internal/export"
	"github.com/clidive410/membership/internal/members"
)

// handleExportCSV replicates fulllistexport.php: ?status=all exports every
// member including archived ones, otherwise the current list (which, as in
// the legacy app, includes Lapsed members). The other statuses (e.g.
// ?status=expired) work too. Sorted by last name.
func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	status := members.ParseStatus(r.URL.Query().Get("status"))
	ms, err := s.opt.Members.ListMembers(r.Context(), members.Filter{Status: status, Sort: "last_name", Today: s.opt.Now()})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var buf bytes.Buffer
	if err := export.WriteMembersCSV(&buf, ms); err != nil {
		s.serverError(w, r, err)
		return
	}
	p := principalFrom(r.Context())
	s.opt.Log.Info("member export", "user_id", p.UserID, "status", status, "rows", len(ms))
	name := "clidive-members-" + string(status) + "-" + s.opt.Now().Format("2006-01-02") + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

type emailsData struct {
	Lists         any
	List          members.MailingList
	Label         string
	ExcludeLapsed bool
	Emails        []string
	Joined        string
}

func (s *Server) emailListArgs(r *http.Request) (members.MailingList, string, bool, bool) {
	list := members.MailingList(r.URL.Query().Get("list"))
	if list == "" {
		list = members.ListAll
	}
	for _, l := range members.MailingLists {
		if l.Key == list {
			return list, l.Label, r.URL.Query().Get("exclude_lapsed") != "", true
		}
	}
	return list, "", false, false
}

// handleEmailLists replaces the legacy `email-all-auto` view and adds one
// list per mailing flag.
func (s *Server) handleEmailLists(w http.ResponseWriter, r *http.Request) {
	list, label, excl, ok := s.emailListArgs(r)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "No such mailing list.")
		return
	}
	emails, err := s.opt.Members.EmailList(r.Context(), list, excl)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "emails", "Email lists", emailsData{
		Lists: members.MailingLists, List: list, Label: label, ExcludeLapsed: excl,
		Emails: emails, Joined: export.JoinEmails(emails, "; "),
	})
}

func (s *Server) handleEmailListText(w http.ResponseWriter, r *http.Request) {
	list, _, excl, ok := s.emailListArgs(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	emails, err := s.opt.Members.EmailList(r.Context(), list, excl)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="clidive-emails-`+string(list)+`.txt"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(strings.Join(emails, "\n") + "\n"))
}
