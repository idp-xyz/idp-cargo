package domain

import (
	"errors"
	"testing"
	"time"
)

var at = time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)

func newActive(t *testing.T) *BusinessParty {
	t.Helper()
	p, e := RegisterParty("TENANT-1", "PARTY-1", "A Logistics", "CN", at)
	if e != nil {
		t.Fatal(e)
	}
	if e = p.Activate("proof-1", at); e != nil {
		t.Fatal(e)
	}
	return p
}
func TestPartyLifecycle(t *testing.T) {
	p := newActive(t)
	if p.Status() != PartyActive {
		t.Fatal("not active")
	}
	if e := p.Suspend("risk", at); e != nil {
		t.Fatal(e)
	}
	if e := p.Archive("archival", at); e != nil {
		t.Fatal(e)
	}
	if e := p.CorrectLegalName("X", "evidence", at); !errors.Is(e, ErrInvalidTransition) {
		t.Fatal(e)
	}
}
func TestRoleOverlapAndTouchingBoundaries(t *testing.T) {
	p := newActive(t)
	r := Role{Code: RoleCarrier, LegalEntityID: "LE-1", ValidFrom: at, ValidUntil: at.Add(24 * time.Hour), EvidenceRef: "source", ApprovedBy: "checker"}
	if e := p.ApproveRole(r, at); e != nil {
		t.Fatal(e)
	}
	if e := p.ApproveRole(r, at); !errors.Is(e, ErrRoleOverlap) {
		t.Fatalf("expected overlap: %v", e)
	}
	next := r
	next.ValidFrom = at.Add(24 * time.Hour)
	next.ValidUntil = time.Time{}
	if e := p.ApproveRole(next, at); e != nil {
		t.Fatal(e)
	}
	sameKindOtherEntity := r
	sameKindOtherEntity.LegalEntityID = "LE-2"
	if e := p.ApproveRole(sameKindOtherEntity, at); e != nil {
		t.Fatal(e)
	}
}
func TestSnapshotIsNotLiveMaster(t *testing.T) {
	p := newActive(t)
	s, e := p.Snapshot(5, at, "verified-copy")
	if e != nil {
		t.Fatal(e)
	}
	if e = p.CorrectLegalName("A Logistics (Renamed)", "registry-proof", at); e != nil {
		t.Fatal(e)
	}
	if s.LegalName != "A Logistics" || s.Ref.SourceRevision != 5 {
		t.Fatalf("historical snapshot changed: %+v", s)
	}
}
func TestRestoreNoEvent(t *testing.T) {
	p, e := RestoreParty("T", "P", "Legal", "US", PartyActive, nil)
	if e != nil || len(p.Pending()) != 0 {
		t.Fatalf("restore should be silent: %v", e)
	}
}
func TestEventsNotPIIAndAcknowledge(t *testing.T) {
	p := newActive(t)
	if len(p.Pending()) != 2 {
		t.Fatal("expected two events")
	}
	if p.Pending()[0].SubjectID != "PARTY-1" {
		t.Fatal("wrong subject")
	}
	p.Acknowledge()
	if len(p.Pending()) != 0 {
		t.Fatal("not cleared")
	}
}
func TestRoleLifecycleGuards(t *testing.T) {
	p := newActive(t)
	r := Role{Code: RoleCustomer, LegalEntityID: "LE-1", ValidFrom: at, EvidenceRef: "e", ApprovedBy: "a"}
	if e := p.ApproveRole(r, at); e != nil {
		t.Fatal(e)
	}
	if e := p.Suspend("risk", at); e != nil {
		t.Fatal(e)
	}
	if e := p.Archive("close", at); !errors.Is(e, ErrInvalidTransition) {
		t.Fatalf("archiving active role must fail: %v", e)
	}
}
func TestLegalEntityAndBranch(t *testing.T) {
	e, err := RegisterLegalEntity("T", "LE", "Cargo Ltd", "CN", "registry", at)
	if err != nil {
		t.Fatal(err)
	}
	b := Branch{ID: "B1", Name: "Beijing", EvidenceRef: "approval", ApprovedAt: at}
	if err = e.ApproveBranch(b); err == nil {
		t.Fatal("draft entity cannot approve branches")
	}
	if err = e.Verify("officer", at); err != nil {
		t.Fatal(err)
	}
	if err = e.ApproveBranch(b); err != nil {
		t.Fatal(err)
	}
	if err = e.ApproveBranch(b); err == nil {
		t.Fatal("duplicate branch")
	}
}
func TestMergeGuard(t *testing.T) {
	m, e := OpenMergeCase("T", "CASE-1", "SRC", "DST", "proposer", "evidence", at)
	if e != nil {
		t.Fatal(e)
	}
	if e = m.ApplyAlias(1, 1, at); !errors.Is(e, ErrUnapprovedMerge) {
		t.Fatal(e)
	}
	if e = m.Assess("impact-review", newPartyForMerge(t, "SRC"), newPartyForMerge(t, "DST"), 3, 4); e != nil {
		t.Fatal(e)
	}
	if e = m.Approve("proposer", at); !errors.Is(e, ErrSelfApproval) {
		t.Fatal(e)
	}
	if e = m.Approve("independent", at); e != nil {
		t.Fatal(e)
	}
	if e = m.ApplyAlias(2, 4, at); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if e = m.ApplyAlias(3, 4, at); e != nil {
		t.Fatal(e)
	}
	if e = m.ApplyAlias(3, 4, at); !errors.Is(e, ErrUnapprovedMerge) {
		t.Fatal(e)
	}
}
func TestMergeReject(t *testing.T) {
	m, e := OpenMergeCase("T", "CASE", "S", "T", "proposer", "e", at)
	if e != nil {
		t.Fatal(e)
	}
	if e = m.Reject("reviewer", "not same legal party", at); e != nil {
		t.Fatal(e)
	}
	if e = m.Approve("reviewer", at); e == nil {
		t.Fatal("cannot approve rejected")
	}
}
func TestInvalidIdentity(t *testing.T) {
	if _, err := RegisterParty("", "P", "N", "US", at); err == nil {
		t.Fatal("tenant required")
	}
	if _, err := RegisterParty("T", "P", "N", "USA", at); err == nil {
		t.Fatal("ISO2 required")
	}
	if _, err := OpenMergeCase("T", "CASE", "X", "X", "proposer", "e", at); err == nil {
		t.Fatal("self merge must fail")
	}
}

func newPartyForMerge(t *testing.T, id PartyID) *BusinessParty {
	t.Helper()
	p, e := RegisterParty("T", id, "Legal Ltd", "US", at)
	if e != nil {
		t.Fatal(e)
	}
	if e = p.Activate("evidence", at); e != nil {
		t.Fatal(e)
	}
	return p
}
func TestMergeCrossTenantGuards(t *testing.T) {
	m, e := OpenMergeCase("T", "CASE", "SRC", "DST", "proposer", "e", at)
	if e != nil {
		t.Fatal(e)
	}
	src := newPartyForMerge(t, "SRC")
	other, e := RegisterParty("OTHER", "DST", "Other Ltd", "US", at)
	if e != nil {
		t.Fatal(e)
	}
	_ = other.Activate("proof", at)
	if e = m.Assess("impact", src, other, 1, 1); !errors.Is(e, ErrCrossTenant) {
		t.Fatalf("cross tenant: %v", e)
	}
}
func TestCannotMutateRolesThroughAccessor(t *testing.T) {
	p := newActive(t)
	r := Role{Code: RoleCarrier, LegalEntityID: "LE", ValidFrom: at, EvidenceRef: "e", ApprovedBy: "a"}
	if err := p.ApproveRole(r, at); err != nil {
		t.Fatal(err)
	}
	roles := p.ApprovedRoles()
	roles[0].Code = RoleCustomer
	if p.ApprovedRoles()[0].Code != RoleCarrier {
		t.Fatal("roles leaked mutable slice")
	}
}
