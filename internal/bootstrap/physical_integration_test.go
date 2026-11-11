//go:build integration

package bootstrap

import (
	"context"
	"testing"
	"time"

	airapi "go.idp.xyz/idp-cargo/internal/contexts/air/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// PHY V0.2 with every real context, scenario CW-GAP-E2E-01 (an air partial shortload found at receipt), as the
// S1 API client runs the steps (T6): two customers' AIR houses are attached to C-1, recorded in FUL and given
// HAWBs under MAWB 781-12345675. At the forwarder's warehouse house A arrives whole, house B one carton short;
// both are weighed and screened, and receipt records B's shortage and weight variance. FUL replans B to the five
// cartons received and confirms it again; PHY refreshes the house, FUL records it, AIR refreshes its waybill
// facts, and the shortage is resolved as FUL_REPLANNED. C-1 is closed, its eleven cartons are tendered to the
// airline terminal, the airline's departure is entered by hand (OD-4 C), C-1 is reconciled and the lots closed.
// FUL could now read PhysicalLoadEvidence v1 of every house.
func TestShortloadReceivedTenderedAndReconciledWithEveryContext(t *testing.T) {
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
	bookModule, err := platform.BOOK(orgModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	airModule, err := platform.AIR(bookModule, fulModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-tender-")
	seedORG(t, platform, tenant, "party-a", "party-b", "airline-mu")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	must := func(r phyapi.Receipt, err error) phyapi.Receipt {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// Houses A and B in C-1, recorded in FUL, with HAWBs under the booking's MAWB.
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	contexts.planAir(t, "party-a", "A")
	contexts.planAir(t, "party-b", "B")
	must(phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		Mode: phyapi.ModeAir, Kind: phyapi.KindAirConsol, OperatingLegalEntityID: "LE", Origin: iataCode("PVG"), Destination: iataCode("JFK"),
		PlannedDeparture: departs.Add(6 * time.Hour), HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: hours(8)}))
	for _, s := range []string{"SH-A", "SH-B"} {
		must(phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-" + s, TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
			ShipmentID: s, OccurredAt: hours(9)}))
		record(t, ctx, fulModule, tenant, "ful-attach-"+s, s, "C-1", 3, hours(9))
	}
	contexts.airline(t)
	contexts.bookAir(t, bookModule, "BK-1", "C-1", "781-12345675", departs.Add(6*time.Hour))
	if _, err = airModule.OpenAirMovement(ctx, airapi.OpenAirMovement{CommandID: "open", TenantID: tenant, ActorID: "ops-1", MovementID: "AM-1",
		ConsolidationID: "C-1", BookingID: "BK-1", OccurredAt: hours(12)}); err != nil {
		t.Fatal(err)
	}
	if _, err = airModule.RegisterHawbSeries(ctx, airapi.RegisterHawbSeries{CommandID: "series", TenantID: tenant, ActorID: "ops-1", LegalEntityID: "LE",
		Prefix: "SHA", Width: 7, Next: 1, OccurredAt: hours(12)}); err != nil {
		t.Fatal(err)
	}
	for i, s := range []string{"SH-A", "SH-B"} {
		if _, err = airModule.AssociateHouseWaybill(ctx, airapi.AssociateHouseWaybill{CommandID: "hawb-" + s, TenantID: tenant, ActorID: "ops-1",
			MovementID: "AM-1", ExpectedRevision: int64(1 + i), ShipmentID: s, OccurredAt: hours(13)}); err != nil {
			t.Fatal(err)
		}
	}

	// Receipt at the warehouse at the consolidation's origin: A whole, B one carton short.
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	if err != nil {
		t.Fatal(err)
	}
	warehouse := phyapi.Facility{Location: c.Current.Origin, Code: "SHA-WH1"}
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}
	for _, lot := range []struct {
		id, shipment string
		packs, kg    int64
	}{{"L-A", "SH-A", 6, 300}, {"L-B", "SH-B", 5, 250}} {
		must(phyModule.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv-" + lot.id, TenantID: tenant, ActorID: "wh-1", LotID: lot.id,
			ShipmentID: lot.shipment, Facility: warehouse, Packs: lot.packs, PackageType: "CT", Condition: phyapi.ConditionGood,
			Evidence: evidence("WMS", "GRN-"+lot.id, 14), OccurredAt: hours(14)}))
		must(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh-" + lot.id, TenantID: tenant, ActorID: "wh-1",
			LotID: lot.id, ExpectedRevision: 1, GrossWeight: kg(lot.kg), Method: phyapi.MethodScale, Evidence: evidence("SCALE-3", "W-"+lot.id, 14),
			OccurredAt: hours(14)}))
		must(phyModule.RecordScreeningResult(ctx, phyapi.RecordScreeningResult{CommandID: "screen-" + lot.id, TenantID: tenant, ActorID: "wh-1",
			LotID: lot.id, ExpectedRevision: 2, Outcome: phyapi.ScreeningPassed, Screener: "PVG cargo security station", Method: "XRAY",
			Reference: "AQ-" + lot.id, Evidence: evidence("PVG-SECURITY", "AQ-"+lot.id, 14), OccurredAt: hours(14)}))
		must(phyModule.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete-" + lot.id, TenantID: tenant, ActorID: "wh-1", LotID: lot.id,
			ExpectedRevision: 3, OccurredAt: hours(15)}))
	}
	open, err := phyModule.ListOpenDiscrepancies(ctx, phyapi.ListOpenDiscrepancies{TenantID: tenant, ShipmentID: "SH-B"})
	if err != nil || len(open) != 2 || open[0].Kind != "SHORT" || open[0].Expected != 6 || open[0].Observed != 5 || open[1].Kind != "WEIGHT_VARIANCE" ||
		open[0].ConsolidationID != "C-1" {
		t.Fatalf("B one carton short: %+v %v", open, err)
	}

	// FUL replans B to the five cartons received; PHY, FUL and AIR follow.
	if _, err = fulModule.ChangeShipmentPart(ctx, fulapi.ChangeShipmentPart{CommandID: "shortload-B", TenantID: tenant, ActorID: "planner-1",
		ShipmentID: "SH-B", ExpectedRevision: 4, ObligationID: "OB-B", ObligationVersion: 1, Packs: 5, GrossWeight: kg(250),
		Reason: "one carton short at receipt", OccurredAt: hours(16)}); err != nil {
		t.Fatal(err)
	}
	if _, err = fulModule.ConfirmShipmentPlan(ctx, fulapi.ConfirmShipmentPlan{CommandID: "reconfirm-B", TenantID: tenant, ActorID: "planner-2",
		ShipmentID: "SH-B", ExpectedRevision: 5, OccurredAt: hours(16)}); err != nil {
		t.Fatal(err)
	}
	must(phyModule.RefreshHouseIntent(ctx, phyapi.RefreshHouseIntent{CommandID: "refresh-B", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ShipmentID: "SH-B", ExpectedMembershipRevision: 1, OccurredAt: hours(17)}))
	record(t, ctx, fulModule, tenant, "ful-refresh-B", "SH-B", "C-1", 6, hours(17))
	if _, err = airModule.RefreshHouseWaybillFacts(ctx, airapi.RefreshHouseWaybillFacts{CommandID: "air-refresh-B", TenantID: tenant, ActorID: "ops-1",
		MovementID: "AM-1", ExpectedRevision: 3, ShipmentID: "SH-B", OccurredAt: hours(17)}); err != nil {
		t.Fatal(err)
	}
	must(phyModule.ResolveCargoDiscrepancy(ctx, phyapi.ResolveCargoDiscrepancy{CommandID: "resolve-short", TenantID: tenant, ActorID: "cs-1",
		LotID: "L-B", ExpectedRevision: 4, DiscrepancyNo: 1, Resolution: phyapi.ResolvedByReplan, OccurredAt: hours(18)}))
	must(phyModule.ResolveCargoDiscrepancy(ctx, phyapi.ResolveCargoDiscrepancy{CommandID: "resolve-weight", TenantID: tenant, ActorID: "cs-1",
		LotID: "L-B", ExpectedRevision: 5, DiscrepancyNo: 2, Resolution: phyapi.ResolvedByAccepted, Reason: "the weight of the carton short",
		OccurredAt: hours(18)}))

	// Cut-off, tender to the airline terminal, the airline's departure, reconciliation.
	must(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: 1, OccurredAt: hours(18)}))
	terminal := phyapi.Facility{Location: c.Current.Origin, Code: "PVG-EASTERN"}
	must(phyModule.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "tender", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1",
		ConsolidationID: "C-1", Kind: phyapi.ExecutionTender, Facility: terminal, OccurredAt: hours(19)}))
	must(phyModule.StartLoading(ctx, phyapi.StartLoading{CommandID: "start", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 1,
		OccurredAt: hours(19)}))
	for i, lot := range []struct {
		id    string
		packs int64
	}{{"L-A", 6}, {"L-B", 5}} {
		must(phyModule.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load-" + lot.id, TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1",
			ExpectedRevision: int64(2 + i), LotID: lot.id, Packs: lot.packs, Evidence: evidence("TRUCK-APP", "DLV-"+lot.id, 19), OccurredAt: hours(19)}))
	}
	must(phyModule.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "tender-receipt", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1",
		ExpectedRevision: 4, Evidence: evidence("PVG-EASTERN", "JJD-20261111-0042", 19), OccurredAt: hours(19)}))
	must(phyModule.RecordCarrierLoadEvidence(ctx, phyapi.RecordCarrierLoadEvidence{CommandID: "departed", TenantID: tenant, ActorID: "ops-1",
		ExecutionID: "X-1", ExpectedRevision: 5, Fact: phyapi.CarrierDeparted, Pieces: 11, GrossWeight: kg(550), Transport: "MU587",
		Evidence: evidence("MU-CARGO", "FSU-DEP-781-12345675", 20), OccurredAt: hours(20)}))
	must(phyModule.ReconcileConsolidation(ctx, phyapi.ReconcileConsolidation{CommandID: "reconcile", TenantID: tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", ExpectedRevision: 2, OccurredAt: hours(21)}))
	for i, lot := range []string{"L-A", "L-B"} {
		must(phyModule.CloseCargoLot(ctx, phyapi.CloseCargoLot{CommandID: "close-" + lot, TenantID: tenant, ActorID: "wh-1", LotID: lot,
			ExpectedRevision: []int64{4, 6}[i], OccurredAt: hours(21)}))
	}

	if c, err = phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"}); err != nil ||
		c.State != phyapi.StateReconciled || len(c.Members) != 2 {
		t.Fatalf("C-1 reconciled: %+v %v", c, err)
	}
	evidenceB, err := phyModule.GetPhysicalLoadEvidence(ctx, phyapi.GetPhysicalLoadEvidence{TenantID: tenant, ShipmentID: "SH-B"})
	if err != nil || evidenceB.ReceivedPacks != 5 || evidenceB.LoadedPacks != 5 || evidenceB.Lots[0].State != "CLOSED" ||
		evidenceB.Lots[0].Measurement.GrossWeight != kg(250) || evidenceB.Tenders[0].GrossWeight != kg(250) ||
		evidenceB.Tenders[0].CarrierEvidence[0].Fact != phyapi.CarrierDeparted || evidenceB.Tenders[0].CloseEvidence.Reference != "JJD-20261111-0042" {
		t.Fatalf("PhysicalLoadEvidence v1 of SH-B: %+v %v", evidenceB, err)
	}
	m, err := airModule.GetAirMovement(ctx, airapi.GetAirMovement{TenantID: tenant, MovementID: "AM-1"})
	if err != nil || m.Pieces != 11 || m.GrossWeight.Amount != 550_000 || len(m.Houses) != 2 {
		t.Fatalf("the master after B's shortload: %d pieces, %+v, %v", m.Pieces, m.GrossWeight, err)
	}
	if plan, err := fulModule.GetLoadIntent(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-B"}); err != nil || plan.Packs != 5 ||
		plan.ConsolidationID != "C-1" {
		t.Fatalf("FUL plans the five cartons tendered: %+v %v", plan, err)
	}
}
