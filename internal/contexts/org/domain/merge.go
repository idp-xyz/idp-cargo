package domain

import (
	"strings"
	"time"
)

type MergeStatus string

const (
	MergeOpen     MergeStatus = "OPEN"
	MergeAssessed MergeStatus = "ASSESSED"
	MergeApproved MergeStatus = "APPROVED"
	MergeRejected MergeStatus = "REJECTED"
	MergeApplied  MergeStatus = "ALIAS_APPLIED"
)

type MergeCase struct {
	tenantID          TenantID
	id                MergeCaseID
	source            PartyID
	target            PartyID
	openedBy          string
	evidenceRef       string
	impactEvidenceRef string
	approvedBy        string
	sourceRevision    int64
	targetRevision    int64
	status            MergeStatus
	pending           []Event
}

func OpenMergeCase(t TenantID, id MergeCaseID, source, target PartyID, actor, evidence string, at time.Time) (*MergeCase, error) {
	if t == "" || id == "" || source == "" || target == "" || source == target || strings.TrimSpace(actor) == "" || strings.TrimSpace(evidence) == "" || at.IsZero() {
		return nil, ErrInvalidIdentity
	}
	return &MergeCase{tenantID: t, id: id, source: source, target: target, openedBy: actor, evidenceRef: evidence, status: MergeOpen}, nil
}
func (m *MergeCase) Assess(impactEvidence string, source, target *BusinessParty, srcRevision, targetRevision int64) error {
	if m.status != MergeOpen || strings.TrimSpace(impactEvidence) == "" || srcRevision < 1 || targetRevision < 1 {
		return ErrInvalidTransition
	}
	if source == nil || target == nil || source.tenantID != m.tenantID || target.tenantID != m.tenantID {
		return ErrCrossTenant
	}
	if source.id != m.source || target.id != m.target || source.status != PartyActive || target.status != PartyActive {
		return ErrInvalidIdentity
	}
	m.impactEvidenceRef = impactEvidence
	m.sourceRevision = srcRevision
	m.targetRevision = targetRevision
	m.status = MergeAssessed
	return nil
}
func (m *MergeCase) Approve(approver string, at time.Time) error {
	if m.status != MergeAssessed || strings.TrimSpace(approver) == "" || at.IsZero() {
		return ErrInvalidTransition
	}
	if approver == m.openedBy {
		return ErrSelfApproval
	}
	m.approvedBy = approver
	m.status = MergeApproved
	m.pending = append(m.pending, Event{Type: "PartnerMergeApproved.v1", At: at.UTC(), TenantID: m.tenantID, SubjectID: string(m.id)})
	return nil
}
func (m *MergeCase) Reject(actor, reason string, at time.Time) error {
	if (m.status != MergeOpen && m.status != MergeAssessed) || strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || at.IsZero() {
		return ErrInvalidTransition
	}
	m.status = MergeRejected
	m.pending = append(m.pending, Event{Type: "PartnerMergeRejected.v1", At: at.UTC(), TenantID: m.tenantID, SubjectID: string(m.id)})
	return nil
}

// ApplyAlias validates captured aggregate revisions. The adapter must check them atomically
// while writing immutable alias and event/outbox in ONE database transaction.
func (m *MergeCase) ApplyAlias(srcRev, targetRev int64, at time.Time) error {
	if m.status != MergeApproved || at.IsZero() {
		return ErrUnapprovedMerge
	}
	if srcRev != m.sourceRevision || targetRev != m.targetRevision {
		return ErrConflict
	}
	m.status = MergeApplied
	m.pending = append(m.pending, Event{Type: "PartnerAliasApplied.v1", At: at.UTC(), TenantID: m.tenantID, SubjectID: string(m.id)})
	return nil
}
func (m *MergeCase) Pending() []Event { return append([]Event(nil), m.pending...) }
func (m *MergeCase) Acknowledge()     { m.pending = nil }

func (m *MergeCase) ID() MergeCaseID                  { return m.id }
func (m *MergeCase) Tenant() TenantID                 { return m.tenantID }
func (m *MergeCase) Status() MergeStatus              { return m.status }
func (m *MergeCase) Participants() (PartyID, PartyID) { return m.source, m.target }
func (m *MergeCase) RevisionPins() (int64, int64)     { return m.sourceRevision, m.targetRevision }
func (m *MergeCase) IsApproved() bool                 { return m.status == MergeApproved }
