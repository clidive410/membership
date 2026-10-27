package members

import "testing"

func TestParseMoney(t *testing.T) {
	cases := map[string]string{"": "", "57": "57.00", "57.5": "57.50", "£12.34": "12.34", "0": "0.00", ".5": "0.50", "-3": "-3.00"}
	for in, want := range cases {
		m, err := ParseMoney(in)
		if err != nil {
			t.Errorf("ParseMoney(%q): %v", in, err)
			continue
		}
		if m.String() != want {
			t.Errorf("ParseMoney(%q) = %q, want %q", in, m.String(), want)
		}
	}
	for _, bad := range []string{"abc", "1.234", "1.2.3", "12,50"} {
		if _, err := ParseMoney(bad); err == nil {
			t.Errorf("ParseMoney(%q) should fail", bad)
		}
	}
}

func TestMoneyScan(t *testing.T) {
	var m Money
	if err := m.Scan([]byte("71.00")); err != nil || m.Pence != 7100 {
		t.Errorf("scan: %v %+v", err, m)
	}
	if err := m.Scan(nil); err != nil || m.Valid {
		t.Errorf("scan nil: %v %+v", err, m)
	}
}

func TestValidate(t *testing.T) {
	m := &Member{FirstName: "Ada", LastName: "Diver", Email1: "ada@example.org", Email1Use: true}
	if errs := m.Validate(); len(errs) != 0 {
		t.Errorf("unexpected errors %v", errs)
	}
	m = &Member{Email1: "not-an-email", Email2Use: true, Gender: "X"}
	errs := m.Validate()
	for _, f := range []string{"FirstName", "LastName", "Email1", "Email2Use", "Gender"} {
		if _, ok := errs[f]; !ok {
			t.Errorf("expected error for %s, got %v", f, errs)
		}
	}
}

func TestLooksLikeEmail(t *testing.T) {
	for s, want := range map[string]bool{"a@b.co": true, "a@b": false, "@b.co": false, "a b@c.de": false, "a@b@c.de": false} {
		if LooksLikeEmail(s) != want {
			t.Errorf("LooksLikeEmail(%q) != %v", s, want)
		}
	}
}
