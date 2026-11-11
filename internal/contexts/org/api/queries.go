package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// IdentitySource marks a PartyRef issued by ORG.
const IdentitySource = "ORG"

// PartyRef is PartyRef v1, the reference other contexts keep to an ORG party at a known revision. It
// carries identifiers only; names and other particulars go into the holder's own snapshot.
type PartyRef struct {
	TenantID         string `json:"tenant_id"`
	PartyID          string `json:"party_id"`
	PartyRevision    int64  `json:"party_revision"`
	IdentitySource   string `json:"identity_source"`
	LegalEntityScope string `json:"legal_entity_scope,omitempty"`
}

// ResolvePartyRole asks whether a party may act in a partner role for a legal entity at an instant.
// TenantID must come from a verified principal. Ask before opening your own transaction: ORG answers
// in a transaction of its own and fails inside another context's transaction.
type ResolvePartyRole struct {
	TenantID      string
	PartyID       string
	RoleCode      string
	LegalEntityID string
	At            time.Time
}

// PartyRole is a positive answer: the reference to keep, the particulars to freeze into the caller's
// own transaction snapshot, and the approved role in force at the instant asked about. Since ORG V0.4 the
// particulars include the English name and the active identifiers, read in the same view as the revision.
type PartyRole struct {
	Ref         PartyRef
	LegalName   string
	CountryCode string
	RoleCode    string
	Validity    kernel.ValidityInterval
	EvidenceRef string
	EnglishName string
	Identifiers []PartyIdentifier
}

// Queries is ORG's read side for other contexts. A party that may not act in the role yields
// ErrRejected; an unknown party or legal entity yields ErrNotFound.
type Queries interface {
	ResolvePartyRole(ctx context.Context, q ResolvePartyRole) (PartyRole, error)
}

// Legal entity statuses (MD-02): only a VERIFIED legal entity contracts, quotes and issues documents.
const (
	LegalEntityDraft     = "DRAFT"
	LegalEntityVerified  = "VERIFIED"
	LegalEntitySuspended = "SUSPENDED"
)

// GetLegalEntity asks for a legal entity of the tenant. TenantID must come from a verified principal.
type GetLegalEntity struct {
	TenantID      string
	LegalEntityID string
}

// LegalEntityRef is LegalEntityRef v1: a legal entity at a revision, with the registered name and
// jurisdiction a holder freezes into its own snapshot (MD-02, MD-05), such as DOC for the issuer of a
// document.
type LegalEntityRef struct {
	TenantID       string
	LegalEntityID  string
	Revision       int64
	RegisteredName string
	Jurisdiction   string // ISO 3166-1 alpha-2
	Status         string
}

// LegalEntityQueries is ORG's read side of legal entities. An unknown legal entity yields ErrNotFound.
type LegalEntityQueries interface {
	GetLegalEntity(ctx context.Context, q GetLegalEntity) (LegalEntityRef, error)
}
