// Package export writes member data as CSV (replacing the legacy
// fulllistexport.php "Excel" export) and formats mailing lists.
package export

import (
	"database/sql"
	"encoding/csv"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/clidive410/membership/internal/members"
)

// Header is the CSV header row. It follows the legacy export's column order,
// with the fields the legacy export omitted added (email use flags, BSAC id
// if known, join date text, last paid, query, previous club).
var Header = []string{
	"MemberID", "Firstname", "Lastname", "Gender", "DOB", "Occupation",
	"Address1", "Address2", "Address3", "Town", "Postcode",
	"Telephone1", "Telephone2", "Telephone3", "Email1", "Email2", "Email1Use", "Email2Use",
	"ICEname", "ICErelation", "ICEtelephones", "ICEaddresses",
	"BSACMemberid", "BSACMemberidIfKnown", "BSACRenewalDueDate", "BSACAnnualFee", "BSACAbated", "BSACDirectDebit",
	"ClidiveJoinDate", "ClidiveJoinDateText", "ClidiveMembershipType", "ClidiveMembershipExpiry", "LastPaid",
	"ArchivedDate", "Query", "Notes",
	"OtherDiverGrades", "OtherInstructorGrades", "OtherQualifications", "PreviousDiveClub",
	"Committee", "Instructor", "Boathandler", "Lifeguard", "Driver", "ShareContactDetails",
}

func yes(b bool) string {
	if b {
		return "Yes"
	}
	return ""
}

func gender(g string) string { return g } // "M", "F" or ""

func date(t sql.NullTime) string { return members.FormatDate(t) }

// Record returns the CSV fields for one member, in Header order.
func Record(m *members.Member) []string {
	return []string{
		strconv.FormatInt(m.ID, 10), m.FirstName, m.LastName, gender(m.Gender), date(m.DateOfBirth), m.Occupation,
		m.Address1, m.Address2, m.Address3, m.Town, m.Postcode,
		m.Phone1, m.Phone2, m.Phone3, m.Email1, m.Email2, yes(m.Email1Use), yes(m.Email2Use),
		m.ICEName, m.ICERelation, m.ICEPhones, m.ICEAddress,
		m.BSACMemberID, m.BSACMemberIDIfKnown, date(m.BSACRenewalDue), m.BSACAnnualFee.String(), yes(m.BSACAbated), yes(m.BSACDirectDebit),
		date(m.JoinDate), m.JoinDateRaw, m.TypeName, date(m.MembershipExpiry), date(m.LastPaid),
		date(m.DeletedAt), m.Query, m.Notes,
		m.DiverGrades, m.InstructorGrades, m.OtherQualifications, m.PreviousClub,
		yes(m.CommitteeMailing), yes(m.InstructorMailing), yes(m.BoatHandlerMailing), yes(m.LifeguardMailing), yes(m.Driver), yes(m.ShareContactDetails),
	}
}

// phoneLike matches values such as "+44 20 7946 0000" or "-12.50" that start
// with a formula character but are plain numbers.
var phoneLike = regexp.MustCompile(`^[+-]?[0-9][0-9 ()./-]*$`)

// SafeCell neutralises spreadsheet formula injection: a cell starting with
// = + - @ TAB or CR is prefixed with a single quote, unless it is a plain
// number or phone number.
func SafeCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '@', '\t', '\r':
		return "'" + s
	case '+', '-':
		if phoneLike.MatchString(s) {
			return s
		}
		return "'" + s
	}
	return s
}

// WriteMembersCSV writes a UTF-8 CSV (with BOM, so Excel detects the
// encoding) of the given members.
func WriteMembersCSV(w io.Writer, ms []members.Member) error {
	if _, err := io.WriteString(w, "\ufeff"); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	cw.UseCRLF = true
	if err := cw.Write(Header); err != nil {
		return err
	}
	for i := range ms {
		rec := Record(&ms[i])
		for j := range rec {
			rec[j] = SafeCell(rec[j])
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// JoinEmails formats addresses for pasting into a mail client's To/Bcc field.
func JoinEmails(emails []string, sep string) string {
	return strings.Join(emails, sep)
}
