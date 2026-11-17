package book

import (
	"context"
	"errors"
	"fmt"
	"testing"

	app "go.idp.xyz/idp-cargo/internal/contexts/air/internal/application"
	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
)

// stub answers GetCarrierBooking with an error; AIR asks BOOK nothing else.
type stub struct {
	bookapi.Queries
	err error
}

func (s stub) GetCarrierBooking(context.Context, bookapi.GetCarrierBooking) (bookapi.CarrierBooking, error) {
	return bookapi.CarrierBooking{}, s.err
}

// BOOK's errors reach AIR's caller in AIR's own kinds, never BOOK's: a booking BOOK does not know or a question it
// rejects is an unknown booking, any other kind is text, and what is not BOOK's passes unchanged.
func TestBookingErrorsAreAIRs(t *testing.T) {
	ctx := context.Background()
	kinds := []error{bookapi.ErrUnauthenticated, bookapi.ErrUnauthorized, bookapi.ErrNotFound, bookapi.ErrConflict, bookapi.ErrIdempotencyConflict,
		bookapi.ErrRejected}
	for _, kind := range kinds {
		_, err := Bookings{BOOK: stub{err: fmt.Errorf("%w: stub", kind)}}.Booking(ctx, "T", "BK-1")
		for _, k := range kinds {
			if errors.Is(err, k) {
				t.Errorf("%v reaches AIR's caller as BOOK's kind: %v", kind, err)
			}
		}
		unknown := kind == bookapi.ErrNotFound || kind == bookapi.ErrRejected
		if err == nil || errors.Is(err, app.ErrUnknownBooking) != unknown {
			t.Errorf("%v: unknown booking %v, want %v: %v", kind, !unknown, unknown, err)
		}
	}
	if _, err := (Bookings{BOOK: stub{err: context.Canceled}}).Booking(ctx, "T", "BK-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context passes unchanged: %v", err)
	}
}
