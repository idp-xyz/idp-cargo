package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// V2.89 batch 6: a conflict between SEA's records and a carrier's loading report at a reseal is released by the
// operator with a reason. The day is the rework day: one container CV-1, never gated in, a carrier report that names it
// (or none: with one live container it can only be about it) dated 24, PHY's reseal at 25.

func conflictDay(t *testing.T, container string) *reDay {
	t.Helper()
	d := newReDay(t)
	nameless(t, d.m, []string{"CSQU3054383"}, container, 24)
	d.reseal("S2", 25, true)
	d.vgm(26)
	return d
}

func (d *reDay) release(reason string, hour int) error {
	return d.v.AcceptCarrierLoadingConflict(d.m, 1, reason, "ops-7", hr(hour))
}

// The item is the conflict, released by a reason that is kept with who and when; the item then reads as a warning that
// names them, and Finalize passes.
func TestACarrierLoadingConflictIsReleasedWithAReason(t *testing.T) {
	for _, container := range []string{"CSQU3054383", ""} {
		d := conflictDay(t, container)
		it := wantItem(t, "the conflict", d.items(), PendGatedBeforeReseal, true, "AcceptCarrierLoadingConflict", "handled outside the system")
		refused(t, "the conflict", d.finalize(30), it.Message[:40])
		if err := d.release("the line confirmed the box was not loaded", 27); err != nil {
			t.Fatalf("release (%q): %v", container, err)
		}
		st := d.v.State()
		if len(st.Releases) != 1 || len(d.v.AddedReleases()) != 1 {
			t.Fatalf("releases: %+v", st.Releases)
		}
		r := st.Releases[0]
		if r.No != 1 || r.FactNo != 1 || r.PHYSeal != "S2" || !r.PHYSealedAt.Equal(hr(25)) || r.Reason != "the line confirmed the box was not loaded" || r.By != "ops-7" || !r.At.Equal(hr(27)) {
			t.Fatalf("the release: %+v", r)
		}
		if e := d.v.Pending(); e[len(e)-1].Type != eventConflictReleased || e[len(e)-1].ReleaseNo != 1 {
			t.Fatalf("the event: %+v", e[len(e)-1])
		}
		items := d.items()
		if pendingBlocking(items) {
			t.Fatalf("after the release: %+v", items)
		}
		wantItem(t, "the warning", items, WarnCarrierConflictReleased, false, "ops-7", "2026-11-11T11:00:00Z", "the line confirmed the box was not loaded", "CSQU3054383")
		if err := d.finalize(30); err != nil {
			t.Fatalf("finalize after the release: %v", err)
		}
		if again, err := RestoreVisit(d.v.State()); err != nil || len(again.State().Releases) != 1 {
			t.Fatalf("restored: %v", err)
		}
	}
}

// Only a conflict raised by the carrier's report alone is released: not one raised by the container's own loading or by
// its gate-in with no return; not without a reason; not on a movement that is not OPEN; not twice for the same report
// and seal; not with two live containers and a report that names none; not on a released or an assigned visit.
func TestACarrierLoadingConflictIsReleasedOnlyWhenItIsTheCarriersAlone(t *testing.T) {
	// SEA's own loading
	d := newReDay(t)
	nameless(t, d.m, []string{"CSQU3054383"}, "CSQU3054383", 24)
	d.event(EventGateIn, "GIN-1", 22, 22)
	d.event(EventLoaded, "LD-1", 23, 23)
	d.reseal("S2", 25, true)
	if err := d.release("checked", 27); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("raised by its own loading: %v", err)
	}
	// its gate-in with no return
	e := newReDay(t)
	nameless(t, e.m, []string{"CSQU3054383"}, "CSQU3054383", 24)
	e.event(EventGateIn, "GIN-1", 22, 22)
	e.reseal("S2", 25, true)
	if err := e.release("checked", 27); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("raised by its gate-in with no return: %v", err)
	}
	// once the return is recorded the conflict is the carrier's alone and is released
	if err := e.out("GOUT-1", 23, 26); err != nil {
		t.Fatal(err)
	}
	if err := e.release("checked", 27); err != nil {
		t.Fatalf("after its return: %v", err)
	}
	// SEA's own loading dated before the reseal, its gate-in dated after it: not at the terminal then, yet loaded
	o := newReDay(t)
	nameless(t, o.m, []string{"CSQU3054383"}, "CSQU3054383", 24)
	o.reseal("S2", 25, true)
	o.vgm(26)
	o.event(EventGateIn, "GIN-9", 26, 26)
	o.event(EventLoaded, "LD-9", 24, 27)
	wantItem(t, "its own loading", o.items(), PendGatedBeforeReseal, true, "by its loading")
	if err := o.release("checked", 28); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("raised by its own loading, not at the terminal: %v", err)
	}
	// no reason, a blank one, one of 501 characters
	f := conflictDay(t, "CSQU3054383")
	for _, reason := range []string{"", "   ", strings.Repeat("x", 501)} {
		if err := f.release(reason, 27); !errors.Is(err, ErrInvalidChange) || len(f.v.State().Releases) != 0 {
			t.Fatalf("reason %q: %v", reason, err)
		}
	}
	if err := f.release(strings.Repeat("x", 500), 27); err != nil {
		t.Fatalf("a reason of 500 characters: %v", err)
	}
	// twice for the same report and seal
	if err := f.release("again", 28); !errors.Is(err, ErrInvalidChange) || len(f.v.State().Releases) != 1 {
		t.Fatalf("twice: %v", err)
	}
	// a movement that is not OPEN
	g := conflictDay(t, "CSQU3054383")
	g.m.state.State = StateFinal
	if err := g.release("checked", 27); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a FINAL movement: %v", err)
	}
	g.m.state.State = StateCancelled
	if err := g.release("checked", 27); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a cancelled movement: %v", err)
	}
	// a report that names no container with two live containers is no conflict of this container
	h := conflictDay(t, "")
	if err := h.v.AcceptCarrierLoadingConflict(h.m, 2, "checked", "ops-7", hr(27)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("two live containers: %v", err)
	}
	// a business time before the visit's last change
	k := conflictDay(t, "CSQU3054383")
	if err := k.release("checked", 10); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a time before the last change: %v", err)
	}
	// an assigned and a released visit
	a := assigned(t, newReDay(t).m)
	if err := a.AcceptCarrierLoadingConflict(newReDay(t).m, 1, "checked", "ops", hr(5)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("an assigned visit: %v", err)
	}
}

// A release covers the report against that seal only: PHY sealing the container again, or another report, is a new
// conflict that needs its own release.
func TestALaterResealOrReportRaisesANewConflict(t *testing.T) {
	d := conflictDay(t, "CSQU3054383")
	if err := d.release("checked", 27); err != nil {
		t.Fatal(err)
	}
	// PHY seals again (S3 at 28): the same report against another seal
	d.reseal("S3", 28, true)
	d.vgm(29)
	wantItem(t, "a new seal", d.items(), PendGatedBeforeReseal, true, "AcceptCarrierLoadingConflict", "2026-11-11T12:00:00Z")
	if _, ok := kinds(d.items())[WarnCarrierConflictReleased]; ok {
		t.Fatal("the release of the first seal does not cover the second")
	}
	if err := d.release("checked again", 30); err != nil {
		t.Fatal(err)
	}
	if st := d.v.State(); len(st.Releases) != 2 || st.Releases[1].No != 2 || st.Releases[1].PHYSeal != "S3" || !st.Releases[1].PHYSealedAt.Equal(hr(28)) {
		t.Fatalf("releases: %+v", st.Releases)
	}
	if err := d.finalize(35); err != nil {
		t.Fatalf("both released: %v", err)
	}
	// PHY seals again with the same seal at another time: another P, the release does not cover it
	h := conflictDay(t, "CSQU3054383")
	if err := h.release("checked", 27); err != nil {
		t.Fatal(err)
	}
	h.reseal("S2", 28, true)
	h.vgm(29)
	wantItem(t, "the same seal, another time", h.items(), PendGatedBeforeReseal, true, "AcceptCarrierLoadingConflict")
	// PHY seals again with another seal at the same time: another P by the seal, the release does not cover it
	k := conflictDay(t, "CSQU3054383")
	if err := k.release("checked", 27); err != nil {
		t.Fatal(err)
	}
	k.reseal("S3", 25, true)
	k.vgm(29)
	wantItem(t, "another seal, the same time", k.items(), PendGatedBeforeReseal, true, "AcceptCarrierLoadingConflict")
	// another carrier report, dated before the reseal, is a new conflict
	f := conflictDay(t, "CSQU3054383")
	if err := f.release("checked", 27); err != nil {
		t.Fatal(err)
	}
	second := CarrierFact{Kind: CarrierLoaded, ContainerNumber: "CSQU3054383", Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line", Reference: "LD-10",
		SHA256: strings.Repeat("ab", 32), OccurredAt: hr(23)}
	if _, _, err := f.m.RecordCarrierFact(second, []string{"CSQU3054383"}, "ops", hr(28)); err != nil {
		t.Fatal(err)
	}
	wantItem(t, "another report", f.items(), PendGatedBeforeReseal, true, "AcceptCarrierLoadingConflict", "2026-11-11T07:00:00Z")
	wantItem(t, "the first is still released", f.items(), WarnCarrierConflictReleased, false)
}

// A visit stored with releases is restored; one that is not consistent is not.
func TestRestoreRefusesAVisitWithInconsistentReleases(t *testing.T) {
	d := conflictDay(t, "CSQU3054383")
	if err := d.release("checked", 27); err != nil {
		t.Fatal(err)
	}
	good := d.v.State()
	for name, mutate := range map[string]func(*VisitState){
		"a number out of order": func(s *VisitState) { s.Releases[0].No = 2 },
		"no fact":               func(s *VisitState) { s.Releases[0].FactNo = 0 },
		"no seal":               func(s *VisitState) { s.Releases[0].PHYSeal = "" },
		"no sealed-at":          func(s *VisitState) { s.Releases[0].PHYSealedAt = time.Time{} },
		"no reason":             func(s *VisitState) { s.Releases[0].Reason = " " },
		"a reason too long":     func(s *VisitState) { s.Releases[0].Reason = strings.Repeat("x", 501) },
		"nobody":                func(s *VisitState) { s.Releases[0].By = "" },
		"before the stuffing":   func(s *VisitState) { s.Releases[0].At = hr(5) },
		"on an assigned visit":  func(s *VisitState) { *s = assigned(t, newReDay(t).m).State(); s.Releases = good.Releases },
	} {
		st := good
		st.Releases = append([]ConflictRelease(nil), good.Releases...)
		mutate(&st)
		if _, err := RestoreVisit(st); err == nil {
			t.Errorf("%s: restored", name)
		}
	}
	if _, err := RestoreVisit(good); err != nil {
		t.Fatal(err)
	}
}

// The conflict message says truthfully what SEA has: no gate-in or loading of the container before PHY's reseal, or the
// container gated in and taken back out before it, and no loading.
func TestTheConflictMessageSaysWhatSEAHas(t *testing.T) {
	d := conflictDay(t, "CSQU3054383")
	it := wantItem(t, "never gated in", d.items(), PendGatedBeforeReseal, true, "while SEA has no gate-in or loading of it before then")
	if strings.Contains(it.Message, "taken back out") {
		t.Fatalf("never gated in: %s", it.Message)
	}
	e := newReDay(t)
	e.event(EventGateIn, "GIN-1", 22, 22)
	if err := e.out("GOUT-1", 23, 23); err != nil {
		t.Fatal(err)
	}
	nameless(t, e.m, []string{"CSQU3054383"}, "CSQU3054383", 24)
	e.reseal("S2", 25, true)
	e.vgm(26)
	it = wantItem(t, "gated in and returned", e.items(), PendGatedBeforeReseal, true, "while SEA has it gated in and taken back out before then, and no loading of it")
	if strings.Contains(it.Message, "no gate-in or loading") {
		t.Fatalf("gated in and returned: %s", it.Message)
	}
	// a gate-in dated after the reseal is no gate-in before it
	f := newReDay(t)
	nameless(t, f.m, []string{"CSQU3054383"}, "CSQU3054383", 24)
	f.reseal("S2", 25, true)
	f.vgm(26)
	f.event(EventGateIn, "GIN-9", 27, 27)
	wantItem(t, "gated in after the reseal", f.items(), PendGatedBeforeReseal, true, "while SEA has no gate-in or loading of it before then")
}
