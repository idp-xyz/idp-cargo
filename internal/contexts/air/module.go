// Package air assembles the AIR (Air Freight) bounded context, V0.1 with V0.2's measured house facts and V0.3's
// facts finalized for DOC. Code outside AIR uses it through package api; everything under internal/ is private to
// AIR.
package air

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/air/api"
	bookacl "go.idp.xyz/idp-cargo/internal/contexts/air/internal/adapters/book"
	fulacl "go.idp.xyz/idp-cargo/internal/contexts/air/internal/adapters/ful"
	"go.idp.xyz/idp-cargo/internal/contexts/air/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/air/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/air/internal/domain"
	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/foundation/numbering"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Module wires AIR's use cases to PostgreSQL through IDP-Bento-Go, to BOOK's CarrierBooking v1 and to FUL's
// AirOperationPlan v1.
type Module struct {
	db       *bentopg.DB
	outbox   *bentooutbox.Store
	bookings application.Bookings
	plans    application.Plans
	auth     api.Authorizer
	clock    bento.SystemClock
	window   kernel.RecordWindow
	numbers  *numbering.Sequences
}

// Option configures a Module.
type Option func(*Module)

// WithRecordWindow sets how far a command's business time may lie before the time AIR records it (T1).
func WithRecordWindow(w kernel.RecordWindow) Option { return func(m *Module) { m.window = w } }

var (
	_ api.Commands = (*Module)(nil)
	_ api.Queries  = (*Module)(nil)
)

// New assembles AIR. book answers CarrierBooking v1 (relationship 5) and ful AirOperationPlan v1 (relationship 9);
// AIR writes to neither.
func New(db *bentopg.DB, book bookapi.Queries, ful fulapi.AirOperationPlanQueries, auth api.Authorizer, opts ...Option) (*Module, error) {
	if db == nil || book == nil || ful == nil || auth == nil {
		return nil, errors.New("air: module needs a database, BOOK's bookings, FUL's plans and an authorizer")
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	numbers, err := numbering.NewSequences(db, "cargo_air")
	if err != nil {
		return nil, err
	}
	m := &Module{db: db, outbox: outbox, bookings: bookacl.Bookings{BOOK: book}, plans: fulacl.Plans{FUL: ful}, auth: auth, window: kernel.DefaultRecordWindow,
		numbers: numbers}
	for _, opt := range opts {
		opt(m)
	}
	if !m.window.Valid() {
		return nil, errors.New("air: the record window must not be negative")
	}
	return m, nil
}

func (m *Module) command(id, tenant, actor, action, subject string, expected int64, at time.Time, payload any) application.Command {
	return application.Command{CommandID: id, Scope: application.Scope{TenantID: domain.TenantID(tenant), ActorID: actor}, Action: application.Action(action),
		Subject: subject, ExpectedRevision: expected, OccurredAt: stored(at), Payload: payload}
}

func (m *Module) OpenAirMovement(ctx context.Context, cmd api.OpenAirMovement) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Open(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionOpenAirMovement, cmd.MovementID,
		0, cmd.OccurredAt, cmd), cmd.ConsolidationID, cmd.BookingID)
	return receipt(r, err)
}

func (m *Module) LinkCarrierBooking(ctx context.Context, cmd api.LinkCarrierBooking) (api.Receipt, error) {
	d := m.deps(cmd.TenantID, cmd.CommandID)
	var b domain.Booking
	r, err := d.Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionLinkCarrierBooking, cmd.MovementID, cmd.ExpectedRevision, cmd.OccurredAt, cmd),
		func(ctx context.Context, mv *domain.Movement) (err error) {
			b, err = d.Bookings.Booking(ctx, domain.TenantID(cmd.TenantID), mv.Current().BookingID)
			return err
		},
		func(_ context.Context, mv *domain.Movement) (int, string, error) {
			return 0, "", mv.LinkBooking(b, cmd.ActorID, stored(cmd.OccurredAt))
		})
	return receipt(r, err)
}

func (m *Module) RegisterHawbSeries(ctx context.Context, cmd api.RegisterHawbSeries) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RegisterSeries(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRegisterHawbSeries,
		cmd.LegalEntityID, 0, cmd.OccurredAt, cmd), cmd.LegalEntityID, cmd.Template, cmd.FirstNumber)
	return receipt(r, err)
}

func (m *Module) AssociateHouseWaybill(ctx context.Context, cmd api.AssociateHouseWaybill) (api.Receipt, error) {
	d := m.deps(cmd.TenantID, cmd.CommandID)
	var p domain.Plan
	r, err := d.Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionAssociateHouseWaybill, cmd.MovementID, cmd.ExpectedRevision, cmd.OccurredAt, cmd),
		m.plan(d, cmd.TenantID, cmd.ShipmentID, &p),
		func(txCtx context.Context, mv *domain.Movement) (int, string, error) {
			at := stored(cmd.OccurredAt)
			hawb, err := d.HAWB(txCtx, domain.TenantID(cmd.TenantID), mv.State().LegalEntityID, cmd.ShipmentID, cmd.HAWB, at)
			if err != nil {
				return 0, "", err
			}
			if err = mv.Associate(p, hawb, cmd.ActorID, at); err != nil {
				return 0, "", err
			}
			return latest(mv), hawb, nil
		})
	return receipt(r, err)
}

func (m *Module) RefreshHouseWaybillFacts(ctx context.Context, cmd api.RefreshHouseWaybillFacts) (api.Receipt, error) {
	d := m.deps(cmd.TenantID, cmd.CommandID)
	var p domain.Plan
	r, err := d.Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRefreshHouseWaybillFacts, cmd.MovementID, cmd.ExpectedRevision, cmd.OccurredAt, cmd),
		m.plan(d, cmd.TenantID, cmd.ShipmentID, &p),
		func(_ context.Context, mv *domain.Movement) (int, string, error) {
			if err := mv.Refresh(p, cmd.ActorID, stored(cmd.OccurredAt)); err != nil {
				return 0, "", err
			}
			e := mv.State().Entries[latest(mv)-1]
			return e.No, e.HAWB, nil
		})
	return receipt(r, err)
}

func (m *Module) EndHouseWaybillAssociation(ctx context.Context, cmd api.EndHouseWaybillAssociation) (api.Receipt, error) {
	d := m.deps(cmd.TenantID, cmd.CommandID)
	var p domain.Plan
	r, err := d.Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionEndHouseWaybillAssociation, cmd.MovementID, cmd.ExpectedRevision, cmd.OccurredAt, cmd),
		m.plan(d, cmd.TenantID, cmd.ShipmentID, &p),
		func(_ context.Context, mv *domain.Movement) (int, string, error) {
			if err := mv.EndAssociation(p, cmd.ActorID, stored(cmd.OccurredAt)); err != nil {
				return 0, "", err
			}
			_, _, ended := mv.Added()
			for no, e := range ended {
				return no, e.HAWB, nil
			}
			return 0, "", nil
		})
	return receipt(r, err)
}

func (m *Module) FinalizeAirDocFacts(ctx context.Context, cmd api.FinalizeAirDocFacts) (api.Receipt, error) {
	d := m.deps(cmd.TenantID, cmd.CommandID)
	var plans []domain.Plan
	r, err := d.Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionFinalizeAirDocFacts, cmd.MovementID, cmd.ExpectedRevision,
		cmd.OccurredAt, cmd),
		func(ctx context.Context, mv *domain.Movement) error {
			for _, h := range mv.Houses() { // every live house's plan, before AIR's transaction (T3)
				p, err := d.Plans.Plan(ctx, domain.TenantID(cmd.TenantID), h.ShipmentID)
				if err != nil {
					return err
				}
				plans = append(plans, p)
			}
			return nil
		},
		func(_ context.Context, mv *domain.Movement) (int, string, error) {
			return 0, "", mv.Finalize(plans, cmd.ActorID, stored(cmd.OccurredAt))
		})
	return receipt(r, err)
}

func (m *Module) CancelAirMovement(ctx context.Context, cmd api.CancelAirMovement) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionCancelAirMovement, cmd.MovementID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), nil,
		func(_ context.Context, mv *domain.Movement) (int, string, error) {
			return 0, "", mv.Cancel(cmd.Reason, cmd.ActorID, stored(cmd.OccurredAt))
		})
	return receipt(r, err)
}

// plan reads a house's AirOperationPlan v1 before AIR's transaction (T3).
func (m *Module) plan(d application.Deps, tenant, shipment string, into *domain.Plan) func(context.Context, *domain.Movement) error {
	return func(ctx context.Context, _ *domain.Movement) (err error) {
		*into, err = d.Plans.Plan(ctx, domain.TenantID(tenant), shipment)
		return err
	}
}

func latest(mv *domain.Movement) int { return len(mv.State().Entries) }

func (m *Module) GetAirMovement(ctx context.Context, q api.GetAirMovement) (api.AirMovement, error) {
	mv, revision, err := m.reads(q.TenantID).Movement(ctx, domain.TenantID(q.TenantID), domain.MovementID(q.MovementID))
	if err != nil {
		return api.AirMovement{}, classify(err)
	}
	return view(mv, revision), nil
}

func (m *Module) FindAirMovementByMAWB(ctx context.Context, q api.FindAirMovementByMAWB) (api.AirMovement, error) {
	mv, revision, err := m.reads(q.TenantID).ByMAWB(ctx, domain.TenantID(q.TenantID), q.MAWB)
	if err != nil {
		return api.AirMovement{}, classify(err)
	}
	return view(mv, revision), nil
}

func (m *Module) FindHouseWaybill(ctx context.Context, q api.FindHouseWaybill) (api.HouseWaybillRef, error) {
	reg, err := m.reads(q.TenantID).HouseWaybill(ctx, domain.TenantID(q.TenantID), q.LegalEntityID, q.HAWB)
	if err != nil {
		return api.HouseWaybillRef{}, classify(err)
	}
	return api.HouseWaybillRef{LegalEntityID: q.LegalEntityID, HAWB: reg.HAWB, ShipmentID: reg.ShipmentID, MovementID: string(reg.MovementID), EntryNo: reg.EntryNo}, nil
}

func (m *Module) GetAirDocFacts(ctx context.Context, q api.GetAirDocFacts) (api.AirDocFacts, error) {
	if q.Version < 0 {
		return api.AirDocFacts{}, classify(application.ErrInvalidQuery)
	}
	mv, _, err := m.reads(q.TenantID).Movement(ctx, domain.TenantID(q.TenantID), domain.MovementID(q.MovementID))
	if err != nil {
		return api.AirDocFacts{}, classify(err)
	}
	v, current, ok := mv.LatestDocFacts()
	if q.Version > 0 {
		v, current, ok = mv.DocFactsVersion(q.Version)
	}
	if !ok {
		return api.AirDocFacts{}, fmt.Errorf("%w: movement %s has no AirDocFacts version %d", api.ErrNotFound, q.MovementID, q.Version)
	}
	return docFactsView(q.TenantID, v, current), nil
}

func docAirport(a domain.DocAirport) api.Airport {
	return api.Airport{Code: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: a.Code}, IATA: a.IATA}
}

func grams(g int64) kernel.Quantity { return kernel.Quantity{Unit: "GRM", Amount: g} }

func cubicCentimetres(v int64) kernel.Quantity {
	if v == 0 {
		return kernel.Quantity{}
	}
	return kernel.Quantity{Unit: "CMQ", Amount: v}
}

func screenings(in []domain.Screening) []api.Screening {
	var out []api.Screening
	for _, s := range in {
		out = append(out, api.Screening{LotID: s.LotID, Outcome: s.Outcome, Screener: s.Screener, Method: s.Method, Reference: s.Reference,
			ScreenedAt: s.ScreenedAt})
	}
	return out
}

func docFactsView(tenant string, v domain.DocFactsVersion, current bool) api.AirDocFacts {
	f := v.Facts
	out := api.AirDocFacts{TenantID: tenant, MovementID: f.MovementID, Version: v.No, Status: api.DocFactsSuperseded,
		Content: append([]byte(nil), v.Content...), SHA256: v.SHA256,
		FinalizedBy: v.By, FinalizedAt: v.At, ConsolidationID: f.ConsolidationID, OperatingLegalEntityID: f.OperatingLegalEntityID,
		AirlinePartyID: f.Airline.PartyID, AirlinePartyRevision: f.Airline.PartyRevision, AirlineDesignator: f.Airline.Designator,
		AWBPrefix: f.Airline.AWBPrefix, MAWB: f.MAWB, MAWBSource: f.MAWBSource, BookingID: f.BookingID, CapacityVersion: f.CapacityVersion,
		Origin: docAirport(f.Origin), Destination: docAirport(f.Destination), LocationEdition: f.LocationEdition, Pieces: f.Pieces,
		GrossWeight: grams(f.GrossWeightGrams), Volume: cubicCentimetres(f.VolumeCubicCentimetres), ChargeableWeight: grams(f.ChargeableGrams),
		ChargeableRule: f.ChargeableRule, SecurityStatus: f.SecurityStatus}
	if current {
		out.Status = api.DocFactsFinal
	}
	for _, s := range f.Flights {
		out.Flights = append(out.Flights, api.DocFlight{Flight: s.Flight, From: docAirport(s.From), To: docAirport(s.To), ETD: s.ETD, ETA: s.ETA})
	}
	for _, h := range f.Houses {
		d := api.DocHouse{ShipmentID: h.ShipmentID, HAWB: h.HAWB, PlanVersion: h.PlanVersion, EvidenceVersion: h.EvidenceVersion, Basis: h.Basis,
			Pieces: h.Pieces, GrossWeight: grams(h.GrossWeightGrams), Volume: cubicCentimetres(h.VolumeCubicCentimetres),
			ChargeableWeight: grams(h.ChargeableGrams), SecurityStatus: h.SecurityStatus, Screenings: screenings(h.Screenings)}
		for _, p := range h.Parts {
			d.Parts = append(d.Parts, api.DocPart{ObligationID: p.ObligationID, ObligationVersion: p.ObligationVersion, OrderID: p.OrderID, Packs: p.Packs,
				PackageType: p.PackageType, GoodsDescription: p.GoodsDescription})
		}
		out.Houses = append(out.Houses, d)
	}
	return out
}

func (m *Module) GetHawbSeries(ctx context.Context, q api.GetHawbSeries) (api.HawbSeries, error) {
	s, revision, err := m.reads(q.TenantID).SeriesOf(ctx, domain.TenantID(q.TenantID), q.LegalEntityID)
	if err != nil {
		if errors.Is(err, application.ErrNoSeries) {
			return api.HawbSeries{}, fmt.Errorf("%w: %w", api.ErrNotFound, err)
		}
		return api.HawbSeries{}, classify(err)
	}
	st := s.State()
	out := api.HawbSeries{LegalEntityID: st.LegalEntityID, Template: st.Template, FirstNumber: st.FirstNumber, Revision: revision}
	var last map[string]int64
	if err = (bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}).WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		last, err = m.numbers.Last(txCtx, q.TenantID, numbering.DocType(application.DocTypeHAWB, q.LegalEntityID))
		return err
	}); err != nil {
		return api.HawbSeries{}, classify(err)
	}
	for period, n := range last {
		out.Last = append(out.Last, api.NumberPeriod{Period: period, Last: n})
	}
	slices.SortFunc(out.Last, func(a, b api.NumberPeriod) int { return strings.Compare(a.Period, b.Period) })
	return out, nil
}

func view(mv *domain.Movement, revision int64) api.AirMovement {
	st := mv.State()
	current := mv.Current()
	out := api.AirMovement{TenantID: string(st.TenantID), MovementID: string(st.MovementID), Revision: revision, State: string(st.State),
		ConsolidationID: st.ConsolidationID, OperatingLegalEntityID: st.LegalEntityID, AirlinePartyID: st.Airline.PartyID,
		AirlinePartyRevision: st.Airline.PartyRevision, AirlineDesignator: st.Airline.Designator, AWBPrefix: st.Airline.AWBPrefix,
		Origin: api.Airport{Code: st.Origin.Code, IATA: st.Origin.IATA}, Destination: api.Airport{Code: st.Destination.Code, IATA: st.Destination.IATA},
		LocationEdition: st.Edition, MAWB: st.MAWB.Number, MAWBStatus: "ASSIGNED", BookingID: current.BookingID, CapacityVersion: current.CapacityVersion,
		DocFactsVersion: len(st.DocFacts)}
	if st.MAWB.Voided {
		out.MAWBStatus = "VOIDED"
	}
	for i := len(st.Links) - 1; i >= 0; i-- {
		l := st.Links[i]
		for n, s := range l.Segments {
			out.Flights = append(out.Flights, api.FlightSegment{Segment: n + 1, Flight: s.Flight, From: api.Airport{Code: s.From.Code, IATA: s.From.IATA},
				To: api.Airport{Code: s.To.Code, IATA: s.To.IATA}, ETD: s.ETD, ETA: s.ETA, BookingID: l.BookingID, CapacityVersion: l.CapacityVersion,
				Current: i == len(st.Links)-1})
		}
	}
	for _, e := range mv.Houses() {
		h := api.House{EntryNo: e.No, ShipmentID: e.ShipmentID, HAWB: e.HAWB, FULRevision: e.FULRevision, PlanVersion: e.PlanVersion, Pieces: e.Facts.Pieces,
			GrossWeight: e.Facts.GrossWeight, Volume: e.Facts.Volume, ChargeableWeight: kernel.Quantity{Unit: "GRM", Amount: e.Facts.ChargeableGrams},
			ChargeableRule: e.Facts.Rule, Basis: e.Facts.Basis, EvidenceVersion: e.Facts.Evidence, SecurityStatus: e.Facts.Security,
			Screenings: screenings(e.Facts.Screenings), Start: e.Start, AssociatedAt: e.At, WindowDrift: mv.WindowDrift(e)}
		for _, p := range e.Facts.Parts {
			h.Parts = append(h.Parts, api.HousePart{ObligationID: p.ObligationID, ObligationVersion: p.ObligationVersion, OrderID: p.OrderID, Packs: p.Packs,
				PackageType: p.PackageType, GrossWeight: p.GrossWeight, Volume: p.Volume, GoodsDescription: p.GoodsDescription})
		}
		out.Houses = append(out.Houses, h)
	}
	pieces, grams, cm3, chargeable := mv.Totals()
	out.Pieces, out.GrossWeight = pieces, kernel.Quantity{Unit: "GRM", Amount: grams}
	if cm3 > 0 {
		out.Volume = kernel.Quantity{Unit: "CMQ", Amount: cm3}
	}
	if chargeable > 0 {
		out.ChargeableWeight = kernel.Quantity{Unit: "GRM", Amount: chargeable}
	}
	return out
}

func (m *Module) reads(tenant string) application.Reads {
	return application.Reads{Tx: bento.TenantTransactor{DB: m.db, Tenant: tenant}, Movements: postgres.MovementStore{DB: m.db},
		Series: postgres.SeriesStore{DB: m.db}, Numbers: m.numbers, Register: postgres.Register{DB: m.db}}
}

func (m *Module) deps(tenant, commandID string) application.Deps {
	return application.Deps{
		Tx:        bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Movements: postgres.MovementStore{DB: m.db},
		Series:    postgres.SeriesStore{DB: m.db},
		Numbers:   m.numbers,
		Register:  postgres.Register{DB: m.db},
		Receipts:  postgres.CommandReceipts{DB: m.db},
		Outbox:    postgres.Outbox{Writer: m.outbox, CommandID: commandID, RecordedAt: m.clock.Now()},
		Auth:      authorizer{m.auth},
		Clock:     m.clock,
		Window:    m.window,
		Bookings:  m.bookings,
		Plans:     m.plans,
	}
}

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
	return api.Receipt{CommandID: r.CommandID, MovementID: r.SubjectID, NewRevision: r.NewRevision, EntryNo: r.EntryNo, HAWB: r.HAWB,
		FactsVersion: r.FactsVersion}, nil
}

type authorizer struct{ api.Authorizer }

func (a authorizer) RequireAirAction(ctx context.Context, s application.Scope, action application.Action, subject string) error {
	return a.AuthorizeAirAction(ctx, string(s.TenantID), s.ActorID, string(action), subject)
}

// classify wraps known AIR, kernel and Bento causes in their api error kind and passes anything else through.
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
	case errors.Is(err, domain.ErrInvalidMovement), errors.Is(err, domain.ErrInvalidBooking), errors.Is(err, domain.ErrInvalidTransition),
		errors.Is(err, domain.ErrInvalidChange), errors.Is(err, domain.ErrNotAttached), errors.Is(err, domain.ErrAlreadyAssociated),
		errors.Is(err, domain.ErrNotAssociated), errors.Is(err, domain.ErrStillAttached), errors.Is(err, domain.ErrNothingToRefresh),
		errors.Is(err, domain.ErrHousesLive), errors.Is(err, domain.ErrNotFinalizable), errors.Is(err, domain.ErrInvalidHAWB), errors.Is(err, domain.ErrUnits), errors.Is(err, domain.ErrInvalidSeries),
		errors.Is(err, domain.ErrSeriesExhausted), errors.Is(err, application.ErrInvalidQuery), errors.Is(err, application.ErrUnknownBooking),
		errors.Is(err, application.ErrUnknownShipment), errors.Is(err, application.ErrNoSeries), errors.Is(err, application.ErrHAWBInUse),
		errors.Is(err, application.ErrHAWBDiffers), errors.Is(err, application.ErrMAWBInUse), errors.Is(err, kernel.ErrOutsideRecordWindow):
		kind = api.ErrRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
