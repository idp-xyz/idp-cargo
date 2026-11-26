package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Customer onboarding (V2.102, design section 9): ORG holds the record of who proposed the profile of the rules for a
// customer, what the proposal makes of it in each module, who confirmed it and what the owner acknowledged. ORG does not
// apply the profile: the platform's onboarding does that in FIN and SEA through their own settings commands, in the identity
// of the people actually signed in (V2.102, batch F): the consultant's proposal holds each module's change set, exactly the
// entries stored in the proposal, with the catalogue version each was resolved against, and the owner's confirmation approves
// them; an onboarding not confirmed is withdrawn instead, which cancels them. It records here the change set, its state and
// the settings version of each module. ORG knows different people, not who the customer's owner or administrator is.

// Actions an Authorizer decides on (AuthorizePartyAction); the subject is the onboarding. All are granted by name
// only: "org:*" and "*" grant none. ActionRecordOnboardingModule is the platform's own: the principal the bootstrap
// builds for the onboarding holds it, and no person's role does.
const (
	ActionProposeOnboarding      = "ProposeOnboarding"
	ActionConfirmOnboarding      = "ConfirmOnboarding"
	ActionRecordOnboardingModule = "RecordOnboardingModule"
)

// Events; the onboarding is the subject.
const (
	OnboardingProposed  = "OnboardingProposed.v1"
	OnboardingConfirmed = "OnboardingConfirmed.v1"
	OnboardingWithdrawn = "OnboardingWithdrawn.v1"
)

// The profiles of the rules, and the modules an onboarding applies them to.
const (
	ProfileStandard = "STANDARD"
	ProfileStrict   = "STRICT"
	ProfileLean     = "LEAN"

	ModuleFIN = "FIN"
	ModuleSEA = "SEA"
)

// States of an onboarding: PROPOSED (waiting for the owner; the modules' change sets are held for it), CONFIRMED (the owner
// confirmed; a module is still to be applied: retry ConfirmOnboarding), COMPLETE (every module's change set is approved) and
// WITHDRAWN (withdrawn before it was confirmed, and never confirmed). A module's state is NOT_STARTED, PROPOSED (its change set
// is held for the onboarding and waits for the owner's approval) or APPROVED.
const (
	OnboardingStateProposed  = "PROPOSED"
	OnboardingStateConfirmed = "CONFIRMED"
	OnboardingStateComplete  = "COMPLETE"
	OnboardingStateWithdrawn = "WITHDRAWN"

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

// ModulePlan is what an onboarding applies to a module: the entries the profile and the overrides make of it, resolved
// when the consultant proposes, and the version of the module's catalogue they were resolved against.
type ModulePlan struct {
	CatalogueVersion string
	Entries          []OnboardingEntry
}

// PlanSHA256 is the SHA-256, in lower-case hexadecimal, of a plan as JSON (encoding/json): the digest ORG keeps with the
// proposal, which the proposal's receipt and GetOnboarding show with the plan, and which the owner's confirmation carries.
func PlanSHA256(plan map[string]ModulePlan) string {
	raw, _ := json.Marshal(plan) // a map of strings, numbers and pointers to numbers cannot fail to marshal
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ProposeOnboarding records the consultant's proposal: the profile, the overrides by module (ModuleFIN, ModuleSEA; each
// override replaces the profile's value of its item at its scope, or adds a legal entity's), the plan, which is the
// entries of each module that the profile and the overrides resolve to (1 to 64 for each, with the module's catalogue
// version: the platform's onboarding fills it, ORG does not know the profiles), the risk statement (1 to 2000
// characters) that the customer's owner will acknowledge, the customer's declared number of finance staff (1 to
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
	Plan                map[string]ModulePlan
	RiskStatement       string
	FinanceStaff        int
	SingleAdministrator bool
}

// ConfirmOnboarding records the owner's confirmation and acknowledgement of the risk statement: AcknowledgedText must
// be the proposal's risk statement exactly, and is kept as acknowledged. PlanSHA256 is the digest of the plan the owner
// read (the proposal's receipt and GetOnboarding show the plan and its digest): it must be the stored plan's, as the text
// must be the risk statement, and is kept with the confirmation; another is refused, and the way out is to read the
// onboarding again and confirm what it shows. AdministratorID is the owner's own declaration
// of the customer's one administrator: it is named if and only if the proposal says the customer declares a single
// administrator, and it is not the proposer. The actor is not the proposer and needs the permission ConfirmOnboarding,
// granted by name only. A retry by the same actor with the same text and administrator answers the record again.
type ConfirmOnboarding struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OnboardingID     string
	AcknowledgedText string
	PlanSHA256       string
	AdministratorID  string
}

// WithdrawOnboarding withdraws an onboarding that is not confirmed, with a reason of 1 to 2000 characters: it is then never
// confirmed, and the platform's onboarding cancels the change sets held for it in FIN and SEA, in the withdrawing person's own
// identity. The consultant who proposed it (ActionProposeOnboarding) or the customer's owner (ActionConfirmOnboarding) withdraws it.
// The record is immutable: who withdrew, when and why. The same person with the same reason again answers the record.
type WithdrawOnboarding struct {
	CommandID    string
	TenantID     string
	ActorID      string
	OnboardingID string
	Reason       string
}

// RecordOnboardingModule records the change set a module made for the onboarding and its state: ModuleProposed when the
// consultant's change set is held for it (the actor is the proposer), then ModuleApproved, with the settings Version the module
// put in force, once the owner confirmed and the module approved it (the actor is the confirmer), once each. The change set is
// 'onb-<onboarding>-fin' or 'onb-<onboarding>-sea'. Only the platform's onboarding records it, on the module's own change set:
// it needs the permission RecordOnboardingModule, granted by name only and held by the principal the bootstrap builds for it.
type RecordOnboardingModule struct {
	CommandID    string
	TenantID     string
	ActorID      string
	OnboardingID string
	Module       string
	ChangeID     string
	State        string
	Version      int
}

// OnboardingReceipt confirms an onboarding command. State is the onboarding's, Notice what the command says besides. Plan
// is what will be applied to each module and PlanSHA256 its digest (PlanSHA256 computes it), so that the owner who is
// shown the proposal's receipt reads the plan and confirms its digest.
type OnboardingReceipt struct {
	CommandID    string
	OnboardingID string
	State        string
	NewRevision  int64
	Notice       string
	Plan         map[string]ModulePlan
	PlanSHA256   string
}

// OnboardingModule is a module's part of an onboarding.
type OnboardingModule struct {
	Module   string
	State    string
	ChangeID string
	// Version is the settings version the module put in force when its change set was approved (0 until then).
	Version int
}

// Onboarding is an onboarding with its confirmation and its modules (FIN and SEA, in that order).
type Onboarding struct {
	TenantID            string
	OnboardingID        string
	State               string
	Profile             string
	Overrides           map[string][]OnboardingEntry
	Plan                map[string]ModulePlan
	PlanSHA256          string
	RiskStatement       string
	FinanceStaff        int
	SingleAdministrator bool
	ProposedBy          string
	ProposedAt          time.Time
	ConfirmedBy         string
	ConfirmedAt         time.Time
	AcknowledgedText    string
	// AdministratorID is the single administrator the owner declared when confirming; empty when the customer declares none.
	AdministratorID string
	// WithdrawnBy, WithdrawnAt and WithdrawReason say who withdrew the onboarding, when and why (State WITHDRAWN).
	WithdrawnBy    string
	WithdrawnAt    time.Time
	WithdrawReason string
	Modules        []OnboardingModule
	Notice         string
}

// GetOnboarding asks for one onboarding of the tenant (ErrNotFound for an unknown one); ListOnboardings for all of
// them in the order proposed.
// GetOnboarding reads one onboarding. A person's read names the actor, who needs "org:ReadOnboarding" (since V2.106,
// the review of V2.102, F13); the platform's onboarding reads in process and names none.
type GetOnboarding struct {
	TenantID     string
	ActorID      string
	OnboardingID string
}

// ListOnboardings lists the tenant's onboardings, with the actor as GetOnboarding.
type ListOnboardings struct {
	TenantID string
	ActorID  string
}

// ActionReadOnboarding is the read of the onboardings by a person (since V2.106).
const ActionReadOnboarding = "ReadOnboarding"

type OnboardingCommands interface {
	ProposeOnboarding(ctx context.Context, cmd ProposeOnboarding) (OnboardingReceipt, error)
	ConfirmOnboarding(ctx context.Context, cmd ConfirmOnboarding) (OnboardingReceipt, error)
	WithdrawOnboarding(ctx context.Context, cmd WithdrawOnboarding) (OnboardingReceipt, error)
	RecordOnboardingModule(ctx context.Context, cmd RecordOnboardingModule) (OnboardingReceipt, error)
}

type OnboardingQueries interface {
	GetOnboarding(ctx context.Context, q GetOnboarding) (Onboarding, error)
	ListOnboardings(ctx context.Context, q ListOnboardings) ([]Onboarding, error)
}
