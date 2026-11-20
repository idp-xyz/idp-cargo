package domain

import (
	"strings"
	"testing"
	"time"
)

// A visit stuffed before V2.88 has no stored seal: the rule of the first corrections stands. These are those cases run on
// such a visit (its stored fields cleared), so that each part of that rule has a test of its own.

// legacy clears what SEA took, as a visit stuffed before this version has it.
func legacy(v *Visit) *Visit {
	v.state.StuffedPhySeal, v.state.StuffedPhySealedAt = "", time.Time{}
	return v
}

func legacyStuffing(seal string, hour int) Stuffing {
	s := stuffed(plan("SH-1", "C-1", 1), 1, seal).Stuffings[0]
	s.SealedAt = hr(hour)
	return s
}

// Follow, legacy: a seal dated before StuffedAt is followed without a difference reason, with no history to show a reseal;
// with a reason it is not, unless the history shows one.
func TestLegacyFollowRule(t *testing.T) {
	d := newReDay(t)
	legacy(d.v)
	if done, err := d.v.FollowSeal(legacyStuffing("OTHER-1", 10), "ops", hr(30)); !done || err != nil {
		t.Fatalf("no reason, dated before StuffedAt, no history: %v %v", done, err)
	}
	e := newReDay(t)
	legacy(e.v)
	e.v.state.DifferenceReason = "PHY keyed the seal wrongly"
	if done, err := e.v.FollowSeal(legacyStuffing("OTHER-1", 10), "ops", hr(30)); done || err != nil {
		t.Fatalf("a reason, dated before StuffedAt, no history: %v %v", done, err)
	}
	// after StuffedAt it is followed with the reason too
	g := newReDay(t)
	legacy(g.v)
	g.v.state.DifferenceReason = "PHY keyed the seal wrongly"
	if done, err := g.v.FollowSeal(legacyStuffing("OTHER-1", 25), "ops", hr(30)); !done || err != nil {
		t.Fatalf("a reason, dated after StuffedAt: %v %v", done, err)
	}
	// the history shows it with the reason, dated before StuffedAt
	h := newReDay(t)
	legacy(h.v)
	h.v.state.DifferenceReason = "PHY keyed the seal wrongly"
	history := []Stuffing{legacyStuffing("S1", 20)}
	if done, err := h.v.followSeal(legacyStuffing("OTHER-1", 20), history, "ops", hr(30)); !done || err != nil {
		t.Fatalf("a reason, the history shows another seal: %v %v", done, err)
	}
}

// Resealed, legacy: each part of the rule.
func TestLegacyResealedRule(t *testing.T) {
	d := newReDay(t) // StuffedAt hour 21, the seal S1 recorded at hour 2, PHY's stuffing S1 sealed at hour 20
	st := legacy(d.v).State()
	p := legacyStuffing("S1", 20)
	if st.resealed(p, d.m.sealHistory("X-1")) {
		t.Fatal("PHY's seal is one the container held when it was taken: not resealed")
	}
	// a sealed-at after StuffedAt is a reseal, with a difference reason too
	reasoned := st
	reasoned.DifferenceReason = "PHY keyed the seal wrongly"
	if !reasoned.resealed(legacyStuffing("S1", 25), nil) {
		t.Fatal("sealed after StuffedAt")
	}
	if reasoned.resealed(legacyStuffing("S1", 20), nil) {
		t.Fatal("a reason, nothing changed, dated before StuffedAt: no reseal")
	}
	// the held seal is judged at the business time it was recorded: PHY's seal recorded after StuffedAt was not held then
	late := st
	late.Seals = append(append([]VisitSeal(nil), st.Seals...), VisitSeal{No: 2, Number: "S2", Party: SealShipper, AppliedAt: hr(25), Source: "followed", By: "ops", At: hr(27)})
	if !late.resealed(legacyStuffing("S2", 20), nil) {
		t.Fatal("a seal with P's key recorded after StuffedAt: PHY applied it since")
	}
	if st.resealed(legacyStuffing("S2", 20), nil) != true {
		t.Fatal("a seal the container never held, no reason")
	}
	// a seal recorded before StuffedAt with P's key: held then
	held := st
	if held.resealed(legacyStuffing("S1", 20), nil) {
		t.Fatal("held at StuffedAt")
	}
}

// The history, legacy: PHY's seal by key alone (the same sealed-at) and by sealed-at alone (the same key). The second
// does not arise from PHY, which refuses a broken seal put back, but the function is asked all the same.
func TestLegacyHistoryComparesKeyAndSealedAt(t *testing.T) {
	d := newReDay(t)
	st := legacy(d.v).State()
	st.DifferenceReason = "PHY keyed the seal wrongly"
	p := legacyStuffing("PHY-2", 20)
	if !st.resealed(p, []Stuffing{legacyStuffing("PHY-1", 20)}) {
		t.Fatal("another key, the same sealed-at")
	}
	if !st.resealed(p, []Stuffing{legacyStuffing("phy 2", 19)}) {
		t.Fatal("the same key, another sealed-at")
	}
	if st.resealed(p, []Stuffing{legacyStuffing("phy 2", 20)}) {
		t.Fatal("the same key and sealed-at: unchanged")
	}
	if !sealChanged(p, []Stuffing{legacyStuffing("PHY-1", 20)}) || !sealChanged(p, []Stuffing{legacyStuffing("PHY-2", 19)}) || sealChanged(p, nil) {
		t.Fatal("sealChanged")
	}
}

// Through the movement (the command's way), a legacy container with a difference reason: the history of entries is read.
func TestLegacyFollowThroughTheMovementHistory(t *testing.T) {
	d := stuffedDay(t, "PHY keyed the seal wrongly") // PHY reseals before the stuffing; the stored seal is PHY-2
	legacy(d.v)
	for _, p := range []*Plan{&d.p1, &d.p2} {
		p.Revision++
		p.Stuffings = []Stuffing{p.Stuffings[0]}
		p.Stuffings[0].Seal = "PHY-3" // the same sealed-at as before, another key
		if err := d.m.Refresh(*p, "ops", hr(40)); err != nil {
			t.Fatal(err)
		}
		if err := d.m.FollowSeals(d.m.State().Entries[len(d.m.State().Entries)-1], []*Visit{d.v}, "ops", hr(40)); err != nil {
			t.Fatal(err)
		}
	}
	if d.v.Seal().Number != "PHY-3" || !strings.Contains(d.v.Seal().Source, "PHY-3") {
		t.Fatalf("followed through the history: %+v", d.v.State().Seals)
	}
}
