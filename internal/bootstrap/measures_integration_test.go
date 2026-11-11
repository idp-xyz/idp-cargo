//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	airapi "go.idp.xyz/idp-cargo/internal/contexts/air/api"
	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FUL V0.4 (second part) and AIR V0.2 with every real context, as the S1 API client runs the steps (T6): house
// A is associated under MU's MAWB with planned quantities. PHY receives it at the warehouse, weighs it at 303 kg
// and the airport security station clears it; FUL records PHY's evidence and AIR refreshes the house to the
// received measures. The consolidation is closed, the house tendered and weighed again at the terminal (318 kg);
// FUL records it, and AIR documents the tendered measures with the chargeable weight on them. AIR V0.3
// finalizes the facts for DOC (AirDocFacts v1): the HAWB with the terminal's measures, SPX with the security
// station's screening evidence and the goods description COM recorded. The airline retimes the flight: AIR
// relinks, the movement is OPEN again and version 1 superseded; version 2 is finalized on the new flight. The
// airline's departure makes the shipment DEPARTED in FUL, which then refuses to cancel it; AIR has nothing to
// refresh and version 2 stays current.
func TestMeasuresReachTheHouseWaybillWithEveryContext(t *testing.T) {
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
	tenant := uniqueTenant("cargo-measures-")
	seedORG(t, platform, tenant, "party-a", "airline-mu")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}
	phyOK := func(r phyapi.Receipt, err error) phyapi.Receipt {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	fulRevision := int64(4) // after planning (3) and recording the attachment
	recordEvidence := func(command string, h int) fulapi.Receipt {
		t.Helper()
		r, err := fulModule.RecordVerifiedLoadEvidence(ctx, fulapi.RecordVerifiedLoadEvidence{CommandID: command, TenantID: tenant, ActorID: "ops-1",
			ShipmentID: "SH-A", ExpectedRevision: fulRevision, OccurredAt: hours(h)})
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		fulRevision = r.NewRevision
		return r
	}
	airRevision := int64(2) // after opening and associating
	refresh := func(command string, h int) (airapi.Receipt, error) {
		r, err := airModule.RefreshHouseWaybillFacts(ctx, airapi.RefreshHouseWaybillFacts{CommandID: command, TenantID: tenant, ActorID: "ops-1",
			MovementID: "AM-1", ExpectedRevision: airRevision, ShipmentID: "SH-A", OccurredAt: hours(h)})
		if err == nil {
			airRevision = r.NewRevision
		}
		return r, err
	}
	house := func() airapi.House {
		t.Helper()
		m, err := airModule.GetAirMovement(ctx, airapi.GetAirMovement{TenantID: tenant, MovementID: "AM-1"})
		if err != nil || len(m.Houses) != 1 {
			t.Fatalf("movement: %+v %v", m, err)
		}
		return m.Houses[0]
	}

	// House A in C-1, recorded in FUL, under MU's MAWB with a HAWB and its planned quantities.
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	contexts.planAir(t, "party-a", "A")
	phyOK(phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		Mode: phyapi.ModeAir, Kind: phyapi.KindAirConsol, OperatingLegalEntityID: "LE", Origin: iataCode("PVG"), Destination: iataCode("JFK"),
		PlannedDeparture: departs.Add(6 * time.Hour), HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: hours(8)}))
	phyOK(phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ShipmentID: "SH-A", OccurredAt: hours(9)}))
	record(t, ctx, fulModule, tenant, "ful-attach", "SH-A", "C-1", 3, hours(9))
	contexts.airline(t)
	contexts.bookAir(t, bookModule, "BK-1", "C-1", "781-12345675", departs.Add(6*time.Hour))
	if _, err = airModule.OpenAirMovement(ctx, airapi.OpenAirMovement{CommandID: "open", TenantID: tenant, ActorID: "ops-1", MovementID: "AM-1",
		ConsolidationID: "C-1", BookingID: "BK-1", OccurredAt: hours(12)}); err != nil {
		t.Fatal(err)
	}
	if _, err = airModule.AssociateHouseWaybill(ctx, airapi.AssociateHouseWaybill{CommandID: "hawb", TenantID: tenant, ActorID: "ops-1",
		MovementID: "AM-1", ExpectedRevision: 1, ShipmentID: "SH-A", HAWB: "SHA0000001", OccurredAt: hours(12)}); err != nil {
		t.Fatal(err)
	}
	if h := house(); h.Basis != airapi.BasisPlanned || h.GrossWeight != kg(300) {
		t.Fatalf("planned: %+v", h)
	}

	// Receipt at the warehouse: FUL records it, AIR takes the received measures.
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	if err != nil {
		t.Fatal(err)
	}
	phyOK(phyModule.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv", TenantID: tenant, ActorID: "wh-1", LotID: "L-A", ShipmentID: "SH-A",
		Facility: phyapi.Facility{Location: c.Current.Origin, Code: "SHA-WH1"}, Packs: 6, PackageType: "CT", Condition: phyapi.ConditionGood,
		Evidence: evidence("WMS", "GRN-1", 13), OccurredAt: hours(13)}))
	phyOK(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh", TenantID: tenant, ActorID: "wh-1", LotID: "L-A",
		ExpectedRevision: 1, GrossWeight: kg(303), Volume: kernel.Quantity{Unit: "MTQ", Amount: 2}, Method: phyapi.MethodScale,
		Evidence: evidence("SCALE-3", "W-1", 13), OccurredAt: hours(13)}))
	if _, err = refresh("air-early", 13); !errors.Is(err, airapi.ErrRejected) {
		t.Fatalf("FUL relays nothing new yet: %v", err)
	}
	phyOK(phyModule.RecordScreeningResult(ctx, phyapi.RecordScreeningResult{CommandID: "screen", TenantID: tenant, ActorID: "wh-1", LotID: "L-A",
		ExpectedRevision: 2, Outcome: phyapi.ScreeningPassed, Screener: "PVG cargo security station", Method: "XRAY", Reference: "AQ-1",
		Evidence: evidence("PVG-SECURITY", "AQ-1", 13), OccurredAt: hours(13)}))
	phyOK(phyModule.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete", TenantID: tenant, ActorID: "wh-1", LotID: "L-A",
		ExpectedRevision: 3, OccurredAt: hours(14)}))
	recordEvidence("ful-received", 14)
	if _, err = refresh("air-received", 14); err != nil {
		t.Fatal(err)
	}
	if h := house(); h.Basis != airapi.BasisReceived || h.GrossWeight != kg(303) || h.Volume.Amount != 2 || h.SecurityStatus != airapi.SecurityCleared ||
		h.ChargeableWeight.Amount != 333_500 || h.HAWB != "SHA0000001" || h.EvidenceVersion != 1 {
		t.Fatalf("received measures, 2 m3 volumetric 333.4 kg: %+v", h)
	}

	// Cut-off, tender, terminal weight: FUL records it, AIR documents the tendered measures.
	phyOK(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: 1, OccurredAt: hours(15)}))
	phyOK(phyModule.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "tender", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1",
		ConsolidationID: "C-1", Kind: phyapi.ExecutionTender, Facility: phyapi.Facility{Location: c.Current.Origin, Code: "PVG-EASTERN"}, OccurredAt: hours(16)}))
	phyOK(phyModule.StartLoading(ctx, phyapi.StartLoading{CommandID: "start", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 1,
		OccurredAt: hours(16)}))
	phyOK(phyModule.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1",
		ExpectedRevision: 2, LotID: "L-A", Packs: 6, Evidence: evidence("TRUCK-APP", "DLV-1", 16), OccurredAt: hours(16)}))
	phyOK(phyModule.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "tender-receipt", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1",
		ExpectedRevision: 3, Evidence: evidence("PVG-EASTERN", "JJD-1", 16), OccurredAt: hours(16)}))
	phyOK(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "terminal-weight", TenantID: tenant, ActorID: "ops-1",
		LotID: "L-A", ExpectedRevision: 4, GrossWeight: kg(318), Volume: kernel.Quantity{Unit: "MTQ", Amount: 1}, Method: phyapi.MethodTerminalWeight,
		Evidence: evidence("PVG-EASTERN", "WGT-1", 17), OccurredAt: hours(17)}))
	recordEvidence("ful-tendered", 17)
	if _, err = refresh("air-tendered", 17); err != nil {
		t.Fatal(err)
	}
	if h := house(); h.Basis != airapi.BasisTendered || h.GrossWeight != kg(318) || h.ChargeableWeight.Amount != 318_000 || h.EvidenceVersion != 2 {
		t.Fatalf("the terminal's weight documented: %+v", h)
	}
	if plan, err := fulModule.GetAirOperationPlan(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-A"}); err != nil ||
		plan.Packs != 6 || plan.GrossWeight != kg(300) || plan.MeasuredGrossWeight != kg(318) || plan.ExecutionStatus != fulapi.ExecutionNotStarted ||
		len(plan.Screenings) != 1 || plan.Screenings[0].Reference != "AQ-1" || plan.Parts[0].GoodsDescription != "Cartons of electronics" {
		t.Fatalf("the plan stays beside the measures: %+v %v", plan, err)
	}

	// AIR V0.3: the facts finalized for DOC.
	finalize := func(command string, h int) airapi.Receipt {
		t.Helper()
		r, err := airModule.FinalizeAirDocFacts(ctx, airapi.FinalizeAirDocFacts{CommandID: command, TenantID: tenant, ActorID: "ops-2", MovementID: "AM-1",
			ExpectedRevision: airRevision, OccurredAt: hours(h)})
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		airRevision = r.NewRevision
		return r
	}
	docFacts := func(version int) airapi.AirDocFacts {
		t.Helper()
		f, err := airModule.GetAirDocFacts(ctx, airapi.GetAirDocFacts{TenantID: tenant, MovementID: "AM-1", Version: version})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	if r := finalize("finalize-1", 17); r.FactsVersion != 1 {
		t.Fatalf("version 1: %+v", r)
	}
	v1 := docFacts(0)
	if v1.Version != 1 || v1.Status != airapi.DocFactsFinal || v1.MAWB != "781-12345675" || v1.AirlineDesignator != "MU" || v1.SecurityStatus != airapi.SecuritySPX ||
		v1.Pieces != 6 || v1.GrossWeight.Amount != 318_000 || v1.ChargeableWeight.Amount != 318_000 || v1.Origin.IATA != "PVG" || v1.Destination.IATA != "JFK" ||
		len(v1.Flights) != 1 || !v1.Flights[0].ETD.Equal(departs.Add(6*time.Hour)) || len(v1.Houses) != 1 {
		t.Fatalf("master of version 1: %+v", v1)
	}
	if h := v1.Houses[0]; h.HAWB != "SHA0000001" || h.ShipmentID != "SH-A" || h.Basis != airapi.BasisTendered || h.Volume.Amount != 1_000_000 ||
		len(h.Screenings) != 1 || h.Screenings[0].Outcome != "PASSED" || h.Screenings[0].Screener != "PVG cargo security station" ||
		h.Screenings[0].Method != "XRAY" || h.Screenings[0].Reference != "AQ-1" || len(h.Parts) != 1 || h.Parts[0].GoodsDescription != "Cartons of electronics" ||
		h.Parts[0].OrderID != "O-A" || h.Parts[0].Packs != 6 {
		t.Fatalf("house of version 1: %+v", h)
	}

	// The airline retimes the flight within the house's window: AIR relinks, version 1 is superseded, version 2 finalized.
	retimed := departs.Add(8 * time.Hour)
	if _, err = bookModule.RecordCarrierChange(ctx, bookapi.RecordCarrierChange{CommandID: "retime", TenantID: tenant, ActorID: "ops-1", BookingID: "BK-1",
		ExpectedRevision: 3, Reason: bookapi.CapacityScheduleChange, Answer: bookapi.Answer{Channel: bookapi.ChannelEmail,
			Evidence: bookapi.Evidence{SHA256: strings.Repeat("5b", 32), StorageID: "mail/mu-2.eml", MediaType: "message/rfc822", CapturedAt: hours(18)}},
		Lines: []bookapi.CapacityLine{{Capacity: bookapi.Capacity{Pieces: 20, GrossWeight: kg(1000)},
			Legs: []bookapi.ConfirmedLeg{{Loading: iataCode("PVG"), Discharge: iataCode("JFK"), Flight: "MU587", ETD: retimed, ETA: retimed.Add(14 * time.Hour)}}}},
		OccurredAt: hours(18)}); err != nil {
		t.Fatal(err)
	}
	link, err := airModule.LinkCarrierBooking(ctx, airapi.LinkCarrierBooking{CommandID: "relink", TenantID: tenant, ActorID: "ops-1", MovementID: "AM-1",
		ExpectedRevision: airRevision, OccurredAt: hours(18)})
	if err != nil {
		t.Fatal(err)
	}
	airRevision = link.NewRevision
	if old := docFacts(0); old.Version != 1 || old.Status != airapi.DocFactsSuperseded || old.SHA256 != v1.SHA256 {
		t.Fatalf("superseded: %+v", old)
	}
	if r := finalize("finalize-2", 19); r.FactsVersion != 2 {
		t.Fatalf("version 2: %+v", r)
	}
	if v2 := docFacts(2); v2.Status != airapi.DocFactsFinal || v2.CapacityVersion != 2 || !v2.Flights[0].ETD.Equal(retimed) || v2.SHA256 == v1.SHA256 {
		t.Fatalf("version 2: %+v", v2)
	}

	// The airline's departure: DEPARTED in FUL, which no longer cancels the shipment; nothing for AIR to refresh.
	phyOK(phyModule.RecordCarrierLoadEvidence(ctx, phyapi.RecordCarrierLoadEvidence{CommandID: "departed", TenantID: tenant, ActorID: "ops-1",
		ExecutionID: "X-1", ExpectedRevision: 4, Fact: phyapi.CarrierDeparted, Pieces: 6, Transport: "MU587",
		Evidence: evidence("MU-CARGO", "FSU-DEP-1", 20), OccurredAt: hours(20)}))
	recordEvidence("ful-departed", 20)
	if plan, err := fulModule.GetAirOperationPlan(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-A"}); err != nil ||
		plan.ExecutionStatus != fulapi.ExecutionDeparted || plan.EvidenceVersion != 3 {
		t.Fatalf("departed: %+v %v", plan, err)
	}
	if _, err = fulModule.CancelShipment(ctx, fulapi.CancelShipment{CommandID: "cancel", TenantID: tenant, ActorID: "planner-1", ShipmentID: "SH-A",
		ExpectedRevision: fulRevision, Reason: "customer withdrew", OccurredAt: hours(21)}); !errors.Is(err, fulapi.ErrRejected) {
		t.Fatalf("a departed shipment is not cancelled: %v", err)
	}
	if _, err = refresh("air-after-departure", 21); !errors.Is(err, airapi.ErrRejected) {
		t.Fatalf("the departure changes no house fact: %v", err)
	}
	if latest := docFacts(0); latest.Version != 2 || latest.Status != airapi.DocFactsFinal {
		t.Fatalf("version 2 stays current: %+v", latest)
	}
}
