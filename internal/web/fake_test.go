package web

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
)

// fakeMembers is an in-memory MemberStore for handler tests.
type fakeMembers struct {
	mu          sync.Mutex
	members     map[int64]*members.Member
	payments    []members.Payment
	audit       []string // "actor action member"
	nextID      int64
	pingErr     error
	panicOnPing bool
}

func newFakeMembers() *fakeMembers {
	return &fakeMembers{members: map[int64]*members.Member{}, nextID: 100}
}

func (f *fakeMembers) add(m members.Member) *members.Member {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.ID == 0 {
		f.nextID++
		m.ID = f.nextID
	}
	f.members[m.ID] = &m
	return &m
}

var fakeTypes = []members.MembershipType{{ID: 1, Name: "Annual"}, {ID: 2, Name: "Standing Order"}, {ID: 3, Name: "Social"}, {ID: 4, Name: "Honorary"}, {ID: 5, Name: "3 Month (eg OD)"}, {ID: 6, Name: "Lapsed"}}

func typeName(id sql.NullInt64) string {
	for _, t := range fakeTypes {
		if id.Valid && t.ID == id.Int64 {
			return t.Name
		}
	}
	return ""
}

func (f *fakeMembers) ListTypes(context.Context) ([]members.MembershipType, error) {
	return fakeTypes, nil
}

func (f *fakeMembers) ListMembers(_ context.Context, flt members.Filter) ([]members.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []members.Member
	for _, m := range f.members {
		switch flt.Status {
		case members.StatusArchived:
			if !m.Archived() {
				continue
			}
		case members.StatusAll:
		case members.StatusNotLapsed:
			if m.Archived() || m.TypeID.Int64 == members.TypeLapsed {
				continue
			}
		default:
			if m.Archived() {
				continue
			}
		}
		if flt.TypeID > 0 && m.TypeID.Int64 != flt.TypeID {
			continue
		}
		if q := strings.ToLower(flt.Query); q != "" && !strings.Contains(strings.ToLower(m.FirstName+" "+m.LastName+" "+m.Email1), q) {
			continue
		}
		c := *m
		c.TypeName = typeName(c.TypeID)
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b members.Member) int { return strings.Compare(a.LastName, b.LastName) })
	return out, nil
}

func (f *fakeMembers) GetMember(_ context.Context, id int64) (*members.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.members[id]
	if !ok {
		return nil, members.ErrNotFound
	}
	c := *m
	c.TypeName = typeName(c.TypeID)
	return &c, nil
}

func (f *fakeMembers) logAudit(a members.Actor, action string) {
	f.audit = append(f.audit, a.Label+" "+action)
}

func (f *fakeMembers) emailTaken(email string, except int64) bool {
	for _, o := range f.members {
		if o.ID != except && !o.Archived() && o.Email1 == email {
			return true
		}
	}
	return false
}

func (f *fakeMembers) CreateMember(_ context.Context, a members.Actor, m *members.Member) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.emailTaken(m.Email1, 0) {
		return 0, members.ErrDuplicateEmail
	}
	f.nextID++
	m.ID = f.nextID
	c := *m
	f.members[m.ID] = &c
	f.logAudit(a, members.ActionMemberCreate)
	return m.ID, nil
}

func (f *fakeMembers) UpdateMember(_ context.Context, a members.Actor, m *members.Member) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.members[m.ID]; !ok {
		return members.ErrNotFound
	}
	if f.emailTaken(m.Email1, m.ID) {
		return members.ErrDuplicateEmail
	}
	c := *m
	f.members[m.ID] = &c
	f.logAudit(a, members.ActionMemberUpdate)
	return nil
}

func (f *fakeMembers) ArchiveMember(_ context.Context, a members.Actor, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.members[id]
	if !ok {
		return members.ErrNotFound
	}
	m.DeletedAt = sql.NullTime{Time: time.Now(), Valid: true}
	f.logAudit(a, members.ActionMemberArchive)
	return nil
}

func (f *fakeMembers) RestoreMember(_ context.Context, a members.Actor, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.members[id]
	if !ok {
		return members.ErrNotFound
	}
	m.DeletedAt = sql.NullTime{}
	f.logAudit(a, members.ActionMemberRestore)
	return nil
}

func (f *fakeMembers) ListPayments(_ context.Context, id int64) ([]members.Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []members.Payment
	for _, p := range f.payments {
		if p.MemberID == id {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeMembers) AddPayment(_ context.Context, a members.Actor, p *members.Payment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p.ID = int64(len(f.payments) + 1)
	p.CreatedBy = a.Label
	f.payments = append(f.payments, *p)
	f.logAudit(a, members.ActionPaymentAdd)
	return nil
}

func (f *fakeMembers) DeletePayment(_ context.Context, a members.Actor, memberID, pid int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.payments {
		if p.ID == pid && p.MemberID == memberID {
			f.payments = slices.Delete(f.payments, i, i+1)
			f.logAudit(a, members.ActionPaymentDelete)
			return nil
		}
	}
	return members.ErrNotFound
}

func (f *fakeMembers) RecordBulkPayment(_ context.Context, a members.Actor, ids []int64, bp members.BulkPayment) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, id := range ids {
		m, ok := f.members[id]
		if !ok || m.Archived() {
			continue
		}
		f.payments = append(f.payments, members.Payment{ID: int64(len(f.payments) + 1), MemberID: id, PaidOn: bp.PaidOn, Amount: bp.Amount, Source: members.SourceBulk, CreatedBy: a.Label})
		if bp.NewExpiry.Valid {
			m.MembershipExpiry = bp.NewExpiry
		}
		f.logAudit(a, members.ActionPaymentAdd)
		n++
	}
	return n, nil
}

func (f *fakeMembers) EmailList(_ context.Context, list members.MailingList, excludeLapsed bool) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	for _, m := range f.members {
		if m.Archived() || (excludeLapsed && m.TypeID.Int64 == members.TypeLapsed) {
			continue
		}
		if list == members.ListCommittee && !m.CommitteeMailing {
			continue
		}
		if m.Email1Use && len(m.Email1) > 5 {
			seen[m.Email1] = true
		}
		if m.Email2Use && len(m.Email2) > 5 {
			seen[m.Email2] = true
		}
	}
	var out []string
	for e := range seen {
		out = append(out, e)
	}
	slices.Sort(out)
	return out, nil
}

func (f *fakeMembers) ListAudit(context.Context, int64, int) ([]members.AuditEntry, error) {
	return nil, nil
}

func (f *fakeMembers) Ping(context.Context) error {
	if f.panicOnPing {
		panic("invented failure")
	}
	return f.pingErr
}

// fakeAuth is an in-memory AuthStore. Sign-in links look members up in fm.
type fakeAuth struct {
	mu       sync.Mutex
	users    map[string]fakeUser // by email
	sessions map[string]*auth.Principal
	tokens   map[string]*fakeToken
	fm       *fakeMembers
	lookups  int // LookupSession calls
	verifies int // VerifySession calls
	lastTTL  time.Duration
}

type fakeUser struct {
	password string
	p        auth.Principal
}

type fakeToken struct {
	userID  int64
	expires time.Time
	used    bool
}

func newFakeAuth() *fakeAuth {
	return &fakeAuth{users: map[string]fakeUser{}, sessions: map[string]*auth.Principal{}, tokens: map[string]*fakeToken{}}
}

func (f *fakeAuth) addUser(email, password string, memberID int64, roles ...auth.Role) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[email] = fakeUser{password: password, p: auth.Principal{UserID: int64(len(f.users) + 1), Email: email, MemberID: memberID, Roles: roles}}
}

// forMethod returns the principal of a session started with method.
func forMethod(p auth.Principal, method string) *auth.Principal {
	p.AuthMethod = method
	if method == auth.LoginMethodMagicLink {
		var roles []auth.Role
		for _, r := range p.Roles {
			if r == auth.RoleMember {
				roles = append(roles, r)
			}
		}
		p.Roles = roles
	}
	return &p
}

// session creates a password session directly and returns its token.
func (f *fakeAuth) session(email string) string { return f.sessionVia(email, auth.LoginMethodPassword) }

// sessionVia creates a session started with method.
func (f *fakeAuth) sessionVia(email, method string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := auth.NewToken()
	f.sessions[tok] = forMethod(f.users[email].p, method)
	return tok
}

func (f *fakeAuth) userByID(id int64) (string, fakeUser, bool) {
	for e, u := range f.users {
		if u.p.UserID == id {
			return e, u, true
		}
	}
	return "", fakeUser{}, false
}

func (f *fakeAuth) Authenticate(_ context.Context, email, password string) (*auth.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[email]
	if !ok || u.password != password {
		return nil, auth.ErrInvalidCredentials
	}
	return forMethod(u.p, auth.LoginMethodPassword), nil
}

func (f *fakeAuth) CreateSession(_ context.Context, userID int64, method string, ttl time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, u, ok := f.userByID(userID)
	if !ok {
		return "", errors.New("no user")
	}
	tok := auth.NewToken()
	f.sessions[tok] = forMethod(u.p, method)
	f.lastTTL = ttl
	return tok, nil
}

func (f *fakeAuth) LookupSession(_ context.Context, token string) (*auth.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	return f.sessions[token], nil
}

func (f *fakeAuth) DeleteSession(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, token)
	return nil
}

// currentMember returns the non-archived member with id, or nil.
func (f *fakeAuth) currentMember(id int64) *members.Member {
	if f.fm == nil || id == 0 {
		return nil
	}
	f.fm.mu.Lock()
	defer f.fm.mu.Unlock()
	if m, ok := f.fm.members[id]; ok && !m.Archived() {
		c := *m
		return &c
	}
	return nil
}

func (f *fakeAuth) RequestLoginLink(_ context.Context, email string, ttl time.Duration) (*auth.LoginLink, error) {
	email = members.NormalizeEmail(email)
	var ids []int64
	f.fm.mu.Lock()
	for _, m := range f.fm.members {
		if !m.Archived() && m.Email1 == email {
			ids = append(ids, m.ID)
		}
	}
	f.fm.mu.Unlock()
	if len(ids) != 1 {
		return nil, auth.ErrNoMember
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[email]
	if !ok {
		u = fakeUser{p: auth.Principal{UserID: int64(len(f.users) + 1), Email: email}}
	}
	if u.p.MemberID != 0 && u.p.MemberID != ids[0] {
		return nil, auth.ErrLoginNotAllowed
	}
	u.p.MemberID = ids[0]
	if !u.p.HasRole(auth.RoleMember) {
		u.p.Roles = append(slices.Clone(u.p.Roles), auth.RoleMember)
	}
	f.users[email] = u
	tok := auth.NewToken()
	f.tokens[tok] = &fakeToken{userID: u.p.UserID, expires: time.Now().Add(ttl)}
	return &auth.LoginLink{Token: tok, UserID: u.p.UserID, MemberID: ids[0]}, nil
}

func (f *fakeAuth) LoginWithToken(_ context.Context, token string) (*auth.Principal, error) {
	f.mu.Lock()
	t, ok := f.tokens[token]
	if !ok || t.used || time.Now().After(t.expires) {
		f.mu.Unlock()
		return nil, auth.ErrInvalidToken
	}
	t.used = true
	_, u, ok := f.userByID(t.userID)
	f.mu.Unlock()
	if !ok || f.currentMember(u.p.MemberID) == nil || !u.p.HasRole(auth.RoleMember) {
		return nil, auth.ErrInvalidToken
	}
	return forMethod(u.p, auth.LoginMethodMagicLink), nil
}

func (f *fakeAuth) VerifySession(_ context.Context, token string) (*auth.SessionStatus, error) {
	f.mu.Lock()
	f.verifies++
	p := f.sessions[token]
	f.mu.Unlock()
	if p == nil {
		return nil, nil
	}
	st := &auth.SessionStatus{UserID: p.UserID, Member: p.HasRole(auth.RoleMember), Committee: p.IsCommittee()}
	if m := f.currentMember(p.MemberID); m != nil {
		st.MemberID, st.Expiry = m.ID, m.MembershipExpiry
	}
	return st, nil
}
