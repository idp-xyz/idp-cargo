// Package domain is the model of FUL (Freight Fulfillment) in S1: the shipment plan. A shipment plans
// parts of exact COM obligation versions of one customer and legal entity, by one transport mode, between
// two locations, in a departure window. Since F1 an obligation may be split into parts planned in several
// shipments, as long as the parts never exceed it; a carrier's shortload or a rollover is replanned here,
// without a COM amendment. Plan versions are immutable, and a plan never asserts loading or delivery;
// those facts come from PHY, HND and evidence later. It depends on the standard library and the kernel only.
package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

type TenantID string
type ShipmentID string
type ObligationID string

type ShipmentStatus string

const (
	ShipmentDraft     ShipmentStatus = "DRAFT"
	ShipmentPlanned   ShipmentStatus = "PLANNED"
	ShipmentCancelled ShipmentStatus = "CANCELLED"
)

var (
	ErrInvalidShipment   = errors.New("ful: invalid shipment")
	ErrInvalidObligation = errors.New("ful: invalid obligation reference")
	ErrInvalidPart       = errors.New("ful: the part does not fit the obligation")
	ErrOtherCustomer     = errors.New("ful: the obligation belongs to another customer or legal entity")
	ErrAlreadyPlanned    = errors.New("ful: the shipment already plans the obligation")
	ErrNotPlanned        = errors.New("ful: the shipment does not plan the obligation version")
	ErrLastObligation    = errors.New("ful: the last obligation of a shipment leaves only by cancelling it")
	ErrInvalidPlan       = errors.New("ful: invalid plan amendment")
	ErrIncompletePlan    = errors.New("ful: the plan needs a transport mode, origin, destination and departure window")
	ErrInvalidTransition = errors.New("ful: invalid state transition")
	ErrInvalidChange     = errors.New("ful: invalid change to the shipment")
	ErrConflict          = errors.New("ful: concurrent revision conflict")
	ErrUnresolvedRoute   = errors.New("ful: the route's locations are not resolved against the location reference data")
	ErrModeMismatch      = errors.New("ful: the order requests another transport mode")
)

// Event types, as package api publishes them.
const (
	eventCreated            = "ShipmentCreated.v1"
	eventObligationAdded    = "ShipmentObligationAdded.v1"
	eventPlanAmended        = "ShipmentPlanAmended.v1"
	eventPlanConfirmed      = "ShipmentPlanConfirmed.v1"
	eventObligationReleased = "ShipmentObligationReleased.v1"
	eventPartChanged        = "ShipmentPartChanged.v1"
	eventCancelled          = "ShipmentCancelled.v1"
	// FUL V0.4
	eventConsolAttachmentRecorded = "ShipmentConsolAttachmentRecorded.v1"
	eventConsolDetachmentRecorded = "ShipmentConsolDetachmentRecorded.v1"
	eventLoadEvidenceRecorded     = "ShipmentLoadEvidenceRecorded.v1"
	eventExecutionStatusChanged   = "ShipmentExecutionStatusChanged.v1"
)

// Event carries identifiers only; the Outbox adapter maps it to a Bento envelope.
type Event struct {
	Type              string
	At                time.Time
	TenantID          TenantID
	SubjectID         string
	ObligationID      ObligationID
	ObligationVersion int64
	Stage             string // events about a consol attachment or load evidence (V0.4)
	ConsolidationID   string
	EvidenceNo        int // events about load evidence (V0.4)
}

// ObligationRef is a COM obligation version as FUL plans it, taken from ObligationSnapshot v2: the order
// and the customer it belongs to, and the packs, gross weight and volume COM committed. FUL holds the
// reference, never a copy of the commitment to change (DDD-001).
type ObligationRef struct {
	ObligationID      ObligationID
	ObligationVersion int64
	OrderID           string
	CustomerPartyID   string
	LegalEntityID     string
	Packs             int64
	PackageType       string
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity // zero when COM has none
	// RequestedMode is the transport mode the order requests (C11), as COM reported it when FUL asked; empty
	// when the order requests none. FUL does not store it: every check asks COM again.
	RequestedMode TransportMode
}

func (o ObligationRef) valid() bool {
	return (o.RequestedMode == "" || o.RequestedMode.Valid()) &&
		identifier(string(o.ObligationID)) && o.ObligationVersion > 0 && identifier(o.OrderID) && identifier(o.CustomerPartyID) &&
		identifier(o.LegalEntityID) && o.Packs > 0 && len(o.PackageType) == 2 && o.GrossWeight.Unit.Valid() && o.GrossWeight.Amount > 0 &&
		(o.Volume == kernel.Quantity{} || o.Volume.Unit.Valid() && o.Volume.Amount > 0)
}

// Whole is the scope of all an obligation version commits.
func (o ObligationRef) Whole() CargoScope {
	return CargoScope{Packs: o.Packs, GrossWeight: o.GrossWeight, Volume: o.Volume}
}

// Remaining is what other shipments leave of the obligation when they plan elsewhere of it.
func (o ObligationRef) Remaining(elsewhere CargoScope) (CargoScope, error) {
	return o.Whole().add(elsewhere, -1)
}

// fits checks that part can be planned while other shipments plan elsewhere of the obligation (F1). A part
// has packs and weight, and volume exactly when the obligation declares one, in the obligation's units;
// with elsewhere it never exceeds the obligation. The part that plans the last packs takes all the weight
// and volume left, and a part that leaves packs leaves weight (and volume) for them, so that what COM
// committed is planned exactly once and the rest can always be planned.
func (o ObligationRef) fits(part, elsewhere CargoScope) error {
	whole := o.Whole()
	if part.Packs <= 0 || part.GrossWeight.Unit != whole.GrossWeight.Unit || part.GrossWeight.Amount <= 0 ||
		part.Volume.Unit != whole.Volume.Unit || (whole.Volume.Unit != "") != (part.Volume.Amount > 0) {
		return fmt.Errorf("%w: a part needs packs and weight, and volume exactly when the obligation declares one, in its units", ErrInvalidPart)
	}
	planned, err := elsewhere.add(part, 1)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPart, err)
	}
	left, err := whole.add(planned, -1)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPart, err)
	}
	switch {
	case left.Packs < 0 || left.GrossWeight.Amount < 0 || left.Volume.Amount < 0:
		return fmt.Errorf("%w: %s plans more than its obligation leaves", ErrInvalidPart, o.ObligationID)
	case left.Packs == 0 && (left.GrossWeight.Amount != 0 || left.Volume.Amount != 0):
		return fmt.Errorf("%w: the part with the last packs of %s takes all its weight and volume left", ErrInvalidPart, o.ObligationID)
	case left.Packs > 0 && (left.GrossWeight.Amount == 0 || (whole.Volume.Unit != "" && left.Volume.Amount == 0)):
		return fmt.Errorf("%w: the part leaves packs of %s without weight or volume", ErrInvalidPart, o.ObligationID)
	}
	return nil
}

// within checks a stored part against its obligation alone, as RestoreShipment can.
func (o ObligationRef) within(part CargoScope) bool {
	whole := o.Whole()
	return part.Packs > 0 && part.Packs <= whole.Packs && part.GrossWeight.Unit == whole.GrossWeight.Unit &&
		part.GrossWeight.Amount > 0 && part.GrossWeight.Amount <= whole.GrossWeight.Amount && part.Volume.Unit == whole.Volume.Unit &&
		(whole.Volume.Unit != "") == (part.Volume.Amount > 0) && part.Volume.Amount <= whole.Volume.Amount
}

// PlannedObligation is the part of an obligation version the shipment plans from AddedAt until it is
// released (ShipmentPart, F1): Part says how much of what COM committed. Changing a part releases the entry
// and adds a new one, so released entries stay as history.
type PlannedObligation struct {
	ObligationRef
	Part       CargoScope
	AddedBy    string
	AddedAt    time.Time
	ReleasedBy string
	ReleasedAt time.Time // zero while planned
}

// Planned reports whether the shipment still plans the obligation.
func (p PlannedObligation) Planned() bool { return p.ReleasedAt.IsZero() }

// ShipmentState is a stored shipment, for persistence.
type ShipmentState struct {
	TenantID        TenantID
	ShipmentID      ShipmentID
	CustomerPartyID string
	LegalEntityID   string
	Status          ShipmentStatus
	Obligations     []PlannedObligation
	Plans           []PlanVersion
	Confirmations   []Confirmation
	CancelledBy     string
	CancelledAt     time.Time
	CancelReason    string
	Attachments     []ConsolAttachment // FUL V0.4, in the order recorded
	Evidence        []EvidenceVersion  // FUL V0.4, PHY's verified evidence in the order recorded
}

// Shipment is the aggregate root. Plan versions and confirmations are only added; a planned obligation
// is released at most once.
type Shipment struct {
	state         ShipmentState
	obligations   int   // obligations already persisted
	plans         int   // plan versions already persisted
	confirmations int   // confirmations already persisted
	released      []int // persisted obligations released since, by index
	attachments   int   // consol attachments already persisted
	// persisted consol attachments ended since, by index
	endedAttachments []int
	evidence         int // evidence versions already persisted
	pending          []Event
}

const (
	maxText = 500 // the longest reason kept
	maxID   = 128 // the longest identifier cargo_ful stores
)

// CreateShipment starts a shipment that plans all of one obligation version, which no other shipment
// plans any part of.
func CreateShipment(tenant TenantID, id ShipmentID, ob ObligationRef, by string, at time.Time) (*Shipment, error) {
	return CreateShipmentForPart(tenant, id, ob, ob.Whole(), CargoScope{}, by, at)
}

// CreateShipmentForPart starts a shipment from a part of one obligation version, for that obligation's
// customer and legal entity, while other shipments plan elsewhere of it. The shipment has an identity of
// its own, never the order's (AIR-01).
func CreateShipmentForPart(tenant TenantID, id ShipmentID, ob ObligationRef, part, elsewhere CargoScope, by string, at time.Time) (*Shipment, error) {
	if !identifier(string(tenant)) || !identifier(string(id)) || blank(by) || at.IsZero() {
		return nil, ErrInvalidShipment
	}
	if !ob.valid() {
		return nil, ErrInvalidObligation
	}
	if err := ob.fits(part, elsewhere); err != nil {
		return nil, err
	}
	s := &Shipment{state: ShipmentState{
		TenantID: tenant, ShipmentID: id, CustomerPartyID: ob.CustomerPartyID, LegalEntityID: ob.LegalEntityID, Status: ShipmentDraft,
		Obligations: []PlannedObligation{{ObligationRef: ob, Part: part, AddedBy: by, AddedAt: at.UTC()}},
		Plans:       []PlanVersion{{No: 1, Change: PlanCreated, Scope: part, By: by, At: at.UTC()}},
	}}
	s.record(eventCreated, at, &ob)
	return s, nil
}

// AddObligation plans all of one more obligation version, which no other shipment plans any part of.
func (s *Shipment) AddObligation(ob ObligationRef, by string, at time.Time) error {
	return s.AddPart(ob, ob.Whole(), CargoScope{}, by, at)
}

// AddPart plans a part of one more obligation version of the shipment's customer and legal entity, while
// the plan is a draft and other shipments plan elsewhere of it. The cargo scope grows by the part.
func (s *Shipment) AddPart(ob ObligationRef, part, elsewhere CargoScope, by string, at time.Time) error {
	if s.state.Status != ShipmentDraft {
		return ErrInvalidTransition
	}
	if !ob.valid() {
		return ErrInvalidObligation
	}
	if ob.CustomerPartyID != s.state.CustomerPartyID || ob.LegalEntityID != s.state.LegalEntityID {
		return ErrOtherCustomer
	}
	if s.planning(ob.ObligationID) >= 0 {
		return fmt.Errorf("%w: %s", ErrAlreadyPlanned, ob.ObligationID)
	}
	if err := s.CheckRequestedModes([]ObligationRef{ob}); err != nil {
		return err
	}
	if err := ob.fits(part, elsewhere); err != nil {
		return err
	}
	if err := s.changeable(by, at); err != nil {
		return err
	}
	scope, err := s.Latest().Scope.add(part, 1)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidObligation, err)
	}
	s.state.Obligations = append(s.state.Obligations, PlannedObligation{ObligationRef: ob, Part: part, AddedBy: by, AddedAt: at.UTC()})
	s.addPlan(PlanObligationAdded, s.Latest().Route, scope, "", by, at)
	s.record(eventObligationAdded, at, &ob)
	return nil
}

// ChangePart replans the part of an obligation version the shipment plans, with the reason, while other
// shipments plan elsewhere of it (F1). A carrier's shortload is replanned by reducing the part here and
// planning the rest in another shipment; COM's commitment is unchanged. The entry is released and a new one
// added, and a confirmed plan must be confirmed again.
func (s *Shipment) ChangePart(id ObligationID, version int64, part, elsewhere CargoScope, reason, by string, at time.Time) error {
	if s.state.Status == ShipmentCancelled {
		return ErrInvalidTransition
	}
	if i := s.planning(id); i >= 0 {
		if err := s.stillPlannable(s.Latest().Scope.Packs - s.state.Obligations[i].Part.Packs + part.Packs); err != nil {
			return err
		}
	}
	i := s.planning(id)
	if i < 0 || s.state.Obligations[i].ObligationVersion != version {
		return fmt.Errorf("%w: %s version %d", ErrNotPlanned, id, version)
	}
	current := s.state.Obligations[i]
	if !text(reason) {
		return ErrInvalidPlan
	}
	if part == current.Part {
		return fmt.Errorf("%w: the part of %s is unchanged", ErrInvalidPart, id)
	}
	if err := current.fits(part, elsewhere); err != nil {
		return err
	}
	if err := s.changeable(by, at); err != nil {
		return err
	}
	scope, err := s.Latest().Scope.add(current.Part, -1)
	if err == nil {
		scope, err = scope.add(part, 1)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPart, err)
	}
	s.release(i, by, at)
	s.state.Obligations = append(s.state.Obligations, PlannedObligation{ObligationRef: current.ObligationRef, Part: part, AddedBy: by, AddedAt: at.UTC()})
	s.addPlan(PlanPartChanged, s.Latest().Route, scope, reason, by, at)
	s.record(eventPartChanged, at, &current.ObligationRef)
	return nil
}

// AmendPlan writes a new plan version with a changed route and the reason for the change. A confirmed
// plan that changes must be confirmed again; once packs departed the route stays.
func (s *Shipment) AmendPlan(route Route, reason, by string, at time.Time) error {
	if s.state.Status == ShipmentCancelled {
		return ErrInvalidTransition
	}
	if s.Departed() > 0 {
		return fmt.Errorf("%w: the route stays", ErrDeparted)
	}
	if !route.valid() || !text(reason) || route.Same(s.Latest().Route) {
		return ErrInvalidPlan
	}
	if !route.Resolved() {
		return ErrUnresolvedRoute
	}
	if err := s.changeable(by, at); err != nil {
		return err
	}
	s.addPlan(PlanAmended, route, s.Latest().Scope, reason, by, at)
	s.record(eventPlanAmended, at, nil)
	return nil
}

// Confirm confirms the latest plan version, which must name the mode, both locations and the departure
// window. Whether COM still reports every planned obligation ACTIVE, at its planned version and reserved
// for this shipment is checked by the caller with COM before this.
func (s *Shipment) Confirm(by string, at time.Time) error {
	if s.state.Status != ShipmentDraft {
		return ErrInvalidTransition
	}
	if !s.Latest().Route.Complete() {
		return ErrIncompletePlan
	}
	if !s.Latest().Route.Resolved() {
		return ErrUnresolvedRoute
	}
	if err := s.changeable(by, at); err != nil {
		return err
	}
	s.state.Confirmations = append(s.state.Confirmations, Confirmation{PlanNo: s.Latest().No, By: by, At: at.UTC()})
	s.state.Status = s.derivedStatus()
	s.record(eventPlanConfirmed, at, nil)
	return nil
}

// ReleaseObligation stops planning the part of one obligation version. The cargo scope shrinks and the plan
// needs confirming again. The last obligation leaves only by cancelling the shipment.
func (s *Shipment) ReleaseObligation(id ObligationID, version int64, by string, at time.Time) error {
	if s.state.Status == ShipmentCancelled {
		return ErrInvalidTransition
	}
	i := s.planning(id)
	if i < 0 || s.state.Obligations[i].ObligationVersion != version {
		return fmt.Errorf("%w: %s version %d", ErrNotPlanned, id, version)
	}
	if len(s.Planned()) == 1 {
		return ErrLastObligation
	}
	if err := s.stillPlannable(s.Latest().Scope.Packs - s.state.Obligations[i].Part.Packs); err != nil {
		return err
	}
	if err := s.changeable(by, at); err != nil {
		return err
	}
	ob := s.state.Obligations[i].ObligationRef
	scope, err := s.Latest().Scope.add(s.state.Obligations[i].Part, -1)
	if err != nil {
		return err
	}
	s.release(i, by, at)
	s.addPlan(PlanObligationReleased, s.Latest().Route, scope, "", by, at)
	s.record(eventObligationReleased, at, &ob)
	return nil
}

// Cancel cancels the shipment for good and releases every obligation it plans. The plan history stays. A
// shipment with departed packs is not cancelled.
func (s *Shipment) Cancel(reason, by string, at time.Time) error {
	if s.state.Status == ShipmentCancelled {
		return ErrInvalidTransition
	}
	if s.Departed() > 0 {
		return ErrDeparted
	}
	if !text(reason) {
		return ErrInvalidChange
	}
	if err := s.changeable(by, at); err != nil {
		return err
	}
	for i := range s.state.Obligations {
		if s.state.Obligations[i].Planned() {
			s.release(i, by, at)
		}
	}
	// The shipment's own allocation to a consolidation ends with it; PHY then ends the membership after asking
	// FUL (EndMembershipForCancelledShipment).
	for i := range s.state.Attachments {
		if s.state.Attachments[i].Current() {
			s.endAttachment(i, AttachmentShipmentCancelled, by, at)
		}
	}
	s.state.CancelledBy, s.state.CancelledAt, s.state.CancelReason = by, at.UTC(), reason
	s.state.Status = ShipmentCancelled
	s.record(eventCancelled, at, nil)
	return nil
}

// CheckRequestedModes checks obligation references, fresh from COM, against the shipment's transport mode:
// where both the route and an order name a mode, they must be the same (C11). A shipment of another mode needs
// the order's transport changed first, in COM.
func (s *Shipment) CheckRequestedModes(obligations []ObligationRef) error {
	mode := s.Latest().Route.Mode
	for _, ob := range obligations {
		if mode != "" && ob.RequestedMode != "" && ob.RequestedMode != mode {
			return fmt.Errorf("%w: order %s of %s requests %s, the shipment plans %s", ErrModeMismatch, ob.OrderID, ob.ObligationID, ob.RequestedMode, mode)
		}
	}
	return nil
}

// Stamp records when FUL entered the plan versions and confirmations added since the shipment was loaded
// (T1). Business times stay as given; the caller checks them against the record window.
func (s *Shipment) Stamp(entered time.Time) {
	for i := s.plans; i < len(s.state.Plans); i++ {
		s.state.Plans[i].EnteredAt = entered.UTC()
	}
	for i := s.confirmations; i < len(s.state.Confirmations); i++ {
		s.state.Confirmations[i].EnteredAt = entered.UTC()
	}
	for i := range s.state.Attachments {
		a := &s.state.Attachments[i]
		if a.EnteredAt.IsZero() {
			a.EnteredAt = entered.UTC()
		}
		if !a.Current() && a.EndEnteredAt.IsZero() {
			a.EndEnteredAt = entered.UTC()
		}
	}
	for i := s.evidence; i < len(s.state.Evidence); i++ {
		s.state.Evidence[i].EnteredAt = entered.UTC()
	}
}

// changeable checks who changes the shipment and that nothing recorded for it is later than at.
func (s *Shipment) changeable(by string, at time.Time) error {
	if blank(by) || at.IsZero() || at.Before(s.lastChange()) {
		return ErrInvalidChange
	}
	return nil
}

func (s *Shipment) addPlan(change PlanChange, route Route, scope CargoScope, reason, by string, at time.Time) {
	s.state.Plans = append(s.state.Plans, PlanVersion{No: len(s.state.Plans) + 1, Change: change, Route: route, Scope: scope,
		Reason: reason, By: by, At: at.UTC()})
	s.state.Status = s.derivedStatus()
}

func (s *Shipment) release(i int, by string, at time.Time) {
	s.state.Obligations[i].ReleasedBy, s.state.Obligations[i].ReleasedAt = by, at.UTC()
	if i < s.obligations {
		s.released = append(s.released, i)
	}
}

// planning returns the index of the entry that plans an obligation, or -1.
func (s *Shipment) planning(id ObligationID) int {
	for i, p := range s.state.Obligations {
		if p.ObligationID == id && p.Planned() {
			return i
		}
	}
	return -1
}

func (s *Shipment) lastChange() time.Time {
	var last time.Time
	for _, p := range s.state.Plans {
		last = later(last, p.At)
	}
	for _, c := range s.state.Confirmations {
		last = later(last, c.At)
	}
	for _, p := range s.state.Obligations {
		last = later(later(last, p.AddedAt), p.ReleasedAt)
	}
	for _, a := range s.state.Attachments {
		last = later(later(last, a.RecordedAt), a.EndedAt)
	}
	for _, v := range s.state.Evidence {
		last = later(last, v.At)
	}
	return later(last, s.state.CancelledAt)
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (s *Shipment) derivedStatus() ShipmentStatus {
	switch {
	case !s.state.CancelledAt.IsZero():
		return ShipmentCancelled
	case len(s.state.Confirmations) > 0 && s.state.Confirmations[len(s.state.Confirmations)-1].PlanNo == len(s.state.Plans):
		return ShipmentPlanned
	}
	return ShipmentDraft
}

// Latest returns the current plan version.
func (s *Shipment) Latest() PlanVersion { return s.state.Plans[len(s.state.Plans)-1] }

// Planned returns the obligation versions the shipment plans a part of now.
func (s *Shipment) Planned() []ObligationRef {
	var planned []ObligationRef
	for _, p := range s.Parts() {
		planned = append(planned, p.ObligationRef)
	}
	return planned
}

// Parts returns the entries the shipment plans now, with their parts.
func (s *Shipment) Parts() []PlannedObligation {
	var parts []PlannedObligation
	for _, p := range s.state.Obligations {
		if p.Planned() {
			parts = append(parts, p)
		}
	}
	return parts
}

// RestoreShipment rebuilds a stored shipment without emitting events.
func RestoreShipment(st ShipmentState) (*Shipment, error) {
	st.Obligations = append([]PlannedObligation(nil), st.Obligations...)
	st.Plans = append([]PlanVersion(nil), st.Plans...)
	st.Confirmations = append([]Confirmation(nil), st.Confirmations...)
	st.Attachments = append([]ConsolAttachment(nil), st.Attachments...)
	st.Evidence = append([]EvidenceVersion(nil), st.Evidence...)
	s := &Shipment{state: st, obligations: len(st.Obligations), plans: len(st.Plans), confirmations: len(st.Confirmations),
		attachments: len(st.Attachments), evidence: len(st.Evidence)}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Shipment) validate() error {
	st := s.state
	cancelled := !st.CancelledAt.IsZero()
	switch {
	case !identifier(string(st.TenantID)), !identifier(string(st.ShipmentID)), !identifier(st.CustomerPartyID), !identifier(st.LegalEntityID):
		return ErrInvalidShipment
	case cancelled == blank(st.CancelledBy), cancelled != text(st.CancelReason):
		return fmt.Errorf("%w: cancellation", ErrInvalidShipment)
	case len(st.Plans) == 0, len(st.Obligations) == 0:
		return fmt.Errorf("%w: no plan or no obligation", ErrInvalidShipment)
	}
	planned := map[ObligationID]bool{}
	for i, p := range st.Obligations {
		released := !p.Planned()
		if !p.valid() || !p.within(p.Part) || p.CustomerPartyID != st.CustomerPartyID || p.LegalEntityID != st.LegalEntityID || blank(p.AddedBy) ||
			p.AddedAt.IsZero() || released == blank(p.ReleasedBy) || (released && p.ReleasedAt.Before(p.AddedAt)) ||
			(!released && (planned[p.ObligationID] || cancelled)) {
			return fmt.Errorf("%w: obligation entry %d", ErrInvalidShipment, i+1)
		}
		planned[p.ObligationID] = planned[p.ObligationID] || !released
	}
	if !cancelled {
		var parts []CargoScope
		for _, p := range s.Parts() {
			parts = append(parts, p.Part)
		}
		scope, err := scopeOf(parts)
		if err != nil || len(parts) == 0 || scope != st.Plans[len(st.Plans)-1].Scope {
			return fmt.Errorf("%w: the latest plan's cargo scope is not the sum of its parts", ErrInvalidShipment)
		}
	}
	for i, p := range st.Plans {
		if p.No != i+1 || !p.Change.valid() || (p.Change == PlanCreated) != (i == 0) || !p.Route.valid() || blank(p.By) || p.At.IsZero() ||
			p.Change.needsReason() != text(p.Reason) || (i > 0 && p.At.Before(st.Plans[i-1].At)) {
			return fmt.Errorf("%w: plan version %d", ErrInvalidShipment, i+1)
		}
	}
	for i, c := range st.Confirmations {
		if c.PlanNo < 1 || c.PlanNo > len(st.Plans) || (i > 0 && c.PlanNo <= st.Confirmations[i-1].PlanNo) ||
			!st.Plans[c.PlanNo-1].Route.Complete() || blank(c.By) || c.At.Before(st.Plans[c.PlanNo-1].At) {
			return fmt.Errorf("%w: confirmation %d", ErrInvalidShipment, i+1)
		}
	}
	if derived := s.derivedStatus(); st.Status != derived {
		return fmt.Errorf("%w: status %s, but the plan makes it %s", ErrInvalidShipment, st.Status, derived)
	}
	if err := s.validateAttachments(); err != nil {
		return err
	}
	return s.validateEvidence()
}

// State returns a copy of the shipment for persistence and read models.
func (s *Shipment) State() ShipmentState {
	st := s.state
	st.Obligations = append([]PlannedObligation(nil), st.Obligations...)
	st.Plans = append([]PlanVersion(nil), st.Plans...)
	st.Confirmations = append([]Confirmation(nil), st.Confirmations...)
	st.Attachments = append([]ConsolAttachment(nil), st.Attachments...)
	st.Evidence = append([]EvidenceVersion(nil), st.Evidence...)
	return st
}

func (s *Shipment) ID() ShipmentID         { return s.state.ShipmentID }
func (s *Shipment) Status() ShipmentStatus { return s.state.Status }
func (s *Shipment) Pending() []Event       { return append([]Event(nil), s.pending...) }

// AddedObligations, AddedPlans and AddedConfirmations return what was written since the shipment was
// created or restored; ReleasedObligations returns the stored entries released since. All for persistence.
func (s *Shipment) AddedObligations() []PlannedObligation {
	return append([]PlannedObligation(nil), s.state.Obligations[s.obligations:]...)
}

func (s *Shipment) AddedPlans() []PlanVersion {
	return append([]PlanVersion(nil), s.state.Plans[s.plans:]...)
}

func (s *Shipment) AddedConfirmations() []Confirmation {
	return append([]Confirmation(nil), s.state.Confirmations[s.confirmations:]...)
}

// ReleasedObligations returns the stored entries released since the shipment was restored, with their
// positions from 1.
func (s *Shipment) ReleasedObligations() map[int]PlannedObligation {
	released := map[int]PlannedObligation{}
	for _, i := range s.released {
		released[i+1] = s.state.Obligations[i]
	}
	return released
}

// Acknowledge is permitted only after the transaction holding the shipment and its Outbox events commits.
func (s *Shipment) Acknowledge() {
	s.pending, s.released, s.endedAttachments = nil, nil, nil
	s.obligations, s.plans, s.confirmations = len(s.state.Obligations), len(s.state.Plans), len(s.state.Confirmations)
	s.attachments = len(s.state.Attachments)
	s.evidence = len(s.state.Evidence)
}

func (s *Shipment) record(kind string, at time.Time, ob *ObligationRef) {
	e := Event{Type: kind, At: at.UTC(), TenantID: s.state.TenantID, SubjectID: string(s.state.ShipmentID)}
	if ob != nil {
		e.ObligationID, e.ObligationVersion = ob.ObligationID, ob.ObligationVersion
	}
	s.pending = append(s.pending, e)
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func text(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= maxText }

func identifier(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= maxID }
