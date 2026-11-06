//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/adapters/postgres"
	app "go.idp.xyz/idp-cargo/internal/contexts/org/internal/application"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
)

func uniqueSuffix() string { return time.Now().UTC().Format("20060102T150405.000000000") }

func outboxRows(t *testing.T, db *bentopg.DB, tenant org.TenantID) int {
	t.Helper()
	q, err := db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err = q.QueryRow(context.Background(), `SELECT count(*) FROM bento.outbox WHERE scope=$1`, string(tenant)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestOutboxExactlyOneRowPerCommittedApproval(t *testing.T) {
	db, _ := testDB(t)
	tenant := org.TenantID("cargo-outbox-" + uniqueSuffix())
	seed(t, db, tenant, "party-o")
	writer, err := bentooutbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	cmd := approvalCommand(tenant, "party-o", "outbox-once")
	h := buildHandler(t, db, tenant, cmd.CommandID, postgres.Outbox{Writer: writer})
	if _, err = h.Handle(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Handle(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if n := outboxRows(t, db, tenant); n != 1 {
		t.Fatalf("outbox rows after approval and replay = %d, want 1", n)
	}
}

type failingReceipts struct{ postgres.CommandReceipts }

func (failingReceipts) Save(context.Context, org.TenantID, app.Receipt) error {
	return errors.New("injected receipt failure after outbox enqueue")
}

// The failure happens after the real Bento Outbox enqueue, so the enqueue must roll back with the business write.
func TestOutboxRolledBackWhenLaterStepFails(t *testing.T) {
	db, _ := testDB(t)
	tenant := org.TenantID("cargo-outbox-rollback-" + uniqueSuffix())
	seed(t, db, tenant, "party-r")
	writer, err := bentooutbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	cmd := approvalCommand(tenant, "party-r", "outbox-rollback")
	h := buildHandler(t, db, tenant, cmd.CommandID, postgres.Outbox{Writer: writer})
	h.Receipts = failingReceipts{postgres.CommandReceipts{DB: db}}
	if _, err = h.Handle(context.Background(), cmd); err == nil {
		t.Fatal("expected injected failure")
	}
	if n := outboxRows(t, db, tenant); n != 0 {
		t.Fatalf("outbox rows after rollback = %d, want 0", n)
	}
}

func TestConcurrentCommandsSingleOutboxRowAndRevisionConflict(t *testing.T) {
	db, _ := testDB(t)
	tenant := org.TenantID("cargo-concurrent-outbox-" + uniqueSuffix())
	seed(t, db, tenant, "party-k")
	writer, err := bentooutbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	cmd := approvalCommand(tenant, "party-k", "concurrent-outbox")
	type outcome struct {
		receipt app.Receipt
		err     error
	}
	const workers = 4
	start := make(chan struct{})
	outcomes := make(chan outcome, workers)
	for i := 0; i < workers; i++ {
		go func() {
			h := buildHandler(t, db, tenant, cmd.CommandID, postgres.Outbox{Writer: writer})
			<-start
			r, err := h.Handle(context.Background(), cmd)
			outcomes <- outcome{r, err}
		}()
	}
	close(start)
	succeeded := 0
	for i := 0; i < workers; i++ {
		o := <-outcomes
		switch {
		case o.err == nil:
			succeeded++
			if o.receipt.NewRevision != 2 {
				t.Errorf("successful handler returned revision %d, want 2", o.receipt.NewRevision)
			}
		case errors.Is(o.err, bentorepo.ErrConflict), errors.Is(o.err, org.ErrConflict):
			t.Logf("losing handler: %v", o.err)
		default:
			t.Errorf("losing handler must fail with a revision conflict, got %v", o.err)
		}
	}
	if succeeded == 0 {
		t.Fatal("no handler succeeded")
	}
	t.Logf("successful handlers: %d of %d", succeeded, workers)
	if n := outboxRows(t, db, tenant); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
}

// Write paths beyond TestRLSMissingWrongTenantAndWriteDenied: wrong-tenant INSERT/DELETE,
// writes with no tenant GUC at all, and switching tenant inside an open tenant transaction.
func TestRLSWritePathsDenied(t *testing.T) {
	db, _ := testDB(t)
	owned := org.TenantID("cargo-rls-write-owner-" + uniqueSuffix())
	other := org.TenantID("cargo-rls-write-other-" + uniqueSuffix())
	seed(t, db, owned, "party-w")
	ctx := context.Background()

	err := (scope.TenantTransactor{DB: db, Tenant: string(other)}).WithinTransaction(ctx, func(txCtx context.Context) error {
		e, err := db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		_, err = e.Exec(txCtx, `INSERT INTO cargo_org.business_party(tenant_id,party_id,legal_name,registration_country,status)
			VALUES($1,'smuggled','X','US','ACTIVE')`, string(owned))
		return err
	})
	if sqlState(err) != "42501" {
		t.Errorf("wrong-tenant INSERT: want RLS violation 42501, got %v", err)
	}

	err = (scope.TenantTransactor{DB: db, Tenant: string(other)}).WithinTransaction(ctx, func(txCtx context.Context) error {
		e, err := db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		tag, err := e.Exec(txCtx, `DELETE FROM cargo_org.legal_entity WHERE tenant_id=$1`, string(owned))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return errors.New("wrong-tenant DELETE affected rows")
		}
		return nil
	})
	if err != nil {
		t.Errorf("wrong-tenant DELETE: %v", err)
	}

	err = db.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
		e, err := db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		tag, err := e.Exec(txCtx, `UPDATE cargo_org.business_party SET legal_name='Unscoped' WHERE tenant_id=$1`, string(owned))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return errors.New("unscoped UPDATE affected rows")
		}
		return nil
	})
	if err != nil {
		t.Errorf("unscoped UPDATE: %v", err)
	}

	err = db.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
		e, err := db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		_, err = e.Exec(txCtx, `INSERT INTO cargo_org.business_party(tenant_id,party_id,legal_name,registration_country,status)
			VALUES($1,'unscoped','X','US','ACTIVE')`, string(owned))
		return err
	})
	if sqlState(err) != "42501" {
		t.Errorf("unscoped INSERT: want RLS violation 42501, got %v", err)
	}

	err = (scope.TenantTransactor{DB: db, Tenant: string(owned)}).WithinTransaction(ctx, func(txCtx context.Context) error {
		return (scope.TenantTransactor{DB: db, Tenant: string(other)}).WithinTransaction(txCtx, func(context.Context) error { return nil })
	})
	if !errors.Is(err, scope.ErrTenantScopeConflict) {
		t.Errorf("nested tenant switch: want ErrTenantScopeConflict, got %v", err)
	}
}

// approveTwoRoles approves CUSTOMER with the given ValidFrom, then SUPPLIER on the same party,
// so the second Save reloads and re-persists the first role.
func approveTwoRoles(t *testing.T, validFrom time.Time, label string) error {
	t.Helper()
	db, _ := testDB(t)
	tenant := org.TenantID("cargo-role-precision-" + label + "-" + uniqueSuffix())
	seed(t, db, tenant, "party-p")
	writer, err := bentooutbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	first := approvalCommand(tenant, "party-p", "precision-1")
	first.Role.ValidFrom = validFrom
	if _, err = buildHandler(t, db, tenant, first.CommandID, postgres.Outbox{Writer: writer}).Handle(context.Background(), first); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	second := approvalCommand(tenant, "party-p", "precision-2")
	second.ExpectedRevision = 2
	second.Role.Code = org.RoleSupplier
	_, err = buildHandler(t, db, tenant, second.CommandID, postgres.Outbox{Writer: writer}).Handle(context.Background(), second)
	return err
}

func TestSecondRoleApprovalMicrosecondValidFrom(t *testing.T) {
	if err := approveTwoRoles(t, time.Now().UTC().Truncate(time.Microsecond), "us"); err != nil {
		t.Fatalf("second approval: %v (sqlstate %s)", err, sqlState(err))
	}
}

// Regression for V2.4: a sub-microsecond ValidFrom (e.g. time.Now()) changed the derived role_id after
// reload and the re-insert hit party_role_approved_no_overlap (23P01).
func TestSecondRoleApprovalSubMicrosecondValidFrom(t *testing.T) {
	vf := time.Now().UTC().Truncate(time.Microsecond).Add(789 * time.Nanosecond)
	if err := approveTwoRoles(t, vf, "ns"); err != nil {
		t.Fatalf("second approval: %v (sqlstate %s)", err, sqlState(err))
	}
}
