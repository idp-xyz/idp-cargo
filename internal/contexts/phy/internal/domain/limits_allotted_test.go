package domain

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// twoContainers is an OPEN own-container consolidation C-S with two 22G1 planned (units 1 and 2): a 22G1 takes
// 28,230 kg of cargo (30,480 kg gross less the 2,250 kg tare) and 33.2 m3 (33,200,000 cm3).
func twoContainers(t *testing.T) *Consolidation {
	t.Helper()
	c := seaConsol(t, "C-S", KindSeaLCLOwn)
	for i := 0; i < 2; i++ {
		if _, err := c.PlanLoadUnit(UnitContainer, unit22G1, "ops", t0.Add(30*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// held checks the stuffings of all by the allotted figure and reports which of them the check holds.
func held(c *Consolidation, all []*LoadExecution, l map[CargoLotID]*CargoLot) map[LoadExecutionID]bool {
	a := AllotLots(all, l)
	out := map[LoadExecutionID]bool{}
	for _, x := range all {
		out[x.ID()] = x.HoldIfOverLimitsAllotted(c, l, a, ev("W", t0.Add(5*time.Hour)), "ops", t0.Add(5*time.Hour))
	}
	return out
}

// TestTheLimitCheckReadsMoreThanTheAllottedFigureNoLonger is the first error of before. A lot of 6 packs and 42,900 kg
// with 2 packs weighed for themselves at 21,450 kg in container 1 and the other 4 apportioned in container 2. Alone,
// container 2 read 42,900 x 4/6 = 28,600 kg of cargo, over the 28,230 kg of a 22G1, and was held. The allotted figure
// is the lot less the weighed packs, 42,900 - 21,450 = 21,450 kg, which the 4 packs that complete the lot take whole:
// with the tare 23,700 kg, within the 30,480 kg. Container 1 reads 21,450 kg and is within as well. Capacity the same
// way: 6 packs and 50,000,000 cm3 with 2 packs weighed at 30,000,000 cm3: alone 50,000,000 x 4/6 = 33,333,333 cm3 over
// 33,200,000 cm3; allotted 50,000,000 - 30,000,000 = 20,000,000 cm3.
func TestTheLimitCheckReadsMoreThanTheAllottedFigureNoLonger(t *testing.T) {
	c := twoContainers(t)
	for name, tc := range map[string]struct {
		lot       *CargoLot
		weighed   LoadLine
		oldReadsG int64 // what container 2 alone read, in grams, and the cargo of the allotted figure
		wantG     int64
		wantCm3   int64
	}{
		"weight":   {measuredLot(t, "L-A", 6, 42900, 0, ""), ownLine("L-A", 2, 21450, kernel.Quantity{}), 28600000, 21450000, 0},
		"capacity": {measuredLot(t, "L-A", 6, 3000, 50000000, "CMQ"), ownLine("L-A", 2, 1000, cm3(30000000)), 2000000, 2000000, 20000000},
	} {
		x1 := stuffedWith(t, "X-1", 1, []LoadLine{tc.weighed})
		x2 := stuffedWith(t, "X-2", 2, []LoadLine{apportionedLine("L-A", 4)})
		l := lots(tc.lot)
		old2, oldCm3, _ := unitCargoOfV282(x2, l)
		if name == "weight" && old2 != tc.oldReadsG || name == "capacity" && oldCm3 != 33333333 {
			t.Errorf("%s: container 2 alone read %d g %d cm3", name, old2, oldCm3)
		}
		got, ok := x2.CheckLimits(c, l, AllotLots([]*LoadExecution{x1, x2}, l))
		if !ok || !got.Checked || got.CargoGrams != tc.wantG || got.CargoCubicCm != tc.wantCm3 || got.OverMaxGross || got.OverCapacity {
			t.Errorf("%s: container 2 by the allotted figure: %+v, want %d g %d cm3 within the limits", name, got, tc.wantG, tc.wantCm3)
		}
		if h := held(c, []*LoadExecution{x1, x2}, l); h["X-1"] || h["X-2"] {
			t.Errorf("%s: held %v; the allotted figures are within the limits", name, h)
		}
		if !x2.HoldIfOverLimits(c, l, ev("alone", t0.Add(5*time.Hour)), "ops", t0.Add(5*time.Hour)) {
			t.Errorf("%s: the stuffing alone, the way of before, was held; the examples are meant to show the difference", name)
		}
	}
}

// TestTheLimitCheckReadsLessThanTheAllottedFigureNoLonger is the second error of before. A lot of 6 packs and 50,000 kg
// with 1 pack weighed for itself at 2,000 kg and 2 packs apportioned in container 1, and 3 apportioned in container 2.
// Alone, container 2 read 50,000 x 3/6 = 25,000 kg, within the 28,230 kg, and was not held. The 5 apportioned packs
// complete the lot and take what the weighed pack leaves, 50,000 - 2,000 = 48,000 kg, container 2's 3 of them
// 48,000 x 3/5 = 28,800 kg: with the tare 31,050 kg over the 30,480 kg. Container 1 reads 2,000 + 48,000 x 2/5 = 21,200
// kg and is within. Capacity the same way: 6 packs and 60,000,000 cm3 with 1 pack weighed at 1,000,000 cm3: alone
// 60,000,000 x 3/6 = 30,000,000 cm3 within 33,200,000; allotted (60,000,000 - 1,000,000) x 3/5 = 35,400,000 cm3.
func TestTheLimitCheckReadsLessThanTheAllottedFigureNoLonger(t *testing.T) {
	c := twoContainers(t)
	for name, tc := range map[string]struct {
		lot  *CargoLot
		own  LoadLine
		text string
		g    int64
		cm3  int64
	}{
		"weight": {measuredLot(t, "L-B", 6, 50000, 0, ""), ownLine("L-B", 1, 2000, kernel.Quantity{}),
			"unit 2 (22G1): tare and cargo 31050.000 kg over the default maximum gross 30480 kg (edition EQUIPMENT-2026-11-S1)", 28800000, 0},
		"capacity": {measuredLot(t, "L-B", 6, 6000, 60000000, "CMQ"), ownLine("L-B", 1, 1000, cm3(1000000)),
			"unit 2 (22G1): cargo 35.400000 m3 over the default capacity 33.200000 m3 (edition EQUIPMENT-2026-11-S1)", 4800000, 35400000},
	} {
		x1 := stuffedWith(t, "X-1", 1, []LoadLine{tc.own, apportionedLine("L-B", 2)})
		x2 := stuffedWith(t, "X-2", 2, []LoadLine{apportionedLine("L-B", 3)})
		l := lots(tc.lot)
		if alone := x2.HoldIfOverLimits(c, l, ev("alone", t0.Add(5*time.Hour)), "ops", t0.Add(5*time.Hour)); alone {
			t.Errorf("%s: the stuffing alone, the way of before, was held; the examples are meant to show the difference", name)
		}
		x2 = stuffedWith(t, "X-2", 2, []LoadLine{apportionedLine("L-B", 3)})
		h := held(c, []*LoadExecution{x1, x2}, l)
		holds := x2.State().Holds
		if h["X-1"] || !h["X-2"] || len(holds) != 1 || holds[0].Reason != HoldOverweight || holds[0].Text != tc.text || holds[0].Evidence.Reference != "W" ||
			x2.Current() != ExecutionOnHold {
			t.Errorf("%s: held %v, holds %+v; container 2 is over by the allotted figure", name, h, holds)
		}
		if got, _ := x2.CheckLimits(c, l, AllotLots([]*LoadExecution{x1, x2}, l)); got.CargoGrams != tc.g && name == "weight" || got.CargoCubicCm != tc.cm3 && name == "capacity" {
			t.Errorf("%s: container 2 reads %+v", name, got)
		}
	}
}

// A measure in a unit PHY does not convert is not checked, as before; the check says so and the figures are not to be
// relied on.
func TestTheLimitCheckOfAnUnconvertedUnit(t *testing.T) {
	c := twoContainers(t)
	l := lots(measuredLot(t, "L-U", 6, 300, 0, ""))
	line := ownLine("L-U", 2, 100, kernel.Quantity{})
	line.GrossWeight.Unit = "ONZ"
	x := stuffedWith(t, "X-1", 1, []LoadLine{line, apportionedLine("L-U", 4)})
	got, ok := x.CheckLimits(c, l, AllotLots([]*LoadExecution{x}, l))
	if !ok || got.Checked || got.Unchecked != "ONZ" || got.OverMaxGross || got.OverCapacity {
		t.Fatalf("check = %+v", got)
	}
	if !x.HoldIfOverLimitsAllotted(c, l, AllotLots([]*LoadExecution{x}, l), ev("W", t0.Add(5*time.Hour)), "ops", t0.Add(5*time.Hour)) ||
		x.State().Holds[0].Text != "unit 1 (22G1): limits not checked, a measure in ONZ" {
		t.Fatalf("holds = %+v", x.State().Holds)
	}
}

// unitCargoOfV282 is the unitCargo of V2.82, kept as the oracle of a stuffing that holds all the live packs of its
// lots.
func unitCargoOfV282(x *LoadExecution, lots map[CargoLotID]*CargoLot) (grams, cubicCm int64, unchecked kernel.UnitCode) {
	add := func(total *int64, q kernel.Quantity, convert func(kernel.Quantity) (int64, bool)) {
		if q.Amount == 0 && q.Unit == "" {
			return
		}
		v, ok := convert(q)
		if !ok && unchecked == "" {
			unchecked = q.Unit
		}
		*total = addCapped(*total, v)
	}
	type lotShare struct{ apportioned, measuredPacks, measuredWeight, measuredVolume int64 }
	byLot := map[CargoLotID]*lotShare{}
	var order []CargoLotID
	for _, line := range x.state.Lines {
		live, w, v := x.lineLive(line.No)
		if live == 0 {
			continue
		}
		lot, ok := lots[line.LotID]
		if ok {
			if _, measured := lot.Measured(); measured && lot.Received() > 0 {
				s := byLot[line.LotID]
				if s == nil {
					s = &lotShare{}
					byLot[line.LotID], order = s, append(order, line.LotID)
				}
				if line.Basis == BasisApportioned {
					s.apportioned += live
					continue
				}
				s.measuredPacks, s.measuredWeight, s.measuredVolume = s.measuredPacks+live, s.measuredWeight+w, s.measuredVolume+v
			}
		}
		add(&grams, kernel.Quantity{Unit: line.GrossWeight.Unit, Amount: w}, toGrams)
		if line.Volume != (kernel.Quantity{}) {
			add(&cubicCm, kernel.Quantity{Unit: line.Volume.Unit, Amount: v}, toCubicCm)
		}
	}
	for _, id := range order {
		s := byLot[id]
		if s.apportioned == 0 {
			continue
		}
		m, _ := lots[id].Measured()
		rest := lots[id].Received() - s.measuredPacks
		w, v := max(m.GrossWeight.Amount-s.measuredWeight, 0), max(m.Volume.Amount-s.measuredVolume, 0)
		if s.apportioned < rest {
			w, v = share(w, s.apportioned, rest), share(v, s.apportioned, rest)
		}
		add(&grams, kernel.Quantity{Unit: m.GrossWeight.Unit, Amount: w}, toGrams)
		if m.Volume != (kernel.Quantity{}) {
			add(&cubicCm, kernel.Quantity{Unit: m.Volume.Unit, Amount: v}, toCubicCm)
		}
	}
	return grams, cubicCm, unchecked
}

// With one stuffing per lot the allotted check reads what the check of before read, on a table and on generated cases:
// every lot wholly in one of two containers, lines measured for themselves and apportioned, with volumes, some packs
// reversed, some measures in a unit that is not converted, some lots not measured, some lots not wholly loaded.
func TestOneStuffingPerLotReadsTheFigureOfBefore(t *testing.T) {
	c := twoContainers(t)
	check := func(label string, all []*LoadExecution, l map[CargoLotID]*CargoLot) {
		t.Helper()
		a := AllotLots(all, l)
		for _, x := range all {
			g, v, u := unitCargoOfV282(x, l)
			if g2, v2, u2 := x.unitCargo(l, a); g != g2 || v != v2 || u != u2 {
				t.Fatalf("%s: %s reads %d g %d cm3 %q, before %d g %d cm3 %q", label, x.ID(), g2, v2, u2, g, v, u)
			}
			if g2, v2, u2 := x.unitCargo(l, AllotLots([]*LoadExecution{x}, l)); g != g2 || v != v2 || u != u2 {
				t.Fatalf("%s: %s alone reads %d g %d cm3 %q, before %d g %d cm3 %q", label, x.ID(), g2, v2, u2, g, v, u)
			}
		}
	}
	// The table: the lot of the existing tests, weighed at 34,000 kg, with a carton measured for its line at 1 kg and
	// eight apportioned; a lot weighed again; a measure that is not converted.
	l := measuredLot(t, "L-T", 10, 34000, 20000000, "CMQ")
	check("one weighed carton, eight apportioned", []*LoadExecution{stuffedWith(t, "X-1", 1,
		[]LoadLine{ownLine("L-T", 1, 1, cm3(1)), apportionedLine("L-T", 8)})}, lots(l))
	odd := ownLine("L-T", 1, 1, kernel.Quantity{})
	odd.GrossWeight.Unit = "ONZ"
	check("ounces", []*LoadExecution{stuffedWith(t, "X-1", 1, []LoadLine{odd, apportionedLine("L-T", 2)})}, lots(l))
	check("no lots known", []*LoadExecution{stuffedWith(t, "X-1", 1, []LoadLine{apportionedLine("L-T", 2)})}, map[CargoLotID]*CargoLot{})
	// The generated cases.
	rng := rand.New(rand.NewSource(2831))
	for i := 0; i < 3000; i++ {
		known := map[CargoLotID]*CargoLot{}
		lines := [2][]LoadLine{}
		reverse := [2][]Reversal{}
		for n := 0; n < 1+rng.Intn(3); n++ {
			id := CargoLotID(fmt.Sprint("L-", n))
			packs, weight, volume := int64(rng.Intn(10)+1), int64(rng.Intn(9000)+1), int64(rng.Intn(9000)+1)
			if rng.Intn(5) > 0 {
				known[id] = measuredLot(t, id, packs, weight, volume*int64(rng.Intn(2)), "CMQ")
			}
			k, left := rng.Intn(2), packs
			for left > 0 && rng.Intn(6) > 0 {
				m := int64(rng.Intn(int(left))) + 1
				left -= m
				var line LoadLine
				switch rng.Intn(3) {
				case 0:
					line = ownLine(id, m, int64(rng.Intn(5000))+1, cm3(int64(rng.Intn(5000))+1))
					if rng.Intn(8) == 0 {
						line.GrossWeight.Unit = "ONZ"
					}
				case 1:
					line = ownLine(id, m, int64(rng.Intn(5000))+1, kernel.Quantity{})
				default:
					line = apportionedLine(id, m)
					if known[id] == nil {
						line = ownLine(id, m, int64(rng.Intn(5000))+1, kernel.Quantity{}) // an apportioned line has a measure
					}
				}
				lines[k] = append(lines[k], line)
				if m > 1 && rng.Intn(4) == 0 {
					r := Reversal{LineNo: len(lines[k]), Packs: 1, Reason: ReversalMisloaded, Evidence: ev("UN", t0.Add(4*time.Hour)), GrossWeight: kg(0)}
					if line.Basis == BasisMeasured {
						r.GrossWeight = kg(line.GrossWeight.Amount / m)
						r.GrossWeight.Unit = line.GrossWeight.Unit
						if line.Volume != (kernel.Quantity{}) {
							r.Volume = cm3(line.Volume.Amount / m)
						}
					} else {
						r.GrossWeight = kg(1)
					}
					reverse[k] = append(reverse[k], r)
				}
			}
		}
		var all []*LoadExecution
		for k := 0; k < 2; k++ {
			if len(lines[k]) > 0 {
				all = append(all, stuffedWith(t, LoadExecutionID(fmt.Sprint("X-", k+1)), k+1, lines[k], reverse[k]...))
			}
		}
		check(fmt.Sprint("case ", i), all, known)
	}
	// And the decision itself: the stuffing of the 22G1 held at 28,230 kg and one gram over; as before.
	l = measuredLot(t, "L-E", 6, 28231, 0, "")
	x := stuffedWith(t, "X-1", 1, []LoadLine{apportionedLine("L-E", 6)})
	if !x.HoldIfOverLimits(c, lots(l), ev("W", t0.Add(5*time.Hour)), "ops", t0.Add(5*time.Hour)) ||
		!strings.Contains(x.State().Holds[0].Text, "30481.000 kg over the default maximum gross 30480 kg") {
		t.Fatalf("holds = %+v", x.State().Holds)
	}
}

// The check is of the container's cargo, whatever the state of the stuffing is; only a stuffing that is LOADING or
// ON_HOLD is held. A sealed container pushed over its limits by a lot weighed again is not held and its check says it
// is over: 9 of 10 cartons at 10,000 kg are 9,000 kg of cargo; at 40,000 kg 36,000 kg, with the tare 38,250 kg, over
// the 30,480 kg.
func TestTheLimitCheckSaysOverForASealedContainerThatIsNotHeld(t *testing.T) {
	c, entry, lot, x := stuffing(t, 10000)
	at := t0.Add(4 * time.Hour)
	if err := stuff(x, c, LoadLine{Packs: 9, Evidence: ev("TALLY-1", at)}, lot, entry, LotLoad{}, 0, at); err != nil {
		t.Fatal(err)
	}
	l := lots(lot)
	before, ok := x.CheckLimits(c, l, AllotLots([]*LoadExecution{x}, l))
	if !ok || !before.Checked || before.CargoGrams != 9000000 || before.GrossGrams != 11250000 || before.OverMaxGross || before.OverCapacity {
		t.Fatalf("before: %+v", before)
	}
	if err := x.Seal(c, seal("SEAL-1", at), "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := lot.Measure(scale(40000, at), "ops", at); err != nil {
		t.Fatal(err)
	}
	a := AllotLots([]*LoadExecution{x}, l)
	if x.HoldIfOverLimitsAllotted(c, l, a, ev("W-2", at), "ops", at) || len(x.State().Holds) != 0 || x.Current() != ExecutionClosed {
		t.Fatalf("a sealed container is not held: %s %+v", x.Current(), x.State().Holds)
	}
	got, _ := x.CheckLimits(c, l, a)
	if !got.Checked || got.CargoGrams != 36000000 || got.GrossGrams != 38250000 || !got.OverMaxGross || got.OverCapacity {
		t.Fatalf("after: %+v", got)
	}
	// The same figures for a loading and for an on-hold stuffing, and the hold's text names the same gross.
	for _, state := range []string{"LOADING", "ON_HOLD"} {
		c2, entry2, lot2, y := stuffing(t, 10000)
		if err := stuff(y, c2, LoadLine{Packs: 9, Evidence: ev("TALLY-1", at)}, lot2, entry2, LotLoad{}, 0, at); err != nil {
			t.Fatal(err)
		}
		if state == "ON_HOLD" {
			if err := y.PlaceHold(HoldDamage, "", ev("D-1", at), "ops", at); err != nil {
				t.Fatal(err)
			}
		}
		if err := lot2.Measure(scale(40000, at), "ops", at); err != nil {
			t.Fatal(err)
		}
		l2 := lots(lot2)
		a2 := AllotLots([]*LoadExecution{y}, l2)
		if g, _ := y.CheckLimits(c2, l2, a2); g != got {
			t.Errorf("%s: check %+v, sealed %+v", state, g, got)
		}
		if !y.HoldIfOverLimitsAllotted(c2, l2, a2, ev("W-2", at), "ops", at) {
			t.Errorf("%s: not held", state)
		} else if h := y.State().Holds; !strings.Contains(h[len(h)-1].Text, "tare and cargo 38250.000 kg over") {
			t.Errorf("%s: %+v", state, h)
		}
	}
}

// A receipt of more packs of a lot is followed by a limit check; this pins why. By exact arithmetic a receipt only
// lowers the apportioned figures: the packs that do not complete the lot take their share of the measure over more
// packs. Through the rounding of the largest-remainder division one unit of the measure can pass from one container to
// another. A lot of 7 packs weighed at 4 kg, 3, 3 and 1 packs apportioned in three containers: the 7 packs complete the
// lot and take all 4 kg, 4 x 3/7 = 1.71, 1.71 and 0.57; the floors 1 + 1 + 0 leave 2 kg for the largest remainders
// (.71, .71, .57): 2, 2 and 0 kg. An eighth pack is received: the 7 packs no longer complete the lot and take 4 x 7/8 =
// 3.5, 3 kg rounded down, 3 x 3/7 = 1.29, 1.29 and 0.43: the floors 1 + 1 + 0 leave 1 kg, which goes to the smallest
// container, whose remainder .43 is the largest after the 1.29s' .29s: 1, 1 and 1 kg. The third container reads one kg
// more after the receipt, and the totals fall from 4 to 3 kg.
func TestAReceiptCanMoveOneUnitOfTheMeasureIntoAnotherContainer(t *testing.T) {
	l := measuredLot(t, "L-7", 7, 4, 0, "")
	all := []*LoadExecution{stuffedWith(t, "X-1", 1, []LoadLine{apportionedLine("L-7", 3)}), stuffedWith(t, "X-2", 2, []LoadLine{apportionedLine("L-7", 3)}),
		stuffedWith(t, "X-3", 3, []LoadLine{apportionedLine("L-7", 1)})}
	read := func() [3]int64 {
		var out [3]int64
		for i, x := range all {
			out[i] = measureAllotted(x, all, lots(l)).weight.Amount
		}
		return out
	}
	if got := read(); got != [3]int64{2, 2, 0} {
		t.Fatalf("7 packs received: %v, want [2 2 0]", got)
	}
	if err := l.Receive(cartons(1, ConditionGood, t0.Add(3*time.Hour)), "ops", t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != [3]int64{1, 1, 1} || l.Received() != 8 {
		t.Fatalf("8 packs received: %v, want [1 1 1]", got)
	}
}

// TestAnUnloadNeverRaisesTheContainerItComesOutOf is the search behind the comment in the application's limits.go: for
// every lot of 3 to 10 packs and 1 to 40 kg over two or three containers (every split of the packs between them,
// apportioned, and with a line measured for itself of 1 to 3 packs and 1 to 40 kg in container 1), the figure of
// container 1 after 1 up to all of its apportioned packs, or its weighed line, are unloaded is never above its figure
// before. It found no example, so the unload does not check the container it comes out of. (Another container's figure
// does rise by an unload: that is what the other stuffings are checked for.)
func TestAnUnloadNeverRaisesTheContainerItComesOutOf(t *testing.T) {
	parts := func(received, measure, measuredPacks, measuredWeight int64, apportioned []int64) []int64 {
		var packs int64
		for _, n := range apportioned {
			packs += n
		}
		if packs == 0 {
			return make([]int64, len(apportioned))
		}
		w, _ := apportioned2(received, measure, measuredPacks, measuredWeight, packs)
		return divide(w, apportioned)
	}
	cases := 0
	for received := int64(3); received <= 10; received++ {
		for measure := int64(1); measure <= 40; measure++ {
			for a1 := int64(0); a1 <= received; a1++ {
				for a2 := int64(0); a1+a2 <= received; a2++ {
					for a3 := int64(0); a1+a2+a3 <= received; a3++ {
						for k := int64(1); k <= a1; k++ { // apportioned packs of container 1 unloaded
							cases++
							before := parts(received, measure, 0, 0, []int64{a1, a2, a3})[0]
							after := parts(received, measure, 0, 0, []int64{a1 - k, a2, a3})[0]
							if after > before {
								t.Fatalf("%d packs, %d kg, apportioned %d/%d/%d, %d unloaded from container 1: %d kg becomes %d kg", received, measure, a1, a2, a3, k, before, after)
							}
						}
						for mk := int64(1); mk <= 3 && mk+a1+a2+a3 <= received; mk++ { // the weighed line of container 1 unloaded whole
							for mw := int64(1); mw <= 40; mw++ {
								cases++
								before := mw + parts(received, measure, mk, mw, []int64{a1, a2, a3})[0]
								after := parts(received, measure, 0, 0, []int64{a1, a2, a3})[0]
								if after > before {
									t.Fatalf("%d packs, %d kg, %d weighed packs of %d kg, apportioned %d/%d/%d: %d kg becomes %d kg", received, measure, mk, mw, a1, a2, a3, before, after)
								}
							}
						}
					}
				}
			}
		}
	}
	t.Logf("%d cases", cases)
}

// apportioned2 is apportioned for a weight alone.
func apportioned2(received, measure, measuredPacks, measuredWeight, packs int64) (int64, int64) {
	return apportioned(Measurement{GrossWeight: kg(measure)}, received, measuredPacks, measuredWeight, 0, packs)
}

// A container is over when its gross is above the maximum and its cargo above the capacity, not when it is at them: a
// 22G1 weighs 2,250 kg, takes 30,480 kg gross, 28,230,000 g of cargo, and 33,200,000 cm3.
func TestAContainerExactlyAtItsLimitsIsNotOver(t *testing.T) {
	c := twoContainers(t)
	grams := func(amount int64) LoadLine {
		l := ownLine("L-X", 1, amount, kernel.Quantity{})
		l.GrossWeight.Unit = "GRM"
		return l
	}
	for name, tc := range map[string]struct {
		line                  LoadLine
		overGross, overVolume bool
	}{
		"gross at the maximum":      {grams(28230000), false, false},
		"gross one gram over":       {grams(28230001), true, false},
		"capacity at the limit":     {ownLine("L-X", 1, 1, cm3(33200000)), false, false},
		"capacity one cm3 over":     {ownLine("L-X", 1, 1, cm3(33200001)), false, true},
		"both at their limits":      {func() LoadLine { l := grams(28230000); l.Volume = cm3(33200000); return l }(), false, false},
		"both one beyond the limit": {func() LoadLine { l := grams(28230001); l.Volume = cm3(33200001); return l }(), true, true},
	} {
		x := stuffedWith(t, "X-1", 1, []LoadLine{tc.line})
		got, ok := x.CheckLimits(c, map[CargoLotID]*CargoLot{}, AllotLots([]*LoadExecution{x}, map[CargoLotID]*CargoLot{}))
		if !ok || got.OverMaxGross != tc.overGross || got.OverCapacity != tc.overVolume {
			t.Errorf("%s: %+v", name, got)
		}
		if held := x.HoldIfOverLimits(c, map[CargoLotID]*CargoLot{}, ev("W", t0.Add(5*time.Hour)), "ops", t0.Add(5*time.Hour)); held != (tc.overGross || tc.overVolume) {
			t.Errorf("%s: held %v", name, held)
		}
	}
}

// A stuffing a command did not address is held only when the command raised its cargo (LimitsCheck.Raised): a lot of 6
// packs and 60,000 kg with 3 apportioned packs in container 1 is 30,000 kg, over; with a person's release of its hold,
// the same figure after another container's load (1 pack apportioned: 40,000 kg for 4 packs, 30,000 + 10,000) is not
// held, and 44,250 kg once the other container's 2 more packs are weighed at 1,000 kg (59,000 kg over 3 + 1) is. A
// measure that stops being convertible counts as a rise.
func TestAStuffingIsHeldWhenTheCommandRaisedItsCargo(t *testing.T) {
	c := twoContainers(t)
	at := t0.Add(5 * time.Hour)
	lot := measuredLot(t, "L-R", 6, 60000, 0, "")
	l := lots(lot)
	x1 := stuffedWith(t, "X-1", 1, []LoadLine{apportionedLine("L-R", 3)})
	a := AllotLots([]*LoadExecution{x1}, l)
	if !x1.HoldIfOverLimitsAllotted(c, l, a, ev("W-1", at), "ops", at) {
		t.Fatal("30,000 kg is over")
	}
	if err := x1.ReleaseHold(1, Evidence{Source: "CSC_PLATE", Reference: "MSKU1234565", OccurredAt: at}, "", "ops", at); err != nil {
		t.Fatal(err)
	}
	before, _ := x1.CheckLimits(c, l, a)
	x2 := stuffedWith(t, "X-2", 2, []LoadLine{apportionedLine("L-R", 1)})
	a = AllotLots([]*LoadExecution{x1, x2}, l)
	if now, _ := x1.CheckLimits(c, l, a); now.CargoGrams != 30000000 || raised(before, now) {
		t.Fatalf("container 1 after a pack elsewhere: %+v, before %+v", now, before)
	}
	if x1.HoldIfRaisedOverLimits(c, l, a, before, ev("W-2", at), "ops", at) || len(x1.State().Holds) != 1 {
		t.Fatalf("held again though its figure stayed: %+v", x1.State().Holds)
	}
	x2 = stuffedWith(t, "X-2", 2, []LoadLine{apportionedLine("L-R", 1), ownLine("L-R", 2, 1000, kernel.Quantity{})})
	a = AllotLots([]*LoadExecution{x1, x2}, l)
	if now, _ := x1.CheckLimits(c, l, a); now.CargoGrams != 44250000 || !raised(before, now) {
		t.Fatalf("container 1 after weighed packs elsewhere: %+v", now)
	}
	if !x1.HoldIfRaisedOverLimits(c, l, a, before, ev("W-3", at), "ops", at) || len(x1.State().Holds) != 2 || x1.State().Holds[1].Evidence.Reference != "W-3" {
		t.Fatalf("not held though its figure rose over the limit: %+v", x1.State().Holds)
	}
}

func raised(before, after LimitsCheck) bool {
	w, v, u := before.RaisedOver(after)
	return w || v || u
}

// A stuffing a command did not address is held only for a dimension the command raised into excess (G2 of the review):
// over the maximum gross with the grams up, over the capacity with the cubic centimetres up, or unchecked now and
// checked before. Grams are 28,230,000 g of cargo at most in a 22G1, and 33,200,000 cm3.
func TestAStuffingIsHeldOnlyForTheDimensionThatRaisedIntoExcess(t *testing.T) {
	c := twoContainers(t)
	line := func(grams, volume int64, unit kernel.UnitCode) LoadLine {
		l := ownLine("L-X", 1, grams, kernel.Quantity{})
		l.GrossWeight.Unit = unit
		if volume > 0 {
			l.Volume = cm3(volume)
		}
		return l
	}
	none := map[CargoLotID]*CargoLot{}
	at := t0.Add(5 * time.Hour)
	for name, tc := range map[string]struct {
		before, after LoadLine
		held          bool
		text          string
	}{
		"over in weight, weight fell, volume rose by one":   {line(28300000, 100, "GRM"), line(28299000, 101, "GRM"), false, ""},
		"over in weight, weight rose":                       {line(28300000, 100, "GRM"), line(28300001, 100, "GRM"), true, "tare and cargo 30550.001 kg over the default maximum gross"},
		"over in volume, volume rose":                       {line(1000, 33200001, "GRM"), line(1000, 33200002, "GRM"), true, "cargo 33.200002 m3 over the default capacity"},
		"over in volume, volume fell, weight rose by one":   {line(1000, 33200002, "GRM"), line(1001, 33200001, "GRM"), false, ""},
		"over in both, only the volume rose":                {line(28300000, 33300000, "GRM"), line(28299000, 33300001, "GRM"), true, "cargo 33.300001 m3 over the default capacity"},
		"over in both, only the weight rose":                {line(28300000, 33300000, "GRM"), line(28300001, 33299999, "GRM"), true, "tare and cargo 30550.001 kg over the default maximum gross"},
		"within the limits, both rose":                      {line(1000, 100, "GRM"), line(2000, 200, "GRM"), false, ""},
		"unchecked before and after":                        {line(1000, 100, "ONZ"), line(2000, 200, "ONZ"), false, ""},
		"checked before, unchecked now":                     {line(1000, 100, "GRM"), line(1000, 100, "ONZ"), true, "limits not checked, a measure in ONZ"},
		"unchecked before, over in weight and rose, now ok": {line(1000, 100, "ONZ"), line(28300001, 100, "GRM"), true, "tare and cargo 30550.001 kg over the default maximum gross"},
	} {
		x := stuffedWith(t, "X-1", 1, []LoadLine{tc.before})
		before, _ := x.CheckLimits(c, none, AllotLots([]*LoadExecution{x}, none))
		x = stuffedWith(t, "X-1", 1, []LoadLine{tc.after})
		held := x.HoldIfRaisedOverLimits(c, none, AllotLots([]*LoadExecution{x}, none), before, ev("W", at), "ops", at)
		if held != tc.held || (held && !strings.Contains(x.State().Holds[0].Text, tc.text)) {
			t.Errorf("%s: held %v, holds %+v", name, held, x.State().Holds)
		}
	}
	// The three conditions themselves.
	over := LimitsCheck{Checked: true, CargoGrams: 10, CargoCubicCm: 10, OverMaxGross: true, OverCapacity: true}
	for name, tc := range map[string]struct {
		before, after LimitsCheck
		w, v, u       bool
	}{
		"nothing moved":        {over, over, false, false, false},
		"grams up":             {over, LimitsCheck{Checked: true, CargoGrams: 11, CargoCubicCm: 10, OverMaxGross: true, OverCapacity: true}, true, false, false},
		"cm3 up":               {over, LimitsCheck{Checked: true, CargoGrams: 10, CargoCubicCm: 11, OverMaxGross: true, OverCapacity: true}, false, true, false},
		"grams up, not over":   {over, LimitsCheck{Checked: true, CargoGrams: 11, CargoCubicCm: 10, OverCapacity: true}, false, false, false},
		"became unchecked":     {over, LimitsCheck{Unchecked: "ONZ", CargoGrams: 10, CargoCubicCm: 10}, false, false, true},
		"unchecked both times": {LimitsCheck{Unchecked: "ONZ"}, LimitsCheck{Unchecked: "ONZ", CargoGrams: 5}, false, false, false},
	} {
		if w, v, u := tc.before.RaisedOver(tc.after); w != tc.w || v != tc.v || u != tc.u {
			t.Errorf("%s: %v %v %v", name, w, v, u)
		}
	}
}
