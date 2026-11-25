package domain

import (
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

	ModuleNotStarted = "NOT_STARTED"
	ModuleProposed   = "PROPOSED"
	ModuleApproved   = "APPROVED"

	MaxRiskStatement   = 2000
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

// Onboarding is a consultant's proposal.
type Onboarding struct {
	ID                  string
	Profile             string
	Overrides           map[string][]OnboardingEntry
	RiskStatement       string
	FinanceStaff        int
	SingleAdministrator bool
	ProposedBy          string
	ProposedAt          time.Time
}

// Confirmation is the owner's.
type Confirmation struct {
	By               string
	At               time.Time
	AcknowledgedText string
}

// ModuleState is what an onboarding recorded for a module: the state and its change set.
type ModuleState struct {
	Module   string
	State    string
	ChangeID string
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
	case strings.TrimSpace(o.ProposedBy) == "" || len(o.ProposedBy) > 128:
		return refuse("an onboarding names its proposer (1 to 128 bytes); this is ORG's own call, so ask the platform's owners if you see it")
	}
	for module, entries := range o.Overrides {
		if !slices.Contains(Modules, module) {
			return refuse("overrides are for FIN and SEA, not %q; use one of them", module)
		}
		if len(entries) > MaxEntries {
			return refuse("overrides of %s are at most %d entries; leave the rest to the module's own settings commands after the onboarding", module, MaxEntries)
		}
		for _, e := range entries {
			switch {
			case e.Key == "" || e.Value == "":
				return refuse("an override of %s names its item and its value; give both", module)
			case e.ScopeKind != "TENANT" && e.ScopeKind != "LEGAL_ENTITY":
				return refuse("an override of %s %s is at scope TENANT or LEGAL_ENTITY, not %q; choose one", module, e.Key, e.ScopeKind)
			case (e.ScopeKind == "TENANT") != (e.ScopeID == ""):
				return refuse("an override of %s %s names a legal entity for scope LEGAL_ENTITY and none for TENANT; fix it", module, e.Key)
			case (e.ThresholdMinor != nil) != (e.ThresholdCurrency != ""):
				return refuse("an override of %s %s gives a threshold with its currency, or neither; fix it", module, e.Key)
			}
		}
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

// StateOf is the onboarding's state from its confirmation and its modules' records.
func StateOf(confirmed bool, modules []ModuleState) string {
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
