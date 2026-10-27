package members

import (
	"strconv"
	"strings"
)

// FieldGroup partitions member fields by who may edit them.
type FieldGroup string

// Field groups. Authorization (internal/auth) maps roles to the groups they
// may edit; e.g. future member self-service may edit GroupPersonal only.
const (
	// GroupPersonal: name, contact details, address, emails and emergency contact.
	GroupPersonal FieldGroup = "personal"
	// GroupClub: mailing-list flags, driver, diving qualifications.
	GroupClub FieldGroup = "club"
	// GroupMembership: Clidive type/expiry/join date, BSAC details, committee notes.
	GroupMembership FieldGroup = "membership"
)

// AllGroups lists every field group.
var AllGroups = []FieldGroup{GroupPersonal, GroupClub, GroupMembership}

// NormalizeEmail lower-cases and trims an address. The primary email is the
// member's identity, so it is always stored normalised.
func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Normalize trims text fields and normalises emails in place.
func (m *Member) Normalize() {
	for _, p := range []*string{&m.FirstName, &m.LastName, &m.Occupation, &m.Address1, &m.Address2, &m.Address3,
		&m.Town, &m.Phone1, &m.Phone2, &m.Phone3, &m.ICEName, &m.ICERelation, &m.ICEPhones, &m.ICEAddress,
		&m.BSACMemberID, &m.BSACMemberIDIfKnown, &m.JoinDateRaw, &m.Query, &m.Notes, &m.DiverGrades,
		&m.InstructorGrades, &m.OtherQualifications, &m.PreviousClub} {
		*p = strings.TrimSpace(*p)
	}
	m.Postcode = strings.ToUpper(strings.TrimSpace(m.Postcode))
	m.Email1 = NormalizeEmail(m.Email1)
	m.Email2 = NormalizeEmail(m.Email2)
}

// Merge copies the fields of the given groups from src into dst. Fields of
// other groups in dst are left untouched, so a handler can parse a full form
// and apply only what the current user may change.
func Merge(dst, src *Member, groups ...FieldGroup) {
	for _, g := range groups {
		switch g {
		case GroupPersonal:
			dst.FirstName, dst.LastName, dst.Gender = src.FirstName, src.LastName, src.Gender
			dst.DateOfBirth, dst.Occupation = src.DateOfBirth, src.Occupation
			dst.Address1, dst.Address2, dst.Address3 = src.Address1, src.Address2, src.Address3
			dst.Town, dst.Postcode = src.Town, src.Postcode
			dst.Phone1, dst.Phone2, dst.Phone3 = src.Phone1, src.Phone2, src.Phone3
			dst.Email1, dst.Email2, dst.Email1Use, dst.Email2Use = src.Email1, src.Email2, src.Email1Use, src.Email2Use
			dst.ICEName, dst.ICERelation, dst.ICEPhones, dst.ICEAddress = src.ICEName, src.ICERelation, src.ICEPhones, src.ICEAddress
			dst.ShareContactDetails = src.ShareContactDetails
		case GroupClub:
			dst.CommitteeMailing, dst.InstructorMailing = src.CommitteeMailing, src.InstructorMailing
			dst.BoatHandlerMailing, dst.LifeguardMailing, dst.Driver = src.BoatHandlerMailing, src.LifeguardMailing, src.Driver
			dst.DiverGrades, dst.InstructorGrades = src.DiverGrades, src.InstructorGrades
			dst.OtherQualifications, dst.PreviousClub = src.OtherQualifications, src.PreviousClub
		case GroupMembership:
			dst.BSACMemberID, dst.BSACMemberIDIfKnown = src.BSACMemberID, src.BSACMemberIDIfKnown
			dst.BSACRenewalDue, dst.BSACAnnualFee = src.BSACRenewalDue, src.BSACAnnualFee
			dst.BSACAbated, dst.BSACDirectDebit = src.BSACAbated, src.BSACDirectDebit
			dst.JoinDate, dst.JoinDateRaw = src.JoinDate, src.JoinDateRaw
			dst.TypeID, dst.MembershipExpiry = src.TypeID, src.MembershipExpiry
			dst.Query, dst.Notes = src.Query, src.Notes
		}
	}
}

type fieldValue struct {
	name  string
	value string
}

func b(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func fieldValues(m *Member) []fieldValue {
	typeID := ""
	if m.TypeID.Valid {
		typeID = strconv.FormatInt(m.TypeID.Int64, 10)
	}
	return []fieldValue{
		{"first_name", m.FirstName}, {"last_name", m.LastName}, {"gender", m.Gender},
		{"date_of_birth", FormatDate(m.DateOfBirth)}, {"occupation", m.Occupation},
		{"address1", m.Address1}, {"address2", m.Address2}, {"address3", m.Address3},
		{"town", m.Town}, {"postcode", m.Postcode},
		{"phone1", m.Phone1}, {"phone2", m.Phone2}, {"phone3", m.Phone3},
		{"email1", m.Email1}, {"email2", m.Email2}, {"email1_use", b(m.Email1Use)}, {"email2_use", b(m.Email2Use)},
		{"ice_name", m.ICEName}, {"ice_relation", m.ICERelation}, {"ice_phones", m.ICEPhones}, {"ice_address", m.ICEAddress},
		{"bsac_member_id", m.BSACMemberID}, {"bsac_member_id_if_known", m.BSACMemberIDIfKnown},
		{"bsac_renewal_due", FormatDate(m.BSACRenewalDue)}, {"bsac_annual_fee", m.BSACAnnualFee.String()},
		{"bsac_abated", b(m.BSACAbated)}, {"bsac_direct_debit", b(m.BSACDirectDebit)},
		{"join_date", FormatDate(m.JoinDate)}, {"join_date_raw", m.JoinDateRaw},
		{"membership_type_id", typeID}, {"membership_expiry", FormatDate(m.MembershipExpiry)},
		{"query_text", m.Query}, {"notes", m.Notes},
		{"diver_grades", m.DiverGrades}, {"instructor_grades", m.InstructorGrades},
		{"other_qualifications", m.OtherQualifications}, {"previous_club", m.PreviousClub},
		{"committee_mailing", b(m.CommitteeMailing)}, {"instructor_mailing", b(m.InstructorMailing)},
		{"boat_handler_mailing", b(m.BoatHandlerMailing)}, {"lifeguard_mailing", b(m.LifeguardMailing)},
		{"driver", b(m.Driver)}, {"share_contact_details", b(m.ShareContactDetails)}, {"deleted_at", FormatDate(m.DeletedAt)},
	}
}

// Changes maps a column name to its [old, new] values.
type Changes map[string][2]string

// Diff returns the fields that differ between old and new. A nil old means
// creation (every non-empty field of new is reported).
func Diff(old, new *Member) Changes {
	nv := fieldValues(new)
	out := Changes{}
	if old == nil {
		for _, f := range nv {
			if f.value != "" && f.value != "0" {
				out[f.name] = [2]string{"", f.value}
			}
		}
		return out
	}
	ov := fieldValues(old)
	for i := range nv {
		if ov[i].value != nv[i].value {
			out[nv[i].name] = [2]string{ov[i].value, nv[i].value}
		}
	}
	return out
}
