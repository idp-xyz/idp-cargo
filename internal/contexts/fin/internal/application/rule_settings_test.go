package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
)

// FIN V2.97: what the rules read of the settings in force for a legal entity at FIN's clock.

// ledgerStore answers a ledger and notes, in the order they happen, the lock and the clock's reads.
type ledgerStore struct {
	SettingsStore
	ledger *settings.Ledger
	trace  *[]string
}

func (s ledgerStore) LockShared(context.Context, fin.TenantID) error {
	*s.trace = append(*s.trace, "lock")
	return nil
}

func (s ledgerStore) Load(context.Context, fin.TenantID) (*settings.Ledger, error) {
	return s.ledger, nil
}
func (s ledgerStore) Read(context.Context, fin.TenantID) (*settings.Ledger, error) {
	return s.ledger, nil
}

type fixedClock struct {
	now   time.Time
	trace *[]string
}

func (c fixedClock) Now() time.Time {
	if c.trace != nil {
		*c.trace = append(*c.trace, "clock")
	}
	return c.now
}

// the domain's names of the values are the catalogue's
func TestTheRuleSettingsNamesAreTheCataloguesValues(t *testing.T) {
	cat := FinSettingsCatalogue()
	for _, c := range []struct{ key, value string }{{SettingF1, fin.F1Unrestricted}, {SettingF1, fin.F1AboveThreshold}, {SettingF2, fin.F2SubmitterAndPreviewer},
		{SettingF3, fin.F3OnlyWhenAmountMoves}, {SettingF4, fin.F4OneStep}, {SettingF6, fin.F6IncludedMarked}, {SettingF7, fin.F7Blocks},
		{SettingF8, fin.F8DefaultToRemaining}, {SettingF9, fin.F9KeepRatio}, {SettingF9, fin.F9Reconfirm}} {
		it, ok := cat.Item(c.key)
		if !ok {
			t.Fatalf("%s is not in the catalogue", c.key)
		}
		if _, ok = it.Rank(c.value); !ok {
			t.Errorf("%s has no value %s", it.Key, c.value)
		}
	}
}

func TestRuleSettingsOfReadsTheScopeChainAtTheClockWithTheVersion(t *testing.T) {
	cat := FinSettingsCatalogue()
	le := settings.Scope{Kind: settings.KindLegalEntity, ID: "LE1"}
	t0 := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	ledger := settings.NewLedger(nil)
	add := func(id string, by string, at, effective time.Time, entries ...settings.Entry) {
		t.Helper()
		cs, err := ledger.Propose(cat, settings.Proposal{ID: id, Entries: entries, Reason: "r", By: by, At: at, Effective: effective})
		if err != nil {
			t.Fatal(err)
		}
		ledger = settings.NewLedger(append(ledger.Sets(), cs))
		if cs.Approval == nil {
			a, err := ledger.Approve(cat, id, "bob", at)
			if err != nil {
				t.Fatal(err)
			}
			cs.Approval = &a
			ledger = settings.NewLedger(append(ledger.Sets()[:len(ledger.Sets())-1], cs))
		}
	}
	param := int64(30_000)
	add("a", "ann", t0, t0, settings.Entry{Scope: le, Key: SettingF1, Value: F1ThresholdValue, Param: &param},
		settings.Entry{Scope: le, Key: SettingF2, Value: fin.F2SubmitterAndPreviewer}, settings.Entry{Scope: le, Key: SettingF7, Value: fin.F7Blocks})
	add("b", "ann", t0.Add(time.Hour), t0.Add(48*time.Hour), settings.Entry{Scope: le, Key: SettingF3, Value: F3OnlyWhenAmount},
		settings.Entry{Scope: le, Key: SettingF6, Value: fin.F6IncludedMarked})
	now := t0.Add(2 * time.Hour)
	var trace []string
	d := Deps{Settings: ledgerStore{ledger: ledger, trace: &trace}, Clock: fixedClock{now, &trace}}
	got, at, err := d.DecisionSettings(context.Background(), "t1", "LE1")
	if len(trace) != 2 || trace[0] != "lock" || trace[1] != "clock" || !at.Equal(now) {
		t.Fatalf("a decision takes the shared lock, then reads the clock, and answers that time: %v, %s", trace, at)
	}
	// F7 BLOCKS is the strictest value, so setting it needs no second person; F1's threshold and F2 loosen or tighten
	// as the ledger says
	if err != nil {
		t.Fatal(err)
	}
	want := fin.RuleSettings{Version: got.Version, F1: F1ThresholdValue, F1Threshold: 30_000, F2: fin.F2SubmitterAndPreviewer, F7: fin.F7Blocks, F3: "EVERY_ATTRIBUTION", F6: "SEPARATE",
		F4: "ALL_PREVIEWED", F8: "REJECT_WITH_DEFAULT", F9: "KEEP_AMOUNT"}
	if got != want || got.Version == 0 {
		t.Fatalf("now: %+v, want %+v", got, want)
	}
	// the change set that takes effect in two days does not apply before, and applies after, with its version
	d.Clock = fixedClock{now: t0.Add(49 * time.Hour)}
	later, err := d.RuleSettingsOf(context.Background(), "t1", "LE1")
	if err != nil || later.F3 != F3OnlyWhenAmount || later.F6 != fin.F6IncludedMarked || later.Version != got.Version+1 || later.F1Threshold != 30_000 {
		t.Fatalf("in two days: %+v %v", later, err)
	}
	// another legal entity has the company's values: the defaults here
	other, err := d.RuleSettingsOf(context.Background(), "t1", "LE2")
	if err != nil || other.F1 != "NOT_ALLOWED" || other.F7 != "DOES_NOT_BLOCK" || other.Version != later.Version {
		t.Fatalf("another legal entity: %+v %v", other, err)
	}
	// before anything was in force: version 0, the standard profile
	d.Clock = fixedClock{now: t0.Add(-time.Hour)}
	none, err := d.RuleSettingsOf(context.Background(), "t1", "LE1")
	if err != nil || none.Version != 0 || none.F7 != "DOES_NOT_BLOCK" || none.F1 != "NOT_ALLOWED" {
		t.Fatalf("before: %+v %v", none, err)
	}
	// no settings store: the standard profile; no clock with a store: a missing dependency
	if r, err := (Deps{}).RuleSettingsOf(context.Background(), "t1", "LE1"); err != nil || r != (fin.RuleSettings{}) {
		t.Fatalf("no store: %+v %v", r, err)
	}
	if _, err := (Deps{Settings: ledgerStore{ledger: ledger}}).RuleSettingsOf(context.Background(), "t1", "LE1"); !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("no clock: %v", err)
	}
}

// F1's explanation says that it governs both rules that keep a person from confirming their own case.
func TestF1ExplanationSaysItGovernsBothRules(t *testing.T) {
	it, ok := FinSettingsCatalogue().Item(SettingF1)
	if !ok || !strings.Contains(it.Explanation, "both rules") || !strings.Contains(it.Explanation, "first apportionment of a line after") ||
		!strings.Contains(it.Explanation, "former share") {
		t.Fatalf("F1: %+v", it)
	}
}

// A threshold value with no amount is refused, not read as 0 (the ledger refuses it when it is proposed; this is the
// rule's own guard, shown on a ledger built past the ledger's checks).
func TestAThresholdValueWithNoAmountIsNotReadAsZero(t *testing.T) {
	at := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	le := settings.Scope{Kind: settings.KindLegalEntity, ID: "LE1"}
	ledger := settings.NewLedger([]settings.ChangeSet{{ID: "x", By: "ann", At: at, Effective: at, Entries: []settings.Entry{{Scope: le, Key: SettingF1, Value: F1ThresholdValue}},
		Approval: &settings.Approval{By: "bob", At: at, Version: 1, InForce: at}}})
	d := Deps{Settings: ledgerStore{ledger: ledger}, Clock: fixedClock{now: at.Add(time.Hour)}}
	if _, err := d.RuleSettingsOf(context.Background(), "t1", "LE1"); !errors.Is(err, fin.ErrInvalid) || !strings.Contains(err.Error(), "has no threshold") {
		t.Fatalf("%v", err)
	}
}

// FIN V2.98: F4, F8 and F9 are read from the scope chain too, each with its default, and a value set for the legal entity
// is the one handed to the rules.
func TestRuleSettingsOfReadsF4F8AndF9(t *testing.T) {
	cat := FinSettingsCatalogue()
	le := settings.Scope{Kind: settings.KindLegalEntity, ID: "LE1"}
	t0 := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	cs, err := settings.NewLedger(nil).Propose(cat, settings.Proposal{ID: "a", By: "ann", At: t0, Effective: t0, Reason: "r", Entries: []settings.Entry{
		{Scope: le, Key: SettingF3, Value: F3OnlyWhenAmount}, {Scope: le, Key: SettingF4, Value: fin.F4OneStep}, {Scope: le, Key: SettingF8, Value: fin.F8DefaultToRemaining},
		{Scope: le, Key: SettingF9, Value: fin.F9Reconfirm}}})
	if err != nil {
		t.Fatal(err)
	}
	ledger := settings.NewLedger([]settings.ChangeSet{cs})
	a, err := ledger.Approve(cat, "a", "bob", t0)
	if err != nil {
		t.Fatal(err)
	}
	cs.Approval = &a
	d := Deps{Settings: ledgerStore{ledger: settings.NewLedger([]settings.ChangeSet{cs})}, Clock: fixedClock{now: t0.Add(time.Hour)}}
	got, err := d.RuleSettingsOf(context.Background(), "t1", "LE1")
	if err != nil || got.F4 != fin.F4OneStep || got.F8 != fin.F8DefaultToRemaining || got.F9 != fin.F9Reconfirm || got.F3 != F3OnlyWhenAmount {
		t.Fatalf("set: %+v %v", got, err)
	}
	other, err := d.RuleSettingsOf(context.Background(), "t1", "LE2")
	if err != nil || other.F4 != "ALL_PREVIEWED" || other.F8 != "REJECT_WITH_DEFAULT" || other.F9 != "KEEP_AMOUNT" {
		t.Fatalf("the defaults: %+v %v", other, err)
	}
}
