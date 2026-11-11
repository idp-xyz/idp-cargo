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
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func (allowAll) AuthorizeConsolidationAction(context.Context, string, string, string, string) error {
	return nil
}

// PHY V0.1 and FUL V0.4 with the real contexts and the S1 location reference data, as the S1 API client runs
// the steps (T6): two customers' AIR shipments from Shanghai Pudong to New York JFK share one consolidation
// (UL-05). PHY attaches each house first and FUL records the attachment after asking PHY (N1-PHY OD-1); the
// close re-confirms every house with FUL. A house shut out by the airline is transferred to the next flight;
// a shortload is replanned in FUL and refreshed in PHY; a cancelled shipment leaves.
func TestHousesShareAnAirConsolidation(t *testing.T) {
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
	tenant := uniqueTenant("cargo-consol-")
	seedORG(t, platform, tenant, "party-a", "party-b")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond) // business times lie in the past (T1)
	departs := at.Add(72 * time.Hour)
	iata := func(code string) kernel.LocationCode {
		return kernel.LocationCode{Scheme: kernel.IATAAirport, Code: code}
	}
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }

	// One planned AIR shipment per customer: 6 cartons of 300 kg, PVG to JFK, departing within days 3 to 5.
	plan := func(party, suffix string) {
		t.Helper()
		kg := kernel.Quantity{Unit: "KGM", Amount: 300}
		if err := errors.Join(
			func() error {
				_, err := orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-" + suffix, TenantID: tenant, ActorID: "org-officer",
					LegalEntityID: "LE", PartyID: party, ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: at, EvidenceRef: "contract", OccurredAt: at})
				return err
			}(),
			func() error {
				_, err := rateModule.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "quote-" + suffix, TenantID: tenant, ActorID: "sales-1",
					QuoteID: "Q-" + suffix, LegalEntityID: "LE", CustomerPartyID: party, Currency: "USD",
					Lines:    []rateapi.QuoteLine{{ChargeCode: "FREIGHT", Sell: usd(100_000), Cost: usd(50_000)}},
					Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(time.Minute)})
				return err
			}(),
			func() error {
				_, err := rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-" + suffix, TenantID: tenant, ActorID: "sales-1",
					QuoteID: "Q-" + suffix, ExpectedRevision: 1, EvidenceKind: rateapi.EvidenceEmail, EvidenceRef: "mail", OccurredAt: at.Add(time.Hour)})
				return err
			}(),
			func() error {
				_, err := comModule.RecordCustomerOrder(ctx, comapi.RecordCustomerOrder{CommandID: "order-" + suffix, TenantID: tenant, ActorID: "cs-1",
					OrderID: "O-" + suffix, QuoteID: "Q-" + suffix, CustomerPartyID: party, OccurredAt: at.Add(2 * time.Hour),
					Transport: comapi.Transport{Mode: comapi.ModeAir},
					Lines:     []comapi.OrderLine{{Description: "Cartons of electronics", Packs: 6, PackageType: "CT", GrossWeight: kg}}})
				return err
			}(),
			func() error {
				_, err := comModule.AllocateOrderToObligation(ctx, comapi.AllocateOrderToObligation{CommandID: "allocate-" + suffix, TenantID: tenant,
					ActorID: "planner-1", OrderID: "O-" + suffix, ExpectedRevision: 1, LineNo: 1, ObligationID: "OB-" + suffix, Packs: 6,
					GrossWeight: kg, OccurredAt: at.Add(3 * time.Hour)})
				return err
			}(),
			func() error {
				_, err := comModule.ReserveObligationForFulfillment(ctx, comapi.ReserveObligationForFulfillment{CommandID: "reserve-" + suffix,
					TenantID: tenant, ActorID: "fulfilment", ObligationID: "OB-" + suffix, ObligationVersion: 1, ShipmentID: "SH-" + suffix,
					OccurredAt: at.Add(4 * time.Hour)})
				return err
			}(),
			func() error {
				_, err := fulModule.CreateShipmentFromObligation(ctx, fulapi.CreateShipmentFromObligation{CommandID: "create-" + suffix, TenantID: tenant,
					ActorID: "planner-1", ShipmentID: "SH-" + suffix, ObligationID: "OB-" + suffix, ObligationVersion: 1, OccurredAt: at.Add(5 * time.Hour)})
				return err
			}(),
			func() error {
				_, err := fulModule.AmendShipmentPlan(ctx, fulapi.AmendShipmentPlan{CommandID: "plan-" + suffix, TenantID: tenant, ActorID: "planner-1",
					ShipmentID: "SH-" + suffix, ExpectedRevision: 1, TransportMode: fulapi.ModeAir, Origin: iata("PVG"), Destination: iata("JFK"),
					DepartureWindow: kernel.ValidityInterval{From: departs, Until: departs.Add(48 * time.Hour)}, Reason: "flight booked",
					OccurredAt: at.Add(6 * time.Hour)})
				return err
			}(),
			func() error {
				_, err := fulModule.ConfirmShipmentPlan(ctx, fulapi.ConfirmShipmentPlan{CommandID: "confirm-" + suffix, TenantID: tenant,
					ActorID: "planner-2", ShipmentID: "SH-" + suffix, ExpectedRevision: 2, OccurredAt: at.Add(7 * time.Hour)})
				return err
			}(),
		); err != nil {
			t.Fatal(err)
		}
	}
	plan("party-a", "A")
	plan("party-b", "B")

	create := func(id string, departure time.Time) {
		t.Helper()
		if _, err := phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol-" + id, TenantID: tenant, ActorID: "ops-1",
			ConsolidationID: id, Mode: phyapi.ModeAir, Kind: phyapi.KindAirConsol, OperatingLegalEntityID: "LE", Origin: iata("PVG"),
			Destination: unlocode("USJFK"), PlannedDeparture: departure, HouseCutOff: departure.Add(-12 * time.Hour), OccurredAt: at.Add(8 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	create("C-1", departs.Add(6*time.Hour))
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	if err != nil || c.Current.Origin != unlocode("CNPVG") || c.Current.OriginIATA != "PVG" || c.Current.DestinationIATA != "JFK" ||
		c.Current.LocationEdition != "UNLOCODE-2026-11-01-S1" {
		t.Fatalf("a route given by IATA code is frozen as UN/LOCODE with its IATA codes and edition: %+v %v", c.Current, err)
	}
	if _, err = phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol-nyc", TenantID: tenant, ActorID: "ops-1",
		ConsolidationID: "C-NYC", Mode: phyapi.ModeAir, Kind: phyapi.KindAirConsol, OperatingLegalEntityID: "LE", Origin: iata("PVG"),
		Destination: unlocode("USNYC"), PlannedDeparture: departs, HouseCutOff: departs, OccurredAt: at.Add(8 * time.Hour)}); !errors.Is(err, phyapi.ErrRejected) {
		t.Fatalf("New York, a city of several airports, is no AIR destination: %v", err)
	}
	if _, err = phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol-sea", TenantID: tenant, ActorID: "ops-1",
		ConsolidationID: "C-SEA", Mode: phyapi.ModeSea, Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"),
		Destination: unlocode("DEHAM"), PlannedDeparture: departs, HouseCutOff: departs.Add(-24 * time.Hour), OccurredAt: at.Add(8 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if sea, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-SEA"}); err != nil ||
		sea.Current.Origin != unlocode("CNSHG") || sea.Current.OriginIATA != "" {
		t.Fatalf("the carriers' CNSHA resolves to the port of Shanghai: %+v %v", sea.Current, err)
	}
	if _, err = phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-A-sea", TenantID: tenant, ActorID: "ops-1",
		ConsolidationID: "C-SEA", ShipmentID: "SH-A", OccurredAt: at.Add(9 * time.Hour)}); !errors.Is(err, phyapi.ErrRejected) {
		t.Fatalf("an AIR shipment in a SEA consolidation: %v", err)
	}

	// T6: PHY attaches, then FUL records the attachment after asking PHY.
	attach := func(command, shipment, consolidation string, membership, fulRevision int64, when time.Time) {
		t.Helper()
		if _, err := phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "phy-" + command, TenantID: tenant, ActorID: "ops-1",
			ConsolidationID: consolidation, ShipmentID: shipment, ExpectedMembershipRevision: membership, OccurredAt: when}); err != nil {
			t.Fatalf("attach %s: %v", shipment, err)
		}
		if fulRevision > 0 {
			record(t, ctx, fulModule, tenant, "ful-"+command, shipment, consolidation, fulRevision, when.Add(time.Minute))
		}
	}
	attach("attach-A", "SH-A", "C-1", 0, 3, at.Add(9*time.Hour))
	attach("attach-B", "SH-B", "C-1", 0, 0, at.Add(9*time.Hour))
	if _, err = fulModule.RecordConsolAttachment(ctx, fulapi.RecordConsolAttachment{CommandID: "ful-attach-B-wrong", TenantID: tenant, ActorID: "ops-1",
		ShipmentID: "SH-B", ExpectedRevision: 3, ConsolidationID: "C-2", OccurredAt: at.Add(9 * time.Hour)}); !errors.Is(err, fulapi.ErrRejected) {
		t.Fatalf("FUL records only the consolidation PHY reports: %v", err)
	}
	closeC1 := func(command string, revision int64, when time.Time) error {
		_, err := phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: command, TenantID: tenant, ActorID: "ops-1",
			ConsolidationID: "C-1", ExpectedRevision: revision, OccurredAt: when})
		return err
	}
	if err = closeC1("close-early", 1, at.Add(10*time.Hour)); !errors.Is(err, phyapi.ErrRejected) {
		t.Fatalf("close while FUL has not recorded SH-B's attachment: %v", err)
	}
	record(t, ctx, fulModule, tenant, "ful-attach-B", "SH-B", "C-1", 3, at.Add(10*time.Hour))
	if err = closeC1("close", 1, at.Add(11*time.Hour)); err != nil {
		t.Fatal(err)
	}
	intent, err := fulModule.GetLoadIntent(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-A"})
	if err != nil || intent.ConsolidationID != "C-1" || intent.MembershipEntryNo != 1 || intent.Parts[0].PackageType != "CT" || intent.Revision != 4 {
		t.Fatalf("LoadIntent v1 of SH-A: %+v %v", intent, err)
	}

	// The airline shuts SH-B out: reopen, transfer it to the next flight, record it in FUL.
	if _, err = phyModule.ReopenConsolidation(ctx, phyapi.ReopenConsolidation{CommandID: "reopen", TenantID: tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", ExpectedRevision: 2, Reason: "SH-B shut out", OccurredAt: at.Add(12 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	create("C-2", departs.Add(30*time.Hour))
	if _, err = phyModule.TransferHouse(ctx, phyapi.TransferHouse{CommandID: "transfer-B", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ToConsolidationID: "C-2", ShipmentID: "SH-B", ExpectedMembershipRevision: 1, Reason: phyapi.ReasonShutOut, OccurredAt: at.Add(13 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	record(t, ctx, fulModule, tenant, "ful-transfer-B", "SH-B", "C-2", 4, at.Add(13*time.Hour))

	// A shortload of SH-A: FUL replans 4 of the 6 cartons, the plan is confirmed again, and PHY refreshes the house.
	if _, err = fulModule.ChangeShipmentPart(ctx, fulapi.ChangeShipmentPart{CommandID: "shortload-A", TenantID: tenant, ActorID: "planner-1",
		ShipmentID: "SH-A", ExpectedRevision: 4, ObligationID: "OB-A", ObligationVersion: 1, Packs: 4, GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 200},
		Reason: "two cartons not ready", OccurredAt: at.Add(14 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err = closeC1("close-stale", 3, at.Add(15*time.Hour)); !errors.Is(err, phyapi.ErrRejected) {
		t.Fatalf("close while SH-A's plan changed: %v", err)
	}
	if _, err = fulModule.ConfirmShipmentPlan(ctx, fulapi.ConfirmShipmentPlan{CommandID: "reconfirm-A", TenantID: tenant, ActorID: "planner-2",
		ShipmentID: "SH-A", ExpectedRevision: 5, OccurredAt: at.Add(15 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = phyModule.RefreshHouseIntent(ctx, phyapi.RefreshHouseIntent{CommandID: "refresh-A", TenantID: tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", ShipmentID: "SH-A", ExpectedMembershipRevision: 1, OccurredAt: at.Add(16 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	record(t, ctx, fulModule, tenant, "ful-refresh-A", "SH-A", "C-1", 6, at.Add(16*time.Hour))
	if err = closeC1("close-again", 3, at.Add(17*time.Hour)); err != nil {
		t.Fatal(err)
	}
	c, err = phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	if err != nil || c.State != phyapi.StateClosed || len(c.Members) != 1 || c.Members[0].ShipmentID != "SH-A" || c.Members[0].Packs != 4 || c.Members[0].EntryNo != 2 {
		t.Fatalf("C-1 after the rollover and the shortload: %+v %v", c, err)
	}

	// The customer of SH-B cancels: FUL's cancellation ends its attachment, then PHY ends the membership.
	if _, err = fulModule.CancelShipment(ctx, fulapi.CancelShipment{CommandID: "cancel-B", TenantID: tenant, ActorID: "planner-1", ShipmentID: "SH-B",
		ExpectedRevision: 5, Reason: "order withdrawn", OccurredAt: at.Add(18 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if intent, err = fulModule.GetLoadIntent(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-B"}); err != nil ||
		intent.Status != fulapi.StatusCancelled || intent.ConsolidationID != "" {
		t.Fatalf("a cancelled shipment keeps no attachment: %+v %v", intent, err)
	}
	if _, err = phyModule.EndMembershipForCancelledShipment(ctx, phyapi.EndMembershipForCancelledShipment{CommandID: "end-B", TenantID: tenant,
		ActorID: "ops-1", ConsolidationID: "C-2", ShipmentID: "SH-B", ExpectedMembershipRevision: 2, OccurredAt: at.Add(19 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	membership, err := phyModule.GetConsolMembership(ctx, phyapi.GetConsolMembership{TenantID: tenant, ShipmentID: "SH-B"})
	if err != nil || membership.Live || len(membership.History) != 2 || membership.History[0].ConsolidationID != "C-1" ||
		membership.History[0].End != phyapi.EntryTransferred || membership.History[1].End != phyapi.EntryShipmentCancelled {
		t.Fatalf("SH-B's history keeps the original consolidation: %+v %v", membership, err)
	}
	if _, err = phyModule.CancelConsolidation(ctx, phyapi.CancelConsolidation{CommandID: "cancel-C-2", TenantID: tenant, ActorID: "ops-1",
		ConsolidationID: "C-2", ExpectedRevision: 1, Reason: "no houses left", OccurredAt: at.Add(20 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// SH-A leaves C-1 after reopening; FUL records the detachment.
	if _, err = phyModule.ReopenConsolidation(ctx, phyapi.ReopenConsolidation{CommandID: "reopen-2", TenantID: tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", ExpectedRevision: 4, Reason: "customer moves SH-A to sea", OccurredAt: at.Add(21 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = phyModule.DetachHouse(ctx, phyapi.DetachHouse{CommandID: "detach-A", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ShipmentID: "SH-A", ExpectedMembershipRevision: 2, Reason: phyapi.ReasonCustomerRequest, OccurredAt: at.Add(22 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = fulModule.RecordConsolDetachment(ctx, fulapi.RecordConsolDetachment{CommandID: "ful-detach-A", TenantID: tenant, ActorID: "ops-1",
		ShipmentID: "SH-A", ExpectedRevision: 7, ConsolidationID: "C-1", OccurredAt: at.Add(22 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if intent, err = fulModule.GetLoadIntent(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-A"}); err != nil || intent.ConsolidationID != "" {
		t.Fatalf("SH-A after leaving C-1: %+v %v", intent, err)
	}
}

// record records in FUL the consolidation PHY reports for a shipment.
func record(t *testing.T, ctx context.Context, ful fulapi.ConsolAttachmentCommands, tenant, command, shipment, consolidation string, revision int64, when time.Time) {
	t.Helper()
	if _, err := ful.RecordConsolAttachment(ctx, fulapi.RecordConsolAttachment{CommandID: command, TenantID: tenant, ActorID: "ops-1",
		ShipmentID: shipment, ExpectedRevision: revision, ConsolidationID: consolidation, OccurredAt: when}); err != nil {
		t.Fatalf("FUL records %s in %s: %v", shipment, consolidation, err)
	}
}
