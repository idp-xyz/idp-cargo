//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	bentoapp "go.idp.xyz/idp-bento-go/application"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Step 4 of the S1 slice with the real contexts: an order allocated to two obligations in COM, a shipment
// planned from them in FUL, and the C6 handshake. COM reserves before FUL plans, FUL releases before COM
// revokes, and FUL notices when COM no longer holds an obligation for the shipment.
func TestShipmentPlanFromReservedObligations(t *testing.T) {
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
	tenant := uniqueTenant("cargo-shipment-")
	seedORG(t, platform, tenant, "party-s")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond) // business times lie in the past (T1)
	if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "customer-role", TenantID: tenant, ActorID: "org-officer",
		LegalEntityID: "LE", PartyID: "party-s", ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: at, ValidUntil: at.Add(30 * 24 * time.Hour),
		EvidenceRef: "customer-contract", OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, err = rateModule.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "create-Q-1", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1",
		LegalEntityID: "LE", CustomerPartyID: "party-s", Currency: "USD", Lines: []rateapi.QuoteLine{{ChargeCode: "AIR_FREIGHT", Sell: usd(100_000), Cost: usd(50_000)}},
		Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err = rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-Q-1", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1",
		ExpectedRevision: 1, EvidenceKind: rateapi.EvidenceSignedDocument, EvidenceRef: "signed-Q-1", OccurredAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	if _, err = comModule.RecordCustomerOrder(ctx, comapi.RecordCustomerOrder{CommandID: "record-O-1", TenantID: tenant, ActorID: "sales-1",
		OrderID: "O-1", QuoteID: "Q-1", CustomerPartyID: "party-s", OccurredAt: at.Add(2 * time.Hour),
		Lines: []comapi.OrderLine{{Description: "Pallets of machine parts", Packs: 10, PackageType: "PX", GrossWeight: kg(500)}}}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"OB-1", "OB-2"} {
		if _, err = comModule.AllocateOrderToObligation(ctx, comapi.AllocateOrderToObligation{CommandID: "allocate-" + id, TenantID: tenant,
			ActorID: "planner-1", OrderID: "O-1", ExpectedRevision: int64(i + 1), LineNo: 1, ObligationID: id, Packs: 5, GrossWeight: kg(250),
			OccurredAt: at.Add(3 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	reserve := func(command, obligation, shipment string) error {
		_, err := comModule.ReserveObligationForFulfillment(ctx, comapi.ReserveObligationForFulfillment{CommandID: command, TenantID: tenant,
			ActorID: "fulfilment", ObligationID: obligation, ObligationVersion: 1, ShipmentID: shipment, OccurredAt: at.Add(4 * time.Hour)})
		return err
	}
	release := func(command, obligation, shipment string) error {
		_, err := comModule.ReleaseObligationReservation(ctx, comapi.ReleaseObligationReservation{CommandID: command, TenantID: tenant,
			ActorID: "fulfilment", ObligationID: obligation, ObligationVersion: 1, ShipmentID: shipment, OccurredAt: at.Add(7 * time.Hour)})
		return err
	}
	create := fulapi.CreateShipmentFromObligation{CommandID: "create-SH-1", TenantID: tenant, ActorID: "planner-1", ShipmentID: "SH-1",
		ObligationID: "OB-1", ObligationVersion: 1, OccurredAt: at.Add(5 * time.Hour)}

	// FUL plans an obligation only once COM holds it for the shipment.
	if _, err = fulModule.CreateShipmentFromObligation(ctx, create); !errors.Is(err, fulapi.ErrRejected) {
		t.Fatalf("plan an obligation COM has not reserved: err = %v", err)
	}
	if err = errors.Join(reserve("reserve-OB-1", "OB-1", "SH-1"), reserve("reserve-OB-2", "OB-2", "SH-1")); err != nil {
		t.Fatal(err)
	}
	create.CommandID = "create-SH-1-again"
	if _, err = fulModule.CreateShipmentFromObligation(ctx, create); err != nil {
		t.Fatal(err)
	}
	if _, err = fulModule.AddObligationToShipment(ctx, fulapi.AddObligationToShipment{CommandID: "add-OB-2", TenantID: tenant, ActorID: "planner-1",
		ShipmentID: "SH-1", ExpectedRevision: 1, ObligationID: "OB-2", ObligationVersion: 1, OccurredAt: at.Add(5 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	departs := at.Add(72 * time.Hour)
	amend := fulapi.AmendShipmentPlan{CommandID: "amend-1", TenantID: tenant, ActorID: "planner-1", ShipmentID: "SH-1", ExpectedRevision: 2,
		TransportMode: fulapi.ModeSea, Origin: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHA"},
		Destination: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "DEHAM"}, DepartureWindow: kernel.ValidityInterval{From: departs, Until: departs.Add(48 * time.Hour)},
		Reason: "carrier schedule received", OccurredAt: at.Add(5 * time.Hour)}
	if _, err = fulModule.AmendShipmentPlan(ctx, amend); err != nil {
		t.Fatal(err)
	}
	if _, err = fulModule.ConfirmShipmentPlan(ctx, fulapi.ConfirmShipmentPlan{CommandID: "confirm-1", TenantID: tenant, ActorID: "planner-2",
		ShipmentID: "SH-1", ExpectedRevision: 3, OccurredAt: at.Add(6 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	plan, err := fulModule.GetShipmentPlanSnapshot(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-1"})
	if err != nil || plan.Status != fulapi.StatusPlanned || plan.Packs != 10 || plan.GrossWeight != kg(500) || plan.CustomerPartyID != "party-s" ||
		plan.LegalEntityID != "LE" || len(plan.Obligations) != 2 || plan.Obligations[0].OrderID != "O-1" {
		t.Fatalf("shipment plan %+v, %v", plan, err)
	}

	// COM refuses to revoke a planned obligation; FUL releases it first, then COM's reservation goes.
	if _, err = comModule.RevokeObligation(ctx, comapi.RevokeObligation{CommandID: "revoke-OB-1", TenantID: tenant, ActorID: "planner-1",
		OrderID: "O-1", ExpectedRevision: 5, ObligationID: "OB-1", OccurredAt: at.Add(8 * time.Hour)}); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("revoke an obligation SH-1 holds: err = %v", err)
	}
	if _, err = fulModule.ReleaseObligationFromShipment(ctx, fulapi.ReleaseObligationFromShipment{CommandID: "release-OB-1", TenantID: tenant,
		ActorID: "planner-1", ShipmentID: "SH-1", ExpectedRevision: 4, ObligationID: "OB-1", ObligationVersion: 1, OccurredAt: at.Add(7 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err = release("unreserve-OB-1", "OB-1", "SH-1"); err != nil {
		t.Fatal(err)
	}
	if r, err := comModule.RevokeObligation(ctx, comapi.RevokeObligation{CommandID: "revoke-OB-1-after", TenantID: tenant, ActorID: "planner-1",
		OrderID: "O-1", ExpectedRevision: 6, ObligationID: "OB-1", OccurredAt: at.Add(8 * time.Hour)}); err != nil || r.NewRevision != 7 {
		t.Fatalf("revoke after FUL released it: %+v, %v", r, err)
	}

	// A reservation released in COM without FUL releasing first, which COM cannot see, is caught when
	// FUL confirms again.
	if err = release("unreserve-OB-2", "OB-2", "SH-1"); err != nil {
		t.Fatal(err)
	}
	if _, err = fulModule.ConfirmShipmentPlan(ctx, fulapi.ConfirmShipmentPlan{CommandID: "confirm-2", TenantID: tenant, ActorID: "planner-2",
		ShipmentID: "SH-1", ExpectedRevision: 5, OccurredAt: at.Add(9 * time.Hour)}); !errors.Is(err, fulapi.ErrRejected) {
		t.Fatalf("confirm a plan whose obligation COM no longer holds for it: err = %v", err)
	}

	// FUL, like the other contexts, refuses to run inside another context's transaction.
	other, err := platform.newDB()
	if err != nil {
		t.Fatal(err)
	}
	err = other.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
		if _, err := fulModule.GetShipmentPlanSnapshot(txCtx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-1"}); !errors.Is(err, bentoapp.ErrTransactionConflict) {
			t.Errorf("FUL query inside another context's transaction: err = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
