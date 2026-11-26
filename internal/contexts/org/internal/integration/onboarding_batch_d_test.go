//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
)

// V2.102 batch D, migration org/000012: what the proposal stores (the plan), the owner's declaration of the single
// administrator, the platform's own record of a module, the limit of the overrides and the confirmation's lock.

func (w *onboardingWorld) proposeWith(id string, c api.ProposeOnboarding) (api.OnboardingReceipt, error) {
	c.CommandID, c.TenantID, c.ActorID, c.OnboardingID = "p-"+id, w.tenant, "ann", id
	if c.Profile == "" {
		c.Profile, c.RiskStatement, c.FinanceStaff = "LEAN", riskText, 3
	}
	if c.Plan == nil {
		c.Plan = plan()
	}
	return w.module.ProposeOnboarding(context.Background(), c)
}

// The proposal keeps the plan: what will be applied in each module, with the module's catalogue version; the receipt's
// onboarding shows it; a proposal with no plan, or an incomplete one, is refused with the way out; the same plan is a replay
// and another plan under the same id is not.
func TestTheProposalKeepsThePlanOfEachModule(t *testing.T) {
	w := newOnboardingWorld(t)
	if _, err := w.propose("ob-1", "ann", "LEAN", 3, false); err != nil {
		t.Fatal(err)
	}
	o := w.get("ob-1")
	fin, sea := o.Plan["FIN"], o.Plan["SEA"]
	if len(o.Plan) != 2 || fin.CatalogueVersion != "fin-v1" || len(fin.Entries) != 1 || fin.Entries[0].Key != "fin.F7" || fin.Entries[0].Value != "BLOCKS" ||
		sea.CatalogueVersion != "sea-v1" || len(sea.Entries) != 1 || sea.Entries[0].Key != "sea.S1" {
		t.Fatalf("the plan: %+v", o.Plan)
	}
	_, err := w.proposeWith("ob-2", api.ProposeOnboarding{Plan: map[string]api.ModulePlan{"FIN": plan()["FIN"]}})
	said(t, "a plan without SEA", err, api.ErrRejected, "ORG_INVALID_ONBOARDING", "the plan names the catalogue version of SEA", "propose through")
	_, err = w.proposeWith("ob-2", api.ProposeOnboarding{Plan: map[string]api.ModulePlan{"FIN": plan()["FIN"], "SEA": {CatalogueVersion: "v"}}})
	said(t, "a plan of SEA with no entries", err, api.ErrRejected, "the plan of SEA is 1 to 64 entries")
	_, err = w.proposeWith("ob-2", api.ProposeOnboarding{Plan: map[string]api.ModulePlan{"FIN": plan()["FIN"], "SEA": plan()["SEA"], "AIR": plan()["SEA"]}})
	said(t, "a plan of another module", err, api.ErrRejected, "the plan is for FIN and SEA")
	var many []api.OnboardingEntry
	for i := 0; i < 65; i++ {
		many = append(many, api.OnboardingEntry{ScopeKind: "TENANT", Key: fmt.Sprintf("fin.F%d", i), Value: "X"})
	}
	_, err = w.proposeWith("ob-2", api.ProposeOnboarding{Plan: plan(many...)})
	said(t, "a plan of 65 entries", err, api.ErrRejected, "the plan of FIN is 1 to 64 entries")
	_, err = w.proposeWith("ob-2", api.ProposeOnboarding{Plan: plan(many[:64]...)})
	if err != nil {
		t.Fatalf("a plan of 64 entries: %v", err)
	}
	// batch E: the request's hash covers what the caller sent, not what the build resolved, so the same proposal with the same command id
	// and another plan (a deploy between a try and its retry) answers the stored record; the same onboarding under another command id,
	// or with another content, is still refused
	other := plan(api.OnboardingEntry{ScopeKind: "TENANT", Key: "fin.F7", Value: "DOES_NOT_BLOCK"})
	again, err := w.proposeWith("ob-1", api.ProposeOnboarding{Profile: "LEAN", RiskStatement: riskText, FinanceStaff: 3, Plan: other})
	if err != nil || !reflect.DeepEqual(again.Plan, o.Plan) || again.PlanSHA256 != o.PlanSHA256 || again.PlanSHA256 != api.PlanSHA256(o.Plan) {
		t.Fatalf("the same proposal with another plan under the same command id: %+v, %v; want the stored plan %+v", again, err, o.Plan)
	}
	_, err = w.module.ProposeOnboarding(context.Background(), api.ProposeOnboarding{CommandID: "p-other", TenantID: w.tenant, ActorID: "ann", OnboardingID: "ob-1", Profile: "LEAN",
		RiskStatement: riskText, FinanceStaff: 3, Plan: other})
	said(t, "another plan under the same id and another command id", err, api.ErrIdempotencyConflict, "propose with another onboarding id")
	_, err = w.proposeWith("ob-1", api.ProposeOnboarding{Profile: "STRICT", RiskStatement: riskText, FinanceStaff: 3, Plan: other})
	said(t, "another profile under the same command id", err, api.ErrIdempotencyConflict, "propose with another onboarding id")
}

// The overrides of a module are at most 64 entries: 64 are kept, 65 refused with the way out.
func TestTheOverridesAreAtMost64Entries(t *testing.T) {
	w := newOnboardingWorld(t)
	entries := func(n int) []api.OnboardingEntry {
		var out []api.OnboardingEntry
		for i := 0; i < n; i++ {
			out = append(out, api.OnboardingEntry{ScopeKind: "LEGAL_ENTITY", ScopeID: fmt.Sprintf("LE-%d", i), Key: "fin.F7", Value: "BLOCKS"})
		}
		return out
	}
	if _, err := w.proposeWith("ob-64", api.ProposeOnboarding{Overrides: map[string][]api.OnboardingEntry{"FIN": entries(64)}}); err != nil {
		t.Fatalf("64 overrides: %v", err)
	}
	_, err := w.proposeWith("ob-65", api.ProposeOnboarding{Overrides: map[string][]api.OnboardingEntry{"FIN": entries(65)}})
	said(t, "65 overrides", err, api.ErrRejected, "ORG_INVALID_ONBOARDING", "at most 64 entries", "leave the rest to the module's own settings commands")
}

// The owner declares the single administrator when confirming: named if and only if the proposal says the customer
// declares one, not the consultant, kept in the confirmation; a retry must say the same.
func TestTheOwnerNamesTheSingleAdministratorWhenConfirming(t *testing.T) {
	w := newOnboardingWorld(t)
	if _, err := w.propose("single", "ann", "LEAN", 3, true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.propose("none", "ann", "LEAN", 3, false); err != nil {
		t.Fatal(err)
	}
	_, err := w.confirmNaming(w.command(), "single", "olga", riskText, "")
	said(t, "none named for a proposal that declares one", err, api.ErrRejected, "names them when confirming", "AdministratorID", "declares none")
	_, err = w.confirmNaming(w.command(), "single", "olga", riskText, "  ")
	said(t, "a blank name", err, api.ErrRejected, "names them when confirming")
	_, err = w.confirmNaming(w.command(), "single", "olga", riskText, "ann")
	said(t, "the consultant", err, api.ErrRejected, "is not the consultant ann", "name the customer's own administrator")
	_, err = w.confirmNaming(w.command(), "single", "olga", riskText, " dan")
	said(t, "spaces around the name", err, api.ErrRejected, "without spaces around it")
	_, err = w.confirmNaming(w.command(), "single", "olga", riskText, strings.Repeat("x", 129))
	said(t, "129 bytes", err, api.ErrRejected, "1 to 128 bytes")
	_, err = w.confirmNaming(w.command(), "none", "olga", riskText, "dan")
	said(t, "a name for a proposal that declares none", err, api.ErrRejected, "does not say the customer declares a single administrator", "leave AdministratorID empty")
	if w.get("single").State != "PROPOSED" || w.get("none").State != "PROPOSED" {
		t.Fatal("a refused confirmation changed an onboarding")
	}
	if _, err = w.confirmNaming("conf-single", "single", "olga", riskText, "olga"); err != nil { // the owner may name themselves
		t.Fatalf("the owner naming themselves: %v", err)
	}
	if o := w.get("single"); o.AdministratorID != "olga" || o.ConfirmedBy != "olga" {
		t.Fatalf("the record: %+v", o)
	}
	if _, err = w.confirmNaming("retry", "single", "olga", riskText, "olga"); err != nil {
		t.Fatalf("a retry saying the same: %v", err)
	}
	_, err = w.confirmNaming("retry-2", "single", "olga", riskText, "dan")
	said(t, "a retry naming another administrator", err, api.ErrIdempotencyConflict, "confirmed by olga already")
	if _, err = w.confirmNaming("conf-none", "none", "olga", riskText, ""); err != nil {
		t.Fatal(err)
	}
	if o := w.get("none"); o.AdministratorID != "" {
		t.Fatalf("an administrator kept where none was declared: %+v", o)
	}
}

// holdOnboarding takes the onboarding's lock on a second connection, inside a transaction, and answers its backend pid.
func holdOnboarding(t *testing.T, tenant, id string) (pid int, release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cargo_org.onboarding/' || $1::text || '/' || $2::text, 0))`, tenant, id); err == nil {
		err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	}
	if err != nil {
		t.Fatal(err)
	}
	var once bool
	release = func() {
		if !once {
			once = true
			tx.Commit(ctx)
			conn.Close(ctx)
		}
	}
	t.Cleanup(release)
	return pid, release
}

// waitBlocked waits until n sessions are blocked, directly or through others, by the holder (pg_blocking_pids).
func waitBlocked(t *testing.T, holder, n int) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	deadline := time.Now().Add(15 * time.Second)
	var blocked int
	for time.Now().Before(deadline) {
		if err = conn.QueryRow(ctx, `WITH RECURSIVE chain(pid) AS (
            SELECT $1::int
            UNION
            SELECT a.pid FROM pg_stat_activity a JOIN chain c ON c.pid = ANY(pg_blocking_pids(a.pid)))
          SELECT count(*) - 1 FROM chain`, holder).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d sessions are blocked by the lock holder, want %d", blocked, n)
}

// Two confirmations of one onboarding at once take turns on the onboarding's lock. The lock is held on a second
// connection; the confirmations are started one after another, each only once the sessions before it are blocked by the
// holder (pg_blocking_pids); when it is released the first started is the one that confirms and the other is refused,
// naming the first.
func TestTwoConfirmationsOfOneOnboardingTakeTurns(t *testing.T) {
	w := newOnboardingWorld(t)
	if _, err := w.propose("ob-1", "ann", "LEAN", 3, false); err != nil {
		t.Fatal(err)
	}
	pid, release := holdOnboarding(t, w.tenant, "ob-1")
	var errs [2]error
	var wg sync.WaitGroup
	for i, who := range []string{"olga", "dan"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = w.confirm("conf-"+who, "ob-1", who, riskText)
		}()
		waitBlocked(t, pid, i+1)
	}
	if o := w.get("ob-1"); o.State != "PROPOSED" { // nothing was written while the lock was held
		t.Fatalf("a confirmation went past the lock: %+v", o)
	}
	release()
	wg.Wait()
	if errs[0] != nil {
		t.Fatalf("the first started: %v", errs[0])
	}
	said(t, "the second started", errs[1], api.ErrIdempotencyConflict, "confirmed by olga already")
	if o := w.get("ob-1"); o.ConfirmedBy != "olga" {
		t.Fatalf("confirmed by %s", o.ConfirmedBy)
	}
}
