//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestAnAssertionPastSEACannotPassAFinalization writes a new VGM assertion past SEA while a finalization holds the
// movement. Whichever of the write's checks runs first at commit, it waits for the finalization and is then refused:
// the assertion's check reads the movement under its share lock. Before migration 000008 it read the movement
// unlocked, and an assertion written before its visit's step was accepted on the movement that became FINAL.
func TestAnAssertionPastSEACannotPassAFinalization(t *testing.T) {
	for _, assertionFirst := range []bool{true, false} {
		ctx := context.Background()
		f := newFixture(t)
		f.stuffOwnContainer(t)
		begin := func() pgx.Tx {
			t.Helper()
			c, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close(context.Background()) })
			tx, err := c.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenant); err != nil {
				t.Fatal(err)
			}
			return tx
		}
		exec := func(tx pgx.Tx, sql string) {
			t.Helper()
			if _, err := tx.Exec(ctx, sql, f.tenant); err != nil {
				t.Fatalf("assertion first %v: %s: %v", assertionFirst, sql, err)
			}
		}
		assertion := func(tx pgx.Tx) {
			exec(tx, `INSERT INTO cargo_sea.vgm_assertion (tenant_id,visit_id,version,mass_grams,method,responsible_party_id,responsible_party_revision,
          determined_at,submitted_at,recorded_by,recorded_at,entered_at)
          VALUES ($1,'CV-1',2,2300000,'METHOD_1','LE',1,'2020-01-01','2020-01-01','x','2020-01-01',now())`)
			exec(tx, `INSERT INTO cargo_sea.vgm_signatory (tenant_id,visit_id,version,name) VALUES ($1,'CV-1',2,'Grace Hopper')`)
		}
		step := func(tx pgx.Tx) {
			exec(tx, `UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1'`)
		}
		// the write past SEA; the order of its statements is the order of its checks at commit
		write := begin()
		if assertionFirst {
			assertion(write)
			step(write)
		} else {
			step(write)
			assertion(write)
		}
		// the finalization: the movement's lock as SEA takes it, held while the write commits
		finalization := begin()
		exec(finalization, `SELECT 1 FROM cargo_sea.ocean_movement WHERE tenant_id=$1 AND movement_id='OM-1' FOR NO KEY UPDATE`)
		done := make(chan error, 1)
		go func() { done <- write.Commit(ctx) }()
		watch, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { watch.Close(context.Background()) })
		waiting := false
		for i := 0; i < 100 && !waiting; i++ {
			select {
			case err := <-done:
				t.Fatalf("assertion first %v: the write did not wait for the finalization: %v", assertionFirst, err)
			case <-time.After(50 * time.Millisecond):
			}
			var kind *string
			if err = watch.QueryRow(ctx, `SELECT wait_event_type FROM pg_stat_activity WHERE pid=$1`, write.Conn().PgConn().PID()).Scan(&kind); err != nil {
				t.Fatal(err)
			}
			waiting = kind != nil && *kind == "Lock"
		}
		if !waiting {
			t.Fatalf("assertion first %v: the write is not waiting on the movement", assertionFirst)
		}
		// what a finalization writes: FINAL at the next revision, with its facts version
		exec(finalization, `UPDATE cargo_sea.ocean_movement SET state='FINAL', revision=revision+1, updated_at=now() WHERE tenant_id=$1 AND movement_id='OM-1'`)
		exec(finalization, `INSERT INTO cargo_sea.ocean_doc_facts_version (tenant_id,movement_id,version_no,content,content_sha256,movement_revision,
          finalized_by,finalized_at,entered_at)
        SELECT $1,'OM-1',1,s.c,encode(sha256(convert_to(s.c,'UTF8')),'hex'),m.revision,'x',now(),now()
        FROM cargo_sea.ocean_movement m, (SELECT '{"version":1,"movement_id":"OM-1","houses":[{"hbl":"X"}]}'::text AS c) s
        WHERE m.tenant_id=$1 AND m.movement_id='OM-1'`)
		if err = finalization.Commit(ctx); err != nil {
			t.Fatalf("assertion first %v: the finalization: %v", assertionFirst, err)
		}
		if err = <-done; sqlState(err) != "23514" || !strings.Contains(err.Error(), "while the movement is open") {
			t.Fatalf("assertion first %v: the write past SEA: %v (state %s), want 23514", assertionFirst, err, sqlState(err))
		}
		read := begin()
		var state string
		var assertions int
		if err = read.QueryRow(ctx, `SELECT m.state, (SELECT count(*) FROM cargo_sea.vgm_assertion a WHERE a.tenant_id=m.tenant_id AND a.visit_id='CV-1')
        FROM cargo_sea.ocean_movement m WHERE m.tenant_id=$1 AND m.movement_id='OM-1'`, f.tenant).Scan(&state, &assertions); err != nil {
			t.Fatal(err)
		}
		_ = read.Rollback(ctx)
		if state != "FINAL" || assertions != 1 {
			t.Fatalf("assertion first %v: the movement is %s with %d assertions, want FINAL with 1", assertionFirst, state, assertions)
		}
	}
}
