package application

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/foundation/settings"
)

// FIN's catalogue is a valid catalogue, and each item has the values of the design's section 5, from strict to loose,
// with the standard profile's value (the one in bold there) as the default, and is set at the company and at a legal
// entity.
func TestFinSettingsCatalogue(t *testing.T) {
	cat := FinSettingsCatalogue()
	if err := cat.Check(); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		key, def string
		values   []string
	}{
		{"fin.F1", "NOT_ALLOWED", []string{"NOT_ALLOWED", "NOT_ABOVE_THRESHOLD", "UNRESTRICTED"}},
		{"fin.F2", "SUBMITTER_ONLY", []string{"SUBMITTER_AND_PREVIEWER", "SUBMITTER_ONLY"}},
		{"fin.F3", "EVERY_ATTRIBUTION", []string{"EVERY_ATTRIBUTION", "ONLY_WHEN_AMOUNT_MOVES"}},
		{"fin.F4", "ALL_PREVIEWED", []string{"ALL_PREVIEWED", "ONE_STEP_WITHOUT_OPENED_OR_LEFT"}},
		{"fin.F6", "SEPARATE", []string{"SEPARATE", "INCLUDED_MARKED"}},
		{"fin.F7", "DOES_NOT_BLOCK", []string{"BLOCKS", "DOES_NOT_BLOCK"}},
		{"fin.F8", "REJECT_WITH_DEFAULT", []string{"REJECT_WITH_DEFAULT", "DEFAULT_TO_REMAINING"}},
		{"fin.F9", "KEEP_AMOUNT", []string{"RECONFIRM", "KEEP_RATIO", "KEEP_AMOUNT"}},
	}
	if len(cat.Items) != len(want) {
		t.Fatalf("%d items, want %d (F5 and S1 are not in this catalogue)", len(cat.Items), len(want))
	}
	for i, w := range want {
		it := cat.Items[i]
		var ids []string
		for _, v := range it.Values {
			ids = append(ids, v.ID)
		}
		if it.Key != w.key || it.Default != w.def || strings.Join(ids, " ") != strings.Join(w.values, " ") {
			t.Errorf("%s: default %s, values %v; want %s, %v", it.Key, it.Default, ids, w.def, w.values)
		}
		if strings.Join(it.Kinds, " ") != settings.KindTenant+" "+settings.KindLegalEntity || !it.Control {
			t.Errorf("%s is set at %v, control %v", it.Key, it.Kinds, it.Control)
		}
	}
	// only F1's threshold asks for a parameter: an amount for a legal entity, which lets more through when higher
	for _, it := range cat.Items {
		for _, v := range it.Values {
			if (v.Param != nil) != (it.Key == SettingF1 && v.ID == F1ThresholdValue) {
				t.Errorf("%s %s has a parameter: %v", it.Key, v.ID, v.Param != nil)
			}
		}
	}
	p := cat.Items[0].Values[1].Param
	if len(p.Kinds) != 1 || p.Kinds[0] != settings.KindLegalEntity || !p.LooserWhenHigher {
		t.Errorf("F1's threshold: %+v", p)
	}
	if !strings.Contains(cat.Table(), "| fin.F8 |") || !strings.Contains(cat.Table(), "**REJECT_WITH_DEFAULT") {
		t.Errorf("the table of the catalogue:\n%s", cat.Table())
	}
}

// A scope of FIN's settings is the company or a legal entity with its ID; the chain of a legal entity is itself and the
// company.
func TestFinHierarchy(t *testing.T) {
	if c, err := FinHierarchy(settings.TenantScope); err != nil || len(c) != 1 || c[0] != settings.TenantScope {
		t.Fatalf("the company: %v, %v", c, err)
	}
	le := settings.Scope{Kind: settings.KindLegalEntity, ID: "LE1"}
	if c, err := FinHierarchy(le); err != nil || len(c) != 2 || c[0] != le || c[1] != settings.TenantScope {
		t.Fatalf("a legal entity: %v, %v", c, err)
	}
	if c, err := FinHierarchy(settings.Scope{Kind: settings.KindLegalEntity, ID: strings.Repeat("x", 128)}); err != nil || len(c) != 2 {
		t.Fatalf("a legal entity ID of 128 bytes: %v, %v", c, err)
	}
	for _, s := range []settings.Scope{{Kind: settings.KindLegalEntity}, {Kind: settings.KindTenant, ID: "t"}, {Kind: "BRANCH", ID: "b"}, {},
		{Kind: settings.KindLegalEntity, ID: strings.Repeat("x", 129)}} {
		if _, err := FinHierarchy(s); err == nil {
			t.Errorf("scope %+v is not one of FIN's", s)
		}
	}
}

// F8's default to the remaining cargo goes only with F3's voucher asked for only when an amount moves, in the same
// change set or already in force, for the company and for a legal entity that inherits F3; the refusal names the way
// out.
func TestF8NeedsF3(t *testing.T) {
	cat := FinSettingsCatalogue()
	le := settings.Scope{Kind: settings.KindLegalEntity, ID: "LE1"}
	at := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	ledger := settings.NewLedger(nil)
	propose := func(id string, entries ...settings.Entry) error {
		cs, err := ledger.Propose(cat, settings.Proposal{ID: id, Entries: entries, Reason: "r", By: "ann", At: at})
		if err != nil {
			return err
		}
		ledger = settings.NewLedger(append(ledger.Sets(), cs))
		if cs.Approval != nil {
			return nil
		}
		a, err := ledger.Approve(cat, id, "bob", at)
		if err != nil {
			t.Fatal(err)
		}
		cs.Approval = &a
		ledger = settings.NewLedger(append(ledger.Sets()[:len(ledger.Sets())-1], cs))
		return nil
	}
	f8 := settings.Entry{Scope: le, Key: SettingF8, Value: F8DefaultToRemained}
	err := propose("a", f8)
	if !errors.Is(err, settings.ErrRule) || !strings.Contains(err.Error(), "fin.F8") || !strings.Contains(err.Error(), "fin.F3") || !strings.Contains(err.Error(), "one change set") {
		t.Fatalf("F8 without F3: %v", err)
	}
	if err = propose("b", f8, settings.Entry{Scope: le, Key: SettingF3, Value: F3OnlyWhenAmount}); err != nil {
		t.Fatalf("F8 with F3 in the same change set: %v", err)
	}
	// F3 cannot go back to a voucher for every attribution while the legal entity defaults to the remaining cargo
	err = propose("c", settings.Entry{Scope: le, Key: SettingF3, Value: "EVERY_ATTRIBUTION"})
	if !errors.Is(err, settings.ErrRule) {
		t.Fatalf("F3 back to every attribution under F8: %v", err)
	}
	// a legal entity that inherits the company's F3 ONLY_WHEN_AMOUNT_MOVES may take F8 alone
	ledger = settings.NewLedger(nil)
	if err = propose("d", settings.Entry{Scope: settings.TenantScope, Key: SettingF3, Value: F3OnlyWhenAmount}); err != nil {
		t.Fatal(err)
	}
	if err = propose("e", f8); err != nil {
		t.Fatalf("F8 under the company's F3: %v", err)
	}
	// and the company's F3 cannot go back while a legal entity has F8
	if err = propose("f", settings.Entry{Scope: settings.TenantScope, Key: SettingF3, Value: "EVERY_ATTRIBUTION"}); !errors.Is(err, settings.ErrRule) ||
		!strings.Contains(err.Error(), "legal entity LE1") {
		t.Fatalf("the company's F3 back under a legal entity's F8: %v", err)
	}
}

// The events of a proposal, an approval and a cancellation carry the change set as subject, the version an approval
// made and no names: a proposal that only tightens is approved at once, so it has both events.
func TestSettingsEvents(t *testing.T) {
	at := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	waiting := settings.ChangeSet{ID: "c1", By: "ann", At: at}
	e := SettingsProposed("t1", waiting).Pending()
	if len(e) != 1 || e[0].Type != "FinSettingsChangeProposed.v1" || e[0].SubjectID != "c1" || e[0].Version != 0 || !e[0].At.Equal(at) {
		t.Fatalf("a proposal waiting for approval: %+v", e)
	}
	auto := waiting
	auto.Approval = &settings.Approval{By: "ann", At: at, Version: 3, InForce: at, Auto: true}
	e = SettingsProposed("t1", auto).Pending()
	if len(e) != 2 || e[1].Type != "FinSettingsChangeApproved.v1" || e[1].Version != 3 || e[1].SubjectID != "c1" || e[1].TenantID != "t1" {
		t.Fatalf("a proposal approved at once: %+v", e)
	}
	e = SettingsApproved("t1", "c1", settings.Approval{By: "bob", At: at, Version: 4}).Pending()
	if len(e) != 1 || e[0].Type != "FinSettingsChangeApproved.v1" || e[0].Version != 4 {
		t.Fatalf("an approval: %+v", e)
	}
	cancelled := SettingsCancelled("t1", "c1", settings.Cancellation{By: "bob", At: at, Reason: "no"})
	if e = cancelled.Pending(); len(e) != 1 || e[0].Type != "FinSettingsChangeCancelled.v1" || e[0].DebtorPartyID != "" || e[0].LegalEntityID != "" {
		t.Fatalf("a cancellation: %+v", e)
	}
	refused := SettingsRefused("t1", "c1", settings.Cancellation{By: "bob", At: at, Reason: "no", Refused: true})
	if e = refused.Pending(); len(e) != 1 || e[0].Type != "FinSettingsChangeRefused.v1" || e[0].SubjectID != "c1" {
		t.Fatalf("a refusal: %+v", e)
	}
	cancelled.Acknowledge()
	if len(cancelled.Pending()) != 0 {
		t.Fatal("acknowledged events are pending")
	}
}
