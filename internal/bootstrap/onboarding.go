package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.idp.xyz/idp-cargo/internal/contexts/fin"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/sea"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// Customer onboarding (V2.102, design section 9 item 2). ORG holds the record; the bootstrap, which composes the
// contexts, applies it. ProposeOnboarding is the consultant's, ConfirmOnboarding the customer's owner's: once the owner
// has confirmed, the onboarding proposes FIN's and SEA's change sets as the consultant and approves them as the owner,
// through the modules' own settings commands, so the usual rules apply (a change set that only tightens is approved when
// it is proposed; one that loosens waits for the owner's approval, who is not the consultant). Each module commits on
// its own, with its change set named from the onboarding and its commands' ids derived from it, so that a retry of the
// same confirmation completes a module that did not and never makes a second change set. ORG's record keeps each module's
// change set and state.
//
// Permissions. The consultant needs org:ProposeOnboarding and the owner org:ConfirmOnboarding, each by name only ("org:*"
// and "*" grant neither). Because the platform acts for them later, when only the owner is present, the consultant must
// also hold fin:ConfigureSettings and sea:ConfigureSettings when proposing, the owner fin:ApproveSettings and
// sea:ApproveSettings when confirming (all by name only, as the modules ask them), and the platform then proposes as the
// consultant with exactly the two ConfigureSettings permissions and declares a single administrator as the owner with
// exactly the two DeclareSingleAdministrator permissions: no principal can make that declaration without ORG's
// confirmed onboarding, except one that holds that permission by name.

// ORGOnboarding is what the onboarding uses of ORG.
type ORGOnboarding interface {
	orgapi.OnboardingCommands
	orgapi.OnboardingQueries
}

// FINSettings is what the onboarding and the settings tools use of FIN's settings.
type FINSettings interface {
	ProposeFinSettingsChange(ctx context.Context, cmd finapi.ProposeFinSettingsChange) (finapi.Receipt, error)
	ApproveFinSettingsChange(ctx context.Context, cmd finapi.ApproveFinSettingsChange) (finapi.Receipt, error)
	DeclareFinSingleAdministrator(ctx context.Context, cmd finapi.DeclareFinSingleAdministrator) (finapi.Receipt, error)
	GetFinSettings(ctx context.Context, q finapi.GetFinSettings) (finapi.FinSettings, error)
	ListFinSettingsChanges(ctx context.Context, q finapi.ListFinSettingsChanges) ([]finapi.FinSettingsChange, error)
	GetFinSettingsCatalogue(ctx context.Context, q finapi.GetFinSettingsCatalogue) (finapi.FinSettingsCatalogue, error)
	GetFinExceptionReport(ctx context.Context, q finapi.GetFinExceptionReport) (finapi.FinExceptionReport, error)
}

// SEASettings is what the onboarding and the settings tools use of SEA's settings.
type SEASettings interface {
	seaapi.SettingsCommands
	seaapi.SettingsQueries
}

// Onboarding applies customer onboardings.
type Onboarding struct {
	org ORGOnboarding
	fin FINSettings
	sea SEASettings
}

// NewOnboarding composes ORG's record with FIN's and SEA's settings commands.
func NewOnboarding(org ORGOnboarding, fin FINSettings, sea SEASettings) (*Onboarding, error) {
	if org == nil || fin == nil || sea == nil {
		return nil, errors.New("bootstrap: the onboarding needs ORG, FIN and SEA")
	}
	return &Onboarding{org: org, fin: fin, sea: sea}, nil
}

// Interfaces FIN and SEA must keep: their settings commands and queries as the onboarding uses them.
var (
	_ FINSettings = (*fin.Module)(nil)
	_ SEASettings = (*sea.Module)(nil)
)

// ErrOnboardingIncomplete says that a confirmed onboarding did not reach every module; the confirmation says which and why.
var ErrOnboardingIncomplete = errors.New("bootstrap: the onboarding is confirmed but a module is not applied yet")

// ModuleFailure is a module that did not reach APPROVED, and why.
type ModuleFailure struct {
	Module string
	Err    error
}

// OnboardingConfirmation is the answer to ConfirmOnboarding: ORG's receipt and onboarding, and the modules that failed.
type OnboardingConfirmation struct {
	Receipt    orgapi.OnboardingReceipt
	Onboarding orgapi.Onboarding
	Failures   []ModuleFailure
}

func holdsNamed(ctx context.Context, bc, tenant, actor string, actions ...string) error {
	for _, a := range actions {
		if err := identity.AuthorizeNamed(ctx, bc, tenant, actor, a); err != nil {
			return err
		}
	}
	return nil
}

func entriesOfOnboarding(in []orgapi.OnboardingEntry) []onboardingEntry {
	out := make([]onboardingEntry, len(in))
	for i, e := range in {
		out[i] = onboardingEntry(e)
	}
	return out
}

// moduleEntries answers the entries that the profile and the overrides make for a module, as the module's own entries.
func moduleEntries(profile Profile, module string, overrides map[string][]orgapi.OnboardingEntry) ([]finapi.FinSettingEntry, []seaapi.SeaSettingEntry, error) {
	merged := profileEntries(profile, module, entriesOfOnboarding(overrides[module]))
	if module == ModuleFIN {
		f, err := finEntries(merged)
		return f, nil, err
	}
	s, err := seaEntries(merged)
	return nil, s, err
}

// checkModules refuses a proposal whose profile and overrides a module would refuse, with the module's own words.
func checkModules(profile Profile, overrides map[string][]orgapi.OnboardingEntry) error {
	for _, module := range []string{ModuleFIN, ModuleSEA} {
		f, s, err := moduleEntries(profile, module, overrides)
		if err == nil && module == ModuleFIN {
			err = fin.CheckSettingsEntries(f)
		} else if err == nil {
			err = sea.CheckSettingsEntries(s)
		}
		if err != nil {
			return fmt.Errorf("%w: the profile %s with the overrides of %s is not a valid configuration: %v; fix the override or choose another profile",
				orgapi.ErrRejected, profile.ID, module, err)
		}
	}
	return nil
}

// ProposeOnboarding is the consultant's proposal: it checks the profile and the overrides with FIN's and SEA's own
// rules, so that an invalid one is refused now and not when the owner confirms, and records it in ORG. The receipt
// carries the notice of a STRICT profile with fewer than three declared finance staff.
func (o *Onboarding) ProposeOnboarding(ctx context.Context, cmd orgapi.ProposeOnboarding) (orgapi.OnboardingReceipt, error) {
	if err := holdsNamed(ctx, "fin", cmd.TenantID, cmd.ActorID, finapi.ActionConfigureSettings); err != nil {
		return orgapi.OnboardingReceipt{}, fmt.Errorf("%w: %v; the platform proposes the modules' change sets as the consultant, so the consultant holds fin:ConfigureSettings by name", orgapi.ErrUnauthorized, err)
	}
	if err := holdsNamed(ctx, "sea", cmd.TenantID, cmd.ActorID, seaapi.ActionConfigureSettings); err != nil {
		return orgapi.OnboardingReceipt{}, fmt.Errorf("%w: %v; the platform proposes the modules' change sets as the consultant, so the consultant holds sea:ConfigureSettings by name", orgapi.ErrUnauthorized, err)
	}
	profile, ok := SettingsProfile(cmd.Profile)
	if !ok {
		return orgapi.OnboardingReceipt{}, fmt.Errorf("%w: profile %q is not one of STANDARD, STRICT and LEAN; choose one (SettingsProfiles lists them)", orgapi.ErrRejected, cmd.Profile)
	}
	for module := range cmd.Overrides {
		if module != ModuleFIN && module != ModuleSEA {
			return orgapi.OnboardingReceipt{}, fmt.Errorf("%w: overrides are for FIN and SEA, not %q; use one of them", orgapi.ErrRejected, module)
		}
	}
	if err := checkModules(profile, cmd.Overrides); err != nil {
		return orgapi.OnboardingReceipt{}, err
	}
	return o.org.ProposeOnboarding(ctx, cmd)
}

// ConfirmOnboarding is the owner's confirmation. It records it in ORG, then applies each module (FIN, then SEA): a
// single administrator is declared when the onboarding says so, the module's change set is proposed as the consultant
// and, when it loosens something, approved as the owner. A module that fails does not stop the other; the answer lists
// what failed and the error is ErrOnboardingIncomplete, with the way out: retry the same command.
func (o *Onboarding) ConfirmOnboarding(ctx context.Context, cmd orgapi.ConfirmOnboarding) (OnboardingConfirmation, error) {
	for _, bc := range []struct{ name, owns string }{{"fin", finapi.ActionApproveSettings}, {"sea", seaapi.ActionApproveSettings}} {
		if err := holdsNamed(ctx, bc.name, cmd.TenantID, cmd.ActorID, bc.owns); err != nil {
			return OnboardingConfirmation{}, fmt.Errorf("%w: %v; the platform approves the modules' change sets as the owner, so the owner holds %s:%s by name",
				orgapi.ErrUnauthorized, err, bc.name, bc.owns)
		}
	}
	receipt, err := o.org.ConfirmOnboarding(ctx, cmd)
	if err != nil {
		return OnboardingConfirmation{}, err
	}
	onb, err := o.org.GetOnboarding(ctx, orgapi.GetOnboarding{TenantID: cmd.TenantID, OnboardingID: cmd.OnboardingID})
	if err != nil {
		return OnboardingConfirmation{}, err
	}
	profile, ok := SettingsProfile(onb.Profile)
	if !ok {
		return OnboardingConfirmation{}, fmt.Errorf("bootstrap: onboarding %s names profile %q, which this build does not know; this build is older than the one that recorded it", onb.OnboardingID, onb.Profile)
	}
	out := OnboardingConfirmation{Receipt: receipt, Onboarding: onb}
	for _, module := range []string{ModuleFIN, ModuleSEA} {
		if err := o.applyModule(ctx, cmd, onb, profile, module); err != nil {
			out.Failures = append(out.Failures, ModuleFailure{Module: module, Err: err})
		}
	}
	if onb, err = o.org.GetOnboarding(ctx, orgapi.GetOnboarding{TenantID: cmd.TenantID, OnboardingID: cmd.OnboardingID}); err != nil {
		return out, err
	}
	out.Onboarding, out.Receipt.State = onb, onb.State
	if len(out.Failures) > 0 {
		var parts []string
		for _, f := range out.Failures {
			parts = append(parts, fmt.Sprintf("%s: %v", f.Module, f.Err))
		}
		return out, fmt.Errorf("%w (%s); fix the cause and send the same ConfirmOnboarding again, which completes the module without making a second change set",
			ErrOnboardingIncomplete, strings.Join(parts, "; "))
	}
	return out, nil
}

// applyModule takes one module to APPROVED: nothing to do when it is already, otherwise each step is idempotent by its
// derived command id, so a module half done is finished and nothing is made twice.
func (o *Onboarding) applyModule(ctx context.Context, cmd orgapi.ConfirmOnboarding, onb orgapi.Onboarding, profile Profile, module string) error {
	for _, m := range onb.Modules {
		if m.Module == module && m.State == orgapi.ModuleApproved {
			return nil
		}
	}
	id, lower := onb.OnboardingID, strings.ToLower(module)
	changeID := "onb-" + id + "-" + lower
	command := func(step string) string { return "onb-" + id + "-" + lower + "-" + step }
	consultant := identity.WithPrincipal(ctx, identity.Principal{TenantID: cmd.TenantID, ActorID: onb.ProposedBy,
		Permissions: []string{lower + ":ConfigureSettings"}})
	declarer := identity.WithPrincipal(ctx, identity.Principal{TenantID: cmd.TenantID, ActorID: cmd.ActorID, Permissions: []string{lower + ":DeclareSingleAdministrator"}})
	finAsked, seaAsked, err := moduleEntries(profile, module, onb.Overrides)
	if err != nil {
		return err
	}
	reason := fmt.Sprintf("customer onboarding %s: profile %s", id, profile.ID)
	var version int
	if module == ModuleFIN {
		if onb.SingleAdministrator {
			if _, err = o.fin.DeclareFinSingleAdministrator(declarer, finapi.DeclareFinSingleAdministrator{CommandID: command("declare"), TenantID: cmd.TenantID,
				ActorID: cmd.ActorID, OnboardingID: id, ProposedBy: onb.ProposedBy}); err != nil {
				return err
			}
		}
		r, err := o.fin.ProposeFinSettingsChange(consultant, finapi.ProposeFinSettingsChange{CommandID: command("propose"), TenantID: cmd.TenantID, ActorID: onb.ProposedBy,
			ChangeID: changeID, Entries: finAsked, Reason: reason})
		if err != nil {
			return err
		}
		version = r.Version
	} else {
		if onb.SingleAdministrator {
			if _, err = o.sea.DeclareSeaSingleAdministrator(declarer, seaapi.DeclareSeaSingleAdministrator{CommandID: command("declare"), TenantID: cmd.TenantID,
				ActorID: cmd.ActorID, OnboardingID: id, ProposedBy: onb.ProposedBy}); err != nil {
				return err
			}
		}
		r, err := o.sea.ProposeSeaSettingsChange(consultant, seaapi.ProposeSeaSettingsChange{CommandID: command("propose"), TenantID: cmd.TenantID, ActorID: onb.ProposedBy,
			ChangeID: changeID, Entries: seaAsked, Reason: reason})
		if err != nil {
			return err
		}
		version = r.SettingsVersion
	}
	record := func(step, state string) error {
		_, err := o.org.RecordOnboardingModule(ctx, orgapi.RecordOnboardingModule{CommandID: command("record-" + step), TenantID: cmd.TenantID, ActorID: cmd.ActorID,
			OnboardingID: id, Module: module, ChangeID: changeID, State: state})
		return err
	}
	if err = record("proposed", orgapi.ModuleProposed); err != nil {
		return err
	}
	if version == 0 { // the change set loosens something: the owner approves it, as a second person
		if module == ModuleFIN {
			_, err = o.fin.ApproveFinSettingsChange(ctx, finapi.ApproveFinSettingsChange{CommandID: command("approve"), TenantID: cmd.TenantID, ActorID: cmd.ActorID, ChangeID: changeID})
		} else {
			_, err = o.sea.ApproveSeaSettingsChange(ctx, seaapi.ApproveSeaSettingsChange{CommandID: command("approve"), TenantID: cmd.TenantID, ActorID: cmd.ActorID, ChangeID: changeID})
		}
		if err != nil {
			return err
		}
	}
	return record("approved", orgapi.ModuleApproved)
}
