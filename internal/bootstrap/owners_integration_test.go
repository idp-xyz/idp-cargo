//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	bentoapp "go.idp.xyz/idp-bento-go/application"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
)

type allowAll struct{}

func (allowAll) AuthorizeRoleApproval(context.Context, string, string, string, string) error {
	return nil
}

// AC4: with one transaction owner per context, a context that calls ORG inside its own transaction
// is refused instead of having ORG's writes join that transaction.
func TestContextsDoNotShareTransactions(t *testing.T) {
	dsn := os.Getenv("IDP_CARGO_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("IDP_CARGO_POSTGRES_DSN required in CI; skipping forbidden")
		}
		t.Skip("set IDP_CARGO_POSTGRES_DSN to a PostgreSQL 16 application role")
	}
	ctx := context.Background()
	platform, err := Open(ctx, Config{PostgresDSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	orgModule, err := platform.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := platform.newDB()
	if err != nil {
		t.Fatal(err)
	}
	other, err := platform.newDB() // stands in for a second bounded context
	if err != nil {
		t.Fatal(err)
	}

	tenant := "cargo-bootstrap-" + time.Now().UTC().Format("20060102T150405.000000000")
	err = (bento.TenantTransactor{DB: fixture, Tenant: tenant}).WithinTransaction(ctx, func(txCtx context.Context) error {
		e, err := fixture.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		if _, err = e.Exec(txCtx, `INSERT INTO cargo_org.business_party(tenant_id,party_id,legal_name,registration_country,status)
            VALUES($1,'party-b','Fixture Corp','US','ACTIVE')`, tenant); err != nil {
			return err
		}
		_, err = e.Exec(txCtx, `INSERT INTO cargo_org.legal_entity(tenant_id,legal_entity_id,registered_name,jurisdiction,registration_evidence_ref,status)
            VALUES($1,'LE','Fixture Corp','US','registry-verified','VERIFIED')`, tenant)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

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
