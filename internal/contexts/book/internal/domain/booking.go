package domain

import (
	"fmt"
	"slices"
	"time"
)

// Status is a booking's state.
type Status string

const (
	StatusDraft                 Status = "DRAFT"
	StatusRequested             Status = "REQUESTED"
	StatusAcknowledged          Status = "ACKNOWLEDGED"
	StatusConfirmed             Status = "CONFIRMED"
	StatusChangeRequested       Status = "CHANGE_REQUESTED"
	StatusCancellationRequested Status = "CANCELLATION_REQUESTED"
	StatusRejected              Status = "REJECTED"
	StatusDeclinedByCarrier     Status = "DECLINED_BY_CARRIER"
	StatusCancelled             Status = "CANCELLED"
)

// Terminal reports whether nothing follows the status; a terminal booking's references are no longer live.
func (s Status) Terminal() bool {
	return s == StatusRejected || s == StatusDeclinedByCarrier || s == StatusCancelled
}

// Awaiting reports whether the carrier has yet to answer something sent.
func (s Status) Awaiting() bool {
	return s == StatusRequested || s == StatusAcknowledged || s == StatusChangeRequested || s == StatusCancellationRequested
}

// SubmissionKind is what the operator sent the carrier.
type SubmissionKind string

const (
	SubmitRequest      SubmissionKind = "REQUEST"
	SubmitChange       SubmissionKind = "CHANGE_REQUEST"
	SubmitCancellation SubmissionKind = "CANCELLATION_REQUEST"
)

// ResponseKind is what the carrier answered.
type ResponseKind string

const (
	ResponseAcknowledged          ResponseKind = "ACKNOWLEDGED"
	ResponseConfirmed             ResponseKind = "CONFIRMED"
	ResponseRejected              ResponseKind = "REJECTED"
	ResponseCarrierChanged        ResponseKind = "CARRIER_CHANGED"
	ResponseCarrierDeclined       ResponseKind = "CARRIER_DECLINED"
	ResponseChangeConfirmed       ResponseKind = "CHANGE_CONFIRMED"
	ResponseChangeRefused         ResponseKind = "CHANGE_REFUSED"
	ResponseCancellationConfirmed ResponseKind = "CANCELLATION_CONFIRMED"
)

// grants reports whether a response writes a capacity version.
func (k ResponseKind) grants() bool {
	return k == ResponseConfirmed || k == ResponseCarrierChanged || k == ResponseChangeConfirmed
}

// next returns the status a step leads to from a status, or false when it may not follow it.
func next(from Status, step string) (Status, bool) {
	switch step {
	case string(SubmitRequest):
		return StatusRequested, from == StatusDraft
	case string(SubmitChange):
		return StatusChangeRequested, from == StatusConfirmed
	case string(SubmitCancellation):
		return StatusCancellationRequested, from == StatusRequested || from == StatusAcknowledged || from == StatusConfirmed ||
			from == StatusChangeRequested
	case string(ResponseAcknowledged):
		return StatusAcknowledged, from == StatusRequested
	case string(ResponseConfirmed):
		return StatusConfirmed, from == StatusRequested || from == StatusAcknowledged
	case string(ResponseRejected):
		return StatusRejected, from == StatusRequested || from == StatusAcknowledged
	case string(ResponseCarrierChanged):
		return from, from == StatusConfirmed || from == StatusChangeRequested
	case string(ResponseChangeConfirmed), string(ResponseChangeRefused):
		return StatusConfirmed, from == StatusChangeRequested
	case string(ResponseCarrierDeclined):
		return StatusDeclinedByCarrier, from == StatusConfirmed || from == StatusChangeRequested
	case string(ResponseCancellationConfirmed):
		return StatusCancelled, from == StatusCancellationRequested
	}
	return "", false
}

// Submission is a request, change request or cancellation request the operator sent, with its evidence.
type Submission struct {
	Step           int
	Kind           SubmissionKind
	RequestVersion int    // the request version sent; for a cancellation the version in force
	Reason         string // change and cancellation
	Channel        Channel
	Evidence       Evidence
	By             string
	At             time.Time // when it was sent
	EnteredAt      time.Time
}

// Response is a carrier answer as the operator recorded it, with its evidence. Verification is
// OPERATOR_ENTERED_WITH_EVIDENCE in N1.
type Response struct {
	Step            int
	Kind            ResponseKind
	Answers         int // the request version it answers
	References      []Reference
	ReasonCode      string
	ReasonText      string
	Channel         Channel
	Evidence        Evidence
	CarrierTime     time.Time
	By              string
	At              time.Time
	EnteredAt       time.Time
	CapacityVersion int // the version it wrote, if it granted capacity
}

// VerificationOperatorEntered is the verification level of every answer in N1 (OD-2, OD-6).
const VerificationOperatorEntered = "OPERATOR_ENTERED_WITH_EVIDENCE"

// CapacityReason says why a capacity version was written.
type CapacityReason string

const (
	CapacityInitial             CapacityReason = "INITIAL"
	CapacityPartialConfirmation CapacityReason = "PARTIAL_CONFIRMATION"
	CapacityScheduleChange      CapacityReason = "SCHEDULE_CHANGE"
	CapacityRollover            CapacityReason = "ROLLOVER"
	CapacityCarrierReduced      CapacityReason = "CARRIER_REDUCED"
	CapacitySplit               CapacityReason = "SPLIT"
	CapacityRequestedChange     CapacityReason = "REQUESTED_CHANGE"
)

func (r CapacityReason) carrierChange() bool {
	return r == CapacityScheduleChange || r == CapacityRollover || r == CapacityCarrierReduced || r == CapacitySplit
}

// CapacityVersion is an immutable version of what the carrier confirmed; the latest is current, earlier
// ones superseded and kept.
type CapacityVersion struct {
	No              int
	Reason          CapacityReason
	Response        int // the step of the response that wrote it
	LocationEdition string
	Lines           []Line
}

// AllocationStatus is ACTIVE or RELEASED.
type AllocationStatus string

const (
	AllocationActive   AllocationStatus = "ACTIVE"
	AllocationReleased AllocationStatus = "RELEASED"
)

// Allocation is one immutable version of an allocation of the current capacity to a consumer: moving it to
// a new capacity version or releasing it writes the allocation's next version.
type Allocation struct {
	No              int
	Version         int
	CapacityVersion int
	ConsumerKind    string
	ConsumerID      string
	Status          AllocationStatus
	Reason          string
	By              string
	At              time.Time
	EnteredAt       time.Time
}

// Carrier is the carrier as ORG confirmed it at the booking's business time, frozen: the party and its
// revision, the role decision and, for an airline, its IATA designator and AWB prefix.
type Carrier struct {
	PartyID        string
	PartyRevision  int64
	Role           string
	ValidFrom      time.Time
	ValidUntil     time.Time
	RoleEvidence   string
	IATADesignator string
	AWBPrefix      string
}

// BookingState is a stored booking, for persistence. ConsumerID is the consolidation the booking serves; a
// reassignment changes it.
type BookingState struct {
	TenantID       TenantID
	BookingID      BookingID
	Kind           Kind
	LegalEntityID  string
	Carrier        Carrier
	ConsumerKind   string
	ConsumerID     string
	Rebooks        BookingID
	Status         Status
	Requests       []Request
	Submissions    []Submission
	Responses      []Response
	Capacities     []CapacityVersion
	Allocations    []Allocation
	WithdrawReason string
	WithdrawnBy    string
	WithdrawnAt    time.Time
}

// Booking is the aggregate root (CarrierBooking). Requests, submissions, responses, capacity versions and
// allocation versions are only added; the status follows the steps, and every step's business time lies
// at or after the previous one.
type Booking struct {
	state   BookingState
	stored  counts // what was persisted when the booking was loaded
	pending []Event
}

type counts struct{ requests, submissions, responses, capacities, allocations int }

// Event carries identifiers only.
type Event struct {
	Type            string
	At              time.Time
	TenantID        TenantID
	BookingID       BookingID
	RequestVersion  int
	CapacityVersion int
	AllocationNo    int
	ConsumerKind    string
	ConsumerID      string
}

// Event types, as package api publishes them.
const (
	eventDrafted               = "CarrierBookingDrafted.v1"
	eventRequestAmended        = "BookingRequestAmended.v1"
	eventRequested             = "BookingRequested.v1"
	eventAcknowledged          = "CarrierBookingAcknowledged.v1"
	eventConfirmed             = "BookingConfirmed.v1"
	eventRejected              = "CarrierBookingRejected.v1"
	eventCapacityChanged       = "BookingCapacityChanged.v1"
	eventChangeRequested       = "BookingChangeRequested.v1"
	eventChangeRefused         = "BookingChangeRefused.v1"
	eventDeclined              = "BookingDeclinedByCarrier.v1"
	eventCancellationRequested = "BookingCancellationRequested.v1"
	eventCancelled             = "CarrierBookingCancelled.v1"
	eventAllocated             = "BookingCapacityAllocated.v1"
	eventReleased              = "BookingCapacityReleased.v1"
)

// Answer is a carrier answer as an operator enters it.
type Answer struct {
	Channel     Channel
	Evidence    Evidence
	CarrierTime time.Time
	References  []Reference
	ReasonCode  string
	ReasonText  string
}

// Grant is capacity a carrier confirmed, with the location reference edition its legs were resolved with.
type Grant struct {
	Lines   []Line
	Edition string
}

// DraftBooking drafts a booking of a kind with a carrier ORG confirmed in the kind's role, for one
// consolidation. An airline needs its IATA designator and AWB prefix (ORG V0.6).
func DraftBooking(tenant TenantID, id BookingID, kind Kind, legalEntity string, carrier Carrier, consolidation string, rebooks BookingID,
	request Request, by string, at time.Time) (*Booking, error) {
	if !identifier(string(tenant)) || !identifier(string(id)) || !kind.Valid() || !identifier(legalEntity) || !identifier(consolidation) ||
		(rebooks != "" && (!identifier(string(rebooks)) || rebooks == id)) || blank(by) || at.IsZero() {
		return nil, ErrInvalidBooking
	}
	if err := carrier.check(kind); err != nil {
		return nil, err
	}
	if err := request.check(kind, carrier.IATADesignator); err != nil {
		return nil, err
	}
	request.No, request.Reason, request.By, request.At = 1, "", by, at.UTC()
	b := &Booking{state: BookingState{TenantID: tenant, BookingID: id, Kind: kind, LegalEntityID: legalEntity, Carrier: carrier,
		ConsumerKind: ConsumerConsolidation, ConsumerID: consolidation, Rebooks: rebooks, Status: StatusDraft, Requests: []Request{request}}}
	b.record(eventDrafted, at, 1, 0, nil)
	return b, nil
}

func (c Carrier) check(k Kind) error {
	if !identifier(c.PartyID) || c.PartyRevision < 1 || c.Role != k.Role() || c.ValidFrom.IsZero() || !text(c.RoleEvidence, maxText) {
		return fmt.Errorf("%w: carrier in role %s", ErrInvalidBooking, k.Role())
	}
	if k.Mode() == ModeAir {
		if !flightForm.MatchString(c.IATADesignator+"1") || !awbPrefix(c.AWBPrefix) {
			return ErrCarrierCodes
		}
	} else if c.IATADesignator != "" || c.AWBPrefix != "" {
		return fmt.Errorf("%w: airline codes on a SEA booking", ErrInvalidBooking)
	}
	return nil
}

func awbPrefix(s string) bool {
	return len(s) == 3 && s[0] >= '0' && s[0] <= '9' && s[1] >= '0' && s[1] <= '9' && s[2] >= '0' && s[2] <= '9'
}

// AmendRequest writes the next request version of a draft, with a reason.
func (b *Booking) AmendRequest(request Request, reason, by string, at time.Time) error {
	if b.state.Status != StatusDraft {
		return fmt.Errorf("%w: a sent request changes only through a change request", ErrInvalidTransition)
	}
	if !text(reason, maxText) {
		return fmt.Errorf("%w: an amendment needs a reason", ErrInvalidChange)
	}
	if err := b.addRequest(request, reason, by, at); err != nil {
		return err
	}
	b.record(eventRequestAmended, at, len(b.state.Requests), 0, nil)
	return nil
}

func (b *Booking) addRequest(request Request, reason, by string, at time.Time) error {
	if err := request.check(b.state.Kind, b.state.Carrier.IATADesignator); err != nil {
		return err
	}
	if err := b.changeable(by, at); err != nil {
		return err
	}
	request.No, request.Reason, request.By, request.At, request.EnteredAt = len(b.state.Requests)+1, reason, by, at.UTC(), time.Time{}
	b.state.Requests = append(b.state.Requests, request)
	return nil
}

// RecordSent records that the operator sent the current request; it never confirms anything (I-05).
func (b *Booking) RecordSent(channel Channel, evidence Evidence, by string, at time.Time) error {
	if err := b.submit(SubmitRequest, "", channel, evidence, by, at); err != nil {
		return err
	}
	b.record(eventRequested, at, b.Request().No, 0, nil)
	return nil
}

// RequestChange records a changed request sent to a confirmed booking's carrier; the current capacity stays
// in force until the carrier answers.
func (b *Booking) RequestChange(request Request, reason string, channel Channel, evidence Evidence, by string, at time.Time) error {
	if b.state.Status != StatusConfirmed {
		return ErrInvalidTransition
	}
	if !text(reason, maxText) {
		return fmt.Errorf("%w: a change request needs a reason", ErrInvalidChange)
	}
	if !channel.valid() || !evidence.valid(at) {
		return ErrInvalidEvidence
	}
	if err := b.addRequest(request, reason, by, at); err != nil {
		return err
	}
	if err := b.submit(SubmitChange, reason, channel, evidence, by, at); err != nil {
		return err
	}
	b.record(eventChangeRequested, at, b.Request().No, 0, nil)
	return nil
}

// RequestCancellation records a cancellation request sent to the carrier; capacity and allocation stay until
// the carrier confirms, because space may still be charged (G02-06).
func (b *Booking) RequestCancellation(reason string, channel Channel, evidence Evidence, by string, at time.Time) error {
	if !text(reason, maxText) {
		return fmt.Errorf("%w: a cancellation request needs a reason", ErrInvalidChange)
	}
	if err := b.submit(SubmitCancellation, reason, channel, evidence, by, at); err != nil {
		return err
	}
	b.record(eventCancellationRequested, at, b.Request().No, 0, nil)
	return nil
}

func (b *Booking) submit(kind SubmissionKind, reason string, channel Channel, evidence Evidence, by string, at time.Time) error {
	status, ok := next(b.state.Status, string(kind))
	if !ok {
		return fmt.Errorf("%w: %s from %s", ErrInvalidTransition, kind, b.state.Status)
	}
	if !channel.valid() || !evidence.valid(at) {
		return ErrInvalidEvidence
	}
	if err := b.changeable(by, at); err != nil {
		return err
	}
	b.state.Submissions = append(b.state.Submissions, Submission{Step: b.steps() + 1, Kind: kind, RequestVersion: b.Request().No,
		Reason: reason, Channel: channel, Evidence: evidence, By: by, At: at.UTC()})
	b.state.Status = status
	return nil
}

// RecordAcknowledgement records that the carrier received the request; no capacity is granted.
func (b *Booking) RecordAcknowledgement(a Answer, by string, at time.Time) error {
	if _, err := b.respond(ResponseAcknowledged, a, by, at); err != nil {
		return err
	}
	b.record(eventAcknowledged, at, b.Request().No, 0, nil)
	return nil
}

// RecordConfirmation records the carrier's confirmation of the request with its evidence (OD-2): capacity
// version 1, at most what was requested, PARTIAL_CONFIRMATION when less, allocated to the consolidation (OD-3).
// An air consolidation's booking then holds exactly one MAWB number (N1-PHY OD-7).
func (b *Booking) RecordConfirmation(a Answer, g Grant, by string, at time.Time) error {
	reason, err := b.grantReason(g, "")
	if err != nil {
		return err
	}
	if b.state.Kind == KindAirConsol && b.MAWB() == "" && !slices.ContainsFunc(a.References, func(r Reference) bool { return r.Type == ReferenceMAWBNumber }) {
		return ErrMAWB
	}
	r, err := b.respond(ResponseConfirmed, a, by, at)
	if err != nil {
		return err
	}
	b.grant(r, reason, g)
	b.record(eventConfirmed, at, b.Request().No, len(b.state.Capacities), nil)
	b.allocate(b.state.ConsumerID, string(reason), by, at)
	return nil
}

// RecordCarrierChange records a carrier's change of a confirmed booking: a new capacity version, within what
// was requested, that supersedes the current one; the allocation moves to it.
func (b *Booking) RecordCarrierChange(reason CapacityReason, a Answer, g Grant, by string, at time.Time) error {
	if !reason.carrierChange() {
		return fmt.Errorf("%w: a carrier change is a schedule change, rollover, reduction or split", ErrInvalidChange)
	}
	if _, err := b.grantReason(g, reason); err != nil {
		return err
	}
	r, err := b.respond(ResponseCarrierChanged, a, by, at)
	if err != nil {
		return err
	}
	b.grant(r, reason, g)
	b.move(string(reason), by, at)
	return nil
}

// RecordChangeConfirmed records the carrier's confirmation of a requested change: a new capacity version
// within the changed request.
func (b *Booking) RecordChangeConfirmed(a Answer, g Grant, by string, at time.Time) error {
	if _, err := b.grantReason(g, CapacityRequestedChange); err != nil {
		return err
	}
	r, err := b.respond(ResponseChangeConfirmed, a, by, at)
	if err != nil {
		return err
	}
	b.grant(r, CapacityRequestedChange, g)
	b.move(string(CapacityRequestedChange), by, at)
	return nil
}

// RecordChangeRefused records that the carrier refused a requested change; the capacity stays as it was.
func (b *Booking) RecordChangeRefused(a Answer, by string, at time.Time) error {
	if _, err := b.respond(ResponseChangeRefused, a, by, at); err != nil {
		return err
	}
	b.record(eventChangeRefused, at, b.Request().No, 0, nil)
	return nil
}

// RecordRejection records that the carrier refused the request, with its reason; terminal.
func (b *Booking) RecordRejection(a Answer, by string, at time.Time) error {
	if blank(a.ReasonCode) && blank(a.ReasonText) {
		return fmt.Errorf("%w: the carrier's reason", ErrInvalidChange)
	}
	if _, err := b.respond(ResponseRejected, a, by, at); err != nil {
		return err
	}
	b.record(eventRejected, at, b.Request().No, 0, nil)
	return nil
}

// RecordDecline records that the carrier withdrew a confirmed booking; the allocation is released and the
// consumer rebooks. Terminal.
func (b *Booking) RecordDecline(a Answer, by string, at time.Time) error {
	if blank(a.ReasonCode) && blank(a.ReasonText) {
		return fmt.Errorf("%w: the carrier's reason", ErrInvalidChange)
	}
	if _, err := b.respond(ResponseCarrierDeclined, a, by, at); err != nil {
		return err
	}
	b.record(eventDeclined, at, b.Request().No, 0, nil)
	b.release("DECLINED_BY_CARRIER", by, at)
	return nil
}

// RecordCancellationConfirmed records the carrier's confirmation of a cancellation; the allocation is
// released. Terminal.
func (b *Booking) RecordCancellationConfirmed(a Answer, by string, at time.Time) error {
	if _, err := b.respond(ResponseCancellationConfirmed, a, by, at); err != nil {
		return err
	}
	b.record(eventCancelled, at, b.Request().No, 0, nil)
	b.release("CANCELLED", by, at)
	return nil
}

// WithdrawDraft cancels a draft that was never sent, with a reason.
func (b *Booking) WithdrawDraft(reason, by string, at time.Time) error {
	if b.state.Status != StatusDraft {
		return ErrInvalidTransition
	}
	if !text(reason, maxText) {
		return fmt.Errorf("%w: a withdrawal needs a reason", ErrInvalidChange)
	}
	if err := b.changeable(by, at); err != nil {
		return err
	}
	b.state.Status, b.state.WithdrawReason, b.state.WithdrawnBy, b.state.WithdrawnAt = StatusCancelled, reason, by, at.UTC()
	b.record(eventCancelled, at, b.Request().No, 0, nil)
	return nil
}

// Reassign moves a confirmed booking's capacity to another consolidation, with a reason. BOOK does not verify
// the consumer; its owner links itself (D1).
func (b *Booking) Reassign(consolidation, reason, by string, at time.Time) error {
	if b.state.Status != StatusConfirmed && b.state.Status != StatusChangeRequested {
		return ErrInvalidTransition
	}
	if !identifier(consolidation) || consolidation == b.state.ConsumerID || !text(reason, maxText) {
		return fmt.Errorf("%w: another consolidation and a reason", ErrInvalidChange)
	}
	if err := b.changeable(by, at); err != nil {
		return err
	}
	b.release(reason, by, at)
	b.state.ConsumerID = consolidation
	b.allocate(consolidation, reason, by, at)
	return nil
}

// respond appends a carrier answer that may follow the current status.
func (b *Booking) respond(kind ResponseKind, a Answer, by string, at time.Time) (*Response, error) {
	status, ok := next(b.state.Status, string(kind))
	if !ok {
		return nil, fmt.Errorf("%w: %s from %s", ErrInvalidTransition, kind, b.state.Status)
	}
	if !a.Channel.valid() || !a.Evidence.valid(at) {
		return nil, ErrInvalidEvidence
	}
	if (a.ReasonCode != "" && !text(a.ReasonCode, 35)) || (a.ReasonText != "" && !text(a.ReasonText, maxText)) {
		return nil, fmt.Errorf("%w: the carrier's reason", ErrInvalidChange)
	}
	refs, err := normalized(a.References, b.state.Kind, b.state.Carrier.AWBPrefix)
	if err != nil {
		return nil, err
	}
	mawb := b.MAWB()
	for _, r := range refs {
		if r.Type == ReferenceMAWBNumber {
			if mawb != "" && r.Value != mawb {
				return nil, fmt.Errorf("%w: %s states %s; a different MAWB is a new booking", ErrMAWB, kind, r.Value)
			}
			mawb = r.Value
		}
	}
	if err = b.changeable(by, at); err != nil {
		return nil, err
	}
	b.state.Responses = append(b.state.Responses, Response{Step: b.steps() + 1, Kind: kind, Answers: b.Request().No, References: refs,
		ReasonCode: a.ReasonCode, ReasonText: a.ReasonText, Channel: a.Channel, Evidence: a.Evidence, CarrierTime: utc(a.CarrierTime),
		By: by, At: at.UTC()})
	b.state.Status = status
	return &b.state.Responses[len(b.state.Responses)-1], nil
}

// grantReason checks granted lines against the request in force and returns the capacity version's reason:
// for a first confirmation INITIAL or PARTIAL_CONFIRMATION, otherwise the reason given.
func (b *Booking) grantReason(g Grant, reason CapacityReason) (CapacityReason, error) {
	request := b.Request()
	if err := checkLines(g.Lines, b.state.Kind, request, b.state.Carrier.IATADesignator); err != nil {
		return "", err
	}
	if !editionForm.MatchString(g.Edition) {
		return "", fmt.Errorf("%w: location edition", ErrInvalidCapacity)
	}
	sum, err := total(g.Lines)
	if err != nil {
		return "", err
	}
	fits, less := sum.within(request.Capacity)
	if !fits {
		return "", ErrExceedsRequest
	}
	if reason == "" {
		reason = CapacityInitial
		if less {
			reason = CapacityPartialConfirmation
		}
	}
	return reason, nil
}

func (b *Booking) grant(r *Response, reason CapacityReason, g Grant) {
	no := len(b.state.Capacities) + 1
	lines := append([]Line(nil), g.Lines...)
	for i := range lines {
		for j := range lines[i].Legs {
			for k := range lines[i].Legs[j].CutOffs {
				c := &lines[i].Legs[j].CutOffs[k]
				c.At = c.At.UTC()
				if c.Location.IsZero() {
					c.Location = lines[i].Legs[j].Loading.Code
				}
			}
			lines[i].Legs[j].ETD, lines[i].Legs[j].ETA = lines[i].Legs[j].ETD.UTC(), utc(lines[i].Legs[j].ETA)
		}
	}
	b.state.Capacities = append(b.state.Capacities, CapacityVersion{No: no, Reason: reason, Response: r.Step, LocationEdition: g.Edition, Lines: lines})
	r.CapacityVersion = no
	if no > 1 {
		b.record(eventCapacityChanged, r.At, b.Request().No, no, nil)
	}
}

// allocate writes a new ACTIVE allocation of the current capacity version to a consolidation.
func (b *Booking) allocate(consolidation, reason, by string, at time.Time) {
	a := Allocation{No: b.allocations() + 1, Version: 1, CapacityVersion: len(b.state.Capacities), ConsumerKind: ConsumerConsolidation,
		ConsumerID: consolidation, Status: AllocationActive, Reason: reason, By: by, At: at.UTC()}
	b.state.Allocations = append(b.state.Allocations, a)
	b.record(eventAllocated, at, 0, a.CapacityVersion, &a)
}

// move writes the active allocation's next version on the current capacity version.
func (b *Booking) move(reason, by string, at time.Time) {
	if a, ok := b.ActiveAllocation(); ok {
		a.Version, a.CapacityVersion, a.Reason, a.By, a.At, a.EnteredAt = a.Version+1, len(b.state.Capacities), reason, by, at.UTC(), time.Time{}
		b.state.Allocations = append(b.state.Allocations, a)
	}
}

// release writes the active allocation's next version as RELEASED.
func (b *Booking) release(reason, by string, at time.Time) {
	if a, ok := b.ActiveAllocation(); ok {
		a.Version, a.Status, a.Reason, a.By, a.At, a.EnteredAt = a.Version+1, AllocationReleased, reason, by, at.UTC(), time.Time{}
		b.state.Allocations = append(b.state.Allocations, a)
		b.record(eventReleased, at, 0, a.CapacityVersion, &a)
	}
}

// ActiveAllocation returns the current ACTIVE allocation version, if any.
func (b *Booking) ActiveAllocation() (Allocation, bool) {
	latest := map[int]Allocation{}
	for _, a := range b.state.Allocations {
		latest[a.No] = a
	}
	for _, a := range latest {
		if a.Status == AllocationActive {
			return a, true
		}
	}
	return Allocation{}, false
}

func (b *Booking) allocations() int {
	n := 0
	for _, a := range b.state.Allocations {
		n = max(n, a.No)
	}
	return n
}

// MAWB returns the MAWB number the carrier stated for the booking, if any.
func (b *Booking) MAWB() string {
	for _, r := range b.state.Responses {
		for _, ref := range r.References {
			if ref.Type == ReferenceMAWBNumber {
				return ref.Value
			}
		}
	}
	return ""
}

func (b *Booking) mawbs() int {
	seen := map[string]bool{}
	for _, r := range b.state.Responses {
		for _, ref := range r.References {
			if ref.Type == ReferenceMAWBNumber {
				seen[ref.Value] = true
			}
		}
	}
	return len(seen)
}

// References returns the distinct references the carrier stated, with the step that first stated each.
func (b *Booking) References() []StatedReference {
	var out []StatedReference
	seen := map[Reference]bool{}
	for _, r := range b.state.Responses {
		for _, ref := range r.References {
			if !seen[ref] {
				seen[ref] = true
				out = append(out, StatedReference{Reference: ref, Step: r.Step})
			}
		}
	}
	return out
}

// StatedReference is a carrier reference with the step of the response that first stated it.
type StatedReference struct {
	Reference
	Step int
}

func (b *Booking) steps() int { return len(b.state.Submissions) + len(b.state.Responses) }

// changeable checks who changes the booking and that nothing recorded for it is later than at.
func (b *Booking) changeable(by string, at time.Time) error {
	if blank(by) || at.IsZero() || at.Before(b.lastChange()) {
		return fmt.Errorf("%w: author and business time not before the booking's last change", ErrInvalidChange)
	}
	return nil
}

func (b *Booking) lastChange() time.Time {
	last := b.state.WithdrawnAt
	for _, r := range b.state.Requests {
		last = later(last, r.At)
	}
	for _, s := range b.state.Submissions {
		last = later(last, s.At)
	}
	for _, r := range b.state.Responses {
		last = later(last, r.At)
	}
	for _, a := range b.state.Allocations {
		last = later(last, a.At)
	}
	return last
}

func (b *Booking) record(kind string, at time.Time, request, capacity int, a *Allocation) {
	e := Event{Type: kind, At: at.UTC(), TenantID: b.state.TenantID, BookingID: b.state.BookingID, RequestVersion: request, CapacityVersion: capacity}
	if a != nil {
		e.AllocationNo, e.ConsumerKind, e.ConsumerID = a.No, a.ConsumerKind, a.ConsumerID
	}
	b.pending = append(b.pending, e)
}

// Stamp records when BOOK entered what was added since the booking was loaded (T1).
func (b *Booking) Stamp(entered time.Time) {
	entered = entered.UTC()
	for i := b.stored.requests; i < len(b.state.Requests); i++ {
		b.state.Requests[i].EnteredAt = entered
	}
	for i := b.stored.submissions; i < len(b.state.Submissions); i++ {
		b.state.Submissions[i].EnteredAt = entered
	}
	for i := b.stored.responses; i < len(b.state.Responses); i++ {
		b.state.Responses[i].EnteredAt = entered
	}
	for i := b.stored.allocations; i < len(b.state.Allocations); i++ {
		b.state.Allocations[i].EnteredAt = entered
	}
}

// Request returns the current request version.
func (b *Booking) Request() Request { return b.state.Requests[len(b.state.Requests)-1] }

// Capacity returns the current capacity version, if the carrier granted any.
func (b *Booking) Capacity() (CapacityVersion, bool) {
	if len(b.state.Capacities) == 0 {
		return CapacityVersion{}, false
	}
	return b.state.Capacities[len(b.state.Capacities)-1], true
}

// PendingSince returns when the request, change or cancellation the carrier has yet to answer was sent.
func (b *Booking) PendingSince() time.Time {
	if !b.state.Status.Awaiting() || len(b.state.Submissions) == 0 {
		return time.Time{}
	}
	return b.state.Submissions[len(b.state.Submissions)-1].At
}

// ConfirmedAt returns the business and recording times of the response that wrote the current capacity.
func (b *Booking) ConfirmedAt() (time.Time, time.Time) {
	c, ok := b.Capacity()
	if !ok {
		return time.Time{}, time.Time{}
	}
	for _, r := range b.state.Responses {
		if r.Step == c.Response {
			return r.At, r.EnteredAt
		}
	}
	return time.Time{}, time.Time{}
}

// RestoreBooking rebuilds a stored booking without emitting events. It replays the steps to check that the
// status follows from them and that capacity and allocations agree with it.
func RestoreBooking(st BookingState) (*Booking, error) {
	st.Requests = append([]Request(nil), st.Requests...)
	st.Submissions = append([]Submission(nil), st.Submissions...)
	st.Responses = append([]Response(nil), st.Responses...)
	st.Capacities = append([]CapacityVersion(nil), st.Capacities...)
	st.Allocations = append([]Allocation(nil), st.Allocations...)
	b := &Booking{state: st, stored: counts{len(st.Requests), len(st.Submissions), len(st.Responses), len(st.Capacities), len(st.Allocations)}}
	if err := b.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidBooking, err)
	}
	return b, nil
}

func (b *Booking) validate() error {
	st := b.state
	if !identifier(string(st.TenantID)) || !identifier(string(st.BookingID)) || !st.Kind.Valid() || !identifier(st.LegalEntityID) ||
		st.ConsumerKind != ConsumerConsolidation || !identifier(st.ConsumerID) || len(st.Requests) == 0 {
		return fmt.Errorf("identity")
	}
	if err := st.Carrier.check(st.Kind); err != nil {
		return err
	}
	for i, r := range st.Requests {
		if r.No != i+1 || blank(r.By) || r.At.IsZero() || (i > 0) != (r.Reason != "") || r.check(st.Kind, st.Carrier.IATADesignator) != nil {
			return fmt.Errorf("request version %d", i+1)
		}
	}
	steps := make([]string, b.steps()+1)
	for _, s := range st.Submissions {
		if s.Step < 1 || s.Step >= len(steps) || steps[s.Step] != "" || s.RequestVersion < 1 || s.RequestVersion > len(st.Requests) ||
			!s.Channel.valid() || !s.Evidence.valid(s.At) {
			return fmt.Errorf("submission at step %d", s.Step)
		}
		steps[s.Step] = string(s.Kind)
	}
	granted := 0
	for _, r := range st.Responses {
		if r.Step < 1 || r.Step >= len(steps) || steps[r.Step] != "" || r.Answers < 1 || r.Answers > len(st.Requests) ||
			!r.Channel.valid() || !r.Evidence.valid(r.At) || r.Kind.grants() != (r.CapacityVersion > 0) {
			return fmt.Errorf("response at step %d", r.Step)
		}
		if r.CapacityVersion > 0 {
			granted++
			if r.CapacityVersion != granted || granted > len(st.Capacities) || st.Capacities[granted-1].Response != r.Step {
				return fmt.Errorf("capacity version of step %d", r.Step)
			}
		}
		steps[r.Step] = string(r.Kind)
	}
	if granted != len(st.Capacities) {
		return fmt.Errorf("capacity versions without a granting answer")
	}
	status := StatusDraft
	for i, step := range steps[1:] {
		var ok bool
		if status, ok = next(status, step); !ok {
			return fmt.Errorf("step %d (%s)", i+1, step)
		}
	}
	withdrawn := !st.WithdrawnAt.IsZero()
	if withdrawn {
		if b.steps() > 0 || status != StatusDraft || !text(st.WithdrawReason, maxText) || blank(st.WithdrawnBy) {
			return fmt.Errorf("withdrawal")
		}
		status = StatusCancelled
	}
	if st.Status != status {
		return fmt.Errorf("status %s, but the steps make it %s", st.Status, status)
	}
	if b.mawbs() > 1 || (st.Kind == KindAirConsol && len(st.Capacities) > 0 && b.MAWB() == "") {
		return ErrMAWB
	}
	versions := map[int]int{}
	for i, a := range st.Allocations {
		if a.Version != versions[a.No]+1 || (a.Version == 1 && a.No != b.allocationsBefore(i)+1) || a.CapacityVersion < 1 ||
			a.CapacityVersion > len(st.Capacities) || a.ConsumerKind != ConsumerConsolidation || !identifier(a.ConsumerID) || blank(a.By) {
			return fmt.Errorf("allocation %d version %d", a.No, a.Version)
		}
		versions[a.No] = a.Version
	}
	active, ok := b.ActiveAllocation()
	if want := len(st.Capacities) > 0 && !status.Terminal(); ok != want ||
		(ok && (active.CapacityVersion != len(st.Capacities) || active.ConsumerID != st.ConsumerID)) {
		return fmt.Errorf("the active allocation does not match the status %s and current capacity", status)
	}
	return nil
}

func (b *Booking) allocationsBefore(i int) int {
	n := 0
	for _, a := range b.state.Allocations[:i] {
		n = max(n, a.No)
	}
	return n
}

// State returns a copy of the booking for persistence and read models.
func (b *Booking) State() BookingState {
	st := b.state
	st.Requests = append([]Request(nil), st.Requests...)
	st.Submissions = append([]Submission(nil), st.Submissions...)
	st.Responses = append([]Response(nil), st.Responses...)
	st.Capacities = append([]CapacityVersion(nil), st.Capacities...)
	st.Allocations = append([]Allocation(nil), st.Allocations...)
	return st
}

func (b *Booking) ID() BookingID    { return b.state.BookingID }
func (b *Booking) Status() Status   { return b.state.Status }
func (b *Booking) Pending() []Event { return append([]Event(nil), b.pending...) }
func (b *Booking) Carrier() Carrier { return b.state.Carrier }
func (b *Booking) Kind() Kind       { return b.state.Kind }
func (b *Booking) Consumer() string { return b.state.ConsumerID }
func (b *Booking) Tenant() TenantID { return b.state.TenantID }

// Added returns what was written since the booking was created or restored, for persistence.
func (b *Booking) Added() (requests []Request, submissions []Submission, responses []Response, capacities []CapacityVersion, allocations []Allocation) {
	st := b.state
	return st.Requests[b.stored.requests:], st.Submissions[b.stored.submissions:], st.Responses[b.stored.responses:],
		st.Capacities[b.stored.capacities:], st.Allocations[b.stored.allocations:]
}

// Acknowledge is permitted only after the transaction holding the booking and its Outbox events commits.
func (b *Booking) Acknowledge() {
	b.pending = nil
	b.stored = counts{len(b.state.Requests), len(b.state.Submissions), len(b.state.Responses), len(b.state.Capacities), len(b.state.Allocations)}
}

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}
