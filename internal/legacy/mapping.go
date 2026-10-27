// Package legacy imports members from the legacy PHP site's `membership`
// and `membershipclidivetype` tables.
//
// Deliberately NOT imported: the legacy login columns `email`, `surname`,
// `password` (stored in plain text), `passhint`, `admin`, `session` and
// `LastLogin`. They are never even selected from the legacy database.
package legacy

import (
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/clidive410/membership/internal/members"
)

// Row is one legacy `membership` row. Every column is read as text
// (CAST(... AS CHAR)) so that MariaDB zero dates and odd values survive and
// can be validated here.
type Row struct {
	MemberID                                             int64
	FirstName, LastName                                  sql.NullString
	DOB, Occupation                                      sql.NullString
	Address1, Address2, Address3, Town, Postcode         sql.NullString
	Telephone1, Telephone2, Telephone3                   sql.NullString
	Email1, Email2, Email1Use, Email2Use                 sql.NullString
	ICEName, ICERelation, ICETelephones, ICEAddresses    sql.NullString
	BSACMemberID, BSACRenewalDueDate, BSACAnnualFee      sql.NullString
	BSACAbated, ClidiveJoinDate, ClidiveMembershipTypeID sql.NullString
	ClidiveMembershipExpiry, Query, Notes                sql.NullString
	OtherDiverGrades, OtherInstructorGrades              sql.NullString
	OtherQualifications, PreviousDiveClub                sql.NullString
	BSACMemberIDIfKnown, Gender, BSACDirectDebit         sql.NullString
	InstructorMailing, CommitteeMailing, DeletedOn       sql.NullString
	BoatHandlerMailing, LifeguardMailing, Driver         sql.NullString
	LastPaid                                             sql.NullString
}

// selectColumns lists the legacy columns read, in Row field order. The
// login/password/session columns are intentionally absent.
var selectColumns = []string{"memberid", "firstname", "lastname", "DOB", "occupation",
	"address1", "address2", "address3", "Town", "Postcode", "telephone1", "telephone2", "telephone3",
	"email1", "email2", "email1use", "email2use", "ICEname", "ICErelation", "ICEtelephones", "ICEaddresses",
	"BSACMemberid", "BSACRenewalDueDate", "BSACAnnualFee", "BSACAbated", "ClidiveJoinDate", "ClidiveMembershipTypeId",
	"ClidiveMembershipExpiry", "Query", "Notes", "OtherDiverGrades", "OtherInstructorGrades",
	"OtherQualifications", "PreviousDiveClub", "BSACMemberidIfKnown", "Gender", "BSACDirectDebit",
	"InstructorMailing", "CommitteeMailing", "DeletedOn", "BoatHandlerMailing", "LifeguardMailing", "Driver",
	"last_paid"}

// excludedColumns are legacy columns that must never be read.
var excludedColumns = []string{"email", "surname", "password", "passhint", "admin", "session", "LastLogin"}

// SelectQuery returns the query used to read legacy rows.
func SelectQuery() string {
	cols := make([]string, len(selectColumns))
	for i, c := range selectColumns {
		if c == "memberid" {
			cols[i] = "`memberid`"
			continue
		}
		cols[i] = fmt.Sprintf("CAST(`%s` AS CHAR)", c)
	}
	return "SELECT " + strings.Join(cols, ", ") + " FROM `membership` ORDER BY `memberid`"
}

func (r *Row) scanTargets() []any {
	return []any{&r.MemberID, &r.FirstName, &r.LastName, &r.DOB, &r.Occupation,
		&r.Address1, &r.Address2, &r.Address3, &r.Town, &r.Postcode, &r.Telephone1, &r.Telephone2, &r.Telephone3,
		&r.Email1, &r.Email2, &r.Email1Use, &r.Email2Use, &r.ICEName, &r.ICERelation, &r.ICETelephones, &r.ICEAddresses,
		&r.BSACMemberID, &r.BSACRenewalDueDate, &r.BSACAnnualFee, &r.BSACAbated, &r.ClidiveJoinDate, &r.ClidiveMembershipTypeID,
		&r.ClidiveMembershipExpiry, &r.Query, &r.Notes, &r.OtherDiverGrades, &r.OtherInstructorGrades,
		&r.OtherQualifications, &r.PreviousDiveClub, &r.BSACMemberIDIfKnown, &r.Gender, &r.BSACDirectDebit,
		&r.InstructorMailing, &r.CommitteeMailing, &r.DeletedOn, &r.BoatHandlerMailing, &r.LifeguardMailing, &r.Driver,
		&r.LastPaid}
}

// Warning describes a value that could not be mapped. It never contains the
// value itself (personal data), only the member id and column.
type Warning struct {
	MemberID int64
	Column   string
	Problem  string
}

func (w Warning) String() string {
	return fmt.Sprintf("member %d: %s: %s", w.MemberID, w.Column, w.Problem)
}

// Mapped is the result of mapping one legacy row.
type Mapped struct {
	Member   members.Member
	LastPaid sql.NullTime // becomes one payment row with source "legacy"
	Warnings []Warning
}

type mapper struct {
	id       int64
	warnings []Warning
}

func (m *mapper) warn(col, problem string) {
	m.warnings = append(m.warnings, Warning{MemberID: m.id, Column: col, Problem: problem})
}

// str returns a trimmed legacy text value with any double encoding repaired
// (see RepairMojibake). Every text column goes through here.
func str(s sql.NullString) string { return strings.TrimSpace(RepairMojibake(s.String)) }

func flag(s sql.NullString) bool {
	n, err := strconv.Atoi(str(s))
	return err == nil && n != 0
}

var (
	isoDate   = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})(?:[ T](\d{2}):(\d{2}):(\d{2}))?`)
	shortDate = regexp.MustCompile(`^(\d{1,2})[./](\d{1,2})[./](\d{2}|\d{4})\D?$`)
)

// date parses legacy DATE/DATETIME text. Zero dates ("0000-00-00") and years
// before 1900 are the legacy "not set" and map to NULL silently; other
// unparseable values map to NULL with a warning.
func (m *mapper) date(col string, s sql.NullString, keepTime bool) sql.NullTime {
	v := str(s)
	if v == "" {
		return sql.NullTime{}
	}
	p := isoDate.FindStringSubmatch(v)
	if p == nil {
		m.warn(col, "unrecognised date format")
		return sql.NullTime{}
	}
	y, _ := strconv.Atoi(p[1])
	mo, _ := strconv.Atoi(p[2])
	d, _ := strconv.Atoi(p[3])
	if y < 1900 {
		return sql.NullTime{}
	}
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Year() != y || int(t.Month()) != mo || t.Day() != d {
		m.warn(col, "invalid calendar date")
		return sql.NullTime{}
	}
	if keepTime && p[4] != "" {
		hh, _ := strconv.Atoi(p[4])
		mm, _ := strconv.Atoi(p[5])
		ss, _ := strconv.Atoi(p[6])
		t = t.Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute + time.Duration(ss)*time.Second)
	}
	return sql.NullTime{Time: t, Valid: true}
}

// joinDate parses the legacy free-text ClidiveJoinDate (VARCHAR with default
// 'getdate()'): ISO "Y-M-D" with or without zero padding, or UK "DD/MM/YY",
// "DD.MM.YY". Anything else is kept verbatim in JoinDateRaw.
func (m *mapper) joinDate(s sql.NullString, now time.Time) (sql.NullTime, string) {
	v := str(s)
	if v == "" || strings.EqualFold(v, "getdate()") {
		return sql.NullTime{}, ""
	}
	if isoDate.MatchString(v) {
		t := m.date("ClidiveJoinDate", s, false)
		if !t.Valid {
			return t, v
		}
		return t, ""
	}
	if p := shortDate.FindStringSubmatch(v); p != nil {
		d, _ := strconv.Atoi(p[1])
		mo, _ := strconv.Atoi(p[2])
		y, _ := strconv.Atoi(p[3])
		if len(p[3]) == 2 {
			// Two-digit years: pivot on the current year.
			if y > now.Year()%100 {
				y += 1900
			} else {
				y += 2000
			}
		}
		t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
		if t.Year() == y && int(t.Month()) == mo && t.Day() == d {
			return sql.NullTime{Time: t, Valid: true}, ""
		}
	}
	m.warn("ClidiveJoinDate", "unparseable join date kept as text")
	return sql.NullTime{}, v
}

// Map converts a legacy row. knownTypes holds the valid membership type ids.
func Map(r Row, knownTypes map[int64]bool, now time.Time) Mapped {
	mp := &mapper{id: r.MemberID}
	m := members.Member{
		ID:                  r.MemberID,
		LegacyID:            sql.NullInt64{Int64: r.MemberID, Valid: true},
		FirstName:           str(r.FirstName),
		LastName:            str(r.LastName),
		DateOfBirth:         mp.date("DOB", r.DOB, false),
		Occupation:          str(r.Occupation),
		Address1:            str(r.Address1),
		Address2:            str(r.Address2),
		Address3:            str(r.Address3),
		Town:                str(r.Town),
		Postcode:            str(r.Postcode),
		Phone1:              str(r.Telephone1),
		Phone2:              str(r.Telephone2),
		Phone3:              str(r.Telephone3),
		Email1:              str(r.Email1),
		Email2:              str(r.Email2),
		Email1Use:           flag(r.Email1Use),
		Email2Use:           flag(r.Email2Use),
		ICEName:             str(r.ICEName),
		ICERelation:         str(r.ICERelation),
		ICEPhones:           str(r.ICETelephones),
		ICEAddress:          str(r.ICEAddresses),
		BSACMemberID:        str(r.BSACMemberID),
		BSACMemberIDIfKnown: str(r.BSACMemberIDIfKnown),
		BSACRenewalDue:      mp.date("BSACRenewalDueDate", r.BSACRenewalDueDate, false),
		BSACAbated:          flag(r.BSACAbated),
		BSACDirectDebit:     flag(r.BSACDirectDebit),
		MembershipExpiry:    mp.date("ClidiveMembershipExpiry", r.ClidiveMembershipExpiry, false),
		Query:               str(r.Query),
		Notes:               str(r.Notes),
		DiverGrades:         str(r.OtherDiverGrades),
		InstructorGrades:    str(r.OtherInstructorGrades),
		OtherQualifications: str(r.OtherQualifications),
		PreviousClub:        str(r.PreviousDiveClub),
		CommitteeMailing:    flag(r.CommitteeMailing),
		InstructorMailing:   flag(r.InstructorMailing),
		BoatHandlerMailing:  flag(r.BoatHandlerMailing),
		LifeguardMailing:    flag(r.LifeguardMailing),
		Driver:              flag(r.Driver),
		DeletedAt:           mp.date("DeletedOn", r.DeletedOn, true),
	}
	if r.DeletedOn.Valid && str(r.DeletedOn) != "" && !m.DeletedAt.Valid {
		// Archived with an unusable timestamp: keep it archived.
		m.DeletedAt = sql.NullTime{Time: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	}
	switch str(r.Gender) {
	case "0":
		m.Gender = "M"
	case "1":
		m.Gender = "F"
	}
	if fee := str(r.BSACAnnualFee); fee != "" {
		money, err := members.ParseMoney(fee)
		if err != nil {
			mp.warn("BSACAnnualFee", "not a number")
		}
		m.BSACAnnualFee = money
	}
	if t := str(r.ClidiveMembershipTypeID); t != "" {
		id, err := strconv.ParseInt(t, 10, 64)
		if err != nil || !knownTypes[id] {
			mp.warn("ClidiveMembershipTypeId", "unknown membership type, left empty")
		} else {
			m.TypeID = sql.NullInt64{Int64: id, Valid: true}
		}
	}
	m.JoinDate, m.JoinDateRaw = mp.joinDate(r.ClidiveJoinDate, now)
	if m.Email1Use && m.Email1 == "" {
		m.Email1Use = false
	}
	if m.Email2Use && m.Email2 == "" {
		m.Email2Use = false
	}
	m.Normalize()
	return Mapped{Member: m, LastPaid: mp.date("last_paid", r.LastPaid, false), Warnings: mp.warnings}
}
