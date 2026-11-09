package application

import (
	"context"
	"errors"
	"testing"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func (f *fixture) policy(command string, entity com.LegalEntityID, expected int64, max, grace int) (Receipt, error) {
	return SetOrderPolicy{Deps: f.deps, Policies: f.policies}.Handle(context.Background(), SetOrderPolicyCommand{
		CommandID: command, Scope: Scope{TenantID: "T", ActorID: "commercial-lead"}, LegalEntityID: entity,
		ExpectedVersion: expected, MaxOrdersPerQuote: max, GraceDays: grace})
}

func TestRecordOrderFollowsThePolicyInForceForTheLegalEntity(t *testing.T) {
	f := newFixture()
	if _, err := f.recorder().Handle(context.Background(), recordCommand("record-1")); err != nil {
		t.Fatal(err)
	}
	second := recordCommand("record-2")
	second.OrderID = "O-2"
	if _, err := f.recorder().Handle(context.Background(), second); !errors.Is(err, com.ErrQuoteFullyOrdered) {
		t.Fatalf("a second order under the built-in policy: err = %v", err)
	}
	if _, err := f.policy("policy-tenant", "", 0, 5, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.policy("policy-LE", "LE", 0, 2, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.recorder().Handle(context.Background(), second); err != nil {
		t.Fatalf("a second order under the legal entity's policy of two: %v", err)
	}
	if p := f.store.orders["O-2"].Policy; p.Scope != com.PolicyLegalEntity || p.Version != 1 || p.MaxOrdersPerQuote != 2 {
		t.Fatalf("the order froze policy %+v", p)
	}
	third := recordCommand("record-3")
	third.OrderID = "O-3"
	if _, err := f.recorder().Handle(context.Background(), third); !errors.Is(err, com.ErrQuoteFullyOrdered) {
		t.Fatalf("a third order: err = %v", err)
	}
	// A cancelled order no longer counts (C1).
	if _, err := (CancelCustomerOrder{Deps: f.deps}).Handle(context.Background(), CancelCustomerOrderCommand{CommandID: "cancel-1",
		Scope: Scope{TenantID: "T", ActorID: "sales-1"}, OrderID: "O-1", ExpectedRevision: 1, OccurredAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.recorder().Handle(context.Background(), third); err != nil {
		t.Fatalf("a third order once the first is cancelled: %v", err)
	}
	if got := f.customers.roles; len(got) == 0 || got[0] != com.RoleCustomer {
		t.Fatalf("ORG was asked for roles %v", got)
	}
}

func TestRecordOrderAsksRATEOnlyForAServiceLevelCompletedOnTheOrder(t *testing.T) {
	f := newFixture()
	c := recordCommand("record-1")
	c.Terms = com.CommercialTerms{ServiceLevel: "PRIORITY"}
	if _, err := f.recorder().Handle(context.Background(), c); !errors.Is(err, ErrServiceLevelNotListed) {
		t.Fatalf("an unlisted service level: err = %v", err)
	}
	if len(f.store.orders) != 0 {
		t.Fatal("an order was written")
	}
	c.CommandID, c.Terms = "record-2", com.CommercialTerms{ServiceLevel: "EXPRESS", IncotermsRule: "FCA", IncotermsVersion: "2020", NamedPlace: "Pudong airport"}
	if _, err := f.recorder().Handle(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if terms := f.store.orders["O-1"].Terms; terms.ServiceLevelSource != com.TermFromOrder || terms.IncotermsSource != com.TermFromOrder {
		t.Fatalf("terms %+v", terms)
	}
	if f.serviceLevels.insideTx || len(f.serviceLevels.asked) != 2 {
		t.Fatalf("RATE was asked %v, inside a transaction %v", f.serviceLevels.asked, f.serviceLevels.insideTx)
	}

	g := newFixture()
	g.quotes.terms = com.CommercialTerms{ServiceLevel: "STANDARD"}
	c = recordCommand("record-1")
	c.Terms = com.CommercialTerms{ServiceLevel: "EXPRESS"}
	if _, err := g.recorder().Handle(context.Background(), c); !errors.Is(err, com.ErrTermsMismatch) {
		t.Fatalf("another service level than the quote's: err = %v", err)
	}
	c.Terms = com.CommercialTerms{}
	if _, err := g.recorder().Handle(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if len(g.serviceLevels.asked) != 0 {
		t.Fatalf("RATE was asked %v about the quote's own service level", g.serviceLevels.asked)
	}
}

func TestCommandsOutsideTheRecordWindowAreRefusedButReplaysAnswer(t *testing.T) {
	f := newFixture()
	late := recordCommand("record-late")
	late.OccurredAt = f.clock.at.Add(-8 * 24 * time.Hour)
	if _, err := f.recorder().Handle(context.Background(), late); !errors.Is(err, kernel.ErrOutsideRecordWindow) {
		t.Fatalf("eight days back: err = %v", err)
	}
	ahead := recordCommand("record-ahead")
	ahead.OccurredAt = f.clock.at.Add(10 * time.Minute)
	if _, err := f.recorder().Handle(context.Background(), ahead); !errors.Is(err, kernel.ErrOutsideRecordWindow) {
		t.Fatalf("ten minutes ahead: err = %v", err)
	}
	r, err := f.recorder().Handle(context.Background(), recordCommand("record-1"))
	if err != nil {
		t.Fatal(err)
	}
	if entered := f.store.orders["O-1"].EnteredAt; !entered.Equal(f.clock.at) {
		t.Fatalf("entered at %s, want %s", entered, f.clock.at)
	}
	f.clock.at = f.clock.at.Add(30 * 24 * time.Hour)
	again, err := f.recorder().Handle(context.Background(), recordCommand("record-1"))
	if err != nil || again != r {
		t.Fatalf("replay a month later: %+v, %v", again, err)
	}
}

func TestAmendAndAddOrderLinesCheckTheRevision(t *testing.T) {
	f := newFixture()
	if _, err := f.recorder().Handle(context.Background(), recordCommand("record-1")); err != nil {
		t.Fatal(err)
	}
	amend := AmendOrderLineCommand{CommandID: "amend-line-1", Scope: Scope{TenantID: "T", ActorID: "cs-1"}, OrderID: "O-1", ExpectedRevision: 1,
		LineNo: 1, Description: "Pallets of machine parts", PackageType: "PX", Measure: packs(10, 520), Reason: "customer corrected the weight",
		OccurredAt: now.Add(time.Hour)}
	r, err := AmendOrderLine{Deps: f.deps}.Handle(context.Background(), amend)
	if err != nil || r.NewRevision != 2 {
		t.Fatalf("amend: %+v, %v", r, err)
	}
	if again, err := (AmendOrderLine{Deps: f.deps}).Handle(context.Background(), amend); err != nil || again != r {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	add := AddOrderLineCommand{CommandID: "add-line-1", Scope: Scope{TenantID: "T", ActorID: "cs-1"}, OrderID: "O-1", ExpectedRevision: 1,
		Description: "Cartons of manuals", PackageType: "CT", Measure: packs(2, 15), Reason: "customer added two cartons", OccurredAt: now.Add(time.Hour)}
	if _, err := (AddOrderLine{Deps: f.deps}).Handle(context.Background(), add); !errors.Is(err, com.ErrConflict) {
		t.Fatalf("add at a stale revision: err = %v", err)
	}
	add.ExpectedRevision = 2
	if _, err := (AddOrderLine{Deps: f.deps}).Handle(context.Background(), add); err != nil {
		t.Fatal(err)
	}
	s := f.store.orders["O-1"]
	if len(s.LineChanges) != 2 || !s.LineChanges[0].EnteredAt.Equal(f.clock.at) || s.LineChanges[1].LineNo != 2 {
		t.Fatalf("line changes %+v", s.LineChanges)
	}
	if got := f.auth.asked[len(f.auth.asked)-1]; got != "AddOrderLine/O-1" {
		t.Fatalf("authorized %s", got)
	}
	if got := f.outbox.events[len(f.outbox.events)-1]; got != "CustomerOrderLineAdded.v1" {
		t.Fatalf("last event %s", got)
	}
}

func TestSetOrderPolicyRecordsVersionsAndReplays(t *testing.T) {
	f := newFixture()
	r, err := f.policy("policy-1", "LE", 0, 3, 2)
	if err != nil || r.ResultType != ResultOrderPolicy || r.PolicyKey != "LE" || r.NewRevision != 1 {
		t.Fatalf("first version: %+v, %v", r, err)
	}
	if again, err := f.policy("policy-1", "LE", 0, 3, 2); err != nil || again != r {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	if _, err := f.policy("policy-1", "LE", 0, 4, 2); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("command id reused: err = %v", err)
	}
	if _, err := f.policy("policy-2", "LE", 0, 4, 2); !errors.Is(err, com.ErrPolicyVersionStale) {
		t.Fatalf("a stale version: err = %v", err)
	}
	if _, err := f.policy("policy-3", "LE", 1, 4, 31); !errors.Is(err, com.ErrInvalidPolicy) {
		t.Fatalf("31 days of grace: err = %v", err)
	}
	r, err = f.policy("policy-4", "", 0, 1, 0)
	if err != nil || r.PolicyKey != TenantPolicyKey {
		t.Fatalf("tenant default: %+v, %v", r, err)
	}
	if got := f.auth.asked[len(f.auth.asked)-1]; got != "SetOrderPolicy/" {
		t.Fatalf("authorized %s", got)
	}
	f.auth.fail = errors.New("not a commercial lead")
	if _, err := f.policy("policy-5", "LE", 1, 1, 0); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthorized: err = %v", err)
	}
	if n := len(f.policies.versions["LE"]); n != 1 {
		t.Fatalf("%d versions of LE", n)
	}
}
