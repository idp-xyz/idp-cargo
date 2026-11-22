//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// Finance attributes a departed house's former share (V2.93), through FIN's commands on PostgreSQL. The world is the
// air consolidation CONSOL-U of three loaded houses: SH-A 300 kg, SH-B 200 kg (volumetric) and SH-C 150 kg, one cost
// line of CNY 1,000.00 by CHARGEABLE, apportioned 460.54 / 307.69 / 230.77 (46,154 / 30,769 / 23,077 minor units: the
// floors leave two, to the largest remainders, SH-C's .92 and SH-A's .85). SH-C is then transferred away (ops-9, after
// a rework ops-1 asked for), and its 230.77 is a former share pending finance's attribution.

// formerWorld is the world with the line apportioned over the three houses, before SH-C leaves.
func formerWorld(t *testing.T) usedWorld {
	t.Helper()
	u := previewWorld(t)
	u.changeBasis(func(b *phyapi.PhysicalCostBasis) {
		b.Houses[2] = ownHouse(u.fixture, "SH-C", 3, 50, 600_000, 3, 150, 600_000)
	})
	a := u.apportionAll("apportion-1", 4, []int{1})
	u.checkLine("before SH-C leaves", a, 1, api.MethodChargeable,
		api.PoolShare{ShareNo: 1, ShipmentID: "SH-A", JobID: "JOB-A", Quantity: 300_000, Unit: "GRM", Amount: cny(46_154), Local: cny(46_154)},
		api.PoolShare{ShareNo: 2, ShipmentID: "SH-B", JobID: "JOB-B", Quantity: 200_000, Unit: "GRM", Amount: cny(30_769), Local: cny(30_769)},
		api.PoolShare{ShareNo: 3, ShipmentID: "SH-C", JobID: "JOB-C", Quantity: 150_000, Unit: "GRM", Amount: cny(23_077), Local: cny(23_077)})
	return u
}

// depart transfers SH-C away: PHY lists it as a former house, with the rework ops-1 asked for and ops-9 who ended its
// place.
func (u usedWorld) depart() {
	u.changeBasis(func(b *phyapi.PhysicalCostBasis) {
		b.Revision = 8
		b.Houses = b.Houses[:2]
		b.FormerHouses = []phyapi.FormerHouse{{ShipmentID: "SH-C", End: phyapi.FormerTransferred, TransferredTo: "CONSOL-2", Reason: "WRONG_CONSOL", EndedBy: "ops-9",
			EndedAt: u.hours(5), EndedEnteredAt: u.hours(5), ReworkFrom: phyapi.ReworkFromEntry, ReworkExecutionID: "X-1", ReworkNo: 1, ReworkUnitNo: 1, ReworkAskedBy: "ops-1"}}
	})
}

func (u usedWorld) attributeCmd(id, actor string, hour int, statements ...api.AttributionStatement) api.PreviewConsolCostApportionment {
	c := u.apportionCmd(id, hour, []int{1})
	c.ActorID, c.Attributions = actor, statements
	return c
}

func statementOf(house string, loss, exception int64, party string) api.AttributionStatement {
	return api.AttributionStatement{LineNo: 1, ShipmentID: "SH-" + house, Loss: loss, Exception: exception, Party: party, Voucher: "CLAIM-1"}
}

// attribute previews line 1 with the statements as ops-2 and submits it as fin-2, who asked for no rework and ended no
// house; it answers the apportionment as the pool shows it.
func (u usedWorld) attribute(id string, hour int, statements ...api.AttributionStatement) api.PoolApportionment {
	u.t.Helper()
	r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd(id, "ops-2", hour, statements...)))
	u.must(u.submit(id+"-submit", "fin-2", int(r.Version), hour))
	p := u.ap.pool(u.pool)
	return p.Apportionments[len(p.Apportionments)-1]
}

func (u usedWorld) jobProfit(job string) api.JobProfit {
	u.t.Helper()
	p, err := u.module.GetJobProfit(u.ctx, api.GetJobProfit{TenantID: u.tenant, JobID: job})
	if err != nil {
		u.t.Fatal(err)
	}
	return p
}

// rawCancel cancels the job past FIN, as the application role writes it: only the database's rule is in the way.
func (u usedWorld) rawCancel(job string) error {
	return u.rawRun(`UPDATE cargo_fin.freight_job SET state='CANCELLED',cancel_reason='x',cancelled_by='x',cancelled_at=now(),cancel_entered_at=now(),revision=revision+1
        WHERE tenant_id=$1 AND job_id='` + job + `'`)
}

func (u usedWorld) cancelJob(id, job string) error {
	_, err := u.module.CancelFreightJob(u.ctx, api.CancelFreightJob{CommandID: id, TenantID: u.tenant, ActorID: "fin-1", JobID: job, ExpectedRevision: 1, Reason: "shipment rolled",
		OccurredAt: u.hours(7)})
	return err
}

// The attribution through the commands: a preview without it is refused, the confirmers are refused and another finance
// user's submission is accepted, and the apportionment, its preview, the views of the pool, the job and the
// consolidation read back what was stated. Both commands replay: the second asking returns the stored receipt and
// writes nothing.
//
// The line is CNY 1,000.00; SH-C's loss is 50.00 and the exception cost 100.00 (the warehouse's, with its reference):
// 850.00 remains for SH-A and SH-B on 300,000 : 200,000 grams, 510.00 and 340.00.
func TestAFormerShareIsAttributedAndReplayedOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	_, err := u.module.PreviewConsolCostApportionment(u.ctx, u.apportionCmd("preview-none", 6, []int{1}))
	u.refusedWith("a preview of the line with no attribution", err, "FIN_ATTRIBUTION_MISSING")
	for _, part := range []string{"line 1", "house SH-C", "23077 CNY", "the loss it would bear by default", "state who bears it"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("the refusal does not say %q: %v", part, err)
		}
	}
	// the former share is pending: nobody's cost, listed apart on SH-C's job and on the pool's view
	if c := u.jobProfit("JOB-C"); c.PoolCost.Minor != 0 || c.FormerSharesPending.Minor != 23_077 || len(c.FormerShares) != 1 || len(c.PoolShares) != 0 {
		t.Fatalf("the profit of JOB-C before the attribution: %+v", c)
	}
	if v := u.ap.pool(u.pool).Attributions; len(v) != 1 || len(v[0].FormerShares) != 1 || v[0].FormerShares[0].ShipmentID != "SH-C" || v[0].FormerShares[0].Amount.Minor != 23_077 {
		t.Fatalf("the pool's view before the attribution: %+v", v)
	}
	statement := statementOf("C", 5_000, 10_000, api.PartyWarehouse)
	statement.PartyReference = "WH-claim-1"
	cmd := u.attributeCmd("preview-1", "ops-2", 6, statement)
	r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, cmd))
	if again := u.must(u.module.PreviewConsolCostApportionment(u.ctx, cmd)); again != r || len(u.ap.pool(u.pool).Previews) != 2 {
		t.Fatalf("the replay of the preview: %+v, then %+v, with %d previews", r, again, len(u.ap.pool(u.pool).Previews))
	}
	_, err = u.submit("submit-ops-1", "ops-1", int(r.Version), 6)
	u.refusedWith("the person who asked for the rework", err, "FIN_ATTRIBUTION_CONFIRMER")
	_, err = u.submit("submit-ops-9", "ops-9", int(r.Version), 6)
	u.refusedWith("the person who ended the house's place", err, "FIN_ATTRIBUTION_CONFIRMER")
	if !strings.Contains(err.Error(), "ops-9 ended the place of house SH-C in the consolidation (TRANSFERRED)") || !strings.Contains(err.Error(), "another finance user submits") {
		t.Fatalf("the refusal does not name the person and the way out: %v", err)
	}
	if n := len(u.ap.pool(u.pool).Apportionments); n != 1 {
		t.Fatalf("a refused submission was written: %d apportionments", n)
	}
	submit := u.submitCmd("submit-fin-2", "fin-2", int(r.Version), 6)
	s := u.must(u.module.SubmitConsolCostApportionment(u.ctx, submit))
	if again := u.must(u.module.SubmitConsolCostApportionment(u.ctx, submit)); again != s {
		t.Fatalf("the replay of the submission: %+v, then %+v", s, again)
	}
	if countOf(u.outboxTypes(t), "ConsolCostsApportioned") != 2 {
		t.Fatalf("the events: %v", u.outboxTypes(t))
	}

	p := u.ap.pool(u.pool)
	if len(p.Apportionments) != 2 || len(p.Previews) != 2 || p.Previews[1].SubmittedAs != 2 {
		t.Fatalf("the pool: %d apportionments, %d previews", len(p.Apportionments), len(p.Previews))
	}
	a := p.Apportionments[1]
	u.checkLine("after the attribution", a, 1, api.MethodChargeable,
		api.PoolShare{ShareNo: 1, ShipmentID: "SH-A", JobID: "JOB-A", Quantity: 300_000, Unit: "GRM", Amount: cny(51_000), Local: cny(51_000)},
		api.PoolShare{ShareNo: 2, ShipmentID: "SH-B", JobID: "JOB-B", Quantity: 200_000, Unit: "GRM", Amount: cny(34_000), Local: cny(34_000)})
	want := api.PoolAttribution{ShipmentID: "SH-C", JobID: "JOB-C", Loss: cny(5_000), LossLocal: cny(5_000), Exception: cny(10_000), ExceptionLocal: cny(10_000),
		Party: api.PartyWarehouse, PartyReference: "WH-claim-1", Voucher: "CLAIM-1", DepartedAt: u.hours(5)}
	if got := a.Lines[0].Attributions; len(got) != 1 || got[0] != want || a.Lines[0].ToAllocate.Minor != 0 || a.Lines[0].Amount.Minor != 100_000 {
		t.Fatalf("the attribution: %+v, want %+v", got, want)
	}
	if got := p.Previews[1].Lines[0].Attributions; len(got) != 1 || got[0] != want {
		t.Fatalf("the preview's attribution: %+v", got)
	}
	// SH-C's job bears the loss, labelled; SH-A's and SH-B's their shares
	c := u.jobProfit("JOB-C")
	if c.PoolCost.Minor != 5_000 || c.ReroutingLoss.Minor != 5_000 || len(c.ReroutingLosses) != 1 || c.ReroutingLosses[0] != (api.JobReroutingLoss{PoolID: "P-U", LineNo: 1, Amount: cny(5_000),
		Local: cny(5_000), Voucher: "CLAIM-1", DepartedAt: u.hours(5)}) || c.FormerSharesPending.Minor != 0 || len(c.FormerShares) != 0 || c.Profit.Minor != -5_000 {
		t.Fatalf("the profit of JOB-C: %+v", c)
	}
	if a, b := u.jobProfit("JOB-A"), u.jobProfit("JOB-B"); a.PoolCost.Minor != 51_000 || b.PoolCost.Minor != 34_000 || a.ReroutingLoss.Minor != 0 {
		t.Fatalf("the profit of SH-A and SH-B: %+v %+v", a, b)
	}
	// the consolidation: the costs are conserved, the exception cost is the warehouse's
	e, err := u.module.GetConsolProfit(u.ctx, api.GetConsolProfit{TenantID: u.tenant, ConsolidationID: "CONSOL-U"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Pooled.Minor != 100_000 || e.Apportioned.Minor != 85_000 || e.Losses.Minor != 5_000 || e.Exceptions.Minor != 10_000 || e.ExceptionsPending.Minor != 0 || e.ToAllocated.Minor != 0 ||
		e.FormerSharesPending.Minor != 0 || e.Retained.Minor != 0 || e.Unapportioned.Minor != 0 || len(e.Houses) != 3 || e.Houses[2].ShipmentID != "SH-C" || e.Houses[2].PoolCost.Minor != 5_000 {
		t.Fatalf("the consolidation's economics: %+v", e)
	}
	if len(e.Lines) != 1 || len(e.Lines[0].Exceptions) != 1 || e.Lines[0].Exceptions[0].Party != api.PartyWarehouse || e.Lines[0].Exceptions[0].Amount.Minor != 10_000 ||
		e.Lines[0].ExceptionPending.Minor != 0 || len(e.Lines[0].Losses) != 1 || e.Lines[0].Losses[0].JobID != "JOB-C" {
		t.Fatalf("the consolidation's lines: %+v", e.Lines)
	}
}

// An attribution in force is carried when the line is apportioned again, the remainder follows the cost, one that no
// longer fits is refused, a correction needs its reason and is a new version beside the old, and one restated as it
// stands is carried and needs no confirmer; all read back from the database.
func TestAnAttributionIsCarriedAndCorrectedOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	u.attribute("attribute-1", 6, statementOf("C", 5_000, 10_000, api.PartyPending))

	// carried: ops-1 and ops-9 may submit what changes nobody's attribution; the cost rose by 100.00 and the remainder
	// with it
	u.must(u.module.ReviseConsolCost(u.ctx, api.ReviseConsolCost{CommandID: "revise-1", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(), LineNo: 1,
		ExpectedVersion: 1, Value: api.ChargeSide{Amount: cny(110_000)}, Reason: "the carrier re-invoiced", OccurredAt: u.hours(7)}))
	r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-carried", "ops-2", 8)))
	u.must(u.submit("submit-carried", "ops-9", int(r.Version), 8))
	a := u.ap.pool(u.pool).Apportionments[2]
	x := a.Lines[0].Attributions[0]
	if !x.Carried || x.Loss.Minor != 5_000 || x.Exception.Minor != 10_000 || x.Party != api.PartyPending || x.Voucher != "CLAIM-1" || !x.DepartedAt.Equal(u.hours(5)) || a.Lines[0].Amount.Minor != 110_000 ||
		a.Lines[0].Shares[0].Amount.Minor != 57_000 || a.Lines[0].Shares[1].Amount.Minor != 38_000 {
		t.Fatalf("the carried attribution: %+v", a.Lines[0])
	}
	// the cost falls below what is carried: refused, and the way out is to state the attribution again
	u.must(u.module.ReviseConsolCost(u.ctx, api.ReviseConsolCost{CommandID: "revise-2", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(), LineNo: 1,
		ExpectedVersion: 2, Value: api.ChargeSide{Amount: cny(12_000)}, Reason: "the carrier credited", OccurredAt: u.hours(9)}))
	_, err := u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-no-fit", "ops-2", 10))
	u.refusedWith("carried amounts that no longer fit", err, "state them again")
	// a correction needs its reason, and is a new version: the earlier ones are kept
	smaller := statementOf("C", 2_000, 4_000, api.PartyCarrier)
	_, err = u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-no-reason", "ops-2", 10, smaller))
	u.refusedWith("a correction without its reason", err, "a correction states its reason")
	smaller.Reason, smaller.Voucher = "the carrier admitted the delay", "CLAIM-2"
	u.attribute("attribute-2", 10, smaller)
	p := u.ap.pool(u.pool)
	if len(p.Apportionments) != 4 || len(p.Apportionments[1].Lines[0].Attributions) != 1 || p.Apportionments[1].Lines[0].Attributions[0].Party != api.PartyPending {
		t.Fatalf("the apportionments: %d", len(p.Apportionments))
	}
	y := p.Apportionments[3].Lines[0].Attributions[0]
	if y.Carried || y.Loss.Minor != 2_000 || y.Exception.Minor != 4_000 || y.Party != api.PartyCarrier || y.Reason != "the carrier admitted the delay" || y.Voucher != "CLAIM-2" ||
		p.Apportionments[3].Lines[0].Amount.Minor != 12_000 || p.Apportionments[3].Lines[0].Shares[0].Amount.Minor != 3_600 || p.Apportionments[3].Lines[0].Shares[1].Amount.Minor != 2_400 {
		t.Fatalf("the correction: %+v", p.Apportionments[3].Lines[0])
	}
	if c := u.jobProfit("JOB-C"); c.ReroutingLoss.Minor != 2_000 || len(c.ReroutingLosses) != 1 || c.ReroutingLosses[0].Voucher != "CLAIM-2" {
		t.Fatalf("the profit of JOB-C: %+v", c)
	}
	// restated as it stands, voucher and all, it is carried, with the reason it had, and the asker may submit it
	same := statementOf("C", 2_000, 4_000, api.PartyCarrier)
	same.Voucher = "CLAIM-2"
	r = u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-same", "ops-2", 11, same)))
	u.must(u.submit("submit-same", "ops-1", int(r.Version), 11))
	if z := u.ap.pool(u.pool).Apportionments[4].Lines[0].Attributions[0]; !z.Carried || z.Reason != "the carrier admitted the delay" {
		t.Fatalf("the statement equal to the one in force: %+v", z)
	}
}

// A job that bears a former share pending, or a loss in force, is not cancelled: the refusal names the way out. Once
// finance attributes the loss away (a correction with a loss of 0) the job is cancelled; an attribution that is carried
// goes on, and a loss for a house whose job is not open is refused.
func TestAJobBearingALossIsNotCancelledOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	err := u.rawCancel("JOB-C")
	if state, constraint := sqlState(err); state != "23514" || constraint != "pool_share_job_open" || !strings.Contains(err.Error(), "have finance attribute its former share first") || !strings.Contains(err.Error(), "reopen the pool if it is closed") {
		t.Fatalf("the cancellation of a job with a former share pending: %v (state %s, constraint %s)", err, state, constraint)
	}
	u.attribute("attribute-loss", 6, statementOf("C", 5_000, 10_000, api.PartyCarrier))
	err = u.rawCancel("JOB-C")
	if state, constraint := sqlState(err); state != "23514" || constraint != "pool_loss_job_open" || !strings.Contains(err.Error(), "have finance change the attribution first") || !strings.Contains(err.Error(), "reopen the pool if it is closed") {
		t.Fatalf("the cancellation of a job that bears a loss: %v (state %s, constraint %s)", err, state, constraint)
	}
	if j := u.ap.job("JOB-C"); j.State != "OPEN" {
		t.Fatalf("JOB-C: %+v", j)
	}
	change := statementOf("C", 0, 15_000, api.PartyCarrier)
	change.Reason = "the carrier bears it all"
	change.Voucher = "CLAIM-3"
	u.attribute("attribute-no-loss", 7, change)
	if err = u.cancelJob("cancel-free", "JOB-C"); err != nil {
		t.Fatalf("the job bears nothing any more: %v", err)
	}
	// carried, the attribution with no loss goes on although its house's job is cancelled
	r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-carried", "ops-2", 8)))
	u.must(u.submit("submit-carried", "fin-2", int(r.Version), 8))
	if x := u.ap.pool(u.pool).Apportionments[3].Lines[0].Attributions[0]; !x.Carried || x.JobID != "JOB-C" || x.Loss.Minor != 0 || x.Exception.Minor != 15_000 {
		t.Fatalf("the carried attribution: %+v", x)
	}
	// a correction that leaves no loss keeps the job the attribution had, although it is cancelled
	fix := statementOf("C", 0, 13_000, api.PartyWarehouse)
	fix.Reason, fix.Voucher = "the warehouse bears part", "CLAIM-6"
	u.attribute("attribute-fix", 8, fix)
	if x := u.ap.pool(u.pool).Apportionments[4].Lines[0].Attributions[0]; x.Carried || x.JobID != "JOB-C" || x.Exception.Minor != 13_000 {
		t.Fatalf("the correction on a cancelled job: %+v", x)
	}
	// but a loss needs an open job
	again := statementOf("C", 1_000, 14_000, api.PartyCarrier)
	again.Reason, again.Voucher = "a loss after all", "CLAIM-4"
	_, err = u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-loss", "ops-2", 9, again))
	u.refusedWith("a loss on a cancelled job", err, "FIN_POOL_JOB_MISSING")
}

// The deterministic race of a submission with a loss and the cancellation of the job that is to bear it, both asked at
// the same revisions: a lock held on a second connection on the job, the two commands queued behind it in each order
// (each started only when the sessions before it are blocked by that holder). Submission first: it commits, and the
// cancellation is refused by FIN for the loss now in force (FIN_JOB_BEARS_POOL_COST). Cancellation first: it commits,
// and the submission is refused for the house's job that is not open, and the pool is as it was.
func TestASubmissionWithALossRacesTheCancelOfThatJob(t *testing.T) {
	for _, submissionFirst := range []bool{true, false} {
		u := formerWorld(t)
		u.depart()
		free := statementOf("C", 0, 15_000, api.PartyCarrier)
		u.attribute("attribute-free", 6, free)
		loss := statementOf("C", 5_000, 10_000, api.PartyCarrier)
		loss.Reason, loss.Voucher = "the loss is established", "CLAIM-5"
		r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-loss", "ops-2", 7, loss)))
		submit := u.submitCmd("submit-loss", "fin-3", int(r.Version), 7)
		cancel := api.CancelFreightJob{CommandID: "cancel", TenantID: u.tenant, ActorID: "fin-1", JobID: "JOB-C", ExpectedRevision: 1, Reason: "shipment rolled", OccurredAt: u.hours(8)}
		doSubmit := func() error { _, err := u.module.SubmitConsolCostApportionment(u.ctx, submit); return err }
		doCancel := func() error { _, err := u.module.CancelFreightJob(u.ctx, cancel); return err }
		var errs []error
		if submissionFirst {
			errs = queuedOnJob(t, u, "JOB-C", doSubmit, doCancel)
		} else {
			errs = queuedOnJob(t, u, "JOB-C", doCancel, doSubmit)
		}
		p := u.ap.pool(u.pool)
		if submissionFirst {
			if errs[0] != nil || !errors.Is(errs[1], api.ErrRejected) || !strings.Contains(errs[1].Error(), "FIN_JOB_BEARS_POOL_COST") || !strings.Contains(errs[1].Error(), "re-routing loss") {
				t.Fatalf("submission first: the submission returned %v, the cancellation %v", errs[0], errs[1])
			}
			if j := u.ap.job("JOB-C"); j.State != "OPEN" || len(p.Apportionments) != 3 || p.Apportionments[2].Lines[0].Attributions[0].Loss.Minor != 5_000 {
				t.Fatalf("submission first: %+v, %d apportionments", j, len(p.Apportionments))
			}
			continue
		}
		if errs[0] != nil || !errors.Is(errs[1], api.ErrRejected) || !strings.Contains(errs[1].Error(), "FIN_POOL_JOB_MISSING") || !strings.Contains(errs[1].Error(), "its job JOB-C is not open") {
			t.Fatalf("cancellation first: the cancellation returned %v, the submission %v", errs[0], errs[1])
		}
		if j := u.ap.job("JOB-C"); j.State != "CANCELLED" || len(p.Apportionments) != 2 {
			t.Fatalf("cancellation first: %+v, %d apportionments", j, len(p.Apportionments))
		}
	}
}

// queuedOnJob is queued with the lock of a job row, which both a submission naming the house's job and the cancellation
// of that job wait for.
func queuedOnJob(t *testing.T, u usedWorld, job string, commands ...func() error) []error {
	t.Helper()
	pid, release := holdingJobPID(t, u, job)
	errs := make([]error, len(commands))
	done := make(chan struct{}, len(commands))
	for i, run := range commands {
		go func() { errs[i] = run(); done <- struct{}{} }()
		waitBlockedBy(t, pid, i+1)
	}
	release()
	for range commands {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("a command did not return after the lock was released")
		}
	}
	return errs
}

// A raw apportionment past FIN, as the application role writes it: the apportionment and its line, shares and
// attributions.
type raw struct{ apportionment, line, shares, attributions []string }

func rawShare(no, share int, shipment, job string, used bool, amount int64) string {
	unit, quantity := "'GRM'", int64(1)
	if !used {
		unit, quantity = "NULL", 0
	}
	return fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment_share (tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,used,quantity,unit,amount_minor,
            local_minor) VALUES ($1,'P-U',%d,1,%d,'%s','%s',%v,%d,%s,%d,%d)`, no, share, shipment, job, used, quantity, unit, amount, amount)
}

func rawAttribution(no int, shipment, job string, loss, lossLocal, exception, exceptionLocal int64, party string) string {
	p := "NULL"
	if party != "" {
		p = "'" + party + "'"
	}
	return fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment_attribution (tenant_id,pool_id,apportionment_no,line_no,shipment_id,job_id,loss_minor,loss_local_minor,
            exception_minor,exception_local_minor,party,voucher,departed_at,carried) VALUES ($1,'P-U',%d,1,'%s','%s',%d,%d,%d,%d,%s,'CLAIM-9',now(),false)`, no, shipment, job, loss,
		lossLocal, exception, exceptionLocal, p)
}

func rawLine(no int, method string, toAllocate, toAllocateLocal int64) string {
	reason := "NULL"
	if method == "MANUAL" || method == "RETAINED" {
		reason = "'agreed'"
	}
	return fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,reason,amount_minor,local_minor,to_allocate_minor,
            to_allocate_local_minor) VALUES ($1,'P-U',%d,1,1,'%s',%s,100000,100000,%d,%d)`, no, method, reason, toAllocate, toAllocateLocal)
}

const rawBump = `UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-U'`

func rawApportionment(no int) string {
	return fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at)
            VALUES ($1,'P-U',%d,7,'ACTUAL','x',now(),now())`, no)
}

func rawJoin(parts ...string) string { return strings.Join(parts, ";\n") }

func (u usedWorld) rawRun(sql string) error {
	return u.fixture.within(u.t, func(ctx context.Context, e bentopg.Executor) error {
		for _, s := range strings.Split(sql, ";\n") {
			if _, err := e.Exec(ctx, s, u.tenant); err != nil {
				return err
			}
		}
		return nil
	})
}

// What the database refuses past FIN on the application role (V2.93), each by its name: an attribution written without
// its pool; one that breaks the conservation of its line (the amounts, the local amounts, a share beside an amount to
// be allocated, no share and nothing to be allocated or attributed, a retained line with an amount to be allocated or
// attributions above its cost); one of a job that is another shipment's or is not open to bear a loss; the checks of
// the columns; one changed or removed; and the controls that the lines are accepted when they conserve: shares and an
// attribution, an attribution and an amount to be allocated with no share, a whole cost attributed, a retained line
// with an attribution, and a loss of 0 on a job that is cancelled.
func TestAttributionsPastFINOnPostgres(t *testing.T) {
	u := newUsedWorld(t)
	u.record("cost-1", "AIR_FREIGHT", api.MethodChargeable, 100_000)
	u.record("cost-2", "ZERO_FEE", api.MethodShipment, 0)
	driver := u.w.drivers["SH-C"]
	driver.ShipmentID = "SH-D"
	u.w.drivers["SH-D"] = driver
	u.must(u.module.OpenFreightJob(u.ctx, api.OpenFreightJob{CommandID: "open-d", TenantID: u.tenant, ActorID: "fin-1", JobID: "JOB-D", ShipmentID: "SH-D", OccurredAt: u.hours(1)}))
	u.must(u.module.CancelFreightJob(u.ctx, api.CancelFreightJob{CommandID: "cancel-d", TenantID: u.tenant, ActorID: "fin-1", JobID: "JOB-D", ExpectedRevision: 1, Reason: "not needed",
		OccurredAt: u.hours(2)}))
	u.must(u.module.OpenFinancePolicy(u.ctx, api.OpenFinancePolicy{CommandID: "policy-2", TenantID: u.tenant, ActorID: "acct-1", LegalEntityID: "LE2", FunctionalCurrency: "CNY",
		BookCode: "MAIN", OccurredAt: u.hours(0)}))
	other := u.w.drivers["SH-C"]
	other.ShipmentID, other.LegalEntityID = "SH-E", "LE2"
	u.w.drivers["SH-E"] = other
	u.must(u.module.OpenFreightJob(u.ctx, api.OpenFreightJob{CommandID: "open-e", TenantID: u.tenant, ActorID: "fin-1", JobID: "JOB-E", ShipmentID: "SH-E", OccurredAt: u.hours(1)}))
	u.apportion("apportion-1", 4, []int{1}, "SH-C")
	conserving := func(no int, extra ...string) string {
		return rawJoin(append([]string{rawBump, rawApportionment(no), rawLine(no, "CHARGEABLE", 0, 0), rawShare(no, 1, "SH-A", "JOB-A", true, 50_000),
			rawShare(no, 2, "SH-B", "JOB-B", true, 35_000)}, extra...)...)
	}
	cases := map[string]struct{ sql, state, check string }{
		"an attribution behind its pool": {rawAttribution(1, "SH-C", "JOB-C", 0, 0, 5_000, 5_000, "CARRIER"), "23514", "fin_records_with_parent"},
		"amounts that do not conserve the line": {conserving(2, rawAttribution(2, "SH-C", "JOB-C", 10_000, 10_000, 4_999, 4_999, "CARRIER")), "23514",
			"pool_apportionment_conserves"},
		"local amounts that do not conserve the line": {conserving(2, rawAttribution(2, "SH-C", "JOB-C", 10_000, 9_999, 5_000, 5_000, "CARRIER")), "23514",
			"pool_apportionment_conserves"},
		"a share beside an amount to be allocated": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 10_000, 10_000), rawShare(2, 1, "SH-A", "JOB-A", true, 50_000),
			rawShare(2, 2, "SH-B", "JOB-B", true, 35_000), rawAttribution(2, "SH-C", "JOB-C", 5_000, 5_000, 0, 0, "")), "23514", "pool_apportionment_conserves"},
		"no share, nothing to be allocated and no attribution": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0)), "23514", "pool_apportionment_conserves"},
		"a share used missing beside attributions": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawShare(2, 1, "SH-A", "JOB-A", false, 0),
			rawAttribution(2, "SH-C", "JOB-C", 100_000, 100_000, 0, 0, "")), "23514", "pool_apportionment_conserves"},
		"a line of no cost with no share, no attribution and nothing to be allocated": {rawJoin(rawBump, rawApportionment(2), `INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,
            apportionment_no,line_no,line_version,method,amount_minor,local_minor,to_allocate_minor,to_allocate_local_minor) VALUES ($1,'P-U',2,2,1,'SHIPMENT',0,0,0,0)`), "23514",
			"pool_apportionment_conserves"},
		"shares numbered with a gap": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawShare(2, 1, "SH-A", "JOB-A", true, 50_000), rawShare(2, 3, "SH-B", "JOB-B", true, 35_000),
			rawAttribution(2, "SH-C", "JOB-C", 10_000, 10_000, 5_000, 5_000, "CARRIER")), "23514", "pool_apportionment_conserves"},
		"an attribution added to a line that conserves without it": {rawJoin(rawBump, rawAttribution(1, "SH-C", "JOB-C", 1, 1, 0, 0, "")), "23514", "pool_apportionment_conserves"},
		"a retained line with an amount to be allocated":           {rawJoin(rawBump, rawApportionment(2), rawLine(2, "RETAINED", 5_000, 5_000)), "23514", "pool_apportionment_conserves"},
		"a retained line with attributions above its cost": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "RETAINED", 0, 0), rawAttribution(2, "SH-C", "JOB-C", 100_001, 100_001, 0, 0, "")),
			"23514", "pool_apportionment_conserves"},
		"a retained line with local attributions above its local cost": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "RETAINED", 0, 0),
			rawAttribution(2, "SH-C", "JOB-C", 50_000, 100_001, 0, 0, "")), "23514", "pool_apportionment_conserves"},
		"a loss on the job of another shipment":     {conserving(2, rawAttribution(2, "SH-C", "JOB-A", 10_000, 10_000, 5_000, 5_000, "CARRIER")), "23514", "pool_attribution_job_open"},
		"a loss on the job of another legal entity": {conserving(2, rawAttribution(2, "SH-E", "JOB-E", 10_000, 10_000, 5_000, 5_000, "CARRIER")), "23514", "pool_attribution_job_open"},
		"an attribution of a job that does not exist": {conserving(2, rawAttribution(2, "SH-C", "JOB-NONE", 10_000, 10_000, 5_000, 5_000, "CARRIER")), "23503",
			"pool_apportionment_attribution_tenant_id_job_id_fkey"},
		"a loss on a job that is cancelled": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawShare(2, 1, "SH-A", "JOB-A", true, 50_000),
			rawShare(2, 2, "SH-B", "JOB-B", true, 35_000), rawAttribution(2, "SH-D", "JOB-D", 15_000, 15_000, 0, 0, "")), "23514", "pool_attribution_job_open"},
		"a negative loss": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawAttribution(2, "SH-C", "JOB-C", -1, 0, 0, 0, "")), "23514",
			"pool_apportionment_attribution_loss_minor_check"},
		"a negative exception cost": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawAttribution(2, "SH-C", "JOB-C", 0, 0, -1, 0, "CARRIER")), "23514",
			"pool_apportionment_attribution_exception_minor_check"},
		"an exception cost with no party": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawAttribution(2, "SH-C", "JOB-C", 0, 0, 100, 100, "")), "23514",
			"pool_attribution_party"},
		"a party with no exception cost": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawAttribution(2, "SH-C", "JOB-C", 100, 100, 0, 0, "CARRIER")), "23514",
			"pool_attribution_party"},
		"a party outside the list": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawAttribution(2, "SH-C", "JOB-C", 0, 0, 100, 100, "NO_SUCH_PARTY")),
			"23514", "pool_apportionment_attribution_party_check"},
		"a local loss with no loss": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawAttribution(2, "SH-C", "JOB-C", 0, 5, 0, 0, "")), "23514",
			"pool_attribution_locals"},
		"a local exception cost with no exception cost": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawAttribution(2, "SH-C", "JOB-C", 0, 0, 0, 5, "")),
			"23514", "pool_attribution_locals"},
		"a blank voucher": {strings.Replace(rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawAttribution(2, "SH-C", "JOB-C", 0, 0, 0, 0, "")), "'CLAIM-9'", "' '", 1),
			"23514", "pool_apportionment_attribution_voucher_check"},
		"an amount to be allocated below 0": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", -1, 0)), "23514", "pool_apportionment_line_to_allocate"},
		"a local amount to be allocated with none to be allocated": {rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 5)), "23514",
			"pool_apportionment_line_to_allocate_pair"},
		"an attribution changed":           {`UPDATE cargo_fin.pool_apportionment_attribution SET voucher='y' WHERE tenant_id=$1`, "42501", ""},
		"an attribution removed":           {`DELETE FROM cargo_fin.pool_apportionment_attribution WHERE tenant_id=$1`, "42501", ""},
		"an attribution of another tenant": {strings.Replace(rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawAttribution(2, "SH-C", "JOB-C", 0, 0, 0, 0, "")), "VALUES ($1,'P-U',2,1,'SH-C'", "VALUES ($1||'-other','P-U',2,1,'SH-C'", 1), "42501", ""},
	}
	for name, tc := range cases {
		err := u.rawRun(tc.sql)
		if state, constraint := sqlState(err); state != tc.state || (tc.check != "" && constraint != tc.check) {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, state, constraint, tc.state, tc.check)
		}
	}
	// the controls
	controls := []struct{ name, sql string }{
		{"shares and an attribution that conserve the line", conserving(2, rawAttribution(2, "SH-C", "JOB-C", 10_000, 10_000, 5_000, 5_000, "CARRIER"))},
		{"an attribution and an amount to be allocated, with no share", rawJoin(rawBump, rawApportionment(3), rawLine(3, "CHARGEABLE", 95_000, 95_000),
			rawAttribution(3, "SH-C", "JOB-C", 0, 0, 5_000, 5_000, "PENDING"))},
		{"a whole cost attributed, with no share", rawJoin(rawBump, rawApportionment(4), rawLine(4, "CHARGEABLE", 0, 0), rawAttribution(4, "SH-C", "JOB-C", 100_000, 100_000, 0, 0, ""))},
		{"a retained line with an attribution", rawJoin(rawBump, rawApportionment(5), rawLine(5, "RETAINED", 0, 0), rawAttribution(5, "SH-C", "JOB-C", 10_000, 10_000, 0, 0, ""))},
		{"an exception cost on a job that is cancelled", rawJoin(rawBump, rawApportionment(6), rawLine(6, "CHARGEABLE", 0, 0), rawShare(6, 1, "SH-A", "JOB-A", true, 50_000),
			rawShare(6, 2, "SH-B", "JOB-B", true, 35_000), rawAttribution(6, "SH-D", "JOB-D", 0, 0, 15_000, 15_000, "OWN_OPERATIONS"))},
	}
	for _, c := range controls {
		if err := u.rawRun(c.sql); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	p := u.ap.pool(u.pool)
	if len(p.Apportionments) != 6 || len(p.Apportionments[1].Lines[0].Attributions) != 1 || p.Apportionments[2].Lines[0].ToAllocate.Minor != 95_000 || len(p.Apportionments[2].Lines[0].Shares) != 0 ||
		p.Apportionments[4].Lines[0].Method != api.MethodRetained || p.Apportionments[5].Lines[0].Attributions[0].JobID != "JOB-D" {
		t.Fatalf("the controls: %+v", p.Apportionments)
	}
	// a row is never changed or removed: the privilege refuses first, and as the migrator role, which holds the
	// privilege, the trigger does
	if dsn := os.Getenv("IDP_CARGO_MIGRATOR_DSN"); dsn != "" {
		conn, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		for name, sql := range map[string]string{"changed": `UPDATE cargo_fin.pool_apportionment_attribution SET voucher='y' WHERE tenant_id=$1`,
			"removed": `DELETE FROM cargo_fin.pool_apportionment_attribution WHERE tenant_id=$1`} {
			_, err := conn.Exec(context.Background(), sql, u.tenant)
			if state, _ := sqlState(err); state != "23001" || err == nil || !strings.Contains(err.Error(), "pool_apportionment_attribution rows are immutable") {
				t.Errorf("an attribution %s by the migrator role: %v", name, err)
			}
		}
	}
	// a write at REPEATABLE READ is refused by name, as every write to cargo_fin
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`SELECT set_config('app.tenant_id',$1,true)`, rawAttribution(2, "SH-X", "JOB-C", 0, 0, 0, 0, "")} {
		if _, err = tx.Exec(ctx, s, u.tenant); err != nil {
			break
		}
	}
	if state, constraint := sqlState(err); state != "23514" || constraint != "fin_read_committed" {
		t.Fatalf("an attribution at REPEATABLE READ: %v (state %s, constraint %s)", err, state, constraint)
	}
	_ = tx.Rollback(ctx)
}

// The table has row security enabled and forced.
func TestTheAttributionTableHasForcedRowSecurity(t *testing.T) {
	f := newFixture(t)
	var enabled, forced bool
	err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE n.nspname = 'cargo_fin' AND c.relname = 'pool_apportionment_attribution'`).Scan(&enabled, &forced)
	})
	if err != nil || !enabled || !forced {
		t.Fatalf("row security of the attribution table: enabled %v, forced %v, %v", enabled, forced, err)
	}
}

// The preview check of fin/000015 compares the attributions and the amounts to be allocated too: an apportionment that
// names a preview with an attribution is accepted when its attribution is the preview's and refused by name for each
// field that differs, for one more, for one less, and for another amount to be allocated.
func TestAnApportionmentIsWhatItsPreviewComputedOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-1", "ops-2", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))))
	pn := int(r.Version)
	pv := u.ap.pool(u.pool).Previews[pn-1]
	if pv.PreviewNo != pn || pv.SubmittedAs != 0 {
		t.Fatalf("the preview: %+v", pv)
	}
	departed := fmt.Sprintf(`(SELECT (x->>'departed_at')::timestamptz FROM cargo_fin.pool_apportionment_preview v, jsonb_array_elements(v.figures) l,
        jsonb_array_elements(l->'attributions') x WHERE v.tenant_id=$1 AND v.pool_id='P-U' AND v.preview_no=%d)`, pn)
	insert := fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at,preview_no)
            SELECT $1,'P-U',2,basis_revision,stage,'x',now(),now(),%[1]d FROM cargo_fin.pool_apportionment_preview WHERE tenant_id=$1 AND pool_id='P-U' AND preview_no=%[1]d`, pn)
	build := func(shares string, att ...string) string {
		parts := []string{rawBump, insert,
			`INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,amount_minor,local_minor,to_allocate_minor,
            to_allocate_local_minor) VALUES ($1,'P-U',2,1,1,'CHARGEABLE',100000,100000,0,0)`, shares}
		return rawJoin(append(parts, att...)...)
	}
	shares := rawJoin(rawShare(2, 1, "SH-A", "JOB-A", true, 51_000), rawShare(2, 2, "SH-B", "JOB-B", true, 34_000))
	// the shares as the preview has them: the quantities are 300,000 and 200,000
	shares = strings.NewReplacer("1,'GRM',51000,51000", "300000,'GRM',51000,51000", "1,'GRM',34000,34000", "200000,'GRM',34000,34000").Replace(shares)
	attribution := func(loss, lossLocal, exception, party, reference, voucher, reason, carried string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment_attribution (tenant_id,pool_id,apportionment_no,line_no,shipment_id,job_id,loss_minor,loss_local_minor,
            exception_minor,exception_local_minor,party,party_reference,voucher,reason,departed_at,carried) VALUES ($1,'P-U',2,1,'SH-C','JOB-C',%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`,
			loss, lossLocal, exception, exception, party, reference, voucher, reason, departed, carried)
	}
	same := func(mutate func(a *[10]string)) string {
		a := [10]string{"5000", "5000", "10000", "'WAREHOUSE'", "NULL", "'CLAIM-1'", "NULL", "false"}
		mutate(&a)
		return attribution(a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7])
	}
	cases := map[string]string{
		"another loss":               build(shares, same(func(a *[10]string) { a[0], a[1] = "4000", "4000" })),
		"another local loss":         build(shares, same(func(a *[10]string) { a[1] = "4999" })),
		"another exception cost":     build(shares, same(func(a *[10]string) { a[2] = "9000" })),
		"another party":              build(shares, same(func(a *[10]string) { a[3] = "'CARRIER'" })),
		"another voucher":            build(shares, same(func(a *[10]string) { a[5] = "'CLAIM-2'" })),
		"a reason the preview lacks": build(shares, same(func(a *[10]string) { a[6] = "'because'" })),
		"another carried":            build(shares, same(func(a *[10]string) { a[7] = "true" })),
		"another job":                build(shares, strings.Replace(same(func(a *[10]string) {}), "'SH-C','JOB-C'", "'SH-C','JOB-A'", 1)),
		"another house":              build(shares, strings.Replace(same(func(a *[10]string) {}), "'SH-C','JOB-C'", "'SH-D','JOB-A'", 1)),
		"no attribution":             build(rawJoin(rawShare(2, 1, "SH-A", "JOB-A", true, 51_000), rawShare(2, 2, "SH-B", "JOB-B", true, 49_000))),
	}
	for name, sql := range cases {
		err := u.rawRun(sql)
		state, constraint := sqlState(err)
		// an attribution of another job or house, or no attribution, may be refused by the line's own rules first;
		// either way it is refused
		if state != "23514" || (constraint != "pool_apportionment_preview_matches" && constraint != "pool_apportionment_conserves" && constraint != "pool_attribution_job_open") {
			t.Errorf("%s: %v (state %s, constraint %s)", name, err, state, constraint)
			continue
		}
		if name != "another job" && name != "another house" && name != "no attribution" && constraint != "pool_apportionment_preview_matches" {
			t.Errorf("%s: refused by %s, not by the preview check", name, constraint)
		}
	}
	// another amount to be allocated: a line that divides nothing
	toAllocate := rawJoin(rawBump, insert,
		rawLine(2, "CHARGEABLE", 85_000, 85_000), same(func(a *[10]string) {}))
	err := u.rawRun(toAllocate)
	if state, constraint := sqlState(err); state != "23514" || constraint != "pool_apportionment_preview_matches" {
		t.Errorf("another amount to be allocated: %v (state %s, constraint %s)", err, state, constraint)
	}
	// an attribution the preview had and the apportionment lacks, the shares being the same (a statement of
	// nothing): refused
	zero := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-zero", "ops-2", 6, statementOf("C", 0, 0, ""))))
	zn := int(zero.Version)
	lacking := rawJoin(rawBump, fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at,preview_no)
            SELECT $1,'P-U',2,basis_revision,stage,'x',now(),now(),%[1]d FROM cargo_fin.pool_apportionment_preview WHERE tenant_id=$1 AND pool_id='P-U' AND preview_no=%[1]d`, zn),
		`INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,amount_minor,local_minor,to_allocate_minor,
            to_allocate_local_minor) VALUES ($1,'P-U',2,1,1,'CHARGEABLE',100000,100000,0,0)`,
		strings.NewReplacer("1,'GRM',51000,51000", "300000,'GRM',60000,60000", "1,'GRM',34000,34000", "200000,'GRM',40000,40000").Replace(
			rawJoin(rawShare(2, 1, "SH-A", "JOB-A", true, 51_000), rawShare(2, 2, "SH-B", "JOB-B", true, 34_000))))
	err = u.rawRun(lacking)
	if state, constraint := sqlState(err); state != "23514" || constraint != "pool_apportionment_preview_matches" {
		t.Errorf("an attribution the preview had and the apportionment lacks: %v (state %s, constraint %s)", err, state, constraint)
	}
	// the control: the apportionment the preview computed
	if err = u.rawRun(build(shares, same(func(a *[10]string) {}))); err != nil {
		t.Fatalf("an apportionment equal to its preview: %v", err)
	}
	if p := u.ap.pool(u.pool); len(p.Apportionments) != 2 || p.Apportionments[1].PreviewNo != pn || p.Previews[pn-1].SubmittedAs != 2 {
		t.Fatalf("the control: %+v", p)
	}
}

// The close at commit: a pool closed while its latest apportionment of a line leaves an amount to be allocated is
// refused by name; the control is the same pool closed with nothing to be allocated.
func TestAClosedPoolWithAnAmountToBeAllocatedPastFINOnPostgres(t *testing.T) {
	for _, toAllocate := range []bool{true, false} {
		u := previewWorld(t)
		r := u.must(u.preview("preview", 4, nil, "SH-C"))
		u.must(u.submit("submit", "fin-1", int(r.Version), 4))
		u.must(u.module.RecordCreditorInvoice(u.ctx, api.RecordCreditorInvoice{CommandID: "invoice", TenantID: u.tenant, ActorID: "ap-1", InvoiceID: "CI-X", LegalEntityID: "LE",
			CreditorPartyID: "airline-mu", SupplierInvoiceNo: "X-1", InvoiceDate: u.hours(5).Format(time.DateOnly), Currency: "CNY",
			Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 100_000}}, OccurredAt: u.hours(5)}))
		u.must(u.module.MatchCreditorInvoice(u.ctx, u.match("CI-X", "ap-1", 5, onPool("P-U", 1, 1, 100_000, true))))
		if toAllocate {
			if err := u.rawRun(rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 95_000, 95_000), rawAttribution(2, "SH-C", "JOB-C", 0, 0, 5_000, 5_000, "PENDING"))); err != nil {
				t.Fatal(err)
			}
		}
		err := u.rawRun(rawJoin(rawBump, `UPDATE cargo_fin.consol_cost_pool SET state='CLOSED' WHERE tenant_id=$1 AND pool_id='P-U'`,
			`INSERT INTO cargo_fin.consol_cost_pool_closure (tenant_id,pool_id,closure_no,closed_by,closed_at,entered_at) VALUES ($1,'P-U',1,'x',now(),now())`))
		state, constraint := sqlState(err)
		if toAllocate && (state != "23514" || constraint != "consol_cost_pool_closable") {
			t.Fatalf("a close with an amount to be allocated: %v (state %s, constraint %s)", err, state, constraint)
		}
		if !toAllocate && err != nil {
			t.Fatalf("a close with nothing to be allocated: %v", err)
		}
		if !toAllocate {
			// a closed pool takes no attribution written past FIN either
			err = u.rawRun(rawJoin(rawBump, rawAttribution(1, "SH-C", "JOB-C", 0, 0, 5_000, 5_000, "CARRIER")))
			if state, constraint := sqlState(err); state != "23514" || constraint != "consol_cost_pool_open" {
				t.Fatalf("an attribution into a closed pool: %v (state %s, constraint %s)", err, state, constraint)
			}
		}
	}
}

// A job that holds a loss in force is not cancelled at commit, whoever comes first, with a lock held on a second
// connection: a transaction that cancels the job holds its row, and an attribution with a loss committed meanwhile
// waits and is refused once the cancellation commits (pool_attribution_job_open); and a transaction that has checked
// its attribution (at SET CONSTRAINTS IMMEDIATE) holds the row, and the cancellation waits and is refused once the
// attribution commits (pool_loss_job_open).
func TestACancelledJobAndALossTakeTurnsAtCommitOnPostgres(t *testing.T) {
	ctx := context.Background()
	newWorld := func() usedWorld {
		u := newUsedWorld(t)
		u.record("cost-1", "AIR_FREIGHT", api.MethodChargeable, 100_000)
		u.apportion("apportion-1", 4, []int{1}, "SH-C")
		return u
	}
	loss := func(no int) string {
		return rawJoin(rawBump, rawApportionment(no), rawLine(no, "CHARGEABLE", 0, 0), rawShare(no, 1, "SH-A", "JOB-A", true, 50_000), rawShare(no, 2, "SH-B", "JOB-B", true, 35_000),
			rawAttribution(no, "SH-C", "JOB-C", 15_000, 15_000, 0, 0, ""))
	}
	const cancel = `UPDATE cargo_fin.freight_job SET state='CANCELLED',cancel_reason='x',cancelled_by='x',cancelled_at=now(),cancel_entered_at=now(),revision=revision+1
        WHERE tenant_id=$1 AND job_id='JOB-C'`
	open := func(u usedWorld) (*pgx.Conn, pgx.Tx, int) {
		conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var pid int
		if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, u.tenant); err == nil {
			err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx); conn.Close(ctx) })
		return conn, tx, pid
	}
	// the cancellation holds the job; the attribution waits at its commit, and is refused
	u := newWorld()
	_, tx, pid := open(u)
	if _, err := tx.Exec(ctx, cancel, u.tenant); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- u.rawRun(loss(2)) }()
	waitBlockedBy(t, pid, 1)
	if err, returned := returnsWithin(done, 200*time.Millisecond); returned {
		t.Fatalf("the attribution committed (%v) while the cancellation held the job", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("the cancellation: %v", err)
	}
	err, returned := returnsWithin(done, 10*time.Second)
	if state, constraint := sqlState(err); !returned || state != "23514" || constraint != "pool_attribution_job_open" {
		t.Fatalf("the attribution after the cancellation: %v (returned %v, state %s, constraint %s)", err, returned, state, constraint)
	}
	// the attribution holds the job; the cancellation waits, and is refused once the attribution commits
	u = newWorld()
	_, tx, pid = open(u)
	for _, s := range strings.Split(loss(2), ";\n") {
		if _, err := tx.Exec(ctx, s, u.tenant); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
		t.Fatalf("the attribution checked: %v", err)
	}
	go func() { done <- u.rawRun(cancel) }()
	waitBlockedBy(t, pid, 1)
	if err, returned := returnsWithin(done, 200*time.Millisecond); returned {
		t.Fatalf("the cancellation committed (%v) while the attribution held the job", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("the attribution: %v", err)
	}
	err, returned = returnsWithin(done, 10*time.Second)
	if state, constraint := sqlState(err); !returned || state != "23514" || constraint != "pool_loss_job_open" {
		t.Fatalf("the cancellation after the attribution: %v (returned %v, state %s, constraint %s)", err, returned, state, constraint)
	}
}

// A line by RETAINED takes attributions too (V2.93): once PHY reports the consolidation cancelled, every house is a
// former house (its shipment cancelled), each of them holds a former share and needs its attribution, and what remains
// of the line's cost is what the consolidation retains. The economics show the losses, the exception costs and the
// retained cost, and conserve.
func TestARetainedLineTakesItsAttributionsOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.changeBasis(func(b *phyapi.PhysicalCostBasis) {
		b.Revision, b.State, b.Houses = 8, "CANCELLED", nil
		for _, h := range []string{"SH-A", "SH-B", "SH-C"} {
			b.FormerHouses = append(b.FormerHouses, phyapi.FormerHouse{ShipmentID: h, End: "SHIPMENT_CANCELLED", Reason: "SHIPMENT_CANCELLED", EndedBy: "ops-9", EndedAt: u.hours(5),
				EndedEnteredAt: u.hours(5), ReworkFrom: phyapi.ReworkFromNone})
		}
	})
	cmd := u.apportionCmd("retain-none", 6, []int{1})
	cmd.Method, cmd.Reason = api.MethodRetained, "the consolidation was cancelled"
	_, err := u.module.PreviewConsolCostApportionment(u.ctx, cmd)
	u.refusedWith("a retained line with no attribution", err, "FIN_ATTRIBUTION_MISSING")
	cmd = u.apportionCmd("retain", 6, []int{1})
	cmd.Method, cmd.Reason = api.MethodRetained, "the consolidation was cancelled"
	cmd.Attributions = []api.AttributionStatement{statementOf("A", 0, 0, ""), statementOf("B", 0, 0, ""), statementOf("C", 5_000, 10_000, api.PartyOwnOperations)}
	r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, cmd))
	u.must(u.submit("retain-submit", "fin-2", int(r.Version), 6))
	a := u.ap.pool(u.pool).Apportionments[1]
	if la := a.Lines[0]; la.Method != api.MethodRetained || len(la.Shares) != 0 || la.ToAllocate.Minor != 0 || len(la.Attributions) != 3 || la.Attributions[2].Loss.Minor != 5_000 ||
		la.Attributions[2].Party != api.PartyOwnOperations || la.Amount.Minor != 100_000 {
		t.Fatalf("the retained line: %+v", la)
	}
	e, err := u.module.GetConsolProfit(u.ctx, api.GetConsolProfit{TenantID: u.tenant, ConsolidationID: "CONSOL-U"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Pooled.Minor != 100_000 || e.Retained.Minor != 85_000 || e.Losses.Minor != 5_000 || e.Exceptions.Minor != 10_000 || e.ExceptionsPending.Minor != 0 || e.Apportioned.Minor != 0 ||
		e.ToAllocated.Minor != 0 || e.FormerSharesPending.Minor != 0 || e.Unapportioned.Minor != 0 {
		t.Fatalf("the economics: %+v", e)
	}
	if c := u.jobProfit("JOB-C"); c.PoolCost.Minor != 5_000 || c.ReroutingLoss.Minor != 5_000 || c.FormerSharesPending.Minor != 0 {
		t.Fatalf("the profit of JOB-C: %+v", c)
	}
}

// FIN refuses to cancel a job that bears a share or a loss before it writes, naming the pool, the line and the way out,
// with the pool open and with the pool closed (where the way out starts with reopening it); the database's rule stays
// behind it.
func TestAJobBearingPoolCostIsRefusedBeforeItIsWrittenOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	refuse := func(name, job string, parts ...string) {
		t.Helper()
		err := u.cancelJob("cancel-"+name, job)
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_JOB_BEARS_POOL_COST") {
			t.Fatalf("%s: %v, want a business refusal", name, err)
		}
		for _, part := range append(parts, "pool P-U", "line 1") {
			if !strings.Contains(err.Error(), part) {
				t.Fatalf("%s: %v does not say %q", name, err, part)
			}
		}
		if state, _ := sqlState(err); state != "" {
			t.Fatalf("%s: a raw database error: %v", name, err)
		}
		if j := u.ap.job(job); j.State != "OPEN" {
			t.Fatalf("%s: the job is %s", name, j.State)
		}
	}
	refuse("pending-share", "JOB-C", "bears a share", "attribute its former share")
	refuse("live-share", "JOB-A", "bears a share")
	u.attribute("attribute-loss", 6, statementOf("C", 5_000, 10_000, api.PartyCarrier))
	refuse("loss", "JOB-C", "bears a re-routing loss", "apportion the line again with the loss changed")
	// the pool closed: the way out starts with reopening it
	u.must(u.module.RecordCreditorInvoice(u.ctx, api.RecordCreditorInvoice{CommandID: "invoice", TenantID: u.tenant, ActorID: "ap-1", InvoiceID: "CI-X", LegalEntityID: "LE",
		CreditorPartyID: "airline-mu", SupplierInvoiceNo: "X-1", InvoiceDate: u.hours(7).Format(time.DateOnly), Currency: "CNY",
		Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 100_000}}, OccurredAt: u.hours(7)}))
	u.must(u.module.MatchCreditorInvoice(u.ctx, u.match("CI-X", "ap-1", 7, onPool("P-U", 1, 1, 100_000, true))))
	u.must(u.module.CloseConsolCostPool(u.ctx, api.CloseConsolCostPool{CommandID: "close", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(),
		OccurredAt: u.hours(9)}))
	refuse("closed-loss", "JOB-C", "bears a re-routing loss", "reopen the pool first (ReopenConsolCostPool)")
	refuse("closed-share", "JOB-A", "bears a share", "reopen the pool first (ReopenConsolCostPool)")
}
