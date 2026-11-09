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
	deny  map[QuoteAction]bool
	calls int
}

func (a *authFake) RequireQuoteAction(_ context.Context, _ Scope, action QuoteAction, _ rate.QuoteID) error {
	a.calls++
	if a.deny[action] {
		return errors.New("policy deny " + string(action))
	}
	return a.fail
}

// clockFake is the time RATE records a command at.
type clockFake struct{ t time.Time }

func (c *clockFake) Now() time.Time { return c.t }

type customersFake struct {
	tx       *txFake
	revision int64
	fail     error
	calls    int
	roles    []rate.RecipientRole
	insideTx bool
}

func (c *customersFake) VerifyRecipient(_ context.Context, _ rate.TenantID, party string, _ rate.LegalEntityID, role rate.RecipientRole, _ time.Time) (rate.CustomerRef, error) {
	c.calls++
	c.roles = append(c.roles, role)
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
	clock     *clockFake
	tx        *txFake
	store     *storeFake
	outbox    *outboxFake
	auth      *authFake
	customers *customersFake
}

func newFixture() *fixture {
	tx := &txFake{}
	f := &fixture{tx: tx, store: &storeFake{quotes: map[rate.QuoteID]rate.QuoteState{}, revs: map[rate.QuoteID]int64{}},
		outbox: &outboxFake{}, auth: &authFake{}, customers: &customersFake{tx: tx, revision: 2}, clock: &clockFake{t: now.Add(2 * time.Hour)}}
	f.deps = Deps{Tx: tx, Quotes: f.store, Receipts: receiptsFake{}, Outbox: f.outbox, Auth: f.auth, Clock: f.clock, Window: kernel.DefaultRecordWindow}
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
		ExpectedRevision: 1, EvidenceKind: rate.EvidenceEmail, EvidenceRef: "signed-quote", OccurredAt: now.Add(time.Hour)}
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
	if stored.Status != rate.QuoteAccepted || stored.Acceptance.CustomerRevision != 3 || f.customers.insideTx ||
		!stored.Acceptance.RecordedAt.Equal(f.clock.t) || stored.Acceptance.EvidenceKind != rate.EvidenceEmail {
		t.Fatalf("stored %+v; ORG asked inside a transaction: %v", stored, f.customers.insideTx)
	}
	if got := fmt.Sprint(f.outbox.events); got != "[QuotePriced.v1 QuotePriceApproved.v1 QuoteAccepted.v1]" {
		t.Fatalf("events %s", got)
	}
}

// T1: a command's business time may lie up to the record window before the time RATE records it, and not
// after it; a command committed earlier still replays later.
func TestBusinessTimeMustLieWithinTheRecordWindow(t *testing.T) {
	f := newFixture()
	h := CreateQuote{Deps: f.deps, Customers: f.customers, Margins: floorFake(0)}
	for name, occurred := range map[string]time.Time{
		"eight days late":       f.clock.t.Add(-8 * 24 * time.Hour),
		"an hour in the future": f.clock.t.Add(time.Hour),
	} {
		c := createCommand("create-" + name)
		c.OccurredAt = occurred
		if _, err := h.Handle(context.Background(), c); !errors.Is(err, kernel.ErrOutsideRecordWindow) || f.customers.calls != 0 {
			t.Errorf("%s: err = %v, ORG calls %d", name, err, f.customers.calls)
		}
	}
	r := f.create(t, 0)
	f.clock.t = f.clock.t.Add(10 * 24 * time.Hour)
	if again := f.create(t, 0); again != r {
		t.Fatalf("a committed command replays outside the window: %+v vs %+v", again, r)
	}
}

// R7: a quote goes to a customer by default, or to an overseas agent, shipper or consignee, whom ORG
// must confirm in that role; acceptance asks ORG again for the same role.
func TestTheRecipientRoleIsCheckedWithORG(t *testing.T) {
	f := newFixture()
	f.create(t, 0)
	agent := createCommand("create-agent")
	agent.QuoteID, agent.Recipient = "Q-2", rate.RecipientOverseasAgent
	if _, err := (CreateQuote{Deps: f.deps, Customers: f.customers, Margins: floorFake(0)}).Handle(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	unknown := createCommand("create-unknown")
	unknown.QuoteID, unknown.Recipient = "Q-3", "FORWARDER"
	if _, err := (CreateQuote{Deps: f.deps, Customers: f.customers, Margins: floorFake(0)}).Handle(context.Background(), unknown); !errors.Is(err, rate.ErrInvalidQuote) {
		t.Fatalf("an unknown role: err = %v", err)
	}
	accept := AcceptQuoteCommand{CommandID: "accept-agent", Scope: Scope{TenantID: "T", ActorID: "sales-1"}, QuoteID: "Q-2",
		ExpectedRevision: 1, EvidenceKind: rate.EvidenceCustomerPortal, EvidenceRef: "portal-77", OccurredAt: now.Add(time.Hour)}
	if _, err := (AcceptQuote{Deps: f.deps, Customers: f.customers}).Handle(context.Background(), accept); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(f.customers.roles); got != "[CUSTOMER OVERSEAS_AGENT OVERSEAS_AGENT]" {
		t.Fatalf("roles asked of ORG: %s", got)
	}
	if f.store.quotes["Q-2"].Recipient != rate.RecipientOverseasAgent {
		t.Fatalf("stored recipient %s", f.store.quotes["Q-2"].Recipient)
	}
}

// R3: the author approves their own quote only when the policy allows ApproveOwnQuotePrice.
func TestTheAuthorApprovesOnlyWithTheDesignatedRole(t *testing.T) {
	f := newFixture()
	f.create(t, 2000)
	own := ApproveQuotePriceCommand{CommandID: "approve-own", Scope: Scope{TenantID: "T", ActorID: "sales-1"}, QuoteID: "Q-1",
		ExpectedRevision: 1, ReasonRef: "margin-exception-1", OccurredAt: now}
	f.auth.deny = map[QuoteAction]bool{ActionApproveOwnQuotePrice: true}
	if _, err := (ApproveQuotePrice{Deps: f.deps}).Handle(context.Background(), own); !errors.Is(err, rate.ErrSelfApproval) {
		t.Fatalf("without the role: err = %v", err)
	}
	f.auth.deny = nil
	own.CommandID = "approve-own-2"
	if _, err := (ApproveQuotePrice{Deps: f.deps}).Handle(context.Background(), own); err != nil {
		t.Fatal(err)
	}
	if a := f.store.quotes["Q-1"].Approval; a == nil || !a.SelfApproved {
		t.Fatalf("approval %+v", a)
	}
}

// R4: the scheduled job expires a quote once its validity has passed.
func TestExpireQuote(t *testing.T) {
	f := newFixture()
	f.create(t, 0)
	until := now.Add(14 * 24 * time.Hour)
	expire := ExpireQuoteCommand{CommandID: "expire-1", Scope: Scope{TenantID: "T", ActorID: "rate-scheduler"}, QuoteID: "Q-1",
		ExpectedRevision: 1, OccurredAt: until.Add(-time.Hour)}
	f.clock.t = until.Add(time.Minute)
	if _, err := (ExpireQuote{Deps: f.deps}).Handle(context.Background(), expire); !errors.Is(err, rate.ErrInvalidTransition) {
		t.Fatalf("still valid: err = %v", err)
	}
	expire.CommandID, expire.OccurredAt = "expire-2", until
	r, err := (ExpireQuote{Deps: f.deps}).Handle(context.Background(), expire)
	if err != nil || r.NewRevision != 2 || f.store.quotes["Q-1"].Status != rate.QuoteExpired {
		t.Fatalf("expire: %+v, %v, %s", r, err, f.store.quotes["Q-1"].Status)
	}
	if got := fmt.Sprint(f.outbox.events); got != "[QuotePriced.v1 QuoteExpired.v1]" {
		t.Fatalf("events %s", got)
	}
}
