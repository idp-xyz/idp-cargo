package domain

import (
	"math/rand"
	"testing"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

func qty(unit kernel.UnitCode, amount int64) kernel.Quantity {
	return kernel.Quantity{Unit: unit, Amount: amount}
}

// permutations calls f with every order of the quantities.
func permutations(qs []kernel.Quantity, f func([]kernel.Quantity)) {
	var rec func(int)
	rec = func(k int) {
		if k == len(qs) {
			f(qs)
			return
		}
		for i := k; i < len(qs); i++ {
			qs[k], qs[i] = qs[i], qs[k]
			rec(k + 1)
			qs[k], qs[i] = qs[i], qs[k]
		}
	}
	rec(0)
}

func summed(qs []kernel.Quantity) kernel.Quantity {
	var s WeightSum
	for _, q := range qs {
		s.Add(q)
	}
	return s.Quantity()
}

// TestWeightsInDifferentUnitsAreAddedInGrams, by hand, in every order of the quantities:
//   - 3,000 KGM and 3 TNE are 3,000 x 1,000 = 3,000,000 g and 3 x 1,000,000 = 3,000,000 g: 6,000,000 GRM (V2.83 read
//     3,003 TNE);
//   - 500 GRM, 2 KGM and 1 TNE are 500 + 2,000 + 1,000,000 = 1,002,500 GRM;
//   - 10 LBR are 4,535.9237 g, rounded up to 4,536 g as every conversion of PHY rounds, and 1 KGM is 1,000 g: 5,536
//     GRM;
//   - quantities in one unit are added as they were: 3 and 4 KGM are 7 KGM, 5 GRM and 5 GRM 10 GRM, and nothing is
//     zero.
func TestWeightsInDifferentUnitsAreAddedInGrams(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []kernel.Quantity
		want kernel.Quantity
	}{
		"two units":              {[]kernel.Quantity{qty("KGM", 3000), qty("TNE", 3)}, qty("GRM", 6000000)},
		"three units":            {[]kernel.Quantity{qty("GRM", 500), qty("KGM", 2), qty("TNE", 1)}, qty("GRM", 1002500)},
		"a pound rounds up":      {[]kernel.Quantity{qty("LBR", 10), qty("KGM", 1)}, qty("GRM", 5536)},
		"one unit, kilograms":    {[]kernel.Quantity{qty("KGM", 3), qty("KGM", 4)}, qty("KGM", 7)},
		"one unit, grams":        {[]kernel.Quantity{qty("GRM", 5), qty("GRM", 5)}, qty("GRM", 10)},
		"one unit, tonnes":       {[]kernel.Quantity{qty("TNE", 2), qty("TNE", 1), qty("TNE", 4)}, qty("TNE", 7)},
		"one unit, pounds":       {[]kernel.Quantity{qty("LBR", 10), qty("LBR", 10)}, qty("LBR", 20)},
		"one unknown unit":       {[]kernel.Quantity{qty("XYZ", 5), qty("XYZ", 6)}, qty("XYZ", 11)},
		"nothing":                {nil, kernel.Quantity{}},
		"an empty one is none":   {[]kernel.Quantity{{}, qty("KGM", 3), {}}, qty("KGM", 3)},
		"zero amounts in a unit": {[]kernel.Quantity{qty("KGM", 3), qty("TNE", 0)}, qty("GRM", 3000)},
	} {
		t.Run(name, func(t *testing.T) {
			permutations(append([]kernel.Quantity(nil), tc.in...), func(order []kernel.Quantity) {
				if got := summed(order); got != tc.want {
					t.Errorf("%v added = %v, want %v", order, got, tc.want)
				}
			})
		})
	}
}

// TestAWeightInAUnitPHYCannotConvertIsNeverReadAsAWeight: with a unit PHY cannot convert among the quantities the sum
// is a quantity in that unit (the smallest unit code of those, whatever the order, with the amounts in that unit added
// and at least 1), which FIN's conversion refuses; neither a zero, which FIN reads as a weight of nothing, nor grams
// that left the quantity out. By hand: 5 XYZ with 3 KGM is 5 XYZ; 5 XYZ, 4 ABC and 3 KGM is 4 ABC (ABC before XYZ); 5
// XYZ, 7 XYZ and 1 KGM is 12 XYZ; 0 XYZ with 3 KGM is 1 XYZ.
func TestAWeightInAUnitPHYCannotConvertIsNeverReadAsAWeight(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []kernel.Quantity
		want kernel.Quantity
	}{
		"one unknown among known": {[]kernel.Quantity{qty("XYZ", 5), qty("KGM", 3)}, qty("XYZ", 5)},
		"two unknown units":       {[]kernel.Quantity{qty("XYZ", 5), qty("ABC", 4), qty("KGM", 3)}, qty("ABC", 4)},
		"one unknown unit twice":  {[]kernel.Quantity{qty("XYZ", 5), qty("XYZ", 7), qty("KGM", 1)}, qty("XYZ", 12)},
		"unknown with no amount":  {[]kernel.Quantity{qty("XYZ", 0), qty("KGM", 3)}, qty("XYZ", 1)},
	} {
		t.Run(name, func(t *testing.T) {
			permutations(append([]kernel.Quantity(nil), tc.in...), func(order []kernel.Quantity) {
				got := summed(order)
				if got != tc.want || got.Amount == 0 {
					t.Errorf("%v added = %v, want %v", order, got, tc.want)
				}
			})
		})
	}
}

// houseOf measures SH-1 in a stuffing with the lines, over the lots.
func houseOf(t *testing.T, l map[CargoLotID]*CargoLot, lines ...LoadLine) measure {
	t.Helper()
	x := stuffedWith(t, "X-A", 1, lines)
	packs, w, v := x.HouseMeasure("SH-1", l)
	return measure{packs, w, v}
}

func bareLot(t *testing.T) *CargoLot {
	t.Helper()
	lot, err := ReceiveCargo("T", "L-0", cfs, seaIntent(3000), cartons(4, ConditionGood, t0.Add(0)), "ops", t0.Add(0))
	if err != nil {
		t.Fatal(err)
	}
	return lot
}

func ownIn(unit kernel.UnitCode, lot CargoLotID, packs, weight int64) LoadLine {
	return LoadLine{ShipmentID: "SH-1", LotID: lot, EntryNo: 1, Packs: packs, GrossWeight: qty(unit, weight), Basis: BasisMeasured}
}

// TestAHouseMeasureConvertsWeightsOfDifferentUnits. A lot L-A of 3 packs weighed at 3,000 kg, all apportioned (3,000
// KGM), with a line of 3 packs weighed for itself at 3 TNE on an unmeasured lot: 3,000,000 g + 3,000,000 g, 6,000,000
// GRM and 6 packs, whichever line was loaded first. A line weighed for itself in another unit than its own lot's
// measure, on the lot L-B of 6 packs and 3,000 kg: 1 pack at 3 TNE and 5 apportioned; the allotment converts before it
// subtracts (V2.84): 3,000 KGM = 3,000,000 g less 3 TNE = 3,000,000 g leaves nothing for the 5 packs that complete the
// lot, so the house is 3,000,000 + 0 = 3,000,000 GRM (V2.83 took 3 off 3,000 and read 5,997,000). A unit that cannot be
// converted among them: the weight is 5 XYZ, not a sum. A house in one unit is read as ever: 7 KGM and 3,000 KGM.
func TestAHouseMeasureConvertsWeightsOfDifferentUnits(t *testing.T) {
	a, bare := measuredLot(t, "L-A", 3, 3000, 0, ""), bareLot(t)
	all := lots(a, bare)
	apportioned, tonnes := apportionedLine("L-A", 3), ownIn("TNE", "L-0", 3, 3)
	for _, lines := range [][]LoadLine{{apportioned, tonnes}, {tonnes, apportioned}} {
		if got := houseOf(t, all, lines...); got != (measure{6, qty("GRM", 6000000), kernel.Quantity{}}) {
			t.Errorf("%v: %v", lines, got)
		}
	}
	b := measuredLot(t, "L-B", 6, 3000, 0, "")
	same := lots(b)
	if got := houseOf(t, same, ownIn("TNE", "L-B", 1, 3), apportionedLine("L-B", 5)); got != (measure{6, qty("GRM", 3000000), kernel.Quantity{}}) {
		t.Errorf("a line in another unit than its lot's: %v", got)
	}
	if got := houseOf(t, all, ownIn("XYZ", "L-0", 2, 5), apportionedLine("L-A", 3)); got != (measure{5, qty("XYZ", 5), kernel.Quantity{}}) {
		t.Errorf("an unconvertible unit: %v", got)
	}
	if got := houseOf(t, all, ownIn("KGM", "L-0", 2, 3), ownIn("KGM", "L-0", 1, 4)); got != (measure{3, kg(7), kernel.Quantity{}}) {
		t.Errorf("one unit: %v", got)
	}
	if got := houseOf(t, all, apportionedLine("L-A", 3)); got != (measure{3, kg(3000), kernel.Quantity{}}) {
		t.Errorf("one unit, apportioned: %v", got)
	}
	// Volumes in different units are converted too (V2.84): 2 MTQ and 5 DMQ are 2,000,000 + 5,000 cm3.
	mixed := houseOf(t, all, LoadLine{ShipmentID: "SH-1", LotID: "L-0", EntryNo: 1, Packs: 1, GrossWeight: qty("TNE", 1), Volume: qty("MTQ", 2), Basis: BasisMeasured},
		LoadLine{ShipmentID: "SH-1", LotID: "L-0", EntryNo: 1, Packs: 1, GrossWeight: kg(1), Volume: qty("DMQ", 5), Basis: BasisMeasured})
	if mixed.weight != qty("GRM", 1001000) || mixed.volume != qty("CMQ", 2005000) {
		t.Errorf("mixed weights and mixed volumes: %v, want 1,001,000 GRM and 2,005,000 CMQ", mixed)
	}
}

// TestAHouseInOneUnitIsMeasuredAsBefore: on 3,000 random houses whose lines share one unit (kilograms with lots
// measured and lines apportioned; grams, tonnes, pounds and an unknown unit on lots that were not measured) the packs,
// weight and volume are those of houseMeasureOfV281, the HouseMeasure that added amounts under the last line's unit, to
// the gram and the unit.
func TestAHouseInOneUnitIsMeasuredAsBefore(t *testing.T) {
	rng := rand.New(rand.NewSource(284))
	bare := bareLot(t)
	for i := 0; i < 3000; i++ {
		unit := []kernel.UnitCode{"KGM", "GRM", "TNE", "LBR", "XYZ"}[rng.Intn(5)]
		l := measuredLot(t, "L-R", int64(rng.Intn(8)+2), int64(rng.Intn(5000)+1), int64(rng.Intn(3000)), "CMQ")
		all := lots(l, bare)
		var lines []LoadLine
		for n := rng.Intn(4) + 1; n > 0; n-- {
			switch {
			case unit == "KGM" && rng.Intn(2) == 0:
				lines = append(lines, apportionedLine("L-R", int64(rng.Intn(3)+1)))
			case unit == "KGM" && rng.Intn(2) == 0:
				lines = append(lines, ownIn(unit, "L-R", int64(rng.Intn(2)+1), int64(rng.Intn(900)+1)))
			default:
				lines = append(lines, ownIn(unit, "L-0", int64(rng.Intn(3)+1), int64(rng.Intn(9000)+1)))
			}
		}
		x := stuffedWith(t, "X-A", 1, lines)
		wp, ww, wv := houseMeasureOfV281(x, "SH-1", all)
		if gp, gw, gv := x.HouseMeasure("SH-1", all); (measure{gp, gw, gv}) != (measure{wp, ww, wv}) {
			t.Fatalf("case %d (%s, %v): %v, before %v", i, unit, lines, measure{gp, gw, gv}, measure{wp, ww, wv})
		}
	}
}
