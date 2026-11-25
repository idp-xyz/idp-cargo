package application

import (
	"fmt"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
)

// FIN's settings (step C1a of the configuration of the rules): the catalogue of the first items, the chain of scopes
// and the rule across items. Since V2.97 FIN's rules read F1, F2, F3, F6 and F7 (RuleSettingsOf), and since V2.98 F4, F8
// and F9 too.

// Events of the settings, with the change set as subject; an approval carries the version it made as Version.
const (
	eventSettingsProposed  = "FinSettingsChangeProposed.v1"
	eventSettingsApproved  = "FinSettingsChangeApproved.v1"
	eventSettingsCancelled = "FinSettingsChangeCancelled.v1"
	eventSettingsRefused   = "FinSettingsChangeRefused.v1"
)

// Keys, and the values the rules of the cross-item check and the later steps name.
const (
	SettingF1 = "fin.F1"
	SettingF2 = "fin.F2"
	SettingF3 = "fin.F3"
	SettingF4 = "fin.F4"
	SettingF6 = "fin.F6"
	SettingF7 = "fin.F7"
	SettingF8 = "fin.F8"
	SettingF9 = "fin.F9"

	F1ThresholdValue    = fin.F1AboveThreshold
	F3OnlyWhenAmount    = fin.F3OnlyWhenAmountMoves
	F8DefaultToRemained = fin.F8DefaultToRemaining
)

// FinHierarchy is FIN's chain of scopes: a legal entity, then the tenant. A branch and a transport mode would be added
// here, as scopes between the two or before them, and the rows already stored name their scopes by kind and ID only.
func FinHierarchy(s settings.Scope) (settings.Chain, error) {
	switch {
	case s == settings.TenantScope:
		return settings.Chain{settings.TenantScope}, nil
	case s.Kind == settings.KindLegalEntity && s.ID != "" && len(s.ID) <= 128:
		return settings.Chain{s, settings.TenantScope}, nil
	}
	return nil, fmt.Errorf("a scope of FIN's settings is the company (no ID) or a legal entity (with its ID), not %s %q", s.Kind, s.ID)
}

var finKinds = []string{settings.KindTenant, settings.KindLegalEntity}

// finItem builds an item of FIN set at the company and at a legal entity.
func finItem(key, zh, en string, control bool, explanation string, def string, values ...settings.Value) settings.Item {
	return settings.Item{Key: key, TitleZH: zh, TitleEN: en, Values: values, Default: def, Kinds: finKinds, Control: control, Explanation: explanation}
}

// FinSettingsCatalogue is the catalogue of FIN's settings: the items of the design's section 5 that concern the
// apportionment of a consolidation's costs, from strict to loose, the standard profile's value as the default. All are
// internal controls: each relaxes a check that another person or a voucher makes. The finance policy (E1 to E4) is
// listed as reference data (V2.100).
func FinSettingsCatalogue() *settings.Catalogue {
	threshold := &settings.Param{Name: "threshold", TitleZH: "金额阈值（本币）", TitleEN: "amount threshold (functional currency)",
		Kinds: []string{settings.KindLegalEntity}, LooserWhenHigher: true}
	c := &settings.Catalogue{Context: "fin", Hierarchy: FinHierarchy, Rules: []settings.Rule{f8NeedsF3}, Reference: finReference, Items: []settings.Item{
		finItem(SettingF1, "职责分离", "Segregation of duties", true,
			"Whether the person who asked for a container to be opened, or who removed or transferred the house, may confirm the attribution of its former share, and whether the person who asked for a rework may submit the first apportionment of a line after it; both rules are governed by this item. Under a threshold the first rule measures the line's local cost, because that submission redistributes the whole line, and the second the former share of each attribution.",
			"NOT_ALLOWED",
			settings.Value{ID: "NOT_ALLOWED", TitleZH: "不能", TitleEN: "may not"},
			settings.Value{ID: F1ThresholdValue, TitleZH: "金额超过阈值时不能", TitleEN: "may not above a threshold", Param: threshold},
			settings.Value{ID: "UNRESTRICTED", TitleZH: "不限制", TitleEN: "unrestricted"}),
		finItem(SettingF2, "确认人怎么认定", "Who counts as the confirmer", true,
			"Whether the previewer is checked as well as the submitter against the persons who may not confirm.",
			"SUBMITTER_ONLY",
			settings.Value{ID: "SUBMITTER_AND_PREVIEWER", TitleZH: "提交人和预览人都查", TitleEN: "submitter and previewer"},
			settings.Value{ID: "SUBMITTER_ONLY", TitleZH: "只查提交人", TitleEN: "submitter only"}),
		finItem(SettingF3, "确认归属是否附凭证", "Voucher for a confirmed attribution", true,
			"Whether every attribution needs a voucher, or only one that moves an amount onto the cargo or onto the original consolidation.",
			"EVERY_ATTRIBUTION",
			settings.Value{ID: "EVERY_ATTRIBUTION", TitleZH: "每一条都要", TitleEN: "every attribution"},
			settings.Value{ID: F3OnlyWhenAmount, TitleZH: "只在有金额落到这票货或原集拼时才要", TitleEN: "only when an amount moves"}),
		finItem(SettingF4, "分摊是否先预览再提交", "Preview before an apportionment", true,
			"Whether every apportionment is previewed and then submitted, or one may be made in one step when no house was opened or left.",
			"ALL_PREVIEWED",
			settings.Value{ID: "ALL_PREVIEWED", TitleZH: "所有分摊都要", TitleEN: "every apportionment"},
			settings.Value{ID: "ONE_STEP_WITHOUT_OPENED_OR_LEFT", TitleZH: "没有开封和离开的票时可一步分摊", TitleEN: "one step when no house opened or left"}),
		finItem(SettingF6, "待确认的前分摊在这票货利润里", "A former share awaiting confirmation in the house's profit", true,
			"Whether a former share not yet attributed is shown apart from the profit, or counted in it with a mark.",
			"SEPARATE",
			settings.Value{ID: "SEPARATE", TitleZH: "不算进，单列", TitleEN: "shown apart"},
			settings.Value{ID: "INCLUDED_MARKED", TitleZH: "算进，另外标注", TitleEN: "included and marked"}),
		finItem(SettingF7, "待归因是否挡关池", "Whether a pending cause blocks closing the pool", true,
			"Whether an exception cost whose party is still pending holds back the close of the pool.",
			"DOES_NOT_BLOCK",
			settings.Value{ID: "BLOCKS", TitleZH: "挡", TitleEN: "blocks"},
			settings.Value{ID: "DOES_NOT_BLOCK", TitleZH: "不挡", TitleEN: "does not block"}),
		finItem(SettingF8, "没写归属时", "When no attribution is stated", true,
			"Whether a missing attribution is refused with the default shown, or the whole is apportioned again over the cargo that remains.",
			"REJECT_WITH_DEFAULT",
			settings.Value{ID: "REJECT_WITH_DEFAULT", TitleZH: "拒绝，给出默认值", TitleEN: "refuse, showing the default"},
			settings.Value{ID: F8DefaultToRemained, TitleZH: "默认全部由剩下的货重摊", TitleEN: "default to the cargo that remains"}),
		finItem(SettingF9, "已确认的归属，费用金额变了以后", "A confirmed attribution when the cost changes", true,
			"Whether the attribution is confirmed again, or carried at its original proportion or its original amount.",
			"KEEP_AMOUNT",
			settings.Value{ID: "RECONFIRM", TitleZH: "要求重新确认", TitleEN: "confirm again"},
			settings.Value{ID: "KEEP_RATIO", TitleZH: "按原比例沿用", TitleEN: "keep the proportion"},
			settings.Value{ID: "KEEP_AMOUNT", TitleZH: "按原金额沿用", TitleEN: "keep the amount"}),
	}}
	return c
}

// finReference is E1 to E4 of the design, the finance policy FIN keeps for each legal entity with its own history:
// listed
// in the catalogue, changed by their own commands and not by the settings commands. Only E1's loosening needs a second
// person (since V2.100); a rate, a tax treatment and a series are facts with a period of their own, and a new one
// starts after the last.
var finReference = []settings.Reference{
	{Key: "fin.E1", TitleZH: "成本差异阈值", TitleEN: "Cost variance threshold",
		Governs:   "the variance of a final match against its accrual, beyond which the invoice waits for approval",
		ChangedBy: "SetCostVarianceThreshold, ApproveCostVarianceThreshold and RefuseCostVarianceThreshold, in the legal entity's finance policy",
		Approval:  true, ApprovalNote: "a looser version (a higher amount or rate, or the first) takes effect when another holder of ApproveSettings approves it"},
	{Key: "fin.E2", TitleZH: "汇率", TitleEN: "Exchange rates",
		Governs:   "the rate each currency is converted into the functional currency at, by type and period",
		ChangedBy: "RecordExchangeRate and EndExchangeRate, in the legal entity's finance policy",
		Approval:  false, ApprovalNote: "a rate is a fact with its own period and source; a rate used is not changed"},
	{Key: "fin.E3", TitleZH: "各费用代码的税务处理", TitleEN: "Tax treatment of charge codes",
		Governs:   "whether a charge code is exempt, a pass-through disbursement or taxable, and at what rate",
		ChangedBy: "SetChargeTaxTreatment, in the legal entity's finance policy",
		Approval:  false, ApprovalNote: "a treatment is a fact in force from a date; a new version starts after the last"},
	{Key: "fin.E4", TitleZH: "发票号段", TitleEN: "Invoice number series",
		Governs:   "the template and first number of the debit notes' and credit notes' numbers",
		ChangedBy: "RegisterInvoiceSeries, in the legal entity's finance policy",
		Approval:  false, ApprovalNote: "a series is registered once and its numbers are never reused"},
}

// f8NeedsF3 is the rule across F8 and F3: defaulting a missing attribution to the remaining cargo only with a voucher
// asked for only when an amount moves, so that an attribution made by default needs none.
func f8NeedsF3(cfg settings.Config) error {
	if cfg[SettingF8].Value == F8DefaultToRemained && cfg[SettingF3].Value != F3OnlyWhenAmount {
		return settings.Refuse(settings.ErrRule, "%s %s goes only with %s %s, because an attribution made by default has no voucher; set both in one change "+
			"set, or keep %s at REJECT_WITH_DEFAULT", SettingF8, F8DefaultToRemained, SettingF3, F3OnlyWhenAmount, SettingF8)
	}
	return nil
}

// SettingsEvents are the events of one settings command, carried as the aggregate of its transaction: identifiers and
// versions only, with the actor ids kept in the rows, as FIN's other events.
type SettingsEvents struct{ events []fin.Event }

// SettingsProposed is the events of a proposal: it was proposed, and approved on the spot when it only tightens.
func SettingsProposed(tenant fin.TenantID, cs settings.ChangeSet) *SettingsEvents {
	out := &SettingsEvents{events: []fin.Event{{Type: eventSettingsProposed, At: cs.At.UTC(), TenantID: tenant, SubjectID: cs.ID}}}
	if cs.Approval != nil {
		out.events = append(out.events, fin.Event{Type: eventSettingsApproved, At: cs.Approval.At.UTC(), TenantID: tenant, SubjectID: cs.ID, Version: cs.Approval.Version})
	}
	return out
}

// SettingsApproved is the event of an approval, with the version it made.
func SettingsApproved(tenant fin.TenantID, id string, a settings.Approval) *SettingsEvents {
	return &SettingsEvents{events: []fin.Event{{Type: eventSettingsApproved, At: a.At.UTC(), TenantID: tenant, SubjectID: id, Version: a.Version}}}
}

// SettingsCancelled is the event of a cancellation.
func SettingsCancelled(tenant fin.TenantID, id string, c settings.Cancellation) *SettingsEvents {
	return &SettingsEvents{events: []fin.Event{{Type: eventSettingsCancelled, At: c.At.UTC(), TenantID: tenant, SubjectID: id}}}
}

// SettingsRefused is the event of an approver's refusal.
func SettingsRefused(tenant fin.TenantID, id string, c settings.Cancellation) *SettingsEvents {
	return &SettingsEvents{events: []fin.Event{{Type: eventSettingsRefused, At: c.At.UTC(), TenantID: tenant, SubjectID: id}}}
}

func (s *SettingsEvents) Pending() []fin.Event { return append([]fin.Event(nil), s.events...) }
func (s *SettingsEvents) Acknowledge()         { s.events = nil }
