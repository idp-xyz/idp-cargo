package fin

import (
	"context"
	"fmt"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.100: the impact of a settings change before it takes effect. A holder of ReadProfit, ConfigureSettings or
// ApproveSettings may ask: the answer holds job profits, and a person who proposes or approves a change has to see what
// it does.
func (m *Module) authorizeImpact(ctx context.Context, tenant, actor, subject string) error {
	if actor == "" {
		return classify(application.ErrUnauthenticated)
	}
	var last error
	for _, action := range []string{api.ActionReadProfit, api.ActionConfigureSettings, api.ActionApproveSettings} {
		if last = m.auth.AuthorizeFinAction(ctx, tenant, actor, action, subject); last == nil {
			return nil
		}
	}
	return classify(fmt.Errorf("%w: %v; the impact of a change is told to a holder of ReadProfit, ConfigureSettings or ApproveSettings", application.ErrUnauthorized, last))
}

// GetFinSettingsChangeImpact answers what a change of the settings would change, read-only.
func (m *Module) GetFinSettingsChangeImpact(ctx context.Context, q api.GetFinSettingsChangeImpact) (api.FinSettingsChangeImpact, error) {
	if err := m.authorizeImpact(ctx, q.TenantID, q.ActorID, q.ChangeID); err != nil {
		return api.FinSettingsChangeImpact{}, err
	}
	ask := application.ImpactAsk{ChangeID: q.ChangeID, Actor: q.ActorID}
	for _, e := range q.Entries {
		a := application.SettingEntryAsk{Entry: settings.Entry{Scope: settings.Scope{Kind: e.ScopeKind, ID: e.ScopeID}, Key: e.Key, Value: e.Value}}
		if e.Threshold != nil {
			minor := e.Threshold.Minor
			a.Param, a.Currency = &minor, e.Threshold.Currency
		}
		ask.Entries = append(ask.Entries, a)
	}
	got, err := m.deps(q.TenantID, "").SettingsChangeImpact(ctx, domain.TenantID(q.TenantID), ask)
	if err != nil {
		return api.FinSettingsChangeImpact{}, classify(err)
	}
	out := api.FinSettingsChangeImpact{TenantID: q.TenantID, ChangeID: q.ChangeID, AsOf: got.At, VersionNow: got.VersionNow, VersionAfter: got.VersionAfter,
		NotAssessed: got.NotAssessed}
	for _, e := range got.Entities {
		money := func(p *int64) *kernel.Money {
			if p == nil {
				return nil
			}
			return &kernel.Money{Currency: e.Currency, Minor: *p}
		}
		v := api.FinImpactEntity{LegalEntityID: e.LegalEntityID}
		for _, c := range e.Changes {
			v.Changes = append(v.Changes, api.FinItemChange{Key: c.Key, From: c.From, To: c.To, FromThreshold: money(c.FromParam), ToThreshold: money(c.ToParam)})
		}
		out.Entities = append(out.Entities, v)
	}
	for _, p := range got.StalePreviews {
		out.StalePreviews = append(out.StalePreviews, api.FinImpactPreview{PoolID: p.PoolID, LegalEntityID: p.LegalEntityID, PreviewNo: p.PreviewNo, PreviewedBy: p.By,
			PreviewedAt: p.At, SettingsVersion: p.SettingsVersion})
	}
	allowed := func(b bool) string { return map[bool]string{true: "ALLOWED", false: "REFUSED"}[b] }
	for _, c := range got.Closes {
		out.Closes = append(out.Closes, api.FinImpactClose{PoolID: c.PoolID, LegalEntityID: c.LegalEntityID, Now: allowed(c.NowAllowed), After: allowed(!c.NowAllowed), Refusal: c.Refusal})
	}
	for _, j := range got.Jobs {
		money := func(n int64) kernel.Money { return kernel.Money{Currency: kernel.CurrencyCode(j.Currency), Minor: n} }
		out.Jobs = append(out.Jobs, api.FinImpactJob{JobID: j.JobID, ShipmentID: j.ShipmentID, LegalEntityID: j.LegalEntityID, ProfitNow: money(j.Now), ProfitAfter: money(j.After),
			BasisNow: j.BasisNow, BasisAfter: j.BasisAfter})
	}
	for _, x := range got.Defaults {
		out.Defaults = append(out.Defaults, api.FinImpactDefault{PoolID: x.PoolID, LegalEntityID: x.LegalEntityID, LineNo: x.LineNo, Houses: x.Houses,
			Change: map[bool]string{true: "WOULD_BE_DEFAULTED", false: "WOULD_STOP_BEING_DEFAULTED"}[x.Defaulted]})
	}
	for _, c := range got.Carried {
		money := func(n int64) kernel.Money { return kernel.Money{Currency: c.Currency, Minor: n} }
		v := api.FinImpactCarried{PoolID: c.PoolID, LegalEntityID: c.LegalEntityID, LineNo: c.LineNo, Now: c.Now, After: c.After}
		for _, h := range c.Houses {
			v.Houses = append(v.Houses, api.FinImpactCarriedHouse{ShipmentID: h.ShipmentID, LossNow: money(h.LossNow), ExceptionNow: money(h.ExceptionNow),
				LossAfter: money(h.LossAfter), ExceptionAfter: money(h.ExceptionAfter), AfterKnown: h.Known})
		}
		out.Carried = append(out.Carried, v)
	}
	for _, x := range got.OneStep {
		out.OneStep = append(out.OneStep, api.FinImpactOneStep{PoolID: x.PoolID, LegalEntityID: x.LegalEntityID,
			Change: map[bool]string{true: "BECOMES_POSSIBLE", false: "BECOMES_REFUSED"}[x.AfterAble], Refusal: x.Refusal})
	}
	for _, c := range got.Checks {
		out.Checks = append(out.Checks, api.FinImpactCheck{PoolID: c.PoolID, LegalEntityID: c.LegalEntityID, PreviewNo: c.PreviewNo, Rule: c.Rule, LineNo: c.LineNo,
			ShipmentID: c.ShipmentID, Now: c.Now, After: c.After, Note: c.Note})
	}
	return out, nil
}
