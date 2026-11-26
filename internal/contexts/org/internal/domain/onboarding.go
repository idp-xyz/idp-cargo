package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrInvalidOnboarding is every refusal of an onboarding command: the message says what is wrong and the way out.
var ErrInvalidOnboarding = errors.New("org: ORG_INVALID_ONBOARDING")

// The profiles, the modules and the states of an onboarding (see api/onboarding.go).
const (
	ProfileStandard = "STANDARD"
	ProfileStrict   = "STRICT"
	ProfileLean     = "LEAN"

	ModuleFIN = "FIN"
	ModuleSEA = "SEA"

	StateProposed  = "PROPOSED"
	StateConfirmed = "CONFIRMED"
	StateComplete  = "COMPLETE"
	// StateWithdrawn is an onboarding withdrawn before it was confirmed (V2.102, batch F): never confirmed.
	StateWithdrawn = "WITHDRAWN"

	ModuleNotStarted = "NOT_STARTED"
	ModuleProposed   = "PROPOSED"
	ModuleApproved   = "APPROVED"

	MaxRiskStatement   = 2000
	MaxWithdrawReason  = 2000
	MaxFinanceStaff    = 10000
	MaxEntries         = 64
	StrictMinimumStaff = 3
)

var onboardingID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,99}$`)

// Modules are the modules an onboarding applies a profile to, in the order it does.
var Modules = []string{ModuleFIN, ModuleSEA}

// ModuleChangeID is the change set a module makes for an onboarding: derived from them, so that a retry of the
// confirmation finds the change set it made and never makes a second.
func ModuleChangeID(onboarding, module string) string {
	return "onb-" + onboarding + "-" + strings.ToLower(module)
}

// OnboardingEntry is a value of an override: one item at one scope, with the amount of a value that asks for one.
type OnboardingEntry struct {
	ScopeKind         string
	ScopeID           string
	Key               string
	Value             string
	ThresholdMinor    *int64 `json:",omitempty"`
	ThresholdCurrency string `json:",omitempty"`
}

// ModulePlan is what an onboarding applies to a module: the entries the profile and the overrides resolve to, and the
// version of the module's catalogue they were resolved against.
type ModulePlan struct {
	CatalogueVersion string
	Entries          []OnboardingEntry
}

// Onboarding is a consultant's proposal.
type Onboarding struct {
	ID        string
	Profile   string
	Overrides map[string][]OnboardingEntry
	Plan      map[string]ModulePlan
	// PlanSHA256 is the digest of the plan (PlanDigest), set by ORG when it records the proposal, never taken from the caller.
	PlanSHA256          string
	RiskStatement       string
	FinanceStaff        int
	SingleAdministrator bool
	ProposedBy          string
	ProposedAt          time.Time
}

// PlanDigest is the SHA-256, in lower-case hexadecimal, of the plan as JSON (encoding/json: the modules in the order of
// their names, each with CatalogueVersion and Entries in the order stored). The owner reads the plan and confirms the
// digest of what they read; ORG keeps the digest with the proposal and refuses a confirmation that carries another.
func PlanDigest(plan map[string]ModulePlan) string {
	raw, _ := json.Marshal(plan) // a map of strings, numbers and pointers to numbers cannot fail to marshal
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Confirmation is the owner's. Administrator is the single administrator the owner declared, empty when the proposal
// says the customer declares none.
type Confirmation struct {
	By               string
	At               time.Time
	AcknowledgedText string
	Administrator    string
	// PlanSHA256 is the digest of the plan the owner read, which is the proposal's.
	PlanSHA256 string
}

// Withdrawal is the withdrawal of an onboarding that was not confirmed: by whom, when and why.
type Withdrawal struct {
	By     string
	At     time.Time
	Reason string
}

// ValidActor says whether an actor id is 1 to 128 bytes without spaces around it, the form every module accepts.
func ValidActor(id string) bool {
	return strings.TrimSpace(id) != "" && strings.TrimSpace(id) == id && len(id) <= 128
}

// ModuleState is what an onboarding recorded for a module: the state and its change set.
type ModuleState struct {
	Module   string
	State    string
	ChangeID string
	// Version is the settings version the module put in force when it approved its change set (ModuleApproved only, 1 or more).
	Version int
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidOnboarding, fmt.Sprintf(format, args...))
}

// Validate checks a proposal on its own: its identifier, profile, overrides (each entry's shape; whether FIN and SEA
// accept them is theirs to say), risk statement and number of finance staff. The risk statement is kept as written.
func (o Onboarding) Validate() error {
	switch {
	case !onboardingID.MatchString(o.ID):
		return refuse("an onboarding is named by 1 to 100 letters, digits and . _ : / -, starting with a letter or digit; choose one")
	case !slices.Contains([]string{ProfileStandard, ProfileStrict, ProfileLean}, o.Profile):
		return refuse("profile %q is not one of STANDARD, STRICT and LEAN; choose one (the profiles are listed by the platform's profile query)", o.Profile)
	case strings.TrimSpace(o.RiskStatement) == "" || utf8.RuneCountInString(o.RiskStatement) > MaxRiskStatement:
		return refuse("the risk statement is 1 to %d characters; write what the customer is told it accepts", MaxRiskStatement)
	case o.FinanceStaff < 1 || o.FinanceStaff > MaxFinanceStaff:
		return refuse("the customer's declared number of finance staff is 1 to %d; ask the customer", MaxFinanceStaff)
	case !ValidActor(o.ProposedBy):
		return refuse("an onboarding names its proposer (1 to 128 bytes, no spaces around it); give the actor id as the identity platform names the person")
	}
	for module, entries := range o.Overrides {
		if !slices.Contains(Modules, module) {
			return refuse("overrides are for FIN and SEA, not %q; use one of them", module)
		}
		if len(entries) > MaxEntries {
			return refuse("overrides of %s are at most %d entries; leave the rest to the module's own settings commands after the onboarding", module, MaxEntries)
		}
		for _, e := range entries {
			if err := e.validate("an override of " + module); err != nil {
				return err
			}
		}
	}
	for _, module := range Modules {
		p, ok := o.Plan[module]
		switch {
		case !ok || strings.TrimSpace(p.CatalogueVersion) == "" || len(p.CatalogueVersion) > 64:
			return refuse("the plan names the catalogue version of %s (1 to 64 characters), which the platform's onboarding fills in; propose through it", module)
		case len(p.Entries) < 1 || len(p.Entries) > MaxEntries:
			return refuse("the plan of %s is 1 to %d entries, resolved from the profile and the overrides; shorten the overrides, or propose the rest through the module's own settings commands after the onboarding", module, MaxEntries)
		}
		for _, e := range p.Entries {
			if err := e.validate("the plan of " + module); err != nil {
				return err
			}
		}
	}
	for module := range o.Plan {
		if !slices.Contains(Modules, module) {
			return refuse("the plan is for FIN and SEA, not %q; propose through the platform's onboarding", module)
		}
	}
	return nil
}

// validate checks an entry's shape; what names it ("an override of FIN") begins the refusal.
func (e OnboardingEntry) validate(what string) error {
	switch {
	case e.Key == "" || e.Value == "":
		return refuse("%s names its item and its value; give both", what)
	case e.ScopeKind != "TENANT" && e.ScopeKind != "LEGAL_ENTITY":
		return refuse("%s %s is at scope TENANT or LEGAL_ENTITY, not %q; choose one", what, e.Key, e.ScopeKind)
	case (e.ScopeKind == "TENANT") != (e.ScopeID == ""):
		return refuse("%s %s names a legal entity for scope LEGAL_ENTITY and none for TENANT; fix it", what, e.Key)
	case (e.ThresholdMinor != nil) != (e.ThresholdCurrency != ""):
		return refuse("%s %s gives a threshold with its currency, or neither; fix it", what, e.Key)
	}
	return nil
}

// StrictNotice is what a STRICT proposal with fewer than three declared finance staff is told, empty otherwise. The
// proposal is not refused.
func StrictNotice(profile string, staff int) string {
	if profile != ProfileStrict || staff >= StrictMinimumStaff {
		return ""
	}
	return fmt.Sprintf("the STRICT profile checks both the submitter and the previewer of an apportionment against its confirmer (F2 SUBMITTER_AND_PREVIEWER), "+
		"which wants at least %d finance staff, and the customer declared %d; the onboarding goes on, but a small team may find itself unable to confirm an "+
		"attribution: consider the STANDARD or LEAN profile, or more finance staff", StrictMinimumStaff, staff)
}

// StateOf is the onboarding's state from its confirmation, its withdrawal and its modules' records.
func StateOf(confirmed, withdrawn bool, modules []ModuleState) string {
	if withdrawn {
		return StateWithdrawn
	}
	if !confirmed {
		return StateProposed
	}
	for _, m := range Modules {
		if ModuleStateOf(modules, m) != ModuleApproved {
			return StateConfirmed
		}
	}
	return StateComplete
}

// ModuleStateOf is a module's state: APPROVED, else PROPOSED, else NOT_STARTED.
func ModuleStateOf(modules []ModuleState, module string) string {
	state := ModuleNotStarted
	for _, m := range modules {
		if m.Module == module && (m.State == ModuleApproved || state == ModuleNotStarted) {
			state = m.State
		}
	}
	return state
}
