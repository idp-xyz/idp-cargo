package domain

import (
	"errors"
	"strings"
	"testing"
)

// V2.100: the release of a conflict with a carrier's loading report under S1, "a carrier report that contradicts SEA's
// records". The day is the conflict day of conflict_test.go. The rules the domain keeps: under SUPERVISOR_ONLY the
// release needs a supervisor, and says the way out when it has none; a release records the version of the settings, the
// value of S1 and the time they were read at; the pending item says who may release it under the value it is read
// under.

func (d *reDay) releaseUnder(rules RuleSettings, supervisor bool, hour int) error {
	return d.v.AcceptCarrierLoadingConflictUnder(d.m, 1, "the line confirmed the box was not loaded", "ops-7", hr(hour), rules, supervisor)
}

func TestSupervisorOnlyNeedsASupervisorToReleaseAConflict(t *testing.T) {
	d := conflictDay(t, "CSQU3054383")
	rules := RuleSettings{Version: 4, S1: S1SupervisorOnly, At: hr(27)}
	events := len(d.v.Pending())
	err := d.releaseUnder(rules, false, 27)
	if !errors.Is(err, ErrNotSupervisor) || !strings.Contains(err.Error(), "SuperviseCarrierConflicts") || !strings.Contains(err.Error(), "ask a holder") {
		t.Fatalf("without the permission: %v", err)
	}
	if len(d.v.State().Releases) != 0 || len(d.v.Pending()) != events || !pendingBlocking(d.items()) {
		t.Fatalf("a refused release wrote: %+v", d.v.State().Releases)
	}
	if err = d.releaseUnder(rules, true, 27); err != nil {
		t.Fatalf("a supervisor releases it: %v", err)
	}
	r := d.v.State().Releases[0]
	if r.SettingsVersion != 4 || r.S1 != S1SupervisorOnly || !r.EnteredAt.Equal(hr(27)) || r.By != "ops-7" {
		t.Fatalf("the release records what it was decided under: %+v", r)
	}
	if err = d.finalize(30); err != nil {
		t.Fatalf("finalize after a supervisor's release: %v", err)
	}
}

func TestWithReasonNeedsNoSupervisorToReleaseAConflict(t *testing.T) {
	d := conflictDay(t, "CSQU3054383")
	if err := d.releaseUnder(RuleSettings{Version: 2, S1: S1WithReason, At: hr(27)}, false, 27); err != nil {
		t.Fatalf("WITH_REASON: %v", err)
	}
	r := d.v.State().Releases[0]
	if r.SettingsVersion != 2 || r.S1 != S1WithReason || !r.EnteredAt.Equal(hr(27)) {
		t.Fatalf("the release: %+v", r)
	}
}

// A release made through the earlier call, without the settings, is what V2.99 made: nothing is recorded of the
// settings, and it is entered when the visit is stamped.
func TestAReleaseWithoutTheSettingsIsAsItWas(t *testing.T) {
	d := conflictDay(t, "CSQU3054383")
	if err := d.release("checked", 27); err != nil {
		t.Fatal(err)
	}
	r := d.v.State().Releases[0]
	if r.SettingsVersion != 0 || r.S1 != "" || !r.EnteredAt.IsZero() {
		t.Fatalf("a release without the settings: %+v", r)
	}
	d.v.Stamp(hr(28))
	if got := d.v.State().Releases[0].EnteredAt; !got.Equal(hr(28)) {
		t.Fatalf("stamped at %s", got)
	}
}

// A release decided under settings is entered at the time they were read at, which the later stamp does not replace;
// one loaded from the store keeps its time too.
func TestAReleaseKeepsTheTimeTheSettingsWereReadAt(t *testing.T) {
	d := conflictDay(t, "CSQU3054383")
	if err := d.releaseUnder(RuleSettings{Version: 1, S1: S1WithReason, At: hr(27)}, false, 27); err != nil {
		t.Fatal(err)
	}
	d.v.Stamp(hr(28))
	if got := d.v.State().Releases[0].EnteredAt; !got.Equal(hr(27)) {
		t.Fatalf("stamped at %s, want the time the settings were read at", got)
	}
}

func TestRestoreRefusesAReleaseOfAnUnknownS1OrANegativeVersion(t *testing.T) {
	d := conflictDay(t, "CSQU3054383")
	if err := d.releaseUnder(RuleSettings{Version: 1, S1: S1WithReason, At: hr(27)}, false, 27); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ConflictRelease){
		"an unknown S1":      func(r *ConflictRelease) { r.S1 = "MAYBE" },
		"a negative version": func(r *ConflictRelease) { r.SettingsVersion = -1 },
	} {
		st := d.v.State()
		change(&st.Releases[0])
		if _, err := RestoreVisit(st); err == nil {
			t.Errorf("%s: restored", name)
		}
	}
	if _, err := RestoreVisit(d.v.State()); err != nil {
		t.Fatalf("restored as it is: %v", err)
	}
}

// The item of the conflict says who may release it under the value of S1 it is read under, and both when it is read
// without the settings (as Finalize reads it); every wording keeps the way out of the V2.89 message.
func TestTheConflictItemSaysWhoMayReleaseIt(t *testing.T) {
	d := conflictDay(t, "CSQU3054383")
	for _, c := range []struct {
		s1   string
		want []string
		not  []string
	}{
		{"", []string{"under WITH_REASON whoever holds AcceptCarrierLoadingConflict", "under SUPERVISOR_ONLY only a supervisor"}, nil},
		{S1WithReason, []string{"S1 is WITH_REASON", "whoever holds AcceptCarrierLoadingConflict may release it"}, []string{"ask a supervisor"}},
		{S1SupervisorOnly, []string{"S1 is SUPERVISOR_ONLY", "only a supervisor", "SuperviseCarrierConflicts", "ask a supervisor"}, []string{"whoever holds"}},
	} {
		d.m.ReadUnder(c.s1)
		it := wantItem(t, c.s1, d.items(), PendGatedBeforeReseal, true, append(c.want, "release the conflict with a reason (AcceptCarrierLoadingConflict)", "handled outside the system")...)
		for _, n := range c.not {
			if strings.Contains(it.Message, n) {
				t.Errorf("%s: %q says %q", c.s1, it.Message, n)
			}
		}
	}
	d.m.ReadUnder("")
}

func TestOpenConflictsAreThoseAReleaseWouldClear(t *testing.T) {
	d := conflictDay(t, "CSQU3054383")
	got := d.m.OpenConflicts([]VisitState{d.v.State()})
	if len(got) != 1 || got[0].Number != "CSQU3054383" || got[0].FactNo != 1 || got[0].VisitID != d.v.ID() {
		t.Fatalf("open conflicts: %+v", got)
	}
	// a report that names no container counts for the only live container
	if n := conflictDay(t, ""); len(n.m.OpenConflicts([]VisitState{n.v.State()})) != 1 {
		t.Fatal("a report naming no container, with one live container")
	}
	// the one the pending item reports: what the release clears is gone, and so is the conflict
	if err := d.release("checked", 27); err != nil {
		t.Fatal(err)
	}
	if got = d.m.OpenConflicts([]VisitState{d.v.State()}); len(got) != 0 {
		t.Fatalf("after the release: %+v", got)
	}
	// without the reseal there is no conflict
	if f := newReDay(t); len(f.m.OpenConflicts([]VisitState{f.v.State()})) != 0 {
		t.Fatal("a day without a conflict")
	}
}
