package application

import (
	"strings"
	"testing"
	"time"

	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
)

// SEA's catalogue is a valid catalogue with the one item of the design's section 5 that concerns SEA: S1, from strict
// to loose, the standard profile's value (the one in bold there) as the default, an internal control set at the company
// and at a legal entity, with no parameter.
func TestSeaSettingsCatalogue(t *testing.T) {
	cat := SeaSettingsCatalogue()
	if err := cat.Check(); err != nil {
		t.Fatal(err)
	}
	if len(cat.Items) != 1 || len(cat.Rules) != 0 {
		t.Fatalf("%d items and %d rules, want S1 alone", len(cat.Items), len(cat.Rules))
	}
	it := cat.Items[0]
	if it.Key != SettingS1 || SettingS1 != "sea.S1" || it.Default != sea.S1WithReason || !it.Control || it.TitleZH != "承运人报告与系统记录矛盾时" ||
		strings.Join(it.Kinds, " ") != settings.KindTenant+" "+settings.KindLegalEntity {
		t.Fatalf("S1: %+v", it)
	}
	if len(it.Values) != 2 || it.Values[0].ID != sea.S1SupervisorOnly || it.Values[1].ID != sea.S1WithReason || it.Values[0].Param != nil || it.Values[1].Param != nil ||
		it.Values[0].TitleZH != "拦住，只有主管能放行" || it.Values[1].TitleZH != "拦住，写明原因后放行" {
		t.Fatalf("the values of S1: %+v", it.Values)
	}
	if !strings.Contains(cat.Table(), "| sea.S1 |") || !strings.Contains(cat.Table(), "**WITH_REASON") {
		t.Errorf("the table of the catalogue:\n%s", cat.Table())
	}
}

func TestSeaHierarchy(t *testing.T) {
	if c, err := SeaHierarchy(settings.TenantScope); err != nil || len(c) != 1 || c[0] != settings.TenantScope {
		t.Fatalf("the company: %v, %v", c, err)
	}
	le := settings.Scope{Kind: settings.KindLegalEntity, ID: "LE1"}
	if c, err := SeaHierarchy(le); err != nil || len(c) != 2 || c[0] != le || c[1] != settings.TenantScope {
		t.Fatalf("a legal entity: %v, %v", c, err)
	}
	if c, err := SeaHierarchy(settings.Scope{Kind: settings.KindLegalEntity, ID: strings.Repeat("x", 128)}); err != nil || len(c) != 2 {
		t.Fatalf("a legal entity ID of 128 bytes: %v, %v", c, err)
	}
	for _, s := range []settings.Scope{{Kind: settings.KindLegalEntity}, {Kind: settings.KindTenant, ID: "t"}, {Kind: "BRANCH", ID: "b"}, {},
		{Kind: settings.KindLegalEntity, ID: strings.Repeat("x", 129)}} {
		if _, err := SeaHierarchy(s); err == nil {
			t.Errorf("scope %+v is not one of SEA's", s)
		}
	}
}

// The events of a proposal, an approval and a cancellation carry the change set as subject, the version an approval
// made and no names: a proposal that only tightens is approved at once, so it has both events.
func TestSeaSettingsEvents(t *testing.T) {
	at := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	waiting := settings.ChangeSet{ID: "c1", By: "ann", At: at}
	e := SettingsProposed("t1", waiting).Pending()
	if len(e) != 1 || e[0].Type != "SeaSettingsChangeProposed.v1" || e[0].SubjectID != "c1" || e[0].SettingsVersion != 0 || !e[0].At.Equal(at) {
		t.Fatalf("a proposal waiting for approval: %+v", e)
	}
	auto := waiting
	auto.Approval = &settings.Approval{By: "ann", At: at, Version: 3, InForce: at, Auto: true}
	e = SettingsProposed("t1", auto).Pending()
	if len(e) != 2 || e[1].Type != "SeaSettingsChangeApproved.v1" || e[1].SettingsVersion != 3 || e[1].SubjectID != "c1" || e[1].TenantID != "t1" {
		t.Fatalf("a proposal approved at once: %+v", e)
	}
	e = SettingsApproved("t1", "c1", settings.Approval{By: "bob", At: at, Version: 4}).Pending()
	if len(e) != 1 || e[0].Type != "SeaSettingsChangeApproved.v1" || e[0].SettingsVersion != 4 {
		t.Fatalf("an approval: %+v", e)
	}
	cancelled := SettingsCancelled("t1", "c1", settings.Cancellation{By: "bob", At: at, Reason: "no"})
	if e = cancelled.Pending(); len(e) != 1 || e[0].Type != "SeaSettingsChangeCancelled.v1" || e[0].SubjectID != "c1" || e[0].Reason != "" {
		t.Fatalf("a cancellation: %+v", e)
	}
	refused := SettingsRefused("t1", "c1", settings.Cancellation{By: "bob", At: at, Reason: "no", Refused: true})
	if e = refused.Pending(); len(e) != 1 || e[0].Type != "SeaSettingsChangeRefused.v1" || e[0].SubjectID != "c1" {
		t.Fatalf("a refusal: %+v", e)
	}
	cancelled.Acknowledge()
	if len(cancelled.Pending()) != 0 {
		t.Fatal("acknowledged events are pending")
	}
}

// S1 has no rule across items and takes no parameter: a value with an amount, an unknown value and a scope of another
// kind are refused by the ledger, each naming the way out.
func TestSeaLedgerRefusesWhatTheCatalogueDoesNotHave(t *testing.T) {
	cat := SeaSettingsCatalogue()
	at := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	le := settings.Scope{Kind: settings.KindLegalEntity, ID: "LE"}
	amount := int64(5)
	for name, c := range map[string]struct {
		entry settings.Entry
		in    string
	}{
		"a parameter":      {settings.Entry{Scope: le, Key: SettingS1, Value: sea.S1WithReason, Param: &amount}, "takes no parameter"},
		"an unknown value": {settings.Entry{Scope: le, Key: SettingS1, Value: "MAYBE"}, "SUPERVISOR_ONLY, WITH_REASON"},
		"an unknown item":  {settings.Entry{Scope: le, Key: "sea.S2", Value: "X"}, "not an item of the sea catalogue"},
		"another scope":    {settings.Entry{Scope: settings.Scope{Kind: "BRANCH", ID: "b"}, Key: SettingS1, Value: sea.S1WithReason}, "choose a scope"},
	} {
		_, err := settings.NewLedger(nil).Propose(cat, settings.Proposal{ID: "c1", Entries: []settings.Entry{c.entry}, Reason: "r", By: "ann", At: at})
		if err == nil || !strings.Contains(err.Error(), c.in) {
			t.Errorf("%s: %v, want a refusal that says %q", name, err, c.in)
		}
	}
}
