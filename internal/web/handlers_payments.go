package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/clidive410/membership/internal/members"
)

func (s *Server) handlePaymentAdd(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMember(w, r)
	if !ok {
		return
	}
	f := r.PostForm
	paidOn, err := time.Parse(members.DateLayout, f.Get("PaidOn"))
	amount, err2 := members.ParseMoney(f.Get("Amount"))
	method := f.Get("Method")
	if err != nil || err2 != nil || !members.ValidPaymentMethod(method) {
		s.renderError(w, r, http.StatusUnprocessableEntity, "Please enter a valid payment date, amount (e.g. 57.50) and method.")
		return
	}
	p := &members.Payment{MemberID: m.ID, PaidOn: paidOn, Amount: amount, Method: method, Note: f.Get("Note"), Source: members.SourceManual}
	if err := s.opt.Members.AddPayment(r.Context(), principalFrom(r.Context()).Actor(), p); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, s.url("/admin/members/"+strconv.FormatInt(m.ID, 10)+"?flash=payment#payments"), http.StatusSeeOther)
}

func (s *Server) handlePaymentDelete(w http.ResponseWriter, r *http.Request) {
	id, ok1 := pathID(r, "id")
	pid, ok2 := pathID(r, "pid")
	if !ok1 || !ok2 {
		s.renderError(w, r, http.StatusNotFound, "No such payment.")
		return
	}
	err := s.opt.Members.DeletePayment(r.Context(), principalFrom(r.Context()).Actor(), id, pid)
	if errors.Is(err, members.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "No such payment.")
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, s.url("/admin/members/"+strconv.FormatInt(id, 10)+"?flash=payment-deleted#payments"), http.StatusSeeOther)
}

type bulkData struct {
	Types    []members.MembershipType
	TypeID   int64
	Members  []members.Member
	Selected bool // pre-tick every checkbox (legacy default)
	Sort     string
	Desc     bool
	Methods  []string
	Today    string
	Error    string
	Headers  []sortHeader
	Now      time.Time
}

func (s *Server) bulkPage(w http.ResponseWriter, r *http.Request, status int, typeID int64, errMsg string) {
	q := r.URL.Query()
	if typeID == 0 {
		typeID = members.TypeStandingOrder // legacy default filter
		if id, err := strconv.ParseInt(q.Get("type"), 10, 64); err == nil && id > 0 {
			typeID = id
		}
	}
	sort := q.Get("sort")
	if _, ok := members.SortColumns[sort]; !ok {
		sort = "last_name"
	}
	desc := q.Get("dir") == "desc"
	ms, err := s.opt.Members.ListMembers(r.Context(), members.Filter{TypeID: typeID, Status: members.StatusCurrent, Sort: sort, Desc: desc})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	types, err := s.opt.Members.ListTypes(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, status, "payments", "Payment management", bulkData{
		Types: types, TypeID: typeID, Members: ms, Selected: q.Get("select") != "none",
		Sort: sort, Desc: desc, Methods: members.PaymentMethods,
		Today: s.opt.Now().Format(members.DateLayout), Error: errMsg, Now: s.opt.Now(),
		Headers: sortHeaders(s.url("/admin/payments"), url.Values{"type": {strconv.FormatInt(typeID, 10)}}, sort, desc, [][2]string{
			{"first_name", "First name"}, {"last_name", "Last name"}, {"expiry", "Clidive expiry"},
			{"last_paid", "Last paid"}, {"notes", "Notes"},
		}),
	})
}

func (s *Server) handleBulkPaymentForm(w http.ResponseWriter, r *http.Request) {
	s.bulkPage(w, r, http.StatusOK, 0, "")
}

// handleBulkPayment replicates paymentmanagement.php: record a payment date
// for every selected member (optionally with amount/method and a new Clidive
// expiry date).
func (s *Server) handleBulkPayment(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	typeID, _ := strconv.ParseInt(f.Get("type"), 10, 64)
	paidOn, err := time.Parse(members.DateLayout, f.Get("PaidOn"))
	if err != nil {
		s.bulkPage(w, r, http.StatusUnprocessableEntity, typeID, "Enter the date of the payment.")
		return
	}
	amount, err := members.ParseMoney(f.Get("Amount"))
	if err != nil {
		s.bulkPage(w, r, http.StatusUnprocessableEntity, typeID, "Enter an amount such as 57 or 57.50, or leave it empty.")
		return
	}
	method := f.Get("Method")
	if !members.ValidPaymentMethod(method) {
		s.bulkPage(w, r, http.StatusUnprocessableEntity, typeID, "Choose a payment method from the list.")
		return
	}
	expiry, err := members.ParseDate(f.Get("NewExpiry"))
	if err != nil {
		s.bulkPage(w, r, http.StatusUnprocessableEntity, typeID, "Enter a valid new expiry date, or leave it empty.")
		return
	}
	var ids []int64
	for _, v := range f["member"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		s.bulkPage(w, r, http.StatusUnprocessableEntity, typeID, "Select at least one member.")
		return
	}
	n, err := s.opt.Members.RecordBulkPayment(r.Context(), principalFrom(r.Context()).Actor(), ids, members.BulkPayment{
		PaidOn: paidOn, Amount: amount, Method: method, Note: f.Get("Note"), NewExpiry: expiry,
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, s.url("/admin/payments?type="+strconv.FormatInt(typeID, 10)+"&flash=bulk&n="+strconv.Itoa(n)), http.StatusSeeOther)
}
