package domain

import (
	"errors"
	"strings"
	"testing"
)

// The pending items of a movement (V2.88): each kind, blocking or warning, what Finalize does with them.

func (d *reDay) items() []PendingItem {
	return d.m.PendingItems([]Plan{d.p1, d.p2}, []VisitState{d.v.State()})
}

func kinds(items []PendingItem) map[string]PendingItem {
	out := map[string]PendingItem{}
	for _, it := range items {
		if _, dup := out[it.Kind]; !dup {
			out[it.Kind] = it
		}
	}
	return out
}

func wantItem(t *testing.T, label string, items []PendingItem, kind string, blocking bool, parts ...string) PendingItem {
	t.Helper()
	it, ok := kinds(items)[kind]
	if !ok || it.Blocking != blocking {
		t.Fatalf("%s: no %s (blocking %v) in %+v", label, kind, blocking, items)
	}
	for _, p := range parts {
		if !strings.Contains(it.Message, p) {
			t.Fatalf("%s: %q does not say %q", label, it.Message, p)
		}
	}
	return it
}

func TestAMovementReadyToFinalizeHasNoPendingItems(t *testing.T) {
	d := newReDay(t)
	if items := d.items(); len(items) != 0 {
		t.Fatalf("pending: %+v", items)
	}
}

// Every blocking kind: listed with who does what, and Finalize refuses with the first and the count of the others.
func TestEveryBlockingPendingItemIsListedAndRefusesFinalize(t *testing.T) {
	type tc struct {
		name, kind string
		scope      string
		subject    string
		parts      []string
		build      func(*reDay)
	}
	for _, c := range []tc{
		{"plan not read", PendHousePlanNotRead, ScopeHouse, "SH-2", []string{"FUL's plan of SH-2 was not read"}, func(d *reDay) { d.p2.ShipmentID = "SH-9" }},
		{"house no longer reported", PendHouseNotReported, ScopeHouse, "SH-2", []string{"STALE_HOUSE: FUL no longer reports", "refresh the house, or end its association"}, func(d *reDay) { d.p2.ConsolidationID = "" }},
		{"FUL behind PHY", PendFULBehind, ScopeHouse, "SH-1", []string{"FUL has not recorded PHY's latest facts", "refresh the house if SEA then reports it STALE_HOUSE"}, func(d *reDay) { d.p1.EvidenceBehind = true }},
		{"house stale", PendHouseStale, ScopeHouse, "SH-1", []string{"STALE_HOUSE: FUL reports newer facts", "refresh it first"}, func(d *reDay) { d.p1.PlanVersion = 2 }},
		{"no seal", PendContainerNoSeal, ScopeContainer, "CSQU3054383", []string{"has no seal", "RecordContainerSeal"}, func(d *reDay) { d.v.state.Seals = nil }},
		{"houses disagree", PendHousesDisagree, ScopeContainer, "CSQU3054383", []string{"carry different seals", "refresh the houses"}, func(d *reDay) {
			d.p1.Stuffings = []Stuffing{d.p1.Stuffings[0]}
			d.p1.Stuffings[0].Seal, d.p1.Stuffings[0].SealedAt = "S3", hr(25)
			d.p1.Revision++
			if err := d.m.Refresh(d.p1, "ops", hr(26)); err != nil {
				t.Fatal(err)
			}
		}},
		{"PHY's seal not on the container", PendSealNotOnContainer, ScopeContainer, "CSQU3054383", []string{"none of the seals PHY sealed", "refresh a house that carries it"}, func(d *reDay) {
			d.resealRecorded("S2", 25, 26)
			d.v.state.Seals = d.v.state.Seals[:1]
		}},
		{"a later shipper seal", PendLaterShipperSeal, ScopeContainer, "CSQU3054383", []string{"SHIPPER seal SHP-9", "only a customs or carrier seal may follow it"}, func(d *reDay) {
			if err := d.v.RecordSeal("SHP-9", SealShipper, hr(23), "tally", "ops", hr(23)); err != nil {
				t.Fatal(err)
			}
		}},
		{"gated in before the reseal", PendGatedBeforeReseal, ScopeContainer, "CSQU3054383", []string{"was at the terminal when PHY sealed it again", "GATE_OUT_TERMINAL", "assert a new VGM"}, func(d *reDay) {
			d.event(EventGateIn, "GIN-1", 22, 22)
			d.reseal("S2", 25, true)
		}},
		{"no VGM", PendNoVGM, ScopeContainer, "CSQU3054383", []string{"has no VGM submitted by the cut-off", "AssertVgm"}, func(d *reDay) { d.v.state.Assertions = nil }},
		{"VGM submitted before the reseal", PendVGMBeforeReseal, ScopeContainer, "CSQU3054383", []string{"no VGM submitted after PHY sealed it", "assert a new one"}, func(d *reDay) { d.reseal("S2", 25, true) }},
		{"VGM determined before the reseal", PendVGMBeforeReseal, ScopeContainer, "CSQU3054383", []string{"mass determined", "assert one determined after it"}, func(d *reDay) {
			d.reseal("S2", 25, true)
			if err := d.v.AssertVGM(d.m, kg(3700), VgmMethod2, "LE", 1, hr(10), "", "Ada", "ops", hr(26)); err != nil {
				t.Fatal(err)
			}
		}},
		{"house not stuffed", PendHouseNotStuffed, ScopeHouse, "SH-2", []string{"is not stuffed in a container"}, func(d *reDay) {
			d.p2 = plan("SH-2", "C-1", 1)
			d.p2.Revision++
			if err := d.m.Refresh(d.p2, "ops", hr(26)); err != nil {
				t.Fatal(err)
			}
		}},
		{"house names an unknown container", PendHouseUnknownContainer, ScopeHouse, "SH-1", []string{"names a stuffing of unit 1 that is not one of the movement's containers"}, func(d *reDay) { d.v.state.ExecutionID = "X-9" }},
		{"packs not stuffed", PendHousePacks, ScopeHouse, "SH-1", []string{"has 6 packs and 3 stuffed"}, func(d *reDay) {
			d.p1.Stuffings = []Stuffing{d.p1.Stuffings[0]}
			d.p1.Stuffings[0].Packs = 3
			d.p1.Revision++
			if err := d.m.Refresh(d.p1, "ops", hr(26)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		d := newReDay(t)
		c.build(d)
		items := d.items()
		it := wantItem(t, c.name, items, c.kind, true, c.parts...)
		if it.Scope != c.scope || it.SubjectID != c.subject {
			t.Fatalf("%s: %+v", c.name, it)
		}
		err := d.finalize(40)
		if !errors.Is(err, ErrNotFinalizable) {
			t.Fatalf("%s: finalize %v", c.name, err)
		}
		blocking := 0
		var first PendingItem
		for _, x := range items {
			if x.Blocking {
				if blocking == 0 {
					first = x
				}
				blocking++
			}
		}
		if !strings.Contains(err.Error(), first.Message) || (blocking > 1) != strings.Contains(err.Error(), "more pending items") {
			t.Fatalf("%s: finalize %q does not name its first item %q (%d blocking)", c.name, err, first.Message, blocking)
		}
	}
}

// Warnings do not block: a finalize that passes today passes with them.
func TestWarningsDoNotBlockFinalize(t *testing.T) {
	// the reseal is dated before StuffedAt (the times are out of order) and the VGM was entered before SEA entered the
	// seal
	d := newReDay(t)
	d.resealRecorded("S2", 20, 30)
	d.vgm(31)
	st := d.v.State()
	st.Seals[len(st.Seals)-1].EnteredAt = hr(40)
	st.Assertions[len(st.Assertions)-1].EnteredAt = hr(30)
	items := d.m.PendingItems([]Plan{d.p1, d.p2}, []VisitState{st})
	it := wantItem(t, "VGM entered before the seal was known", items, WarnVGMEnteredBeforeSealKnown, false, "before SEA learned of PHY's new seal", "check that its mass was determined after the reseal")
	if it.Scope != ScopeContainer || it.SubjectID != "CSQU3054383" || len(items) != 2 {
		t.Fatalf("%+v", items)
	}
	if err := d.m.Finalize([]Plan{d.p1, d.p2}, []VisitState{st}, "ops", hr(50)); err != nil {
		t.Fatalf("a warning blocked finalize: %v", err)
	}
	// in the ordinary order (the reseal after StuffedAt) the same entry times warn of nothing: the determination time
	// blocks an old weighing already
	e := newReDay(t)
	e.reseal("S2", 25, true)
	e.vgm(26)
	st = e.v.State()
	st.Seals[len(st.Seals)-1].EnteredAt = hr(40)
	st.Assertions[len(st.Assertions)-1].EnteredAt = hr(30)
	if items = e.m.PendingItems([]Plan{e.p1, e.p2}, []VisitState{st}); len(items) != 0 {
		t.Fatalf("pending: %+v", items)
	}
	// a reseal dated before StuffedAt, told only by the seal the container did not hold
	f := newReDay(t)
	f.resealRecorded("S2", 20, 30)
	f.vgm(31)
	items = f.items()
	wantItem(t, "times out of order", items, WarnResealDatedBeforeStuffing, false, "the times are out of order")
	if err := f.finalize(40); err != nil {
		t.Fatalf("a warning blocked finalize: %v", err)
	}
	// without a reseal there is nothing to warn about
	if items = newReDay(t).items(); len(items) != 0 {
		t.Fatalf("pending: %+v", items)
	}
}

// A co-load movement has only the houses' items (no containers, no FUL evidence asked).
func TestACoLoadHasOnlyHouseItems(t *testing.T) {
	c := open(t, coload())
	p := plan("SH-1", "C-1", 1)
	if err := c.Associate(p, "H1", "ops", at); err != nil {
		t.Fatal(err)
	}
	p.EvidenceBehind = true
	if items := c.PendingItems([]Plan{p}, nil); len(items) != 0 {
		t.Fatalf("pending: %+v", items)
	}
	p.PlanVersion = 2
	items := c.PendingItems([]Plan{p}, nil)
	if len(items) != 1 || items[0].Kind != PendHouseStale {
		t.Fatalf("pending: %+v", items)
	}
}

// Finalize names the houses' items before the bill's, and the bill's before the containers'.
func TestFinalizeNamesTheHouseItemsBeforeTheBillAndTheBillBeforeTheContainers(t *testing.T) {
	d := newReDay(t)
	d.m.state.Bills[0].Kind = BillCoLoad // not the carrier's bill for an own container
	d.p1.PlanVersion = 2
	refused(t, "a stale house first", d.finalize(40), "STALE_HOUSE: FUL reports newer facts")
	d.p1.PlanVersion = 1
	d.v.state.Assertions = nil
	refused(t, "the bill before a container's VGM", d.finalize(40), "the carrier's master bill reference")
}

// Without a reseal F3 and the determination time are not asked: a gate-in dated before PHY's sealed-at, and a mass
// determined before it, are those of a container PHY did not seal again.
func TestF3AndTheDeterminationTimeAreAskedOfAResealedContainerOnly(t *testing.T) {
	d := newReDay(t)
	d.event(EventGateIn, "GIN-0", 10, 22)
	if err := d.finalize(40); err != nil {
		t.Fatalf("a gate-in dated before PHY's sealed-at, no reseal: %v", err)
	}
	e := newReDay(t)
	e.v.state.Assertions[0].DeterminedAt = hr(10)
	if err := e.finalize(40); err != nil {
		t.Fatalf("a mass determined before PHY's sealed-at, no reseal: %v", err)
	}
}

// A container taken as stuffed with a difference reason, whose PHY seal is later changed and recorded with a business
// time before StuffedAt (review G1): the movement's history of entries, the ended ones included, shows PHY sealed it
// again.
func TestAResealTheBusinessTimesCannotShowIsTakenFromTheEntryHistory(t *testing.T) {
	build := func() *reDay {
		d := &reDay{t: t, m: open(t, booking(1))}
		d.p1, d.p2 = stuffed(plan("SH-1", "C-1", 1), 1, "PHY-1"), stuffed(plan("SH-2", "C-1", 1), 1, "PHY-1")
		if err := errors.Join(d.m.Associate(d.p1, "H1", "ops", at), d.m.Associate(d.p2, "H2", "ops", at), d.m.RecordMasterBill("MAEU123456789", BillCarrierMBL, sha, "ops", hr(1))); err != nil {
			t.Fatal(err)
		}
		d.v = assigned(t, d.m)
		sealed(t, d.v, "OURS-1", 2)
		if err := d.v.StuffingDone(d.m, "PHY keyed the seal wrongly", "cfs", hr(30)); err != nil {
			t.Fatal(err)
		}
		d.vgm(31)
		return d
	}
	// PHY's seal never changed: today's behaviour, no reseal, nothing pending
	d := build()
	if d.v.State().resealed(d.p1.Stuffings[0], d.m.sealHistory("X-1")) || len(d.items()) != 0 {
		t.Fatalf("a difference reason, a seal that never changed: %+v", d.items())
	}
	if err := d.finalize(40); err != nil {
		t.Fatal(err)
	}
	// PHY reseals, dated 25 (before StuffedAt 30), and the houses are refreshed through the movement: the history shows
	// it
	e := build()
	for _, p := range []*Plan{&e.p1, &e.p2} {
		p.Revision++
		p.Stuffings = []Stuffing{p.Stuffings[0]}
		p.Stuffings[0].Seal, p.Stuffings[0].SealedAt = "PHY-2", hr(25)
		if err := e.m.Refresh(*p, "ops", hr(34)); err != nil {
			t.Fatal(err)
		}
		if err := e.m.FollowSeals(e.m.State().Entries[len(e.m.State().Entries)-1], []*Visit{e.v}, "ops", hr(34)); err != nil {
			t.Fatal(err)
		}
	}
	if !e.v.State().resealed(e.p1.Stuffings[0], e.m.sealHistory("X-1")) || e.v.Seal().Number != "PHY-2" {
		t.Fatalf("followed through the history: %+v", e.v.State().Seals)
	}
	items := e.items()
	wantItem(t, "times out of order", items, WarnResealDatedBeforeStuffing, false)
	// F2 is no longer exempted by the reason; the VGM v1 was determined after PHY's seal (31 after 25), so it stands,
	// with the warning
	if err := e.finalize(40); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// without the history (the seal not followed) the old behaviour would publish OURS-1: F2 asks for PHY's seal
	// instead
	f := build()
	for _, p := range []*Plan{&f.p1, &f.p2} {
		p.Revision++
		p.Stuffings = []Stuffing{p.Stuffings[0]}
		p.Stuffings[0].Seal, p.Stuffings[0].SealedAt = "PHY-2", hr(25)
		if err := f.m.Refresh(*p, "ops", hr(34)); err != nil {
			t.Fatal(err)
		}
	}
	wantItem(t, "the seal is not on the container", f.items(), PendSealNotOnContainer, true, "none of the seals PHY sealed")
	refused(t, "finalize", f.finalize(40), "none of the seals PHY sealed")
	// the VGM determined before the reseal is blocked when the history shows it (determined 10, PHY sealed at 25)
	g := build()
	for _, p := range []*Plan{&g.p1, &g.p2} {
		p.Revision++
		p.Stuffings = []Stuffing{p.Stuffings[0]}
		p.Stuffings[0].Seal, p.Stuffings[0].SealedAt = "PHY-2", hr(25)
		if err := g.m.Refresh(*p, "ops", hr(34)); err != nil {
			t.Fatal(err)
		}
		if err := g.m.FollowSeals(g.m.State().Entries[len(g.m.State().Entries)-1], []*Visit{g.v}, "ops", hr(34)); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.v.AssertVGM(g.m, kg(3700), VgmMethod2, "LE", 1, hr(10), "", "Ada", "ops", hr(35)); err != nil {
		t.Fatal(err)
	}
	wantItem(t, "the weighing is older than the reseal", g.items(), PendVGMBeforeReseal, true, "mass determined")
}

// A reseal dated to the very instant of the first seal, with another seal: the history shows it by the seal's key
// alone, the sealed-at being the same (PHY refuses a broken seal put back, so a reseal always changes the key).
func TestAResealWithTheSameSealedAtAndAnotherKeyIsTakenFromTheEntryHistory(t *testing.T) {
	d := &reDay{t: t, m: open(t, booking(1))}
	d.p1, d.p2 = stuffed(plan("SH-1", "C-1", 1), 1, "PHY-1"), stuffed(plan("SH-2", "C-1", 1), 1, "PHY-1")
	if err := errors.Join(d.m.Associate(d.p1, "H1", "ops", at), d.m.Associate(d.p2, "H2", "ops", at), d.m.RecordMasterBill("MAEU123456789", BillCarrierMBL, sha, "ops", hr(1))); err != nil {
		t.Fatal(err)
	}
	d.v = assigned(t, d.m)
	sealed(t, d.v, "OURS-1", 2)
	if err := d.v.StuffingDone(d.m, "PHY keyed the seal wrongly", "cfs", hr(30)); err != nil {
		t.Fatal(err)
	}
	d.vgm(31)
	sealedAt := d.p1.Stuffings[0].SealedAt
	for _, p := range []*Plan{&d.p1, &d.p2} {
		p.Revision++
		p.Stuffings = []Stuffing{p.Stuffings[0]}
		p.Stuffings[0].Seal = "PHY-2" // the same sealed-at as the seal before
		if err := d.m.Refresh(*p, "ops", hr(34)); err != nil {
			t.Fatal(err)
		}
		if err := d.m.FollowSeals(d.m.State().Entries[len(d.m.State().Entries)-1], []*Visit{d.v}, "ops", hr(34)); err != nil {
			t.Fatal(err)
		}
	}
	if !d.p1.Stuffings[0].SealedAt.Equal(sealedAt) || d.v.Seal().Number != "PHY-2" || !d.v.State().resealed(d.p1.Stuffings[0], d.m.sealHistory("X-1")) {
		t.Fatalf("resealed and followed by the key alone: %+v", d.v.State().Seals)
	}
}
