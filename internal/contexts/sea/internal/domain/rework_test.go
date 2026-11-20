package domain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.87: SEA follows PHY's rework of a container. These tests drive the model the way the commands do: a movement of
// two houses H1 and H2 in unit 1 (PHY's stuffing X-1, sealed S1 at hour 20), a container taken as stuffed at hour 21
// with a VGM at hour 22; PHY then seals X-1 again with S2 at hour 25, which FUL relays and SEA refreshes the houses to.

func hr(n int) time.Time { return at.Add(time.Duration(n) * time.Hour) }

// reDay is the day of the model tests.
type reDay struct {
	t      *testing.T
	m      *Movement
	v      *Visit
	p1, p2 Plan // FUL's plans of the two houses as they stand
}

func newReDay(t *testing.T) *reDay {
	t.Helper()
	d := &reDay{t: t, m: open(t, booking(1))}
	d.p1, d.p2 = stuffed(plan("SH-1", "C-1", 1), 1, "S1"), stuffed(plan("SH-2", "C-1", 1), 1, "S1")
	if err := errors.Join(d.m.Associate(d.p1, "H1", "ops", at), d.m.Associate(d.p2, "H2", "ops", at), d.m.RecordMasterBill("MAEU123456789", BillCarrierMBL, sha, "ops", hr(1))); err != nil {
		t.Fatal(err)
	}
	d.v = assigned(t, d.m)
	sealed(t, d.v, "S1", 2)
	if err := d.v.StuffingDone(d.m, "", "cfs", hr(21)); err != nil {
		t.Fatal(err)
	}
	d.vgm(22)
	return d
}

func (d *reDay) vgm(hour int) {
	d.t.Helper()
	if err := d.v.AssertVGM(d.m, kernel.Quantity{Unit: "KGM", Amount: 3700}, VgmMethod2, "LE", 1, hr(hour), "", "Ada Lovelace", "ops", hr(hour)); err != nil {
		d.t.Fatal(err)
	}
}

// reseal is PHY sealing X-1 again with seal at hour: FUL relays it and SEA refreshes each house (follow also records
// the seal on the visit, as the commands do).
func (d *reDay) reseal(seal string, hour int, follow bool) {
	d.t.Helper()
	for _, p := range []*Plan{&d.p1, &d.p2} {
		p.Revision++
		p.Stuffings = []Stuffing{p.Stuffings[0]}
		p.Stuffings[0].Seal, p.Stuffings[0].SealedAt = seal, hr(hour)
		if err := d.m.Refresh(*p, "ops", hr(hour)); err != nil {
			d.t.Fatal(err)
		}
		if follow {
			e := d.m.State().Entries[len(d.m.State().Entries)-1]
			if err := FollowSeals(e, []*Visit{d.v}, "ops", hr(hour)); err != nil {
				d.t.Fatal(err)
			}
		}
	}
}

func (d *reDay) finalize(hour int) error {
	return d.m.Finalize([]Plan{d.p1, d.p2}, []VisitState{d.v.State()}, "ops", hr(hour))
}

func (d *reDay) event(kind, ref string, occurred, recorded int) {
	d.t.Helper()
	if _, _, err := d.v.RecordContainerEvent(kind, loc("CNSHG"), hr(occurred), "terminal", ref, "ops", hr(recorded)); err != nil {
		d.t.Fatal(err)
	}
}

func refused(t *testing.T, label string, err error, want ...string) {
	t.Helper()
	if !errors.Is(err, ErrNotFinalizable) {
		t.Fatalf("%s: %v, want a refusal", label, err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("%s: %q does not say %q", label, err, w)
		}
	}
}

// Without a reseal every new rule holds and the movement finalizes as before: F4 holds because an assertion needs
// STUFFED and StuffedAt is not before PHY's sealed-at.
func TestFinalizeWithoutAResealIsAsBefore(t *testing.T) {
	d := newReDay(t)
	if err := d.finalize(30); err != nil {
		t.Fatal(err)
	}
	if st := d.v.State(); len(st.Seals) != 1 || st.Seals[0].Number != "S1" {
		t.Fatalf("seals: %+v", st.Seals)
	}
}

func TestFollowSeal(t *testing.T) {
	newer := func(seal string, hour int) Stuffing {
		s := stuffed(plan("SH-1", "C-1", 1), 1, seal).Stuffings[0]
		s.SealedAt = hr(hour)
		return s
	}
	d := newReDay(t)
	v := d.v
	// PHY's seal the visit does not hold, dated at or before StuffedAt: without a difference reason it is one PHY
	// applied since (the visit's seals are never removed), so it is followed; with one it cannot be told from the
	// difference the reason was stated for, and is not
	reasoned := newReDay(t)
	reasoned.v.state.DifferenceReason = "PHY keyed the seal wrongly"
	reasoned.v.state.StuffedPhySeal, reasoned.v.state.StuffedPhySealedAt = "", time.Time{} // stuffed before V2.88: the legacy rule
	for _, hour := range []int{21, 10} {
		if done, err := reasoned.v.FollowSeal(newer("OTHER-1", hour), "ops", hr(30)); done || err != nil {
			t.Fatalf("with a difference reason, sealed at %d: %v %v", hour, done, err)
		}
	}
	backdated := newReDay(t)
	if done, err := backdated.v.FollowSeal(newer("OTHER-1", 10), "ops", hr(30)); !done || err != nil || backdated.v.Seal().Number != "OTHER-1" {
		t.Fatalf("a reseal dated before StuffedAt, no difference reason: %v %v", done, err)
	}
	other := newer("OTHER-1", 25)
	other.ExecutionID = "X-2"
	if done, err := v.FollowSeal(other, "ops", hr(30)); done || err != nil {
		t.Fatalf("another execution: %v %v", done, err)
	}
	if len(v.State().Seals) != 1 {
		t.Fatalf("seals: %+v", v.State().Seals)
	}
	// PHY sealed X-1 again: the seal is followed, as a SHIPPER seal applied when PHY sealed it
	done, err := v.FollowSeal(newer("s2", 25), "ops-2", hr(30))
	if err != nil || !done {
		t.Fatalf("follow: %v %v", done, err)
	}
	seals := v.State().Seals
	if len(seals) != 2 || seals[1].No != 2 || seals[1].Number != "S2" || seals[1].Party != SealShipper || !seals[1].AppliedAt.Equal(hr(25)) ||
		seals[1].By != "ops-2" || !seals[1].At.Equal(hr(30)) || !strings.Contains(seals[1].Source, "PHY") || !strings.Contains(seals[1].Source, "X-1") {
		t.Fatalf("the followed seal: %+v", seals)
	}
	if e := v.Pending(); e[len(e)-1].Type != eventSealRecorded || e[len(e)-1].SealNo != 2 {
		t.Fatalf("events: %+v", e)
	}
	// the second house carries the same seal: found held, nothing recorded (also keyed with other separators and case)
	for _, seal := range []string{"S2", "s-2", "S1"} {
		if done, err = v.FollowSeal(newer(seal, 25), "ops", hr(31)); done || err != nil {
			t.Fatalf("%s held: %v %v", seal, done, err)
		}
	}
	if len(v.State().Seals) != 2 {
		t.Fatalf("seals: %+v", v.State().Seals)
	}
	// the container may have gone on: a VGM, gate-in and loading do not stop the seal being followed
	for name, tc := range map[string]struct {
		hours int // events recorded before PHY resealed
	}{"VGM_SUBMITTED": {0}, "GATED_IN": {1}, "LOADED": {2}} {
		d := newReDay(t)
		if tc.hours >= 1 {
			d.event(EventGateIn, "GIN-1", 22, 22)
		}
		if tc.hours >= 2 {
			d.event(EventLoaded, "LD-1", 23, 23)
		}
		if done, err = d.v.FollowSeal(newer("S2", 25), "ops", hr(30)); !done || err != nil || string(d.v.State().Status) != name {
			t.Fatalf("%s: %v %v %s", name, done, err, d.v.State().Status)
		}
	}
	// not yet stuffed, or released: not followed, whatever PHY did
	a := assigned(t, d.m)
	if done, err = a.FollowSeal(newer("S2", 25), "ops", hr(30)); done || err != nil || len(a.State().Seals) != 0 {
		t.Fatalf("an ASSIGNED visit: %v %v", done, err)
	}
	if err = a.Release("sent back", "ops", hr(30)); err != nil {
		t.Fatal(err)
	}
	if done, err = a.FollowSeal(newer("S2", 25), "ops", hr(31)); done || err != nil {
		t.Fatalf("a RELEASED visit: %v %v", done, err)
	}
}

// A seal SEA's rules cannot take is not recorded, and it is not an error: the house's entry is written all the same and
// the seal is a pending item of the movement (PendSealNotOnContainer) that says what to do. A later refresh records it
// when the business time allows.
func TestASealTheRulesCannotTakeIsNotRecordedAndIsPending(t *testing.T) {
	newer := func(seal string, hour int) Stuffing {
		s := stuffed(plan("SH-1", "C-1", 1), 1, seal).Stuffings[0]
		s.SealedAt = hr(hour)
		return s
	}
	for name, tc := range map[string]struct {
		s    Stuffing
		at   time.Time
		want string
	}{
		"business time before PHY sealed it": {newer("S2", 25), hr(24), "at a business time not before PHY sealed it (2026-11-11T09:00:00Z) and not before the container's last change"},
		"no letters or digits":               {newer("--", 25), hr(30), "PHY has to seal the container again"},
		"a letter outside A-Z":               {newer("S\u00c9AL-1", 25), hr(30), "PHY has to seal the container again"},
		"too long":                           {newer(strings.Repeat("A", 36), 25), hr(30), "PHY has to seal the container again"},
	} {
		d := newReDay(t)
		done, err := d.v.FollowSeal(tc.s, "ops", tc.at)
		if done || err != nil || len(d.v.State().Seals) != 1 {
			t.Errorf("%s: %v %v", name, done, err)
		}
		d.p1.Stuffings, d.p2.Stuffings = []Stuffing{tc.s}, []Stuffing{tc.s}
		d.p1.Revision++
		d.p2.Revision++
		if err = errors.Join(d.m.Refresh(d.p1, "ops", hr(26)), d.m.Refresh(d.p2, "ops", hr(26))); err != nil {
			t.Fatal(err)
		}
		it, ok := findItem(d.m.PendingItems([]Plan{d.p1, d.p2}, []VisitState{d.v.State()}), PendSealNotOnContainer)
		if !ok || !it.Blocking || it.Scope != ScopeContainer || it.SubjectID != "CSQU3054383" || !strings.Contains(it.Message, tc.want) {
			t.Errorf("%s: %+v %v", name, it, ok)
		}
		refused(t, name+": finalize", d.finalize(40), tc.want)
	}
	// the business time was before the visit's last change: not recorded; a later refresh at a time that allows records
	// it
	d := newReDay(t)
	if err := d.v.RecordSeal("CUS-1", SealCustoms, hr(27), "notice", "ops", hr(27)); err != nil {
		t.Fatal(err)
	}
	if done, err := d.v.FollowSeal(newer("S2", 25), "ops", hr(26)); done || err != nil || len(d.v.State().Seals) != 2 {
		t.Fatalf("before the visit's last change: %v %v", done, err)
	}
	if done, err := d.v.FollowSeal(newer("S2", 25), "ops", hr(28)); !done || err != nil || d.v.Seal().Number != "S2" {
		t.Fatalf("a later time: %v %v", done, err)
	}
}

func findItem(items []PendingItem, kind string) (PendingItem, bool) {
	for _, it := range items {
		if it.Kind == kind {
			return it, true
		}
	}
	return PendingItem{}, false
}

// The owner's day in the model: a reseal after the container was taken as stuffed needs the new seal and a VGM
// declared after it (F2, F4).
func TestFinalizeAfterAReseal(t *testing.T) {
	d := newReDay(t)
	d.reseal("S2", 25, false)
	refused(t, "F2: the new seal is not recorded", d.finalize(30), "none of the seals PHY sealed", `"S2"`)
	if done, err := d.v.FollowSeal(d.p1.Stuffings[0], "ops", hr(30)); !done || err != nil {
		t.Fatal(done, err)
	}
	refused(t, "F4: the VGM was submitted before PHY sealed it again", d.finalize(31), "no VGM submitted after PHY sealed it", "assert a new one")
	d.vgm(32)
	if err := d.finalize(33); err != nil {
		t.Fatalf("with S2 and a new VGM: %v", err)
	}
	// F4's edge: a VGM submitted when PHY sealed counts (not before)
	e := newReDay(t)
	e.reseal("S2", 25, true)
	e.vgm(25)
	if err := e.finalize(30); err != nil {
		t.Fatalf("a VGM submitted when PHY sealed: %v", err)
	}
	// two VGMs after the reseal: the latest counts
	f := newReDay(t)
	f.reseal("S2", 25, true)
	f.vgm(26)
	f.vgm(27)
	if err := f.finalize(30); err != nil {
		t.Fatalf("two VGMs after the reseal: %v", err)
	}
	// a rejected new VGM is not ready, as before
	g := newReDay(t)
	g.reseal("S2", 25, true)
	g.vgm(26)
	if err := g.v.RecordVgmResponse(VgmRejected, "portal", "ops", hr(28)); err != nil {
		t.Fatal(err)
	}
	refused(t, "rejected", g.finalize(30), "no VGM submitted by the cut-off or accepted")
}

func TestFinalizeLaterSealsAfterPHYsSeal(t *testing.T) {
	for name, tc := range map[string]struct {
		later []struct{ number, party string }
		want  string // empty: published
	}{
		"a customs seal":                        {[]struct{ number, party string }{{"CUS-1", SealCustoms}}, ""},
		"a carrier seal":                        {[]struct{ number, party string }{{"CAR-1", SealCarrier}}, ""},
		"customs then carrier":                  {[]struct{ number, party string }{{"CUS-1", SealCustoms}, {"CAR-1", SealCarrier}}, ""},
		"a shipper seal":                        {[]struct{ number, party string }{{"SHP-9", SealShipper}}, "SHIPPER seal SHP-9"},
		"the broken seal again, by the shipper": {[]struct{ number, party string }{{"S1", SealShipper}}, "SHIPPER seal S1"},
		"customs, then a shipper seal":          {[]struct{ number, party string }{{"CUS-1", SealCustoms}, {"SHP-9", SealShipper}}, "SHIPPER seal SHP-9"},
		"the broken seal again, by customs":     {[]struct{ number, party string }{{"S1", SealCustoms}}, ""},
	} {
		d := newReDay(t)
		d.reseal("S2", 25, true)
		d.vgm(26)
		for i, s := range tc.later {
			if err := d.v.RecordSeal(s.number, s.party, hr(27+i), "notice", "ops", hr(27+i)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		err := d.finalize(40)
		if tc.want == "" && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if tc.want != "" && (!errors.Is(err, ErrNotFinalizable) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "only a customs or carrier seal")) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Without a reseal a later SHIPPER seal that is not PHY's is refused too, a customs one is not.
	d := newReDay(t)
	if err := d.v.RecordSeal("SHP-9", SealShipper, hr(23), "tally", "ops", hr(23)); err != nil {
		t.Fatal(err)
	}
	refused(t, "a shipper seal after PHY's, no reseal", d.finalize(40), "SHIPPER seal SHP-9")
}

// A container taken as stuffed with a reason (PHY's seal was none of the container's) keeps that rule while PHY has not
// sealed it again since; after a reseal PHY's new seal is asked for like any.
func TestFinalizeTheDifferenceReasonBeforeAndAfterAReseal(t *testing.T) {
	build := func() *reDay {
		d := &reDay{t: t, m: open(t, booking(1))}
		d.p1, d.p2 = stuffed(plan("SH-1", "C-1", 1), 1, "PHY-1"), stuffed(plan("SH-2", "C-1", 1), 1, "PHY-1")
		if err := errors.Join(d.m.Associate(d.p1, "H1", "ops", at), d.m.Associate(d.p2, "H2", "ops", at), d.m.RecordMasterBill("MAEU123456789", BillCarrierMBL, sha, "ops", hr(1))); err != nil {
			t.Fatal(err)
		}
		d.v = assigned(t, d.m)
		sealed(t, d.v, "OURS-1", 2)
		if err := d.v.StuffingDone(d.m, "PHY keyed the seal wrongly", "cfs", hr(21)); err != nil {
			t.Fatal(err)
		}
		d.vgm(22)
		return d
	}
	d := build()
	if err := d.finalize(30); err != nil {
		t.Fatalf("before a reseal, with the reason, as today: %v", err)
	}
	d = build()
	d.reseal("PHY-2", 25, false)
	refused(t, "after a reseal, PHY's new seal is not recorded", d.finalize(30), "none of the seals PHY sealed")
	d = build()
	d.reseal("PHY-2", 25, true)
	d.vgm(27)
	if err := d.finalize(30); err != nil {
		t.Fatalf("after a reseal, followed, with a new VGM: %v", err)
	}
}

// F1: houses that carry one stuffing with different seals or sealing times are to be refreshed.
func TestFinalizeTheHousesAgreeOnPHYsSeal(t *testing.T) {
	d := newReDay(t)
	d.p1.Revision++
	d.p1.Stuffings = []Stuffing{d.p1.Stuffings[0]}
	d.p1.Stuffings[0].Seal, d.p1.Stuffings[0].SealedAt = "S2", hr(25)
	if err := d.m.Refresh(d.p1, "ops", hr(26)); err != nil {
		t.Fatal(err)
	}
	refused(t, "H1 carries S2 and H2 S1", d.finalize(30), "different seals", "H1", "H2", "refresh the houses")
	// the same seal keyed alike but sealed at another time is no agreement either
	d.p2.Revision++
	d.p2.Stuffings = []Stuffing{d.p2.Stuffings[0]}
	d.p2.Stuffings[0].Seal, d.p2.Stuffings[0].SealedAt = "s-2", hr(24)
	if err := d.m.Refresh(d.p2, "ops", hr(26)); err != nil {
		t.Fatal(err)
	}
	refused(t, "the same seal sealed at another time", d.finalize(30), "different seals")
	d.p2.Revision++
	d.p2.Stuffings = []Stuffing{d.p2.Stuffings[0]}
	d.p2.Stuffings[0].SealedAt = hr(25)
	if err := d.m.Refresh(d.p2, "ops", hr(27)); err != nil {
		t.Fatal(err)
	}
	if done, err := d.v.FollowSeal(d.p1.Stuffings[0], "ops", hr(28)); !done || err != nil {
		t.Fatal(done, err)
	}
	d.vgm(29)
	if err := d.finalize(30); err != nil {
		t.Fatalf("the same seal as keyed in each house: %v", err)
	}
}

// F3: a container that gated in or was loaded before PHY sealed it again is refused, and the message is F3's, not F4's.
func TestFinalizeAContainerGatedInBeforeTheReseal(t *testing.T) {
	const gatedMessage = "gated in or was loaded before PHY sealed it again"
	gate := func(d *reDay) { d.event(EventGateIn, "GIN-1", 22, 22) }
	for name, prepare := range map[string]func(*reDay){
		"gate-in": gate,
		"loading": func(d *reDay) { gate(d); d.event(EventLoaded, "LD-1", 23, 23) },
		"the movement's on-board date, by the carrier's report": func(d *reDay) {
			d.vgm(22)
			if _, _, err := d.m.RecordCarrierFact(CarrierFact{Kind: CarrierLoaded, BillNumber: "MAEU123456789", Vessel: "EVER GIVEN", Voyage: "0123W",
				Source: "line", Reference: "LD-9", SHA256: strings.Repeat("ab", 32), OccurredAt: hr(23)}, nil, "ops", hr(23)); err != nil {
				d.t.Fatal(err)
			}
		},
	} {
		d := newReDay(t)
		prepare(d)
		d.reseal("S2", 25, true)
		refused(t, name, d.finalize(30), gatedMessage, "CSQU3054383", "return from the terminal")
		// not as the VGM's message: the old VGM is before PHY's seal too
		if strings.Contains(d.finalize(30).Error(), "assert a new one") {
			t.Fatalf("%s: F4's message shows before F3's", name)
		}
	}
	// a loading alone: the gate-in is dated after the reseal, the loading before it (the reports need not agree)
	d := newReDay(t)
	d.reseal("S2", 25, true)
	d.vgm(26)
	d.event(EventGateIn, "GIN-9", 26, 26)
	d.event(EventLoaded, "LD-9", 24, 27)
	refused(t, "a loading before the reseal", d.finalize(30), "gated in or was loaded before PHY sealed it again")
	// a gate-in after the reseal is accepted (with the new VGM, which a gated-in container could not take: it is
	// asserted first)
	d = newReDay(t)
	d.reseal("S2", 25, true)
	d.vgm(26)
	d.event(EventGateIn, "GIN-2", 27, 27)
	d.event(EventLoaded, "LD-2", 28, 28)
	if err := d.finalize(30); err != nil {
		t.Fatalf("gated in and loaded after the reseal: %v", err)
	}
	// a gate-in exactly when PHY sealed it is not before
	d = newReDay(t)
	d.reseal("S2", 25, true)
	d.vgm(26)
	d.event(EventGateIn, "GIN-3", 25, 27)
	if err := d.finalize(30); err != nil {
		t.Fatalf("gated in when PHY sealed: %v", err)
	}
	// without a reseal a container gated in before PHY's seal time cannot be: gate-in needs a VGM, which needs
	// StuffedAt not before PHY's sealed-at; the rule does not apply to an unchanged stuffing
	d = newReDay(t)
	gate(d)
	if err := d.finalize(30); err != nil {
		t.Fatalf("gated in, no reseal: %v", err)
	}
}

// F0: FUL's evidence must be PHY's current one; on an own-container movement only. The stuffing path's check says the
// same with its own refusal.
func TestHousesAreCheckedAgainstFULsEvidence(t *testing.T) {
	d := newReDay(t)
	d.p2.EvidenceBehind = true
	refused(t, "F0", d.finalize(30), "FUL has not recorded PHY's latest facts", "H2", "SH-2", "record them in FUL, and refresh the house if SEA then reports it STALE_HOUSE")
	// F0 is asked before the comparison of the facts: FUL behind says so, not that the facts differ
	d.p2.Revision++
	d.p2.PlanVersion = 2
	refused(t, "F0 first", d.finalize(30), "FUL has not recorded")
	if err := d.m.CheckHouses([]Plan{d.p1, d.p2}, ErrNotStuffed); !errors.Is(err, ErrNotStuffed) || errors.Is(err, ErrNotFinalizable) || !strings.Contains(err.Error(), "FUL has not recorded") {
		t.Fatalf("the stuffing path's refusal: %v", err)
	}
	d.p2.PlanVersion, d.p2.EvidenceBehind = 1, false
	d.p2.Revision--
	if err := d.m.CheckHouses([]Plan{d.p1, d.p2}, ErrNotStuffed); err != nil {
		t.Fatalf("current: %v", err)
	}
	if err := d.m.CheckHouses([]Plan{d.p1}, ErrNotStuffed); !errors.Is(err, ErrNotStuffed) || !strings.Contains(err.Error(), "was not read") {
		t.Fatalf("a plan not read: %v", err)
	}
	// STALE_HOUSE as Finalize gives it: FUL reports newer facts than SEA froze
	stale := d.p2
	stale.Stuffings = nil
	stale.MeasuredBasis, stale.EvidenceVersion, stale.MeasuredPacks = "", 0, 0
	if err := d.m.CheckHouses([]Plan{d.p1, stale}, ErrNotStuffed); !errors.Is(err, ErrNotStuffed) || !strings.Contains(err.Error(), "STALE_HOUSE") {
		t.Fatalf("stale: %v", err)
	}
	// a co-load movement has no containers of its own: its houses are not asked for FUL's evidence
	c := open(t, coload())
	p := plan("SH-1", "C-1", 1)
	if err := errors.Join(c.Associate(p, "H1", "ops", at), c.RecordMasterBill("COLOAD-1", BillCoLoad, sha, "ops", hr(1))); err != nil {
		t.Fatal(err)
	}
	p.EvidenceBehind = true
	if err := c.Finalize([]Plan{p}, nil, "ops", hr(2)); err != nil {
		t.Fatalf("a co-load with FUL behind: %v", err)
	}
}

// A container no live house names has no PHY seal to compare: none of F1 to F4 apply, as before.
func TestFinalizeAContainerNoHouseNamesIsAsBefore(t *testing.T) {
	d := newReDay(t)
	second, err := AssignContainer("CV-2", d.m, 2, "TEMU1234565", "22G1", plate, "ops", hr(1))
	if err != nil {
		t.Fatal(err)
	}
	// the container of unit 2 is stuffed on a stuffing of unit 2 carried by H1 alone, whose seal is not recorded
	// anywhere: here it is only sealed and given a VGM
	sealed(t, second, "ANY-1", 2)
	st := second.State()
	st.Status, st.ExecutionID, st.StuffedBy, st.StuffedAt = VisitVGM, "X-9", "cfs", hr(21)
	st.Assertions = []VgmAssertion{{Version: 1, MassGrams: 3_700_000, Method: VgmMethod2, PartyID: "LE", PartyRevision: 1, DeterminedAt: hr(22), SubmittedAt: hr(22), By: "ops", At: hr(22)}}
	if err := d.m.Finalize([]Plan{d.p1, d.p2}, []VisitState{d.v.State(), st}, "ops", hr(30)); err != nil {
		t.Fatalf("an empty container: %v", err)
	}
}
