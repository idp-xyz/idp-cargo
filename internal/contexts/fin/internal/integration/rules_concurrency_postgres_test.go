//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	finpg "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/adapters/postgres"
)

// FIN V2.97, batch 2: a decision and the settings it uses are of one moment. A preview, a submission and a close take
// the tenant's settings lock in shared mode, read FIN's clock after it and stamp the decision with that time; a
// settings command takes it exclusively and stamps its time after it. So the version a decision records is the one the
// ledger holds at the decision's recorded time. The tests hold and queue the lock deterministically (pg_blocking_pids).

// holdSettingsLock holds the tenant's settings lock (the exclusive or the shared statement) on a connection of its own
// until release is called; pid is its backend.
func holdSettingsLock(t *testing.T, tenant, statement string) (pid int, release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, statement, tenant); err == nil {
		err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	}
	if err != nil {
		t.Fatalf("holding the settings lock: %v", err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback(ctx)
			conn.Close(ctx)
		}
	}
	t.Cleanup(release)
	return pid, release
}

// waitingInTheSettingsLock counts the sessions that wait for the tenant's settings lock.
func waitingInTheSettingsLock(t *testing.T) int {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var n int
	if err = conn.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
        WHERE l.locktype = 'advisory' AND NOT l.granted AND a.query LIKE '%cargo_fin.settings/%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The review's case. F1 is UNRESTRICTED (version 1) and ops-2 previews the attribution of SH-C under it. A tightening
// of F1 to NOT_ALLOWED is queued for the settings lock, then the submission by ops-1, the asker, who is barred under
// the new value. The tightening commits first (version 2); the submission, which waited for it, reads version 2: it is
// refused as stale, saying what changed and the way out, and a new preview submitted by ops-1 is refused as the
// asker's.
func TestASubmissionWaitsForASettingsChangeAndReadsItsVersionOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	free := u.configure(time0, le("fin.F1", "UNRESTRICTED"))
	pre := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-1", "ops-2", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))))
	pid, release := holdSettingsLock(t, u.tenant, finpg.SettingsLockSQL)
	var wg sync.WaitGroup
	var proposeErr, submitErr error
	var submitted api.Receipt
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, proposeErr = u.module.ProposeFinSettingsChange(u.ctx, api.ProposeFinSettingsChange{CommandID: "tighten-1", TenantID: u.tenant, ActorID: "ann", ChangeID: "tighten",
			Entries: []api.FinSettingEntry{le("fin.F1", "NOT_ALLOWED")}, Reason: "the asker may not confirm"})
	}()
	waitBlockedBy(t, pid, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		submitted, submitErr = u.submit("submit-1", "ops-1", int(pre.Version), 6)
	}()
	waitBlockedBy(t, pid, 2)
	if n := waitingInTheSettingsLock(t); n != 2 {
		t.Fatalf("%d sessions wait in the settings lock, want 2", n)
	}
	release()
	wg.Wait()
	if proposeErr != nil {
		t.Fatal(proposeErr)
	}
	u.refusedWith("the submission that waited", submitErr, "FIN_PREVIEW_STALE")
	if !strings.Contains(submitErr.Error(), fmt.Sprintf("the settings changed from version %d to %d", free, free+1)) || !strings.Contains(submitErr.Error(), "preview again") {
		t.Fatalf("the refusal says what changed and the way out: %v (%+v)", submitErr, submitted)
	}
	if n := len(u.ap.pool(u.pool).Apportionments); n != 1 {
		t.Fatalf("a refused submission was written: %d apportionments", n)
	}
	_, err := u.attributeBy("again", "ops-2", "ops-1", 7, statementOf("C", 5_000, 10_000, api.PartyWarehouse))
	u.refusedWith("the barred asker after the preview again", err, "FIN_ATTRIBUTION_CONFIRMER")
}

// hookClock is FIN's clock that stops at its nth reading until it is let go, and says when it is there.
type hookClock struct {
	mu      sync.Mutex
	calls   int
	at      int
	reached chan struct{}
	proceed chan struct{}
}

func (c *hookClock) Now() time.Time {
	c.mu.Lock()
	c.calls++
	hit := c.calls == c.at
	c.mu.Unlock()
	if hit {
		close(c.reached)
		<-c.proceed
	}
	return time.Now().UTC()
}

// A settings command waits for a decision that holds the shared lock, and its recorded time is after the decision's;
// the decision is stamped with the time it read its settings at, which is after the lock was granted. The decision's
// clock is stopped at the reading it makes after it took the lock (its third: the module's, the command's, then the
// decision's; the test checks that the lock is held there), a tightening is proposed meanwhile and waits for the
// decision, which is let go once the tightening queues; it commits with the version before the tightening, stamped
// after the moment the clock was let go and before the tightening's time. This is shown for a preview, a submission and
// a close.
func TestASettingsChangeWaitsForADecisionAndIsStampedAfterItOnPostgres(t *testing.T) {
	for _, decision := range []string{"preview", "submission", "close"} {
		u := formerWorld(t)
		u.depart()
		statement := statementOf("C", 5_000, 10_000, api.PartyWarehouse)
		pre := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-1", "ops-2", 6, statement)))
		if decision == "close" {
			u.must(u.submit("submit-1", "fin-2", int(pre.Version), 6))
			u.settleInvoice(7)
		}
		clock := &hookClock{at: 3, reached: make(chan struct{}), proceed: make(chan struct{})}
		slow, err := finmodule.New(u.db, u.w, u.w, u.w, u.w, u.w, allow{}, finmodule.WithClock(clock))
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var decisionErr, proposeErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch decision {
			case "preview":
				_, decisionErr = slow.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-2", "ops-2", 6, statement))
			case "submission":
				_, decisionErr = slow.SubmitConsolCostApportionment(u.ctx, u.submitCmd("submit-1", "fin-2", int(pre.Version), 6))
			default:
				_, decisionErr = slow.CloseConsolCostPool(u.ctx, api.CloseConsolCostPool{CommandID: "close", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool,
					ExpectedRevision: u.revision(), OccurredAt: u.hours(8)})
			}
		}()
		select {
		case <-clock.reached:
		case <-time.After(20 * time.Second):
			t.Fatalf("%s: the decision did not reach its settings", decision)
		}
		// the decision holds the settings lock in shared mode, granted
		conn, err := pgx.Connect(u.ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		var holder int
		var mode string
		if err = conn.QueryRow(u.ctx, `SELECT pid, mode FROM pg_locks WHERE locktype = 'advisory' AND granted AND mode = 'ShareLock'`).Scan(&holder, &mode); err != nil {
			t.Fatalf("%s holds the settings lock: %d %s %v", decision, holder, mode, err)
		}
		conn.Close(u.ctx)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, proposeErr = u.module.ProposeFinSettingsChange(u.ctx, api.ProposeFinSettingsChange{CommandID: "tighten-1", TenantID: u.tenant, ActorID: "ann", ChangeID: "tighten",
				Entries: []api.FinSettingEntry{le("fin.F7", "BLOCKS")}, Reason: "a pending cause holds the close back"})
		}()
		waitBlockedBy(t, holder, 1)
		letGo := time.Now().UTC()
		close(clock.proceed)
		wg.Wait()
		if decisionErr != nil || proposeErr != nil {
			t.Fatalf("%s: %v; the tightening: %v", decision, decisionErr, proposeErr)
		}
		p := u.ap.pool(u.pool)
		var version int
		var stamped time.Time
		switch decision {
		case "preview":
			v := p.Previews[len(p.Previews)-1]
			version, stamped = v.SettingsVersion, v.EnteredAt
		case "submission":
			a := p.Apportionments[len(p.Apportionments)-1]
			version, stamped = a.SettingsVersion, a.EnteredAt
		default:
			c := p.Closures[len(p.Closures)-1]
			version = c.SettingsVersion
			// the pool's view does not show when a closure was entered
			if err = u.fixture.within(t, func(ctx context.Context, e bentopg.Executor) error {
				return e.QueryRow(ctx, `SELECT entered_at FROM cargo_fin.consol_cost_pool_closure WHERE tenant_id=$1`, u.tenant).Scan(&stamped)
			}); err != nil {
				t.Fatal(err)
			}
		}
		changes, err := u.module.ListFinSettingsChanges(u.ctx, api.ListFinSettingsChanges{TenantID: u.tenant})
		if err != nil || len(changes) != 1 {
			t.Fatalf("%v %v", changes, err)
		}
		if version != 0 || changes[0].Version != 1 || !stamped.After(letGo) || !changes[0].ProposedAt.After(stamped) || !changes[0].ApprovedAt.After(stamped) {
			t.Fatalf("%s (version %d, stamped %s, the clock let go at %s) and the tightening (version %d, proposed %s)", decision, version, stamped, letGo,
				changes[0].Version, changes[0].ProposedAt)
		}
	}
}

// The review's stress probe: previews are made while change sets are proposed, and none fails (a reader outside the
// lock used to meet the entries of a change set it had not read).
func TestPreviewsWhileSettingsChangeFailWithNoInternalErrorOnPostgres(t *testing.T) {
	u := previewWorld(t)
	var wg sync.WaitGroup
	var proposeErrs, previewErrs []error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			_, err := u.module.ProposeFinSettingsChange(u.ctx, api.ProposeFinSettingsChange{CommandID: fmt.Sprintf("stress-%d", i), TenantID: u.tenant, ActorID: "ann",
				ChangeID: fmt.Sprintf("stress-%d", i), Entries: []api.FinSettingEntry{le("fin.F7", "BLOCKS"), le("fin.F2", "SUBMITTER_AND_PREVIEWER")}, Reason: "stress"})
			if err != nil {
				proposeErrs = append(proposeErrs, err)
			}
		}
	}()
	for i := 0; i < 40; i++ {
		if _, err := u.preview(fmt.Sprintf("stress-preview-%d", i), 4, nil, "SH-C"); err != nil {
			previewErrs = append(previewErrs, err)
		}
	}
	wg.Wait()
	if len(proposeErrs) > 0 || len(previewErrs) > 0 {
		t.Fatalf("%d proposals and %d previews failed: %v, %v", len(proposeErrs), len(previewErrs), proposeErrs, previewErrs)
	}
	if v := u.settingsVersion(); v != 60 {
		t.Fatalf("the version in force: %d", v)
	}
}
