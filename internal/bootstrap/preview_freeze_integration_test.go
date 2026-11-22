//go:build integration

package bootstrap

import (
	"strings"
	"testing"
	"time"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// boxA is the owner's day: H01, H02 and H03 of 10, 15 and 5 W/M (10, 15 and 5 m³ over 312, 300 and 288 kg) stuffed
// into one 22G1 and sealed, and one cost line, the line's freight of CNY 3,000.00 by CHARGEABLE, in FIN's pool P-1,
// with the real ORG, RATE, COM, FUL, PHY and FIN.
func boxA(t *testing.T, prefix string) *oceanDay {
	t.Helper()
	d := newOceanDay(t, prefix, map[string]int64{"H01": 312, "H02": 300, "H03": 288}, map[string]int64{"H01": 10_000_000, "H02": 15_000_000, "H03": 5_000_000},
		[]string{"22G1"}, nil)
	d.stuff("X-1", 1)
	for _, h := range []string{"H01", "H02", "H03"} {
		d.load("X-1", h, 6, 0)
	}
	d.seal("X-1")
	_, err := d.fin.RecordConsolCost(d.ctx, finapi.RecordConsolCost{CommandID: "fin-freight", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision,
		ChargeCode: "OCEAN_FREIGHT", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: 300_000}},
		Reason: "the line's freight for the container", OccurredAt: d.hours(16)})
	d.must("the freight", err)
	return d
}

// later is a business time after everything the pool has recorded so far from hour 30 on, a minute for each preview and
// each apportionment.
func (d *oceanDay) later() time.Time {
	p := d.pool()
	return d.hours(30).Add(time.Duration(len(p.Previews)+len(p.Apportionments)) * time.Minute)
}

// previewBy previews the lines as the person; the receipt's Version is the preview's number.
func (d *oceanDay) previewBy(who, id string, notUsed ...string) (finapi.Receipt, error) {
	d.t.Helper()
	return d.fin.PreviewConsolCostApportionment(d.ctx, finapi.PreviewConsolCostApportionment{CommandID: id, TenantID: d.tenant, ActorID: who, PoolID: "P-1",
		ExpectedRevision: d.pool().Revision, NotUsed: notUsed, OccurredAt: d.later()})
}

func (d *oceanDay) submitBy(who, id string, preview int) (finapi.Receipt, error) {
	d.t.Helper()
	return d.fin.SubmitConsolCostApportionment(d.ctx, finapi.SubmitConsolCostApportionment{CommandID: id, TenantID: d.tenant, ActorID: who, PoolID: "P-1",
		ExpectedRevision: d.pool().Revision, PreviewNo: preview, OccurredAt: d.later()})
}

// jobBears is what the job of a house bears of the pool, in CNY minor units.
func (d *oceanDay) jobBears(house string) int64 {
	d.t.Helper()
	p, err := d.fin.GetJobProfit(d.ctx, finapi.GetJobProfit{TenantID: d.tenant, JobID: "JOB-" + house})
	d.must("the profit of JOB-"+house, err)
	return p.PoolCost.Minor
}

func (d *oceanDay) sharesOf(a finapi.PoolApportionment) (out []int64) {
	for _, s := range a.Lines[0].Shares {
		out = append(out, s.Amount.Minor)
	}
	return out
}

func sameFigures(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A pool with no rework is previewed and submitted and writes what the one-step ApportionConsolCosts wrote before
// (V2.91), on the owner's box A with the real contexts: CNY 3,000.00 on W/M 10 : 15 : 5 is 1,000.00, 1,500.00 and
// 500.00, at the stage ACTUAL on the basis revision PHY answered, as the apportionments of the ocean tests were (a
// preview changes no job's cost; the submission does).
func TestAPoolWithNoReworkIsApportionedAsBeforeWithEveryContext(t *testing.T) {
	d := boxA(t, "cargo-box-a-")
	before := d.basis()
	r, err := d.previewBy("fin-1", "fin-preview")
	d.must("the preview", err)
	p := d.pool()
	if len(p.Apportionments) != 0 || len(p.Previews) != 1 || p.Previews[0].Stage != finapi.BasisActual || p.Previews[0].BasisRevision != before.Revision ||
		!sameFigures(d.sharesOf(finapi.PoolApportionment{Lines: p.Previews[0].Lines}), 100_000, 150_000, 50_000) {
		t.Fatalf("the preview: %+v", p)
	}
	for _, h := range []string{"H01", "H02", "H03"} {
		if got := d.jobBears(h); got != 0 {
			t.Fatalf("a preview charged JOB-%s %d", h, got)
		}
	}
	_, err = d.submitBy("fin-1", "fin-submit", r.Version)
	d.must("the submission", err)
	p = d.pool()
	a := p.Apportionments[0]
	if len(p.Apportionments) != 1 || a.PreviewNo != 1 || a.By != "fin-1" || a.Stage != finapi.BasisActual || a.BasisRevision != before.Revision || p.InWork ||
		!sameFigures(d.sharesOf(a), 100_000, 150_000, 50_000) || a.Lines[0].Shares[0].Quantity != 10_000 || a.Lines[0].Shares[1].Quantity != 15_000 ||
		a.Lines[0].Shares[2].Quantity != 5_000 || a.Lines[0].Shares[0].Unit != "MWM" {
		t.Fatalf("the apportionment: %+v", p)
	}
	if d.jobBears("H01") != 100_000 || d.jobBears("H02") != 150_000 || d.jobBears("H03") != 50_000 {
		t.Fatalf("what the houses bear: %d %d %d", d.jobBears("H01"), d.jobBears("H02"), d.jobBears("H03"))
	}
	// the close passes as it did: the invoice is matched final and the apportionment is on the current basis
	_, err = d.fin.RecordCreditorInvoice(d.ctx, finapi.RecordCreditorInvoice{CommandID: "fin-invoice", TenantID: d.tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		LegalEntityID: "LE", CreditorPartyID: "line-1", SupplierInvoiceNo: "LINE-INV-2611-9", InvoiceDate: d.hours(19).Format("2006-01-02"), Currency: "CNY",
		Lines: []finapi.CreditorInvoiceLine{{ChargeCode: "OCEAN_FREIGHT", Amount: 300_000}}, OccurredAt: d.hours(19)})
	d.must("the invoice", err)
	invoice, err := d.fin.GetCreditorInvoice(d.ctx, finapi.GetCreditorInvoice{TenantID: d.tenant, InvoiceID: "CI-LINE-1"})
	d.must("the invoice read", err)
	_, err = d.fin.MatchCreditorInvoice(d.ctx, finapi.MatchCreditorInvoice{CommandID: "fin-match", TenantID: d.tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		ExpectedRevision: invoice.Revision, Matches: []finapi.CreditorMatch{{LineNo: 1, Target: finapi.AccrualRef{Kind: "POOL_COST", ID: "P-1", LineNo: 1}, Amount: 300_000, Final: true}},
		OccurredAt: d.hours(20)})
	d.must("the match", err)
	_, err = d.fin.CloseConsolCostPool(d.ctx, finapi.CloseConsolCostPool{CommandID: "fin-close", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: d.pool().Revision, OccurredAt: d.hours(40)})
	d.must("the close", err)
}

// The owner's box A through a rework (V2.91), with the real ORG, RATE, COM, FUL, PHY and FIN. A preview and a
// submission before the rework (1,000.00 / 1,500.00 / 500.00), and a second preview left. PHY opens the rework (asked
// by fin-asker): a preview, a submission of the preview kept and the close are refused naming the container and the way
// out, and the pool shows the apportionment submitted, in work. PHY unloads H03 and seals the container again: the
// preview kept is refused as stale; ops-1 previews again with H03 left out (it is attached with nothing loaded),
// 1,200.00 / 1,800.00 / 0; the person who asked for the rework is refused its submission and another finance user's is
// accepted; the asker's next submission is accepted, the rework being carried.
func TestAPoolFrozenForAReworkIsConfirmedByAnotherPerson(t *testing.T) {
	d := boxA(t, "cargo-box-a-rework-")
	first, err := d.previewBy("fin-1", "fin-preview-1")
	d.must("the first preview", err)
	_, err = d.submitBy("fin-1", "fin-submit-1", first.Version)
	d.must("the first submission", err)
	kept, err := d.previewBy("fin-1", "fin-preview-kept")
	d.must("the preview kept", err)
	if p := d.pool(); len(p.Apportionments) != 1 || len(p.Previews) != 2 || !sameFigures(d.sharesOf(p.Apportionments[0]), 100_000, 150_000, 50_000) {
		t.Fatalf("before the rework: %+v", p)
	}

	_, err = d.phy.CloseCargoLot(d.ctx, phyapi.CloseCargoLot{CommandID: "close-lot-h03", TenantID: d.tenant, ActorID: "cfs-1", LotID: "L-H03", ExpectedRevision: 3, OccurredAt: d.hours(16)})
	d.must("close the lot of H03", err)
	_, err = d.phy.OpenStuffingForRework(d.ctx, phyapi.OpenStuffingForRework{CommandID: "rework-1", TenantID: d.tenant, ActorID: "fin-asker", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Reason: "the consignee of H03 withdrew its cargo", OccurredAt: d.hours(22)})
	d.must("open the container for rework", err)
	d.revision["X-1"]++

	// frozen: the preview, the submission and the close are refused naming the container and the way out
	inWork := func(what string, err error) {
		t.Helper()
		for _, want := range []string{"FIN_POOL_IN_WORK", "load unit 1 (stuffing X-1)", "wait for PHY to seal the container again or to give its stuffing up"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: %v, want it to say %q", what, err, want)
			}
		}
	}
	_, err = d.previewBy("ops-1", "fin-preview-open")
	inWork("a preview", err)
	_, err = d.submitBy("fin-1", "fin-submit-open", kept.Version)
	inWork("the submission of the preview kept", err)
	_, err = d.fin.CloseConsolCostPool(d.ctx, finapi.CloseConsolCostPool{CommandID: "fin-close-open", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: d.pool().Revision, OccurredAt: d.hours(32)})
	inWork("the close", err)
	p := d.pool()
	if !p.InWork || len(p.WorkUnits) != 1 || p.WorkUnits[0] != (finapi.WorkUnit{UnitNo: 1, ExecutionID: "X-1"}) || len(p.Apportionments) != 1 || p.State != finapi.PoolOpen ||
		!sameFigures(d.sharesOf(p.Apportionments[0]), 100_000, 150_000, 50_000) || d.jobBears("H03") != 50_000 {
		t.Fatalf("the pool in work shows the last submitted apportionment: %+v", p)
	}
	profit, err := d.fin.GetConsolProfit(d.ctx, finapi.GetConsolProfit{TenantID: d.tenant, ConsolidationID: "C-1"})
	d.must("the consolidation's economics", err)
	if !profit.InWork || profit.Apportioned.Minor != 300_000 {
		t.Fatalf("the economics in work: %+v", profit)
	}

	// H03 leaves and the container is sealed again
	_, err = d.phy.UnloadHouseForRework(d.ctx, phyapi.UnloadHouseForRework{CommandID: "unload-h03", TenantID: d.tenant, ActorID: "wh-1", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], ShipmentID: "SH-H03", Evidence: phyapi.Evidence{Source: "CFS-WMS", Reference: "UNSTUFF-H03", OccurredAt: d.hours(24)}, OccurredAt: d.hours(24)})
	d.must("unload H03", err)
	d.revision["X-1"]++
	_, err = d.previewBy("ops-1", "fin-preview-unloaded", "SH-H03")
	inWork("a preview with H03 named out, the container still open", err)
	_, err = d.phy.CloseLoadExecution(d.ctx, phyapi.CloseLoadExecution{CommandID: "reseal-1", TenantID: d.tenant, ActorID: "cfs-2", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Evidence: d.evidence(phyapi.EvidenceSeal, "SEAL-X-1-NEW", 25), OccurredAt: d.hours(25)})
	d.must("seal again", err)
	if p = d.pool(); p.InWork || len(p.WorkUnits) != 0 {
		t.Fatalf("sealed again: %+v", p)
	}

	// the preview made before is stale
	_, err = d.submitBy("fin-1", "fin-submit-kept", kept.Version)
	if err == nil || !strings.Contains(err.Error(), "FIN_PREVIEW_STALE") || !strings.Contains(err.Error(), "packing.1 changed") || !strings.Contains(err.Error(), `house."SH-H03" changed`) ||
		!strings.Contains(err.Error(), "preview again") {
		t.Fatalf("the preview made before the rework: %v", err)
	}
	// a new preview by ops-1, with H03 left out; the asker may not submit it, another finance user may
	fresh, err := d.previewBy("ops-1", "fin-preview-2", "SH-H03")
	d.must("the new preview", err)
	p = d.pool()
	if v := p.Previews[len(p.Previews)-1]; v.By != "ops-1" || v.Stage != finapi.BasisActual || !sameFigures(d.sharesOf(finapi.PoolApportionment{Lines: v.Lines}), 120_000, 180_000, 0) {
		t.Fatalf("the new preview: %+v", v)
	}
	_, err = d.submitBy("fin-asker", "fin-submit-asker", fresh.Version)
	if err == nil || !strings.Contains(err.Error(), "FIN_CONFIRMER_ASKED_REWORK") || !strings.Contains(err.Error(), "fin-asker asked for rework 1 of load unit 1 (stuffing X-1)") ||
		!strings.Contains(err.Error(), "another finance user submits") {
		t.Fatalf("the person who asked for the rework: %v", err)
	}
	if p = d.pool(); len(p.Apportionments) != 1 || d.jobBears("H03") != 50_000 {
		t.Fatalf("the asker's submission was written: %+v", p)
	}
	_, err = d.submitBy("fin-1", "fin-submit-2", fresh.Version)
	d.must("another finance user's submission", err)
	p = d.pool()
	a := p.Apportionments[len(p.Apportionments)-1]
	if len(p.Apportionments) != 2 || a.By != "fin-1" || a.PreviewNo != int(fresh.Version) || a.Stage != finapi.BasisActual || !sameFigures(d.sharesOf(a), 120_000, 180_000, 0) ||
		!a.Lines[0].Shares[2].NotUsed || p.Previews[fresh.Version-1].SubmittedAs != 2 {
		t.Fatalf("the apportionment after the rework: %+v", p)
	}
	if d.jobBears("H01") != 120_000 || d.jobBears("H02") != 180_000 || d.jobBears("H03") != 0 {
		t.Fatalf("what the houses bear after the rework: %d %d %d", d.jobBears("H01"), d.jobBears("H02"), d.jobBears("H03"))
	}
	// the rework is carried by that submission: the asker's next one is accepted
	next, err := d.previewBy("ops-1", "fin-preview-3")
	d.must("the next preview", err)
	if _, err = d.submitBy("fin-asker", "fin-submit-3", next.Version); err != nil {
		t.Fatalf("the asker's submission after the rework was carried: %v", err)
	}
}

// A rework given up leaves no packing list in PHY's cost basis (a cancelled stuffing has none, as the stuffing given up
// empty in TestAnEmptyStuffingIsGivenUpAndThePoolClosesWithPHYAndFIN), and until V2.91 FIN could not read who asked for
// it (V2.91 called this test TestAReworkGivenUpLeavesNothingForFINToReadWithEveryContext): the person who asked for the
// rework was not refused. Since V2.92 the basis tells of the stuffing given up and its reworks (GivenUpStuffings), so
// the pool is not in work, the preview made before is stale, and the person who asked for the rework is refused, as for
// any ended rework (run with the real contexts).
func TestTheAskerOfAReworkGivenUpIsRefusedAndAnotherFinanceUserAcceptedWithEveryContext(t *testing.T) {
	d := newOceanDay(t, "cargo-box-a-given-up-", map[string]int64{"H01": 312, "H02": 300}, map[string]int64{"H01": 10_000_000, "H02": 15_000_000}, []string{"22G1"}, nil)
	d.stuff("X-1", 1)
	for _, h := range []string{"H01", "H02"} {
		d.load("X-1", h, 6, 0)
	}
	d.seal("X-1")
	_, err := d.fin.RecordConsolCost(d.ctx, finapi.RecordConsolCost{CommandID: "fin-freight", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision,
		ChargeCode: "OCEAN_FREIGHT", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: 300_000}},
		Reason: "the line's freight for the container", OccurredAt: d.hours(16)})
	d.must("the freight", err)
	kept, err := d.previewBy("fin-1", "fin-preview-kept")
	d.must("the preview", err)
	_, err = d.phy.OpenStuffingForRework(d.ctx, phyapi.OpenStuffingForRework{CommandID: "rework-1", TenantID: d.tenant, ActorID: "fin-asker", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Reason: "the cargo is not going", OccurredAt: d.hours(22)})
	d.must("open the container for rework", err)
	d.revision["X-1"]++
	if _, err = d.previewBy("ops-1", "fin-preview-open"); err == nil || !strings.Contains(err.Error(), "FIN_POOL_IN_WORK") {
		t.Fatalf("a preview in rework: %v", err)
	}
	for _, h := range []string{"H01", "H02"} {
		_, err = d.phy.CloseCargoLot(d.ctx, phyapi.CloseCargoLot{CommandID: "close-lot-" + h, TenantID: d.tenant, ActorID: "cfs-1", LotID: "L-" + h, ExpectedRevision: 3, OccurredAt: d.hours(16)})
		d.must("close the lot of "+h, err)
		_, err = d.phy.UnloadHouseForRework(d.ctx, phyapi.UnloadHouseForRework{CommandID: "unload-" + h, TenantID: d.tenant, ActorID: "wh-1", ExecutionID: "X-1",
			ExpectedRevision: d.revision["X-1"], ShipmentID: "SH-" + h, Evidence: phyapi.Evidence{Source: "CFS-WMS", Reference: "UNSTUFF-" + h, OccurredAt: d.hours(24)}, OccurredAt: d.hours(24)})
		d.must("unload "+h, err)
		d.revision["X-1"]++
	}
	_, err = d.phy.CancelLoadExecution(d.ctx, phyapi.CancelLoadExecution{CommandID: "give-up", TenantID: d.tenant, ActorID: "cfs-2", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Reason: "the cargo is not going", OccurredAt: d.hours(26)})
	d.must("give the stuffing up", err)
	b := d.basis()
	if len(b.PackingLists) != 0 || b.LoadUnits[0].Status != phyapi.UnitStatusPlanned || b.LoadUnits[0].ExecutionState != "" {
		t.Fatalf("PHY's basis after the stuffing is given up: %+v, %+v", b.PackingLists, b.LoadUnits[0])
	}
	if p := d.pool(); p.InWork {
		t.Fatalf("a rework given up: %+v", p)
	}
	_, err = d.submitBy("fin-1", "fin-submit-kept", kept.Version)
	if err == nil || !strings.Contains(err.Error(), "FIN_PREVIEW_STALE") || !strings.Contains(err.Error(), "packing.1") {
		t.Fatalf("the preview made before: %v", err)
	}
	if len(b.GivenUpStuffings) != 1 || b.GivenUpStuffings[0].ExecutionID != "X-1" || len(b.GivenUpStuffings[0].Reworks) != 1 ||
		b.GivenUpStuffings[0].Reworks[0].AskedBy != "fin-asker" || b.GivenUpStuffings[0].Reworks[0].End != phyapi.ReworkGivenUp {
		t.Fatalf("PHY's basis tells of the stuffing given up: %+v", b.GivenUpStuffings)
	}
	fresh, err := d.previewBy("ops-1", "fin-preview-planned")
	d.must("a preview on the plan", err)
	_, err = d.submitBy("fin-asker", "fin-submit-asker", fresh.Version)
	if err == nil || !strings.Contains(err.Error(), "FIN_CONFIRMER_ASKED_REWORK") || !strings.Contains(err.Error(), "fin-asker asked for rework 1 of load unit 1 (given-up stuffing X-1)") {
		t.Fatalf("the person who asked for a rework that was given up: %v", err)
	}
	if _, err = d.submitBy("fin-1", "fin-submit-other", fresh.Version); err != nil {
		t.Fatalf("another finance user: %v", err)
	}
}
