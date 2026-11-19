//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func cmq(n int64) kernel.Quantity { return kernel.Quantity{Unit: "CMQ", Amount: n} }

func (f *fixture) basis(t *testing.T) api.PhysicalCostBasis {
	t.Helper()
	b, err := f.module.GetPhysicalCostBasis(context.Background(), api.GetPhysicalCostBasis{TenantID: f.tenant, ConsolidationID: "C-S"})
	if err != nil {
		t.Fatalf("cost basis: %v", err)
	}
	return b
}

// TestTheCostBasisShowsEachHouseInEachUnit stuffs two containers of an own-container consolidation with three houses: A
// wholly in unit 1, B wholly in unit 2 and C split over both, 4 packs and 2. Each lot is 6 packs. A's lot is 312 kg and
// 800,000 cm3, B's 288 kg and 200,000 cm3, C's 800 kg and 2,000,000 cm3. A stuffing measures a house as its totals are
// measured, by itself: all of a lot's packs take its whole measure, and part of a lot's packs take their share rounded
// down. So C's 4 packs in unit 1 are 4/6 of 800 kg = 533 kg and 4/6 of 2,000,000 cm3 = 1,333,333 cm3; its 2 packs in
// unit 2 are 266 kg and 666,666 cm3, and the totals of C are the sums, 799 kg and 1,999,999 cm3. Unloading 1 of the 2
// packs leaves 1 pack, 1/6 of the lot: 133 kg and 333,333 cm3. xid is the execution id of the stuffing of a unit. The
// ids sort opposite to the unit numbers, so that the order of units in the answers cannot come from the order the
// stuffings are read in (by execution id).
func xid(unit int) string { return map[int]string{1: "X-B", 2: "X-A"}[unit] }

// threeHousesInTwoUnits builds the day of the first test below: a closed own-container consolidation C-S with two
// planned 22G1 units and houses SH-A, SH-B and SH-C, each with a lot of 6 packs received at the CFS, weighed and
// completed. No stuffing is opened.
func threeHousesInTwoUnits(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	for _, s := range []string{"SH-A", "SH-B", "SH-C"} {
		f.ful.sea[s] = true
	}
	f.createSea(t, "C-S", api.KindSeaLCLOwn)
	ok(t, 2)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-1", "C-S", 1, "22G1")))
	ok(t, 3)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-2", "C-S", 2, "22G1")))
	for i, s := range []string{"SH-C", "SH-B", "SH-A"} { // attached against the shipment order, which the answers keep
		if _, err := f.module.AttachHouse(ctx, f.attach(fmt.Sprint("attach-", i), "C-S", s, 0)); err != nil {
			t.Fatal(err)
		}
		f.ful.allocate(s, "C-S")
	}
	ok(t, 4)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-S", 3)))
	at := f.at.Add(3 * time.Hour)
	weights := map[string]int64{"A": 312, "B": 288, "C": 800}
	volumes := map[string]int64{"A": 800_000, "B": 200_000, "C": 2_000_000}
	for _, s := range []string{"A", "B", "C"} {
		lot := "L-" + s
		rcv := f.receive("rcv-"+lot, lot, "SH-"+s, 0, 6, api.ConditionGood, at)
		rcv.Facility = cfsShanghai
		ok(t, 1)(f.module.ReceiveCargo(ctx, rcv))
		ok(t, 2)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure-" + lot, TenantID: f.tenant, ActorID: "ops-1",
			LotID: lot, ExpectedRevision: 1, GrossWeight: kgm(weights[s]), Volume: cmq(volumes[s]), Method: api.MethodScale,
			Evidence: evidence("measure-"+lot, at), OccurredAt: at}))
		ok(t, 3)(f.module.CompleteReceipt(ctx, api.CompleteReceipt{CommandID: "complete-" + lot, TenantID: f.tenant, ActorID: "ops-1", LotID: lot,
			ExpectedRevision: 2, OccurredAt: at}))
	}
	return f
}

func TestTheCostBasisShowsEachHouseInEachUnit(t *testing.T) {
	ctx := context.Background()
	f := threeHousesInTwoUnits(t)
	// before any stuffing: no entries, both units PLANNED without a stuffing
	b := f.basis(t)
	if len(b.Houses) != 3 || len(b.LoadUnits) != 2 {
		t.Fatalf("basis = %+v", b)
	}
	for _, h := range b.Houses {
		if len(h.Units) != 0 {
			t.Errorf("%s has entries before any stuffing: %+v", h.ShipmentID, h.Units)
		}
	}
	for _, u := range b.LoadUnits {
		if u.Status != api.UnitStatusPlanned || u.ExecutionID != "" || u.ExecutionState != "" || u.ExecutionRevision != 0 {
			t.Errorf("unit %d before any stuffing: %+v", u.UnitNo, u)
		}
	}

	// the stuffings opened: IN_USE with the state OPEN, then LOADING, the revision moving on
	opened := f.hoursIn(4)
	revision := map[int]int64{}
	for unit := 1; unit <= 2; unit++ {
		x := xid(unit)
		ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-"+x, x, unit, opened)))
		u := f.basis(t).LoadUnits[unit-1]
		if u.Status != api.UnitStatusInUse || u.ExecutionID != x || u.ExecutionState != api.ExecutionOpen || u.ExecutionRevision != 1 {
			t.Fatalf("unit %d once opened: %+v", unit, u)
		}
		ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-" + x, TenantID: f.tenant, ActorID: "ops-1", ExecutionID: x,
			ExpectedRevision: 1, OccurredAt: opened}))
		revision[unit] = 2
		if u = f.basis(t).LoadUnits[unit-1]; u.ExecutionState != api.ExecutionLoading || u.ExecutionRevision != 2 {
			t.Fatalf("unit %d once loading: %+v", unit, u)
		}
	}
	for _, h := range f.basis(t).Houses {
		if len(h.Units) != 0 {
			t.Fatalf("nothing is loaded yet, but %s has entries: %+v", h.ShipmentID, h.Units)
		}
	}

	type entry struct {
		unit  int
		packs int64
		kilos int64
		cubic int64
	}
	check := func(when string, want map[string][]entry) {
		t.Helper()
		b := f.basis(t)
		for _, h := range b.Houses {
			var got []entry
			var packs int64
			var weight, volume kernel.Quantity
			for _, e := range h.Units {
				if e.ExecutionID != xid(e.UnitNo) {
					t.Errorf("%s: %s: entry of unit %d names %s", when, h.ShipmentID, e.UnitNo, e.ExecutionID)
				}
				got = append(got, entry{e.UnitNo, e.LoadedPacks, e.LoadedGrossWeight.Amount, e.LoadedVolume.Amount})
				if e.LoadedGrossWeight.Unit != "KGM" || e.LoadedVolume.Unit != "CMQ" {
					t.Errorf("%s: %s: units of %+v", when, h.ShipmentID, e)
				}
				packs += e.LoadedPacks
				weight.Amount += e.LoadedGrossWeight.Amount
				volume.Amount += e.LoadedVolume.Amount
			}
			w := want[h.ShipmentID]
			if len(got) != len(w) {
				t.Errorf("%s: %s: entries %v, want %v", when, h.ShipmentID, got, w)
				continue
			}
			for i := range w {
				if got[i] != w[i] {
					t.Errorf("%s: %s: entries %v, want %v", when, h.ShipmentID, got, w)
				}
			}
			if len(got) > 0 && (packs != h.LoadedPacks || weight.Amount != h.LoadedGrossWeight.Amount || volume.Amount != h.LoadedVolume.Amount) {
				t.Errorf("%s: %s: entries sum to %d packs, %d kg, %d cm3; the totals are %d, %+v, %+v", when, h.ShipmentID, packs, weight.Amount,
					volume.Amount, h.LoadedPacks, h.LoadedGrossWeight, h.LoadedVolume)
			}
		}
	}
	// the packing list of each unit with a stuffing has the lines of the houses' entries for it, in unit and shipment
	// order
	lines := func(when string) {
		t.Helper()
		b := f.basis(t)
		if len(b.PackingLists) != 2 || b.PackingLists[0].UnitNo != 1 || b.PackingLists[1].UnitNo != 2 {
			t.Fatalf("%s: packing lists %+v", when, b.PackingLists)
		}
		for i, l := range b.PackingLists {
			var want []api.UnitPackingLine
			for _, h := range b.Houses {
				for _, e := range h.Units {
					if e.UnitNo == l.UnitNo {
						want = append(want, api.UnitPackingLine{ShipmentID: h.ShipmentID, LoadedPacks: e.LoadedPacks, LoadedGrossWeight: e.LoadedGrossWeight,
							LoadedVolume: e.LoadedVolume})
					}
				}
			}
			if !reflect.DeepEqual(l.Lines, want) || l.ExecutionID != xid(l.UnitNo) || l.ExecutionState != b.LoadUnits[i].ExecutionState || len(l.Digest) != 64 {
				t.Errorf("%s: unit %d packing list %+v, want lines %+v", when, l.UnitNo, l, want)
			}
		}
	}
	// each load moves the unit's revision on by one
	load := func(unit int, shipment string, packs int64) api.Receipt {
		t.Helper()
		x := xid(unit)
		r := ok(t, revision[unit]+1)(f.module.RecordCargoLoaded(ctx, f.load(fmt.Sprintf("load-%s-%s-%d", x, shipment, revision[unit]), x, revision[unit], "L-"+shipment, packs, opened)))
		revision[unit]++
		if u := f.basis(t).LoadUnits[unit-1]; u.ExecutionRevision != revision[unit] || u.ExecutionState != api.ExecutionLoading || u.Status != api.UnitStatusInUse {
			t.Fatalf("unit %d after a load: %+v, want revision %d", unit, u, revision[unit])
		}
		return r
	}
	load(1, "A", 6)
	check("A in unit 1", map[string][]entry{"SH-A": {{1, 6, 312, 800_000}}})
	load(1, "C", 4)
	check("4 of C in unit 1", map[string][]entry{"SH-A": {{1, 6, 312, 800_000}}, "SH-C": {{1, 4, 533, 1_333_333}}})
	load(2, "B", 6)
	lineC2 := load(2, "C", 2)
	full := map[string][]entry{"SH-A": {{1, 6, 312, 800_000}}, "SH-B": {{2, 6, 288, 200_000}},
		"SH-C": {{1, 4, 533, 1_333_333}, {2, 2, 266, 666_666}}}
	check("all loaded", full)
	lines("all loaded")
	for _, h := range f.basis(t).Houses {
		if h.ShipmentID == "SH-C" && (h.LoadedPacks != 6 || h.LoadedGrossWeight != kgm(799) || h.LoadedVolume != cmq(1_999_999)) {
			t.Errorf("C's totals: %+v", h)
		}
	}
	if x, err := f.module.GetLoadExecution(ctx, api.GetLoadExecution{TenantID: f.tenant, ExecutionID: xid(2)}); err != nil || x.Revision != revision[2] {
		t.Errorf("the unit's revision is the stuffing's: %+v, %v", x, err)
	}

	// the consolidation query answers the same state and revision as the cost basis
	c := f.consolidation(t, "C-S")
	for i, u := range f.basis(t).LoadUnits {
		cu := c.LoadUnits[i]
		if cu.ExecutionState != u.ExecutionState || cu.ExecutionRevision != u.ExecutionRevision || cu.ExecutionState != api.ExecutionLoading || cu.ExecutionRevision != revision[i+1] {
			t.Errorf("unit %d: the consolidation says %q at %d, the cost basis %q at %d", i+1, cu.ExecutionState, cu.ExecutionRevision, u.ExecutionState,
				u.ExecutionRevision)
		}
	}

	// unloading one of C's 2 packs from unit 2 shrinks the entry, the revision moves on; unloading the other removes it
	unload := func(name string, packs int64) {
		t.Helper()
		ok(t, revision[2]+1)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: name, TenantID: f.tenant, ActorID: "ops-1", ExecutionID: xid(2),
			ExpectedRevision: revision[2], LineNo: lineC2.ItemNo, Packs: packs, Reason: api.ReversalMisloaded, Evidence: evidence(name, opened), OccurredAt: opened}))
		revision[2]++
		if u := f.basis(t).LoadUnits[1]; u.ExecutionRevision != revision[2] {
			t.Fatalf("unit 2 after %s: %+v, want revision %d", name, u, revision[2])
		}
	}
	unload("unload-1", 1)
	check("one pack of C out of unit 2", map[string][]entry{"SH-A": {{1, 6, 312, 800_000}}, "SH-B": {{2, 6, 288, 200_000}},
		"SH-C": {{1, 4, 533, 1_333_333}, {2, 1, 133, 333_333}}})
	unload("unload-2", 1)
	check("all of C out of unit 2", map[string][]entry{"SH-A": {{1, 6, 312, 800_000}}, "SH-B": {{2, 6, 288, 200_000}},
		"SH-C": {{1, 4, 533, 1_333_333}}})

	// the packs of C put back, then both containers sealed: CLOSED, the revision moved on, the entries as they were
	load(2, "C", 2)
	check("C back in unit 2", full)
	for unit := 1; unit <= 2; unit++ {
		x := xid(unit)
		ok(t, revision[unit]+1)(f.module.CloseLoadExecution(ctx, api.CloseLoadExecution{CommandID: "seal-" + x, TenantID: f.tenant, ActorID: "ops-1", ExecutionID: x,
			ExpectedRevision: revision[unit], Evidence: api.Evidence{Source: api.EvidenceSeal, Reference: "SEAL-" + x, OccurredAt: opened}, OccurredAt: opened}))
		revision[unit]++
		if u := f.basis(t).LoadUnits[unit-1]; u.Status != api.UnitStatusClosed || u.ExecutionState != api.ExecutionClosed || u.ExecutionRevision != revision[unit] {
			t.Fatalf("unit %d once sealed: %+v, want revision %d", unit, u, revision[unit])
		}
	}
	check("sealed", full)
	lines("sealed")
}

// hoursIn is a business time in the past, hours after the fixture's start.
func (f *fixture) hoursIn(hours int) time.Time { return f.at.Add(time.Duration(hours) * time.Hour) }

// TestATenderedHouseHasNoUnitEntries loads the houses of an air consolidation by a tender: the totals are as ever, the
// houses have no unit entries and the consolidation has no load units.
func TestATenderedHouseHasNoUnitEntries(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.create(t, "C-1")
	if _, err := f.module.AttachHouse(ctx, f.attach("attach-0", "C-1", "SH-1", 0)); err != nil {
		t.Fatal(err)
	}
	f.ful.allocate("SH-1", "C-1")
	ok(t, 2)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-1", 1)))
	at := f.at.Add(3 * time.Hour)
	ok(t, 1)(f.module.ReceiveCargo(ctx, f.receive("rcv-1", "L-1", "SH-1", 0, 6, api.ConditionGood, at)))
	ok(t, 2)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure-1", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-1",
		ExpectedRevision: 1, GrossWeight: kgm(300), Volume: kernel.Quantity{Unit: "MTQ", Amount: 2}, Method: api.MethodScale, Evidence: evidence("measure-1", at),
		OccurredAt: at}))
	ok(t, 3)(f.module.RecordScreeningResult(ctx, api.RecordScreeningResult{CommandID: "screen-1", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-1",
		ExpectedRevision: 2, Outcome: api.ScreeningPassed, Screener: "PVG security station", Method: "XRAY", Reference: "SC-1",
		Evidence: api.Evidence{Source: "PVG-SECURITY", Reference: "SC-1", OccurredAt: at}, OccurredAt: at}))
	ok(t, 4)(f.module.CompleteReceipt(ctx, api.CompleteReceipt{CommandID: "complete-1", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-1",
		ExpectedRevision: 3, OccurredAt: at.Add(10 * time.Minute)}))
	at = f.at.Add(4 * time.Hour)
	f.tender(t, "X-1", "C-1", at)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-1", "X-1", 2, "L-1", 6, at)))
	b, err := f.module.GetPhysicalCostBasis(ctx, api.GetPhysicalCostBasis{TenantID: f.tenant, ConsolidationID: "C-1"})
	if err != nil || len(b.Houses) != 1 || len(b.LoadUnits) != 0 {
		t.Fatalf("basis = %+v, %v", b, err)
	}
	if h := b.Houses[0]; h.LoadedPacks != 6 || h.LoadedGrossWeight != kgm(300) || len(h.Units) != 0 {
		t.Fatalf("a house loaded by a tender: %+v", h)
	}
}

// TestAUnitsPackingListHasADigestOfWhatItHolds reads the digest of unit 1 of the three-house day: the same on two reads
// with nothing changed, different after a load, an unload, a hold with its release and the seal. It recomputes the
// digest of unit 1, which holds two lines (A, 6 packs, 312 kg, 800,000 cm3; C, 4 packs, 533 kg, 1,333,333 cm3), from
// the canonical text written on api.UnitPackingList, so that the form is pinned.
func TestAUnitsPackingListHasADigestOfWhatItHolds(t *testing.T) {
	ctx := context.Background()
	f := threeHousesInTwoUnits(t)
	at := f.hoursIn(4)
	x := xid(1)
	digest := func() string {
		t.Helper()
		b := f.basis(t)
		return b.PackingLists[0].Digest
	}
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-"+x, x, 1, at)))
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-"+xid(2), xid(2), 2, at)))
	if got := f.basis(t).PackingLists; len(got) != 2 || got[0].UnitNo != 1 || got[1].UnitNo != 2 || len(got[0].Lines) != 0 || got[0].Digest == "" || got[0].ExecutionState != api.ExecutionOpen {
		t.Fatalf("a unit with a stuffing and no packs has no lines and a digest: %+v", got)
	}
	seen := map[string]string{digest(): "open"}
	changed := func(what string) {
		t.Helper()
		d := digest()
		if again := digest(); again != d {
			t.Fatalf("two reads after %s differ: %s and %s", what, d, again)
		}
		if was, dup := seen[d]; dup {
			t.Fatalf("the digest after %s is the one after %s", what, was)
		}
		seen[d] = what
	}
	revision := int64(1)
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: x, ExpectedRevision: 1, OccurredAt: at}))
	revision = 2
	changed("loading started")
	step := func(r api.Receipt, err error) api.Receipt {
		t.Helper()
		revision++
		return ok(t, revision)(r, err)
	}
	step(f.module.RecordCargoLoaded(ctx, f.load("load-a", x, revision, "L-A", 6, at)))
	changed("a load of A")
	loadC := step(f.module.RecordCargoLoaded(ctx, f.load("load-c", x, revision, "L-C", 4, at)))
	changed("a load of C")
	step(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "unload", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: x, ExpectedRevision: revision,
		LineNo: loadC.ItemNo, Packs: 1, Reason: api.ReversalMisloaded, Evidence: evidence("unload", at), OccurredAt: at}))
	changed("an unload of C")
	step(f.module.PlaceLoadExecutionOnHold(ctx, api.PlaceLoadExecutionOnHold{CommandID: "hold", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: x, ExpectedRevision: revision,
		Reason: api.HoldCustoms, Text: "customs look", Evidence: evidence("hold", at), OccurredAt: at}))
	if f.basis(t).PackingLists[0].ExecutionState != api.ExecutionOnHold {
		t.Fatalf("held: %+v", f.basis(t).PackingLists[0])
	}
	changed("a hold")
	step(f.module.ReleaseLoadExecutionHold(ctx, api.ReleaseLoadExecutionHold{CommandID: "release", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: x, ExpectedRevision: revision,
		HoldNo: 1, Evidence: evidence("release", at), OccurredAt: at}))
	if f.basis(t).PackingLists[0].ExecutionState != api.ExecutionLoading {
		t.Fatalf("released: %+v", f.basis(t).PackingLists[0])
	}
	// back to LOADING with the lines of the unload: the same stuffing, state and lines, so the same digest
	if d := digest(); seen[d] != "an unload of C" {
		t.Fatalf("the same stuffing, state and lines as after the unload give the same digest: %s", seen[d])
	}
	// put the pack back so that unit 1 holds A 6 packs and C 4 packs, then recompute the digest from the canonical text
	step(f.module.RecordCargoLoaded(ctx, f.load("load-c-1", x, revision, "L-C", 1, at)))
	list := f.basis(t).PackingLists[0]
	if len(list.Lines) != 2 || list.Lines[0].ShipmentID != "SH-A" || list.Lines[1].ShipmentID != "SH-C" {
		t.Fatalf("lines: %+v", list.Lines)
	}
	text := fmt.Sprintf("unit 1\nexecution %q\nstate \"LOADING\"\n", x)
	for _, l := range list.Lines {
		text += fmt.Sprintf("line %q %d %q %d %q %d\n", l.ShipmentID, l.LoadedPacks, "KGM", l.LoadedGrossWeight.Amount, "CMQ", l.LoadedVolume.Amount)
	}
	sum := sha256.Sum256([]byte(text))
	if list.Digest != hex.EncodeToString(sum[:]) || list.Lines[0].LoadedPacks != 6 || list.Lines[0].LoadedGrossWeight != kgm(312) ||
		list.Lines[1].LoadedPacks != 4 || list.Lines[1].LoadedGrossWeight != kgm(533) || list.Lines[1].LoadedVolume != cmq(1_333_333) {
		t.Fatalf("digest %s of %+v is not the SHA-256 of %q", list.Digest, list, text)
	}
	// the unit holds again what it held after the load of C (the stuffing, state and lines are the same), so the digest is
	// the same
	if d := digest(); seen[d] != "a load of C" {
		t.Fatalf("the same stuffing, state and lines as after the load of C give its digest: %s", seen[d])
	}
	d := digest()
	ok(t, revision+1)(f.module.CloseLoadExecution(ctx, api.CloseLoadExecution{CommandID: "seal", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: x, ExpectedRevision: revision,
		Evidence: api.Evidence{Source: api.EvidenceSeal, Reference: "SEAL", OccurredAt: at}, OccurredAt: at}))
	if digest() == d {
		t.Fatal("the seal changes the state and so the digest")
	}
}

// TestAPackingListChangesWithoutTheStuffingRevisionMoving shows why the digest exists: a lot measured again after the
// seal, and more packs received of a lot after some were loaded, change what a unit holds as the cost basis reads it
// without moving the stuffing's revision. The digest moves.
func TestAPackingListChangesWithoutTheStuffingRevisionMoving(t *testing.T) {
	ctx := context.Background()
	// A's lot (6 packs, 312 kg and 800,000 cm3) is loaded whole into unit 1 and the container sealed (revision 4: open,
	// start, one load, the seal); A is measured again at 360 kg: the lot's own record moves, the stuffing's does not.
	f := threeHousesInTwoUnits(t)
	at := f.hoursIn(4)
	x := xid(1)
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open", x, 1, at)))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: x, ExpectedRevision: 1, OccurredAt: at}))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load", x, 2, "L-A", 6, at)))
	ok(t, 4)(f.module.CloseLoadExecution(ctx, api.CloseLoadExecution{CommandID: "seal", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: x, ExpectedRevision: 3,
		Evidence: api.Evidence{Source: api.EvidenceSeal, Reference: "SEAL", OccurredAt: at}, OccurredAt: at}))
	before := f.basis(t)
	if u, l := before.LoadUnits[0], before.PackingLists[0]; u.ExecutionRevision != 4 || u.Status != api.UnitStatusClosed || len(l.Lines) != 1 || l.Lines[0].LoadedGrossWeight != kgm(312) {
		t.Fatalf("before: %+v %+v", u, l)
	}
	ok(t, 4)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure-again", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-A",
		ExpectedRevision: 3, GrossWeight: kgm(360), Volume: cmq(800_000), Method: api.MethodScale, Evidence: evidence("measure-again", at), OccurredAt: at}))
	after := f.basis(t)
	if u, l := after.LoadUnits[0], after.PackingLists[0]; l.Lines[0].LoadedGrossWeight != kgm(360) || after.Houses[0].Units[0].LoadedGrossWeight != kgm(360) {
		t.Fatalf("the weight read after the lot is measured again: %+v %+v", u, l)
	} else if u.ExecutionRevision != 4 || u.ExecutionState != api.ExecutionClosed {
		t.Fatalf("the stuffing's record does not move when its lot is measured again: %+v", u)
	}
	if before.PackingLists[0].Digest == after.PackingLists[0].Digest {
		t.Fatal("the digest moves when a lot is measured again")
	}

	// a lot of 4 packs received and weighed at 400 kg but not completed: 2 packs loaded read 2/4 of it, 200 kg; 2 packs
	// more received make it 6 packs and the 2 loaded read 2/6, 133 kg, with the stuffing at the same revision (3)
	g := newFixture(t)
	g.ful.sea["SH-S1"] = true
	g.createSea(t, "C-S", api.KindSeaLCLOwn)
	ok(t, 2)(g.module.PlanLoadUnit(ctx, g.planUnit("plan-1", "C-S", 1, "22G1")))
	if _, err := g.module.AttachHouse(ctx, g.attach("attach", "C-S", "SH-S1", 0)); err != nil {
		t.Fatal(err)
	}
	g.ful.allocate("SH-S1", "C-S")
	ok(t, 3)(g.module.CloseConsolidation(ctx, g.closeCmd("close", "C-S", 2)))
	rcv := g.receive("rcv", "L-R", "SH-S1", 0, 4, api.ConditionGood, at)
	rcv.Facility = cfsShanghai
	ok(t, 1)(g.module.ReceiveCargo(ctx, rcv))
	ok(t, 2)(g.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure", TenantID: g.tenant, ActorID: "ops-1", LotID: "L-R",
		ExpectedRevision: 1, GrossWeight: kgm(400), Volume: cmq(600_000), Method: api.MethodScale, Evidence: evidence("measure", at), OccurredAt: at}))
	ok(t, 1)(g.module.OpenLoadExecution(ctx, g.stuff("open", "X-R", 1, at)))
	ok(t, 2)(g.module.StartLoading(ctx, api.StartLoading{CommandID: "start", TenantID: g.tenant, ActorID: "ops-1", ExecutionID: "X-R", ExpectedRevision: 1, OccurredAt: at}))
	ok(t, 3)(g.module.RecordCargoLoaded(ctx, g.load("load", "X-R", 2, "L-R", 2, at)))
	first := g.basis(t)
	if l := first.PackingLists[0]; len(l.Lines) != 1 || l.Lines[0].LoadedPacks != 2 || l.Lines[0].LoadedGrossWeight != kgm(200) || first.LoadUnits[0].ExecutionRevision != 3 {
		t.Fatalf("2 of 4 packs: %+v, unit %+v", l, first.LoadUnits[0])
	}
	more := g.receive("rcv-more", "L-R", "SH-S1", 2, 2, api.ConditionGood, at)
	more.Facility = cfsShanghai
	ok(t, 3)(g.module.ReceiveCargo(ctx, more))
	second := g.basis(t)
	if l := second.PackingLists[0]; l.Lines[0].LoadedGrossWeight != kgm(133) || second.LoadUnits[0].ExecutionRevision != 3 {
		t.Fatalf("2 of 6 packs: %+v, unit %+v", l, second.LoadUnits[0])
	}
	if first.PackingLists[0].Digest == second.PackingLists[0].Digest {
		t.Fatal("the digest moves when more packs are received of a lot")
	}
}

// TestMixedVolumeUnitsInOneUnitReadZero: two lots of one house in one unit, weighed in the same weight unit but with
// volumes in CMQ and MTQ, make the entry's volume read zero, as the house's total does; the weights add (200 + 100 kg).
func TestMixedVolumeUnitsInOneUnitReadZero(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.ful.sea["SH-S1"] = true
	f.createSea(t, "C-S", api.KindSeaLCLOwn)
	ok(t, 2)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-1", "C-S", 1, "22G1")))
	if _, err := f.module.AttachHouse(ctx, f.attach("attach", "C-S", "SH-S1", 0)); err != nil {
		t.Fatal(err)
	}
	f.ful.allocate("SH-S1", "C-S")
	ok(t, 3)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-S", 2)))
	at := f.hoursIn(4)
	for i, facility := range []api.Facility{cfsShanghai, {Location: unlocode("CNSHG"), Code: "CFS-2"}} {
		lot := fmt.Sprint("L-", i+1)
		rcv := f.receive("rcv-"+lot, lot, "SH-S1", 0, 2, api.ConditionGood, at)
		rcv.Facility = facility
		ok(t, 1)(f.module.ReceiveCargo(ctx, rcv))
		volume := []kernel.Quantity{cmq(500_000), {Unit: "MTQ", Amount: 1}}[i]
		ok(t, 2)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure-" + lot, TenantID: f.tenant, ActorID: "ops-1", LotID: lot,
			ExpectedRevision: 1, GrossWeight: kgm(int64(200 - 100*i)), Volume: volume, Method: api.MethodScale, Evidence: evidence("measure-"+lot, at), OccurredAt: at}))
		ok(t, 3)(f.module.CompleteReceipt(ctx, api.CompleteReceipt{CommandID: "complete-" + lot, TenantID: f.tenant, ActorID: "ops-1", LotID: lot,
			ExpectedRevision: 2, OccurredAt: at}))
	}
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open", "X-1", 1, at)))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 1, OccurredAt: at}))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-1", "X-1", 2, "L-1", 2, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("load-2", "X-1", 3, "L-2", 2, at)))
	h := f.basis(t).Houses[0]
	if len(h.Units) != 1 || h.Units[0].LoadedPacks != 4 || h.Units[0].LoadedGrossWeight != kgm(300) || h.Units[0].LoadedVolume != (kernel.Quantity{}) ||
		h.LoadedVolume != (kernel.Quantity{}) || h.LoadedGrossWeight != kgm(300) {
		t.Fatalf("house = %+v", h)
	}
}
