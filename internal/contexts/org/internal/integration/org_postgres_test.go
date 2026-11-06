//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/adapters/postgres"
	app "go.idp.xyz/idp-cargo/internal/contexts/org/internal/application"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
)

type allowExplicitTestAuthorization struct{}

func (allowExplicitTestAuthorization) RequireRoleApproval(_ context.Context, c app.Scope, p org.PartyID, r org.RoleCode) error {
	if c.ActorID != "test-approved" {
		return errors.New("test policy: forbidden")
	}
	return nil
}

type failingOutbox struct{}

func (failingOutbox) EnqueueORGEvent(context.Context, org.Event) error {
	return errors.New("injected failure")
}

func testDB(t *testing.T) (*bentopg.DB, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("IDP_CARGO_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("IDP_CARGO_POSTGRES_DSN required in CI; skipping forbidden")
		}
		t.Skip("set IDP_CARGO_POSTGRES_DSN to actual PostgreSQL 16 application role")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db, err := bentopg.NewDB(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CheckSchema(ctx); err != nil {
		t.Fatal("Bento PostgreSQL migration/schema check:", err)
	}
	var major int
	if err = pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int / 10000").Scan(&major); err != nil {
		t.Fatal(err)
	}
	if major != 16 {
		t.Fatalf("expected PostgreSQL major 16, got %d", major)
	}
	var bypass, super, owner bool
	err = pool.QueryRow(ctx, `SELECT rolbypassrls,rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&bypass, &super)
	if err != nil {
		t.Fatal(err)
	}
	if bypass || super {
		t.Fatal("application role must not bypass RLS or be superuser")
	}
	err = pool.QueryRow(ctx, `SELECT pg_has_role(current_user,'cargo_migrator','MEMBER')`).Scan(&owner)
	if err == nil && owner {
		t.Fatal("application role must not inherit migrator privileges")
	}
	return db, pool
}
func seed(t *testing.T, db *bentopg.DB, tenant org.TenantID, party org.PartyID) {
	t.Helper()
	ctx := context.Background()
	err := (scope.TenantTransactor{DB: db, Tenant: string(tenant)}).WithinTransaction(ctx, func(txCtx context.Context) error {
		e, err := db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		_, err = e.Exec(txCtx, `INSERT INTO cargo_org.business_party(tenant_id,party_id,legal_name,registration_country,status)
            VALUES($1,$2,'Fixture Corp','US','ACTIVE')`, string(tenant), string(party))
		if err != nil {
			return err
		}
		_, err = e.Exec(txCtx, `INSERT INTO cargo_org.legal_entity(tenant_id,legal_entity_id,registered_name,jurisdiction,registration_evidence_ref,status)
            VALUES($1,'LE','Fixture Corp','US','registry-verified','VERIFIED') ON CONFLICT(tenant_id,legal_entity_id) DO NOTHING`, string(tenant))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
func buildHandler(t *testing.T, db *bentopg.DB, tenant org.TenantID, commandID string, writer postgres.Outbox) app.ApproveRole {
	writer.CommandID = commandID
	writer.RecordedAt = time.Now().UTC()
	return app.ApproveRole{
		Tx:      scope.TenantTransactor{DB: db, Tenant: string(tenant)},
		Parties: postgres.PartyStore{DB: db}, Entities: postgres.LegalEntityEligibility{DB: db},
		Receipts: postgres.CommandReceipts{DB: db}, Auth: allowExplicitTestAuthorization{}, Outbox: writer,
	}
}
func approvalCommand(tenant org.TenantID, party org.PartyID, command string) app.RoleApprovalCommand {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return app.RoleApprovalCommand{
		CommandID: command, PartyID: party, Scope: app.Scope{TenantID: tenant, ActorID: "test-approved", LegalEntityID: "LE"},
		ExpectedRevision: 1, OccurredAt: now,
		Role: org.Role{Code: org.RoleCustomer, LegalEntityID: "LE", ValidFrom: now, EvidenceRef: "test-evidence"},
	}
}
func TestRoleApprovalRealPostgresAtomicAndIdempotent(t *testing.T) {
	db, pool := testDB(t)
	// Test fixtures intentionally get unique identities per run, to keep old evidence intact.
	uniq := time.Now().UTC().Format("20060102T150405.000000000")
	tenant := org.TenantID("cargo-test-" + uniq)
	party := org.PartyID("party-a")
	seed(t, db, tenant, party)
	writer, err := bentooutbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	cmd := approvalCommand(tenant, party, "role-approve-1")
	h := buildHandler(t, db, tenant, cmd.CommandID, postgres.Outbox{Writer: writer})
	receipt, err := h.Handle(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.NewRevision != 2 {
		t.Fatalf("revision: %d", receipt.NewRevision)
	}
	repeat, err := h.Handle(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if repeat.NewRevision != receipt.NewRevision || repeat.RequestHash != receipt.RequestHash {
		t.Fatal("idempotent replay mismatch")
	}
	cmd.Role.EvidenceRef = "tamper"
	if _, err = h.Handle(context.Background(), cmd); !errors.Is(err, app.ErrIdempotencyConflict) {
		t.Fatalf("mismatched replay: %v", err)
	}
	var roleCount, revision int
	if err = pool.QueryRow(context.Background(), `SELECT count(*) FROM cargo_org.party_role WHERE tenant_id=$1`, string(tenant)).Scan(&roleCount); err != nil {
		t.Fatal(err)
	}
	// Query via unscope app connection is deliberately RLS-denied: public read is not authoritative.
	if roleCount != 0 {
		t.Fatal("RLS data leaked outside tenant-scoped transaction")
	}
	err = (scope.TenantTransactor{DB: db, Tenant: string(tenant)}).WithinTransaction(context.Background(), func(ctx context.Context) error {
		q, err := db.ReadExecutor(ctx)
		if err != nil {
			return err
		}
		if err = q.QueryRow(ctx, `SELECT count(*) FROM cargo_org.party_role WHERE tenant_id=$1`, string(tenant)).Scan(&roleCount); err != nil {
			return err
		}
		return q.QueryRow(ctx, `SELECT revision FROM cargo_org.business_party WHERE tenant_id=$1 AND party_id=$2`, string(tenant), string(party)).Scan(&revision)
	})
	if err != nil {
		t.Fatal(err)
	}
	if roleCount != 1 || revision != 2 {
		t.Fatalf("role=%d revision=%d", roleCount, revision)
	}
}
func TestRoleApprovalRollbackOnOutboxFailure(t *testing.T) {
	db, _ := testDB(t)
	uniq := time.Now().UTC().Format("20060102T150405.000000000")
	tenant := org.TenantID("cargo-rollback-" + uniq)
	party := org.PartyID("party-b")
	seed(t, db, tenant, party)
	cmd := approvalCommand(tenant, party, "rollback")
	h := app.ApproveRole{Tx: scope.TenantTransactor{DB: db, Tenant: string(tenant)},
		Parties: postgres.PartyStore{DB: db}, Entities: postgres.LegalEntityEligibility{DB: db},
		Receipts: postgres.CommandReceipts{DB: db}, Auth: allowExplicitTestAuthorization{}, Outbox: failingOutbox{}}
	if _, err := h.Handle(context.Background(), cmd); err == nil {
		t.Fatal("injected failure should rollback")
	}
	err := (scope.TenantTransactor{DB: db, Tenant: string(tenant)}).WithinTransaction(context.Background(), func(ctx context.Context) error {
		q, err := db.ReadExecutor(ctx)
		if err != nil {
			return err
		}
		var revision, roles, receipts int
		if err = q.QueryRow(ctx, `SELECT revision FROM cargo_org.business_party WHERE tenant_id=$1 AND party_id=$2`, string(tenant), string(party)).Scan(&revision); err != nil {
			return err
		}
		if err = q.QueryRow(ctx, `SELECT count(*) FROM cargo_org.party_role WHERE tenant_id=$1`, string(tenant)).Scan(&roles); err != nil {
			return err
		}
		if err = q.QueryRow(ctx, `SELECT count(*) FROM cargo_org.command_receipt WHERE tenant_id=$1`, string(tenant)).Scan(&receipts); err != nil {
			return err
		}
		if revision != 1 || roles != 0 || receipts != 0 {
			return errors.New("transaction atomicity violated")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRLSMissingWrongTenantAndWriteDenied(t *testing.T) {
	db, _ := testDB(t)
	uniq := time.Now().UTC().Format("20060102T150405.000000000")
	owned := org.TenantID("cargo-rls-owner-" + uniq)
	other := org.TenantID("cargo-rls-other-" + uniq)
	party := org.PartyID("party-hidden")
	seed(t, db, owned, party)
	// Same party key does not confer visibility across tenants.
	err := (scope.TenantTransactor{DB: db, Tenant: string(other)}).WithinTransaction(context.Background(), func(ctx context.Context) error {
		_, _, err := (postgres.PartyStore{DB: db}).Load(ctx, other, party)
		if err == nil {
			return errors.New("foreign party exposed")
		}
		q, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		result, err := q.Exec(ctx, `UPDATE cargo_org.business_party SET legal_name='Unauthorized' WHERE tenant_id=$1 AND party_id=$2`, string(owned), string(party))
		if err != nil {
			return err
		}
		if result.RowsAffected() != 0 {
			return errors.New("wrong tenant updated protected row")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A deliberately mismatched tenant scope must be rejected by the command layer.
	h := app.ApproveRole{Tx: scope.TenantTransactor{DB: db, Tenant: string(other)},
		Parties: postgres.PartyStore{DB: db}, Entities: postgres.LegalEntityEligibility{DB: db},
		Receipts: postgres.CommandReceipts{DB: db}, Auth: allowExplicitTestAuthorization{}, Outbox: failingOutbox{}}
	cmd := approvalCommand(owned, party, "wrong-tenant")
	if _, err = h.Handle(context.Background(), cmd); !errors.Is(err, app.ErrUnauthorized) {
		t.Fatalf("tenant scope mismatch should fail closed: %v", err)
	}
}

func TestConcurrentSameCommandCannotDuplicateBusinessMutation(t *testing.T) {
	db, _ := testDB(t)
	uniq := time.Now().UTC().Format("20060102T150405.000000000")
	tenant := org.TenantID("cargo-concurrent-" + uniq)
	party := org.PartyID("party-c")
	seed(t, db, tenant, party)
	writer, err := bentooutbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	cmd := approvalCommand(tenant, party, "concurrent-approval")
	// Handler per invocation; reference commandID is fixed and unchanged.
	outcomes := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			h := buildHandler(t, db, tenant, cmd.CommandID, postgres.Outbox{Writer: writer})
			_, err := h.Handle(context.Background(), cmd)
			outcomes <- err
		}()
	}
	err1, err2 := <-outcomes, <-outcomes
	if err1 != nil && err2 != nil {
		t.Fatalf("both rejected: %v / %v", err1, err2)
	}
	// The loser may return revision conflict or identical confirmed receipt,
	// but neither can create duplicate role facts or Outbox messages.
	err = (scope.TenantTransactor{DB: db, Tenant: string(tenant)}).WithinTransaction(context.Background(), func(ctx context.Context) error {
		q, err := db.ReadExecutor(ctx)
		if err != nil {
			return err
		}
		var roles, receiptCount int
		if err = q.QueryRow(ctx, `SELECT count(*) FROM cargo_org.party_role WHERE tenant_id=$1`, string(tenant)).Scan(&roles); err != nil {
			return err
		}
		if err = q.QueryRow(ctx, `SELECT count(*) FROM cargo_org.command_receipt WHERE tenant_id=$1`, string(tenant)).Scan(&receiptCount); err != nil {
			return err
		}
		if roles != 1 || receiptCount != 1 {
			return errors.New("concurrent command created duplicates")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
