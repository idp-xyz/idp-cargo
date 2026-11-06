// Package domain owns ORG identity and scoped business roles.
// It intentionally has NO dependency on infrastructure, AI, IAM or other business contexts.
package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

type TenantID string
type PartyID string
type LegalEntityID string
type MergeCaseID string

type RoleCode string

const (
	RoleCustomer      RoleCode = "CUSTOMER"
	RoleSupplier      RoleCode = "SUPPLIER"
	RoleCarrier       RoleCode = "CARRIER"
	RoleOverseasAgent RoleCode = "OVERSEAS_AGENT"
	RoleShipper       RoleCode = "SHIPPER"
	RoleConsignee     RoleCode = "CONSIGNEE"
)

type PartyStatus string

const (
	PartyDraft     PartyStatus = "DRAFT"
	PartyActive    PartyStatus = "ACTIVE"
	PartySuspended PartyStatus = "SUSPENDED"
	PartyArchived  PartyStatus = "ARCHIVED"
)

var (
	ErrInvalidIdentity   = errors.New("org: invalid identity")
	ErrInvalidTransition = errors.New("org: invalid state transition")
	ErrRoleOverlap       = errors.New("org: overlapping approved role")
	ErrMissingEvidence   = errors.New("org: missing evidence")
	ErrCrossTenant       = errors.New("org: cross-tenant identity mismatch")
	ErrUnapprovedMerge   = errors.New("org: unapproved merge")
	ErrConflict          = errors.New("org: concurrent revision conflict")
	ErrSelfApproval      = errors.New("org: independent approver required")

	ErrPartyNotActive         = errors.New("org: party not active")
	ErrRoleNotEffective       = errors.New("org: no approved role in force")
	ErrLegalEntityNotVerified = errors.New("org: legal entity not verified")
)

type Event struct {
	Type      string
	At        time.Time
	TenantID  TenantID
	SubjectID string
	// Deliberately excludes legal identifiers and PII; event transport maps this to Bento Envelope.
}

type Role struct {
	Code          RoleCode
	LegalEntityID LegalEntityID
	ValidFrom     time.Time
	ValidUntil    time.Time // zero => open-ended
	EvidenceRef   string
	ApprovedBy    string
	ApprovedAt    time.Time // Actual approval instant, not role validity start.
}

// Validity is the half-open interval in which the role is in force.
func (r Role) Validity() kernel.ValidityInterval {
	return kernel.ValidityInterval{From: r.ValidFrom, Until: r.ValidUntil}
}

func (r Role) valid() bool {
	return r.Code != "" && r.LegalEntityID != "" && r.Validity().Valid() &&
		strings.TrimSpace(r.EvidenceRef) != "" && strings.TrimSpace(r.ApprovedBy) != ""
}
func overlaps(a, b Role) bool {
	return a.Code == b.Code && a.LegalEntityID == b.LegalEntityID && a.Validity().Overlaps(b.Validity())
}

type BusinessParty struct {
	tenantID    TenantID
	id          PartyID
	legalName   string
	countryCode string
	status      PartyStatus
	roles       []Role
	pending     []Event
}

func RegisterParty(tenant TenantID, id PartyID, name, country string, at time.Time) (*BusinessParty, error) {
	if strings.TrimSpace(string(tenant)) == "" || strings.TrimSpace(string(id)) == "" || strings.TrimSpace(name) == "" ||
		len(country) != 2 || country != strings.ToUpper(country) || at.IsZero() {
		return nil, ErrInvalidIdentity
	}
	p := &BusinessParty{tenantID: tenant, id: id, legalName: strings.TrimSpace(name), countryCode: country, status: PartyDraft}
	p.record("BusinessPartyRegistered.v1", at)
	return p, nil
}
func (p *BusinessParty) Activate(evidence string, at time.Time) error {
	if p.status != PartyDraft || at.IsZero() {
		return ErrInvalidTransition
	}
	if strings.TrimSpace(evidence) == "" {
		return ErrMissingEvidence
	}
	p.status = PartyActive
	p.record("BusinessPartyActivated.v1", at)
	return nil
}
func (p *BusinessParty) CorrectLegalName(newName, evidence string, at time.Time) error {
	if p.status == PartyArchived || at.IsZero() {
		return ErrInvalidTransition
	}
	if strings.TrimSpace(evidence) == "" {
		return ErrMissingEvidence
	}
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return ErrInvalidIdentity
	}
	if newName == p.legalName {
		return nil
	}
	p.legalName = newName
	p.record("PartyLegalNameCorrected.v1", at)
	return nil
}
func (p *BusinessParty) ApproveRole(role Role, at time.Time) error {
	if p.status != PartyActive || at.IsZero() {
		return ErrInvalidTransition
	}
	if !role.valid() {
		return ErrInvalidIdentity
	}
	for _, x := range p.roles {
		if overlaps(x, role) {
			return ErrRoleOverlap
		}
	}
	role.ApprovedAt = at.UTC()
	p.roles = append(p.roles, role)
	p.record("PartnerRoleApproved.v1", at)
	return nil
}

// EffectiveRole returns the approved role of the given code for a legal entity that is in force at
// an instant. Only an active party can act in a role.
func (p *BusinessParty) EffectiveRole(code RoleCode, entity LegalEntityID, at time.Time) (Role, error) {
	if p.status != PartyActive {
		return Role{}, fmt.Errorf("%w: party is %s", ErrPartyNotActive, p.status)
	}
	for _, r := range p.roles {
		if r.Code == code && r.LegalEntityID == entity && r.Validity().Contains(at) {
			return r, nil
		}
	}
	return Role{}, ErrRoleNotEffective
}
func (p *BusinessParty) Suspend(reason string, at time.Time) error {
	if p.status != PartyActive || at.IsZero() {
		return ErrInvalidTransition
	}
	if strings.TrimSpace(reason) == "" {
		return ErrMissingEvidence
	}
	p.status = PartySuspended
	p.record("BusinessPartySuspended.v1", at)
	return nil
}
func (p *BusinessParty) Archive(reason string, at time.Time) error {
	if p.status != PartySuspended || at.IsZero() {
		return ErrInvalidTransition
	}
	if strings.TrimSpace(reason) == "" {
		return ErrMissingEvidence
	}
	for _, r := range p.roles {
		if r.ValidUntil.IsZero() || at.Before(r.ValidUntil) {
			return fmt.Errorf("%w: active role", ErrInvalidTransition)
		}
	}
	p.status = PartyArchived
	p.record("BusinessPartyArchived.v1", at)
	return nil
}
func (p *BusinessParty) Pending() []Event { return append([]Event(nil), p.pending...) }

// Acknowledge is permitted only after the transaction containing both aggregate write and outbox enqueue commits.
func (p *BusinessParty) Acknowledge() { p.pending = nil }
func (p *BusinessParty) record(kind string, at time.Time) {
	p.pending = append(p.pending, Event{Type: kind, At: at.UTC(), TenantID: p.tenantID, SubjectID: string(p.id)})
}

// RestoreParty reconstructs a stored aggregate without emitting events.
func RestoreParty(tenant TenantID, id PartyID, name, country string, status PartyStatus, roles []Role) (*BusinessParty, error) {
	if tenant == "" || id == "" || strings.TrimSpace(name) == "" || len(country) != 2 {
		return nil, ErrInvalidIdentity
	}
	switch status {
	case PartyDraft, PartyActive, PartySuspended, PartyArchived:
	default:
		return nil, ErrInvalidIdentity
	}
	p := &BusinessParty{tenantID: tenant, id: id, legalName: name, countryCode: country, status: status, roles: append([]Role(nil), roles...)}
	for i, r := range p.roles {
		if !r.valid() {
			return nil, ErrInvalidIdentity
		}
		for j := 0; j < i; j++ {
			if overlaps(r, p.roles[j]) {
				return nil, ErrRoleOverlap
			}
		}
	}
	return p, nil
}

func (p *BusinessParty) ID() PartyID           { return p.id }
func (p *BusinessParty) Tenant() TenantID      { return p.tenantID }
func (p *BusinessParty) Status() PartyStatus   { return p.status }
func (p *BusinessParty) Name() string          { return p.legalName }
func (p *BusinessParty) Country() string       { return p.countryCode }
func (p *BusinessParty) ApprovedRoles() []Role { return append([]Role(nil), p.roles...) }
