package application

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
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
	quotes map[rate.QuoteID]rate.QuoteState
	revs   map[rate.QuoteID]int64
}

func (s *storeFake) Load(_ context.Context, _ rate.TenantID, id rate.QuoteID) (*rate.Quote, int64, error) {
	st, ok := s.quotes[id]
	if !ok {
		return nil, 0, errors.New("quote not found")
	}
	q, err := rate.RestoreQuote(st)
	return q, s.revs[id], err
}
func (s *storeFake) Insert(_ context.Context, q *rate.Quote) (int64, error) {
	s.quotes[q.ID()], s.revs[q.ID()] = q.State(), 1
	return 1, nil
}
func (s *storeFake) Save(_ context.Context, q *rate.Quote, expected int64) (int64, error) {
	if s.revs[q.ID()] != expected {
		return 0, rate.ErrConflict
	}
	s.quotes[q.ID()] = q.State()
	s.revs[q.ID()]++
	return s.revs[q.ID()], nil
}

type receiptsFake map[string]Receipt

func (r receiptsFake) Find(_ context.Context, _ rate.TenantID, id string) (Receipt, bool, error) {
	got, ok := r[id]
	return got, ok, nil
}
func (r receiptsFake) Save(_ context.Context, _ rate.TenantID, got Receipt) error {
	r[got.CommandID] = got
	return nil
}

type outboxFake struct{ events []string }

func (o *outboxFake) EnqueueRATEEvent(_ context.Context, e rate.Event) error {
	o.events = append(o.events, e.Type)
	return nil
}

type authFake struct {
	fail  error
	calls int
}

func (a *authFake) RequireQuoteAction(context.Context, Scope, QuoteAction, rate.QuoteID) error {
	a.calls++
	return a.fail
}

type customersFake struct {
	tx       *txFake
	revision int64
	fail     error
	calls    int
	insideTx bool
}

func (c *customersFake) VerifyCustomer(_ context.Context, _ rate.TenantID, party string, _ rate.LegalEntityID, _ time.Time) (rate.CustomerRef, error) {
	c.calls++
	c.insideTx = c.insideTx || c.tx.depth > 0
	if c.fail != nil {
		return rate.CustomerRef{}, c.fail
	}
	return rate.CustomerRef{PartyID: party, PartyRevision: c.revision}, nil
}

type floorFake int

func (f floorFake) MarginFloorBP(context.Context, rate.TenantID, rate.LegalEntityID) (int, error) {
	return int(f), nil
}

type fixture struct {
	deps      Deps
	tx        *txFake
	store     *storeFake
	outbox    *outboxFake
	auth      *authFake
	customers *customersFake
}

func newFixture() *fixture {
	tx := &txFake{}
	f := &fixture{tx: tx, store: &storeFake{quotes: map[rate.QuoteID]rate.QuoteState{}, revs: map[rate.QuoteID]int64{}},
		outbox: &outboxFake{}, auth: &authFake{}, customers: &customersFake{tx: tx, revision: 2}}
	f.deps = Deps{Tx: tx, Quotes: f.store, Receipts: receiptsFake{}, Outbox: f.outbox, Auth: f.auth}
	return f
}

func usd(minor int64) kernel.Money { return kernel.Money{Currency: "USD", Minor: minor} }

func createCommand(id string) CreateQuoteCommand {
	return CreateQuoteCommand{
		CommandID: id, Scope: Scope{TenantID: "T", ActorID: "sales-1"}, QuoteID: "Q-1", LegalEntityID: "LE", CustomerPartyID: "P",
		Currency: "USD", Lines: []rate.Line{{ChargeCode: "FREIGHT", Sell: usd(100_000), Cost: usd(90_000)}},
		Validity: kernel.ValidityInterval{From: now, Until: now.Add(14 * 24 * time.Hour)}, OccurredAt: now,
	}
}

func (f *fixture) create(t *testing.T, floorBP int) Receipt {
	t.Helper()
	r, err := CreateQuote{Deps: f.deps, Customers: f.customers, Margins: floorFake(floorBP)}.Handle(context.Background(), createCommand("create-1"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCreateQuoteAsksORGOutsideTransactionsAndReplays(t *testing.T) {
	f := newFixture()
	r := f.create(t, 2000)
	if r.NewRevision != 1 || f.store.quotes["Q-1"].Status != rate.QuoteAwaitingApproval || fmt.Sprint(f.outbox.events) != "[QuotePriced.v1]" {
		t.Fatalf("receipt %+v, quote %+v, events %v", r, f.store.quotes["Q-1"], f.outbox.events)
	}
	if f.customers.calls != 1 || f.customers.insideTx {
		t.Fatalf("ORG calls %d, inside a transaction %v", f.customers.calls, f.customers.insideTx)
	}
	if again := f.create(t, 2000); again != r || f.customers.calls != 1 || len(f.outbox.events) != 1 {
		t.Fatalf("replay = %+v; ORG calls %d; events %v", again, f.customers.calls, f.outbox.events)
	}
	changed := createCommand("create-1")
	changed.Lines[0].Sell = usd(120_000)
	if _, err := (CreateQuote{Deps: f.deps, Customers: f.customers, Margins: floorFake(2000)}).Handle(context.Background(), changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed payload under the same command id: err = %v", err)
	}
}

func TestCreateQuoteStopsBeforeWritingWhenRefused(t *testing.T) {
	f := newFixture()
	f.auth.fail = errors.New("policy deny")
	h := CreateQuote{Deps: f.deps, Customers: f.customers, Margins: floorFake(0)}
	if _, err := h.Handle(context.Background(), createCommand("create-1")); !errors.Is(err, ErrUnauthorized) || f.customers.calls != 0 {
		t.Fatalf("unauthorized: err = %v, ORG calls %d", err, f.customers.calls)
	}
	f.auth.fail = nil
	f.customers.fail = errors.New("org: not a customer")
	if _, err := h.Handle(context.Background(), createCommand("create-2")); err == nil || len(f.store.quotes) != 0 || len(f.outbox.events) != 0 {
		t.Fatalf("ORG refusal: err = %v, quotes %d, events %v", err, len(f.store.quotes), f.outbox.events)
	}
	h.Tx = &scopedTx{tenant: "OTHER"}
	if _, err := h.Handle(context.Background(), createCommand("create-3")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("foreign tenant scope: err = %v", err)
	}
}

func TestApproveThenAcceptRechecksTheCustomer(t *testing.T) {
	f := newFixture()
	f.create(t, 2000)
	approve := ApproveQuotePriceCommand{CommandID: "approve-1", Scope: Scope{TenantID: "T", ActorID: "manager-1"}, QuoteID: "Q-1",
		ExpectedRevision: 1, ReasonRef: "margin-exception-1", OccurredAt: now}
	if r, err := (ApproveQuotePrice{Deps: f.deps}).Handle(context.Background(), approve); err != nil || r.NewRevision != 2 {
		t.Fatalf("approve: %+v, %v", r, err)
	}
	accept := AcceptQuoteCommand{CommandID: "accept-1", Scope: Scope{TenantID: "T", ActorID: "sales-1"}, QuoteID: "Q-1",
		ExpectedRevision: 1, EvidenceRef: "signed-quote", OccurredAt: now.Add(time.Hour)}
	h := AcceptQuote{Deps: f.deps, Customers: f.customers}
	if _, err := h.Handle(context.Background(), accept); !errors.Is(err, rate.ErrConflict) {
		t.Fatalf("stale revision: err = %v", err)
	}
	f.customers.revision = 3
	accept.CommandID, accept.ExpectedRevision = "accept-2", 2
	r, err := h.Handle(context.Background(), accept)
	if err != nil || r.NewRevision != 3 {
		t.Fatalf("accept: %+v, %v", r, err)
	}
	stored := f.store.quotes["Q-1"]
	if stored.Status != rate.QuoteAccepted || stored.Acceptance.CustomerRevision != 3 || f.customers.insideTx {
		t.Fatalf("stored %+v; ORG asked inside a transaction: %v", stored, f.customers.insideTx)
	}
	if got := fmt.Sprint(f.outbox.events); got != "[QuotePriced.v1 QuotePriceApproved.v1 QuoteAccepted.v1]" {
		t.Fatalf("events %s", got)
	}
}
