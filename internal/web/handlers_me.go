package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
)

type meData struct {
	M        *members.Member
	Payments []members.Payment
	Now      time.Time
}

// handleMe shows the signed-in user their own membership record,
// read-only. Requests for changes go to the membership secretary by email.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.MemberID == 0 {
		if p.Can(auth.PermViewMembers) {
			http.Redirect(w, r, s.url("/admin/members"), http.StatusSeeOther)
			return
		}
		s.renderError(w, r, http.StatusNotFound, "Your sign-in is not linked to a membership record. Please email membership@clidive.org.")
		return
	}
	m, err := s.opt.Members.GetMember(r.Context(), p.MemberID)
	if errors.Is(err, members.ErrNotFound) || (err == nil && m.Archived()) {
		s.renderError(w, r, http.StatusForbidden, "Your membership record is no longer current. Please email membership@clidive.org.")
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	payments, err := s.opt.Members.ListPayments(r.Context(), m.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "me", "My membership", meData{M: m, Payments: payments, Now: s.opt.Now()})
}
