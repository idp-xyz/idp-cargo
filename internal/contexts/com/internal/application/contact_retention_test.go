package application

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

type retentionReaderFake struct {
	asOf  time.Time
	limit int
}

func (r *retentionReaderFake) DueForContactErasure(_ context.Context, _ com.TenantID, asOf time.Time, limit int) ([]DueOrder, error) {
	r.asOf, r.limit = asOf, limit
	return []DueOrder{{OrderID: "O-1", RetainedUntil: asOf}}, nil
}

// An allocated order is closed; its contacts can be erased only once COM's clock passes the retention's end,
// and the erasure leaves the order's revision as it was (P1).
func TestCloseAndEraseThroughTheApplication(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	if _, err := f.recorder().Handle(ctx, recordCommand("record")); err != nil {
		t.Fatal(err)
	}
	closer := CloseCustomerOrder{Deps: f.deps}
	closeAt := now.Add(2 * time.Hour)
	closing := CloseCustomerOrderCommand{CommandID: "close", Scope: Scope{TenantID: "T", ActorID: "cs-1"}, OrderID: "O-1", ExpectedRevision: 2, OccurredAt: closeAt}
	if _, err := closer.Handle(ctx, closing); !errors.Is(err, com.ErrConflict) {
		t.Fatalf("closing at a revision the order is not at: %v", err)
	}
	early := closing
	early.CommandID, early.ExpectedRevision = "close-early", 1
	if _, err := closer.Handle(ctx, early); !errors.Is(err, com.ErrInvalidTransition) {
		t.Fatalf("closing before the line is allocated: %v", err)
	}
	if _, err := f.allocate("allocate", 1, "OB-1", packs(10, 500)); err != nil {
		t.Fatal(err)
	}
	r, err := closer.Handle(ctx, closing)
	if err != nil || r.NewRevision != 3 {
		t.Fatalf("close: %+v %v", r, err)
	}
	if !slices.Contains(f.auth.asked, "CloseCustomerOrder/O-1") || !slices.Contains(f.outbox.events, "CustomerOrderClosed.v1") {
		t.Fatalf("asked %v, events %v", f.auth.asked, f.outbox.events)
	}
	retention := *f.store.orders["O-1"].Retention
	if retention.Months != com.MinContactRetentionMonths || !retention.StartedAt.Equal(closeAt) {
		t.Fatalf("retention %+v", retention)
	}

	eraser := EraseOrderContacts{Deps: f.deps}
	erase := EraseOrderContactsCommand{CommandID: "erase", Scope: Scope{TenantID: "T", ActorID: "dpo"}, OrderID: "O-1", OccurredAt: now.Add(7 * time.Hour)}
	if _, err = eraser.Handle(ctx, erase); !errors.Is(err, com.ErrContactsRetained) {
		t.Fatalf("erasing within the retention: %v", err)
	}
	f.clock.at = retention.RetainedUntil
	stale := erase
	stale.CommandID = "erase-stale"
	if _, err = eraser.Handle(ctx, stale); !errors.Is(err, kernel.ErrOutsideRecordWindow) {
		t.Fatalf("a business time three years before COM records it: %v", err)
	}
	erase.CommandID, erase.OccurredAt = "erase-due", retention.RetainedUntil
	r, err = eraser.Handle(ctx, erase)
	if err != nil || r.NewRevision != 3 || f.store.revs["O-1"] != 3 || f.store.orders["O-1"].Erasure == nil {
		t.Fatalf("erase: %+v %v, revision %d", r, err, f.store.revs["O-1"])
	}
	if again, err := eraser.Handle(ctx, erase); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	erase.CommandID = "erase-twice"
	if _, err = eraser.Handle(ctx, erase); !errors.Is(err, com.ErrInvalidTransition) {
		t.Fatalf("erasing twice: %v", err)
	}
}

func TestCancellationRetainsContactsForTheConfiguredMonths(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	if _, err := f.recorder().Handle(ctx, recordCommand("record")); err != nil {
		t.Fatal(err)
	}
	f.deps.RetentionMonths = 48
	if _, err := (CancelCustomerOrder{Deps: f.deps}).Handle(ctx, CancelCustomerOrderCommand{CommandID: "cancel", Scope: Scope{TenantID: "T", ActorID: "sales-1"},
		OrderID: "O-1", ExpectedRevision: 1, OccurredAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if r := f.store.orders["O-1"].Retention; r == nil || r.Cause != com.RetentionCancelled || r.Months != 48 {
		t.Fatalf("retention %+v", r)
	}
}

func TestListOrdersDueForContactErasure(t *testing.T) {
	f := newFixture()
	reader := &retentionReaderFake{}
	list := ListOrdersDueForContactErasure{Tx: f.deps.Tx, Orders: reader, Auth: f.auth, Clock: f.clock}
	s := Scope{TenantID: "T", ActorID: "privacy-job"}
	for _, limit := range []int{0, MaxDueOrders + 1} {
		if _, _, err := list.Handle(context.Background(), s, limit); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
	due, asOf, err := list.Handle(context.Background(), s, 50)
	if err != nil || len(due) != 1 || !asOf.Equal(f.clock.at) || reader.limit != 50 || !slices.Contains(f.auth.asked, "EraseOrderContacts/") {
		t.Fatalf("due %v at %s: %v (asked %v)", due, asOf, err, f.auth.asked)
	}
	f.auth.fail = errors.New("not the privacy job")
	if _, _, err = list.Handle(context.Background(), s, 50); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthorized: %v", err)
	}
}
