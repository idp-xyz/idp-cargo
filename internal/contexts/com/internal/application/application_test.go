package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var now = time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)

type txFake struct{ depth int }

func (x *txFake) WithinTransaction(ctx context.Context, fn func(context.Context) error) error {
	x.depth++
	defer func() { x.depth-- }()
	return fn(ctx)
}

type scopedTx struct {
	txFake
	tenant string
}

func (s *scopedTx) TenantScope() string { return s.tenant }

type storeFake struct {
	orders map[com.OrderID]com.OrderState
	revs   map[com.OrderID]int64
	saved  []com.Obligation
}

func (s *storeFake) Load(_ context.Context, _ com.TenantID, id com.OrderID) (*com.CustomerOrder, int64, error) {
	st, ok := s.orders[id]
	if !ok {
		return nil, 0, errors.New("order not found")
	}
	o, err := com.RestoreOrder(st)
	return o, s.revs[id], err
}
func (s *storeFake) Insert(_ context.Context, o *com.CustomerOrder) (int64, error) {
	s.orders[o.ID()], s.revs[o.ID()] = o.State(), 1
	return 1, nil
}
func (s *storeFake) Save(_ context.Context, o *com.CustomerOrder, expected int64) (int64, error) {
	if s.revs[o.ID()] != expected {
		return 0, com.ErrConflict
	}
	s.saved = append(s.saved, o.Added()...)
	s.orders[o.ID()] = o.State()
	s.revs[o.ID()]++
	return s.revs[o.ID()], nil
}

type receiptsFake map[string]Receipt

func (r receiptsFake) Find(_ context.Context, _ com.TenantID, id string) (Receipt, bool, error) {
	got, ok := r[id]
	return got, ok, nil
}
func (r receiptsFake) Save(_ context.Context, _ com.TenantID, got Receipt) error {
	r[got.CommandID] = got
	return nil
}

type outboxFake struct{ events []string }

func (o *outboxFake) EnqueueCOMEvent(_ context.Context, e com.Event) error {
	o.events = append(o.events, strings.TrimSuffix(e.Type+"/"+string(e.ObligationID), "/"))
	return nil
}

type authFake struct {
	fail  error
	calls int
}

func (a *authFake) RequireOrderAction(context.Context, Scope, OrderAction, com.OrderID) error {
	a.calls++
	return a.fail
}

type quotesFake struct {
	tx       *txFake
	fail     error
	calls    int
	insideTx bool
}

func (q *quotesFake) AcceptedQuote(context.Context, com.TenantID, string) (com.AcceptedQuote, error) {
	q.calls++
	q.insideTx = q.insideTx || q.tx.depth > 0
	if q.fail != nil {
		return com.AcceptedQuote{}, q.fail
	}
	return com.AcceptedQuote{QuoteID: "Q-1", QuoteRevision: 3, LegalEntityID: "LE", CustomerPartyID: "P", CustomerRevision: 2,
		Total: kernel.Money{Currency: "USD", Minor: 100_000}, Validity: kernel.ValidityInterval{From: now.Add(-time.Hour), Until: now.Add(14 * 24 * time.Hour)},
		AcceptedAt: now.Add(-30 * time.Minute), EvidenceRef: "signed-quote", TermsHash: strings.Repeat("cd", 32)}, nil
}

type customersFake struct {
	tx       *txFake
	fail     error
	calls    int
	insideTx bool
}

func (c *customersFake) VerifyCustomer(_ context.Context, _ com.TenantID, party string, _ com.LegalEntityID, _ time.Time) (com.CustomerSnapshot, error) {
	c.calls++
	c.insideTx = c.insideTx || c.tx.depth > 0
	if c.fail != nil {
		return com.CustomerSnapshot{}, c.fail
	}
	return com.CustomerSnapshot{PartyID: party, PartyRevision: 2, LegalName: "Fixture Corp", CountryCode: "US",
		RoleValidity: kernel.ValidityInterval{From: now.Add(-time.Hour)}, RoleEvidenceRef: "customer-contract"}, nil
}

type fixture struct {
	deps      Deps
	store     *storeFake
	outbox    *outboxFake
	auth      *authFake
	quotes    *quotesFake
	customers *customersFake
}

func newFixture() *fixture {
	tx := &txFake{}
	f := &fixture{store: &storeFake{orders: map[com.OrderID]com.OrderState{}, revs: map[com.OrderID]int64{}},
		outbox: &outboxFake{}, auth: &authFake{}, quotes: &quotesFake{tx: tx}, customers: &customersFake{tx: tx}}
	f.deps = Deps{Tx: tx, Orders: f.store, Receipts: receiptsFake{}, Outbox: f.outbox, Auth: f.auth}
	return f
}

func kg(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }

func recordCommand(id string) RecordCustomerOrderCommand {
	return RecordCustomerOrderCommand{CommandID: id, Scope: Scope{TenantID: "T", ActorID: "sales-1"}, OrderID: "O-1", QuoteID: "Q-1",
		CustomerPartyID: "P", Lines: []com.OrderLine{{Description: "Pallets of machine parts", Quantity: kg(500)}}, OccurredAt: now}
}

func (f *fixture) recorder() RecordCustomerOrder {
	return RecordCustomerOrder{Deps: f.deps, Quotes: f.quotes, Customers: f.customers}
}

func TestRecordOrderAsksRATEAndORGOutsideTransactionsAndReplays(t *testing.T) {
	f := newFixture()
	r, err := f.recorder().Handle(context.Background(), recordCommand("record-1"))
	if err != nil {
		t.Fatal(err)
	}
	stored := f.store.orders["O-1"]
	if r.NewRevision != 1 || stored.Status != com.OrderRecorded || stored.Quote.QuoteRevision != 3 || stored.Customer.LegalName != "Fixture Corp" ||
		fmt.Sprint(f.outbox.events) != "[CustomerOrderRecorded.v1]" {
		t.Fatalf("receipt %+v, order %+v, events %v", r, stored, f.outbox.events)
	}
	if f.quotes.calls != 1 || f.customers.calls != 1 || f.quotes.insideTx || f.customers.insideTx {
		t.Fatalf("RATE calls %d (inside a transaction %v), ORG calls %d (inside a transaction %v)",
			f.quotes.calls, f.quotes.insideTx, f.customers.calls, f.customers.insideTx)
	}
	if again, err := f.recorder().Handle(context.Background(), recordCommand("record-1")); err != nil || again != r || f.quotes.calls != 1 || len(f.outbox.events) != 1 {
		t.Fatalf("replay = %+v, %v; RATE calls %d; events %v", again, err, f.quotes.calls, f.outbox.events)
	}
	changed := recordCommand("record-1")
	changed.Lines[0].Quantity = kg(600)
	if _, err := f.recorder().Handle(context.Background(), changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed payload under the same command id: err = %v", err)
	}
}

func TestRecordOrderStopsBeforeWritingWhenRefused(t *testing.T) {
	f := newFixture()
	f.auth.fail = errors.New("policy deny")
	if _, err := f.recorder().Handle(context.Background(), recordCommand("record-1")); !errors.Is(err, ErrUnauthorized) || f.quotes.calls != 0 {
		t.Fatalf("unauthorized: err = %v, RATE calls %d", err, f.quotes.calls)
	}
	f.auth.fail = nil
	f.quotes.fail = fmt.Errorf("%w: not accepted", ErrQuoteNotAccepted)
	if _, err := f.recorder().Handle(context.Background(), recordCommand("record-2")); !errors.Is(err, ErrQuoteNotAccepted) || f.customers.calls != 0 {
		t.Fatalf("quote not accepted: err = %v, ORG calls %d", err, f.customers.calls)
	}
	f.quotes.fail = nil
	other := recordCommand("record-3")
	other.CustomerPartyID = "OTHER"
	if _, err := f.recorder().Handle(context.Background(), other); !errors.Is(err, com.ErrQuoteMismatch) || f.customers.calls != 0 {
		t.Fatalf("another customer's quote: err = %v, ORG calls %d", err, f.customers.calls)
	}
	f.customers.fail = fmt.Errorf("%w: role ended", ErrCustomerNotConfirmed)
	if _, err := f.recorder().Handle(context.Background(), recordCommand("record-4")); !errors.Is(err, ErrCustomerNotConfirmed) {
		t.Fatalf("ORG refusal: err = %v", err)
	}
	if len(f.store.orders) != 0 || len(f.outbox.events) != 0 {
		t.Fatalf("refused commands wrote orders %d, events %v", len(f.store.orders), f.outbox.events)
	}
	h := f.recorder()
	h.Tx = &scopedTx{tenant: "OTHER"}
	if _, err := h.Handle(context.Background(), recordCommand("record-5")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("foreign tenant scope: err = %v", err)
	}
}

func TestAllocateChecksTheRevisionAndWritesOnce(t *testing.T) {
	f := newFixture()
	if _, err := f.recorder().Handle(context.Background(), recordCommand("record-1")); err != nil {
		t.Fatal(err)
	}
	allocate := func(command string, revision, amount int64, obligation com.ObligationID) (Receipt, error) {
		return AllocateOrderToObligation{Deps: f.deps}.Handle(context.Background(), AllocateOrderToObligationCommand{
			CommandID: command, Scope: Scope{TenantID: "T", ActorID: "planner-1"}, OrderID: "O-1", ExpectedRevision: revision,
			LineNo: 1, ObligationID: obligation, Quantity: kg(amount), OccurredAt: now.Add(time.Hour)})
	}
	if _, err := allocate("allocate-0", 2, 200, "OB-1"); !errors.Is(err, com.ErrConflict) {
		t.Fatalf("stale revision: err = %v", err)
	}
	r, err := allocate("allocate-1", 1, 200, "OB-1")
	if err != nil || r.NewRevision != 2 || f.store.orders["O-1"].Status != com.OrderPartiallyAllocated {
		t.Fatalf("allocate: %+v, %v", r, err)
	}
	if again, err := allocate("allocate-1", 1, 200, "OB-1"); err != nil || again != r {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if _, err := allocate("allocate-2", 2, 301, "OB-2"); !errors.Is(err, com.ErrOverAllocation) || f.store.revs["O-1"] != 2 {
		t.Fatalf("over-allocation: err = %v, revision %d", err, f.store.revs["O-1"])
	}
	if r, err = allocate("allocate-3", 2, 300, "OB-2"); err != nil || r.NewRevision != 3 || f.store.orders["O-1"].Status != com.OrderAllocated {
		t.Fatalf("allocate the rest: %+v, %v", r, err)
	}
	if len(f.store.saved) != 2 || f.store.saved[0].ObligationID != "OB-1" || f.store.saved[1].ObligationID != "OB-2" {
		t.Fatalf("saved obligations %+v", f.store.saved)
	}
	if got := fmt.Sprint(f.outbox.events); got != "[CustomerOrderRecorded.v1 FulfillmentObligationAllocated.v1/OB-1 FulfillmentObligationAllocated.v1/OB-2]" {
		t.Fatalf("events %s", got)
	}
}
