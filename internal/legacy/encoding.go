package legacy

import (
	"unicode/utf8"
)

// The legacy `membership` table is declared latin1, but the PHP site wrote
// UTF-8 bytes into it. Reading those columns through a utf8mb4 connection
// makes MariaDB convert each stored byte from latin1, so "é" (C3 A9) arrives
// as "Ã©" and an en dash (E2 80 93) as "â€“". RepairMojibake undoes that.

// cp1252High maps the runes that MariaDB's latin1 (which is Windows-1252)
// produces for bytes 0x80-0x9F back to those bytes. MariaDB maps the five
// bytes undefined in cp1252 (0x81, 0x8D, 0x8F, 0x90, 0x9D) to the C1 control
// of the same value; those, and the rest of U+0080-U+00FF, map to themselves.
var cp1252High = map[rune]byte{
	'€': 0x80, '‚': 0x82, 'ƒ': 0x83, '„': 0x84, '…': 0x85, '†': 0x86, '‡': 0x87,
	'ˆ': 0x88, '‰': 0x89, 'Š': 0x8A, '‹': 0x8B, 'Œ': 0x8C, 'Ž': 0x8E,
	'‘': 0x91, '’': 0x92, '“': 0x93, '”': 0x94, '•': 0x95, '–': 0x96, '—': 0x97,
	'˜': 0x98, '™': 0x99, 'š': 0x9A, '›': 0x9B, 'œ': 0x9C, 'ž': 0x9E, 'Ÿ': 0x9F,
}

// latin1Byte returns the cp1252/latin1 byte for r, if there is one.
func latin1Byte(r rune) (byte, bool) {
	if r <= 0xFF {
		return byte(r), true
	}
	b, ok := cp1252High[r]
	return b, ok
}

// repairOnce reverses one round of "UTF-8 bytes decoded as latin1/cp1252".
// It returns s unchanged unless every rune maps to a latin1/cp1252 byte and
// those bytes form valid UTF-8 containing at least one multi-byte sequence.
func repairOnce(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	b := make([]byte, 0, len(s))
	for _, r := range s {
		c, ok := latin1Byte(r)
		if !ok {
			// A rune outside latin1/cp1252 (e.g. "€" is fine, but "ł" or an
			// emoji is not) means the text is already real UTF-8.
			return s
		}
		b = append(b, c)
	}
	if !utf8.Valid(b) {
		// e.g. a correct "é" maps to the lone byte E9, which is not UTF-8.
		return s
	}
	return string(b)
}

// RepairMojibake fixes text that was double-encoded (UTF-8 bytes read as
// latin1/cp1252), repeating until nothing changes so that text encoded twice
// over is fixed too. Correct UTF-8 and plain ASCII come back unchanged, and
// RepairMojibake(RepairMojibake(s)) == RepairMojibake(s).
func RepairMojibake(s string) string {
	for {
		r := repairOnce(s)
		if r == s {
			return s
		}
		s = r // each round strictly shortens the string, so this ends
	}
}
