package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// V2.88: a container is resealed when PHY's seal differs from what SEA took when it took the container as stuffed
// (StuffedPhySeal, StuffedPhySealedAt), not by business times, entries or the difference reason.

// stuffedDay is a movement whose houses first carried PHY's seal "OLD-1", then (PHY resealed) "PHY-2", before SEA took
// the stuffing at hour 30; the container has its own seal OURS-1 and, with a reason, is taken although PHY's seal is
// none of its seals.
func stuffedDay(t *testing.T, reason string) *reDay {
	t.Helper()
	d := &reDay{t: t, m: open(t, booking(1))}
	d.p1, d.p2 = stuffed(plan("SH-1", "C-1", 1), 1, "OLD-1"), stuffed(plan("SH-2", "C-1", 1), 1, "OLD-1")
	if err := errors.Join(d.m.Associate(d.p1, "H1", "ops", at), d.m.Associate(d.p2, "H2", "ops", at), d.m.RecordMasterBill("MAEU123456789", BillCarrierMBL, sha, "ops", hr(1))); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Plan{&d.p1, &d.p2} { // PHY resealed before SEA took the stuffing
		p.Revision++
		p.Stuffings = []Stuffing{p.Stuffings[0]}
		p.Stuffings[0].Seal, p.Stuffings[0].SealedAt = "PHY-2", hr(25)
		if err := d.m.Refresh(*p, "ops", hr(26)); err != nil {
			t.Fatal(err)
		}
	}
	d.v = assigned(t, d.m)
	if reason == "" {
		sealed(t, d.v, "PHY-2", 2)
	} else {
		sealed(t, d.v, "OURS-1", 2)
	}
	if err := d.v.StuffingDone(d.m, reason, "cfs", hr(30)); err != nil {
		t.Fatal(err)
	}
	d.vgm(31)
	return d
}

func TestStuffingDoneKeepsWhatSEATook(t *testing.T) {
	d := stuffedDay(t, "")
	st := d.v.State()
	if st.StuffedPhySeal != "PHY-2" || !st.StuffedPhySealedAt.Equal(hr(25)) {
		t.Fatalf("what SEA took: %q %v", st.StuffedPhySeal, st.StuffedPhySealedAt)
	}
	restored, err := RestoreVisit(st)
	if err != nil || restored.State().StuffedPhySeal != "PHY-2" {
		t.Fatalf("restore: %v", err)
	}
	for name, mut := range map[string]func(*VisitState){
		"a seal without its time":              func(s *VisitState) { s.StuffedPhySealedAt = time.Time{} },
		"a time without its seal":              func(s *VisitState) { s.StuffedPhySeal = "" },
		"a blank seal":                         func(s *VisitState) { s.StuffedPhySeal = "  " },
		"sealed after the container was taken": func(s *VisitState) { s.StuffedPhySealedAt = s.StuffedAt.Add(time.Second) },
		"on a visit not stuffed": func(s *VisitState) {
			s.Status, s.ExecutionID, s.StuffedBy, s.StuffedAt = VisitAssigned, "", "", time.Time{}
			s.Assertions = nil
		},
	} {
		bad := d.v.State()
		mut(&bad)
		if _, err = RestoreVisit(bad); !errors.Is(err, ErrInvalidVisit) {
			t.Errorf("%s: restored: %v", name, err)
		}
	}
}

// PHY resealed before SEA's stuffing-done (the README's case 2): the entries' history holds the older seal, but nothing
// changed since SEA took the container, so there is no reseal: no warning, no block, no seal followed, and a difference
// reason stated against SEA's own seal is kept.
func TestAResealBeforeTheStuffingIsNoResealSinceTheStuffing(t *testing.T) {
	for _, reason := range []string{"", "PHY keyed the seal wrongly"} {
		d := stuffedDay(t, reason)
		if d.v.State().resealed(d.p1.Stuffings[0], d.m.sealHistory("X-1")) {
			t.Fatalf("reason %q: resealed, though PHY's seal is the one SEA took", reason)
		}
		if items := d.items(); len(items) != 0 {
			t.Fatalf("reason %q: pending %+v", reason, items)
		}
		// the refresh that reads the same seal again follows nothing; the container keeps its own seal
		if done, err := d.v.FollowSeal(d.p1.Stuffings[0], "ops", hr(40)); done || err != nil {
			t.Fatalf("reason %q: followed: %v %v", reason, done, err)
		}
		if err := d.finalize(40); err != nil {
			t.Fatalf("reason %q: %v", reason, err)
		}
		want := "PHY-2"
		if reason != "" {
			want = "OURS-1"
		}
		if d.v.Seal().Number != want {
			t.Fatalf("reason %q: the seal %s", reason, d.v.Seal().Number)
		}
	}
}

// A real reseal after the stuffing, by seal key alone and by sealed-at alone, with or without a reason.
func TestAResealAfterTheStuffingIsAnyChangeFromWhatSEATook(t *testing.T) {
	for name, change := range map[string]func(*Stuffing){
		"another seal":                func(s *Stuffing) { s.Seal = "PHY-3" },
		"the same seal, another time": func(s *Stuffing) { s.SealedAt = hr(26) },
		"the same seal keyed alike":   nil,
	} {
		for _, reason := range []string{"", "PHY keyed the seal wrongly"} {
			d := stuffedDay(t, reason)
			p := d.p1.Stuffings[0]
			if change != nil {
				change(&p)
			} else {
				p.Seal = strings.ToLower(strings.ReplaceAll(p.Seal, "-", " ")) // "phy 2": the same key
			}
			resealed := d.v.State().resealed(p, d.m.sealHistory("X-1"))
			if resealed != (change != nil) {
				t.Errorf("%s, reason %q: resealed %v", name, reason, resealed)
			}
		}
	}
}

// A visit stuffed before V2.88 has no stored seal: the rule of the first corrections stands (the history).
func TestAVisitStuffedBeforeV288KeepsTheLegacyRule(t *testing.T) {
	d := stuffedDay(t, "PHY keyed the seal wrongly")
	d.v.state.StuffedPhySeal, d.v.state.StuffedPhySealedAt = "", time.Time{}
	// the history holds OLD-1 and PHY-2 differ from P=PHY-2: legacy sees a reseal; the reason is dropped
	if !d.v.State().resealed(d.p1.Stuffings[0], d.m.sealHistory("X-1")) {
		t.Fatal("the legacy rule reads the history")
	}
	if _, err := RestoreVisit(d.v.State()); err != nil {
		t.Fatalf("a visit with no stored seal restores: %v", err)
	}
}
