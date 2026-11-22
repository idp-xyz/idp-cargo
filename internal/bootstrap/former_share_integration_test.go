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

// Finance attributes a departed house's former share of the original consolidation's costs (V2.93), on the owner's box
// A with the real ORG, RATE, COM, FUL, PHY and FIN: C-1 with H01, H02 and H03 of 10, 15 and 5 W/M, freight CNY 3,000.00
// apportioned 1,000.00, 1,500.00 and 500.00. H03 is unloaded in a rework asked by ops-1, the container is sealed again
// and ops-9 transfers H03 to C-2.

// transferH03 makes the day of the owner's example: the rework asked by ops-1, the container sealed again, C-2 created
// and H03 transferred to it by ops-9 at hour 26, and C-2's pool P-2 with one cost of CNY 100.01 apportioned on H03,
// which is attached there with nothing loaded.
func (d *oceanDay) transferH03() {
	d.t.Helper()
	d.reworkBy("rework-1", "ops-1", 22)
	d.unloadForRework("H03", 23)
	d.resealAt("reseal-1", 24)
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	departs := d.at.Add(72 * time.Hour)
	_, err := d.phy.CreateConsolidation(d.ctx, phyapi.CreateConsolidation{CommandID: "consol-2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", Mode: phyapi.ModeSea,
		Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"), PlannedDeparture: departs.Add(6 * time.Hour),
		HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: d.hours(8)})
	d.must("create C-2", err)
	_, err = d.phy.PlanLoadUnit(d.ctx, phyapi.PlanLoadUnit{CommandID: "plan-c2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", ExpectedRevision: 1,
		Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: d.hours(8)})
	d.must("plan the unit of C-2", err)
	_, err = d.phy.TransferHouse(d.ctx, phyapi.TransferHouse{CommandID: "transfer-h03", TenantID: d.tenant, ActorID: "ops-9", ConsolidationID: "C-1", ToConsolidationID: "C-2",
		ShipmentID: "SH-H03", ExpectedMembershipRevision: 1, Reason: phyapi.ReasonWrongConsol, OccurredAt: d.hours(26)})
	d.must("transfer H03", err)
	_, err = d.fin.OpenConsolCostPool(d.ctx, finapi.OpenConsolCostPool{CommandID: "fin-pool2", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-2", ConsolidationID: "C-2",
		OccurredAt: d.hours(33)})
	d.must("open P-2", err)
	_, err = d.fin.RecordConsolCost(d.ctx, finapi.RecordConsolCost{CommandID: "fin-cost2", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-2", ExpectedRevision: 1,
		ChargeCode: "OCEAN_FREIGHT", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: 10_001}},
		Reason: "the line's charge for the second container", OccurredAt: d.hours(33)})
	d.must("cost of P-2", err)
	p2, err := d.fin.GetConsolCostPool(d.ctx, finapi.GetConsolCostPool{TenantID: d.tenant, PoolID: "P-2"})
	d.must("P-2", err)
	_, err = previewAndSubmit(d.ctx, d.fin, finapi.PreviewConsolCostApportionment{CommandID: "fin-apportion-p2", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-2",
		ExpectedRevision: p2.Revision, LineNos: []int{1}, OccurredAt: d.hours(34)})
	d.must("apportion P-2", err)
}

func (d *oceanDay) jobProfit(house string) finapi.JobProfit {
	d.t.Helper()
	p, err := d.fin.GetJobProfit(d.ctx, finapi.GetJobProfit{TenantID: d.tenant, JobID: "JOB-" + house})
	d.must("the profit of JOB-"+house, err)
	return p
}

func (d *oceanDay) consolProfit() finapi.ConsolProfit {
	d.t.Helper()
	p, err := d.fin.GetConsolProfit(d.ctx, finapi.GetConsolProfit{TenantID: d.tenant, ConsolidationID: "C-1"})
	d.must("C-1's economics", err)
	return p
}

// previewWith previews line 1 as the person with the attributions stated.
func (d *oceanDay) previewWith(who, id string, attributions ...finapi.AttributionStatement) (finapi.Receipt, error) {
	d.t.Helper()
	return d.fin.PreviewConsolCostApportionment(d.ctx, finapi.PreviewConsolCostApportionment{CommandID: id, TenantID: d.tenant, ActorID: who, PoolID: "P-1",
		ExpectedRevision: d.pool().Revision, LineNos: []int{1}, Attributions: attributions, OccurredAt: d.later()})
}

func statement(house string, loss, exception int64, party string) finapi.AttributionStatement {
	return finapi.AttributionStatement{LineNo: 1, ShipmentID: "SH-" + house, Loss: loss, Exception: exception, Party: party, Voucher: "CLAIM-1"}
}

// settleFreight records the line's invoice for the freight and matches it final; closeP1 closes the pool.
func (d *oceanDay) settleFreight() {
	d.t.Helper()
	_, err := d.fin.RecordCreditorInvoice(d.ctx, finapi.RecordCreditorInvoice{CommandID: "fin-invoice", TenantID: d.tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		LegalEntityID: "LE", CreditorPartyID: "line-1", SupplierInvoiceNo: "LINE-INV-2611-9", InvoiceDate: d.hours(19).Format(time.DateOnly), Currency: "CNY",
		Lines: []finapi.CreditorInvoiceLine{{ChargeCode: "OCEAN_FREIGHT", Amount: 300_000}}, OccurredAt: d.hours(19)})
	d.must("the invoice", err)
	invoice, err := d.fin.GetCreditorInvoice(d.ctx, finapi.GetCreditorInvoice{TenantID: d.tenant, InvoiceID: "CI-LINE-1"})
	d.must("the invoice read", err)
	_, err = d.fin.MatchCreditorInvoice(d.ctx, finapi.MatchCreditorInvoice{CommandID: "fin-match", TenantID: d.tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		ExpectedRevision: invoice.Revision, Matches: []finapi.CreditorMatch{{LineNo: 1, Target: finapi.AccrualRef{Kind: "POOL_COST", ID: "P-1", LineNo: 1}, Amount: 300_000, Final: true}},
		OccurredAt: d.hours(20)})
	d.must("the match", err)
}

func (d *oceanDay) closeP1(id string) error {
	d.t.Helper()
	_, err := d.fin.CloseConsolCostPool(d.ctx, finapi.CloseConsolCostPool{CommandID: id, TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: d.pool().Revision, OccurredAt: d.hours(45)})
	return err
}

func said(t *testing.T, what string, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: accepted, want it refused saying %q", what, parts)
	}
	for _, part := range parts {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("%s: %v, want it to say %q", what, err, part)
		}
	}
}

// TestAFormerShareIsPendingUntilFinanceAttributesItWithEveryContext is the owner's example. Before finance confirms,
// H03's former share of 500.00 stands in C-1's pool and is nobody's cost: H03's job bears only what C-2's pool gave it
// (100.01) and lists the 500.00 apart, C-1's economics list it apart, and C-1 cannot close, naming H03. A preview of
// line 1 with no attribution is refused naming the default, 500.00. With the loss 100.00 and the exception cost 200.00
// (party pending) under the voucher CLAIM-1, ops-1 who asked for the rework and ops-9 who transferred the house are
// refused as confirmers and fin-1 is accepted: H01 1,080.00 and H02 1,620.00, H03 bears the loss (in its profit) and
// C-1 holds the exception cost pending; C-1 closes.
func TestAFormerShareIsPendingUntilFinanceAttributesItWithEveryContext(t *testing.T) {
	d := boxA(t, "cargo-box-a-attributed-")
	first, err := d.previewBy("fin-1", "fin-pre-1")
	d.must("the first preview", err)
	_, err = d.submitBy("fin-1", "fin-sub-1", first.Version)
	d.must("the first submission", err)
	if !sameFigures(d.sharesOf(d.pool().Apportionments[0]), 100_000, 150_000, 50_000) {
		t.Fatalf("before the rework: %+v", d.pool().Apportionments)
	}
	d.transferH03()

	// C-2's pool apportions H03 normally: the one house bears all of CNY 100.01
	p2, err := d.fin.GetConsolCostPool(d.ctx, finapi.GetConsolCostPool{TenantID: d.tenant, PoolID: "P-2"})
	d.must("P-2", err)
	if len(p2.Apportionments) != 1 || p2.Apportionments[0].Lines[0].Shares[0].ShipmentID != "SH-H03" || p2.Apportionments[0].Lines[0].Shares[0].Amount.Minor != 10_001 ||
		len(p2.Apportionments[0].Lines[0].Attributions) != 0 {
		t.Fatalf("C-2's apportionment: %+v", p2.Apportionments)
	}

	// before confirmation: the 500.00 is pending, in nobody's cost, and listed apart
	h03 := d.jobProfit("H03")
	if h03.PoolCost.Minor != 10_001 || h03.Profit.Minor != h03.Revenue.Minor-h03.Cost.Minor-10_001 || h03.FormerSharesPending.Minor != 50_000 || len(h03.FormerShares) != 1 ||
		h03.FormerShares[0].PoolID != "P-1" || h03.FormerShares[0].LineNo != 1 || h03.FormerShares[0].Local.Minor != 50_000 || len(h03.PoolShares) != 1 ||
		h03.PoolShares[0].PoolID != "P-2" || h03.ReroutingLoss.Minor != 0 || len(h03.ReroutingLosses) != 0 {
		t.Fatalf("the profit of JOB-H03 before the confirmation: %+v", h03)
	}
	c1 := d.consolProfit()
	if c1.Pooled.Minor != 300_000 || c1.Apportioned.Minor != 250_000 || c1.FormerSharesPending.Minor != 50_000 || c1.Unapportioned.Minor != 0 || c1.Losses.Minor != 0 ||
		c1.Exceptions.Minor != 0 || c1.ToAllocated.Minor != 0 || len(c1.Houses) != 3 || c1.Houses[0].PoolCost.Minor != 100_000 || c1.Houses[1].PoolCost.Minor != 150_000 ||
		c1.Houses[2].ShipmentID != "SH-H03" || c1.Houses[2].PoolCost.Minor != 0 {
		t.Fatalf("C-1's economics before the confirmation: %+v", c1)
	}
	if v := d.pool().Attributions; len(v) != 1 || v[0].LineNo != 1 || len(v[0].FormerShares) != 1 || v[0].FormerShares[0].ShipmentID != "SH-H03" || v[0].FormerShares[0].Amount.Minor != 50_000 ||
		v[0].FormerShares[0].End != phyapi.FormerTransferred || len(v[0].Losses) != 0 {
		t.Fatalf("the pool before the confirmation: %+v", v)
	}
	d.settleFreight()
	said(t, "the close of C-1", d.closeP1("fin-close-early"), "FIN_POOL_NOT_CLOSABLE", "cost line 1 holds a former share of house SH-H03", "attribute it")

	// a preview of line 1 with no attribution is refused, naming the default loss
	_, err = d.previewWith("ops-2", "fin-pre-none")
	said(t, "no attribution", err, "FIN_ATTRIBUTION_MISSING", "line 1", "house SH-H03", "50000 CNY", "the loss it would bear by default", "state who bears it")

	// the attribution: loss 100.00, exception cost 200.00 pending, the voucher CLAIM-1
	pre, err := d.previewWith("ops-2", "fin-pre-attr", statement("H03", 10_000, 20_000, finapi.PartyPending))
	d.must("the preview with the attribution", err)
	_, err = d.submitBy("ops-1", "fin-sub-ops-1", pre.Version)
	said(t, "ops-1 asked for the rework", err, "FIN_ATTRIBUTION_CONFIRMER", "ops-1 asked for rework 1 of load unit 1 (stuffing X-1), which unloaded house SH-H03",
		"the attribution of house SH-H03 on line 1 is new or changed", "another finance user submits")
	_, err = d.submitBy("ops-9", "fin-sub-ops-9", pre.Version)
	said(t, "ops-9 transferred the house", err, "FIN_ATTRIBUTION_CONFIRMER", "ops-9 ended the place of house SH-H03 in the consolidation (TRANSFERRED)")
	if p := d.pool(); len(p.Apportionments) != 1 {
		t.Fatalf("a refused submission was written: %+v", p.Apportionments)
	}
	_, err = d.submitBy("fin-1", "fin-sub-fin-1", pre.Version)
	d.must("fin-1's submission", err)

	p := d.pool()
	a := p.Apportionments[1]
	x := a.Lines[0].Attributions
	if len(p.Apportionments) != 2 || a.By != "fin-1" || !sameFigures(d.sharesOf(a), 108_000, 162_000) || len(a.Lines[0].Shares) != 2 || len(x) != 1 || x[0].ShipmentID != "SH-H03" ||
		x[0].JobID != "JOB-H03" || x[0].Loss.Minor != 10_000 || x[0].LossLocal.Minor != 10_000 || x[0].Exception.Minor != 20_000 || x[0].ExceptionLocal.Minor != 20_000 ||
		x[0].Party != finapi.PartyPending || x[0].Voucher != "CLAIM-1" || x[0].Carried || !x[0].DepartedAt.Equal(d.hours(26)) || a.Lines[0].ToAllocate.Minor != 0 {
		t.Fatalf("the apportionment with the attribution: %+v", p.Apportionments)
	}
	if v := p.Attributions; len(v) != 1 || len(v[0].Losses) != 1 || v[0].Losses[0] != (finapi.HouseLoss{ShipmentID: "SH-H03", JobID: "JOB-H03", Amount: kernel.Money{Currency: "CNY", Minor: 10_000},
		Local: kernel.Money{Currency: "CNY", Minor: 10_000}}) || len(v[0].Exceptions) != 0 || v[0].ExceptionPending.Minor != 20_000 || len(v[0].FormerShares) != 0 || v[0].ToAllocate.Minor != 0 {
		t.Fatalf("the pool after the attribution: %+v", v)
	}
	// H03's job bears the loss, labelled, and no former share is left; C-1 holds the exception cost pending
	h03 = d.jobProfit("H03")
	if h03.PoolCost.Minor != 20_001 || h03.ReroutingLoss.Minor != 10_000 || len(h03.ReroutingLosses) != 1 || h03.ReroutingLosses[0].PoolID != "P-1" || h03.ReroutingLosses[0].LineNo != 1 ||
		h03.ReroutingLosses[0].Voucher != "CLAIM-1" || !h03.ReroutingLosses[0].DepartedAt.Equal(d.hours(26)) || h03.FormerSharesPending.Minor != 0 || len(h03.FormerShares) != 0 ||
		h03.Profit.Minor != h03.Revenue.Minor-h03.Cost.Minor-20_001 {
		t.Fatalf("the profit of JOB-H03 after the attribution: %+v", h03)
	}
	c1 = d.consolProfit()
	if c1.Pooled.Minor != 300_000 || c1.Apportioned.Minor != 270_000 || c1.Losses.Minor != 10_000 || c1.Exceptions.Minor != 20_000 || c1.ExceptionsPending.Minor != 20_000 ||
		c1.FormerSharesPending.Minor != 0 || c1.ToAllocated.Minor != 0 || c1.Unapportioned.Minor != 0 || c1.Houses[0].PoolCost.Minor != 108_000 || c1.Houses[1].PoolCost.Minor != 162_000 ||
		c1.Houses[2].PoolCost.Minor != 10_000 || len(c1.Lines) != 1 || c1.Lines[0].ExceptionPending.Minor != 20_000 {
		t.Fatalf("C-1's economics after the attribution: %+v", c1)
	}
	// the party is pending and does not stop the close
	if err = d.closeP1("fin-close-late"); err != nil {
		t.Fatalf("the close of C-1 with the exception cost pending: %v", err)
	}
}

// The other outcome: no loss and no exception cost, all of it to the remaining cargo (1,200.00 and 1,800.00), still
// under a voucher; H03's job bears only what C-2's pool gave it and C-1 closes.
func TestAFormerShareAllToTheRemainingCargoWithEveryContext(t *testing.T) {
	d := boxA(t, "cargo-box-a-attributed-rest-")
	first, err := d.previewBy("fin-1", "fin-pre-1")
	d.must("the first preview", err)
	_, err = d.submitBy("fin-1", "fin-sub-1", first.Version)
	d.must("the first submission", err)
	d.transferH03()

	novoucher := statement("H03", 0, 0, "")
	novoucher.Voucher = ""
	_, err = d.previewWith("ops-2", "fin-pre-novoucher", novoucher)
	said(t, "no voucher", err, "voucher")
	pre, err := d.previewWith("ops-2", "fin-pre-rest", statement("H03", 0, 0, ""))
	d.must("the preview", err)
	_, err = d.submitBy("fin-1", "fin-sub-rest", pre.Version)
	d.must("the submission", err)
	p := d.pool()
	a := p.Apportionments[1]
	if !sameFigures(d.sharesOf(a), 120_000, 180_000) || len(a.Lines[0].Attributions) != 1 || a.Lines[0].Attributions[0].Loss.Minor != 0 || a.Lines[0].Attributions[0].Exception.Minor != 0 ||
		a.Lines[0].Attributions[0].Party != "" || a.Lines[0].Attributions[0].Voucher != "CLAIM-1" {
		t.Fatalf("all to the remaining cargo: %+v", a)
	}
	if h03 := d.jobProfit("H03"); h03.PoolCost.Minor != 10_001 || h03.ReroutingLoss.Minor != 0 || len(h03.ReroutingLosses) != 0 || h03.FormerSharesPending.Minor != 0 {
		t.Fatalf("the profit of JOB-H03: %+v", h03)
	}
	c1 := d.consolProfit()
	if c1.Apportioned.Minor != 300_000 || c1.Losses.Minor != 0 || c1.Exceptions.Minor != 0 || c1.Unapportioned.Minor != 0 || c1.Houses[0].PoolCost.Minor != 120_000 ||
		c1.Houses[1].PoolCost.Minor != 180_000 || c1.Houses[2].PoolCost.Minor != 0 {
		t.Fatalf("C-1's economics: %+v", c1)
	}
	d.settleFreight()
	if err = d.closeP1("fin-close"); err != nil {
		t.Fatalf("the close: %v", err)
	}
}

// Every house leaves C-1: H03 in the first rework (asked by ops-1), H01 and H02 in a second (asked by ask-2), after
// which the stuffing is given up and ops-9 detaches the three. No house is live: what the attributions leave of line 1
// is to be allocated, and the close is refused with the way out; attributing the rest as the exception cost of a house
// that left (a correction, with its reason) leaves nothing to be allocated, and the pool closes.
func TestWhenEveryHouseLeavesTheRemainderIsToBeAllocatedWithEveryContext(t *testing.T) {
	d := boxA(t, "cargo-box-a-everyone-")
	first, err := d.previewBy("fin-1", "fin-pre-1")
	d.must("the first preview", err)
	_, err = d.submitBy("fin-1", "fin-sub-1", first.Version)
	d.must("the first submission", err)
	d.reworkBy("rework-1", "ops-1", 22)
	d.unloadForRework("H03", 23)
	d.resealAt("reseal-1", 24)
	d.reworkBy("rework-2", "ask-2", 25)
	d.unloadForRework("H01", 26)
	d.unloadForRework("H02", 27)
	_, err = d.phy.CancelLoadExecution(d.ctx, phyapi.CancelLoadExecution{CommandID: "give-up", TenantID: d.tenant, ActorID: "cfs-2", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Reason: "the booking is rolled", OccurredAt: d.hours(28)})
	d.must("give the stuffing up", err)
	for i, h := range []string{"H01", "H02", "H03"} {
		_, err = d.phy.DetachHouse(d.ctx, phyapi.DetachHouse{CommandID: "detach-" + h, TenantID: d.tenant, ActorID: "ops-9", ConsolidationID: "C-1", ShipmentID: "SH-" + h,
			ExpectedMembershipRevision: 1, Reason: phyapi.ReasonSpaceShortage, OccurredAt: d.hours(29 + i)})
		d.must("detach "+h, err)
	}
	if b := d.basis(); len(b.Houses) != 0 || len(b.FormerHouses) != 3 {
		t.Fatalf("no house is live: %+v", b)
	}
	d.settleFreight()
	said(t, "the close with the former shares not attributed", d.closeP1("fin-close-1"), "FIN_POOL_NOT_CLOSABLE", "cost line 1 holds a former share of house SH-H01")

	// a line that costs nothing is not apportioned while no house is live: the preview of every line says so and names
	// the way out
	_, err = d.fin.RecordConsolCost(d.ctx, finapi.RecordConsolCost{CommandID: "fin-zero", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision,
		ChargeCode: "ZERO_FEE", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: 0}},
		Reason: "a fee that was waived", OccurredAt: d.later()})
	d.must("the zero line", err)
	_, err = d.fin.PreviewConsolCostApportionment(d.ctx, finapi.PreviewConsolCostApportionment{CommandID: "fin-pre-zero", TenantID: d.tenant, ActorID: "ops-2", PoolID: "P-1",
		ExpectedRevision: d.pool().Revision, Attributions: []finapi.AttributionStatement{statement("H01", 0, 0, ""), statement("H02", 0, 0, ""),
			statement("H03", 10_000, 20_000, finapi.PartyCarrier)}, OccurredAt: d.later()})
	said(t, "the zero line with no house live", err, "cost line 2 costs nothing", "preview the other lines")
	pre, err := d.previewWith("ops-2", "fin-pre-all", statement("H01", 0, 0, ""), statement("H02", 0, 0, ""), statement("H03", 10_000, 20_000, finapi.PartyCarrier))
	d.must("the preview of all three", err)
	_, err = d.submitBy("fin-1", "fin-sub-all", pre.Version)
	d.must("the submission", err)
	p := d.pool()
	la := p.Apportionments[1].Lines[0]
	if len(la.Shares) != 0 || la.ToAllocate.Minor != 270_000 || la.ToAllocateLocal.Minor != 270_000 || len(la.Attributions) != 3 || la.Amount.Minor != 300_000 {
		t.Fatalf("the line with no live house: %+v", la)
	}
	if v := p.Attributions; len(v) != 1 || v[0].ToAllocate.Minor != 270_000 || len(v[0].Exceptions) != 1 || v[0].Exceptions[0].Party != finapi.PartyCarrier || v[0].Exceptions[0].Amount.Minor != 20_000 {
		t.Fatalf("the pool's view: %+v", v)
	}
	c1 := d.consolProfit()
	if c1.ToAllocated.Minor != 270_000 || c1.Losses.Minor != 10_000 || c1.Exceptions.Minor != 20_000 || c1.ExceptionsPending.Minor != 0 || c1.Unapportioned.Minor != 0 || c1.Apportioned.Minor != 0 {
		t.Fatalf("C-1's economics: %+v", c1)
	}
	said(t, "the close with an amount to be allocated", d.closeP1("fin-close-2"), "FIN_POOL_NOT_CLOSABLE", "cost line 1 has 270000 to be allocated", "add cargo and apportion the line again",
		"attribute it as a loss or an exception cost of a house that left", "cancel the consolidation and retain it")
	if p = d.pool(); p.State != finapi.PoolOpen {
		t.Fatalf("a refused close changed the pool: %+v", p)
	}
	// the way out: the rest is the exception cost of a house that left; the others' attributions are carried
	rest := statement("H03", 10_000, 290_000, finapi.PartyPending)
	rest.Reason = "the rest of the booking is the consolidation's"
	pre, err = d.previewWith("ops-2", "fin-pre-rest", rest)
	d.must("the correction", err)
	_, err = d.submitBy("fin-1", "fin-sub-rest", pre.Version)
	d.must("the correction submitted", err)
	la = d.pool().Apportionments[2].Lines[0]
	if la.ToAllocate.Minor != 0 || len(la.Attributions) != 3 || !la.Attributions[0].Carried || !la.Attributions[1].Carried || la.Attributions[2].Carried || la.Attributions[2].Reason == "" {
		t.Fatalf("the correction: %+v", la)
	}
	_, err = d.fin.ReleasePoolAccrual(d.ctx, finapi.ReleasePoolAccrual{CommandID: "fin-release-zero", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: d.pool().Revision, LineNo: 2, ExpectedVersion: 1, Reason: "nothing will be invoiced", OccurredAt: d.later()})
	d.must("release the zero line", err)
	if err = d.closeP1("fin-close-3"); err != nil {
		t.Fatalf("the close once nothing is to be allocated: %v", err)
	}
}

// A pool with no departure is previewed, submitted and closed as it was in V2.92 (see
// TestAPoolWithNoReworkIsApportionedAsBeforeWithEveryContext, which is not changed): nothing in it is attributed.
func TestAPoolWithNoDepartureHasNoAttributionWithEveryContext(t *testing.T) {
	d := boxA(t, "cargo-box-a-nodeparture-")
	r, err := d.previewBy("fin-1", "fin-pre")
	d.must("the preview", err)
	_, err = d.submitBy("fin-1", "fin-sub", r.Version)
	d.must("the submission", err)
	p := d.pool()
	la := p.Apportionments[0].Lines[0]
	if !sameFigures(d.sharesOf(p.Apportionments[0]), 100_000, 150_000, 50_000) || len(la.Attributions) != 0 || la.ToAllocate.Minor != 0 || len(p.Previews[0].Lines[0].Attributions) != 0 {
		t.Fatalf("the apportionment: %+v", p.Apportionments)
	}
	if v := p.Attributions; len(v) != 1 || len(v[0].Losses) != 0 || len(v[0].Exceptions) != 0 || len(v[0].FormerShares) != 0 || v[0].ToAllocate.Minor != 0 || v[0].ExceptionPending.Minor != 0 {
		t.Fatalf("the view: %+v", v)
	}
	c1 := d.consolProfit()
	if c1.Pooled.Minor != 300_000 || c1.Apportioned.Minor != 300_000 || c1.Unapportioned.Minor != 0 || c1.Losses.Minor != 0 || c1.Exceptions.Minor != 0 || c1.FormerSharesPending.Minor != 0 {
		t.Fatalf("the economics: %+v", c1)
	}
	for house, want := range map[string]int64{"H01": 100_000, "H02": 150_000, "H03": 50_000} {
		if got := d.jobProfit(house); got.PoolCost.Minor != want || got.FormerSharesPending.Minor != 0 || len(got.FormerShares) != 0 || got.ReroutingLoss.Minor != 0 {
			t.Fatalf("the profit of JOB-%s: %+v", house, got)
		}
	}
	d.settleFreight()
	if err = d.closeP1("fin-close"); err != nil {
		t.Fatalf("the close: %v", err)
	}
}

// attribute previews line 1 with the attributions and submits it, as the same person.
func (d *oceanDay) attribute(id, who string, attributions ...finapi.AttributionStatement) error {
	d.t.Helper()
	r, err := d.previewWith(who, id, attributions...)
	if err != nil {
		return err
	}
	_, err = d.submitBy(who, id+"-submit", r.Version)
	return err
}
