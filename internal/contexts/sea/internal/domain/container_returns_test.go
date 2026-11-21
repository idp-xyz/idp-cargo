package domain

import (
	"errors"
	"strings"
	"testing"
)

// V2.89: a container taken back out of the terminal. The day is the rework day of rework_test.go: PHY's stuffing X-1
// sealed S1 at hour 20, the container taken as stuffed at hour 21 with a VGM at hour 22.

func (d *reDay) out(ref string, occurred, recorded int) error {
	_, _, err := d.v.RecordContainerEvent(EventGateOut, loc("CNSHG"), hr(occurred), "terminal", ref, "ops", hr(recorded))
	return err
}

// The return is accepted on a gated-in container only: not before the stuffing, not while VGM is submitted and the
// container never gated in, not on a loaded container, not on a released one.
func TestTheReturnIsAcceptedOnAGatedInVisitOnly(t *testing.T) {
	out := func(v *Visit) error {
		_, _, err := v.RecordContainerEvent(EventGateOut, loc("CNSHG"), hr(30), "terminal", "GOUT-1", "ops", hr(30))
		return err
	}
	stuffedOnly := func() *Visit {
		d := newReDay(t)
		v := assigned(t, d.m)
		sealed(t, v, "S1", 2)
		if err := v.StuffingDone(d.m, "", "cfs", hr(21)); err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		name   string
		build  func() *Visit
		status VisitStatus
		want   error
	}{
		{"ASSIGNED", func() *Visit { return assigned(t, open(t, booking(1))) }, VisitAssigned, ErrInvalidEvent},
		{"STUFFED", stuffedOnly, VisitStuffed, ErrInvalidEvent},
		{"VGM_SUBMITTED", func() *Visit { return newReDay(t).v }, VisitVGM, ErrInvalidEvent},
		{"GATED_IN", func() *Visit { d := newReDay(t); d.event(EventGateIn, "GIN-1", 23, 23); return d.v }, VisitGatedIn, nil},
		{"LOADED", func() *Visit {
			d := newReDay(t)
			d.event(EventGateIn, "GIN-1", 23, 23)
			d.event(EventLoaded, "LD-1", 24, 24)
			return d.v
		}, VisitLoaded, ErrInvalidEvent},
		{"RELEASED", func() *Visit {
			v := assigned(t, open(t, booking(1)))
			if err := v.Release("not needed", "ops", hr(5)); err != nil {
				t.Fatal(err)
			}
			return v
		}, VisitReleased, ErrInvalidTransition},
	}
	for _, c := range cases {
		v := c.build()
		if got := v.State().Status; got != c.status {
			t.Fatalf("%s: the visit is %s", c.name, got)
		}
		events := len(v.State().Events)
		err := out(v)
		if c.want == nil {
			if err != nil || v.State().Status != VisitVGM || len(v.State().Events) != events+1 {
				t.Fatalf("%s: %v, status %s", c.name, err, v.State().Status)
			}
			continue
		}
		if !errors.Is(err, c.want) || v.State().Status != c.status || len(v.State().Events) != events || len(v.Pending()) != len(c.build().Pending()) {
			t.Fatalf("%s: %v, status %s", c.name, err, v.State().Status)
		}
		if c.name == "LOADED" && !strings.Contains(err.Error(), "outside the system") {
			t.Fatalf("a loaded container is told it is handled outside the system: %v", err)
		}
	}
}

// After the return the visit is VGM_SUBMITTED, with its VGM standing; a VGM can be asserted again and the container
// can gate in again, and a second round (in, out, in) is allowed. The same source and reference again is a replay.
func TestAReturnedContainerIsAsAnyVGMSubmittedVisitAndGatesInAgain(t *testing.T) {
	d := newReDay(t)
	d.event(EventGateIn, "GIN-1", 23, 23)
	if err := d.out("GOUT-1", 24, 24); err != nil {
		t.Fatal(err)
	}
	st := d.v.State()
	if st.Status != VisitVGM || len(st.Assertions) != 1 || len(st.Events) != 2 || st.Events[1].Kind != EventGateOut || st.Events[1].No != 2 {
		t.Fatalf("after the return: %+v", st)
	}
	if n, added, err := d.v.RecordContainerEvent(EventGateOut, loc("CNSHG"), hr(24), "terminal", "GOUT-1", "ops", hr(30)); err != nil || added || n != 2 || len(d.v.State().Events) != 2 {
		t.Fatalf("the replay: %d %v %v", n, added, err)
	}
	if _, _, err := d.v.RecordContainerEvent(EventGateOut, loc("CNSHG"), hr(25), "terminal", "GOUT-1", "ops", hr(30)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("the same reference with another time: %v", err)
	}
	d.vgm(26)
	d.event(EventGateIn, "GIN-2", 27, 27)
	if d.v.State().Status != VisitGatedIn {
		t.Fatalf("gated in again: %s", d.v.State().Status)
	}
	if err := d.out("GOUT-2", 28, 28); err != nil {
		t.Fatal(err)
	}
	d.event(EventGateIn, "GIN-3", 29, 29)
	st = d.v.State()
	if st.Status != VisitGatedIn || len(st.Events) != 5 {
		t.Fatalf("a second round: %s, %d events", st.Status, len(st.Events))
	}
	if again, err := RestoreVisit(st); err != nil || again.State().Status != VisitGatedIn {
		t.Fatalf("restored: %v", err)
	}
	// a replay after the status has moved on is still the replay
	if n, added, err := d.v.RecordContainerEvent(EventGateOut, loc("CNSHG"), hr(24), "terminal", "GOUT-1", "ops", hr(31)); err != nil || added || n != 2 {
		t.Fatalf("the replay of the first return: %d %v %v", n, added, err)
	}
}

// A stored visit is GATED_IN or LOADED exactly when its latest gate-in comes after its latest gate-out, LOADED exactly
// when a loading is recorded, and no gate-out comes after a loading.
func TestRestoreRefusesAVisitThatDisagreesWithItsGateEvents(t *testing.T) {
	build := func(status VisitStatus, kinds ...string) VisitState {
		d := newReDay(t)
		st := d.v.State()
		for i, k := range kinds {
			st.Events = append(st.Events, VisitEvent{No: i + 1, Kind: k, Location: loc("CNSHG"), OccurredAt: hr(23), Source: "terminal", Reference: "R" + string(rune('A'+i)), By: "ops", At: hr(23)})
		}
		st.Status = status
		return st
	}
	in, out, load := EventGateIn, EventGateOut, EventLoaded
	for name, tc := range map[string]struct {
		st VisitState
		ok bool
	}{
		"gated in":                           {build(VisitGatedIn, in), true},
		"returned":                           {build(VisitVGM, in, out), true},
		"returned and gated in again":        {build(VisitGatedIn, in, out, in), true},
		"loaded":                             {build(VisitLoaded, in, load), true},
		"returned, gated in, loaded":         {build(VisitLoaded, in, out, in, load), true},
		"gated in with its return recorded":  {build(VisitGatedIn, in, out), false},
		"returned but still VGM_SUBMITTED":   {build(VisitVGM, in), false},
		"loaded with its return recorded":    {build(VisitLoaded, in, load, out), false},
		"loaded, returned, gated in again":   {build(VisitLoaded, in, load, out, in), false},
		"loaded after a return, not loaded":  {build(VisitGatedIn, in, out, in, load), false},
		"a return before any gate-in":        {build(VisitVGM, out), false},
		"two returns in a row":               {build(VisitVGM, in, out, out), false},
		"two rounds, gated in":               {build(VisitGatedIn, in, out, in, out, in), true},
		"loaded and the gate-in then return": {build(VisitLoaded, in, out, load), false},
	} {
		_, err := RestoreVisit(tc.st)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// F3 with returns. The container is at the terminal at PHY's sealed-at when its latest gate-in before that time is not
// followed by a gate-out before it; it is then refused, the message naming the return and a new VGM, and the pending
// list says so at each stage.
func TestFinalizeAfterAReturnFromTheTerminal(t *testing.T) {
	// gated in at 22 and returned at 23, before PHY resealed at 25: not raised
	d := newReDay(t)
	d.event(EventGateIn, "GIN-1", 22, 22)
	if err := d.out("GOUT-1", 23, 23); err != nil {
		t.Fatal(err)
	}
	if items := d.items(); len(items) != 0 {
		t.Fatalf("returned, before the reseal: %+v", items)
	}
	d.reseal("S2", 25, true)
	items := d.items()
	wantItem(t, "the old VGM", items, PendVGMBeforeReseal, true, "assert a new one")
	if _, ok := kinds(items)[PendGatedBeforeReseal]; ok {
		t.Fatalf("returned before the reseal: F3 raised: %+v", items)
	}
	refused(t, "the old VGM", d.finalize(30), "assert a new one")
	d.vgm(26)
	if items = d.items(); len(items) != 0 {
		t.Fatalf("with a new VGM: %+v", items)
	}
	if err := d.finalize(30); err != nil {
		t.Fatalf("returned before the reseal, a new VGM: %v", err)
	}
	// and gated in again afterwards
	d = newReDay(t)
	d.event(EventGateIn, "GIN-1", 22, 22)
	if err := d.out("GOUT-1", 23, 23); err != nil {
		t.Fatal(err)
	}
	d.reseal("S2", 25, true)
	d.vgm(26)
	d.event(EventGateIn, "GIN-2", 27, 27)
	if items = d.items(); len(items) != 0 {
		t.Fatalf("gated in again: %+v", items)
	}
	if err := d.finalize(30); err != nil {
		t.Fatalf("gated in again: %v", err)
	}

	// no return recorded: raised, with the way out; a return recorded later, dated after the reseal, is the way out all
	// the same, with a warning that asks for its time to be checked
	d = newReDay(t)
	d.event(EventGateIn, "GIN-1", 22, 22)
	d.reseal("S2", 25, true)
	items = d.items()
	it := wantItem(t, "at the terminal", items, PendGatedBeforeReseal, true, "CSQU3054383", "was at the terminal when PHY sealed it again", "RecordContainerEvent GATE_OUT_TERMINAL", "assert a new VGM", "gate it in again")
	if strings.Contains(it.Message, "outside the system") {
		t.Fatalf("a container at the terminal is not told it is handled outside the system: %s", it.Message)
	}
	if _, ok := kinds(items)[WarnReturnDatedAfterReseal]; ok {
		t.Fatalf("no return recorded, no warning: %+v", items)
	}
	if err := d.out("GOUT-1", 27, 28); err != nil {
		t.Fatal(err)
	}
	d.vgm(29)
	items = d.items()
	if _, ok := kinds(items)[PendGatedBeforeReseal]; ok || pendingBlocking(items) {
		t.Fatalf("returned after the reseal: only the warning is left: %+v", items)
	}
	wantItem(t, "the times", items, WarnReturnDatedAfterReseal, false, "CSQU3054383", "the times say it was resealed at the terminal", "check them")
	if err := d.finalize(30); err != nil {
		t.Fatalf("returned after the reseal, a new VGM: only a warning: %v", err)
	}
	// a gate-in again after a return dated after the reseal changes nothing
	d = newReDay(t)
	d.event(EventGateIn, "GIN-1", 22, 22)
	d.reseal("S2", 25, true)
	if err := d.out("GOUT-1", 27, 28); err != nil {
		t.Fatal(err)
	}
	d.vgm(29)
	d.event(EventGateIn, "GIN-2", 30, 30)
	items = d.items()
	wantItem(t, "gated in again", items, WarnReturnDatedAfterReseal, false)
	if pendingBlocking(items) {
		t.Fatalf("gated in again: %+v", items)
	}
	// a return exactly when PHY sealed it counts as the return, with the warning
	d = newReDay(t)
	d.event(EventGateIn, "GIN-1", 22, 22)
	d.reseal("S2", 25, true)
	if err := d.out("GOUT-1", 25, 26); err != nil {
		t.Fatal(err)
	}
	items = d.items()
	if _, ok := kinds(items)[PendGatedBeforeReseal]; ok {
		t.Fatalf("returned when PHY sealed: %+v", items)
	}
	wantItem(t, "returned when PHY sealed", items, WarnReturnDatedAfterReseal, false, "check them")
	// a gate-in after the reseal, then its return: the container was not at the terminal at the reseal, no item
	d = newReDay(t)
	d.reseal("S2", 25, true)
	d.vgm(26)
	d.event(EventGateIn, "GIN-1", 27, 27)
	if err := d.out("GOUT-1", 28, 28); err != nil {
		t.Fatal(err)
	}
	if items = d.items(); len(items) != 0 {
		t.Fatalf("gated in and returned after the reseal: %+v", items)
	}
	// without a reseal a return changes nothing: no F3, no warning
	d = newReDay(t)
	d.event(EventGateIn, "GIN-1", 22, 22)
	if err := d.out("GOUT-1", 27, 28); err != nil {
		t.Fatal(err)
	}
	if items = d.items(); len(items) != 0 {
		t.Fatalf("no reseal: %+v", items)
	}
	// the order is the events' own: the latest gate-in before the reseal and a later gate-out before it
	d = newReDay(t)
	d.event(EventGateIn, "GIN-1", 22, 22)
	if err := d.out("GOUT-1", 23, 23); err != nil {
		t.Fatal(err)
	}
	d.vgm(23)
	d.event(EventGateIn, "GIN-2", 24, 24)
	d.reseal("S2", 25, true)
	wantItem(t, "gated in again before the reseal", d.items(), PendGatedBeforeReseal, true, "gated in (2026-11-11T08:00:00Z)")
}

// A container loaded before the reseal is refused with the message for a loaded container, whatever its returns say.
func TestAContainerLoadedBeforeTheResealIsHandledOutsideTheSystem(t *testing.T) {
	d := newReDay(t)
	d.event(EventGateIn, "GIN-1", 22, 22)
	d.event(EventLoaded, "LD-1", 23, 23)
	d.reseal("S2", 25, true)
	it := wantItem(t, "loaded", d.items(), PendGatedBeforeReseal, true, "CSQU3054383", "was loaded before PHY sealed it again", "by its loading", "handled outside the system")
	if strings.Contains(it.Message, "GATE_OUT_TERMINAL") {
		t.Fatalf("a loaded container is not told to record its return: %s", it.Message)
	}
	if err := d.out("GOUT-1", 24, 24); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("a loaded container is not returned: %v", err)
	}
	refused(t, "loaded", d.finalize(30), "handled outside the system")
	// returned, gated in again and loaded after the reseal: finalizes
	e := newReDay(t)
	e.event(EventGateIn, "GIN-1", 22, 22)
	if err := e.out("GOUT-1", 23, 23); err != nil {
		t.Fatal(err)
	}
	e.reseal("S2", 25, true)
	e.vgm(26)
	e.event(EventGateIn, "GIN-2", 27, 27)
	e.event(EventLoaded, "LD-1", 28, 28)
	if items := e.items(); len(items) != 0 {
		t.Fatalf("loaded after the rework: %+v", items)
	}
}

func pendingBlocking(items []PendingItem) bool {
	for _, it := range items {
		if it.Blocking {
			return true
		}
	}
	return false
}

// A container at the terminal when PHY sealed it again that is loaded afterwards (gate-in 22, reseal 25, loading 26) is
// told it is handled outside the system, not to record a return that its LOADED status would not allow.
func TestAContainerGatedInBeforeAndLoadedAfterTheResealIsHandledOutsideTheSystem(t *testing.T) {
	d := newReDay(t)
	d.event(EventGateIn, "GIN-1", 22, 22)
	d.reseal("S2", 25, true)
	d.event(EventLoaded, "LD-1", 26, 26)
	it := wantItem(t, "loaded after", d.items(), PendGatedBeforeReseal, true, "was at the terminal when PHY sealed it again", "loaded since", "handled outside the system")
	if strings.Contains(it.Message, "GATE_OUT_TERMINAL") || strings.Contains(it.Message, "assert a new VGM") {
		t.Fatalf("a way out that does not exist: %s", it.Message)
	}
	refused(t, "loaded after", d.finalize(30), "handled outside the system")
}

func (d *twoDay) items() []PendingItem {
	return d.m.PendingItems([]Plan{d.p}, []VisitState{d.v1.State(), d.v2.State()})
}

// itemsOf are the items about one container.
func itemsOf(items []PendingItem, container string) []PendingItem {
	var out []PendingItem
	for _, it := range items {
		if it.SubjectID == container {
			out = append(out, it)
		}
	}
	return out
}

func nameless(t *testing.T, m *Movement, live []string, container string, occurred int) {
	t.Helper()
	bill := "MAEU123456789"
	if container != "" {
		bill = ""
	}
	f := CarrierFact{Kind: CarrierLoaded, ContainerNumber: container, BillNumber: bill, Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line",
		Reference: "LD-9", SHA256: strings.Repeat("ab", 32), OccurredAt: hr(occurred)}
	if _, _, err := m.RecordCarrierFact(f, live, "ops", hr(occurred)); err != nil {
		t.Fatal(err)
	}
}

// With two live containers a carrier's loading report that names no container cannot show which was loaded: it blocks
// none (not one that never gated in, not one at the terminal, not one with its own return) and only warns, for each
// container PHY sealed again; a report that names a container blocks that container and no other.
func TestACarrierReportNamingNoContainerOnlyWarns(t *testing.T) {
	both := []string{"CSQU3054383", "TEMU1234565"}
	seals := map[string]string{"X-1": "S2"}
	// never gated in: not blocked, the warning stands
	d := newTwoDay(t)
	nameless(t, d.m, both, "", 24)
	d.reseal(25, seals)
	d.vgm(d.v1, 26)
	items := d.items()
	if pendingBlocking(items) {
		t.Fatalf("never gated in, a report naming no container: %+v", items)
	}
	wantItem(t, "the warning", items, WarnCarrierLoadingBeforeReseal, false, "CSQU3054383", "without naming a container", "check whether this container was the one loaded", "handled outside the system")
	if got := itemsOf(items, "TEMU1234565"); len(got) != 0 {
		t.Fatalf("the container PHY did not seal again is unaffected: %+v", got)
	}
	if err := d.finalize(30); err != nil {
		t.Fatal(err)
	}
	// both resealed: both warned
	b := newTwoDay(t)
	nameless(t, b.m, both, "", 24)
	b.reseal(25, map[string]string{"X-1": "S2", "X-2": "T2"})
	b.vgm(b.v1, 26)
	b.vgm(b.v2, 26)
	if items = b.items(); pendingBlocking(items) || len(itemsOf(items, "CSQU3054383")) != 1 || len(itemsOf(items, "TEMU1234565")) != 1 {
		t.Fatalf("both resealed: %+v", items)
	}
	// at the terminal: told to record its return, the warning beside it
	e := newTwoDay(t)
	e.event(e.v1, EventGateIn, "GIN-1", 22, 22)
	nameless(t, e.m, both, "", 24)
	e.reseal(25, seals)
	items = e.items()
	wantItem(t, "at the terminal", items, PendGatedBeforeReseal, true, "was at the terminal when PHY sealed it again", "RecordContainerEvent GATE_OUT_TERMINAL")
	wantItem(t, "the warning", items, WarnCarrierLoadingBeforeReseal, false)
	// with its own return: nothing blocks, the warning stands
	f := newTwoDay(t)
	f.event(f.v1, EventGateIn, "GIN-1", 22, 22)
	f.event(f.v1, EventGateOut, "GOUT-1", 23, 23)
	nameless(t, f.m, both, "", 24)
	f.reseal(25, seals)
	f.vgm(f.v1, 26)
	items = f.items()
	if pendingBlocking(items) {
		t.Fatalf("its own return, a report naming no container: %+v", items)
	}
	wantItem(t, "the warning", items, WarnCarrierLoadingBeforeReseal, false)
	// a report dated when PHY sealed: no warning
	g := newTwoDay(t)
	g.reseal(25, seals)
	g.vgm(g.v1, 26)
	nameless(t, g.m, both, "", 25)
	if items = g.items(); len(items) != 0 {
		t.Fatalf("a report dated when PHY sealed: %+v", items)
	}
	// no reseal: no warning
	h := newTwoDay(t)
	nameless(t, h.m, both, "", 24)
	if items = h.items(); len(items) != 0 {
		t.Fatalf("no reseal: %+v", items)
	}
	// a report that names CV-1 blocks CV-1 and not CV-2, and the message reads well
	k := newTwoDay(t)
	nameless(t, k.m, both, "CSQU3054383", 24)
	k.reseal(25, map[string]string{"X-1": "S2", "X-2": "T2"})
	k.vgm(k.v1, 26)
	k.vgm(k.v2, 26)
	items = k.items()
	it := wantItem(t, "a report naming the container", items, PendGatedBeforeReseal, true, "the carrier's report of a loading", "naming this container", "AcceptCarrierLoadingConflict", "handled outside the system")
	if it.SubjectID != "CSQU3054383" || strings.Contains(it.Message, "its the") || len(itemsOf(items, "TEMU1234565")) != 0 {
		t.Fatalf("the message and the other container: %+v", items)
	}
	if _, ok := kinds(items)[WarnCarrierLoadingBeforeReseal]; ok {
		t.Fatal("a report that names the container is not the nameless one")
	}
	// its own loading blocks
	l := newReDay(t)
	l.event(EventGateIn, "GIN-1", 22, 22)
	l.event(EventLoaded, "LD-1", 23, 23)
	l.reseal("S2", 25, true)
	wantItem(t, "its loading", l.items(), PendGatedBeforeReseal, true, "by its loading")
}

// With exactly one live container (a released visit is not live) a carrier's loading report that names no container can
// only be about that container: it counts as naming it and blocks as a named report does, with no warning. The count
// is of the live containers as they stand when the items are read: a container assigned later makes the report a
// warning from then on, because the report no longer shows which container was loaded.
func TestACarrierReportNamingNoContainerWithOneLiveContainerNamesIt(t *testing.T) {
	one := []string{"CSQU3054383"}
	second := func(d *reDay) *Visit {
		t.Helper()
		v, err := AssignContainer("CV-2", d.m, 2, "TEMU1234565", "22G1", plate, "ops", hr(1))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	with := func(d *reDay, extra *Visit) []PendingItem {
		vs := []VisitState{d.v.State()}
		if extra != nil {
			vs = append(vs, extra.State())
		}
		return d.m.PendingItems([]Plan{d.p1, d.p2}, vs)
	}
	// one container, never gated in: blocked as by a named report, outside the system, no warning
	d := newReDay(t)
	nameless(t, d.m, one, "", 24)
	d.reseal("S2", 25, true)
	d.vgm(26)
	items := d.items()
	wantItem(t, "one container", items, PendGatedBeforeReseal, true, "the carrier's report of a loading", "naming no container, while this is the movement's only live container", "AcceptCarrierLoadingConflict", "handled outside the system")
	if _, ok := kinds(items)[WarnCarrierLoadingBeforeReseal]; ok {
		t.Fatalf("a blocking report is not also a warning: %+v", items)
	}
	refused(t, "one container", d.finalize(30), "handled outside the system")
	// a released second visit is not live: still one, still blocked
	e := newReDay(t)
	nameless(t, e.m, one, "", 24)
	e.reseal("S2", 25, true)
	e.vgm(26)
	gone := second(e)
	if err := gone.Release("not needed", "ops", hr(26)); err != nil {
		t.Fatal(err)
	}
	items = with(e, gone)
	wantItem(t, "a released second visit", items, PendGatedBeforeReseal, true, "handled outside the system")
	if _, ok := kinds(items)[WarnCarrierLoadingBeforeReseal]; ok {
		t.Fatalf("a released second visit: %+v", items)
	}
	// a second container assigned later: from then on the report warns
	f := newReDay(t)
	nameless(t, f.m, one, "", 24)
	f.reseal("S2", 25, true)
	f.vgm(26)
	wantItem(t, "before the second container", with(f, nil), PendGatedBeforeReseal, true)
	items = with(f, second(f))
	if _, ok := kinds(items)[PendGatedBeforeReseal]; ok {
		t.Fatalf("with a second live container the report only warns: %+v", items)
	}
	wantItem(t, "after the second container", items, WarnCarrierLoadingBeforeReseal, false, "CSQU3054383")
}

// A return is dated not before the gate-in it follows (the latest one); a time equal to it is accepted.
func TestAReturnIsNotDatedBeforeItsGateIn(t *testing.T) {
	d := newReDay(t)
	d.event(EventGateIn, "GIN-1", 23, 23)
	before := len(d.v.State().Events)
	if err := d.out("GOUT-1", 22, 24); !errors.Is(err, ErrInvalidEvent) || !strings.Contains(err.Error(), "give the time it was taken back out") {
		t.Fatalf("a return before the gate-in: %v", err)
	}
	if st := d.v.State(); len(st.Events) != before || st.Status != VisitGatedIn {
		t.Fatalf("a refused return wrote: %+v", st)
	}
	if err := d.out("GOUT-2", 23, 24); err != nil {
		t.Fatalf("a return at the time of the gate-in: %v", err)
	}
	d.vgm(25)
	d.event(EventGateIn, "GIN-3", 27, 27)
	if err := d.out("GOUT-4", 26, 28); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("a return before the latest gate-in: %v", err)
	}
	if err := d.out("GOUT-5", 28, 28); err != nil {
		t.Fatal(err)
	}
}

// A gate-in recorded on a loaded visit leaves it loaded.
func TestAGateInOnALoadedVisitLeavesItLoaded(t *testing.T) {
	d := newReDay(t)
	d.event(EventGateIn, "GIN-1", 23, 23)
	d.event(EventLoaded, "LD-1", 24, 24)
	d.event(EventGateIn, "GIN-2", 25, 25)
	st := d.v.State()
	if st.Status != VisitLoaded || len(st.Events) != 3 {
		t.Fatalf("a gate-in on a loaded visit: %s, %d events", st.Status, len(st.Events))
	}
	if again, err := RestoreVisit(st); err != nil || again.State().Status != VisitLoaded {
		t.Fatalf("restored: %v", err)
	}
}
