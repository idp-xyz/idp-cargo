//go:build integration

package bootstrap

import (
	"context"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Sea freight part 1b-ii with the real COM, FUL and PHY and the real reference data: two LCL orders planned as SEA
// houses of an own-container consolidation, a 22G1 planned, the houses frozen, received and weighed at the CFS,
// stuffed into the container and sealed; FUL takes the sealed container's measures as tendered (OD-2).
func TestStuffingAnOwnContainerWithEveryContext(t *testing.T) {
	ctx := context.Background()
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
	fulModule, phyModule, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-stuffing-")
	seedORG(t, platform, tenant, "party-l", "party-m")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}
	must := func(r phyapi.Receipt, err error) phyapi.Receipt {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	contexts.planSea(t, "party-l", "L", comapi.ContainerLCL, fulapi.ContainerLCL)
	contexts.planSea(t, "party-m", "M", comapi.ContainerLCL, fulapi.ContainerLCL)
	must(phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		Mode: phyapi.ModeSea, Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"),
		PlannedDeparture: departs.Add(6 * time.Hour), HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: hours(8)}))
	must(phyModule.PlanLoadUnit(ctx, phyapi.PlanLoadUnit{CommandID: "plan-unit", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: 1, Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: hours(8)}))
	for _, s := range []string{"SH-L", "SH-M"} {
		must(phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-" + s, TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
			ShipmentID: s, OccurredAt: hours(9)}))
		record(t, ctx, fulModule, tenant, "ful-attach-"+s, s, "C-1", 3, hours(9))
	}
	must(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: 2, OccurredAt: hours(10)}))

	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	if err != nil {
		t.Fatal(err)
	}
	cfs := phyapi.Facility{Location: c.Current.Origin, Code: "SHA-CFS1"}
	for _, lot := range []string{"L-L", "L-M"} {
		shipment := "SH-" + lot[2:]
		must(phyModule.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv-" + lot, TenantID: tenant, ActorID: "cfs-1", LotID: lot, ShipmentID: shipment,
			Facility: cfs, Packs: 6, PackageType: "CT", Condition: phyapi.ConditionGood, Evidence: evidence("CFS-WMS", "GRN-"+lot, 11), OccurredAt: hours(11)}))
		must(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh-" + lot, TenantID: tenant, ActorID: "cfs-1", LotID: lot,
			ExpectedRevision: 1, GrossWeight: kg(300), Method: phyapi.MethodScale, Evidence: evidence("CFS-SCALE", "W-"+lot, 11), OccurredAt: hours(11)}))
		must(phyModule.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete-" + lot, TenantID: tenant, ActorID: "cfs-1", LotID: lot,
			ExpectedRevision: 2, OccurredAt: hours(11)}))
	}

	must(phyModule.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "stuff", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
		ConsolidationID: "C-1", Kind: phyapi.ExecutionStuffing, Facility: cfs, LoadUnitNo: 1, OccurredAt: hours(12)}))
	must(phyModule.StartLoading(ctx, phyapi.StartLoading{CommandID: "start", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1", ExpectedRevision: 1,
		OccurredAt: hours(12)}))
	for i, lot := range []string{"L-L", "L-M"} {
		must(phyModule.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load-" + lot, TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
			ExpectedRevision: int64(2 + i), LotID: lot, Packs: 6, Evidence: evidence("CFS-TALLY", "T-"+lot, 12), OccurredAt: hours(12)}))
	}
	must(phyModule.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "seal", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
		ExpectedRevision: 4, Evidence: evidence(phyapi.EvidenceSeal, "CN-SEAL-77120", 12), OccurredAt: hours(12)}))

	if c, err = phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"}); err != nil || len(c.LoadUnits) != 1 ||
		c.LoadUnits[0].Status != phyapi.UnitStatusClosed || c.LoadUnits[0].ExecutionID != "X-1" || c.LoadUnits[0].EquipmentEdition != "EQUIPMENT-2026-11-S1" {
		t.Fatalf("the sealed 22G1: %+v %v", c.LoadUnits, err)
	}
	if _, err = fulModule.RecordVerifiedLoadEvidence(ctx, fulapi.RecordVerifiedLoadEvidence{CommandID: "ful-stuffed", TenantID: tenant, ActorID: "ops-1",
		ShipmentID: "SH-L", ExpectedRevision: 4, OccurredAt: hours(13)}); err != nil {
		t.Fatal(err)
	}
	// FUL relays the measures in its operation plan; OceanOperationPlan v1 comes with part 1c, so the plan FUL answers
	// for any shipment is read here.
	plan, err := fulModule.GetAirOperationPlan(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-L"})
	if err != nil || plan.MeasuredBasis != fulapi.MeasuredTendered || plan.MeasuredPacks != 6 || plan.MeasuredGrossWeight != kg(300) {
		t.Fatalf("FUL's measures of SH-L: %+v %v", plan, err)
	}
}
