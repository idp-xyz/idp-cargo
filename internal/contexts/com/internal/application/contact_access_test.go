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

type partyReaderFake struct{ store *storeFake }

func (r partyReaderFake) ReadOrder(ctx context.Context, tenant com.TenantID, id com.OrderID) (*com.CustomerOrder, int64, error) {
	return r.store.Load(ctx, tenant, id)
}

func (partyReaderFake) ReadContacts(context.Context, com.TenantID, com.OrderID) (map[string]map[int64]com.ContactSnapshot, error) {
	return map[string]map[int64]com.ContactSnapshot{com.PartyShipper: {1: {Source: com.FromORG, ORGContactID: "K1", ORGVersion: 1,
		Name: "Test Person A", Email: "a@example.test"}}}, nil
}

type accessLogFake struct {
	reads []ContactAccess
	fail  error
}

func (l *accessLogFake) RecordContactsRead(_ context.Context, _ com.TenantID, r ContactAccess) error {
	if l.fail != nil {
		return l.fail
	}
	l.reads = append(l.reads, r)
	return nil
}

func (l *accessLogFake) ListContactsReads(context.Context, com.TenantID, AccessFilter) ([]ContactAccess, error) {
	return l.reads, nil
}

// Reads of order contacts state their purpose and are logged; once the order is cancelled they are
// restricted to a customs audit, a legal claim or the person's own request (privacy assessment V1.2, P7).
func TestOrderContactReadsStateTheirPurposeAndAreLogged(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	if _, err := f.recorder().Handle(ctx, recordCommand("record")); err != nil {
		t.Fatal(err)
	}
	log := &accessLogFake{}
	q := GetOrderParties{Tx: f.deps.Tx, Orders: partyReaderFake{f.store}, Auth: f.auth, Log: log}
	clerk := Scope{TenantID: "T", ActorID: "doc-officer"}
	if _, _, _, err := q.Handle(ctx, clerk, "O-1", true, ""); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("contacts without a purpose: %v", err)
	}
	if _, _, _, err := (GetOrderParties{Tx: f.deps.Tx, Orders: partyReaderFake{f.store}, Auth: f.auth}).Handle(ctx, clerk, "O-1", true,
		kernel.PurposeDocuments); !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("contacts without an access log: %v", err)
	}
	if _, _, contacts, err := q.Handle(ctx, clerk, "O-1", false, ""); err != nil || contacts != nil || len(log.reads) != 0 {
		t.Fatalf("parties without contacts need no purpose and leave no log: %v %v", contacts, log.reads)
	}
	_, _, contacts, err := q.Handle(ctx, clerk, "O-1", true, kernel.PurposeDocuments)
	if err != nil || len(contacts) != 1 || len(log.reads) != 1 {
		t.Fatalf("contacts: %v %v, logged %v", contacts, err, log.reads)
	}
	if r := log.reads[0]; r.ActorID != "doc-officer" || r.Action != string(ActionReadOrderContacts) || r.Restricted || r.Purpose != kernel.PurposeDocuments || r.Contacts != 1 {
		t.Fatalf("logged %+v", r)
	}
	log.fail = errors.New("log down")
	if _, _, contacts, err = q.Handle(ctx, clerk, "O-1", true, kernel.PurposeDocuments); err == nil || contacts != nil {
		t.Fatalf("a read that cannot be logged returned %v, %v", contacts, err)
	}
	log.fail = nil

	if _, err = (CancelCustomerOrder{Deps: f.deps}).Handle(ctx, CancelCustomerOrderCommand{CommandID: "cancel", Scope: Scope{TenantID: "T", ActorID: "sales-1"},
		OrderID: "O-1", ExpectedRevision: 1, OccurredAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = q.Handle(ctx, clerk, "O-1", true, kernel.PurposeDocuments); !errors.Is(err, ErrRestrictedPurpose) {
		t.Fatalf("restricted contacts for documents: %v", err)
	}
	f.auth.asked = nil
	if _, _, contacts, err = q.Handle(ctx, Scope{TenantID: "T", ActorID: "dpo"}, "O-1", true, kernel.PurposeDataSubjectRequest); err != nil || len(contacts) != 1 {
		t.Fatalf("restricted contacts for the person's request: %v %v", contacts, err)
	}
	if !slices.Contains(f.auth.asked, "ReadRestrictedOrderContacts/O-1") {
		t.Fatalf("asked %v", f.auth.asked)
	}
	if r := log.reads[len(log.reads)-1]; !r.Restricted || r.Action != string(ActionReadRestrictedOrderContacts) || r.Purpose != kernel.PurposeDataSubjectRequest {
		t.Fatalf("logged %+v", r)
	}

	list := ListContactAccess{Tx: f.deps.Tx, Log: log, Auth: f.auth}
	if _, err = list.Handle(ctx, Scope{TenantID: "T", ActorID: "dpo"}, AccessFilter{Limit: 0}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("no limit: %v", err)
	}
	if _, err = list.Handle(ctx, Scope{TenantID: "T", ActorID: "dpo"}, AccessFilter{Limit: 10, Since: now, Until: now}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("an empty window: %v", err)
	}
	if reads, err := list.Handle(ctx, Scope{TenantID: "T", ActorID: "dpo"}, AccessFilter{OrderID: "O-1", Limit: 10}); err != nil || len(reads) != 2 ||
		!slices.Contains(f.auth.asked, "ReadContactAccessLog/O-1") {
		t.Fatalf("log: %v %v, asked %v", reads, err, f.auth.asked)
	}
}
