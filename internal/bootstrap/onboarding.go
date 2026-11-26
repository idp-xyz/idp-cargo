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

// Customer onboarding (V2.102, design section 9 items 2, 9 and 10). ORG holds the record; the bootstrap, which composes the
// contexts, applies it, in the identity of the people actually signed in (batch F). ProposeOnboarding is the consultant's: it
// records the proposal in ORG, then calls FIN's and SEA's own propose commands with the consultant's own context (their own
// principal and permissions, not one the platform builds), each holding its change set for the onboarding: the change set is not in
// force, and not approved even when it only tightens. ConfirmOnboarding is the customer's owner's: it records the confirmation in ORG,
// then calls each module's own approve command with the owner's own context, which approves the held change set (the owner is not the
// consultant, so the usual rule that the approver is not the proposer holds). WithdrawOnboarding withdraws an onboarding that is not
// confirmed, by the consultant or the owner, and cancels the held change sets with the withdrawing person's own context. Each module commits on
// its own, with its change set named from the onboarding and its commands' ids derived from it, so that a retry of the same command
// completes a module that did not and never makes a second change set. ORG's record keeps each module's change set, its state and the
// settings version the module put in force.
//
// Permissions. The consultant needs org:ProposeOnboarding, fin:ConfigureSettings and sea:ConfigureSettings when proposing, the owner
// org:ConfirmOnboarding, fin:ApproveSettings and sea:ApproveSettings when confirming (all by name only, as the commands ask them),
// and whoever withdraws needs their org permission and, for each module, ConfigureSettings or ApproveSettings, which the
// modules' cancel commands ask. The modules ask nothing new of the people. What is the platform's own needs its mark on the context,
// which nothing outside this package can set and no token carries (the authorizer asks for it besides the permission by name):
// ORG's own ProposeOnboarding, ConfirmOnboarding and WithdrawOnboarding (the only way in is this onboarding, which checks the plan
// and the person, and checks the person's org permission by name before it calls ORG), ORG's record of each module's progress
// (org:RecordOnboardingModule alone, held only by the principal built here for it), the declaration of a single administrator in FIN and SEA
// (as the owner, with exactly the two DeclareSingleAdministrator permissions), and the holding and the approval of a change set
// for an onboarding (the marked context is the person's own: their principal and permissions stay, and the authorizer asks for their
// ConfigureSettings, or ApproveSettings, by name besides the mark, so that no person can freeze a tenant's settings with a made-up
// onboarding or approve a held change set outside the confirmation).
//
// What the trail says. What is applied is what the consultant proposed: ProposeOnboarding resolves the profile and the overrides
// into each module's entries, with the module's catalogue version, and ORG keeps them; the confirmation approves exactly those, and
// refuses, before it records anything, a module whose catalogue is another version by then, whose change set is missing, is not
// the onboarding's or has no longer its stored entries. Each module's change set is proposed by the consultant and approved by the
// owner: its proposer, its approver, their times and its settings version are the module's own records, and its reason names the
// onboarding, the consultant and the time of their proposal.

// ORGOnboarding is what the onboarding uses of ORG.
type ORGOnboarding interface {
	orgapi.OnboardingCommands
	orgapi.OnboardingQueries
}

// FINSettings is what the onboarding and the settings tools use of FIN's settings.
type FINSettings interface {
	ProposeFinSettingsChange(ctx context.Context, cmd finapi.ProposeFinSettingsChange) (finapi.Receipt, error)
	ApproveFinSettingsChange(ctx context.Context, cmd finapi.ApproveFinSettingsChange) (finapi.Receipt, error)
	CancelFinSettingsChange(ctx context.Context, cmd finapi.CancelFinSettingsChange) (finapi.Receipt, error)
	DeclareFinSingleAdministrator(ctx context.Context, cmd finapi.DeclareFinSingleAdministrator) (finapi.Receipt, error)
	GetFinSettings(ctx context.Context, q finapi.GetFinSettings) (finapi.FinSettings, error)
	ListFinSettingsChanges(ctx context.Context, q finapi.ListFinSettingsChanges) ([]finapi.FinSettingsChange, error)
	GetFinSettingsCatalogue(ctx context.Context, q finapi.GetFinSettingsCatalogue) (finapi.FinSettingsCatalogue, error)
	GetFinExceptionReport(ctx context.Context, q finapi.GetFinExceptionReport) (finapi.FinExceptionReport, error)
	ListFinSelfApprovalReviews(ctx context.Context, q finapi.ListFinSelfApprovalReviews) (finapi.FinSelfApprovalReviews, error)
	ListAfterTheFactPending(ctx context.Context, q finapi.ListAfterTheFactPending) (finapi.AfterTheFactPending, error)
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

// ErrOnboardingIncomplete says that an onboarding step did not reach every module and that sending the same command again (the
// proposal, the confirmation or the withdrawal) can cure it; the answer says which module and why.
var ErrOnboardingIncomplete = errors.New("bootstrap: the onboarding is recorded but a module is not done yet")

// ErrOnboardingBlocked says that a module of an onboarding can never be completed by sending the same command again (its change set's
// id or a command id is taken by another change, its change set is not the onboarding's, or it was refused or cancelled): the way
// out is to propose a new onboarding under another id. A build that is not the proposal's is not in this list: the build the plan was
// made with cures it (ErrOnboardingBuild, which is curable).
var ErrOnboardingBlocked = errors.New("bootstrap: the onboarding is recorded but a module cannot be completed by sending the same command again")

// ModuleFailure is a module that did not reach the step asked, and why. Permanent says that sending the same command again cannot cure it.
type ModuleFailure struct {
	Module    string
	Err       error
	Permanent bool
}

// OnboardingConfirmation is the answer to ConfirmOnboarding: ORG's receipt and onboarding, and the modules that failed.
// The receipt's notice says by whom each module's change set was proposed and approved (see Trail).
type OnboardingConfirmation struct {
	Receipt    orgapi.OnboardingReceipt
	Onboarding orgapi.Onboarding
	Failures   []ModuleFailure
}

type platformRecorder struct{}

// recordingAsPlatform is ctx for the platform's own record of a module's progress in ORG: the actor with the one permission, and the
// mark that the authorizer asks for. The actor is the proposer for the PROPOSED row and the confirmer for the APPROVED one.
func recordingAsPlatform(ctx context.Context, tenant, actor string) context.Context {
	return platformPrincipal(ctx, tenant, actor, "org:"+orgapi.ActionRecordOnboardingModule)
}

// platformPrincipal is ctx for the platform acting as the actor with the one permission, and the platform's mark.
func platformPrincipal(ctx context.Context, tenant, actor, permission string) context.Context {
	p := identity.Principal{TenantID: tenant, ActorID: actor, Permissions: []string{permission}}
	return context.WithValue(identity.WithPrincipal(ctx, p), platformRecorder{}, true)
}

// asPlatform marks a person's own ctx as the platform's, for what only the onboarding does: the person's principal stays (their
// own identity and permissions), and the authorizer still asks for their permission by name besides the mark.
func asPlatform(ctx context.Context) context.Context {
	return context.WithValue(ctx, platformRecorder{}, true)
}

// ErrInvalidActor is the refusal of an actor id (or the administrator's) that is blank, has spaces around it or is longer
// than 128 bytes: no module accepts it, so it is refused before anything is called, and nothing is recorded.
var ErrInvalidActor = errors.New("bootstrap: ONBOARDING_INVALID_ACTOR")

// ErrOnboardingBuild says that this build is not the one the onboarding was proposed with: a catalogue's version differs
// from the plan's, or this build's rules refuse the plan's entries. Before the confirmation nothing is recorded; after it,
// the confirmation stands and the same ConfirmOnboarding completes the module once the build is the proposal's (and the same ProposeOnboarding, for a module that
// has not held its change set yet).
var ErrOnboardingBuild = errors.New("bootstrap: ONBOARDING_BUILD_DIFFERS")

func checkActor(what, id string, optional bool) error {
	if optional && id == "" {
		return nil
	}
	if !domainValidActor(id) {
		return fmt.Errorf("%w: %w: %s %q is blank, has spaces around it or is longer than 128 bytes (%d); give the actor id exactly as the identity platform names the person, "+
			"and nothing is recorded", orgapi.ErrRejected, ErrInvalidActor, what, id, len(id))
	}
	return nil
}

func domainValidActor(id string) bool {
	return strings.TrimSpace(id) != "" && strings.TrimSpace(id) == id && len(id) <= 128
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

// buildDiffers says why this build is not the one a module's plan was made with, nil when it is: the module's catalogue is
// another version, or this build's own rules refuse the plan's entries (a rule that changed under the same version).
func (o *Onboarding) buildDiffers(ctx context.Context, module string, plan orgapi.ModulePlan) error {
	current, err := o.catalogueVersion(ctx, module)
	if err != nil {
		return err
	}
	if current != plan.CatalogueVersion {
		return fmt.Errorf("the catalogue of %s is version %s in this build and the proposal was resolved against version %s, so what the owner reads is not what %s would apply here",
			module, current, plan.CatalogueVersion, module)
	}
	f, s, err := storedEntries(module, entriesOfOnboarding(plan.Entries))
	if err == nil && module == ModuleFIN {
		err = fin.CheckSettingsEntries(f)
	} else if err == nil {
		err = sea.CheckSettingsEntries(s)
	}
	if err != nil {
		return fmt.Errorf("this build's rules of %s refuse the entries of the plan, which was resolved by a build that accepted them (%v)", module, err)
	}
	return nil
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

// heldReason is the reason of each module's change set: it names the onboarding, the consultant and the time of their proposal, and says that the
// change set is held until the owner confirms the onboarding, which approves it. The owner and the time of the confirmation are the approval's own
// record in the module (who approved it, when and as which settings version).
func heldReason(onboarding, consultant string, proposedAt time.Time) string {
	return fmt.Sprintf("onboarding %s: proposed by %s at %s; held until the customer's owner confirms the onboarding, which approves it",
		onboarding, consultant, proposedAt.UTC().Format(time.RFC3339))
}

// withdrawReason is the reason of the cancellation of a held change set by a withdrawal; the reason the person gave is in ORG's record.
func withdrawReason(onboarding, by string, at time.Time) string {
	return fmt.Sprintf("onboarding %s withdrawn by %s at %s; the reason is in the onboarding's record", onboarding, by, at.UTC().Format(time.RFC3339))
}

// ProposeOnboarding is the consultant's proposal, made in the consultant's own identity: it resolves the profile and the overrides into
// each module's entries, checks them with FIN's and SEA's own rules, so that an invalid one is refused now and not when the owner confirms,
// records the proposal, with those entries and the modules' catalogue versions, in ORG, and then proposes each module's change set with
// that module's own propose command and the consultant's own context, held for the onboarding: not in force, not approved even when it only
// tightens. The receipt carries the notice of a STRICT profile with fewer than three declared finance staff. The same command again
// completes a module that did not propose and makes nothing twice; when sending it again cannot cure a module, the error is
// ErrOnboardingBlocked, and the way out is to propose a new onboarding under another id. While another onboarding of the tenant waits, none
// is proposed: confirm or withdraw it first.
func (o *Onboarding) ProposeOnboarding(ctx context.Context, cmd orgapi.ProposeOnboarding) (orgapi.OnboardingReceipt, error) {
	if err := checkActor("the consultant's actor id", cmd.ActorID, false); err != nil {
		return orgapi.OnboardingReceipt{}, err
	}
	if err := holdsNamed(ctx, "org", cmd.TenantID, cmd.ActorID, orgapi.ActionProposeOnboarding); err != nil {
		return orgapi.OnboardingReceipt{}, fmt.Errorf("%w: %v; the consultant holds org:ProposeOnboarding by name", orgapi.ErrUnauthorized, err)
	}
	if err := holdsNamed(ctx, "fin", cmd.TenantID, cmd.ActorID, finapi.ActionConfigureSettings); err != nil {
		return orgapi.OnboardingReceipt{}, fmt.Errorf("%w: %v; the consultant proposes the modules' change sets in their own name, so the consultant holds fin:ConfigureSettings by name", orgapi.ErrUnauthorized, err)
	}
	if err := holdsNamed(ctx, "sea", cmd.TenantID, cmd.ActorID, seaapi.ActionConfigureSettings); err != nil {
		return orgapi.OnboardingReceipt{}, fmt.Errorf("%w: %v; the consultant proposes the modules' change sets in their own name, so the consultant holds sea:ConfigureSettings by name", orgapi.ErrUnauthorized, err)
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
	// no change set is proposed while one is held for an onboarding that is neither confirmed nor withdrawn: refuse it before anything is recorded
	others, err := o.org.ListOnboardings(ctx, orgapi.ListOnboardings{TenantID: cmd.TenantID})
	if err != nil {
		return orgapi.OnboardingReceipt{}, err
	}
	for _, other := range others {
		if other.OnboardingID != cmd.OnboardingID && other.State == orgapi.OnboardingStateProposed {
			return orgapi.OnboardingReceipt{}, fmt.Errorf("%w: onboarding %s of this tenant waits for its owner, and while it waits no other change is proposed or approved in FIN or SEA; "+
				"have the customer's owner confirm it (ConfirmOnboarding) or withdraw it (WithdrawOnboarding), then propose this one again", orgapi.ErrRejected, other.OnboardingID)
		}
	}
	plan, err := o.resolve(ctx, profile, cmd.Overrides)
	if err != nil {
		return orgapi.OnboardingReceipt{}, err
	}
	cmd.Plan = plan
	receipt, err := o.org.ProposeOnboarding(asPlatform(ctx), cmd)
	if err != nil {
		return receipt, err
	}
	onb, err := o.org.GetOnboarding(ctx, orgapi.GetOnboarding{TenantID: cmd.TenantID, OnboardingID: cmd.OnboardingID})
	if err != nil {
		return receipt, err
	}
	if onb.State != orgapi.OnboardingStateProposed { // a replay of a proposal that was confirmed or withdrawn since: there is nothing to hold
		return receipt, nil
	}
	var failures []ModuleFailure
	for _, module := range []string{ModuleFIN, ModuleSEA} {
		if err := o.holdModule(ctx, onb, module); err != nil {
			var p permanent
			failures = append(failures, ModuleFailure{Module: module, Err: err, Permanent: errors.As(err, &p)})
		}
	}
	receipt.Notice = joinNotice(receipt.Notice, "the platform proposed FIN's and SEA's change sets in "+cmd.ActorID+"'s own name, held for the onboarding: they are not in force, and not approved "+
		"until the customer's owner confirms it (ConfirmOnboarding), which approves them in the owner's own name; while they wait no other change is proposed or approved in FIN or SEA")
	return receipt, stepError("ProposeOnboarding", failures)
}

// stepError answers the error of an onboarding step for the modules that failed it: nil when none did; ErrOnboardingIncomplete when sending the
// same command again can cure each; ErrOnboardingBlocked when it cannot cure one of them, naming the way out, a new onboarding under another id.
func stepError(command string, failures []ModuleFailure) error {
	if len(failures) == 0 {
		return nil
	}
	var parts, blocked, retry []string
	for _, f := range failures {
		parts = append(parts, fmt.Sprintf("%s: %v", f.Module, f.Err))
		if f.Permanent {
			blocked = append(blocked, f.Module)
		} else {
			retry = append(retry, f.Module)
		}
	}
	if len(blocked) == 0 {
		return fmt.Errorf("%w (%s); fix the cause and send the same %s again, which completes the module without making a second change set",
			ErrOnboardingIncomplete, strings.Join(parts, "; "), command)
	}
	more := ""
	if len(retry) > 0 {
		more = "; " + strings.Join(retry, " and ") + " is completed by sending the same " + command + " again once its cause is fixed"
	}
	return fmt.Errorf("%w (%s); sending the same %s again cannot cure %s: propose a new onboarding under another id%s",
		ErrOnboardingBlocked, strings.Join(parts, "; "), command, strings.Join(blocked, " and "), more)
}

// ConfirmOnboarding is the owner's confirmation of the plan they read (the SHA-256 of the plan they were shown), with their
// declaration of the customer's single administrator when the proposal says there is one, in the owner's own identity. Before anything is
// called it refuses an actor id or an administrator's that no module accepts (ErrInvalidActor), and before ORG records the confirmation it
// refuses a build that is not the one the plan was made with (ErrOnboardingBuild) and a module whose change set is not held as the proposal
// stored it: missing, not the onboarding's, with other entries, or no longer waiting (ErrOnboardingHeld): nothing is recorded in either case. It
// records the confirmation in ORG, then takes each module (FIN, then SEA) to APPROVED: a single administrator is declared when the onboarding says
// so, the module's held change set is approved with its own approve command and the owner's own context, and ORG records the module's
// approval with the settings version it made. A module that fails does not stop the other; the answer lists what failed. When sending the same
// command again can cure it the error is ErrOnboardingIncomplete; when it cannot, ErrOnboardingBlocked, and the way out is a new onboarding under another id.
func (o *Onboarding) ConfirmOnboarding(ctx context.Context, cmd orgapi.ConfirmOnboarding) (OnboardingConfirmation, error) {
	if err := checkActor("the owner's actor id", cmd.ActorID, false); err != nil {
		return OnboardingConfirmation{}, err
	}
	if err := checkActor("the administrator's actor id", cmd.AdministratorID, true); err != nil {
		return OnboardingConfirmation{}, err
	}
	if err := holdsNamed(ctx, "org", cmd.TenantID, cmd.ActorID, orgapi.ActionConfirmOnboarding); err != nil {
		return OnboardingConfirmation{}, fmt.Errorf("%w: %v; the owner holds org:ConfirmOnboarding by name", orgapi.ErrUnauthorized, err)
	}
	for _, bc := range []struct{ name, owns string }{{"fin", finapi.ActionApproveSettings}, {"sea", seaapi.ActionApproveSettings}} {
		if err := holdsNamed(ctx, bc.name, cmd.TenantID, cmd.ActorID, bc.owns); err != nil {
			return OnboardingConfirmation{}, fmt.Errorf("%w: %v; the owner approves the modules' change sets in their own name, so the owner holds %s:%s by name",
				orgapi.ErrUnauthorized, err, bc.name, bc.owns)
		}
	}
	// before ORG records the owner's confirmation, this build must be the one the plan was made with and each module must hold the change set the plan
	// stored: otherwise nothing is recorded and the owner can confirm again, with the build fixed, the change set completed, or a new onboarding
	before, err := o.org.GetOnboarding(ctx, orgapi.GetOnboarding{TenantID: cmd.TenantID, OnboardingID: cmd.OnboardingID})
	if err != nil {
		return OnboardingConfirmation{}, err
	}
	if before.State == orgapi.OnboardingStateProposed {
		for _, module := range []string{ModuleFIN, ModuleSEA} {
			if err := o.buildDiffers(ctx, module, before.Plan[module]); err != nil {
				return OnboardingConfirmation{}, fmt.Errorf("%w: %w: %v; nothing is recorded, the onboarding is still waiting for the owner: run the build the plan was made with and confirm again, "+
					"or have the consultant propose a new onboarding under another id", orgapi.ErrRejected, ErrOnboardingBuild, err)
			}
		}
		for _, module := range []string{ModuleFIN, ModuleSEA} {
			if err := o.heldAsProposed(ctx, before, module); err != nil {
				return OnboardingConfirmation{}, fmt.Errorf("%w: %w: %v; nothing is recorded, the onboarding is still waiting for the owner: the consultant sends the same ProposeOnboarding "+
					"again to complete a change set that is missing, or withdraw the onboarding (WithdrawOnboarding) and propose a new one under another id", orgapi.ErrRejected, ErrOnboardingHeld, err)
			}
		}
	}
	receipt, err := o.org.ConfirmOnboarding(asPlatform(ctx), cmd)
	if err != nil {
		return OnboardingConfirmation{}, err
	}
	onb, err := o.org.GetOnboarding(ctx, orgapi.GetOnboarding{TenantID: cmd.TenantID, OnboardingID: cmd.OnboardingID})
	if err != nil {
		return OnboardingConfirmation{}, err
	}
	receipt.Notice = joinNotice(receipt.Notice, "the platform approves the change sets that "+onb.ProposedBy+" proposed at "+onb.ProposedAt.UTC().Format(time.RFC3339)+", held for the onboarding, in "+
		onb.ConfirmedBy+"'s own name, at the confirmation at "+onb.ConfirmedAt.UTC().Format(time.RFC3339)+"; each module's approval says its version")
	out := OnboardingConfirmation{Receipt: receipt, Onboarding: onb}
	for _, module := range []string{ModuleFIN, ModuleSEA} {
		if err := o.approveModule(ctx, cmd, onb, module); err != nil {
			var p permanent
			out.Failures = append(out.Failures, ModuleFailure{Module: module, Err: err, Permanent: errors.As(err, &p)})
		}
	}
	if onb, err = o.org.GetOnboarding(ctx, orgapi.GetOnboarding{TenantID: cmd.TenantID, OnboardingID: cmd.OnboardingID}); err != nil {
		return out, err
	}
	out.Onboarding, out.Receipt.State = onb, onb.State
	return out, stepError("ConfirmOnboarding", out.Failures)
}

// ErrOnboardingHeld says that a module does not hold the change set that the onboarding's plan stored, as the owner would approve it: nothing is recorded.
var ErrOnboardingHeld = errors.New("bootstrap: ONBOARDING_HELD_CHANGE_SET")

// WithdrawOnboarding withdraws an onboarding that is not confirmed, in the identity of the person who withdraws it, the consultant who proposed it
// (org:ProposeOnboarding) or the customer's owner (org:ConfirmOnboarding), with a reason of 1 to 2000 characters. ORG records the withdrawal, which is
// immutable, and a withdrawn onboarding is never confirmed; then each module's held change set is cancelled with that module's own cancel
// command and the withdrawing person's own context, so each person needs, for each module, ConfigureSettings or ApproveSettings by name, as
// the cancel commands ask, which is checked before anything is recorded. The same command again completes a module that did not cancel.
func (o *Onboarding) WithdrawOnboarding(ctx context.Context, cmd orgapi.WithdrawOnboarding) (orgapi.OnboardingReceipt, error) {
	if err := checkActor("the actor id of the withdrawal", cmd.ActorID, false); err != nil {
		return orgapi.OnboardingReceipt{}, err
	}
	onb, err := o.org.GetOnboarding(ctx, orgapi.GetOnboarding{TenantID: cmd.TenantID, OnboardingID: cmd.OnboardingID})
	if err != nil {
		return orgapi.OnboardingReceipt{}, err
	}
	// the consultant who proposed it withdraws as its proposer, the customer's owner as the one who confirms
	orgAction := orgapi.ActionConfirmOnboarding
	if cmd.ActorID == onb.ProposedBy {
		orgAction = orgapi.ActionProposeOnboarding
	}
	if err := holdsNamed(ctx, "org", cmd.TenantID, cmd.ActorID, orgAction); err != nil {
		return orgapi.OnboardingReceipt{}, fmt.Errorf("%w: %v; the consultant who proposed the onboarding (org:ProposeOnboarding) or the customer's owner (org:ConfirmOnboarding), "+
			"each by name, withdraws it", orgapi.ErrUnauthorized, err)
	}
	for _, bc := range []struct{ name, configure, approve string }{
		{"fin", finapi.ActionConfigureSettings, finapi.ActionApproveSettings}, {"sea", seaapi.ActionConfigureSettings, seaapi.ActionApproveSettings}} {
		if identity.AuthorizeNamed(ctx, bc.name, cmd.TenantID, cmd.ActorID, bc.configure) != nil && identity.AuthorizeNamed(ctx, bc.name, cmd.TenantID, cmd.ActorID, bc.approve) != nil {
			return orgapi.OnboardingReceipt{}, fmt.Errorf("%w: %s cancels the change set held for the onboarding in %s with %s's cancel command, which asks %s:%s or %s:%s, by name; "+
				"ask for one of them, and nothing is recorded", orgapi.ErrUnauthorized, cmd.ActorID, strings.ToUpper(bc.name), strings.ToUpper(bc.name), bc.name, bc.configure, bc.name, bc.approve)
		}
	}
	receipt, err := o.org.WithdrawOnboarding(asPlatform(ctx), cmd)
	if err != nil {
		return receipt, err
	}
	onb, err = o.org.GetOnboarding(ctx, orgapi.GetOnboarding{TenantID: cmd.TenantID, OnboardingID: cmd.OnboardingID})
	if err != nil {
		return receipt, err
	}
	var failures []ModuleFailure
	for _, module := range []string{ModuleFIN, ModuleSEA} {
		if err := o.cancelModule(ctx, cmd, onb, module); err != nil {
			var p permanent
			failures = append(failures, ModuleFailure{Module: module, Err: err, Permanent: errors.As(err, &p)})
		}
	}
	receipt.Notice = joinNotice(receipt.Notice, "the onboarding was withdrawn by "+onb.WithdrawnBy+" at "+onb.WithdrawnAt.UTC().Format(time.RFC3339)+" and is never confirmed; the change sets held for it "+
		"are cancelled in "+onb.WithdrawnBy+"'s own name")
	return receipt, stepError("WithdrawOnboarding", failures)
}

func joinNotice(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// permanent marks a failure that sending the same command again cannot cure; its text says the way out.
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

const newOnboarding = "propose a new onboarding under another id (ProposeOnboarding with a new OnboardingID), and have the owner confirm that"

// settingsChange is a module's change set as the onboarding reads it.
type settingsChange struct {
	Status     string
	ProposedBy string
	Reason     string
	Hold       string
	Version    int
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
				out := settingsChange{Status: c.Status, ProposedBy: c.ProposedBy, Reason: c.Reason, Hold: c.HeldForOnboarding, Version: c.Version}
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
			out := settingsChange{Status: c.Status, ProposedBy: c.ProposedBy, Reason: c.Reason, Hold: c.HeldForOnboarding, Version: c.Version}
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

// asPermanent classes an error of a module's own command, for the step (declare, propose, approve or cancel) that got it, as one that
// sending the same command again can never cure (Permanent: the way out is a new onboarding under another id) or not. Each
// error a module can answer to a step, and how it is classed:
//
//   - a command id the onboarding derived is taken by another command (idempotency conflict): permanent;
//   - the change set was cancelled or refused (SETTINGS_CANCELLED, SETTINGS_REFUSED), and an approver who is the proposer
//     (SETTINGS_SAME_ACTOR, SETTINGS_HELD_FOR_ONBOARDING) or an acknowledgement where there is none to give (SETTINGS_ACKNOWLEDGEMENT), which depend only on
//     the people and the record and which the confirmation's own checks prevent: permanent;
//   - a change set that is in force already when a withdrawal would cancel it (SETTINGS_IN_FORCE): permanent, since a change in force is
//     not cancelled and the onboarding was not confirmed through the platform;
//   - the module refuses the plan's own entries (an unknown item or value, a scope, a parameter, a duplicate or a shape,
//     SETTINGS_UNKNOWN_ITEM, _UNKNOWN_VALUE, _SCOPE, _PARAMETER, _DUPLICATE, _INVALID when proposing): not permanent, because the
//     build the plan was made with accepted it, so running that build cures it; the answer names both ways out, as it does for a
//     build that differs (buildDiffers, which checks the same before the module is called);
//   - SETTINGS_RULE against the tenant's other change sets (the plan's own entries pass this build's rules), and
//     SETTINGS_ONBOARDING_WAITING (another onboarding's change set waits in the module): curable, by cancelling or approving
//     what conflicts, or confirming or withdrawing the other onboarding, with the module's own words;
//   - FIN_NO_POLICY (an override names a legal entity that has no policy yet), a clock that is behind (SETTINGS_CLOCK,
//     SETTINGS_CLOCK_SKEW, SETTINGS_ORDER, SETTINGS_EFFECTIVE_TIME, a declaration at the instant of another or before it, an
//     approval before its proposal), a change set already approved (the retry finds it), a missing permission, a conflict or
//     a database that is down: curable, by what the words say, and then the same command completes the module.
func asPermanent(module, step string, err error) error {
	command := "ConfirmOnboarding"
	switch step {
	case "propose":
		command = "ProposeOnboarding"
	case "cancel":
		command = "WithdrawOnboarding"
	}
	switch {
	case errors.Is(err, finapi.ErrIdempotencyConflict), errors.Is(err, seaapi.ErrIdempotencyConflict):
		return permanent{fmt.Errorf("a command id the onboarding derived for %s is taken by another command (%w); %s", module, err, newOnboarding)}
	case errors.Is(err, settings.ErrCancelled), errors.Is(err, settings.ErrRefusedChange): // ErrCancelled is reached only by a cancellation between the listing and the approval, a race
		return permanent{fmt.Errorf("the change set of %s was cancelled or refused (%w); %s", module, err, newOnboarding)}
	case errors.Is(err, settings.ErrSameActor), errors.Is(err, settings.ErrAcknowledgement), errors.Is(err, settings.ErrHeld):
		return permanent{fmt.Errorf("the people of the onboarding cannot approve the change set of %s (%w); %s", module, err, newOnboarding)}
	case step == "cancel" && errors.Is(err, settings.ErrInForce):
		return permanent{fmt.Errorf("the change set of %s is in force and is not cancelled (%w); change the settings with the module's own commands, and %s", module, err, newOnboarding)}
	case step == "propose" && (errors.Is(err, settings.ErrUnknownItem) || errors.Is(err, settings.ErrUnknownValue) || errors.Is(err, settings.ErrScope) ||
		errors.Is(err, settings.ErrParameter) || errors.Is(err, settings.ErrDuplicate) || errors.Is(err, settings.ErrInvalid)):
		return fmt.Errorf("%w: %s refuses the plan's entries (%w); run the build the plan was made with and send the same %s again, or propose a new onboarding under another id",
			ErrOnboardingBuild, module, err, command)
	}
	return err
}

func hasModuleState(modules []orgapi.OnboardingModule, module, state string) bool {
	return slices.ContainsFunc(modules, func(m orgapi.OnboardingModule) bool { return m.Module == module && m.State == state })
}

// heldAs says why a module's change set is not the onboarding's, as the proposal stored it: it was proposed by the consultant, with the reason the
// onboarding gives, held for this onboarding, with exactly the stored entries. It answers "" when it is.
func heldAs(change settingsChange, onb orgapi.Onboarding, stored []onboardingEntry) string {
	switch {
	case change.ProposedBy != onb.ProposedBy:
		return "it was proposed by " + change.ProposedBy + ", not by the consultant " + onb.ProposedBy
	case change.Reason != heldReason(onb.OnboardingID, onb.ProposedBy, onb.ProposedAt):
		return "its reason is not the onboarding's"
	case change.Hold != onb.OnboardingID:
		return "it is not held for this onboarding"
	case !sameEntries(stored, change.Entries):
		return "its entries are not the plan's"
	}
	return ""
}

// heldAsProposed checks, before the owner's confirmation is recorded, that a module holds the change set the plan stored: found by its derived
// id, the onboarding's own (heldAs), and still waiting for the owner (or approved already, which the retry finds).
func (o *Onboarding) heldAsProposed(ctx context.Context, onb orgapi.Onboarding, module string) error {
	changeID := orgChangeID(onb.OnboardingID, module)
	change, found, err := o.findChange(ctx, module, onb.TenantID, changeID)
	switch {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("%s holds no change set %s for the onboarding yet (the consultant's proposal did not reach it)", module, changeID)
	}
	if why := heldAs(change, onb, entriesOfOnboarding(onb.Plan[module].Entries)); why != "" {
		return fmt.Errorf("change set %s of %s is not the onboarding's: %s", changeID, module, why)
	}
	if change.Status == "CANCELLED" || change.Status == "REFUSED" {
		return fmt.Errorf("change set %s of %s was %s, so there is nothing for the owner to approve", changeID, module, strings.ToLower(change.Status))
	}
	return nil
}

func orgChangeID(onboarding, module string) string {
	return "onb-" + onboarding + "-" + strings.ToLower(module)
}

// stepCommand is the derived command id of an onboarding's step in a module.
func stepCommand(onboarding, module, step string) string {
	return "onb-" + onboarding + "-" + strings.ToLower(module) + "-" + step
}

// recordModule writes ORG's record of a module's progress, as the platform: PROPOSED as the proposer, APPROVED as the confirmer, with the settings version.
func (o *Onboarding) recordModule(ctx context.Context, onb orgapi.Onboarding, module, state string, version int) error {
	actor := onb.ProposedBy
	if state == orgapi.ModuleApproved {
		actor = onb.ConfirmedBy
	}
	_, err := o.org.RecordOnboardingModule(recordingAsPlatform(ctx, onb.TenantID, actor), orgapi.RecordOnboardingModule{CommandID: stepCommand(onb.OnboardingID, module, "record-"+strings.ToLower(state)),
		TenantID: onb.TenantID, ActorID: actor, OnboardingID: onb.OnboardingID, Module: module, ChangeID: orgChangeID(onb.OnboardingID, module), State: state, Version: version})
	return err
}

// holdModule takes one module to holding the onboarding's change set, from the entries the proposal stored, in the consultant's own identity (ctx is
// theirs): the change set is found by its derived id and proposed when it is missing, and ORG's record is written when it is missing, so that a crash
// between the module's proposal and ORG's record is completed by a retry without a second change set. Each step is idempotent by its derived command id.
func (o *Onboarding) holdModule(ctx context.Context, onb orgapi.Onboarding, module string) error {
	id, tenant := onb.OnboardingID, onb.TenantID
	changeID := orgChangeID(id, module)
	plan, ok := onb.Plan[module]
	if !ok {
		return permanent{fmt.Errorf("the onboarding stores no plan for %s; %s", module, newOnboarding)}
	}
	// a build that is not the proposal's is not permanent: the build the plan was made with cures it
	if err := o.buildDiffers(ctx, module, plan); err != nil {
		return fmt.Errorf("%w: %v; run the build the plan was made with and send the same ProposeOnboarding again, or propose a new onboarding under another id", ErrOnboardingBuild, err)
	}
	reason := heldReason(id, onb.ProposedBy, onb.ProposedAt)
	stored := entriesOfOnboarding(plan.Entries)
	change, found, err := o.findChange(ctx, module, tenant, changeID)
	if err != nil {
		return err
	}
	if found {
		if why := heldAs(change, onb, stored); why != "" {
			return permanent{fmt.Errorf("change set %s of %s exists and is not this onboarding's (%s); %s", changeID, module, why, newOnboarding)}
		}
		if change.Status == "CANCELLED" || change.Status == "REFUSED" {
			return permanent{fmt.Errorf("change set %s of %s was %s; %s", changeID, module, strings.ToLower(change.Status), newOnboarding)}
		}
	} else {
		finAsked, seaAsked, err := storedEntries(module, stored)
		if err != nil {
			return err
		}
		held := asPlatform(ctx) // the consultant's own context, marked as the onboarding's: the authorizer asks for the mark and ConfigureSettings by name
		if module == ModuleFIN {
			_, err = o.fin.ProposeFinSettingsChange(held, finapi.ProposeFinSettingsChange{CommandID: stepCommand(id, module, "propose"), TenantID: tenant, ActorID: onb.ProposedBy,
				ChangeID: changeID, Entries: finAsked, Reason: reason, OnboardingID: id})
		} else {
			_, err = o.sea.ProposeSeaSettingsChange(held, seaapi.ProposeSeaSettingsChange{CommandID: stepCommand(id, module, "propose"), TenantID: tenant, ActorID: onb.ProposedBy,
				ChangeID: changeID, Entries: seaAsked, Reason: reason, OnboardingID: id})
		}
		if err != nil {
			return asPermanent(module, "propose", err)
		}
		if _, found, err = o.findChange(ctx, module, tenant, changeID); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("change set %s of %s was proposed and is not listed; send the same ProposeOnboarding again", changeID, module)
		}
	}
	if !hasModuleState(onb.Modules, module, orgapi.ModuleProposed) {
		return o.recordModule(ctx, onb, module, orgapi.ModuleProposed, 0)
	}
	return nil
}

// approveModule takes one module to APPROVED, in the owner's own identity (ctx is theirs): a single administrator is declared when the onboarding
// says so (the declaration is the platform's own, made as the owner); the held change set, found by its derived id and checked to be the onboarding's,
// is approved with the module's own approve command when it waits; and ORG's record is written when it is missing, with the version the module made.
// Whether the module is done is decided from the module's own change set, not from ORG's record, so a module half done is finished, a crash between
// the module's approval and ORG's record is completed by a retry, and nothing is made twice. Each step is idempotent by its derived command id.
func (o *Onboarding) approveModule(ctx context.Context, cmd orgapi.ConfirmOnboarding, onb orgapi.Onboarding, module string) error {
	id, lower := onb.OnboardingID, strings.ToLower(module)
	changeID := orgChangeID(id, module)
	plan, ok := onb.Plan[module]
	if !ok {
		return permanent{fmt.Errorf("the onboarding stores no plan for %s; %s", module, newOnboarding)}
	}
	// a build that is not the proposal's is not permanent: the build the plan was made with cures it
	if err := o.buildDiffers(ctx, module, plan); err != nil {
		return fmt.Errorf("%w: %v; the owner's confirmation is recorded and stays: run the build the plan was made with and send the same ConfirmOnboarding again, "+
			"or propose a new onboarding under another id", ErrOnboardingBuild, err)
	}
	stored := entriesOfOnboarding(plan.Entries)
	change, found, err := o.findChange(ctx, module, cmd.TenantID, changeID)
	if err != nil {
		return err
	}
	switch {
	case !found:
		return fmt.Errorf("%s holds no change set %s for the onboarding (the consultant's proposal did not reach it); the consultant sends the same ProposeOnboarding again to complete it, "+
			"and then the same ConfirmOnboarding completes the module", module, changeID)
	case heldAs(change, onb, stored) != "":
		return permanent{fmt.Errorf("change set %s of %s exists and is not this onboarding's (%s); %s", changeID, module, heldAs(change, onb, stored), newOnboarding)}
	case change.Status == "CANCELLED" || change.Status == "REFUSED":
		return permanent{fmt.Errorf("change set %s of %s was %s; %s", changeID, module, strings.ToLower(change.Status), newOnboarding)}
	}
	if onb.SingleAdministrator {
		declarer := platformPrincipal(ctx, cmd.TenantID, cmd.ActorID, lower+":DeclareSingleAdministrator")
		if module == ModuleFIN {
			_, err = o.fin.DeclareFinSingleAdministrator(declarer, finapi.DeclareFinSingleAdministrator{CommandID: stepCommand(id, module, "declare"), TenantID: cmd.TenantID,
				ActorID: cmd.ActorID, OnboardingID: id, ProposedBy: onb.ProposedBy, AdministratorID: onb.AdministratorID})
		} else {
			_, err = o.sea.DeclareSeaSingleAdministrator(declarer, seaapi.DeclareSeaSingleAdministrator{CommandID: stepCommand(id, module, "declare"), TenantID: cmd.TenantID,
				ActorID: cmd.ActorID, OnboardingID: id, ProposedBy: onb.ProposedBy, AdministratorID: onb.AdministratorID})
		}
		if err != nil {
			return asPermanent(module, "declare", err)
		}
	}
	if !hasModuleState(onb.Modules, module, orgapi.ModuleProposed) { // a crash between the consultant's proposal and ORG's record of it
		if err = o.recordModule(ctx, onb, module, orgapi.ModuleProposed, 0); err != nil {
			return err
		}
	}
	version := change.Version
	if change.Status == "PROPOSED" { // the owner approves it, in their own name and as a second person
		owner := asPlatform(ctx)
		if module == ModuleFIN {
			var r finapi.Receipt
			r, err = o.fin.ApproveFinSettingsChange(owner, finapi.ApproveFinSettingsChange{CommandID: stepCommand(id, module, "approve"), TenantID: cmd.TenantID, ActorID: cmd.ActorID, ChangeID: changeID})
			version = r.Version
		} else {
			var r seaapi.Receipt
			r, err = o.sea.ApproveSeaSettingsChange(owner, seaapi.ApproveSeaSettingsChange{CommandID: stepCommand(id, module, "approve"), TenantID: cmd.TenantID, ActorID: cmd.ActorID, ChangeID: changeID})
			version = r.SettingsVersion
		}
		if err != nil {
			return asPermanent(module, "approve", err)
		}
	}
	if !hasModuleState(onb.Modules, module, orgapi.ModuleApproved) {
		if version < 1 {
			return fmt.Errorf("%s approved change set %s and its settings version is not known; send the same ConfirmOnboarding again", module, changeID)
		}
		return o.recordModule(ctx, onb, module, orgapi.ModuleApproved, version)
	}
	return nil
}

// cancelModule cancels one module's change set held for a withdrawn onboarding, in the identity of the person who withdraws it (ctx is theirs), with the
// module's own cancel command. A change set that is missing, cancelled or refused already is done; one that is in force is not cancelled.
func (o *Onboarding) cancelModule(ctx context.Context, cmd orgapi.WithdrawOnboarding, onb orgapi.Onboarding, module string) error {
	id := onb.OnboardingID
	changeID := orgChangeID(id, module)
	change, found, err := o.findChange(ctx, module, cmd.TenantID, changeID)
	switch {
	case err != nil:
		return err
	case !found, change.Status == "CANCELLED", change.Status == "REFUSED":
		return nil
	case change.Hold != id || change.ProposedBy != onb.ProposedBy:
		return permanent{fmt.Errorf("change set %s of %s exists and is not this onboarding's (it is not held for it); %s", changeID, module, newOnboarding)}
	}
	reason := withdrawReason(id, onb.WithdrawnBy, onb.WithdrawnAt)
	if module == ModuleFIN {
		_, err = o.fin.CancelFinSettingsChange(ctx, finapi.CancelFinSettingsChange{CommandID: stepCommand(id, module, "withdraw"), TenantID: cmd.TenantID, ActorID: cmd.ActorID, ChangeID: changeID, Reason: reason})
	} else {
		_, err = o.sea.CancelSeaSettingsChange(ctx, seaapi.CancelSeaSettingsChange{CommandID: stepCommand(id, module, "withdraw"), TenantID: cmd.TenantID, ActorID: cmd.ActorID, ChangeID: changeID, Reason: reason})
	}
	if err != nil {
		return asPermanent(module, "cancel", err)
	}
	return nil
}
