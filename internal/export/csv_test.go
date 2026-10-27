package export

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/clidive410/membership/internal/members"
)

func TestWriteMembersCSV(t *testing.T) {
	ms := []members.Member{
		{
			ID: 7, FirstName: "Ada", LastName: "O'Diver, Jr", Gender: "F",
			DateOfBirth: sql.NullTime{Time: time.Date(1990, 5, 4, 0, 0, 0, 0, time.UTC), Valid: true},
			Phone1:      "+44 20 0000 0000",
			Email1:      "ada@example.org", Email1Use: true,
			BSACAnnualFee: members.Money{Pence: 7100, Valid: true},
			TypeName:      "Standing Order",
			Notes:         "line one\nline \"two\"",
			Query:         "=HYPERLINK(\"http://evil.example\")",
			Driver:        true,
		},
		{ID: 8, FirstName: "Bob", LastName: "Snorkel", DeletedAt: sql.NullTime{Time: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true}},
	}
	var buf bytes.Buffer
	if err := WriteMembersCSV(&buf, ms); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "\ufeff") {
		t.Error("missing UTF-8 BOM")
	}
	recs, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(out, "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d records", len(recs))
	}
	col := func(row int, name string) string {
		for i, h := range recs[0] {
			if h == name {
				return recs[row][i]
			}
		}
		t.Fatalf("no column %s", name)
		return ""
	}
	want := map[string]string{
		"MemberID": "7", "Lastname": "O'Diver, Jr", "Gender": "F", "DOB": "1990-05-04",
		"Telephone1": "+44 20 0000 0000", "Email1Use": "Yes", "Email2Use": "", "BSACAnnualFee": "71.00",
		"ClidiveMembershipType": "Standing Order", "Notes": "line one\nline \"two\"",
		"Query": "'=HYPERLINK(\"http://evil.example\")", "Driver": "Yes", "ArchivedDate": "",
	}
	for k, v := range want {
		if got := col(1, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if col(2, "ArchivedDate") != "2020-01-02" {
		t.Errorf("ArchivedDate = %q", col(2, "ArchivedDate"))
	}
	if len(recs[1]) != len(Header) {
		t.Errorf("record has %d fields, header %d", len(recs[1]), len(Header))
	}
}

func TestSafeCell(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"plain":        "plain",
		"=1+1":         "'=1+1",
		"@SUM(A1)":     "'@SUM(A1)",
		"+44 1234 567": "+44 1234 567",
		"-12.50":       "-12.50",
		"+cmd|' /C'":   "'+cmd|' /C'",
		"-2+3+cmd":     "'-2+3+cmd",
	}
	for in, want := range cases {
		if got := SafeCell(in); got != want {
			t.Errorf("SafeCell(%q) = %q, want %q", in, got, want)
		}
	}
}
