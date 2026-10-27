package legacy

import (
	"testing"
	"unicode/utf8"
)

// mojibake encodes s as UTF-8 and decodes those bytes as cp1252, which is
// what MariaDB does when it converts UTF-8 bytes stored in a latin1 column.
func mojibake(s string) string {
	byByte := map[byte]rune{}
	for r, b := range cp1252High {
		byByte[b] = r
	}
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); i++ {
		if r, ok := byByte[s[i]]; ok {
			out = append(out, r)
		} else {
			out = append(out, rune(s[i]))
		}
	}
	return string(out)
}

func TestMojibakeHelperMatchesKnownForms(t *testing.T) {
	for in, want := range map[string]string{
		"–": "â€“", "é": "Ã©", "ü": "Ã¼", "£": "Â£", "’": "â€™",
	} {
		if got := mojibake(in); got != want {
			t.Errorf("mojibake(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepairMojibake(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"en dash", "Pool session â€“ Tuesday", "Pool session – Tuesday"},
		{"e acute", "RenÃ©e", "Renée"},
		{"u umlaut", "MÃ¼ller", "Müller"},
		{"pound sign", "Paid Â£57.50 cash", "Paid £57.50 cash"},
		{"curly apostrophe", "Oâ€™Brien", "O’Brien"},
		{"mixed", "CafÃ© Â£3 â€“ Ã¼ber", "Café £3 – über"},
		{"encoded twice", mojibake(mojibake("Zoë – £5")), "Zoë – £5"},
		{"C1 byte from undefined cp1252 slot", mojibake("Ý"), "Ý"}, // C3 9D: 0x9D is undefined in cp1252
		{"ascii", "Plain text, 123 Example Road", "Plain text, 123 Example Road"},
		{"empty", "", ""},
		// Correct UTF-8 must not change.
		{"correct e acute", "Renée", "Renée"},
		{"correct pound and dash", "£57.50 – paid", "£57.50 – paid"},
		{"correct umlaut", "Müller", "Müller"},
		{"correct non-latin", "Łódź ☃", "Łódź ☃"},
		{"correct latin1-only accents", "Ångström façade", "Ångström façade"},
	}
	for _, c := range cases {
		got := RepairMojibake(c.in)
		if got != c.want {
			t.Errorf("%s: RepairMojibake(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		if again := RepairMojibake(got); again != got {
			t.Errorf("%s: not idempotent: %q -> %q", c.name, got, again)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: invalid UTF-8 result", c.name)
		}
	}
}

func TestRepairRoundTrip(t *testing.T) {
	for _, s := range []string{"é", "ü", "£", "–", "—", "€", "“quoted”", "naïve café", "Ærø"} {
		if got := RepairMojibake(mojibake(s)); got != s {
			t.Errorf("round trip %q: got %q", s, got)
		}
	}
}

func FuzzRepairMojibakeIdempotent(f *testing.F) {
	for _, s := range []string{"Ã©", "é", "â€“", "abc", "Â£"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			return
		}
		once := RepairMojibake(s)
		if RepairMojibake(once) != once {
			t.Errorf("not idempotent for %q", s)
		}
		if RepairMojibake(mojibake(s)) != RepairMojibake(s) && RepairMojibake(s) == s {
			// s is a fixpoint, so its mojibake must repair back to it.
			t.Errorf("mojibake(%q) did not repair back", s)
		}
	})
}
