package members

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Store is the MariaDB-backed member repository.
type Store struct {
	DB *sql.DB
}

// NewStore returns a Store using db.
func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

// Status filters the member list.
type Status string

// Member list statuses. "current" mirrors the legacy default list: every
// non-archived member, including those with the Lapsed type.
const (
	StatusCurrent   Status = "current"
	StatusNotLapsed Status = "notlapsed" // current, excluding the Lapsed type
	StatusExpired   Status = "expired"   // current, Clidive membership expiry before today (see Member.Expired)
	StatusArchived  Status = "archived"
	StatusAll       Status = "all"
)

// ParseStatus returns a valid Status, defaulting to StatusCurrent.
func ParseStatus(s string) Status {
	switch Status(s) {
	case StatusNotLapsed, StatusExpired, StatusArchived, StatusAll:
		return Status(s)
	}
	return StatusCurrent
}

// SortColumns maps sort keys accepted from the UI to SQL expressions. Only
// keys in this map can ever reach the ORDER BY clause.
var SortColumns = map[string]string{
	"id":         "m.id",
	"first_name": "m.first_name",
	"last_name":  "m.last_name",
	"email":      "m.email1",
	"bsac":       "m.bsac_renewal_due",
	"type":       "mt.sort_order",
	"expiry":     "m.membership_expiry",
	"last_paid":  "lp.last_paid",
	"notes":      "m.notes",
	"archived":   "m.deleted_at",
}

// Filter selects and orders members for listing and export.
type Filter struct {
	Query  string // free text over names, emails, BSAC number, postcode
	TypeID int64  // 0 = any; -1 = no type set
	Status Status
	Today  time.Time // reference day for StatusExpired; zero means time.Now()
	Sort   string    // key of SortColumns, default last_name
	Desc   bool
}

const memberColumns = `m.id, m.legacy_member_id, m.first_name, m.last_name, m.gender, m.date_of_birth,
	m.occupation, m.address1, m.address2, m.address3, m.town, m.postcode, m.phone1, m.phone2, m.phone3,
	m.email1, m.email2, m.email1_use, m.email2_use, m.ice_name, m.ice_relation, m.ice_phones, m.ice_address,
	m.bsac_member_id, m.bsac_member_id_if_known, m.bsac_renewal_due, m.bsac_annual_fee, m.bsac_abated,
	m.bsac_direct_debit, m.join_date, m.join_date_raw, m.membership_type_id, COALESCE(mt.name, ''),
	m.membership_expiry, m.query_text, m.notes, m.diver_grades, m.instructor_grades, m.other_qualifications,
	m.previous_club, m.committee_mailing, m.instructor_mailing, m.boat_handler_mailing, m.lifeguard_mailing,
	m.driver, m.share_contact_details, lp.last_paid, m.created_at, m.updated_at, m.deleted_at`

const memberFrom = ` FROM members m
	LEFT JOIN membership_types mt ON mt.id = m.membership_type_id
	LEFT JOIN (SELECT member_id, MAX(paid_on) AS last_paid FROM payments GROUP BY member_id) lp ON lp.member_id = m.id`

type scanner interface{ Scan(...any) error }

func scanMember(s scanner) (*Member, error) {
	var m Member
	err := s.Scan(&m.ID, &m.LegacyID, &m.FirstName, &m.LastName, &m.Gender, &m.DateOfBirth,
		&m.Occupation, &m.Address1, &m.Address2, &m.Address3, &m.Town, &m.Postcode, &m.Phone1, &m.Phone2, &m.Phone3,
		&m.Email1, &m.Email2, &m.Email1Use, &m.Email2Use, &m.ICEName, &m.ICERelation, &m.ICEPhones, &m.ICEAddress,
		&m.BSACMemberID, &m.BSACMemberIDIfKnown, &m.BSACRenewalDue, &m.BSACAnnualFee, &m.BSACAbated,
		&m.BSACDirectDebit, &m.JoinDate, &m.JoinDateRaw, &m.TypeID, &m.TypeName,
		&m.MembershipExpiry, &m.Query, &m.Notes, &m.DiverGrades, &m.InstructorGrades, &m.OtherQualifications,
		&m.PreviousClub, &m.CommitteeMailing, &m.InstructorMailing, &m.BoatHandlerMailing, &m.LifeguardMailing,
		&m.Driver, &m.ShareContactDetails, &m.LastPaid, &m.CreatedAt, &m.UpdatedAt, &m.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListTypes returns all membership types in display order.
func (s *Store) ListTypes(ctx context.Context) ([]MembershipType, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT id, name FROM membership_types ORDER BY sort_order, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MembershipType
	for rows.Next() {
		var t MembershipType
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ListMembers returns the members matching f.
func (s *Store) ListMembers(ctx context.Context, f Filter) ([]Member, error) {
	var where []string
	var args []any
	switch f.Status {
	case StatusNotLapsed:
		where = append(where, "m.deleted_at IS NULL", fmt.Sprintf("(m.membership_type_id IS NULL OR m.membership_type_id <> %d)", TypeLapsed))
	case StatusExpired:
		today := f.Today
		if today.IsZero() {
			today = time.Now()
		}
		where = append(where, "m.deleted_at IS NULL", "m.membership_expiry < ?")
		args = append(args, today.Format("2006-01-02"))
	case StatusArchived:
		where = append(where, "m.deleted_at IS NOT NULL")
	case StatusAll:
	default:
		where = append(where, "m.deleted_at IS NULL")
	}
	switch {
	case f.TypeID > 0:
		where = append(where, "m.membership_type_id = ?")
		args = append(args, f.TypeID)
	case f.TypeID < 0:
		where = append(where, "m.membership_type_id IS NULL")
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		for _, word := range strings.Fields(q) {
			where = append(where, `CONCAT_WS(' ', m.first_name, m.last_name, m.email1, m.email2, m.bsac_member_id, m.postcode, CAST(m.id AS CHAR)) LIKE ?`)
			args = append(args, "%"+escapeLike(word)+"%")
		}
	}
	q := "SELECT " + memberColumns + memberFrom
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	col, ok := SortColumns[f.Sort]
	if !ok {
		col = SortColumns["last_name"]
	}
	dir := "ASC"
	if f.Desc {
		dir = "DESC"
	}
	q += fmt.Sprintf(" ORDER BY %s %s, m.last_name, m.first_name, m.id", col, dir)

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// GetMember returns one member (archived or not).
func (s *Store) GetMember(ctx context.Context, id int64) (*Member, error) {
	m, err := scanMember(s.DB.QueryRowContext(ctx, "SELECT "+memberColumns+memberFrom+" WHERE m.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// writableColumns are the columns written by Create/Update, in the order of
// writableValues.
const writableColumns = `first_name, last_name, gender, date_of_birth, occupation, address1, address2, address3,
	town, postcode, phone1, phone2, phone3, email1, email2, email1_use, email2_use, ice_name, ice_relation,
	ice_phones, ice_address, bsac_member_id, bsac_member_id_if_known, bsac_renewal_due, bsac_annual_fee,
	bsac_abated, bsac_direct_debit, join_date, join_date_raw, membership_type_id, membership_expiry, query_text,
	notes, diver_grades, instructor_grades, other_qualifications, previous_club, committee_mailing,
	instructor_mailing, boat_handler_mailing, lifeguard_mailing, driver, share_contact_details`

func writableValues(m *Member) []any {
	return []any{m.FirstName, m.LastName, m.Gender, m.DateOfBirth, m.Occupation, m.Address1, m.Address2, m.Address3,
		m.Town, m.Postcode, m.Phone1, m.Phone2, m.Phone3, m.Email1, m.Email2, m.Email1Use, m.Email2Use, m.ICEName, m.ICERelation,
		m.ICEPhones, m.ICEAddress, m.BSACMemberID, m.BSACMemberIDIfKnown, m.BSACRenewalDue, m.BSACAnnualFee,
		m.BSACAbated, m.BSACDirectDebit, m.JoinDate, m.JoinDateRaw, m.TypeID, m.MembershipExpiry, m.Query,
		m.Notes, m.DiverGrades, m.InstructorGrades, m.OtherQualifications, m.PreviousClub, m.CommitteeMailing,
		m.InstructorMailing, m.BoatHandlerMailing, m.LifeguardMailing, m.Driver, m.ShareContactDetails}
}

func columnList() []string {
	var cols []string
	for _, c := range strings.Split(writableColumns, ",") {
		cols = append(cols, strings.TrimSpace(c))
	}
	return cols
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func updateSet() string {
	cols := columnList()
	for i, c := range cols {
		cols[i] = c + " = ?"
	}
	return strings.Join(cols, ", ")
}

// execer is satisfied by *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Actor identifies who performs a mutation, for the audit log.
type Actor struct {
	UserID int64  // 0 for system actors
	Label  string // user email, or e.g. "migrate-legacy"
}

// Audit actions.
const (
	ActionMemberCreate  = "member.create"
	ActionMemberUpdate  = "member.update"
	ActionMemberArchive = "member.archive"
	ActionMemberRestore = "member.restore"
	ActionPaymentAdd    = "payment.add"
	ActionPaymentDelete = "payment.delete"
	ActionLegacyImport  = "legacy.import"
)

// WriteAudit appends one audit_log row using db (normally the mutation's
// transaction).
func WriteAudit(ctx context.Context, db execer, a Actor, action string, memberID int64, entity string, entityID int64, changes any) error {
	var js sql.NullString
	if changes != nil {
		b, err := json.Marshal(changes)
		if err != nil {
			return err
		}
		js = sql.NullString{String: string(b), Valid: true}
	}
	_, err := db.ExecContext(ctx, `INSERT INTO audit_log (actor_user_id, actor, action, member_id, entity, entity_id, changes)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, nullID(a.UserID), a.Label, action, nullID(memberID), entity, nullID(entityID), js)
	return err
}

func nullID(id int64) sql.NullInt64 { return sql.NullInt64{Int64: id, Valid: id != 0} }

// inTx runs fn in a transaction.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrDuplicateEmail is returned when email1 already identifies another
// non-archived member.
var ErrDuplicateEmail = errors.New("another current member already uses this primary email")

func checkEmailFree(ctx context.Context, db execer, email string, exceptID int64) error {
	if email == "" {
		return nil
	}
	var id int64
	err := db.QueryRowContext(ctx, "SELECT id FROM members WHERE email1 = ? AND deleted_at IS NULL AND id <> ? LIMIT 1", email, exceptID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	return ErrDuplicateEmail
}

// CreateMember inserts m and sets m.ID.
func (s *Store) CreateMember(ctx context.Context, a Actor, m *Member) (int64, error) {
	m.Normalize()
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := checkEmailFree(ctx, tx, m.Email1, 0); err != nil {
			return err
		}
		if _, err := InsertMember(ctx, tx, m); err != nil {
			return err
		}
		return WriteAudit(ctx, tx, a, ActionMemberCreate, m.ID, "member", m.ID, Diff(nil, m))
	})
	return m.ID, err
}

// InsertMember inserts m without auditing (used inside callers'
// transactions). If m.ID is non-zero it is used as the primary key.
func InsertMember(ctx context.Context, db execer, m *Member) (int64, error) {
	n := len(columnList())
	cols := "legacy_member_id, " + writableColumns
	vals := append([]any{m.LegacyID}, writableValues(m)...)
	if m.ID != 0 {
		cols = "id, " + cols
		vals = append([]any{m.ID}, vals...)
		n++
	}
	if m.DeletedAt.Valid {
		cols += ", deleted_at"
		vals = append(vals, m.DeletedAt)
		n++
	}
	res, err := db.ExecContext(ctx, "INSERT INTO members ("+cols+") VALUES ("+placeholders(n+1)+")", vals...)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	m.ID = id
	return id, nil
}

// UpdateMemberRow writes all editable fields of m without auditing.
func UpdateMemberRow(ctx context.Context, db execer, m *Member) error {
	_, err := db.ExecContext(ctx, "UPDATE members SET "+updateSet()+" WHERE id = ?", append(writableValues(m), m.ID)...)
	return err
}

func getMemberTx(ctx context.Context, tx *sql.Tx, id int64) (*Member, error) {
	m, err := scanMember(tx.QueryRowContext(ctx, "SELECT "+memberColumns+memberFrom+" WHERE m.id = ? FOR UPDATE", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// UpdateMember saves all editable fields of m (it does not change the
// archived state). Changed fields are written to the audit log, and a linked
// login identity follows a change of the primary email.
func (s *Store) UpdateMember(ctx context.Context, a Actor, m *Member) error {
	m.Normalize()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		old, err := getMemberTx(ctx, tx, m.ID)
		if err != nil {
			return err
		}
		if !old.DeletedAt.Valid && old.Email1 != m.Email1 {
			if err := checkEmailFree(ctx, tx, m.Email1, m.ID); err != nil {
				return err
			}
		}
		m.DeletedAt = old.DeletedAt
		changes := Diff(old, m)
		if len(changes) == 0 {
			return nil
		}
		if err := UpdateMemberRow(ctx, tx, m); err != nil {
			return err
		}
		if _, ok := changes["email1"]; ok {
			if _, err := tx.ExecContext(ctx, "UPDATE users SET email = ? WHERE member_id = ?", m.Email1, m.ID); err != nil {
				return fmt.Errorf("updating login email: %w", err)
			}
		}
		return WriteAudit(ctx, tx, a, ActionMemberUpdate, m.ID, "member", m.ID, changes)
	})
}

// ArchiveMember soft-deletes a member (legacy "archive"/delete).
func (s *Store) ArchiveMember(ctx context.Context, a Actor, id int64) error {
	return s.setArchived(ctx, a, id, true)
}

// RestoreMember undoes ArchiveMember.
func (s *Store) RestoreMember(ctx context.Context, a Actor, id int64) error {
	return s.setArchived(ctx, a, id, false)
}

func (s *Store) setArchived(ctx context.Context, a Actor, id int64, archive bool) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		m, err := getMemberTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if m.DeletedAt.Valid == archive {
			return nil // already in the requested state
		}
		q, action := "UPDATE members SET deleted_at = UTC_TIMESTAMP() WHERE id = ?", ActionMemberArchive
		if !archive {
			if err := checkEmailFree(ctx, tx, m.Email1, id); err != nil {
				return err
			}
			q, action = "UPDATE members SET deleted_at = NULL WHERE id = ?", ActionMemberRestore
		}
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
		return WriteAudit(ctx, tx, a, action, id, "member", id, nil)
	})
}

// ListPayments returns a member's payments, most recent first.
func (s *Store) ListPayments(ctx context.Context, memberID int64) ([]Payment, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, member_id, paid_on, amount, method, note, source, created_by, created_at
		FROM payments WHERE member_id = ? ORDER BY paid_on DESC, id DESC`, memberID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		var p Payment
		if err := rows.Scan(&p.ID, &p.MemberID, &p.PaidOn, &p.Amount, &p.Method, &p.Note, &p.Source, &p.CreatedBy, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func paymentChanges(p *Payment) map[string]string {
	return map[string]string{"paid_on": p.PaidOn.Format(DateLayout), "amount": p.Amount.String(), "method": p.Method, "note": p.Note, "source": p.Source}
}

// AddPayment records a payment for one member.
func (s *Store) AddPayment(ctx context.Context, a Actor, p *Payment) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := getMemberTx(ctx, tx, p.MemberID); err != nil {
			return err
		}
		p.CreatedBy = a.Label
		if err := InsertPayment(ctx, tx, p); err != nil {
			return err
		}
		return WriteAudit(ctx, tx, a, ActionPaymentAdd, p.MemberID, "payment", p.ID, paymentChanges(p))
	})
}

// InsertPayment inserts p without auditing.
func InsertPayment(ctx context.Context, db execer, p *Payment) error {
	if p.Source == "" {
		p.Source = SourceManual
	}
	res, err := db.ExecContext(ctx, `INSERT INTO payments (member_id, paid_on, amount, method, note, source, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, p.MemberID, p.PaidOn.Format(DateLayout), p.Amount, p.Method, p.Note, p.Source, p.CreatedBy)
	if err != nil {
		return err
	}
	p.ID, err = res.LastInsertId()
	return err
}

// DeletePayment removes a payment recorded in error.
func (s *Store) DeletePayment(ctx context.Context, a Actor, memberID, paymentID int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var p Payment
		err := tx.QueryRowContext(ctx, "SELECT id, member_id, paid_on, amount, method, note, source FROM payments WHERE id = ? AND member_id = ? FOR UPDATE",
			paymentID, memberID).Scan(&p.ID, &p.MemberID, &p.PaidOn, &p.Amount, &p.Method, &p.Note, &p.Source)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM payments WHERE id = ?", paymentID); err != nil {
			return err
		}
		return WriteAudit(ctx, tx, a, ActionPaymentDelete, memberID, "payment", paymentID, paymentChanges(&p))
	})
}

// BulkPayment describes a payment recorded for many members at once
// (legacy paymentmanagement.php).
type BulkPayment struct {
	PaidOn    time.Time
	Amount    Money
	Method    string
	Note      string
	NewExpiry sql.NullTime // when set, also renew Clidive membership to this date
}

// RecordBulkPayment records bp for every non-archived member in ids, in one
// transaction. It returns the number of members updated.
func (s *Store) RecordBulkPayment(ctx context.Context, a Actor, ids []int64, bp BulkPayment) (int, error) {
	n := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			m, err := getMemberTx(ctx, tx, id)
			if errors.Is(err, ErrNotFound) {
				continue
			} else if err != nil {
				return err
			}
			if m.Archived() {
				continue
			}
			p := &Payment{MemberID: id, PaidOn: bp.PaidOn, Amount: bp.Amount, Method: bp.Method, Note: bp.Note, Source: SourceBulk, CreatedBy: a.Label}
			if err := InsertPayment(ctx, tx, p); err != nil {
				return err
			}
			if err := WriteAudit(ctx, tx, a, ActionPaymentAdd, id, "payment", p.ID, paymentChanges(p)); err != nil {
				return err
			}
			if bp.NewExpiry.Valid && FormatDate(bp.NewExpiry) != FormatDate(m.MembershipExpiry) {
				if _, err := tx.ExecContext(ctx, "UPDATE members SET membership_expiry = ? WHERE id = ?", bp.NewExpiry, id); err != nil {
					return err
				}
				ch := Changes{"membership_expiry": {FormatDate(m.MembershipExpiry), FormatDate(bp.NewExpiry)}}
				if err := WriteAudit(ctx, tx, a, ActionMemberUpdate, id, "member", id, ch); err != nil {
					return err
				}
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// AuditEntry is one row of the audit log.
type AuditEntry struct {
	At      time.Time
	Actor   string
	Action  string
	Entity  string
	Changes string // raw JSON
}

// ListAudit returns the most recent audit entries for a member.
func (s *Store) ListAudit(ctx context.Context, memberID int64, limit int) ([]AuditEntry, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT at, actor, action, entity, COALESCE(changes, '')
		FROM audit_log WHERE member_id = ? ORDER BY at DESC, id DESC LIMIT ?`, memberID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.At, &e.Actor, &e.Action, &e.Entity, &e.Changes); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MailingList identifies one of the email distribution lists.
type MailingList string

// Mailing lists. "all" replicates the legacy `email-all-auto` view.
const (
	ListAll         MailingList = "all"
	ListCommittee   MailingList = "committee"
	ListInstructor  MailingList = "instructor"
	ListBoatHandler MailingList = "boathandler"
	ListLifeguard   MailingList = "lifeguard"
	ListDriver      MailingList = "driver"
)

// MailingLists describes the lists in UI order. Column is the flag column
// ("" for the all-members list).
var MailingLists = []struct {
	Key    MailingList
	Label  string
	Column string
}{
	{ListAll, "All members", ""},
	{ListCommittee, "Committee", "committee_mailing"},
	{ListInstructor, "Instructors", "instructor_mailing"},
	{ListBoatHandler, "Boat handlers", "boat_handler_mailing"},
	{ListLifeguard, "Lifeguards", "lifeguard_mailing"},
	{ListDriver, "Drivers", "driver"},
}

// MailingListColumn returns the flag column for list and whether list is known.
func MailingListColumn(list MailingList) (string, bool) {
	for _, l := range MailingLists {
		if l.Key == list {
			return l.Column, true
		}
	}
	return "", false
}

// EmailList returns the distinct, trimmed, sorted addresses on a mailing
// list: like the legacy view, emailN is used only when emailNuse is set, the
// address is longer than 5 characters, and the member is not archived.
func (s *Store) EmailList(ctx context.Context, list MailingList, excludeLapsed bool) ([]string, error) {
	col, ok := MailingListColumn(list)
	if !ok {
		return nil, fmt.Errorf("unknown mailing list %q", list)
	}
	cond := "m.deleted_at IS NULL"
	if col != "" {
		cond += " AND m." + col // col comes from the fixed MailingLists table
	}
	if excludeLapsed {
		cond += fmt.Sprintf(" AND (m.membership_type_id IS NULL OR m.membership_type_id <> %d)", TypeLapsed)
	}
	q := fmt.Sprintf(`SELECT e FROM (
		SELECT TRIM(m.email1) AS e FROM members m WHERE m.email1_use AND CHAR_LENGTH(TRIM(m.email1)) > 5 AND %[1]s
		UNION
		SELECT TRIM(m.email2) AS e FROM members m WHERE m.email2_use AND CHAR_LENGTH(TRIM(m.email2)) > 5 AND %[1]s
	) x ORDER BY e`, cond)
	rows, err := s.DB.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Ping checks database connectivity (health endpoint).
func (s *Store) Ping(ctx context.Context) error { return s.DB.PingContext(ctx) }
