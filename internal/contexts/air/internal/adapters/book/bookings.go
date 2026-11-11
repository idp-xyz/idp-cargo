// Package book is AIR's anticorruption layer for BOOK's CarrierBooking v1 (relationship 5): the booking's MAWB
// number, airline and flights as AIR links them. BOOK's error kinds are kept as text only.
package book

import (
	"context"
	"errors"
	"fmt"

	app "go.idp.xyz/idp-cargo/internal/contexts/air/internal/application"
	air "go.idp.xyz/idp-cargo/internal/contexts/air/internal/domain"
	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
)

// Bookings reads CarrierBooking v1. Call it outside AIR's transactions.
type Bookings struct{ BOOK bookapi.Queries }

var _ app.Bookings = Bookings{}

func (b Bookings) Booking(ctx context.Context, tenant air.TenantID, id string) (air.Booking, error) {
	got, err := b.BOOK.GetCarrierBooking(ctx, bookapi.GetCarrierBooking{TenantID: string(tenant), BookingID: id})
	if errors.Is(err, bookapi.ErrNotFound) {
		return air.Booking{}, fmt.Errorf("%w: %s: %v", app.ErrUnknownBooking, id, err.Error())
	}
	if err != nil {
		return air.Booking{}, err
	}
	if got.TenantID != string(tenant) || got.BookingID != id {
		return air.Booking{}, fmt.Errorf("%w: BOOK answered for another booking", app.ErrUnknownBooking)
	}
	inForce := got.Capacity.Version > 0 && (got.Status == bookapi.StatusConfirmed || got.Status == bookapi.StatusChangeRequested ||
		got.Status == bookapi.StatusCancellationRequested)
	out := air.Booking{BookingID: got.BookingID, Revision: got.Revision, AirConsol: got.Kind == bookapi.KindAirConsol, InForce: inForce,
		ConsumerID: got.ConsumerID, LegalEntityID: got.LegalEntityID, Edition: got.Request.LocationEdition, MAWB: got.MAWBNumber,
		CapacityVersion: got.Capacity.Version, Lines: len(got.Capacity.Lines),
		Airline: air.Airline{PartyID: got.CarrierPartyID, PartyRevision: got.CarrierPartyRevision, Designator: got.CarrierIATADesignator, AWBPrefix: got.CarrierAWBPrefix}}
	if legs := got.Request.Legs; len(legs) > 0 {
		out.Origin = air.Airport{Code: legs[0].Loading.Code, IATA: legs[0].Loading.IATA}
		out.Destination = air.Airport{Code: legs[len(legs)-1].Discharge.Code, IATA: legs[len(legs)-1].Discharge.IATA}
	}
	if len(got.Capacity.Lines) > 0 {
		for _, l := range got.Capacity.Lines[0].Legs {
			out.Segments = append(out.Segments, air.Segment{Flight: l.Flight, From: air.Airport{Code: l.Loading.Code, IATA: l.Loading.IATA},
				To: air.Airport{Code: l.Discharge.Code, IATA: l.Discharge.IATA}, ETD: l.ETD, ETA: l.ETA})
		}
	}
	return out, nil
}
