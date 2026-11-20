//go:build integration

package bootstrap

import (
	"strings"
	"testing"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulmodule "go.idp.xyz/idp-cargo/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// A sealed container is opened for rework, a whole house is unloaded and the container is sealed again (PHY V2.85),
// with the real ORG, RATE, COM, FUL, PHY and FIN. The owner's day: three houses H01, H02 and H03 of 10, 15 and 5 W/M in
// one 22G1, H03 leaves. FIN's apportionment of three cost lines (OCEAN_FREIGHT by CHARGEABLE, THC by TEU, LASHING by
// GROSS_WEIGHT, each CNY 100.01) is asked before the rework, during it and after, and the test asserts what today's
// code gives. It is not a statement of what FIN should do: FIN is not changed in this step. The later steps will
// change, in FIN, what is asserted as "during" here: an apportionment is held back while a container is in rework
// (today only the container methods refuse, since they need every container sealed; CHARGEABLE and GROSS_WEIGHT read
// the loaded facts and go on), the confirmation of the held back apportionment needs a person other than the one who
// asked for the rework (UnitPackingList.Reworks.AskedBy, PHY), and the container's costs are attributed to the houses
// again after the reseal, so that "after" would then not need the house that left to be named as not used. FUL and SEA,
// whose view of the container follows PHY's CLOSED executions, follow the new seal and the contents in their own steps.
func TestAContainerIsReworkedWithEveryContextAndFINApportionsAsTodayAssert(t *testing.T) {
	d := newOceanDay(t, "cargo-ocean-rework-", map[string]int64{"H01": 312, "H02": 300, "H03": 288},
		map[string]int64{"H01": 10_000_000, "H02": 15_000_000, "H03": 5_000_000}, []string{"22G1"},
		[]struct{ code, method string }{{"OCEAN_FREIGHT", finapi.MethodChargeable}, {"THC", finapi.MethodTEU}, {"LASHING", finapi.MethodGrossWeight}})
	d.stuff("X-1", 1)
	for _, h := range []string{"H01", "H02", "H03"} {
		d.load("X-1", h, 6, 0)
	}
	d.seal("X-1") // SEAL-X-1; the execution is at revision 6
	_, err := d.phy.CloseCargoLot(d.ctx, phyapi.CloseCargoLot{CommandID: "close-lot-h03", TenantID: d.tenant, ActorID: "cfs-1", LotID: "L-H03", ExpectedRevision: 3, OccurredAt: d.hours(16)})
	d.must("close the lot of H03", err)

	shares := func(label string, a finapi.PoolApportionment, line int, want ...int64) {
		t.Helper()
		if len(a.Lines) < line {
			t.Fatalf("%s: apportionment %+v", label, a)
		}
		got := a.Lines[line-1].Shares
		if len(got) != len(want) {
			t.Fatalf("%s: %d shares %+v, want %v", label, len(got), got, want)
		}
		for i, w := range want {
			if got[i].Amount != (kernel.Money{Currency: "CNY", Minor: w}) {
				t.Fatalf("%s: shares %+v, want %v", label, got, want)
			}
		}
	}
	last := func() finapi.PoolApportionment {
		t.Helper()
		ap := d.pool().Apportionments
		return ap[len(ap)-1]
	}
	// apportion applies the lines asked, and leaves the houses named out of them
	apportion := func(id string, notUsed []string, lines ...int) error {
		t.Helper()
		_, err := d.fin.ApportionConsolCosts(d.ctx, finapi.ApportionConsolCosts{CommandID: id, TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1",
			ExpectedRevision: d.pool().Revision, LineNos: lines, NotUsed: notUsed, OccurredAt: d.hours(30)})
		return err
	}

	// BEFORE: sealed. Chargeable on W/M 10 : 15 : 5 (10,001 x 10/30 = 3,333.67 ...: the two minor units left go to the
	// larger remainders, H03's .83 and H01's .67), the TEU on one container each, the weight on 312 : 300 : 288 kg
	// (10,001 x 312/900 = 3,467.01 and so on: the unit left goes to H02's .67).
	before := d.basis()
	if before.PackingLists[0].ExecutionState != phyapi.ExecutionClosed || before.LoadUnits[0].Status != phyapi.UnitStatusClosed || len(before.PackingLists[0].Lines) != 3 {
		t.Fatalf("before: %+v", before.PackingLists)
	}
	if err = apportion("fin-before", nil, 1, 2, 3); err != nil {
		t.Fatalf("apportion before: %v", err)
	}
	a := last()
	if a.Stage != finapi.BasisActual {
		t.Fatalf("before: %+v", a)
	}
	shares("before, W/M", a, 1, 3_334, 5_000, 1_667)
	shares("before, TEU", a, 2, 3_334, 3_334, 3_333)
	shares("before, weight", a, 3, 3_467, 3_334, 3_200)

	ful := fulOf(t)
	// FUL takes the sealed container: the execution, the unit, the seal and the house's lines
	for h, line := range map[string]int{"H01": 1, "H03": 3} {
		plan, err := takes(t, d, ful, "ful-before-"+h, "SH-"+h)
		if err != nil || plan.MeasuredBasis != fulapi.MeasuredTendered || len(plan.Stuffings) != 1 || plan.Stuffings[0].ExecutionID != "X-1" || plan.Stuffings[0].Seal != "SEAL-X-1" ||
			plan.Stuffings[0].Packs != 6 || len(plan.Stuffings[0].LineNos) != 1 || plan.Stuffings[0].LineNos[0] != line {
			t.Fatalf("FUL before the rework, %s: %v %+v", h, err, plan)
		}
	}

	// OPENED: the same facts, the container not sealed. PHY reads the unit IN_USE and the packing list REWORK; FIN's
	// container methods refuse at once (they need every container sealed), CHARGEABLE and GROSS_WEIGHT read the loaded
	// facts, which have not moved, and give the same shares. FIN does not know the container is open.
	cmd := phyapi.OpenStuffingForRework{CommandID: "rework-1", TenantID: d.tenant, ActorID: "ops-asker", ExecutionID: "X-1", ExpectedRevision: d.revision["X-1"],
		Reason: "the consignee of H03 withdrew its cargo", OccurredAt: d.hours(22)}
	if _, err = d.phy.OpenStuffingForRework(d.ctx, cmd); err != nil {
		t.Fatal(err)
	}
	d.revision["X-1"]++
	during := d.basis()
	if p := during.PackingLists[0]; p.ExecutionState != phyapi.ExecutionRework || len(p.Lines) != 3 || p.Digest == before.PackingLists[0].Digest || len(p.Reworks) != 1 ||
		p.Reworks[0].AskedBy != "ops-asker" || during.LoadUnits[0].Status != phyapi.UnitStatusInUse || during.LoadUnits[0].ExecutionState != phyapi.ExecutionRework {
		t.Fatalf("during: %+v, unit %+v", during.PackingLists, during.LoadUnits[0])
	}
	err = apportion("fin-open-teu", nil, 2)
	if err == nil || !strings.Contains(err.Error(), "FIN_POOL_NO_BASIS") || !strings.Contains(err.Error(), "load unit 1 is IN_USE") {
		t.Fatalf("the container method while the container is in rework: %v", err)
	}
	if err = apportion("fin-open-1", nil, 1, 3); err != nil {
		t.Fatalf("apportion while the container is open: %v", err)
	}
	a = last()
	if a.Stage != finapi.BasisActual {
		t.Fatalf("while open: %+v", a)
	}
	shares("open, W/M", a, 1, 3_334, 5_000, 1_667)
	shares("open, weight", a, 2, 3_467, 3_334, 3_200) // lines 1 and 3 of the pool are the two lines of this apportionment

	// H03 UNLOADED: it is attached with nothing loaded. With no house named out, FIN reads the stage PLANNED (a house
	// that takes part is not loaded) and divides on the plan, H03 among the houses; the container methods are refused
	// for the stage; naming H03 out gives ACTUAL on the two houses in the container.
	unload := phyapi.UnloadHouseForRework{CommandID: "unload-h03", TenantID: d.tenant, ActorID: "wh-1", ExecutionID: "X-1", ExpectedRevision: d.revision["X-1"], ShipmentID: "SH-H03",
		Evidence: phyapi.Evidence{Source: "CFS-WMS", Reference: "UNSTUFF-H03", OccurredAt: d.hours(24)}, OccurredAt: d.hours(24)}
	r, err := d.phy.UnloadHouseForRework(d.ctx, unload)
	if err != nil || r.ItemNo != 1 {
		t.Fatalf("unload H03: %+v %v", r, err)
	}
	d.revision["X-1"]++
	out := d.basis()
	h03 := out.Houses[2]
	if h03.ShipmentID != "SH-H03" || h03.LoadedPacks != 0 || len(h03.Units) != 0 || out.PackingLists[0].ExecutionState != phyapi.ExecutionRework || len(out.PackingLists[0].Lines) != 2 {
		t.Fatalf("H03 unloaded: %+v, %+v", h03, out.PackingLists[0])
	}
	if err = apportion("fin-planned", nil, 1); err != nil {
		t.Fatalf("apportion with H03 not loaded: %v", err)
	}
	a = last()
	if a.Stage != finapi.BasisPlanned {
		t.Fatalf("H03 is a house that takes part and is not loaded: %+v", a)
	}
	shares("planned, W/M", a, 1, 3_334, 3_334, 3_333)
	err = apportion("fin-planned-teu", nil, 2)
	if err == nil || !strings.Contains(err.Error(), "FIN_POOL_NO_BASIS") || !strings.Contains(err.Error(), "known once a house is stuffed") {
		t.Fatalf("the container method at the stage PLANNED: %v", err)
	}
	if err = apportion("fin-h03-out", []string{"SH-H03"}, 1, 3); err != nil {
		t.Fatalf("apportion with H03 named out: %v", err)
	}
	a = last()
	if a.Stage != finapi.BasisActual {
		t.Fatalf("H03 named out: %+v", a)
	}
	shares("H03 out, W/M", a, 1, 4_000, 6_001, 0)
	shares("H03 out, weight", a, 2, 5_099, 4_902, 0)

	// FUL during the rework (run): the adapter reads the CLOSED executions only, so the container in REWORK is not
	// relayed. H01's cargo is still in it, and FUL takes the evidence it has anyway: the packs as received, no
	// stuffing. Nothing is refused; the stuffing FUL had recorded is gone from the plan until the container is sealed
	// again. A later step of FUL's decides what a house in a container in rework reads.
	for _, h := range []string{"H01", "H03"} {
		plan, err := takes(t, d, ful, "ful-rework-"+h, "SH-"+h)
		if err != nil || plan.MeasuredBasis != fulapi.MeasuredReceived || plan.MeasuredPacks != 6 || len(plan.Stuffings) != 0 {
			t.Fatalf("FUL during the rework, %s: %v %+v", h, err, plan)
		}
	}

	// SEALED AGAIN: SEAL-2, the same execution, the same unit; H03 stays attached, with nothing loaded. The container
	// methods work at ACTUAL again, with H03 named out, and give H03 nothing.
	reseal := phyapi.CloseLoadExecution{CommandID: "reseal-1", TenantID: d.tenant, ActorID: "cfs-2", ExecutionID: "X-1", ExpectedRevision: d.revision["X-1"],
		Evidence: d.evidence(phyapi.EvidenceSeal, "SEAL-X-1-NEW", 25), OccurredAt: d.hours(25)}
	if _, err = d.phy.CloseLoadExecution(d.ctx, reseal); err != nil {
		t.Fatal(err)
	}
	after := d.basis()
	p := after.PackingLists[0]
	if p.ExecutionID != "X-1" || p.ExecutionState != phyapi.ExecutionClosed || len(p.Lines) != 2 || after.LoadUnits[0].Status != phyapi.UnitStatusClosed || p.Digest == before.PackingLists[0].Digest ||
		p.Digest == out.PackingLists[0].Digest || len(p.Reworks) != 1 || p.Reworks[0].NewSealReference != "SEAL-X-1-NEW" || p.Reworks[0].BrokenSeal.Reference != "SEAL-X-1" {
		t.Fatalf("after: %+v", p)
	}
	if err = apportion("fin-after", nil, 2); err == nil || !strings.Contains(err.Error(), "FIN_POOL_NO_BASIS") {
		t.Fatalf("after the reseal the stage is still PLANNED while H03 takes part: %v", err)
	}
	if err = apportion("fin-after-out", []string{"SH-H03"}, 1, 2, 3); err != nil {
		t.Fatalf("apportion after the reseal with H03 named out: %v", err)
	}
	a = last()
	if a.Stage != finapi.BasisActual {
		t.Fatalf("after: %+v", a)
	}
	shares("after, W/M", a, 1, 4_000, 6_001, 0)
	shares("after, TEU", a, 2, 5_001, 5_000, 0)
	shares("after, weight", a, 3, 5_099, 4_902, 0)

	// FUL after the reseal (run): H01 is tendered again in the same execution under the new seal; H03 has no packs in a
	// sealed container and reads as received, with no stuffing, though it is still attached to the consolidation.
	plan, err := takes(t, d, ful, "ful-after-H01", "SH-H01")
	if err != nil || plan.MeasuredBasis != fulapi.MeasuredTendered || len(plan.Stuffings) != 1 || plan.Stuffings[0].ExecutionID != "X-1" || plan.Stuffings[0].Seal != "SEAL-X-1-NEW" ||
		plan.Stuffings[0].LoadUnitNo != 1 || plan.Stuffings[0].GrossWeight != (kernel.Quantity{Unit: "KGM", Amount: 312}) {
		t.Fatalf("FUL after the rework, H01: %v %+v", err, plan)
	}
	if plan, err = takes(t, d, ful, "ful-after-H03", "SH-H03"); err != nil || plan.MeasuredBasis != fulapi.MeasuredReceived || len(plan.Stuffings) != 0 {
		t.Fatalf("FUL after the rework, H03: %v %+v", err, plan)
	}
}

// fulOf wires another FUL over the same database as the day's PHY, which a command of FUL's asks for the shipment's
// evidence (the day keeps no FUL of its own).
func fulOf(t *testing.T) *fulmodule.Module {
	t.Helper()
	platform := openPlatform(t)
	orgModule, err := platform.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := platform.RATE(orgModule, allowAll{}, marginFloor(2000))
	if err != nil {
		t.Fatal(err)
	}
	comModule, err := platform.COM(orgModule, rateModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	fulModule, _, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	return fulModule
}

// takes asks FUL to take PHY's evidence of the shipment and returns its OceanOperationPlan v1 and the error of the
// command.
func takes(t *testing.T, d *oceanDay, ful *fulmodule.Module, command, shipment string) (fulapi.OceanOperationPlan, error) {
	t.Helper()
	plan, err := ful.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: shipment})
	d.must("FUL's plan of "+shipment, err)
	_, err = ful.RecordVerifiedLoadEvidence(d.ctx, fulapi.RecordVerifiedLoadEvidence{CommandID: command, TenantID: d.tenant, ActorID: "ops-1", ShipmentID: shipment,
		ExpectedRevision: plan.Revision, OccurredAt: d.hours(31)})
	after, gotErr := ful.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: shipment})
	d.must("FUL's plan of "+shipment+" after", gotErr)
	return after, err
}
