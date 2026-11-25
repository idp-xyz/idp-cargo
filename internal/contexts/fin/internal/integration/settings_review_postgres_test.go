//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// The settings' corrections after the independent review: the database recomputes whether a change set loosens, ties
// the recorded times to its own clock and writes history forwards; commands are stamped after they hold the tenant's
// lock; an approved exception is shown and announced; change sets take effect in the order of their versions; two
// identical commands together answer one receipt; a change set far ahead is refused; an approver refuses a proposal.

// A raw write cannot loosen alone: the automatic approval of a change set the database finds loosening is refused, and
// so is a loosening flag that is not the database's answer, each measured against the values in force at the effective
// time, the company's value for a legal entity (the floor) and a threshold raised.
func TestFinSettingsDatabaseRecomputesWhetherAChangeSetLoosens(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS"))) // the company tightens F7: in force at once
	s.must(s.propose("U1", "ann", time.Time{}, tenantEntry("fin.F1", "UNRESTRICTED")))
	s.must(s.approve("U1", "bob")) // the company's F1 is the loosest, approved by a second person
	s.must(s.propose("TH", "ann", time.Time{}, api.FinSettingEntry{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", Key: "fin.F1", Value: "NOT_ABOVE_THRESHOLD",
		Threshold: &kernel.Money{Currency: "CNY", Minor: 1000}})) // tighter than the company's value: on the spot
	if c := s.changes()["TH"]; c.Loosening || c.Version == 0 {
		t.Fatalf("a threshold under the company's UNRESTRICTED: %+v", c)
	}
	chg := func(id string, loosening bool, entries ...string) []string {
		return append([]string{rawChange(id, "mallory", "now()", loosening)}, entries...)
	}
	auto := func(id string) string {
		return rawApproval(id, "mallory", "proposed_at", nextVersion, "effective_at", true)
	}
	refused := func(name, constraint string, stmts ...string) {
		t.Helper()
		err := s.rawTx(t, stmts...)
		if state, c := sqlState(err); err == nil || state != "23514" || c != constraint {
			t.Errorf("%s: %v (state %s, constraint %s), want %s", name, err, state, c, constraint)
		}
	}
	accepted := func(name string, stmts ...string) {
		t.Helper()
		err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, stmt := range stmts {
				if _, err := e.Exec(ctx, stmt, s.tenant); err != nil {
					return err
				}
			}
			return errRollBack
		})
		if !errors.Is(err, errRollBack) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// against the value in force: the company's F7 is BLOCKS
	refused("the reviewer's case: a loosening written alone", "settings_approval_loosening",
		append(chg("R1", false, rawEntry("R1", 1, "TENANT", "", "fin.F7", "DOES_NOT_BLOCK", "NULL")), auto("R1"))...)
	accepted("a tightening written alone", append(chg("R1", false, rawEntry("R1", 1, "TENANT", "", "fin.F2", "SUBMITTER_AND_PREVIEWER", "NULL")), auto("R1"))...)
	accepted("a value kept written alone", append(chg("R1", false, rawEntry("R1", 1, "TENANT", "", "fin.F7", "BLOCKS", "NULL")), auto("R1"))...)
	// the floor: a legal entity looser than the company's BLOCKS, though the legal entity has no value of its own
	refused("a legal entity looser than the company", "settings_approval_loosening",
		append(chg("R2", false, rawEntry("R2", 1, "LEGAL_ENTITY", "LE", "fin.F7", "DOES_NOT_BLOCK", "NULL")), auto("R2"))...)
	// the company's entry of the same change set counts: the company tightens F2 and a legal entity stays at the
	// default
	refused("a legal entity looser than the company's new value", "settings_approval_loosening",
		append(chg("R3", false, rawEntry("R3", 1, "TENANT", "", "fin.F2", "SUBMITTER_AND_PREVIEWER", "NULL"), rawEntry("R3", 2, "LEGAL_ENTITY", "LE", "fin.F2", "SUBMITTER_ONLY", "NULL")),
			auto("R3"))...)
	// a threshold raised on the same value; lowered it is not looser (the company's F1 is UNRESTRICTED)
	refused("a threshold raised", "settings_approval_loosening",
		append(chg("R4", false, rawEntry("R4", 1, "LEGAL_ENTITY", "LE", "fin.F1", "NOT_ABOVE_THRESHOLD", "2000")), auto("R4"))...)
	accepted("a threshold lowered", append(chg("R4", false, rawEntry("R4", 1, "LEGAL_ENTITY", "LE", "fin.F1", "NOT_ABOVE_THRESHOLD", "500")), auto("R4"))...)
	accepted("a threshold kept", append(chg("R4", false, rawEntry("R4", 1, "LEGAL_ENTITY", "LE", "fin.F1", "NOT_ABOVE_THRESHOLD", "1000")), auto("R4"))...)
	// a loosening at a time the value in force is another: the change set measured at its effective time, not now
	future := "now() + interval '2 hours'"
	refused("a loosening in the future, written alone", "settings_approval_loosening",
		append([]string{rawChange("R5", "mallory", future, false), rawEntry("R5", 1, "TENANT", "", "fin.F7", "DOES_NOT_BLOCK", "NULL")}, auto("R5"))...)
	// the stored flag is the database's answer, both ways, at commit
	refused("flagged as not loosening, and it is", "settings_change_loosening_flag",
		chg("R6", false, rawEntry("R6", 1, "TENANT", "", "fin.F7", "DOES_NOT_BLOCK", "NULL"))...)
	refused("flagged as loosening, and it is not", "settings_change_loosening_flag",
		chg("R7", true, rawEntry("R7", 1, "TENANT", "", "fin.F2", "SUBMITTER_AND_PREVIEWER", "NULL"))...)
	accepted("flagged as loosening, and it is, waiting for a second person", chg("R8", true, rawEntry("R8", 1, "TENANT", "", "fin.F7", "DOES_NOT_BLOCK", "NULL"))...)
	// a change set approved by a second person may loosen: the approval of a loosening change set by someone else is
	// accepted
	if err := s.rawTx(t, append(chg("R9", true, rawEntry("R9", 1, "TENANT", "", "fin.F7", "DOES_NOT_BLOCK", "NULL")),
		rawApproval("R9", "bob", "proposed_at", nextVersion, "effective_at", false))...); err != nil {
		t.Errorf("a loosening approved by another person: %v", err)
	}
	// the ledger and the database agree on every change set the commands made
	for id, c := range s.changes() {
		var db bool
		err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
			return e.QueryRow(ctx, `SELECT cargo_fin.settings_loosening($1,$2)`, s.tenant, id).Scan(&db)
		})
		if err != nil || (id != "R9" && db != c.Loosening) {
			t.Errorf("%s: the ledger says loosening %v, the database %v (%v)", id, c.Loosening, db, err)
		}
	}
}

// The ledger and the database give the same answer to "does it loosen", over random sequences of the real commands: the
// database refuses a change set whose flag it does not agree with at commit, so a disagreement shows as a raw error.
func TestFinSettingsDatabaseAgreesWithTheLedger(t *testing.T) {
	keys := map[string][]string{"fin.F1": {"NOT_ALLOWED", "NOT_ABOVE_THRESHOLD", "UNRESTRICTED"}, "fin.F2": {"SUBMITTER_AND_PREVIEWER", "SUBMITTER_ONLY"},
		"fin.F3": {"EVERY_ATTRIBUTION", "ONLY_WHEN_AMOUNT_MOVES"}, "fin.F7": {"BLOCKS", "DOES_NOT_BLOCK"}, "fin.F9": {"RECONFIRM", "KEEP_RATIO", "KEEP_AMOUNT"}}
	order := []string{"fin.F1", "fin.F2", "fin.F3", "fin.F7", "fin.F9"}
	for seed := int64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			s := newSettingsWorld(t)
			rng := rand.New(rand.NewSource(seed))
			var pending []string
			asserts := 0
			for i := 0; i < 40; i++ {
				switch rng.Intn(4) {
				case 0, 1, 2:
					key := order[rng.Intn(len(order))]
					value := keys[key][rng.Intn(len(keys[key]))]
					entry := api.FinSettingEntry{ScopeKind: "TENANT", Key: key, Value: value}
					switch rng.Intn(3) {
					case 1:
						entry.ScopeKind, entry.ScopeID = "LEGAL_ENTITY", "LE"
					case 2:
						entry.ScopeKind, entry.ScopeID = "LEGAL_ENTITY", "LE2"
					}
					if value == "NOT_ABOVE_THRESHOLD" {
						if entry.ScopeKind == "TENANT" {
							continue
						}
						cur := map[string]kernel.CurrencyCode{"LE": "CNY", "LE2": "USD"}[entry.ScopeID]
						entry.Threshold = &kernel.Money{Currency: cur, Minor: int64(100 * (1 + rng.Intn(4)))}
					}
					id := fmt.Sprintf("c%d", i)
					r, err := s.propose(id, "ann", time.Time{}, entry)
					if err != nil {
						if strings.Contains(err.Error(), "SQLSTATE") {
							t.Fatalf("the database disagreed with the ledger over %+v: %v", entry, err)
						}
						continue
					}
					if r.Version == 0 {
						pending = append(pending, id)
					}
					asserts++
				case 3:
					if len(pending) == 0 {
						continue
					}
					k := rng.Intn(len(pending))
					id := pending[k]
					pending = append(pending[:k], pending[k+1:]...)
					var err error
					if rng.Intn(3) == 0 {
						_, err = s.cancel(id, "eve")
					} else {
						_, err = s.approve(id, "bob")
					}
					if err != nil && strings.Contains(err.Error(), "SQLSTATE") {
						t.Fatalf("the database refused %s: %v", id, err)
					}
				}
			}
			if asserts < 10 {
				t.Fatalf("only %d proposals were accepted", asserts)
			}
		})
	}
}

// The values' order the database holds is the catalogue's: each value's rank, each item's default and the one threshold
// that lets more through when higher.
func TestFinSettingsDatabaseOrderIsTheCatalogues(t *testing.T) {
	s := newSettingsWorld(t)
	cat, err := s.module.GetFinSettingsCatalogue(context.Background(), api.GetFinSettingsCatalogue{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range cat.Items {
		for rank, v := range it.Values {
			var gotRank, gotDefault *int
			var higher bool
			err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
				return e.QueryRow(ctx, `SELECT cargo_fin.settings_rank($1,$2), cargo_fin.settings_default_rank($1), cargo_fin.settings_looser_when_higher($1,$2)`,
					it.Key, v.ID).Scan(&gotRank, &gotDefault, &higher)
			})
			wantDefault := 0
			for i, w := range it.Values {
				if w.ID == it.Default {
					wantDefault = i
				}
			}
			if err != nil || gotRank == nil || *gotRank != rank || gotDefault == nil || *gotDefault != wantDefault || higher != (v.Parameter != "") {
				t.Errorf("%s %s: rank %v want %d, default %v want %d, looser when higher %v want %v (%v)", it.Key, v.ID, gotRank, rank, gotDefault, wantDefault, higher,
					v.Parameter != "", err)
			}
		}
	}
	var unknown *int
	if err = s.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT cargo_fin.settings_rank('fin.F7','NOPE')`).Scan(&unknown)
	}); err != nil || unknown != nil {
		t.Errorf("an unknown value has rank %v (%v)", unknown, err)
	}
}

// A command is stamped once it holds the tenant's lock, so commands are stamped in the order they commit: a proposal
// and an approval that waited for the lock are stamped after it was released, the approval after the proposal.
func TestFinSettingsCommandsAreStampedAfterTheyHoldTheLock(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("base", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES"))) // waits for approval
	errs := s.queuedSettings(
		func() error {
			_, err := s.propose("c1", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS"))
			return err
		},
		func() error { _, err := s.approve("base", "bob"); return err })
	if ok, _ := pairOf(errs); ok != 2 {
		t.Fatalf("two commands: %v", errs)
	}
	h := s.changes()
	released := s.released.Truncate(time.Microsecond)
	if h["c1"].ProposedAt.Before(released) {
		t.Fatalf("the proposal is stamped %v, before the lock was released at %v: it was read before the wait", h["c1"].ProposedAt, released)
	}
	if h["base"].ApprovedAt.Before(h["c1"].ProposedAt) || h["base"].ApprovedAt.Before(released) {
		t.Fatalf("the approval is stamped %v, before the proposal %v or the release %v", h["base"].ApprovedAt, h["c1"].ProposedAt, released)
	}
}

// The database writes history forwards and ties the recorded times to its own clock: a row cannot be back-dated, nor
// set in the future beyond a minute, nor recorded before the latest time of the tenant (the reviewer's cases), and an
// effective time is within 366 days.
func TestFinSettingsDatabaseTiesTheTimesToItsOwnClock(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("L1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
	s.must(s.propose("L2", "ann", time.Time{}, leEntry("LE2", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
	back := func(id, at string) []string {
		return []string{fmt.Sprintf(`INSERT INTO cargo_fin.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
          VALUES ($1,'%s','r','mallory',%s,%s,false)`, id, at, at), rawEntry(id, 1, "TENANT", "", "fin.F2", "SUBMITTER_AND_PREVIEWER", "NULL"),
			fmt.Sprintf(`INSERT INTO cargo_fin.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,approved_at,version_no,
              in_force_at,auto) VALUES ($1,'%s','mallory',%s,%s,false,'mallory',%s,%s,%s,true)`, id, at, at, at, nextVersion, at)}
	}
	for _, c := range []struct {
		name, constraint string
		stmts            []string
	}{
		{"the reviewer's back-dated change set (47 hours ago)", "settings_time_window", back("B1", "now() - interval '47 hours'")},
		{"a change set set in the future by two minutes", "settings_time_window", back("B2", "now() + interval '2 minutes'")},
		{"a cancellation two minutes ahead", "settings_time_window", []string{rawCancellation("L1", "now() + interval '2 minutes'")}},
		{"an effective time more than 366 days after the proposal", "settings_change_effective_limit", []string{rawChange("B3", "ann", "now() + interval '367 days'", false)}},
	} {
		err := s.rawTx(t, c.stmts...)
		if state, constraint := sqlState(err); err == nil || state != "23514" || constraint != c.constraint {
			t.Errorf("%s: %v (state %s, constraint %s), want %s", c.name, err, state, constraint, c.constraint)
		}
	}
	if err := s.rawTx(t, rawChange("B4", "ann", "now() + interval '366 days'", false), rawEntry("B4", 1, "TENANT", "", "fin.F2", "SUBMITTER_AND_PREVIEWER", "NULL")); err != nil {
		t.Errorf("an effective time of exactly 366 days: %v", err)
	}
	// the reviewer's stale-clock case: the approval stamped 30 seconds ahead is recorded, then a cancellation stamped
	// now is before it (it would have been "cancelled before it was approved")
	if err := s.rawTx(t, rawApproval("L1", "bob", "proposed_at + interval '30 seconds'", nextVersion, "greatest(effective_at, proposed_at + interval '30 seconds')", false)); err != nil {
		t.Fatalf("an approval stamped 30 seconds ahead: %v", err)
	}
	for _, c := range []struct {
		name, constraint string
		stmts            []string
	}{
		{"a cancellation stamped before the latest approval", "settings_time_monotone", []string{rawCancellation("L2", "now()")}},
		{"a proposal stamped before the latest approval", "settings_time_monotone", []string{rawChange("B5", "ann", "now()", false), rawEntry("B5", 1, "TENANT", "", "fin.F2", "SUBMITTER_AND_PREVIEWER", "NULL")}},
		{"an approval stamped before the latest approval", "settings_time_monotone", []string{rawApproval("L2", "bob", "proposed_at", nextVersion, "effective_at", false)}},
	} {
		err := s.rawTx(t, c.stmts...)
		if state, constraint := sqlState(err); err == nil || state != "23514" || constraint != c.constraint {
			t.Errorf("%s: %v (state %s, constraint %s), want %s", c.name, err, state, constraint, c.constraint)
		}
	}
	// the commands then answer the same: a command stamped before that time is refused, and one after it is accepted
	// once the clock has passed it
	if _, err := s.cancel("L2", "eve"); err == nil || !strings.Contains(err.Error(), "SETTINGS_CLOCK") || !strings.Contains(err.Error(), "try the command again") {
		t.Errorf("a command stamped before the latest record: %v", err)
	}
}

// A company tightening leaves a legal entity's own, looser value in force as an approved exception: the settings show
// it as an exception with the change set that set it, the proposal that tightens the company names the legal entity,
// and a replay answers the same notice.
func TestFinSettingsAnApprovedExceptionIsShownAndAnnounced(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("le", "ann", time.Time{}, leEntry("LE", "fin.F2", "SUBMITTER_ONLY"), leEntry("LE", "fin.F7", "DOES_NOT_BLOCK")))
	got, _ := s.settings("LEGAL_ENTITY", "LE", time.Time{})
	if got["fin.F2"].Exception || got["fin.F2"].ChangeID != "le" || got["fin.F2"].Default {
		t.Fatalf("a value as loose as the company's: %+v", got["fin.F2"])
	}
	cmd := api.ProposeFinSettingsChange{CommandID: "co-1", TenantID: s.tenant, ActorID: "ann", ChangeID: "co", Reason: "tighten",
		Entries: []api.FinSettingEntry{tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER")}}
	r, err := s.module.ProposeFinSettingsChange(context.Background(), cmd)
	if err != nil || r.Version == 0 {
		t.Fatalf("the company tightens: %+v, %v", r, err)
	}
	for _, want := range []string{"LE keeps its own fin.F2 SUBMITTER_ONLY", "change set le", "looser than the company's SUBMITTER_AND_PREVIEWER", "propose a change set for that legal entity"} {
		if !strings.Contains(r.Notice, want) {
			t.Errorf("the notice does not say %q: %q", want, r.Notice)
		}
	}
	if strings.Contains(r.Notice, "fin.F7") {
		t.Errorf("the notice names an item the change set did not touch: %q", r.Notice)
	}
	if again, err := s.module.ProposeFinSettingsChange(context.Background(), cmd); err != nil || again != r {
		t.Fatalf("a replay answers the same receipt and notice: %+v, %v", again, err)
	}
	got, _ = s.settings("LEGAL_ENTITY", "LE", time.Time{})
	if f2 := got["fin.F2"]; f2.Value != "SUBMITTER_ONLY" || !f2.Exception || f2.ChangeID != "le" || f2.FromScopeKind != "LEGAL_ENTITY" {
		t.Fatalf("the legal entity's own value: %+v", f2)
	}
	if got["fin.F7"].Exception {
		t.Fatalf("F7 is as loose as the company's: %+v", got["fin.F7"])
	}
	if got, _ = s.settings("LEGAL_ENTITY", "LE2", time.Time{}); got["fin.F2"].Exception || got["fin.F2"].Value != "SUBMITTER_AND_PREVIEWER" {
		t.Fatalf("a legal entity without a value of its own: %+v", got["fin.F2"])
	}
	if got, _ = s.settings("TENANT", "", time.Time{}); got["fin.F2"].Exception {
		t.Fatalf("the company: %+v", got["fin.F2"])
	}
	// a change set with no company entry has no notice, whoever stays looser: LE2's own tightening says nothing of LE's
	// exception
	if r = s.must(s.propose("le2", "ann", time.Time{}, leEntry("LE2", "fin.F2", "SUBMITTER_AND_PREVIEWER"))); r.Notice != "" {
		t.Fatalf("a legal entity's own change set has a notice: %q", r.Notice)
	}
	// a proposal with no exception left has no notice
	if r = s.must(s.propose("co2", "ann", time.Time{}, tenantEntry("fin.F3", "ONLY_WHEN_AMOUNT_MOVES"))); r.Notice != "" {
		// F3 loosens: it waits, and no legal entity holds F3
		t.Fatalf("a notice with nothing to say: %q", r.Notice)
	}
}

// Change sets take effect in the order of their versions: the reviewer's case, a change set in force now proposed after
// one in force in two hours, is refused with the way out, and the version in force never goes back in time.
func TestFinSettingsChangeSetsTakeEffectInTheOrderOfTheirVersions(t *testing.T) {
	s := newSettingsWorld(t)
	later := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	s.must(s.propose("c1", "ann", later, tenantEntry("fin.F7", "BLOCKS")))
	_, err := s.propose("c2", "ann", time.Time{}, tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER"))
	s.refused("a change set in force now after one in force in two hours", err, api.ErrRejected, "SETTINGS_ORDER", "c1", "at or after", "cancel change set c1")
	if r, err := s.propose("c2", "ann", later, tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER")); err != nil || r.Version != 2 {
		t.Fatalf("at the same time: %+v, %v", r, err)
	}
	prev := 0
	for h := 0; h <= 4; h++ {
		_, v := s.settings("TENANT", "", time.Now().UTC().Add(time.Duration(h)*time.Hour))
		if v < prev {
			t.Fatalf("the version in force went from %d back to %d at +%d hours", prev, v, h)
		}
		prev = v
	}
}

// Two identical commands started together while the lock is held: the second waits and answers the first's receipt, as
// a replay does, and nothing is written twice.
func TestFinSettingsTwoIdenticalCommandsTogetherAnswerOneReceipt(t *testing.T) {
	s := newSettingsWorld(t)
	propose := api.ProposeFinSettingsChange{CommandID: "same-p", TenantID: s.tenant, ActorID: "ann", ChangeID: "c1", Reason: "r",
		Entries: []api.FinSettingEntry{leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")}}
	var receipts [2]api.Receipt
	errs := s.queuedSettings(
		func() (err error) {
			receipts[0], err = s.module.ProposeFinSettingsChange(context.Background(), propose)
			return
		},
		func() (err error) {
			receipts[1], err = s.module.ProposeFinSettingsChange(context.Background(), propose)
			return
		})
	if errs[0] != nil || errs[1] != nil || receipts[0] != receipts[1] || receipts[0].SubjectID != "c1" || s.count("settings_change") != 1 {
		t.Fatalf("two identical proposals: %v, %+v, %d change sets", errs, receipts, s.count("settings_change"))
	}
	approve := api.ApproveFinSettingsChange{CommandID: "same-a", TenantID: s.tenant, ActorID: "bob", ChangeID: "c1"}
	errs = s.queuedSettings(
		func() (err error) {
			receipts[0], err = s.module.ApproveFinSettingsChange(context.Background(), approve)
			return
		},
		func() (err error) {
			receipts[1], err = s.module.ApproveFinSettingsChange(context.Background(), approve)
			return
		})
	if errs[0] != nil || errs[1] != nil || receipts[0] != receipts[1] || receipts[0].Version != 1 || s.count("settings_approval") != 1 {
		t.Fatalf("two identical approvals: %v, %+v, %d approvals", errs, receipts, s.count("settings_approval"))
	}
	// the same command id with another payload is still refused
	other := approve
	other.ChangeID = "c2"
	if _, err := s.module.ApproveFinSettingsChange(context.Background(), other); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the command id with another payload: %v", err)
	}
}

// An effective time more than 366 days away, and one in the year 10000, are refused with the way out, not by an
// unclassified error.
func TestFinSettingsAnEffectiveTimeFarAheadIsRefused(t *testing.T) {
	s := newSettingsWorld(t)
	for _, at := range []time.Time{time.Now().UTC().Add(367 * 24 * time.Hour), time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(20000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		_, err := s.propose("far", "ann", at, tenantEntry("fin.F7", "BLOCKS"))
		s.refused(at.Format("2006"), err, api.ErrRejected, "SETTINGS_EFFECTIVE_TIME", "nearer time")
	}
	if n := s.count("settings_change"); n != 0 {
		t.Fatalf("%d change sets written", n)
	}
	if r, err := s.propose("near", "ann", time.Now().UTC().Add(300*24*time.Hour), tenantEntry("fin.F7", "BLOCKS")); err != nil || r.Version != 1 {
		t.Fatalf("300 days ahead: %+v, %v", r, err)
	}
}

// An approver refuses a proposal that waits for approval, with a reason: it is recorded as a cancellation of kind
// REFUSED, by the approver's permission; it can no longer be approved, an approved change set is cancelled instead, the
// database refuses a refusal of an approved one, and the event is its own.
func TestFinSettingsAnApproverRefusesAProposal(t *testing.T) {
	s := newSettingsWorld(t)
	ctx := context.Background()
	s.must(s.propose("w1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
	s.must(s.propose("w2", "ann", time.Time{}, leEntry("LE2", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
	refuse := api.RefuseFinSettingsChange{CommandID: "ref-1", TenantID: s.tenant, ActorID: "bob", ChangeID: "w1", Reason: "the audit has not signed it"}
	if _, err := s.module.RefuseFinSettingsChange(ctx, api.RefuseFinSettingsChange{CommandID: "ref-x", TenantID: s.tenant, ActorID: "ann", ChangeID: "w1", Reason: "r"}); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("someone who may not approve refuses: %v", err)
	}
	r, err := s.module.RefuseFinSettingsChange(ctx, refuse)
	if err != nil || r.NewRevision != 2 || r.SubjectID != "w1" {
		t.Fatalf("a refusal: %+v, %v", r, err)
	}
	if again, err := s.module.RefuseFinSettingsChange(ctx, refuse); err != nil || again != r {
		t.Fatalf("a replay of the refusal: %+v, %v", again, err)
	}
	c := s.changes()["w1"]
	if c.Status != "REFUSED" || c.CancelledBy != "bob" || c.CancelReason != "the audit has not signed it" || c.Version != 0 {
		t.Fatalf("a refused change set: %+v", c)
	}
	_, err = s.approve("w1", "carl")
	s.refused("approving a refused change set", err, api.ErrRejected, "propose it again")
	_, err = s.cancel("w1", "ann")
	s.refused("cancelling a refused change set", err, api.ErrRejected, "nothing more to do")
	_, err = s.module.RefuseFinSettingsChange(ctx, api.RefuseFinSettingsChange{CommandID: "ref-2", TenantID: s.tenant, ActorID: "bob", ChangeID: "w1", Reason: "again"})
	s.refused("refusing twice", err, api.ErrRejected, "nothing more to do")
	_, err = s.module.RefuseFinSettingsChange(ctx, api.RefuseFinSettingsChange{CommandID: "ref-3", TenantID: s.tenant, ActorID: "bob", ChangeID: "nope", Reason: "r"})
	s.refused("refusing an unknown change set", err, api.ErrNotFound, "ListFinSettingsChanges")
	s.must(s.approve("w2", "bob"))
	_, err = s.module.RefuseFinSettingsChange(ctx, api.RefuseFinSettingsChange{CommandID: "ref-4", TenantID: s.tenant, ActorID: "bob", ChangeID: "w2", Reason: "late"})
	s.refused("refusing an approved change set", err, api.ErrRejected, "CancelFinSettingsChange")
	_, err = s.module.RefuseFinSettingsChange(ctx, api.RefuseFinSettingsChange{CommandID: "ref-5", TenantID: s.tenant, ActorID: "bob", ChangeID: "w2", Reason: " "})
	s.refused("a refusal without a reason", err, api.ErrRejected)
	if !contains(s.outboxTypes(t), "FinSettingsChangeRefused") {
		t.Fatalf("the events: %v", s.outboxTypes(t))
	}
	// the database: kind REFUSED is only for a change set not approved
	err = s.rawTx(t, `INSERT INTO cargo_fin.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason,kind) VALUES ($1,'w2','x',now(),'r','REFUSED')`)
	if state, constraint := sqlState(err); err == nil || state != "23514" || constraint != "settings_refusal_approved" {
		t.Errorf("a refusal of an approved change set: %v (%s %s)", err, state, constraint)
	}
	err = s.rawTx(t, `INSERT INTO cargo_fin.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason,kind) VALUES ($1,'w2','x',now(),'r','OTHER')`)
	if state, _ := sqlState(err); err == nil || state != "23514" {
		t.Errorf("a cancellation of another kind: %v", err)
	}
}

func contains(list []string, want string) bool {
	for _, x := range list {
		if x == want {
			return true
		}
	}
	return false
}

// The immutability triggers refuse UPDATE and DELETE even to the owner of the tables, who holds the privileges the
// application role lacks.
func TestFinSettingsTheOwnerCannotChangeOrDeleteARow(t *testing.T) {
	dsn := os.Getenv("IDP_CARGO_MIGRATOR_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("IDP_CARGO_MIGRATOR_DSN required in CI; skipping forbidden")
		}
		t.Skip("set IDP_CARGO_MIGRATOR_DSN to the owner role")
	}
	s := newSettingsWorld(t)
	s.must(s.propose("c1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
	s.must(s.approve("c1", "bob"))
	s.must(s.propose("c2", "ann", time.Time{}, leEntry("LE2", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
	s.must(s.cancel("c2", "eve"))
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, table := range []string{"settings_change", "settings_change_entry", "settings_approval", "settings_cancellation"} {
		for _, stmt := range []string{`UPDATE cargo_fin.` + table + ` SET tenant_id = tenant_id WHERE tenant_id = $1`, `DELETE FROM cargo_fin.` + table + ` WHERE tenant_id = $1`} {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, s.tenant)
			if err == nil {
				_, err = tx.Exec(ctx, stmt, s.tenant)
			}
			state, _ := sqlState(err)
			if state != "23001" || err == nil || !strings.Contains(err.Error(), "rows are immutable") {
				t.Errorf("%s as the owner: %v (state %s)", stmt, err, state)
			}
			_ = tx.Rollback(ctx)
		}
	}
}

// The database's length and pattern checks, each at its limit and one over: they hold for a raw write as the commands'
// own limits hold for a command.
func TestFinSettingsDatabaseLengthAndPatternChecks(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("L1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
	rep := func(n int) string { return strings.Repeat("x", n) }
	change := func(id, reason, by string) []string {
		return []string{fmt.Sprintf(`INSERT INTO cargo_fin.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
          VALUES ($1,'%s','%s','%s',now(),now(),false)`, id, reason, by), rawEntry(id, 1, "TENANT", "", "fin.F2", "SUBMITTER_AND_PREVIEWER", "NULL")}
	}
	type raw struct {
		name, constraint  string
		okStmts, badStmts []string
	}
	approve := func(by string) []string {
		return []string{fmt.Sprintf(`INSERT INTO cargo_fin.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,approved_at,
          version_no,in_force_at,auto) SELECT tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,'%s',proposed_at,%s,effective_at,false
          FROM cargo_fin.settings_change WHERE tenant_id=$1 AND change_id='L1'`, by, nextVersion)}
	}
	cancel := func(by string) []string {
		return []string{fmt.Sprintf(`INSERT INTO cargo_fin.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason) VALUES ($1,'L1','%s',now(),'r')`, by)}
	}
	for _, c := range []raw{
		{"a change set's reason", "settings_change_reason_check", change("K1", rep(500), "ann"), change("K1", rep(501), "ann")},
		{"a change set's proposer", "settings_change_proposed_by_check", change("K1", "r", rep(128)), change("K1", "r", rep(129))},
		{"a change set's identifier", "settings_change_change_id_check", change("a"+rep(127), "r", "ann"), change("a"+rep(128), "r", "ann")},
		{"an identifier with a blank", "settings_change_change_id_check", change("a-b", "r", "ann"), change("a b", "r", "ann")},
		{"an identifier that starts with a dash", "settings_change_change_id_check", change("a-", "r", "ann"), change("-a", "r", "ann")},
		{"an entry's legal entity", "settings_entry_scope", []string{rawChange("K1", "ann", "now()", false), rawEntry("K1", 1, "LEGAL_ENTITY", rep(128), "fin.F2", "SUBMITTER_ONLY", "NULL")},
			[]string{rawChange("K1", "ann", "now()", false), rawEntry("K1", 1, "LEGAL_ENTITY", rep(129), "fin.F2", "SUBMITTER_ONLY", "NULL")}},
		{"an entry's number", "settings_change_entry_entry_no_check", []string{rawChange("K1", "ann", "now()", false), rawEntry("K1", 64, "TENANT", "", "fin.F2", "SUBMITTER_ONLY", "NULL")},
			[]string{rawChange("K1", "ann", "now()", false), rawEntry("K1", 65, "TENANT", "", "fin.F2", "SUBMITTER_ONLY", "NULL")}},
		{"an approver", "settings_approval_approved_by_check", approve(rep(128)), approve(rep(129))},
		{"a canceller", "settings_cancellation_cancelled_by_check", cancel(rep(128)), cancel(rep(129))},
		{"a blank canceller", "settings_cancellation_cancelled_by_check", cancel("a"), cancel(" ")},
		{"a blank proposer", "settings_change_proposed_by_check", change("K1", "r", "a"), change("K1", "r", " ")},
		{"a blank reason", "settings_change_reason_check", change("K1", "r", "ann"), change("K1", " ", "ann")},
	} {
		// the largest is accepted (the transaction is rolled back at once, and entries need no change set to be
		// complete)
		err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, stmt := range c.okStmts {
				if _, err := e.Exec(ctx, stmt, s.tenant); err != nil {
					return err
				}
			}
			return errRollBack
		})
		if !errors.Is(err, errRollBack) {
			t.Errorf("%s at its limit: %v", c.name, err)
		}
		err = s.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, stmt := range c.badStmts {
				if _, err := e.Exec(ctx, stmt, s.tenant); err != nil {
					return err
				}
			}
			return errRollBack
		})
		if state, constraint := sqlState(err); err == nil || errors.Is(err, errRollBack) || state != "23514" || constraint != c.constraint {
			t.Errorf("%s one over: %v (state %s, constraint %s), want %s", c.name, err, state, constraint, c.constraint)
		}
	}
	// the blank tenant of a change set
	err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `INSERT INTO cargo_fin.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening) VALUES (' ','K','r','a',now(),now(),false)`)
		return err
	})
	if state, _ := sqlState(err); err == nil || (state != "23514" && state != "42501") {
		t.Errorf("a blank tenant: %v", err)
	}
}

// The ledger's wording of a refusal reaches the caller through the commands: an unknown change set says where to list
// them.
func TestFinSettingsRefusalsSayTheWayOutThroughTheCommands(t *testing.T) {
	s := newSettingsWorld(t)
	_, err := s.approve("nope", "bob")
	s.refused("approve unknown", err, api.ErrNotFound, "ListFinSettingsChanges")
	_, err = s.cancel("nope", "ann")
	s.refused("cancel unknown", err, api.ErrNotFound, "ListFinSettingsChanges")
	s.must(s.propose("c1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
	s.must(s.approve("c1", "bob"))
	_, err = s.approve("c1", "carl")
	s.refused("approve twice", err, api.ErrRejected, "nothing more to do")
}

// The database measures a change set against the values in force at its effective time: the latest entry of a scope in
// force by then, by the time in force and then the version, leaving out change sets cancelled and change sets that take
// effect later (called directly on a change set written in a transaction that is rolled back).
func TestFinSettingsDatabaseMeasuresAgainstTheValuesInForce(t *testing.T) {
	s := newSettingsWorld(t)
	hour := func(n int) time.Time {
		return time.Now().UTC().Add(time.Duration(n) * time.Hour).Truncate(time.Microsecond)
	}
	s.must(s.propose("h1", "ann", time.Time{}, tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER"))) // strict, in force now
	s.must(s.propose("h2", "ann", hour(1), tenantEntry("fin.F2", "SUBMITTER_ONLY")))              // loose, in force in an hour
	s.must(s.approve("h2", "bob"))
	s.must(s.propose("h3", "ann", hour(2), tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER"))) // strict again, in two hours
	s.must(s.propose("h4", "ann", hour(3), tenantEntry("fin.F7", "BLOCKS")))                  // two change sets in force together
	s.must(s.propose("h5", "ann", hour(3), tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.approve("h5", "bob"))
	s.must(s.propose("h6", "ann", hour(4), tenantEntry("fin.F6", "INCLUDED_MARKED"))) // approved, then cancelled
	s.must(s.approve("h6", "bob"))
	s.must(s.cancel("h6", "eve"))
	loosens := func(effective string, entries ...string) bool {
		t.Helper()
		var got bool
		err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, stmt := range append([]string{rawChange("Q", "mallory", effective, false)}, entries...) {
				if _, err := e.Exec(ctx, stmt, s.tenant); err != nil {
					return err
				}
			}
			if err := e.QueryRow(ctx, `SELECT cargo_fin.settings_loosening($1,'Q')`, s.tenant).Scan(&got); err != nil {
				return err
			}
			return errRollBack
		})
		if !errors.Is(err, errRollBack) {
			t.Fatal(err)
		}
		return got
	}
	f2 := func(value string) string { return rawEntry("Q", 1, "TENANT", "", "fin.F2", value, "NULL") }
	// at once: h1's strict value is the one in force
	if !loosens("now()", f2("SUBMITTER_ONLY")) || loosens("now()", f2("SUBMITTER_AND_PREVIEWER")) {
		t.Error("at once: the value in force is h1's, SUBMITTER_AND_PREVIEWER")
	}
	// in ninety minutes: h2's loose value is in force, which h3 (in two hours) does not change yet
	if loosens("now() + interval '90 minutes'", f2("SUBMITTER_ONLY")) {
		t.Error("in 90 minutes: SUBMITTER_ONLY is in force (the latest by time, and one still to come is not)")
	}
	// at the very moment h2 takes effect it is in force
	var inForce2 time.Time
	if err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT in_force_at FROM cargo_fin.settings_approval WHERE tenant_id=$1 AND change_id='h2'`, s.tenant).Scan(&inForce2)
	}); err != nil {
		t.Fatal(err)
	}
	if loosens(fmt.Sprintf("'%s'::timestamptz", inForce2.Format(time.RFC3339Nano)), f2("SUBMITTER_ONLY")) {
		t.Error("at the moment h2 takes effect: it is in force")
	}
	// in two and a half hours h3's strict value is in force
	if !loosens("now() + interval '150 minutes'", f2("SUBMITTER_ONLY")) {
		t.Error("in 150 minutes: h3's strict value is in force")
	}
	// two change sets in force at the same time: the later version is the one in force
	f7 := func(value string) string { return rawEntry("Q", 1, "TENANT", "", "fin.F7", value, "NULL") }
	if loosens("now() + interval '200 minutes'", f7("DOES_NOT_BLOCK")) {
		t.Error("at the time h4 and h5 are both in force: h5's DOES_NOT_BLOCK, the later version, is in force")
	}
	// a cancelled change set is not in force
	f6 := rawEntry("Q", 1, "TENANT", "", "fin.F6", "INCLUDED_MARKED", "NULL")
	if !loosens("now() + interval '5 hours'", f6) {
		t.Error("h6 was cancelled: F6 is at its default, so INCLUDED_MARKED loosens it")
	}
}

// The database's own boundaries that the other raw-write tests do not reach: a tie of times in force is broken by the
// version; the latest time recorded counts proposals and cancellations too; a raw proposal takes the tenant's lock; the
// kind of a cancellation, the receipt's notice and a blank tenant have their checks.
func TestFinSettingsDatabaseRemainingBoundaries(t *testing.T) {
	t.Run("a tie of times in force is broken by the version", func(t *testing.T) {
		s := newSettingsWorld(t)
		at := time.Now().Add(time.Hour)
		s.must(s.propose("A", "ann", at, tenantEntry("fin.F9", "RECONFIRM")))  // tightens: approved at once, version 1
		s.must(s.propose("B", "ann", at, tenantEntry("fin.F9", "KEEP_RATIO"))) // loosens against A at that time: waits
		s.must(s.approve("B", "bob"))                                          // version 2, in force at the same time as A
		if _, err := s.propose("C", "ann", at, tenantEntry("fin.F9", "KEEP_RATIO")); err != nil {
			t.Fatalf("the value in force at that time is the later version's, so keeping it is not a loosening: %v", err)
		}
	})
	t.Run("the latest time recorded counts proposals", func(t *testing.T) {
		s := newSettingsWorld(t)
		s.must(s.propose("L1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
		s.must(s.propose("L2", "ann", time.Time{}, leEntry("LE2", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
		at := "proposed_at + interval '1 microsecond'"
		err := s.rawTx(t, rawApproval("L1", "bob", at, nextVersion, "greatest(effective_at, "+at+")", false))
		if state, c := sqlState(err); err == nil || state != "23514" || c != "settings_time_monotone" {
			t.Fatalf("an approval before the latest proposal: %v (%s, %s)", err, state, c)
		}
	})
	t.Run("the latest time recorded counts cancellations", func(t *testing.T) {
		s := newSettingsWorld(t)
		s.must(s.propose("L1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
		s.must(s.propose("L2", "ann", time.Time{}, leEntry("LE2", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
		s.must(s.cancel("L2", "ann"))
		at := "(SELECT proposed_at FROM cargo_fin.settings_change WHERE tenant_id=$1 AND change_id='L2') + interval '1 microsecond'"
		err := s.rawTx(t, rawApproval("L1", "bob", at, nextVersion, "greatest(effective_at, "+at+")", false))
		if state, c := sqlState(err); err == nil || state != "23514" || c != "settings_time_monotone" {
			t.Fatalf("an approval before the latest cancellation: %v (%s, %s)", err, state, c)
		}
	})
	t.Run("a raw proposal waits for the tenant's lock", func(t *testing.T) {
		s := newSettingsWorld(t)
		pid, release := holdSettings(t, s.tenant)
		done := make(chan error, 1)
		go func() {
			done <- s.rawTx(t, rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "TENANT", "", "fin.F2", "SUBMITTER_AND_PREVIEWER", "NULL"))
		}()
		waitBlockedBy(t, pid, 1)
		release()
		if err, returned := returnsWithin(done, 10*time.Second); !returned || err != nil {
			t.Fatalf("the raw proposal after the lock was released: %v (returned %v)", err, returned)
		}
	})
	t.Run("the kind of a cancellation", func(t *testing.T) {
		s := newSettingsWorld(t)
		s.must(s.propose("L1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
		err := s.rawTx(t, `INSERT INTO cargo_fin.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason,kind) VALUES ($1,'L1','x',now(),'r','OTHER')`)
		if state, c := sqlState(err); err == nil || state != "23514" || c != "settings_cancellation_kind_check" {
			t.Fatalf("an unknown kind: %v (%s, %s)", err, state, c)
		}
	})
	t.Run("the role of a cancellation", func(t *testing.T) {
		s := newSettingsWorld(t)
		s.must(s.propose("L1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
		for _, c := range []struct{ name, stmt, constraint string }{
			{"an unknown role", `INSERT INTO cargo_fin.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason,cancelled_role) VALUES ($1,'L1','x',now(),'r','ADMIN')`, "settings_cancellation_role"},
			{"a refusal by a configurer", `INSERT INTO cargo_fin.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason,kind,cancelled_role) VALUES ($1,'L1','x',now(),'r','REFUSED','CONFIGURER')`, "settings_refusal_by_approver"},
		} {
			if state, k := sqlState(s.rawTx(t, c.stmt)); state != "23514" || k != c.constraint {
				t.Errorf("%s: %s, %s", c.name, state, k)
			}
		}
		if err := s.rawTx(t, `INSERT INTO cargo_fin.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason,kind,cancelled_role) VALUES ($1,'L1','x',now(),'r','REFUSED','APPROVER')`); err != nil {
			t.Errorf("a refusal by an approver: %v", err)
		}
	})
	t.Run("the receipt's notice", func(t *testing.T) {
		s := newSettingsWorld(t)
		receipt := func(notice string) error {
			return s.within(t, func(ctx context.Context, e bentopg.Executor) error {
				if _, err := e.Exec(ctx, `INSERT INTO cargo_fin.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision,result_notice)
                  VALUES ($1,'raw-notice',repeat('a',64),'FinSettings','x',1,$2)`, s.tenant, notice); err != nil {
					return err
				}
				return errRollBack
			})
		}
		if err := receipt(strings.Repeat("n", 4000)); !errors.Is(err, errRollBack) {
			t.Errorf("a notice of 4000 characters: %v", err)
		}
		for _, bad := range []string{"", strings.Repeat("n", 4001)} {
			if state, c := sqlState(receipt(bad)); state != "23514" || c != "command_receipt_notice" {
				t.Errorf("a notice of %d characters: %s, %s", len(bad), state, c)
			}
		}
	})
	t.Run("a blank tenant", func(t *testing.T) {
		s := newSettingsWorld(t)
		err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
			if _, err := e.Exec(ctx, `SELECT set_config('app.tenant_id',' ',true)`); err != nil {
				return err
			}
			_, err := e.Exec(ctx, `INSERT INTO cargo_fin.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening) VALUES (' ','K','r','a',now(),now(),false)`)
			return err
		})
		if state, c := sqlState(err); state != "23514" || c != "settings_change_tenant_id_check" {
			t.Fatalf("a blank tenant: %v (%s, %s)", err, state, c)
		}
	})
}

// The reviewer's case of round 2: a configurer's change set approved for a time far ahead stands in the way of every later
// one; the way out the refusals name, to cancel it, is open to an approver, whose role is recorded.
func TestFinSettingsAnApproverCancelsAFarOffChangeSetThatStandsInTheWay(t *testing.T) {
	s := newSettingsWorld(t)
	far := time.Now().Add(300 * 24 * time.Hour)
	s.must(s.propose("future", "eve", far, tenantEntry("fin.F7", "BLOCKS"))) // tightens: approved on the spot, version 1
	s.must(s.propose("L1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
	_, err := s.propose("urgent", "eve", time.Time{}, tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER"))
	s.refused("an urgent tightening behind the far-off one", err, api.ErrRejected, "SETTINGS_ORDER", "cancel change set future first", "ApproveSettings")
	_, err = s.approve("L1", "bob")
	s.refused("the approval of a loosening behind it", err, api.ErrRejected, "SETTINGS_ORDER", "ConfigureSettings or of ApproveSettings")
	if _, err = s.module.CancelFinSettingsChange(context.Background(), api.CancelFinSettingsChange{CommandID: s.command(), TenantID: s.tenant, ActorID: "zed",
		ChangeID: "future", Reason: "in the way"}); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("someone with neither permission cancels: %v", err)
	}
	if _, err = s.module.CancelFinSettingsChange(context.Background(), api.CancelFinSettingsChange{CommandID: s.command(), TenantID: s.tenant, ActorID: "bob",
		ChangeID: "future", Reason: "it blocks every later change"}); err != nil {
		t.Fatalf("an approver cancels an approved change set not yet in force: %v", err)
	}
	h := s.changes()
	if c := h["future"]; c.Status != "CANCELLED" || c.CancelledBy != "bob" || c.CancelledRole != "APPROVER" || c.CancelReason != "it blocks every later change" {
		t.Fatalf("the cancellation: %+v", c)
	}
	s.must(s.approve("L1", "bob"))
	s.must(s.propose("urgent", "eve", time.Time{}, tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER")))
	if _, err = s.module.CancelFinSettingsChange(context.Background(), api.CancelFinSettingsChange{CommandID: s.command(), TenantID: s.tenant, ActorID: "bob",
		ChangeID: "L1", Reason: "too late"}); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "in force") {
		t.Fatalf("an approver cancels a change set in force: %v", err)
	}
	s.must(s.propose("L2", "ann", time.Time{}, leEntry("LE2", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
	s.must(s.cancel("L2", "eve"))
	if c := s.changes()["L2"]; c.CancelledRole != "CONFIGURER" {
		t.Fatalf("a configurer's cancellation: %+v", c)
	}
}

// The database's refusal of a time outside its window is answered as a named refusal with the way out, whichever command
// meets it, when this server's clock is 90 seconds from the database's; 30 seconds off is accepted.
func TestFinSettingsAClockOffByMoreThanAMinuteIsRefusedWithItsWayOut(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("L1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
	skewed := func(d time.Duration) *settingsWorld {
		w := *s
		w.module = settingsModule(t, s.fixture, finmodule.WithClock(offsetClock{d}))
		return &w
	}
	skewCase := func(what string, err error) {
		t.Helper()
		s.refused(what, err, api.ErrRejected, "SETTINGS_CLOCK_SKEW", "differ by more than a minute", "synchronise the servers' clocks", "try the command again")
	}
	w := skewed(90 * time.Second)
	_, err := w.propose("S1", "ann", time.Time{}, tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER"))
	skewCase("a proposal with the clock 90 seconds ahead", err)
	// behind, in a tenant with nothing recorded yet (with rows recorded the ledger's own rule, that history is written
	// forwards, answers first)
	fresh := newSettingsWorld(t)
	fresh.module = settingsModule(t, fresh.fixture, finmodule.WithClock(offsetClock{-90 * time.Second}))
	_, err = fresh.propose("S1", "ann", time.Time{}, tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER"))
	skewCase("a proposal with the clock 90 seconds behind", err)
	_, err = w.approve("L1", "bob")
	skewCase("an approval", err)
	_, err = w.cancel("L1", "ann")
	skewCase("a cancellation", err)
	_, err = w.module.RefuseFinSettingsChange(context.Background(), api.RefuseFinSettingsChange{CommandID: s.command(), TenantID: s.tenant, ActorID: "bob", ChangeID: "L1", Reason: "no"})
	skewCase("a refusal", err)
	if h := s.changes(); len(h) != 1 || h["L1"].Status != "PROPOSED" {
		t.Fatalf("nothing was written: %+v", h)
	}
	if _, err = skewed(30*time.Second).propose("S2", "ann", time.Time{}, tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER")); err != nil {
		t.Fatalf("a clock 30 seconds off: %v", err)
	}
}

type offsetClock struct{ d time.Duration }

func (c offsetClock) Now() time.Time { return time.Now().UTC().Add(c.d) }

// The window around the database's clock is one minute: 58 seconds either way is accepted, 62 refused, for a proposal and
// a cancellation (the approval's time is the same check).
func TestFinSettingsTheTimeWindowIsOneMinuteWide(t *testing.T) {
	s := newSettingsWorld(t) // nothing recorded yet, so that a time in the past is not before the latest
	w := newSettingsWorld(t)
	w.must(w.propose("L1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
	proposal := func(at string) []string {
		return []string{fmt.Sprintf(`INSERT INTO cargo_fin.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
          VALUES ($1,'W1','r','ann',%s,%s,false)`, at, at), rawEntry("W1", 1, "TENANT", "", "fin.F2", "SUBMITTER_AND_PREVIEWER", "NULL")}
	}
	try := func(world *settingsWorld, stmts []string) error {
		return world.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, stmt := range stmts {
				if _, err := e.Exec(ctx, stmt, world.tenant); err != nil {
					return err
				}
			}
			return errRollBack
		})
	}
	for _, c := range []struct {
		name  string
		world *settingsWorld
		stmts []string
		ok    bool
	}{
		{"a proposal 58 seconds ago", s, proposal("now() - interval '58 seconds'"), true},
		{"a proposal 62 seconds ago", s, proposal("now() - interval '62 seconds'"), false},
		{"a proposal 58 seconds ahead", s, proposal("now() + interval '58 seconds'"), true},
		{"a proposal 62 seconds ahead", s, proposal("now() + interval '62 seconds'"), false},
		{"a cancellation 58 seconds ahead", w, []string{rawCancellation("L1", "now() + interval '58 seconds'")}, true},
		{"a cancellation 62 seconds ahead", w, []string{rawCancellation("L1", "now() + interval '62 seconds'")}, false},
	} {
		err := try(c.world, c.stmts)
		if state, constraint := sqlState(err); c.ok != errors.Is(err, errRollBack) || (!c.ok && (state != "23514" || constraint != "settings_time_window")) {
			t.Errorf("%s: %v (%s, %s)", c.name, err, state, constraint)
		}
	}
}
