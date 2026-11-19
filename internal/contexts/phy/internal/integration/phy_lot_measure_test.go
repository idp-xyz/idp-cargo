//go:build integration

package integration

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// splitRun is one house SH-M of a closed own-container consolidation C-S with two planned 22G1 containers, and its
// one lot L-M of 6 cartons received at the CFS, weighed and completed, which the tests load into the containers.
type splitRun struct {
	t   *testing.T
	f   *fixture
	rev map[string]int64 // the revision of each stuffing's record
	lot int64            // the lot's revision
	n   int
}

type figure struct{ packs, kilos, cm3 int64 }

func newSplitRun(t *testing.T, kilos, cm3 int64) *splitRun {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	f.ful.sea["SH-M"] = true
	f.createSea(t, "C-S", api.KindSeaLCLOwn)
	ok(t, 2)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-1", "C-S", 1, "22G1")))
	ok(t, 3)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-2", "C-S", 2, "22G1")))
	if _, err := f.module.AttachHouse(ctx, f.attach("attach", "C-S", "SH-M", 0)); err != nil {
		t.Fatal(err)
	}
	f.ful.allocate("SH-M", "C-S")
	ok(t, 4)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-S", 3)))
	at := f.hoursIn(3)
	rcv := f.receive("rcv", "L-M", "SH-M", 0, 6, api.ConditionGood, at)
	rcv.Facility = cfsShanghai
	ok(t, 1)(f.module.ReceiveCargo(ctx, rcv))
	ok(t, 2)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-M",
		ExpectedRevision: 1, GrossWeight: kgm(kilos), Volume: cmq(cm3), Method: api.MethodScale, Evidence: evidence("measure", at), OccurredAt: at}))
	ok(t, 3)(f.module.CompleteReceipt(ctx, api.CompleteReceipt{CommandID: "complete", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-M",
		ExpectedRevision: 2, OccurredAt: at}))
	return &splitRun{t: t, f: f, rev: map[string]int64{}, lot: 3}
}

func (s *splitRun) id(what string) string {
	s.n++
	return fmt.Sprint(what, "-", s.n)
}

// open opens the stuffing of the unit and starts loading it.
func (s *splitRun) open(x string, unit int) {
	s.t.Helper()
	ctx, at := context.Background(), s.f.hoursIn(4)
	ok(s.t, 1)(s.f.module.OpenLoadExecution(ctx, s.f.stuff(s.id("open"), x, unit, at)))
	ok(s.t, 2)(s.f.module.StartLoading(ctx, api.StartLoading{CommandID: s.id("start"), TenantID: s.f.tenant, ActorID: "ops-1", ExecutionID: x,
		ExpectedRevision: 1, OccurredAt: at}))
	s.rev[x] = 2
}

// load records packs of the lot in the stuffing, weighed for themselves when kilos is given; it returns the line.
func (s *splitRun) load(x string, packs, kilos, cm3 int64) int {
	s.t.Helper()
	cmd := s.f.load(s.id("load"), x, s.rev[x], "L-M", packs, s.f.hoursIn(4))
	if kilos > 0 {
		cmd.GrossWeight, cmd.Volume = kgm(kilos), cmq(cm3)
	}
	s.rev[x]++
	return ok(s.t, s.rev[x])(s.f.module.RecordCargoLoaded(context.Background(), cmd)).ItemNo
}

func (s *splitRun) unload(x string, line int, packs int64) {
	s.t.Helper()
	at := s.f.hoursIn(4)
	s.rev[x]++
	ok(s.t, s.rev[x])(s.f.module.RecordCargoUnloaded(context.Background(), api.RecordCargoUnloaded{CommandID: s.id("unload"), TenantID: s.f.tenant,
		ActorID: "ops-1", ExecutionID: x, ExpectedRevision: s.rev[x] - 1, LineNo: line, Packs: packs, Reason: api.ReversalMisloaded,
		Evidence: evidence(s.id("tally"), at), OccurredAt: at}))
}

func (s *splitRun) seal(x string) {
	s.t.Helper()
	at := s.f.hoursIn(4)
	s.rev[x]++
	ok(s.t, s.rev[x])(s.f.module.CloseLoadExecution(context.Background(), api.CloseLoadExecution{CommandID: s.id("seal"), TenantID: s.f.tenant, ActorID: "ops-1",
		ExecutionID: x, ExpectedRevision: s.rev[x] - 1, Evidence: api.Evidence{Source: api.EvidenceSeal, Reference: s.id("SEAL"), OccurredAt: at}, OccurredAt: at}))
}

// expect checks what the house is loaded in the containers as the cost basis reads it (the house's totals, its entry in
// each unit and the lines of the units' packing lists) and as the shipment's physical facts read it (each stuffing's
// loaded packs, weight and volume): units says for each unit with packs of the house its packs, kilos and cubic
// centimetres, and house is the total. The figures of the three must be one.
func (s *splitRun) expect(name string, house figure, units map[int]figure) {
	s.t.Helper()
	ctx := context.Background()
	got := func(packs int64, w, v kernel.Quantity) figure { return figure{packs, w.Amount, v.Amount} }
	b := s.f.basis(s.t)
	var h api.HouseCostBasis
	for _, x := range b.Houses {
		if x.ShipmentID == "SH-M" {
			h = x
		}
	}
	if g := got(h.LoadedPacks, h.LoadedGrossWeight, h.LoadedVolume); g != house {
		s.t.Errorf("%s: the house reads %+v, want %+v", name, g, house)
	}
	entries := map[int]figure{}
	for _, u := range h.Units {
		entries[u.UnitNo] = got(u.LoadedPacks, u.LoadedGrossWeight, u.LoadedVolume)
	}
	if !reflect.DeepEqual(entries, units) {
		s.t.Errorf("%s: the house's entries read %+v, want %+v", name, entries, units)
	}
	var sum figure
	for _, e := range entries {
		sum = figure{sum.packs + e.packs, sum.kilos + e.kilos, sum.cm3 + e.cm3}
	}
	if sum != house {
		s.t.Errorf("%s: the entries add up to %+v, the house is %+v", name, sum, house)
	}
	lines := map[int]figure{}
	for _, l := range b.PackingLists {
		for _, x := range l.Lines {
			if x.ShipmentID == "SH-M" {
				lines[l.UnitNo] = got(x.LoadedPacks, x.LoadedGrossWeight, x.LoadedVolume)
			}
		}
	}
	if !reflect.DeepEqual(lines, units) {
		s.t.Errorf("%s: the packing lists' lines read %+v, want the entries %+v", name, lines, units)
	}
	e, err := s.f.module.GetPhysicalLoadEvidence(ctx, api.GetPhysicalLoadEvidence{TenantID: s.f.tenant, ShipmentID: "SH-M"})
	if err != nil {
		s.t.Fatalf("%s: physical facts: %v", name, err)
	}
	tenders := map[int]figure{}
	for _, t := range e.Tenders {
		if t.LoadedPacks > 0 {
			tenders[t.LoadUnitNo] = got(t.LoadedPacks, t.GrossWeight, t.Volume)
		}
	}
	if !reflect.DeepEqual(tenders, units) {
		s.t.Errorf("%s: the shipment's physical facts read %+v, want the same as the cost basis, %+v", name, tenders, units)
	}
}

// TestALotsMeasureOverTwoContainersOnPostgres runs the two cases of the review of V2.79 with the real commands.
//
// A 600 kg lot of 6 packs, 1,000 cm3: 2 packs weighed for themselves at 300 kg and 400 cm3 in container 1, the other 4
// apportioned in container 2. V2.81 read container 2 at 600 x 4/6 = 400 kg and 1,000 x 4/6 = 666 cm3, and the house at
// 700 kg and 1,066 cm3 of a lot of 600 kg and 1,000 cm3. The lot less the weighed packs is 300 kg and 600 cm3 for the 4
// remaining packs, which complete it: 300 kg and 600 cm3, and 300 + 300 kg, 400 + 600 cm3.
//
// An 800 kg lot of 6 packs, 1,000 cm3, all apportioned, 4 packs in container 1 and 2 in container 2: V2.81 read 533 and
// 266 kg and 666 and 333 cm3, the house at 799 kg and 999 cm3. The weight is 533.33 and 266.67 kg, floors 533
// and 266, one
// kg left for the larger remainder, the 2 packs' (.67): 533 + 267 kg. The volume is 666.67 and 333.33 cm3, the
// one cm3 left
// goes to the 4 packs' (.67): 667 + 333 cm3. Then, with all figures for 6 packs of 800 kg and 1,000 cm3:
//   - 2 of container 1's 4 packs unloaded: 4 of 6 packs live take 800 x 4/6 = 533 kg (rounded down), 266.5 each, floors
//     266 + 266, the kg left to the lower unit number on equal remainders (container 1): 267 + 266; 1,000 x
//     4/6 = 666 cm3,
//     333 each;
//   - those 2 packs loaded into container 2: all 6 live again, container 1 holds 2 packs, container 2 holds 4:
//     800 x 2/6
//     = 266.67 and 533.33: 267 + 533 kg; 1,000 x 2/6 = 333.33 and 666.67: 333 + 667 cm3;
//   - both containers sealed and the lot weighed again at 1,000 kg and 1,500 cm3: 333.33 + 666.67: 333 + 667 kg and
//     500 + 1,000 cm3, which still add up to the lot, the stuffings' records not having moved.
func TestALotsMeasureOverTwoContainersOnPostgres(t *testing.T) {
	ctx := context.Background()

	s := newSplitRun(t, 600, 1000)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.load("X-1", 2, 300, 400)
	s.load("X-2", 4, 0, 0)
	s.expect("600 kg: 2 weighed + 4 apportioned", figure{6, 600, 1000}, map[int]figure{1: {2, 300, 400}, 2: {4, 300, 600}})

	s = newSplitRun(t, 800, 1000)
	s.open("X-1", 1)
	s.open("X-2", 2)
	line := s.load("X-1", 4, 0, 0)
	s.expect("800 kg: 4 packs in container 1", figure{4, 533, 666}, map[int]figure{1: {4, 533, 666}})
	s.load("X-2", 2, 0, 0)
	s.expect("800 kg: 4 + 2 packs", figure{6, 800, 1000}, map[int]figure{1: {4, 533, 667}, 2: {2, 267, 333}})

	s.unload("X-1", line, 2)
	s.expect("800 kg: 2 of container 1's packs unloaded", figure{4, 533, 666}, map[int]figure{1: {2, 267, 333}, 2: {2, 266, 333}})
	s.load("X-2", 2, 0, 0)
	s.expect("800 kg: the 2 packs loaded into container 2", figure{6, 800, 1000}, map[int]figure{1: {2, 267, 333}, 2: {4, 533, 667}})

	s.seal("X-1")
	s.seal("X-2")
	before := s.f.basis(t)
	ok(t, 4)(s.f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure-again", TenantID: s.f.tenant, ActorID: "ops-1",
		LotID: "L-M", ExpectedRevision: 3, GrossWeight: kgm(1000), Volume: cmq(1500), Method: api.MethodScale, Evidence: evidence("measure-again", s.f.hoursIn(4)),
		OccurredAt: s.f.hoursIn(4)}))
	s.expect("1,000 kg after the seals", figure{6, 1000, 1500}, map[int]figure{1: {2, 333, 500}, 2: {4, 667, 1000}})
	after := s.f.basis(t)
	for i, u := range after.LoadUnits {
		if u.Status != api.UnitStatusClosed || u.ExecutionRevision != s.rev[fmt.Sprint("X-", i+1)] {
			t.Errorf("unit %d once sealed and the lot weighed again: %+v", i+1, u)
		}
	}
	if before.PackingLists[0].Digest == after.PackingLists[0].Digest || before.PackingLists[1].Digest == after.PackingLists[1].Digest {
		t.Error("both packing lists' digests move when the lot is weighed again")
	}
}

// TestOneExecutionPerLotReadsTheFiguresOfBeforeOnPostgres is the proof of a lot in one execution, one level above
// the pure test against the V2.81 function: in the day of TestTheCostBasisShowsEachHouseInEachUnit (A 312 kg and
// 800,000 cm3, B 288 kg and 200,000 cm3, C 800 kg and 2,000,000 cm3, 6 packs each) every lot is wholly in one
// container, A and C in unit 1 and B in unit 2. The cost basis (the houses' totals, their entries and the packing
// lists' lines) and the shipment's physical facts answer, for every house, exactly the lot's figures that the
// existing tests assert for whole lots.
func TestOneExecutionPerLotReadsTheFiguresOfBeforeOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := threeHousesInTwoUnits(t)
	at := f.hoursIn(4)
	for unit := 1; unit <= 2; unit++ {
		x := xid(unit)
		ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-"+x, x, unit, at)))
		ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-" + x, TenantID: f.tenant, ActorID: "ops-1", ExecutionID: x,
			ExpectedRevision: 1, OccurredAt: at}))
	}
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-a", xid(1), 2, "L-A", 6, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("load-c", xid(1), 3, "L-C", 6, at)))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-b", xid(2), 2, "L-B", 6, at)))
	want := map[string]struct {
		unit int
		figure
	}{"SH-A": {1, figure{6, 312, 800_000}}, "SH-B": {2, figure{6, 288, 200_000}}, "SH-C": {1, figure{6, 800, 2_000_000}}}
	b := f.basis(t)
	for _, h := range b.Houses {
		w := want[h.ShipmentID]
		got := figure{h.LoadedPacks, h.LoadedGrossWeight.Amount, h.LoadedVolume.Amount}
		if got != w.figure || len(h.Units) != 1 || h.Units[0].UnitNo != w.unit ||
			(figure{h.Units[0].LoadedPacks, h.Units[0].LoadedGrossWeight.Amount, h.Units[0].LoadedVolume.Amount}) != w.figure {
			t.Errorf("%s: cost basis %+v entries %+v, want %+v in unit %d", h.ShipmentID, got, h.Units, w.figure, w.unit)
		}
		e, err := f.module.GetPhysicalLoadEvidence(ctx, api.GetPhysicalLoadEvidence{TenantID: f.tenant, ShipmentID: h.ShipmentID})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, td := range e.Tenders {
			if td.LoadedPacks > 0 {
				n++
				if (figure{td.LoadedPacks, td.GrossWeight.Amount, td.Volume.Amount}) != w.figure || td.LoadUnitNo != w.unit {
					t.Errorf("%s: physical facts %+v in unit %d, want %+v in unit %d", h.ShipmentID, td, td.LoadUnitNo, w.figure, w.unit)
				}
			}
		}
		if n != 1 {
			t.Errorf("%s: %d executions with packs in the physical facts", h.ShipmentID, n)
		}
	}
	for _, l := range b.PackingLists {
		for _, x := range l.Lines {
			if w := want[x.ShipmentID]; w.unit != l.UnitNo || (figure{x.LoadedPacks, x.LoadedGrossWeight.Amount, x.LoadedVolume.Amount}) != w.figure {
				t.Errorf("packing list of unit %d: %+v", l.UnitNo, x)
			}
		}
	}
}

// TestASealedContainersFigureMovesWhenAnotherContainerLoads pins what the allotment makes of "sealed": a container's
// figure for a lot is its part of the lot's measure as the lot is loaded now. A lot of 6 packs at 301 kg and 600 cm3, 5
// packs in container 1, which is sealed: 301 x 5/6 = 250.83, so 250 kg. When container 2 takes the sixth pack the parts
// are 250.83 and 50.17, the floors 250 + 50 leave 1 kg for the larger remainder (.83) and the sealed container reads
// 251
// kg, with the packing list's digest changed and the stuffing's revision where the seal left it. A later change of this
// is a decision, not an accident.
func TestASealedContainersFigureMovesWhenAnotherContainerLoads(t *testing.T) {
	s := newSplitRun(t, 301, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.load("X-1", 5, 0, 0)
	s.seal("X-1")
	s.expect("5 of 6 packs in the sealed container", figure{5, 250, 500}, map[int]figure{1: {5, 250, 500}})
	before := s.f.basis(t)
	s.load("X-2", 1, 0, 0)
	s.expect("the sixth pack in container 2", figure{6, 301, 600}, map[int]figure{1: {5, 251, 500}, 2: {1, 50, 100}})
	after := s.f.basis(t)
	if u := after.LoadUnits[0]; u.Status != api.UnitStatusClosed || u.ExecutionRevision != s.rev["X-1"] || u.ExecutionRevision != before.LoadUnits[0].ExecutionRevision {
		t.Fatalf("the sealed container's record did not move: %+v", u)
	}
	if before.PackingLists[0].Digest == after.PackingLists[0].Digest {
		t.Fatal("the sealed container's packing list digest moves with its figure")
	}
}
