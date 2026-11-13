package domain

import (
	"errors"
	"testing"
)

// Rated estimates (RATE V0.5 part 2): a job takes RATE's rated lines once and never with a quote; a pool takes one
// rated estimate of costs.
func TestRatedEstimates(t *testing.T) {
	p := policy(t)
	lines := []EstimateLine{
		{LineNo: 1, ChargeCode: "AIR_FREIGHT", HasSell: true, Sell: cny(300_000), HasCost: true, Cost: cny(180_000), CreditorPartyID: "coloader", CreditorRevision: 2},
		{LineNo: 2, ChargeCode: "DOC_FEE", HasSell: true, Sell: cny(15_000)},
		{LineNo: 3, ChargeCode: "THC", HasCost: true, Cost: cny(8_000), CreditorPartyID: "coloader", CreditorRevision: 2},
	}
	j := job(t, p)
	if err := j.ApplyEstimate(Basis{BasisID: "FIN-1", PlanVersion: 2}, 4, nil, p, "fin-1", hour(2)); !errors.Is(err, ErrNotPriceable) {
		t.Fatalf("nothing rated: %v", err)
	}
	unconfirmed := append([]EstimateLine(nil), lines...)
	unconfirmed[2].CreditorRevision = 0
	if err := j.ApplyEstimate(Basis{BasisID: "FIN-1", PlanVersion: 2}, 4, unconfirmed, p, "fin-1", hour(2)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a supplier ORG did not confirm: %v", err)
	}
	j = job(t, p)
	if err := j.ApplyEstimate(Basis{BasisID: "FIN-1", PlanVersion: 2}, 4, lines, p, "fin-1", hour(2)); err != nil {
		t.Fatal(err)
	}
	st := j.State()
	if len(st.Bases) != 1 || st.Bases[0].Purpose != PurposeShipmentEstimate || len(st.Lines) != 3 || st.Lines[0].Source != SourceRated ||
		st.Lines[0].Sell[0].PartyRevision != 4 || st.Lines[0].Cost[0].PartyID != "coloader" || len(st.Lines[1].Cost) != 0 || len(st.Lines[2].Sell) != 0 {
		t.Fatalf("job: %+v", st)
	}
	if err := j.ApplyEstimate(Basis{BasisID: "FIN-2", PlanVersion: 2}, 4, lines, p, "fin-1", hour(2)); !errors.Is(err, ErrBasisApplied) {
		t.Fatalf("a second estimate: %v", err)
	}
	if err := j.ApplyBasis(Basis{BasisID: "QUOTE:Q-1@2", QuoteID: "Q-1", QuoteRevision: 2, OrderID: "O-A"}, "party-a", 4,
		[]BasisLine{{LineNo: 1, ChargeCode: "AIR_FREIGHT", Sell: cny(1)}}, p, "fin-1", hour(2)); !errors.Is(err, ErrBasisApplied) {
		t.Fatalf("a quote on a rated job: %v", err)
	}

	pl := pool(t, airBasis())
	costs := []EstimateLine{lines[0], lines[2]}
	if err := pl.RecordEstimate("FIN-3", lines, MethodChargeable, p, "fin-1", hour(3)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a sell line in a pool: %v", err)
	}
	pl = pool(t, airBasis())
	costs[0].HasSell = false
	if err := pl.RecordEstimate("FIN-3", costs, MethodChargeable, p, "fin-1", hour(3)); err != nil {
		t.Fatal(err)
	}
	if ls := pl.State().Lines; len(ls) != 2 || ls[0].Source != SourceRated || ls[0].BasisID != "FIN-3" || ls[1].BasisLineNo != 3 ||
		ls[0].Versions[0].CreditorPartyID != "coloader" {
		t.Fatalf("pool: %+v", ls)
	}
	if err := pl.RecordEstimate("FIN-4", costs, MethodChargeable, p, "fin-1", hour(3)); !errors.Is(err, ErrBasisApplied) {
		t.Fatalf("a second estimate in the pool: %v", err)
	}
}
