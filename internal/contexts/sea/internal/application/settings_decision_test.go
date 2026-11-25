package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
)

// decisionTrace records the order in which a decision touches the settings lock, the clock and the ledger.
type decisionTrace struct{ calls []string }

type tracedClock struct {
	trace *decisionTrace
	now   time.Time
}

func (c tracedClock) Now() time.Time {
	c.trace.calls = append(c.trace.calls, "clock")
	return c.now
}

// tracedSettings is a settings store of one ledger that answers only what a decision reads.
type tracedSettings struct {
	SettingsStore
	trace  *decisionTrace
	ledger *settings.Ledger
}

func (s tracedSettings) LockShared(context.Context, sea.TenantID) error {
	s.trace.calls = append(s.trace.calls, "lock")
	return nil
}

func (s tracedSettings) Load(context.Context, sea.TenantID) (*settings.Ledger, error) {
	s.trace.calls = append(s.trace.calls, "ledger")
	return s.ledger, nil
}

// The decision's settings: the shared lock first, then the clock, then the ledger as of that time; the legal entity's
// own value, else the company's, else the standard; a change set that takes effect later does not count; the version is
// the one in force at the time read, and that time is the one the decision is stamped with.
func TestADecisionReadsTheSettingsUnderTheLockAndAtTheTimeAfterIt(t *testing.T) {
	at := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	entry := func(scope settings.Scope, value string) []settings.Entry {
		return []settings.Entry{{Scope: scope, Key: SettingS1, Value: value}}
	}
	company := settings.Scope{Kind: settings.KindTenant}
	le := settings.Scope{Kind: settings.KindLegalEntity, ID: "LE"}
	ledger := settings.NewLedger([]settings.ChangeSet{
		{ID: "c1", Entries: entry(company, sea.S1SupervisorOnly), By: "ann", At: at.Add(-3 * time.Hour), Effective: at.Add(-3 * time.Hour),
			Approval: &settings.Approval{By: "ann", At: at.Add(-3 * time.Hour), Version: 1, InForce: at.Add(-3 * time.Hour), Auto: true}},
		{ID: "c2", Entries: entry(le, sea.S1WithReason), By: "ann", At: at.Add(-2 * time.Hour), Effective: at.Add(time.Hour), Loosening: true,
			Approval: &settings.Approval{By: "bob", At: at.Add(-2 * time.Hour), Version: 2, InForce: at.Add(time.Hour)}},
	})
	trace := &decisionTrace{}
	d := Deps{Clock: tracedClock{trace, at}, Settings: tracedSettings{trace: trace, ledger: ledger}}
	got, err := d.DecisionSettings(context.Background(), "t1", "LE")
	if err != nil {
		t.Fatal(err)
	}
	if want := (sea.RuleSettings{Version: 1, S1: sea.S1SupervisorOnly, At: at}); got != want {
		t.Fatalf("the settings: %+v, want %+v (the legal entity's own value takes effect in an hour)", got, want)
	}
	if strings.Join(trace.calls, " ") != "lock clock ledger" {
		t.Fatalf("the order of the read: %v, want the lock, the clock after it, the ledger", trace.calls)
	}
	// another legal entity takes the company's value; both at a time after the legal entity's own change took effect
	d.Clock = tracedClock{trace, at.Add(2 * time.Hour)}
	if got, err = d.DecisionSettings(context.Background(), "t1", "LE"); err != nil || got.S1 != sea.S1WithReason || got.Version != 2 || !got.At.Equal(at.Add(2*time.Hour)) {
		t.Fatalf("the legal entity's own value in force: %+v, %v", got, err)
	}
	if got, err = d.DecisionSettings(context.Background(), "t1", "LE2"); err != nil || got.S1 != sea.S1SupervisorOnly || got.Version != 2 {
		t.Fatalf("another legal entity: %+v, %v", got, err)
	}
	// before any change set: the standard value, version 0 (a clock behind a change set that is recorded reads
	// it, since V2.100)
	d.Clock, d.Settings = tracedClock{trace, at.Add(-4 * time.Hour)}, tracedSettings{trace: trace, ledger: settings.NewLedger(nil)}
	if got, err = d.DecisionSettings(context.Background(), "t1", "LE"); err != nil || got.S1 != sea.S1WithReason || got.Version != 0 {
		t.Fatalf("before any change set: %+v, %v", got, err)
	}
	// the movement names a legal entity, or the settings cannot be told
	trace.calls = nil
	if _, err = d.DecisionSettings(context.Background(), "t1", " "); !errors.Is(err, sea.ErrInvalidChange) || !strings.Contains(err.Error(), "ask the platform's owners") ||
		len(trace.calls) != 0 {
		t.Fatalf("a movement with no legal entity: %v, %v", err, trace.calls)
	}
	// with no settings store wired the standard profile is answered, at the time read; with no clock nothing is
	if got, err = (Deps{Clock: tracedClock{trace, at}}).DecisionSettings(context.Background(), "t1", "LE"); err != nil || got.S1 != sea.S1WithReason || got.Version != 0 || !got.At.Equal(at) {
		t.Fatalf("no store: %+v, %v", got, err)
	}
	if _, err = (Deps{Settings: tracedSettings{trace: trace, ledger: ledger}}).DecisionSettings(context.Background(), "t1", "LE"); !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("no clock: %v", err)
	}
}
