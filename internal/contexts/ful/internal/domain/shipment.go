// Package domain is the model of FUL (Freight Fulfillment) in S1: the shipment plan. A shipment plans
// exact COM obligation versions of one customer and legal entity, by one transport mode, between two
// locations, in a departure window. Its plan versions are immutable, and it never asserts loading or
// delivery; those facts come from PHY, HND and evidence later. It depends on the standard library and
// the kernel only.
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
	ErrOtherCustomer     = errors.New("ful: the obligation belongs to another customer or legal entity")
	ErrAlreadyPlanned    = errors.New("ful: the shipment already plans the obligation")
	ErrNotPlanned        = errors.New("ful: the shipment does not plan the obligation version")
	ErrLastObligation    = errors.New("ful: the last obligation of a shipment leaves only by cancelling it")
	ErrInvalidPlan       = errors.New("ful: invalid plan amendment")
	ErrIncompletePlan    = errors.New("ful: the plan needs a transport mode, origin, destination and departure window")
	ErrInvalidTransition = errors.New("ful: invalid state transition")
	ErrInvalidChange     = errors.New("ful: invalid change to the shipment")
	ErrConflict          = errors.New("ful: concurrent revision conflict")
)

// Event types, as package api publishes them.
const (
	eventCreated            = "ShipmentCreated.v1"
	eventObligationAdded    = "ShipmentObligationAdded.v1"
	eventPlanAmended        = "ShipmentPlanAmended.v1"
	eventPlanConfirmed      = "ShipmentPlanConfirmed.v1"
	eventObligationReleased = "ShipmentObligationReleased.v1"
	eventCancelled          = "ShipmentCancelled.v1"
)

// Event carries identifiers only; the Outbox adapter maps it to a Bento envelope.
type Event struct {
	Type              string
	At                time.Time
	TenantID          TenantID
	SubjectID         string
	ObligationID      ObligationID
	ObligationVersion int64
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
}

func (o ObligationRef) valid() bool {
	return identifier(string(o.ObligationID)) && o.ObligationVersion > 0 && identifier(o.OrderID) && identifier(o.CustomerPartyID) &&
		identifier(o.LegalEntityID) && o.Packs > 0 && len(o.PackageType) == 2 && o.GrossWeight.Unit.Valid() && o.GrossWeight.Amount > 0 &&
		(o.Volume == kernel.Quantity{} || o.Volume.Unit.Valid() && o.Volume.Amount > 0)
}

// PlannedObligation is an obligation version the shipment plans from AddedAt until it is released.
// Released entries stay as history.
type PlannedObligation struct {
	ObligationRef
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
}

// Shipment is the aggregate root. Plan versions and confirmations are only added; a planned obligation
// is released at most once.
type Shipment struct {
	state         ShipmentState
	obligations   int   // obligations already persisted
	plans         int   // plan versions already persisted
	confirmations int   // confirmations already persisted
	released      []int // persisted obligations released since, by index
	pending       []Event
}

const (
	maxText = 500 // the longest reason kept
	maxID   = 128 // the longest identifier cargo_ful stores
)

// CreateShipment starts a shipment from one obligation version, for that obligation's customer and legal
// entity. The shipment has an identity of its own, never the order's (AIR-01).
func CreateShipment(tenant TenantID, id ShipmentID, ob ObligationRef, by string, at time.Time) (*Shipment, error) {
	if !identifier(string(tenant)) || !identifier(string(id)) || blank(by) || at.IsZero() {
		return nil, ErrInvalidShipment
	}
	if !ob.valid() {
		return nil, ErrInvalidObligation
	}
	scope, err := scopeOf([]ObligationRef{ob})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidObligation, err)
	}
	s := &Shipment{state: ShipmentState{
		TenantID: tenant, ShipmentID: id, CustomerPartyID: ob.CustomerPartyID, LegalEntityID: ob.LegalEntityID, Status: ShipmentDraft,
		Obligations: []PlannedObligation{{ObligationRef: ob, AddedBy: by, AddedAt: at.UTC()}},
		Plans:       []PlanVersion{{No: 1, Change: PlanCreated, Scope: scope, By: by, At: at.UTC()}},
	}}
	s.record(eventCreated, at, &ob)
	return s, nil
}

// AddObligation plans one more obligation version of the shipment's customer and legal entity, while the
// plan is a draft. The cargo scope grows by its packs, weight and volume.
func (s *Shipment) AddObligation(ob ObligationRef, by string, at time.Time) error {
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
	if err := s.changeable(by, at); err != nil {
		return err
	}
	scope, err := s.Latest().Scope.add(ob, 1)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidObligation, err)
	}
	s.state.Obligations = append(s.state.Obligations, PlannedObligation{ObligationRef: ob, AddedBy: by, AddedAt: at.UTC()})
	s.addPlan(PlanObligationAdded, s.Latest().Route, scope, "", by, at)
	s.record(eventObligationAdded, at, &ob)
	return nil
}

// AmendPlan writes a new plan version with a changed route and the reason for the change. A confirmed
// plan that changes must be confirmed again.
func (s *Shipment) AmendPlan(route Route, reason, by string, at time.Time) error {
	if s.state.Status == ShipmentCancelled {
		return ErrInvalidTransition
	}
	if !route.valid() || !text(reason) || route.Same(s.Latest().Route) {
		return ErrInvalidPlan
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
	if err := s.changeable(by, at); err != nil {
		return err
	}
	s.state.Confirmations = append(s.state.Confirmations, Confirmation{PlanNo: s.Latest().No, By: by, At: at.UTC()})
	s.state.Status = s.derivedStatus()
	s.record(eventPlanConfirmed, at, nil)
	return nil
}

// ReleaseObligation stops planning one obligation version. The cargo scope shrinks and the plan needs
// confirming again. The last obligation leaves only by cancelling the shipment.
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
	if err := s.changeable(by, at); err != nil {
		return err
	}
	ob := s.state.Obligations[i].ObligationRef
	scope, err := s.Latest().Scope.add(ob, -1)
	if err != nil {
		return err
	}
	s.release(i, by, at)
	s.addPlan(PlanObligationReleased, s.Latest().Route, scope, "", by, at)
	s.record(eventObligationReleased, at, &ob)
	return nil
}

// Cancel cancels the shipment for good and releases every obligation it plans. The plan history stays.
func (s *Shipment) Cancel(reason, by string, at time.Time) error {
	if s.state.Status == ShipmentCancelled {
		return ErrInvalidTransition
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
	s.state.CancelledBy, s.state.CancelledAt, s.state.CancelReason = by, at.UTC(), reason
	s.state.Status = ShipmentCancelled
	s.record(eventCancelled, at, nil)
	return nil
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

// Planned returns the obligation versions the shipment plans now.
func (s *Shipment) Planned() []ObligationRef {
	var planned []ObligationRef
	for _, p := range s.state.Obligations {
		if p.Planned() {
			planned = append(planned, p.ObligationRef)
		}
	}
	return planned
}

// RestoreShipment rebuilds a stored shipment without emitting events.
func RestoreShipment(st ShipmentState) (*Shipment, error) {
	st.Obligations = append([]PlannedObligation(nil), st.Obligations...)
	st.Plans = append([]PlanVersion(nil), st.Plans...)
	st.Confirmations = append([]Confirmation(nil), st.Confirmations...)
	s := &Shipment{state: st, obligations: len(st.Obligations), plans: len(st.Plans), confirmations: len(st.Confirmations)}
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
		if !p.valid() || p.CustomerPartyID != st.CustomerPartyID || p.LegalEntityID != st.LegalEntityID || blank(p.AddedBy) ||
			p.AddedAt.IsZero() || released == blank(p.ReleasedBy) || (released && p.ReleasedAt.Before(p.AddedAt)) ||
			(!released && (planned[p.ObligationID] || cancelled)) {
			return fmt.Errorf("%w: obligation entry %d", ErrInvalidShipment, i+1)
		}
		planned[p.ObligationID] = planned[p.ObligationID] || !released
	}
	if !cancelled {
		scope, err := scopeOf(s.Planned())
		if err != nil || len(s.Planned()) == 0 || scope != st.Plans[len(st.Plans)-1].Scope {
			return fmt.Errorf("%w: the latest plan's cargo scope is not the sum of its obligations", ErrInvalidShipment)
		}
	}
	for i, p := range st.Plans {
		if p.No != i+1 || !p.Change.valid() || (p.Change == PlanCreated) != (i == 0) || !p.Route.valid() || blank(p.By) || p.At.IsZero() ||
			(p.Change == PlanAmended) != text(p.Reason) || (i > 0 && p.At.Before(st.Plans[i-1].At)) {
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
	return nil
}

// State returns a copy of the shipment for persistence and read models.
func (s *Shipment) State() ShipmentState {
	st := s.state
	st.Obligations = append([]PlannedObligation(nil), st.Obligations...)
	st.Plans = append([]PlanVersion(nil), st.Plans...)
	st.Confirmations = append([]Confirmation(nil), st.Confirmations...)
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
	s.pending, s.released = nil, nil
	s.obligations, s.plans, s.confirmations = len(s.state.Obligations), len(s.state.Plans), len(s.state.Confirmations)
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
