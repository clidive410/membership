package legacy

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/clidive410/membership/internal/members"
)

func ns(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

var known = map[int64]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true}
var now = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// fakeRow is an invented member; no real data.
func fakeRow() Row {
	return Row{
		MemberID:                42,
		FirstName:               ns("  Testy "),
		LastName:                ns("McTestface"),
		DOB:                     ns("1980-02-29 00:00:00"),
		Occupation:              ns("Lighthouse keeper"),
		Address1:                ns("1 Example Street"),
		Town:                    ns("London"),
		Postcode:                ns("zz1 1zz"),
		Telephone1:              ns("+44 20 0000 0000"),
		Email1:                  ns(" Testy@Example.ORG "),
		Email2:                  ns("testy.alt@example.net"),
		Email1Use:               ns("1"),
		Email2Use:               ns("0"),
		ICEName:                 ns("Next Of Kin"),
		ICERelation:             ns("Sibling"),
		ICETelephones:           ns("07000 000000"),
		BSACMemberID:            ns("123456"),
		BSACRenewalDueDate:      ns("2026-04-01 00:00:00"),
		BSACAnnualFee:           ns("71"),
		BSACAbated:              ns("1"),
		ClidiveJoinDate:         ns("2015-3-7"),
		ClidiveMembershipTypeID: ns("2"),
		ClidiveMembershipExpiry: ns("0000-00-00 00:00:00"),
		Notes:                   ns("Invented note"),
		OtherDiverGrades:        ns("Sports Diver"),
		Gender:                  ns("1"),
		BSACDirectDebit:         ns("1"),
		InstructorMailing:       ns("0"),
		CommitteeMailing:        ns("1"),
		BoatHandlerMailing:      ns("1"),
		LifeguardMailing:        ns("0"),
		Driver:                  ns("1"),
		LastPaid:                ns("2026-01-15"),
	}
}

func TestMapFullRow(t *testing.T) {
	got := Map(fakeRow(), known, now)
	m := got.Member
	if len(got.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", got.Warnings)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"ID", m.ID, int64(42)},
		{"LegacyID", m.LegacyID.Int64, int64(42)},
		{"FirstName", m.FirstName, "Testy"},
		{"Gender", m.Gender, "F"},
		{"DOB", members.FormatDate(m.DateOfBirth), "1980-02-29"},
		{"Postcode", m.Postcode, "ZZ1 1ZZ"},
		{"Email1 normalised", m.Email1, "testy@example.org"},
		{"Email1Use", m.Email1Use, true},
		{"Email2Use", m.Email2Use, false},
		{"ICEPhones", m.ICEPhones, "07000 000000"},
		{"BSACRenewalDue", members.FormatDate(m.BSACRenewalDue), "2026-04-01"},
		{"BSACAnnualFee", m.BSACAnnualFee.String(), "71.00"},
		{"BSACAbated", m.BSACAbated, true},
		{"BSACDirectDebit", m.BSACDirectDebit, true},
		{"JoinDate", members.FormatDate(m.JoinDate), "2015-03-07"},
		{"JoinDateRaw", m.JoinDateRaw, ""},
		{"TypeID", m.TypeID.Int64, int64(2)},
		{"Expiry zero date is NULL", m.MembershipExpiry.Valid, false},
		{"DiverGrades", m.DiverGrades, "Sports Diver"},
		{"CommitteeMailing", m.CommitteeMailing, true},
		{"InstructorMailing", m.InstructorMailing, false},
		{"BoatHandlerMailing", m.BoatHandlerMailing, true},
		{"Driver", m.Driver, true},
		{"not archived", m.DeletedAt.Valid, false},
		{"LastPaid", members.FormatDate(got.LastPaid), "2026-01-15"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestMapNullsAndOddities(t *testing.T) {
	r := Row{
		MemberID:                7,
		FirstName:               ns("Nul"),
		LastName:                ns("Values"),
		Gender:                  sql.NullString{},
		DOB:                     ns("2001-02-30 00:00:00"), // impossible date
		ClidiveJoinDate:         ns("getdate()"),
		ClidiveMembershipTypeID: ns("99"),
		Email1Use:               ns("1"), // but no email1
		DeletedOn:               ns("2019-06-01 10:11:12"),
	}
	got := Map(r, known, now)
	m := got.Member
	if m.Gender != "" || m.DateOfBirth.Valid || m.JoinDate.Valid || m.JoinDateRaw != "" || m.TypeID.Valid || m.Email1Use {
		t.Errorf("unexpected mapping: %+v", m)
	}
	if !m.DeletedAt.Valid || m.DeletedAt.Time.Format(time.DateTime) != "2019-06-01 10:11:12" {
		t.Errorf("DeletedAt = %v", m.DeletedAt)
	}
	if got.LastPaid.Valid {
		t.Error("LastPaid should be NULL")
	}
	cols := map[string]bool{}
	for _, w := range got.Warnings {
		cols[w.Column] = true
		if w.MemberID != 7 {
			t.Errorf("warning for wrong member: %v", w)
		}
	}
	if !cols["DOB"] || !cols["ClidiveMembershipTypeId"] {
		t.Errorf("expected DOB and type warnings, got %v", got.Warnings)
	}
}

func TestJoinDateFormats(t *testing.T) {
	cases := []struct{ in, want, raw string }{
		{"2010-1-2", "2010-01-02", ""},
		{"2010-11-22", "2010-11-22", ""},
		{"03.04.05", "2005-04-03", ""},
		{"03/04/98", "1998-04-03", ""},
		{"03/04/98.", "1998-04-03", ""},
		{"25/12/2001", "2001-12-25", ""},
		{"", "", ""},
		{"sometime in spring", "", "sometime in spring"},
		{"31/02/05", "", "31/02/05"},
	}
	for _, c := range cases {
		mp := &mapper{id: 1}
		d, raw := mp.joinDate(ns(c.in), now)
		if members.FormatDate(d) != c.want || raw != c.raw {
			t.Errorf("joinDate(%q) = %q, %q; want %q, %q", c.in, members.FormatDate(d), raw, c.want, c.raw)
		}
	}
}

func TestSelectQuerySkipsLoginColumns(t *testing.T) {
	q := SelectQuery()
	for _, c := range excludedColumns {
		if strings.Contains(q, "`"+c+"`") {
			t.Errorf("legacy query must not read %s: %s", c, q)
		}
	}
	r := fakeRow()
	if n := len(r.scanTargets()); n != len(selectColumns) {
		t.Errorf("scan targets (%d) and columns (%d) differ", n, len(selectColumns))
	}
}

func TestWarningHasNoValues(t *testing.T) {
	r := fakeRow()
	r.DOB = ns("not-a-date-secret")
	got := Map(r, known, now)
	for _, w := range got.Warnings {
		if strings.Contains(w.String(), "secret") {
			t.Errorf("warning leaks the value: %s", w)
		}
	}
}
