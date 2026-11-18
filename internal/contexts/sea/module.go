// Package sea assembles the SEA (Sea Freight) bounded context: ocean movements, master bill references, house bills
// and container visits (V0.2, without VGM). Code outside SEA uses it through package api; everything under internal/
// is private to SEA.
package sea

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	bookacl "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/adapters/book"
	fulacl "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/adapters/ful"
	seahttp "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/adapters/http"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/foundation/numbering"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Module wires SEA's use cases to PostgreSQL through IDP-Bento-Go, to BOOK's CarrierBooking v1 and to FUL's
// OceanOperationPlan v1.
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

// WithRecordWindow sets how far a command's business time may lie before the time SEA records it (T1).
func WithRecordWindow(w kernel.RecordWindow) Option { return func(m *Module) { m.window = w } }

var (
	_ api.Commands   = (*Module)(nil)
	_ api.Queries    = (*Module)(nil)
	_ api.VgmQueries = (*Module)(nil)
)

// RegisterHTTP registers SEA's inbound HTTP routes; the composition root calls it.
func (m *Module) RegisterHTTP(mux *http.ServeMux) { seahttp.Register(mux, m) }

// New assembles SEA. book answers CarrierBooking v1 (relationship 6) and ful OceanOperationPlan v1 (relationship
// 10); SEA writes to neither.
func New(db *bentopg.DB, book bookapi.Queries, ful fulapi.OceanOperationPlanQueries, auth api.Authorizer, opts ...Option) (*Module, error) {
	if db == nil || book == nil || ful == nil || auth == nil {
		return nil, errors.New("sea: module needs a database, BOOK's bookings, FUL's plans and an authorizer")
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	numbers, err := numbering.NewSequences(db, "cargo_sea")
	if err != nil {
		return nil, err
	}
	m := &Module{db: db, outbox: outbox, bookings: bookacl.Bookings{BOOK: book}, plans: fulacl.Plans{FUL: ful}, auth: auth, window: kernel.DefaultRecordWindow,
		numbers: numbers}
	for _, opt := range opts {
		opt(m)
	}
	if !m.window.Valid() {
		return nil, errors.New("sea: the record window must not be negative")
	}
	return m, nil
}

func (m *Module) command(id, tenant, actor, action, subject string, expected int64, at time.Time, payload any) application.Command {
	return application.Command{CommandID: id, Scope: application.Scope{TenantID: domain.TenantID(tenant), ActorID: actor}, Action: application.Action(action),
		Subject: subject, ExpectedRevision: expected, OccurredAt: stored(at), Payload: payload}
}

func (m *Module) OpenOceanMovement(ctx context.Context, cmd api.OpenOceanMovement) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Open(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionOpenOceanMovement, cmd.MovementID,
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
		func(_ context.Context, mv *domain.Movement) (application.Result, error) {
			return application.Result{}, mv.LinkBooking(b, cmd.ActorID, stored(cmd.OccurredAt))
		})
	return receipt(r, err)
}

func (m *Module) RecordMasterBill(ctx context.Context, cmd api.RecordMasterBill) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordMasterBill, cmd.MovementID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), nil,
		func(_ context.Context, mv *domain.Movement) (application.Result, error) {
			evidence := domain.Evidence{SHA256: cmd.Evidence.SHA256, StorageID: cmd.Evidence.StorageID, MediaType: cmd.Evidence.MediaType,
				CapturedAt: stored(cmd.Evidence.CapturedAt)}
			if err := mv.RecordMasterBill(domain.NormalizeBillNumber(cmd.Number), cmd.Kind, evidence, cmd.ActorID, stored(cmd.OccurredAt)); err != nil {
				return application.Result{}, err
			}
			return application.Result{BillNo: len(mv.State().Bills)}, nil
		})
	return receipt(r, err)
}

func (m *Module) RegisterHblSeries(ctx context.Context, cmd api.RegisterHblSeries) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RegisterSeries(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRegisterHblSeries,
		cmd.LegalEntityID, 0, cmd.OccurredAt, cmd), cmd.LegalEntityID, cmd.Template, cmd.FirstNumber)
	return receipt(r, err)
}

func (m *Module) AssociateHouseBill(ctx context.Context, cmd api.AssociateHouseBill) (api.Receipt, error) {
	d := m.deps(cmd.TenantID, cmd.CommandID)
	var p domain.Plan
	r, err := d.Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionAssociateHouseBill, cmd.MovementID, cmd.ExpectedRevision, cmd.OccurredAt, cmd),
		m.plan(d, cmd.TenantID, cmd.ShipmentID, &p),
		func(txCtx context.Context, mv *domain.Movement) (application.Result, error) {
			at := stored(cmd.OccurredAt)
			hbl, err := d.HBL(txCtx, domain.TenantID(cmd.TenantID), mv.State().LegalEntityID, cmd.ShipmentID, cmd.HBL, at)
			if err != nil {
				return application.Result{}, err
			}
			if err = mv.Associate(p, hbl, cmd.ActorID, at); err != nil {
				return application.Result{}, err
			}
			return application.Result{EntryNo: latest(mv), HBL: hbl}, nil
		})
	return receipt(r, err)
}

func (m *Module) RefreshHouseBillFacts(ctx context.Context, cmd api.RefreshHouseBillFacts) (api.Receipt, error) {
	d := m.deps(cmd.TenantID, cmd.CommandID)
	var p domain.Plan
	r, err := d.Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRefreshHouseBillFacts, cmd.MovementID, cmd.ExpectedRevision, cmd.OccurredAt, cmd),
		m.plan(d, cmd.TenantID, cmd.ShipmentID, &p),
		func(_ context.Context, mv *domain.Movement) (application.Result, error) {
			if err := mv.Refresh(p, cmd.ActorID, stored(cmd.OccurredAt)); err != nil {
				return application.Result{}, err
			}
			e := mv.State().Entries[latest(mv)-1]
			return application.Result{EntryNo: e.No, HBL: e.HBL}, nil
		})
	return receipt(r, err)
}

func (m *Module) EndHouseBillAssociation(ctx context.Context, cmd api.EndHouseBillAssociation) (api.Receipt, error) {
	d := m.deps(cmd.TenantID, cmd.CommandID)
	var p domain.Plan
	r, err := d.Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionEndHouseBillAssociation, cmd.MovementID, cmd.ExpectedRevision, cmd.OccurredAt, cmd),
		m.plan(d, cmd.TenantID, cmd.ShipmentID, &p),
		func(_ context.Context, mv *domain.Movement) (application.Result, error) {
			if err := mv.EndAssociation(p, cmd.ActorID, stored(cmd.OccurredAt)); err != nil {
				return application.Result{}, err
			}
			_, ended := mv.Changed()
			for no, e := range ended {
				return application.Result{EntryNo: no, HBL: e.HBL}, nil
			}
			return application.Result{}, nil
		})
	return receipt(r, err)
}

func (m *Module) FinalizeOceanDocFacts(ctx context.Context, cmd api.FinalizeOceanDocFacts) (api.Receipt, error) {
	d := m.deps(cmd.TenantID, cmd.CommandID)
	var plans []domain.Plan
	r, err := d.Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionFinalizeOceanDocFacts, cmd.MovementID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), func(ctx context.Context, mv *domain.Movement) error {
		plans = nil
		for _, h := range mv.Houses() {
			p, err := d.Plans.Plan(ctx, domain.TenantID(cmd.TenantID), h.ShipmentID)
			if err != nil {
				return err
			}
			plans = append(plans, p)
		}
		return nil
	}, func(txCtx context.Context, mv *domain.Movement) (application.Result, error) {
		if d.Visits == nil {
			return application.Result{}, application.ErrMissingDependency
		}
		ids, err := d.Visits.OfMovement(txCtx, domain.TenantID(cmd.TenantID), mv.ID())
		if err != nil {
			return application.Result{}, err
		}
		var visits []domain.VisitState
		for _, id := range ids {
			v, _, err := d.Visits.Read(txCtx, domain.TenantID(cmd.TenantID), id)
			if err != nil {
				return application.Result{}, err
			}
			visits = append(visits, v.State())
		}
		if err = mv.Finalize(plans, visits, cmd.ActorID, stored(cmd.OccurredAt)); err != nil {
			return application.Result{}, err
		}
		return application.Result{FactsVersion: len(mv.State().DocFacts)}, nil
	})
	return receipt(r, err)
}

func (m *Module) GetOceanDocFacts(ctx context.Context, q api.GetOceanDocFacts) (api.OceanDocFacts, error) {
	read, err := m.reads(q.TenantID).Movement(ctx, domain.TenantID(q.TenantID), domain.MovementID(q.MovementID))
	mv := read.Movement
	if err != nil {
		return api.OceanDocFacts{}, classify(err)
	}
	v, current, ok := mv.DocFactsVersion(q.Version)
	if !ok {
		return api.OceanDocFacts{}, api.ErrNotFound
	}
	var out api.OceanDocFacts
	if err = json.Unmarshal(v.Content, &out); err != nil {
		return api.OceanDocFacts{}, err
	}
	out.Content, out.SHA256, out.FinalizedBy, out.FinalizedAt = slices.Clone(v.Content), v.SHA256, v.By, v.At
	out.Status = api.DocFactsSuperseded
	if current {
		out.Status = api.DocFactsFinal
	}
	return out, nil
}

func (m *Module) RecordOceanCarrierFact(ctx context.Context, cmd api.RecordOceanCarrierFact) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordCarrierFact(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordOceanCarrierFact, cmd.MovementID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), domain.CarrierFact{Kind: cmd.Kind, ContainerNumber: cmd.ContainerNumber, BillNumber: cmd.BillNumber,
		Vessel: cmd.Vessel, Voyage: cmd.Voyage, Source: cmd.Source, Reference: cmd.Reference, SHA256: cmd.SHA256, OccurredAt: stored(cmd.HappenedAt)})
	return receipt(r, err)
}

func (m *Module) CancelOceanMovement(ctx context.Context, cmd api.CancelOceanMovement) (api.Receipt, error) {
	d := m.deps(cmd.TenantID, cmd.CommandID)
	r, err := d.Change(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionCancelOceanMovement, cmd.MovementID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), nil,
		func(txCtx context.Context, mv *domain.Movement) (application.Result, error) {
			at := stored(cmd.OccurredAt)
			live, last, err := d.VisitsHeld(txCtx, domain.TenantID(cmd.TenantID), mv.ID())
			switch {
			case err != nil:
				return application.Result{}, err
			case live > 0:
				return application.Result{}, fmt.Errorf("%w: %d", application.ErrContainersAssigned, live)
			case at.Before(last):
				return application.Result{}, fmt.Errorf("%w: a movement is cancelled not before its last container was released", domain.ErrInvalidChange)
			}
			return application.Result{}, mv.Cancel(cmd.Reason, cmd.ActorID, at)
		})
	return receipt(r, err)
}

// code is a number or code as SEA keeps it: without the spaces around it, in capital letters.
func code(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

func (m *Module) AssignContainer(ctx context.Context, cmd api.AssignContainer) (api.Receipt, error) {
	var plate domain.Plate
	if cmd.TareKg != 0 || cmd.MaxGrossKg != 0 || cmd.PlateSource != "" {
		plate = domain.Plate{TareKg: cmd.TareKg, MaxGrossKg: cmd.MaxGrossKg, Source: cmd.PlateSource}
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).AssignContainer(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionAssignContainer, cmd.VisitID,
		0, cmd.OccurredAt, cmd), domain.MovementID(cmd.MovementID), cmd.LoadUnitNo, code(cmd.ContainerNumber), code(cmd.SizeType), plate)
	return receipt(r, err)
}

func (m *Module) RecordContainerSeal(ctx context.Context, cmd api.RecordContainerSeal) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ChangeVisit(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordContainerSeal, cmd.VisitID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), func(_ context.Context, v *domain.Visit) (int, error) {
		if err := v.RecordSeal(code(cmd.Number), cmd.Party, stored(cmd.AppliedAt), cmd.Source, cmd.ActorID, stored(cmd.OccurredAt)); err != nil {
			return 0, err
		}
		return v.Seal().No, nil
	})
	return receipt(r, err)
}

func (m *Module) RecordStuffingDone(ctx context.Context, cmd api.RecordStuffingDone) (api.Receipt, error) {
	d := m.deps(cmd.TenantID, cmd.CommandID)
	r, err := d.ChangeVisit(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordStuffingDone, cmd.VisitID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), func(txCtx context.Context, v *domain.Visit) (int, error) {
		mv, err := d.Movement(txCtx, domain.TenantID(cmd.TenantID), v.State().MovementID)
		if err != nil {
			return 0, err
		}
		return 0, v.StuffingDone(mv, cmd.DifferenceReason, cmd.ActorID, stored(cmd.OccurredAt))
	})
	return receipt(r, err)
}

func (m *Module) AssertVgm(ctx context.Context, cmd api.AssertVgm) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).AssertVGM(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionAssertVgm, cmd.VisitID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Mass, cmd.Method, cmd.ResponsiblePartyID, cmd.ResponsiblePartyRevision, stored(cmd.DeterminedAt),
		cmd.LateReason, cmd.Signatory)
	return receipt(r, err)
}

func (m *Module) RecordContainerEvent(ctx context.Context, cmd api.RecordContainerEvent) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordContainerEvent(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordContainerEvent, cmd.VisitID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Kind, cmd.Location, stored(cmd.HappenedAt), cmd.Source, cmd.Reference)
	return receipt(r, err)
}

func (m *Module) RecordVgmResponse(ctx context.Context, cmd api.RecordVgmResponse) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordVgmResponse(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordVgmResponse, cmd.VisitID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Response, cmd.Source)
	return receipt(r, err)
}

func (m *Module) ReadVgmSignatory(ctx context.Context, q api.ReadVgmSignatory) (api.VgmSignatory, error) {
	name, err := m.deps(q.TenantID, "").ReadVgmSignatory(ctx, m.command("", q.TenantID, q.ActorID, api.ActionReadVgmSignatory, q.VisitID, 0, time.Time{}, q),
		q.Version, q.Purpose)
	if err != nil {
		return api.VgmSignatory{}, classify(err)
	}
	return api.VgmSignatory{Name: name}, nil
}

func (m *Module) ReadVgmSignatoryAccess(ctx context.Context, q api.ReadVgmSignatoryAccess) ([]api.VgmSignatoryAccess, error) {
	rows, err := m.deps(q.TenantID, "").VgmSignatoryAccess(ctx, m.command("", q.TenantID, q.ActorID, api.ActionReadVgmSignatoryLog, q.VisitID, 0, time.Time{}, q), q.Version)
	if err != nil {
		return nil, classify(err)
	}
	out := make([]api.VgmSignatoryAccess, 0, len(rows))
	for _, a := range rows {
		out = append(out, api.VgmSignatoryAccess{AccessID: a.AccessID, ActorID: a.ActorID, Purpose: a.Purpose, RecordedAt: a.RecordedAt})
	}
	return out, nil
}

func (m *Module) ReleaseContainer(ctx context.Context, cmd api.ReleaseContainer) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ChangeVisit(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionReleaseContainer, cmd.VisitID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), func(_ context.Context, v *domain.Visit) (int, error) {
		return 0, v.Release(cmd.Reason, cmd.ActorID, stored(cmd.OccurredAt))
	})
	return receipt(r, err)
}

// plan reads a house's OceanOperationPlan v1 before SEA's transaction (T3).
func (m *Module) plan(d application.Deps, tenant, shipment string, into *domain.Plan) func(context.Context, *domain.Movement) error {
	return func(ctx context.Context, _ *domain.Movement) (err error) {
		*into, err = d.Plans.Plan(ctx, domain.TenantID(tenant), shipment)
		return err
	}
}

func latest(mv *domain.Movement) int { return len(mv.State().Entries) }

func (m *Module) GetOceanMovement(ctx context.Context, q api.GetOceanMovement) (api.OceanMovement, error) {
	read, err := m.reads(q.TenantID).Movement(ctx, domain.TenantID(q.TenantID), domain.MovementID(q.MovementID))
	if err != nil {
		return api.OceanMovement{}, classify(err)
	}
	return attachVisits(view(read.Movement, read.Revision), read.Visits), nil
}

func (m *Module) FindOceanMovementByMasterBill(ctx context.Context, q api.FindOceanMovementByMasterBill) (api.OceanMovement, error) {
	read, err := m.reads(q.TenantID).ByMasterBill(ctx, domain.TenantID(q.TenantID), q.IssuerPartyID, q.Number)
	if err != nil {
		return api.OceanMovement{}, classify(err)
	}
	return attachVisits(view(read.Movement, read.Revision), read.Visits), nil
}

func (m *Module) GetContainerVisit(ctx context.Context, q api.GetContainerVisit) (api.ContainerVisit, error) {
	v, revision, err := m.reads(q.TenantID).Visit(ctx, domain.TenantID(q.TenantID), domain.VisitID(q.VisitID))
	if err != nil {
		return api.ContainerVisit{}, classify(err)
	}
	return visitView(v, revision), nil
}

func (m *Module) ListContainerVisits(ctx context.Context, q api.ListContainerVisits) ([]api.ContainerVisit, error) {
	reads, err := m.reads(q.TenantID).VisitsOf(ctx, domain.TenantID(q.TenantID), domain.MovementID(q.MovementID))
	if err != nil {
		return nil, classify(err)
	}
	out := make([]api.ContainerVisit, 0, len(reads))
	for _, r := range reads {
		out = append(out, visitView(r.Visit, r.Revision))
	}
	return out, nil
}

func (m *Module) FindLiveContainerVisit(ctx context.Context, q api.FindLiveContainerVisit) (api.ContainerVisit, error) {
	v, revision, err := m.reads(q.TenantID).LiveVisitByNumber(ctx, domain.TenantID(q.TenantID), code(q.ContainerNumber))
	if err != nil {
		return api.ContainerVisit{}, classify(err)
	}
	return visitView(v, revision), nil
}

func (m *Module) FindHouseBill(ctx context.Context, q api.FindHouseBill) (api.HouseBillRef, error) {
	reg, err := m.reads(q.TenantID).HouseBill(ctx, domain.TenantID(q.TenantID), q.LegalEntityID, q.HBL)
	if err != nil {
		return api.HouseBillRef{}, classify(err)
	}
	return api.HouseBillRef{LegalEntityID: q.LegalEntityID, HBL: reg.HBL, ShipmentID: reg.ShipmentID, MovementID: string(reg.MovementID), EntryNo: reg.EntryNo}, nil
}

func (m *Module) GetHblSeries(ctx context.Context, q api.GetHblSeries) (api.HblSeries, error) {
	s, revision, err := m.reads(q.TenantID).SeriesOf(ctx, domain.TenantID(q.TenantID), q.LegalEntityID)
	if err != nil {
		if errors.Is(err, application.ErrNoSeries) {
			return api.HblSeries{}, fmt.Errorf("%w: %w", api.ErrNotFound, err)
		}
		return api.HblSeries{}, classify(err)
	}
	st := s.State()
	out := api.HblSeries{LegalEntityID: st.LegalEntityID, Template: st.Template, FirstNumber: st.FirstNumber, Revision: revision}
	var last map[string]int64
	if err = (bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}).WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		last, err = m.numbers.Last(txCtx, q.TenantID, numbering.DocType(application.DocTypeHBL, q.LegalEntityID))
		return err
	}); err != nil {
		return api.HblSeries{}, classify(err)
	}
	for period, n := range last {
		out.Last = append(out.Last, api.NumberPeriod{Period: period, Last: n})
	}
	slices.SortFunc(out.Last, func(a, b api.NumberPeriod) int { return strings.Compare(a.Period, b.Period) })
	return out, nil
}

func view(mv *domain.Movement, revision int64) api.OceanMovement {
	st := mv.State()
	current := mv.Current()
	out := api.OceanMovement{TenantID: string(st.TenantID), MovementID: string(st.MovementID), Revision: revision, State: string(st.State),
		DocFactsVersion: len(st.DocFacts),
		Kind:            string(st.Kind), ConsolidationID: st.ConsolidationID, OperatingLegalEntityID: st.LegalEntityID, CarrierPartyID: st.Carrier.PartyID,
		CarrierPartyRevision: st.Carrier.PartyRevision, CarrierRole: st.Carrier.Role, Origin: st.Origin, Destination: st.Destination,
		LocationEdition: st.Edition, BookingID: current.BookingID, CapacityVersion: current.CapacityVersion}
	for i := len(st.Links) - 1; i >= 0; i-- {
		l := st.Links[i]
		for n, g := range l.Legs {
			leg := api.VoyageLeg{Leg: n + 1, Vessel: g.Vessel, VesselIMO: g.VesselIMO, Voyage: g.Voyage, From: g.From, To: g.To, ETD: g.ETD, ETA: g.ETA,
				BookingID: l.BookingID, CapacityVersion: l.CapacityVersion, Current: i == len(st.Links)-1}
			for _, c := range g.CutOffs {
				leg.CutOffs = append(leg.CutOffs, api.CutOff{Kind: c.Kind, At: c.At, TimeZone: c.TimeZone, Location: c.Location})
			}
			out.Legs = append(out.Legs, leg)
		}
	}
	if b, ok := mv.CurrentBill(); ok {
		out.MasterBillNumber, out.MasterBillKind = b.Number, b.Kind
	}
	for i := len(st.Bills) - 1; i >= 0; i-- {
		b := st.Bills[i]
		out.MasterBills = append(out.MasterBills, api.MasterBill{BillNo: b.No, Number: b.Number, Kind: b.Kind, Status: b.Status, RecordedBy: b.By,
			RecordedAt: b.At, Evidence: api.Evidence{SHA256: b.Evidence.SHA256, StorageID: b.Evidence.StorageID, MediaType: b.Evidence.MediaType,
				CapturedAt: b.Evidence.CapturedAt}})
	}
	for _, e := range mv.Houses() {
		f := e.Facts
		h := api.House{EntryNo: e.No, ShipmentID: e.ShipmentID, HBL: e.HBL, FULRevision: e.FULRevision, PlanVersion: e.PlanVersion,
			ContainerMode: e.ContainerMode, Packs: f.Packs, GrossWeight: f.GrossWeight, Volume: f.Volume, Basis: f.Basis, EvidenceVersion: f.Evidence,
			Start: e.Start, AssociatedAt: e.At, WindowDrift: mv.WindowDrift(e)}
		for _, p := range f.Parts {
			h.Parts = append(h.Parts, api.HousePart{ObligationID: p.ObligationID, ObligationVersion: p.ObligationVersion, OrderID: p.OrderID, Packs: p.Packs,
				PackageType: p.PackageType, GrossWeight: p.GrossWeight, Volume: p.Volume, GoodsDescription: p.GoodsDescription})
		}
		for _, s := range f.Stuffings {
			h.Containers = append(h.Containers, api.HouseContainer{ExecutionID: s.ExecutionID, LoadUnitNo: s.LoadUnitNo, TypeCode: s.TypeCode, Packs: s.Packs,
				GrossWeight: s.GrossWeight, Volume: s.Volume, LineNos: append([]int(nil), s.LineNos...), Seal: s.Seal, SealSHA256: s.SealSHA256,
				SealedAt: s.SealedAt})
		}
		out.Houses = append(out.Houses, h)
	}
	packs, grams, cm3, weighed, measured := mv.Totals()
	out.Packs = packs
	if weighed {
		out.GrossWeight = kernel.Quantity{Unit: "GRM", Amount: grams}
	}
	if measured && cm3 > 0 {
		out.Volume = kernel.Quantity{Unit: "CMQ", Amount: cm3}
	}
	for _, f := range st.CarrierFacts {
		out.CarrierFacts = append(out.CarrierFacts, api.OceanCarrierFact{FactNo: f.No, Kind: f.Kind, ContainerNumber: f.ContainerNumber,
			BillNumber: f.BillNumber, Vessel: f.Vessel, Voyage: f.Voyage, Source: f.Source, Reference: f.Reference, SHA256: f.SHA256,
			OccurredAt: f.OccurredAt, RecordedBy: f.By, RecordedAt: f.At})
	}
	return out
}

// attachVisits adds the movement's live visits and names, on each house, the container of the stuffed visit that
// keeps that house's stuffing.
func attachVisits(out api.OceanMovement, reads []application.VisitRead) api.OceanMovement {
	numberOf := map[string]string{}
	for _, r := range reads {
		st := r.Visit.State()
		if st.Status == domain.VisitReleased {
			continue
		}
		out.Containers = append(out.Containers, visitView(r.Visit, r.Revision))
		if st.Status == domain.VisitStuffed || st.Status == domain.VisitVGM || st.Status == domain.VisitGatedIn || st.Status == domain.VisitLoaded {
			numberOf[st.ExecutionID] = st.Number
		}
	}
	for i := range out.Houses {
		for j := range out.Houses[i].Containers {
			out.Houses[i].Containers[j].ContainerNumber = numberOf[out.Houses[i].Containers[j].ExecutionID]
		}
	}
	return out
}

func visitView(v *domain.Visit, revision int64) api.ContainerVisit {
	st := v.State()
	out := api.ContainerVisit{TenantID: string(st.TenantID), VisitID: string(st.VisitID), Revision: revision, MovementID: string(st.MovementID),
		ConsolidationID: st.ConsolidationID, LoadUnitNo: st.LoadUnitNo, ContainerNumber: st.Number, SizeType: st.SizeType, TareKg: st.Plate.TareKg,
		MaxGrossKg: st.Plate.MaxGrossKg, PlateSource: st.Plate.Source, Status: string(st.Status), ExecutionID: st.ExecutionID,
		DifferenceReason: st.DifferenceReason, StuffedAt: st.StuffedAt, ReleaseReason: st.ReleaseReason, ReleasedAt: st.ReleasedAt,
		AssignedBy: st.AssignedBy, AssignedAt: st.AssignedAt}
	if seal := v.Seal(); seal.No != 0 {
		out.SealNumber, out.SealParty = seal.Number, seal.Party
	}
	for i := len(st.Seals) - 1; i >= 0; i-- {
		s := st.Seals[i]
		out.Seals = append(out.Seals, api.ContainerSeal{SealNo: s.No, Number: s.Number, Party: s.Party, AppliedAt: s.AppliedAt, Source: s.Source,
			RecordedBy: s.By, RecordedAt: s.At})
	}
	for i := len(st.Assertions) - 1; i >= 0; i-- {
		a := st.Assertions[i]
		out.Assertions = append(out.Assertions, api.VgmAssertion{Version: a.Version, MassGrams: a.MassGrams, Method: a.Method,
			ResponsiblePartyID: a.PartyID, ResponsiblePartyRevision: a.PartyRevision, DeterminedAt: a.DeterminedAt, SubmittedAt: a.SubmittedAt,
			LateReason: a.LateReason, RecordedBy: a.By, RecordedAt: a.At, Response: a.Response, ResponseSource: a.ResponseSource,
			RespondedAt: a.RespondedAt, ResponseBy: a.ResponseBy})
	}
	for i := len(st.Events) - 1; i >= 0; i-- {
		e := st.Events[i]
		out.Events = append(out.Events, api.ContainerEvent{EventNo: e.No, Kind: e.Kind, Location: e.Location, HappenedAt: e.OccurredAt,
			Source: e.Source, Reference: e.Reference, RecordedBy: e.By, RecordedAt: e.At})
	}
	return out
}

func (m *Module) reads(tenant string) application.Reads {
	return application.Reads{Tx: bento.TenantTransactor{DB: m.db, Tenant: tenant}, Movements: postgres.MovementStore{DB: m.db},
		Visits: postgres.VisitStore{DB: m.db}, Series: postgres.SeriesStore{DB: m.db}, Numbers: m.numbers, Register: postgres.Register{DB: m.db}}
}

func (m *Module) deps(tenant, commandID string) application.Deps {
	return application.Deps{
		Tx:        bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Movements: postgres.MovementStore{DB: m.db},
		Visits:    postgres.VisitStore{DB: m.db},
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
	out := api.Receipt{CommandID: r.CommandID, NewRevision: r.NewRevision, EntryNo: r.EntryNo, HBL: r.HBL, BillNo: r.BillNo, SealNo: r.SealNo,
		AssertionNo: r.AssertionNo, EventNo: r.EventNo, FactsVersion: r.FactsVersion, FactNo: r.FactNo}
	if r.Kind == "ContainerVisit" {
		out.VisitID = r.SubjectID
	} else {
		out.MovementID = r.SubjectID
	}
	return out, nil
}

type authorizer struct{ api.Authorizer }

func (a authorizer) RequireSeaAction(ctx context.Context, s application.Scope, action application.Action, subject string) error {
	return a.AuthorizeSeaAction(ctx, string(s.TenantID), s.ActorID, string(action), subject)
}

// classify wraps known SEA, kernel and Bento causes in their api error kind and passes anything else through.
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
		errors.Is(err, domain.ErrHousesLive), errors.Is(err, domain.ErrInvalidHBL), errors.Is(err, domain.ErrInvalidMasterBill),
		errors.Is(err, domain.ErrInvalidSeries), errors.Is(err, domain.ErrSeriesExhausted), errors.Is(err, domain.ErrInvalidVisit),
		errors.Is(err, domain.ErrInvalidContainer), errors.Is(err, domain.ErrInvalidSeal), errors.Is(err, domain.ErrInvalidVGM), errors.Is(err, domain.ErrInvalidEvent), errors.Is(err, domain.ErrNotStuffed),
		errors.Is(err, domain.ErrStuffingDiffers), errors.Is(err, domain.ErrCargoInContainer), errors.Is(err, domain.ErrNotFinalizable), errors.Is(err, application.ErrInvalidQuery),
		errors.Is(err, application.ErrUnknownBooking), errors.Is(err, application.ErrUnknownShipment), errors.Is(err, application.ErrNoSeries),
		errors.Is(err, application.ErrHBLInUse), errors.Is(err, application.ErrHBLDiffers), errors.Is(err, application.ErrMasterBillInUse),
		errors.Is(err, application.ErrContainerInUse), errors.Is(err, application.ErrUnitHasContainer), errors.Is(err, application.ErrContainersAssigned),
		errors.Is(err, kernel.ErrOutsideRecordWindow):
		kind = api.ErrRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
