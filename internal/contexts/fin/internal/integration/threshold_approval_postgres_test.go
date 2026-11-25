//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// FIN V2.100: the cost variance threshold (E1) under approval, through FIN's commands on PostgreSQL. creditorWorld has
// version 1 (CNY 100.00 or 1%), approved by ctrl-1 because the first version is looser than none.

type thresholds struct {
	ap
	n int
}

func (th *thresholds) policy() api.FinancePolicy {
	th.t.Helper()
	p, err := th.module.GetFinancePolicy(th.ctx, api.GetFinancePolicy{TenantID: th.tenant, LegalEntityID: "LE"})
	if err != nil {
		th.t.Fatal(err)
	}
	return p
}

func (th *thresholds) setCmd(actor string, amount int64, rate int) api.SetCostVarianceThreshold {
	th.n++
	return api.SetCostVarianceThreshold{CommandID: fmt.Sprintf("thr-set-%d", th.n), TenantID: th.tenant, ActorID: actor, LegalEntityID: "LE",
		ExpectedRevision: th.policy().Revision, Amount: amount, RateBP: rate, OccurredAt: th.hours(10).Add(time.Duration(th.n) * time.Minute)}
}

func (th *thresholds) set(actor string, amount int64, rate int) (api.Receipt, error) {
	return th.module.SetCostVarianceThreshold(th.ctx, th.setCmd(actor, amount, rate))
}

func (th *thresholds) approveCmd(actor string, version int) api.ApproveCostVarianceThreshold {
	th.n++
	return api.ApproveCostVarianceThreshold{CommandID: fmt.Sprintf("thr-approve-%d", th.n), TenantID: th.tenant, ActorID: actor, LegalEntityID: "LE",
		ExpectedRevision: th.policy().Revision, Version: version}
}

func (th *thresholds) refuseCmd(actor string, version int, reason string) api.RefuseCostVarianceThreshold {
	th.n++
	return api.RefuseCostVarianceThreshold{CommandID: fmt.Sprintf("thr-refuse-%d", th.n), TenantID: th.tenant, ActorID: actor, LegalEntityID: "LE",
		ExpectedRevision: th.policy().Revision, Version: version, Reason: reason}
}

func (th *thresholds) version(no int) api.VarianceThreshold {
	th.t.Helper()
	v := th.policy().VarianceThresholds
	if no > len(v) {
		th.t.Fatalf("no version %d: %+v", no, v)
	}
	return v[no-1]
}

func thresholdWorld(t *testing.T) *thresholds { return &thresholds{ap: creditorWorld(t)} }

// A version is stamped with FIN's clock, never with the command's business time (which stays as the command's record):
// a version that is not looser is in force at once, approved by its setter; a looser one waits and is in force only
// from
// its approval, by another person; a second version is refused while one waits; a refused version never takes effect;
// every command replays.
func TestAThresholdIsLooserOrNotOnPostgres(t *testing.T) {
	th := thresholdWorld(t)
	before := time.Now().UTC().Add(-time.Minute)
	v1 := th.version(1)
	if v1.Status != "APPROVED" || v1.DecidedBy != "ctrl-1" || v1.Automatic || !v1.Looser || v1.ProposedAt.Before(before) || !v1.InForceAt.Equal(v1.DecidedAt) ||
		!v1.SetAt.Equal(th.hours(0)) || p(th).InForceThreshold != 1 || p(th).ProposedThreshold != 0 {
		t.Fatalf("version 1: %+v", v1)
	}

	// looser by amount: proposed, not in force
	set := th.setCmd("acct-1", 50_000, 100)
	r := th.must(th.module.SetCostVarianceThreshold(th.ctx, set))
	if again := th.must(th.module.SetCostVarianceThreshold(th.ctx, set)); again != r || r.Version != 2 || len(th.policy().VarianceThresholds) != 2 {
		t.Fatalf("replay of a set: %+v then %+v", r, again)
	}
	if v := th.version(2); v.Status != "PROPOSED" || !v.Looser || v.InForceAt != (time.Time{}) || p(th).InForceThreshold != 1 || p(th).ProposedThreshold != 2 {
		t.Fatalf("version 2 waits: %+v", v)
	}
	// stamped with FIN's clock, never with the business time of the command
	if v := th.version(2); v.ProposedAt.Before(before) || v.SetAt.Equal(v.ProposedAt) {
		t.Fatalf("version 2 times: %+v", v)
	}
	_, err := th.set("acct-1", 1, 1)
	th.rejected("a second set while one waits", err, "FIN_THRESHOLD_PROPOSED")
	if !strings.Contains(err.Error(), "wait for it, or have an approver other than its setter refuse it") {
		t.Fatalf("the refusal names no way out: %v", err)
	}
	_, err = th.module.ApproveCostVarianceThreshold(th.ctx, th.approveCmd("acct-1", 2))
	th.rejected("the setter approving", err, "FIN_THRESHOLD_APPROVAL")
	_, err = th.module.RefuseCostVarianceThreshold(th.ctx, th.refuseCmd("acct-1", 2, "withdrawn"))
	th.rejected("the setter refusing", err, "FIN_THRESHOLD_APPROVAL")
	_, err = th.module.ApproveCostVarianceThreshold(th.ctx, th.approveCmd("ctrl-1", 1))
	th.rejected("approving another version than the one that waits", err, "waits for approval")
	_, err = th.module.RefuseCostVarianceThreshold(th.ctx, th.refuseCmd("ctrl-1", 2, " "))
	th.rejected("a refusal without a reason", err, "reason")

	// refused: never in force, and a new version is compared with the one in force
	refuse := th.refuseCmd("ctrl-1", 2, "too loose for this quarter")
	rr := th.must(th.module.RefuseCostVarianceThreshold(th.ctx, refuse))
	if again := th.must(th.module.RefuseCostVarianceThreshold(th.ctx, refuse)); again != rr {
		t.Fatalf("replay of a refusal: %+v then %+v", rr, again)
	}
	if v := th.version(2); v.Status != "REFUSED" || v.DecidedBy != "ctrl-1" || v.RefuseReason != "too loose for this quarter" || p(th).InForceThreshold != 1 ||
		p(th).ProposedThreshold != 0 {
		t.Fatalf("version 2 refused: %+v", v)
	}
	_, err = th.module.ApproveCostVarianceThreshold(th.ctx, th.approveCmd("ctrl-1", 2))
	th.rejected("approving a refused version", err, "no cost variance threshold waits")

	// lowering: in force at once, approved by its setter
	th.must(th.set("acct-1", 5_000, 50))
	if v := th.version(3); v.Status != "APPROVED" || !v.Automatic || v.DecidedBy != "acct-1" || v.Looser || !v.InForceAt.Equal(v.ProposedAt) || p(th).InForceThreshold != 3 {
		t.Fatalf("version 3, lowered: %+v", v)
	}
	// the same values are not looser either
	th.must(th.set("acct-1", 5_000, 50))
	if v := th.version(4); v.Looser || v.Status != "APPROVED" {
		t.Fatalf("version 4, the same: %+v", v)
	}
	// looser by rate alone
	th.must(th.set("acct-1", 5_000, 51))
	if v := th.version(5); !v.Looser || v.Status != "PROPOSED" {
		t.Fatalf("version 5, a higher rate: %+v", v)
	}
	approve := th.approveCmd("ctrl-2", 5)
	ra := th.must(th.module.ApproveCostVarianceThreshold(th.ctx, approve))
	if again := th.must(th.module.ApproveCostVarianceThreshold(th.ctx, approve)); again != ra || ra.Version != 5 {
		t.Fatalf("replay of an approval: %+v then %+v", ra, again)
	}
	if v := th.version(5); v.Status != "APPROVED" || v.Automatic || v.DecidedBy != "ctrl-2" || !v.InForceAt.Equal(v.DecidedAt) || !v.DecidedAt.After(v.ProposedAt) ||
		p(th).InForceThreshold != 5 {
		t.Fatalf("version 5 approved: %+v", v)
	}
	// a higher amount with a lower rate is looser too
	th.must(th.set("acct-1", 5_001, 1))
	if v := th.version(6); !v.Looser {
		t.Fatalf("version 6: %+v", v)
	}

	counts := map[string]int{}
	for _, kind := range th.outboxTypes(t) {
		counts[kind]++
	}
	for kind, want := range map[string]int{"CostVarianceThresholdSet": 6, "CostVarianceThresholdApproved": 4, "CostVarianceThresholdRefused": 1} {
		if counts[kind] != want {
			t.Errorf("outbox: %d %s, want %d", counts[kind], kind, want)
		}
	}
}

func p(th *thresholds) api.FinancePolicy { return th.policy() }

// Approval and refusal ask the authorizer for ApproveSettings, the permission granted by name only, and setting asks
// for
// its own action.
func TestAThresholdApprovalAsksForApproveSettingsOnPostgres(t *testing.T) {
	th := thresholdWorld(t)
	th.must(th.set("acct-1", 50_000, 100))
	var asked []string
	m, err := finmodule.New(th.db, th.w, th.w, th.w, th.w, th.w, recordingAuthorizer{&asked})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.RefuseCostVarianceThreshold(th.ctx, th.refuseCmd("ctrl-1", 2, "no")); err != nil {
		t.Fatal(err)
	}
	th.must(th.set("acct-1", 50_000, 100))
	if _, err = m.ApproveCostVarianceThreshold(th.ctx, th.approveCmd("ctrl-1", 3)); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 2 || asked[0] != "ApproveSettings" || asked[1] != "ApproveSettings" {
		t.Fatalf("the actions asked about: %v", asked)
	}
}

// A match is weighed against the threshold in force when it is read, and records the version it used: a match before
// the approval keeps version 1 and awaits its variance's approval, one after it uses version 2; a past decision is not
// reopened by the approval.
func TestAMatchBeforeAndAfterAThresholdApprovalOnPostgres(t *testing.T) {
	th := thresholdWorld(t)
	th.must(th.set("acct-1", 50_000, 100)) // CNY 500.00: a variance of CNY 200.00 is within it, not within CNY 100.00
	th.must(th.module.RecordCreditorInvoice(th.ctx, th.record("CI-T1", "trucker", "T1", 5, "CNY", line("PICKUP", 170_000))))
	th.must(th.module.MatchCreditorInvoice(th.ctx, th.match("CI-T1", "ap-1", 5, onJob("JOB-A", 1, 1, 170_000, true))))
	if i := th.invoice("CI-T1"); i.ThresholdVersion != 1 || !i.ApprovalRequired {
		t.Fatalf("a match while version 2 waits: %+v", i)
	}
	th.must(th.module.ApproveCostVarianceThreshold(th.ctx, th.approveCmd("ctrl-1", 2)))
	th.must(th.module.RecordCreditorInvoice(th.ctx, th.record("CI-T2", "trucker", "T2", 5, "CNY", line("DELIVERY", 50_000))))
	th.must(th.module.MatchCreditorInvoice(th.ctx, th.match("CI-T2", "ap-1", 5, onJob("JOB-B", 1, 1, 50_000, true))))
	if i := th.invoice("CI-T2"); i.ThresholdVersion != 2 || i.ApprovalRequired {
		t.Fatalf("a match after the approval: %+v", i)
	}
	if i := th.invoice("CI-T1"); i.ThresholdVersion != 1 || !i.ApprovalRequired || !i.AwaitingApproval {
		t.Fatalf("the earlier match is not reopened by the approval: %+v", i)
	}
}

// holdingPolicy takes the row lock FIN's policy commands take (FOR NO KEY UPDATE) on a connection of its own.
func holdingPolicy(t *testing.T, th *thresholds) (pid int, release func()) {
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
	var one int
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, th.tenant); err == nil {
		err = tx.QueryRow(ctx, `SELECT 1 FROM cargo_fin.finance_policy WHERE tenant_id=$1 AND legal_entity_id='LE' FOR NO KEY UPDATE`, th.tenant).Scan(&one)
	}
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	}
	if err != nil {
		t.Fatalf("holding the lock of the policy: %v", err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback(ctx)
			conn.Close(ctx)
		}
	}
	t.Cleanup(release)
	return pid, release
}

// A final match and the approval of a looser threshold at the same moment: the policy's lock is held by the test, the
// two commands queue behind it in a forced order (pg_blocking_pids shows each waits for the holder), and whichever is
// granted the lock first decides: a match first is weighed against version 1, an approval first makes the match
// read version 2. Either way the invoice records the version it read.
func TestAMatchRacesAThresholdApprovalOnPostgres(t *testing.T) {
	for _, matchFirst := range []bool{true, false} {
		th := thresholdWorld(t)
		th.must(th.set("acct-1", 50_000, 100))
		th.must(th.module.RecordCreditorInvoice(th.ctx, th.record("CI-T1", "trucker", "T1", 5, "CNY", line("PICKUP", 170_000))))
		match := th.match("CI-T1", "ap-1", 5, onJob("JOB-A", 1, 1, 170_000, true))
		approve := th.approveCmd("ctrl-1", 2)
		runMatch := func() error { _, err := th.module.MatchCreditorInvoice(th.ctx, match); return err }
		runApprove := func() error { _, err := th.module.ApproveCostVarianceThreshold(th.ctx, approve); return err }
		commands := []func() error{runMatch, runApprove}
		if !matchFirst {
			commands = []func() error{runApprove, runMatch}
		}
		pid, release := holdingPolicy(t, th)
		errs := make([]error, 2)
		done := make(chan int, 2)
		for i, run := range commands {
			go func() { errs[i] = run(); done <- i }()
			waitBlockedBy(t, pid, i+1)
		}
		release()
		<-done
		<-done
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("match first %v: %v", matchFirst, errs)
		}
		i := th.invoice("CI-T1")
		wantVersion, wantRequired := 2, false
		if matchFirst {
			wantVersion, wantRequired = 1, true
		}
		if i.ThresholdVersion != wantVersion || i.ApprovalRequired != wantRequired {
			t.Fatalf("match first %v: the invoice read version %d (approval required %v), want %d (%v)", matchFirst, i.ThresholdVersion, i.ApprovalRequired, wantVersion, wantRequired)
		}
		if p(th).InForceThreshold != 2 {
			t.Fatalf("version 2 is in force after the approval: %+v", p(th))
		}
	}
}

// What the database refuses past FIN, each by a raw write in a transaction of its own.
func TestAThresholdPastFINOnPostgres(t *testing.T) {
	th := thresholdWorld(t)
	f := th.fixture
	th.must(th.set("acct-1", 50_000, 100)) // version 2 waits
	th.must(th.module.RecordCreditorInvoice(th.ctx, th.record("CI-X", "trucker", "X1", 5, "CNY", line("PICKUP", 170_000))))
	touch := `UPDATE cargo_fin.finance_policy SET revision=revision+1 WHERE tenant_id=$1 AND legal_entity_id='LE';
`
	version := func(no int, amount int64, rate int, looser, proposed string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.cost_variance_threshold (tenant_id,legal_entity_id,threshold_no,amount_minor,rate_bp,set_by,set_at,entered_at,
            proposed_at,looser) VALUES ($1,'LE',%d,%d,%d,'x',now(),now(),%s,%s)`, no, amount, rate, proposed, looser)
	}
	decision := func(no int, kind, by, at, auto string) string {
		reason, inForce := "NULL", at
		if kind == "REFUSED" {
			reason, inForce = "'x'", "NULL"
		}
		return fmt.Sprintf(`INSERT INTO cargo_fin.cost_variance_threshold_decision (tenant_id,legal_entity_id,threshold_no,kind,decided_by,decided_at,in_force_at,
            auto,reason) VALUES ($1,'LE',%d,'%s','%s',%s,%s,%s,%s)`, no, kind, by, at, inForce, auto, reason)
	}
	v2At := `(SELECT proposed_at FROM cargo_fin.cost_variance_threshold WHERE tenant_id=$1 AND legal_entity_id='LE' AND threshold_no=2)`
	matchedAt := func(entered string, threshold int, required bool) string {
		return fmt.Sprintf(`UPDATE cargo_fin.creditor_invoice SET state='MATCHED',matched_by='x',matched_at=now(),match_entered_at=`+entered+`,approval_required=%v,threshold_no=%d,
            revision=revision+1 WHERE tenant_id=$1 AND invoice_id='CI-X';
INSERT INTO cargo_fin.creditor_invoice_match (tenant_id,invoice_id,match_no,line_no,target_kind,job_id,cost_side,target_line,target_version,amount_minor,final,
            local_minor,accrual_amount_minor,accrual_local_minor,prior_amount_minor,prior_local_minor,variance_minor,variance_local_minor)
            VALUES ($1,'CI-X',1,1,'JOB_COST','JOB-A','COST',1,1,170000,true,170000,150000,150000,0,0,20000,20000)`, required, threshold)
	}
	matched := func(threshold int, required bool) string { return matchedAt("now()", threshold, required) }
	for name, tc := range map[string]struct{ sql, state, check string }{
		"a version without its proposal time":             {touch + version(3, 99_999, 100, "NULL", "NULL"), "23514", "cost_variance_threshold_proposal"},
		"a looser flag that is not the database's answer": {touch + version(3, 99_999, 100, "false", "now()"), "23514", "cost_variance_threshold_looser"},
		"a version not looser, without its approval":      {touch + version(3, 5, 5, "false", "now()"), "23514", "cost_variance_threshold_auto"},
		"a version while an earlier one waits":            {touch + version(3, 99_999, 100, "true", "now()"), "23514", "cost_variance_threshold_one_proposed"},
		"a decision of a version written before this rule": {version(3, 1, 1, "NULL", "NULL") + ";\n" + decision(3, "APPROVED", "x", "now()", "true"), "23514",
			"cost_variance_decision_legacy"},
		"a looser version approved by its setter":        {decision(2, "APPROVED", "acct-1", "now()", "true"), "23514", "cost_variance_decision_other_actor"},
		"an automatic approval by another person":        {decision(2, "APPROVED", "ctrl-1", "now()", "true"), "23514", "cost_variance_decision_other_actor"},
		"an approval by the setter, not automatic":       {decision(2, "APPROVED", "acct-1", "now()", "false"), "23514", "cost_variance_decision_other_actor"},
		"a refusal by the setter":                        {decision(2, "REFUSED", "acct-1", "now()", "false"), "23514", "cost_variance_decision_other_actor"},
		"a decision before its proposal":                 {decision(2, "APPROVED", "ctrl-1", v2At+" - interval '5 seconds'", "false"), "23514", "cost_variance_decision_after_proposal"},
		"a decision a few minutes ahead of the database": {touch + decision(2, "APPROVED", "ctrl-1", "now() + interval '5 minutes'", "false"), "23514", "cost_variance_time_window"},
		"a decision not written with its policy":         {decision(2, "APPROVED", "ctrl-1", "now()", "false"), "23514", "fin_records_with_parent"},
		"a decision without a reason for a refusal":      {touch + strings.Replace(decision(2, "REFUSED", "ctrl-1", "now()", "false"), "'x')", "NULL)", 1), "23514", "cost_variance_decision_shape"},
		"a match that names a threshold not in force":    {matched(2, false), "23514", "creditor_invoice_consistent"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range strings.Split(tc.sql, ";\n") {
				if _, err := e.Exec(ctx, s, f.tenant); err != nil {
					return err
				}
			}
			return nil
		})
		state, constraint := sqlState(err)
		if state != tc.state || constraint != tc.check {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, state, constraint, tc.state, tc.check)
		}
		if name == "a match that names a threshold not in force" && (err == nil || !strings.Contains(err.Error(), "another threshold")) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// the same match naming the threshold in force is written: the rule above is what refused it
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, s := range strings.Split(matched(1, true), ";\n") {
			if _, err := e.Exec(ctx, s, f.tenant); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("a match that names the threshold in force: %v", err)
	}

	// an approved version is in force from its approval: a match recorded before that time does not name it
	c := thresholdWorld(t)
	c.must(c.set("acct-1", 50_000, 100))
	c.must(c.module.RecordCreditorInvoice(c.ctx, c.record("CI-X", "trucker", "X1", 5, "CNY", line("PICKUP", 170_000))))
	c.must(c.module.ApproveCostVarianceThreshold(c.ctx, c.approveCmd("ctrl-1", 2)))
	early := `(SELECT decided_at - interval '10 seconds' FROM cargo_fin.cost_variance_threshold_decision WHERE tenant_id=$1 AND threshold_no=2)`
	err := c.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, s := range strings.Split(matchedAt(early, 2, false), ";\n") {
			if _, err := e.Exec(ctx, s, c.tenant); err != nil {
				return err
			}
		}
		return nil
	})
	if state, constraint := sqlState(err); state != "23514" || constraint != "creditor_invoice_consistent" || !strings.Contains(err.Error(), "another threshold") {
		t.Errorf("a match before the approval that names the version: %v", err)
	}

	// a decision is written once, and a version written after a refusal does not go back in time
	w := thresholdWorld(t)
	w.must(w.set("acct-1", 50_000, 100))
	w.must(w.module.RefuseCostVarianceThreshold(w.ctx, w.refuseCmd("ctrl-1", 2, "no")))
	for name, tc := range map[string]struct{ sql, state, check string }{
		"a second decision of a version": {touch + decision(2, "REFUSED", "ctrl-2", "now()", "false"), "23505", ""},
		"a version proposed before the latest recorded time": {touch + version(3, 99_999, 100, "true",
			`(SELECT decided_at - interval '10 seconds' FROM cargo_fin.cost_variance_threshold_decision WHERE tenant_id=$1 AND threshold_no=2)`), "23514", "cost_variance_time_monotone"},
		"a decision rewritten": {`UPDATE cargo_fin.cost_variance_threshold_decision SET decided_by='y' WHERE tenant_id=$1`, "42501|23001", ""},
		"a decision deleted":   {`DELETE FROM cargo_fin.cost_variance_threshold_decision WHERE tenant_id=$1`, "42501|23001", ""},
	} {
		err := w.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range strings.Split(tc.sql, ";\n") {
				if _, err := e.Exec(ctx, s, w.tenant); err != nil {
					return err
				}
			}
			return nil
		})
		state, constraint := sqlState(err)
		if !strings.Contains(tc.state, state) || state == "" || (tc.check != "" && constraint != tc.check) {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, state, constraint, tc.state, tc.check)
		}
	}
}

// The decision table has row security enabled and forced, as the settings' tables have.
func TestTheThresholdDecisionTableHasForcedRowSecurity(t *testing.T) {
	f := newFixture(t)
	var enabled, forced bool
	err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE n.nspname = 'cargo_fin' AND c.relname = 'cost_variance_threshold_decision'`).Scan(&enabled, &forced)
	})
	if err != nil || !enabled || !forced {
		t.Fatalf("row security of the decision table: enabled %v, forced %v, %v", enabled, forced, err)
	}
}
