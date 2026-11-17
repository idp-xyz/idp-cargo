// Package book is SEA's anticorruption layer for BOOK's CarrierBooking v1 (relationship 6): the booking's carrier or
// co-loader, port route, voyage legs and cut-offs as SEA links them. BOOK's error kinds are kept as text only.
package book

import (
	"context"
	"errors"
	"fmt"

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
	if errors.Is(err, bookapi.ErrNotFound) {
		return sea.Booking{}, fmt.Errorf("%w: %s: %v", app.ErrUnknownBooking, id, err.Error())
	}
	if err != nil {
		return sea.Booking{}, err
	}
	if got.TenantID != string(tenant) || got.BookingID != id {
		return sea.Booking{}, fmt.Errorf("%w: BOOK answered for another booking", app.ErrUnknownBooking)
	}
	inForce := got.Capacity.Version > 0 && (got.Status == bookapi.StatusConfirmed || got.Status == bookapi.StatusChangeRequested ||
		got.Status == bookapi.StatusCancellationRequested)
	out := sea.Booking{BookingID: got.BookingID, Revision: got.Revision, Kind: kinds[got.Kind], InForce: inForce, ConsumerID: got.ConsumerID,
		LegalEntityID: got.LegalEntityID, Edition: got.Request.LocationEdition, CapacityVersion: got.Capacity.Version, Lines: len(got.Capacity.Lines),
		Carrier: sea.Carrier{PartyID: got.CarrierPartyID, PartyRevision: got.CarrierPartyRevision, Role: got.CarrierRole}}
	if legs := got.Request.Legs; len(legs) > 0 {
		out.Origin, out.Destination = legs[0].Loading.Code, legs[len(legs)-1].Discharge.Code
	}
	if len(got.Capacity.Lines) > 0 {
		for _, l := range got.Capacity.Lines[0].Legs {
			leg := sea.Leg{Vessel: l.Vessel, VesselIMO: l.VesselIMO, Voyage: l.Voyage, From: l.Loading.Code, To: l.Discharge.Code, ETD: l.ETD, ETA: l.ETA}
			for _, c := range l.CutOffs {
				leg.CutOffs = append(leg.CutOffs, sea.CutOff{Kind: c.Kind, At: c.At, TimeZone: c.TimeZone, Location: c.Location})
			}
			out.Legs = append(out.Legs, leg)
		}
	}
	return out, nil
}
