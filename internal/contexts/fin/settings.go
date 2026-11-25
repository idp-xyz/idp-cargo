package fin

import (
	"context"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.96: the settings (step C1a). The commands carry no business time: FIN records its own clock.

// ProposeFinSettingsChange proposes a change set of FIN's settings.
func (m *Module) ProposeFinSettingsChange(ctx context.Context, cmd api.ProposeFinSettingsChange) (api.Receipt, error) {
	if cmd.EffectiveAt.Year() > 9000 { // beyond what a command's payload can be hashed with, and beyond any limit
		return api.Receipt{}, classify(settings.Refuse(settings.ErrEffective, "the effective time is more than a year after now; choose a nearer time, "+
			"and propose again then for a later change"))
	}
	asks := make([]application.SettingEntryAsk, len(cmd.Entries))
	for i, e := range cmd.Entries {
		asks[i].Entry = settings.Entry{Scope: settings.Scope{Kind: e.ScopeKind, ID: e.ScopeID}, Key: e.Key, Value: e.Value}
		if e.Threshold != nil {
			minor := e.Threshold.Minor
			asks[i].Param, asks[i].Currency = &minor, e.Threshold.Currency
		}
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ProposeSettings(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionConfigureSettings,
		cmd.ChangeID, 0, m.clock.Now(), cmd), asks, cmd.Reason, stored(cmd.EffectiveAt))
	return receipt(r, err)
}

// ApproveFinSettingsChange approves a change set that waits for a second person.
func (m *Module) ApproveFinSettingsChange(ctx context.Context, cmd api.ApproveFinSettingsChange) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ApproveSettings(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionApproveSettings,
		cmd.ChangeID, 0, m.clock.Now(), cmd))
	return receipt(r, err)
}

// CancelFinSettingsChange cancels a change set that is not in force yet.
// A holder of ConfigureSettings cancels as a configurer; one who holds only ApproveSettings cancels as an approver, so
// that a change set approved with a far-off time cannot be left in the way of every later change by the person who
// proposed it. The role the canceller acted under is recorded.
func (m *Module) CancelFinSettingsChange(ctx context.Context, cmd api.CancelFinSettingsChange) (api.Receipt, error) {
	action, role := api.ActionConfigureSettings, settings.RoleConfigurer
	if m.auth != nil && m.auth.AuthorizeFinAction(ctx, cmd.TenantID, cmd.ActorID, action, cmd.ChangeID) != nil {
		action, role = api.ActionApproveSettings, settings.RoleApprover
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).CancelSettings(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, action,
		cmd.ChangeID, 0, m.clock.Now(), cmd), cmd.Reason, role)
	return receipt(r, err)
}

// RefuseFinSettingsChange records an approver's refusal of a change set that waits for approval.
func (m *Module) RefuseFinSettingsChange(ctx context.Context, cmd api.RefuseFinSettingsChange) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RefuseSettings(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionApproveSettings,
		cmd.ChangeID, 0, m.clock.Now(), cmd), cmd.Reason)
	return receipt(r, err)
}

// GetFinSettings answers the settings in force for a scope at a time.
func (m *Module) GetFinSettings(ctx context.Context, q api.GetFinSettings) (api.FinSettings, error) {
	scope := settings.Scope{Kind: q.ScopeKind, ID: q.ScopeID}
	in, err := m.deps(q.TenantID, "").SettingsAt(ctx, domain.TenantID(q.TenantID), scope, stored(q.AsOf))
	if err != nil {
		return api.FinSettings{}, classify(err)
	}
	out := api.FinSettings{TenantID: q.TenantID, ScopeKind: q.ScopeKind, ScopeID: q.ScopeID, AsOf: in.At, Version: in.Version}
	for _, it := range application.FinSettingsCatalogue().Items {
		s := in.Config[it.Key]
		item := api.FinSetting{Key: it.Key, Value: s.Value, FromScopeKind: s.From.Kind, FromScopeID: s.From.ID, Default: !s.Set, ChangeID: s.ChangeID,
			Exception: s.Exception}
		if s.Param != nil {
			item.Threshold = &kernel.Money{Currency: kernel.CurrencyCode(in.Currency), Minor: *s.Param}
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// ListFinSettingsChanges answers the change sets of the tenant with their history.
func (m *Module) ListFinSettingsChanges(ctx context.Context, q api.ListFinSettingsChanges) ([]api.FinSettingsChange, error) {
	if q.TenantID == "" {
		return nil, classify(application.ErrInvalidQuery)
	}
	sets, now, err := m.deps(q.TenantID, "").SettingsChanges(ctx, domain.TenantID(q.TenantID))
	if err != nil {
		return nil, classify(err)
	}
	out := []api.FinSettingsChange{}
	for _, c := range sets {
		v := api.FinSettingsChange{ChangeID: c.ID, Status: string(c.Status(now)), Reason: c.Reason, Loosening: c.Loosening, ProposedBy: c.By,
			ProposedAt: c.At, EffectiveAt: c.Effective, Entries: []api.FinSettingEntryView{}}
		for _, e := range c.Entries {
			v.Entries = append(v.Entries, api.FinSettingEntryView{ScopeKind: e.Scope.Kind, ScopeID: e.Scope.ID, Key: e.Key, Value: e.Value, ThresholdMinor: e.Param})
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

// GetFinSettingsCatalogue answers the catalogue of the settings, and the same as a table.
func (m *Module) GetFinSettingsCatalogue(context.Context, api.GetFinSettingsCatalogue) (api.FinSettingsCatalogue, error) {
	cat := application.FinSettingsCatalogue()
	out := api.FinSettingsCatalogue{Table: cat.Table()}
	for _, it := range cat.Items {
		item := api.FinSettingItem{Key: it.Key, TitleZH: it.TitleZH, TitleEN: it.TitleEN, Default: it.Default, ScopeKinds: append([]string(nil), it.Kinds...),
			InternalControl: it.Control, Explanation: it.Explanation}
		for _, v := range it.Values {
			value := api.FinSettingValue{ID: v.ID, TitleZH: v.TitleZH, TitleEN: v.TitleEN}
			if v.Param != nil {
				value.Parameter = v.Param.Name
			}
			item.Values = append(item.Values, value)
		}
		out.Items = append(out.Items, item)
	}
	for _, r := range cat.Reference {
		out.Reference = append(out.Reference, api.FinReferenceItem{Key: r.Key, TitleZH: r.TitleZH, TitleEN: r.TitleEN, Governs: r.Governs, ChangedBy: r.ChangedBy,
			NeedsApproval: r.Approval, ApprovalNote: r.ApprovalNote})
	}
	return out, nil
}
