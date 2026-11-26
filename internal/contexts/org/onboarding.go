package org

import (
	"context"
	"slices"

	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// Customer onboarding (V2.102): the record of a consultant's proposal, the owner's confirmation and each module's
// change set and state. ORG does not apply the profile; the platform's onboarding does, through FIN's and SEA's own
// commands, and records the outcome here.

var (
	_ api.OnboardingCommands = (*Module)(nil)
	_ api.OnboardingQueries  = (*Module)(nil)
)

func (m *Module) onboardings(tenant, commandID string) application.Onboardings {
	return application.Onboardings{
		Tx:     bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Store:  postgres.Onboardings{DB: m.db},
		Auth:   authorizer{m.auth},
		Outbox: postgres.Outbox{Writer: m.outbox, CommandID: commandID, RecordedAt: m.clock.Now()},
		Clock:  m.clock,
	}
}

func entriesIn(asked []api.OnboardingEntry) []domain.OnboardingEntry {
	out := make([]domain.OnboardingEntry, len(asked))
	for i, e := range asked {
		out[i] = domain.OnboardingEntry(e)
	}
	return out
}

func entriesOut(kept []domain.OnboardingEntry) []api.OnboardingEntry {
	out := make([]api.OnboardingEntry, len(kept))
	for i, e := range kept {
		out[i] = api.OnboardingEntry(e)
	}
	return out
}

func planOut(kept map[string]domain.ModulePlan) map[string]api.ModulePlan {
	out := map[string]api.ModulePlan{}
	for module, p := range kept {
		out[module] = api.ModulePlan{CatalogueVersion: p.CatalogueVersion, Entries: entriesOut(p.Entries)}
	}
	return out
}

func onboardingReceipt(commandID string, r application.OnboardingResult, err error) (api.OnboardingReceipt, error) {
	if err != nil {
		return api.OnboardingReceipt{}, classify(err)
	}
	return api.OnboardingReceipt{CommandID: commandID, OnboardingID: r.Record.Onboarding.ID, State: r.Record.State(), NewRevision: r.Revision, Notice: r.Notice,
		Plan: planOut(r.Record.Onboarding.Plan), PlanSHA256: r.Record.Onboarding.PlanSHA256}, nil
}

// ProposeOnboarding records a consultant's proposal.
func (m *Module) ProposeOnboarding(ctx context.Context, cmd api.ProposeOnboarding) (api.OnboardingReceipt, error) {
	overrides := map[string][]domain.OnboardingEntry{}
	for module, entries := range cmd.Overrides {
		overrides[module] = entriesIn(entries)
	}
	plan := map[string]domain.ModulePlan{}
	for module, p := range cmd.Plan {
		plan[module] = domain.ModulePlan{CatalogueVersion: p.CatalogueVersion, Entries: entriesIn(p.Entries)}
	}
	r, err := m.onboardings(cmd.TenantID, cmd.CommandID).Propose(ctx, application.ProposeCommand{CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID),
		Onboarding: domain.Onboarding{ID: cmd.OnboardingID, Profile: cmd.Profile, Overrides: overrides, Plan: plan, RiskStatement: cmd.RiskStatement, FinanceStaff: cmd.FinanceStaff,
			SingleAdministrator: cmd.SingleAdministrator}})
	return onboardingReceipt(cmd.CommandID, r, err)
}

// ConfirmOnboarding records the owner's confirmation and acknowledgement.
func (m *Module) ConfirmOnboarding(ctx context.Context, cmd api.ConfirmOnboarding) (api.OnboardingReceipt, error) {
	r, err := m.onboardings(cmd.TenantID, cmd.CommandID).Confirm(ctx, application.ConfirmCommand{CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID),
		OnboardingID: cmd.OnboardingID, Acknowledged: cmd.AcknowledgedText, PlanSHA256: cmd.PlanSHA256, Administrator: cmd.AdministratorID})
	return onboardingReceipt(cmd.CommandID, r, err)
}

// RecordOnboardingModule records a module's change set and state for a confirmed onboarding.
func (m *Module) RecordOnboardingModule(ctx context.Context, cmd api.RecordOnboardingModule) (api.OnboardingReceipt, error) {
	r, err := m.onboardings(cmd.TenantID, cmd.CommandID).RecordModule(ctx, application.ModuleCommand{CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID),
		OnboardingID: cmd.OnboardingID, Module: cmd.Module, ChangeID: cmd.ChangeID, State: cmd.State})
	return onboardingReceipt(cmd.CommandID, r, err)
}

func onboardingView(tenant string, r application.OnboardingRecord) api.Onboarding {
	o := r.Onboarding
	out := api.Onboarding{TenantID: tenant, OnboardingID: o.ID, State: r.State(), Profile: o.Profile, Overrides: map[string][]api.OnboardingEntry{},
		Plan: planOut(o.Plan), PlanSHA256: o.PlanSHA256, RiskStatement: o.RiskStatement,
		FinanceStaff: o.FinanceStaff, SingleAdministrator: o.SingleAdministrator, ProposedBy: o.ProposedBy, ProposedAt: o.ProposedAt,
		Notice: domain.StrictNotice(o.Profile, o.FinanceStaff)}
	for module, entries := range o.Overrides {
		out.Overrides[module] = entriesOut(entries)
	}
	if k := r.Confirmation; k != nil {
		out.ConfirmedBy, out.ConfirmedAt, out.AcknowledgedText, out.AdministratorID = k.By, k.At, k.AcknowledgedText, k.Administrator
	}
	for _, module := range domain.Modules {
		v := api.OnboardingModule{Module: module, State: domain.ModuleStateOf(r.Modules, module)}
		if i := slices.IndexFunc(r.Modules, func(s domain.ModuleState) bool { return s.Module == module }); i >= 0 {
			v.ChangeID = r.Modules[i].ChangeID
		}
		out.Modules = append(out.Modules, v)
	}
	return out
}

// GetOnboarding answers one onboarding.
func (m *Module) GetOnboarding(ctx context.Context, q api.GetOnboarding) (api.Onboarding, error) {
	r, err := m.onboardings(q.TenantID, "").Get(ctx, domain.TenantID(q.TenantID), q.OnboardingID)
	if err != nil {
		return api.Onboarding{}, classify(err)
	}
	return onboardingView(q.TenantID, r), nil
}

// ListOnboardings answers the tenant's onboardings in the order proposed.
func (m *Module) ListOnboardings(ctx context.Context, q api.ListOnboardings) ([]api.Onboarding, error) {
	recs, err := m.onboardings(q.TenantID, "").List(ctx, domain.TenantID(q.TenantID))
	if err != nil {
		return nil, classify(err)
	}
	out := []api.Onboarding{}
	for _, r := range recs {
		out = append(out, onboardingView(q.TenantID, r))
	}
	return out, nil
}
