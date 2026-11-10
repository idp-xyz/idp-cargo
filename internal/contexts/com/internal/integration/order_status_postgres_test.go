//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var noVolume = kernel.Quantity{}

// TestTheOrderStatusFollowsItsLinesAndObligationsAtCommit writes past COM as the application role: a write
// that changes what an order's status derives from, without the status, is refused when the transaction
// commits; with the status COM would write, it commits and COM goes on with the order.
func TestTheOrderStatusFollowsItsLinesAndObligationsAtCommit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	drums := api.OrderLine{Description: "Drums of resin", Packs: 2, PackageType: "DR", GrossWeight: kg(400)}
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", drums)); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"OB-A", "OB-B"} {
		if _, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-"+id, int64(i+1), 1, id, cargo{1, kg(200), noVolume})); err != nil {
			t.Fatal(err)
		}
	}
	revokedAt := f.at.Add(4 * time.Hour)
	refusedAtCommit := func(why string, statements ...func(context.Context, bentopg.Executor) error) {
		t.Helper()
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range statements {
				if err := s(ctx, e); err != nil {
					t.Fatalf("%s: a statement failed before commit: %v", why, err)
				}
			}
			return nil
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "customer_order_status_derived" {
			t.Errorf("%s: err = %v, want customer_order_status_derived at commit", why, err)
		}
	}
	exec := func(sql string, args ...any) func(context.Context, bentopg.Executor) error {
		return func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, sql, append([]any{f.tenant}, args...)...)
			return err
		}
	}
	refusedAtCommit("a revocation without the order's status", exec(revokeSQL, "OB-A", revokedAt))
	refusedAtCommit("a status the obligations do not make", exec(`UPDATE cargo_com.customer_order SET status='PARTIALLY_ALLOCATED' WHERE tenant_id=$1`))
	refusedAtCommit("a line added to an allocated order without its status",
		exec(`INSERT INTO cargo_com.order_line (tenant_id,order_id,line_no,description,package_type,packs,gross_weight_unit,gross_weight,
            added_by,added_at,added_entered_at,added_reason,change_no) VALUES ($1,'O-1',2,'Cartons','CT',2,'KGM',15,'rogue',now(),now(),'rogue',1)`))
	refusedAtCommit("more packs on an allocated line without its status",
		exec(`INSERT INTO cargo_com.order_line_amendment (tenant_id,order_id,line_no,line_version,description,package_type,packs,
            gross_weight_unit,gross_weight,amended_by,amended_at,entered_at,reason,change_no)
            VALUES ($1,'O-1',1,2,'Drums of resin','DR',3,'KGM',600,'rogue',now(),now(),'rogue',1)`))
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		if err := exec(revokeSQL, "OB-B", revokedAt)(ctx, e); err != nil {
			return err
		}
		return exec(statusSQL, "O-1")(ctx, e)
	}); err != nil {
		t.Fatalf("a revocation with the status COM derives: %v", err)
	}
	var status string
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT status FROM cargo_com.customer_order WHERE tenant_id=$1 AND order_id='O-1'`, f.tenant).Scan(&status)
	}); err != nil || status != "PARTIALLY_ALLOCATED" {
		t.Fatalf("status %s, %v", status, err)
	}
	// COM restores the order written in SQL and allocates the freed drum again.
	if _, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-OB-C", f.revision(t, "O-1"), 1, "OB-C", cargo{1, kg(200), noVolume})); err != nil {
		t.Fatalf("COM goes on with the order: %v", err)
	}
	if s := f.snapshotV2(t, "OB-B", 2); s.Status != api.ObligationRevoked {
		t.Fatalf("OB-B version 2: %+v", s)
	}
}
