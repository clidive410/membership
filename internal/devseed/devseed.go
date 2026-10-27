// Package devseed generates invented members for local UI work and demos.
// Every name, address and email is made up; emails use the reserved
// example.org/example.net domains (RFC 2606).
package devseed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/clidive410/membership/internal/members"
)

var (
	firstNames = []string{"Ada", "Ben", "Cara", "Dev", "Elin", "Finn", "Gwen", "Hari", "Iris", "Jon", "Kai", "Lena",
		"Milo", "Nia", "Owen", "Pia", "Quin", "Rhys", "Sana", "Theo", "Uma", "Vic", "Wren", "Xan", "Yara", "Zoë",
		"José", "Chloé", "Søren", "Niamh"}
	lastNames = []string{"Anchor", "Barnacle", "Coral", "Drift", "Eddy", "Fathom", "Gill", "Harbour", "Inlet", "Jetty",
		"Kelp", "Lagoon", "Mooring", "Neap", "Oyster", "Pebble", "Quay", "Reef", "Shoal", "Tide", "Undertow",
		"Wrasse", "Müller", "O'Neill", "Ångström", "Dupré"}
	streets = []string{"Harbour Road", "Quay Street", "Lighthouse Lane", "Seaview Terrace", "Mariner Close", "Dock Walk"}
	towns   = []string{"Exampleton", "Testbury", "Sampleford", "Mocksea", "Demohaven"}
	grades  = []string{"Ocean Diver", "Sports Diver", "Dive Leader", "Advanced Diver", "First Class Diver"}
	notes   = []string{"", "", "", "Prefers Tuesday pool sessions", "Paid £71 cash – receipt given", "Renewal chased by email",
		"Interested in boat handling course", "Kit hire – owes £10", "Moved from another branch"}
	methods = []string{"Standing order", "Bank transfer", "Cash", "Card"}
)

// Member returns the i-th invented member for a seed. The same (seed, i)
// always gives the same member.
func Member(r *rand.Rand, i int, today time.Time) members.Member {
	pick := func(xs []string) string { return xs[r.IntN(len(xs))] }
	first, last := firstNames[i%len(firstNames)], pick(lastNames)
	local := strings.NewReplacer("ë", "e", "é", "e", "ø", "o", "ö", "o", "ü", "u", "å", "a", "'", "").Replace(strings.ToLower(first + "." + last))
	day := func(fromDays, toDays int) sql.NullTime {
		d := today.AddDate(0, 0, fromDays+r.IntN(toDays-fromDays+1))
		return sql.NullTime{Time: time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
	}
	typeID := []int64{members.TypeAnnual, members.TypeStandingOrder, members.TypeStandingOrder, members.TypeSocial,
		members.TypeHonorary, members.TypeThreeMonth, members.TypeLapsed}[r.IntN(7)]
	m := members.Member{
		FirstName:        first,
		LastName:         last,
		Gender:           []string{"M", "F", ""}[r.IntN(3)],
		DateOfBirth:      day(-70*365, -18*365),
		Address1:         fmt.Sprintf("%d %s", 1+r.IntN(120), pick(streets)),
		Town:             pick(towns),
		Postcode:         fmt.Sprintf("ZZ%d %dZZ", 1+r.IntN(9), 1+r.IntN(9)),
		Phone1:           fmt.Sprintf("07700 900%03d", r.IntN(1000)), // Ofcom drama range
		Email1:           fmt.Sprintf("%s.%d@example.org", local, i+1),
		Email1Use:        r.IntN(10) > 0,
		ICEName:          pick(firstNames) + " " + last,
		ICERelation:      pick([]string{"Partner", "Parent", "Sibling", "Friend"}),
		ICEPhones:        fmt.Sprintf("07700 900%03d", r.IntN(1000)),
		BSACMemberID:     fmt.Sprintf("T%06d", 100000+r.IntN(900000)),
		BSACRenewalDue:   day(-60, 330),
		BSACDirectDebit:  r.IntN(2) == 0,
		JoinDate:         day(-20*365, -30),
		TypeID:           sql.NullInt64{Int64: typeID, Valid: true},
		MembershipExpiry: day(-200, 365),
		Notes:            pick(notes),
		DiverGrades:      pick(grades),
		CommitteeMailing: r.IntN(8) == 0, InstructorMailing: r.IntN(6) == 0,
		BoatHandlerMailing: r.IntN(5) == 0, LifeguardMailing: r.IntN(7) == 0, Driver: r.IntN(4) == 0,
	}
	if r.IntN(4) == 0 {
		m.Email2 = fmt.Sprintf("%s.alt%d@example.net", local, i+1)
		m.Email2Use = r.IntN(2) == 0
	}
	if r.IntN(3) == 0 {
		m.BSACAnnualFee = members.Money{Pence: 7100, Valid: true}
	}
	return m
}

// Result counts what Seed inserted.
type Result struct{ Members, Archived, Payments int }

// Seed inserts n invented members (with a payment history) through the
// normal store, so audit entries are written too. It refuses to touch a
// database that already holds members, so it cannot mix invented records
// into real data.
func Seed(ctx context.Context, store *members.Store, n int, seed uint64, today time.Time) (*Result, error) {
	var count int
	if err := store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM members").Scan(&count); err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, errors.New("seed-dev: the members table is not empty; use a fresh scratch database")
	}
	r := rand.New(rand.NewPCG(seed, 0x636c6964697665))
	actor := members.Actor{Label: "seed-dev"}
	res := &Result{}
	for i := range n {
		m := Member(r, i, today)
		id, err := store.CreateMember(ctx, actor, &m)
		if err != nil {
			return nil, fmt.Errorf("member %d: %w", i, err)
		}
		res.Members++
		for p := range r.IntN(4) {
			pay := &members.Payment{MemberID: id, PaidOn: today.AddDate(-p, 0, -r.IntN(60)),
				Method: methods[r.IntN(len(methods))], Source: members.SourceManual}
			if r.IntN(2) == 0 {
				pay.Amount = members.Money{Pence: int64(3000 + 500*r.IntN(10)), Valid: true}
			}
			if err := store.AddPayment(ctx, actor, pay); err != nil {
				return nil, err
			}
			res.Payments++
		}
		if r.IntN(8) == 0 {
			if err := store.ArchiveMember(ctx, actor, id); err != nil {
				return nil, err
			}
			res.Archived++
		}
	}
	return res, nil
}
