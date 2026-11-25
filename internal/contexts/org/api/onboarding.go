package api

import (
	"context"
	"time"
)

// Customer onboarding (V2.102, design section 9): ORG holds the record of who proposed the profile of the rules for a
// customer, who confirmed it and what the owner acknowledged. ORG does not apply the profile: the platform's
// onboarding does that in FIN and SEA through their own settings commands once the owner has confirmed, and records here
// the change set and the state of each module. ORG knows two different people, not who the customer's owner is.

// Actions an Authorizer decides on (AuthorizePartyAction); the subject is the onboarding. Both are granted by name
// only: "org:*" and "*" grant neither.
const (
	ActionProposeOnboarding = "ProposeOnboarding"
	ActionConfirmOnboarding = "ConfirmOnboarding"
)

// Events; the onboarding is the subject.
const (
	OnboardingProposed  = "OnboardingProposed.v1"
	OnboardingConfirmed = "OnboardingConfirmed.v1"
)

// The profiles of the rules, and the modules an onboarding applies them to.
const (
	ProfileStandard = "STANDARD"
	ProfileStrict   = "STRICT"
	ProfileLean     = "LEAN"

	ModuleFIN = "FIN"
	ModuleSEA = "SEA"
)

// States of an onboarding: PROPOSED (waiting for the owner), CONFIRMED (the owner confirmed; a module is still to be
// applied: retry ConfirmOnboarding) and COMPLETE (every module's change set is approved). A module's state is
// NOT_STARTED, PROPOSED (its change set is proposed and waits for the owner's approval) or APPROVED.
const (
	OnboardingStateProposed  = "PROPOSED"
	OnboardingStateConfirmed = "CONFIRMED"
	OnboardingStateComplete  = "COMPLETE"

	ModuleNotStarted = "NOT_STARTED"
	ModuleProposed   = "PROPOSED"
	ModuleApproved   = "APPROVED"
)

// OnboardingEntry is one value an override proposes for one item in one scope, as the module's own entry: the company
// (ScopeKind TENANT, no ScopeID) or a legal entity (LEGAL_ENTITY and its ID). A threshold (FIN's F1 NOT_ABOVE_THRESHOLD)
// is an amount in minor units of ThresholdCurrency, the legal entity's functional currency.
type OnboardingEntry struct {
	ScopeKind         string
	ScopeID           string
	Key               string
	Value             string
	ThresholdMinor    *int64 `json:",omitempty"`
	ThresholdCurrency string `json:",omitempty"`
}

// ProposeOnboarding records the consultant's proposal: the profile, the overrides by module (ModuleFIN, ModuleSEA; each
// override replaces the profile's value of its item at its scope, or adds a legal entity's), the risk statement (1 to
// 2000 characters) that the customer's owner will acknowledge, the customer's declared number of finance staff (1 to
// 10,000) and whether the customer declares a single administrator. OnboardingID names it (1 to 100 characters). The
// actor needs the permission ProposeOnboarding, granted by name only. A proposal is idempotent by command id; the same
// onboarding id with another content is refused. Receipt.Notice says that the STRICT profile wants at least three
// finance staff when fewer are declared: the proposal is recorded all the same.
type ProposeOnboarding struct {
	CommandID           string
	TenantID            string
	ActorID             string
	OnboardingID        string
	Profile             string
	Overrides           map[string][]OnboardingEntry
	RiskStatement       string
	FinanceStaff        int
	SingleAdministrator bool
}

// ConfirmOnboarding records the owner's confirmation and acknowledgement of the risk statement: AcknowledgedText must
// be the proposal's risk statement exactly, and is kept as acknowledged. The actor is not the proposer and needs the
// permission ConfirmOnboarding, granted by name only. A retry by the same actor with the same text answers the record
// again.
type ConfirmOnboarding struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OnboardingID     string
	AcknowledgedText string
}

// RecordOnboardingModule records the change set a module made for the confirmed onboarding and its state (ModuleProposed,
// then ModuleApproved), once each. The change set is 'onb-<onboarding>-fin' or 'onb-<onboarding>-sea'. The actor is the
// confirmer (the permission ConfirmOnboarding); it is the platform's onboarding that records it, on the module's receipt.
type RecordOnboardingModule struct {
	CommandID    string
	TenantID     string
	ActorID      string
	OnboardingID string
	Module       string
	ChangeID     string
	State        string
}

// OnboardingReceipt confirms an onboarding command. State is the onboarding's, Notice what the command says besides.
type OnboardingReceipt struct {
	CommandID    string
	OnboardingID string
	State        string
	NewRevision  int64
	Notice       string
}

// OnboardingModule is a module's part of an onboarding.
type OnboardingModule struct {
	Module   string
	State    string
	ChangeID string
}

// Onboarding is an onboarding with its confirmation and its modules (FIN and SEA, in that order).
type Onboarding struct {
	TenantID            string
	OnboardingID        string
	State               string
	Profile             string
	Overrides           map[string][]OnboardingEntry
	RiskStatement       string
	FinanceStaff        int
	SingleAdministrator bool
	ProposedBy          string
	ProposedAt          time.Time
	ConfirmedBy         string
	ConfirmedAt         time.Time
	AcknowledgedText    string
	Modules             []OnboardingModule
	Notice              string
}

// GetOnboarding asks for one onboarding of the tenant (ErrNotFound for an unknown one); ListOnboardings for all of
// them in the order proposed.
type GetOnboarding struct {
	TenantID     string
	OnboardingID string
}

type ListOnboardings struct{ TenantID string }

type OnboardingCommands interface {
	ProposeOnboarding(ctx context.Context, cmd ProposeOnboarding) (OnboardingReceipt, error)
	ConfirmOnboarding(ctx context.Context, cmd ConfirmOnboarding) (OnboardingReceipt, error)
	RecordOnboardingModule(ctx context.Context, cmd RecordOnboardingModule) (OnboardingReceipt, error)
}

type OnboardingQueries interface {
	GetOnboarding(ctx context.Context, q GetOnboarding) (Onboarding, error)
	ListOnboardings(ctx context.Context, q ListOnboardings) ([]Onboarding, error)
}
