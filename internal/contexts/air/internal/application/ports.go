// Package application runs AIR's use cases. Business rules stay in domain; this package orders the steps:
// replay check, authorization, questions to BOOK and FUL outside any AIR transaction (rule T3), and one
// transaction for the movement (and, when a HAWB is drawn, the series and the HAWB register), its Outbox events
// and the command receipt (rule T2). A house command locks the movement first and then the legal entity's series.
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

	air "go.idp.xyz/idp-cargo/internal/contexts/air/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/numbering"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var (
	ErrUnauthenticated     = errors.New("air application: unauthenticated")
	ErrUnauthorized        = errors.New("air application: unauthorized")
	ErrIdempotencyConflict = errors.New("air application: command id reused with a different payload")
	ErrMissingDependency   = errors.New("air application: required dependency missing")
	ErrInvalidQuery        = errors.New("air application: invalid query")
	ErrUnknownBooking      = errors.New("air application: BOOK does not know the booking")
	ErrUnknownShipment     = errors.New("air application: FUL does not know the shipment")
	ErrNoSeries            = errors.New("air application: the legal entity has no HAWB series; give the number or register a series")
	ErrHAWBInUse           = errors.New("air application: the HAWB number belongs to another shipment of the legal entity")
	ErrHAWBDiffers         = errors.New("air application: the shipment has another HAWB number, which moves with it")
	ErrMAWBInUse           = errors.New("air application: the MAWB number is assigned to another movement of the tenant")
)

// Scope is the verified caller of a command.
type Scope struct {
	TenantID air.TenantID
	ActorID  string
}

// Action is a command name an Authorizer decides on.
type Action string

// Receipt is a committed command's result.
type Receipt struct {
	CommandID    string
	SubjectID    string // movement, or legal entity for a series
	NewRevision  int64
	EntryNo      int
	HAWB         string
	FactsVersion int // the AirDocFacts version a finalization published
	RequestHash  string
}

type TxRunner interface {
	WithinTransaction(context.Context, func(context.Context) error) error
}

// MovementStore loads and writes movements; Load locks the movement row.
type MovementStore interface {
	Load(context.Context, air.TenantID, air.MovementID) (*air.Movement, int64, error)
	Read(context.Context, air.TenantID, air.MovementID) (*air.Movement, int64, error)
	Insert(context.Context, *air.Movement) (int64, error)
	Save(context.Context, *air.Movement, int64) (int64, error)
	ByMAWB(context.Context, air.TenantID, string) (air.MovementID, bool, error)
}

// SeriesStore reads and writes HAWB series, which never change once registered.
type SeriesStore interface {
	Read(context.Context, air.TenantID, string) (*air.Series, int64, error)
	Insert(context.Context, *air.Series) (int64, error)
}

// Numbers is the platform's sequence of a tenant's document type (idp-platform-go, MD-28), in AIR's schema: Next takes
// the next value in the command's transaction, Seed starts a sequence after a value, Last answers the last value per
// period.
type Numbers interface {
	Next(ctx context.Context, tenant, docType, period string) (int64, error)
	Seed(ctx context.Context, tenant, docType, period string, last int64) error
	Last(ctx context.Context, tenant, docType string) (map[string]int64, error)
}

// Registration is where a HAWB number belongs.
type Registration struct {
	HAWB       string
	ShipmentID string
	MovementID air.MovementID // the movement with the live entry, if any
	EntryNo    int
}

// HAWBRegister keeps every HAWB number of a legal entity with its shipment, for ever: a number belongs to one
// shipment and a shipment keeps one number.
type HAWBRegister interface {
	OfShipment(ctx context.Context, tenant air.TenantID, legalEntity, shipment string) (string, bool, error)
	Find(ctx context.Context, tenant air.TenantID, legalEntity, hawb string) (Registration, bool, error)
	Claim(ctx context.Context, tenant air.TenantID, legalEntity, hawb, shipment string, drawn bool, at time.Time) error
}

type Receipts interface {
	Find(context.Context, air.TenantID, string) (Receipt, bool, error)
	Save(context.Context, air.TenantID, Receipt, string) error
}

type IntegrationOutbox interface {
	EnqueueAIREvent(context.Context, air.Event) error
}

type Authorization interface {
	RequireAirAction(context.Context, Scope, Action, string) error
}

// Bookings reads CarrierBooking v1 (relationship 5) outside AIR's transactions.
type Bookings interface {
	Booking(ctx context.Context, tenant air.TenantID, booking string) (air.Booking, error)
}

// Plans reads FUL's AirOperationPlan v1 (relationship 9) outside AIR's transactions.
type Plans interface {
	Plan(ctx context.Context, tenant air.TenantID, shipment string) (air.Plan, error)
}

// Clock tells the time AIR records a command (T1).
type Clock interface{ Now() time.Time }

// Deps are the ports every AIR command uses.
type Deps struct {
	Tx        TxRunner
	Movements MovementStore
	Series    SeriesStore
	Numbers   Numbers
	Register  HAWBRegister
	Receipts  Receipts
	Outbox    IntegrationOutbox
	Auth      Authorization
	Clock     Clock
	Window    kernel.RecordWindow
	Bookings  Bookings
	Plans     Plans
}

// Command is what every AIR command states; Payload is the command as the caller sent it.
type Command struct {
	CommandID        string
	Scope            Scope
	Action           Action
	Subject          string
	ExpectedRevision int64
	OccurredAt       time.Time
	Payload          any
}

func (d Deps) begin(ctx context.Context, c Command) (hash string, entered time.Time, stored Receipt, replayed bool, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if d.Tx == nil || d.Movements == nil || d.Series == nil || d.Numbers == nil || d.Register == nil || d.Receipts == nil || d.Outbox == nil || d.Auth == nil || d.Clock == nil {
		return "", time.Time{}, Receipt{}, false, ErrMissingDependency
	}
	if c.Scope.TenantID == "" || strings.TrimSpace(c.Scope.ActorID) == "" || c.CommandID == "" || c.OccurredAt.IsZero() {
		return "", time.Time{}, Receipt{}, false, ErrUnauthenticated
	}
	if scoped, ok := d.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(c.Scope.TenantID) {
		return "", time.Time{}, Receipt{}, false, ErrUnauthorized
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
	if err = d.Auth.RequireAirAction(ctx, c.Scope, c.Action, c.Subject); err != nil {
		return "", time.Time{}, Receipt{}, false, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	entered = d.Clock.Now().UTC().Truncate(time.Microsecond)
	if err = d.Window.Check(c.OccurredAt, entered); err != nil {
		return "", time.Time{}, Receipt{}, false, fmt.Errorf("%w: business time %s, recorded %s", err, c.OccurredAt.Format(time.RFC3339), entered.Format(time.RFC3339))
	}
	return hash, entered, Receipt{}, false, nil
}

type aggregate interface {
	Pending() []air.Event
	Acknowledge()
}

// write commits mutate's aggregates, their Outbox events and the receipt in one transaction.
func (d Deps) write(ctx context.Context, c Command, hash, kind string, mutate func(context.Context) (Receipt, []aggregate, error)) (Receipt, error) {
	var result Receipt
	var changed []aggregate
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
		receipt, aggs, err := mutate(txCtx)
		if err != nil {
			return err
		}
		for _, a := range aggs {
			for _, e := range a.Pending() {
				if err = d.Outbox.EnqueueAIREvent(txCtx, e); err != nil {
					return err
				}
			}
		}
		receipt.CommandID, receipt.RequestHash = c.CommandID, hash
		if err = d.Receipts.Save(txCtx, c.Scope.TenantID, receipt, kind); err != nil {
			return err
		}
		result, changed = receipt, aggs
		return nil
	})
	if err != nil {
		return Receipt{}, err
	}
	for _, a := range changed {
		a.Acknowledge()
	}
	return result, nil
}

// Open opens a movement from its booking.
func (d Deps) Open(ctx context.Context, c Command, consolidation, booking string) (Receipt, error) {
	if d.Bookings == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	b, err := d.Bookings.Booking(ctx, c.Scope.TenantID, booking)
	if err != nil {
		return Receipt{}, err
	}
	return d.write(ctx, c, hash, "AirMovement", func(txCtx context.Context) (Receipt, []aggregate, error) {
		m, err := air.OpenMovement(c.Scope.TenantID, air.MovementID(c.Subject), consolidation, b, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		m.Stamp(entered)
		revision, err := d.Movements.Insert(txCtx, m)
		return Receipt{SubjectID: c.Subject, NewRevision: revision}, []aggregate{m}, err
	})
}

// Change runs a command on a stored movement locked at the expected revision. read asks BOOK or FUL first,
// outside AIR's transaction, with the movement as it stood; apply may draw from the series in the transaction.
func (d Deps) Change(ctx context.Context, c Command, read func(context.Context, *air.Movement) error,
	apply func(context.Context, *air.Movement) (entry int, hawb string, err error)) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	if read != nil {
		var current *air.Movement
		if err = d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
			var revision int64
			current, revision, err = d.Movements.Read(txCtx, c.Scope.TenantID, air.MovementID(c.Subject))
			if err == nil && revision != c.ExpectedRevision {
				return air.ErrConflict
			}
			return err
		}); err != nil {
			return Receipt{}, err
		}
		if err = read(ctx, current); err != nil {
			return Receipt{}, err
		}
	}
	return d.write(ctx, c, hash, "AirMovement", func(txCtx context.Context) (Receipt, []aggregate, error) {
		m, revision, err := d.Movements.Load(txCtx, c.Scope.TenantID, air.MovementID(c.Subject))
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, air.ErrConflict
		}
		entry, hawb, err := apply(txCtx, m)
		if err != nil {
			return Receipt{}, nil, err
		}
		m.Stamp(entered)
		facts := 0
		if added := m.AddedDocFacts(); len(added) > 0 {
			facts = added[len(added)-1].No
		}
		if revision, err = d.Movements.Save(txCtx, m, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, EntryNo: entry, HAWB: hawb, FactsVersion: facts}, []aggregate{m}, nil
	})
}

// HAWB returns the number a house takes: the one it holds under the legal entity, else the one given, else the next
// of the entity's template, taken from the platform's sequence of the period of the business time in this
// transaction. A number taken is registered to the shipment.
func (d Deps) HAWB(ctx context.Context, tenant air.TenantID, legalEntity, shipment, given string, at time.Time) (string, error) {
	held, ok, err := d.Register.OfShipment(ctx, tenant, legalEntity, shipment)
	if err != nil {
		return "", err
	}
	if ok {
		if given != "" && given != held {
			return "", fmt.Errorf("%w: %s holds %s", ErrHAWBDiffers, shipment, held)
		}
		return held, nil
	}
	if given != "" {
		if !air.ValidHAWB(given) {
			return "", air.ErrInvalidHAWB
		}
		return given, d.Register.Claim(ctx, tenant, legalEntity, given, shipment, false, at)
	}
	s, _, err := d.Series.Read(ctx, tenant, legalEntity)
	if err != nil {
		return "", err
	}
	tmpl, err := numbering.Parse(s.State().Template)
	if err != nil {
		return "", fmt.Errorf("%w: %v", air.ErrInvalidSeries, err)
	}
	seq, err := d.Numbers.Next(ctx, string(tenant), numbering.DocType(DocTypeHAWB, legalEntity), tmpl.Period(at))
	if err != nil {
		return "", err
	}
	number, err := tmpl.Render(seq, at)
	switch {
	case errors.Is(err, numbering.ErrExhausted):
		return "", fmt.Errorf("%w: %v", air.ErrSeriesExhausted, err)
	case err != nil:
		return "", err
	case !air.ValidHAWB(number):
		return "", fmt.Errorf("%w: the template renders %q", air.ErrInvalidHAWB, number)
	}
	return number, d.Register.Claim(ctx, tenant, legalEntity, number, shipment, true, at)
}

// DocTypeHAWB is the document type of HAWB numbers in the platform's sequences (<type>/<legal entity>).
const DocTypeHAWB = "HAWB"

// RegisterSeries gives a legal entity its HAWB numbering: a template of the platform's language in Cargo's subset
// whose numbers are HAWB numbers (capital letters and digits, twelve at most) and which does not reset (AIR does not
// know the issuing office's time zone, so a year or month in the number could follow UTC instead), and its first
// number, which seeds the sequence.
func (d Deps) RegisterSeries(ctx context.Context, c Command, legalEntity, template string, first int64) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	tmpl, err := numbering.Parse(template)
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: %v", air.ErrInvalidSeries, err)
	}
	if tmpl.Resets() {
		return Receipt{}, fmt.Errorf("%w: %s restarts by date; a HAWB template does not reset yet (the issuing office's time zone is not known)",
			air.ErrInvalidSeries, template)
	}
	longest, err := tmpl.Longest()
	if err != nil || !air.ValidHAWB(longest) {
		return Receipt{}, fmt.Errorf("%w: %s renders %q; HAWB numbers are capital letters and digits, twelve at most", air.ErrInvalidSeries, template, longest)
	}
	if _, err = tmpl.Render(first, c.OccurredAt); err != nil {
		return Receipt{}, fmt.Errorf("%w: first number %d: %v", air.ErrInvalidSeries, first, err)
	}
	period := tmpl.Period(c.OccurredAt)
	return d.write(ctx, c, hash, "HawbSeries", func(txCtx context.Context) (Receipt, []aggregate, error) {
		s, err := air.RegisterSeries(c.Scope.TenantID, legalEntity, template, first, period, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		s.Stamp(entered)
		revision, err := d.Series.Insert(txCtx, s)
		if err != nil {
			return Receipt{}, nil, err
		}
		if err = d.Numbers.Seed(txCtx, string(c.Scope.TenantID), numbering.DocType(DocTypeHAWB, legalEntity), period, first-1); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: legalEntity, NewRevision: revision}, []aggregate{s}, nil
	})
}
