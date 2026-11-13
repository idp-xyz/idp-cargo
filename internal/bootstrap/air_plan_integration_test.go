//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FUL V0.3 with the real contexts and the S1 location reference data: an AIR order is planned by air only
// (C11); the route is given with an IATA airport code and resolved to UN/LOCODEs, a city of several airports
// is refused, and the plan freezes the IATA codes and the edition (F2, option 4); business times later than
// the time FUL records them are refused (T1).
func TestAirPlanAgainstTheLocationReferenceData(t *testing.T) {
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
	fulModule, err := platform.FUL(comModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-air-")
	seedORG(t, platform, tenant, "party-a")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "party-a", ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: at, EvidenceRef: "contract", OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, err = rateModule.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "quote", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1", LegalEntityID: "LE",
		CustomerPartyID: "party-a", Currency: "USD", Lines: []rateapi.QuoteLine{{ChargeCode: "AIR_FREIGHT", Sell: usd(100_000), Cost: usd(50_000)}},
		Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err = rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1",
		ExpectedRevision: 1, EvidenceKind: rateapi.EvidenceEmail, EvidenceRef: "mail", OccurredAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	kg := kernel.Quantity{Unit: "KGM", Amount: 300}
	if _, err = comModule.RecordCustomerOrder(ctx, comapi.RecordCustomerOrder{CommandID: "order", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1",
		QuoteID: "Q-1", CustomerPartyID: "party-a", OccurredAt: at.Add(2 * time.Hour), Transport: comapi.Transport{Mode: comapi.ModeAir},
		Lines: []comapi.OrderLine{{Description: "Cartons of electronics", Packs: 6, PackageType: "CT", GrossWeight: kg}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = comModule.AllocateOrderToObligation(ctx, comapi.AllocateOrderToObligation{CommandID: "allocate", TenantID: tenant, ActorID: "planner-1",
		OrderID: "O-1", ExpectedRevision: 1, LineNo: 1, ObligationID: "OB-1", Packs: 6, GrossWeight: kg, OccurredAt: at.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = comModule.ReserveObligationForFulfillment(ctx, comapi.ReserveObligationForFulfillment{CommandID: "reserve", TenantID: tenant,
		ActorID: "fulfilment", ObligationID: "OB-1", ObligationVersion: 1, ShipmentID: "SH-1", OccurredAt: at.Add(4 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = fulModule.CreateShipmentFromObligation(ctx, fulapi.CreateShipmentFromObligation{CommandID: "create-future", TenantID: tenant,
		ActorID: "planner-1", ShipmentID: "SH-1", ObligationID: "OB-1", ObligationVersion: 1, OccurredAt: time.Now().UTC().Add(time.Hour)}); !errors.Is(err, fulapi.ErrRejected) {
		t.Fatalf("a business time an hour after FUL records the command: %v", err)
	}
	if _, err = fulModule.CreateShipmentFromObligation(ctx, fulapi.CreateShipmentFromObligation{CommandID: "create", TenantID: tenant,
		ActorID: "planner-1", ShipmentID: "SH-1", ObligationID: "OB-1", ObligationVersion: 1, OccurredAt: at.Add(5 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	departs := at.Add(72 * time.Hour)
	amend := func(command string, revision int64, mode string, origin, destination kernel.LocationCode) error {
		_, err := fulModule.AmendShipmentPlan(ctx, fulapi.AmendShipmentPlan{CommandID: command, TenantID: tenant, ActorID: "planner-1",
			ShipmentID: "SH-1", ExpectedRevision: revision, TransportMode: mode, Origin: origin, Destination: destination,
			DepartureWindow: kernel.ValidityInterval{From: departs, Until: departs.Add(24 * time.Hour)}, Reason: "flight booked", OccurredAt: at.Add(6 * time.Hour)})
		return err
	}
	iata := func(code string) kernel.LocationCode {
		return kernel.LocationCode{Scheme: kernel.IATAAirport, Code: code}
	}
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	if err = amend("sea", 1, fulapi.ModeSea, unlocode("CNSHA"), unlocode("DEHAM")); !errors.Is(err, fulapi.ErrRejected) {
		t.Fatalf("SEA for an order that requests AIR: %v", err)
	}
	if err = amend("nyc", 1, fulapi.ModeAir, iata("PVG"), unlocode("USNYC")); !errors.Is(err, fulapi.ErrRejected) {
		t.Fatalf("New York, a city of several airports: %v", err)
	}
	if err = amend("nyc-iata", 1, fulapi.ModeAir, iata("PVG"), iata("NYC")); !errors.Is(err, fulapi.ErrRejected) {
		t.Fatalf("the IATA city code NYC: %v", err)
	}
	if err = amend("jfk", 1, fulapi.ModeAir, iata("PVG"), unlocode("USJFK")); err != nil {
		t.Fatalf("PVG to JFK: %v", err)
	}
	plan, err := fulModule.GetShipmentPlanSnapshotV2(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-1"})
	if err != nil || plan.Origin != unlocode("CNPVG") || plan.OriginIATA != "PVG" || plan.Destination != unlocode("USJFK") || plan.DestinationIATA != "JFK" ||
		plan.LocationEdition != "UNLOCODE-2026-11-01-S1" || plan.EnteredAt.IsZero() || !plan.EnteredAt.After(plan.PlannedAt) {
		t.Fatalf("frozen air plan: %+v %v", plan, err)
	}
	if _, err = fulModule.ConfirmShipmentPlan(ctx, fulapi.ConfirmShipmentPlan{CommandID: "confirm", TenantID: tenant, ActorID: "planner-2",
		ShipmentID: "SH-1", ExpectedRevision: 2, OccurredAt: at.Add(7 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// The customer switches the order to SEA; the AIR plan can then no longer be changed or confirmed as AIR.
	if _, err = comModule.ChangeOrderTransport(ctx, comapi.ChangeOrderTransport{CommandID: "to-sea", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1",
		ExpectedRevision: 3, Transport: comapi.Transport{Mode: comapi.ModeSea}, Reason: "the customer chose sea freight", OccurredAt: at.Add(8 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	later := departs.Add(24 * time.Hour)
	if _, err = fulModule.AmendShipmentPlan(ctx, fulapi.AmendShipmentPlan{CommandID: "later-flight", TenantID: tenant, ActorID: "planner-1",
		ShipmentID: "SH-1", ExpectedRevision: 3, TransportMode: fulapi.ModeAir, Origin: iata("PVG"), Destination: iata("JFK"),
		DepartureWindow: kernel.ValidityInterval{From: later, Until: later.Add(24 * time.Hour)}, Reason: "later flight", OccurredAt: at.Add(9 * time.Hour)}); !errors.Is(err, fulapi.ErrRejected) {
		t.Fatalf("an AIR plan for an order that now requests SEA: %v", err)
	}
	if _, err = fulModule.AmendShipmentPlan(ctx, fulapi.AmendShipmentPlan{CommandID: "by-sea", TenantID: tenant, ActorID: "planner-1",
		ShipmentID: "SH-1", ExpectedRevision: 3, TransportMode: fulapi.ModeSea, Origin: unlocode("CNSHA"), Destination: unlocode("USLAX"),
		DepartureWindow: kernel.ValidityInterval{From: later, Until: later.Add(24 * time.Hour)}, Reason: "replanned by sea", OccurredAt: at.Add(9 * time.Hour)}); err != nil {
		t.Fatalf("replan by sea: %v", err)
	}
	plan, err = fulModule.GetShipmentPlanSnapshotV2(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-1"})
	if err != nil || plan.Origin != unlocode("CNSHG") || plan.OriginIATA != "" || plan.Destination != unlocode("USLAX") || plan.Status != fulapi.StatusDraft {
		t.Fatalf("sea plan with the carriers' CNSHA for the port of Shanghai: %+v %v", plan, err)
	}
}
