package domain

import (
	"errors"
	"strings"
	"testing"
)

// Finance attributes a departed house's former share of the original consolidation's costs (V2.93). The owner's box A:
// C-1 with H01, H02 and H03 of 10, 15 and 5 W/M, freight CNY 3,000.00 apportioned 1,000.00, 1,500.00 and 500.00; H03
// leaves (transferred by ops-9 after a rework asked by ops-1).

func boxA() CostBasis {
	house := func(id string, litres, kilos int64) HouseFacts {
		return HouseFacts{ShipmentID: id, Packs: 6, GrossWeight: kg(kilos), Volume: cmq(litres * 1000), LoadedPacks: 6, LoadedGrossWeight: kg(kilos),
			LoadedVolume: cmq(litres * 1000), LastLoadedAt: hour(1)}
	}
	return CostBasis{ConsolidationID: "C-1", Revision: 7, Kind: "SEA_LCL_OWN", OperatingLegalEntityID: "LE", State: "CLOSED",
		Houses: []HouseFacts{house("H01", 10_000, 312), house("H02", 15_000, 300), house("H03", 5_000, 288)}}
}

// boxAAfter is box A once H03 left: the two houses that remain and the house that left, with the rework that unloaded
// it, who asked for it and who ended its place.
func boxAAfter() CostBasis {
	b := boxA()
	b.Revision, b.Houses = 8, b.Houses[:2]
	b.FormerHouses = []FormerHouseFacts{{ShipmentID: "H03", End: "TRANSFERRED", TransferredTo: "C-2", Reason: "WRONG_CONSOL", EndedBy: "ops-9", EndedAt: hour(26),
		EndedEnteredAt: hour(27), ReworkFrom: "ENTRY", ReworkExecutionID: "X-1", ReworkNo: 1, ReworkUnitNo: 1, ReworkAskedBy: "ops-1"}}
	return b
}

func boxAJobs() map[string]string {
	return map[string]string{"H01": "JOB-H01", "H02": "JOB-H02", "H03": "JOB-H03"}
}

// boxAPool is the pool of C-1 with its freight line apportioned before H03 left.
func boxAPool(t *testing.T) *ConsolCostPool {
	t.Helper()
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	if err := apportion(p, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	return p
}

func attribution(line int, house string, loss, exception int64, party string) AttributionInput {
	return AttributionInput{LineNo: line, ShipmentID: house, Loss: loss, Exception: exception, Party: party, Voucher: "CLAIM-1"}
}

// attributed previews and submits the attributions on box A after H03 left, by the person given.
func attributed(p *ConsolCostPool, by string, h int, attributions ...AttributionInput) error {
	b := boxAAfter()
	in := ApportionInput{Jobs: boxAJobs(), Attributions: attributions}
	if err := p.Preview(in, b, "ops-2", hour(h)); err != nil {
		return err
	}
	return p.Submit(len(p.State().Previews), SubmitInput{Basis: b, Jobs: in.Jobs}, by, hour(h))
}

// lastPreview is the number of the latest preview of the pool.
func lastPreview(p *ConsolCostPool) int { return len(p.State().Previews) }

func shareAmounts(la LineApportionment) (out []int64) {
	for _, s := range la.Shares {
		out = append(out, s.Amount)
	}
	return out
}

func sameAmounts(got []int64, want ...int64) bool {
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

func latest(t *testing.T, p *ConsolCostPool, line int) (Apportionment, LineApportionment) {
	t.Helper()
	a, la, ok := p.SharesOfLine(line)
	if !ok {
		t.Fatalf("line %d is not apportioned", line)
	}
	return a, la
}

// A former share is a used share of a house PHY lists as having left, on the apportionment in force of a line. A share
// of a house left out of the line, or a share of nothing, is none; a house that is live bears no former share.
func TestAFormerShareIsAUsedShareOfAHouseThatLeft(t *testing.T) {
	p := boxAPool(t)
	if got := p.FormerShares(boxA()); len(got) != 0 {
		t.Fatalf("before H03 left: %+v", got)
	}
	got := p.FormerShares(boxAAfter())
	if len(got) != 1 || got[0].LineNo != 1 || got[0].Share.ShipmentID != "H03" || got[0].Share.Amount != 50_000 || got[0].House.End != "TRANSFERRED" {
		t.Fatalf("after H03 left: %+v", got)
	}
	if h := p.HousesToAttribute(boxAAfter()); len(h) != 1 || h[0] != "H03" {
		t.Fatalf("the houses to attribute: %v", h)
	}
	// a house left out of the line bore nothing of it
	q := pool(t, boxA())
	recordCost(t, q, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	if err := apportion(q, ApportionInput{Jobs: boxAJobs(), NotUsed: []string{"H03"}}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if got := q.FormerShares(boxAAfter()); len(got) != 0 {
		t.Fatalf("a house left out bears no former share: %+v", got)
	}
	// a line that costs nothing gives a share of nothing
	z := pool(t, boxA())
	recordCost(t, z, "OCEAN_FREIGHT", cny(0), MethodChargeable)
	if err := apportion(z, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if got := z.FormerShares(boxAAfter()); len(got) != 0 || len(z.HousesToAttribute(boxAAfter())) != 0 {
		t.Fatalf("a share of nothing is no former share: %+v", got)
	}
	// the house comes back: it is live and bears its share as the others do
	back := boxA()
	back.Revision = 9
	if got := p.FormerShares(back); len(got) != 0 {
		t.Fatalf("a live house: %+v", got)
	}
}

// A preview of a line that holds a former share of a house with no attribution stated and none in force is refused: it
// names the line, the house, the former share (the loss it would bear by default) and the way out.
func TestAPreviewOfALineHoldingAFormerShareNeedsItsAttribution(t *testing.T) {
	p := boxAPool(t)
	err := p.Preview(ApportionInput{Jobs: boxAJobs()}, boxAAfter(), "ops-2", hour(30))
	refused(t, "no attribution", err, ErrAttributionMissing, "line 1", "house H03", "50000 CNY", "the loss it would bear by default", "state who bears it", "voucher")
	if lastPreview(p) != 1 {
		t.Fatal("a refused preview was stored")
	}
	// an attribution of another house does not do
	err = p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H01", 0, 0, "")}}, boxAAfter(), "ops-2", hour(30))
	refused(t, "another house", err, ErrInvalid, "house H01 is not a house PHY reports as having left")
	// a line that is not previewed has none to state
	p2 := pool(t, boxA())
	recordCost(t, p2, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	if err = p2.RecordCost("THC", "", "THC-1", MethodChargeable, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(60_000)}, "terminal", policy(t), "ops-1", hour(3)); err != nil {
		t.Fatal(err)
	}
	if err = apportion(p2, ApportionInput{LineNos: []int{1}, Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	err = p2.Preview(ApportionInput{LineNos: []int{2}, Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H03", 0, 0, "")}}, boxAAfter(), "ops-2", hour(30))
	refused(t, "a line not previewed", err, ErrInvalid, "names line 1, which the preview does not divide")
	// a line with no former share is previewed as before, whatever else left
	if err = p2.Preview(ApportionInput{LineNos: []int{2}, Jobs: boxAJobs()}, boxAAfter(), "ops-2", hour(30)); err != nil {
		t.Fatalf("line 2 holds no former share: %v", err)
	}
}

// The owner's two outcomes on box A. Loss 100.00, exception cost 200.00 with the party pending: 2,700.00 is divided
// over H01 and H02 on 10 : 15, 1,080.00 and 1,620.00. Loss 0 and exception cost 0: all of it goes to the remaining
// cargo, 1,200.00 and 1,800.00, and the voucher is still required.
func TestAnAttributionTakesItsAmountsOutBeforeTheRemainderIsDivided(t *testing.T) {
	p := boxAPool(t)
	if err := attributed(p, "fin-1", 30, AttributionInput{LineNo: 1, ShipmentID: "H03", Loss: 10_000, Exception: 20_000, Party: PartyPending, PartyReference: "claim 7",
		Voucher: "CLAIM-1"}); err != nil {
		t.Fatal(err)
	}
	a, la := latest(t, p, 1)
	if a.No != 2 || !sameAmounts(shareAmounts(la), 108_000, 162_000) || len(la.Shares) != 2 || la.Shares[0].ShipmentID != "H01" || la.Amount != 300_000 || la.ToAllocate != 0 {
		t.Fatalf("the apportionment: %+v", la)
	}
	if len(la.Attributions) != 1 {
		t.Fatalf("the attributions: %+v", la.Attributions)
	}
	x := la.Attributions[0]
	if x.ShipmentID != "H03" || x.JobID != "JOB-H03" || x.Loss != 10_000 || x.LossLocal != 10_000 || x.Exception != 20_000 || x.ExceptionLocal != 20_000 || x.Party != PartyPending ||
		x.PartyReference != "claim 7" || x.Voucher != "CLAIM-1" || x.Reason != "" || x.Carried || !x.DepartedAt.Equal(hour(26)) {
		t.Fatalf("the attribution: %+v", x)
	}
	// the old apportionment is kept, with H03's share
	if old := p.State().Apportionments[0]; !sameAmounts(shareAmounts(old.Lines[0]), 100_000, 150_000, 50_000) || len(old.Lines[0].Attributions) != 0 {
		t.Fatalf("the first apportionment: %+v", old.Lines[0])
	}
	// no former share is left pending
	if got := p.FormerShares(boxAAfter()); len(got) != 0 {
		t.Fatalf("the former share after the attribution: %+v", got)
	}

	q := boxAPool(t)
	if err := attributed(q, "fin-1", 30, attribution(1, "H03", 0, 0, "")); err != nil {
		t.Fatal(err)
	}
	_, la = latest(t, q, 1)
	if !sameAmounts(shareAmounts(la), 120_000, 180_000) || len(la.Attributions) != 1 || la.Attributions[0].Loss != 0 || la.Attributions[0].Exception != 0 || la.Attributions[0].Party != "" {
		t.Fatalf("all to the remaining cargo: %+v", la)
	}
	// the voucher is required, also then
	r := boxAPool(t)
	no := AttributionInput{LineNo: 1, ShipmentID: "H03"}
	refused(t, "no voucher", r.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{no}}, boxAAfter(), "ops-2", hour(30)), ErrInvalid, "voucher")
	no.Voucher = "  "
	refused(t, "a blank voucher", r.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{no}}, boxAAfter(), "ops-2", hour(30)), ErrInvalid, "voucher")
}

// The line's local cost is divided over the remainder and each loss and exception cost by largest remainder, in
// proportion to the amounts in the line's currency: USD 3,000.01 at 7.1234 is CNY 21,370.27 (2,137,027 minor), divided
// 264,199 : 12,345 : 23,457 into 1,881,995 + 87,938 + 167,094; the remainder's part is shared by the houses on 10 : 15.
func TestTheLocalAmountsOfAnAttributionConserveByLargestRemainder(t *testing.T) {
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", usd(300_001), MethodChargeable)
	if err := apportion(p, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if err := attributed(p, "fin-1", 30, attribution(1, "H03", 12_345, 23_457, PartyCarrier)); err != nil {
		t.Fatal(err)
	}
	_, la := latest(t, p, 1)
	x := la.Attributions[0]
	if la.Amount != 300_001 || la.Local != 2_137_027 || x.Loss != 12_345 || x.LossLocal != 87_938 || x.Exception != 23_457 || x.ExceptionLocal != 167_094 {
		t.Fatalf("the line and its attribution: %+v", la)
	}
	var amount, local int64
	for _, s := range la.Shares {
		amount, local = amount+s.Amount, local+s.Local
	}
	if amount != 264_199 || local != 1_881_995 || amount+x.Loss+x.Exception != la.Amount || local+x.LossLocal+x.ExceptionLocal != la.Local {
		t.Fatalf("the shares %d (%d local) do not conserve the line", amount, local)
	}
}

// A loss or an exception cost below 0, an amount beyond the line's cost, and a party that does not match the exception
// cost are refused naming the line and the house.
func TestAttributionAmountsAreChecked(t *testing.T) {
	p := boxAPool(t)
	preview := func(a AttributionInput) error {
		return p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{a}}, boxAAfter(), "ops-2", hour(30))
	}
	refused(t, "a negative loss", preview(attribution(1, "H03", -1, 0, "")), ErrInvalid, "house H03 on line 1")
	refused(t, "a negative exception", preview(attribution(1, "H03", 0, -1, PartyCarrier)), ErrInvalid, "house H03 on line 1")
	refused(t, "losses and exceptions above the line", preview(attribution(1, "H03", 200_000, 100_001, PartyWarehouse)), ErrInvalid, "line 1", "(300001) exceed its cost of 300000 CNY")
	refused(t, "a loss above the line", preview(attribution(1, "H03", 300_001, 0, "")), ErrInvalid, "line 1")
	refused(t, "an exception cost without a party", preview(attribution(1, "H03", 0, 5, "")), ErrInvalid, "who is responsible")
	refused(t, "a party without an exception cost", preview(attribution(1, "H03", 5, 0, PartyCarrier)), ErrInvalid, "who is responsible")
	refused(t, "a party that is none", preview(attribution(1, "H03", 0, 5, "NO_SUCH_PARTY")), ErrInvalid, "who is responsible")
	refused(t, "an amount beyond what FIN keeps", preview(attribution(1, "H03", MaxAmount+1, 0, "")), ErrInvalid, "line 1")
	twice := ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H03", 1, 0, ""), attribution(1, "H03", 2, 0, "")}}
	refused(t, "a house twice", p.Preview(twice, boxAAfter(), "ops-2", hour(30)), ErrInvalid, "attributed twice on line 1")
	// every party is accepted, and the whole cost can be attributed: nothing remains for the houses
	for _, party := range []string{PartyCarrier, PartyWarehouse, PartyOwnOperations, PartyPending} {
		if err := preview(attribution(1, "H03", 0, 300_000, party)); err != nil {
			t.Fatalf("party %s: %v", party, err)
		}
	}
	if err := attributed(p, "fin-1", 31, attribution(1, "H03", 100_000, 200_000, PartyCarrier)); err != nil {
		t.Fatalf("the whole cost: %v", err)
	}
	if _, la := latest(t, p, 1); !sameAmounts(shareAmounts(la), 0, 0) || la.ToAllocate != 0 {
		t.Fatalf("nothing remains for the houses: %+v", la)
	}
}

// An attribution in force is carried when the line is apportioned again and none is stated for the house: it is the
// same loss, exception cost and party; the remainder follows a change of the line's cost; carried amounts that no
// longer fit are refused, and must be stated again.
func TestAnAttributionInForceIsCarriedForward(t *testing.T) {
	p := boxAPool(t)
	if err := attributed(p, "fin-1", 30, attribution(1, "H03", 10_000, 20_000, PartyCarrier)); err != nil {
		t.Fatal(err)
	}
	// the house still has to be reached by the next apportionment: its job is one of those locked
	if h := p.HousesToAttribute(boxAAfter()); len(h) != 1 || h[0] != "H03" {
		t.Fatalf("the houses to attribute with an attribution in force: %v", h)
	}
	if err := attributed(p, "fin-1", 31); err != nil {
		t.Fatalf("carried: %v", err)
	}
	a, la := latest(t, p, 1)
	x := la.Attributions[0]
	if a.No != 3 || !x.Carried || x.Loss != 10_000 || x.Exception != 20_000 || x.Party != PartyCarrier || x.Voucher != "CLAIM-1" || !sameAmounts(shareAmounts(la), 108_000, 162_000) {
		t.Fatalf("carried: %+v", la)
	}
	// the cost rises: the remainder absorbs it
	if err := p.ReviseCost(1, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(330_000)}, "the carrier re-invoiced", policy(t), "fin-1", hour(32)); err != nil {
		t.Fatal(err)
	}
	if err := attributed(p, "fin-1", 33); err != nil {
		t.Fatal(err)
	}
	if _, la = latest(t, p, 1); la.Amount != 330_000 || !sameAmounts(shareAmounts(la), 120_000, 180_000) || la.Attributions[0].Loss != 10_000 {
		t.Fatalf("the cost rose: %+v", la)
	}
	// the cost falls below what is carried: refused, and the way out is to state the attribution again
	if err := p.ReviseCost(1, 2, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(25_000)}, "the carrier credited", policy(t), "fin-1", hour(34)); err != nil {
		t.Fatal(err)
	}
	err := p.Preview(ApportionInput{Jobs: boxAJobs()}, boxAAfter(), "ops-2", hour(35))
	refused(t, "carried amounts that no longer fit", err, ErrInvalid, "line 1", "30000", "25000 CNY", "state them again")
	again := attribution(1, "H03", 5_000, 10_000, PartyCarrier)
	again.Reason = "the carrier credited part of the freight"
	if err = attributed(p, "fin-1", 36, again); err != nil {
		t.Fatalf("stated again: %v", err)
	}
	if _, la = latest(t, p, 1); la.Attributions[0].Carried || la.Attributions[0].Loss != 5_000 {
		t.Fatalf("stated again: %+v", la)
	}
}

// A statement that differs from the one in force is a correction and needs a reason; one equal to it is the one in
// force.
func TestACorrectionNeedsItsReasonAndAnEqualStatementIsCarried(t *testing.T) {
	p := boxAPool(t)
	if err := attributed(p, "fin-1", 30, attribution(1, "H03", 10_000, 20_000, PartyPending)); err != nil {
		t.Fatal(err)
	}
	b := boxAAfter()
	change := attribution(1, "H03", 10_000, 20_000, PartyCarrier) // the party is now known
	refused(t, "a correction without its reason", p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{change}}, b, "ops-2", hour(31)), ErrInvalid,
		"replaces a different one in force", "reason")
	change.Reason = "the carrier admitted the delay"
	change.Voucher = "CLAIM-2"
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{change}}, b, "ops-2", hour(31)); err != nil {
		t.Fatal(err)
	}
	if err := p.Submit(lastPreview(p), SubmitInput{Basis: b, Jobs: boxAJobs()}, "fin-1", hour(31)); err != nil {
		t.Fatal(err)
	}
	a, la := latest(t, p, 1)
	x := la.Attributions[0]
	if a.No != 3 || x.Carried || x.Party != PartyCarrier || x.Reason != "the carrier admitted the delay" || x.Voucher != "CLAIM-2" || x.Loss != 10_000 {
		t.Fatalf("the correction: %+v", la)
	}
	// the earlier ones are kept
	if n := len(p.State().Apportionments); n != 3 || p.State().Apportionments[1].Lines[0].Attributions[0].Party != PartyPending {
		t.Fatalf("the apportionments: %d", n)
	}
	// stating what is in force again, voucher and all, carries it (no reason is needed and nobody is asked to confirm
	// it)
	same := attribution(1, "H03", 10_000, 20_000, PartyCarrier)
	same.Voucher, same.PartyReference = "CLAIM-2", ""
	if err := attributed(p, "ops-9", 32, same); err != nil {
		t.Fatalf("the same attribution again: %v", err)
	}
	if _, la = latest(t, p, 1); !la.Attributions[0].Carried || la.Attributions[0].Reason != "the carrier admitted the delay" {
		t.Fatalf("the same attribution again is carried: %+v", la.Attributions[0])
	}
}

// Nobody confirms who bears a house's former share who asked for the rework that unloaded it (whatever the rework's end
// and wherever PHY found it) or who ended its place in the consolidation; a third person does; an attribution carried
// unchanged needs no check; the rule of V2.91 stays.
func TestTheConfirmerOfAnAttributionIsNeitherTheAskerNorWhoEndedTheHouse(t *testing.T) {
	asked := "ops-1 asked for rework 1 of load unit 1 (stuffing X-1), which unloaded house H03"
	ended := "ops-9 ended the place of house H03 in the consolidation (TRANSFERRED)"
	p := boxAPool(t)
	b := boxAAfter()
	in := ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H03", 10_000, 20_000, PartyPending)}}
	if err := p.Preview(in, b, "ops-3", hour(30)); err != nil {
		t.Fatal(err)
	}
	no := lastPreview(p)
	refused(t, "the asker", p.Submit(no, SubmitInput{Basis: b, Jobs: boxAJobs()}, "ops-1", hour(30)), ErrAttributionConfirmer, asked,
		"the attribution of house H03 on line 1 is new or changed", "another finance user submits")
	refused(t, "who ended it", p.Submit(no, SubmitInput{Basis: b, Jobs: boxAJobs()}, "ops-9", hour(30)), ErrAttributionConfirmer, ended)
	if len(p.State().Apportionments) != 1 {
		t.Fatal("a refused submission was written")
	}
	if err := p.Submit(no, SubmitInput{Basis: b, Jobs: boxAJobs()}, "fin-1", hour(30)); err != nil {
		t.Fatalf("a third person: %v", err)
	}
	// carried unchanged: the asker and the one who ended it may submit the next, V2.91's rule too being met (the
	// rework's line is carried by fin-1's submission)
	for i, who := range []string{"ops-1", "ops-9"} {
		if err := p.Preview(ApportionInput{Jobs: boxAJobs()}, b, "ops-3", hour(31+i)); err != nil {
			t.Fatal(err)
		}
		if err := p.Submit(len(p.State().Previews), SubmitInput{Basis: b, Jobs: boxAJobs()}, who, hour(31+i)); err != nil {
			t.Fatalf("%s submits what is carried: %v", who, err)
		}
	}
	// changed: they are refused again
	change := attribution(1, "H03", 5_000, 20_000, PartyPending)
	change.Reason = "less of a loss"
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{change}}, b, "ops-3", hour(33)); err != nil {
		t.Fatal(err)
	}
	refused(t, "a change by the asker", p.Submit(len(p.State().Previews), SubmitInput{Basis: b, Jobs: boxAJobs()}, "ops-1", hour(33)), ErrAttributionConfirmer, asked)

	// the person is both: the refusal says both
	both := boxAAfter()
	both.FormerHouses[0].EndedBy = "ops-1"
	q := boxAPool(t)
	if err := q.Preview(in, both, "ops-3", hour(30)); err != nil {
		t.Fatal(err)
	}
	refused(t, "both", q.Submit(lastPreview(q), SubmitInput{Basis: both, Jobs: boxAJobs()}, "ops-1", hour(30)), ErrAttributionConfirmer, asked, "and ended the place of house H03")

	// the rework is found in the reversals, or there is none: whoever asked is refused, and nobody is when none asked
	for _, from := range []string{"REVERSAL", "ENTRY"} {
		r := boxAAfter()
		r.FormerHouses[0].ReworkFrom = from
		s := boxAPool(t)
		if err := s.Preview(in, r, "ops-3", hour(30)); err != nil {
			t.Fatal(err)
		}
		refused(t, "the asker, found from "+from, s.Submit(lastPreview(s), SubmitInput{Basis: r, Jobs: boxAJobs()}, "ops-1", hour(30)), ErrAttributionConfirmer, asked)
	}
	none := boxAAfter()
	none.FormerHouses[0].ReworkFrom, none.FormerHouses[0].ReworkExecutionID, none.FormerHouses[0].ReworkNo, none.FormerHouses[0].ReworkAskedBy = "NONE", "", 0, ""
	none.FormerHouses[0].EndedBy = ""
	n := boxAPool(t)
	if err := n.Preview(in, none, "ops-3", hour(30)); err != nil {
		t.Fatal(err)
	}
	if err := n.Submit(lastPreview(n), SubmitInput{Basis: none, Jobs: boxAJobs()}, "", hour(30)); err == nil || errors.Is(err, ErrAttributionConfirmer) {
		t.Fatalf("a submitter with no name is refused for its name, not as a confirmer: %v", err)
	}
	if err := n.Submit(lastPreview(n), SubmitInput{Basis: none, Jobs: boxAJobs()}, "ops-1", hour(30)); err != nil {
		t.Fatalf("no rework unloaded the house and nobody ended it by name: %v", err)
	}
}

// A detached house is the same as a transferred one: whoever detached it does not confirm.
func TestWhoDetachedAHouseDoesNotConfirmItsAttribution(t *testing.T) {
	b := boxAAfter()
	b.FormerHouses[0].End, b.FormerHouses[0].TransferredTo, b.FormerHouses[0].ReworkAskedBy = "DETACHED", "", ""
	p := boxAPool(t)
	in := ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H03", 10_000, 0, "")}}
	if err := p.Preview(in, b, "ops-3", hour(30)); err != nil {
		t.Fatal(err)
	}
	refused(t, "who detached it", p.Submit(lastPreview(p), SubmitInput{Basis: b, Jobs: boxAJobs()}, "ops-9", hour(30)), ErrAttributionConfirmer,
		"ops-9 ended the place of house H03 in the consolidation (DETACHED)")
	if err := p.Submit(lastPreview(p), SubmitInput{Basis: b, Jobs: boxAJobs()}, "ops-1", hour(30)); err != nil {
		t.Fatalf("the person who asked for the rework did not detach the house: %v", err)
	}
}

// A loss is borne by the house's job, which must be open: with none, a loss is refused and an attribution with no loss
// goes on the job the house's share was on.
func TestALossIsBorneByTheHousesOpenJob(t *testing.T) {
	p := boxAPool(t)
	noJob := map[string]string{"H01": "JOB-H01", "H02": "JOB-H02"}
	err := p.Preview(ApportionInput{Jobs: noJob, Attributions: []AttributionInput{attribution(1, "H03", 10_000, 0, "")}}, boxAAfter(), "ops-2", hour(30))
	refused(t, "a loss with no open job", err, ErrPoolJobMissing, "house H03 bears a loss on line 1")
	if err = p.Preview(ApportionInput{Jobs: noJob, Attributions: []AttributionInput{attribution(1, "H03", 0, 20_000, PartyCarrier)}}, boxAAfter(), "ops-2", hour(30)); err != nil {
		t.Fatalf("no loss: %v", err)
	}
	if x := p.State().Previews[lastPreview(p)-1].Lines[0].Attributions[0]; x.JobID != "JOB-H03" {
		t.Fatalf("the job of the share: %+v", x)
	}
}

// With every house gone the remainder is to be allocated on the line, and the close is refused with the way out; with
// no remainder the close passes; a consolidation cancelled retains the remainder. A pool that never had a house is
// refused as before.
func TestWithNoLiveHouseTheRemainderIsToBeAllocated(t *testing.T) {
	gone := boxAAfter()
	gone.Houses = nil
	for i := range gone.FormerHouses {
		gone.FormerHouses[i].EndedAt = hour(26)
	}
	gone.FormerHouses = append([]FormerHouseFacts{{ShipmentID: "H01", End: "DETACHED", EndedBy: "ops-8", EndedAt: hour(25)}, {ShipmentID: "H02", End: "DETACHED", EndedBy: "ops-8", EndedAt: hour(25)}},
		gone.FormerHouses...)
	p := boxAPool(t)
	p.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true}})
	in := ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H01", 0, 0, ""), attribution(1, "H02", 0, 0, ""), attribution(1, "H03", 10_000, 20_000, PartyCarrier)}}
	if err := p.Preview(in, gone, "ops-2", hour(30)); err != nil {
		t.Fatal(err)
	}
	if err := p.Submit(lastPreview(p), SubmitInput{Basis: gone, Jobs: boxAJobs()}, "fin-1", hour(30)); err != nil {
		t.Fatal(err)
	}
	_, la := latest(t, p, 1)
	if len(la.Shares) != 0 || la.ToAllocate != 270_000 || la.ToAllocateLocal != 270_000 || la.Amount != 300_000 || len(la.Attributions) != 3 {
		t.Fatalf("the line: %+v", la)
	}
	err := p.Close(gone, "fin-1", hour(31))
	refused(t, "to be allocated", err, ErrPoolNotClosable, "cost line 1 has 270000 to be allocated", "add cargo", "attribute it as a loss or an exception cost", "cancel the consolidation and retain")
	// attribute it as an exception cost of the house that left: the whole cost is attributed and nothing
	// is to be allocated
	in.Attributions = []AttributionInput{attribution(1, "H01", 0, 0, ""), attribution(1, "H02", 0, 0, ""), attribution(1, "H03", 10_000, 290_000, PartyPending)}
	for i := range in.Attributions {
		in.Attributions[i].Reason = "the rest is the consolidation's"
	}
	if err = p.Preview(in, gone, "ops-2", hour(32)); err != nil {
		t.Fatal(err)
	}
	if err = p.Submit(lastPreview(p), SubmitInput{Basis: gone, Jobs: boxAJobs()}, "fin-1", hour(32)); err != nil {
		t.Fatal(err)
	}
	if _, la = latest(t, p, 1); la.ToAllocate != 0 {
		t.Fatalf("the line: %+v", la)
	}
	if err = p.Close(gone, "fin-1", hour(33)); err != nil {
		t.Fatalf("nothing is left to be allocated and the party is pending: %v", err)
	}

	// no house, no house that left: refused as before
	q := pool(t, boxA())
	recordCost(t, q, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	empty := boxA()
	empty.Houses = nil
	refused(t, "no house at all", q.Preview(ApportionInput{Jobs: boxAJobs()}, empty, "ops-2", hour(4)), ErrPoolNoBasis, "no live houses")

	// the consolidation is cancelled: the remainder is retained, attributions and all
	cancelled := gone
	cancelled.State = "CANCELLED"
	r := boxAPool(t)
	rin := ApportionInput{Method: MethodRetained, Reason: "the consolidation was cancelled", Jobs: boxAJobs(), Attributions: in.Attributions}
	rin.Attributions = []AttributionInput{attribution(1, "H01", 0, 0, ""), attribution(1, "H02", 0, 0, ""), attribution(1, "H03", 10_000, 20_000, PartyOwnOperations)}
	if err = r.Preview(rin, cancelled, "ops-2", hour(30)); err != nil {
		t.Fatal(err)
	}
	if err = r.Submit(lastPreview(r), SubmitInput{Basis: cancelled, Jobs: boxAJobs()}, "fin-1", hour(30)); err != nil {
		t.Fatal(err)
	}
	_, la = latest(t, r, 1)
	if la.Method != MethodRetained || len(la.Shares) != 0 || la.ToAllocate != 0 || la.Amount != 300_000 || len(la.Attributions) != 3 || la.Attributions[2].Loss != 10_000 {
		t.Fatalf("retained with attributions: %+v", la)
	}
	r.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true}})
	if err = r.Close(cancelled, "fin-1", hour(31)); err != nil {
		t.Fatalf("the close of a cancelled consolidation: %v", err)
	}
	// without the attribution the retain is refused too, as the former share would vanish
	s := boxAPool(t)
	err = s.Preview(ApportionInput{Method: MethodRetained, Reason: "cancelled", Jobs: boxAJobs()}, cancelled, "ops-2", hour(30))
	refused(t, "retained with no attribution", err, ErrAttributionMissing, "line 1", "house H01")
}

// The close is refused while a line holds a former share nobody attributed, even when every house is gone (before V2.93
// those shares stood); it passes once the line is attributed, with the party still pending.
func TestTheCloseNeedsEveryFormerShareAttributed(t *testing.T) {
	p := boxAPool(t)
	p.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true}})
	after := boxAAfter()
	err := p.Close(after, "fin-1", hour(31))
	refused(t, "a former share pending", err, ErrPoolNotClosable, "cost line 1 holds a former share of house H03", "attribute it by apportioning the line again")
	gone := boxAAfter()
	gone.Houses = nil
	refused(t, "every house gone", p.Close(gone, "fin-1", hour(31)), ErrPoolNotClosable, "cost line 1 holds a former share of house H03")
	cancelled := boxAAfter()
	cancelled.State = "CANCELLED"
	refused(t, "the consolidation cancelled", p.Close(cancelled, "fin-1", hour(31)), ErrPoolNotClosable, "former share of house H03")
	if len(p.State().Closures) != 0 {
		t.Fatal("a refused close was written")
	}
	if err = attributed(p, "fin-1", 32, attribution(1, "H03", 10_000, 20_000, PartyPending)); err != nil {
		t.Fatal(err)
	}
	if err = p.Close(after, "fin-1", hour(33)); err != nil {
		t.Fatalf("a pending party does not stop the close: %v", err)
	}
}

// A house PHY lists as live again is no former house: its old attribution lapses and it takes its share of the line.
func TestAnAttributionLapsesWhenTheHouseIsAttachedAgain(t *testing.T) {
	p := boxAPool(t)
	if err := attributed(p, "fin-1", 30, attribution(1, "H03", 10_000, 20_000, PartyPending)); err != nil {
		t.Fatal(err)
	}
	back := boxA()
	back.Revision = 9
	if err := apportion(p, ApportionInput{Jobs: boxAJobs()}, back, "fin-1", hour(31)); err != nil {
		t.Fatal(err)
	}
	_, la := latest(t, p, 1)
	if len(la.Attributions) != 0 || !sameAmounts(shareAmounts(la), 100_000, 150_000, 50_000) {
		t.Fatalf("the house is live again: %+v", la)
	}
}

// MANUAL shares sum to the remainder.
func TestManualSharesSumToWhatRemainsAfterTheAttributions(t *testing.T) {
	p := boxAPool(t)
	in := ApportionInput{LineNos: []int{1}, Method: MethodManual, Reason: "agreed by phone", Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H03", 10_000, 20_000, PartyCarrier)},
		Manual: []ManualShare{{ShipmentID: "H01", Amount: 150_000}, {ShipmentID: "H02", Amount: 150_000}}}
	refused(t, "shares that sum to the cost", p.Preview(in, boxAAfter(), "ops-2", hour(30)), ErrManualShares, "shares sum to 300000, the line is 270000")
	in.Manual = []ManualShare{{ShipmentID: "H01", Amount: 100_000}, {ShipmentID: "H02", Amount: 170_000}}
	if err := p.Preview(in, boxAAfter(), "ops-2", hour(30)); err != nil {
		t.Fatal(err)
	}
	if got := shareAmounts(p.State().Previews[lastPreview(p)-1].Lines[0]); !sameAmounts(got, 100_000, 170_000) {
		t.Fatalf("the manual shares: %v", got)
	}
}

// A pool and an apportionment with attributions are restored as they were, and what State gives is a copy.
func TestAnApportionmentWithAttributionsIsRestored(t *testing.T) {
	p := boxAPool(t)
	if err := attributed(p, "fin-1", 30, attribution(1, "H03", 10_000, 20_000, PartyCarrier)); err != nil {
		t.Fatal(err)
	}
	st := p.State()
	st.Apportionments[1].Lines[0].Attributions[0].Loss = 1
	st.Previews[1].Lines[0].Attributions[0].Loss = 1
	if _, la := latest(t, p, 1); la.Attributions[0].Loss != 10_000 || p.State().Previews[1].Lines[0].Attributions[0].Loss != 10_000 {
		t.Fatal("State gave the pool's own attributions")
	}
	q, err := RestorePool(p.State())
	if err != nil {
		t.Fatal(err)
	}
	if _, la := latest(t, q, 1); len(la.Attributions) != 1 || la.Attributions[0].Loss != 10_000 || !strings.HasPrefix(la.Attributions[0].JobID, "JOB-H03") {
		t.Fatalf("restored: %+v", la)
	}
	if added, _, _, apportionments := q.Added(); len(added) != 0 || len(apportionments) != 0 {
		t.Fatalf("a restored pool has nothing to write: %+v", apportionments)
	}
}

// A pool that never had a house leave is previewed, submitted and closed as before: no attribution, nothing to be
// allocated, no former share.
func TestAPoolWithNoFormerHouseIsAsBefore(t *testing.T) {
	p := boxAPool(t)
	p.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true}})
	_, la := latest(t, p, 1)
	if len(la.Attributions) != 0 || la.ToAllocate != 0 || la.ToAllocateLocal != 0 || !sameAmounts(shareAmounts(la), 100_000, 150_000, 50_000) {
		t.Fatalf("the line: %+v", la)
	}
	if got := p.FormerShares(boxA()); len(got) != 0 || len(p.HousesToAttribute(boxA())) != 0 {
		t.Fatalf("former shares of a pool no house left: %+v", got)
	}
	if err := p.Close(boxA(), "fin-1", hour(31)); err != nil {
		t.Fatalf("the close: %v", err)
	}
}

// The attributions are inputs: a statement for a house with no share and none in force is refused even when PHY lists
// the house.
func TestAnAttributionNeedsAShareOrAnAttributionInForce(t *testing.T) {
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	if err := apportion(p, ApportionInput{Jobs: boxAJobs(), NotUsed: []string{"H03"}}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	err := p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H03", 1, 0, "")}}, boxAAfter(), "ops-2", hour(30))
	refused(t, "a house that bore nothing", err, ErrInvalid, "house H03 holds no former share on line 1 and no attribution in force on it")
}

// A submission that carries a loss needs the open job of the house that bears it, as the preview read it: with the job
// cancelled since, the submission is refused, and the preview is made again; a loss of 0 needs no open job.
func TestASubmissionWithALossNeedsTheHousesOpenJob(t *testing.T) {
	p := boxAPool(t)
	b := boxAAfter()
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H03", 10_000, 20_000, PartyCarrier)}}, b, "ops-2", hour(30)); err != nil {
		t.Fatal(err)
	}
	no := lastPreview(p)
	closed := map[string]string{"H01": "JOB-H01", "H02": "JOB-H02"}
	refused(t, "the job is not open", p.Submit(no, SubmitInput{Basis: b, Jobs: closed}, "fin-1", hour(30)), ErrPoolJobMissing, "house H03 bears a loss on line 1", "JOB-H03 is not open",
		"preview again")
	refused(t, "another job", p.Submit(no, SubmitInput{Basis: b, Jobs: map[string]string{"H01": "JOB-H01", "H02": "JOB-H02", "H03": "JOB-X"}}, "fin-1", hour(30)), ErrPoolJobMissing)
	if len(p.State().Apportionments) != 1 {
		t.Fatal("a refused submission was written")
	}
	if err := p.Submit(no, SubmitInput{Basis: b, Jobs: boxAJobs()}, "fin-1", hour(30)); err != nil {
		t.Fatalf("the job is open: %v", err)
	}
	// a loss of 0 is borne by nobody: no open job is needed
	q := boxAPool(t)
	if err := q.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H03", 0, 20_000, PartyCarrier)}}, b, "ops-2", hour(30)); err != nil {
		t.Fatal(err)
	}
	if err := q.Submit(lastPreview(q), SubmitInput{Basis: b, Jobs: closed}, "fin-1", hour(30)); err != nil {
		t.Fatalf("no loss: %v", err)
	}
}

// A line that costs nothing, with no house live and nothing attributed on it, is not previewed: it needs no
// apportionment and the preview of the other lines goes on. With the houses live it is divided as always.
func TestAZeroLineWithNoLiveHouseIsRefusedAtThePreview(t *testing.T) {
	gone := boxAAfter()
	gone.Houses = nil
	gone.FormerHouses = append([]FormerHouseFacts{{ShipmentID: "H01", End: "DETACHED", EndedBy: "ops-8", EndedAt: hour(25)}, {ShipmentID: "H02", End: "DETACHED", EndedBy: "ops-8", EndedAt: hour(25)}},
		gone.FormerHouses...)
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	if err := p.RecordCost("ZERO_FEE", "", "Z-1", MethodChargeable, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(0)}, "waived", policy(t), "ops-1", hour(3)); err != nil {
		t.Fatal(err)
	}
	if err := apportion(p, ApportionInput{LineNos: []int{1}, Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	all := []AttributionInput{attribution(1, "H01", 0, 0, ""), attribution(1, "H02", 0, 0, ""), attribution(1, "H03", 0, 0, "")}
	err := p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: all}, gone, "ops-2", hour(30))
	refused(t, "the zero line", err, ErrInvalid, "cost line 2 costs nothing", "no house is live", "preview the other lines")
	if err = p.Preview(ApportionInput{LineNos: []int{1}, Jobs: boxAJobs(), Attributions: all}, gone, "ops-2", hour(30)); err != nil {
		t.Fatalf("the other line: %v", err)
	}
	// with a house live the zero line is divided into shares of nothing, as in V2.92
	if err = p.Preview(ApportionInput{LineNos: []int{2}, Jobs: boxAJobs()}, boxA(), "ops-2", hour(30)); err != nil {
		t.Fatalf("the zero line with the houses live: %v", err)
	}
}

// A statement is the one in force only when every figure is equal: a changed exception cost, party, reference or
// voucher is a correction (it needs its reason, and the confirmer is checked), as a changed loss is.
func TestAStatementIsTheOneInForceOnlyWhenEveryFigureIsEqual(t *testing.T) {
	base := AttributionInput{LineNo: 1, ShipmentID: "H03", Loss: 10_000, Exception: 20_000, Party: PartyCarrier, PartyReference: "claim 7", Voucher: "CLAIM-1"}
	p := boxAPool(t)
	if err := attributed(p, "fin-1", 30, base); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(a *AttributionInput){
		"the loss":      func(a *AttributionInput) { a.Loss = 11_000 },
		"the exception": func(a *AttributionInput) { a.Exception = 21_000 },
		"the party":     func(a *AttributionInput) { a.Party = PartyWarehouse },
		"the reference": func(a *AttributionInput) { a.PartyReference = "claim 8" },
		"the voucher":   func(a *AttributionInput) { a.Voucher = "CLAIM-2" },
	} {
		a := base
		change(&a)
		refused(t, name+" without a reason", p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{a}}, boxAAfter(), "ops-2", hour(31)), ErrInvalid, "a correction states its reason")
		a.Reason = "corrected"
		b := boxAAfter()
		if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{a}}, b, "ops-2", hour(31)); err != nil {
			t.Fatalf("%s with a reason: %v", name, err)
		}
		no := lastPreview(p)
		if x := p.State().Previews[no-1].Lines[0].Attributions[0]; x.Carried {
			t.Fatalf("%s: carried", name)
		}
		refused(t, name+" by the asker", p.Submit(no, SubmitInput{Basis: b, Jobs: boxAJobs()}, "ops-1", hour(31)), ErrAttributionConfirmer)
	}
}
