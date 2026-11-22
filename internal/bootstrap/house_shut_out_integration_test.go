//go:build integration

package bootstrap

import (
	"context"
	"strings"
	"testing"
	"time"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// A house shut out before stuffing leaves the closed consolidation (PHY V2.87), with the real ORG, RATE, COM, FUL, PHY
// and FIN, and SEA and BOOK as house_leaves_integration_test.go wires them. The day: three houses H01, H02 and H04 of
// 10, 15 and 5 W/M, each received at the CFS with its lot; H01 and H02 are stuffed in the one 22G1 of C-1 and sealed
// (SEAL-X-1), H04 is attached and never loaded. FIN's first cost line (OCEAN_FREIGHT by CHARGEABLE, CNY 100.01) is
// apportioned before the house leaves, while it is attached and nothing of it is loaded.  What FUL, SEA and FIN do
// after the house left is asserted as today's code gives it: it is not a statement of what they should do, and none of
// them is changed in this step. The later steps change it: FIN's (PR 5 and 6) take the cost of the house that left out
// of the pool it left and into the one it goes to, and SEA's (PR 3) end the house's association with the movement when
// it leaves.

// shutOutDay builds the day up to the first seal and the first apportionment.
func shutOutDay(t *testing.T, prefix string) *oceanDay {
	t.Helper()
	d := newOceanDay(t, prefix, map[string]int64{"H01": 312, "H02": 300, "H04": 288},
		map[string]int64{"H01": 10_000_000, "H02": 15_000_000, "H04": 5_000_000}, []string{"22G1"},
		[]struct{ code, method string }{{"OCEAN_FREIGHT", finapi.MethodChargeable}, {"THC", finapi.MethodTEU}, {"LASHING", finapi.MethodGrossWeight}})
	d.stuff("X-1", 1)
	d.load("X-1", "H01", 6, 0)
	d.load("X-1", "H02", 6, 0)
	d.seal("X-1")
	if err := d.apportion("fin-before", 1); err != nil {
		t.Fatalf("apportion before: %v", err)
	}
	return d
}

// detachH04 detaches H04 with SHUT_OUT at the hour given.
func (d *oceanDay) detachH04(hour int) {
	d.t.Helper()
	r, err := d.phy.DetachHouse(d.ctx, phyapi.DetachHouse{CommandID: "detach-h04", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: "SH-H04",
		ExpectedMembershipRevision: 1, Reason: phyapi.ReasonShutOut, OccurredAt: d.hours(hour)})
	if err != nil || r.EntryNo != 1 || r.NewRevision != 2 {
		d.t.Fatalf("detach: %+v %v", r, err)
	}
}

func (d *oceanDay) reconcile(command string, hour int) error {
	d.t.Helper()
	c, err := d.phy.GetConsolidation(d.ctx, phyapi.GetConsolidation{TenantID: d.tenant, ConsolidationID: "C-1"})
	d.must("C-1", err)
	_, err = d.phy.ReconcileConsolidation(d.ctx, phyapi.ReconcileConsolidation{CommandID: command, TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: c.Revision, OccurredAt: d.hours(hour)})
	return err
}

func (d *oceanDay) jobCost(job string) int64 {
	d.t.Helper()
	jp, err := d.fin.GetJobProfit(d.ctx, finapi.GetJobProfit{TenantID: d.tenant, JobID: job})
	d.must("the profit of "+job, err)
	return jp.PoolCost.Minor
}

func TestAHouseShutOutBeforeStuffingLeavesTheConsolidationWithEveryContext(t *testing.T) {
	d := shutOutDay(t, "cargo-ocean-shut-")
	ful := &fulmoduleHandle{fulOf(t)}
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
	// BEFORE: PHY reads three houses, H04 with nothing loaded, and refuses to reconcile while H04 is attached and never
	// loaded; FIN apportioned line 1 at the stage PLANNED on the three houses (it has no loaded basis for H04), 3,334,
	// 3,334 and 3,333, and JOB-H04 is charged 3,333 of it
	if b := d.basis(); len(b.Houses) != 3 || b.Houses[2].ShipmentID != "SH-H04" || b.Houses[2].LoadedPacks != 0 || b.PackingLists[0].ExecutionState != phyapi.ExecutionClosed {
		t.Fatalf("before: %+v", b)
	}
	if err := d.reconcile("reconcile-early", 26); err == nil || !strings.Contains(err.Error(), "SH-H04 plans 6 packs, 0 loaded") {
		t.Fatalf("reconcile with H04 attached and never loaded: %v", err)
	}
	first := d.pool().Apportionments[0]
	if first.Stage != finapi.BasisPlanned {
		t.Fatalf("line 1 is apportioned before the leaving: %+v", first)
	}
	shares("W/M before", first, 1, 3_334, 3_334, 3_333)
	if got := d.jobCost("JOB-H04"); got != 3_333 {
		t.Fatalf("JOB-H04 before: %d", got)
	}
	if plan := ful.plan(t, d, "SH-H04"); plan.ConsolidationID != "C-1" || plan.MembershipEntryNo != 1 {
		t.Fatalf("FUL before the detach: %+v", plan)
	}

	// PHY: the operator detaches H04 with a reason; the entry that ended names no rework
	d.detachH04(26)
	m, err := d.phy.GetConsolMembership(d.ctx, phyapi.GetConsolMembership{TenantID: d.tenant, ShipmentID: "SH-H04"})
	if err != nil || m.Live || len(m.History) != 1 || m.History[0].End != phyapi.EntryDetached || m.History[0].Reason != phyapi.ReasonShutOut ||
		m.History[0].ReworkExecutionID != "" || m.History[0].ReworkNo != 0 {
		t.Fatalf("the membership of H04: %+v %v", m, err)
	}
	if b := d.basis(); len(b.Houses) != 2 || b.State != phyapi.StateClosed || len(b.PackingLists[0].Lines) != 2 {
		t.Fatalf("PHY's cost basis after: %+v", b)
	}

	// FUL (run): it still records H04 in C-1 until it is asked to take PHY's membership; then it records the leaving
	plan := ful.plan(t, d, "SH-H04")
	if plan.ConsolidationID != "C-1" || plan.MembershipEntryNo != 1 {
		t.Fatalf("FUL records H04 in C-1 until the detachment is recorded: %+v", plan)
	}
	if _, err = ful.RecordConsolDetachment(d.ctx, fulapi.RecordConsolDetachment{CommandID: "ful-detach", TenantID: d.tenant, ActorID: "ops-1", ShipmentID: "SH-H04",
		ExpectedRevision: plan.Revision, ConsolidationID: "C-1", OccurredAt: d.hours(27)}); err != nil {
		t.Fatalf("FUL records the detachment: %v", err)
	}
	if plan = ful.plan(t, d, "SH-H04"); plan.ConsolidationID != "" || plan.MembershipEntryNo != 0 || plan.Status != fulapi.StatusPlanned || len(plan.Stuffings) != 0 {
		t.Fatalf("FUL after the detachment: attached nowhere, still planned: %+v", plan)
	}
	if plan = ful.plan(t, d, "SH-H01"); plan.ConsolidationID != "C-1" {
		t.Fatalf("FUL still records H01 in C-1: %+v", plan)
	}

	// PHY (run): the consolidation reconciles with H01 and H02 loaded as planned and the container sealed
	if err = d.reconcile("reconcile", 28); err != nil {
		t.Fatalf("reconcile after the house left: %v", err)
	}
	if c, err := d.phy.GetConsolidation(d.ctx, phyapi.GetConsolidation{TenantID: d.tenant, ConsolidationID: "C-1"}); err != nil || c.State != phyapi.StateReconciled || len(c.Members) != 2 {
		t.Fatalf("C-1: %+v %v", c, err)
	}

	// FIN (run): the other two lines are apportioned at the stage ACTUAL on the two houses that are attached; the
	// PLANNED share H04 was given on line 1 stands (3,333), as a former share pending finance's attribution since V2.93
	// (before it, charged to the job): the job's cost has none of it and lists it apart; the pool cannot close
	// (FIN_POOL_NOT_CLOSABLE, naming the house) and line 1 is not apportioned again (FIN_ATTRIBUTION_MISSING) until
	// finance attributes it; once it is, on H01 and H02, the pool closes and the job carries nothing from it.
	if err = d.apportion("fin-after", 2, 3); err != nil {
		t.Fatalf("apportion after: %v", err)
	}
	ap := d.pool().Apportionments
	if len(ap) != 2 || ap[1].Stage != finapi.BasisActual {
		t.Fatalf("after: %+v", ap)
	}
	shares("TEU after", ap[1], 1, 5_001, 5_000)
	shares("weight after", ap[1], 2, 5_099, 4_902)
	if got := ap[0].Lines[0].Shares; len(got) != 3 || got[2].ShipmentID != "SH-H04" {
		t.Fatalf("H04 keeps its PLANNED share of line 1: %+v", got)
	}
	if got := d.jobCost("JOB-H04"); got != 0 {
		t.Fatalf("JOB-H04 after the leaving is not charged its PLANNED share, which is pending: %d", got)
	}
	if jp, err := d.fin.GetJobProfit(d.ctx, finapi.GetJobProfit{TenantID: d.tenant, JobID: "JOB-H04"}); err != nil || jp.FormerSharesPending.Minor != 3_333 || len(jp.FormerShares) != 1 {
		t.Fatalf("JOB-H04 lists its former share apart: %+v %v", jp, err)
	}
	if err = d.closeInvoicedPool("P-1", 3); err == nil || !strings.Contains(err.Error(), "FIN_POOL_NOT_CLOSABLE") || !strings.Contains(err.Error(), "former share of house SH-H04") {
		t.Fatalf("P-1 closes with the PLANNED share of the house that left not attributed: %v", err)
	}
	if err = d.apportion("fin-reapportion", 1); err == nil || !strings.Contains(err.Error(), "FIN_ATTRIBUTION_MISSING") || !strings.Contains(err.Error(), "3333 CNY") {
		t.Fatalf("apportion line 1 again without finance's attribution: %v", err)
	}
	if err = d.attribute("fin-attribute", "fin-1", statement("H04", 0, 0, "")); err != nil {
		t.Fatalf("attribute the former share of line 1, all to H01 and H02: %v", err)
	}
	if err = d.closePoolAgain("P-1"); err != nil {
		t.Fatalf("close P-1 after line 1 is apportioned again: %v", err)
	}
	if got := d.jobCost("JOB-H04"); got != 0 {
		t.Fatalf("JOB-H04 once line 1 is apportioned again: %d", got)
	}
}

// TestAHouseShutOutBeforeStuffingIsTransferredWithEveryContext: the same day, H04 moves to a second own-container
// consolidation C-2, which FUL then records; C-2 is closed, stuffed with the lot of H04 and sealed, and C-1 reconciles
// with H01 and H02.
func TestAHouseShutOutBeforeStuffingIsTransferredWithEveryContext(t *testing.T) {
	d := shutOutDay(t, "cargo-ocean-shutmove-")
	ful := &fulmoduleHandle{fulOf(t)}
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	departs := d.at.Add(72 * time.Hour)
	_, err := d.phy.CreateConsolidation(d.ctx, phyapi.CreateConsolidation{CommandID: "consol-2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", Mode: phyapi.ModeSea,
		Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"), PlannedDeparture: departs.Add(6 * time.Hour),
		HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: d.hours(8)})
	d.must("create C-2", err)
	_, err = d.phy.PlanLoadUnit(d.ctx, phyapi.PlanLoadUnit{CommandID: "plan-c2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", ExpectedRevision: 1,
		Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: d.hours(8)})
	d.must("plan the unit of C-2", err)

	r, err := d.phy.TransferHouse(d.ctx, phyapi.TransferHouse{CommandID: "transfer-h04", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-1", ToConsolidationID: "C-2",
		ShipmentID: "SH-H04", ExpectedMembershipRevision: 1, Reason: phyapi.ReasonWrongConsol, OccurredAt: d.hours(26)})
	if err != nil || r.ConsolidationID != "C-2" || r.EntryNo != 2 {
		t.Fatalf("transfer: %+v %v", r, err)
	}
	m, err := d.phy.GetConsolMembership(d.ctx, phyapi.GetConsolMembership{TenantID: d.tenant, ShipmentID: "SH-H04"})
	if err != nil || !m.Live || m.Entry.ConsolidationID != "C-2" || len(m.History) != 2 || m.History[0].End != phyapi.EntryTransferred || m.History[0].ReworkNo != 0 ||
		m.History[0].ReworkExecutionID != "" || m.History[1].ReworkNo != 0 {
		t.Fatalf("the membership of H04: %+v %v", m, err)
	}

	// FUL (run): one command records the new attachment and ends the old
	plan := ful.plan(t, d, "SH-H04")
	if plan.ConsolidationID != "C-1" {
		t.Fatalf("FUL still records H04 in C-1: %+v", plan)
	}
	if _, err = ful.RecordConsolAttachment(d.ctx, fulapi.RecordConsolAttachment{CommandID: "ful-attach-c2", TenantID: d.tenant, ActorID: "ops-1", ShipmentID: "SH-H04",
		ExpectedRevision: plan.Revision, ConsolidationID: "C-2", OccurredAt: d.hours(27)}); err != nil {
		t.Fatalf("FUL records the attachment to C-2: %v", err)
	}
	if plan = ful.plan(t, d, "SH-H04"); plan.ConsolidationID != "C-2" || plan.MembershipEntryNo != 2 {
		t.Fatalf("FUL after the transfer: %+v", plan)
	}

	// C-2 is closed, its container stuffed from the lot of H04 and sealed: 5 W/M
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
	d.loadAt("X-2", "H04", 6, 30)
	d.sealAt("X-2", 31)
	b2, err := d.phy.GetPhysicalCostBasis(d.ctx, phyapi.GetPhysicalCostBasis{TenantID: d.tenant, ConsolidationID: "C-2"})
	d.must("C-2's cost basis", err)
	if len(b2.Houses) != 1 || b2.Houses[0].ShipmentID != "SH-H04" || b2.Houses[0].LoadedPacks != 6 || b2.PackingLists[0].ExecutionID != "X-2" || b2.PackingLists[0].ExecutionState != phyapi.ExecutionClosed {
		t.Fatalf("C-2's cost basis: %+v", b2)
	}

	// C-1 has H01 and H02 left, each loaded as planned, and its container sealed: it reconciles
	if err = d.reconcile("reconcile-c1", 32); err != nil {
		t.Fatalf("reconcile C-1: %v", err)
	}

	// FIN (run), after the transfer: P-2 is opened on C-2 with one line of CNY 100.01, apportioned on the one house;
	// JOB-H04 holds 3,333 from P-1 (line 1, apportioned at the stage PLANNED before the leaving) and 10,001 from P-2.
	// Until V2.93 it was charged in both pools (13,334), and nothing said so; since V2.93 the 3,333 is a former share
	// pending finance's attribution and the job's cost is 10,001. P-1 cannot close as it stands; once finance
	// attributes the share and line 1 is apportioned again, on H01 and H02, it closes and the job's cost is still
	// 10,001.
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
	if _, err = previewAndSubmit(d.ctx, d.fin, finapi.PreviewConsolCostApportionment{CommandID: "fin-apportion-p2", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-2",
		ExpectedRevision: p2.Revision, LineNos: []int{1}, OccurredAt: d.hours(34)}); err != nil {
		t.Fatalf("apportion P-2: %v", err)
	}
	if got := d.jobCost("JOB-H04"); got != 10_001 {
		t.Fatalf("JOB-H04 is charged by P-2 only, its share of P-1 is pending: %d, want 10,001", got)
	}
	if jp, err := d.fin.GetJobProfit(d.ctx, finapi.GetJobProfit{TenantID: d.tenant, JobID: "JOB-H04"}); err != nil || jp.FormerSharesPending.Minor != 3_333 || len(jp.FormerShares) != 1 {
		t.Fatalf("JOB-H04 lists its former share of P-1 apart: %+v %v", jp, err)
	}
	if err = d.closeInvoicedPool("P-1", 3); err == nil || !strings.Contains(err.Error(), "FIN_POOL_NOT_CLOSABLE") || !strings.Contains(err.Error(), "former share of house SH-H04") {
		t.Fatalf("P-1 closes with the PLANNED share of the house that left not attributed: %v", err)
	}
	if err = d.attribute("fin-attribute", "fin-1", statement("H04", 0, 0, "")); err != nil {
		t.Fatalf("attribute the former share of line 1 of P-1: %v", err)
	}
	if err = d.closePoolAgain("P-1"); err != nil {
		t.Fatalf("close P-1 after line 1 is apportioned again: %v", err)
	}
	if got := d.jobCost("JOB-H04"); got != 10_001 {
		t.Fatalf("JOB-H04 is charged by P-2 only: %d, want 10,001", got)
	}
}

// TestSEAFinalizesAfterAShutOutHouseLeftOnceItsAssociationIsEnded runs SEA's movement of C-1 through the round (the
// three houses associated before the leaving) and asserts what today's SEA does after H04 left C-1 and FUL recorded it,
// as for a house unloaded in a rework (house_leaves_integration_test.go): SEA cannot refresh the house, nor finalize
// the movement (STALE_HOUSE, naming H04), while H04's association stands; SEA's own command ends it and the movement
// finalizes with H01 and H02. The step that makes SEA follow the leaving is PR 3.
func TestSEAFinalizesAfterAShutOutHouseLeftOnceItsAssociationIsEnded(t *testing.T) {
	ctx := context.Background()
	d := shutOutDay(t, "cargo-ocean-shutsea-")
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
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H04"} {
		_, err = seaModule.AssociateHouseBill(ctx, seaapi.AssociateHouseBill{CommandID: "hbl-" + s, TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1", ExpectedRevision: movement().Revision, ShipmentID: s, OccurredAt: d.hours(11)})
		d.must("hbl "+s, err)
	}
	ful := &fulmoduleHandle{fulOf(t)}
	d.detachH04(26)
	plan := ful.plan(t, d, "SH-H04")
	_, err = ful.RecordConsolDetachment(ctx, fulapi.RecordConsolDetachment{CommandID: "ful-detach", TenantID: d.tenant, ActorID: "ops-1", ShipmentID: "SH-H04", ExpectedRevision: plan.Revision, ConsolidationID: "C-1", OccurredAt: d.hours(27)})
	d.must("ful detach", err)
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H04"} {
		p := ful.plan(t, d, s)
		_, err = ful.RecordVerifiedLoadEvidence(ctx, fulapi.RecordVerifiedLoadEvidence{CommandID: "ful-ev-" + s, TenantID: d.tenant, ActorID: "ops-1", ShipmentID: s, ExpectedRevision: p.Revision, OccurredAt: d.hours(28)})
		d.must("FUL takes the evidence of "+s, err)
		_, err = seaModule.RefreshHouseBillFacts(ctx, seaapi.RefreshHouseBillFacts{CommandID: "refresh-" + s, TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1", ExpectedRevision: movement().Revision, ShipmentID: s, OccurredAt: d.hours(28)})
		if s == "SH-H04" {
			if err == nil || !strings.Contains(err.Error(), `SH-H04 is PLANNED SEA LCL in "" of LE`) {
				t.Fatalf("SEA refreshes the house that left: %v", err)
			}
		} else {
			d.must("SEA refreshes "+s, err)
		}
	}
	r, err := seaModule.AssignContainer(ctx, seaapi.AssignContainer{CommandID: "assign", TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-1", MovementID: "OM-1", LoadUnitNo: 1, ContainerNumber: "csqu3054383", SizeType: "22G1", TareKg: 2180, MaxGrossKg: 30480, PlateSource: seaapi.PlateCSC, OccurredAt: d.hours(12)})
	d.must("assign", err)
	visit := r.NewRevision
	r, err = seaModule.RecordContainerSeal(ctx, seaapi.RecordContainerSeal{CommandID: "sea-seal", TenantID: d.tenant, ActorID: "cfs-1", VisitID: "CV-1", ExpectedRevision: visit, Number: "seal-x-1", Party: seaapi.SealShipper, AppliedAt: d.hours(25), Source: "CFS", OccurredAt: d.hours(25)})
	d.must("SEA's seal", err)
	visit = r.NewRevision
	// V2.88: a stuffing is not taken while a house FUL no longer reports in the consolidation is still associated
	_, err = seaModule.RecordStuffingDone(ctx, seaapi.RecordStuffingDone{CommandID: "stuffed-early", TenantID: d.tenant, ActorID: "cfs-1", VisitID: "CV-1", ExpectedRevision: visit, OccurredAt: d.hours(26)})
	if err == nil || !strings.Contains(err.Error(), "STALE_HOUSE") || !strings.Contains(err.Error(), "SH-H04") {
		t.Fatalf("SEA takes the stuffing with the house that left still associated: %v", err)
	}
	_, err = seaModule.EndHouseBillAssociation(ctx, seaapi.EndHouseBillAssociation{CommandID: "end-h04", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: movement().Revision, ShipmentID: "SH-H04", OccurredAt: d.hours(29)})
	d.must("end the association of H04", err)
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
	d.must("finalize", finalize("finalize-2"))
	facts, err := seaModule.GetOceanDocFacts(ctx, seaapi.GetOceanDocFacts{TenantID: d.tenant, MovementID: "OM-1"})
	if err != nil || facts.Status != seaapi.DocFactsFinal || facts.Version != 1 || len(facts.Houses) != 2 || len(facts.Containers) != 1 {
		t.Fatalf("the FINAL facts: %+v %v", facts, err)
	}
}
