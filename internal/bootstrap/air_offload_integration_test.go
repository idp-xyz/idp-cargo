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

// The owner's scenario 1 of stage 4 (验收标准_阶段4_执行中的异常.md; V2.111), with every context on PostgreSQL: houses A
// and B of C-1, 6 cartons each, are tendered under one master. The airline flies 10 on MU587 of the first day and
// offloads 2 of A, then flies them on MU587 of the next day under the same master. FUL shows A partly departed with 2
// awaiting a later flight, on its work list, then departed on two flights; PHY reconciles C-1 once the offload is
// accepted; the master and both HAWBs stay as they were.
func TestTheAirlineFliesTheOffloadedPiecesOnTheNextFlightWithEveryContext(t *testing.T) {
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
	tenant := uniqueTenant("cargo-offload-")
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
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}

	// A and B in C-1, recorded in FUL, with HAWBs under the booking's MAWB; received, weighed, screened
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
		Template: "SHA{seq:7}", FirstNumber: 1, OccurredAt: hours(12)}); err != nil {
		t.Fatal(err)
	}
	for i, s := range []string{"SH-A", "SH-B"} {
		if _, err = airModule.AssociateHouseWaybill(ctx, airapi.AssociateHouseWaybill{CommandID: "hawb-" + s, TenantID: tenant, ActorID: "ops-1",
			MovementID: "AM-1", ExpectedRevision: int64(1 + i), ShipmentID: s, OccurredAt: hours(13)}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := airModule.GetAirMovement(ctx, airapi.GetAirMovement{TenantID: tenant, MovementID: "AM-1"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	if err != nil {
		t.Fatal(err)
	}
	warehouse := phyapi.Facility{Location: c.Current.Origin, Code: "SHA-WH1"}
	for _, lot := range []struct{ id, shipment string }{{"L-A", "SH-A"}, {"L-B", "SH-B"}} {
		must(phyModule.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv-" + lot.id, TenantID: tenant, ActorID: "wh-1", LotID: lot.id,
			ShipmentID: lot.shipment, Facility: warehouse, Packs: 6, PackageType: "CT", Condition: phyapi.ConditionGood,
			Evidence: evidence("WMS", "GRN-"+lot.id, 14), OccurredAt: hours(14)}))
		must(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh-" + lot.id, TenantID: tenant, ActorID: "wh-1",
			LotID: lot.id, ExpectedRevision: 1, GrossWeight: kg(300), Method: phyapi.MethodScale, Evidence: evidence("SCALE-3", "W-"+lot.id, 14),
			OccurredAt: hours(14)}))
		must(phyModule.RecordScreeningResult(ctx, phyapi.RecordScreeningResult{CommandID: "screen-" + lot.id, TenantID: tenant, ActorID: "wh-1",
			LotID: lot.id, ExpectedRevision: 2, Outcome: phyapi.ScreeningPassed, Screener: "PVG cargo security station", Method: "XRAY",
			Reference: "AQ-" + lot.id, Evidence: evidence("PVG-SECURITY", "AQ-"+lot.id, 14), OccurredAt: hours(14)}))
		must(phyModule.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete-" + lot.id, TenantID: tenant, ActorID: "wh-1", LotID: lot.id,
			ExpectedRevision: 3, OccurredAt: hours(15)}))
	}

	// cut-off and tender of the twelve cartons
	must(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: 1, OccurredAt: hours(18)}))
	terminal := phyapi.Facility{Location: c.Current.Origin, Code: "PVG-EASTERN"}
	must(phyModule.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "tender", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1",
		ConsolidationID: "C-1", Kind: phyapi.ExecutionTender, Facility: terminal, OccurredAt: hours(19)}))
	execution := func() phyapi.LoadExecution {
		t.Helper()
		x, err := phyModule.GetLoadExecution(ctx, phyapi.GetLoadExecution{TenantID: tenant, ExecutionID: "X-1"})
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	must(phyModule.StartLoading(ctx, phyapi.StartLoading{CommandID: "start", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 1,
		OccurredAt: hours(19)}))
	for _, lot := range []string{"L-A", "L-B"} {
		must(phyModule.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load-" + lot, TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1",
			ExpectedRevision: execution().Revision, LotID: lot, Packs: 6, Evidence: evidence("TRUCK-APP", "DLV-"+lot, 19), OccurredAt: hours(19)}))
	}
	must(phyModule.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "tender-receipt", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1",
		ExpectedRevision: execution().Revision, Evidence: evidence("PVG-EASTERN", "JJD-0042", 19), OccurredAt: hours(19)}))
	carrier := func(command, fact, shipment string, pieces int64, flight string, h int) {
		t.Helper()
		must(phyModule.RecordCarrierLoadEvidence(ctx, phyapi.RecordCarrierLoadEvidence{CommandID: command, TenantID: tenant, ActorID: "ops-1",
			ExecutionID: "X-1", ExpectedRevision: execution().Revision, Fact: fact, ShipmentID: shipment, Pieces: pieces, Transport: flight,
			Evidence: evidence("MU-CARGO", "FSU-"+command, h), OccurredAt: hours(h)}))
	}
	ful := func(s string, h int) fulapi.ShipmentExecution {
		t.Helper()
		e, err := fulModule.GetShipmentExecution(ctx, fulapi.GetShipmentExecution{TenantID: tenant, ShipmentID: s})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = fulModule.RecordVerifiedLoadEvidence(ctx, fulapi.RecordVerifiedLoadEvidence{CommandID: "ful-evidence-" + s + "-" + string(rune('a'+h)),
			TenantID: tenant, ActorID: "ops-1", ShipmentID: s, ExpectedRevision: e.Revision, OccurredAt: hours(h)}); err != nil {
			t.Fatal(err)
		}
		if e, err = fulModule.GetShipmentExecution(ctx, fulapi.GetShipmentExecution{TenantID: tenant, ShipmentID: s}); err != nil {
			t.Fatal(err)
		}
		return e
	}
	awaiting := func() []fulapi.PiecesAwaitingFlight {
		t.Helper()
		got, err := fulModule.ListPiecesAwaitingFlight(ctx, fulapi.ListPiecesAwaitingFlight{TenantID: tenant, ConsolidationID: "C-1"})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// the first day: 10 depart on MU587/01DEC, 2 of A are offloaded
	carrier("dep-1", phyapi.CarrierDeparted, "", 10, "MU587/01DEC", 20)
	carrier("ofld-1", phyapi.CarrierOffloaded, "SH-A", 2, "MU587/01DEC", 20)
	a := ful("SH-A", 21)
	if a.ExecutionStatus != fulapi.ExecutionPartiallyDeparted || a.DepartedPacks != 4 || a.AwaitingLaterFlight != 2 || len(a.Flights) != 1 ||
		a.Flights[0] != (fulapi.FlightPieces{Transport: "MU587/01DEC", Departed: 4, Offloaded: 2}) {
		t.Fatalf("A after the first day: %+v", a)
	}
	if b := ful("SH-B", 21); b.ExecutionStatus != fulapi.ExecutionDeparted || b.DepartedPacks != 6 || b.AwaitingLaterFlight != 0 {
		t.Fatalf("B after the first day: %+v", b)
	}
	if got := awaiting(); len(got) != 1 || got[0].ShipmentID != "SH-A" || got[0].Pieces != 2 || got[0].ConsolidationID != "C-1" {
		t.Fatalf("the work list after the first day: %+v", got)
	}
	if err = func() error {
		_, err := phyModule.ReconcileConsolidation(ctx, phyapi.ReconcileConsolidation{CommandID: "reconcile-early", TenantID: tenant, ActorID: "ops-1",
			ConsolidationID: "C-1", ExpectedRevision: 2, OccurredAt: hours(21)})
		return err
	}(); err == nil {
		t.Fatal("C-1 reconciled with the offload open")
	}

	// the next day: the 2 of A depart on MU587/02DEC, reported for the master
	carrier("dep-2", phyapi.CarrierDeparted, "", 2, "MU587/02DEC", 44)
	a = ful("SH-A", 45)
	want := []fulapi.FlightPieces{{Transport: "MU587/01DEC", Departed: 4, Offloaded: 2}, {Transport: "MU587/02DEC", Departed: 2}}
	if a.ExecutionStatus != fulapi.ExecutionDeparted || a.DepartedPacks != 6 || a.AwaitingLaterFlight != 0 || len(a.Flights) != 2 || a.Flights[0] != want[0] ||
		a.Flights[1] != want[1] {
		t.Fatalf("A after the next day: %+v", a)
	}
	if got := awaiting(); len(got) != 0 {
		t.Fatalf("the work list after the next day: %+v", got)
	}

	// PHY: the offload accepted, C-1 reconciles; the master and both HAWBs are as they were
	var offload int
	for _, d := range execution().Discrepancies {
		if d.Kind == "OFFLOADED" && d.Resolution == "" {
			offload = d.DiscrepancyNo
		}
	}
	if offload == 0 {
		t.Fatalf("no open offload: %+v", execution().Discrepancies)
	}
	must(phyModule.ResolveLoadingDiscrepancy(ctx, phyapi.ResolveLoadingDiscrepancy{CommandID: "accept-offload", TenantID: tenant, ActorID: "ops-1",
		ExecutionID: "X-1", ExpectedRevision: execution().Revision, DiscrepancyNo: offload, Resolution: phyapi.ResolvedByAccepted,
		Reason: "the airline flew the 2 cartons on MU587/02DEC under the same master", OccurredAt: hours(45)}))
	must(phyModule.ReconcileConsolidation(ctx, phyapi.ReconcileConsolidation{CommandID: "reconcile", TenantID: tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", ExpectedRevision: 2, OccurredAt: hours(46)}))
	if c, err = phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"}); err != nil || c.State != phyapi.StateReconciled {
		t.Fatalf("C-1: %+v %v", c, err)
	}
	after, err := airModule.GetAirMovement(ctx, airapi.GetAirMovement{TenantID: tenant, MovementID: "AM-1"})
	if err != nil || after.MAWB != before.MAWB || len(after.Houses) != 2 || after.Houses[0].HAWB != before.Houses[0].HAWB ||
		after.Houses[1].HAWB != before.Houses[1].HAWB || after.CapacityVersion != before.CapacityVersion {
		t.Fatalf("the master and its houses: %+v %v", after, err)
	}
}
