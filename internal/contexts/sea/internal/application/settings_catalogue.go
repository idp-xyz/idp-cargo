package application

import (
	"fmt"
	"time"

	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
)

// SEA's settings (V2.100, the configuration of the rules, design sections 3 and 5): the catalogue, which has one item,
// S1, and the chain of scopes. FIN's settings (fin/internal/application/settings_catalogue.go) are the pattern, copied
// here so that SEA depends on no other context; what the two could share later is named in the README.

// Events of the settings, with the change set as subject; an approval carries the version it made as SettingsVersion.
const (
	eventSettingsProposed  = "SeaSettingsChangeProposed.v1"
	eventSettingsApproved  = "SeaSettingsChangeApproved.v1"
	eventSettingsCancelled = "SeaSettingsChangeCancelled.v1"
	eventSettingsRefused   = "SeaSettingsChangeRefused.v1"
	eventDeclared          = "SeaSingleAdministratorDeclared.v1"
	eventSelfReviewed      = "SeaSelfApprovalReviewed.v1"
)

// SettingS1 is the key of S1, "a carrier report that contradicts SEA's records".
const SettingS1 = "sea.S1"

// SeaHierarchy is SEA's chain of scopes: a legal entity, then the tenant. A branch and a transport mode would be added
// here, as scopes between the two or before them, and the rows already stored name their scopes by kind and ID only.
func SeaHierarchy(s settings.Scope) (settings.Chain, error) {
	switch {
	case s == settings.TenantScope:
		return settings.Chain{settings.TenantScope}, nil
	case s.Kind == settings.KindLegalEntity && s.ID != "" && len(s.ID) <= 128:
		return settings.Chain{s, settings.TenantScope}, nil
	}
	return nil, fmt.Errorf("a scope of SEA's settings is the company (no ID) or a legal entity (with its ID), not %s %q", s.Kind, s.ID)
}

// SeaSettingsCatalogue is the catalogue of SEA's settings, from strict to loose, the standard profile's value as the
// default. S1 is an internal control: SUPERVISOR_ONLY adds a second permission to the release of a contradiction that
// WITH_REASON leaves to whoever may release it. It has no parameter and no rule across items.
func SeaSettingsCatalogue() *settings.Catalogue {
	return &settings.Catalogue{Context: "sea", Hierarchy: SeaHierarchy, Items: []settings.Item{{
		Key: SettingS1, TitleZH: "承运人报告与系统记录矛盾时", TitleEN: "A carrier report that contradicts SEA's records", Control: true,
		Explanation: "Whether the release of a conflict between a carrier's loading report and SEA's own records needs a supervisor, or whoever may release a conflict does so with a reason. " +
			"The conflict blocks the final declaration either way; the value says who may clear it. A release already made keeps its validity when the value changes.",
		Default: sea.S1WithReason, Kinds: []string{settings.KindTenant, settings.KindLegalEntity},
		Values: []settings.Value{
			{ID: sea.S1SupervisorOnly, TitleZH: "拦住，只有主管能放行", TitleEN: "blocked; only a supervisor may release it"},
			{ID: sea.S1WithReason, TitleZH: "拦住，写明原因后放行", TitleEN: "blocked; released with a reason"},
		}}}}
}

// SettingsEvents are the events of one settings command, carried as the aggregate of its transaction: identifiers and
// versions only, with the actor ids kept in the rows, as SEA's other events.
type SettingsEvents struct{ events []sea.Event }

// SettingsProposed is the events of a proposal: it was proposed, and approved on the spot when it only tightens.
func SettingsProposed(tenant sea.TenantID, cs settings.ChangeSet) *SettingsEvents {
	out := &SettingsEvents{events: []sea.Event{{Type: eventSettingsProposed, At: cs.At.UTC(), TenantID: tenant, SubjectID: cs.ID}}}
	if cs.Approval != nil {
		out.events = append(out.events, sea.Event{Type: eventSettingsApproved, At: cs.Approval.At.UTC(), TenantID: tenant, SubjectID: cs.ID,
			SettingsVersion: cs.Approval.Version})
	}
	return out
}

// SettingsApproved is the event of an approval, with the version it made.
func SettingsApproved(tenant sea.TenantID, id string, a settings.Approval) *SettingsEvents {
	return &SettingsEvents{events: []sea.Event{{Type: eventSettingsApproved, At: a.At.UTC(), TenantID: tenant, SubjectID: id, SettingsVersion: a.Version}}}
}

// SettingsCancelled is the event of a cancellation.
func SettingsCancelled(tenant sea.TenantID, id string, c settings.Cancellation) *SettingsEvents {
	return &SettingsEvents{events: []sea.Event{{Type: eventSettingsCancelled, At: c.At.UTC(), TenantID: tenant, SubjectID: id}}}
}

// SettingsRefused is the event of an approver's refusal.
func SettingsRefused(tenant sea.TenantID, id string, c settings.Cancellation) *SettingsEvents {
	return &SettingsEvents{events: []sea.Event{{Type: eventSettingsRefused, At: c.At.UTC(), TenantID: tenant, SubjectID: id}}}
}

func (s *SettingsEvents) Pending() []sea.Event { return append([]sea.Event(nil), s.events...) }
func (s *SettingsEvents) Acknowledge()         { s.events = nil }

// SingleAdministratorDeclared is the event of a declaration, with the onboarding as its subject.
func SingleAdministratorDeclared(tenant sea.TenantID, onboarding string, at time.Time) *SettingsEvents {
	return &SettingsEvents{events: []sea.Event{{Type: eventDeclared, At: at.UTC(), TenantID: tenant, SubjectID: onboarding}}}
}

// SelfApprovalReviewed is the event of the review of a self-approved change set, with the change set as its subject.
func SelfApprovalReviewed(tenant sea.TenantID, id string, at time.Time) *SettingsEvents {
	return &SettingsEvents{events: []sea.Event{{Type: eventSelfReviewed, At: at.UTC(), TenantID: tenant, SubjectID: id}}}
}
