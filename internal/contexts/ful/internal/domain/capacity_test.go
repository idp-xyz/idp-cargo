package domain

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// attached returns planned's shipment attached to C-1, with a capacity of booking BK-1 departing at etd.
func attached(t *testing.T) (*Shipment, Route, time.Time, func(version int, etd time.Time) Capacity) {
	t.Helper()
	s, route := planned(t)
	at := s.Latest().At.Add(time.Hour)
	m := Membership{ConsolidationID: "C-1", EntryNo: 1, Revision: 1, AttachedAt: at, Mode: ModeSea, Origin: route.Origin, Destination: route.Destination}
	if err := s.RecordConsolAttachment(m, "planner-1", at); err != nil {
		t.Fatal(err)
	}
	capacity := func(version int, etd time.Time) Capacity {
		return Capacity{BookingID: "BK-1", BookingRevision: int64(version + 1), Mode: ModeSea, ConsolidationID: "C-1", CapacityVersion: version, Line: 1,
			CarrierReference: "BKG-881", Legs: []CapacityLeg{{Loading: route.Origin, Discharge: route.Destination, Vessel: "EVER GIVEN", Voyage: "OM-1",
				ETD: etd, ETA: etd.Add(30 * 24 * time.Hour)}}}
	}
	return s, route, at, capacity
}

// V2.110: the main carriage links to BOOK's capacity of its consolidation and links again when the carrier changes it;
// the same carriage changes nothing, and every link is kept.
func TestTheMainCarriageLinksToTheConsolidationsCapacity(t *testing.T) {
	s, route, at, capacity := attached(t)
	within := route.Departure.From.Add(24 * time.Hour)
	other := capacity(1, within)
	other.ConsolidationID = "C-2"
	if _, err := s.LinkCapacity(other, "ops-1", at); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("a booking of another consolidation: %v", err)
	}
	air := capacity(1, within)
	air.Mode = ModeAir
	if _, err := s.LinkCapacity(air, "ops-1", at); !errors.Is(err, ErrCapacityMismatch) {
		t.Fatalf("an air booking for a sea shipment: %v", err)
	}
	bad := capacity(1, within)
	bad.Legs = nil
	if _, err := s.LinkCapacity(bad, "ops-1", at); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a capacity without legs: %v", err)
	}
	if changed, err := s.LinkCapacity(capacity(1, within), "ops-1", at); err != nil || !changed {
		t.Fatalf("link: %v %v", changed, err)
	}
	if changed, err := s.LinkCapacity(capacity(1, within), "ops-1", at.Add(time.Minute)); err != nil || changed {
		t.Fatalf("the same carriage again: %v %v", changed, err)
	}
	if _, _, awaiting := s.AwaitingAnswer(); awaiting {
		t.Fatal("a departure within the window awaits no answer")
	}
	if err := s.AnswerDateChange(1, AnswerAccepted, "mail-1", "ops-1", at.Add(time.Minute)); !errors.Is(err, ErrNothingToAnswer) {
		t.Fatalf("an answer to a departure within the window: %v", err)
	}
	later := capacity(2, within.Add(12*time.Hour)) // a schedule change still within the window
	if changed, err := s.LinkCapacity(later, "ops-1", at.Add(2*time.Minute)); err != nil || !changed {
		t.Fatalf("a new capacity version: %v %v", changed, err)
	}
	if links := s.Links(); len(links) != 2 || links[0].CapacityVersion != 1 || links[1].CapacityVersion != 2 || links[1].PlanNo != s.Latest().No {
		t.Fatalf("links: %+v", links)
	}
	if got := eventList(s); got == "" {
		t.Fatal("no events")
	}
	restored, err := RestoreShipment(s.State())
	if err != nil {
		t.Fatal(err)
	}
	if current, _ := restored.CurrentLink(); current.No != 2 || !current.Departure().Equal(within.Add(12*time.Hour)) {
		t.Fatalf("restored current link: %+v", current)
	}
	if len(s.AddedLinks()) != 2 {
		t.Fatalf("added links: %d", len(s.AddedLinks()))
	}
}

// V2.110, the owner's decision of 2026-11-26: a departure outside the plan's window awaits the customer's answer; a
// refusal keeps it awaiting, an acceptance widens the agreed window to it, a later change asks again, and amending the
// window starts over.
func TestADepartureOutsideTheWindowAwaitsTheCustomersAnswer(t *testing.T) {
	s, route, at, capacity := attached(t)
	outside := route.Departure.Until.Add(48 * time.Hour)
	if _, err := s.LinkCapacity(capacity(1, outside), "ops-1", at); err != nil {
		t.Fatal(err)
	}
	if link, refused, awaiting := s.AwaitingAnswer(); !awaiting || refused || link.No != 1 {
		t.Fatalf("awaiting: %+v %v %v", link, refused, awaiting)
	}
	if w := s.AgreedWindow(); !w.From.Equal(route.Departure.From) || !w.Until.Equal(route.Departure.Until) {
		t.Fatalf("the agreed window before an answer: %+v", w)
	}
	for _, bad := range []struct {
		link             int
		answer, evidence string
	}{{2, AnswerAccepted, "mail-1"}, {1, "MAYBE", "mail-1"}, {1, AnswerAccepted, " "}} {
		if err := s.AnswerDateChange(bad.link, bad.answer, bad.evidence, "ops-1", at); err == nil {
			t.Fatalf("answer %+v taken", bad)
		}
	}
	if err := s.AnswerDateChange(1, AnswerRefused, "mail-1", "ops-1", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, refused, awaiting := s.AwaitingAnswer(); !awaiting || !refused {
		t.Fatalf("after a refusal: refused %v awaiting %v", refused, awaiting)
	}
	if err := s.AnswerDateChange(1, AnswerAccepted, "mail-2", "ops-1", at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, awaiting := s.AwaitingAnswer(); awaiting {
		t.Fatal("an accepted departure still awaits")
	}
	if w := s.AgreedWindow(); !w.From.Equal(route.Departure.From) || !w.Until.Equal(outside.Add(time.Microsecond)) || !w.Contains(outside) {
		t.Fatalf("the agreed window: %+v", w)
	}
	if a, ok := s.Answer(1); !ok || a.No != 2 || a.Answer != AnswerAccepted {
		t.Fatalf("the answer that counts: %+v %v", a, ok)
	}
	if _, err := RestoreShipment(s.State()); err != nil {
		t.Fatal(err)
	}
	// the carrier changes again: a later departure asks again, and the earlier link takes no answer now
	again := outside.Add(24 * time.Hour)
	if _, err := s.LinkCapacity(capacity(2, again), "ops-1", at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if link, _, awaiting := s.AwaitingAnswer(); !awaiting || link.No != 2 {
		t.Fatalf("a second change: %+v %v", link, awaiting)
	}
	if err := s.AnswerDateChange(1, AnswerAccepted, "mail-3", "ops-1", at.Add(4*time.Minute)); !errors.Is(err, ErrNothingToAnswer) {
		t.Fatalf("an answer to a superseded link: %v", err)
	}
	// an earlier departure than the window widens its start once accepted
	early := route.Departure.From.Add(-24 * time.Hour)
	if _, err := s.LinkCapacity(capacity(3, early), "ops-1", at.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerDateChange(3, AnswerAccepted, "mail-4", "ops-1", at.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// an acceptance is of its date: the agreed window is the plan's widened to this carriage's departure alone
	if w := s.AgreedWindow(); !w.From.Equal(early) || !w.Until.Equal(route.Departure.Until) {
		t.Fatalf("widened to the earlier departure alone: %+v", w)
	}
	// amending the window starts over: the acceptances answered the old one
	amended := route
	amended.Departure = kernel.ValidityInterval{From: route.Departure.From, Until: route.Departure.Until.Add(time.Hour)}
	if err := s.AmendPlan(amended, "the customer moved the window", "planner-1", at.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if w := s.AgreedWindow(); !w.From.Equal(amended.Departure.From) || !w.Until.Equal(amended.Departure.Until) {
		t.Fatalf("the agreed window after an amendment: %+v", w)
	}
	if link, refused, awaiting := s.AwaitingAnswer(); !awaiting || refused || link.No != 3 {
		t.Fatalf("after an amendment the current link awaits again: %+v %v %v", link, refused, awaiting)
	}
	restored, err := RestoreShipment(s.State())
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Answers()) != 3 || len(restored.Links()) != 3 {
		t.Fatalf("restored: %d answers, %d links", len(restored.Answers()), len(restored.Links()))
	}
}

// V2.110, after the review: an acceptance is of its date. A later carriage departing at another date asks again even
// inside an earlier accepted range, and its refusal counts; a carriage departing at an accepted date carries the
// acceptance; amending the window back does not revive an answer given since; a carriage of a consolidation the
// shipment has left awaits nothing until the new one's capacity is linked.
func TestAnAcceptanceIsOfItsDate(t *testing.T) {
	s, route, at, capacity := attached(t)
	later := route.Departure.Until.Add(5 * 24 * time.Hour)   // the customer accepts this
	between := route.Departure.Until.Add(3 * 24 * time.Hour) // inside what the acceptance widened, but another date
	step := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	if _, err := s.LinkCapacity(capacity(1, later), "ops-1", step(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerDateChange(1, AnswerAccepted, "mail-1", "ops-1", step(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkCapacity(capacity(2, between), "ops-1", step(3)); err != nil {
		t.Fatal(err)
	}
	if link, refused, awaiting := s.AwaitingAnswer(); !awaiting || refused || link.No != 2 {
		t.Fatalf("another date inside the earlier acceptance asks again: %+v %v %v", link, refused, awaiting)
	}
	if err := s.AnswerDateChange(2, AnswerRefused, "mail-2", "ops-1", step(4)); err != nil {
		t.Fatal(err)
	}
	if _, refused, awaiting := s.AwaitingAnswer(); !awaiting || !refused {
		t.Fatalf("the refusal counts: refused %v, awaiting %v", refused, awaiting)
	}
	if w := s.AgreedWindow(); !w.Until.Equal(route.Departure.Until) {
		t.Fatalf("a refused carriage widens nothing: %+v", w)
	}
	// back to the accepted date under a new capacity version: the acceptance carries
	if _, err := s.LinkCapacity(capacity(3, later), "ops-1", step(5)); err != nil {
		t.Fatal(err)
	}
	if _, _, awaiting := s.AwaitingAnswer(); awaiting {
		t.Fatal("the accepted date again awaits")
	}
	if w := s.AgreedWindow(); !w.Until.Equal(later.Add(time.Microsecond)) {
		t.Fatalf("the accepted date carried: %+v", w)
	}
	// amend the window, refuse under it, amend it back: the acceptance given under the first window does not revive
	if err := s.AnswerDateChange(3, AnswerAccepted, "mail-3", "ops-1", step(6)); err != nil {
		t.Fatal(err)
	}
	moved := route
	moved.Departure = kernel.ValidityInterval{From: route.Departure.From, Until: route.Departure.Until.Add(time.Hour)}
	if err := s.AmendPlan(moved, "the customer moved the window", "planner-1", step(7)); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerDateChange(3, AnswerRefused, "mail-4", "ops-1", step(8)); err != nil {
		t.Fatal(err)
	}
	if err := s.AmendPlan(route, "back to the first window", "planner-1", step(9)); err != nil {
		t.Fatal(err)
	}
	if _, refused, awaiting := s.AwaitingAnswer(); !awaiting || refused {
		t.Fatalf("after the window is amended back: refused %v, awaiting %v", refused, awaiting)
	}
	if _, err := RestoreShipment(s.State()); err != nil {
		t.Fatal(err)
	}

	// the shipment moves to C-2: C-1's carriage is not its carriage any more
	m := Membership{ConsolidationID: "C-2", EntryNo: 1, Revision: 2, AttachedAt: step(10), Mode: ModeSea, Origin: route.Origin, Destination: route.Destination}
	if err := s.RecordConsolAttachment(m, "planner-1", step(10)); err != nil {
		t.Fatal(err)
	}
	if _, _, awaiting := s.AwaitingAnswer(); awaiting {
		t.Fatal("the old consolidation's carriage awaits")
	}
	if err := s.AnswerDateChange(3, AnswerAccepted, "mail-5", "ops-1", step(11)); !errors.Is(err, ErrNothingToAnswer) {
		t.Fatalf("an answer to the old consolidation's carriage: %v", err)
	}
	if w := s.AgreedWindow(); !w.Until.Equal(route.Departure.Until) {
		t.Fatalf("the agreed window without a carriage: %+v", w)
	}
}

// A stored answer dated after the next link was made, or before its own link, does not restore; a cancelled shipment
// links nothing, and one whose packs all departed is linked and answered (its bill may be issued after the departure).
func TestCapacityLinksRestoreOnlyAsRecorded(t *testing.T) {
	s, route, at, capacity := attached(t)
	outside := route.Departure.Until.Add(48 * time.Hour)
	if _, err := s.LinkCapacity(capacity(1, outside), "ops-1", at); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerDateChange(1, AnswerAccepted, "mail-1", "ops-1", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkCapacity(capacity(2, outside.Add(time.Hour)), "ops-1", at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	st := s.State()
	st.Answers[0].At = at.Add(3 * time.Minute) // after link 2 was made
	if _, err := RestoreShipment(st); !errors.Is(err, ErrInvalidShipment) {
		t.Fatalf("an answer to a superseded link: %v", err)
	}
	st = s.State()
	st.Answers[0].At = at.Add(-time.Minute)
	if _, err := RestoreShipment(st); !errors.Is(err, ErrInvalidShipment) {
		t.Fatalf("an answer before its link: %v", err)
	}
	st = s.State()
	st.Links[1].No = 3
	if _, err := RestoreShipment(st); !errors.Is(err, ErrInvalidShipment) {
		t.Fatalf("links not numbered from 1: %v", err)
	}

	// all packs departed on a later vessel: the link and the customer's answer are still recorded
	d, route2, at2, capacity2 := attached(t)
	e := LoadEvidence{Lots: 1, ReceiptComplete: true, Received: CargoScope{Packs: 4, GrossWeight: kg(400)}, ConsolidationID: "C-1",
		Tendered: CargoScope{Packs: 4, GrossWeight: kg(400)}, Departed: 4}
	if _, err := d.RecordEvidence(e, "ops-1", at2); err != nil {
		t.Fatal(err)
	}
	departedLate := route2.Departure.Until.Add(24 * time.Hour)
	if _, err := d.LinkCapacity(capacity2(1, departedLate), "ops-1", at2); err != nil {
		t.Fatalf("a departed shipment: %v", err)
	}
	if err := d.AnswerDateChange(1, AnswerAccepted, "mail-late", "ops-1", at2.Add(time.Minute)); err != nil {
		t.Fatalf("an answer after the departure: %v", err)
	}
	if w := d.AgreedWindow(); !w.Contains(departedLate) {
		t.Fatalf("the agreed window after the departure: %+v", w)
	}
	c, route3, at3, capacity3 := attached(t)
	if err := c.Cancel("the customer cancelled", "planner-1", at3); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LinkCapacity(capacity3(1, route3.Departure.From), "ops-1", at3); !errors.Is(err, ErrShipmentCancelled) {
		t.Fatalf("a cancelled shipment: %v", err)
	}
	if _, _, awaiting := c.AwaitingAnswer(); awaiting {
		t.Fatal("a cancelled shipment awaits nothing")
	}
}
