package domain

import (
	"errors"
	"testing"
	"time"
)

var unit22G1 = UnitLimits{TypeCode: "22G1", TEUTenths: 10, MaxGrossKg: 30480, TareKg: 2250, CapacityCM3: 33200000, Edition: "EQUIPMENT-2026-11-S1"}

func seaConsol(t *testing.T, id ConsolidationID, kind Kind) *Consolidation {
	t.Helper()
	c, err := CreateConsolidation("T", id, ModeSea, kind, "LE", shgHAM, t0.Add(72*time.Hour), t0.Add(60*time.Hour), "ops-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	c.Stamp(t0)
	c.Acknowledge()
	return c
}

func TestPlanLoadUnitRules(t *testing.T) {
	at := t0.Add(time.Hour)
	bad := func(f func(*UnitLimits)) UnitLimits { l := unit22G1; f(&l); return l }
	for name, tc := range map[string]struct {
		consol func(*testing.T) *Consolidation
		kind   LoadUnitKind
		limits UnitLimits
		by     string
		at     time.Time
		want   error
	}{
		"container on own LCL":       {func(t *testing.T) *Consolidation { return seaConsol(t, "C-1", KindSeaLCLOwn) }, UnitContainer, unit22G1, "ops-1", at, nil},
		"container on co-load":       {func(t *testing.T) *Consolidation { return seaConsol(t, "C-1", KindSeaLCLCoload) }, UnitContainer, unit22G1, "ops-1", at, ErrLoadUnitKind},
		"container on air":           {func(t *testing.T) *Consolidation { return airConsol(t, "C-1") }, UnitContainer, unit22G1, "ops-1", at, ErrLoadUnitKind},
		"ULD on own LCL":             {func(t *testing.T) *Consolidation { return seaConsol(t, "C-1", KindSeaLCLOwn) }, "ULD", unit22G1, "ops-1", at, ErrLoadUnitKind},
		"no kind":                    {func(t *testing.T) *Consolidation { return seaConsol(t, "C-1", KindSeaLCLOwn) }, "", unit22G1, "ops-1", at, ErrLoadUnitKind},
		"lower-case type":            {func(t *testing.T) *Consolidation { return seaConsol(t, "C-1", KindSeaLCLOwn) }, UnitContainer, bad(func(l *UnitLimits) { l.TypeCode = "22g1" }), "ops-1", at, ErrInvalidLoadUnit},
		"tare not below max gross":   {func(t *testing.T) *Consolidation { return seaConsol(t, "C-1", KindSeaLCLOwn) }, UnitContainer, bad(func(l *UnitLimits) { l.TareKg = l.MaxGrossKg }), "ops-1", at, ErrInvalidLoadUnit},
		"no TEU":                     {func(t *testing.T) *Consolidation { return seaConsol(t, "C-1", KindSeaLCLOwn) }, UnitContainer, bad(func(l *UnitLimits) { l.TEUTenths = 0 }), "ops-1", at, ErrInvalidLoadUnit},
		"no capacity":                {func(t *testing.T) *Consolidation { return seaConsol(t, "C-1", KindSeaLCLOwn) }, UnitContainer, bad(func(l *UnitLimits) { l.CapacityCM3 = 0 }), "ops-1", at, ErrInvalidLoadUnit},
		"no edition":                 {func(t *testing.T) *Consolidation { return seaConsol(t, "C-1", KindSeaLCLOwn) }, UnitContainer, bad(func(l *UnitLimits) { l.Edition = " " }), "ops-1", at, ErrInvalidLoadUnit},
		"no actor":                   {func(t *testing.T) *Consolidation { return seaConsol(t, "C-1", KindSeaLCLOwn) }, UnitContainer, unit22G1, " ", at, ErrInvalidChange},
		"before the current version": {func(t *testing.T) *Consolidation { return seaConsol(t, "C-1", KindSeaLCLOwn) }, UnitContainer, unit22G1, "ops-1", t0.Add(-time.Minute), ErrInvalidChange},
		"cancelled": {func(t *testing.T) *Consolidation {
			c := seaConsol(t, "C-1", KindSeaLCLOwn)
			if err := c.Cancel("no space", "ops-1", t0.Add(time.Minute), 0, 0); err != nil {
				t.Fatal(err)
			}
			return c
		}, UnitContainer, unit22G1, "ops-1", at, ErrInvalidTransition},
		"closed": {func(t *testing.T) *Consolidation {
			c := seaConsol(t, "C-1", KindSeaLCLOwn)
			if err := c.Close(1, t0, "ops-1", t0.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			return c
		}, UnitContainer, unit22G1, "ops-1", at, nil},
	} {
		t.Run(name, func(t *testing.T) {
			c := tc.consol(t)
			c.Acknowledge()
			no, err := c.PlanLoadUnit(tc.kind, tc.limits, tc.by, tc.at)
			if !errors.Is(err, tc.want) || (err == nil) != (tc.want == nil) {
				t.Fatalf("PlanLoadUnit = %d, %v; want %v", no, err, tc.want)
			}
			if err != nil && (len(c.Pending()) != 0 || len(c.State().LoadUnits) != 0) {
				t.Fatalf("a refused unit changed the consolidation: %+v", c.State().LoadUnits)
			}
		})
	}
}

func TestLoadUnitLifecycle(t *testing.T) {
	c := seaConsol(t, "C-1", KindSeaLCLOwn)
	unit42G1 := UnitLimits{TypeCode: "42G1", TEUTenths: 20, MaxGrossKg: 30480, TareKg: 3750, CapacityCM3: 67700000, Edition: "EQUIPMENT-2026-11-S1"}
	for i, l := range []UnitLimits{unit22G1, unit42G1} {
		no, err := c.PlanLoadUnit(UnitContainer, l, "ops-1", t0.Add(time.Hour))
		if err != nil || no != i+1 {
			t.Fatalf("unit %d = %d, %v", i+1, no, err)
		}
	}
	if p := c.Pending(); len(p) != 2 || p[0].Type != eventLoadUnitPlanned || p[1].ItemNo != 2 || p[1].ConsolidationID != "C-1" || p[1].Reason != "" {
		t.Fatalf("events = %+v", p)
	}
	entered := t0.Add(time.Hour + time.Second)
	c.Stamp(entered)
	if added := c.AddedLoadUnits(); len(added) != 2 || added[1].Limits != unit42G1 || !added[0].PlannedEnteredAt.Equal(entered) || added[0].Status != UnitPlanned {
		t.Fatalf("added = %+v", added)
	}
	c.Acknowledge()
	if len(c.AddedLoadUnits()) != 0 || len(c.RemovedLoadUnits()) != 0 {
		t.Fatal("acknowledged units are still added")
	}

	// Removal: a reason, a planned unit, a time not before its planning.
	for name, tc := range map[string]struct {
		no     int
		reason string
		at     time.Time
		want   error
	}{
		"no reason":           {1, " ", t0.Add(2 * time.Hour), ErrInvalidChange},
		"reason too long":     {1, string(make([]rune, 501)), t0.Add(2 * time.Hour), ErrInvalidChange},
		"no such unit":        {3, "rolled", t0.Add(2 * time.Hour), ErrInvalidLoadUnit},
		"unit zero":           {0, "rolled", t0.Add(2 * time.Hour), ErrInvalidLoadUnit},
		"before its planning": {1, "rolled", t0.Add(30 * time.Minute), ErrInvalidChange},
	} {
		if err := c.RemovePlannedLoadUnit(tc.no, tc.reason, "ops-1", tc.at, false); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if len(c.Pending()) != 0 || len(c.LiveLoadUnits()) != 2 {
		t.Fatal("a refused removal changed the consolidation")
	}
	if err := c.RemovePlannedLoadUnit(1, "cargo fits one 40ft", "ops-2", t0.Add(2*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if err := c.RemovePlannedLoadUnit(1, "again", "ops-2", t0.Add(2*time.Hour), false); !errors.Is(err, ErrInvalidLoadUnit) {
		t.Fatalf("second removal = %v", err)
	}
	c.Stamp(t0.Add(3 * time.Hour))
	removed := c.RemovedLoadUnits()
	if len(removed) != 1 || removed[0].No != 1 || removed[0].Status != UnitRemoved || removed[0].RemovedBy != "ops-2" ||
		!removed[0].RemoveEnteredAt.Equal(t0.Add(3*time.Hour)) || !removed[0].PlannedEnteredAt.Equal(entered) {
		t.Fatalf("removed = %+v", removed)
	}
	if live := c.LiveLoadUnits(); len(live) != 1 || live[0].No != 2 {
		t.Fatalf("live = %+v", live)
	}
	if p := c.Pending(); len(p) != 1 || p[0].Type != eventLoadUnitRemoved || p[0].ItemNo != 1 || p[0].Reason != "" {
		t.Fatalf("events = %+v (the reason is free text and stays out)", p)
	}
	c.Acknowledge()

	// A unit planned and removed before the consolidation is written is added and removed: the store writes it as
	// planned, then its removal.
	no, err := c.PlanLoadUnit(UnitContainer, unit22G1, "ops-1", t0.Add(4*time.Hour))
	if err != nil || no != 3 {
		t.Fatal(no, err)
	}
	if err = c.RemovePlannedLoadUnit(3, "planned by mistake", "ops-1", t0.Add(4*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	c.Stamp(t0.Add(5 * time.Hour))
	if added, removed := c.AddedLoadUnits(), c.RemovedLoadUnits(); len(added) != 1 || added[0].Status != UnitRemoved ||
		added[0].RemoveEnteredAt.IsZero() || len(removed) != 1 || removed[0].No != 3 {
		t.Fatalf("added %+v, removed %+v", added, removed)
	}
	c.Acknowledge()

	// The state restores as written, and a removed unit is still refused.
	r, err := RestoreConsolidation(c.State())
	if err != nil {
		t.Fatal(err)
	}
	if got := r.State().LoadUnits; len(got) != 3 || got[0].RemoveReason != "cargo fits one 40ft" || len(r.AddedLoadUnits()) != 0 {
		t.Fatalf("restored %+v", got)
	}
	if err = r.RemovePlannedLoadUnit(1, "again", "ops-1", t0.Add(6*time.Hour), false); !errors.Is(err, ErrInvalidLoadUnit) {
		t.Fatal(err)
	}
}

func TestRestoreRefusesInvalidLoadUnits(t *testing.T) {
	c := seaConsol(t, "C-1", KindSeaLCLOwn)
	if _, err := c.PlanLoadUnit(UnitContainer, unit22G1, "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	c.Stamp(t0.Add(time.Hour))
	good := c.State()
	for name, mutate := range map[string]func(*ConsolidationState){
		"numbered from 2":        func(st *ConsolidationState) { st.LoadUnits[0].No = 2 },
		"on a co-load":           func(st *ConsolidationState) { st.Kind = KindSeaLCLCoload },
		"unknown kind":           func(st *ConsolidationState) { st.LoadUnits[0].Kind = "ULD" },
		"unknown status":         func(st *ConsolidationState) { st.LoadUnits[0].Status = "IN_USE" },
		"removed without reason": func(st *ConsolidationState) { st.LoadUnits[0].Status = UnitRemoved },
		"planned with a removal": func(st *ConsolidationState) { st.LoadUnits[0].RemoveReason = "x" },
		"invalid limits":         func(st *ConsolidationState) { st.LoadUnits[0].Limits.TareKg = 0 },
		"no planner":             func(st *ConsolidationState) { st.LoadUnits[0].PlannedBy = "" },
	} {
		st := good
		st.LoadUnits = append([]PlannedLoadUnit(nil), good.LoadUnits...)
		mutate(&st)
		if _, err := RestoreConsolidation(st); !errors.Is(err, ErrInvalidConsolidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := RestoreConsolidation(good); err != nil {
		t.Fatal(err)
	}
}

// A load unit planned or removed is a change of the consolidation: nothing later may be dated before it.
func TestNothingIsDatedBeforeALoadUnitChange(t *testing.T) {
	planned := func(t *testing.T) *Consolidation {
		c := seaConsol(t, "C-1", KindSeaLCLOwn)
		if _, err := c.PlanLoadUnit(UnitContainer, unit22G1, "ops-1", t0.Add(5*time.Hour)); err != nil {
			t.Fatal(err)
		}
		return c
	}
	early := t0.Add(time.Hour)
	for name, change := range map[string]func(*Consolidation, time.Time) error{
		"cancel": func(c *Consolidation, at time.Time) error {
			return c.Cancel("vessel omitted the port", "ops-1", at, 0, 0)
		},
		"close": func(c *Consolidation, at time.Time) error { return c.Close(1, t0, "ops-1", at) },
		"amend": func(c *Consolidation, at time.Time) error {
			return c.Amend(Route{}, t0.Add(80*time.Hour), t0.Add(70*time.Hour), "vessel change", "ops-1", at, 0)
		},
		"plan another": func(c *Consolidation, at time.Time) error {
			_, err := c.PlanLoadUnit(UnitContainer, unit22G1, "ops-1", at)
			return err
		},
		"remove": func(c *Consolidation, at time.Time) error {
			return c.RemovePlannedLoadUnit(1, "rolled", "ops-1", at, false)
		},
	} {
		c := planned(t)
		if err := change(c, early); !errors.Is(err, ErrInvalidChange) {
			t.Errorf("%s before the unit's planning: %v", name, err)
		}
		if err := change(c, t0.Add(5*time.Hour)); err != nil {
			t.Errorf("%s at the unit's planning: %v", name, err)
		}
	}
	// A removal moves the bound on too.
	c := planned(t)
	if err := c.RemovePlannedLoadUnit(1, "rolled", "ops-1", t0.Add(6*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel("vessel omitted the port", "ops-1", t0.Add(5*time.Hour+30*time.Minute), 0, 0); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("cancel before the removal: %v", err)
	}
	// A stored removal dated before its planning does not restore.
	st := c.State()
	st.LoadUnits[0].RemovedAt = t0.Add(4 * time.Hour)
	if _, err := RestoreConsolidation(st); !errors.Is(err, ErrInvalidConsolidation) {
		t.Fatalf("removal before planning restored: %v", err)
	}
}
