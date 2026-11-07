//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	bentoapp "go.idp.xyz/idp-bento-go/application"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Steps 1 to 3 of the S1 slice with the real contexts: ORG approves a CUSTOMER role, RATE accepts a quote,
// and COM records the customer's order against it and allocates the order to fulfillment obligations,
// asking RATE and ORG through their query sides.
func TestOrderFromAnAcceptedQuote(t *testing.T) {
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
	tenant := uniqueTenant("cargo-order-")
	seedORG(t, platform, tenant, "party-s", "party-t")

	at := time.Now().UTC().Truncate(time.Microsecond)
	roleEnds := at.Add(10 * 24 * time.Hour)
	if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "order-role", TenantID: tenant, ActorID: "org-officer",
		LegalEntityID: "LE", PartyID: "party-s", ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: at, ValidUntil: roleEnds,
		EvidenceRef: "customer-contract", OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	accepted := func(id string) {
		t.Helper()
		if _, err := rateModule.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "create-" + id, TenantID: tenant, ActorID: "sales-1",
			QuoteID: id, LegalEntityID: "LE", CustomerPartyID: "party-s", Currency: "USD",
			Lines:    []rateapi.QuoteLine{{ChargeCode: "FREIGHT", Sell: usd(100_000), Cost: usd(50_000)}},
			Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		if _, err := rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-" + id, TenantID: tenant, ActorID: "sales-1",
			QuoteID: id, ExpectedRevision: 1, EvidenceRef: "signed-" + id, OccurredAt: at.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	accepted("Q-1")
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	order := func(id, quote, party string, recordedAt time.Time) comapi.RecordCustomerOrder {
		return comapi.RecordCustomerOrder{CommandID: "record-" + id, TenantID: tenant, ActorID: "sales-1", OrderID: id, QuoteID: quote,
			CustomerPartyID: party, OccurredAt: recordedAt, Lines: []comapi.OrderLine{
				{Description: "Pallets of machine parts", Packs: 10, PackageType: "PX", GrossWeight: kg(500), Volume: kernel.Quantity{Unit: "DMQ", Amount: 12_000}},
				{Description: "Cartons of spare parts", Packs: 12, PackageType: "CT", GrossWeight: kg(60)}}}
	}
	if _, err = comModule.RecordCustomerOrder(ctx, order("O-1", "Q-1", "party-s", at.Add(2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	for i, a := range []comapi.AllocateOrderToObligation{
		{LineNo: 1, ObligationID: "OB-1", Packs: 10, GrossWeight: kg(500), Volume: kernel.Quantity{Unit: "DMQ", Amount: 12_000}},
		{LineNo: 2, ObligationID: "OB-2", Packs: 12, GrossWeight: kg(60)},
	} {
		a.CommandID, a.TenantID, a.ActorID, a.OrderID = "allocate-"+a.ObligationID, tenant, "planner-1", "O-1"
		a.ExpectedRevision, a.OccurredAt = int64(i+1), at.Add(3*time.Hour)
		if r, err := comModule.AllocateOrderToObligation(ctx, a); err != nil || r.NewRevision != int64(i+2) {
			t.Fatalf("allocate %s: %+v, %v", a.ObligationID, r, err)
		}
	}

	// A later ORG change, here a second role for the customer, does not reach the order's frozen snapshot.
	if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "shipper-role", TenantID: tenant, ActorID: "org-officer",
		LegalEntityID: "LE", PartyID: "party-s", ExpectedRevision: 2, RoleCode: "SHIPPER", ValidFrom: at, ValidUntil: roleEnds,
		EvidenceRef: "shipper-mandate", OccurredAt: at.Add(4 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := comModule.GetObligationSnapshot(ctx, comapi.GetObligationSnapshot{TenantID: tenant, ObligationID: "OB-2", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Customer != (comapi.CustomerRef{PartyID: "party-s", PartyRevision: 2}) || snapshot.QuoteID != "Q-1" || snapshot.QuoteRevision != 2 ||
		snapshot.LegalEntityID != "LE" || snapshot.OrderLineNo != 2 || snapshot.Quantity != (kernel.Quantity{Unit: "XCT", Amount: 12}) ||
		!snapshot.AllocatedAt.Equal(at.Add(3*time.Hour)) {
		t.Fatalf("obligation snapshot %+v", snapshot)
	}

	// FUL will reserve through ObligationReservation v1 and plan against ObligationSnapshot v2.
	if _, err = comModule.ReserveObligationForFulfillment(ctx, comapi.ReserveObligationForFulfillment{CommandID: "reserve-OB-2",
		TenantID: tenant, ActorID: "fulfilment", ObligationID: "OB-2", ObligationVersion: 1, ShipmentID: "SH-1", OccurredAt: at.Add(5 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	v2, err := comModule.GetObligationSnapshotV2(ctx, comapi.GetObligationSnapshot{TenantID: tenant, ObligationID: "OB-2", Version: 1})
	if err != nil || v2.Packs != 12 || v2.PackageType != "CT" || v2.GrossWeight != kg(60) || v2.Status != comapi.ObligationActive ||
		v2.LatestVersion != 1 || v2.HeldByShipmentID != "SH-1" || v2.Customer != snapshot.Customer {
		t.Fatalf("obligation snapshot v2 %+v, %v", v2, err)
	}

	// RATE decides which terms were accepted and ORG who is a customer when the order is recorded.
	accepted("Q-2")
	accepted("Q-3")
	if _, err = comModule.RecordCustomerOrder(ctx, order("O-2", "Q-2", "party-t", at.Add(2*time.Hour))); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("order by another party than the quote's customer: err = %v", err)
	}
	if _, err = comModule.RecordCustomerOrder(ctx, order("O-3", "Q-3", "party-s", roleEnds.Add(24*time.Hour))); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("order after the customer role ended: err = %v", err)
	}
	if _, err = comModule.RecordCustomerOrder(ctx, order("O-4", "Q-404", "party-s", at.Add(2*time.Hour))); !errors.Is(err, comapi.ErrRejected) || errors.Is(err, comapi.ErrNotFound) {
		t.Fatalf("order against a quote RATE does not know: err = %v, want ErrRejected only", err)
	}

	// COM, like ORG and RATE, refuses to run inside another context's transaction.
	other, err := platform.newDB()
	if err != nil {
		t.Fatal(err)
	}
	err = other.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
		if _, err := comModule.GetObligationSnapshot(txCtx, comapi.GetObligationSnapshot{TenantID: tenant, ObligationID: "OB-1", Version: 1}); !errors.Is(err, bentoapp.ErrTransactionConflict) {
			t.Errorf("COM query inside another context's transaction: err = %v", err)
		}
		if _, err := comModule.RecordCustomerOrder(txCtx, order("O-5", "Q-2", "party-s", at.Add(2*time.Hour))); !errors.Is(err, bentoapp.ErrTransactionConflict) {
			t.Errorf("COM command inside another context's transaction: err = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
