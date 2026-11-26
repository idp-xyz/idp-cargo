package fin

import (
	"context"
	"fmt"
	"time"

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
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ApproveSettingsAs(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionApproveSettings,
		cmd.ChangeID, 0, m.clock.Now(), cmd), cmd.RiskAcknowledgement)
	return receipt(r, err)
}

// DeclareFinSingleAdministrator records the tenant's declaration of a single administrator (V2.102).
func (m *Module) DeclareFinSingleAdministrator(ctx context.Context, cmd api.DeclareFinSingleAdministrator) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).DeclareSingleAdministrator(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionDeclareSingleAdministrator, cmd.OnboardingID, 0, m.clock.Now(), cmd), cmd.ProposedBy, cmd.AdministratorID)
	return receipt(r, err)
}

// GetFinExceptionReport answers FIN's part of the exception report to a holder of ApproveSettings.
func (m *Module) GetFinExceptionReport(ctx context.Context, q api.GetFinExceptionReport) (api.FinExceptionReport, error) {
	if q.ActorID == "" {
		return api.FinExceptionReport{}, classify(application.ErrUnauthenticated)
	}
	if m.auth == nil || m.auth.AuthorizeFinAction(ctx, q.TenantID, q.ActorID, api.ActionApproveSettings, q.TenantID) != nil {
		return api.FinExceptionReport{}, classify(fmt.Errorf("%w: the exception report is told to a holder of ApproveSettings; ask for the permission, granted by name",
			application.ErrUnauthorized))
	}
	got, err := m.deps(q.TenantID, "").ExceptionReport(ctx, domain.TenantID(q.TenantID), q.From, q.To)
	if err != nil {
		return api.FinExceptionReport{}, classify(err)
	}
	out := api.FinExceptionReport{TenantID: q.TenantID, From: q.From, To: q.To, AsOf: got.At}
	for _, x := range got.Report.Exceptions {
		out.Exceptions = append(out.Exceptions, api.FinExceptionLine{ScopeKind: x.Scope.Kind, ScopeID: x.Scope.ID, Key: x.Key, Value: x.Value, ThanValue: x.TenantValue,
			ChangeID: x.ChangeID, ProposedBy: x.ProposedBy, ApprovedBy: x.ApprovedBy})
	}
	for _, x := range got.Report.Looser {
		out.LooserThanStandard = append(out.LooserThanStandard, api.FinExceptionLine{ScopeKind: x.Scope.Kind, ScopeID: x.Scope.ID, Key: x.Key, Value: x.Value,
			ThresholdMinor: x.Param, ThanValue: x.Standard, ChangeID: x.ChangeID, ProposedBy: x.ProposedBy, ApprovedBy: x.ApprovedBy})
	}
	for _, x := range got.Report.SelfApproved {
		line := api.FinSelfApproval{ChangeID: x.ChangeID, ApprovedBy: x.By, ApprovedAt: x.At, Acknowledgement: x.Acknowledgement}
		for _, e := range x.Entries {
			line.Entries = append(line.Entries, api.FinSettingEntryView{ScopeKind: e.Scope.Kind, ScopeID: e.Scope.ID, Key: e.Key, Value: e.Value, ThresholdMinor: e.Param})
		}
		out.SelfApproved = append(out.SelfApproved, line)
	}
	for _, x := range got.Report.Changers {
		out.Changers = append(out.Changers, api.FinSettingsChanger{ActorID: x.Actor, Proposed: x.Proposed, Approved: x.Approved})
	}
	for _, x := range got.Declarations {
		out.Declarations = append(out.Declarations, api.FinSingleAdministratorDeclaration{OnboardingID: x.OnboardingID, ProposedBy: x.ProposedBy,
			ConfirmedBy: x.ConfirmedBy, AdministratorID: x.Administrator, DeclaredAt: x.At})
	}
	for _, w := range got.Work {
		out.Work = append(out.Work, api.FinActorWork{ActorID: w.Actor, Submitted: w.Submitted, Confirmed: w.Confirmed})
	}
	return out, nil
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
			v.RiskAcknowledgement = a.Acknowledgement
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
	return SettingsCatalogue(), nil
}

// SettingsCatalogue answers the catalogue of FIN's settings without a module: the catalogue document is written from
// it (cmd/cargo-settings-doc), and the onboarding reads the items and values it checks a profile against.
func SettingsCatalogue() api.FinSettingsCatalogue {
	cat := application.FinSettingsCatalogue()
	out := api.FinSettingsCatalogue{Table: cat.Table(), Version: cat.Version()}
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
	return out
}

// CheckSettingsEntries checks entries as a first proposal of a tenant with no settings would be checked: each entry
// alone, the configuration they make as a whole, and the rules across items. It writes nothing and reads no legal
// entity, so a legal entity that FIN does not know is refused only when the change set is proposed. The
// onboarding uses it to refuse an invalid profile or override when the consultant proposes it, not when the owner confirms.
func CheckSettingsEntries(entries []api.FinSettingEntry) error {
	asked := make([]settings.Entry, len(entries))
	for i, e := range entries {
		asked[i] = settings.Entry{Scope: settings.Scope{Kind: e.ScopeKind, ID: e.ScopeID}, Key: e.Key, Value: e.Value}
		if e.Threshold != nil {
			minor := e.Threshold.Minor
			asked[i].Param = &minor
		}
	}
	_, err := settings.NewLedger(nil).Propose(application.FinSettingsCatalogue(), settings.Proposal{ID: "check", Entries: asked, Reason: "check", By: "check", At: time.Now().UTC()})
	if err == nil {
		return nil
	}
	return classify(err)
}
