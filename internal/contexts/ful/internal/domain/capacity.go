package domain

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FUL V0.4, the capacity a shipment's main carriage travels on (LINK_CONFIRMED_CAPACITY and RECORD_CAPACITY_VERSION;
// stage 4, V2.110). FUL links the main carriage to BOOK's confirmed capacity of the consolidation it records the
// shipment attached to, and links again when BOOK holds a new capacity version or the shipment's cargo moves to
// another booking (a schedule change, a rollover, a reduction, a container rolled to another movement): each link is
// kept, so the original stays visible. A link never changes the plan or the execution status.
//
// When the first departure of the current carriage (the latest link, of the consolidation the shipment is attached to
// now) lies outside the plan's departure window, the customer is asked (the owner's decision of 2026-11-26: "accept the
// new date, or change the window"): the answer is recorded against the link, with its evidence. An acceptance is of that
// date: it widens the window the customer agreed to, which AIR and SEA hold the house to and DOC issues by, to that
// departure, and a later link departing at another date asks again. The link's own latest answer decides; a link to
// the same departure as an earlier accepted one carries that acceptance. An answer counts while the plan's departure
// window is the one it answered: amending the window asks again, and amending it back does not revive an answer given
// since. A link is a record and changes neither the plan nor the execution status, so a shipment whose packs departed
// is linked and answered too: its bill may be issued after the departure.

var (
	ErrCapacityMismatch = errors.New("ful: the booking's capacity does not fit the shipment")
	ErrNothingToAnswer  = errors.New("ful: the shipment's capacity departs within its window, or the link is not the current one")
)

// Customer answers to a departure outside the window.
const (
	AnswerAccepted = "ACCEPTED"
	AnswerRefused  = "REFUSED"
)

// CapacityLeg is a confirmed leg as BOOK holds it: the carrier's flight or vessel and voyage, its loading and discharge
// places, and the carrier's estimated departure and arrival (ETA zero when the carrier gave none).
type CapacityLeg struct {
	Loading   kernel.LocationCode
	Discharge kernel.LocationCode
	Flight    string
	Vessel    string
	Voyage    string
	ETD       time.Time
	ETA       time.Time
}

// Capacity is what FUL reads of BOOK's CarrierBooking v1 for a link: the booking and its revision, its mode, the
// consolidation its capacity is allocated to, the current capacity version and the line, with the carrier's reference
// and legs.
type Capacity struct {
	BookingID        string
	BookingRevision  int64
	Mode             TransportMode
	ConsolidationID  string
	CapacityVersion  int
	Line             int
	CarrierReference string
	Legs             []CapacityLeg
}

func (c Capacity) valid() bool {
	if !identifier(c.BookingID) || c.BookingRevision < 1 || !c.Mode.Valid() || !identifier(c.ConsolidationID) || c.CapacityVersion < 1 ||
		c.Line < 1 || (c.CarrierReference != "" && !text(c.CarrierReference)) || len(c.Legs) == 0 {
		return false
	}
	for _, l := range c.Legs {
		if l.ETD.IsZero() || (!l.ETA.IsZero() && l.ETA.Before(l.ETD)) || (l.Flight != "" && !identifier(l.Flight)) ||
			(l.Vessel != "" && !text(l.Vessel)) || (l.Voyage != "" && !identifier(l.Voyage)) || !l.Loading.IsZero() && !identifier(l.Loading.Code) ||
			!l.Discharge.IsZero() && !identifier(l.Discharge.Code) {
			return false
		}
	}
	return true
}

// same reports whether two capacities are the same carriage: the booking, capacity version and line, and the legs.
func (c Capacity) same(o Capacity) bool {
	return c.BookingID == o.BookingID && c.CapacityVersion == o.CapacityVersion && c.Line == o.Line && c.CarrierReference == o.CarrierReference &&
		c.ConsolidationID == o.ConsolidationID && slices.EqualFunc(c.Legs, o.Legs, func(a, b CapacityLeg) bool {
		return a.Loading == b.Loading && a.Discharge == b.Discharge && a.Flight == b.Flight && a.Vessel == b.Vessel && a.Voyage == b.Voyage &&
			a.ETD.Equal(b.ETD) && a.ETA.Equal(b.ETA)
	})
}

func sameInterval(a, b kernel.ValidityInterval) bool {
	return a.From.Equal(b.From) && a.Until.Equal(b.Until)
}

// Departure is the first leg's estimated departure.
func (c Capacity) Departure() time.Time { return c.Legs[0].ETD }

// CapacityLink is one link of the main carriage to a booking's capacity: numbered from 1, with the plan version it
// was made under. Rows are immutable.
type CapacityLink struct {
	No     int
	Stage  string
	PlanNo int
	Capacity
	By        string
	At        time.Time
	EnteredAt time.Time // when FUL recorded it (T1)
}

// DateAnswer is the customer's answer to a link whose departure lies outside the window: ACCEPTED or REFUSED, with
// the evidence of it (an e-mail, a signed note) and the departure window it answered. Rows are immutable.
type DateAnswer struct {
	No          int
	LinkNo      int
	Answer      string
	EvidenceRef string
	Window      kernel.ValidityInterval
	By          string
	At          time.Time
	EnteredAt   time.Time
}

// LinkCapacity links the main carriage to BOOK's capacity: allocated to the consolidation FUL records the shipment
// attached to, of the plan's mode. The same carriage as the current link changes nothing and reports false. A cancelled
// shipment links none.
func (s *Shipment) LinkCapacity(c Capacity, by string, at time.Time) (bool, error) {
	if s.state.Status == ShipmentCancelled {
		return false, ErrShipmentCancelled
	}
	if !c.valid() {
		return false, fmt.Errorf("%w: capacity", ErrInvalidChange)
	}
	a, ok := s.Attachment(StageMain)
	if !ok || a.ConsolidationID != c.ConsolidationID {
		return false, fmt.Errorf("%w: booking %s is allocated to %s", ErrNotAttached, c.BookingID, c.ConsolidationID)
	}
	if mode := s.Latest().Route.Mode; c.Mode != mode {
		return false, fmt.Errorf("%w: booking %s is %s, the plan %s", ErrCapacityMismatch, c.BookingID, c.Mode, mode)
	}
	if current, ok := s.CurrentLink(); ok && current.Capacity.same(c) {
		return false, nil
	}
	if err := s.changeable(by, at); err != nil {
		return false, err
	}
	c.Legs = slices.Clone(c.Legs)
	s.state.Links = append(s.state.Links, CapacityLink{No: len(s.state.Links) + 1, Stage: StageMain, PlanNo: s.Latest().No, Capacity: c,
		By: by, At: at.UTC()})
	s.pending = append(s.pending, Event{Type: eventCapacityLinked, At: at.UTC(), TenantID: s.state.TenantID, SubjectID: string(s.state.ShipmentID),
		Stage: StageMain, ConsolidationID: c.ConsolidationID, LinkNo: len(s.state.Links)})
	return true, nil
}

// AnswerDateChange records the customer's answer to the current link, whose departure lies outside the plan's
// departure window.
func (s *Shipment) AnswerDateChange(linkNo int, answer, evidence, by string, at time.Time) error {
	if s.state.Status == ShipmentCancelled {
		return ErrShipmentCancelled
	}
	if (answer != AnswerAccepted && answer != AnswerRefused) || !text(evidence) {
		return fmt.Errorf("%w: answer %q with its evidence", ErrInvalidChange, answer)
	}
	current, ok := s.carriage()
	window := s.Latest().Route.Departure
	if !ok || current.No != linkNo || window.Contains(current.Departure()) {
		return fmt.Errorf("%w: link %d", ErrNothingToAnswer, linkNo)
	}
	if err := s.changeable(by, at); err != nil {
		return err
	}
	s.state.Answers = append(s.state.Answers, DateAnswer{No: len(s.state.Answers) + 1, LinkNo: linkNo, Answer: answer, EvidenceRef: evidence,
		Window: window, By: by, At: at.UTC()})
	s.pending = append(s.pending, Event{Type: eventDateAnswerRecorded, At: at.UTC(), TenantID: s.state.TenantID, SubjectID: string(s.state.ShipmentID),
		Stage: StageMain, ConsolidationID: current.ConsolidationID, LinkNo: linkNo})
	return nil
}

// CurrentLink returns the latest capacity link.
func (s *Shipment) CurrentLink() (CapacityLink, bool) {
	if n := len(s.state.Links); n > 0 {
		return s.state.Links[n-1], true
	}
	return CapacityLink{}, false
}

// carriage returns the latest link when it is of the consolidation the shipment is attached to now on the main
// carriage: once the shipment moved to another consolidation, the carriage of the old one is not its carriage, until it
// is linked to the new one's capacity.
func (s *Shipment) carriage() (CapacityLink, bool) {
	l, ok := s.CurrentLink()
	if a, attached := s.Attachment(StageMain); !ok || !attached || a.ConsolidationID != l.ConsolidationID {
		return CapacityLink{}, false
	}
	return l, true
}

// Links returns the capacity links in the order made; Answers the customer's answers.
func (s *Shipment) Links() []CapacityLink { return slices.Clone(s.state.Links) }
func (s *Shipment) Answers() []DateAnswer { return slices.Clone(s.state.Answers) }

// Answer returns the answer to a link that counts: the link's latest, when it was given while the plan's departure
// window was the current one.
func (s *Shipment) Answer(linkNo int) (DateAnswer, bool) {
	for i := len(s.state.Answers) - 1; i >= 0; i-- {
		if a := s.state.Answers[i]; a.LinkNo == linkNo {
			return a, sameInterval(a.Window, s.Latest().Route.Departure)
		}
	}
	return DateAnswer{}, false
}

// accepted reports whether the customer accepted the current carriage's departure: its own latest answer is ACCEPTED
// and counts, or it was never answered and an earlier link departing at the same instant was accepted. An answer given
// under another window asks again.
func (s *Shipment) accepted(c CapacityLink) bool {
	if a, counts := s.Answer(c.No); a.No != 0 {
		return counts && a.Answer == AnswerAccepted
	}
	for _, l := range s.state.Links[:c.No-1] {
		if a, ok := s.Answer(l.No); ok && a.Answer == AnswerAccepted && l.Departure().Equal(c.Departure()) {
			return true
		}
	}
	return false
}

// AgreedWindow is the window the customer agreed to: the plan's departure window, widened to the current carriage's
// departure when the customer accepted it.
func (s *Shipment) AgreedWindow() kernel.ValidityInterval {
	w := s.Latest().Route.Departure
	c, ok := s.carriage()
	if !ok || w.Contains(c.Departure()) || !s.accepted(c) {
		return w
	}
	if d := c.Departure(); d.Before(w.From) {
		w.From = d
	} else {
		w.Until = d.Add(time.Microsecond)
	}
	return w
}

// AwaitingAnswer reports the current carriage when its departure lies outside the window the customer agreed to: the
// customer's answer is awaited, or the answer that counts was a refusal (refused true).
func (s *Shipment) AwaitingAnswer() (link CapacityLink, refused bool, awaiting bool) {
	c, ok := s.carriage()
	if !ok || s.state.Status == ShipmentCancelled || s.AgreedWindow().Contains(c.Departure()) {
		return CapacityLink{}, false, false
	}
	a, counts := s.Answer(c.No)
	return c, counts && a.Answer == AnswerRefused, true
}

func (s *Shipment) validateLinks() error {
	for i, l := range s.state.Links {
		if l.No != i+1 || l.Stage != StageMain || l.PlanNo < 1 || l.PlanNo > len(s.state.Plans) || !l.Capacity.valid() || blank(l.By) || l.At.IsZero() ||
			(i > 0 && l.At.Before(s.state.Links[i-1].At)) {
			return fmt.Errorf("%w: capacity link %d", ErrInvalidShipment, i+1)
		}
	}
	for i, a := range s.state.Answers {
		ok := a.No == i+1 && a.LinkNo >= 1 && a.LinkNo <= len(s.state.Links) && (a.Answer == AnswerAccepted || a.Answer == AnswerRefused) &&
			text(a.EvidenceRef) && !blank(a.By) && !a.At.Before(s.state.Links[a.LinkNo-1].At) && (i == 0 || !a.At.Before(s.state.Answers[i-1].At))
		if ok && a.LinkNo < len(s.state.Links) { // answered while its link was the current one
			ok = !s.state.Links[a.LinkNo].At.Before(a.At)
		}
		if !ok {
			return fmt.Errorf("%w: date answer %d", ErrInvalidShipment, i+1)
		}
	}
	return nil
}

// AddedLinks and AddedAnswers return what was recorded since the shipment was restored, for persistence.
func (s *Shipment) AddedLinks() []CapacityLink { return slices.Clone(s.state.Links[s.links:]) }
func (s *Shipment) AddedAnswers() []DateAnswer { return slices.Clone(s.state.Answers[s.answers:]) }
