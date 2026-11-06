package application

import (
	"context"
	"errors"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/domain"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)

type txFake struct{ calls int }

func (x *txFake) WithinTransaction(ctx context.Context, fn func(context.Context) error) error {
	x.calls++
	return fn(ctx)
}

type partFake struct {
	p            *org.BusinessParty
	revision     int64
	loads, saves int
}

func (f *partFake) Load(_ context.Context, t org.TenantID, id org.PartyID) (*org.BusinessParty, int64, error) {
	f.loads++
	return f.p, f.revision, nil
}
func (f *partFake) Save(_ context.Context, p *org.BusinessParty, rev int64) (int64, error) {
	f.saves++
	if rev != f.revision {
		return 0, org.ErrConflict
	}
	f.revision++
	return f.revision, nil
}

type entityFake struct {
	fail  error
	calls int
}

func (f *entityFake) RequireVerifiedLegalEntity(context.Context, org.TenantID, org.LegalEntityID) error {
	f.calls++
	return f.fail
}

type authFake struct {
	fail  error
	calls int
}

func (f *authFake) RequireRoleApproval(context.Context, Scope, org.PartyID, org.RoleCode) error {
	f.calls++
	return f.fail
}

type receiptFake struct {
	have   bool
	result Receipt
	saves  int
}

func (f *receiptFake) Find(context.Context, org.TenantID, string) (Receipt, bool, error) {
	return f.result, f.have, nil
}
func (f *receiptFake) Save(_ context.Context, _ org.TenantID, r Receipt) error {
	f.have = true
	f.result = r
	f.saves++
	return nil
}

type outboxFake struct {
	calls int
	fail  error
}

func (f *outboxFake) EnqueueORGEvent(context.Context, org.Event) error { f.calls++; return f.fail }
func makeHandler(t *testing.T) (ApproveRole, *partFake, *receiptFake, *outboxFake, *authFake, *entityFake) {
	t.Helper()
	p, e := org.RegisterParty("T", "P", "Legal", "US", now)
	if e != nil {
		t.Fatal(e)
	}
	if e = p.Activate("registry", now); e != nil {
		t.Fatal(e)
	}
	p.Acknowledge()
	pa := &partFake{p: p, revision: 1}
	re := &receiptFake{}
	out := &outboxFake{}
	auth := &authFake{}
	entity := &entityFake{}
	return ApproveRole{Tx: &txFake{}, Parties: pa, Receipts: re, Outbox: out, Auth: auth, Entities: entity}, pa, re, out, auth, entity
}
func command() RoleApprovalCommand {
	return RoleApprovalCommand{CommandID: "C-1", Scope: Scope{TenantID: "T", ActorID: "a", LegalEntityID: "LE"}, PartyID: "P", ExpectedRevision: 1, Role: org.Role{Code: org.RoleCustomer, LegalEntityID: "LE", ValidFrom: now, ApprovedBy: "a", EvidenceRef: "e"}, OccurredAt: now}
}
func TestApproveRoleHappyAndDuplicate(t *testing.T) {
	h, party, receipts, out, _, _ := makeHandler(t)
	c := command()
	r, e := h.Handle(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	if r.NewRevision != 2 || party.saves != 1 || out.calls != 1 || receipts.saves != 1 {
		t.Fatalf("bad side effects: %+v", r)
	}
	r2, e := h.Handle(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	if r2.NewRevision != 2 || party.saves != 1 || out.calls != 1 {
		t.Fatal("duplicate repeated side effects")
	}
}
func TestDuplicateChangedPayloadDenied(t *testing.T) {
	h, party, _, _, _, _ := makeHandler(t)
	c := command()
	_, _ = h.Handle(context.Background(), c)
	c.Role.EvidenceRef = "different"
	if _, err := h.Handle(context.Background(), c); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatal(err)
	}
	if party.saves != 1 {
		t.Fatal("unexpected mutation")
	}
}
func TestUnauthorizedNoDBRead(t *testing.T) {
	h, party, _, out, auth, _ := makeHandler(t)
	auth.fail = errors.New("policy deny")
	if _, err := h.Handle(context.Background(), command()); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if party.loads != 0 || out.calls != 0 {
		t.Fatal("unauthorized action accessed party")
	}
}
func TestEntityNotVerified(t *testing.T) {
	h, party, _, out, _, entity := makeHandler(t)
	entity.fail = errors.New("entity not verified")
	if _, err := h.Handle(context.Background(), command()); err == nil {
		t.Fatal("expected deny")
	}
	if party.loads != 0 || out.calls != 0 {
		t.Fatal("ineligible entity caused side effect")
	}
}
func TestOptimisticConflict(t *testing.T) {
	h, party, _, out, _, _ := makeHandler(t)
	party.revision = 3
	if _, err := h.Handle(context.Background(), command()); !errors.Is(err, org.ErrConflict) {
		t.Fatal(err)
	}
	if party.saves != 0 || out.calls != 0 {
		t.Fatal("conflict caused side effect")
	}
}
func TestCrossTenantReturnedFromRepo(t *testing.T) {
	h, party, _, _, _, _ := makeHandler(t)
	p, e := org.RegisterParty("OTHER", "P", "Legal", "US", now)
	if e != nil {
		t.Fatal(e)
	}
	party.p = p
	if _, err := h.Handle(context.Background(), command()); !errors.Is(err, org.ErrCrossTenant) {
		t.Fatal(err)
	}
}
func TestOutboxFailureDoesNotCommitReceipt(t *testing.T) {
	h, _, receipts, out, _, _ := makeHandler(t)
	out.fail = errors.New("outbox not available")
	if _, err := h.Handle(context.Background(), command()); err == nil {
		t.Fatal("expect failure")
	}
	if receipts.saves != 0 {
		t.Fatal("receipt incorrectly committed")
	}
	// A real PG Transactor must rollback party write, which this fake does NOT simulate.
}
func TestInvalidScopeFailsClosed(t *testing.T) {
	h, party, _, _, _, _ := makeHandler(t)
	c := command()
	c.Scope.TenantID = ""
	if _, err := h.Handle(context.Background(), c); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal(err)
	}
	if party.loads != 0 {
		t.Fatal("accessed persistence with invalid scope")
	}
}

func TestSpoofedApproverCannotBeAccepted(t *testing.T) {
	h, party, _, out, _, _ := makeHandler(t)
	c := command()
	c.Role.ApprovedBy = "other-user"
	_, err := h.Handle(context.Background(), c)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("identity spoof must fail: %v", err)
	}
	if party.loads != 0 || out.calls != 0 {
		t.Fatal("spoofed request touched persistent domain state")
	}
}
func TestEmptyApproverIsBoundToAuthenticatedActor(t *testing.T) {
	h, party, _, _, _, _ := makeHandler(t)
	c := command()
	c.Role.ApprovedBy = ""
	_, err := h.Handle(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	roles := party.p.ApprovedRoles()
	if len(roles) != 1 || roles[0].ApprovedBy != "a" {
		t.Fatal("trusted approver was not bound")
	}
}
