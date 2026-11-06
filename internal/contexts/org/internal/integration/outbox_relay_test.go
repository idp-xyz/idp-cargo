//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.idp.xyz/idp-bento-go/application"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-cargo/internal/foundation/messaging"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

type outboxState struct {
	attempt, failures    int64
	published, abandoned bool
}

func readOutbox(t *testing.T, db *bentopg.DB, id eventing.EventID) outboxState {
	t.Helper()
	q, err := db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var s outboxState
	err = q.QueryRow(context.Background(), fmt.Sprintf(`SELECT attempt, failures, status = 'PUBLISHED', status = 'ABANDONED'
		FROM %s.outbox WHERE event_id = $1`, pgx.Identifier{db.Schema()}.Sanitize()), string(id)).
		Scan(&s.attempt, &s.failures, &s.published, &s.abandoned)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func enqueueAll(t *testing.T, db *bentopg.DB, store *bentooutbox.Store, envelopes ...eventing.Envelope) {
	t.Helper()
	err := db.Transactor().WithinTransaction(context.Background(), func(ctx context.Context) error {
		for _, e := range envelopes {
			if err := store.Enqueue(ctx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func testRelay(store *bentooutbox.Store, publisher eventing.Publisher, clock application.Clock, maxFailures uint32) messaging.Relay {
	return messaging.Relay{
		Outbox: store, Publisher: publisher, Clock: clock,
		Batch: 10, LeaseFor: 30 * time.Second, MaxAttempts: 10, MaxFailures: maxFailures,
		RetryBase: time.Second, RetryMax: time.Minute, IdleWait: 50 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func runOnce(t *testing.T, relay messaging.Relay, wantClaimed int) {
	t.Helper()
	claimed, err := relay.RunOnce(context.Background())
	if err != nil || claimed != wantClaimed {
		t.Fatalf("RunOnce = %d, %v; want %d claimed", claimed, err, wantClaimed)
	}
}

func TestRelayRetriesFailedPublishThenSucceeds(t *testing.T) {
	db := contractDB(t)
	store, err := bentooutbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	event := orgContractEnvelope(t, "relay-retry", "cargo-relay", "party-1", at)
	enqueueAll(t, db, store, event)
	calls := 0
	publisher := eventing.PublisherFunc(func(context.Context, eventing.Envelope) error {
		calls++
		if calls == 1 {
			return errors.New("sink unavailable")
		}
		return nil
	})
	clock := &testClock{now: at.Add(time.Minute)}
	relay := testRelay(store, publisher, clock, 5)
	runOnce(t, relay, 1) // first attempt fails; retry due one second later
	runOnce(t, relay, 0) // not due yet
	clock.now = clock.now.Add(2 * time.Second)
	runOnce(t, relay, 1)
	if s := readOutbox(t, db, event.ID); !s.published || s.attempt != 2 || s.failures != 1 {
		t.Fatalf("outbox state %+v, want published on attempt 2 after 1 failure", s)
	}
}

func TestRelayAbandonsAfterFailureBudgetAndUnblocksPartition(t *testing.T) {
	db := contractDB(t)
	store, err := bentooutbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	poison := orgContractEnvelope(t, "relay-poison", "cargo-relay", "party-1", at)
	next := orgContractEnvelope(t, "relay-next", "cargo-relay", "party-1", at.Add(time.Second))
	enqueueAll(t, db, store, poison, next)
	var delivered []eventing.EventID
	publisher := eventing.PublisherFunc(func(_ context.Context, e eventing.Envelope) error {
		if e.ID == poison.ID {
			return errors.New("sink cannot process this event")
		}
		delivered = append(delivered, e.ID)
		return nil
	})
	clock := &testClock{now: at.Add(time.Minute)}
	relay := testRelay(store, publisher, clock, 2)
	runOnce(t, relay, 1) // poison: failure 1
	clock.now = clock.now.Add(time.Minute)
	runOnce(t, relay, 1) // poison: failure 2 exhausts the budget
	clock.now = clock.now.Add(time.Minute)
	runOnce(t, relay, 1) // the successor in the same partition is the head now
	if s := readOutbox(t, db, poison.ID); !s.abandoned || s.failures != 2 {
		t.Fatalf("poison state %+v, want abandoned after 2 failures", s)
	}
	if len(delivered) != 1 || delivered[0] != next.ID || !readOutbox(t, db, next.ID).published {
		t.Fatalf("delivered %v, want only %s published", delivered, next.ID)
	}
}

// TestWorkerCrashRecoveryDrill kills cargo-worker with SIGKILL while the sink holds the first
// delivery, so it is never marked published, then starts a second worker that re-delivers it after
// the lease expires and stops gracefully on SIGTERM.
func TestWorkerCrashRecoveryDrill(t *testing.T) {
	db := contractDB(t)
	store, err := bentooutbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	event := orgContractEnvelope(t, "drill-1", "cargo-drill", "party-1", time.Now().UTC().Add(-time.Minute))
	enqueueAll(t, db, store, event)

	var mu sync.Mutex
	received := map[eventing.EventID]int{}
	firstArrived := make(chan struct{})
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope eventing.Envelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		received[envelope.ID]++
		n := received[envelope.ID]
		mu.Unlock()
		if envelope.ID == event.ID && n == 1 {
			close(firstArrived)
			<-r.Context().Done() // hold the delivery until the worker process dies
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sink.Close()

	bin := filepath.Join(t.TempDir(), "cargo-worker")
	if out, err := exec.Command("go", "build", "-o", bin, "go.idp.xyz/idp-cargo/cmd/cargo-worker").CombinedOutput(); err != nil {
		t.Fatalf("build cargo-worker: %v\n%s", err, out)
	}
	start := func() (*exec.Cmd, *bytes.Buffer) {
		var logs bytes.Buffer
		cmd := exec.Command(bin, "-schema", db.Schema(), "-sink", sink.URL, "-lease", "3s", "-sink-timeout", "2s", "-poll", "100ms")
		cmd.Env = os.Environ()
		cmd.Stdout, cmd.Stderr = &logs, &logs
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd, &logs
	}

	first, firstLogs := start()
	select {
	case <-firstArrived:
	case <-time.After(30 * time.Second):
		_ = first.Process.Kill()
		t.Fatalf("first delivery never reached the sink:\n%s", firstLogs)
	}
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	if s := readOutbox(t, db, event.ID); s.published || s.attempt != 1 {
		t.Fatalf("after the crash: %+v, want attempt 1 and unpublished", s)
	}

	second, secondLogs := start()
	deadline := time.Now().Add(30 * time.Second)
	for !readOutbox(t, db, event.ID).published {
		if time.Now().After(deadline) {
			_ = second.Process.Kill()
			t.Fatalf("delivery was not re-published after the lease expired:\n%s", secondLogs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s := readOutbox(t, db, event.ID); s.attempt != 2 || s.failures != 0 {
		t.Fatalf("after recovery: %+v, want attempt 2 without failures", s)
	}
	if err := second.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := second.Wait(); err != nil {
		t.Fatalf("graceful shutdown: %v\n%s", err, secondLogs)
	}
	mu.Lock()
	defer mu.Unlock()
	if received[event.ID] != 2 {
		t.Fatalf("sink received the event %d times, want 2 (at-least-once redelivery)", received[event.ID])
	}
	t.Logf("second worker:\n%s", secondLogs)
}
