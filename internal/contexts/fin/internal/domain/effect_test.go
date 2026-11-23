package domain

import (
	"slices"
	"strings"
	"testing"
)

// FIN V2.94: what submitting a preview changes. The owner's box A after H03 left (see pending_test.go): freight CNY
// 3,000.00 stands as 1,000.00, 1,500.00 and 500.00 (H03's pending).

// previewA stores a preview of box A after H03 left with the attributions given, by ops-2, and answers its number.
func previewA(t *testing.T, p *ConsolCostPool, attributions ...AttributionInput) int {
	t.Helper()
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: attributions}, boxAAfter(), "ops-2", hour(30)); err != nil {
		t.Fatal(err)
	}
	return lastPreview(p)
}

func effectOf(t *testing.T, p *ConsolCostPool, no int, basis CostBasis) PreviewEffect {
	t.Helper()
	e, err := p.PreviewEffect(no, SubmitInput{Basis: basis, Jobs: boxAJobs()})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func houseOf(t *testing.T, e PreviewEffect, line int, shipment string) HouseEffect {
	t.Helper()
	for _, h := range e.Houses {
		if h.LineNo == line && h.ShipmentID == shipment {
			return h
		}
	}
	t.Fatalf("no row for house %s on line %d: %+v", shipment, line, e.Houses)
	return HouseEffect{}
}

// The owner's example: a loss of 100.00 and an exception cost of 200.00 (party pending) for H03. H01 goes from 1,000.00
// to 1,080.00 (profit -80.00), H02 from 1,500.00 to 1,620.00 (-120.00), H03's 500.00 pending becomes a loss of 100.00
// (-100.00), the exception cost is 200.00 pending, and the line is 3,000.00 on both sides.
func TestTheEffectOfALossAndAnExceptionCost(t *testing.T) {
	p := boxAPool(t)
	e := effectOf(t, p, previewA(t, p, attribution(1, "H03", 10_000, 20_000, PartyPending)), boxAAfter())
	type figures struct {
		kind, now         string
		was, will, diff   int64
		profit            int64
		live, former      bool
		nowLocal, willLoc int64
	}
	want := map[string]figures{
		"H01": {BearsShare, BearsShare, 100_000, 108_000, 8_000, -8_000, true, false, 100_000, 108_000},
		"H02": {BearsShare, BearsShare, 150_000, 162_000, 12_000, -12_000, true, false, 150_000, 162_000},
		"H03": {BearsFormerShare, BearsLoss, 50_000, 10_000, -40_000, -10_000, false, true, 50_000, 10_000},
	}
	if len(e.Houses) != 3 {
		t.Fatalf("the rows: %+v", e.Houses)
	}
	for shipment, w := range want {
		h := houseOf(t, e, 1, shipment)
		if h.NowKind != w.kind || h.PreviewKind != map[string]string{"H01": BearsShare, "H02": BearsShare, "H03": BearsLoss}[shipment] || h.Now != w.was ||
			h.Preview != w.will || h.Difference != w.diff || h.ProfitChange != w.profit || h.Live != w.live || h.Former != w.former || h.NowLocal != w.nowLocal ||
			h.PreviewLocal != w.willLoc || h.DifferenceLocal != w.diff || h.JobID != "JOB-"+shipment {
			t.Fatalf("%s: %+v", shipment, h)
		}
	}
	if len(e.Lines) != 1 {
		t.Fatalf("the lines: %+v", e.Lines)
	}
	l := e.Lines[0]
	if l.CostNow != 300_000 || l.Cost != 300_000 || l.CostLocal != 300_000 || l.CostChanged || l.Shares != 270_000 ||
		l.Losses != 10_000 || l.Exceptions != 20_000 || l.ToAllocate != 0 || !l.Conserved || l.Method != MethodChargeable {
		t.Fatalf("the line: %+v", l)
	}
	if len(e.Exceptions) != 1 || e.Exceptions[0].Party != PartyPending || e.Exceptions[0].Before != 0 || e.Exceptions[0].After != 20_000 || e.Exceptions[0].AfterLocal != 20_000 {
		t.Fatalf("the exception costs: %+v", e.Exceptions)
	}
	wantTotals := []HouseTotal{{"H01", "JOB-H01", -8_000}, {"H02", "JOB-H02", -12_000}, {"H03", "JOB-H03", -10_000}}
	if !slices.Equal(e.Totals, wantTotals) {
		t.Fatalf("the totals: %+v", e.Totals)
	}
	if !e.Current || len(e.Changes) != 0 || e.SubmittedAs != 0 || e.InWork || e.PreviewedBy != "ops-2" || e.Stage != BasisActual {
		t.Fatalf("the state: %+v", e)
	}
}

// A former share attributed nothing leaves the house's profit as it is (the share was never in it) and the other houses
// take the 500.00 between them; a loss of the whole share lowers the house's profit by it and changes the others by
// nothing.
func TestTheEffectOfAttributingAFormerShareNothingOrItsWholeAmount(t *testing.T) {
	p := boxAPool(t)
	e := effectOf(t, p, previewA(t, p, attribution(1, "H03", 0, 0, "")), boxAAfter())
	h3 := houseOf(t, e, 1, "H03")
	if h3.NowKind != BearsFormerShare || h3.PreviewKind != BearsNothing || h3.Now != 50_000 || h3.Preview != 0 || h3.Difference != -50_000 || h3.ProfitChange != 0 {
		t.Fatalf("H03 attributed nothing: %+v", h3)
	}
	if h1 := houseOf(t, e, 1, "H01"); h1.Now != 100_000 || h1.Preview != 120_000 || h1.ProfitChange != -20_000 {
		t.Fatalf("H01: %+v", h1)
	}
	if h2 := houseOf(t, e, 1, "H02"); h2.Now != 150_000 || h2.Preview != 180_000 || h2.ProfitChange != -30_000 {
		t.Fatalf("H02: %+v", h2)
	}
	q := boxAPool(t)
	e = effectOf(t, q, previewA(t, q, attribution(1, "H03", 50_000, 0, "")), boxAAfter())
	if h3 = houseOf(t, e, 1, "H03"); h3.PreviewKind != BearsLoss || h3.Preview != 50_000 || h3.ProfitChange != -50_000 {
		t.Fatalf("H03 attributed all as a loss: %+v", h3)
	}
	if h1 := houseOf(t, e, 1, "H01"); h1.Difference != 0 || h1.ProfitChange != 0 {
		t.Fatalf("H01 is not changed: %+v", h1)
	}
}

// A loss in force that a correction takes back to nothing and an exception cost that changes party are told before and
// after, and a loss that grows lowers the profit by the growth only.
func TestTheEffectOfACorrection(t *testing.T) {
	p := boxAPool(t)
	if err := attributed(p, "fin-1", 32, attribution(1, "H03", 10_000, 20_000, PartyPending)); err != nil {
		t.Fatal(err)
	}
	c := attribution(1, "H03", 15_000, 20_000, PartyCarrier)
	c.Reason = "the carrier accepted the claim"
	c.Voucher = "CLAIM-2"
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{c}}, boxAAfter(), "ops-2", hour(40)); err != nil {
		t.Fatal(err)
	}
	e := effectOf(t, p, lastPreview(p), boxAAfter())
	h3 := houseOf(t, e, 1, "H03")
	if h3.NowKind != BearsLoss || h3.Now != 10_000 || h3.PreviewKind != BearsLoss || h3.Preview != 15_000 || h3.Difference != 5_000 || h3.ProfitChange != -5_000 {
		t.Fatalf("H03: %+v", h3)
	}
	if len(e.Exceptions) != 2 || e.Exceptions[0].Party != PartyCarrier || e.Exceptions[0].Before != 0 || e.Exceptions[0].After != 20_000 ||
		e.Exceptions[1].Party != PartyPending || e.Exceptions[1].Before != 20_000 || e.Exceptions[1].After != 0 {
		t.Fatalf("the exception costs: %+v", e.Exceptions)
	}
	var r ReasonEffect
	for _, x := range e.Reasons {
		if x.ShipmentID == "H03" {
			r = x
		}
	}
	if !r.Attributed || r.Voucher != "CLAIM-2" || r.AttributionNote != "the carrier accepted the claim" || r.Party != PartyCarrier {
		t.Fatalf("the reason: %+v", r)
	}
}

// An amount to be allocated: with every house gone the remainder has no share; the line shows it before and after.
func TestTheEffectOfAnAmountToBeAllocated(t *testing.T) {
	p := boxAPool(t)
	gone := boxAGone()
	in := ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H01", 0, 0, ""), attribution(1, "H02", 0, 0, ""),
		attribution(1, "H03", 10_000, 20_000, PartyCarrier)}}
	if err := p.Preview(in, gone, "ops-2", hour(30)); err != nil {
		t.Fatal(err)
	}
	e, err := p.PreviewEffect(lastPreview(p), SubmitInput{Basis: gone, Jobs: boxAJobs()})
	if err != nil {
		t.Fatal(err)
	}
	l := e.Lines[0]
	if l.ToAllocate != 270_000 || l.Shares != 0 || l.Losses != 10_000 || l.Exceptions != 20_000 || !l.Conserved || l.Cost != 300_000 {
		t.Fatalf("the line: %+v", l)
	}
	for _, s := range []string{"H01", "H02"} {
		if h := houseOf(t, e, 1, s); h.NowKind != BearsFormerShare || h.PreviewKind != BearsNothing || h.ProfitChange != 0 {
			t.Fatalf("%s: %+v", s, h)
		}
	}
	if h := houseOf(t, e, 1, "H03"); h.Now != 50_000 || h.Preview != 10_000 || h.ProfitChange != -10_000 {
		t.Fatalf("H03: %+v", h)
	}
	// a line the preview retains shows what is retained
	cancelled := gone
	cancelled.State = "CANCELLED"
	q := boxAPool(t)
	rin := ApportionInput{Method: MethodRetained, Reason: "the consolidation was cancelled", Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H01", 0, 0, ""),
		attribution(1, "H02", 0, 0, ""), attribution(1, "H03", 10_000, 20_000, PartyOwnOperations)}}
	if err = q.Preview(rin, cancelled, "ops-2", hour(30)); err != nil {
		t.Fatal(err)
	}
	e, err = q.PreviewEffect(lastPreview(q), SubmitInput{Basis: cancelled, Jobs: boxAJobs()})
	if err != nil {
		t.Fatal(err)
	}
	if l = e.Lines[0]; l.Retained != 270_000 || l.Method != MethodRetained || !l.Conserved || l.Reason != "the consolidation was cancelled" {
		t.Fatalf("the retained line: %+v", l)
	}
}

// The check that the parts sum to the line's cost is shown, not enforced: a figure that does not sum is told as such,
// in either currency, and a cost that is no longer the one the preview divided is told too.
func TestTheEffectShowsWhetherThePartsSumToTheLine(t *testing.T) {
	la := LineApportionment{LineNo: 1, Method: MethodChargeable, Amount: 300_000, Local: 300_000, Shares: []Share{{ShipmentID: "H01", Used: true, Amount: 200_000, Local: 200_000},
		{ShipmentID: "H02", Used: true, Amount: 99_999, Local: 100_000}}}
	if e := lineEffect(la, "CNY", 300_000, 300_000); e.Conserved || e.CostChanged {
		t.Fatalf("a line short by one: %+v", e)
	}
	la.Shares[1].Amount = 100_000
	la.Shares[1].Local = 99_999
	if e := lineEffect(la, "CNY", 300_000, 300_000); e.Conserved {
		t.Fatalf("a line short by one in the functional currency: %+v", e)
	}
	la.Shares[1].Local = 100_000
	if e := lineEffect(la, "CNY", 300_000, 300_000); !e.Conserved {
		t.Fatalf("a line that sums: %+v", e)
	}
	if e := lineEffect(la, "CNY", 310_000, 310_000); !e.CostChanged || e.CostNow != 310_000 || e.Cost != 300_000 {
		t.Fatalf("a cost that moved: %+v", e)
	}
	// a share left out is no part
	la.Shares = append(la.Shares, Share{ShipmentID: "H03", Used: false})
	if e := lineEffect(la, "CNY", 300_000, 300_000); !e.Conserved {
		t.Fatalf("a share left out: %+v", e)
	}
}

// The preview is current while what it was computed on is; once PHY's basis moves the effect says so, naming what
// changed, and a submitted preview says it was submitted and is not told as stale. Neither refuses.
func TestTheEffectSaysWhetherThePreviewIsStaleOrSubmitted(t *testing.T) {
	p := boxAPool(t)
	no := previewA(t, p, attribution(1, "H03", 10_000, 20_000, PartyPending))
	later := boxAAfter()
	later.Revision = 9
	e := effectOf(t, p, no, later)
	if e.Current || len(e.Changes) == 0 || !strings.Contains(e.Changes[0], "basis changed from [revision 8] to [revision 9]") || e.SubmittedAs != 0 {
		t.Fatalf("stale: %+v", e)
	}
	if e = effectOf(t, p, no, boxAAfter()); !e.Current {
		t.Fatalf("current: %+v", e)
	}
	// the house's job moved
	e, err := p.PreviewEffect(no, SubmitInput{Basis: boxAAfter(), Jobs: map[string]string{"H01": "JOB-H01", "H02": "JOB-OTHER"}})
	if err != nil || e.Current || len(e.Changes) != 1 || !strings.Contains(e.Changes[0], `house."H02"`) {
		t.Fatalf("another job: %+v %v", e, err)
	}
	// a container in rework is told as such
	working := boxAAfter()
	working.LoadUnits = []LoadUnitFacts{{UnitNo: 1, ExecutionState: ExecutionRework}}
	if e = effectOf(t, p, no, working); !e.InWork || e.Current {
		t.Fatalf("in rework: %+v", e)
	}
	if e = effectOf(t, p, no, boxAAfter()); e.InWork {
		t.Fatalf("not in rework: %+v", e)
	}
	// submitted
	if err = p.Submit(no, SubmitInput{Basis: boxAAfter(), Jobs: boxAJobs()}, "fin-1", hour(31)); err != nil {
		t.Fatal(err)
	}
	e = effectOf(t, p, no, boxAAfter())
	if e.SubmittedAs != 2 || e.Current || len(e.Changes) != 0 {
		t.Fatalf("submitted: %+v", e)
	}
	if _, err = p.PreviewEffect(0, SubmitInput{Basis: boxAAfter()}); err == nil {
		t.Fatal("preview 0")
	}
	if _, err = p.PreviewEffect(no+1, SubmitInput{Basis: boxAAfter()}); err == nil {
		t.Fatal("an unknown preview")
	}
	other := boxAAfter()
	other.ConsolidationID = "C-9"
	if _, err = p.PreviewEffect(no, SubmitInput{Basis: other}); err == nil {
		t.Fatal("another consolidation's basis")
	}
}

// The reasons: PHY's departure of the house (how, when, who, why, to where) and the preview's attribution, for the
// house that left; a live house has none.
func TestTheEffectGivesTheReasons(t *testing.T) {
	p := boxAPool(t)
	a := attribution(1, "H03", 10_000, 20_000, PartyCarrier)
	a.PartyReference, a.Reason = "CLAIM-REF-7", "the carrier left the cargo behind"
	e := effectOf(t, p, previewA(t, p, a), boxAAfter())
	if len(e.Reasons) != 1 {
		t.Fatalf("the reasons: %+v", e.Reasons)
	}
	r := e.Reasons[0]
	if r.LineNo != 1 || r.ShipmentID != "H03" || !r.Former || r.End != "TRANSFERRED" || r.EndReason != "WRONG_CONSOL" || r.EndedBy != "ops-9" || !r.EndedAt.Equal(hour(26)) ||
		r.TransferredTo != "C-2" || !r.Attributed || r.Party != PartyCarrier || r.PartyReference != "CLAIM-REF-7" || r.Voucher != "CLAIM-1" ||
		r.AttributionNote != "the carrier left the cargo behind" || r.Carried {
		t.Fatalf("the reason: %+v", r)
	}
	if e.Lines[0].Method != MethodChargeable {
		t.Fatalf("the method: %+v", e.Lines[0])
	}
	// a MANUAL line's reason is shown
	q := pool(t, boxA())
	recordCost(t, q, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	if err := q.Preview(ApportionInput{Jobs: boxAJobs(), Method: MethodManual, Reason: "agreed with the customers", LineNos: []int{1},
		Manual: []ManualShare{{ShipmentID: "H01", Amount: 100_000}, {ShipmentID: "H02", Amount: 100_000}, {ShipmentID: "H03", Amount: 100_000}}}, boxA(), "ops-2", hour(5)); err != nil {
		t.Fatal(err)
	}
	e, err := q.PreviewEffect(1, SubmitInput{Basis: boxA(), Jobs: boxAJobs()})
	if err != nil {
		t.Fatal(err)
	}
	if e.Lines[0].Method != MethodManual || e.Lines[0].Reason != "agreed with the customers" || len(e.Reasons) != 0 || !e.Current {
		t.Fatalf("a manual line: %+v", e)
	}
	if h := houseOf(t, e, 1, "H01"); h.NowKind != BearsNothing || h.PreviewKind != BearsShare || h.Preview != 100_000 || h.ProfitChange != -100_000 {
		t.Fatalf("a line apportioned for the first time: %+v", h)
	}
}

// Two lines in one preview: the totals sum each house's profit change over the lines.
func TestTheEffectTotalsEachHousesProfitChangeOverTheLines(t *testing.T) {
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	recordCost(t, p, "THC", cny(90_000), MethodChargeable)
	if err := apportion(p, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	a1, a2 := attribution(1, "H03", 0, 0, ""), attribution(2, "H03", 0, 0, "")
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{a1, a2}}, boxAAfter(), "ops-2", hour(30)); err != nil {
		t.Fatal(err)
	}
	e := effectOf(t, p, lastPreview(p), boxAAfter())
	want := []HouseTotal{{"H01", "JOB-H01", -20_000 - 6_000}, {"H02", "JOB-H02", -30_000 - 9_000}, {"H03", "JOB-H03", 0}}
	if !slices.Equal(e.Totals, want) {
		t.Fatalf("the totals: %+v, want %+v", e.Totals, want)
	}
}

// A line in a foreign currency: USD 3,000.01 at the ESTIMATE rate 7.1234 is CNY 21,370.27, divided over the houses by
// 10 : 15 : 5. H03's former share pending is then lowered to a loss of USD 123.45 (CNY 879.38); the difference and the
// profit change are told in the functional currency, apart from the line's currency.
func TestTheEffectOfAForeignCurrencyLineIsToldInBothCurrencies(t *testing.T) {
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", usd(300_001), MethodChargeable)
	if err := apportion(p, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	_, now := latest(t, p, 1)
	e := effectOf(t, p, previewA(t, p, attribution(1, "H03", 12_345, 0, "")), boxAAfter())
	if e.Lines[0].Currency != "USD" || e.Lines[0].CostLocal != 2_137_027 || e.Lines[0].CostNowLocal != 2_137_027 || !e.Lines[0].Conserved {
		t.Fatalf("the line: %+v", e.Lines[0])
	}
	h3 := houseOf(t, e, 1, "H03")
	if h3.NowKind != BearsFormerShare || h3.Now != now.Shares[2].Amount || h3.NowLocal != now.Shares[2].Local || h3.NowLocal == h3.Now ||
		h3.Preview != 12_345 || h3.PreviewLocal != 87_938 || h3.Difference != 12_345-h3.Now || h3.DifferenceLocal != 87_938-h3.NowLocal ||
		h3.DifferenceLocal == h3.Difference || h3.ProfitChange != -87_938 {
		t.Fatalf("H03: %+v", h3)
	}
	h1 := houseOf(t, e, 1, "H01")
	if h1.DifferenceLocal != h1.PreviewLocal-h1.NowLocal || h1.DifferenceLocal == h1.Difference || h1.ProfitChange != h1.NowLocal-h1.PreviewLocal || h1.ProfitChange == -h1.Difference {
		t.Fatalf("H01: %+v", h1)
	}
}

// A preview of a pool that is closed since is not Current, whatever the fingerprint says, and the way out is to reopen
// the pool; an open pool has neither the flag nor the way out.
func TestTheEffectOfAPreviewOfAClosedPoolSaysSoAndNamesTheWayOut(t *testing.T) {
	p := boxAPool(t)
	if err := p.Preview(ApportionInput{Jobs: boxAJobs()}, boxA(), "ops-2", hour(5)); err != nil {
		t.Fatal(err)
	}
	no := lastPreview(p)
	e, err := p.PreviewEffect(no, SubmitInput{Basis: boxA(), Jobs: boxAJobs()})
	if err != nil || !e.Current || e.PoolClosed || e.WayOut != "" {
		t.Fatalf("an open pool: %+v %v", e, err)
	}
	settle(p)
	if err = p.Close(boxA(), "fin-1", hour(6)); err != nil {
		t.Fatal(err)
	}
	e, err = p.PreviewEffect(no, SubmitInput{Basis: boxA(), Jobs: boxAJobs()})
	if err != nil || e.Current || !e.PoolClosed || e.SubmittedAs != 0 || len(e.Changes) != 0 {
		t.Fatalf("a closed pool: %+v %v", e, err)
	}
	mustSay(t, "closed", e.WayOut, "the pool is closed", "reopen it with a reason (ReopenConsolCostPool)", "submit the preview if it is still current")
}

// What the line in force has to be allocated is told as well as the preview's: a correction that attributes the rest
// takes it from CNY 2,700.00 to nothing.
func TestTheEffectGivesTheAmountToBeAllocatedInForceAndInThePreview(t *testing.T) {
	p := boxAPool(t)
	gone := boxAGone()
	in := ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H01", 0, 0, ""), attribution(1, "H02", 0, 0, ""),
		attribution(1, "H03", 10_000, 20_000, PartyCarrier)}}
	if err := apportion(p, in, gone, "fin-1", hour(30)); err != nil {
		t.Fatal(err)
	}
	rest := attribution(1, "H03", 10_000, 290_000, PartyCarrier)
	rest.Reason = "the rest is the consolidation's"
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{rest}}, gone, "ops-2", hour(31)); err != nil {
		t.Fatal(err)
	}
	e, err := p.PreviewEffect(lastPreview(p), SubmitInput{Basis: gone, Jobs: boxAJobs()})
	if err != nil {
		t.Fatal(err)
	}
	if l := e.Lines[0]; l.ToAllocateNow != 270_000 || l.ToAllocateNowLocal != 270_000 || l.ToAllocate != 0 || l.ToAllocLocal != 0 || !l.Conserved {
		t.Fatalf("the line: %+v", l)
	}
	// a line not apportioned has none in force
	q := pool(t, boxA())
	recordCost(t, q, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	if err = q.Preview(ApportionInput{Jobs: boxAJobs()}, boxA(), "ops-2", hour(5)); err != nil {
		t.Fatal(err)
	}
	if e, err = q.PreviewEffect(1, SubmitInput{Basis: boxA(), Jobs: boxAJobs()}); err != nil || e.Lines[0].ToAllocateNow != 0 {
		t.Fatalf("not apportioned: %+v %v", e.Lines, err)
	}
}

// A cost that changes in only one of the two currencies is a cost that changed.
func TestACostChangedInOnlyOneCurrencyIsFlaggedInTheEffect(t *testing.T) {
	la := LineApportionment{LineNo: 1, Method: MethodChargeable, Amount: 300_000, Local: 2_100_000, Shares: []Share{{ShipmentID: "H01", Used: true, Amount: 300_000, Local: 2_100_000}}}
	if e := lineEffect(la, "USD", 300_000, 2_100_000); e.CostChanged {
		t.Fatalf("the same cost: %+v", e)
	}
	if e := lineEffect(la, "USD", 310_000, 2_100_000); !e.CostChanged {
		t.Fatalf("the amount moved: %+v", e)
	}
	if e := lineEffect(la, "USD", 300_000, 2_150_000); !e.CostChanged {
		t.Fatalf("the functional amount moved: %+v", e)
	}
}

// A live house that neither the apportionment in force nor the preview uses has no row, and a house the preview leaves
// out keeps its job from the apportionment in force.
func TestTheEffectHasNoRowForAHouseNeitherApportionmentUsesAndKeepsAJobForOneLeftOut(t *testing.T) {
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	if err := apportion(p, ApportionInput{Jobs: boxAJobs(), NotUsed: []string{"H03"}}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), NotUsed: []string{"H03"}}, boxA(), "ops-2", hour(5)); err != nil {
		t.Fatal(err)
	}
	e, err := p.PreviewEffect(lastPreview(p), SubmitInput{Basis: boxA(), Jobs: boxAJobs()})
	if err != nil || len(e.Houses) != 2 || e.Houses[0].ShipmentID != "H01" || e.Houses[1].ShipmentID != "H02" {
		t.Fatalf("the rows: %+v %v", e.Houses, err)
	}
	// every house took part; the preview leaves H03 out
	q := pool(t, boxA())
	recordCost(t, q, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	if err = apportion(q, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if err = q.Preview(ApportionInput{Jobs: boxAJobs(), NotUsed: []string{"H03"}}, boxA(), "ops-2", hour(5)); err != nil {
		t.Fatal(err)
	}
	if e, err = q.PreviewEffect(lastPreview(q), SubmitInput{Basis: boxA(), Jobs: boxAJobs()}); err != nil {
		t.Fatal(err)
	}
	h3 := houseOf(t, e, 1, "H03")
	if h3.JobID != "JOB-H03" || h3.NowKind != BearsShare || h3.PreviewKind != BearsNothing || h3.Now != 50_000 || h3.ProfitChange != 50_000 {
		t.Fatalf("H03: %+v", h3)
	}
}
