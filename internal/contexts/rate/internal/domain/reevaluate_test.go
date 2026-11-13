package domain

import (
	"errors"
	"testing"
	"time"
)

// The cards a quote is rated from: the tariff's freight at 30/25/20 per chargeable kg (breaks at 45 and 100 kg,
// at least 300.00), a fuel surcharge of 10% of the freight (at most 500.00) and a flat document fee; the co-loader buys
// the freight at 20.00, 12.00 from 100 kg, at least 200.00.
var (
	reFreight = airFreight(1)
	reFuel    = RateLine{No: 2, ChargeCode: "FUEL_SURCHARGE", Currency: "CNY", Calculator: CalcPercentOf, PercentBP: 1000, BaseCode: "AIR_FREIGHT",
		Maximum: 50_000}
	reDoc = flat(3, "DOC_FEE", "CNY", 15_000)
	reBuy = RateLine{No: 1, ChargeCode: "AIR_FREIGHT", Currency: "CNY", Calculator: CalcPerUnit, Unit: UnitKgChargeable, Rate: UnitRate{Minor: 2000},
		Minimum: 20_000, Step: 500, VolumeFactor: 6000, Breaks: []RateBreak{{From: 100_000, Rate: 1200}}}
	reCards = []PublishedVersion{version("T1", ScopeTariff, "", airEntry(1, reFreight, reFuel, reDoc)),
		version("S1", ScopeSupplier, "coloader", airEntry(1, reBuy))}
)

// ratedQuote is the accepted ONE_OFF quote RATE builds from an evaluation of cargo, every SELL line RATED (with the BUY
// line of its code as its cost), plus a manual line, and the rules RATE reads back for re-evaluating it.
func ratedQuote(t *testing.T, cargo Cargo) (*Quote, map[int]RatedRule) {
	t.Helper()
	in := EvaluationInput{Purpose: PurposeQuoteLine, LegalEntityID: "LE", Sides: []Side{SideSell, SideBuy}, Customer: CustomerRef{PartyID: "P", PartyRevision: 2},
		Mode: ModeAir, ContainerMode: ContainerLoose, Origin: "CNPVG", Destination: "DEFRA", ServiceLevel: "STANDARD", Commodity: CommodityGeneral,
		At: t0.Add(time.Hour), Cargo: cargo}
	_, lines, err := Evaluate(in, reCards)
	if err != nil {
		t.Fatal(err)
	}
	cards := map[RateCardID]map[ChargeCode]RateLine{"T1": {"AIR_FREIGHT": reFreight, "FUEL_SURCHARGE": reFuel, "DOC_FEE": reDoc}, "S1": {"AIR_FREIGHT": reBuy}}
	side := func(sl SnapshotLine) (*UnitRule, *PercentRule) {
		card := cards[sl.RateCardID][sl.ChargeCode]
		switch sl.Calculator {
		case CalcPerUnit:
			return &UnitRule{Line: card}, nil
		case CalcPercentOf:
			return nil, &PercentRule{Line: card, BP: sl.PercentBP}
		}
		return nil, nil
	}
	d := draft(0)
	d.Currency = "CNY"
	d.Scope = &QuoteScope{Mode: ModeAir, ContainerMode: ContainerLoose, Origin: "CNPVG", Destination: "DEFRA", Cargo: cargo}
	rules := map[int]RatedRule{}
	for _, sl := range lines {
		if sl.Side != SideSell || sl.Gap != GapNone {
			continue
		}
		l := Line{ChargeCode: sl.ChargeCode, Sell: sl.Amount, Cost: cny(0), Source: SourceRated,
			Rated: &RatedRef{SnapshotID: "RS-1", SellLineNo: sl.LineNo, RatedSell: sl.Amount}, Basis: LineBasis{Kind: BasisLumpSum}}
		if sl.Calculator == CalcPerUnit && !sl.MinimumApplied && !sl.MaximumApplied {
			l.Basis = LineBasis{Kind: BasisPerUnit, Unit: sl.Unit, Rate: sl.Rate, Quantity: sl.Charged}
		}
		var rule RatedRule
		rule.Sell, rule.SellPercent = side(sl)
		for _, bl := range lines {
			if bl.Side == SideBuy && bl.Gap == GapNone && bl.ChargeCode == sl.ChargeCode {
				l.Cost, l.Rated.CostLineNo = bl.Amount, bl.LineNo
				rule.Cost, rule.CostPercent = side(bl)
			}
		}
		d.Lines = append(d.Lines, l)
		rules[len(d.Lines)] = rule
	}
	d.Lines = append(d.Lines, Line{ChargeCode: "PICKUP", Sell: cny(20_000), Cost: cny(15_000)})
	q, err := PriceQuote(d, at)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Accept("sales-1", EvidenceEmail, "mail", 2, at.Add(time.Hour), at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return q, rules
}

func byCode(lines []ShippedLine) map[ChargeCode]ShippedLine {
	out := map[ChargeCode]ShippedLine{}
	for _, l := range lines {
		out[l.ChargeCode] = l
	}
	return out
}

// OD-7 A: a quote's RATED lines re-evaluated on the cargo shipped by the card lines they were rated from; on the cargo
// quoted, the quote itself.
func TestQuoteOnShipment(t *testing.T) {
	// on the cargo quoted, every line is the quoted line: 150 kg, 90 kg (charged as 100 kg, the cheaper break), 5 kg (the
	// minimum), 10 m3 (by volume, the fuel at its maximum) and 2 t
	for _, cargo := range []Cargo{{Pieces: 3, GrossGrams: 150_000}, {Pieces: 2, GrossGrams: 90_000}, {Pieces: 1, GrossGrams: 5_000},
		{Pieces: 9, GrossGrams: 100_000, VolumeCM3: 10_000_000}, {Pieces: 40, GrossGrams: 2_000_000}} {
		q, rules := ratedQuote(t, cargo)
		lines, err := q.OnShipment(cargo, rules)
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range q.State().Lines {
			if lines[i].Sell != l.Sell || lines[i].Cost != l.Cost {
				t.Errorf("on %+v, line %s: %+v, quoted %v / %v", cargo, l.ChargeCode, lines[i], l.Sell, l.Cost)
			}
		}
	}
	q, rules := ratedQuote(t, Cargo{Pieces: 3, GrossGrams: 150_000})
	for name, tc := range map[string]struct {
		cargo                    Cargo
		freight, fuel, cost, qty int64
	}{
		// 1,500 kg at 20.00 = 30,000.00 bought at 12.00 = 18,000.00; the fuel at its maximum of 500.00
		"heavier than quoted": {Cargo{Pieces: 30, GrossGrams: 1_500_000}, 3_000_000, 50_000, 1_800_000, 1_500_000},
		// 60 kg at 25.00 = 1,500.00 bought at 20.00 = 1,200.00; the fuel 150.00
		"lighter, in another break": {Cargo{Pieces: 2, GrossGrams: 60_000}, 150_000, 15_000, 120_000, 60_000},
		// 5 kg: the minimum of 300.00, bought at its minimum of 200.00; the fuel 30.00
		"under the minimum": {Cargo{Pieces: 1, GrossGrams: 5_000}, 30_000, 3_000, 20_000, 5_000},
	} {
		lines, err := q.OnShipment(tc.cargo, rules)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := byCode(lines)
		f, fuel, doc, pickup := got["AIR_FREIGHT"], got["FUEL_SURCHARGE"], got["DOC_FEE"], got["PICKUP"]
		switch {
		case !f.Reevaluated || f.Sell != cny(tc.freight) || f.Cost != cny(tc.cost) || f.Quantity != tc.qty || f.Unit != UnitKgChargeable:
			t.Errorf("%s: freight %+v", name, f)
		case !fuel.Reevaluated || fuel.Sell != cny(tc.fuel):
			t.Errorf("%s: fuel %+v", name, fuel)
		case doc.Reevaluated || doc.Sell != cny(15_000) || pickup.Reevaluated || pickup.Sell != cny(20_000) || pickup.Cost != cny(15_000):
			t.Errorf("%s: flat and manual lines %+v %+v", name, doc, pickup)
		}
	}
	// a cargo without weight or volume leaves the lines as quoted
	if lines, err := q.OnShipment(Cargo{Pieces: 3}, rules); err != nil || byCode(lines)["AIR_FREIGHT"].Reevaluated ||
		byCode(lines)["FUEL_SURCHARGE"].Reevaluated {
		t.Fatalf("no weight or volume shipped: %+v %v", lines, err)
	}
	documents := reFreight
	documents.Unit = UnitDocument
	if _, _, on, _ := (UnitRule{Line: documents}).onCargo(Cargo{Documents: 3}); on {
		t.Fatal("documents are not shipped quantities")
	}
	// only an accepted ONE_OFF quote is re-evaluated, on cargo within RATE's bounds
	pending, _ := PriceQuote(func() QuoteDraft { d := draft(0, freight(100, 10)); d.Lines[0].ChargeCode = "AIR_FREIGHT"; return d }(), at)
	if _, err := pending.OnShipment(Cargo{GrossGrams: 1}, nil); !errors.Is(err, ErrInvalidQuote) {
		t.Fatalf("a quote not accepted: %v", err)
	}
	for _, c := range []Cargo{{GrossGrams: -1}, {Documents: -1}, {GrossGrams: MaxQuantity + 1}} {
		if _, err := q.OnShipment(c, rules); !errors.Is(err, ErrInvalidQuote) {
			t.Errorf("cargo %+v: %v", c, err)
		}
	}
}
