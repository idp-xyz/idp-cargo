//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// ddlBound is how long ddl retries a statement. The policy is created on a table every PHY test uses, and `go test
// ./...` runs the packages in parallel against one database, so the statement can wait behind another package's open
// transaction; while it waits, the sessions of that package queue behind it too, and the lock tests there count only
// sessions blocked by their own holder. Each attempt therefore waits at most 300 ms (lock_timeout) and is retried, so
// the statement never queues anyone for long, until it succeeds or ddlBound passes: the transactions of the other tests
// last well under a second, so ten seconds is many attempts, and a statement that still cannot run fails the test.
const ddlBound = 10 * time.Second

// ddl runs a statement as the migrator with a short lock timeout, retrying while the lock is not free (SQLSTATE 55P03).
func ddl(ctx context.Context, conn *pgx.Conn, statement string) error {
	if _, err := conn.Exec(ctx, `SET lock_timeout = '300ms'`); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET lock_timeout`) }()
	deadline := time.Now().Add(ddlBound)
	for {
		_, err := conn.Exec(ctx, statement)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "55P03" || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestTheCostBasisIsReadInOneSnapshot places a detach between the statement that reads the live houses and the one that
// reads the former houses. A restrictive policy on membership_entry_part, for this test's tenant only, calls a function
// that waits for a shared advisory lock when the statement is the live-houses one (the only one that aggregates the
// parts by part number); a second connection holds the lock exclusively, so the basis read is blocked inside its first
// statements, and the test waits until a session is blocked by that connection (pg_blocking_pids). The detach of SH-2
// commits meanwhile, and the lock is released. In one snapshot the answer is the state when the read began: both houses
// live and none former. Read committed would have put SH-2 in both lists, as the live-houses statement saw it and the
// former-houses statement saw it detached. The policy and the function are created and dropped by the migrator role,
// which owns the table; the test skips without IDP_CARGO_MIGRATOR_DSN outside CI.
func TestTheCostBasisIsReadInOneSnapshot(t *testing.T) {
	const key = 92920001
	ctx := context.Background()
	f := newFixture(t)
	conn := f.migrator(t)
	if conn == nil {
		t.Skip("set IDP_CARGO_MIGRATOR_DSN")
	}
	defer conn.Close(ctx)
	f.create(t, "C-1")
	for _, h := range []string{"SH-1", "SH-2"} {
		if _, err := f.module.AttachHouse(ctx, f.attach("attach-"+h, "C-1", h, 0)); err != nil {
			t.Fatal(err)
		}
		f.ful.allocate(h, "C-1")
	}
	drop := func() {
		_ = ddl(ctx, conn, `DROP POLICY IF EXISTS zz_gate ON cargo_phy.membership_entry_part`)
		_ = ddl(ctx, conn, `DROP FUNCTION IF EXISTS cargo_phy.zz_gate(text)`)
	}
	drop()
	for _, q := range []string{
		fmt.Sprintf(`CREATE FUNCTION cargo_phy.zz_gate(t text) RETURNS boolean LANGUAGE sql AS $$ SELECT CASE WHEN t = '%s' AND current_query() LIKE
          '%%json_agg(p ORDER BY p.part_no)%%' THEN pg_advisory_xact_lock_shared(%d) IS NOT NULL ELSE true END $$`, f.tenant, key),
		`GRANT EXECUTE ON FUNCTION cargo_phy.zz_gate(text) TO PUBLIC`,
		`CREATE POLICY zz_gate ON cargo_phy.membership_entry_part AS RESTRICTIVE FOR SELECT USING (cargo_phy.zz_gate(tenant_id))`,
	} {
		if err := ddl(ctx, conn, q); err != nil {
			drop()
			t.Fatalf("%s: %v", q, err)
		}
	}
	defer drop()
	if _, err := conn.Exec(ctx, fmt.Sprintf(`SELECT pg_advisory_lock(%d)`, key)); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock_all()`) }()

	type result struct {
		basis api.PhysicalCostBasis
		err   error
	}
	done := make(chan result, 1)
	go func() {
		b, err := f.module.GetPhysicalCostBasis(ctx, api.GetPhysicalCostBasis{TenantID: f.tenant, ConsolidationID: "C-1"})
		done <- result{b, err}
	}()
	var blocked int
	for deadline := time.Now().Add(15 * time.Second); blocked == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if err := conn.QueryRow(ctx, `SELECT coalesce(max(pid),0) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, conn.PgConn().PID()).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
	}
	if blocked == 0 {
		t.Fatal("the basis read was never blocked by the test's lock")
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := f.module.DetachHouse(dctx, f.detach("detach-sh2", "C-1", "SH-2", 1, api.ReasonRollover, "", f.at.Add(5*time.Hour))); err != nil {
		t.Fatalf("the detach while the basis is read: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock_all()`); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.basis.Houses) != 2 || len(r.basis.FormerHouses) != 0 {
		t.Fatalf("the basis read began before the detach: houses %+v, former %+v; want both houses live and none former", r.basis.Houses, r.basis.FormerHouses)
	}
	after := f.basisOf(t, "C-1")
	if len(after.Houses) != 1 || len(after.FormerHouses) != 1 || after.FormerHouses[0].ShipmentID != "SH-2" {
		t.Fatalf("the next read: houses %+v, former %+v", after.Houses, after.FormerHouses)
	}
}
