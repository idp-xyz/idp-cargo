package sea

import (
	"context"
	"fmt"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
)

// SEA V2.100: SEA's settings, as FIN's (V2.96). The commands carry no business time: SEA records its own clock.

var (
	_ api.SettingsCommands = (*Module)(nil)
	_ api.SettingsQueries  = (*Module)(nil)
)

// entriesOf turns the entries as asked into the ledger's. SEA's items take no parameter.
func entriesOf(asked []api.SeaSettingEntry) []settings.Entry {
	out := make([]settings.Entry, len(asked))
	for i, e := range asked {
		out[i] = settings.Entry{Scope: settings.Scope{Kind: e.ScopeKind, ID: e.ScopeID}, Key: e.Key, Value: e.Value}
	}
	return out
}

// ProposeSeaSettingsChange proposes a change set of SEA's settings.
func (m *Module) ProposeSeaSettingsChange(ctx context.Context, cmd api.ProposeSeaSettingsChange) (api.Receipt, error) {
	if cmd.EffectiveAt.Year() > 9000 { // beyond what a command's payload can be hashed with, and beyond any limit
		return api.Receipt{}, classify(settings.Refuse(settings.ErrEffective, "the effective time is more than a year after now; choose a nearer time, "+
			"and propose again then for a later change"))
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ProposeSettings(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionConfigureSettings,
		cmd.ChangeID, 0, m.clock.Now(), cmd), entriesOf(cmd.Entries), cmd.Reason, stored(cmd.EffectiveAt))
	return receipt(r, err)
}

// ApproveSeaSettingsChange approves a change set that waits for a second person.
func (m *Module) ApproveSeaSettingsChange(ctx context.Context, cmd api.ApproveSeaSettingsChange) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ApproveSettingsAs(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionApproveSettings,
		cmd.ChangeID, 0, m.clock.Now(), cmd), cmd.RiskAcknowledgement)
	return receipt(r, err)
}

// DeclareSeaSingleAdministrator records the tenant's declaration of a single administrator (V2.102).
func (m *Module) DeclareSeaSingleAdministrator(ctx context.Context, cmd api.DeclareSeaSingleAdministrator) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).DeclareSingleAdministrator(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionDeclareSingleAdministrator, cmd.OnboardingID, 0, m.clock.Now(), cmd), cmd.ProposedBy)
	return receipt(r, err)
}

// GetSeaExceptionReport answers SEA's part of the exception report to a holder of ApproveSettings.
func (m *Module) GetSeaExceptionReport(ctx context.Context, q api.GetSeaExceptionReport) (api.SeaExceptionReport, error) {
	if q.ActorID == "" {
		return api.SeaExceptionReport{}, classify(application.ErrUnauthenticated)
	}
	if m.auth == nil || m.auth.AuthorizeSeaAction(ctx, q.TenantID, q.ActorID, api.ActionApproveSettings, q.TenantID) != nil {
		return api.SeaExceptionReport{}, classify(fmt.Errorf("%w: the exception report is told to a holder of ApproveSettings; ask for the permission, granted by name",
			application.ErrUnauthorized))
	}
	got, err := m.reads(q.TenantID).ExceptionReport(ctx, domain.TenantID(q.TenantID), q.From, q.To)
	if err != nil {
		return api.SeaExceptionReport{}, classify(err)
	}
	out := api.SeaExceptionReport{TenantID: q.TenantID, From: q.From, To: q.To, AsOf: got.At}
	for _, x := range got.Report.Exceptions {
		out.Exceptions = append(out.Exceptions, api.SeaExceptionLine{ScopeKind: x.Scope.Kind, ScopeID: x.Scope.ID, Key: x.Key, Value: x.Value, ThanValue: x.TenantValue,
			ChangeID: x.ChangeID, ProposedBy: x.ProposedBy, ApprovedBy: x.ApprovedBy})
	}
	for _, x := range got.Report.Looser {
		out.LooserThanStandard = append(out.LooserThanStandard, api.SeaExceptionLine{ScopeKind: x.Scope.Kind, ScopeID: x.Scope.ID, Key: x.Key, Value: x.Value,
			ThanValue: x.Standard, ChangeID: x.ChangeID, ProposedBy: x.ProposedBy, ApprovedBy: x.ApprovedBy})
	}
	for _, x := range got.Report.SelfApproved {
		line := api.SeaSelfApproval{ChangeID: x.ChangeID, ApprovedBy: x.By, ApprovedAt: x.At, Acknowledgement: x.Acknowledgement}
		for _, e := range x.Entries {
			line.Entries = append(line.Entries, api.SeaSettingEntryView{ScopeKind: e.Scope.Kind, ScopeID: e.Scope.ID, Key: e.Key, Value: e.Value})
		}
		out.SelfApproved = append(out.SelfApproved, line)
	}
	for _, x := range got.Report.Changers {
		out.Changers = append(out.Changers, api.SeaSettingsChanger{ActorID: x.Actor, Proposed: x.Proposed, Approved: x.Approved})
	}
	for _, x := range got.Declarations {
		out.Declarations = append(out.Declarations, api.SeaSingleAdministratorDeclaration{OnboardingID: x.OnboardingID, ProposedBy: x.ProposedBy,
			ConfirmedBy: x.ConfirmedBy, DeclaredAt: x.At})
	}
	return out, nil
}

// CancelSeaSettingsChange cancels a change set that is not in force yet.
// A holder of ConfigureSettings cancels as a configurer; one who holds only ApproveSettings cancels as an approver, so
// that a change set approved with a far-off time cannot be left in the way of every later change by the person who
// proposed it. The role the canceller acted under is recorded.
func (m *Module) CancelSeaSettingsChange(ctx context.Context, cmd api.CancelSeaSettingsChange) (api.Receipt, error) {
	action, role := api.ActionConfigureSettings, settings.RoleConfigurer
	if m.auth != nil && m.auth.AuthorizeSeaAction(ctx, cmd.TenantID, cmd.ActorID, action, cmd.ChangeID) != nil {
		action, role = api.ActionApproveSettings, settings.RoleApprover
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).CancelSettings(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, action,
		cmd.ChangeID, 0, m.clock.Now(), cmd), cmd.Reason, role)
	return receipt(r, err)
}

// RefuseSeaSettingsChange records an approver's refusal of a change set that waits for approval.
func (m *Module) RefuseSeaSettingsChange(ctx context.Context, cmd api.RefuseSeaSettingsChange) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RefuseSettings(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionApproveSettings,
		cmd.ChangeID, 0, m.clock.Now(), cmd), cmd.Reason)
	return receipt(r, err)
}

// GetSeaSettings answers the settings in force for a scope at a time.
func (m *Module) GetSeaSettings(ctx context.Context, q api.GetSeaSettings) (api.SeaSettings, error) {
	scope := settings.Scope{Kind: q.ScopeKind, ID: q.ScopeID}
	in, err := m.reads(q.TenantID).SettingsAt(ctx, domain.TenantID(q.TenantID), scope, stored(q.AsOf))
	if err != nil {
		return api.SeaSettings{}, classify(err)
	}
	out := api.SeaSettings{TenantID: q.TenantID, ScopeKind: q.ScopeKind, ScopeID: q.ScopeID, AsOf: in.At, Version: in.Version}
	for _, it := range application.SeaSettingsCatalogue().Items {
		s := in.Config[it.Key]
		out.Items = append(out.Items, api.SeaSetting{Key: it.Key, Value: s.Value, FromScopeKind: s.From.Kind, FromScopeID: s.From.ID, Default: !s.Set,
			ChangeID: s.ChangeID, Exception: s.Exception})
	}
	return out, nil
}

// ListSeaSettingsChanges answers the change sets of the tenant with their history.
func (m *Module) ListSeaSettingsChanges(ctx context.Context, q api.ListSeaSettingsChanges) ([]api.SeaSettingsChange, error) {
	sets, now, err := m.reads(q.TenantID).SettingsChanges(ctx, domain.TenantID(q.TenantID))
	if err != nil {
		return nil, classify(err)
	}
	out := []api.SeaSettingsChange{}
	for _, c := range sets {
		v := api.SeaSettingsChange{ChangeID: c.ID, Status: string(c.Status(now)), Reason: c.Reason, Loosening: c.Loosening, ProposedBy: c.By,
			ProposedAt: c.At, EffectiveAt: c.Effective, Entries: []api.SeaSettingEntryView{}}
		for _, e := range c.Entries {
			v.Entries = append(v.Entries, api.SeaSettingEntryView{ScopeKind: e.Scope.Kind, ScopeID: e.Scope.ID, Key: e.Key, Value: e.Value})
		}
		if a := c.Approval; a != nil {
			v.ApprovedBy, v.ApprovedAt, v.Version, v.InForceAt, v.AutoApproved = a.By, a.At, a.Version, a.InForce, a.Auto
			v.RiskAcknowledgement = a.Acknowledgement
		}
		if x := c.Cancellation; x != nil {
			v.CancelledBy, v.CancelledAt, v.CancelReason, v.CancelledRole = x.By, x.At, x.Reason, x.Role
		}
		out = append(out, v)
	}
	return out, nil
}

// GetSeaSettingsCatalogue answers the catalogue of the settings, and the same as a table.
func (m *Module) GetSeaSettingsCatalogue(context.Context, api.GetSeaSettingsCatalogue) (api.SeaSettingsCatalogue, error) {
	return SettingsCatalogue(), nil
}

// SettingsCatalogue answers the catalogue of SEA's settings without a module: the catalogue document is written from
// it (cmd/cargo-settings-doc), and the onboarding reads the items and values it checks a profile against.
func SettingsCatalogue() api.SeaSettingsCatalogue {
	cat := application.SeaSettingsCatalogue()
	out := api.SeaSettingsCatalogue{Table: cat.Table(), Version: cat.Version()}
	for _, it := range cat.Items {
		item := api.SeaSettingItem{Key: it.Key, TitleZH: it.TitleZH, TitleEN: it.TitleEN, Default: it.Default, ScopeKinds: append([]string(nil), it.Kinds...),
			InternalControl: it.Control, Explanation: it.Explanation}
		for _, v := range it.Values {
			item.Values = append(item.Values, api.SeaSettingValue{ID: v.ID, TitleZH: v.TitleZH, TitleEN: v.TitleEN})
		}
		out.Items = append(out.Items, item)
	}
	return out
}

// CheckSettingsEntries checks entries as a first proposal of a tenant with no settings would be checked: each entry
// alone, the configuration they make as a whole, and the rules across items. It writes nothing and reads no legal
// entity, so a legal entity that SEA does not know is refused only when the change set is proposed. The
// onboarding uses it to refuse an invalid profile or override when the consultant proposes it, not when the owner confirms.
func CheckSettingsEntries(entries []api.SeaSettingEntry) error {
	asked := make([]settings.Entry, len(entries))
	for i, e := range entries {
		asked[i] = settings.Entry{Scope: settings.Scope{Kind: e.ScopeKind, ID: e.ScopeID}, Key: e.Key, Value: e.Value}
	}
	_, err := settings.NewLedger(nil).Propose(application.SeaSettingsCatalogue(), settings.Proposal{ID: "check", Entries: asked, Reason: "check", By: "check", At: time.Now().UTC()})
	if err == nil {
		return nil
	}
	return classify(err)
}

// releaseBy says who may release a conflict under a value of S1.
func releaseBy(s1 string) string {
	if s1 == domain.S1SupervisorOnly {
		return api.ReleaseBySupervisor
	}
	return api.ReleaseByAnyOperator
}

// GetSeaSettingsChangeImpact answers what a change set not yet in force, or the entries asked about, would change.
func (m *Module) GetSeaSettingsChangeImpact(ctx context.Context, q api.GetSeaSettingsChangeImpact) (api.SeaSettingsChangeImpact, error) {
	in, err := m.reads(q.TenantID).SettingsImpact(ctx, domain.TenantID(q.TenantID), q.ChangeID, entriesOf(q.Entries))
	if err != nil {
		return api.SeaSettingsChangeImpact{}, classify(err)
	}
	out := api.SeaSettingsChangeImpact{TenantID: q.TenantID, ChangeID: in.ChangeID, TakesEffect: in.TakesEffect, Entities: []api.SeaImpactEntity{}}
	for _, e := range in.Entities {
		entity := api.SeaImpactEntity{LegalEntityID: e.LegalEntity, From: e.From, To: e.To, Conflicts: []api.SeaImpactConflict{}}
		for _, c := range e.Conflicts {
			entity.Conflicts = append(entity.Conflicts, api.SeaImpactConflict{MovementID: string(c.Movement), VisitID: string(c.Visit), ContainerNumber: c.Number,
				FactNo: c.FactNo, ReleaseBefore: releaseBy(e.From), ReleaseAfter: releaseBy(e.To)})
		}
		out.Entities = append(out.Entities, entity)
	}
	return out, nil
}
