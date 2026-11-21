package domain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.87 batch 2: what the independent review found. The seal text forms, a reseal recorded in PHY after SEA's stuffing
// but dated before it, the VGM's determination time, F3 per container, a house in two containers, F1 and F2 by seal
// key.

// twoDay is one house H1 with 3 packs in each of two containers CV-1 (unit 1, PHY's X-1, seal S1) and CV-2 (unit 2,
// X-2, seal T1), both taken as stuffed at hour 21 with a VGM at 22.
type twoDay struct {
	t      *testing.T
	m      *Movement
	p      Plan
	v1, v2 *Visit
}

func newTwoDay(t *testing.T) *twoDay {
	t.Helper()
	d := &twoDay{t: t, m: open(t, booking(1))}
	d.p = stuffed(plan("SH-1", "C-1", 1), 1, "S1")
	second := d.p.Stuffings[0]
	second.ExecutionID, second.LoadUnitNo, second.Seal, second.LineNos = "X-2", 2, "T1", []int{2}
	d.p.Stuffings[0].Packs, second.Packs = 3, 3
	d.p.Stuffings = append(d.p.Stuffings, second)
	if err := errors.Join(d.m.Associate(d.p, "H1", "ops", at), d.m.RecordMasterBill("MAEU123456789", BillCarrierMBL, sha, "ops", hr(1))); err != nil {
		t.Fatal(err)
	}
	d.v1 = assigned(t, d.m)
	v2, err := AssignContainer("CV-2", d.m, 2, "TEMU1234565", "22G1", plate, "ops", hr(1))
	if err != nil {
		t.Fatal(err)
	}
	d.v2 = v2
	sealed(t, d.v1, "S1", 2)
	sealed(t, d.v2, "T1", 2)
	for _, v := range []*Visit{d.v1, d.v2} {
		if err = v.StuffingDone(d.m, "", "cfs", hr(21)); err != nil {
			t.Fatal(err)
		}
		d.vgm(v, 22)
	}
	return d
}

func (d *twoDay) vgm(v *Visit, hour int) { d.determined(v, hour, hour) }

func (d *twoDay) determined(v *Visit, determined, submitted int) {
	d.t.Helper()
	if err := v.AssertVGM(d.m, kernel.Quantity{Unit: "KGM", Amount: 3700}, VgmMethod2, "LE", 1, hr(determined), "", "Ada Lovelace", "ops", hr(submitted)); err != nil {
		d.t.Fatal(err)
	}
}

// reseal is PHY sealing the executions given again, at the hour; the house is refreshed and the seals followed.
func (d *twoDay) reseal(hour int, seals map[string]string) {
	d.t.Helper()
	d.p.Revision++
	d.p.Stuffings = append([]Stuffing(nil), d.p.Stuffings...)
	for i, s := range d.p.Stuffings {
		if seal, ok := seals[s.ExecutionID]; ok {
			d.p.Stuffings[i].Seal, d.p.Stuffings[i].SealedAt = seal, hr(hour)
		}
	}
	if err := d.m.Refresh(d.p, "ops", hr(hour)); err != nil {
		d.t.Fatal(err)
	}
	if err := FollowSeals(d.m.State().Entries[len(d.m.State().Entries)-1], []*Visit{d.v1, d.v2}, "ops", hr(hour)); err != nil {
		d.t.Fatal(err)
	}
}

func (d *twoDay) finalize(hour int) error {
	return d.m.Finalize([]Plan{d.p}, []VisitState{d.v1.State(), d.v2.State()}, "ops", hr(hour))
}

func (d *twoDay) event(v *Visit, kind, ref string, occurred, recorded int) {
	d.t.Helper()
	if _, _, err := v.RecordContainerEvent(kind, loc("CNSHG"), hr(occurred), "terminal", ref, "ops", hr(recorded)); err != nil {
		d.t.Fatal(err)
	}
}

// FollowSeals goes on after the first stuffing: a house in two containers, both sealed again, has both seals followed.
func TestAHouseInTwoContainersBothResealedFollowsBothSeals(t *testing.T) {
	d := newTwoDay(t)
	d.reseal(25, map[string]string{"X-1": "S2", "X-2": "T2"})
	if d.v1.Seal().Number != "S2" || d.v2.Seal().Number != "T2" || len(d.v1.State().Seals) != 2 || len(d.v2.State().Seals) != 2 {
		t.Fatalf("seals: %+v %+v", d.v1.State().Seals, d.v2.State().Seals)
	}
	d.vgm(d.v1, 26)
	d.vgm(d.v2, 26)
	if err := d.finalize(30); err != nil {
		t.Fatal(err)
	}
}

// F3 reads the container's own events: another container's loading does not make this one gated in.
func TestF3IsPerContainer(t *testing.T) {
	d := newTwoDay(t)
	d.event(d.v2, EventGateIn, "GIN-2", 22, 22)
	d.event(d.v2, EventLoaded, "LD-2", 23, 23)
	d.reseal(25, map[string]string{"X-1": "S2"})
	d.vgm(d.v1, 26)
	if err := d.finalize(30); err != nil {
		t.Fatalf("CV-2 loaded, CV-1 resealed and never gated in: %v", err)
	}
	// the reworked container itself gated in
	e := newTwoDay(t)
	e.event(e.v1, EventGateIn, "GIN-1", 22, 22)
	e.reseal(25, map[string]string{"X-1": "S2"})
	if err := e.finalize(30); !errors.Is(err, ErrNotFinalizable) || !strings.Contains(err.Error(), "CSQU3054383") || !strings.Contains(err.Error(), "was at the terminal when PHY sealed it again") {
		t.Fatalf("CV-1 gated in: %v", err)
	}
	// its own loading, gate-in dated after the reseal: by its loading
	f := newTwoDay(t)
	f.reseal(25, map[string]string{"X-1": "S2"})
	f.vgm(f.v1, 26)
	f.event(f.v1, EventGateIn, "GIN-1", 26, 26)
	f.event(f.v1, EventLoaded, "LD-1", 24, 27)
	if err := f.finalize(30); !errors.Is(err, ErrNotFinalizable) || !strings.Contains(err.Error(), "by its loading") {
		t.Fatalf("CV-1 loaded: %v", err)
	}
}

// The carrier's loading reports count for a container when they name its number or name no container (its bill), not
// another container's; a time equal to PHY's sealed-at is not before it, for the carrier's report and for the events.
func TestF3CarrierReportsAndBoundaries(t *testing.T) {
	report := func(d *twoDay, container, bill string, occurred int, ref string) {
		t.Helper()
		f := CarrierFact{Kind: CarrierLoaded, ContainerNumber: container, BillNumber: bill, Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line", Reference: ref,
			SHA256: strings.Repeat("ab", 32), OccurredAt: hr(occurred)}
		if _, _, err := d.m.RecordCarrierFact(f, []string{"CSQU3054383", "TEMU1234565"}, "ops", hr(occurred)); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		container, bill string
		occurred        int
		refused         bool
	}{
		"the other container's report":      {"TEMU1234565", "", 23, false},
		"this container's report":           {"CSQU3054383", "", 23, true},
		"a report naming the bill":          {"", "MAEU123456789", 23, false},
		"this container's report at PHY's":  {"CSQU3054383", "", 25, false},
		"a report naming the bill at PHY's": {"", "MAEU123456789", 25, false},
	} {
		d := newTwoDay(t)
		report(d, tc.container, tc.bill, tc.occurred, "LD-9")
		d.reseal(25, map[string]string{"X-1": "S2"})
		d.vgm(d.v1, 26)
		err := d.finalize(30)
		if tc.refused != (err != nil) || (tc.refused && !strings.Contains(err.Error(), "by its carrier's report of its loading") && !strings.Contains(err.Error(), "the carrier's report")) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a loading event equal to PHY's sealed-at is not before it
	d := newTwoDay(t)
	d.reseal(25, map[string]string{"X-1": "S2"})
	d.vgm(d.v1, 26)
	d.event(d.v1, EventGateIn, "GIN-1", 26, 26)
	d.event(d.v1, EventLoaded, "LD-1", 25, 27)
	if err := d.finalize(30); err != nil {
		t.Fatalf("a loading when PHY sealed: %v", err)
	}
}

// F2 by the seal's key: PHY's text keyed differently from the container's seal is the same seal.
func TestF2AndFollowCompareSealsByKey(t *testing.T) {
	d := &reDay{t: t, m: open(t, booking(1))}
	d.p1, d.p2 = stuffed(plan("SH-1", "C-1", 1), 1, "seal-x 1"), stuffed(plan("SH-2", "C-1", 1), 1, "SEAL X1")
	if err := errors.Join(d.m.Associate(d.p1, "H1", "ops", at), d.m.Associate(d.p2, "H2", "ops", at), d.m.RecordMasterBill("MAEU123456789", BillCarrierMBL, sha, "ops", hr(1))); err != nil {
		t.Fatal(err)
	}
	d.v = assigned(t, d.m)
	sealed(t, d.v, "SEALX1", 2)
	if err := d.v.StuffingDone(d.m, "", "cfs", hr(21)); err != nil {
		t.Fatalf("keyed differently, no difference: %v", err)
	}
	d.vgm(22)
	if err := d.finalize(30); err != nil {
		t.Fatalf("F1 and F2 by key: %v", err)
	}
}

// F1: two houses with the same sealed-at and different seal keys are refused; the same key keyed differently agrees.
func TestF1ComparesTheSealKeys(t *testing.T) {
	d := newReDay(t)
	for i, p := range []*Plan{&d.p1, &d.p2} {
		p.Revision++
		p.Stuffings = []Stuffing{p.Stuffings[0]}
		p.Stuffings[0].SealedAt = hr(25)
		p.Stuffings[0].Seal = []string{"S-2", "S3"}[i]
		if err := d.m.Refresh(*p, "ops", hr(26)); err != nil {
			t.Fatal(err)
		}
	}
	refused(t, "different keys, one sealed-at", d.finalize(30), "different seals")
	d.p2.Revision++
	d.p2.Stuffings = []Stuffing{d.p2.Stuffings[0]}
	d.p2.Stuffings[0].Seal = "s 2"
	if err := d.m.Refresh(d.p2, "ops", hr(27)); err != nil {
		t.Fatal(err)
	}
	if done, err := d.v.FollowSeal(d.p1.Stuffings[0], "ops", hr(28)); !done || err != nil {
		t.Fatal(done, err)
	}
	d.vgm(29)
	if err := d.finalize(30); err != nil {
		t.Fatalf("the same key keyed differently: %v", err)
	}
}

// The seal text SEA's form does not take: PHY's text with the runs of other characters made one space when that has the
// same key; otherwise a refusal that says what to do, and a seal recorded by hand with that key lets the refresh pass.
func TestFollowSealTextForms(t *testing.T) {
	newer := func(seal string) Stuffing {
		s := stuffed(plan("SH-1", "C-1", 1), 1, seal).Stuffings[0]
		s.SealedAt = hr(25)
		return s
	}
	for text, want := range map[string]string{"seal#9 new": "SEAL 9 NEW", "  s2  ": "S2", "seal-x 1": "SEAL-X 1", "ab//cd": "AB CD", "ab..cd": "AB CD"} {
		d := newReDay(t)
		if done, err := d.v.FollowSeal(newer(text), "ops", hr(30)); !done || err != nil || d.v.Seal().Number != want ||
			!strings.Contains(d.v.Seal().Source, `(PHY's text "`+text+`")`) {
			t.Errorf("%q: %v %v %+v", text, done, err, d.v.Seal())
		}
	}
	// a text whose cleaned form has no form with the same key (a letter outside A-Z): not recorded, no error; the
	// pending item says PHY has to seal again (the message is asserted with the other forms in
	// TestASealTheRulesCannotTakeIsNotRecordedAndIsPending)
	d := newReDay(t)
	if done, err := d.v.FollowSeal(newer("S\u00c9AL1"), "ops", hr(30)); done || err != nil {
		t.Fatalf("letters outside A-Z: %v %v", done, err)
	}
	// a text whose cleaned form is too long but whose letters and digits fit: not recorded; recorded by hand with that
	// key, the refresh then finds it held
	long := strings.Repeat("AB ", 13) + "C" // 40 characters, 27 letters
	if done, err := d.v.FollowSeal(newer(long), "ops", hr(30)); done || err != nil {
		t.Fatalf("a long text: %v %v", done, err)
	}
	for _, p := range []*Plan{&d.p1, &d.p2} {
		p.Revision++
		p.Stuffings = []Stuffing{newer(long)}
		if err := d.m.Refresh(*p, "ops", hr(26)); err != nil {
			t.Fatal(err)
		}
	}
	it, ok := findItem(d.m.PendingItems([]Plan{d.p1, d.p2}, []VisitState{d.v.State()}), PendSealNotOnContainer)
	if !ok || !strings.Contains(it.Message, "record the container's seal by hand") || !strings.Contains(it.Message, "ABABABABABABABABABABABABABC") {
		t.Fatalf("the pending item: %+v", it)
	}
	if err := d.v.RecordSeal("ABABABABABABABABABABABABABC", SealShipper, hr(30), "by hand", "ops", hr(30)); err != nil {
		t.Fatal(err)
	}
	if done, err := d.v.FollowSeal(newer(long), "ops", hr(31)); done || err != nil {
		t.Fatalf("recorded by hand, the refresh records nothing: %v %v", done, err)
	}
	if _, ok = findItem(d.m.PendingItems([]Plan{d.p1, d.p2}, []VisitState{d.v.State()}), PendSealNotOnContainer); ok {
		t.Fatal("the item is resolved by the seal recorded by hand")
	}
}

// A reseal recorded in PHY after SEA's stuffing but dated before StuffedAt (review F1): followed when the visit has no
// difference reason, and then counted as a reseal.
func TestABackdatedResealIsFollowedAndCounted(t *testing.T) {
	d := newReDay(t) // StuffedAt is hour 21, VGM v1 submitted and determined at 22
	d.resealRecorded("S2", 20, 30)
	if d.v.Seal().Number != "S2" || !d.v.Seal().AppliedAt.Equal(hr(20)) {
		t.Fatalf("seals: %+v", d.v.State().Seals)
	}
	st := d.v.State()
	if !st.resealed(d.p1.Stuffings[0], nil) {
		t.Fatal("the container is resealed")
	}
	// F2 asks for S2, which is recorded; the VGM was determined after the reseal's time: published, as the limits say
	if err := d.finalize(30); err != nil {
		t.Fatalf("a VGM determined after the backdated reseal: %v", err)
	}
	// the stuffed visit's seal S1 is not the stuffing PHY keeps: with a difference reason it is not told apart
	e := newReDay(t)
	e.v.state.DifferenceReason = "PHY keyed the seal wrongly"
	e.v.state.StuffedPhySeal, e.v.state.StuffedPhySealedAt = "", time.Time{} // stuffed before V2.88: the legacy rule
	e.resealRecorded("S2", 20, 30)
	if len(e.v.State().Seals) != 1 {
		t.Fatalf("with a difference reason a backdated seal is not followed: %+v", e.v.State().Seals)
	}
	if e.v.State().resealed(e.p1.Stuffings[0], nil) {
		t.Fatal("with a difference reason it is not counted as resealed")
	}
}

// F4 reads the mass's determination time too, for a resealed container (review F4).
func TestTheVGMIsDeterminedAfterTheReseal(t *testing.T) {
	d := newReDay(t)
	d.reseal("S2", 25, true)
	if err := d.v.AssertVGM(d.m, kernel.Quantity{Unit: "KGM", Amount: 3700}, VgmMethod2, "LE", 1, hr(10), "", "Ada Lovelace", "ops", hr(26)); err != nil {
		t.Fatal(err)
	}
	refused(t, "an old weighing submitted again", d.finalize(30), "mass determined", "before PHY sealed the container again", "assert one determined after it")
	if err := d.v.AssertVGM(d.m, kernel.Quantity{Unit: "KGM", Amount: 3700}, VgmMethod2, "LE", 1, hr(25), "", "Ada Lovelace", "ops", hr(27)); err != nil {
		t.Fatal(err)
	}
	if err := d.finalize(30); err != nil {
		t.Fatalf("determined when PHY sealed: %v", err)
	}
	// without a reseal the determination time is not asked: a container PHY did not seal again keeps its old weighing
	e := newReDay(t)
	if err := e.finalize(30); err != nil {
		t.Fatal(err)
	}
}

// resealRecorded is PHY sealing X-1 again with a sealed-at of one hour and SEA reading it at another (the review's
// chronology: PHY's rework is recorded afterwards with business times before SEA's own).
func (d *reDay) resealRecorded(seal string, sealed, recorded int) {
	d.t.Helper()
	for _, p := range []*Plan{&d.p1, &d.p2} {
		p.Revision++
		p.Stuffings = []Stuffing{p.Stuffings[0]}
		p.Stuffings[0].Seal, p.Stuffings[0].SealedAt = seal, hr(sealed)
		if err := d.m.Refresh(*p, "ops", hr(recorded)); err != nil {
			d.t.Fatal(err)
		}
		if err := FollowSeals(d.m.State().Entries[len(d.m.State().Entries)-1], []*Visit{d.v}, "ops", hr(recorded)); err != nil {
			d.t.Fatal(err)
		}
	}
}
