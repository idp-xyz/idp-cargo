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

var errNoOrder = errors.New("order not found")

type storeFake struct {
	orders   map[com.OrderID]com.OrderState
	revs     map[com.OrderID]int64
	saved    []com.Obligation
	reserved []com.Reservation
	released []com.Reservation
}

func (s *storeFake) Load(_ context.Context, _ com.TenantID, id com.OrderID) (*com.CustomerOrder, int64, error) {
	st, ok := s.orders[id]
	if !ok {
		return nil, 0, errNoOrder
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
	s.reserved = append(s.reserved, o.AddedReservations()...)
	s.released = append(s.released, o.ReleasedReservations()...)
	s.orders[o.ID()] = o.State()
	s.revs[o.ID()]++
	return s.revs[o.ID()], nil
}
func (s *storeFake) Erase(_ context.Context, o *com.CustomerOrder) error {
	if _, added := o.AddedErasure(); !added {
		return com.ErrInvalidTransition
	}
	s.orders[o.ID()] = o.State()
	return nil
}
func (s *storeFake) OrdersOfQuote(_ context.Context, _ com.TenantID, quote string) (int, error) {
	n := 0
	for _, st := range s.orders {
		if st.Quote.QuoteID == quote && st.Status != com.OrderCancelled {
			n++
		}
	}
	return n, nil
}
func (s *storeFake) OrderOf(_ context.Context, _ com.TenantID, id com.ObligationID) (com.OrderID, error) {
	for order, st := range s.orders {
		for _, ob := range st.Obligations {
			if ob.ObligationID == id {
				return order, nil
			}
		}
	}
	return "", errNoOrder
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
	asked []string // action/order
}

func (a *authFake) RequireOrderAction(_ context.Context, _ Scope, action OrderAction, id com.OrderID) error {
	a.calls++
	a.asked = append(a.asked, string(action)+"/"+string(id))
	return a.fail
}

type quotesFake struct {
	tx       *txFake
	fail     error
	calls    int
	insideTx bool
	terms    com.CommercialTerms
}

func (q *quotesFake) AcceptedQuote(context.Context, com.TenantID, string) (com.AcceptedQuote, error) {
	q.calls++
	q.insideTx = q.insideTx || q.tx.depth > 0
	if q.fail != nil {
		return com.AcceptedQuote{}, q.fail
	}
	return com.AcceptedQuote{QuoteID: "Q-1", QuoteRevision: 3, LegalEntityID: "LE", CustomerPartyID: "P", CustomerRevision: 2,
		Total: kernel.Money{Currency: "USD", Minor: 100_000}, Validity: kernel.ValidityInterval{From: now.Add(-time.Hour), Until: now.Add(14 * 24 * time.Hour)},
		RecipientRole: com.RoleCustomer, AcceptedAt: now.Add(-30 * time.Minute), EvidenceRef: "signed-quote", Terms: q.terms,
		TermsContract: com.QuoteSnapshotV2, TermsHash: strings.Repeat("cd", 32)}, nil
}

type customersFake struct {
	tx       *txFake
	fail     error
	calls    int
	insideTx bool
	roles    []string
}

func (c *customersFake) VerifyOrderingParty(_ context.Context, _ com.TenantID, party, role string, _ com.LegalEntityID, _ time.Time) (com.CustomerSnapshot, error) {
	c.calls++
	c.insideTx = c.insideTx || c.tx.depth > 0
	c.roles = append(c.roles, role)
	if c.fail != nil {
		return com.CustomerSnapshot{}, c.fail
	}
	return com.CustomerSnapshot{PartyID: party, PartyRevision: 2, LegalName: "Fixture Corp", CountryCode: "US", RoleCode: role,
		RoleValidity: kernel.ValidityInterval{From: now.Add(-time.Hour)}, RoleEvidenceRef: "customer-contract"}, nil
}

type policiesFake struct {
	versions map[com.LegalEntityID][]com.PolicySetting
	inForce  int
}

func (p *policiesFake) InForce(_ context.Context, _ com.TenantID, entity com.LegalEntityID) (com.OrderPolicy, error) {
	p.inForce++
	for _, scope := range []com.LegalEntityID{entity, ""} {
		if v := p.versions[scope]; len(v) > 0 {
			return v[len(v)-1].OrderPolicy, nil
		}
	}
	return com.BuiltInOrderPolicy(), nil
}

func (p *policiesFake) Latest(_ context.Context, _ com.TenantID, entity com.LegalEntityID) (*com.PolicySetting, error) {
	if v := p.versions[entity]; len(v) > 0 {
		latest := v[len(v)-1]
		return &latest, nil
	}
	return nil, nil
}

func (p *policiesFake) Insert(_ context.Context, s com.PolicySetting) error {
	p.versions[s.LegalEntityID] = append(p.versions[s.LegalEntityID], s)
	return nil
}

type serviceLevelsFake struct {
	tx       *txFake
	asked    []string
	insideTx bool
}

func (s *serviceLevelsFake) Listed(_ context.Context, _ com.TenantID, code string) error {
	s.asked = append(s.asked, code)
	s.insideTx = s.insideTx || s.tx.depth > 0
	if code == "STANDARD" || code == "EXPRESS" {
		return nil
	}
	return ErrServiceLevelNotListed
}

type clockFake struct{ at time.Time }

func (c *clockFake) Now() time.Time { return c.at }

type fixture struct {
	deps          Deps
	store         *storeFake
	outbox        *outboxFake
	auth          *authFake
	quotes        *quotesFake
	customers     *customersFake
	policies      *policiesFake
	serviceLevels *serviceLevelsFake
	clock         *clockFake
}

// newFixture records commands eight hours after now, so the business times the tests state lie within the
// record window before it (T1).
func newFixture() *fixture {
	tx := &txFake{}
	f := &fixture{store: &storeFake{orders: map[com.OrderID]com.OrderState{}, revs: map[com.OrderID]int64{}},
		outbox: &outboxFake{}, auth: &authFake{}, quotes: &quotesFake{tx: tx}, customers: &customersFake{tx: tx},
		policies: &policiesFake{versions: map[com.LegalEntityID][]com.PolicySetting{}}, serviceLevels: &serviceLevelsFake{tx: tx},
		clock: &clockFake{at: now.Add(8 * time.Hour)}}
	f.deps = Deps{Tx: tx, Orders: f.store, Receipts: receiptsFake{}, Outbox: f.outbox, Auth: f.auth, Clock: f.clock, Window: kernel.DefaultRecordWindow}
	return f
}

// packs returns n pallets weighing w kilograms.
func packs(n, w int64) com.Measure {
	return com.Measure{Packs: n, GrossWeight: kernel.Quantity{Unit: "KGM", Amount: w}}
}

func recordCommand(id string) RecordCustomerOrderCommand {
	return RecordCustomerOrderCommand{CommandID: id, Scope: Scope{TenantID: "T", ActorID: "sales-1"}, OrderID: "O-1", QuoteID: "Q-1",
		CustomerPartyID: "P", Lines: []com.OrderLine{{Description: "Pallets of machine parts", PackageType: "PX", Measure: packs(10, 500)}},
		OccurredAt: now}
}

func (f *fixture) recorder() RecordCustomerOrder {
	return RecordCustomerOrder{Deps: f.deps, Quotes: f.quotes, Customers: f.customers, Policies: f.policies, ServiceLevels: f.serviceLevels}
}

func (f *fixture) allocate(command string, revision int64, obligation com.ObligationID, m com.Measure) (Receipt, error) {
	return AllocateOrderToObligation{Deps: f.deps}.Handle(context.Background(), AllocateOrderToObligationCommand{
		CommandID: command, Scope: Scope{TenantID: "T", ActorID: "planner-1"}, OrderID: "O-1", ExpectedRevision: revision,
		LineNo: 1, ObligationID: obligation, Measure: m, OccurredAt: now.Add(time.Hour)})
}

func (f *fixture) reserve(command string, obligation com.ObligationID, shipment string) (Receipt, error) {
	return ReserveObligationForFulfillment{Deps: f.deps}.Handle(context.Background(), ReserveObligationForFulfillmentCommand{
		CommandID: command, Scope: Scope{TenantID: "T", ActorID: "fulfilment"}, ObligationID: obligation, ObligationVersion: 1,
		ShipmentID: shipment, OccurredAt: now.Add(2 * time.Hour)})
}

func (f *fixture) release(command string, obligation com.ObligationID, shipment string) (Receipt, error) {
	return ReleaseObligationReservation{Deps: f.deps}.Handle(context.Background(), ReleaseObligationReservationCommand{
		CommandID: command, Scope: Scope{TenantID: "T", ActorID: "fulfilment"}, ObligationID: obligation, ObligationVersion: 1,
		ShipmentID: shipment, OccurredAt: now.Add(3 * time.Hour)})
}

func (f *fixture) revoke(command string, revision int64, obligation com.ObligationID) (Receipt, error) {
	return RevokeObligation{Deps: f.deps}.Handle(context.Background(), RevokeObligationCommand{
		CommandID: command, Scope: Scope{TenantID: "T", ActorID: "planner-1"}, OrderID: "O-1", ExpectedRevision: revision,
		ObligationID: obligation, OccurredAt: now.Add(4 * time.Hour)})
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
	changed.Lines[0].Measure = packs(10, 600)
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
	if _, err := f.allocate("allocate-0", 2, "OB-1", packs(4, 200)); !errors.Is(err, com.ErrConflict) {
		t.Fatalf("stale revision: err = %v", err)
	}
	r, err := f.allocate("allocate-1", 1, "OB-1", packs(4, 200))
	if err != nil || r.NewRevision != 2 || f.store.orders["O-1"].Status != com.OrderPartiallyAllocated {
		t.Fatalf("allocate: %+v, %v", r, err)
	}
	if again, err := f.allocate("allocate-1", 1, "OB-1", packs(4, 200)); err != nil || again != r {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if _, err := f.allocate("allocate-2", 2, "OB-2", packs(7, 200)); !errors.Is(err, com.ErrOverAllocation) || f.store.revs["O-1"] != 2 {
		t.Fatalf("over-allocation: err = %v, revision %d", err, f.store.revs["O-1"])
	}
	if r, err = f.allocate("allocate-3", 2, "OB-2", packs(6, 300)); err != nil || r.NewRevision != 3 || f.store.orders["O-1"].Status != com.OrderAllocated {
		t.Fatalf("allocate the rest: %+v, %v", r, err)
	}
	if len(f.store.saved) != 2 || f.store.saved[0].ObligationID != "OB-1" || f.store.saved[1].ObligationID != "OB-2" {
		t.Fatalf("saved obligations %+v", f.store.saved)
	}
	if got := fmt.Sprint(f.outbox.events); got != "[CustomerOrderRecorded.v1 FulfillmentObligationAllocated.v1/OB-1 FulfillmentObligationAllocated.v1/OB-2]" {
		t.Fatalf("events %s", got)
	}
}

func TestRevokeAmendAndCancelCheckTheRevision(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	if _, err := f.recorder().Handle(ctx, recordCommand("record-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.allocate("allocate-1", 1, "OB-1", packs(10, 500)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.revoke("revoke-0", 1, "OB-1"); !errors.Is(err, com.ErrConflict) {
		t.Fatalf("stale revision: err = %v", err)
	}
	r, err := f.revoke("revoke-1", 2, "OB-1")
	if err != nil || r.NewRevision != 3 || f.store.orders["O-1"].Status != com.OrderRecorded {
		t.Fatalf("revoke: %+v, %v", r, err)
	}
	if again, err := f.revoke("revoke-1", 2, "OB-1"); err != nil || again != r {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if _, err := f.allocate("allocate-2", 3, "OB-2", packs(10, 500)); err != nil {
		t.Fatal(err)
	}
	amend := AmendObligationCommand{CommandID: "amend-1", Scope: Scope{TenantID: "T", ActorID: "planner-1"}, OrderID: "O-1",
		ExpectedRevision: 4, ObligationID: "OB-2", NewObligationID: "OB-3", Measure: packs(8, 400), OccurredAt: now.Add(5 * time.Hour)}
	if r, err = (AmendObligation{Deps: f.deps}).Handle(ctx, amend); err != nil || r.NewRevision != 5 {
		t.Fatalf("amend: %+v, %v", r, err)
	}
	cancel := CancelCustomerOrderCommand{CommandID: "cancel-1", Scope: Scope{TenantID: "T", ActorID: "sales-1"}, OrderID: "O-1",
		ExpectedRevision: 4, OccurredAt: now.Add(6 * time.Hour)}
	if _, err = (CancelCustomerOrder{Deps: f.deps}).Handle(ctx, cancel); !errors.Is(err, com.ErrConflict) {
		t.Fatalf("cancel at a stale revision: err = %v", err)
	}
	cancel.CommandID, cancel.ExpectedRevision = "cancel-2", 5
	if r, err = (CancelCustomerOrder{Deps: f.deps}).Handle(ctx, cancel); err != nil || r.NewRevision != 6 || f.store.orders["O-1"].Status != com.OrderCancelled {
		t.Fatalf("cancel: %+v, %v", r, err)
	}
	want := "[CustomerOrderRecorded.v1 FulfillmentObligationAllocated.v1/OB-1 FulfillmentObligationRevoked.v1/OB-1 " +
		"FulfillmentObligationAllocated.v1/OB-2 FulfillmentObligationRevoked.v1/OB-2 FulfillmentObligationAllocated.v1/OB-3 " +
		"FulfillmentObligationRevoked.v1/OB-3 CustomerOrderCancelled.v1]"
	if got := fmt.Sprint(f.outbox.events); got != want {
		t.Fatalf("events %s", got)
	}
	if got := fmt.Sprint(f.auth.asked); got != "[RecordCustomerOrder/O-1 AllocateOrderToObligation/O-1 RevokeObligation/O-1 "+
		"RevokeObligation/O-1 AllocateOrderToObligation/O-1 AmendObligation/O-1 CancelCustomerOrder/O-1 CancelCustomerOrder/O-1]" {
		t.Fatalf("authorized %s", got)
	}
}

func TestReservationsAreAddressedToTheObligation(t *testing.T) {
	f := newFixture()
	if _, err := f.recorder().Handle(context.Background(), recordCommand("record-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.allocate("allocate-1", 1, "OB-1", packs(10, 500)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reserve("reserve-0", "OB-9", "S-1"); !errors.Is(err, errNoOrder) || f.auth.calls != 2 {
		t.Fatalf("unknown obligation: err = %v, authorizer calls %d", err, f.auth.calls)
	}
	r, err := f.reserve("reserve-1", "OB-1", "S-1")
	if err != nil || r.OrderID != "O-1" || r.NewRevision != 3 {
		t.Fatalf("reserve: %+v, %v", r, err)
	}
	if again, err := f.reserve("reserve-1", "OB-1", "S-1"); err != nil || again != r {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if _, err := f.reserve("reserve-2", "OB-1", "S-1"); !errors.Is(err, com.ErrObligationReserved) {
		t.Fatalf("the same shipment again: err = %v", err)
	}
	if _, err := f.revoke("revoke-1", 3, "OB-1"); !errors.Is(err, com.ErrObligationReserved) {
		t.Fatalf("revoke while reserved: err = %v", err)
	}
	if _, err := f.release("release-0", "OB-1", "S-2"); !errors.Is(err, com.ErrNotReserved) {
		t.Fatalf("release by another shipment: err = %v", err)
	}
	if r, err = f.release("release-1", "OB-1", "S-1"); err != nil || r.NewRevision != 4 {
		t.Fatalf("release: %+v, %v", r, err)
	}
	if r, err = f.revoke("revoke-2", 4, "OB-1"); err != nil || r.NewRevision != 5 {
		t.Fatalf("revoke after the release: %+v, %v", r, err)
	}
	if len(f.store.reserved) != 1 || f.store.reserved[0].ShipmentID != "S-1" || !f.store.reserved[0].Held() ||
		len(f.store.released) != 1 || f.store.released[0].No != 1 || f.store.released[0].Held() {
		t.Fatalf("stored reservations %+v, releases %+v", f.store.reserved, f.store.released)
	}
	if got := fmt.Sprint(f.auth.asked[2:]); got != "[ReserveObligationForFulfillment/O-1 ReserveObligationForFulfillment/O-1 "+
		"RevokeObligation/O-1 ReleaseObligationReservation/O-1 ReleaseObligationReservation/O-1 RevokeObligation/O-1]" {
		t.Fatalf("authorized %s", got)
	}
	if got := fmt.Sprint(f.outbox.events[2:]); got != "[ObligationReserved.v1/OB-1 ObligationReservationReleased.v1/OB-1 FulfillmentObligationRevoked.v1/OB-1]" {
		t.Fatalf("events %s", got)
	}
}
