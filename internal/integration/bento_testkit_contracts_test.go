//go:build bento && integration

package integration

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.idp.xyz/idp-bento-go/application"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentoinbox "go.idp.xyz/idp-bento-go/postgres/inbox"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-bento-go/testkit"
	"go.idp.xyz/idp-cargo/internal/contexts/org/adapters/postgres"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/domain"
	scope "go.idp.xyz/idp-cargo/internal/platform/bento"
)

var contractSeq atomic.Uint64

// contractDB provisions a throwaway Bento schema the way the workflow provisions the real one: the
// migrator role creates it, applies the framework migration and grants the application role DML only.
// The returned DB runs as the application role. Bento's Outbox claims are global, so every contract
// scenario needs its own schema.
func contractDB(t *testing.T) *bentopg.DB {
	t.Helper()
	_, appPool := testDB(t)
	dsn := os.Getenv("IDP_CARGO_MIGRATOR_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("IDP_CARGO_MIGRATOR_DSN required in CI; skipping forbidden")
		}
		t.Skip("set IDP_CARGO_MIGRATOR_DSN to provision isolated Bento contract schemas")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var appRole string
	if err = appPool.QueryRow(ctx, `SELECT current_user`).Scan(&appRole); err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("cargo_bento_contract_%d_%d", os.Getpid(), contractSeq.Add(1))
	ident, role := pgx.Identifier{schema}.Sanitize(), pgx.Identifier{appRole}.Sanitize()
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+ident); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP SCHEMA `+ident+` CASCADE`) })
	for _, m := range bentopg.Migrations() {
		sql, err := bentopg.RenderMigration(m.ID, schema)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", m.ID, err)
		}
	}
	if _, err = admin.Exec(ctx, fmt.Sprintf(`GRANT USAGE ON SCHEMA %[1]s TO %[2]s;
GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA %[1]s TO %[2]s;
GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA %[1]s TO %[2]s`, ident, role)); err != nil {
		t.Fatal(err)
	}
	db, err := bentopg.NewDB(appPool, bentopg.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

func orgContractEnvelope(t *testing.T, command string, tenant org.TenantID, party string, recordedAt time.Time) eventing.Envelope {
	t.Helper()
	envelope, err := postgres.ORGEnvelope(command, org.Event{Type: "PartnerRoleApproved.v1", At: recordedAt.Add(-time.Second), TenantID: tenant, SubjectID: party}, recordedAt)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func TestBentoOutboxContract(t *testing.T) {
	testkit.RunOutboxContract(t, func(t *testing.T) testkit.OutboxContractScenario {
		db := contractDB(t)
		store, err := bentooutbox.NewStore(db)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
		tenant := org.TenantID("cargo-outbox-contract")
		return testkit.OutboxContractScenario{
			Transactor:     db.Transactor(),
			Writer:         store,
			Claimer:        store,
			Finalizer:      store,
			First:          orgContractEnvelope(t, "contract-1", tenant, "party-1", at),
			SamePartition:  orgContractEnvelope(t, "contract-2", tenant, "party-1", at.Add(time.Second)),
			OtherPartition: orgContractEnvelope(t, "contract-3", tenant, "party-2", at.Add(2*time.Second)),
			Now:            at.Add(time.Minute),
			LeaseFor:       30 * time.Second,
		}
	})
}

func TestBentoInboxContract(t *testing.T) {
	testkit.RunInboxContract(t, func(t *testing.T) testkit.InboxContractScenario {
		db := contractDB(t)
		store, err := bentoinbox.NewStore(db)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
		event := orgContractEnvelope(t, "contract-1", "cargo-inbox-contract", "party-1", at)
		return testkit.InboxContractScenario{
			Transactor: db.Transactor(),
			Starter:    store,
			Completer:  store,
			Key:        eventing.InboxKey{Consumer: "idp-cargo.com", Source: event.Source, EventID: event.ID},
			Now:        at,
		}
	})
}

func seedLegalEntity(t *testing.T, db *bentopg.DB, tenant org.TenantID) {
	t.Helper()
	err := (scope.TenantTransactor{DB: db, Tenant: tenant}).WithinTransaction(context.Background(), func(ctx context.Context) error {
		e, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		_, err = e.Exec(ctx, `INSERT INTO cargo_org.legal_entity(tenant_id,legal_entity_id,registered_name,jurisdiction,registration_evidence_ref,status)
            VALUES($1,'LE','Contract Corp','US','registry-verified','VERIFIED')`, string(tenant))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// samePartyFacts compares persisted facts at timestamptz (microsecond) precision.
func samePartyFacts(got, want *org.BusinessParty) bool {
	if got == nil || want == nil {
		return got == want
	}
	if got.Tenant() != want.Tenant() || got.ID() != want.ID() || got.Name() != want.Name() ||
		got.Country() != want.Country() || got.Status() != want.Status() {
		return false
	}
	gotRoles, wantRoles := got.ApprovedRoles(), want.ApprovedRoles()
	if len(gotRoles) != len(wantRoles) {
		return false
	}
	sameMicro := func(a, b time.Time) bool { return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond)) }
	for i, g := range gotRoles {
		w := wantRoles[i]
		if g.Code != w.Code || g.LegalEntityID != w.LegalEntityID || g.EvidenceRef != w.EvidenceRef || g.ApprovedBy != w.ApprovedBy ||
			!sameMicro(g.ValidFrom, w.ValidFrom) || !sameMicro(g.ValidUntil, w.ValidUntil) || !sameMicro(g.ApprovedAt, w.ApprovedAt) {
			return false
		}
	}
	return true
}

func TestBentoRepositoryContract(t *testing.T) {
	testkit.RunRepositoryContract(t, func(t *testing.T) testkit.RepositoryContractScenario[scope.ScopedPartyKey, *org.BusinessParty] {
		db, _ := testDB(t)
		tenant := org.TenantID(fmt.Sprintf("cargo-repo-contract-%s-%d", uniqueSuffix(), contractSeq.Add(1)))
		seedLegalEntity(t, db, tenant)
		at := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
		restore := func(id org.PartyID, name, country string) *org.BusinessParty {
			party, err := org.RestoreParty(tenant, id, name, country, org.PartyActive, nil)
			if err != nil {
				t.Fatal(err)
			}
			return party
		}
		updated := restore("party-a", "Contract Corp", "US")
		if err := updated.CorrectLegalName("Contract Corp Ltd", "registry-proof", at); err != nil {
			t.Fatal(err)
		}
		role := org.Role{Code: org.RoleCustomer, LegalEntityID: "LE", ValidFrom: at, EvidenceRef: "contract-evidence", ApprovedBy: "contract-approver"}
		if err := updated.ApproveRole(role, at); err != nil {
			t.Fatal(err)
		}
		tx := scope.TenantTransactor{DB: db, Tenant: tenant}
		repo := postgres.PartyRepository{Store: postgres.PartyStore{DB: db}}
		return testkit.RepositoryContractScenario[scope.ScopedPartyKey, *org.BusinessParty]{
			// Cargo binds one tenant per transaction, so the "other scope" is a second party of the same
			// tenant; cross-tenant isolation is covered by the RLS tests in this package.
			Transactor: &scope.TransactionPort{Within: func(ctx context.Context, fn application.TxFunc) error {
				return tx.WithinTransaction(ctx, fn)
			}},
			Loader:              repo,
			Inserter:            repo,
			Updater:             repo,
			Key:                 scope.ScopedPartyKey{TenantID: tenant, ID: "party-a"},
			OtherScopeKey:       scope.ScopedPartyKey{TenantID: tenant, ID: "party-b"},
			Aggregate:           restore("party-a", "Contract Corp", "US"),
			UpdatedAggregate:    updated,
			OtherScopeAggregate: restore("party-b", "Other Corp", "DE"),
			Equal:               samePartyFacts,
		}
	})
}
