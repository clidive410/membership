package web

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
)

type listData struct {
	Members  []members.Member // the current page
	Types    []members.MembershipType
	Filter   members.Filter
	TypeArg  string
	Count    int // all matching members
	Headers  []sortHeader
	Pager    pager
	Filtered bool // any filter other than the default
	Now      time.Time
}

func parseFilter(r *http.Request) (members.Filter, string) {
	q := r.URL.Query()
	f := members.Filter{
		Query:  strings.TrimSpace(q.Get("q")),
		Status: members.ParseStatus(q.Get("status")),
		Sort:   q.Get("sort"),
		Desc:   q.Get("dir") == "desc",
	}
	if _, ok := members.SortColumns[f.Sort]; !ok {
		f.Sort = "last_name"
	}
	typeArg := q.Get("type")
	switch typeArg {
	case "none":
		f.TypeID = -1
	default:
		if id, err := strconv.ParseInt(typeArg, 10, 64); err == nil && id > 0 {
			f.TypeID = id
		} else {
			typeArg = ""
		}
	}
	return f, typeArg
}

func (s *Server) handleMemberList(w http.ResponseWriter, r *http.Request) {
	f, typeArg := parseFilter(r)
	f.Today = s.opt.Now()
	ms, err := s.opt.Members.ListMembers(r.Context(), f)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	types, err := s.opt.Members.ListTypes(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	q := r.URL.Query()
	headers := sortHeaders(s.url("/admin/members"), q, f.Sort, f.Desc, []sortCol{
		{"id", "ID", "text-end"}, {"first_name", "First name", ""}, {"last_name", "Last name", ""}, {"email", "Email", ""},
		{"type", "Clidive type", ""}, {"expiry", "Clidive expiry", "text-end"}, {"bsac", "BSAC renewal", "text-end"},
		{"last_paid", "Last paid", "text-end"}, {"notes", "Notes", ""},
	})
	requested, _ := strconv.Atoi(q.Get("page"))
	lo, hi, pg := paginate(s.url("/admin/members"), q, requested, len(ms), pageSize)
	s.render(w, r, http.StatusOK, "members", "Members", listData{
		Members: ms[lo:hi], Types: types, Filter: f, TypeArg: typeArg, Count: len(ms), Headers: headers, Pager: pg,
		Filtered: f.Query != "" || typeArg != "" || f.Status != members.StatusCurrent, Now: s.opt.Now(),
	})
}

type memberData struct {
	M        *members.Member
	New      bool
	Types    []members.MembershipType
	Payments []members.Payment
	Audit    []members.AuditEntry
	Errors   map[string]string
	Editable map[members.FieldGroup]bool
	Methods  []string
	Today    string
	Now      time.Time
}

func (d memberData) CanEdit(g string) bool { return d.Editable[members.FieldGroup(g)] }

func (d memberData) CanEditAny() bool { return len(d.Editable) > 0 }

func (s *Server) memberPage(w http.ResponseWriter, r *http.Request, status int, d memberData) {
	ctx := r.Context()
	p := principalFrom(ctx)
	var err error
	if d.Types, err = s.opt.Members.ListTypes(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	if !d.New {
		if d.Payments, err = s.opt.Members.ListPayments(ctx, d.M.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
		if p.Can(auth.PermViewAudit) {
			if d.Audit, err = s.opt.Members.ListAudit(ctx, d.M.ID, 50); err != nil {
				s.serverError(w, r, err)
				return
			}
		}
	}
	d.Editable = map[members.FieldGroup]bool{}
	groups := p.EditableGroups()
	if d.New {
		groups = members.AllGroups // creation requires PermCreateMembers
	}
	for _, g := range groups {
		d.Editable[g] = true
	}
	d.Methods = members.PaymentMethods
	d.Now = s.opt.Now()
	d.Today = d.Now.Format(members.DateLayout)
	title := "New member"
	if !d.New {
		title = d.M.FullName()
	}
	s.render(w, r, status, "member", title, d)
}

func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(mux.Vars(r)[name], 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) loadMember(w http.ResponseWriter, r *http.Request) (*members.Member, bool) {
	id, ok := pathID(r, "id")
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "No such member.")
		return nil, false
	}
	m, err := s.opt.Members.GetMember(r.Context(), id)
	if errors.Is(err, members.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "No such member.")
		return nil, false
	} else if err != nil {
		s.serverError(w, r, err)
		return nil, false
	}
	return m, true
}

func (s *Server) handleMemberShow(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMember(w, r)
	if !ok {
		return
	}
	s.memberPage(w, r, http.StatusOK, memberData{M: m})
}

func (s *Server) handleMemberNew(w http.ResponseWriter, r *http.Request) {
	// Legacy default: new members use email 1 for the mailing list.
	s.memberPage(w, r, http.StatusOK, memberData{M: &members.Member{Email1Use: true}, New: true})
}

// parseMemberForm reads every member field from the posted form. Field
// names match the Member struct field names.
func parseMemberForm(r *http.Request) (*members.Member, map[string]string) {
	f := r.PostForm
	errs := map[string]string{}
	m := &members.Member{
		FirstName: f.Get("FirstName"), LastName: f.Get("LastName"), Gender: f.Get("Gender"),
		Occupation: f.Get("Occupation"),
		Address1:   f.Get("Address1"), Address2: f.Get("Address2"), Address3: f.Get("Address3"),
		Town: f.Get("Town"), Postcode: f.Get("Postcode"),
		Phone1: f.Get("Phone1"), Phone2: f.Get("Phone2"), Phone3: f.Get("Phone3"),
		Email1: f.Get("Email1"), Email2: f.Get("Email2"),
		Email1Use: f.Get("Email1Use") != "", Email2Use: f.Get("Email2Use") != "",
		ICEName: f.Get("ICEName"), ICERelation: f.Get("ICERelation"), ICEPhones: f.Get("ICEPhones"), ICEAddress: f.Get("ICEAddress"),
		BSACMemberID: f.Get("BSACMemberID"), BSACMemberIDIfKnown: f.Get("BSACMemberIDIfKnown"),
		BSACAbated: f.Get("BSACAbated") != "", BSACDirectDebit: f.Get("BSACDirectDebit") != "",
		JoinDateRaw: f.Get("JoinDateRaw"),
		Query:       f.Get("Query"), Notes: f.Get("Notes"),
		DiverGrades: f.Get("DiverGrades"), InstructorGrades: f.Get("InstructorGrades"),
		OtherQualifications: f.Get("OtherQualifications"), PreviousClub: f.Get("PreviousClub"),
		CommitteeMailing: f.Get("CommitteeMailing") != "", InstructorMailing: f.Get("InstructorMailing") != "",
		BoatHandlerMailing: f.Get("BoatHandlerMailing") != "", LifeguardMailing: f.Get("LifeguardMailing") != "",
		Driver:              f.Get("Driver") != "",
		ShareContactDetails: f.Get("ShareContactDetails") != "",
	}
	for field, dst := range map[string]*sql.NullTime{
		"DateOfBirth": &m.DateOfBirth, "BSACRenewalDue": &m.BSACRenewalDue,
		"JoinDate": &m.JoinDate, "MembershipExpiry": &m.MembershipExpiry,
	} {
		d, err := members.ParseDate(f.Get(field))
		if err != nil {
			errs[field] = "Enter a valid date."
		}
		*dst = d
	}
	if m.JoinDate.Valid {
		m.JoinDateRaw = "" // a real date supersedes the legacy text
	}
	fee, err := members.ParseMoney(f.Get("BSACAnnualFee"))
	if err != nil {
		errs["BSACAnnualFee"] = "Enter an amount such as 57 or 57.50."
	}
	m.BSACAnnualFee = fee
	if t := f.Get("TypeID"); t != "" {
		id, err := strconv.ParseInt(t, 10, 64)
		if err != nil || id <= 0 {
			errs["TypeID"] = "Choose a membership type."
		} else {
			m.TypeID = sql.NullInt64{Int64: id, Valid: true}
		}
	}
	return m, errs
}

func (s *Server) handleMemberCreate(w http.ResponseWriter, r *http.Request) {
	m, errs := parseMemberForm(r)
	m.Normalize()
	for k, v := range m.Validate() {
		errs[k] = v
	}
	if len(errs) > 0 {
		s.memberPage(w, r, http.StatusUnprocessableEntity, memberData{M: m, New: true, Errors: errs})
		return
	}
	p := principalFrom(r.Context())
	id, err := s.opt.Members.CreateMember(r.Context(), p.Actor(), m)
	if errors.Is(err, members.ErrDuplicateEmail) {
		s.memberPage(w, r, http.StatusUnprocessableEntity, memberData{M: m, New: true, Errors: map[string]string{"Email1": err.Error() + "."}})
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, s.url("/admin/members/"+strconv.FormatInt(id, 10)+"?flash=created"), http.StatusSeeOther)
}

func (s *Server) handleMemberUpdate(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	groups := p.EditableGroups()
	if len(groups) == 0 {
		s.renderError(w, r, http.StatusForbidden, "You do not have permission to edit members.")
		return
	}
	existing, ok := s.loadMember(w, r)
	if !ok {
		return
	}
	posted, errs := parseMemberForm(r)
	// Only fields of groups this user may edit are applied; anything else in
	// the form (e.g. crafted fields) is ignored.
	allowed := map[members.FieldGroup]bool{}
	for _, g := range groups {
		allowed[g] = true
	}
	members.Merge(existing, posted, groups...)
	existing.Normalize()
	for k, v := range existing.Validate() {
		errs[k] = v
	}
	// Report only problems the user can fix: a membership manager must not be
	// blocked by, say, an odd legacy email address they may not edit.
	for field := range errs {
		if !allowed[fieldGroup(field)] {
			delete(errs, field)
		}
	}
	if len(errs) > 0 {
		s.memberPage(w, r, http.StatusUnprocessableEntity, memberData{M: existing, Errors: errs})
		return
	}
	err := s.opt.Members.UpdateMember(r.Context(), p.Actor(), existing)
	if errors.Is(err, members.ErrDuplicateEmail) {
		s.memberPage(w, r, http.StatusUnprocessableEntity, memberData{M: existing, Errors: map[string]string{"Email1": err.Error() + "."}})
		return
	} else if errors.Is(err, members.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "No such member.")
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, s.url("/admin/members/"+strconv.FormatInt(existing.ID, 10)+"?flash=saved"), http.StatusSeeOther)
}

// fieldGroup returns the group of a form field that can carry an error.
func fieldGroup(field string) members.FieldGroup {
	switch field {
	case "DateOfBirth", "FirstName", "LastName", "Gender", "Email1", "Email2", "Email1Use", "Email2Use", "Postcode":
		return members.GroupPersonal
	default:
		return members.GroupMembership // BSACRenewalDue, JoinDate, MembershipExpiry, BSACAnnualFee, TypeID
	}
}

func (s *Server) handleMemberArchive(w http.ResponseWriter, r *http.Request) {
	s.setArchived(w, r, true)
}

func (s *Server) handleMemberRestore(w http.ResponseWriter, r *http.Request) {
	s.setArchived(w, r, false)
}

func (s *Server) setArchived(w http.ResponseWriter, r *http.Request, archive bool) {
	id, ok := pathID(r, "id")
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "No such member.")
		return
	}
	a := principalFrom(r.Context()).Actor()
	var err error
	flash := "archived"
	if archive {
		err = s.opt.Members.ArchiveMember(r.Context(), a, id)
	} else {
		err = s.opt.Members.RestoreMember(r.Context(), a, id)
		flash = "restored"
	}
	switch {
	case errors.Is(err, members.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "No such member.")
	case errors.Is(err, members.ErrDuplicateEmail):
		s.renderError(w, r, http.StatusConflict, "Cannot restore: another current member uses the same primary email.")
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, s.url("/admin/members/"+strconv.FormatInt(id, 10)+"?flash="+flash), http.StatusSeeOther)
	}
}
