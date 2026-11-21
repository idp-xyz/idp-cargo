// Package application runs SEA's use cases. Business rules stay in domain; this package orders the steps:
// authorization, replay check, questions to BOOK and FUL outside any SEA transaction (rule T3), and one transaction
// for the movement (and, when an HBL is drawn, the series and the HBL register), its Outbox events and the command
// receipt (rule T2). A command is authorized before its replay is answered: a replay answers only an actor still
// allowed, and a command id in use tells no one else. The transaction holds the command id first, so the same
// command sent twice at once commits once and the other answers its receipt. A house command locks the movement
// first and then the legal entity's sequence.
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

	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/numbering"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var (
	ErrUnauthenticated     = errors.New("sea application: unauthenticated")
	ErrUnauthorized        = errors.New("sea application: unauthorized")
	ErrIdempotencyConflict = errors.New("sea application: command id reused with a different payload")
	ErrMissingDependency   = errors.New("sea application: required dependency missing")
	ErrInvalidQuery        = errors.New("sea application: invalid query")
	ErrUnknownBooking      = errors.New("sea application: BOOK does not know the booking")
	ErrUnknownShipment     = errors.New("sea application: FUL does not know the shipment")
	ErrNoSeries            = errors.New("sea application: the legal entity has no HBL series; give the number or register a series")
	ErrHBLInUse            = errors.New("sea application: the HBL number belongs to another shipment of the legal entity")
	ErrHBLDiffers          = errors.New("sea application: the shipment has another HBL number, which moves with it")
	ErrMasterBillInUse     = errors.New("sea application: the number is the current master bill reference of another movement of the tenant")
)

// Scope is the verified caller of a command.
type Scope struct {
	TenantID sea.TenantID
	ActorID  string
}

// Action is a command name an Authorizer decides on.
type Action string

// Receipt is a committed command's result.
type Receipt struct {
	CommandID    string
	Kind         string // OceanMovement, HblSeries or ContainerVisit; what result_type stores
	SubjectID    string // movement, legal entity for a series, or visit
	NewRevision  int64
	EntryNo      int
	HBL          string
	BillNo       int // the master bill entry a RecordMasterBill wrote
	SealNo       int // the seal a RecordContainerSeal wrote
	AssertionNo  int // the VGM assertion an AssertVgm or RecordVgmResponse wrote
	EventNo      int // the container event a RecordContainerEvent wrote
	ReleaseNo    int // the release an AcceptCarrierLoadingConflict wrote
	FactNo       int // the carrier fact a RecordOceanCarrierFact wrote
	FactsVersion int // the OceanDocFacts version a FinalizeOceanDocFacts published
	RequestHash  string
}

type TxRunner interface {
	WithinTransaction(context.Context, func(context.Context) error) error
}

// MovementStore loads and writes movements; Load locks the movement row.
type MovementStore interface {
	Load(context.Context, sea.TenantID, sea.MovementID) (*sea.Movement, int64, error)
	Read(context.Context, sea.TenantID, sea.MovementID) (*sea.Movement, int64, error)
	Insert(context.Context, *sea.Movement) (int64, error)
	Save(context.Context, *sea.Movement, int64) (int64, error)
	// InsertCarrierFacts writes facts added since the movement was loaded, without moving its revision.
	InsertCarrierFacts(context.Context, *sea.Movement) error
	ByMasterBill(ctx context.Context, tenant sea.TenantID, issuer, number string) (sea.MovementID, bool, error)
}

// SeriesStore reads and writes HBL series, which never change once registered.
type SeriesStore interface {
	Read(context.Context, sea.TenantID, string) (*sea.Series, int64, error)
	Insert(context.Context, *sea.Series) (int64, error)
}

// Numbers is the platform's sequence of a tenant's document type (idp-platform-go, MD-28), in SEA's schema: Next takes
// the next value in the command's transaction, Seed starts a sequence after a value, Last answers the last value per
// period.
type Numbers interface {
	Next(ctx context.Context, tenant, docType, period string) (int64, error)
	Seed(ctx context.Context, tenant, docType, period string, last int64) error
	Last(ctx context.Context, tenant, docType string) (map[string]int64, error)
}

// Registration is where an HBL number belongs.
type Registration struct {
	HBL        string
	ShipmentID string
	MovementID sea.MovementID // the movement with the live entry, if any
	EntryNo    int
}

// HBLRegister keeps every HBL number of a legal entity with its shipment, for ever: a number belongs to one shipment
// and a shipment keeps one number.
type HBLRegister interface {
	OfShipment(ctx context.Context, tenant sea.TenantID, legalEntity, shipment string) (string, bool, error)
	Find(ctx context.Context, tenant sea.TenantID, legalEntity, hbl string) (Registration, bool, error)
	Claim(ctx context.Context, tenant sea.TenantID, legalEntity, hbl, shipment string, drawn bool, at time.Time) error
}

type Receipts interface {
	Find(context.Context, sea.TenantID, string) (Receipt, bool, error)
	Save(context.Context, sea.TenantID, Receipt, string) error
}

type IntegrationOutbox interface {
	EnqueueSEAEvent(context.Context, sea.Event) error
}

type Authorization interface {
	RequireSeaAction(context.Context, Scope, Action, string) error
}

// Bookings reads CarrierBooking v1 (relationship 6) outside SEA's transactions.
type Bookings interface {
	Booking(ctx context.Context, tenant sea.TenantID, booking string) (sea.Booking, error)
}

// Plans reads FUL's OceanOperationPlan v1 and OceanEvidenceCurrency v1 (relationship 10) outside SEA's transactions.
type Plans interface {
	Plan(ctx context.Context, tenant sea.TenantID, shipment string) (sea.Plan, error)
	// EvidenceBehind asks FUL whether PHY's facts of the shipment are newer than the evidence FUL recorded: whether
	// RecordVerifiedLoadEvidence would record a new version now.
	EvidenceBehind(ctx context.Context, tenant sea.TenantID, shipment string) (bool, error)
}

// Clock tells the time SEA records a command (T1).
type Clock interface{ Now() time.Time }

// Deps are the ports every SEA command uses.
type Deps struct {
	Tx        TxRunner
	Movements MovementStore
	Visits    VisitStore
	Series    SeriesStore
	Numbers   Numbers
	Register  HBLRegister
	Receipts  Receipts
	Outbox    IntegrationOutbox
	Auth      Authorization
	Clock     Clock
	Window    kernel.RecordWindow
	Bookings  Bookings
	Plans     Plans
}

// Command is what every SEA command states; Payload is the command as the caller sent it.
type Command struct {
	CommandID        string
	Scope            Scope
	Action           Action
	Subject          string
	ExpectedRevision int64
	OccurredAt       time.Time
	Payload          any
	// Earlier is the payload as an earlier version hashed it, when that differs: a receipt stored with that hash is
	// still answered. Nothing is stored with it.
	Earlier any
}

// requestHash is the hash a command's receipt is stored with. It covers the command's name: two commands of the same
// fields (a refresh and an end of one house) are not replays of each other.
func requestHash(action Action, payload any) (string, error) {
	blob, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(string(action)+"\x00"), blob...))
	return hex.EncodeToString(sum[:]), nil
}

// answers reports whether a stored receipt is this command's: stored with its hash, or with the hash an earlier
// version gave the same command.
func (c Command) answers(stored, hash string) bool {
	if stored == hash {
		return true
	}
	if c.Earlier == nil {
		return false
	}
	earlier, err := requestHash(c.Action, c.Earlier)
	return err == nil && stored == earlier
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
	if hash, err = requestHash(c.Action, c.Payload); err != nil {
		return "", time.Time{}, Receipt{}, false, err
	}
	// Authorized before a replay too: a replay answers only an actor still allowed, and a command id in use tells no
	// one else.
	if err = d.Auth.RequireSeaAction(ctx, c.Scope, c.Action, c.Subject); err != nil {
		return "", time.Time{}, Receipt{}, false, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if stored, replayed, err = d.replay(ctx, c, hash); err != nil || replayed {
		return hash, time.Time{}, stored, replayed, err
	}
	// The business time is admitted after the replay, so a command retried later still answers its receipt.
	entered = d.Clock.Now().UTC().Truncate(time.Microsecond)
	if err = d.Window.Check(c.OccurredAt, entered); err != nil {
		return "", time.Time{}, Receipt{}, false, fmt.Errorf("%w: business time %s, recorded %s", err, c.OccurredAt.Format(time.RFC3339), entered.Format(time.RFC3339))
	}
	return hash, entered, Receipt{}, false, nil
}

// replay answers the stored receipt of a command whose payload is unchanged.
func (d Deps) replay(ctx context.Context, c Command, hash string) (stored Receipt, replayed bool, err error) {
	err = d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		r, ok, err := d.Receipts.Find(txCtx, c.Scope.TenantID, c.CommandID)
		if err != nil || !ok {
			return err
		}
		if !c.answers(r.RequestHash, hash) {
			return ErrIdempotencyConflict
		}
		stored, replayed = r, true
		return nil
	})
	return stored, replayed, err
}

// committedMeanwhile answers the receipt of a command that a concurrent attempt committed while this attempt was
// preparing outside the transaction: what this attempt then read was the state the command had already changed, so
// its refusal (cause) says nothing about the command. Without a receipt the refusal stands.
func (d Deps) committedMeanwhile(ctx context.Context, c Command, hash string, cause error) (Receipt, error) {
	if stored, replayed, err := d.replay(ctx, c, hash); err == nil && replayed {
		return stored, nil
	}
	return Receipt{}, cause
}

type aggregate interface {
	Pending() []sea.Event
	Acknowledge()
}

// commandLocker is a Receipts store that can hold the command id for the transaction.
type commandLocker interface {
	Lock(ctx context.Context, tenant sea.TenantID, commandID string) error
}

// write commits mutate's aggregates, their Outbox events and the receipt in one transaction. The command id is held
// first, so that the same command sent twice at once (a client retrying before the first answer) commits once: the
// other waits, finds the receipt and answers it.
func (d Deps) write(ctx context.Context, c Command, hash, kind string, mutate func(context.Context) (Receipt, []aggregate, error)) (Receipt, error) {
	var result Receipt
	var changed []aggregate
	err := d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		if l, ok := d.Receipts.(commandLocker); ok {
			if err := l.Lock(txCtx, c.Scope.TenantID, c.CommandID); err != nil {
				return err
			}
		}
		r, ok, err := d.Receipts.Find(txCtx, c.Scope.TenantID, c.CommandID)
		if err != nil {
			return err
		}
		if ok {
			if !c.answers(r.RequestHash, hash) {
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
				if err = d.Outbox.EnqueueSEAEvent(txCtx, e); err != nil {
					return err
				}
			}
		}
		receipt.CommandID, receipt.Kind, receipt.RequestHash = c.CommandID, kind, hash
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
	return d.write(ctx, c, hash, "OceanMovement", func(txCtx context.Context) (Receipt, []aggregate, error) {
		m, err := sea.OpenMovement(c.Scope.TenantID, sea.MovementID(c.Subject), consolidation, b, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		m.Stamp(entered)
		revision, err := d.Movements.Insert(txCtx, m)
		return Receipt{SubjectID: c.Subject, NewRevision: revision}, []aggregate{m}, err
	})
}

// Result is what a change of a movement adds to its receipt.
type Result struct {
	EntryNo      int
	HBL          string
	BillNo       int
	FactsVersion int
	// FollowOnly says the command wrote no entry: the house's facts are as SEA froze them, and what is left is to
	// follow the seals of the entry it has, if the visits take them; else there is nothing to refresh.
	FollowOnly bool
}

// Change runs a command on a stored movement locked at the expected revision. read asks BOOK or FUL first,
// outside SEA's transaction, with the movement as it stood; apply may draw from the series in the transaction.
func (d Deps) Change(ctx context.Context, c Command, read func(context.Context, *sea.Movement) error,
	apply func(context.Context, *sea.Movement) (Result, error)) (Receipt, error) {
	return d.change(ctx, c, read, nil, apply)
}

// change is Change; with follow, which answers whether the command writes a house entry that carries stuffings, the
// transaction also takes the movement's live visits and records, on those SEA took as stuffed, the seal PHY sealed the
// stuffing with again (Visit.FollowSeal). The locks are taken in the order ChangeVisit takes them: the command id, the
// visits (in the order they were assigned), then the movement. A visit assigned after the visits were listed makes the
// command a conflict, to be tried again: its lock was not taken.
func (d Deps) change(ctx context.Context, c Command, read func(context.Context, *sea.Movement) error, follow func() bool,
	apply func(context.Context, *sea.Movement) (Result, error)) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	if read != nil {
		var current *sea.Movement
		if err = d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
			var revision int64
			current, revision, err = d.Movements.Read(txCtx, c.Scope.TenantID, sea.MovementID(c.Subject))
			if err == nil && revision != c.ExpectedRevision {
				return sea.ErrConflict
			}
			return err
		}); err != nil {
			return d.committedMeanwhile(ctx, c, hash, err)
		}
		if err = read(ctx, current); err != nil {
			return d.committedMeanwhile(ctx, c, hash, err)
		}
	}
	return d.write(ctx, c, hash, "OceanMovement", func(txCtx context.Context) (Receipt, []aggregate, error) {
		var visits []*sea.Visit
		var revisions []int64
		var listed []sea.VisitID
		locking := follow != nil && d.Visits != nil && follow()
		if locking {
			var err error
			if listed, err = d.Visits.OfMovement(txCtx, c.Scope.TenantID, sea.MovementID(c.Subject)); err != nil {
				return Receipt{}, nil, err
			}
			for _, id := range listed {
				if seen, _, err := d.Visits.Read(txCtx, c.Scope.TenantID, id); err != nil {
					return Receipt{}, nil, err
				} else if !seen.Live() {
					continue // a released visit never changes again
				}
				v, rev, err := d.Visits.Load(txCtx, c.Scope.TenantID, id)
				if err != nil {
					return Receipt{}, nil, err
				}
				visits, revisions = append(visits, v), append(revisions, rev)
			}
		}
		m, revision, err := d.Movements.Load(txCtx, c.Scope.TenantID, sea.MovementID(c.Subject))
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, sea.ErrConflict
		}
		if locking {
			// assigning a container locks the movement, which this transaction now holds: the list cannot grow
			if now, err := d.Visits.OfMovement(txCtx, c.Scope.TenantID, m.ID()); err != nil {
				return Receipt{}, nil, err
			} else if len(now) != len(listed) {
				return Receipt{}, nil, fmt.Errorf("%w: a container was assigned while the command was taking the movement's visits; try again", sea.ErrConflict)
			}
		}
		result, err := apply(txCtx, m)
		if err != nil {
			return Receipt{}, nil, err
		}
		if len(visits) > 0 && result.EntryNo > 0 {
			if err = m.FollowSeals(m.State().Entries[result.EntryNo-1], visits, c.Scope.ActorID, c.OccurredAt); err != nil {
				return Receipt{}, nil, err
			}
		}
		if result.FollowOnly {
			followed := false
			for _, v := range visits {
				followed = followed || len(v.Pending()) > 0
			}
			if !followed {
				return Receipt{}, nil, sea.ErrNothingToRefresh
			}
			// a seal recorded on a FINAL movement returns it to OPEN, as RecordContainerSeal does
			if err = m.Supersede(c.Scope.ActorID, c.OccurredAt); err != nil {
				return Receipt{}, nil, err
			}
		}
		aggs := []aggregate{}
		if !result.FollowOnly || len(m.Pending()) > 0 {
			m.Stamp(entered)
			if revision, err = d.Movements.Save(txCtx, m, revision); err != nil {
				return Receipt{}, nil, err
			}
			aggs = append(aggs, m)
		}
		for i, v := range visits {
			if len(v.Pending()) == 0 {
				continue
			}
			v.Stamp(entered)
			if _, err = d.Visits.Save(txCtx, v, revisions[i]); err != nil {
				return Receipt{}, nil, err
			}
			aggs = append(aggs, v)
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, EntryNo: result.EntryNo, HBL: result.HBL, BillNo: result.BillNo, FactsVersion: result.FactsVersion}, aggs, nil
	})
}

// ChangeHouse is Change for a command that writes a house's entry (Associate, Refresh): when PHY sealed a stuffing the
// entry carries again, the seal is recorded on the movement's container taken as stuffed with it, in the same
// transaction. It takes the command id, then the visits, then the movement, as ChangeVisit does.
func (d Deps) ChangeHouse(ctx context.Context, c Command, read func(context.Context, *sea.Movement) error, stuffings func() bool,
	apply func(context.Context, *sea.Movement) (Result, error)) (Receipt, error) {
	return d.change(ctx, c, read, stuffings, apply)
}

// RecordCarrierFact records a carrier report on a movement that is OPEN or FINAL. The movement's revision does not
// move, so a FINAL movement stays at the revision its document facts were published; only a loading that returns a
// FINAL movement to OPEN is a step of its revision.
func (d Deps) RecordCarrierFact(ctx context.Context, c Command, fact sea.CarrierFact) (Receipt, error) {
	if d.Movements == nil || d.Visits == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.write(ctx, c, hash, "OceanMovement", func(txCtx context.Context) (Receipt, []aggregate, error) {
		m, revision, err := d.Movements.Load(txCtx, c.Scope.TenantID, sea.MovementID(c.Subject))
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, sea.ErrConflict
		}
		ids, err := d.Visits.OfMovement(txCtx, m.State().TenantID, m.ID())
		if err != nil {
			return Receipt{}, nil, err
		}
		var live []string
		for _, id := range ids {
			v, _, err := d.Visits.Read(txCtx, m.State().TenantID, id)
			if err != nil {
				return Receipt{}, nil, err
			}
			if v.State().Status != sea.VisitReleased {
				live = append(live, v.State().Number)
			}
		}
		final := m.State().State == sea.StateFinal
		no, added, err := m.RecordCarrierFact(fact, live, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		if !added {
			return Receipt{SubjectID: c.Subject, NewRevision: revision, FactNo: no}, nil, nil
		}
		m.Stamp(entered)
		if final && m.State().State == sea.StateOpen {
			if revision, err = d.Movements.Save(txCtx, m, revision); err != nil {
				return Receipt{}, nil, err
			}
		}
		if err = d.Movements.InsertCarrierFacts(txCtx, m); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, FactNo: no}, []aggregate{m}, nil
	})
}

// HBL returns the number a house takes: the one it holds under the legal entity, else the one given, else the next
// of the entity's template, taken from the platform's sequence of the period of the business time in this
// transaction. A number taken is registered to the shipment. A number of the series that was given by hand to
// another shipment before the sequence reached it is passed over: the sequence moves on, so that one number keyed
// in advance cannot stop the series.
func (d Deps) HBL(ctx context.Context, tenant sea.TenantID, legalEntity, shipment, given string, at time.Time) (string, error) {
	held, ok, err := d.Register.OfShipment(ctx, tenant, legalEntity, shipment)
	if err != nil {
		return "", err
	}
	if ok {
		if given != "" && given != held {
			return "", fmt.Errorf("%w: %s holds %s", ErrHBLDiffers, shipment, held)
		}
		return held, nil
	}
	if given != "" {
		if !sea.ValidHBL(given) {
			return "", sea.ErrInvalidHBL
		}
		return given, d.Register.Claim(ctx, tenant, legalEntity, given, shipment, false, at)
	}
	s, _, err := d.Series.Read(ctx, tenant, legalEntity)
	if err != nil {
		return "", err
	}
	tmpl, err := numbering.Parse(s.State().Template)
	if err != nil {
		return "", fmt.Errorf("%w: %v", sea.ErrInvalidSeries, err)
	}
	for {
		seq, err := d.Numbers.Next(ctx, string(tenant), numbering.DocType(DocTypeHBL, legalEntity), tmpl.Period(at))
		if err != nil {
			return "", err
		}
		number, err := tmpl.Render(seq, at)
		switch {
		case errors.Is(err, numbering.ErrExhausted):
			return "", fmt.Errorf("%w: %v", sea.ErrSeriesExhausted, err)
		case err != nil:
			return "", err
		case !sea.ValidHBL(number):
			return "", fmt.Errorf("%w: the template renders %q", sea.ErrInvalidHBL, number)
		}
		// Looked up rather than claimed and caught: a unique violation would abort the transaction and with it the
		// sequence's step, and the next draw would render the same number again.
		_, taken, err := d.Register.Find(ctx, tenant, legalEntity, number)
		if err != nil {
			return "", err
		}
		if !taken {
			return number, d.Register.Claim(ctx, tenant, legalEntity, number, shipment, true, at)
		}
	}
}

// DocTypeHBL is the document type of HBL numbers in the platform's sequences (<type>/<legal entity>).
const DocTypeHBL = "HBL"

// RegisterSeries gives a legal entity its HBL numbering: a template of the platform's language in Cargo's subset
// whose numbers are HBL numbers (capital letters and digits, sixteen at most) and which does not reset (SEA does not
// know the issuing office's time zone, so a year or month in the number could follow UTC instead), and its first
// number, which seeds the sequence.
func (d Deps) RegisterSeries(ctx context.Context, c Command, legalEntity, template string, first int64) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	tmpl, err := numbering.Parse(template)
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: %v", sea.ErrInvalidSeries, err)
	}
	if tmpl.Resets() {
		return Receipt{}, fmt.Errorf("%w: %s restarts by date; an HBL template does not reset yet (the issuing office's time zone is not known)",
			sea.ErrInvalidSeries, template)
	}
	longest, err := tmpl.Longest()
	if err != nil || !sea.ValidHBL(longest) {
		return Receipt{}, fmt.Errorf("%w: %s renders %q; HBL numbers are capital letters and digits, sixteen at most", sea.ErrInvalidSeries, template, longest)
	}
	if _, err = tmpl.Render(first, c.OccurredAt); err != nil {
		return Receipt{}, fmt.Errorf("%w: first number %d: %v", sea.ErrInvalidSeries, first, err)
	}
	period := tmpl.Period(c.OccurredAt)
	return d.write(ctx, c, hash, "HblSeries", func(txCtx context.Context) (Receipt, []aggregate, error) {
		s, err := sea.RegisterSeries(c.Scope.TenantID, legalEntity, template, first, period, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		s.Stamp(entered)
		revision, err := d.Series.Insert(txCtx, s)
		if err != nil {
			return Receipt{}, nil, err
		}
		if err = d.Numbers.Seed(txCtx, string(c.Scope.TenantID), numbering.DocType(DocTypeHBL, legalEntity), period, first-1); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: legalEntity, NewRevision: revision}, []aggregate{s}, nil
	})
}
