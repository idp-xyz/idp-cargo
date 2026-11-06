//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	bentoapp "go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	orgmodule "go.idp.xyz/idp-cargo/internal/contexts/org"
	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
)

func execInTenant(t *testing.T, db *bentopg.DB, tenant org.TenantID, sql string, args ...any) {
	t.Helper()
	err := (scope.TenantTransactor{DB: db, Tenant: string(tenant)}).WithinTransaction(context.Background(), func(txCtx context.Context) error {
		e, err := db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		_, err = e.Exec(txCtx, sql, args...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Other contexts resolve a party's role through the module. The answer follows the approved role's
// half-open validity, the party's and the legal entity's status, and the tenant's RLS scope.
func TestModuleResolvePartyRoleThroughAPI(t *testing.T) {
	db, pool := testDB(t)
	tenant := org.TenantID("cargo-query-" + uniqueSuffix())
	seed(t, db, tenant, "party-q")
	execInTenant(t, db, tenant, `INSERT INTO cargo_org.legal_entity(tenant_id,legal_entity_id,registered_name,jurisdiction,registration_evidence_ref,status)
        VALUES($1,'LE-2','Second Corp','US','registry-verified','VERIFIED')`, string(tenant))
	module, err := orgmodule.New(db, testAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	from := time.Now().UTC().Truncate(time.Microsecond)
	until := from.Add(30 * 24 * time.Hour)
	if _, err = module.ApproveRole(ctx, api.ApproveRole{
		CommandID: "query-1", TenantID: string(tenant), ActorID: "test-approved", LegalEntityID: "LE", PartyID: "party-q",
		ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: from, ValidUntil: until, EvidenceRef: "customer-contract", OccurredAt: from,
	}); err != nil {
		t.Fatal(err)
	}

	q := api.ResolvePartyRole{TenantID: string(tenant), PartyID: "party-q", RoleCode: "CUSTOMER", LegalEntityID: "LE", At: from.Add(time.Hour)}
	got, err := module.ResolvePartyRole(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	wantRef := api.PartyRef{TenantID: string(tenant), PartyID: "party-q", PartyRevision: 2, IdentitySource: "ORG", LegalEntityScope: "LE"}
	if got.Ref != wantRef || got.LegalName != "Fixture Corp" || got.CountryCode != "US" || got.RoleCode != "CUSTOMER" ||
		got.EvidenceRef != "customer-contract" || !got.Validity.From.Equal(from) || !got.Validity.Until.Equal(until) {
		t.Fatalf("ResolvePartyRole = %+v", got)
	}

	other := org.TenantID("cargo-query-other-" + uniqueSuffix())
	seed(t, db, other, "party-x")
	for name, tc := range map[string]struct {
		edit func(*api.ResolvePartyRole)
		want error
	}{
		"before the role starts":  {func(q *api.ResolvePartyRole) { q.At = from.Add(-time.Second) }, api.ErrRejected},
		"when the role ends":      {func(q *api.ResolvePartyRole) { q.At = until }, api.ErrRejected},
		"role never approved":     {func(q *api.ResolvePartyRole) { q.RoleCode = "CARRIER" }, api.ErrRejected},
		"other legal entity":      {func(q *api.ResolvePartyRole) { q.LegalEntityID = "LE-2" }, api.ErrRejected},
		"unknown party":           {func(q *api.ResolvePartyRole) { q.PartyID = "no-such-party" }, api.ErrNotFound},
		"unknown legal entity":    {func(q *api.ResolvePartyRole) { q.LegalEntityID = "no-such-entity" }, api.ErrNotFound},
		"party of another tenant": {func(q *api.ResolvePartyRole) { q.TenantID = string(other) }, api.ErrNotFound},
		"no tenant":               {func(q *api.ResolvePartyRole) { q.TenantID = "" }, api.ErrUnauthenticated},
	} {
		query := q
		tc.edit(&query)
		if _, err := module.ResolvePartyRole(ctx, query); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}

	// A context with its own transaction owner must ask before opening its transaction (rule T3).
	caller, err := bentopg.NewDB(pool)
	if err != nil {
		t.Fatal(err)
	}
	err = caller.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
		_, err := module.ResolvePartyRole(txCtx, q)
		return err
	})
	if !errors.Is(err, bentoapp.ErrTransactionConflict) {
		t.Fatalf("query inside another owner's transaction: err = %v, want %v", err, bentoapp.ErrTransactionConflict)
	}

	execInTenant(t, db, tenant, `UPDATE cargo_org.legal_entity SET status='SUSPENDED' WHERE tenant_id=$1 AND legal_entity_id='LE'`, string(tenant))
	if _, err = module.ResolvePartyRole(ctx, q); !errors.Is(err, api.ErrRejected) || !errors.Is(err, org.ErrLegalEntityNotVerified) {
		t.Fatalf("suspended legal entity: err = %v", err)
	}
	execInTenant(t, db, tenant, `UPDATE cargo_org.legal_entity SET status='VERIFIED' WHERE tenant_id=$1 AND legal_entity_id='LE'`, string(tenant))
	execInTenant(t, db, tenant, `UPDATE cargo_org.business_party SET status='SUSPENDED' WHERE tenant_id=$1 AND party_id='party-q'`, string(tenant))
	if _, err = module.ResolvePartyRole(ctx, q); !errors.Is(err, api.ErrRejected) || !errors.Is(err, org.ErrPartyNotActive) {
		t.Fatalf("suspended party: err = %v", err)
	}
}
