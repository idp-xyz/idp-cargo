// Package book assembles the BOOK (Carrier Booking) bounded context, V0.1. Code outside BOOK uses it through
// package api; everything under internal/ is private to BOOK.
package book

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/book/api"
	orgacl "go.idp.xyz/idp-cargo/internal/contexts/book/internal/adapters/org"
	"go.idp.xyz/idp-cargo/internal/contexts/book/internal/adapters/postgres"
	refacl "go.idp.xyz/idp-cargo/internal/contexts/book/internal/adapters/reference"
	"go.idp.xyz/idp-cargo/internal/contexts/book/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/book/internal/domain"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
	refapi "go.idp.xyz/idp-cargo/internal/reference/api"
)

// Module wires BOOK's use cases to PostgreSQL through IDP-Bento-Go, to ORG's query side and to the location
// reference data.
type Module struct {
	db        *bentopg.DB
	outbox    *bentooutbox.Store
	carriers  application.Carriers
	auth      api.Authorizer
	clock     bento.SystemClock
	window    kernel.RecordWindow
	locations application.Locations
}

// Option configures a Module.
type Option func(*Module)

// WithRecordWindow sets how far a command's business time may lie before the time BOOK records it (T1).
func WithRecordWindow(w kernel.RecordWindow) Option { return func(m *Module) { m.window = w } }

// WithLocations gives BOOK the location reference data legs are resolved against (F2); without it no
// booking can be drafted.
func WithLocations(l refapi.Locations) Option {
	return func(m *Module) {
		if l != nil {
			m.locations = refacl.Locations{Ref: l}
		}
	}
}

var (
	_ api.Commands = (*Module)(nil)
	_ api.Queries  = (*Module)(nil)
)

// New assembles BOOK. org confirms carriers in their role (relationship 30). BOOK never queries its
// consumers: it is upstream of AIR, SEA and FUL (D1).
func New(db *bentopg.DB, org orgapi.Queries, auth api.Authorizer, opts ...Option) (*Module, error) {
	if db == nil || org == nil || auth == nil {
		return nil, errors.New("book: module needs a database, ORG queries and an authorizer")
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	m := &Module{db: db, outbox: outbox, carriers: orgacl.Carriers{ORG: org}, auth: auth, window: kernel.DefaultRecordWindow}
	for _, opt := range opts {
		opt(m)
	}
	if !m.window.Valid() {
		return nil, errors.New("book: the record window must not be negative")
	}
	return m, nil
}

func (m *Module) command(id, tenant, actor, action, booking string, expected int64, at time.Time, payload any) application.Command {
	return application.Command{CommandID: id, Scope: application.Scope{TenantID: domain.TenantID(tenant), ActorID: actor}, Action: application.Action(action),
		BookingID: domain.BookingID(booking), ExpectedRevision: expected, OccurredAt: stored(at), Payload: payload}
}

func (m *Module) DraftCarrierBooking(ctx context.Context, cmd api.DraftCarrierBooking) (api.Receipt, error) {
	c := m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionDraftCarrierBooking, cmd.BookingID, 0, cmd.OccurredAt, cmd)
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Draft(ctx, c, m.carriers, domain.Kind(cmd.Kind), cmd.LegalEntityID, cmd.CarrierPartyID,
		cmd.ConsolidationID, domain.BookingID(cmd.Rebooks), request(cmd.Request))
	return receipt(r, err)
}

func (m *Module) AmendBookingRequest(ctx context.Context, cmd api.AmendBookingRequest) (api.Receipt, error) {
	d := m.deps(cmd.TenantID, cmd.CommandID)
	var resolved domain.Request
	r, err := d.Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionAmendBookingRequest, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd),
		func(ctx context.Context, b *domain.Booking) (err error) {
			resolved, err = d.ResolveRequest(ctx, b.Kind(), request(cmd.Request))
			return err
		},
		func(b *domain.Booking) error {
			return b.AmendRequest(resolved, cmd.Reason, cmd.ActorID, stored(cmd.OccurredAt))
		})
	return receipt(r, err)
}

func (m *Module) RecordBookingRequestSent(ctx context.Context, cmd api.RecordBookingRequestSent) (api.Receipt, error) {
	return m.simple(ctx, cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordBookingRequestSent, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd,
		func(b *domain.Booking) error {
			return b.RecordSent(domain.Channel(cmd.Channel), evidence(cmd.Evidence), cmd.ActorID, stored(cmd.OccurredAt))
		})
}

func (m *Module) RecordCarrierAcknowledgement(ctx context.Context, cmd api.RecordCarrierAcknowledgement) (api.Receipt, error) {
	return m.simple(ctx, cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordCarrierAcknowledgement, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd,
		func(b *domain.Booking) error {
			return b.RecordAcknowledgement(answer(cmd.Answer), cmd.ActorID, stored(cmd.OccurredAt))
		})
}

func (m *Module) RecordCarrierConfirmation(ctx context.Context, cmd api.RecordCarrierConfirmation) (api.Receipt, error) {
	return m.granting(ctx, cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordCarrierConfirmation, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd,
		cmd.Lines, func(b *domain.Booking, g domain.Grant) error {
			return b.RecordConfirmation(answer(cmd.Answer), g, cmd.ActorID, stored(cmd.OccurredAt))
		})
}

func (m *Module) RecordCarrierRejection(ctx context.Context, cmd api.RecordCarrierRejection) (api.Receipt, error) {
	return m.simple(ctx, cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordCarrierRejection, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd,
		func(b *domain.Booking) error {
			return b.RecordRejection(answer(cmd.Answer), cmd.ActorID, stored(cmd.OccurredAt))
		})
}

func (m *Module) RecordCarrierChange(ctx context.Context, cmd api.RecordCarrierChange) (api.Receipt, error) {
	return m.granting(ctx, cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordCarrierChange, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd,
		cmd.Lines, func(b *domain.Booking, g domain.Grant) error {
			return b.RecordCarrierChange(domain.CapacityReason(cmd.Reason), answer(cmd.Answer), g, cmd.ActorID, stored(cmd.OccurredAt))
		})
}

func (m *Module) RequestBookingChange(ctx context.Context, cmd api.RequestBookingChange) (api.Receipt, error) {
	d := m.deps(cmd.TenantID, cmd.CommandID)
	var resolved domain.Request
	r, err := d.Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRequestBookingChange, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd),
		func(ctx context.Context, b *domain.Booking) (err error) {
			resolved, err = d.ResolveRequest(ctx, b.Kind(), request(cmd.Request))
			return err
		},
		func(b *domain.Booking) error {
			return b.RequestChange(resolved, cmd.Reason, domain.Channel(cmd.Channel), evidence(cmd.Evidence), cmd.ActorID, stored(cmd.OccurredAt))
		})
	return receipt(r, err)
}

func (m *Module) RecordChangeConfirmed(ctx context.Context, cmd api.RecordChangeConfirmed) (api.Receipt, error) {
	return m.granting(ctx, cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordChangeConfirmed, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd,
		cmd.Lines, func(b *domain.Booking, g domain.Grant) error {
			return b.RecordChangeConfirmed(answer(cmd.Answer), g, cmd.ActorID, stored(cmd.OccurredAt))
		})
}

func (m *Module) RecordChangeRefused(ctx context.Context, cmd api.RecordChangeRefused) (api.Receipt, error) {
	return m.simple(ctx, cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordChangeRefused, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd,
		func(b *domain.Booking) error {
			return b.RecordChangeRefused(answer(cmd.Answer), cmd.ActorID, stored(cmd.OccurredAt))
		})
}

func (m *Module) RecordCarrierDecline(ctx context.Context, cmd api.RecordCarrierDecline) (api.Receipt, error) {
	return m.simple(ctx, cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordCarrierDecline, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd,
		func(b *domain.Booking) error {
			return b.RecordDecline(answer(cmd.Answer), cmd.ActorID, stored(cmd.OccurredAt))
		})
}

func (m *Module) RequestBookingCancellation(ctx context.Context, cmd api.RequestBookingCancellation) (api.Receipt, error) {
	return m.simple(ctx, cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRequestBookingCancellation, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd,
		func(b *domain.Booking) error {
			return b.RequestCancellation(cmd.Reason, domain.Channel(cmd.Channel), evidence(cmd.Evidence), cmd.ActorID, stored(cmd.OccurredAt))
		})
}

func (m *Module) RecordCancellationConfirmed(ctx context.Context, cmd api.RecordCancellationConfirmed) (api.Receipt, error) {
	return m.simple(ctx, cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordCancellationConfirmed, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd,
		func(b *domain.Booking) error {
			return b.RecordCancellationConfirmed(answer(cmd.Answer), cmd.ActorID, stored(cmd.OccurredAt))
		})
}

func (m *Module) WithdrawDraftBooking(ctx context.Context, cmd api.WithdrawDraftBooking) (api.Receipt, error) {
	return m.simple(ctx, cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionWithdrawDraftBooking, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd,
		func(b *domain.Booking) error { return b.WithdrawDraft(cmd.Reason, cmd.ActorID, stored(cmd.OccurredAt)) })
}

func (m *Module) ReassignBookingCapacity(ctx context.Context, cmd api.ReassignBookingCapacity) (api.Receipt, error) {
	return m.simple(ctx, cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionReassignBookingCapacity, cmd.BookingID, cmd.ExpectedRevision, cmd.OccurredAt, cmd,
		func(b *domain.Booking) error {
			return b.Reassign(cmd.ConsolidationID, cmd.Reason, cmd.ActorID, stored(cmd.OccurredAt))
		})
}

func (m *Module) simple(ctx context.Context, id, tenant, actor, action, booking string, expected int64, at time.Time, payload any,
	apply func(*domain.Booking) error) (api.Receipt, error) {
	r, err := m.deps(tenant, id).Change(ctx, m.command(id, tenant, actor, action, booking, expected, at, payload), nil, apply)
	return receipt(r, err)
}

// granting runs a command whose carrier answer grants capacity: the lines' ports are resolved first (F2).
func (m *Module) granting(ctx context.Context, id, tenant, actor, action, booking string, expected int64, at time.Time, payload any,
	in []api.CapacityLine, apply func(*domain.Booking, domain.Grant) error) (api.Receipt, error) {
	d := m.deps(tenant, id)
	var g domain.Grant
	r, err := d.Change(ctx, m.command(id, tenant, actor, action, booking, expected, at, payload),
		func(ctx context.Context, b *domain.Booking) (err error) {
			g, err = d.ResolveGrant(ctx, b.Kind(), lines(in))
			return err
		},
		func(b *domain.Booking) error { return apply(b, g) })
	return receipt(r, err)
}

func (m *Module) GetCarrierBooking(ctx context.Context, q api.GetCarrierBooking) (api.CarrierBooking, error) {
	b, err := m.reads(q.TenantID).Get(ctx, domain.TenantID(q.TenantID), domain.BookingID(q.BookingID))
	if err != nil {
		return api.CarrierBooking{}, classify(err)
	}
	return view(b), nil
}

func (m *Module) ListCapacityForConsumer(ctx context.Context, q api.ListCapacityForConsumer) ([]api.CarrierBooking, error) {
	list, err := m.reads(q.TenantID).ForConsolidation(ctx, domain.TenantID(q.TenantID), q.ConsolidationID)
	if err != nil {
		return nil, classify(err)
	}
	out := make([]api.CarrierBooking, 0, len(list))
	for _, b := range list {
		out = append(out, view(b))
	}
	return out, nil
}

func (m *Module) FindBookingByCarrierReference(ctx context.Context, q api.FindBookingByCarrierReference) (api.CarrierBooking, error) {
	b, found, err := m.reads(q.TenantID).ByReference(ctx, domain.TenantID(q.TenantID), q.CarrierPartyID,
		domain.Reference{Type: domain.ReferenceType(q.Type), Value: q.Value})
	if err != nil {
		return api.CarrierBooking{}, classify(err)
	}
	if !found {
		return api.CarrierBooking{}, fmt.Errorf("%w: no live booking of %s holds %s %s", api.ErrNotFound, q.CarrierPartyID, q.Type, q.Value)
	}
	return view(b), nil
}

func (m *Module) ListBookingsAwaitingCarrier(ctx context.Context, q api.ListBookingsAwaitingCarrier) ([]api.BookingSummary, error) {
	list, err := m.reads(q.TenantID).Awaiting(ctx, domain.TenantID(q.TenantID), q.SentBefore, q.Limit)
	if err != nil {
		return nil, classify(err)
	}
	out := make([]api.BookingSummary, 0, len(list))
	for _, b := range list {
		st := b.Booking.State()
		out = append(out, api.BookingSummary{BookingID: string(st.BookingID), Kind: string(st.Kind), Status: string(st.Status),
			CarrierPartyID: st.Carrier.PartyID, ConsumerID: st.ConsumerID, PendingSince: b.Booking.PendingSince()})
	}
	return out, nil
}

func (m *Module) ListCutOffsDue(ctx context.Context, q api.ListCutOffsDue) ([]api.CutOffDue, error) {
	list, err := m.reads(q.TenantID).CutOffsDue(ctx, domain.TenantID(q.TenantID), q.From, q.Until, q.Limit)
	if err != nil {
		return nil, classify(err)
	}
	out := make([]api.CutOffDue, 0, len(list))
	for _, d := range list {
		out = append(out, api.CutOffDue{BookingID: string(d.BookingID), ConsumerID: d.ConsumerID, CapacityVersion: d.CapacityVersion, Line: d.Line,
			Leg: d.Leg, Flight: d.Flight, Voyage: d.Voyage, CutOff: cutOffView(d.CutOff)})
	}
	return out, nil
}

// view is CarrierBooking v1.
func view(r application.Booking) api.CarrierBooking {
	b := r.Booking
	st := b.State()
	req := b.Request()
	out := api.CarrierBooking{TenantID: string(st.TenantID), BookingID: string(st.BookingID), Revision: r.Revision, Kind: string(st.Kind),
		Status: string(st.Status), LegalEntityID: st.LegalEntityID, CarrierPartyID: st.Carrier.PartyID, CarrierPartyRevision: st.Carrier.PartyRevision,
		CarrierRole: st.Carrier.Role, CarrierIATADesignator: st.Carrier.IATADesignator, CarrierAWBPrefix: st.Carrier.AWBPrefix,
		ConsumerKind: st.ConsumerKind, Rebooks: string(st.Rebooks), MAWBNumber: b.MAWB(), AwaitingCarrierSince: b.PendingSince(),
		Request: api.RequestVersion{Version: req.No, ForwarderReference: req.ForwarderReference, Capacity: capacityView(req.Capacity),
			HandlingCodes: req.HandlingCodes, LocationEdition: req.LocationEdition, Reason: req.Reason, WrittenAt: req.At}}
	if !st.Status.Terminal() {
		out.ConsumerID = st.ConsumerID
	}
	for _, l := range req.Legs {
		out.Request.Legs = append(out.Request.Legs, api.ResolvedLeg{Loading: locView(l.Loading), Discharge: locView(l.Discharge), Flight: l.Flight,
			Vessel: l.Vessel, Voyage: l.Voyage, DepartureDate: l.DepartureDate})
	}
	if c, ok := b.Capacity(); ok {
		at, entered := b.ConfirmedAt()
		out.Capacity = api.CapacityVersion{Version: c.No, Reason: string(c.Reason), LocationEdition: c.LocationEdition, ConfirmedAt: at, RecordedAt: entered}
		for _, line := range c.Lines {
			lv := api.CapacityLineView{Line: line.No, Capacity: capacityView(line.Capacity), CarrierReference: line.CarrierReference}
			for _, g := range line.Legs {
				leg := api.CapacityLeg{Loading: locView(g.Loading), Discharge: locView(g.Discharge), Flight: g.Flight, Vessel: g.Vessel,
					VesselIMO: g.VesselIMO, Voyage: g.Voyage, ETD: g.ETD, ETA: g.ETA}
				for _, k := range g.CutOffs {
					leg.CutOffs = append(leg.CutOffs, cutOffView(k))
				}
				lv.Legs = append(lv.Legs, leg)
			}
			out.Capacity.Lines = append(out.Capacity.Lines, lv)
		}
		for i := 1; i < c.No; i++ {
			out.SupersededCapacity = append(out.SupersededCapacity, i)
		}
		out.ConfirmationLevel = api.VerificationOperatorEntered
	}
	if !st.Status.Terminal() {
		for _, ref := range b.References() {
			out.References = append(out.References, api.CarrierReference{Type: string(ref.Type), Value: ref.Value})
		}
	}
	return out
}

func locView(l domain.Location) api.ResolvedLocation {
	return api.ResolvedLocation{Code: l.Code, IATA: l.IATA}
}

func capacityView(c domain.Capacity) api.Capacity {
	out := api.Capacity{Pieces: c.Pieces, GrossWeight: c.GrossWeight, Volume: c.Volume}
	for _, e := range c.Equipment {
		out.Equipment = append(out.Equipment, api.Equipment{SizeType: e.SizeType, Count: e.Count})
	}
	return out
}

func cutOffView(c domain.CutOff) api.CutOff {
	return api.CutOff{Kind: string(c.Kind), At: c.At, TimeZone: c.Zone, Location: c.Location}
}

func request(r api.Request) domain.Request {
	out := domain.Request{ForwarderReference: r.ForwarderReference, Capacity: capacity(r.Capacity), HandlingCodes: slices.Clone(r.HandlingCodes)}
	for _, l := range r.Legs {
		out.Legs = append(out.Legs, domain.RequestedLeg{Loading: domain.Location{Code: l.Loading}, Discharge: domain.Location{Code: l.Discharge},
			Flight: l.Flight, Vessel: l.Vessel, Voyage: l.Voyage, DepartureDate: l.DepartureDate})
	}
	return out
}

func capacity(c api.Capacity) domain.Capacity {
	out := domain.Capacity{Pieces: c.Pieces, GrossWeight: c.GrossWeight, Volume: c.Volume}
	for _, e := range c.Equipment {
		out.Equipment = append(out.Equipment, domain.Equipment{SizeType: e.SizeType, Count: e.Count})
	}
	return out
}

func lines(in []api.CapacityLine) []domain.Line {
	var out []domain.Line
	for i, l := range in {
		line := domain.Line{No: i + 1, Capacity: capacity(l.Capacity), CarrierReference: l.CarrierReference}
		for _, g := range l.Legs {
			leg := domain.Leg{Loading: domain.Location{Code: g.Loading}, Discharge: domain.Location{Code: g.Discharge}, Flight: g.Flight, Vessel: g.Vessel,
				VesselIMO: g.VesselIMO, Voyage: g.Voyage, ETD: stored(g.ETD), ETA: stored(g.ETA)}
			for _, c := range g.CutOffs {
				leg.CutOffs = append(leg.CutOffs, domain.CutOff{Kind: domain.CutOffKind(c.Kind), At: stored(c.At), Zone: c.TimeZone, Location: c.Location})
			}
			line.Legs = append(line.Legs, leg)
		}
		out = append(out, line)
	}
	return out
}

func evidence(e api.Evidence) domain.Evidence {
	return domain.Evidence{SHA256: e.SHA256, StorageID: e.StorageID, MediaType: e.MediaType, CapturedAt: stored(e.CapturedAt)}
}

func answer(a api.Answer) domain.Answer {
	out := domain.Answer{Channel: domain.Channel(a.Channel), Evidence: evidence(a.Evidence), CarrierTime: stored(a.CarrierTime),
		ReasonCode: a.ReasonCode, ReasonText: a.ReasonText}
	for _, r := range a.References {
		out.References = append(out.References, domain.Reference{Type: domain.ReferenceType(r.Type), Value: r.Value})
	}
	return out
}

func (m *Module) reads(tenant string) application.Reads {
	store := postgres.BookingStore{DB: m.db}
	return application.Reads{Tx: bento.TenantTransactor{DB: m.db, Tenant: tenant}, Bookings: store, Lists: store}
}

func (m *Module) deps(tenant, commandID string) application.Deps {
	return application.Deps{
		Tx:        bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Bookings:  postgres.BookingStore{DB: m.db},
		Receipts:  postgres.CommandReceipts{DB: m.db},
		Outbox:    postgres.Outbox{Writer: m.outbox, CommandID: commandID, RecordedAt: m.clock.Now()},
		Auth:      authorizer{m.auth},
		Clock:     m.clock,
		Window:    m.window,
		Locations: m.locations,
	}
}

// stored truncates to the microsecond precision of timestamptz.
func stored(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Microsecond)
}

func receipt(r application.Receipt, err error) (api.Receipt, error) {
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	return api.Receipt{CommandID: r.CommandID, BookingID: string(r.BookingID), NewRevision: r.NewRevision}, nil
}

type authorizer struct{ api.Authorizer }

func (a authorizer) RequireBookingAction(ctx context.Context, s application.Scope, action application.Action, id domain.BookingID) error {
	return a.AuthorizeBookingAction(ctx, string(s.TenantID), s.ActorID, string(action), string(id))
}

// classify wraps known BOOK, kernel and Bento causes in their api error kind and passes anything else through.
func classify(err error) error {
	var kind error
	switch {
	case errors.Is(err, application.ErrUnauthenticated):
		kind = api.ErrUnauthenticated
	case errors.Is(err, application.ErrUnauthorized), errors.Is(err, bento.ErrTenantScopeConflict):
		kind = api.ErrUnauthorized
	case errors.Is(err, application.ErrIdempotencyConflict):
		kind = api.ErrIdempotencyConflict
	case errors.Is(err, domain.ErrConflict), errors.Is(err, bentorepo.ErrConflict), errors.Is(err, bentorepo.ErrAlreadyExists), postgres.Conflict(err):
		kind = api.ErrConflict
	case errors.Is(err, bentorepo.ErrNotFound):
		kind = api.ErrNotFound
	case errors.Is(err, domain.ErrInvalidBooking), errors.Is(err, domain.ErrInvalidRequest), errors.Is(err, domain.ErrInvalidCapacity),
		errors.Is(err, domain.ErrInvalidEvidence), errors.Is(err, domain.ErrInvalidReference), errors.Is(err, domain.ErrInvalidTransition),
		errors.Is(err, domain.ErrInvalidChange), errors.Is(err, domain.ErrCarrierCodes), errors.Is(err, domain.ErrExceedsRequest),
		errors.Is(err, domain.ErrMAWB), errors.Is(err, application.ErrInvalidQuery), errors.Is(err, application.ErrCarrierNotConfirmed),
		errors.Is(err, application.ErrLocationRefused), errors.Is(err, application.ErrReferenceInUse), errors.Is(err, kernel.ErrOutsideRecordWindow),
		errors.Is(err, kernel.ErrUnitMismatch):
		kind = api.ErrRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
