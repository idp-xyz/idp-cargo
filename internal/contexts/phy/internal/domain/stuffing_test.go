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
	if err := errors.Join(m.Attach(c, seaIntent(weight), "ops", t0.Add(time.Hour)), c.Close(1, t0.Add(time.Hour), "ops", t0.Add(2*time.Hour))); err != nil {
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

// A 22G1 weighs 2,250 kg and takes 30,480 kg gross: 28,230 kg of cargo.
func TestStuffingHoldsOverTheContainersLimits(t *testing.T) {
	c, entry, lot, x := stuffing(t, 30000) // 10 cartons of 3,000 kg
	at := t0.Add(4 * time.Hour)
	if err := x.Load(LoadLine{Packs: 9, Evidence: ev("TALLY-1", at)}, c, lot, entry, LotLoad{}, 0, "ops", at); err != nil {
		t.Fatal(err)
	}
	if x.Current() != ExecutionLoading || x.Placed() != 0 {
		t.Fatalf("27,000 kg and the tare are within 30,480 kg: %s", x.Current())
	}
	if err := x.Load(LoadLine{Packs: 1, Evidence: ev("TALLY-2", at)}, c, lot, entry, LotLoad{Packs: 9, GrossWeight: 27000}, 9, "ops", at); err != nil {
		t.Fatalf("the line is recorded: %v", err)
	}
	holds := x.State().Holds
	if x.Current() != ExecutionOnHold || len(holds) != 1 || holds[0].Reason != HoldOverweight || holds[0].Evidence.Reference != "TALLY-2" ||
		!strings.Contains(holds[0].Text, "tare and cargo 32250.000 kg over the default maximum gross 30480 kg (edition EQUIPMENT-2026-11-S1)") || x.Tendered() != 10 {
		t.Fatalf("30,000 kg and the tare are over 30,480 kg: %s, %+v", x.Current(), holds)
	}
	if err := x.Close(Evidence{Source: EvidenceSeal, Reference: "SEAL-1", OccurredAt: at}, "ops", at); !errors.Is(err, ErrOnHold) {
		t.Fatalf("no seal while held: %v", err)
	}
	// The plate allows 32,500 kg: the operator releases the hold with it.
	if err := x.ReleaseHold(1, Evidence{Source: "CSC_PLATE", Reference: "MSKU1234565", OccurredAt: at}, "", "ops", at); err != nil {
		t.Fatal(err)
	}
	for name, e := range map[string]Evidence{
		"a tender receipt": {Source: "CFS", Reference: "R-1", OccurredAt: at},
		"no reference":     {Source: EvidenceSeal, OccurredAt: at},
	} {
		if err := x.Close(e, "ops", at); !errors.Is(err, ErrInvalidEvidence) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := x.Close(Evidence{Source: EvidenceSeal, Reference: "SEAL-1", OccurredAt: at}, "ops", at); err != nil {
		t.Fatal(err)
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

func TestStuffingVolumeAndUnits(t *testing.T) {
	c, entry, lot, x := stuffing(t, 3000)
	at := t0.Add(4 * time.Hour)
	// 40 m3 measured for the line is over the 33.2 m3 of a 22G1.
	if err := x.Load(LoadLine{Packs: 5, GrossWeight: kg(1500), Volume: kernel.Quantity{Unit: "MTQ", Amount: 40}, Evidence: ev("TALLY-1", at)}, c, lot,
		entry, LotLoad{}, 0, "ops", at); err != nil {
		t.Fatal(err)
	}
	if h := x.State().Holds; len(h) != 1 || h[0].Reason != HoldOverweight || !strings.Contains(h[0].Text, "cargo 40.000 m3 over the default capacity 33.200 m3") {
		t.Fatalf("holds = %+v", h)
	}
	if err := x.Load(LoadLine{Packs: 1, Evidence: ev("TALLY-2", at)}, c, lot, entry, LotLoad{Packs: 5}, 5, "ops", at); !errors.Is(err, ErrNotLoading) {
		t.Fatalf("no line while held: %v", err)
	}
	for unit, want := range map[kernel.UnitCode]bool{"GRM": true, "KGM": true, "TNE": true, "LBR": false, "": false} {
		if got := checkable(LoadLine{GrossWeight: kernel.Quantity{Unit: unit, Amount: 1}}); got != want {
			t.Errorf("weight in %q checkable = %v", unit, got)
		}
	}
	for unit, want := range map[kernel.UnitCode]bool{"CMQ": true, "DMQ": true, "LTR": true, "MTQ": true, "FTQ": false} {
		if got := checkable(LoadLine{GrossWeight: kg(1), Volume: kernel.Quantity{Unit: unit, Amount: 1}}); got != want {
			t.Errorf("volume in %q checkable = %v", unit, got)
		}
	}
}

func TestStuffingNeedsTheHousesFrozen(t *testing.T) {
	c, entry, lot, x := stuffing(t, 3000)
	at := t0.Add(4 * time.Hour)
	if err := c.Reopen("a late house", "ops", at, 0); err != nil {
		t.Fatal(err)
	}
	if err := x.Load(LoadLine{Packs: 1, Evidence: ev("TALLY-1", at)}, c, lot, entry, LotLoad{}, 0, "ops", at); !errors.Is(err, ErrNotFrozen) {
		t.Fatalf("a reopened consolidation's houses are not stuffed: %v", err)
	}
	if err := x.Close(Evidence{Source: EvidenceSeal, Reference: "SEAL-1", OccurredAt: at}, "ops", at); !errors.Is(err, ErrInvalidLoad) {
		t.Fatalf("an empty container is not sealed: %v", err)
	}
}
