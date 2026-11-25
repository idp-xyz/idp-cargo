//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// FIN V2.100, after the independent review: the database bounds the time of a match, an approval is read under the
// policy's lock, a clock behind a record is refused with its cause, and the impact gives profit figures to ReadProfit.

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

// A raw write cannot choose the time of a match: the database holds it within a minute of its clock and not before the
// latest threshold approval in force, as it holds the decisions' times. The write that the review made, dating a match
// between versions 2 and 3 and naming the looser version 2 with no approval needed, is refused.
func TestARawMatchCannotChooseItsOwnTimeOnPostgres(t *testing.T) {
	th := thresholdWorld(t)
	th.must(th.set("acct-1", 50_000, 100))
	th.must(th.module.ApproveCostVarianceThreshold(th.ctx, th.approveCmd("ctrl-1", 2)))
	time.Sleep(50 * time.Millisecond)
	th.must(th.set("acct-1", 10_000, 100)) // version 3, a lowering, in force at once
	th.must(th.module.RecordCreditorInvoice(th.ctx, th.record("CI-X", "trucker", "X1", 5, "CNY", line("PICKUP", 170_000))))
	for name, tc := range map[string]struct{ entered, constraint string }{
		"a time between versions 2 and 3, naming version 2": {`(SELECT decided_at + interval '10 milliseconds' FROM cargo_fin.cost_variance_threshold_decision
            WHERE tenant_id=$1 AND threshold_no=2)`, "creditor_match_time_monotone"},
		"a time ahead of the database's clock": {`now() + interval '5 minutes'`, "creditor_match_time_window"},
		"a time behind the database's clock":   {`now() - interval '5 minutes'`, "creditor_match_time_window"},
	} {
		err := th.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range []string{`UPDATE cargo_fin.creditor_invoice SET state='MATCHED',matched_by='x',matched_at=now(),match_entered_at=` + tc.entered + `,
            approval_required=false,threshold_no=2,revision=revision+1 WHERE tenant_id=$1 AND invoice_id='CI-X'`,
				`INSERT INTO cargo_fin.creditor_invoice_match (tenant_id,invoice_id,match_no,line_no,target_kind,job_id,cost_side,target_line,target_version,amount_minor,final,
            local_minor,accrual_amount_minor,accrual_local_minor,prior_amount_minor,prior_local_minor,variance_minor,variance_local_minor)
            VALUES ($1,'CI-X',1,1,'JOB_COST','JOB-A','COST',1,1,170000,true,170000,150000,150000,0,0,20000,20000)`} {
				if _, err := e.Exec(ctx, s, th.tenant); err != nil {
					return err
				}
			}
			return nil
		})
		if state, constraint := sqlState(err); state != "23514" || constraint != tc.constraint {
			t.Errorf("%s: %v (state %s, constraint %s), want %s", name, err, state, constraint, tc.constraint)
		}
	}
}

// An approval is in force from FIN's clock read under the policy's lock: a match granted the lock first, and an
// approval queued behind it for a while, leaves the approval in force from a time after the match's own and after the
// moment the holder of the lock let go (not from the time the approval was asked).
func TestAnApprovalIsInForceFromTheClockReadUnderTheLockOnPostgres(t *testing.T) {
	th := thresholdWorld(t)
	th.must(th.set("acct-1", 50_000, 100))
	th.must(th.module.RecordCreditorInvoice(th.ctx, th.record("CI-T1", "trucker", "T1", 5, "CNY", line("PICKUP", 170_000))))
	match := th.match("CI-T1", "ap-1", 5, onJob("JOB-A", 1, 1, 170_000, true))
	approve := th.approveCmd("ctrl-1", 2)
	pid, release := holdingPolicy(t, th)
	errs := make([]error, 2)
	done := make(chan int, 2)
	run := []func() error{
		func() error { _, err := th.module.MatchCreditorInvoice(th.ctx, match); return err },
		func() error { _, err := th.module.ApproveCostVarianceThreshold(th.ctx, approve); return err },
	}
	for i, r := range run {
		go func() { errs[i] = r(); done <- i }()
		waitBlockedBy(t, pid, i+1)
	}
	time.Sleep(300 * time.Millisecond) // the approval has waited for a while: a clock read before the lock is older
	released := time.Now().UTC()
	release()
	<-done
	<-done
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("%v", errs)
	}
	var enteredAt time.Time
	if err := th.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT match_entered_at FROM cargo_fin.creditor_invoice WHERE tenant_id=$1 AND invoice_id='CI-T1'`, th.tenant).Scan(&enteredAt)
	}); err != nil {
		t.Fatal(err)
	}
	v := th.version(2)
	if th.invoice("CI-T1").ThresholdVersion != 1 {
		t.Fatalf("the match was granted the lock first and reads version 1: %+v", th.invoice("CI-T1"))
	}
	if !v.InForceAt.After(enteredAt) || v.InForceAt.Before(released.Add(-5*time.Millisecond)) {
		t.Fatalf("the approval is in force from %s, not after the match (%s) and the release of the lock (%s)", v.InForceAt, enteredAt, released)
	}
}

// The first version has nothing to be compared with and is looser: a raw write that stores it as not looser, approved
// by its setter alone, is refused as a wrong looser flag.
func TestARawFirstVersionStoredAsNotLooserIsRefusedOnPostgres(t *testing.T) {
	f := newFixture(t)
	if _, err := f.module.OpenFinancePolicy(context.Background(), api.OpenFinancePolicy{CommandID: "policy-raw", TenantID: f.tenant, ActorID: "acct-1",
		LegalEntityID: "LE", FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: f.at}); err != nil {
		t.Fatal(err)
	}
	err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, s := range []string{`UPDATE cargo_fin.finance_policy SET revision=revision+1 WHERE tenant_id=$1 AND legal_entity_id='LE'`,
			`INSERT INTO cargo_fin.cost_variance_threshold (tenant_id,legal_entity_id,threshold_no,amount_minor,rate_bp,set_by,set_at,entered_at,proposed_at,looser)
             VALUES ($1,'LE',1,100,1,'x',now(),now(),now(),false)`,
			`INSERT INTO cargo_fin.cost_variance_threshold_decision (tenant_id,legal_entity_id,threshold_no,kind,decided_by,decided_at,in_force_at,auto,reason)
             VALUES ($1,'LE',1,'APPROVED','x',now(),now(),true,NULL)`} {
			if _, err := e.Exec(ctx, s, f.tenant); err != nil {
				return err
			}
		}
		return nil
	})
	if state, constraint := sqlState(err); state != "23514" || constraint != "cost_variance_threshold_looser" {
		t.Fatalf("a first version stored as not looser: %v (state %s, constraint %s)", err, state, constraint)
	}
}

// A decision on a clock behind the latest record is refused with the cause and the way out (synchronise the clocks),
// not with a raw database error; the same for a setting; and a match on a clock behind an approval reads it.
func TestAThresholdOnAClockBehindARecordOnPostgres(t *testing.T) {
	th := thresholdWorld(t)
	th.must(th.set("acct-1", 50_000, 100))
	proposed := th.version(2).ProposedAt
	behind, err := finmodule.New(th.db, th.w, th.w, th.w, th.w, th.w, allow{}, finmodule.WithClock(fixedClock{proposed.Add(-20 * time.Second)}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = behind.ApproveCostVarianceThreshold(th.ctx, th.approveCmd("ctrl-1", 2))
	th.rejected("an approval on a clock behind the proposal", err, "synchronise the clocks")
	_, err = behind.RefuseCostVarianceThreshold(th.ctx, th.refuseCmd("ctrl-1", 2, "no"))
	th.rejected("a refusal on a clock behind the proposal", err, "synchronise the clocks")
	if th.version(2).Status != "PROPOSED" {
		t.Fatalf("a refused command changed the version: %+v", th.version(2))
	}
	th.must(th.module.ApproveCostVarianceThreshold(th.ctx, th.approveCmd("ctrl-1", 2)))
	_, err = behind.SetCostVarianceThreshold(th.ctx, api.SetCostVarianceThreshold{CommandID: "behind-set", TenantID: th.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: th.policy().Revision, Amount: 1, RateBP: 1, OccurredAt: th.hours(15)})
	th.rejected("a setting on a clock behind the approval", err, "synchronise the clocks")

	// a match on that clock is read at the time of the approval, not before it
	th.must(th.module.RecordCreditorInvoice(th.ctx, th.record("CI-T1", "trucker", "T1", 5, "CNY", line("PICKUP", 170_000))))
	approved := th.version(2).InForceAt
	late, err := finmodule.New(th.db, th.w, th.w, th.w, th.w, th.w, allow{}, finmodule.WithClock(fixedClock{approved.Add(-20 * time.Second)}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = late.MatchCreditorInvoice(th.ctx, th.match("CI-T1", "ap-1", 5, onJob("JOB-A", 1, 1, 170_000, true))); err != nil {
		t.Fatal(err)
	}
	if i := th.invoice("CI-T1"); i.ThresholdVersion != 2 || i.ApprovalRequired {
		t.Fatalf("a match on a clock behind the approval: %+v", i)
	}
}

// F6's figures are given to a holder of ReadProfit only; a holder of ConfigureSettings or ApproveSettings is told how
// many jobs' profit would change.
func TestTheImpactGivesProfitFiguresOnlyToAHolderOfReadProfitOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	ask := api.GetFinSettingsChangeImpact{TenantID: u.tenant, ActorID: "ann", Entries: []api.FinSettingEntry{le("fin.F6", "INCLUDED_MARKED")}}
	for name, c := range map[string]struct {
		actions []string
		figures bool
	}{
		"ReadProfit":                       {[]string{api.ActionReadProfit}, true},
		"ConfigureSettings":                {[]string{api.ActionConfigureSettings}, false},
		"ApproveSettings":                  {[]string{api.ActionApproveSettings}, false},
		"ConfigureSettings and ReadProfit": {[]string{api.ActionConfigureSettings, api.ActionReadProfit}, true},
	} {
		m, err := finmodule.New(u.db, u.w, u.w, u.w, u.w, u.w, onlyActions(c.actions))
		if err != nil {
			t.Fatal(err)
		}
		got, err := m.GetFinSettingsChangeImpact(u.ctx, ask)
		if err != nil || got.JobsChanged != 1 || (len(got.Jobs) == 1) != c.figures || (c.figures && got.Jobs[0].JobID != "JOB-C") || (!c.figures && len(got.Jobs) != 0) {
			t.Errorf("%s: %+v %v", name, got.Jobs, err)
		}
	}
}

// A change of F1's threshold alone is a change of the item (from a threshold to another), and moves the line of the
// threshold: a preview's attribution of a former share of CNY 230.77 is checked above a threshold of CNY 100.00 and not
// above one of CNY 1,000.00.
func TestTheImpactOfAChangeOfF1sThresholdAloneOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("pv", "ops-2", 6, statementOf("C", 5_000, 10_000, api.PartyCarrier))))
	u.configure(time0, thresholdOf(10_000))
	got := u.impact(thresholdOf(100_000))
	if len(got.Entities) != 1 || len(got.Entities[0].Changes) != 1 {
		t.Fatalf("a threshold alone: %+v", got.Entities)
	}
	c := got.Entities[0].Changes[0]
	if c.Key != "fin.F1" || c.From != "NOT_ABOVE_THRESHOLD" || c.To != "NOT_ABOVE_THRESHOLD" || c.FromThreshold == nil || c.FromThreshold.Minor != 10_000 ||
		c.ToThreshold == nil || c.ToThreshold.Minor != 100_000 || c.ToThreshold.Currency != "CNY" {
		t.Fatalf("the item: %+v", c)
	}
	if len(got.Checks) != 1 || got.Checks[0].Rule != "F1" || got.Checks[0].Now != "checked" || got.Checks[0].After != "not checked" {
		t.Fatalf("the check: %+v", got.Checks)
	}
	// the same threshold changes nothing; a threshold that stays below the share changes no check
	if same := u.impact(thresholdOf(10_000)); len(same.Entities) != 0 || len(same.Checks) != 0 {
		t.Fatalf("the same threshold: %+v", same)
	}
	if below := u.impact(thresholdOf(20_000)); len(below.Entities) != 1 || len(below.Checks) != 0 {
		t.Fatalf("a threshold still below the share: %+v", below)
	}
}

// afterRead answers PHY as the world does and then runs a hook, once, between PHY's read and FIN's snapshot.
type afterRead struct {
	*world
	hook func()
}

func (a *afterRead) GetPhysicalCostBasis(ctx context.Context, q phyapi.GetPhysicalCostBasis) (phyapi.PhysicalCostBasis, error) {
	b, err := a.world.GetPhysicalCostBasis(ctx, q)
	if h := a.hook; h != nil {
		a.hook = nil
		h()
	}
	return b, err
}

// A pool opened after PHY's basis was read and before FIN's snapshot belongs to a legal entity that changes: the impact
// cannot tell its answer, and names it (NotAssessed) instead of leaving it out.
func TestAPoolOpenedBetweenPHYAndTheSnapshotIsNotAssessedOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	u.settleInvoice(7)
	u.attribute("attribute", 6, statementOf("C", 5_000, 10_000, api.PartyPending))
	second := u.w.costBases["CONSOL-U"]
	second.ConsolidationID = "CONSOL-2"
	u.w.costBases["CONSOL-2"] = second
	reader := &afterRead{world: u.w}
	reader.hook = func() {
		u.must(u.module.OpenConsolCostPool(u.ctx, api.OpenConsolCostPool{CommandID: "pool-2", TenantID: u.tenant, ActorID: "fin-1", PoolID: "P-2",
			ConsolidationID: "CONSOL-2", OccurredAt: u.hours(8)}))
	}
	m, err := finmodule.New(u.db, u.w, u.w, u.w, u.w, reader, allow{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.GetFinSettingsChangeImpact(u.ctx, api.GetFinSettingsChangeImpact{TenantID: u.tenant, ActorID: "ann", Entries: []api.FinSettingEntry{le("fin.F7", "BLOCKS")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.NotAssessed) != 1 || got.NotAssessed[0] != "P-2" || len(got.Closes) != 1 || got.Closes[0].PoolID != u.pool {
		t.Fatalf("not assessed %v, closes %+v", got.NotAssessed, got.Closes)
	}
	if again := u.impact(le("fin.F7", "BLOCKS")); len(again.NotAssessed) != 0 {
		t.Fatalf("asked again: %v", again.NotAssessed)
	}
	if !strings.Contains(u.ap.pool("P-2").PoolID, "P-2") {
		t.Fatal("the second pool is not there")
	}
}
