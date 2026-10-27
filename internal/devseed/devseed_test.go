package devseed

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func TestMemberIsDeterministicAndInvented(t *testing.T) {
	today := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	gen := func() []string {
		r := rand.New(rand.NewPCG(1, 2))
		var out []string
		for i := range 40 {
			m := Member(r, i, today)
			if !strings.HasSuffix(m.Email1, "@example.org") || m.Validate()["Email1"] != "" {
				t.Errorf("member %d: bad email %q", i, m.Email1)
			}
			if errs := m.Validate(); len(errs) > 0 {
				t.Errorf("member %d invalid: %v", i, errs)
			}
			out = append(out, m.FirstName+" "+m.LastName+" "+m.Email1)
		}
		return out
	}
	a, b := gen(), gen()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("not deterministic at %d", i)
		}
	}
}
