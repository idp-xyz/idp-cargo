// Package application runs BOOK's use cases. Business rules stay in domain; this package orders the steps:
// replay check, authorization, questions to ORG and the location reference data outside any BOOK
// transaction (rule T3), and one transaction for the booking, its Outbox events and the command receipt
// (rule T2).
package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	book "go.idp.xyz/idp-cargo/internal/contexts/book/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var (
	ErrUnauthenticated     = errors.New("book application: unauthenticated")
	ErrUnauthorized        = errors.New("book application: unauthorized")
	ErrIdempotencyConflict = errors.New("book application: command id reused with a different payload")
	ErrMissingDependency   = errors.New("book application: required dependency missing")
	ErrInvalidQuery        = errors.New("book application: invalid query")
	ErrCarrierNotConfirmed = errors.New("book application: ORG does not confirm the carrier in the role for the legal entity")
	ErrLocationRefused     = errors.New("book application: the location reference data does not resolve the location for the transport mode")
	ErrReferenceInUse      = errors.New("book application: another live booking of the carrier holds the reference")
)

// Scope is the verified caller of a command.
type Scope struct {
	TenantID book.TenantID
	ActorID  string
}

// Action is a command name an Authorizer decides on.
type Action string

// Receipt is a committed command's result.
type Receipt struct {
	CommandID   string
	BookingID   book.BookingID
	NewRevision int64
	RequestHash string
}

type TxRunner interface {
	WithinTransaction(context.Context, func(context.Context) error) error
}

// BookingStore loads and writes bookings. Load locks the booking for a command; Read does not.
type BookingStore interface {
	Load(context.Context, book.TenantID, book.BookingID) (*book.Booking, int64, error)
	Read(context.Context, book.TenantID, book.BookingID) (*book.Booking, int64, error)
	Insert(context.Context, *book.Booking) (int64, error)
	Save(context.Context, *book.Booking, int64) (int64, error)
}

type Receipts interface {
	Find(context.Context, book.TenantID, string) (Receipt, bool, error)
	Save(context.Context, book.TenantID, Receipt) error
}

type IntegrationOutbox interface {
	EnqueueBOOKEvent(context.Context, book.Event) error
}

type Authorization interface {
	RequireBookingAction(context.Context, Scope, Action, book.BookingID) error
}

// Carriers asks ORG whether a party may act in a role for a legal entity at an instant, and returns what
// BOOK freezes of it. It is called outside BOOK's transactions; a refusal is ErrCarrierNotConfirmed.
type Carriers interface {
	Carrier(ctx context.Context, tenant book.TenantID, party, role, legalEntity string, at time.Time) (book.Carrier, error)
}

// ResolvedLocation is a location resolved against the location reference data (F2).
type ResolvedLocation struct {
	Code    kernel.LocationCode
	IATA    string
	Edition string
}

// Locations resolves a UN/LOCODE, an alias carriers use, or for AIR an IATA airport code, for a mode. It is
// called outside BOOK's transactions; a refusal is ErrLocationRefused.
type Locations interface {
	Resolve(ctx context.Context, mode book.Mode, l kernel.LocationCode) (ResolvedLocation, error)
}

// Clock tells the time BOOK records a command (T1).
type Clock interface{ Now() time.Time }

// Deps are the ports every BOOK command uses.
type Deps struct {
	Tx        TxRunner
	Bookings  BookingStore
	Receipts  Receipts
	Outbox    IntegrationOutbox
	Auth      Authorization
	Clock     Clock
	Window    kernel.RecordWindow
	Locations Locations
}

// Command is what every BOOK command states. Payload is the whole command as the caller sent it; its hash
// guards replays.
type Command struct {
	CommandID        string
	Scope            Scope
	Action           Action
	BookingID        book.BookingID
	ExpectedRevision int64
	OccurredAt       time.Time
	Payload          any
}

func (d Deps) check(ctx context.Context, c Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.Tx == nil || d.Bookings == nil || d.Receipts == nil || d.Outbox == nil || d.Auth == nil || d.Clock == nil {
		return ErrMissingDependency
	}
	if c.Scope.TenantID == "" || strings.TrimSpace(c.Scope.ActorID) == "" || c.CommandID == "" || c.OccurredAt.IsZero() {
		return ErrUnauthenticated
	}
	if scoped, ok := d.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(c.Scope.TenantID) {
		return ErrUnauthorized
	}
	return nil
}

// begin checks a command, returns its stored receipt if it was committed already, and otherwise authorizes
// it and returns the time BOOK records it, if the business time lies within the record window before (T1).
func (d Deps) begin(ctx context.Context, c Command) (hash string, entered time.Time, stored Receipt, replayed bool, err error) {
	if err = d.check(ctx, c); err != nil {
		return "", time.Time{}, Receipt{}, false, err
	}
	blob, err := json.Marshal(c.Payload)
	if err != nil {
		return "", time.Time{}, Receipt{}, false, err
	}
	sum := sha256.Sum256(blob)
	hash = hex.EncodeToString(sum[:])
	if err = d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		r, ok, err := d.Receipts.Find(txCtx, c.Scope.TenantID, c.CommandID)
		if err != nil || !ok {
			return err
		}
		if r.RequestHash != hash {
			return ErrIdempotencyConflict
		}
		stored, replayed = r, true
		return nil
	}); err != nil || replayed {
		return hash, time.Time{}, stored, replayed, err
	}
	if err = d.Auth.RequireBookingAction(ctx, c.Scope, c.Action, c.BookingID); err != nil {
		return "", time.Time{}, Receipt{}, false, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	entered = d.Clock.Now().UTC().Truncate(time.Microsecond)
	if err = d.Window.Check(c.OccurredAt, entered); err != nil {
		return "", time.Time{}, Receipt{}, false, fmt.Errorf("%w: business time %s, recorded %s", err, c.OccurredAt.Format(time.RFC3339), entered.Format(time.RFC3339))
	}
	return hash, entered, Receipt{}, false, nil
}

// write commits mutate's booking, its Outbox events and the command receipt in one transaction. A receipt
// stored meanwhile by a concurrent replay wins.
func (d Deps) write(ctx context.Context, c Command, hash string, mutate func(context.Context) (*book.Booking, int64, error)) (Receipt, error) {
	var result Receipt
	var changed *book.Booking
	err := d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		r, ok, err := d.Receipts.Find(txCtx, c.Scope.TenantID, c.CommandID)
		if err != nil {
			return err
		}
		if ok {
			if r.RequestHash != hash {
				return ErrIdempotencyConflict
			}
			result = r
			return nil
		}
		b, revision, err := mutate(txCtx)
		if err != nil {
			return err
		}
		for _, e := range b.Pending() {
			if err = d.Outbox.EnqueueBOOKEvent(txCtx, e); err != nil {
				return err
			}
		}
		result = Receipt{CommandID: c.CommandID, BookingID: b.ID(), NewRevision: revision, RequestHash: hash}
		if err = d.Receipts.Save(txCtx, c.Scope.TenantID, result); err != nil {
			return err
		}
		changed = b
		return nil
	})
	if err != nil {
		return Receipt{}, err
	}
	if changed != nil {
		changed.Acknowledge()
	}
	return result, nil
}

// Change runs a command on a stored booking: prepare asks other contexts and the reference data first, outside
// BOOK's transaction (T3); apply changes the booking locked at the expected revision.
func (d Deps) Change(ctx context.Context, c Command, prepare func(context.Context, *book.Booking) error, apply func(*book.Booking) error) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	if prepare != nil {
		var current *book.Booking
		if err = d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
			var revision int64
			current, revision, err = d.Bookings.Read(txCtx, c.Scope.TenantID, c.BookingID)
			if err == nil && revision != c.ExpectedRevision {
				return book.ErrConflict
			}
			return err
		}); err != nil {
			return Receipt{}, err
		}
		if err = prepare(ctx, current); err != nil {
			return Receipt{}, err
		}
	}
	return d.write(ctx, c, hash, func(txCtx context.Context) (*book.Booking, int64, error) {
		b, revision, err := d.Bookings.Load(txCtx, c.Scope.TenantID, c.BookingID)
		if err != nil {
			return nil, 0, err
		}
		if revision != c.ExpectedRevision {
			return nil, 0, book.ErrConflict
		}
		if err = apply(b); err != nil {
			return nil, 0, err
		}
		b.Stamp(entered)
		revision, err = d.Bookings.Save(txCtx, b, revision)
		return b, revision, err
	})
}

// Draft drafts a booking after ORG confirmed the carrier and the request's legs were resolved.
func (d Deps) Draft(ctx context.Context, c Command, carriers Carriers, kind book.Kind, legalEntity, carrierParty, consolidation string,
	rebooks book.BookingID, request book.Request) (Receipt, error) {
	if carriers == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	if !kind.Valid() {
		return Receipt{}, book.ErrInvalidBooking
	}
	carrier, err := carriers.Carrier(ctx, c.Scope.TenantID, carrierParty, kind.Role(), legalEntity, c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	if request, err = d.ResolveRequest(ctx, kind, request); err != nil {
		return Receipt{}, err
	}
	return d.write(ctx, c, hash, func(txCtx context.Context) (*book.Booking, int64, error) {
		b, err := book.DraftBooking(c.Scope.TenantID, c.BookingID, kind, legalEntity, carrier, consolidation, rebooks, request, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return nil, 0, err
		}
		b.Stamp(entered)
		revision, err := d.Bookings.Insert(txCtx, b)
		return b, revision, err
	})
}

// ResolveRequest resolves a request's ports for the kind's mode and freezes the edition (F2).
func (d Deps) ResolveRequest(ctx context.Context, kind book.Kind, r book.Request) (book.Request, error) {
	editions := map[string]bool{}
	for i := range r.Legs {
		for _, l := range []*book.Location{&r.Legs[i].Loading, &r.Legs[i].Discharge} {
			edition, err := d.resolve(ctx, kind.Mode(), l)
			if err != nil {
				return book.Request{}, err
			}
			editions[edition] = true
			r.LocationEdition = edition
		}
	}
	if len(editions) > 1 {
		return book.Request{}, fmt.Errorf("%w: the reference data edition changed while the legs were resolved; try again", book.ErrConflict)
	}
	return r, nil
}

// ResolveGrant resolves the ports of confirmed lines and their cut-off places.
func (d Deps) ResolveGrant(ctx context.Context, kind book.Kind, lines []book.Line) (book.Grant, error) {
	g := book.Grant{Lines: lines}
	editions := map[string]bool{}
	for i := range g.Lines {
		g.Lines[i].No = i + 1
		for j := range g.Lines[i].Legs {
			leg := &g.Lines[i].Legs[j]
			for _, l := range []*book.Location{&leg.Loading, &leg.Discharge} {
				edition, err := d.resolve(ctx, kind.Mode(), l)
				if err != nil {
					return book.Grant{}, err
				}
				editions[edition] = true
				g.Edition = edition
			}
			for k := range leg.CutOffs {
				if c := &leg.CutOffs[k]; !c.Location.IsZero() {
					place := book.Location{Code: c.Location}
					edition, err := d.resolve(ctx, kind.Mode(), &place)
					if err != nil {
						return book.Grant{}, err
					}
					editions[edition] = true
					c.Location = place.Code
				}
			}
		}
	}
	if len(editions) > 1 {
		return book.Grant{}, fmt.Errorf("%w: the reference data edition changed while the legs were resolved; try again", book.ErrConflict)
	}
	return g, nil
}

// resolve replaces a location given as UN/LOCODE, alias or IATA code by its UN/LOCODE and, for AIR, IATA code.
func (d Deps) resolve(ctx context.Context, mode book.Mode, l *book.Location) (string, error) {
	if d.Locations == nil {
		return "", fmt.Errorf("%w: BOOK has no location reference data", ErrMissingDependency)
	}
	if l.Code.IsZero() {
		return "", fmt.Errorf("%w: a leg needs both ports", book.ErrInvalidRequest)
	}
	r, err := d.Locations.Resolve(ctx, mode, l.Code)
	if err != nil {
		return "", err
	}
	l.Code, l.IATA = r.Code, ""
	if mode == book.ModeAir {
		l.IATA = r.IATA
	}
	return r.Edition, nil
}
