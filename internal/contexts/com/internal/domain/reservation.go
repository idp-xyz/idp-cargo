package domain

import (
	"fmt"
	"time"
)

// Reservation records that a FUL shipment holds an obligation version (C6). Since F1 several shipments
// may each hold one, because FUL can plan parts of one obligation in several shipments; COM does not know
// the parts, FUL keeps them within the obligation. While any shipment holds an obligation, COM refuses to
// revoke it or to cancel its order; FUL releases it first. COM never asks FUL: the reservation is COM's own
// fact, set and cleared through COM's api. Reservations are numbered from 1 within the order and kept
// after release.
type Reservation struct {
	No                int
	ObligationID      ObligationID
	ObligationVersion int64
	ShipmentID        string
	ReservedBy        string
	ReservedAt        time.Time
	ReleasedBy        string
	ReleasedAt        time.Time // zero while the shipment holds the obligation
}

// Held reports whether the shipment still holds the obligation.
func (r Reservation) Held() bool { return r.ReleasedAt.IsZero() }

// Reserve records that a shipment holds an obligation version. The version must be the obligation's
// latest and ACTIVE, and the shipment must not hold the obligation already; other shipments may (F1).
func (o *CustomerOrder) Reserve(id ObligationID, version int64, shipment, by string, at time.Time) error {
	if o.state.Status == OrderClosed {
		return fmt.Errorf("%w: the order is closed", ErrInvalidTransition)
	}
	ob, ok := o.Obligation(id, version)
	if !ok {
		return fmt.Errorf("%w: %q version %d", ErrUnknownObligation, id, version)
	}
	if latest, _ := o.Latest(id); ob.Status != ObligationActive || latest.Version != version {
		return fmt.Errorf("%w: %q version %d", ErrObligationNotActive, id, version)
	}
	if o.heldBy(id, shipment) >= 0 {
		return fmt.Errorf("%w: shipment %s holds %q already", ErrObligationReserved, shipment, id)
	}
	if !identifier(shipment) || blank(by) || at.IsZero() || at.Before(o.lastChange(id)) {
		return ErrInvalidChange
	}
	o.state.Reservations = append(o.state.Reservations, Reservation{No: len(o.state.Reservations) + 1, ObligationID: id,
		ObligationVersion: version, ShipmentID: shipment, ReservedBy: by, ReservedAt: at.UTC()})
	o.record(eventObligationReserved, at, &ob, shipment)
	return nil
}

// ReleaseReservation clears the reservation a shipment holds on an obligation version, once FUL has
// released the obligation or cancelled the shipment (C6).
func (o *CustomerOrder) ReleaseReservation(id ObligationID, version int64, shipment, by string, at time.Time) error {
	i := o.heldBy(id, shipment)
	if i < 0 || o.state.Reservations[i].ObligationVersion != version {
		return fmt.Errorf("%w: %q version %d, shipment %s", ErrNotReserved, id, version, shipment)
	}
	r := &o.state.Reservations[i]
	if blank(by) || at.IsZero() || at.Before(r.ReservedAt) {
		return ErrInvalidChange
	}
	r.ReleasedBy, r.ReleasedAt = by, at.UTC()
	if i < o.reserved {
		o.released = append(o.released, i)
	}
	ob, _ := o.Obligation(id, version)
	o.record(eventReservationReleased, at, &ob, shipment)
	return nil
}

// Held returns the earliest reservation in force on an obligation, if any shipment holds it.
func (o *CustomerOrder) Held(id ObligationID) (Reservation, bool) {
	if holders := o.Holders(id); len(holders) > 0 {
		return holders[0], true
	}
	return Reservation{}, false
}

// Holders returns every reservation in force on an obligation, in the order they were made.
func (o *CustomerOrder) Holders(id ObligationID) []Reservation {
	var holders []Reservation
	for _, r := range o.state.Reservations {
		if r.ObligationID == id && r.Held() {
			holders = append(holders, r)
		}
	}
	return holders
}

// heldBy returns the index of the reservation a shipment holds on an obligation, or -1.
func (o *CustomerOrder) heldBy(id ObligationID, shipment string) int {
	for i, r := range o.state.Reservations {
		if r.ObligationID == id && r.ShipmentID == shipment && r.Held() {
			return i
		}
	}
	return -1
}

// AddedReservations returns the reservations made since the order was recorded or restored, for
// persistence.
func (o *CustomerOrder) AddedReservations() []Reservation {
	return append([]Reservation(nil), o.state.Reservations[o.reserved:]...)
}

// ReleasedReservations returns the stored reservations released since the order was restored, for
// persistence.
func (o *CustomerOrder) ReleasedReservations() []Reservation {
	released := make([]Reservation, len(o.released))
	for k, i := range o.released {
		released[k] = o.state.Reservations[i]
	}
	return released
}

// validateReservations checks restored reservations: numbered from 1, each on an ACTIVE version of the
// order, released at most once and not before it was made, and at most one in force per obligation and
// shipment, on the obligation's latest version.
func (o *CustomerOrder) validateReservations() error {
	type holder struct {
		obligation ObligationID
		shipment   string
	}
	holding := map[holder]bool{}
	for i, r := range o.state.Reservations {
		ob, ok := o.Obligation(r.ObligationID, r.ObligationVersion)
		released := !r.Held()
		if r.No != i+1 || !ok || ob.Status != ObligationActive || !identifier(r.ShipmentID) || blank(r.ReservedBy) ||
			r.ReservedAt.Before(ob.AllocatedAt) || released == blank(r.ReleasedBy) || (released && r.ReleasedAt.Before(r.ReservedAt)) {
			return fmt.Errorf("%w: reservation %d", ErrInvalidOrder, i+1)
		}
		if released {
			continue
		}
		h := holder{r.ObligationID, r.ShipmentID}
		if latest, _ := o.Latest(r.ObligationID); holding[h] || latest.Version != r.ObligationVersion {
			return fmt.Errorf("%w: reservation %d", ErrInvalidOrder, i+1)
		}
		holding[h] = true
	}
	return nil
}
