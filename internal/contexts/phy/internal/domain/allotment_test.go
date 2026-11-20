package domain

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// The executions of these tests are stuffings restored with the lines they hold; every line is of house SH-1.

// measuredLot returns a SEA lot of SH-1 with packs received and a measurement of weight kg and volume cubic
// centimetres (none when volume is zero); unit is the unit of the volume.
func measuredLot(t *testing.T, id CargoLotID, packs, weight, volume int64, unit kernel.UnitCode) *CargoLot {
	t.Helper()
	lot, err := ReceiveCargo("T", id, cfs, seaIntent(3000), cartons(packs, ConditionGood, t0.Add(time.Hour)), "ops", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	m := scale(weight, t0.Add(2*time.Hour))
	if volume > 0 {
		m.Volume = kernel.Quantity{Unit: unit, Amount: volume}
	}
	if err = lot.Measure(m, "ops", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return lot
}

func lots(l ...*CargoLot) map[CargoLotID]*CargoLot {
	out := map[CargoLotID]*CargoLot{}
	for _, x := range l {
		out[x.ID()] = x
	}
	return out
}

// apportionedLine is a line apportioned from the lot's first measurement; its own figures are never read.
func apportionedLine(lot CargoLotID, packs int64) LoadLine {
	return LoadLine{ShipmentID: "SH-1", LotID: lot, EntryNo: 1, Packs: packs, GrossWeight: kg(1), Basis: BasisApportioned, MeasurementNo: 1}
}

// ownLine is a line measured for itself, with a volume when volume is given.
func ownLine(lot CargoLotID, packs, weight int64, volume kernel.Quantity) LoadLine {
	return LoadLine{ShipmentID: "SH-1", LotID: lot, EntryNo: 1, Packs: packs, GrossWeight: kg(weight), Volume: volume, Basis: BasisMeasured}
}

// stuffedWith restores a loading stuffing of the unit (a tender when the unit is 0) holding the lines, and the
// reversals if any.
func stuffedWith(t *testing.T, id LoadExecutionID, unit int, lines []LoadLine, reversals ...Reversal) *LoadExecution {
	t.Helper()
	for i := range lines {
		lines[i].No, lines[i].By, lines[i].At = i+1, "ops", t0.Add(3*time.Hour)
	}
	for i := range reversals {
		reversals[i].No, reversals[i].By, reversals[i].At = i+1, "ops", t0.Add(4*time.Hour)
	}
	kind := ExecutionStuffing
	if unit == 0 {
		kind = ExecutionTender // a tender names no unit
	}
	x, err := RestoreLoadExecution(LoadExecutionState{TenantID: "T", ExecutionID: id, ConsolidationID: "C-S", Kind: kind, Mode: ModeSea,
		Facility: cfs, LoadUnitNo: unit, State: ExecutionLoading, OpenedBy: "ops", OpenedAt: t0.Add(3 * time.Hour), StartedBy: "ops",
		StartedAt: t0.Add(3 * time.Hour), Lines: lines, Reversals: reversals})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

type measure struct {
	packs  int64
	weight kernel.Quantity
	volume kernel.Quantity
}

func (m measure) String() string { return fmt.Sprintf("%d packs, %v, %v", m.packs, m.weight, m.volume) }

// measureAllotted is SH-1's measure in x as the cost basis and the shipment's physical facts read it: over all xs.
func measureAllotted(x *LoadExecution, all []*LoadExecution, l map[CargoLotID]*CargoLot) measure {
	packs, w, v := x.HouseMeasureAllotted("SH-1", l, AllotLots(all, l))
	return measure{packs, w, v}
}

func cm3(n int64) kernel.Quantity { return kernel.Quantity{Unit: "CMQ", Amount: n} }

// TestALotsMeasureOverTwoContainersAddsUp is the review of V2.79's two cases. A 600 kg lot of 6 packs: 2 packs weighed
// for themselves at 300 kg in container 1, the other 4 apportioned in container 2. V2.81 read container 2 at
// 600 x 4/6 = 400 kg, the two at 700 kg of a 600 kg lot. The lot's measure less what the weighed packs took is
// 300 kg, which the 4 remaining packs, completing the lot, take whole: 300 + 300. An 800 kg lot of 6 packs loaded 4
// and 2, all apportioned: V2.81 read 800 x 4/6 = 533 and 800 x 2/6 = 266, 799 kg of 800; the parts are 533.33 and
// 266.67 kg, the floors 533 + 266 leave 1 kg, which goes to the larger remainder (2/3 against 1/3): 533 + 267.
func TestALotsMeasureOverTwoContainersAddsUp(t *testing.T) {
	l600 := measuredLot(t, "L-600", 6, 600, 0, "")
	a := stuffedWith(t, "X-A", 1, []LoadLine{ownLine("L-600", 2, 300, kernel.Quantity{})})
	b := stuffedWith(t, "X-B", 2, []LoadLine{apportionedLine("L-600", 4)})
	all := []*LoadExecution{a, b}
	if got := measureAllotted(a, all, lots(l600)); got.packs != 2 || got.weight != kg(300) {
		t.Errorf("container 1 = %v, want 2 packs, 300 kg", got)
	}
	got := measureAllotted(b, all, lots(l600))
	if got.packs != 4 || got.weight != kg(300) {
		t.Errorf("container 2 = %v, want 4 packs, 300 kg", got)
	}
	if _, w, _ := b.HouseMeasure("SH-1", lots(l600)); w != kg(400) {
		t.Errorf("container 2 alone reads %v; the defect was 400 kg", w)
	}

	l800 := measuredLot(t, "L-800", 6, 800, 0, "")
	c := stuffedWith(t, "X-C", 1, []LoadLine{apportionedLine("L-800", 4)})
	d := stuffedWith(t, "X-D", 2, []LoadLine{apportionedLine("L-800", 2)})
	all = []*LoadExecution{c, d}
	if got := measureAllotted(c, all, lots(l800)); got.weight != kg(533) {
		t.Errorf("container 1 = %v, want 533 kg", got)
	}
	if got := measureAllotted(d, all, lots(l800)); got.weight != kg(267) {
		t.Errorf("container 2 = %v, want 267 kg", got)
	}
	if _, w, _ := d.HouseMeasure("SH-1", lots(l800)); w != kg(266) {
		t.Errorf("container 2 alone reads %v; the defect was 266 kg", w)
	}
}

// TestAllotmentTiesAndRemainders: the parts add up to the amount exactly, the leftover units go to the largest
// remainders, and equal remainders go to the lower load unit number and then the lower execution id, whatever the
// order the executions are given in.
func TestAllotmentTiesAndRemainders(t *testing.T) {
	// 10 kg over 1, 2 and 4 of 7 packs: 10/7 = 1.43, 20/7 = 2.86, 40/7 = 5.71; floors 1 + 2 + 5 = 8, two left for the
	// remainders 3/7, 6/7 and 5/7: the second and third take them: 1, 3, 6.
	l := measuredLot(t, "L-1", 7, 10, 0, "")
	x1 := stuffedWith(t, "X-1", 1, []LoadLine{apportionedLine("L-1", 1)})
	x2 := stuffedWith(t, "X-2", 2, []LoadLine{apportionedLine("L-1", 2)})
	x3 := stuffedWith(t, "X-3", 3, []LoadLine{apportionedLine("L-1", 4)})
	for name, order := range map[string][]*LoadExecution{"in order": {x1, x2, x3}, "reversed": {x3, x2, x1}, "mixed": {x2, x3, x1}} {
		want := map[LoadExecutionID]int64{"X-1": 1, "X-2": 3, "X-3": 6}
		for _, x := range order {
			if got := measureAllotted(x, order, lots(l)); got.weight.Amount != want[x.ID()] {
				t.Errorf("%s: %s = %v, want %d kg", name, x.ID(), got, want[x.ID()])
			}
		}
	}
	// 100 kg over three single packs of three: 33.33 each, one kg left, equal remainders: the lowest unit number takes
	// it. Given 101 kg, two are left and the two lowest unit numbers take them.
	for _, tc := range []struct {
		weight int64
		want   [3]int64 // units 1, 2, 3
	}{{100, [3]int64{34, 33, 33}}, {101, [3]int64{34, 34, 33}}, {99, [3]int64{33, 33, 33}}} {
		l := measuredLot(t, "L-3", 3, tc.weight, 0, "")
		u3 := stuffedWith(t, "X-U3", 3, []LoadLine{apportionedLine("L-3", 1)})
		u1 := stuffedWith(t, "X-U1", 1, []LoadLine{apportionedLine("L-3", 1)})
		u2 := stuffedWith(t, "X-U2", 2, []LoadLine{apportionedLine("L-3", 1)})
		all := []*LoadExecution{u3, u1, u2}
		for i, x := range []*LoadExecution{u1, u2, u3} {
			if got := measureAllotted(x, all, lots(l)); got.weight.Amount != tc.want[i] {
				t.Errorf("%d kg: unit %d = %v, want %d kg", tc.weight, i+1, got, tc.want[i])
			}
		}
	}
	// Executions of one load unit number (tenders have none) come after each other by execution id.
	l = measuredLot(t, "L-4", 3, 100, 0, "")
	b := stuffedWith(t, "X-B", 0, []LoadLine{apportionedLine("L-4", 1)})
	a := stuffedWith(t, "X-A", 0, []LoadLine{apportionedLine("L-4", 1)})
	c := stuffedWith(t, "X-C", 0, []LoadLine{apportionedLine("L-4", 1)})
	all := []*LoadExecution{c, b, a}
	for _, tc := range []struct {
		x    *LoadExecution
		want int64
	}{{a, 34}, {b, 33}, {c, 33}} {
		if got := measureAllotted(tc.x, all, lots(l)); got.weight.Amount != tc.want {
			t.Errorf("%s = %v, want %d kg", tc.x.ID(), got, tc.want)
		}
	}
}

// TestAPartlyLoadedLotAllotsItsShare: 5 of a 7-pack lot of 1,000 kg are loaded apportioned in two containers, 2 and 3
// (one pack of the first was unloaded again: 3 loaded, 2 live). The 5 live packs take 1,000 x 5/7 = 714 kg (rounded
// down), of which the 2 take 714 x 2/5 = 285.6 and the 3 take 428.4: floors 285 + 428 leave 1 kg for the larger
// remainder (0.6 against 0.4, the first): 286 + 428.
func TestAPartlyLoadedLotAllotsItsShare(t *testing.T) {
	l := measuredLot(t, "L-1", 7, 1000, 7000, "CMQ")
	a := stuffedWith(t, "X-A", 1, []LoadLine{apportionedLine("L-1", 3)}, Reversal{LineNo: 1, Packs: 1, GrossWeight: kg(0), Reason: ReversalUnstuffed})
	b := stuffedWith(t, "X-B", 2, []LoadLine{apportionedLine("L-1", 3)})
	all := []*LoadExecution{a, b}
	ga, gb := measureAllotted(a, all, lots(l)), measureAllotted(b, all, lots(l))
	if ga.packs != 2 || gb.packs != 3 || ga.weight != kg(286) || gb.weight != kg(428) {
		t.Errorf("parts = %v and %v, want 2 packs 286 kg and 3 packs 428 kg", ga, gb)
	}
	// the volume is divided by the same rule: 7,000 x 5/7 = 5,000 cm3, 2,000 and 3,000
	if ga.volume != cm3(2000) || gb.volume != cm3(3000) {
		t.Errorf("volumes = %v and %v, want 2,000 and 3,000 CMQ", ga.volume, gb.volume)
	}
	if total := ga.weight.Amount + gb.weight.Amount; total != 714 {
		t.Errorf("the parts add up to %d kg, want 714", total)
	}
}

// TestLinesMeasuredForThemselvesInTwoContainers: a 10-pack lot of 1,000 kg with 2 packs weighed at 150 kg and 1 at
// 50 kg in two containers, the other 7 apportioned 3 and 4. The weighed packs took 200 kg, leaving 800 kg for 7
// packs, which complete the lot: 3 take 800 x 3/7 = 342.9, 4 take 457.1; floors 342 + 457 leave 1 kg for the larger
// remainder (6/7 against 1/7): 343 + 457. The containers read 150 + 343 = 493 and 50 + 457 = 507: 1,000 kg.
func TestLinesMeasuredForThemselvesInTwoContainers(t *testing.T) {
	l := measuredLot(t, "L-1", 10, 1000, 0, "")
	a := stuffedWith(t, "X-A", 1, []LoadLine{ownLine("L-1", 2, 150, kernel.Quantity{}), apportionedLine("L-1", 3)})
	b := stuffedWith(t, "X-B", 2, []LoadLine{ownLine("L-1", 1, 50, kernel.Quantity{}), apportionedLine("L-1", 4)})
	all := []*LoadExecution{a, b}
	ga, gb := measureAllotted(a, all, lots(l)), measureAllotted(b, all, lots(l))
	if ga.packs != 5 || gb.packs != 5 || ga.weight != kg(493) || gb.weight != kg(507) {
		t.Errorf("containers = %v and %v, want 5 packs 493 kg and 5 packs 507 kg", ga, gb)
	}
}

// TestMeasuredPacksAreBelieved: when the lines measured for themselves exceed the lot's measure nothing remains for
// the apportioned packs, and the containers add up to more than the lot, as for one container.
func TestMeasuredPacksAreBelieved(t *testing.T) {
	l := measuredLot(t, "L-1", 4, 100, 0, "")
	a := stuffedWith(t, "X-A", 1, []LoadLine{ownLine("L-1", 2, 80, kernel.Quantity{})})
	b := stuffedWith(t, "X-B", 2, []LoadLine{ownLine("L-1", 1, 50, kernel.Quantity{}), apportionedLine("L-1", 1)})
	all := []*LoadExecution{a, b}
	ga, gb := measureAllotted(a, all, lots(l)), measureAllotted(b, all, lots(l))
	if ga.weight != kg(80) || gb.weight != kg(50) || gb.packs != 2 {
		t.Errorf("containers = %v and %v, want 80 kg and 2 packs 50 kg: 130 kg of a 100 kg lot", ga, gb)
	}
}

// TestVolumeUnknownInOneContainerOnly: a volume not known, or in mixed units, reads zero for the container it is in,
// not for the others; the lot's volume is still divided over all.
func TestVolumeUnknownInOneContainerOnly(t *testing.T) {
	// 6 packs, 600 kg and 600 cm3: container A has 2 packs weighed at 300 kg without a volume (unknown), and 2
	// apportioned; container B 2 apportioned. 300 kg and 600 cm3 remain for 4 packs: 150 kg and 300 cm3 each part.
	l := measuredLot(t, "L-1", 6, 600, 600, "CMQ")
	a := stuffedWith(t, "X-A", 1, []LoadLine{ownLine("L-1", 2, 300, kernel.Quantity{}), apportionedLine("L-1", 2)})
	b := stuffedWith(t, "X-B", 2, []LoadLine{apportionedLine("L-1", 2)})
	all := []*LoadExecution{a, b}
	if ga := measureAllotted(a, all, lots(l)); ga.packs != 4 || ga.weight != kg(450) || ga.volume != (kernel.Quantity{}) {
		t.Errorf("container A = %v, want 4 packs 450 kg and no volume", ga)
	}
	if gb := measureAllotted(b, all, lots(l)); gb.packs != 2 || gb.weight != kg(150) || gb.volume != cm3(300) {
		t.Errorf("container B = %v, want 2 packs 150 kg 300 CMQ", gb)
	}
	// mixed units: a line weighed for itself with its volume in cubic metres beside the lot's cubic centimetres. Since
	// V2.84 the volumes are converted and added: the line's 1 MTQ is 1,000,000 cm3, more than the lot's 600 cm3, so
	// nothing of the lot's volume is left for its other packs, and container A reads 1,000,000 + 0 cm3 (it read no
	// volume, and container B 300 cm3 less 1, before).
	m := stuffedWith(t, "X-M", 1, []LoadLine{ownLine("L-1", 2, 300, kernel.Quantity{Unit: "MTQ", Amount: 1}), apportionedLine("L-1", 2)})
	all = []*LoadExecution{m, b}
	if gm := measureAllotted(m, all, lots(l)); gm.weight != kg(450) || gm.volume != cm3(1000000) {
		t.Errorf("mixed container = %v, want 450 kg and 1,000,000 CMQ", gm)
	}
	if gb := measureAllotted(b, all, lots(l)); gb.volume != cm3(0) {
		t.Errorf("container B = %v beside a mixed one, want a volume of 0 CMQ: the line's 1 MTQ takes the lot's 600 cm3", gb)
	}
}

// TestALotWithNoMeasurementHasNothingToAllot: lines of a lot never measured (their own figures, measured for themselves
// only) and of a lot the map does not know count as their lines say, as HouseMeasure counts them.
func TestALotWithNoMeasurementHasNothingToAllot(t *testing.T) {
	bare, err := ReceiveCargo("T", "L-0", cfs, seaIntent(3000), cartons(4, ConditionGood, t0.Add(time.Hour)), "ops", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	x := stuffedWith(t, "X-A", 1, []LoadLine{ownLine("L-0", 4, 120, cm3(10))})
	if got := measureAllotted(x, []*LoadExecution{x}, lots(bare)); got.packs != 4 || got.weight != kg(120) || got.volume != cm3(10) {
		t.Errorf("unmeasured lot = %v", got)
	}
}

// TestAllotmentOfOneExecutionIsTodaysMeasure: with one execution holding a lot's packs the figures are those of
// V2.81 to the gram, on a table of cases and on random ones. houseMeasureOfV281 is a copy of that function.
func TestAllotmentOfOneExecutionIsTodaysMeasure(t *testing.T) {
	check := func(t *testing.T, name string, x *LoadExecution, l map[CargoLotID]*CargoLot) {
		t.Helper()
		wp, ww, wv := houseMeasureOfV281(x, "SH-1", l)
		if got := measureAllotted(x, []*LoadExecution{x}, l); got != (measure{wp, ww, wv}) {
			t.Errorf("%s: allotted %v, V2.81 %v", name, got, measure{wp, ww, wv})
		}
		if gp, gw, gv := x.HouseMeasure("SH-1", l); (measure{gp, gw, gv}) != (measure{wp, ww, wv}) {
			t.Errorf("%s: HouseMeasure %v, V2.81 %v", name, measure{gp, gw, gv}, measure{wp, ww, wv})
		}
	}
	l10 := measuredLot(t, "L-10", 10, 1000, 5000, "CMQ")
	l7 := measuredLot(t, "L-7", 7, 100, 0, "")
	bare, _ := ReceiveCargo("T", "L-0", cfs, seaIntent(3000), cartons(4, ConditionGood, t0.Add(time.Hour)), "ops", t0.Add(time.Hour))
	all := lots(l10, l7, bare)
	for name, lines := range map[string][]LoadLine{
		"apportioned, partly":        {apportionedLine("L-10", 3)},
		"apportioned, completing":    {apportionedLine("L-10", 10)},
		"measured only":              {ownLine("L-10", 4, 400, cm3(100))},
		"measured and apportioned":   {ownLine("L-10", 4, 400, cm3(100)), apportionedLine("L-10", 6), apportionedLine("L-7", 2)},
		"measured over the measure":  {ownLine("L-7", 3, 500, cm3(1)), apportionedLine("L-7", 2)},
		"two lots, one unknown":      {apportionedLine("L-10", 5), apportionedLine("L-7", 7), ownLine("L-0", 2, 9, kernel.Quantity{})},
		"volume missing and present": {ownLine("L-10", 1, 50, kernel.Quantity{}), apportionedLine("L-10", 5)},
		// "mixed volume units" was a case here until V2.84, which converts the volumes
		// (TestAHouseMeasureConvertsVolumesOfDifferentUnits)
	} {
		check(t, name, stuffedWith(t, "X-A", 1, lines), all)
	}
	check(t, "reversed", stuffedWith(t, "X-A", 1, []LoadLine{apportionedLine("L-10", 6), ownLine("L-10", 2, 150, cm3(20))},
		Reversal{LineNo: 1, Packs: 2, GrossWeight: kg(1), Reason: ReversalMisloaded}, Reversal{LineNo: 2, Packs: 2, GrossWeight: kg(150), Volume: cm3(20), Reason: ReversalUnstuffed}), all)
	rng := rand.New(rand.NewSource(282))
	for i := 0; i < 2000; i++ {
		packs, weight := int64(rng.Intn(10)+1), int64(rng.Intn(5000)+1)
		l := measuredLot(t, "L-R", packs, weight, int64(rng.Intn(3000)), "CMQ")
		var lines []LoadLine
		for left := packs; left > 0 && rng.Intn(6) > 0; {
			n := int64(rng.Intn(int(left)) + 1)
			if rng.Intn(3) == 0 {
				lines = append(lines, ownLine("L-R", n, int64(rng.Intn(2000)+1), cm3(int64(rng.Intn(500)+1))))
			} else {
				lines = append(lines, apportionedLine("L-R", n))
			}
			left -= n
		}
		if len(lines) > 0 {
			check(t, fmt.Sprint("random ", i), stuffedWith(t, "X-A", 1, lines), lots(l))
		}
	}
}

// TestRandomSplitsAddUp: a lot's packs loaded over up to four containers in any order, some weighed for themselves
// within the lot's measure, the rest apportioned. The containers add up to what the lot's measure gives the packs
// loaded: the weighed lines, and of what remains of the measure the share of the apportioned packs in the packs not
// weighed (all of it when they complete the lot), exactly, in weight and volume. So when every pack is loaded and
// some are apportioned, the containers add up to the lot's measure.
func TestRandomSplitsAddUp(t *testing.T) {
	rng := rand.New(rand.NewSource(2821))
	for i := 0; i < 3000; i++ {
		packs, weight, volume := int64(rng.Intn(10)+1), int64(rng.Intn(9000)+1), int64(rng.Intn(9000)+1)
		l := measuredLot(t, "L-R", packs, weight, volume, "CMQ")
		parts := make([][]LoadLine, 1+rng.Intn(4))
		var weighed, weighedVolume, weighedPacks, apportionedPacks int64
		for left := packs; left > 0; {
			n := int64(rng.Intn(int(left)) + 1)
			left -= n
			if left == 0 && rng.Intn(4) == 0 {
				break // the last packs are not loaded
			}
			k := rng.Intn(len(parts))
			if weight-weighed > 1 && volume-weighedVolume > 1 && rng.Intn(3) == 0 {
				w, v := int64(rng.Intn(int(weight-weighed-1)))+1, int64(rng.Intn(int(volume-weighedVolume-1)))+1
				parts[k] = append(parts[k], ownLine("L-R", n, w, cm3(v)))
				weighed, weighedVolume, weighedPacks = weighed+w, weighedVolume+v, weighedPacks+n
				continue
			}
			parts[k] = append(parts[k], apportionedLine("L-R", n))
			apportionedPacks += n
		}
		var all []*LoadExecution
		for k, lines := range parts {
			if len(lines) > 0 {
				all = append(all, stuffedWith(t, LoadExecutionID(fmt.Sprint("X-", k)), k+1, lines))
			}
		}
		if len(all) == 0 {
			continue
		}
		wantW, wantV := weighed, weighedVolume
		if apportionedPacks > 0 {
			rest, restW, restV := packs-weighedPacks, weight-weighed, volume-weighedVolume
			if apportionedPacks < rest {
				restW, restV = share(restW, apportionedPacks, rest), share(restV, apportionedPacks, rest)
			}
			wantW, wantV = wantW+restW, wantV+restV
		}
		var w, v, p int64
		for _, x := range all {
			g := measureAllotted(x, all, lots(l))
			w, v, p = w+g.weight.Amount, v+g.volume.Amount, p+g.packs
		}
		if p != weighedPacks+apportionedPacks || w != wantW || v != wantV {
			t.Fatalf("case %d (%d packs, %d kg, %d cm3; %d weighed packs, %d kg, %d cm3; %d apportioned): containers read %d packs %d kg %d cm3, want %d kg %d cm3",
				i, packs, weight, volume, weighedPacks, weighed, weighedVolume, apportionedPacks, p, w, v, wantW, wantV)
		}
		if weighedPacks+apportionedPacks == packs && apportionedPacks > 0 && (w != weight || v != volume) {
			t.Fatalf("case %d: every pack loaded and the containers read %d kg %d cm3 of %d kg %d cm3", i, w, v, weight, volume)
		}
	}
}

// houseMeasureOfV281 is the HouseMeasure of V2.81, kept as the oracle of the one-execution case.
func houseMeasureOfV281(x *LoadExecution, shipment ShipmentID, lots map[CargoLotID]*CargoLot) (int64, kernel.Quantity, kernel.Quantity) {
	var packs int64
	var weight, volume kernel.Quantity
	unknown := false
	addVolume := func(unit kernel.UnitCode, amount int64) {
		switch {
		case unit == "":
			unknown = true
		case volume.Unit == "" || volume.Unit == unit:
			volume = kernel.Quantity{Unit: unit, Amount: volume.Amount + amount}
		default:
			unknown = true
		}
	}
	type lotShare struct{ apportioned, measuredPacks, measuredWeight, measuredVolume int64 }
	byLot := map[CargoLotID]*lotShare{}
	var order []CargoLotID // lots in the order their lines were loaded, so that the result never depends on map order
	for _, line := range x.state.Lines {
		if line.ShipmentID != shipment {
			continue
		}
		live, w, v := x.lineLive(line.No)
		if live == 0 {
			continue
		}
		packs += live
		lot, ok := lots[line.LotID]
		_, measured := lot.Measured()
		if ok && measured && lot.Received() > 0 {
			if byLot[line.LotID] == nil {
				byLot[line.LotID], order = &lotShare{}, append(order, line.LotID)
			}
			s := byLot[line.LotID]
			if line.Basis == BasisApportioned {
				s.apportioned += live
				continue
			}
			s.measuredPacks, s.measuredWeight, s.measuredVolume = s.measuredPacks+live, s.measuredWeight+w, s.measuredVolume+v
		}
		weight = kernel.Quantity{Unit: line.GrossWeight.Unit, Amount: weight.Amount + w}
		addVolume(line.Volume.Unit, v)
	}
	for _, id := range order {
		s := byLot[id]
		if s.apportioned == 0 {
			continue
		}
		lot := lots[id]
		m, _ := lot.Measured()
		rest := lot.Received() - s.measuredPacks
		w, v := max(m.GrossWeight.Amount-s.measuredWeight, 0), max(m.Volume.Amount-s.measuredVolume, 0)
		if s.apportioned < rest {
			w, v = share(w, s.apportioned, rest), share(v, s.apportioned, rest)
		}
		weight = kernel.Quantity{Unit: m.GrossWeight.Unit, Amount: weight.Amount + w}
		addVolume(m.Volume.Unit, v)
	}
	if unknown {
		volume = kernel.Quantity{}
	}
	return packs, weight, volume
}

// Equal remainders go to the lower load unit number before the lower execution id: here the stuffing of unit 1 has the
// greatest execution id, and 100 kg over three single packs leaves one kg that it takes.
func TestAllotmentTieGoesToTheUnitBeforeTheExecutionId(t *testing.T) {
	l := measuredLot(t, "L-T", 3, 100, 0, "")
	a := stuffedWith(t, "X-A", 3, []LoadLine{apportionedLine("L-T", 1)})
	b := stuffedWith(t, "X-B", 2, []LoadLine{apportionedLine("L-T", 1)})
	c := stuffedWith(t, "X-C", 1, []LoadLine{apportionedLine("L-T", 1)})
	all := []*LoadExecution{a, b, c}
	for _, tc := range []struct {
		x    *LoadExecution
		want int64
	}{{c, 34}, {b, 33}, {a, 33}} {
		if got := measureAllotted(tc.x, all, lots(l)); got.weight.Amount != tc.want {
			t.Errorf("%s of unit %d = %v, want %d kg", tc.x.ID(), tc.x.State().LoadUnitNo, got, tc.want)
		}
	}
}

// An execution passed twice counts once, and a nil element is skipped (no caller does either).
func TestAllotmentCountsAnExecutionOnceAndSkipsNil(t *testing.T) {
	l := measuredLot(t, "L-D", 6, 600, 0, "")
	a := stuffedWith(t, "X-A", 1, []LoadLine{apportionedLine("L-D", 3)})
	b := stuffedWith(t, "X-B", 2, []LoadLine{apportionedLine("L-D", 3)})
	if got := measureAllotted(a, []*LoadExecution{a, a, b}, lots(l)); got.weight != kg(300) {
		t.Errorf("a passed twice = %v, want 300 kg", got)
	}
	if got := measureAllotted(a, []*LoadExecution{nil, a, nil, b}, lots(l)); got.weight != kg(300) {
		t.Errorf("with nil elements = %v, want 300 kg", got)
	}
}
