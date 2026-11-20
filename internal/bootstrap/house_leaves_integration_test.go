//go:build integration

package bootstrap

import (
	"context"
	"strings"
	"testing"
	"time"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulmodule "go.idp.xyz/idp-cargo/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// A house unloaded in a rework leaves the closed consolidation (PHY V2.86), with the real ORG, RATE, COM, FUL, PHY and
// FIN. The owner's day: three houses H01, H02 and H03 of 10, 15 and 5 W/M in one 22G1 of C-1, H03 is unloaded in a
// rework of the container and leaves C-1. FIN's apportionment of three cost lines (OCEAN_FREIGHT by CHARGEABLE, THC by
// TEU, LASHING by GROSS_WEIGHT, each CNY 100.01) is asked for the first line before the rework and for the other two
// after the house left, and the test asserts what today's code gives. It is not a statement of what FIN should do: FIN
// is not changed in this step. The later steps will change, in FIN, what is asserted as "after" here: the cost of the
// house that left is taken from the pool of the consolidation it goes to, and the share it was given before the rework
// (line 1 below) is taken back; today it keeps it. FUL records the house's leaving from PHY's membership when it is
// asked to, and not before.

// leaveOf builds the day up to the second seal, and returns FUL over the same database.
func leaveOf(t *testing.T, prefix string) (*oceanDay, *fulmoduleHandle) {
	t.Helper()
	d := sealedDay(t, prefix)
	return d, d.reworkAndSealAgain()
}

// sealedDay is the day up to the first seal: the container sealed (SEAL-X-1) with the three houses, the lot of H03
// closed and the first cost line apportioned.
func sealedDay(t *testing.T, prefix string) *oceanDay {
	t.Helper()
	d := newOceanDay(t, prefix, map[string]int64{"H01": 312, "H02": 300, "H03": 288},
		map[string]int64{"H01": 10_000_000, "H02": 15_000_000, "H03": 5_000_000}, []string{"22G1"},
		[]struct{ code, method string }{{"OCEAN_FREIGHT", finapi.MethodChargeable}, {"THC", finapi.MethodTEU}, {"LASHING", finapi.MethodGrossWeight}})
	d.stuff("X-1", 1)
	for _, h := range []string{"H01", "H02", "H03"} {
		d.load("X-1", h, 6, 0)
	}
	d.seal("X-1")
	_, err := d.phy.CloseCargoLot(d.ctx, phyapi.CloseCargoLot{CommandID: "close-lot-h03", TenantID: d.tenant, ActorID: "cfs-1", LotID: "L-H03", ExpectedRevision: 3, OccurredAt: d.hours(16)})
	d.must("close the lot of H03", err)
	if err = d.apportion("fin-before", 1); err != nil {
		t.Fatalf("apportion before: %v", err)
	}
	return d
}

// reworkAndSealAgain opens the container for rework (ops-asker), unloads H03 and seals it again (SEAL-X-1-NEW). It
// returns FUL over the same database.
func (d *oceanDay) reworkAndSealAgain() *fulmoduleHandle {
	d.t.Helper()
	_, err := d.phy.OpenStuffingForRework(d.ctx, phyapi.OpenStuffingForRework{CommandID: "rework-1", TenantID: d.tenant, ActorID: "ops-asker", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Reason: "the consignee of H03 withdrew its cargo", OccurredAt: d.hours(22)})
	d.must("open the rework", err)
	d.revision["X-1"]++
	_, err = d.phy.UnloadHouseForRework(d.ctx, phyapi.UnloadHouseForRework{CommandID: "unload-h03", TenantID: d.tenant, ActorID: "wh-1", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], ShipmentID: "SH-H03", Evidence: phyapi.Evidence{Source: "CFS-WMS", Reference: "UNSTUFF-H03", OccurredAt: d.hours(24)}, OccurredAt: d.hours(24)})
	d.must("unload H03", err)
	d.revision["X-1"]++
	_, err = d.phy.CloseLoadExecution(d.ctx, phyapi.CloseLoadExecution{CommandID: "reseal-1", TenantID: d.tenant, ActorID: "cfs-2", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Evidence: d.evidence(phyapi.EvidenceSeal, "SEAL-X-1-NEW", 25), OccurredAt: d.hours(25)})
	d.must("seal again", err)
	d.revision["X-1"]++
	return &fulmoduleHandle{fulOf(d.t)}
}

func TestAHouseUnloadedInAReworkLeavesTheConsolidationWithEveryContext(t *testing.T) {
	d, ful := leaveOf(t, "cargo-ocean-leave-")
	shares := func(label string, a finapi.PoolApportionment, line int, want ...int64) {
		t.Helper()
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
	// BEFORE the house leaves it is attached, with nothing loaded: PHY reads three houses, FUL records the attachment
	before := d.basis()
	if len(before.Houses) != 3 || before.Houses[2].ShipmentID != "SH-H03" || before.Houses[2].LoadedPacks != 0 || before.PackingLists[0].ExecutionState != phyapi.ExecutionClosed {
		t.Fatalf("before: %+v", before)
	}
	if plan := ful.plan(t, d, "SH-H03"); plan.ConsolidationID != "C-1" || plan.MembershipEntryNo != 1 {
		t.Fatalf("FUL before the detach: %+v", plan)
	}
	first := d.pool().Apportionments[0]
	if first.Stage != finapi.BasisActual {
		t.Fatalf("the first apportionment, before the rework: %+v", first)
	}
	shares("W/M before the rework", first, 1, 3_334, 5_000, 1_667)

	// PHY: the operator detaches H03 with a reason; the entry that ended names the rework
	r, err := d.phy.DetachHouse(d.ctx, phyapi.DetachHouse{CommandID: "detach-h03", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: "SH-H03",
		ExpectedMembershipRevision: 1, Reason: phyapi.ReasonSpaceShortage, OccurredAt: d.hours(26)})
	if err != nil || r.EntryNo != 1 || r.NewRevision != 2 {
		t.Fatalf("detach: %+v %v", r, err)
	}
	m, err := d.phy.GetConsolMembership(d.ctx, phyapi.GetConsolMembership{TenantID: d.tenant, ShipmentID: "SH-H03"})
	if err != nil || m.Live || len(m.History) != 1 || m.History[0].End != phyapi.EntryDetached || m.History[0].Reason != phyapi.ReasonSpaceShortage ||
		m.History[0].ReworkExecutionID != "X-1" || m.History[0].ReworkNo != 1 {
		t.Fatalf("the membership of H03: %+v %v", m, err)
	}
	after := d.basis()
	if len(after.Houses) != 2 || after.State != phyapi.StateClosed || after.PackingLists[0].ExecutionState != phyapi.ExecutionClosed || len(after.PackingLists[0].Lines) != 2 {
		t.Fatalf("PHY's cost basis after: %+v", after)
	}

	// FUL (run): it still records H03 in C-1 until it is asked to take PHY's membership; then it records the leaving
	if plan := ful.plan(t, d, "SH-H03"); plan.ConsolidationID != "C-1" || plan.MembershipEntryNo != 1 {
		t.Fatalf("FUL records H03 in C-1 until the detachment is recorded: %+v", plan)
	}
	plan := ful.plan(t, d, "SH-H03")
	if _, err = ful.RecordConsolDetachment(d.ctx, fulapi.RecordConsolDetachment{CommandID: "ful-detach", TenantID: d.tenant, ActorID: "ops-1", ShipmentID: "SH-H03",
		ExpectedRevision: plan.Revision, ConsolidationID: "C-1", OccurredAt: d.hours(27)}); err != nil {
		t.Fatalf("FUL records the detachment: %v", err)
	}
	if plan = ful.plan(t, d, "SH-H03"); plan.ConsolidationID != "" || plan.MembershipEntryNo != 0 || plan.MembershipRevision != 0 || plan.Status != fulapi.StatusPlanned ||
		plan.Revision != 5 || len(plan.Stuffings) != 0 {
		t.Fatalf("FUL after the detachment: attached nowhere, still planned: %+v", plan)
	}
	if plan = ful.plan(t, d, "SH-H01"); plan.ConsolidationID != "C-1" {
		t.Fatalf("FUL still records H01 in C-1: %+v", plan)
	}

	// FIN (run): the pool apportions the other two lines on the two houses that are attached, at the stage ACTUAL, with
	// no house named out (the house is not attached, so it is not one that takes part and is not loaded); the share H03
	// was given on line 1 before the rework stands: a later step of FIN's takes it back.
	if err = d.apportion("fin-after", 2, 3); err != nil {
		t.Fatalf("apportion after: %v", err)
	}
	ap := d.pool().Apportionments
	if len(ap) != 2 || ap[1].Stage != finapi.BasisActual {
		t.Fatalf("after: %+v", ap)
	}
	shares("TEU after", ap[1], 1, 5_001, 5_000)
	shares("weight after", ap[1], 2, 5_099, 4_902)
	if got := len(ap[0].Lines[0].Shares); got != 3 || ap[0].Lines[0].Shares[2].ShipmentID != "SH-H03" {
		t.Fatalf("H03 keeps its share of line 1: %+v", ap[0].Lines[0].Shares)
	}
}

// TestAHouseUnloadedInAReworkIsTransferredWithEveryContext: the same day, H03 moves to a second own-container
// consolidation C-2 instead, which FUL then records; C-2 is closed, stuffed with the lot that was opened again, and
// sealed, and C-1 reconciles with H01 and H02.
func TestAHouseUnloadedInAReworkIsTransferredWithEveryContext(t *testing.T) {
	d, ful := leaveOf(t, "cargo-ocean-move-")
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	departs := d.at.Add(72 * time.Hour)
	_, err := d.phy.CreateConsolidation(d.ctx, phyapi.CreateConsolidation{CommandID: "consol-2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", Mode: phyapi.ModeSea,
		Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"), PlannedDeparture: departs.Add(6 * time.Hour),
		HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: d.hours(8)})
	d.must("create C-2", err)
	_, err = d.phy.PlanLoadUnit(d.ctx, phyapi.PlanLoadUnit{CommandID: "plan-c2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", ExpectedRevision: 1,
		Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: d.hours(8)})
	d.must("plan the unit of C-2", err)

	r, err := d.phy.TransferHouse(d.ctx, phyapi.TransferHouse{CommandID: "transfer-h03", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-1", ToConsolidationID: "C-2",
		ShipmentID: "SH-H03", ExpectedMembershipRevision: 1, Reason: phyapi.ReasonWrongConsol, OccurredAt: d.hours(26)})
	if err != nil || r.ConsolidationID != "C-2" || r.EntryNo != 2 {
		t.Fatalf("transfer: %+v %v", r, err)
	}
	m, err := d.phy.GetConsolMembership(d.ctx, phyapi.GetConsolMembership{TenantID: d.tenant, ShipmentID: "SH-H03"})
	if err != nil || !m.Live || m.Entry.ConsolidationID != "C-2" || len(m.History) != 2 || m.History[0].End != phyapi.EntryTransferred || m.History[0].ReworkExecutionID != "X-1" ||
		m.History[0].ReworkNo != 1 || m.History[1].ReworkNo != 0 {
		t.Fatalf("the membership of H03: %+v %v", m, err)
	}

	// FUL (run): one command records the new attachment and ends the old
	plan := ful.plan(t, d, "SH-H03")
	if plan.ConsolidationID != "C-1" {
		t.Fatalf("FUL still records H03 in C-1: %+v", plan)
	}
	if _, err = ful.RecordConsolAttachment(d.ctx, fulapi.RecordConsolAttachment{CommandID: "ful-attach-c2", TenantID: d.tenant, ActorID: "ops-1", ShipmentID: "SH-H03",
		ExpectedRevision: plan.Revision, ConsolidationID: "C-2", OccurredAt: d.hours(27)}); err != nil {
		t.Fatalf("FUL records the attachment to C-2: %v", err)
	}
	if plan = ful.plan(t, d, "SH-H03"); plan.ConsolidationID != "C-2" || plan.MembershipEntryNo != 2 {
		t.Fatalf("FUL after the transfer: %+v", plan)
	}

	// C-2 is closed, its container stuffed from the lot that was opened again, and sealed
	c2, err := d.phy.GetConsolidation(d.ctx, phyapi.GetConsolidation{TenantID: d.tenant, ConsolidationID: "C-2"})
	d.must("C-2", err)
	_, err = d.phy.CloseConsolidation(d.ctx, phyapi.CloseConsolidation{CommandID: "close-c2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", ExpectedRevision: c2.Revision,
		OccurredAt: d.hours(28)})
	d.must("close C-2", err)
	_, err = d.phy.OpenLoadExecution(d.ctx, phyapi.OpenLoadExecution{CommandID: "stuff-x2", TenantID: d.tenant, ActorID: "cfs-1", ExecutionID: "X-2", ConsolidationID: "C-2",
		Kind: phyapi.ExecutionStuffing, Facility: d.cfs, LoadUnitNo: 1, OccurredAt: d.hours(29)})
	d.must("open X-2", err)
	_, err = d.phy.StartLoading(d.ctx, phyapi.StartLoading{CommandID: "start-x2", TenantID: d.tenant, ActorID: "cfs-1", ExecutionID: "X-2", ExpectedRevision: 1, OccurredAt: d.hours(29)})
	d.must("start X-2", err)
	d.revision["X-2"] = 2
	d.loadAt("X-2", "H03", 6, 30)
	d.sealAt("X-2", 31)
	b2, err := d.phy.GetPhysicalCostBasis(d.ctx, phyapi.GetPhysicalCostBasis{TenantID: d.tenant, ConsolidationID: "C-2"})
	d.must("C-2's cost basis", err)
	if len(b2.Houses) != 1 || b2.Houses[0].ShipmentID != "SH-H03" || b2.Houses[0].LoadedPacks != 6 || b2.PackingLists[0].ExecutionID != "X-2" || b2.PackingLists[0].ExecutionState != phyapi.ExecutionClosed {
		t.Fatalf("C-2's cost basis: %+v", b2)
	}
	if plan = ful.plan(t, d, "SH-H03"); plan.ConsolidationID != "C-2" {
		t.Fatalf("FUL: %+v", plan)
	}

	// C-1 has H01 and H02 left, each loaded as planned, and its container sealed: it reconciles
	c1, err := d.phy.GetConsolidation(d.ctx, phyapi.GetConsolidation{TenantID: d.tenant, ConsolidationID: "C-1"})
	d.must("C-1", err)
	if _, err = d.phy.ReconcileConsolidation(d.ctx, phyapi.ReconcileConsolidation{CommandID: "reconcile-c1", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: c1.Revision, OccurredAt: d.hours(32)}); err != nil {
		t.Fatalf("reconcile C-1: %v", err)
	}
	if c1, err = d.phy.GetConsolidation(d.ctx, phyapi.GetConsolidation{TenantID: d.tenant, ConsolidationID: "C-1"}); err != nil || c1.State != phyapi.StateReconciled || len(c1.Members) != 2 {
		t.Fatalf("C-1: %+v %v", c1, err)
	}

	// FIN (run), after the transfer, with today's code. The house is charged in both pools: P-2 is opened on C-2 with one
	// line of CNY 100.01, apportioned on the one house, and JOB-H03 carries 1,667 from P-1 (line 1, apportioned before
	// the rework) and 10,001 from P-2. Nothing says so. P-1 cannot be closed as it stands (its line 1 is not apportioned
	// on the loaded basis it has now): FIN_POOL_NOT_CLOSABLE. Once line 1 is apportioned again, on H01 and H02, the pool
	// closes and the job's cost falls to 10,001. The later FIN steps take the house out of the source pool's
	// apportionments when it leaves, so that neither the double cost nor the refused close arises.
	if err = d.apportion("fin-after-transfer", 2, 3); err != nil {
		t.Fatalf("apportion lines 2 and 3 of P-1 after the transfer: %v", err)
	}
	_, err = d.fin.OpenConsolCostPool(d.ctx, finapi.OpenConsolCostPool{CommandID: "fin-pool2", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-2", ConsolidationID: "C-2",
		OccurredAt: d.hours(33)})
	d.must("open P-2", err)
	_, err = d.fin.RecordConsolCost(d.ctx, finapi.RecordConsolCost{CommandID: "fin-cost2", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-2", ExpectedRevision: 1,
		ChargeCode: "OCEAN_FREIGHT", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: 10_001}},
		Reason: "the line's charge for the second container", OccurredAt: d.hours(33)})
	d.must("cost of P-2", err)
	p2, err := d.fin.GetConsolCostPool(d.ctx, finapi.GetConsolCostPool{TenantID: d.tenant, PoolID: "P-2"})
	d.must("P-2", err)
	if _, err = d.fin.ApportionConsolCosts(d.ctx, finapi.ApportionConsolCosts{CommandID: "fin-apportion-p2", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-2",
		ExpectedRevision: p2.Revision, LineNos: []int{1}, OccurredAt: d.hours(34)}); err != nil {
		t.Fatalf("apportion P-2: %v", err)
	}
	jobCost := func() int64 {
		t.Helper()
		jp, err := d.fin.GetJobProfit(d.ctx, finapi.GetJobProfit{TenantID: d.tenant, JobID: "JOB-H03"})
		d.must("the profit of JOB-H03", err)
		return jp.PoolCost.Minor
	}
	if got := jobCost(); got != 1_667+10_001 {
		t.Fatalf("JOB-H03 is charged by both pools: %d, want 11,668", got)
	}
	if err = d.closeInvoicedPool("P-1", 3); err == nil || !strings.Contains(err.Error(), "FIN_POOL_NOT_CLOSABLE") {
		t.Fatalf("P-1 closes with the house's line 1 apportioned before it left: %v", err)
	}
	if err = d.apportion("fin-reapportion", 1); err != nil {
		t.Fatalf("apportion line 1 of P-1 again: %v", err)
	}
	if err = d.closePoolAgain("P-1"); err != nil {
		t.Fatalf("close P-1 after line 1 is apportioned again: %v", err)
	}
	if got := jobCost(); got != 10_001 {
		t.Fatalf("JOB-H03 is charged by P-2 only: %d, want 10,001", got)
	}
}

// fulmoduleHandle is FUL over the day's database, with the one read the tests make of it.
type fulmoduleHandle struct{ *fulmodule.Module }

func (f *fulmoduleHandle) plan(t *testing.T, d *oceanDay, shipment string) fulapi.OceanOperationPlan {
	t.Helper()
	p, err := f.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: shipment})
	d.must("FUL's plan of "+shipment, err)
	return p
}

// loadAt loads packs of the house's lot into the stuffing at the hour given; sealAt seals it then.
func (d *oceanDay) loadAt(x, house string, packs int64, hour int) {
	d.t.Helper()
	_, err := d.phy.RecordCargoLoaded(d.ctx, phyapi.RecordCargoLoaded{CommandID: "load-" + x + "-" + house, TenantID: d.tenant, ActorID: "cfs-1", ExecutionID: x,
		ExpectedRevision: d.revision[x], LotID: "L-" + house, Packs: packs, Evidence: d.evidence("CFS-TALLY", "T-"+x+house, hour), OccurredAt: d.hours(hour)})
	d.must("load "+x+" "+house, err)
	d.revision[x]++
}

func (d *oceanDay) sealAt(x string, hour int) {
	d.t.Helper()
	_, err := d.phy.CloseLoadExecution(d.ctx, phyapi.CloseLoadExecution{CommandID: "seal-" + x, TenantID: d.tenant, ActorID: "cfs-1", ExecutionID: x,
		ExpectedRevision: d.revision[x], Evidence: d.evidence(phyapi.EvidenceSeal, "SEAL-"+x, hour), OccurredAt: d.hours(hour)})
	d.must("seal "+x, err)
	d.revision[x]++
}

// TestSEAFinalizesAfterAHouseLeftOnceItsAssociationIsEnded runs SEA's movement of C-1 through the round (the three
// houses associated before the rework) and asserts what today's SEA does after H03 left C-1 and FUL recorded it: SEA's
// own facts are not changed by this step, and it does not read PHY's seal or rework. SEA cannot refresh the house, nor
// finalize the movement (STALE_HOUSE, naming H03), while H03's association stands; SEA's own command ends it (FUL no
// longer reports H03 in the consolidation) and the movement finalizes with H01 and H02. The step that deals with the
// seal of the container and the rework in SEA is a later one: the seal SEA records here is the one PHY sealed it with
// again, typed by the operator, and SEA does not compare it with PHY's.
func TestSEAFinalizesAfterAHouseLeftOnceItsAssociationIsEnded(t *testing.T) {
	ctx := context.Background()
	d := sealedDay(t, "cargo-ocean-sea-")
	platform := openPlatform(t)
	orgModule, err := platform.ORG(allowAll{})
	d.must("org", err)
	rateModule, err := platform.RATE(orgModule, allowAll{}, marginFloor(2000))
	d.must("rate", err)
	comModule, err := platform.COM(orgModule, rateModule, allowAll{})
	d.must("com", err)
	fulModule, _, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	d.must("ful", err)
	bookModule, err := platform.BOOK(orgModule, allowAll{})
	d.must("book", err)
	seaModule, err := platform.SEA(bookModule, fulModule, allowAll{})
	d.must("sea", err)
	departs := d.at.Add(72 * time.Hour)
	contexts := slice{tenant: d.tenant, at: d.at, departs: departs}
	contexts.bookSea(t, bookModule, "BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", departs.Add(6*time.Hour))
	_, err = seaModule.OpenOceanMovement(ctx, seaapi.OpenOceanMovement{CommandID: "open-1", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1", ConsolidationID: "C-1", BookingID: "BK-1", OccurredAt: d.hours(11)})
	d.must("open movement", err)
	_, err = seaModule.RegisterHblSeries(ctx, seaapi.RegisterHblSeries{CommandID: "series", TenantID: d.tenant, ActorID: "ops-1", LegalEntityID: "LE", Template: "SHAL{seq:7}", FirstNumber: 1, OccurredAt: d.hours(11)})
	d.must("series", err)
	movement := func() seaapi.OceanMovement {
		m, err := seaModule.GetOceanMovement(ctx, seaapi.GetOceanMovement{TenantID: d.tenant, MovementID: "OM-1"})
		d.must("movement", err)
		return m
	}
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		_, err = seaModule.AssociateHouseBill(ctx, seaapi.AssociateHouseBill{CommandID: "hbl-" + s, TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1", ExpectedRevision: movement().Revision, ShipmentID: s, OccurredAt: d.hours(11)})
		d.must("hbl "+s, err)
	}
	ful := d.reworkAndSealAgain()
	_, err = d.phy.DetachHouse(ctx, phyapi.DetachHouse{CommandID: "detach-h03", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: "SH-H03", ExpectedMembershipRevision: 1, Reason: phyapi.ReasonSpaceShortage, OccurredAt: d.hours(26)})
	d.must("detach", err)
	plan := ful.plan(t, d, "SH-H03")
	_, err = ful.RecordConsolDetachment(ctx, fulapi.RecordConsolDetachment{CommandID: "ful-detach", TenantID: d.tenant, ActorID: "ops-1", ShipmentID: "SH-H03", ExpectedRevision: plan.Revision, ConsolidationID: "C-1", OccurredAt: d.hours(27)})
	d.must("ful detach", err)
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		p := ful.plan(t, d, s)
		_, err = ful.RecordVerifiedLoadEvidence(ctx, fulapi.RecordVerifiedLoadEvidence{CommandID: "ful-ev-" + s, TenantID: d.tenant, ActorID: "ops-1", ShipmentID: s, ExpectedRevision: p.Revision, OccurredAt: d.hours(28)})
		d.must("FUL takes the evidence of "+s, err)
		_, err = seaModule.RefreshHouseBillFacts(ctx, seaapi.RefreshHouseBillFacts{CommandID: "refresh-" + s, TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1", ExpectedRevision: movement().Revision, ShipmentID: s, OccurredAt: d.hours(28)})
		if s == "SH-H03" {
			if err == nil || !strings.Contains(err.Error(), `SH-H03 is PLANNED SEA LCL in "" of LE`) {
				t.Fatalf("SEA refreshes the house that left: %v", err)
			}
		} else {
			d.must("SEA refreshes "+s, err)
		}
	}
	r, err := seaModule.AssignContainer(ctx, seaapi.AssignContainer{CommandID: "assign", TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-1", MovementID: "OM-1", LoadUnitNo: 1, ContainerNumber: "csqu3054383", SizeType: "22G1", TareKg: 2180, MaxGrossKg: 30480, PlateSource: seaapi.PlateCSC, OccurredAt: d.hours(12)})
	d.must("assign", err)
	visit := r.NewRevision
	r, err = seaModule.RecordContainerSeal(ctx, seaapi.RecordContainerSeal{CommandID: "sea-seal", TenantID: d.tenant, ActorID: "cfs-1", VisitID: "CV-1", ExpectedRevision: visit, Number: "seal-x-1-new", Party: seaapi.SealShipper, AppliedAt: d.hours(25), Source: "CFS", OccurredAt: d.hours(25)})
	d.must("SEA's seal", err)
	visit = r.NewRevision
	r, err = seaModule.RecordStuffingDone(ctx, seaapi.RecordStuffingDone{CommandID: "stuffed", TenantID: d.tenant, ActorID: "cfs-1", VisitID: "CV-1", ExpectedRevision: visit, OccurredAt: d.hours(26)})
	d.must("stuffing done", err)
	visit = r.NewRevision
	r, err = seaModule.AssertVgm(ctx, seaapi.AssertVgm{CommandID: "vgm", TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: visit, Mass: kernel.Quantity{Unit: "KGM", Amount: 3700}, Method: seaapi.VgmMethod2, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1, DeterminedAt: d.hours(27), Signatory: "Ada", OccurredAt: d.hours(27)})
	d.must("VGM", err)
	visit = r.NewRevision
	r, err = seaModule.RecordVgmResponse(ctx, seaapi.RecordVgmResponse{CommandID: "vgm-answer", TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: visit, Response: seaapi.VgmAccepted, Source: "portal", OccurredAt: d.hours(28)})
	d.must("VGM answer", err)
	visit = r.NewRevision
	for i, e := range []struct{ kind, reference string }{{seaapi.EventGateIn, "GIN-1"}, {seaapi.EventLoaded, "LD-1"}} {
		r, err = seaModule.RecordContainerEvent(ctx, seaapi.RecordContainerEvent{CommandID: "event-" + e.reference, TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: visit, Kind: e.kind, Location: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHG"}, HappenedAt: d.hours(29 + i), Source: "terminal", Reference: e.reference, OccurredAt: d.hours(29 + i)})
		d.must(e.kind, err)
		visit = r.NewRevision
	}
	bill := seaapi.Evidence{SHA256: strings.Repeat("9c", 32), StorageID: "mail/mbl.pdf", MediaType: "application/pdf", CapturedAt: d.hours(31)}
	_, err = seaModule.RecordMasterBill(ctx, seaapi.RecordMasterBill{CommandID: "mbl", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1", ExpectedRevision: movement().Revision, Number: "linu2611000457", Kind: seaapi.MasterBillCarrierMBL, Evidence: bill, OccurredAt: d.hours(31)})
	d.must("master bill", err)
	finalize := func(command string) error {
		_, err := seaModule.FinalizeOceanDocFacts(ctx, seaapi.FinalizeOceanDocFacts{CommandID: command, TenantID: d.tenant, ActorID: "ops-2", MovementID: "OM-1",
			ExpectedRevision: movement().Revision, OccurredAt: d.hours(32)})
		return err
	}
	if err = finalize("finalize-1"); err == nil || !strings.Contains(err.Error(), "STALE_HOUSE") || !strings.Contains(err.Error(), "SH-H03") {
		t.Fatalf("SEA finalizes with the house that left still associated: %v", err)
	}
	if m := movement(); m.State != seaapi.StateOpen || m.DocFactsVersion != 0 {
		t.Fatalf("the movement stays OPEN: %+v", m)
	}
	_, err = seaModule.EndHouseBillAssociation(ctx, seaapi.EndHouseBillAssociation{CommandID: "end-h03", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: movement().Revision, ShipmentID: "SH-H03", OccurredAt: d.hours(32)})
	d.must("end the association of H03", err)
	d.must("finalize", finalize("finalize-2"))
	facts, err := seaModule.GetOceanDocFacts(ctx, seaapi.GetOceanDocFacts{TenantID: d.tenant, MovementID: "OM-1"})
	if err != nil || facts.Status != seaapi.DocFactsFinal || facts.Version != 1 || len(facts.Houses) != 2 || len(facts.Containers) != 1 {
		t.Fatalf("the FINAL facts: %+v %v", facts, err)
	}
}

// closeInvoicedPool records the carrier's invoice for the first n cost lines of the pool, matches it final and tries to
// close the pool; closePoolAgain closes it once more, after a refusal, the invoice already matched.
func (d *oceanDay) closeInvoicedPool(pool string, n int) error {
	d.t.Helper()
	var lines []finapi.CreditorInvoiceLine
	for _, code := range []string{"OCEAN_FREIGHT", "THC", "LASHING"}[:n] {
		lines = append(lines, finapi.CreditorInvoiceLine{ChargeCode: code, Amount: 10_001})
	}
	_, err := d.fin.RecordCreditorInvoice(d.ctx, finapi.RecordCreditorInvoice{CommandID: "fin-invoice", TenantID: d.tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		LegalEntityID: "LE", CreditorPartyID: "line-1", SupplierInvoiceNo: "LINE-INV-2611-4", InvoiceDate: d.hours(19).Format(time.DateOnly), Currency: "CNY", Lines: lines,
		OccurredAt: d.hours(35)})
	d.must("record the line's invoice", err)
	invoice, err := d.fin.GetCreditorInvoice(d.ctx, finapi.GetCreditorInvoice{TenantID: d.tenant, InvoiceID: "CI-LINE-1"})
	d.must("the line's invoice", err)
	matches := make([]finapi.CreditorMatch, n)
	for i := range matches {
		matches[i] = finapi.CreditorMatch{LineNo: i + 1, Target: finapi.AccrualRef{Kind: "POOL_COST", ID: pool, LineNo: i + 1}, Amount: 10_001, Final: true}
	}
	_, err = d.fin.MatchCreditorInvoice(d.ctx, finapi.MatchCreditorInvoice{CommandID: "fin-match", TenantID: d.tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		ExpectedRevision: invoice.Revision, Matches: matches, OccurredAt: d.hours(36)})
	d.must("match the line's invoice", err)
	return d.closePoolAgain(pool)
}

func (d *oceanDay) closePoolAgain(pool string) error {
	d.t.Helper()
	p, err := d.fin.GetConsolCostPool(d.ctx, finapi.GetConsolCostPool{TenantID: d.tenant, PoolID: pool})
	d.must("the pool", err)
	_, err = d.fin.CloseConsolCostPool(d.ctx, finapi.CloseConsolCostPool{CommandID: "fin-close-" + p.State + "-" + strings.Repeat("x", int(p.Revision)), TenantID: d.tenant, ActorID: "fin-1",
		PoolID: pool, ExpectedRevision: p.Revision, OccurredAt: d.hours(37)})
	return err
}
