package members

import (
	"database/sql"
	"testing"
)

func TestMergeOnlyCopiesAllowedGroups(t *testing.T) {
	dst := &Member{FirstName: "Old", TypeID: sql.NullInt64{Int64: 1, Valid: true}, Driver: false}
	src := &Member{FirstName: "New", TypeID: sql.NullInt64{Int64: 6, Valid: true}, Driver: true}
	Merge(dst, src, GroupMembership)
	if dst.FirstName != "Old" || dst.TypeID.Int64 != 6 || dst.Driver {
		t.Errorf("membership merge copied the wrong fields: %+v", dst)
	}
	Merge(dst, src, GroupPersonal, GroupClub)
	if dst.FirstName != "New" || !dst.Driver {
		t.Errorf("personal/club merge failed: %+v", dst)
	}
}

func TestDiff(t *testing.T) {
	a := &Member{FirstName: "Ann", Email1: "ann@example.org"}
	b := *a
	b.FirstName = "Anne"
	b.Driver = true
	d := Diff(a, &b)
	if len(d) != 2 || d["first_name"] != [2]string{"Ann", "Anne"} || d["driver"] != [2]string{"0", "1"} {
		t.Errorf("Diff = %v", d)
	}
	if c := Diff(nil, a); c["email1"][1] != "ann@example.org" || len(c) != 2 {
		t.Errorf("creation diff = %v", c)
	}
}

func TestNormalize(t *testing.T) {
	m := &Member{Email1: "  Ann.Diver@Example.ORG ", Postcode: " se1 9aa"}
	m.Normalize()
	if m.Email1 != "ann.diver@example.org" || m.Postcode != "SE1 9AA" {
		t.Errorf("Normalize: %+v", m)
	}
}
