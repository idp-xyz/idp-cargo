package domain

import (
	"fmt"
	"time"
)

// ObligationStatus is the status of one version of an obligation.
type ObligationStatus string

const (
	ObligationActive  ObligationStatus = "ACTIVE"
	ObligationRevoked ObligationStatus = "REVOKED"
)

// Obligation is one version of a fulfillment obligation: packs of one line, with their declared gross
// weight and volume, that COM commits to fulfil. Versions never change. Allocation writes version 1,
// ACTIVE; revocation writes the next version, REVOKED, with the same line, measure and allocation (C6).
type Obligation struct {
	ObligationID ObligationID
	Version      int64
	LineNo       int
	Measure      Measure
	Status       ObligationStatus
	AllocatedBy  string
	AllocatedAt  time.Time
	RevokedBy    string
	RevokedAt    time.Time // zero unless the version is REVOKED
}

// Allocate commits packs of a line, with their declared gross weight and volume, to a new obligation at
// version 1. The line's active obligations never exceed its packs, gross weight or volume (C3). A line is
// fully allocated when all its packs are.
func (o *CustomerOrder) Allocate(lineNo int, id ObligationID, m Measure, by string, at time.Time) error {
	if err := o.open(); err != nil {
		return err
	}
	if o.state.Status == OrderAllocated {
		return ErrInvalidTransition
	}
	if err := o.allocate(lineNo, id, m, by, at); err != nil {
		return err
	}
	o.state.Status = o.derivedStatus()
	return nil
}

func (o *CustomerOrder) allocate(lineNo int, id ObligationID, m Measure, by string, at time.Time) error {
	if at.IsZero() || at.Before(o.state.RecordedAt) || !identifier(string(id)) || blank(by) || !m.valid() {
		return ErrInvalidAllocation
	}
	if o.holds(id) {
		return fmt.Errorf("%w: obligation %q already exists", ErrInvalidAllocation, id)
	}
	remaining, err := o.Remaining(lineNo)
	if err != nil {
		return err
	}
	left, err := remaining.sub(m)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAllocation, err)
	}
	if left.negative() {
		return ErrOverAllocation
	}
	ob := Obligation{ObligationID: id, Version: 1, LineNo: lineNo, Measure: m, Status: ObligationActive, AllocatedBy: by, AllocatedAt: at.UTC()}
	o.state.Obligations = append(o.state.Obligations, ob)
	o.record(eventObligationAllocated, at, &ob, "")
	return nil
}

// Revoke revokes an active obligation that no shipment holds. It writes the next version, REVOKED, and
// the obligation's packs, weight and volume become allocatable again (C6).
func (o *CustomerOrder) Revoke(id ObligationID, by string, at time.Time) error {
	if err := o.open(); err != nil {
		return err
	}
	ob, err := o.revocable(id, by, at)
	if err != nil {
		return err
	}
	o.revoke(ob, by, at)
	o.state.Status = o.derivedStatus()
	return nil
}

// Amend replaces an active obligation that no shipment holds by a new obligation on the same line: the
// old one is revoked and the new one allocated, and both are recorded (C6). If the new allocation is
// refused, nothing changes.
func (o *CustomerOrder) Amend(id, newID ObligationID, m Measure, by string, at time.Time) error {
	if err := o.open(); err != nil {
		return err
	}
	ob, err := o.revocable(id, by, at)
	if err != nil {
		return err
	}
	obligations, events := len(o.state.Obligations), len(o.pending)
	o.revoke(ob, by, at)
	if err = o.allocate(ob.LineNo, newID, m, by, at); err != nil {
		o.state.Obligations, o.pending = o.state.Obligations[:obligations], o.pending[:events]
		return err
	}
	o.state.Status = o.derivedStatus()
	return nil
}

// Cancel revokes every active obligation and cancels the order for good. While a shipment holds one of
// its obligations the order cannot be cancelled (C6), nor once it is closed. The order's contacts are then
// kept for months from at, restricted, before they may be erased (P1).
func (o *CustomerOrder) Cancel(by string, at time.Time, months int) error {
	if err := o.open(); err != nil {
		return err
	}
	if blank(by) || at.IsZero() || at.Before(o.state.RecordedAt) {
		return ErrInvalidChange
	}
	if !ValidRetentionMonths(months) {
		return fmt.Errorf("%w: %d months", ErrInvalidRetention, months)
	}
	var active []Obligation
	for _, ob := range o.current() {
		if ob.Status != ObligationActive {
			continue
		}
		if _, err := o.revocable(ob.ObligationID, by, at); err != nil {
			return err
		}
		active = append(active, ob)
	}
	for _, ob := range active {
		o.revoke(ob, by, at)
	}
	o.state.Status, o.state.CancelledBy, o.state.CancelledAt = OrderCancelled, by, at.UTC()
	o.startRetention(RetentionCancelled, at, months)
	o.record(eventOrderCancelled, at, nil, "")
	return nil
}

// revocable returns the latest version of an obligation if it can be revoked: it is ACTIVE, no shipment
// holds it, and at is not before anything already recorded for it.
func (o *CustomerOrder) revocable(id ObligationID, by string, at time.Time) (Obligation, error) {
	ob, ok := o.Latest(id)
	switch {
	case !ok:
		return Obligation{}, fmt.Errorf("%w: %q", ErrUnknownObligation, id)
	case ob.Status != ObligationActive:
		return Obligation{}, fmt.Errorf("%w: %q is revoked", ErrObligationNotActive, id)
	case blank(by) || at.IsZero() || at.Before(o.lastChange(id)):
		return Obligation{}, ErrInvalidChange
	}
	if r, held := o.Held(id); held {
		return Obligation{}, fmt.Errorf("%w: shipment %s holds %q", ErrObligationReserved, r.ShipmentID, id)
	}
	return ob, nil
}

func (o *CustomerOrder) revoke(ob Obligation, by string, at time.Time) {
	ob.Version, ob.Status, ob.RevokedBy, ob.RevokedAt = ob.Version+1, ObligationRevoked, by, at.UTC()
	o.state.Obligations = append(o.state.Obligations, ob)
	o.record(eventObligationRevoked, at, &ob, "")
}

// Remaining returns what of a line its active obligations do not commit.
func (o *CustomerOrder) Remaining(lineNo int) (Measure, error) {
	if lineNo < 1 || lineNo > len(o.lines.lines) {
		return Measure{}, fmt.Errorf("%w: no line %d", ErrInvalidAllocation, lineNo)
	}
	remaining := o.lines.lines[lineNo-1].Measure
	for _, ob := range o.current() {
		if ob.LineNo != lineNo || ob.Status != ObligationActive {
			continue
		}
		var err error
		if remaining, err = remaining.sub(ob.Measure); err != nil {
			return Measure{}, err
		}
	}
	return remaining, nil
}

// Obligation returns one version of an obligation of the order.
func (o *CustomerOrder) Obligation(id ObligationID, version int64) (Obligation, bool) {
	for _, ob := range o.state.Obligations {
		if ob.ObligationID == id && ob.Version == version {
			return ob, true
		}
	}
	return Obligation{}, false
}

// Latest returns the latest version of an obligation of the order.
func (o *CustomerOrder) Latest(id ObligationID) (Obligation, bool) {
	var latest Obligation
	found := false
	for _, ob := range o.state.Obligations {
		if ob.ObligationID == id && (!found || ob.Version > latest.Version) {
			latest, found = ob, true
		}
	}
	return latest, found
}

// current returns the latest version of every obligation, in the order the obligations were allocated.
func (o *CustomerOrder) current() []Obligation {
	index := map[ObligationID]int{}
	var latest []Obligation
	for _, ob := range o.state.Obligations {
		i, ok := index[ob.ObligationID]
		switch {
		case !ok:
			index[ob.ObligationID] = len(latest)
			latest = append(latest, ob)
		case ob.Version > latest[i].Version:
			latest[i] = ob
		}
	}
	return latest
}

func (o *CustomerOrder) holds(id ObligationID) bool {
	_, ok := o.Latest(id)
	return ok
}

// lastChange returns the latest instant recorded for an obligation: its allocation, its revocation, and
// the reservations made and released on it.
func (o *CustomerOrder) lastChange(id ObligationID) time.Time {
	var last time.Time
	for _, ob := range o.state.Obligations {
		if ob.ObligationID == id {
			last = later(later(last, ob.AllocatedAt), ob.RevokedAt)
		}
	}
	for _, r := range o.state.Reservations {
		if r.ObligationID == id {
			last = later(later(last, r.ReservedAt), r.ReleasedAt)
		}
	}
	return last
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (o *CustomerOrder) derivedStatus() OrderStatus {
	if !o.state.CancelledAt.IsZero() {
		return OrderCancelled
	}
	if !o.state.ClosedAt.IsZero() {
		return OrderClosed
	}
	active := false
	for _, ob := range o.current() {
		active = active || ob.Status == ObligationActive
	}
	if !active {
		return OrderRecorded
	}
	for no := 1; no <= len(o.lines.lines); no++ {
		if remaining, err := o.Remaining(no); err != nil || remaining.Packs != 0 {
			return OrderPartiallyAllocated
		}
	}
	return OrderAllocated
}

// validateObligations checks restored obligation versions: version 1 allocates and is ACTIVE, a later
// version revokes an ACTIVE one with the same line, measure and allocation, a cancelled order has no
// active obligation, and every line's active obligations fit the line.
func (o *CustomerOrder) validateObligations() error {
	s := o.state
	versions := map[ObligationID]map[int64]Obligation{}
	for _, ob := range s.Obligations {
		if versions[ob.ObligationID] == nil {
			versions[ob.ObligationID] = map[int64]Obligation{}
		}
		_, duplicate := versions[ob.ObligationID][ob.Version]
		if duplicate || !identifier(string(ob.ObligationID)) || ob.LineNo < 1 || ob.LineNo > len(o.lines.lines) || !ob.Measure.valid() ||
			blank(ob.AllocatedBy) || ob.AllocatedAt.Before(s.RecordedAt) {
			return fmt.Errorf("%w: obligation %q version %d", ErrInvalidOrder, ob.ObligationID, ob.Version)
		}
		versions[ob.ObligationID][ob.Version] = ob
	}
	for id, byVersion := range versions {
		for v, ob := range byVersion {
			prev, revokes := byVersion[v-1]
			switch {
			case v == 1 && ob.Status == ObligationActive && ob.RevokedBy == "" && ob.RevokedAt.IsZero():
			case v > 1 && revokes && prev.Status == ObligationActive && ob.Status == ObligationRevoked && !blank(ob.RevokedBy) &&
				!ob.RevokedAt.Before(ob.AllocatedAt) && ob.LineNo == prev.LineNo && ob.Measure == prev.Measure &&
				ob.AllocatedBy == prev.AllocatedBy && ob.AllocatedAt.Equal(prev.AllocatedAt):
			default:
				return fmt.Errorf("%w: obligation %q version %d", ErrInvalidOrder, id, v)
			}
		}
	}
	for _, ob := range o.current() {
		if ob.Status == ObligationActive && !s.CancelledAt.IsZero() {
			return fmt.Errorf("%w: cancelled, but obligation %q is active", ErrInvalidOrder, ob.ObligationID)
		}
	}
	for no := 1; no <= len(o.lines.lines); no++ {
		if remaining, err := o.Remaining(no); err != nil || remaining.negative() {
			return fmt.Errorf("%w: line %d is over-allocated", ErrInvalidOrder, no)
		}
	}
	return nil
}
