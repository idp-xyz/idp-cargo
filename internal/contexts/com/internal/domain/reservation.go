package domain

import (
	"fmt"
	"time"
)

// Reservation records that a FUL shipment holds an obligation version (C6). While a shipment holds an
// obligation, COM refuses to revoke it or to cancel its order; FUL releases it first. COM never asks FUL:
// the reservation is COM's own fact, set and cleared through COM's api. Reservations are numbered from 1
// within the order and kept after release.
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
// latest and ACTIVE, and no shipment may hold the obligation already.
func (o *CustomerOrder) Reserve(id ObligationID, version int64, shipment, by string, at time.Time) error {
	ob, ok := o.Obligation(id, version)
	if !ok {
		return fmt.Errorf("%w: %q version %d", ErrUnknownObligation, id, version)
	}
	if latest, _ := o.Latest(id); ob.Status != ObligationActive || latest.Version != version {
		return fmt.Errorf("%w: %q version %d", ErrObligationNotActive, id, version)
	}
	if r, held := o.Held(id); held {
		return fmt.Errorf("%w: shipment %s holds %q", ErrObligationReserved, r.ShipmentID, id)
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
	i := o.held(id)
	if i < 0 || o.state.Reservations[i].ObligationVersion != version || o.state.Reservations[i].ShipmentID != shipment {
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

// Held returns the reservation in force on an obligation, if a shipment holds it.
func (o *CustomerOrder) Held(id ObligationID) (Reservation, bool) {
	if i := o.held(id); i >= 0 {
		return o.state.Reservations[i], true
	}
	return Reservation{}, false
}

func (o *CustomerOrder) held(id ObligationID) int {
	for i, r := range o.state.Reservations {
		if r.ObligationID == id && r.Held() {
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
// order, released at most once and not before it was made, and at most one in force per obligation, on
// its latest version.
func (o *CustomerOrder) validateReservations() error {
	holding := map[ObligationID]bool{}
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
		if latest, _ := o.Latest(r.ObligationID); holding[r.ObligationID] || latest.Version != r.ObligationVersion {
			return fmt.Errorf("%w: reservation %d", ErrInvalidOrder, i+1)
		}
		holding[r.ObligationID] = true
	}
	return nil
}
