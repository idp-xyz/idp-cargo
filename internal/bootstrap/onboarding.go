package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/fin"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/sea"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
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
// confirmed onboarding, except one that holds that permission by name. The platform records each module's progress in
// ORG as the owner with org:RecordOnboardingModule alone, by name only, which only the principal built here holds (the
// authorizer asks for the platform's mark on the context as well, which nothing outside this package can set).
//
// What the trail says, and what it cannot. What is applied is what the consultant proposed: ProposeOnboarding resolves
// the profile and the overrides into each module's entries, with the module's catalogue version, and ORG keeps them; the
// confirmation applies exactly those, and refuses a module whose catalogue is another version by then. The platform
// still proposes each module's change set in the consultant's name, at the owner's confirmation, with a permission it
// gives itself: there is no user directory, so the consultant's permissions can be checked only when they propose, and are
// not checked again at the confirmation. Each change set's reason says so: it names the onboarding, the consultant and the
// time of their proposal, and the owner and the time of the confirmation.

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

// ErrOnboardingIncomplete says that a confirmed onboarding did not reach every module and that sending the same
// confirmation again can cure it; the confirmation says which module and why.
var ErrOnboardingIncomplete = errors.New("bootstrap: the onboarding is confirmed but a module is not applied yet")

// ErrOnboardingBlocked says that a module of a confirmed onboarding can never be applied by sending the same confirmation
// again (its change set's id or a command id is taken by another change, its change set was refused or cancelled, or its
// catalogue is another version than the proposal's): the way out is to propose a new onboarding under another id.
var ErrOnboardingBlocked = errors.New("bootstrap: the onboarding is confirmed but a module cannot be applied by sending the same confirmation again")

// ModuleFailure is a module that did not reach APPROVED, and why. Permanent says that sending the same confirmation again
// cannot cure it.
type ModuleFailure struct {
	Module    string
	Err       error
	Permanent bool
}

// OnboardingConfirmation is the answer to ConfirmOnboarding: ORG's receipt and onboarding, and the modules that failed.
// The receipt's notice says on whose proposal the platform acts (see Trail).
type OnboardingConfirmation struct {
	Receipt    orgapi.OnboardingReceipt
	Onboarding orgapi.Onboarding
	Failures   []ModuleFailure
}

type platformRecorder struct{}

// recordingAsPlatform is ctx for the platform's own record of a module's progress in ORG: the owner's actor with the one
// permission, and the mark that the authorizer asks for.
func recordingAsPlatform(ctx context.Context, tenant, actor string) context.Context {
	p := identity.Principal{TenantID: tenant, ActorID: actor, Permissions: []string{"org:" + orgapi.ActionRecordOnboardingModule}}
	return context.WithValue(identity.WithPrincipal(ctx, p), platformRecorder{}, true)
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

func orgEntries(in []onboardingEntry) []orgapi.OnboardingEntry {
	out := make([]orgapi.OnboardingEntry, len(in))
	for i, e := range in {
		out[i] = orgapi.OnboardingEntry(e)
	}
	return out
}

// moduleEntries answers the entries that the profile and the overrides make for a module, as the module's own entries.
func moduleEntries(profile Profile, module string, overrides map[string][]orgapi.OnboardingEntry) ([]finapi.FinSettingEntry, []seaapi.SeaSettingEntry, error) {
	return storedEntries(module, profileEntries(profile, module, entriesOfOnboarding(overrides[module])))
}

// storedEntries answers entries, resolved or stored, as the module's own entries.
func storedEntries(module string, merged []onboardingEntry) ([]finapi.FinSettingEntry, []seaapi.SeaSettingEntry, error) {
	if module == ModuleFIN {
		f, err := finEntries(merged)
		return f, nil, err
	}
	s, err := seaEntries(merged)
	return nil, s, err
}

// catalogueVersion is the version of a module's catalogue in this build.
func (o *Onboarding) catalogueVersion(ctx context.Context, module string) (string, error) {
	if module == ModuleFIN {
		c, err := o.fin.GetFinSettingsCatalogue(ctx, finapi.GetFinSettingsCatalogue{})
		return c.Version, err
	}
	c, err := o.sea.GetSeaSettingsCatalogue(ctx, seaapi.GetSeaSettingsCatalogue{})
	return c.Version, err
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

// resolve answers the plan of a proposal: each module's entries from the profile and the overrides, checked with the
// modules' own rules, with the module's catalogue version.
func (o *Onboarding) resolve(ctx context.Context, profile Profile, overrides map[string][]orgapi.OnboardingEntry) (map[string]orgapi.ModulePlan, error) {
	if err := checkModules(profile, overrides); err != nil {
		return nil, err
	}
	plan := map[string]orgapi.ModulePlan{}
	for _, module := range []string{ModuleFIN, ModuleSEA} {
		version, err := o.catalogueVersion(ctx, module)
		if err != nil {
			return nil, err
		}
		plan[module] = orgapi.ModulePlan{CatalogueVersion: version, Entries: orgEntries(profileEntries(profile, module, entriesOfOnboarding(overrides[module])))}
	}
	return plan, nil
}

// maxReason is the longest reason of a change set (settings.MaxReason, 500 characters).
const maxReason = 500

// onboardingReason is the reason of each module's change set: it names the onboarding, the consultant and the time of
// their proposal, and the owner and the time of the confirmation, and says that the platform acts on the proposal.
func onboardingReason(onboarding, consultant string, proposedAt time.Time, owner string, confirmedAt time.Time) string {
	return fmt.Sprintf("onboarding %s: proposed by %s at %s, confirmed by %s at %s; the platform applies the proposal, the consultant's permissions checked when they proposed",
		onboarding, consultant, proposedAt.UTC().Format(time.RFC3339), owner, confirmedAt.UTC().Format(time.RFC3339))
}

// ProposeOnboarding is the consultant's proposal: it resolves the profile and the overrides into each module's entries,
// checks them with FIN's and SEA's own rules, so that an invalid one is refused now and not when the owner confirms, and
// records the proposal, with those entries and the modules' catalogue versions, in ORG. The receipt carries the notice
// of a STRICT profile with fewer than three declared finance staff.
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
	// the reason of each change set names the actors and times and must fit the 500 characters a change set's reason has, whoever the owner is (up to 128 bytes)
	if n := len(onboardingReason(cmd.OnboardingID, cmd.ActorID, time.Time{}, strings.Repeat("x", 128), time.Time{})); n > maxReason {
		return orgapi.OnboardingReceipt{}, fmt.Errorf("%w: the onboarding id and the consultant's actor id are too long for the reason of the modules' change sets, which names them (%d of %d characters); "+
			"propose under a shorter onboarding id", orgapi.ErrRejected, n, maxReason)
	}
	plan, err := o.resolve(ctx, profile, cmd.Overrides)
	if err != nil {
		return orgapi.OnboardingReceipt{}, err
	}
	cmd.Plan = plan
	return o.org.ProposeOnboarding(ctx, cmd)
}

// ConfirmOnboarding is the owner's confirmation, with their declaration of the customer's single administrator when the
// proposal says there is one. It records it in ORG, then applies each module (FIN, then SEA) exactly as the proposal
// stored it: a single administrator is declared when the onboarding says so, the module's change set is proposed as the
// consultant and, when it loosens something, approved as the owner. A module that fails does not stop the other; the
// answer lists what failed. When sending the same command again can cure it the error is ErrOnboardingIncomplete; when it
// cannot, ErrOnboardingBlocked, and the way out is a new onboarding under another id.
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
	receipt.Notice = joinNotice(receipt.Notice, "the platform applies the proposal of "+onb.ProposedBy+" made at "+onb.ProposedAt.UTC().Format(time.RFC3339)+
		", as confirmed by "+onb.ConfirmedBy+" at "+onb.ConfirmedAt.UTC().Format(time.RFC3339)+"; the consultant's permissions were checked when they proposed, not at the confirmation")
	out := OnboardingConfirmation{Receipt: receipt, Onboarding: onb}
	for _, module := range []string{ModuleFIN, ModuleSEA} {
		if err := o.applyModule(ctx, cmd, onb, module); err != nil {
			var p permanent
			out.Failures = append(out.Failures, ModuleFailure{Module: module, Err: err, Permanent: errors.As(err, &p)})
		}
	}
	if onb, err = o.org.GetOnboarding(ctx, orgapi.GetOnboarding{TenantID: cmd.TenantID, OnboardingID: cmd.OnboardingID}); err != nil {
		return out, err
	}
	out.Onboarding, out.Receipt.State = onb, onb.State
	if len(out.Failures) == 0 {
		return out, nil
	}
	var parts, blocked, retry []string
	for _, f := range out.Failures {
		parts = append(parts, fmt.Sprintf("%s: %v", f.Module, f.Err))
		if f.Permanent {
			blocked = append(blocked, f.Module)
		} else {
			retry = append(retry, f.Module)
		}
	}
	if len(blocked) == 0 {
		return out, fmt.Errorf("%w (%s); fix the cause and send the same ConfirmOnboarding again, which completes the module without making a second change set",
			ErrOnboardingIncomplete, strings.Join(parts, "; "))
	}
	more := ""
	if len(retry) > 0 {
		more = "; " + strings.Join(retry, " and ") + " is completed by sending the same ConfirmOnboarding again once its cause is fixed"
	}
	return out, fmt.Errorf("%w (%s); sending the same ConfirmOnboarding again cannot cure %s: propose a new onboarding under another id%s",
		ErrOnboardingBlocked, strings.Join(parts, "; "), strings.Join(blocked, " and "), more)
}

func joinNotice(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// permanent marks a failure that sending the same confirmation again cannot cure; its text says the way out.
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

const newOnboarding = "propose a new onboarding under another id (ProposeOnboarding with a new OnboardingID), and have the owner confirm that"

// settingsChange is a module's change set as the onboarding reads it.
type settingsChange struct {
	Status     string
	ProposedBy string
	Reason     string
	Entries    []onboardingEntry
}

// findChange reads the module's own change set by its id.
func (o *Onboarding) findChange(ctx context.Context, module, tenant, id string) (settingsChange, bool, error) {
	if module == ModuleFIN {
		list, err := o.fin.ListFinSettingsChanges(ctx, finapi.ListFinSettingsChanges{TenantID: tenant})
		if err != nil {
			return settingsChange{}, false, err
		}
		for _, c := range list {
			if c.ChangeID == id {
				out := settingsChange{Status: c.Status, ProposedBy: c.ProposedBy, Reason: c.Reason}
				for _, e := range c.Entries {
					out.Entries = append(out.Entries, onboardingEntry{ScopeKind: e.ScopeKind, ScopeID: e.ScopeID, Key: e.Key, Value: e.Value, ThresholdMinor: e.ThresholdMinor})
				}
				return out, true, nil
			}
		}
		return settingsChange{}, false, nil
	}
	list, err := o.sea.ListSeaSettingsChanges(ctx, seaapi.ListSeaSettingsChanges{TenantID: tenant})
	if err != nil {
		return settingsChange{}, false, err
	}
	for _, c := range list {
		if c.ChangeID == id {
			out := settingsChange{Status: c.Status, ProposedBy: c.ProposedBy, Reason: c.Reason}
			for _, e := range c.Entries {
				out.Entries = append(out.Entries, onboardingEntry{ScopeKind: e.ScopeKind, ScopeID: e.ScopeID, Key: e.Key, Value: e.Value})
			}
			return out, true, nil
		}
	}
	return settingsChange{}, false, nil
}

// sameEntries says that a module's change set has the stored entries and no others, in any order.
func sameEntries(stored, got []onboardingEntry) bool {
	key := func(e onboardingEntry) string {
		t := "-"
		if e.ThresholdMinor != nil {
			t = fmt.Sprint(*e.ThresholdMinor)
		}
		return strings.Join([]string{e.ScopeKind, e.ScopeID, e.Key, e.Value, t}, "\x00")
	}
	var a, b []string
	for _, e := range stored {
		a = append(a, key(e))
	}
	for _, e := range got {
		b = append(b, key(e))
	}
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// asPermanent marks an error of a module's own command as one that sending the same confirmation again cannot cure when
// it says that a command id is reused with another payload, or that the change set was refused or cancelled.
func asPermanent(module string, err error) error {
	switch {
	case errors.Is(err, finapi.ErrIdempotencyConflict), errors.Is(err, seaapi.ErrIdempotencyConflict):
		return permanent{fmt.Errorf("a command id the onboarding derived for %s is taken by another command (%w); %s", module, err, newOnboarding)}
	case errors.Is(err, settings.ErrCancelled):
		return permanent{fmt.Errorf("the change set of %s was cancelled or refused (%w); %s", module, err, newOnboarding)}
	}
	return err
}

func hasModuleState(modules []orgapi.OnboardingModule, module, state string) bool {
	return slices.ContainsFunc(modules, func(m orgapi.OnboardingModule) bool { return m.Module == module && m.State == state })
}

// applyModule takes one module to APPROVED from the entries the proposal stored. Whether the module is done is decided
// from the module's own change set (found by its derived id), not from ORG's record, which is written afterwards when it
// is missing: so a module half done is finished, a crash between the module's approval and ORG's record is completed by a
// retry, and nothing is made twice. Each step is idempotent by its derived command id.
func (o *Onboarding) applyModule(ctx context.Context, cmd orgapi.ConfirmOnboarding, onb orgapi.Onboarding, module string) error {
	id, lower := onb.OnboardingID, strings.ToLower(module)
	changeID := "onb-" + id + "-" + lower
	command := func(step string) string { return "onb-" + id + "-" + lower + "-" + step }
	plan, ok := onb.Plan[module]
	if !ok {
		return permanent{fmt.Errorf("the onboarding stores no plan for %s; %s", module, newOnboarding)}
	}
	current, err := o.catalogueVersion(ctx, module)
	if err != nil {
		return err
	}
	if current != plan.CatalogueVersion {
		return permanent{fmt.Errorf("the catalogue of %s is version %s now and the proposal was resolved against version %s, so what the owner confirmed is not what %s would apply now; %s",
			module, current, plan.CatalogueVersion, module, newOnboarding)}
	}
	reason := onboardingReason(id, onb.ProposedBy, onb.ProposedAt, onb.ConfirmedBy, onb.ConfirmedAt)
	stored := entriesOfOnboarding(plan.Entries)
	change, found, err := o.findChange(ctx, module, cmd.TenantID, changeID)
	if err != nil {
		return err
	}
	if found {
		switch {
		case change.ProposedBy != onb.ProposedBy || change.Reason != reason || !sameEntries(stored, change.Entries):
			return permanent{fmt.Errorf("change set %s of %s exists and is not this onboarding's (it was proposed by %s with another reason or other entries); %s", changeID, module,
				change.ProposedBy, newOnboarding)}
		case change.Status == "CANCELLED" || change.Status == "REFUSED":
			return permanent{fmt.Errorf("change set %s of %s was %s; %s", changeID, module, strings.ToLower(change.Status), newOnboarding)}
		}
	}
	finAsked, seaAsked, err := storedEntries(module, stored)
	if err != nil {
		return err
	}
	declarer := identity.WithPrincipal(ctx, identity.Principal{TenantID: cmd.TenantID, ActorID: cmd.ActorID, Permissions: []string{lower + ":DeclareSingleAdministrator"}})
	consultant := identity.WithPrincipal(ctx, identity.Principal{TenantID: cmd.TenantID, ActorID: onb.ProposedBy, Permissions: []string{lower + ":ConfigureSettings"}})
	if onb.SingleAdministrator {
		if module == ModuleFIN {
			_, err = o.fin.DeclareFinSingleAdministrator(declarer, finapi.DeclareFinSingleAdministrator{CommandID: command("declare"), TenantID: cmd.TenantID,
				ActorID: cmd.ActorID, OnboardingID: id, ProposedBy: onb.ProposedBy, AdministratorID: onb.AdministratorID})
		} else {
			_, err = o.sea.DeclareSeaSingleAdministrator(declarer, seaapi.DeclareSeaSingleAdministrator{CommandID: command("declare"), TenantID: cmd.TenantID,
				ActorID: cmd.ActorID, OnboardingID: id, ProposedBy: onb.ProposedBy, AdministratorID: onb.AdministratorID})
		}
		if err != nil {
			return asPermanent(module, err)
		}
	}
	if !found {
		if module == ModuleFIN {
			_, err = o.fin.ProposeFinSettingsChange(consultant, finapi.ProposeFinSettingsChange{CommandID: command("propose"), TenantID: cmd.TenantID, ActorID: onb.ProposedBy,
				ChangeID: changeID, Entries: finAsked, Reason: reason})
		} else {
			_, err = o.sea.ProposeSeaSettingsChange(consultant, seaapi.ProposeSeaSettingsChange{CommandID: command("propose"), TenantID: cmd.TenantID, ActorID: onb.ProposedBy,
				ChangeID: changeID, Entries: seaAsked, Reason: reason})
		}
		if err != nil {
			return asPermanent(module, err)
		}
		if change, found, err = o.findChange(ctx, module, cmd.TenantID, changeID); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("change set %s of %s was proposed and is not listed; send the same ConfirmOnboarding again", changeID, module)
		}
	}
	record := func(step, state string) error {
		_, err := o.org.RecordOnboardingModule(recordingAsPlatform(ctx, cmd.TenantID, cmd.ActorID), orgapi.RecordOnboardingModule{CommandID: command("record-" + step), TenantID: cmd.TenantID,
			ActorID: cmd.ActorID, OnboardingID: id, Module: module, ChangeID: changeID, State: state})
		return err
	}
	if !hasModuleState(onb.Modules, module, orgapi.ModuleProposed) {
		if err = record("proposed", orgapi.ModuleProposed); err != nil {
			return err
		}
	}
	if change.Status == "PROPOSED" { // the change set loosens something: the owner approves it, as a second person
		if module == ModuleFIN {
			_, err = o.fin.ApproveFinSettingsChange(ctx, finapi.ApproveFinSettingsChange{CommandID: command("approve"), TenantID: cmd.TenantID, ActorID: cmd.ActorID, ChangeID: changeID})
		} else {
			_, err = o.sea.ApproveSeaSettingsChange(ctx, seaapi.ApproveSeaSettingsChange{CommandID: command("approve"), TenantID: cmd.TenantID, ActorID: cmd.ActorID, ChangeID: changeID})
		}
		if err != nil {
			return asPermanent(module, err)
		}
	}
	if !hasModuleState(onb.Modules, module, orgapi.ModuleApproved) {
		return record("approved", orgapi.ModuleApproved)
	}
	return nil
}
