package domain

import (
	"errors"
	"testing"
	"time"
)

// acceptedOneOff is an accepted ONE_OFF quote of three lines: AIR_FREIGHT RATED at 20.00 per chargeable kg on 150 kg,
// DOC_FEE overridden as a lump sum and PICKUP entered by hand.
func acceptedOneOff(t *testing.T) *Quote {
	t.Helper()
	d := draft(0)
	d.Currency, d.DiscountThresholdBP = "CNY", 1000 // the document fee's 6.7% off needs no approval
	d.Scope = &QuoteScope{Mode: ModeAir, ContainerMode: ContainerLoose, Origin: "CNPVG", Destination: "DEFRA", Cargo: Cargo{Pieces: 3, GrossGrams: 100_000,
		VolumeCM3: 900_000}}
	d.Lines = []Line{
		{ChargeCode: "AIR_FREIGHT", Sell: cny(300_000), Cost: cny(180_000), Source: SourceRated,
			Rated: &RatedRef{SnapshotID: "RS-1", SellLineNo: 1, CostLineNo: 4, RatedSell: cny(300_000)},
			Basis: LineBasis{Kind: BasisPerUnit, Unit: UnitKgChargeable, Rate: UnitRate{Minor: 2000}, Quantity: 150_000}},
		{ChargeCode: "DOC_FEE", Sell: cny(14_000), Cost: cny(0), Source: SourceRatedOverridden,
			Rated: &RatedRef{SnapshotID: "RS-1", SellLineNo: 2, RatedSell: cny(15_000), OverrideReason: "agreed"}, Basis: LineBasis{Kind: BasisLumpSum}},
		{ChargeCode: "PICKUP", Sell: cny(20_000), Cost: cny(15_000)},
	}
	q, err := PriceQuote(d, at)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Accept("sales-1", EvidenceEmail, "mail", 2, at.Add(time.Hour), at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return q
}

// OD-7 A: a quote's per-unit lines are re-evaluated on the cargo shipped at its frozen rates, by the quantity rule of
// the card line each was rated from; lump sums and manual lines are as quoted.
func TestQuoteOnShipment(t *testing.T) {
	q := acceptedOneOff(t)
	sellRule := airFreight(1) // chargeable weight at 6000 cm3/kg in 0.5 kg steps
	buyRule := airFreight(1)
	rules := map[int]RatedRule{1: {Sell: sellRule, Cost: &buyRule, CostRate: UnitRate{Minor: 1200}, CostUnit: UnitKgChargeable}}
	for name, tc := range map[string]struct {
		cargo      Cargo
		sell, cost int64
		quantity   int64
	}{
		// 1,000 kg gross: 1,000 kg at 20.00 = 20,000.00, bought at 12.00 = 12,000.00
		"heavier than quoted": {Cargo{Pieces: 30, GrossGrams: 1_000_000}, 2_000_000, 1_200_000, 1_000_000},
		// 10 m3 at 6000 cm3/kg is 1,666.67 kg, 1,667 kg in 0.5 kg steps
		"by volume": {Cargo{Pieces: 30, GrossGrams: 100_000, VolumeCM3: 10_000_000}, 3_334_000, 2_000_400, 1_667_000},
		// 10 kg: the frozen rate on 10 kg, no minimum (OD-7 A: the quote's frozen unit rates)
		"lighter than quoted": {Cargo{Pieces: 1, GrossGrams: 10_000}, 20_000, 12_000, 10_000},
	} {
		lines, err := q.OnShipment(tc.cargo, rules)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f, doc, pickup := lines[0], lines[1], lines[2]
		switch {
		case !f.Reevaluated || f.Sell != cny(tc.sell) || f.Cost != cny(tc.cost) || f.Quantity != tc.quantity || f.Unit != UnitKgChargeable:
			t.Errorf("%s: freight %+v", name, f)
		case doc.Reevaluated || doc.Sell != cny(14_000) || pickup.Reevaluated || pickup.Sell != cny(20_000) || pickup.Cost != cny(15_000):
			t.Errorf("%s: lump sums %+v %+v", name, doc, pickup)
		}
	}
	// a cost from a BUY line a minimum bounded stays as quoted; a cargo without the unit leaves the line as quoted
	lines, err := q.OnShipment(Cargo{Pieces: 30, GrossGrams: 1_000_000}, map[int]RatedRule{1: {Sell: sellRule}})
	if err != nil || lines[0].Sell != cny(2_000_000) || lines[0].Cost != cny(180_000) {
		t.Fatalf("a cost as quoted: %+v %v", lines[0], err)
	}
	if lines, err = q.OnShipment(Cargo{Pieces: 3}, rules); err != nil || lines[0].Reevaluated || lines[0].Sell != cny(300_000) {
		t.Fatalf("no weight or volume shipped: %+v %v", lines[0], err)
	}
	documents := sellRule
	documents.Unit = UnitDocument
	if _, err = q.OnShipment(Cargo{Pieces: 3, GrossGrams: 1}, map[int]RatedRule{1: {Sell: documents}}); !errors.Is(err, ErrInvalidQuote) {
		t.Fatalf("a card line of another unit: %v", err)
	}
	if Shipped(RateLine{Unit: UnitDocument}, Cargo{Documents: 3}) != 0 {
		t.Fatal("documents are not shipped quantities")
	}
	// only an accepted ONE_OFF quote is re-evaluated
	pending, _ := PriceQuote(func() QuoteDraft { d := draft(0, freight(100, 10)); d.Lines[0].ChargeCode = "AIR_FREIGHT"; return d }(), at)
	if _, err = pending.OnShipment(Cargo{GrossGrams: 1}, nil); !errors.Is(err, ErrInvalidQuote) {
		t.Fatalf("a quote not accepted: %v", err)
	}
	if _, err = q.OnShipment(Cargo{GrossGrams: -1}, rules); !errors.Is(err, ErrInvalidQuote) {
		t.Fatalf("negative cargo: %v", err)
	}
}
