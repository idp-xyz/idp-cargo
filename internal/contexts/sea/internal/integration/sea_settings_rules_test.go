//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// The database's rules of V2.100 (migration 000012), written past SEA's commands on the application role.

func rawChange(id, by, effective string, loosening bool) string {
	return fmt.Sprintf(`INSERT INTO cargo_sea.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
      VALUES ($1,'%s','a reason','%s',now(),%s,%v)`, id, by, effective, loosening)
}

func rawEntry(id string, no int, kind, scopeID, key, value string) string {
	return fmt.Sprintf(`INSERT INTO cargo_sea.settings_change_entry (tenant_id,change_id,entry_no,scope_kind,scope_id,item_key,value_id)
      VALUES ($1,'%s',%d,'%s','%s','%s','%s')`, id, no, kind, scopeID, key, value)
}

// rawApproval approves the change set as the columns of its row say, at and inForce being SQL over proposed_at and
// effective_at.
func rawApproval(id, by, at string, version any, inForce string, auto bool) string {
	return fmt.Sprintf(`INSERT INTO cargo_sea.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,approved_at,
        version_no,in_force_at,auto)
      SELECT tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,'%s',%s,%v,%s,%v FROM cargo_sea.settings_change
      WHERE tenant_id=$1 AND change_id='%s'`, by, at, version, inForce, auto, id)
}

func rawCancellation(id, at string) string {
	return fmt.Sprintf(`INSERT INTO cargo_sea.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason) VALUES ($1,'%s','x',%s,'a reason')`, id, at)
}

const nextVersion = `(SELECT coalesce(max(version_no),0)+1 FROM cargo_sea.settings_approval WHERE tenant_id=$1)`

// constraintOf is the name of the constraint a database refusal names.
func constraintOf(err error) string {
	got := refusal(err)
	if i := strings.LastIndex(got, "["); i >= 0 {
		return strings.TrimSuffix(got[i+1:], "]")
	}
	return ""
}

// What the database refuses past SEA's commands on the application role, each by its name: an entry's scope, item and
// value, an effective time before the proposal, a change set without entries or with a gap in their numbers or whose
// entry comes later, an approval by the proposer or an automatic one by another, an automatic one of a change set that
// loosens, not at the proposal, or not with its change set, an approval before the proposal or not taking effect at the
// later of its time and the effective time, an approval after a cancellation, a cancellation of a change set in force
// or before it was proposed, a second approval or cancellation, a version twice or skipped, a change of any row,
// another tenant's rows, and a write at REPEATABLE READ. Each rule is shown on a change set the commands made, so that
// what is refused is the rule and not something else.
func TestSeaSettingsDatabaseRulesRefuseRawWrites(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY"))) // tightening, approved at once: version 1
	s.must(s.propose("L1", "ann", time.Time{}, leEntry("LE", "WITH_REASON")))   // loosening, waits
	s.must(s.propose("L2", "ann", time.Time{}, leEntry("LE2", "WITH_REASON")))  // loosening, waits
	if err := s.rawTx(t, rawChange("R3", "ann", "now()", false), rawEntry("R3", 1, "LEGAL_ENTITY", "LE", "sea.S1", "SUPERVISOR_ONLY")); err != nil {
		t.Fatal(err) // a change set with no approval, to show an approval not written with it
	}
	if err := s.rawTx(t, rawCancellation("L2", "now()")); err != nil {
		t.Fatal(err) // L2 is cancelled
	}
	s.must(s.propose("L3", "ann", time.Time{}, leEntry("LE2", "WITH_REASON"))) // an approved exception: LE2's own value, looser than the company's
	s.must(s.approve("L3", "bob"))
	later := "proposed_at + interval '1 minute'"
	type rule struct {
		name       string
		stmts      []string
		state      string
		constraint string
	}
	chg := func(id string, loosening bool) []string {
		return []string{rawChange(id, "ann", "now()", loosening), rawEntry(id, 1, "LEGAL_ENTITY", "LE", "sea.S1", "SUPERVISOR_ONLY")}
	}
	with := func(base []string, more ...string) []string { return append(slices.Clone(base), more...) }
	for _, c := range []rule{
		{"the company has no ID", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "TENANT", "x", "sea.S1", "SUPERVISOR_ONLY")}, "23514", "settings_entry_scope"},
		{"a legal entity needs its ID", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "LEGAL_ENTITY", "", "sea.S1", "SUPERVISOR_ONLY")}, "23514", "settings_entry_scope"},
		{"another kind of scope", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "BRANCH", "LE/B", "sea.S1", "SUPERVISOR_ONLY")}, "23514", "settings_entry_scope"},
		{"a value the item does not have", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "TENANT", "", "sea.S1", "NOPE")}, "23514", "settings_entry_value_known"},
		{"another item's value", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "TENANT", "", "sea.S1", "BLOCKS")}, "23514", "settings_entry_value_known"},
		{"an item of no catalogue", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "TENANT", "", "sea.S2", "WITH_REASON")}, "23514", "settings_entry_value_known"},
		{"an item of FIN's catalogue", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "TENANT", "", "fin.F7", "BLOCKS")}, "23514", "settings_entry_value_known"},
		{"an effective time before the proposal", []string{rawChange("R1", "ann", "now() - interval '1 second'", false)}, "23514", "settings_change_effective"},
		{"an effective time more than 366 days after", []string{rawChange("R1", "ann", "now() + interval '367 days'", false)}, "23514", "settings_change_effective_limit"},
		{"a change set without entries", []string{rawChange("R1", "ann", "now()", false)}, "23514", "settings_change_entries"},
		{"entries that skip a number", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 2, "TENANT", "", "sea.S1", "SUPERVISOR_ONLY")}, "23514", "settings_change_entries"},
		{"an entry written after its change set", []string{rawEntry("R3", 2, "LEGAL_ENTITY", "LE2", "sea.S1", "SUPERVISOR_ONLY")}, "23514", "settings_entry_with_change"},
		{"an entry twice for one scope and item", with(chg("R1", false), rawEntry("R1", 2, "LEGAL_ENTITY", "LE", "sea.S1", "SUPERVISOR_ONLY")), "23505", "settings_entry_once"},
		// the database recomputes loosening from the order of the values, and holds the flag to its answer
		{"a loosening stored as not loosening", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "LEGAL_ENTITY", "LE", "sea.S1", "WITH_REASON")}, "23514", "settings_change_loosening_flag"},
		{"a tightening stored as loosening", []string{rawChange("R1", "ann", "now()", true), rawEntry("R1", 1, "LEGAL_ENTITY", "LE", "sea.S1", "SUPERVISOR_ONLY")}, "23514", "settings_change_loosening_flag"},
		{"a company loosening stored as not loosening", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "TENANT", "", "sea.S1", "WITH_REASON")}, "23514", "settings_change_loosening_flag"},
		{"a legal entity looser than the company's own entry of the same change set, stored as not loosening", []string{rawChange("R1", "ann", "now()", false),
			rawEntry("R1", 1, "TENANT", "", "sea.S1", "SUPERVISOR_ONLY"), rawEntry("R1", 2, "LEGAL_ENTITY", "LE", "sea.S1", "WITH_REASON")}, "23514", "settings_change_loosening_flag"},
		{"a legal entity's own value kept, looser than the company's, stored as not loosening", []string{rawChange("R1", "ann", "now()", false),
			rawEntry("R1", 1, "LEGAL_ENTITY", "LE2", "sea.S1", "WITH_REASON")}, "23514", "settings_change_loosening_flag"},
		{"an automatic approval of a change set that loosens, stored as not loosening", []string{rawChange("R1", "ann", "now()", false),
			rawEntry("R1", 1, "LEGAL_ENTITY", "LE", "sea.S1", "WITH_REASON"), rawApproval("R1", "ann", "proposed_at", nextVersion, "effective_at", true)}, "23514", "settings_approval_loosening"},
		{"approval by the proposer", []string{rawApproval("L1", "ann", "proposed_at", 91, "effective_at", false)}, "23514", "settings_approval_other_actor"},
		{"an automatic approval by another", with(chg("R4", false), rawApproval("R4", "bob", "proposed_at", 91, "effective_at", true)), "23514", "settings_approval_other_actor"},
		{"an automatic approval of a change set that loosens", with(chg("R4", true), rawApproval("R4", "ann", "proposed_at", 91, "effective_at", true)), "23514", "settings_approval_tightening_only"},
		{"an automatic approval later than the proposal", with(chg("R4", false), rawApproval("R4", "ann", later, 91, later, true)), "23514", "settings_approval_automatic_at_proposal"},
		{"an automatic approval not written with its change set", []string{rawApproval("R3", "ann", "proposed_at", 91, "effective_at", true)}, "23514", "settings_approval_with_change"},
		{"an approval before the proposal", []string{rawApproval("L1", "bob", "proposed_at - interval '1 hour'", 91, "effective_at", false)}, "23514", "settings_approval_after_proposal"},
		{"an approval that does not take effect when it should", []string{rawApproval("L1", "bob", later, 91, "proposed_at", false)}, "23514", "settings_approval_in_force"},
		{"an approval of a cancelled change set", []string{rawApproval("L2", "bob", later, 91, "greatest(effective_at, "+later+")", false)}, "23514", "settings_approval_cancelled"},
		{"a second approval", []string{rawApproval("T1", "bob", later, 91, "greatest(effective_at, "+later+")", false)}, "23505", "settings_approval_pkey"},
		{"a version twice", []string{rawApproval("L1", "bob", later, 1, "greatest(effective_at, "+later+")", false)}, "23505", "settings_approval_version_once"},
		{"a version skipped", []string{rawApproval("L1", "bob", later, 9, "greatest(effective_at, "+later+")", false)}, "23514", "settings_versions_consistent"},
		{"an approval naming another proposer", []string{`INSERT INTO cargo_sea.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,approved_at,
          version_no,in_force_at,auto) SELECT tenant_id,change_id,'zed',proposed_at,effective_at,loosening,'bob',proposed_at,91,effective_at,false
          FROM cargo_sea.settings_change WHERE tenant_id=$1 AND change_id='L1'`}, "23503", "settings_approval_matches_change"},
		{"a cancellation of a change set in force", []string{rawCancellation("T1", "now()")}, "23514", "settings_cancellation_in_force"},
		{"a cancellation before the proposal", []string{rawCancellation("L1", "now() - interval '2 days'")}, "23514", "settings_cancellation_after_proposal"},
		{"a second cancellation", []string{rawCancellation("L2", "now()")}, "23505", "settings_cancellation_pkey"},
		{"a cancellation at the moment the change set takes effect", with(chg("R6", true), rawApproval("R6", "bob", "proposed_at", nextVersion, "proposed_at", false),
			rawCancellation("R6", "now()")), "23514", "settings_cancellation_in_force"},
		{"a cancellation of a change set that is not there", []string{rawCancellation("NOPE", "now()")}, "23503", ""},
		{"a refusal of a change set approved", []string{`INSERT INTO cargo_sea.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason,kind,cancelled_role)
          VALUES ($1,'T1','x',now(),'r','REFUSED','APPROVER')`}, "23514", "settings_refusal_approved"},
		{"a refusal by a configurer", []string{`INSERT INTO cargo_sea.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason,kind,cancelled_role)
          VALUES ($1,'L1','x',now(),'r','REFUSED','CONFIGURER')`}, "23514", "settings_refusal_by_approver"},
		{"a cancellation in no role", []string{`INSERT INTO cargo_sea.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason,cancelled_role)
          VALUES ($1,'L1','x',now(),'r','ADMIN')`}, "23514", "settings_cancellation_role"},
		{"a cancellation of no kind", []string{`INSERT INTO cargo_sea.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason,kind)
          VALUES ($1,'L1','x',now(),'r','DROPPED')`}, "23514", "settings_cancellation_kind_check"},
		{"a change set without a reason", []string{`INSERT INTO cargo_sea.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
          VALUES ($1,'R7',' ','ann',now(),now(),false)`}, "23514", "settings_change_reason_check"},
		{"a change set without a proposer", []string{`INSERT INTO cargo_sea.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
          VALUES ($1,'R7','r',' ',now(),now(),false)`}, "23514", "settings_change_proposed_by_check"},
		{"a cancellation without a reason", []string{`INSERT INTO cargo_sea.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason)
          VALUES ($1,'L1','x',now(),' ')`}, "23514", "settings_cancellation_reason_check"},
		// the recorded times: within a minute of the database's clock, and not before the latest recorded for the
		// tenant
		{"a proposal back-dated by more than a minute", []string{`INSERT INTO cargo_sea.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
          VALUES ($1,'R8','r','ann',now() - interval '2 minutes',now() - interval '2 minutes',false)`, rawEntry("R8", 1, "TENANT", "", "sea.S1", "SUPERVISOR_ONLY")}, "23514", "settings_time_window"},
		{"a proposal dated more than a minute ahead", []string{`INSERT INTO cargo_sea.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
          VALUES ($1,'R8','r','ann',now() + interval '2 minutes',now() + interval '2 minutes',false)`, rawEntry("R8", 1, "TENANT", "", "sea.S1", "SUPERVISOR_ONLY")}, "23514", "settings_time_window"},
		{"a proposal before the latest time recorded for the tenant", []string{`INSERT INTO cargo_sea.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
          VALUES ($1,'R8','r','ann',now() - interval '30 seconds',now() - interval '30 seconds',false)`, rawEntry("R8", 1, "TENANT", "", "sea.S1", "SUPERVISOR_ONLY")}, "23514", "settings_time_monotone"},
		{"a cancellation dated more than a minute ahead", []string{`INSERT INTO cargo_sea.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason)
          VALUES ($1,'L1','x',now() + interval '2 minutes','r')`}, "23514", "settings_time_window"},
		{"another tenant's change set", []string{`INSERT INTO cargo_sea.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
          VALUES ($1 || '-other','X','r','ann',now(),now(),false)`}, "42501", ""},
	} {
		err := s.rawTx(t, c.stmts...)
		state, constraint := sqlState(err), constraintOf(err)
		if err == nil || state != c.state || (c.constraint != "" && constraint != c.constraint) {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", c.name, err, state, constraint, c.state, c.constraint)
		}
	}
	// a change set written as SEA writes it is accepted, so that what was refused above was the rule: a tightening with
	// its automatic approval
	if err := s.rawTx(t, with(chg("OK1", false), rawApproval("OK1", "ann", "proposed_at", nextVersion, "effective_at", true))...); err != nil {
		t.Errorf("a change set written as SEA writes it: %v", err)
	}
	// the rows are never changed or deleted, whatever the table
	for _, table := range []string{"settings_change", "settings_change_entry", "settings_approval", "settings_cancellation"} {
		for _, stmt := range []string{`UPDATE cargo_sea.` + table + ` SET tenant_id = tenant_id WHERE tenant_id = $1`, `DELETE FROM cargo_sea.` + table + ` WHERE tenant_id = $1`} {
			if err := s.rawTx(t, stmt); err == nil {
				t.Errorf("%s: no refusal", stmt)
			} else if state := sqlState(err); state != "23001" && state != "42501" {
				t.Errorf("%s: %v (state %s)", stmt, err, state)
			}
		}
	}
	// nothing the refusals tried is left, and the change sets the commands made are as they were
	if n := s.count("settings_change"); n != 6 {
		t.Errorf("%d change sets, want T1, L1, L2, R3, L3 and OK1", n)
	}
	if n := s.count("settings_approval"); n != 3 {
		t.Errorf("%d approvals, want T1's, L3's and OK1's", n)
	}
	// row security is enabled and forced, and hides another tenant's rows
	for _, table := range []string{"settings_change", "settings_change_entry", "settings_approval", "settings_cancellation"} {
		var enabled, forced bool
		var others int
		err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
			if err := e.QueryRow(ctx, `SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
                WHERE n.nspname = 'cargo_sea' AND c.relname = $1`, table).Scan(&enabled, &forced); err != nil {
				return err
			}
			return e.QueryRow(ctx, `SELECT count(*) FROM cargo_sea.`+table+` WHERE tenant_id <> $1`, s.tenant).Scan(&others)
		})
		if err != nil || !enabled || !forced || others != 0 {
			t.Errorf("%s: row security enabled %v, forced %v, other tenants' rows seen %d, %v", table, enabled, forced, others, err)
		}
	}
	// a write at REPEATABLE READ is refused before it runs, on each table
	conn, err := pgx.Connect(context.Background(), os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	for _, table := range []string{"settings_change", "settings_change_entry", "settings_approval", "settings_cancellation"} {
		tx, err := conn.BeginTx(context.Background(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id',$1,true)`, s.tenant)
		if err == nil {
			_, err = tx.Exec(context.Background(), `INSERT INTO cargo_sea.`+table+` (tenant_id) VALUES ($1)`, s.tenant)
		}
		if state, constraint := sqlState(err), constraintOf(err); state != "23514" || constraint != "sea_read_committed" {
			t.Errorf("%s at REPEATABLE READ: %v (state %s, constraint %s)", table, err, state, constraint)
		}
		_ = tx.Rollback(context.Background())
	}
}

var errRollBack = errors.New("roll back")

// Every value of SEA's catalogue is accepted by the database's list of values, at the company and at a legal entity:
// the list in the migration and the catalogue in the code agree.
func TestSeaSettingsDatabaseKnowsEveryValueOfTheCatalogue(t *testing.T) {
	s := newSettingsWorld(t)
	cat, err := s.module.GetSeaSettingsCatalogue(context.Background(), api.GetSeaSettingsCatalogue{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, it := range cat.Items {
		for _, v := range it.Values {
			n++
			id := fmt.Sprintf("V%d", n)
			for _, scope := range []struct{ kind, id string }{{"TENANT", ""}, {"LEGAL_ENTITY", "LE"}} {
				err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
					for _, stmt := range []string{rawChange(id, "ann", "now()", false), rawEntry(id, 1, scope.kind, scope.id, it.Key, v.ID)} {
						if _, err := e.Exec(ctx, stmt, s.tenant); err != nil {
							return err
						}
					}
					return errRollBack
				})
				if !errors.Is(err, errRollBack) {
					t.Errorf("%s %s at %s: %v", it.Key, v.ID, scope.kind, err)
				}
			}
		}
	}
	if n != 2 {
		t.Errorf("%d values in the catalogue, the migration lists 2", n)
	}
}

// The database keeps "not both" by itself, without the application's lock, as the rows are written: a cancellation not
// yet committed makes a later approval wait for it and then be refused, and an approval not yet committed makes a
// cancellation wait and then be refused when the change set is in force. The first connection holds the change set's
// lock by writing its row; the second is shown waiting for it.
func TestSeaSettingsDatabaseOrdersAnApprovalAndACancellation(t *testing.T) {
	for _, c := range []struct {
		name           string
		holder, second func(id string) string
		constraint     string
	}{
		{"a cancellation, then an approval", func(id string) string { return rawCancellation(id, "now()") },
			func(id string) string {
				return rawApproval(id, "bob", "proposed_at + interval '1 minute'", nextVersion, "greatest(effective_at, proposed_at + interval '1 minute')", false)
			}, "settings_approval_cancelled"},
		{"an approval, then a cancellation", func(id string) string {
			return rawApproval(id, "bob", "now()", nextVersion, "greatest(effective_at, now())", false)
		}, func(id string) string { return rawCancellation(id, "now() + interval '1 minute'") }, "settings_cancellation_in_force"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSettingsWorld(t)
			s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
			s.must(s.propose("L1", "ann", time.Time{}, leEntry("LE", "WITH_REASON")))
			ctx := context.Background()
			conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var pid int
			if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, s.tenant); err == nil {
				_, err = tx.Exec(ctx, c.holder("L1"), s.tenant)
			}
			if err == nil {
				err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
			}
			if err != nil {
				t.Fatalf("the first write: %v", err)
			}
			watch, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
			if err != nil {
				t.Fatal(err)
			}
			defer watch.Close(ctx)
			done := make(chan error, 1)
			go func() { done <- s.rawTx(t, c.second("L1")) }()
			blocked := func() (n int) {
				if err := watch.QueryRow(ctx, blockedByHolder, pid).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			for i := 0; blocked() < 1; i++ {
				select {
				case err := <-done:
					t.Fatalf("the second write returned (%v) while the first was not committed", err)
				case <-time.After(25 * time.Millisecond):
				}
				if i > 1200 {
					t.Fatal("the second write is not waiting")
				}
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatalf("committing the first write: %v", err)
			}
			err = result(t, "the second write", done)
			if state, constraint := sqlState(err), constraintOf(err); state != "23514" || constraint != c.constraint {
				t.Fatalf("the second write: %v (state %s, constraint %s), want %s", err, state, constraint, c.constraint)
			}
		})
	}
}

// The columns a release records: the version of SEA's settings and the value of S1 together or neither, the version not
// below 0, the value one of the two; a release written before V2.100 has neither; and the receipts of the settings
// commands, whose result type and columns the database holds to each other.
func TestSeaSettingsDatabaseRulesOfTheReleaseAndTheReceipt(t *testing.T) {
	f := newReworkFixture(t)
	f.day(t)
	ok(t)(f.carrierFact("load-fact", "OM-1", f.get(t, "OM-1").Revision, api.CarrierLoaded, "CSQU3054383", "", "LD-9", 4, 4))
	release := func(columns, values string) string {
		return fmt.Sprintf(`INSERT INTO cargo_sea.carrier_conflict_release (tenant_id,visit_id,release_no,fact_no,phy_seal,phy_sealed_at,reason,recorded_by,recorded_at,entered_at%s)
        VALUES ($1,'CV-1',1,1,'S2','2026-01-01T00:00:00Z','checked','x',now(),now()%s)`, columns, values)
	}
	for name, tc := range map[string]struct {
		columns, values string
		constraint      string
	}{
		"a version without a value":  {",settings_version", ",3", "carrier_conflict_release_settings_together"},
		"a value without a version":  {",s1_value", ",'WITH_REASON'", "carrier_conflict_release_settings_together"},
		"a negative version":         {",settings_version,s1_value", ",-1,'WITH_REASON'", "carrier_conflict_release_settings_version"},
		"a value of S1 there is not": {",settings_version,s1_value", ",1,'MAYBE'", "carrier_conflict_release_s1_value"},
		"a value of another item":    {",settings_version,s1_value", ",1,'BLOCKS'", "carrier_conflict_release_s1_value"},
		"a value in lower case":      {",settings_version,s1_value", ",1,'with_reason'", "carrier_conflict_release_s1_value"},
	} {
		err := f.rawTx(t, release(tc.columns, tc.values), stepVisit("CV-1"))
		if sqlState(err) != "23514" || constraintOf(err) != tc.constraint {
			t.Errorf("%s: %v (%s), want %s", name, err, refusal(err), tc.constraint)
		}
	}
	// accepted: neither, as a release of V2.99 has, the version 0 with SUPERVISOR_ONLY and the version 5 with
	// WITH_REASON, each a release of the same report against another seal, numbered in turn
	for i, tc := range []struct{ columns, values string }{
		{"", ""}, {",settings_version,s1_value", ",0,'SUPERVISOR_ONLY'"}, {",settings_version,s1_value", ",5,'WITH_REASON'"},
	} {
		stmt := fmt.Sprintf(`INSERT INTO cargo_sea.carrier_conflict_release (tenant_id,visit_id,release_no,fact_no,phy_seal,phy_sealed_at,reason,recorded_by,recorded_at,entered_at%s)
        VALUES ($1,'CV-1',%d,1,'S%d','2026-01-01T00:00:00Z','checked','x',now(),now()%s)`, tc.columns, i+1, i+2, tc.values)
		if err := f.rawTx(t, stmt, stepVisit("CV-1")); err != nil {
			t.Errorf("a release with %q: %v", tc.values, err)
		}
	}
	got := f.visit(t, "CV-1")
	if len(got.Releases) != 3 || got.Releases[2].S1 != "" || got.Releases[2].SettingsVersion != 0 || got.Releases[1].S1 != "SUPERVISOR_ONLY" || got.Releases[1].SettingsVersion != 0 ||
		got.Releases[0].S1 != "WITH_REASON" || got.Releases[0].SettingsVersion != 5 {
		t.Errorf("the releases as the visit reads them (latest first): %+v", got.Releases)
	}
	// the receipts: the result type is known, the settings' columns go with it only
	receipt := func(kind, columns, values string) string {
		return fmt.Sprintf(`INSERT INTO cargo_sea.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision%s)
        VALUES ($1,'raw-%s-%s',repeat('a',64),'%s','x',1%s)`, columns, kind, strings.ReplaceAll(columns, ",", ""), kind, values)
	}
	for name, tc := range map[string]struct {
		kind, columns, values string
		state, constraint     string
	}{
		"a result type there is not":          {"Unknown", "", "", "23514", "command_receipt_result_type_check"},
		"FIN's result type":                   {"FinSettings", "", "", "23514", "command_receipt_result_type_check"},
		"a version on a receipt of a visit":   {"ContainerVisit", ",result_settings_version", ",1", "23514", "command_receipt_settings_of_settings"},
		"a notice on a receipt of a movement": {"OceanMovement", ",result_notice", ",'n'", "23514", "command_receipt_settings_of_settings"},
		"a version of 0":                      {"SeaSettings", ",result_settings_version", ",0", "23514", "command_receipt_settings_version"},
		"a notice of nothing":                 {"SeaSettings", ",result_notice", ",''", "23514", "command_receipt_notice"},
		"a notice of 4,001 characters":        {"SeaSettings", ",result_notice", ",repeat('n',4001)", "23514", "command_receipt_notice"},
	} {
		err := f.rawTx(t, receipt(tc.kind, tc.columns, tc.values))
		if sqlState(err) != tc.state || constraintOf(err) != tc.constraint {
			t.Errorf("%s: %v (%s), want %s %s", name, err, refusal(err), tc.state, tc.constraint)
		}
	}
	for name, tc := range map[string]struct{ columns, values string }{
		"a receipt of the settings with a version and a notice": {",result_settings_version,result_notice", ",3,'a notice'"},
		"a receipt of the settings with none":                   {"", ""},
	} {
		if err := f.rawTx(t, receipt("SeaSettings", tc.columns, tc.values)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A proposal written past SEA takes the tenant's settings lock by itself, as the commands do: while the lock is held the
// insert waits (shown by pg_blocking_pids from the test's own holder), and goes through when it is released.
func TestSeaSettingsADatabaseProposalWaitsForTheSettingsLock(t *testing.T) {
	s := newSettingsWorld(t)
	h := holdSettings(t, s.tenant)
	results := map[string]chan error{}
	h.start("raw proposal", func() error {
		return s.rawTx(t, rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "TENANT", "", "sea.S1", "WITH_REASON"))
	}, results)
	h.release()
	if err := result(t, "raw proposal", results["raw proposal"]); err != nil {
		t.Fatal(err)
	}
	if s.count("settings_change") != 1 {
		t.Fatal("the proposal was not written")
	}
}
