//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	bentoapp "go.idp.xyz/idp-bento-go/application"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
)

// AC4: with one transaction owner per context, a context that calls ORG inside its own transaction
// is refused instead of having ORG's writes join that transaction.
func TestContextsDoNotShareTransactions(t *testing.T) {
	ctx := context.Background()
	platform := openPlatform(t)
	orgModule, err := platform.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := platform.newDB() // stands in for a second bounded context
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-bootstrap-")
	seedORG(t, platform, tenant, "party-b")

	at := time.Now().UTC().Truncate(time.Microsecond)
	approve := orgapi.ApproveRole{CommandID: "bootstrap-1", TenantID: tenant, ActorID: "bootstrap-test", LegalEntityID: "LE",
		PartyID: "party-b", ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: at, EvidenceRef: "bootstrap-evidence", OccurredAt: at}
	query := orgapi.ResolvePartyRole{TenantID: tenant, PartyID: "party-b", RoleCode: "CUSTOMER", LegalEntityID: "LE", At: at}

	err = other.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
		if _, err := orgModule.ApproveRole(txCtx, approve); !errors.Is(err, bentoapp.ErrTransactionConflict) {
			t.Errorf("command inside another context's transaction: err = %v, want %v", err, bentoapp.ErrTransactionConflict)
		}
		if _, err := orgModule.ResolvePartyRole(txCtx, query); !errors.Is(err, bentoapp.ErrTransactionConflict) {
			t.Errorf("query inside another context's transaction: err = %v, want %v", err, bentoapp.ErrTransactionConflict)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = orgModule.ApproveRole(ctx, approve); err != nil {
		t.Fatalf("command outside any transaction: %v", err)
	}
	if got, err := orgModule.ResolvePartyRole(ctx, query); err != nil || got.Ref.PartyRevision != 2 {
		t.Fatalf("query outside any transaction: %+v, %v", got, err)
	}
}
