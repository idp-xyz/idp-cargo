package domain

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// TestVolumesInDifferentUnitsAreAddedInCubicCentimetres, by hand, in every order of the quantities: 3 DMQ, 1 MTQ and 7
// CMQ are 3,000 + 1,000,000 + 7 = 1,003,007 cm3; 2 LTR are 2,000 cm3 and 1 FTQ is 28,316.846592 cm3 rounded up to
// 28,317, so 2 LTR and 1 FTQ are 30,317 CMQ; one unit is added as it was (3 + 4 DMQ are 7 DMQ).
func TestVolumesInDifferentUnitsAreAddedInCubicCentimetres(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []kernel.Quantity
		want kernel.Quantity
	}{
		"three units":      {[]kernel.Quantity{qty("DMQ", 3), qty("MTQ", 1), qty("CMQ", 7)}, qty("CMQ", 1003007)},
		"litres and feet":  {[]kernel.Quantity{qty("LTR", 2), qty("FTQ", 1)}, qty("CMQ", 30317)},
		"one unit":         {[]kernel.Quantity{qty("DMQ", 3), qty("DMQ", 4)}, qty("DMQ", 7)},
		"one unit, metres": {[]kernel.Quantity{qty("MTQ", 3), qty("MTQ", 4)}, qty("MTQ", 7)},
		"nothing":          {nil, kernel.Quantity{}},
		"an unknown unit":  {[]kernel.Quantity{qty("XYZ", 5), qty("CMQ", 3)}, qty("XYZ", 5)},
	} {
		t.Run(name, func(t *testing.T) {
			permutations(append([]kernel.Quantity(nil), tc.in...), func(order []kernel.Quantity) {
				var s VolumeSum
				for _, q := range order {
					s.Add(q)
				}
				if got := s.Quantity(); got != tc.want {
					t.Errorf("%v added = %v, want %v", order, got, tc.want)
				}
			})
		})
	}
}

// TestAQuantityPHYCannotReadIsNotDroppedFromASum: a quantity without a unit but with an amount, a negative amount, and
// an amount too big to convert are not lost in a mixed sum. By hand: 5 of no unit with 1 KGM is 5 of no unit, which
// FIN's conversion refuses (the unit that sorts first is the empty one); -5 KGM with 1 TNE is 1 KGM (a negative amount
// cannot be converted, so it is reported in its unit, at least 1); 9,000,000,000,000 TNE is 9 x 10^18 g and
// 300,000,000,000 TNE more are 3 x 10^17 g, over the largest int64 of 9.22 x 10^18 g, so the grams stop at the largest
// int64; two unknown amounts of the largest int64 do not wrap; all zeroes in two units are a zero in the base unit.
func TestAQuantityPHYCannotReadIsNotDroppedFromASum(t *testing.T) {
	for name, tc := range map[string]struct {
		in     []kernel.Quantity
		weight kernel.Quantity
	}{
		"no unit, with an amount":     {[]kernel.Quantity{qty("", 5), qty("KGM", 1)}, qty("", 5)},
		"no unit, three quantities":   {[]kernel.Quantity{qty("", 5), qty("KGM", 1), qty("TNE", 1)}, qty("", 5)},
		"a negative amount":           {[]kernel.Quantity{qty("KGM", -5), qty("TNE", 1)}, qty("KGM", 1)},
		"grams are capped":            {[]kernel.Quantity{qty("TNE", 9000000000000), qty("TNE", 300000000000), qty("KGM", 1)}, qty("GRM", math.MaxInt64)},
		"an unknown amount is capped": {[]kernel.Quantity{qty("XYZ", math.MaxInt64), qty("XYZ", 5), qty("KGM", 1)}, qty("XYZ", math.MaxInt64)},
		"zeroes in two units":         {[]kernel.Quantity{qty("KGM", 0), qty("TNE", 0)}, qty("GRM", 0)},
	} {
		t.Run(name, func(t *testing.T) {
			permutations(append([]kernel.Quantity(nil), tc.in...), func(order []kernel.Quantity) {
				if got := summed(order); got != tc.weight {
					t.Errorf("%v added = %v, want %v", order, got, tc.weight)
				}
			})
		})
	}
	var v VolumeSum
	v.Add(qty("", 5))
	v.Add(qty("MTQ", 1))
	if got := v.Quantity(); got != qty("", 5) {
		t.Errorf("a volume of no unit with 1 MTQ = %v, want 5 of no unit", got)
	}
}

func volumeLot(t *testing.T, packs, weight, volume int64, unit kernel.UnitCode) map[CargoLotID]*CargoLot {
	t.Helper()
	return lots(measuredLot(t, "L-V", packs, weight, volume, unit))
}

// TestAHouseMeasureConvertsVolumesOfDifferentUnits is the reviewer's case of V2.84 and its neighbours, by hand. A lot
// L-V of 6 packs weighed at 300 kg and measured at 600 cm3, with 2 packs measured for themselves at 100 kg and 1 MTQ
// and the other 4 apportioned. The weight: 300 - 100 = 200 kg for the 4. The volume: 1 MTQ is 1,000,000 cm3, more than
// the lot's 600 cm3, so the lines measured for themselves are believed and nothing is left for the 4 (V2.83 took 1 off
// 600 and read 599 in the allotment, 1,000,599 cm3 in the check, and no volume for the house): the house reads
// 1,000,000 + 0 = 1,000,000 CMQ, the container's check 300,000 g and 1,000,000 cm3. With a lot of 2,000,000 cm3 the 4
// take 2,000,000 - 1,000,000 = 1,000,000 and the house reads the lot's 2,000,000 CMQ, as the check does.
func TestAHouseMeasureConvertsVolumesOfDifferentUnits(t *testing.T) {
	for name, tc := range map[string]struct {
		lotVolume   int64
		house, cm3s int64
	}{"the lot's measure is smaller": {600, 1000000, 1000000}, "the lot's measure is greater": {2000000, 2000000, 2000000}} {
		t.Run(name, func(t *testing.T) {
			l := volumeLot(t, 6, 300, tc.lotVolume, "CMQ")
			x := stuffedWith(t, "X-A", 1, []LoadLine{ownLine("L-V", 2, 100, qty("MTQ", 1)), apportionedLine("L-V", 4)})
			got := measureAllotted(x, []*LoadExecution{x}, l)
			if got != (measure{6, kg(300), qty("CMQ", tc.house)}) {
				t.Errorf("house = %v, want 6 packs, 300 KGM and %d CMQ", got, tc.house)
			}
			if one := (measure{}); true {
				p, w, v := x.HouseMeasure("SH-1", l)
				one = measure{p, w, v}
				if one != got {
					t.Errorf("HouseMeasure %v, allotted %v", one, got)
				}
			}
			grams, cubic, unchecked := x.unitCargo(l, AllotLots([]*LoadExecution{x}, l))
			if grams != 300000 || cubic != tc.cm3s || unchecked != "" {
				t.Errorf("the check reads %d g and %d cm3 (%q), want 300,000 g and %d cm3", grams, cubic, unchecked, tc.cm3s)
			}
		})
	}
}

// TestAnAllotmentConvertsBeforeItSubtracts: a lot of 6 packs, 600 kg and 2,000,000 cm3; container A holds 2 packs
// measured for themselves at 300 kg and 1 MTQ, containers B and C 2 apportioned packs each. The 4 packs take the lot's
// 600 - 300 = 300 kg and 2,000,000 - 1,000,000 = 1,000,000 cm3, 150 kg and 500,000 cm3 each. With A's line in grams and
// decimetres (300,000 g and 1,000 DMQ) the same is left, the weight read in grams (150,000 GRM) since a line differs
// from the lot's unit.
func TestAnAllotmentConvertsBeforeItSubtracts(t *testing.T) {
	l := volumeLot(t, 6, 600, 2000000, "CMQ")
	for name, tc := range map[string]struct {
		own    LoadLine
		weight kernel.Quantity
	}{
		"kilograms and cubic metres": {ownLine("L-V", 2, 300, qty("MTQ", 1)), kg(150)},
		"grams and decimetres": {LoadLine{ShipmentID: "SH-1", LotID: "L-V", EntryNo: 1, Packs: 2, GrossWeight: qty("GRM", 300000), Volume: qty("DMQ", 1000),
			Basis: BasisMeasured}, qty("GRM", 150000)},
	} {
		own := tc.own
		t.Run(name, func(t *testing.T) {
			a := stuffedWith(t, "X-A", 1, []LoadLine{own})
			b := stuffedWith(t, "X-B", 2, []LoadLine{apportionedLine("L-V", 2)})
			c := stuffedWith(t, "X-C", 3, []LoadLine{apportionedLine("L-V", 2)})
			all := []*LoadExecution{a, b, c}
			for _, x := range []*LoadExecution{b, c} {
				if got := measureAllotted(x, all, l); got != (measure{2, tc.weight, qty("CMQ", 500000)}) {
					t.Errorf("%s = %v, want 2 packs, %v and 500,000 CMQ", x.ID(), got, tc.weight)
				}
			}
		})
	}
}

// TestAVolumeInAnUnconvertedUnitIsNeverAVolume: a line weighed for itself with 5 XYZ for a volume beside apportioned
// packs of a lot measured in cubic centimetres. The allotment cannot compare the two and takes the amounts off as
// before; the house's volume is the 5 XYZ, which FIN's CubicCm refuses, and the container's check says the unit is not
// converted.
func TestAVolumeInAnUnconvertedUnitIsNeverAVolume(t *testing.T) {
	l := volumeLot(t, 6, 300, 600, "CMQ")
	x := stuffedWith(t, "X-A", 1, []LoadLine{ownLine("L-V", 2, 100, qty("XYZ", 5)), apportionedLine("L-V", 4)})
	if got := measureAllotted(x, []*LoadExecution{x}, l); got != (measure{6, kg(300), qty("XYZ", 5)}) {
		t.Errorf("house = %v, want 6 packs, 300 KGM and 5 XYZ", got)
	}
	if _, _, unchecked := x.unitCargo(l, AllotLots([]*LoadExecution{x}, l)); unchecked != "XYZ" {
		t.Errorf("the check says %q, want the unit XYZ not converted", unchecked)
	}
}

// TestAVolumeNotKnownStaysNotKnownBesideAKnownOne: a line with no volume beside a line of 1 MTQ makes the house's
// volume not known, zero, as before; so does an apportioned line of a lot measured without a volume.
func TestAVolumeNotKnownStaysNotKnownBesideAKnownOne(t *testing.T) {
	bare := bareLot(t)
	if got := houseOf(t, lots(bare), ownIn("KGM", "L-0", 1, 50), LoadLine{ShipmentID: "SH-1", LotID: "L-0", EntryNo: 1, Packs: 1, GrossWeight: kg(1),
		Volume: qty("MTQ", 1), Basis: BasisMeasured}); got.volume != (kernel.Quantity{}) || got.weight != kg(51) {
		t.Errorf("a line with no volume and a line of 1 MTQ: %v, want 51 KGM and no volume", got)
	}
	noVolume := volumeLot(t, 6, 300, 0, "")
	x := stuffedWith(t, "X-A", 1, []LoadLine{ownLine("L-V", 2, 100, qty("MTQ", 1)), apportionedLine("L-V", 4)})
	if got := measureAllotted(x, []*LoadExecution{x}, noVolume); got.volume != (kernel.Quantity{}) || got.weight != kg(300) {
		t.Errorf("a lot measured without a volume: %v, want 300 KGM and no volume", got)
	}
}

// TestAHouseInOneVolumeUnitIsMeasuredAsBefore: on 3,000 random houses whose volumes share one unit (cubic centimetres,
// decimetres or metres on the lots and the lines) the packs, weight and volume are those of houseMeasureOfV281.
func TestAHouseInOneVolumeUnitIsMeasuredAsBefore(t *testing.T) {
	rng := rand.New(rand.NewSource(2841))
	for i := 0; i < 3000; i++ {
		unit := []kernel.UnitCode{"CMQ", "DMQ", "MTQ", "LTR"}[rng.Intn(4)]
		l := measuredLot(t, "L-R", int64(rng.Intn(8)+2), int64(rng.Intn(5000)+1), int64(rng.Intn(3000)+1), unit)
		all := lots(l)
		var lines []LoadLine
		for n := rng.Intn(4) + 1; n > 0; n-- {
			if rng.Intn(2) == 0 {
				lines = append(lines, apportionedLine("L-R", int64(rng.Intn(3)+1)))
			} else {
				lines = append(lines, ownLine("L-R", int64(rng.Intn(2)+1), int64(rng.Intn(900)+1), qty(unit, int64(rng.Intn(2000)+1))))
			}
		}
		x := stuffedWith(t, "X-A", 1, lines)
		wp, ww, wv := houseMeasureOfV281(x, "SH-1", all)
		if gp, gw, gv := x.HouseMeasure("SH-1", all); (measure{gp, gw, gv}) != (measure{wp, ww, wv}) {
			t.Fatalf("case %d (%s, %v): %v, before %v", i, unit, lines, measure{gp, gw, gv}, measure{wp, ww, wv})
		}
		if gp, gw, gv := x.HouseMeasureAllotted("SH-1", all, AllotLots([]*LoadExecution{x}, all)); (measure{gp, gw, gv}) != (measure{wp, ww, wv}) {
			t.Fatalf("case %d allotted (%s, %v): %v, before %v", i, unit, lines, measure{gp, gw, gv}, measure{wp, ww, wv})
		}
	}
}

// lotInTonnes is a SEA lot of 6 packs of SH-1 measured at 3 TNE, a plan in tonnes.
func lotInTonnes(t *testing.T) map[CargoLotID]*CargoLot {
	t.Helper()
	i := seaIntent(3)
	i.Scope.GrossWeight = qty("TNE", 3)
	i.Parts[0].GrossWeight = qty("TNE", 3)
	lot, err := ReceiveCargo("T", "L-T", cfs, i, cartons(6, ConditionGood, t0.Add(time.Hour)), "ops", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = lot.Measure(Measurement{GrossWeight: qty("TNE", 3), Method: MethodScale, Evidence: ev("SCALE-T", t0.Add(2*time.Hour))}, "ops", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return lots(lot)
}

// TestTheLimitCheckReadsAnAllottedPartInTheUnitItIsGivenIn: the allotment gives a part in grams or cubic centimetres
// where a line was in another unit than its lot's, and the check must read it in that unit, not in the lot's (a lot in
// tonnes or cubic metres would make it a million times too big).
//   - Volume: a lot of 6 packs measured at 2 MTQ (2,000,000 cm3); container A holds 2 packs measured for themselves at
//     100 KGM and 500,001 CMQ, container B the other 4 apportioned. The 4 take 2,000,000 - 500,001 = 1,499,999 cm3.
//     The check reads 500,001 cm3 for A and 1,499,999 cm3 for B, which add up to the lot's 2,000,000 cm3, and B's is
//     the house's published volume (1,499,999 CMQ) converted.
//   - Weight: a lot of 6 packs measured at 3 TNE (3,000,000 g); A holds 2 packs measured at 1,000 KGM (1,000,000 g), B
//     the 4 apportioned, which take 3,000,000 - 1,000,000 = 2,000,000 g. The check reads 1,000,000 g and 2,000,000 g,
//     and B's published weight is 2,000,000 GRM.
func TestTheLimitCheckReadsAnAllottedPartInTheUnitItIsGivenIn(t *testing.T) {
	c := twoContainers(t)
	t.Run("volume", func(t *testing.T) {
		l := lots(measuredLot(t, "L-V", 6, 300, 2, "MTQ"))
		a := stuffedWith(t, "X-A", 1, []LoadLine{ownLine("L-V", 2, 100, qty("CMQ", 500001))})
		b := stuffedWith(t, "X-B", 2, []LoadLine{apportionedLine("L-V", 4)})
		all := []*LoadExecution{a, b}
		allot := AllotLots(all, l)
		for x, want := range map[*LoadExecution]int64{a: 500001, b: 1499999} {
			got, ok := x.CheckLimits(c, l, allot)
			if !ok || !got.Checked || got.CargoCubicCm != want {
				t.Errorf("%s: the check reads %+v, want %d cm3", x.ID(), got, want)
			}
			published, _ := toCubicCm(measureAllotted(x, all, l).volume)
			if published != want {
				t.Errorf("%s: the published volume is %d cm3, want %d", x.ID(), published, want)
			}
		}
		if got := measureAllotted(b, all, l).volume; got != qty("CMQ", 1499999) {
			t.Errorf("B's published volume %v, want 1,499,999 CMQ", got)
		}
	})
	t.Run("weight", func(t *testing.T) {
		l := lotInTonnes(t)
		a := stuffedWith(t, "X-A", 1, []LoadLine{ownIn("KGM", "L-T", 2, 1000)})
		b := stuffedWith(t, "X-B", 2, []LoadLine{apportionedLine("L-T", 4)})
		all := []*LoadExecution{a, b}
		allot := AllotLots(all, l)
		for x, want := range map[*LoadExecution]int64{a: 1000000, b: 2000000} {
			got, ok := x.CheckLimits(c, l, allot)
			if !ok || !got.Checked || got.CargoGrams != want {
				t.Errorf("%s: the check reads %+v, want %d g", x.ID(), got, want)
			}
		}
		if got := measureAllotted(b, all, l).weight; got != qty("GRM", 2000000) {
			t.Errorf("B's published weight %v, want 2,000,000 GRM", got)
		}
	})
}
