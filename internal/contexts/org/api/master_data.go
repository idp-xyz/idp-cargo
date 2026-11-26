package api

import (
	"context"
	"time"
)

// ORG's master data commands (since V2.106): a business party is registered in DRAFT, activated once who it is has
// been checked (the evidence of that check), and its legal name corrected, suspended and archived with a reason; a
// legal entity of the tenant (one of the forwarder's own companies) is registered in DRAFT and verified, after which
// it may contract, quote and issue documents. Until V2.106 both were written by the demo's seed alone. Each command
// is one revision of its party or legal entity, committed with its Outbox event and its receipt.

// Master data actions an Authorizer decides on.
const (
	ActionRegisterParty         = "RegisterParty"
	ActionActivateParty         = "ActivateParty"
	ActionCorrectPartyLegalName = "CorrectPartyLegalName"
	ActionSuspendParty          = "SuspendParty"
	ActionArchiveParty          = "ArchiveParty"
	ActionRegisterLegalEntity   = "RegisterLegalEntity"
	ActionVerifyLegalEntity     = "VerifyLegalEntity"
)

// Party statuses.
const (
	PartyDraft     = "DRAFT"
	PartyActive    = "ACTIVE"
	PartySuspended = "SUSPENDED"
	PartyArchived  = "ARCHIVED"
)

// RegisterParty registers a party in DRAFT with its legal name and country of registration (ISO 3166-1 alpha-2).
type RegisterParty struct {
	CommandID   string
	TenantID    string
	ActorID     string
	PartyID     string
	LegalName   string
	CountryCode string
	OccurredAt  time.Time
}

// ActivateParty makes a DRAFT party ACTIVE, naming the evidence that it is who it says (a business licence, a
// registry extract); only an active party can be approved for a role.
type ActivateParty struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	EvidenceRef      string
	OccurredAt       time.Time
}

// CorrectPartyLegalName corrects the legal name, naming the evidence; an archived party is not changed.
type CorrectPartyLegalName struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	LegalName        string
	EvidenceRef      string
	OccurredAt       time.Time
}

// SuspendParty suspends an active party with a reason: it can act in no role while suspended.
type SuspendParty struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// ArchiveParty archives a suspended party whose roles have all ended, with a reason.
type ArchiveParty struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// RegisterLegalEntity registers one of the tenant's own companies in DRAFT: its registered name, jurisdiction (ISO
// 3166-1 alpha-2) and the evidence of its registration.
type RegisterLegalEntity struct {
	CommandID      string
	TenantID       string
	ActorID        string
	LegalEntityID  string
	RegisteredName string
	Jurisdiction   string
	EvidenceRef    string
	OccurredAt     time.Time
}

// VerifyLegalEntity verifies a DRAFT legal entity: from then on it contracts, quotes and issues documents (MD-02).
type VerifyLegalEntity struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LegalEntityID    string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// LegalEntityReceipt confirms a committed legal entity command.
type LegalEntityReceipt struct {
	CommandID     string
	LegalEntityID string
	NewRevision   int64
}

// MasterDataCommands are ORG's master data commands.
type MasterDataCommands interface {
	RegisterParty(ctx context.Context, cmd RegisterParty) (Receipt, error)
	ActivateParty(ctx context.Context, cmd ActivateParty) (Receipt, error)
	CorrectPartyLegalName(ctx context.Context, cmd CorrectPartyLegalName) (Receipt, error)
	SuspendParty(ctx context.Context, cmd SuspendParty) (Receipt, error)
	ArchiveParty(ctx context.Context, cmd ArchiveParty) (Receipt, error)
	RegisterLegalEntity(ctx context.Context, cmd RegisterLegalEntity) (LegalEntityReceipt, error)
	VerifyLegalEntity(ctx context.Context, cmd VerifyLegalEntity) (LegalEntityReceipt, error)
}

// ListParties lists the tenant's parties by id, a page at a time: those after After (a party id), at most Limit (1 to
// 200; 0 is 50). Status, RoleCode and NameContains (case-insensitive, in the legal or English name) narrow it.
type ListParties struct {
	TenantID     string
	Status       string
	RoleCode     string
	NameContains string
	After        string
	Limit        int
}

// ApprovedRole is a role a party is approved for, for a legal entity of the tenant.
type ApprovedRole struct {
	RoleCode      string
	LegalEntityID string
	ValidFrom     time.Time
	ValidUntil    time.Time // zero: open-ended
	EvidenceRef   string
	ApprovedBy    string
	ApprovedAt    time.Time
}

// PartySummary is a party in a list: its names, status and approved roles.
type PartySummary struct {
	PartyID     string
	Revision    int64
	LegalName   string
	EnglishName string
	CountryCode string
	Status      string
	Roles       []ApprovedRole
}

// PartyList is a page of parties; Next is the After of the next page, empty on the last.
type PartyList struct {
	Parties []PartySummary
	Next    string
}

// ListLegalEntities lists the tenant's legal entities.
type ListLegalEntities struct {
	TenantID string
}

// MasterDataQueries are the reads of ORG's master data for people. ResolvePartyRole, GetLegalEntity and the address
// book reads stay the other contexts' reads.
type MasterDataQueries interface {
	ListParties(ctx context.Context, q ListParties) (PartyList, error)
	ListLegalEntities(ctx context.Context, q ListLegalEntities) ([]LegalEntityRef, error)
}
