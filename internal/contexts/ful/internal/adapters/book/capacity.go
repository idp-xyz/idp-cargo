// Package book is FUL's anticorruption layer for BOOK's CarrierBooking v1 (stage 4, V2.110): the confirmed capacity a
// shipment's main carriage is linked to. BOOK's error kinds are kept as text only.
package book

import (
	"context"
	"errors"
	"fmt"
	"time"

	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/application"
	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

// Capacity reads a booking's current capacity from BOOK. Call it outside FUL's transactions.
type Capacity struct {
	BOOK bookapi.Queries
}

var _ app.CarrierCapacity = Capacity{}

// Booking kinds BOOK holds, by the mode of the capacity they book.
var modes = map[string]ful.TransportMode{
	bookapi.KindAirConsol:             ful.ModeAir,
	bookapi.KindSeaLCLConsolEquipment: ful.ModeSea,
	bookapi.KindSeaLCLCoLoad:          ful.ModeSea,
}

func (c Capacity) Capacity(ctx context.Context, tenant ful.TenantID, booking string, line int) (ful.Capacity, error) {
	got, err := c.BOOK.GetCarrierBooking(ctx, bookapi.GetCarrierBooking{TenantID: string(tenant), BookingID: booking})
	if errors.Is(err, bookapi.ErrNotFound) {
		return ful.Capacity{}, fmt.Errorf("%w: BOOK has no booking %s: %v", app.ErrCapacityUnavailable, booking, err.Error())
	}
	if err != nil {
		return ful.Capacity{}, err
	}
	switch {
	case got.TenantID != string(tenant) || got.BookingID != booking:
		return ful.Capacity{}, fmt.Errorf("%w: BOOK answered for another booking", app.ErrCapacityUnavailable)
	case got.Status != bookapi.StatusConfirmed && got.Status != bookapi.StatusChangeRequested:
		return ful.Capacity{}, fmt.Errorf("%w: booking %s is %s", app.ErrCapacityUnavailable, booking, got.Status)
	case got.Capacity.Version < 1:
		return ful.Capacity{}, fmt.Errorf("%w: booking %s holds no confirmed capacity", app.ErrCapacityUnavailable, booking)
	case got.ConsumerKind != bookapi.ConsumerConsolidation || got.ConsumerID == "":
		return ful.Capacity{}, fmt.Errorf("%w: booking %s is allocated to no consolidation", app.ErrCapacityUnavailable, booking)
	}
	mode, ok := modes[got.Kind]
	if !ok {
		return ful.Capacity{}, fmt.Errorf("%w: booking %s is of kind %s", app.ErrCapacityUnavailable, booking, got.Kind)
	}
	lines := got.Capacity.Lines
	var chosen *bookapi.CapacityLineView
	switch {
	case line == 0 && len(lines) == 1:
		chosen = &lines[0]
	case line == 0:
		return ful.Capacity{}, fmt.Errorf("%w: booking %s has %d lines; name one", app.ErrCapacityUnavailable, booking, len(lines))
	default:
		for i := range lines {
			if lines[i].Line == line {
				chosen = &lines[i]
			}
		}
	}
	if chosen == nil || len(chosen.Legs) == 0 {
		return ful.Capacity{}, fmt.Errorf("%w: booking %s has no line %d with legs", app.ErrCapacityUnavailable, booking, line)
	}
	out := ful.Capacity{BookingID: got.BookingID, BookingRevision: got.Revision, Mode: mode, ConsolidationID: got.ConsumerID,
		CapacityVersion: got.Capacity.Version, Line: chosen.Line, CarrierReference: chosen.CarrierReference}
	for _, l := range chosen.Legs {
		out.Legs = append(out.Legs, ful.CapacityLeg{Loading: l.Loading.Code, Discharge: l.Discharge.Code, Flight: l.Flight,
			Vessel: l.Vessel, Voyage: l.Voyage, ETD: stored(l.ETD), ETA: stored(l.ETA)})
	}
	return out, nil
}

// stored truncates to the microsecond precision of timestamptz, as FUL keeps times.
func stored(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Microsecond)
}
