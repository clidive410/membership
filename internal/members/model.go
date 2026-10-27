// Package members contains the membership domain model and its MariaDB
// repository.
package members

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Well-known membership type IDs (identical to the legacy IDs).
const (
	TypeAnnual        = 1
	TypeStandingOrder = 2
	TypeSocial        = 3
	TypeHonorary      = 4
	TypeThreeMonth    = 5
	TypeLapsed        = 6
)

// ErrNotFound is returned when a member or payment does not exist.
var ErrNotFound = errors.New("not found")

// DateLayout is the layout used for dates in forms, CSV and the database.
const DateLayout = "2006-01-02"

// MembershipType is a Clidive membership category.
type MembershipType struct {
	ID   int64
	Name string
}

// Member is one club member. Text fields use "" for "not set".
type Member struct {
	ID       int64
	LegacyID sql.NullInt64

	FirstName   string
	LastName    string
	Gender      string // "M", "F" or "" (not stated)
	DateOfBirth sql.NullTime
	Occupation  string

	Address1, Address2, Address3 string
	Town, Postcode               string
	Phone1, Phone2, Phone3       string

	Email1, Email2       string
	Email1Use, Email2Use bool

	ICEName, ICERelation, ICEPhones, ICEAddress string

	BSACMemberID        string
	BSACMemberIDIfKnown string
	BSACRenewalDue      sql.NullTime
	BSACAnnualFee       Money
	BSACAbated          bool
	BSACDirectDebit     bool

	JoinDate         sql.NullTime
	JoinDateRaw      string // legacy free text that could not be parsed
	TypeID           sql.NullInt64
	TypeName         string // read-only, joined from membership_types
	MembershipExpiry sql.NullTime
	Query            string
	Notes            string

	DiverGrades         string
	InstructorGrades    string
	OtherQualifications string
	PreviousClub        string

	CommitteeMailing   bool
	InstructorMailing  bool
	BoatHandlerMailing bool
	LifeguardMailing   bool
	Driver             bool

	LastPaid  sql.NullTime // read-only, latest payments.paid_on
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt sql.NullTime // soft delete ("archived")
}

// FullName returns "First Last".
func (m *Member) FullName() string {
	return strings.TrimSpace(m.FirstName + " " + m.LastName)
}

// Archived reports whether the member has been soft-deleted.
func (m *Member) Archived() bool { return m.DeletedAt.Valid }

// Validate checks the fields the committee must always provide.
func (m *Member) Validate() map[string]string {
	errs := map[string]string{}
	if strings.TrimSpace(m.FirstName) == "" {
		errs["FirstName"] = "First name is required."
	}
	if strings.TrimSpace(m.LastName) == "" {
		errs["LastName"] = "Last name is required."
	}
	if strings.TrimSpace(m.Email1) == "" {
		errs["Email1"] = "Email 1 is required: it is the member's identity."
	}
	if m.Gender != "" && m.Gender != "M" && m.Gender != "F" {
		errs["Gender"] = "Gender must be M, F or not stated."
	}
	for field, e := range map[string]string{"Email1": m.Email1, "Email2": m.Email2} {
		if _, dup := errs[field]; !dup && e != "" && !LooksLikeEmail(e) {
			errs[field] = "This does not look like an email address."
		}
	}
	if m.Email1Use && m.Email1 == "" {
		errs["Email1Use"] = "Cannot use an empty email 1 for mailing lists."
	}
	if m.Email2Use && m.Email2 == "" {
		errs["Email2Use"] = "Cannot use an empty email 2 for mailing lists."
	}
	if len(m.Postcode) > 10 {
		errs["Postcode"] = "Postcode is too long."
	}
	return errs
}

// LooksLikeEmail is a deliberately loose check (one @, a dot in the domain,
// no spaces).
func LooksLikeEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && at == strings.LastIndexByte(s, '@') &&
		strings.Contains(s[at+1:], ".") && !strings.ContainsAny(s, " \t\r\n,;<>")
}

// Payment is one recorded payment of club membership fees.
type Payment struct {
	ID        int64
	MemberID  int64
	PaidOn    time.Time
	Amount    Money
	Method    string
	Note      string
	Source    string // manual, bulk, legacy
	CreatedBy string
	CreatedAt time.Time
}

// Payment sources.
const (
	SourceManual = "manual"
	SourceBulk   = "bulk"
	SourceLegacy = "legacy"
)

// PaymentMethods are offered in the UI; free text is not allowed so the data
// stays reportable.
var PaymentMethods = []string{"", "Standing order", "Bank transfer", "Cash", "Cheque", "Card", "Other"}

// ValidPaymentMethod reports whether s is one of PaymentMethods.
func ValidPaymentMethod(s string) bool {
	for _, m := range PaymentMethods {
		if m == s {
			return true
		}
	}
	return false
}

// Money is a nullable amount in pence, stored as DECIMAL(10,2).
type Money struct {
	Pence int64
	Valid bool
}

// ParseMoney parses "57", "57.5" or "57.50" (an optional leading £ is
// ignored). The empty string yields an invalid (NULL) Money.
func ParseMoney(s string) (Money, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "£"))
	if s == "" {
		return Money{}, nil
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 2 {
		return Money{}, fmt.Errorf("invalid amount %q: at most 2 decimal places", s)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	w, err1 := strconv.ParseUint(whole, 10, 40)
	f, err2 := strconv.ParseUint(frac, 10, 8)
	if err1 != nil || err2 != nil {
		return Money{}, fmt.Errorf("invalid amount %q", s)
	}
	p := int64(w)*100 + int64(f)
	if neg {
		p = -p
	}
	return Money{Pence: p, Valid: true}, nil
}

// String formats as "57.00", or "" when NULL.
func (m Money) String() string {
	if !m.Valid {
		return ""
	}
	sign := ""
	p := m.Pence
	if p < 0 {
		sign, p = "-", -p
	}
	return fmt.Sprintf("%s%d.%02d", sign, p/100, p%100)
}

// Scan implements sql.Scanner.
func (m *Money) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*m = Money{}
		return nil
	case []byte:
		pm, err := ParseMoney(string(v))
		*m = pm
		return err
	case string:
		pm, err := ParseMoney(v)
		*m = pm
		return err
	case int64:
		*m = Money{Pence: v * 100, Valid: true}
		return nil
	case float64:
		pm, err := ParseMoney(strconv.FormatFloat(v, 'f', 2, 64))
		*m = pm
		return err
	}
	return fmt.Errorf("cannot scan %T into Money", src)
}

// Value implements driver.Valuer.
func (m Money) Value() (driver.Value, error) {
	if !m.Valid {
		return nil, nil
	}
	return m.String(), nil
}

// FormatDate formats a nullable date as YYYY-MM-DD ("" when NULL).
func FormatDate(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format(DateLayout)
}

// ParseDate parses YYYY-MM-DD; "" yields NULL.
func ParseDate(s string) (sql.NullTime, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return sql.NullTime{}, nil
	}
	t, err := time.Parse(DateLayout, s)
	if err != nil {
		return sql.NullTime{}, fmt.Errorf("invalid date %q (expected YYYY-MM-DD)", s)
	}
	return sql.NullTime{Time: t, Valid: true}, nil
}

// MembershipActive reports whether the member counts as a current, paid-up
// member on the given day: not archived, not of the Lapsed type, and with no
// Clidive expiry date or one that is today or later. (Many legacy members
// have no expiry date; they count as active unless Lapsed or archived.)
func (m *Member) MembershipActive(today time.Time) bool {
	if m.Archived() || (m.TypeID.Valid && m.TypeID.Int64 == TypeLapsed) {
		return false
	}
	if !m.MembershipExpiry.Valid {
		return true
	}
	y, mo, d := today.Date()
	return !m.MembershipExpiry.Time.Before(time.Date(y, mo, d, 0, 0, 0, 0, time.UTC))
}
