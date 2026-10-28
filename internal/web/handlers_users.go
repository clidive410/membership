package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
)

// accessView is one login's portal access, as shown on the member page
// card and on each row of /admin/users.
type accessView struct {
	User      *auth.User  // nil: no login yet
	Email     string      // the login's email, or the member's primary email
	Current   auth.Access // AccessNone without a login
	Self      bool        // the viewer's own login: no demotion (lock-out)
	CanManage bool        // the viewer may change it (admins)
	Action    string      // form action (in-app URL)
}

// Levels lists the choices of the role select.
func (v accessView) Levels() []auth.Access { return auth.AccessLevels }

func newAccessView(p *auth.Principal, u *auth.User, email, action string) accessView {
	v := accessView{User: u, Email: email, Current: auth.AccessNone, CanManage: p.Can(auth.PermManageUsers), Action: action}
	if u != nil {
		v.Email, v.Current, v.Self = u.Email, u.Access(), u.ID == p.UserID
	}
	return v
}

// accessName labels an access level in the UI.
func accessName(a auth.Access) string {
	switch a {
	case auth.AccessAdmin:
		return "Admin"
	case auth.AccessMembershipManager:
		return "Membership manager"
	case auth.AccessMember:
		return "Member"
	}
	return "No access"
}

type usersData struct {
	Rows []accessView
}

func (s *Server) handleUserList(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	users, err := s.opt.Auth.ListUsers(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := usersData{}
	for i := range users {
		u := &users[i]
		d.Rows = append(d.Rows, newAccessView(p, u, u.Email, s.url("/admin/users/"+strconv.FormatInt(u.ID, 10)+"/access")))
	}
	s.render(w, r, http.StatusOK, "users", "Portal access", d)
}

// handleUserInvite is the "Invite by email" form of /admin/users, for
// committee members with no member record.
func (s *Server) handleUserInvite(w http.ResponseWriter, r *http.Request) {
	email := members.NormalizeEmail(r.PostForm.Get("email"))
	back := s.url("/admin/users")
	if !members.LooksLikeEmail(email) || len(email) > 254 {
		http.Redirect(w, r, back+"?flash=bad-email", http.StatusSeeOther)
		return
	}
	u, err := s.opt.Auth.UserForMember(r.Context(), 0, email) // member id 0 matches no member: lookup by email
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.changeAccess(w, r, email, 0, u, "invite", back, "")
}

func (s *Server) handleUserAccess(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "uid")
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "No such login.")
		return
	}
	u, err := s.opt.Auth.GetUser(r.Context(), id)
	if errors.Is(err, members.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "No such login.")
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.changeAccess(w, r, u.Email, 0, u, r.PostForm.Get("action"), s.url("/admin/users"), "#user-"+strconv.FormatInt(u.ID, 10))
}

// handleMemberAccess is the "Portal access" card of a member page. The
// login is the one linked to the member or, failing that, the one with the
// member's primary email (created if needed).
func (s *Server) handleMemberAccess(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMember(w, r)
	if !ok {
		return
	}
	back := s.url("/admin/members/" + strconv.FormatInt(m.ID, 10))
	u, err := s.opt.Auth.UserForMember(r.Context(), m.ID, m.Email1)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	email := m.Email1
	if u != nil {
		email = u.Email
	}
	if email == "" {
		http.Redirect(w, r, back+"?flash=no-email#access", http.StatusSeeOther)
		return
	}
	memberID := m.ID
	if m.Archived() {
		memberID = 0 // never link a login to an archived member
	}
	s.changeAccess(w, r, email, memberID, u, r.PostForm.Get("action"), back, "#access")
}

// changeAccess applies the posted access level ("access") and, for action
// "invite", emails a set-password link. It redirects to back (plus a
// flash and the fragment) when done.
func (s *Server) changeAccess(w http.ResponseWriter, r *http.Request, email string, memberID int64, target *auth.User, action, back, fragment string) {
	p := principalFrom(r.Context())
	done := func(flash string) { http.Redirect(w, r, back+"?flash="+flash+fragment, http.StatusSeeOther) }

	access := auth.Access(r.PostForm.Get("access"))
	if !auth.ValidAccess(access) {
		s.renderError(w, r, http.StatusBadRequest, "Unknown access level.")
		return
	}
	if target != nil && target.ID == p.UserID && access != auth.AccessAdmin {
		done("self-lockout")
		return
	}
	if action == "invite" && access == auth.AccessNone {
		done("choose-role")
		return
	}
	current := auth.AccessNone
	if target != nil {
		current = target.Access()
	}
	if access != current || target == nil {
		_, err := s.opt.Auth.SetAccess(r.Context(), p.Actor(), auth.AccessRequest{Email: email, MemberID: memberID, Access: access})
		switch {
		case errors.Is(err, auth.ErrSelfLockout):
			done("self-lockout")
			return
		case errors.Is(err, auth.ErrLinkConflict):
			done("link-conflict")
			return
		case err != nil:
			s.serverError(w, r, err)
			return
		}
	}
	if action != "invite" {
		if access == auth.AccessNone {
			done("access-revoked")
		} else {
			done("access-saved")
		}
		return
	}
	inv, err := s.opt.Auth.InviteUser(r.Context(), p.Actor(), auth.InviteRequest{Email: email, MemberID: memberID, TTL: InviteTTL})
	switch {
	case errors.Is(err, auth.ErrLinkConflict):
		done("link-conflict")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	msg, err := InviteMessage(s.opt.PublicURL, inv, InviteTTL)
	if err == nil {
		err = s.opt.Mailer.Send(r.Context(), msg)
	}
	if err != nil {
		s.opt.Log.Error("emailing invitation failed", "user_id", inv.UserID, "err", err)
		done("invite-failed")
		return
	}
	s.opt.Log.Info("invitation emailed", "user_id", inv.UserID, "by", p.UserID)
	done("invite-sent")
}
