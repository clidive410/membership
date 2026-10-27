package legacy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/clidive410/membership/internal/members"
)

// Actor is the audit-log actor used for imports.
var Actor = members.Actor{Label: "migrate-legacy"}

// legacyPaymentNote marks the payment row created from legacy last_paid.
const legacyPaymentNote = "Imported from legacy last_paid"

// Result summarises an import run. It contains counts and member ids only.
type Result struct {
	Types           int
	Read            int
	Created         int
	Updated         int
	Unchanged       int
	PaymentsCreated int
	PaymentsUpdated int
	PaymentsRemoved int
	Warnings        []Warning
	DuplicateEmails int // non-archived members sharing a primary email
}

// OpenLegacy opens the legacy database read-only in spirit: the importer
// only ever runs SELECTs against it.
func OpenLegacy(ctx context.Context, dsn string) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing legacy DSN: %w", err)
	}
	cfg.ParseTime = false // dates are read as text, see Row
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["charset"] = "utf8mb4" // the server converts from latin1
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to legacy database: %w", err)
	}
	return db, nil
}

// Importer copies legacy data into the application database.
type Importer struct {
	Legacy *sql.DB
	App    *sql.DB
	Log    *slog.Logger
	Now    func() time.Time
	DryRun bool // run everything in a transaction and roll back
}

// Run performs the import. It is idempotent: members are matched on
// legacy_member_id and updated in place; the legacy last_paid becomes (and
// stays in sync with) exactly one payment row with source "legacy". All
// changes happen in a single transaction.
func (im *Importer) Run(ctx context.Context) (*Result, error) {
	now := time.Now
	if im.Now != nil {
		now = im.Now
	}
	log := im.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	res := &Result{}

	types, err := im.readTypes(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := im.readMembers(ctx)
	if err != nil {
		return nil, err
	}
	res.Read = len(rows)

	tx, err := im.App.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	known := map[int64]bool{}
	for _, t := range types {
		known[t.ID] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO membership_types (id, name, sort_order) VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE name = VALUES(name)`, t.ID, t.Name, t.ID*10); err != nil {
			return nil, fmt.Errorf("importing membership type %d: %w", t.ID, err)
		}
		res.Types++
	}

	for _, r := range rows {
		mapped := Map(r, known, now())
		res.Warnings = append(res.Warnings, mapped.Warnings...)
		if err := im.upsertMember(ctx, tx, &mapped, res); err != nil {
			return nil, fmt.Errorf("importing legacy member %d: %w", r.MemberID, err)
		}
	}

	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT email1 FROM members
		WHERE deleted_at IS NULL AND email1 <> '' GROUP BY email1 HAVING COUNT(*) > 1) d`).Scan(&res.DuplicateEmails); err != nil {
		return nil, err
	}

	if im.DryRun {
		log.Info("dry run: rolling back")
		return res, nil
	}
	return res, tx.Commit()
}

func (im *Importer) readTypes(ctx context.Context) ([]members.MembershipType, error) {
	rows, err := im.Legacy.QueryContext(ctx, "SELECT ClidiveMembershipTypeId, ClidiveMembershipTypeDesc FROM membershipclidivetype ORDER BY ClidiveMembershipTypeId")
	if err != nil {
		return nil, fmt.Errorf("reading legacy membership types: %w", err)
	}
	defer rows.Close()
	var out []members.MembershipType
	for rows.Next() {
		var t members.MembershipType
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (im *Importer) readMembers(ctx context.Context) ([]Row, error) {
	rows, err := im.Legacy.QueryContext(ctx, SelectQuery())
	if err != nil {
		return nil, fmt.Errorf("reading legacy members: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(r.scanTargets()...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (im *Importer) upsertMember(ctx context.Context, tx *sql.Tx, mp *Mapped, res *Result) error {
	m := &mp.Member
	legacyID := m.LegacyID.Int64

	var existingID int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM members WHERE legacy_member_id = ? FOR UPDATE", legacyID).Scan(&existingID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Keep the legacy id as primary key unless an app-created member
		// already took it.
		var taken int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM members WHERE id = ?", legacyID).Scan(&taken); err != nil {
			return err
		}
		if taken > 0 {
			m.ID = 0
		}
		if _, err := members.InsertMember(ctx, tx, m); err != nil {
			return err
		}
		if err := members.WriteAudit(ctx, tx, Actor, members.ActionLegacyImport, m.ID, "member", m.ID, members.Diff(nil, m)); err != nil {
			return err
		}
		res.Created++
	case err != nil:
		return err
	default:
		m.ID = existingID
		old, err := scanExisting(ctx, tx, existingID)
		if err != nil {
			return err
		}
		changes := members.Diff(old, m)
		if len(changes) == 0 {
			res.Unchanged++
		} else {
			if err := members.UpdateMemberRow(ctx, tx, m); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE members SET deleted_at = ? WHERE id = ?", m.DeletedAt, m.ID); err != nil {
				return err
			}
			if err := members.WriteAudit(ctx, tx, Actor, members.ActionLegacyImport, m.ID, "member", m.ID, changes); err != nil {
				return err
			}
			res.Updated++
		}
	}
	return im.syncLegacyPayment(ctx, tx, m.ID, mp.LastPaid, res)
}

// scanExisting loads the fields compared by members.Diff.
func scanExisting(ctx context.Context, tx *sql.Tx, id int64) (*members.Member, error) {
	var m members.Member
	err := tx.QueryRowContext(ctx, `SELECT first_name, last_name, gender, date_of_birth, occupation, address1, address2, address3,
		town, postcode, phone1, phone2, phone3, email1, email2, email1_use, email2_use, ice_name, ice_relation, ice_phones,
		ice_address, bsac_member_id, bsac_member_id_if_known, bsac_renewal_due, bsac_annual_fee, bsac_abated,
		bsac_direct_debit, join_date, join_date_raw, membership_type_id, membership_expiry, query_text, notes, diver_grades,
		instructor_grades, other_qualifications, previous_club, committee_mailing, instructor_mailing,
		boat_handler_mailing, lifeguard_mailing, driver, deleted_at FROM members WHERE id = ?`, id).Scan(
		&m.FirstName, &m.LastName, &m.Gender, &m.DateOfBirth, &m.Occupation, &m.Address1, &m.Address2, &m.Address3,
		&m.Town, &m.Postcode, &m.Phone1, &m.Phone2, &m.Phone3, &m.Email1, &m.Email2, &m.Email1Use, &m.Email2Use,
		&m.ICEName, &m.ICERelation, &m.ICEPhones, &m.ICEAddress, &m.BSACMemberID, &m.BSACMemberIDIfKnown,
		&m.BSACRenewalDue, &m.BSACAnnualFee, &m.BSACAbated, &m.BSACDirectDebit, &m.JoinDate, &m.JoinDateRaw,
		&m.TypeID, &m.MembershipExpiry, &m.Query, &m.Notes, &m.DiverGrades, &m.InstructorGrades,
		&m.OtherQualifications, &m.PreviousClub, &m.CommitteeMailing, &m.InstructorMailing,
		&m.BoatHandlerMailing, &m.LifeguardMailing, &m.Driver, &m.DeletedAt)
	return &m, err
}

func (im *Importer) syncLegacyPayment(ctx context.Context, tx *sql.Tx, memberID int64, lastPaid sql.NullTime, res *Result) error {
	var pid int64
	var paidOn time.Time
	err := tx.QueryRowContext(ctx, "SELECT id, paid_on FROM payments WHERE member_id = ? AND source = ? ORDER BY id LIMIT 1",
		memberID, members.SourceLegacy).Scan(&pid, &paidOn)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	switch {
	case lastPaid.Valid && !exists:
		p := &members.Payment{MemberID: memberID, PaidOn: lastPaid.Time, Note: legacyPaymentNote, Source: members.SourceLegacy, CreatedBy: Actor.Label}
		if err := members.InsertPayment(ctx, tx, p); err != nil {
			return err
		}
		res.PaymentsCreated++
		return members.WriteAudit(ctx, tx, Actor, members.ActionPaymentAdd, memberID, "payment", p.ID,
			map[string]string{"paid_on": p.PaidOn.Format(members.DateLayout), "source": p.Source})
	case lastPaid.Valid && exists && !paidOn.Equal(lastPaid.Time):
		if _, err := tx.ExecContext(ctx, "UPDATE payments SET paid_on = ? WHERE id = ?", lastPaid.Time.Format(members.DateLayout), pid); err != nil {
			return err
		}
		res.PaymentsUpdated++
		return members.WriteAudit(ctx, tx, Actor, members.ActionLegacyImport, memberID, "payment", pid,
			members.Changes{"paid_on": {paidOn.Format(members.DateLayout), lastPaid.Time.Format(members.DateLayout)}})
	case !lastPaid.Valid && exists:
		if _, err := tx.ExecContext(ctx, "DELETE FROM payments WHERE id = ?", pid); err != nil {
			return err
		}
		res.PaymentsRemoved++
		return members.WriteAudit(ctx, tx, Actor, members.ActionPaymentDelete, memberID, "payment", pid,
			map[string]string{"paid_on": paidOn.Format(members.DateLayout), "source": members.SourceLegacy})
	}
	return nil
}
