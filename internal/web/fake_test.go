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
	mu       sync.Mutex
	members  map[int64]*members.Member
	payments []members.Payment
	audit    []string // "actor action member"
	nextID   int64
	pingErr  error
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

func (f *fakeMembers) Ping(context.Context) error { return f.pingErr }

// fakeAuth is an in-memory AuthStore.
type fakeAuth struct {
	mu       sync.Mutex
	users    map[string]fakeUser // by email
	sessions map[string]*auth.Principal
}

type fakeUser struct {
	password string
	p        auth.Principal
}

func newFakeAuth() *fakeAuth {
	return &fakeAuth{users: map[string]fakeUser{}, sessions: map[string]*auth.Principal{}}
}

func (f *fakeAuth) addUser(email, password string, memberID int64, roles ...auth.Role) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[email] = fakeUser{password: password, p: auth.Principal{UserID: int64(len(f.users) + 1), Email: email, MemberID: memberID, Roles: roles}}
}

// session creates a session directly and returns its token.
func (f *fakeAuth) session(email string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.users[email]
	tok := auth.NewToken()
	p := u.p
	f.sessions[tok] = &p
	return tok
}

func (f *fakeAuth) Authenticate(_ context.Context, email, password string) (*auth.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[email]
	if !ok || u.password != password {
		return nil, auth.ErrInvalidCredentials
	}
	p := u.p
	return &p, nil
}

func (f *fakeAuth) CreateSession(_ context.Context, userID int64, _ string, _ time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.p.UserID == userID {
			tok := auth.NewToken()
			p := u.p
			f.sessions[tok] = &p
			return tok, nil
		}
	}
	return "", errors.New("no user")
}

func (f *fakeAuth) LookupSession(_ context.Context, token string) (*auth.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[token], nil
}

func (f *fakeAuth) DeleteSession(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, token)
	return nil
}
