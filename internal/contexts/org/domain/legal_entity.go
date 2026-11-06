package domain

import (
	"strings"
	"time"
)

type LegalEntityStatus string

const (
	EntityDraft     LegalEntityStatus = "DRAFT"
	EntityVerified  LegalEntityStatus = "VERIFIED"
	EntitySuspended LegalEntityStatus = "SUSPENDED"
)

type Branch struct {
	ID          string
	Name        string
	EvidenceRef string
	ApprovedAt  time.Time
}

type LegalEntity struct {
	tenantID                TenantID
	id                      LegalEntityID
	registeredName          string
	jurisdiction            string
	registrationEvidenceRef string
	status                  LegalEntityStatus
	branches                []Branch
	pending                 []Event
}

func RegisterLegalEntity(t TenantID, id LegalEntityID, name, jurisdiction, evidence string, at time.Time) (*LegalEntity, error) {
	if t == "" || id == "" || strings.TrimSpace(name) == "" || len(jurisdiction) != 2 || jurisdiction != strings.ToUpper(jurisdiction) || strings.TrimSpace(evidence) == "" || at.IsZero() {
		return nil, ErrInvalidIdentity
	}
	e := &LegalEntity{tenantID: t, id: id, registeredName: name, jurisdiction: jurisdiction, registrationEvidenceRef: evidence, status: EntityDraft}
	e.pending = append(e.pending, Event{Type: "LegalEntityRegistered.v1", At: at.UTC(), TenantID: t, SubjectID: string(id)})
	return e, nil
}
func (e *LegalEntity) Verify(approver string, at time.Time) error {
	if e.status != EntityDraft || strings.TrimSpace(approver) == "" || at.IsZero() {
		return ErrInvalidTransition
	}
	e.status = EntityVerified
	e.pending = append(e.pending, Event{Type: "LegalEntityVerified.v1", At: at.UTC(), TenantID: e.tenantID, SubjectID: string(e.id)})
	return nil
}
func (e *LegalEntity) ApproveBranch(branch Branch) error {
	if e.status != EntityVerified || strings.TrimSpace(branch.ID) == "" || strings.TrimSpace(branch.Name) == "" || strings.TrimSpace(branch.EvidenceRef) == "" || branch.ApprovedAt.IsZero() {
		return ErrInvalidIdentity
	}
	for _, b := range e.branches {
		if b.ID == branch.ID {
			return ErrInvalidIdentity
		}
	}
	e.branches = append(e.branches, branch)
	e.pending = append(e.pending, Event{Type: "BranchAffiliationApproved.v1", At: branch.ApprovedAt.UTC(), TenantID: e.tenantID, SubjectID: string(e.id)})
	return nil
}
func (e *LegalEntity) Pending() []Event { return append([]Event(nil), e.pending...) }
func (e *LegalEntity) Acknowledge()     { e.pending = nil }

func (e *LegalEntity) ID() LegalEntityID          { return e.id }
func (e *LegalEntity) Tenant() TenantID           { return e.tenantID }
func (e *LegalEntity) Status() LegalEntityStatus  { return e.status }
func (e *LegalEntity) ApprovedBranches() []Branch { return append([]Branch(nil), e.branches...) }
