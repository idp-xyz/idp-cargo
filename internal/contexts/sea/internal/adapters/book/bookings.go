// Package book is SEA's anticorruption layer for BOOK's CarrierBooking v1 (relationship 6): the booking's carrier or
// co-loader, port route, voyage legs and cut-offs as SEA links them. BOOK's error kinds are kept as text only.
package book

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/application"
	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
)

// Bookings reads CarrierBooking v1. Call it outside SEA's transactions.
type Bookings struct{ BOOK bookapi.Queries }

var _ app.Bookings = Bookings{}

// kinds maps BOOK's sea LCL booking kinds to the kind of the consolidation they carry (N1-PHY OD-6): a container
// booked with a shipping line for the consolidator's own container, space bought from a co-loader for a co-load.
var kinds = map[string]sea.Kind{bookapi.KindSeaLCLConsolEquipment: sea.KindOwn, bookapi.KindSeaLCLCoLoad: sea.KindCoLoad}

func (b Bookings) Booking(ctx context.Context, tenant sea.TenantID, id string) (sea.Booking, error) {
	got, err := b.BOOK.GetCarrierBooking(ctx, bookapi.GetCarrierBooking{TenantID: string(tenant), BookingID: id})
	if err != nil {
		return sea.Booking{}, foreign(err, id)
	}
	if got.TenantID != string(tenant) || got.BookingID != id {
		return sea.Booking{}, fmt.Errorf("%w: BOOK answered for another booking", app.ErrUnknownBooking)
	}
	inForce := got.Capacity.Version > 0 && (got.Status == bookapi.StatusConfirmed || got.Status == bookapi.StatusChangeRequested ||
		got.Status == bookapi.StatusCancellationRequested)
	out := sea.Booking{BookingID: got.BookingID, Revision: got.Revision, Kind: kinds[got.Kind], InForce: inForce, LegalEntityID: got.LegalEntityID,
		Edition: got.Request.LocationEdition, CapacityVersion: got.Capacity.Version, Lines: len(got.Capacity.Lines),
		Carrier: sea.Carrier{PartyID: got.CarrierPartyID, PartyRevision: got.CarrierPartyRevision, Role: got.CarrierRole}}
	if got.ConsumerKind == bookapi.ConsumerConsolidation { // capacity allocated to anything else is allocated to no consolidation
		out.ConsumerID = got.ConsumerID
	}
	if legs := got.Request.Legs; len(legs) > 0 {
		out.Origin, out.Destination = legs[0].Loading.Code, legs[len(legs)-1].Discharge.Code
	}
	// the legs of every capacity line (since V2.105: a movement sails on one line), the first as Legs too
	for i, line := range got.Capacity.Lines {
		var legs []sea.Leg
		for _, l := range line.Legs {
			// BOOK keeps a vessel's name as it was entered; SEA keeps it without the spaces around it. Instants are
			// kept to the microsecond, as they are stored.
			leg := sea.Leg{Vessel: strings.TrimSpace(l.Vessel), VesselIMO: l.VesselIMO, Voyage: l.Voyage, From: l.Loading.Code, To: l.Discharge.Code,
				ETD: micro(l.ETD), ETA: micro(l.ETA)}
			for _, c := range l.CutOffs {
				leg.CutOffs = append(leg.CutOffs, sea.CutOff{Kind: c.Kind, At: micro(c.At), TimeZone: c.TimeZone, Location: c.Location})
			}
			legs = append(legs, leg)
		}
		if i == 0 {
			out.Legs = legs
		}
		out.LineLegs = append(out.LineLegs, legs)
	}
	return out, nil
}

// foreign turns an error of BOOK into SEA's: a booking BOOK does not know, or a question BOOK rejects, is an unknown
// booking; any other of BOOK's kinds is kept as text, so that no caller of SEA matches BOOK's kinds; anything else
// (a cancelled context, a failing database) passes unchanged.
func foreign(err error, id string) error {
	switch {
	case errors.Is(err, bookapi.ErrNotFound), errors.Is(err, bookapi.ErrRejected):
		return fmt.Errorf("%w: %s: %v", app.ErrUnknownBooking, id, err.Error())
	case errors.Is(err, bookapi.ErrUnauthenticated), errors.Is(err, bookapi.ErrUnauthorized), errors.Is(err, bookapi.ErrConflict),
		errors.Is(err, bookapi.ErrIdempotencyConflict):
		return fmt.Errorf("sea: BOOK did not answer for booking %s: %v", id, err.Error())
	}
	return err
}

func micro(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Microsecond)
}
