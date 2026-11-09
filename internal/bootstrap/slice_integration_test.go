//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	bentoapp "go.idp.xyz/idp-bento-go/application"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Steps 1 and 2 of the S1 slice with the real contexts: ORG approves a CUSTOMER role, then RATE
// prices, approves and accepts a quote, asking ORG through its query side each time.
func TestQuoteForAnApprovedCustomer(t *testing.T) {
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
	tenant := uniqueTenant("cargo-slice-")
	seedORG(t, platform, tenant, "party-s", "party-n")

	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond) // business times lie in the past (T1)
	roleEnds := at.Add(2 * time.Hour)
	if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "slice-role", TenantID: tenant, ActorID: "org-officer",
		LegalEntityID: "LE", PartyID: "party-s", ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: at, ValidUntil: roleEnds,
		EvidenceRef: "customer-contract", OccurredAt: at}); err != nil {
		t.Fatal(err)
	}

	quote := func(id, party string, sell, cost int64) rateapi.CreateQuote {
		return rateapi.CreateQuote{CommandID: "create-" + id, TenantID: tenant, ActorID: "sales-1", QuoteID: id, LegalEntityID: "LE",
			CustomerPartyID: party, Currency: "USD", Lines: []rateapi.QuoteLine{{ChargeCode: "FREIGHT", Sell: usd(sell), Cost: usd(cost)}},
			Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(time.Minute)}
	}
	if _, err = rateModule.CreateQuote(ctx, quote("Q-1", "party-s", 100_000, 90_000)); err != nil {
		t.Fatal(err)
	}
	if _, err = rateModule.ApproveQuotePrice(ctx, rateapi.ApproveQuotePrice{CommandID: "approve-Q-1", TenantID: tenant, ActorID: "manager-1",
		QuoteID: "Q-1", ExpectedRevision: 1, ReasonRef: "margin-exception-1", OccurredAt: at.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err = rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-Q-1", TenantID: tenant, ActorID: "sales-1",
		QuoteID: "Q-1", ExpectedRevision: 2, EvidenceKind: rateapi.EvidenceSignedDocument, EvidenceRef: "signed-quote-1", OccurredAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := rateModule.GetQuoteSnapshot(ctx, rateapi.GetQuoteSnapshot{TenantID: tenant, QuoteID: "Q-1"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Customer != (rateapi.CustomerRef{PartyID: "party-s", PartyRevision: 2}) || snapshot.Total != usd(100_000) ||
		snapshot.PriceApprovedBy != "manager-1" || snapshot.QuoteRevision != 3 || !snapshot.AcceptedAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("snapshot %+v", snapshot)
	}

	// ORG decides who is a customer: no role means no quote, and a role that has ended blocks acceptance.
	if _, err = rateModule.CreateQuote(ctx, quote("Q-2", "party-n", 100_000, 50_000)); !errors.Is(err, rateapi.ErrRejected) {
		t.Fatalf("quote for a party without the CUSTOMER role: err = %v", err)
	}
	if _, err = rateModule.CreateQuote(ctx, quote("Q-5", "party-unknown", 100_000, 50_000)); !errors.Is(err, rateapi.ErrRejected) || errors.Is(err, rateapi.ErrNotFound) {
		t.Fatalf("quote for a party ORG does not know: err = %v, want ErrRejected only", err)
	}
	if _, err = rateModule.CreateQuote(ctx, quote("Q-3", "party-s", 100_000, 50_000)); err != nil {
		t.Fatal(err)
	}
	if _, err = rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-Q-3", TenantID: tenant, ActorID: "sales-1",
		QuoteID: "Q-3", ExpectedRevision: 1, EvidenceKind: rateapi.EvidenceSignedDocument, EvidenceRef: "signed-quote-3", OccurredAt: roleEnds.Add(time.Hour)}); !errors.Is(err, rateapi.ErrRejected) {
		t.Fatalf("acceptance after the customer role ended: err = %v", err)
	}

	// RATE, like ORG, refuses to run inside another context's transaction.
	other, err := platform.newDB()
	if err != nil {
		t.Fatal(err)
	}
	err = other.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
		if _, err := rateModule.GetQuoteSnapshot(txCtx, rateapi.GetQuoteSnapshot{TenantID: tenant, QuoteID: "Q-1"}); !errors.Is(err, bentoapp.ErrTransactionConflict) {
			t.Errorf("RATE query inside another context's transaction: err = %v", err)
		}
		if _, err := rateModule.CreateQuote(txCtx, quote("Q-4", "party-s", 100_000, 50_000)); !errors.Is(err, bentoapp.ErrTransactionConflict) {
			t.Errorf("RATE command inside another context's transaction: err = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
