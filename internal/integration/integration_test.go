// Package integration runs end-to-end tests against a real MariaDB server.
//
// They are skipped unless MEMBERSHIP_TEST_MYSQL_DSN points at a server
// (no database name), e.g.
//
//	MEMBERSHIP_TEST_MYSQL_DSN='root:secret@tcp(127.0.0.1:3306)/' go test ./internal/integration
//
// Each run creates two scratch databases (legacy_test_<n>, membership_test_<n>)
// filled with invented data, and drops them afterwards.
package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/db"
	"github.com/clidive410/membership/internal/legacy"
	"github.com/clidive410/membership/internal/members"
)

// legacyDDL mirrors the structure of the legacy tables (structure only).
const legacyDDL = "CREATE TABLE `membership` (" +
	"`memberid` int(10) unsigned NOT NULL AUTO_INCREMENT," +
	"`firstname` varchar(100) NOT NULL DEFAULT ''," +
	"`lastname` varchar(100) NOT NULL DEFAULT ''," +
	"`DOB` datetime DEFAULT NULL," +
	"`occupation` varchar(45) DEFAULT NULL," +
	"`address1` varchar(45) NOT NULL DEFAULT ''," +
	"`address2` varchar(45) DEFAULT NULL," +
	"`address3` varchar(45) DEFAULT NULL," +
	"`Town` varchar(45) NOT NULL DEFAULT ''," +
	"`Postcode` varchar(8) NOT NULL DEFAULT ''," +
	"`telephone1` varchar(15) DEFAULT NULL," +
	"`telephone2` varchar(15) DEFAULT NULL," +
	"`telephone3` varchar(15) DEFAULT NULL," +
	"`email1` varchar(50) NOT NULL DEFAULT ''," +
	"`email2` varchar(50) DEFAULT NULL," +
	"`email1use` tinyint(1) NOT NULL DEFAULT 1," +
	"`email2use` tinyint(1) NOT NULL DEFAULT 0," +
	"`ICEname` varchar(45) DEFAULT NULL," +
	"`ICErelation` varchar(45) DEFAULT NULL," +
	"`ICEtelephones` varchar(100) DEFAULT NULL," +
	"`ICEaddresses` varchar(200) DEFAULT NULL," +
	"`BSACMemberid` varchar(10) DEFAULT NULL," +
	"`BSACRenewalDueDate` datetime DEFAULT NULL," +
	"`BSACAnnualFee` decimal(10,0) DEFAULT NULL," +
	"`BSACAbated` tinyint(1) DEFAULT NULL," +
	"`ClidiveJoinDate` varchar(45) DEFAULT 'getdate()'," +
	"`ClidiveMembershipTypeId` int(10) unsigned DEFAULT NULL," +
	"`ClidiveMembershipExpiry` datetime DEFAULT NULL," +
	"`Query` varchar(255) DEFAULT NULL," +
	"`Notes` varchar(255) DEFAULT NULL," +
	"`OtherDiverGrades` varchar(100) DEFAULT NULL," +
	"`OtherInstructorGrades` varchar(100) DEFAULT NULL," +
	"`OtherQualifications` varchar(200) DEFAULT NULL," +
	"`PreviousDiveClub` varchar(50) DEFAULT NULL," +
	"`BSACMemberidIfKnown` varchar(10) DEFAULT NULL," +
	"`Gender` tinyint(1) DEFAULT NULL," +
	"`email` varchar(100) DEFAULT NULL," +
	"`surname` varchar(60) DEFAULT NULL," +
	"`password` varchar(40) DEFAULT 'password'," +
	"`passhint` varchar(100) NOT NULL DEFAULT 'passhint'," +
	"`admin` tinyint(3) unsigned NOT NULL DEFAULT 0," +
	"`session` varchar(32) DEFAULT NULL," +
	"`LastLogin` datetime DEFAULT NULL," +
	"`BSACDirectDebit` tinyint(3) unsigned NOT NULL DEFAULT 0," +
	"`InstructorMailing` tinyint(3) unsigned NOT NULL DEFAULT 0," +
	"`CommitteeMailing` tinyint(3) unsigned NOT NULL DEFAULT 0," +
	"`DeletedOn` datetime DEFAULT NULL," +
	"`BoatHandlerMailing` tinyint(3) unsigned NOT NULL DEFAULT 0," +
	"`LifeguardMailing` tinyint(3) unsigned NOT NULL DEFAULT 0," +
	"`Driver` tinyint(1) unsigned NOT NULL DEFAULT 0," +
	"`last_paid` date DEFAULT NULL," +
	"PRIMARY KEY (`memberid`)" +
	") ENGINE=InnoDB DEFAULT CHARSET=latin1"

const typesDDL = "CREATE TABLE `membershipclidivetype` (" +
	"`ClidiveMembershipTypeId` int(10) unsigned NOT NULL AUTO_INCREMENT," +
	"`ClidiveMembershipTypeDesc` varchar(45) NOT NULL DEFAULT ''," +
	"PRIMARY KEY (`ClidiveMembershipTypeId`)) ENGINE=InnoDB DEFAULT CHARSET=latin1"

// Invented people only.
var legacyRows = []string{
	"INSERT INTO membershipclidivetype VALUES (1,'Annual'),(2,'Standing Order'),(3,'Social'),(4,'Honorary'),(5,'3 Month (eg OD)'),(6,'Lapsed')",
	`INSERT INTO membership (memberid, firstname, lastname, DOB, address1, Town, Postcode, telephone1, email1, email2, email1use, email2use,
		ICEname, BSACMemberid, BSACRenewalDueDate, BSACAnnualFee, BSACAbated, ClidiveJoinDate, ClidiveMembershipTypeId,
		ClidiveMembershipExpiry, Notes, OtherDiverGrades, Gender, email, surname, password, passhint, admin, session,
		BSACDirectDebit, CommitteeMailing, Driver, last_paid)
	 VALUES (101, 'Alice', 'Example', '1985-06-15 00:00:00', '1 Test Road', 'London', 'ZZ1 1ZZ', '020 0000 0001',
		'Alice@Example.org', 'alice2@example.net', 1, 1, 'Bob Example', 'X0001', '2026-04-01 00:00:00', 71, 0, '2012-9-3', 2,
		'2027-03-31 00:00:00', 'Invented member', 'Ocean Diver', 1, 'alice-login', 'Example', 'plaintext-secret', 'hint', 1, 'sess',
		1, 1, 1, '2026-01-10')`,
	`INSERT INTO membership (memberid, firstname, lastname, DOB, address1, Town, Postcode, email1, email1use,
		BSACRenewalDueDate, ClidiveJoinDate, ClidiveMembershipTypeId, ClidiveMembershipExpiry, Gender, DeletedOn, password)
	 VALUES (102, 'Barry', 'Archived', '0000-00-00 00:00:00', '2 Test Road', 'London', 'ZZ2 2ZZ', 'barry@example.org', 1,
		'0000-00-00 00:00:00', '', 6, '0000-00-00 00:00:00', 0, '2020-05-06 07:08:09', 'plaintext-secret')`,
	`INSERT INTO membership (memberid, firstname, lastname, address1, Town, Postcode, email1, email1use, email2, email2use,
		ClidiveJoinDate, ClidiveMembershipTypeId, Gender, InstructorMailing)
	 VALUES (103, 'Cara', 'Oddity', '3 Test Road', 'London', 'ZZ3 3ZZ', 'cara@example.org', 0, 'cara.alt@example.org', 1,
		'31/12/99?', 3, NULL, 1)`,
}

type fixture struct {
	ctx       context.Context
	server    *sql.DB
	legacy    *sql.DB
	app       *sql.DB
	legacyDSN string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	base := os.Getenv("MEMBERSHIP_TEST_MYSQL_DSN")
	if base == "" {
		t.Skip("MEMBERSHIP_TEST_MYSQL_DSN not set; skipping MariaDB integration test")
	}
	ctx := context.Background()
	cfg, err := mysql.ParseDSN(base)
	if err != nil {
		t.Fatal(err)
	}
	server, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	n := rand.IntN(1_000_000)
	legacyName, appName := fmt.Sprintf("legacy_test_%d", n), fmt.Sprintf("membership_test_%d", n)
	for _, name := range []string{legacyName, appName} {
		if _, err := server.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, name := range []string{legacyName, appName} {
			server.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+name+"`")
		}
		server.Close()
	})
	dsnFor := func(name string) string {
		c := cfg.Clone()
		c.DBName = name
		return c.FormatDSN()
	}
	f := &fixture{ctx: ctx, server: server, legacyDSN: dsnFor(legacyName)}

	f.legacy, err = legacy.OpenLegacy(ctx, f.legacyDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.legacy.Close() })
	for _, stmt := range append([]string{legacyDDL, typesDDL}, legacyRows...) {
		if _, err := f.legacy.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("legacy setup: %v", err)
		}
	}
	f.app, err = db.Open(ctx, dsnFor(appName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.app.Close() })
	if err := db.Migrate(ctx, f.app, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	// Migrations are idempotent.
	if err := db.Migrate(ctx, f.app, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) importOnce(t *testing.T) *legacy.Result {
	t.Helper()
	res, err := (&legacy.Importer{Legacy: f.legacy, App: f.app,
		Now: func() time.Time { return time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC) }}).Run(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestLegacyImportIdempotent(t *testing.T) {
	f := setup(t)
	store := members.NewStore(f.app)

	res := f.importOnce(t)
	if res.Read != 3 || res.Created != 3 || res.Updated != 0 || res.PaymentsCreated != 1 || res.Types != 6 || res.RenewalTypes != -1 {
		t.Fatalf("first import: %+v", res)
	}

	alice, err := store.GetMember(f.ctx, 101)
	if err != nil {
		t.Fatal(err)
	}
	if alice.Email1 != "alice@example.org" || alice.Gender != "F" || members.FormatDate(alice.DateOfBirth) != "1985-06-15" ||
		alice.TypeName != "Standing Order" || alice.BSACAnnualFee.String() != "71.00" || !alice.Email2Use ||
		members.FormatDate(alice.JoinDate) != "2012-09-03" || members.FormatDate(alice.LastPaid) != "2026-01-10" ||
		!alice.CommitteeMailing || !alice.Driver || !alice.BSACDirectDebit || alice.ShareContactDetails {
		t.Errorf("alice mapped wrong: %+v", alice)
	}
	barry, _ := store.GetMember(f.ctx, 102)
	if !barry.Archived() || barry.DeletedAt.Time.Format(time.DateTime) != "2020-05-06 07:08:09" || barry.DateOfBirth.Valid || barry.MembershipExpiry.Valid {
		t.Errorf("barry mapped wrong: %+v", barry)
	}
	cara, _ := store.GetMember(f.ctx, 103)
	if members.FormatDate(cara.JoinDate) != "1999-12-31" || cara.Gender != "" || !cara.InstructorMailing {
		t.Errorf("cara mapped wrong: %+v", cara)
	}

	// Legacy login data never reaches the new database.
	var n int
	f.app.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	if n != 0 {
		t.Errorf("import created %d users", n)
	}
	f.app.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE()
		AND COLUMN_NAME IN ('password', 'passhint', 'session', 'surname')`).Scan(&n)
	if n != 0 {
		t.Errorf("legacy login columns exist in the new schema")
	}
	var leaked int
	f.app.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM audit_log WHERE changes LIKE '%plaintext-secret%'").Scan(&leaked)
	if leaked != 0 {
		t.Error("legacy password leaked into the audit log")
	}

	// Second run: nothing changes.
	res = f.importOnce(t)
	if res.Created != 0 || res.Updated != 0 || res.Unchanged != 3 || res.PaymentsCreated != 0 || res.PaymentsUpdated != 0 {
		t.Errorf("second import not idempotent: %+v", res)
	}
	var payments int
	f.app.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM payments").Scan(&payments)
	if payments != 1 {
		t.Errorf("payments = %d", payments)
	}

	// Changes in the legacy data are applied as updates.
	if _, err := f.legacy.ExecContext(f.ctx, "UPDATE membership SET lastname = 'Renamed', last_paid = '2026-02-01' WHERE memberid = 101"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.legacy.ExecContext(f.ctx, "UPDATE membership SET last_paid = '2025-12-01' WHERE memberid = 103"); err != nil {
		t.Fatal(err)
	}
	res = f.importOnce(t)
	if res.Updated != 1 || res.Unchanged != 2 || res.PaymentsUpdated != 1 || res.PaymentsCreated != 1 {
		t.Errorf("third import: %+v", res)
	}
	alice, _ = store.GetMember(f.ctx, 101)
	if alice.LastName != "Renamed" || members.FormatDate(alice.LastPaid) != "2026-02-01" {
		t.Errorf("update not applied: %+v", alice)
	}
	var audits int
	f.app.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM audit_log WHERE member_id = 101 AND action = 'legacy.import'").Scan(&audits)
	if audits < 2 {
		t.Errorf("expected audit entries for import and update, got %d", audits)
	}

	// Dry run commits nothing.
	f.legacy.ExecContext(f.ctx, "UPDATE membership SET lastname = 'DryRun' WHERE memberid = 101")
	if _, err := (&legacy.Importer{Legacy: f.legacy, App: f.app, DryRun: true}).Run(f.ctx); err != nil {
		t.Fatal(err)
	}
	alice, _ = store.GetMember(f.ctx, 101)
	if alice.LastName != "Renamed" {
		t.Error("dry run changed data")
	}
}

func TestStoresAgainstMariaDB(t *testing.T) {
	f := setup(t)
	f.importOnce(t)
	store := members.NewStore(f.app)
	as := auth.NewStore(f.app)
	actor := members.Actor{Label: "tester@example.org"}

	cur, err := store.ListMembers(f.ctx, members.Filter{Status: members.StatusNotArchived})
	if err != nil || len(cur) != 2 {
		t.Fatalf("not-archived list: %d %v", len(cur), err)
	}
	if def, _ := store.ListMembers(f.ctx, members.Filter{Today: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)}); len(def) != len(cur) {
		t.Errorf("current list with nothing expired yet should equal the not-archived list: %d vs %d", len(def), len(cur))
	}
	all, _ := store.ListMembers(f.ctx, members.Filter{Status: members.StatusAll, Sort: "last_paid", Desc: true})
	if len(all) != 3 || all[0].ID != 101 {
		t.Errorf("all list sorted by last paid: %+v", all)
	}
	withExpiry := 0
	for _, m := range cur {
		if m.MembershipExpiry.Valid {
			withExpiry++
		}
	}
	expired, _ := store.ListMembers(f.ctx, members.Filter{Status: members.StatusExpired, Today: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)})
	if withExpiry == 0 || len(expired) != withExpiry {
		t.Errorf("expired list far in the future should hold every current member with an expiry date: %d vs %d", len(expired), withExpiry)
	}
	if expired, _ = store.ListMembers(f.ctx, members.Filter{Status: members.StatusExpired, Today: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)}); len(expired) != 0 {
		t.Errorf("expired list far in the past should be empty, got %d", len(expired))
	}
	if def, _ := store.ListMembers(f.ctx, members.Filter{Today: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}); len(def) != len(cur)-withExpiry {
		t.Errorf("current list far in the future should hold only members without an expiry date: %d vs %d", len(def), len(cur)-withExpiry)
	}
	found, _ := store.ListMembers(f.ctx, members.Filter{Query: "odd 100%", Status: members.StatusAll})
	if len(found) != 0 {
		t.Errorf("LIKE wildcards must be escaped, got %d", len(found))
	}
	found, _ = store.ListMembers(f.ctx, members.Filter{Query: "oddity", TypeID: 3})
	if len(found) != 1 || found[0].ID != 103 {
		t.Errorf("search: %+v", found)
	}

	// Update with audit and duplicate-email protection.
	cara, _ := store.GetMember(f.ctx, 103)
	cara.Email1 = "ALICE@example.org"
	if err := store.UpdateMember(f.ctx, actor, cara); err != members.ErrDuplicateEmail {
		t.Errorf("duplicate email: %v", err)
	}
	cara, _ = store.GetMember(f.ctx, 103)
	cara.Notes = "Changed by test"
	if err := store.UpdateMember(f.ctx, actor, cara); err != nil {
		t.Fatal(err)
	}
	entries, _ := store.ListAudit(f.ctx, 103, 10)
	if len(entries) == 0 || entries[0].Actor != "tester@example.org" || !strings.Contains(entries[0].Changes, "Changed by test") {
		t.Errorf("audit: %+v", entries)
	}

	// Email lists follow the legacy view rules.
	emails, _ := store.EmailList(f.ctx, members.ListAll, false)
	if strings.Join(emails, ",") != "alice@example.org,alice2@example.net,cara.alt@example.org" {
		t.Errorf("all list = %v", emails)
	}
	emails, _ = store.EmailList(f.ctx, members.ListInstructor, false)
	if strings.Join(emails, ",") != "cara.alt@example.org" {
		t.Errorf("instructor list = %v", emails)
	}

	// Bulk payment skips archived members and renews expiry.
	n, err := store.RecordBulkPayment(f.ctx, actor, []int64{101, 102, 103}, members.BulkPayment{
		PaidOn: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), NewExpiry: sql.NullTime{Time: time.Date(2027, 9, 30, 0, 0, 0, 0, time.UTC), Valid: true},
	})
	if err != nil || n != 2 {
		t.Fatalf("bulk: %d %v", n, err)
	}
	cara, _ = store.GetMember(f.ctx, 103)
	if members.FormatDate(cara.MembershipExpiry) != "2027-09-30" || members.FormatDate(cara.LastPaid) != "2026-10-01" {
		t.Errorf("bulk not applied: %+v", cara)
	}
	if err := store.ArchiveMember(f.ctx, actor, 103); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreMember(f.ctx, actor, 103); err != nil {
		t.Fatal(err)
	}

	// Users, sessions, roles and magic-link tokens.
	uid, err := as.EnsureUser(f.ctx, " Alice@Example.org ")
	if err != nil {
		t.Fatal(err)
	}
	if err := as.LinkMember(f.ctx, uid, 101); err != nil {
		t.Fatal(err)
	}
	if err := as.SetPassword(f.ctx, uid, "a-long-test-password"); err != nil {
		t.Fatal(err)
	}
	if err := as.GrantRole(f.ctx, uid, auth.RoleMembershipManager); err != nil {
		t.Fatal(err)
	}
	if _, err := as.Authenticate(f.ctx, "alice@example.org", "wrong-password!!"); err != auth.ErrInvalidCredentials {
		t.Errorf("wrong password: %v", err)
	}
	p, err := as.Authenticate(f.ctx, "ALICE@example.org", "a-long-test-password")
	if err != nil || p.MemberID != 101 || !p.HasRole(auth.RoleMembershipManager) {
		t.Fatalf("authenticate: %+v %v", p, err)
	}
	tok, _ := as.CreateSession(f.ctx, uid, auth.LoginMethodPassword, time.Hour)
	if p, _ := as.LookupSession(f.ctx, tok); p == nil || p.UserID != uid {
		t.Error("session lookup failed")
	}
	var stored string
	f.app.QueryRowContext(f.ctx, "SELECT token_hash FROM sessions LIMIT 1").Scan(&stored)
	if stored == tok {
		t.Error("raw session token stored")
	}
	if err := as.SetDisabled(f.ctx, uid, true); err != nil {
		t.Fatal(err)
	}
	if p, _ := as.LookupSession(f.ctx, tok); p != nil {
		t.Error("disabled user's session still valid")
	}
	as.SetDisabled(f.ctx, uid, false)

	// Changing the member's primary email moves the login identity with it.
	alice, _ := store.GetMember(f.ctx, 101)
	alice.Email1 = "alice.new@example.org"
	if err := store.UpdateMember(f.ctx, actor, alice); err != nil {
		t.Fatal(err)
	}
	if id, err := as.UserIDByEmail(f.ctx, "alice.new@example.org"); err != nil || id != uid {
		t.Errorf("login email not synced: %d %v", id, err)
	}

	lt, _ := as.CreateLoginToken(f.ctx, uid, 15*time.Minute)
	if got, err := as.ConsumeLoginToken(f.ctx, lt); err != nil || got != uid {
		t.Errorf("consume: %d %v", got, err)
	}
	if _, err := as.ConsumeLoginToken(f.ctx, lt); err == nil {
		t.Error("login token reusable")
	}
	if err := as.PurgeExpired(f.ctx); err != nil {
		t.Error(err)
	}
}

// TestLegacyImportRepairsEncoding stores UTF-8 bytes in the latin1 legacy
// columns, as the old PHP site did, and checks that the import repairs the
// resulting double encoding (and stays idempotent).
func TestLegacyImportRepairsEncoding(t *testing.T) {
	f := setup(t)
	// _binary literals bypass conversion, so the latin1 columns hold the raw
	// UTF-8 bytes of "Zoë", "Müller-René" and "Paid £5 – invented".
	if _, err := f.legacy.ExecContext(f.ctx, `INSERT INTO membership (memberid, firstname, lastname, address1, Town, Postcode,
		email1, email1use, Notes, OtherQualifications)
		VALUES (104, _binary X'5A6FC3AB', _binary X'4DC3BC6C6C65722D52656EC3A9', '4 Test Road', 'London', 'ZZ4 4ZZ',
		'zoe@example.org', 1, _binary X'5061696420C2A33520E2809320696E76656E746564', 'Plain ASCII')`); err != nil {
		t.Fatal(err)
	}
	f.importOnce(t)
	m, err := members.NewStore(f.app).GetMember(f.ctx, 104)
	if err != nil {
		t.Fatal(err)
	}
	if m.FirstName != "Zoë" || m.LastName != "Müller-René" || m.Notes != "Paid £5 – invented" || m.OtherQualifications != "Plain ASCII" {
		t.Errorf("encoding not repaired: %q %q %q %q", m.FirstName, m.LastName, m.Notes, m.OtherQualifications)
	}
	if res := f.importOnce(t); res.Updated != 0 || res.Unchanged != 4 {
		t.Errorf("re-import after repair not idempotent: %+v", res)
	}
}

func TestMigrationLock(t *testing.T) {
	f := setup(t)
	log := slog.New(slog.DiscardHandler)

	// Another session holds the lock: Migrate gives up after the timeout.
	holder, err := f.server.Conn(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	var got int
	if err := holder.QueryRowContext(f.ctx, "SELECT GET_LOCK(?, 5)", db.MigrationLock).Scan(&got); err != nil || got != 1 {
		t.Fatalf("taking lock: %v (%d)", err, got)
	}
	start := time.Now()
	err = db.MigrateWithTimeout(f.ctx, f.app, time.Second, log)
	if !errors.Is(err, db.ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	if d := time.Since(start); d < 900*time.Millisecond || d > 10*time.Second {
		t.Errorf("waited %s", d)
	}
	if _, err := holder.ExecContext(f.ctx, "SELECT RELEASE_LOCK(?)", db.MigrationLock); err != nil {
		t.Fatal(err)
	}

	// Concurrent starts both succeed, and the lock is free afterwards.
	errc := make(chan error, 3)
	for range 3 {
		go func() { errc <- db.Migrate(f.ctx, f.app, log) }()
	}
	for range 3 {
		if err := <-errc; err != nil {
			t.Errorf("concurrent migrate: %v", err)
		}
	}
	var free int
	if err := f.server.QueryRowContext(f.ctx, "SELECT IS_FREE_LOCK(?)", db.MigrationLock).Scan(&free); err != nil || free != 1 {
		t.Errorf("lock not released: %v (%d)", err, free)
	}
}
