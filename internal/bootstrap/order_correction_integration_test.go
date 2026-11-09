//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// COM V0.5 with the real contexts: an overseas agent orders on the FOB quote it was sent (R7), completing
// the service level from RATE's list (C8); the legal entity's order policy lets the quote back two orders
// (C1); the agent then corrects the declared weight and adds cartons (C10).
func TestOrderCorrectionPolicyAndTermsWithRealContexts(t *testing.T) {
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
	tenant := uniqueTenant("cargo-com05-")
	seedORG(t, platform, tenant, "party-a", "party-b")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond) // business times lie in the past (T1)
	if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "agent-role", TenantID: tenant, ActorID: "org-officer",
		LegalEntityID: "LE", PartyID: "party-a", ExpectedRevision: 1, RoleCode: "OVERSEAS_AGENT", ValidFrom: at, ValidUntil: at.Add(30 * 24 * time.Hour),
		EvidenceRef: "agency-agreement", OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	fob := rateapi.CommercialTerms{IncotermsRule: "FOB", IncotermsVersion: "2020", NamedPlace: "Shanghai port"}
	quote := rateapi.CreateQuote{CommandID: "create-Q-1", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1", LegalEntityID: "LE",
		CustomerPartyID: "party-a", RecipientRole: rateapi.RecipientOverseasAgent, Currency: "USD", Terms: fob,
		Lines:    []rateapi.QuoteLine{{ChargeCode: "FREIGHT", Sell: usd(100_000), Cost: usd(50_000)}},
		Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(time.Minute)}
	if _, err = rateModule.CreateQuote(ctx, quote); err != nil {
		t.Fatal(err)
	}
	unlisted := quote
	unlisted.CommandID, unlisted.QuoteID, unlisted.Terms.ServiceLevel = "create-Q-2", "Q-2", "PRIORITY"
	if _, err = rateModule.CreateQuote(ctx, unlisted); !errors.Is(err, rateapi.ErrRejected) {
		t.Fatalf("RATE prices a quote for an unlisted service level: err = %v", err)
	}
	if _, err = rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-Q-1", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1",
		ExpectedRevision: 1, EvidenceKind: rateapi.EvidenceEmail, EvidenceRef: "mail-Q-1", OccurredAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = comModule.SetOrderPolicy(ctx, comapi.SetOrderPolicy{CommandID: "policy-LE", TenantID: tenant, ActorID: "commercial-lead",
		LegalEntityID: "LE", MaxOrdersPerQuote: 2}); err != nil {
		t.Fatal(err)
	}
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	pallets := comapi.OrderLine{Description: "Pallets of machine parts", Packs: 10, PackageType: "PX", GrossWeight: kg(500)}
	order := func(id string, terms comapi.CommercialTerms) comapi.RecordCustomerOrder {
		return comapi.RecordCustomerOrder{CommandID: "record-" + id, TenantID: tenant, ActorID: "cs-1", OrderID: id, QuoteID: "Q-1",
			CustomerPartyID: "party-a", Lines: []comapi.OrderLine{pallets}, Terms: terms, OccurredAt: at.Add(2 * time.Hour)}
	}
	if _, err = comModule.RecordCustomerOrder(ctx, order("O-0", comapi.CommercialTerms{ServiceLevel: "PRIORITY"})); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("an order completing an unlisted service level: err = %v", err)
	}
	if _, err = comModule.RecordCustomerOrder(ctx, order("O-1", comapi.CommercialTerms{ServiceLevel: "EXPRESS"})); err != nil {
		t.Fatalf("the agent orders and completes the service level: %v", err)
	}
	if _, err = comModule.RecordCustomerOrder(ctx, order("O-2", comapi.CommercialTerms{})); err != nil {
		t.Fatalf("a second order on the quote: %v", err)
	}
	if _, err = comModule.RecordCustomerOrder(ctx, order("O-3", comapi.CommercialTerms{})); !errors.Is(err, comapi.ErrConflict) {
		t.Fatalf("a third order on a policy of two: err = %v", err)
	}
	heavier := comapi.AmendOrderLine{CommandID: "amend-O-1", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1", ExpectedRevision: 1, LineNo: 1,
		Description: pallets.Description, Packs: 10, PackageType: "PX", GrossWeight: kg(520), Reason: "agent corrected the weight",
		OccurredAt: at.Add(3 * time.Hour)}
	if r, err := comModule.AmendOrderLine(ctx, heavier); err != nil || r.NewRevision != 2 {
		t.Fatalf("correct the weight: %+v, %v", r, err)
	}
	if r, err := comModule.AddOrderLine(ctx, comapi.AddOrderLine{CommandID: "add-O-1", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1",
		ExpectedRevision: 2, Line: comapi.OrderLine{Description: "Cartons of manuals", Packs: 2, PackageType: "CT", GrossWeight: kg(15)},
		Reason: "agent added two cartons", OccurredAt: at.Add(3 * time.Hour)}); err != nil || r.NewRevision != 3 {
		t.Fatalf("add a line: %+v, %v", r, err)
	}
	for i, a := range []comapi.AllocateOrderToObligation{
		{LineNo: 1, ObligationID: "OB-1", Packs: 10, GrossWeight: kg(520)},
		{LineNo: 2, ObligationID: "OB-2", Packs: 2, GrossWeight: kg(15)},
	} {
		a.CommandID, a.TenantID, a.ActorID, a.OrderID = "allocate-"+a.ObligationID, tenant, "planner-1", "O-1"
		a.ExpectedRevision, a.OccurredAt = int64(i+3), at.Add(4*time.Hour)
		if _, err := comModule.AllocateOrderToObligation(ctx, a); err != nil {
			t.Fatalf("allocate %s: %v", a.ObligationID, err)
		}
	}
	s, err := comModule.GetObligationSnapshotV3(ctx, comapi.GetObligationSnapshot{TenantID: tenant, ObligationID: "OB-1", Version: 1})
	if err != nil || s.GrossWeight != kg(520) || s.Customer.PartyID != "party-a" {
		t.Fatalf("obligation of the corrected line: %+v, %v", s, err)
	}
	if _, err = rateModule.GetServiceLevel(ctx, rateapi.GetServiceLevel{TenantID: tenant, Code: "PRIORITY"}); !errors.Is(err, rateapi.ErrNotFound) {
		t.Fatalf("an unlisted service level: err = %v", err)
	}
	if list, err := rateModule.ListServiceLevels(ctx, rateapi.ListServiceLevels{TenantID: tenant}); err != nil || len(list) != 4 {
		t.Fatalf("service-level list %v, %v", list, err)
	}
}
