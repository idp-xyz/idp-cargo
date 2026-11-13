//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// F1 with the real contexts: 30 pallets are planned on one flight, the airline loads 22, and the planner
// replans the other 8 on the next flight. COM's obligation is never amended; COM holds it for both
// shipments and refuses to revoke it while either does.
func TestAShortloadIsReplannedAcrossShipments(t *testing.T) {
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
	tenant := uniqueTenant("cargo-parts-")
	seedORG(t, platform, tenant, "party-p")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond) // business times lie in the past (T1)
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	if err = errors.Join(
		func() error {
			_, err := orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "customer-role", TenantID: tenant, ActorID: "org-officer",
				LegalEntityID: "LE", PartyID: "party-p", ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: at, ValidUntil: at.Add(30 * 24 * time.Hour),
				EvidenceRef: "customer-contract", OccurredAt: at})
			return err
		}(),
		func() error {
			_, err := rateModule.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "create-Q-1", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1",
				LegalEntityID: "LE", CustomerPartyID: "party-p", Currency: "USD", Lines: []rateapi.QuoteLine{{ChargeCode: "AIR_FREIGHT", Sell: usd(300_000), Cost: usd(150_000)}},
				Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(time.Minute)})
			return err
		}(),
		func() error {
			_, err := rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-Q-1", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1",
				ExpectedRevision: 1, EvidenceKind: rateapi.EvidenceSignedDocument, EvidenceRef: "signed-Q-1", OccurredAt: at.Add(time.Hour)})
			return err
		}(),
		func() error {
			_, err := comModule.RecordCustomerOrder(ctx, comapi.RecordCustomerOrder{CommandID: "record-O-1", TenantID: tenant, ActorID: "sales-1",
				OrderID: "O-1", QuoteID: "Q-1", CustomerPartyID: "party-p", OccurredAt: at.Add(2 * time.Hour),
				Lines: []comapi.OrderLine{{Description: "Pallets of machine parts", Packs: 30, PackageType: "PX", GrossWeight: kg(1500)}}})
			return err
		}(),
		func() error {
			_, err := comModule.AllocateOrderToObligation(ctx, comapi.AllocateOrderToObligation{CommandID: "allocate-OB-1", TenantID: tenant,
				ActorID: "planner-1", OrderID: "O-1", ExpectedRevision: 1, LineNo: 1, ObligationID: "OB-1", Packs: 30, GrossWeight: kg(1500),
				OccurredAt: at.Add(3 * time.Hour)})
			return err
		}(),
	); err != nil {
		t.Fatal(err)
	}
	order := int64(2) // the revision of O-1 after recording and allocating; every COM receipt below updates it
	reserve := func(command, shipment string, when time.Time) error {
		r, err := comModule.ReserveObligationForFulfillment(ctx, comapi.ReserveObligationForFulfillment{CommandID: command, TenantID: tenant,
			ActorID: "fulfilment", ObligationID: "OB-1", ObligationVersion: 1, ShipmentID: shipment, OccurredAt: when})
		if err == nil {
			order = r.NewRevision
		}
		return err
	}
	revoke := func(command string, when time.Time) error {
		_, err := comModule.RevokeObligation(ctx, comapi.RevokeObligation{CommandID: command, TenantID: tenant, ActorID: "planner-1",
			OrderID: "O-1", ExpectedRevision: order, ObligationID: "OB-1", OccurredAt: when})
		return err
	}

	// All 30 pallets planned on MU587.
	if err = reserve("reserve-SH-A", "SH-A", at.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = fulModule.CreateShipmentFromObligation(ctx, fulapi.CreateShipmentFromObligation{CommandID: "create-SH-A", TenantID: tenant,
		ActorID: "planner-1", ShipmentID: "SH-A", ObligationID: "OB-1", ObligationVersion: 1, OccurredAt: at.Add(5 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// MU587 loads 22: the part of SH-A shrinks, COM is not asked to amend anything.
	if _, err = fulModule.ChangeShipmentPart(ctx, fulapi.ChangeShipmentPart{CommandID: "shortload-SH-A", TenantID: tenant, ActorID: "planner-1",
		ShipmentID: "SH-A", ExpectedRevision: 1, ObligationID: "OB-1", ObligationVersion: 1, Packs: 22, GrossWeight: kg(1100),
		Reason: "MU587 loaded 22 of 30 pallets", OccurredAt: at.Add(6 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// The other 8 go on the next flight, in SH-B, which COM now holds OB-1 for as well.
	if _, err = fulModule.CreateShipmentFromObligation(ctx, fulapi.CreateShipmentFromObligation{CommandID: "create-SH-B-early", TenantID: tenant,
		ActorID: "planner-1", ShipmentID: "SH-B", ObligationID: "OB-1", ObligationVersion: 1, OccurredAt: at.Add(7 * time.Hour)}); !errors.Is(err, fulapi.ErrRejected) {
		t.Fatalf("plan before COM holds OB-1 for SH-B: err = %v", err)
	}
	if err = reserve("reserve-SH-B", "SH-B", at.Add(7*time.Hour)); err != nil {
		t.Fatalf("a second shipment reserves the same obligation (F1): %v", err)
	}
	if _, err = fulModule.CreateShipmentFromObligation(ctx, fulapi.CreateShipmentFromObligation{CommandID: "create-SH-B", TenantID: tenant,
		ActorID: "planner-1", ShipmentID: "SH-B", ObligationID: "OB-1", ObligationVersion: 1, OccurredAt: at.Add(8 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	b, err := fulModule.GetShipmentPlanSnapshotV2(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-B"})
	if err != nil || b.Packs != 8 || b.GrossWeight != kg(400) || len(b.Parts) != 1 || b.Parts[0].ObligationPacks != 30 {
		t.Fatalf("SH-B plans the rest: %+v, %v", b, err)
	}
	v3, err := comModule.GetObligationSnapshotV3(ctx, comapi.GetObligationSnapshot{TenantID: tenant, ObligationID: "OB-1", Version: 1})
	if err != nil || !slices.Equal(v3.HeldByShipmentIDs, []string{"SH-A", "SH-B"}) || v3.Packs != 30 {
		t.Fatalf("COM holds OB-1 for both, unamended: %+v, %v", v3, err)
	}
	if v2, err := comModule.GetObligationSnapshotV2(ctx, comapi.GetObligationSnapshot{TenantID: tenant, ObligationID: "OB-1", Version: 1}); err != nil || v2.HeldByShipmentID != "" {
		t.Fatalf("v2 names a holder only when there is exactly one: %+v, %v", v2, err)
	}
	if err = revoke("revoke-1", at.Add(9*time.Hour)); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("revoke while two shipments hold OB-1: err = %v", err)
	}
	// Cancelling SH-B and releasing its reservation still leaves SH-A holding OB-1.
	if _, err = fulModule.CancelShipment(ctx, fulapi.CancelShipment{CommandID: "cancel-SH-B", TenantID: tenant, ActorID: "planner-1",
		ShipmentID: "SH-B", ExpectedRevision: 1, Reason: "consignee collects", OccurredAt: at.Add(10 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	released, err := comModule.ReleaseObligationReservation(ctx, comapi.ReleaseObligationReservation{CommandID: "unreserve-SH-B", TenantID: tenant,
		ActorID: "fulfilment", ObligationID: "OB-1", ObligationVersion: 1, ShipmentID: "SH-B", OccurredAt: at.Add(10 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	order = released.NewRevision
	if err = revoke("revoke-2", at.Add(11*time.Hour)); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("revoke while SH-A still holds OB-1: err = %v", err)
	}
}
