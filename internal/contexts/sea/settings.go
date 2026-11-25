package sea

import (
	"context"

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
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ApproveSettings(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionApproveSettings,
		cmd.ChangeID, 0, m.clock.Now(), cmd))
	return receipt(r, err)
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
	cat := application.SeaSettingsCatalogue()
	out := api.SeaSettingsCatalogue{Table: cat.Table()}
	for _, it := range cat.Items {
		item := api.SeaSettingItem{Key: it.Key, TitleZH: it.TitleZH, TitleEN: it.TitleEN, Default: it.Default, ScopeKinds: append([]string(nil), it.Kinds...),
			InternalControl: it.Control, Explanation: it.Explanation}
		for _, v := range it.Values {
			item.Values = append(item.Values, api.SeaSettingValue{ID: v.ID, TitleZH: v.TitleZH, TitleEN: v.TitleEN})
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
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
