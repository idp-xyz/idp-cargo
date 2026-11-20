//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	phymodule "go.idp.xyz/idp-cargo/internal/contexts/phy"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// finGrams is a COPY of the conversion FIN applies to a weight it reads from PHY's cost basis, fin.Grams in
// internal/contexts/fin/internal/domain/pool.go (internal to FIN, so not importable from here): a change in FIN's is
// not seen by this test. FIN's adapter, internal/contexts/fin/internal/adapters/phy, passes LoadedGrossWeight of the
// house and of its units on unchanged. A zero amount is zero grams, GRM, KGM and TNE are 1, 1,000 and 1,000,000 grams,
// any other unit is refused. PHY itself also converts LBR (pounds), which FIN refuses: a house whose lines are all in
// LBR is published in LBR and FIN refuses it, while a house with LBR among other units is published in GRM, which FIN
// reads.
func finGrams(q kernel.Quantity) (int64, error) {
	switch {
	case q.Amount == 0:
		return 0, nil
	case q.Amount < 0:
		return 0, errors.New("negative weight")
	}
	switch q.Unit {
	case "GRM":
		return q.Amount, nil
	case "KGM":
		return q.Amount * 1000, nil
	case "TNE":
		return q.Amount * 1000000, nil
	}
	return 0, errors.New("weight in " + string(q.Unit))
}

// TestAHouseInTwoContainersInTwoWeightUnitsHasATotalInGrams: the house's lot L-A (3 packs, 3,000 KGM, under a plan in
// KGM) is stuffed into container 1 and its lot L-B (3 packs, 3 TNE, under a plan in TNE) into container 2 (the same
// stand-in for FUL as in TestAHouseWithLinesInTwoWeightUnitsIsPublishedInGrams, and for the same reason). Each
// container's entry is in its own single unit and unchanged (3,000 KGM and 3 TNE); the house's total, which V2.83 read
// as zero because the two entries' units differ, is 3,000,000 + 3,000,000 = 6,000,000 GRM, and FIN's conversion of it
// is 6,000,000 g, the sum of the conversions of the two entries.
func TestAHouseInTwoContainersInTwoWeightUnitsHasATotalInGrams(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	u := &unitFUL{fulStub: f.ful}
	module, err := phymodule.New(f.db, u, policy{}, phymodule.WithLocations(locationStub{}), phymodule.WithEquipment(equipmentStub{}))
	if err != nil {
		t.Fatal(err)
	}
	f.module = module
	f.ful.sea["SH-M"] = true
	f.createSea(t, "C-S", api.KindSeaLCLOwn)
	ok(t, 2)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-1", "C-S", 1, "22G1")))
	ok(t, 3)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-2", "C-S", 2, "22G1")))
	if _, err = f.module.AttachHouse(ctx, f.attach("attach", "C-S", "SH-M", 0)); err != nil {
		t.Fatal(err)
	}
	f.ful.allocate("SH-M", "C-S")
	ok(t, 4)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-S", 3)))
	s := &splitRun{t: t, f: f, rev: map[string]int64{}, lot: 2}
	at := f.hoursIn(3)
	for _, lot := range []struct {
		id       string
		unit     kernel.UnitCode
		amount   int64
		facility api.Facility
	}{{"L-A", "KGM", 3000, cfsShanghai}, {"L-B", "TNE", 3, cfsTwo}} {
		u.set(lot.unit)
		rcv := f.receive("rcv-"+lot.id, lot.id, "SH-M", 0, 3, api.ConditionGood, at)
		rcv.Facility = lot.facility
		ok(t, 1)(f.module.ReceiveCargo(ctx, rcv))
		ok(t, 2)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure-" + lot.id, TenantID: f.tenant, ActorID: "ops-1", LotID: lot.id,
			ExpectedRevision: 1, GrossWeight: kernel.Quantity{Unit: lot.unit, Amount: lot.amount}, Method: api.MethodScale, Evidence: evidence("measure-"+lot.id, at), OccurredAt: at}))
	}
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.loadAs("ops-1", "X-1", "L-A", 3, 0, 0, 4)
	s.loadAs("ops-1", "X-2", "L-B", 3, 0, 0, 4)
	var house api.HouseCostBasis
	for _, h := range f.basis(t).Houses {
		if h.ShipmentID == "SH-M" {
			house = h
		}
	}
	if len(house.Units) != 2 || house.Units[0].LoadedGrossWeight != (kernel.Quantity{Unit: "KGM", Amount: 3000}) ||
		house.Units[1].LoadedGrossWeight != (kernel.Quantity{Unit: "TNE", Amount: 3}) {
		t.Fatalf("the entries of the house: %+v; want 3,000 KGM and 3 TNE, each in its own unit", house.Units)
	}
	if house.LoadedGrossWeight != (kernel.Quantity{Unit: "GRM", Amount: 6000000}) {
		t.Errorf("the house's total is %+v, want 6,000,000 GRM", house.LoadedGrossWeight)
	}
	var sum int64
	for _, e := range house.Units {
		g, err := finGrams(e.LoadedGrossWeight)
		if err != nil {
			t.Fatal(err)
		}
		sum += g
	}
	if total, err := finGrams(house.LoadedGrossWeight); err != nil || total != sum || total != 6000000 {
		t.Errorf("FIN reads the total as %d g (%v), the entries' conversions add up to %d g; want 6,000,000 both", total, err, sum)
	}
}

// TestVolumesInDifferentUnitsAreConvertedThroughTheRealCommands runs PHY's real commands on PostgreSQL; only FUL is a
// stand-in (fulStub), which declares a plan with no volume, as the real COM and FUL chain does (the reviewer of V2.84
// ran that chain and got the case below from it; the same chain is run again in this version, see the README). With no
// volume in the plan nothing makes a line's own volume agree with its lot's measure. House SH-M plans 6 packs and 300
// KGM; lot L-M of its 6 packs is measured at 300 KGM and the volume of the case in CMQ; 2 packs are loaded measured for
// themselves at 100 KGM and 1 MTQ and the other 4 apportioned. By hand, in cm3: 1 MTQ is 1,000,000 cm3. With a lot of
// 2,000,000 CMQ the 4 packs take 2,000,000 - 1,000,000 = 1,000,000 and the house is 1,000,000 + 1,000,000 = 2,000,000
// CMQ; with the reviewer's lot of 600 CMQ the line's 1,000,000 cm3 is more than the lot's measure and nothing is left,
// so the house is 1,000,000 CMQ (V2.83 subtracted 1 from 600, read 1,000,599 cm3 in the limits and no volume for the
// house). The weight is 100 + 200 = 300 KGM in both. The published entry, the house's total, the packing-list line, the
// shipment's physical facts and the limits check all read that one figure, in cubic centimetres.
func TestVolumesInDifferentUnitsAreConvertedThroughTheRealCommands(t *testing.T) {
	for name, tc := range map[string]struct {
		lot, own kernel.Quantity
		want     int64
	}{
		"a lot of 2,000,000 CMQ":        {cmq(2000000), kernel.Quantity{Unit: "MTQ", Amount: 1}, 2000000},
		"the reviewer's lot of 600 CMQ": {cmq(600), kernel.Quantity{Unit: "MTQ", Amount: 1}, 1000000},
		// a lot of 2 MTQ with 500,001 CMQ taken by the line: the 4 packs take 2,000,000 - 500,001 = 1,499,999 cm3
		"a lot of 2 MTQ and a line in CMQ": {kernel.Quantity{Unit: "MTQ", Amount: 2}, cmq(500001), 2000000},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			f.ful.sea["SH-M"] = true
			f.createSea(t, "C-S", api.KindSeaLCLOwn)
			ok(t, 2)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-1", "C-S", 1, "22G1")))
			if _, err := f.module.AttachHouse(ctx, f.attach("attach", "C-S", "SH-M", 0)); err != nil {
				t.Fatal(err)
			}
			f.ful.allocate("SH-M", "C-S")
			ok(t, 3)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-S", 2)))
			at := f.hoursIn(3)
			rcv := f.receive("rcv", "L-M", "SH-M", 0, 6, api.ConditionGood, at)
			rcv.Facility = cfsShanghai
			ok(t, 1)(f.module.ReceiveCargo(ctx, rcv))
			ok(t, 2)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-M",
				ExpectedRevision: 1, GrossWeight: kgm(300), Volume: tc.lot, Method: api.MethodScale, Evidence: evidence("measure", at), OccurredAt: at}))
			ok(t, 3)(f.module.CompleteReceipt(ctx, api.CompleteReceipt{CommandID: "complete", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-M", ExpectedRevision: 2, OccurredAt: at}))
			s := &splitRun{t: t, f: f, rev: map[string]int64{}, lot: 3}
			s.open("X-1", 1)
			own := f.load("load-own", "X-1", s.rev["X-1"], "L-M", 2, f.hoursIn(4))
			own.GrossWeight, own.Volume = kgm(100), tc.own
			ok(t, 3)(f.module.RecordCargoLoaded(ctx, own))
			ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("load-rest", "X-1", 3, "L-M", 4, f.hoursIn(4))))
			want, weight := cmq(tc.want), kgm(300)
			b := f.basis(t)
			var house api.HouseCostBasis
			for _, h := range b.Houses {
				if h.ShipmentID == "SH-M" {
					house = h
				}
			}
			list := b.PackingLists[0]
			if len(house.Units) != 1 || house.Units[0].LoadedVolume != want || house.Units[0].LoadedGrossWeight != weight || house.LoadedVolume != want ||
				house.LoadedGrossWeight != weight {
				t.Errorf("the house: total %+v %+v, entry %+v; want %+v and %+v", house.LoadedGrossWeight, house.LoadedVolume, house.Units, weight, want)
			}
			if len(list.Lines) != 1 || list.Lines[0].LoadedVolume != want || list.Lines[0].LoadedGrossWeight != weight || list.Digest != digestOf(list) {
				t.Errorf("the packing list's line: %+v (digest %s, worked out %s), want %+v", list.Lines, list.Digest, digestOf(list), want)
			}
			evidenceOf, err := f.module.GetPhysicalLoadEvidence(ctx, api.GetPhysicalLoadEvidence{TenantID: f.tenant, ShipmentID: "SH-M"})
			if err != nil || len(evidenceOf.Tenders) != 1 || evidenceOf.Tenders[0].Volume != want || evidenceOf.Tenders[0].GrossWeight != weight {
				t.Errorf("the physical facts: %+v, %v; want %+v", evidenceOf.Tenders, err, want)
			}
			if l := list.Limits; !l.Checked || l.CargoGrams != 300000 || l.CargoCubicCm != tc.want {
				t.Errorf("Limits %+v; want 300,000 g and %d cm3, the one figure of the entry", l, tc.want)
			}
		})
	}
}

// TestAHouseInTwoContainersHasATotalVolumeInCubicCentimetres runs PHY's real commands (only FUL's plan is a stand-in,
// with no volume as in the real chain). House SH-M's lot L-M of 6 packs is measured at 300 KGM and 2,000,000 CMQ;
// container 1 takes 2 packs measured for themselves at 100 KGM and 1 MTQ, container 2 the other 4 apportioned. The
// entries are in their own units, 1 MTQ and 2,000,000 - 1,000,000 = 1,000,000 CMQ; the house's total, which read no
// volume while the entries' units differed, is 1,000,000 + 1,000,000 = 2,000,000 CMQ, the packing lists' lines are the
// entries, and the limits of the two containers add up to the same 2,000,000 cm3.
func TestAHouseInTwoContainersHasATotalVolumeInCubicCentimetres(t *testing.T) {
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
		ExpectedRevision: 1, GrossWeight: kgm(300), Volume: cmq(2000000), Method: api.MethodScale, Evidence: evidence("measure", at), OccurredAt: at}))
	ok(t, 3)(f.module.CompleteReceipt(ctx, api.CompleteReceipt{CommandID: "complete", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-M", ExpectedRevision: 2, OccurredAt: at}))
	s := &splitRun{t: t, f: f, rev: map[string]int64{}, lot: 3}
	s.open("X-1", 1)
	s.open("X-2", 2)
	own := f.load("load-own", "X-1", s.rev["X-1"], "L-M", 2, f.hoursIn(4))
	own.GrossWeight, own.Volume = kgm(100), kernel.Quantity{Unit: "MTQ", Amount: 1}
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, own))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-rest", "X-2", s.rev["X-2"], "L-M", 4, f.hoursIn(4))))
	b := f.basis(t)
	var house api.HouseCostBasis
	for _, h := range b.Houses {
		if h.ShipmentID == "SH-M" {
			house = h
		}
	}
	if len(house.Units) != 2 || house.Units[0].LoadedVolume != (kernel.Quantity{Unit: "MTQ", Amount: 1}) || house.Units[1].LoadedVolume != cmq(1000000) ||
		house.LoadedVolume != cmq(2000000) || house.LoadedGrossWeight != kgm(300) {
		t.Errorf("the house: total %+v %+v, entries %+v; want 1 MTQ and 1,000,000 CMQ, 2,000,000 CMQ and 300 KGM", house.LoadedGrossWeight, house.LoadedVolume, house.Units)
	}
	var cubic int64
	for _, l := range b.PackingLists {
		cubic += l.Limits.CargoCubicCm
	}
	if cubic != 2000000 {
		t.Errorf("the limits of the two containers add up to %d cm3, want 2,000,000", cubic)
	}
}
