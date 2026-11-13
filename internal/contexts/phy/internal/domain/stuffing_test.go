package domain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var cfs = Facility{Location: loc("CNSHG"), Code: "CFS-1"}

// seaIntent is SH-1 planned by SEA from Shanghai to Hamburg: 10 cartons of the weight given.
func seaIntent(weight int64) LoadIntent {
	i := intent("SH-1", 1)
	i.Mode, i.Route = ModeSea, shgHAM
	i.Scope = CargoScope{Packs: 10, GrossWeight: kg(weight)}
	i.Parts = []Part{{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", Packs: 10, PackageType: "CT", GrossWeight: kg(weight)}}
	return i
}

// stuffing returns a CLOSED own-container consolidation with SH-1 attached and a 22G1 planned (unit 1), the house's
// live entry, its lot of 10 cartons received at the CFS and weighed at the weight given, and a loading stuffing of
// unit 1.
func stuffing(t *testing.T, weight int64) (*Consolidation, Entry, *CargoLot, *LoadExecution) {
	t.Helper()
	c := seaConsol(t, "C-S", KindSeaLCLOwn)
	m, _ := NewMembership("T", "SH-1")
	if _, err := c.PlanLoadUnit(UnitContainer, unit22G1, "ops", t0.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	overrun := "" // a 22G1 takes 28,230 kg of cargo; a heavier plan closes with an overrun reason
	if weight > 28230 {
		overrun = "the plate allows more"
	}
	if err := errors.Join(m.Attach(c, seaIntent(weight), "ops", t0.Add(time.Hour)),
		c.Close([]CargoScope{seaIntent(weight).Scope}, t0.Add(time.Hour), overrun, "ops", t0.Add(2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	lot, err := ReceiveCargo("T", "L-1", cfs, seaIntent(weight), cartons(10, ConditionGood, t0.Add(time.Hour)), "ops", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(lot.Measure(scale(weight, t0.Add(2*time.Hour)), "ops", t0.Add(2*time.Hour)),
		lot.Complete(seaIntent(weight), "ops", t0.Add(2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	x, err := OpenStuffing("T", "X-1", c, 1, cfs, false, "ops", t0.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = x.Start("ops", t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	live, _ := m.Live()
	return c, live, lot, x
}

func TestOpenStuffingRules(t *testing.T) {
	at := t0.Add(3 * time.Hour)
	closed := func(t *testing.T) *Consolidation { c, _, _, _ := stuffing(t, 3000); return c }
	for name, tc := range map[string]struct {
		consol   func(*testing.T) *Consolidation
		unit     int
		facility Facility
		taken    bool
		at       time.Time
		want     error
	}{
		"a planned container of a closed own LCL": {closed, 1, cfs, false, at, nil},
		"the unit has its stuffing":               {closed, 1, cfs, true, at, ErrLoadUnitInUse},
		"no such unit":                            {closed, 2, cfs, false, at, ErrInvalidLoadUnit},
		"at another place":                        {closed, 1, Facility{Location: loc("DEHAM"), Code: "CFS-9"}, false, at, ErrInvalidExecution},
		"before the unit was planned":             {closed, 1, cfs, false, t0.Add(10 * time.Minute), ErrInvalidChange},
		"before the consolidation closed":         {closed, 1, cfs, false, t0.Add(90 * time.Minute), ErrInvalidChange},
		"houses not frozen": {func(t *testing.T) *Consolidation {
			c := seaConsol(t, "C-O", KindSeaLCLOwn)
			if _, err := c.PlanLoadUnit(UnitContainer, unit22G1, "ops", t0.Add(30*time.Minute)); err != nil {
				t.Fatal(err)
			}
			return c
		}, 1, cfs, false, at, ErrNotFrozen},
		"a co-load":     {func(t *testing.T) *Consolidation { return seaConsol(t, "C-K", KindSeaLCLCoload) }, 1, cfs, false, at, ErrKindNotStuffed},
		"an air consol": {func(t *testing.T) *Consolidation { return airConsol(t, "C-A") }, 1, terminal, false, at, ErrKindNotStuffed},
		"a removed unit": {func(t *testing.T) *Consolidation {
			c := closed(t)
			if err := c.RemovePlannedLoadUnit(1, "rolled", "ops", at, false); err != nil {
				t.Fatal(err)
			}
			return c
		}, 1, cfs, false, at, ErrInvalidLoadUnit},
	} {
		t.Run(name, func(t *testing.T) {
			x, err := OpenStuffing("T", "X-9", tc.consol(t), tc.unit, tc.facility, tc.taken, "ops", tc.at)
			if !errors.Is(err, tc.want) || (err == nil) != (tc.want == nil) {
				t.Fatalf("OpenStuffing = %v; want %v", err, tc.want)
			}
			if err == nil && (x.State().Kind != ExecutionStuffing || x.State().LoadUnitNo != tc.unit || x.Pending()[0].ItemNo != tc.unit) {
				t.Fatalf("stuffing = %+v", x.State())
			}
		})
	}
	if _, err := OpenLoadExecution("T", "X-9", closed(t), ExecutionStuffing, cfs, "ops", at); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("a stuffing opens on its unit: %v", err)
	}
	if err := closed(t).RemovePlannedLoadUnit(1, "rolled", "ops", at, true); !errors.Is(err, ErrLoadUnitInUse) {
		t.Fatalf("a unit with its stuffing is not removed: %v", err)
	}
}

// stuff loads a line into the stuffing and checks the container's limits, as RecordCargoLoaded does.
func stuff(x *LoadExecution, c *Consolidation, line LoadLine, lot *CargoLot, entry Entry, lotLoaded LotLoad, houseLoaded int64, at time.Time) error {
	if err := x.Load(line, c, lot, entry, lotLoaded, houseLoaded, "ops", at); err != nil {
		return err
	}
	x.HoldIfOverLimits(c, map[CargoLotID]*CargoLot{lot.ID(): lot}, line.Evidence, "ops", at)
	return nil
}

func seal(ref string, at time.Time) Evidence {
	return Evidence{Source: EvidenceSeal, Reference: ref, OccurredAt: at}
}

// A 22G1 weighs 2,250 kg and takes 30,480 kg gross: 28,230 kg of cargo.
func TestStuffingHoldsOverTheContainersLimits(t *testing.T) {
	c, entry, lot, x := stuffing(t, 30000) // 10 cartons of 3,000 kg
	at := t0.Add(4 * time.Hour)
	if err := stuff(x, c, LoadLine{Packs: 9, Evidence: ev("TALLY-1", at)}, lot, entry, LotLoad{}, 0, at); err != nil {
		t.Fatal(err)
	}
	if x.Current() != ExecutionLoading || x.Placed() != 0 {
		t.Fatalf("27,000 kg and the tare are within 30,480 kg: %s", x.Current())
	}
	if err := stuff(x, c, LoadLine{Packs: 1, Evidence: ev("TALLY-2", at)}, lot, entry, LotLoad{Packs: 9, GrossWeight: 27000}, 9, at); err != nil {
		t.Fatalf("the line is recorded: %v", err)
	}
	holds := x.State().Holds
	if x.Current() != ExecutionOnHold || len(holds) != 1 || holds[0].Reason != HoldOverweight || holds[0].Evidence.Reference != "TALLY-2" ||
		holds[0].Text != "unit 1 (22G1): tare and cargo 32250.000 kg over the default maximum gross 30480 kg (edition EQUIPMENT-2026-11-S1)" ||
		x.Tendered() != 10 {
		t.Fatalf("30,000 kg and the tare are over 30,480 kg: %s, %+v", x.Current(), holds)
	}
	if x.HoldIfOverLimits(c, map[CargoLotID]*CargoLot{lot.ID(): lot}, ev("again", at), "ops", at) {
		t.Fatal("an OVERWEIGHT hold placed is not placed again")
	}
	if err := x.Seal(c, seal("SEAL-1", at), "ops", at); !errors.Is(err, ErrOnHold) {
		t.Fatalf("no seal while held: %v", err)
	}
	// Two cartons come out again while held; the plate allows more anyway, and the operator releases the hold.
	if err := x.Unload(2, 1, ReversalUnstuffed, ev("UN-1", at), lot, "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := x.ReleaseHold(1, Evidence{Source: "CSC_PLATE", Reference: "MSKU1234565", OccurredAt: at}, "", "ops", at); err != nil {
		t.Fatal(err)
	}
	if x.HoldIfOverLimits(c, map[CargoLotID]*CargoLot{lot.ID(): lot}, ev("check", at), "ops", at) {
		t.Fatal("27,000 kg and the tare, after the reversal, are within the limits")
	}
	// A later line over the limits holds again.
	if err := stuff(x, c, LoadLine{Packs: 1, Evidence: ev("TALLY-3", at)}, lot, entry, LotLoad{Packs: 9, GrossWeight: 27000}, 9, at); err != nil {
		t.Fatal(err)
	}
	if h := x.State().Holds; x.Current() != ExecutionOnHold || len(h) != 2 || h[1].Evidence.Reference != "TALLY-3" {
		t.Fatalf("held again: %s, %+v", x.Current(), h)
	}
	if err := x.ReleaseHold(2, Evidence{Source: "CSC_PLATE", Reference: "MSKU1234565", OccurredAt: at}, "", "ops", at); err != nil {
		t.Fatal(err)
	}
	for name, e := range map[string]Evidence{
		"a tender receipt": {Source: "CFS", Reference: "R-1", OccurredAt: at},
		"no reference":     {Source: EvidenceSeal, OccurredAt: at},
	} {
		if err := x.Seal(c, e, "ops", at); !errors.Is(err, ErrInvalidEvidence) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := x.Close(seal("SEAL-1", at), "ops", at); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("a stuffing is sealed, not closed as a tender: %v", err)
	}
	if err := x.Seal(c, seal("SEAL-1", at), "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := x.Unload(1, 1, ReversalOffloadedByForwarder, ev("UN-2", at), lot, "ops", at); !errors.Is(err, ErrNotLoading) {
		t.Fatalf("a sealed container is not unstuffed: %v", err)
	}
	if err := c.Reopen("a late house", "ops", at, 0, Stuffed{Sealed: 1}); !errors.Is(err, ErrExecutionsClosed) {
		t.Fatalf("a consolidation with a sealed container does not reopen: %v", err)
	}
	st := x.State()
	if _, err := RestoreLoadExecution(st); err != nil {
		t.Fatal(err)
	}
	st.CloseEvidence.Source = "CFS"
	if _, err := RestoreLoadExecution(st); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("a stuffing restored closed without its seal: %v", err)
	}
	st = x.State()
	st.LoadUnitNo = 0
	if _, err := RestoreLoadExecution(st); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("a stuffing restored without its unit: %v", err)
	}
}

// The container's cargo is measured as PHY publishes it: apportioned lines from their lot's latest measurement.
func TestStuffingIsCheckedAtThePublishedMeasure(t *testing.T) {
	// Five of ten cartons stuffed at the 10,000 kg measure; the lot is weighed again at 40,000 kg during stuffing.
	c, entry, lot, x := stuffing(t, 10000)
	at := t0.Add(4 * time.Hour)
	if err := stuff(x, c, LoadLine{Packs: 5, Evidence: ev("TALLY-1", at)}, lot, entry, LotLoad{}, 0, at); err != nil || x.Placed() != 0 {
		t.Fatal(x.Placed(), err)
	}
	if err := lot.Measure(scale(40000, at), "ops", at); err != nil {
		t.Fatal(err)
	}
	lots := map[CargoLotID]*CargoLot{lot.ID(): lot}
	if x.HoldIfOverLimits(c, lots, ev("W-2", at), "ops", at) {
		t.Fatal("5 of 10 cartons of 40,000 kg and the tare, 22,250 kg, are within the limits")
	}
	// Four more: 36,000 kg published, with the tare 38,250 kg; the lines as recorded would make 23,250 kg.
	if err := stuff(x, c, LoadLine{Packs: 4, Evidence: ev("TALLY-2", at)}, lot, entry, LotLoad{Packs: 5, GrossWeight: 5000}, 5, at); err != nil {
		t.Fatal(err)
	}
	_, published, _ := x.HouseMeasure("SH-1", lots)
	if published != kg(36000) || x.Current() != ExecutionOnHold || !strings.Contains(x.State().Holds[0].Text, "38250.000 kg") {
		t.Fatalf("published %+v; %s %+v", published, x.Current(), x.State().Holds)
	}
}

func TestStuffingRemeasuredOverTheLimits(t *testing.T) {
	c, entry, lot, x := stuffing(t, 10000)
	at := t0.Add(4 * time.Hour)
	if err := stuff(x, c, LoadLine{Packs: 9, Evidence: ev("TALLY-1", at)}, lot, entry, LotLoad{}, 0, at); err != nil || x.Placed() != 0 {
		t.Fatal(x.Placed(), err)
	}
	if err := lot.Measure(scale(34000, at), "ops", at); err != nil {
		t.Fatal(err)
	}
	// 9 of 10 cartons of 34,000 kg is 30,600 kg; with the tare 32,850 kg, over 30,480 kg.
	if !x.HoldIfOverLimits(c, map[CargoLotID]*CargoLot{lot.ID(): lot}, ev("W-2", at), "ops", at) || x.Current() != ExecutionOnHold ||
		!strings.Contains(x.State().Holds[0].Text, "32850.000 kg") || x.State().Holds[0].Evidence.Reference != "W-2" {
		t.Fatalf("the weighing holds the stuffing: %s %+v", x.Current(), x.State().Holds)
	}
}

func TestStuffingWithMeasuredAndApportionedLines(t *testing.T) {
	// Ten cartons weighed at 34,000 kg; one carton measured for its line at 1 kg; eight apportioned. PHY publishes
	// 33,999 kg of the nine others over the rest, 30,222 kg for the eight, with the tare 32,473 kg.
	c, entry, lot, x := stuffing(t, 10000)
	at := t0.Add(4 * time.Hour)
	if err := lot.Measure(scale(34000, at), "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := stuff(x, c, LoadLine{Packs: 1, GrossWeight: kg(1), Evidence: ev("TALLY-1", at)}, lot, entry, LotLoad{}, 0, at); err != nil {
		t.Fatal(err)
	}
	if err := stuff(x, c, LoadLine{Packs: 8, Evidence: ev("TALLY-2", at)}, lot, entry, LotLoad{Packs: 1, GrossWeight: 1}, 1, at); err != nil {
		t.Fatal(err)
	}
	_, published, _ := x.HouseMeasure("SH-1", map[CargoLotID]*CargoLot{lot.ID(): lot})
	if x.Current() != ExecutionOnHold || !strings.Contains(x.State().Holds[0].Text, decimal((published.Amount+2250)*1000, 3)+" kg") {
		t.Fatalf("published %+v; %s %+v", published, x.Current(), x.State().Holds)
	}
}

func TestStuffingVolumeAndUnits(t *testing.T) {
	c, entry, lot, x := stuffing(t, 3000)
	at := t0.Add(4 * time.Hour)
	// 33.200001 m3 measured for the line is over the 33.2 m3 of a 22G1.
	if err := stuff(x, c, LoadLine{Packs: 5, GrossWeight: kg(1500), Volume: kernel.Quantity{Unit: "CMQ", Amount: 33200001}, Evidence: ev("TALLY-1", at)},
		lot, entry, LotLoad{}, 0, at); err != nil {
		t.Fatal(err)
	}
	if h := x.State().Holds; len(h) != 1 || h[0].Reason != HoldOverweight ||
		!strings.Contains(h[0].Text, "cargo 33.200001 m3 over the default capacity 33.200000 m3") {
		t.Fatalf("holds = %+v", h)
	}
	if err := x.Load(LoadLine{Packs: 1, Evidence: ev("TALLY-2", at)}, c, lot, entry, LotLoad{Packs: 5}, 5, "ops", at); !errors.Is(err, ErrNotLoading) {
		t.Fatalf("no line while held: %v", err)
	}
	for q, want := range map[kernel.Quantity]int64{{Unit: "GRM", Amount: 7}: 7, {Unit: "KGM", Amount: 2}: 2000, {Unit: "TNE", Amount: 3}: 3000000,
		{Unit: "LBR", Amount: 1}: 454, {Unit: "LBR", Amount: 100000}: 45359237} {
		if got, ok := toGrams(q); !ok || got != want {
			t.Errorf("%+v = %d g, %v; want %d", q, got, ok, want)
		}
	}
	for q, want := range map[kernel.Quantity]int64{{Unit: "CMQ", Amount: 7}: 7, {Unit: "DMQ", Amount: 2}: 2000, {Unit: "LTR", Amount: 2}: 2000,
		{Unit: "MTQ", Amount: 3}: 3000000, {Unit: "FTQ", Amount: 1}: 28317, {Unit: "FTQ", Amount: 1000000}: 28316846592} {
		if got, ok := toCubicCm(q); !ok || got != want {
			t.Errorf("%+v = %d cm3, %v; want %d", q, got, ok, want)
		}
	}
	if _, ok := toGrams(kernel.Quantity{Unit: "ONZ", Amount: 1}); ok {
		t.Error("ounces are not converted")
	}
	if got, _ := toGrams(kernel.Quantity{Unit: "TNE", Amount: 1 << 62}); got != 1<<63-1 {
		t.Errorf("capped: %d", got)
	}
}

// A measure in a unit PHY does not convert is recorded and holds the stuffing, not checked.
func TestStuffingInAnUnconvertedUnit(t *testing.T) {
	c, entry, lot, x := stuffing(t, 3000)
	at := t0.Add(4 * time.Hour)
	if err := x.Load(LoadLine{Packs: 1, Evidence: ev("TALLY-1", at)}, c, lot, entry, LotLoad{}, 0, "ops", at); err != nil {
		t.Fatal(err)
	}
	st := x.State()
	st.Lines[0].GrossWeight.Unit, st.Lines[0].Basis, st.Lines[0].MeasurementNo = "ONZ", BasisMeasured, 0
	r, err := RestoreLoadExecution(st)
	if err != nil {
		t.Fatal(err)
	}
	if !r.HoldIfOverLimits(c, map[CargoLotID]*CargoLot{}, ev("TALLY-1", at), "ops", at) ||
		r.State().Holds[0].Text != "unit 1 (22G1): limits not checked, a measure in ONZ" {
		t.Fatalf("holds = %+v", r.State().Holds)
	}
}

func TestStuffingNeedsTheHousesFrozen(t *testing.T) {
	c, entry, lot, x := stuffing(t, 3000)
	at := t0.Add(4 * time.Hour)
	if err := stuff(x, c, LoadLine{Packs: 1, Evidence: ev("TALLY-1", at)}, lot, entry, LotLoad{}, 0, at); err != nil {
		t.Fatal(err)
	}
	if err := c.Reopen("a late house", "ops", at.Add(-time.Minute), 0, Stuffed{Last: x.LastChange()}); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a reopen dated before the stuffing it pauses: %v", err)
	}
	if err := c.Reopen("a late house", "ops", at, 0, Stuffed{Last: x.LastChange()}); err != nil {
		t.Fatal(err)
	}
	if err := x.Load(LoadLine{Packs: 1, Evidence: ev("TALLY-2", at)}, c, lot, entry, LotLoad{Packs: 1}, 1, "ops", at); !errors.Is(err, ErrNotFrozen) {
		t.Fatalf("a reopened consolidation's houses are not stuffed: %v", err)
	}
	if err := x.Seal(c, seal("SEAL-1", at), "ops", at); !errors.Is(err, ErrNotFrozen) {
		t.Fatalf("a container is not sealed on an open house list: %v", err)
	}
	// Closed again at +10h: nothing is stuffed or sealed inside the reopened period.
	if err := c.Close([]CargoScope{seaIntent(3000).Scope}, t0.Add(time.Hour), "", "ops", t0.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := x.Load(LoadLine{Packs: 1, Evidence: ev("TALLY-3", t0.Add(7*time.Hour))}, c, lot, entry, LotLoad{Packs: 1}, 1, "ops", t0.Add(7*time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a line dated in the reopened period: %v", err)
	}
	if err := x.Seal(c, seal("SEAL-1", t0.Add(8*time.Hour)), "ops", t0.Add(8*time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a seal dated in the reopened period: %v", err)
	}
	if err := x.Seal(c, seal("SEAL-1", t0.Add(11*time.Hour)), "ops", t0.Add(11*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyContainerNotSealed(t *testing.T) {
	c, _, _, x := stuffing(t, 3000)
	if err := x.Seal(c, seal("SEAL-1", t0.Add(4*time.Hour)), "ops", t0.Add(4*time.Hour)); !errors.Is(err, ErrInvalidLoad) {
		t.Fatalf("an empty container is not sealed: %v", err)
	}
}
