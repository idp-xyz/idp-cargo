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
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func (allowAll) AuthorizeAirAction(context.Context, string, string, string, string) error {
	return nil
}

// AIR V0.1 with every real context, as the S1 API client runs the steps (T6): two customers' AIR houses are
// attached to a PHY consolidation and recorded in FUL; the consolidation is booked with MU and the airline
// confirms MAWB 781-12345675; AIR opens the movement from the booking (MAWB and flights, N1-PHY OD-7) and draws a
// HAWB for each house from the legal entity's series (N1-HAWB), with planned quantities and chargeable weight. The
// airline retimes the flight under the same MAWB, and AIR relinks it; house B is rolled to the next consolidation,
// whose booking has another MAWB: AIR ends it in the first movement and associates it in the second with its HAWB.
func TestHouseWaybillsUnderTheMasterWithEveryContext(t *testing.T) {
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
	tenant := uniqueTenant("cargo-awb-")
	seedORG(t, platform, tenant, "party-a", "party-b", "airline-mu")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	iata := func(code string) kernel.LocationCode {
		return kernel.LocationCode{Scheme: kernel.IATAAirport, Code: code}
	}
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	evidence := func(h int) bookapi.Evidence {
		return bookapi.Evidence{SHA256: strings.Repeat("5a", 32), StorageID: "mail/mu.eml", MediaType: "message/rfc822", CapturedAt: hours(h)}
	}

	// Houses: two planned AIR shipments, attached to C-1 in PHY and recorded in FUL.
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	contexts.planAir(t, "party-a", "A")
	contexts.planAir(t, "party-b", "B")
	consolidation := func(id string, departure time.Time) {
		t.Helper()
		if _, err := phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol-" + id, TenantID: tenant, ActorID: "ops-1",
			ConsolidationID: id, Mode: phyapi.ModeAir, Kind: phyapi.KindAirConsol, OperatingLegalEntityID: "LE", Origin: iata("PVG"), Destination: iata("JFK"),
			PlannedDeparture: departure, HouseCutOff: departure.Add(-12 * time.Hour), OccurredAt: hours(8)}); err != nil {
			t.Fatal(err)
		}
	}
	consolidation("C-1", departs.Add(6*time.Hour))
	for _, s := range []string{"SH-A", "SH-B"} {
		if _, err = phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-" + s, TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
			ShipmentID: s, OccurredAt: hours(9)}); err != nil {
			t.Fatal(err)
		}
		record(t, ctx, fulModule, tenant, "ful-attach-"+s, s, "C-1", 3, hours(9))
	}

	// The airline: codes and CARRIER role in ORG, then the consolidation's booking, confirmed with its MAWB.
	for i, code := range [][2]string{{orgapi.IdentifierIATAAirline, "MU"}, {orgapi.IdentifierIATAAWBPrefix, "781"}} {
		if _, err = orgModule.RecordPartyIdentifier(ctx, orgapi.RecordPartyIdentifier{CommandID: "mu-" + code[0], TenantID: tenant, ActorID: "md-1",
			PartyID: "airline-mu", ExpectedRevision: int64(1 + i), Type: code[0], Country: orgapi.InternationalIssuer, Value: code[1], EvidenceRef: "IATA"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "mu-carrier", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "airline-mu", ExpectedRevision: 3, RoleCode: orgapi.RoleCarrier, ValidFrom: at.Add(-time.Hour), EvidenceRef: "contract", OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	book := func(id, consolidation, mawb string, etd time.Time) {
		t.Helper()
		kg := kernel.Quantity{Unit: "KGM", Amount: 1000}
		if _, err := bookModule.DraftCarrierBooking(ctx, bookapi.DraftCarrierBooking{CommandID: "draft-" + id, TenantID: tenant, ActorID: "ops-1", BookingID: id,
			Kind: bookapi.KindAirConsol, LegalEntityID: "LE", CarrierPartyID: "airline-mu", ConsolidationID: consolidation,
			Request: bookapi.Request{Legs: []bookapi.RequestedLeg{{Loading: iata("PVG"), Discharge: iata("JFK"), Flight: "MU587", DepartureDate: etd.Format(time.DateOnly)}},
				Capacity: bookapi.Capacity{Pieces: 20, GrossWeight: kg}}, OccurredAt: hours(10)}); err != nil {
			t.Fatal(err)
		}
		if _, err := bookModule.RecordBookingRequestSent(ctx, bookapi.RecordBookingRequestSent{CommandID: "sent-" + id, TenantID: tenant, ActorID: "ops-1",
			BookingID: id, ExpectedRevision: 1, Channel: bookapi.ChannelCarrierPortal, Evidence: evidence(10), OccurredAt: hours(10)}); err != nil {
			t.Fatal(err)
		}
		if _, err := bookModule.RecordCarrierConfirmation(ctx, bookapi.RecordCarrierConfirmation{CommandID: "confirm-" + id, TenantID: tenant, ActorID: "ops-1",
			BookingID: id, ExpectedRevision: 2, Answer: bookapi.Answer{Channel: bookapi.ChannelEmail, Evidence: evidence(11),
				References: []bookapi.CarrierReference{{Type: bookapi.ReferenceMAWBNumber, Value: mawb}}},
			Lines: []bookapi.CapacityLine{{Capacity: bookapi.Capacity{Pieces: 20, GrossWeight: kg}, Legs: []bookapi.ConfirmedLeg{{Loading: iata("PVG"),
				Discharge: iata("JFK"), Flight: "MU587", ETD: etd, ETA: etd.Add(14 * time.Hour)}}}}, OccurredAt: hours(11)}); err != nil {
			t.Fatal(err)
		}
	}
	book("BK-1", "C-1", "781-12345675", departs.Add(6*time.Hour))

	// AIR: the movement from the booking, the series, a HAWB drawn for each house.
	if _, err = airModule.OpenAirMovement(ctx, airapi.OpenAirMovement{CommandID: "open-1", TenantID: tenant, ActorID: "ops-1", MovementID: "AM-1",
		ConsolidationID: "C-1", BookingID: "BK-1", OccurredAt: hours(12)}); err != nil {
		t.Fatal(err)
	}
	if _, err = airModule.RegisterHawbSeries(ctx, airapi.RegisterHawbSeries{CommandID: "series", TenantID: tenant, ActorID: "ops-1", LegalEntityID: "LE",
		Prefix: "SHA", Width: 7, Next: 1, OccurredAt: hours(12)}); err != nil {
		t.Fatal(err)
	}
	for i, s := range []string{"SH-A", "SH-B"} {
		r, err := airModule.AssociateHouseWaybill(ctx, airapi.AssociateHouseWaybill{CommandID: "hawb-" + s, TenantID: tenant, ActorID: "ops-1", MovementID: "AM-1",
			ExpectedRevision: int64(1 + i), ShipmentID: s, OccurredAt: hours(13)})
		if err != nil || r.HAWB != []string{"SHA0000001", "SHA0000002"}[i] {
			t.Fatalf("HAWB of %s: %+v %v", s, r, err)
		}
	}
	m, err := airModule.GetAirMovement(ctx, airapi.GetAirMovement{TenantID: tenant, MovementID: "AM-1"})
	if err != nil || m.MAWB != "781-12345675" || m.AirlineDesignator != "MU" || m.Origin.IATA != "PVG" || m.LocationEdition != "UNLOCODE-2026-11-01-S1" ||
		len(m.Houses) != 2 || m.Pieces != 12 || m.GrossWeight.Amount != 600_000 || m.ChargeableWeight.Amount != 600_000 || m.Houses[0].Parts[0].PackageType != "CT" {
		t.Fatalf("the master and its houses: %+v %v", m, err)
	}

	// The airline retimes the flight under the same MAWB; AIR relinks, the houses stay.
	retimed := departs.Add(8 * time.Hour)
	if _, err = bookModule.RecordCarrierChange(ctx, bookapi.RecordCarrierChange{CommandID: "retime", TenantID: tenant, ActorID: "ops-1", BookingID: "BK-1",
		ExpectedRevision: 3, Reason: bookapi.CapacityScheduleChange, Answer: bookapi.Answer{Channel: bookapi.ChannelEmail, Evidence: evidence(14)},
		Lines: []bookapi.CapacityLine{{Capacity: bookapi.Capacity{Pieces: 20, GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 1000}},
			Legs: []bookapi.ConfirmedLeg{{Loading: iata("PVG"), Discharge: iata("JFK"), Flight: "MU587", ETD: retimed, ETA: retimed.Add(14 * time.Hour)}}}},
		OccurredAt: hours(14)}); err != nil {
		t.Fatal(err)
	}
	if _, err = airModule.LinkCarrierBooking(ctx, airapi.LinkCarrierBooking{CommandID: "relink", TenantID: tenant, ActorID: "ops-1", MovementID: "AM-1",
		ExpectedRevision: 3, OccurredAt: hours(14)}); err != nil {
		t.Fatal(err)
	}
	if m, err = airModule.GetAirMovement(ctx, airapi.GetAirMovement{TenantID: tenant, MovementID: "AM-1"}); err != nil || m.CapacityVersion != 2 ||
		!m.Flights[0].ETD.Equal(retimed) || len(m.Houses) != 2 {
		t.Fatalf("relinked: %+v %v", m.Flights, err)
	}

	// House B is rolled to C-2: PHY transfers it, FUL records it, AIR ends it under MAWB 1 and associates it under
	// C-2's MAWB with the HAWB it has.
	consolidation("C-2", departs.Add(30*time.Hour))
	if _, err = phyModule.TransferHouse(ctx, phyapi.TransferHouse{CommandID: "roll-B", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ToConsolidationID: "C-2", ShipmentID: "SH-B", ExpectedMembershipRevision: 1, Reason: phyapi.ReasonShutOut, OccurredAt: hours(15)}); err != nil {
		t.Fatal(err)
	}
	record(t, ctx, fulModule, tenant, "ful-roll-B", "SH-B", "C-2", 4, hours(15))
	if _, err = airModule.EndHouseWaybillAssociation(ctx, airapi.EndHouseWaybillAssociation{CommandID: "end-B", TenantID: tenant, ActorID: "ops-1",
		MovementID: "AM-1", ExpectedRevision: 4, ShipmentID: "SH-B", OccurredAt: hours(16)}); err != nil {
		t.Fatal(err)
	}
	book("BK-2", "C-2", "781-23456786", departs.Add(30*time.Hour))
	if _, err = airModule.OpenAirMovement(ctx, airapi.OpenAirMovement{CommandID: "open-2", TenantID: tenant, ActorID: "ops-1", MovementID: "AM-2",
		ConsolidationID: "C-2", BookingID: "BK-2", OccurredAt: hours(16)}); err != nil {
		t.Fatal(err)
	}
	if _, err = airModule.AssociateHouseWaybill(ctx, airapi.AssociateHouseWaybill{CommandID: "hawb-B-wrong", TenantID: tenant, ActorID: "ops-1", MovementID: "AM-2",
		ExpectedRevision: 1, ShipmentID: "SH-A", OccurredAt: hours(16)}); !errors.Is(err, airapi.ErrRejected) {
		t.Fatalf("a house FUL attaches to another consolidation: %v", err)
	}
	r, err := airModule.AssociateHouseWaybill(ctx, airapi.AssociateHouseWaybill{CommandID: "hawb-B-2", TenantID: tenant, ActorID: "ops-1", MovementID: "AM-2",
		ExpectedRevision: 1, ShipmentID: "SH-B", OccurredAt: hours(16)})
	if err != nil || r.HAWB != "SHA0000002" {
		t.Fatalf("SH-B under C-2's MAWB keeps its HAWB: %+v %v", r, err)
	}
	ref, err := airModule.FindHouseWaybill(ctx, airapi.FindHouseWaybill{TenantID: tenant, LegalEntityID: "LE", HAWB: "SHA0000002"})
	if err != nil || ref.ShipmentID != "SH-B" || ref.MovementID != "AM-2" {
		t.Fatalf("SHA0000002: %+v %v", ref, err)
	}
	if byMAWB, err := airModule.FindAirMovementByMAWB(ctx, airapi.FindAirMovementByMAWB{TenantID: tenant, MAWB: "781-23456786"}); err != nil ||
		byMAWB.MovementID != "AM-2" || len(byMAWB.Houses) != 1 {
		t.Fatalf("by the second MAWB: %+v %v", byMAWB, err)
	}
	if plan, err := fulModule.GetAirOperationPlan(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-B"}); err != nil ||
		plan.ConsolidationID != "C-2" || plan.MembershipRevision != 2 {
		t.Fatalf("AirOperationPlan v1 of SH-B: %+v %v", plan, err)
	}
}
