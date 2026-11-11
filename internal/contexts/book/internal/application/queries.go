package application

import (
	"context"
	"time"

	book "go.idp.xyz/idp-cargo/internal/contexts/book/internal/domain"
)

// CutOffDue is a cut-off of a booking's current capacity that falls in a window.
type CutOffDue struct {
	BookingID       book.BookingID
	ConsumerID      string
	CapacityVersion int
	Line, Leg       int
	Flight, Voyage  string
	CutOff          book.CutOff
}

// BookingQueries are the store's lists, each in one snapshot.
type BookingQueries interface {
	ForConsumer(ctx context.Context, tenant book.TenantID, consumerKind, consumerID string) ([]book.BookingID, error)
	ByReference(ctx context.Context, tenant book.TenantID, carrierParty string, ref book.Reference) (book.BookingID, bool, error)
	Awaiting(ctx context.Context, tenant book.TenantID, sentBefore time.Time, limit int) ([]book.BookingID, error)
	CutOffsDue(ctx context.Context, tenant book.TenantID, from, until time.Time, limit int) ([]CutOffDue, error)
}

// Reads answers BOOK's queries without locks.
type Reads struct {
	Tx       TxRunner
	Bookings BookingStore
	Lists    BookingQueries
}

// Booking reads one booking with its revision.
type Booking struct {
	Booking  *book.Booking
	Revision int64
}

// Get reads a booking.
func (r Reads) Get(ctx context.Context, tenant book.TenantID, id book.BookingID) (Booking, error) {
	if tenant == "" || id == "" {
		return Booking{}, ErrInvalidQuery
	}
	var out Booking
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		b, revision, err := r.Bookings.Read(txCtx, tenant, id)
		out = Booking{b, revision}
		return err
	})
	return out, err
}

// list reads the bookings a store list names, in the same snapshot.
func (r Reads) list(ctx context.Context, tenant book.TenantID, ids func(context.Context) ([]book.BookingID, error)) ([]Booking, error) {
	var out []Booking
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		list, err := ids(txCtx)
		if err != nil {
			return err
		}
		for _, id := range list {
			b, revision, err := r.Bookings.Read(txCtx, tenant, id)
			if err != nil {
				return err
			}
			out = append(out, Booking{b, revision})
		}
		return nil
	})
	return out, err
}

// ForConsolidation lists the bookings whose capacity is allocated to a consolidation.
func (r Reads) ForConsolidation(ctx context.Context, tenant book.TenantID, consolidation string) ([]Booking, error) {
	if tenant == "" || consolidation == "" {
		return nil, ErrInvalidQuery
	}
	return r.list(ctx, tenant, func(txCtx context.Context) ([]book.BookingID, error) {
		return r.Lists.ForConsumer(txCtx, tenant, book.ConsumerConsolidation, consolidation)
	})
}

// ByReference reads the live booking of a carrier that holds a reference; found is false when none does.
func (r Reads) ByReference(ctx context.Context, tenant book.TenantID, carrier string, ref book.Reference) (Booking, bool, error) {
	if tenant == "" || carrier == "" || ref.Value == "" {
		return Booking{}, false, ErrInvalidQuery
	}
	if ref.Type == book.ReferenceMAWBNumber {
		if v, ok := book.NormalizeMAWB(ref.Value); ok {
			ref.Value = v
		}
	}
	list, err := r.list(ctx, tenant, func(txCtx context.Context) ([]book.BookingID, error) {
		id, found, err := r.Lists.ByReference(txCtx, tenant, carrier, ref)
		if err != nil || !found {
			return nil, err
		}
		return []book.BookingID{id}, nil
	})
	if err != nil || len(list) == 0 {
		return Booking{}, false, err
	}
	return list[0], true, nil
}

// Awaiting lists bookings waiting for the carrier since before an instant, oldest first.
func (r Reads) Awaiting(ctx context.Context, tenant book.TenantID, sentBefore time.Time, limit int) ([]Booking, error) {
	if limit == 0 {
		limit = 50
	}
	if tenant == "" || sentBefore.IsZero() || limit < 1 || limit > 200 {
		return nil, ErrInvalidQuery
	}
	return r.list(ctx, tenant, func(txCtx context.Context) ([]book.BookingID, error) {
		return r.Lists.Awaiting(txCtx, tenant, sentBefore, limit)
	})
}

// CutOffsDue lists cut-offs in [from, until) of the current capacity of bookings in force.
func (r Reads) CutOffsDue(ctx context.Context, tenant book.TenantID, from, until time.Time, limit int) ([]CutOffDue, error) {
	if limit == 0 {
		limit = 100
	}
	if tenant == "" || from.IsZero() || !until.After(from) || limit < 1 || limit > 500 {
		return nil, ErrInvalidQuery
	}
	var out []CutOffDue
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		out, err = r.Lists.CutOffsDue(txCtx, tenant, from, until, limit)
		return err
	})
	return out, err
}
