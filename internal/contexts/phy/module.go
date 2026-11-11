// Package phy assembles the PHY (Physical Logistics) bounded context for consolidations and their houses
// (PHY V0.1). Code outside PHY uses it through package api; everything under internal/ is private to PHY.
package phy

import (
	"context"
	"errors"
	"fmt"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	fulacl "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/adapters/ful"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/adapters/postgres"
	refacl "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/adapters/reference"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
	refapi "go.idp.xyz/idp-cargo/internal/reference/api"
)

// Module wires PHY's use cases to PostgreSQL through IDP-Bento-Go, to FUL's LoadIntent v1 and to the
// location reference data.
type Module struct {
	db        *bentopg.DB
	outbox    *bentooutbox.Store
	intents   application.LoadIntents
	auth      api.Authorizer
	clock     bento.SystemClock
	window    kernel.RecordWindow
	locations application.Locations // nil until wired
}

// Option configures a Module.
type Option func(*Module)

// WithRecordWindow sets how far a command's business time may lie before the time PHY records it (T1). The
// default is kernel.DefaultRecordWindow.
func WithRecordWindow(w kernel.RecordWindow) Option { return func(m *Module) { m.window = w } }

// WithLocations gives PHY the location reference data that consolidation routes are resolved against (F2).
// Without it no consolidation can be created.
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

// New assembles PHY. ful answers LoadIntent v1 for the shipments PHY attaches and closes (relationship 7);
// the composition root passes FUL's module. PHY never writes to FUL: the caller records the attachment in
// FUL after PHY committed it (rule T6 in S1).
func New(db *bentopg.DB, ful fulapi.LoadIntentQueries, auth api.Authorizer, opts ...Option) (*Module, error) {
	if db == nil || ful == nil || auth == nil {
		return nil, errors.New("phy: module needs a database, FUL's load intents and an authorizer")
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	m := &Module{db: db, outbox: outbox, intents: fulacl.LoadIntents{FUL: ful}, auth: auth, window: kernel.DefaultRecordWindow}
	for _, opt := range opts {
		opt(m)
	}
	if !m.window.Valid() {
		return nil, errors.New("phy: the record window must not be negative")
	}
	return m, nil
}

func (m *Module) CreateConsolidation(ctx context.Context, cmd api.CreateConsolidation) (api.Receipt, error) {
	r, err := application.CreateConsolidation{Deps: m.deps(cmd.TenantID, cmd.CommandID), Locations: m.locations}.Handle(ctx,
		application.CreateConsolidationCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID),
			ConsolidationID: domain.ConsolidationID(cmd.ConsolidationID), Mode: domain.Mode(cmd.Mode), Kind: domain.Kind(cmd.Kind),
			Operator: cmd.OperatingLegalEntityID, Origin: cmd.Origin, Destination: cmd.Destination,
			PlannedDeparture: stored(cmd.PlannedDeparture), HouseCutOff: stored(cmd.HouseCutOff), OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) AmendConsolidation(ctx context.Context, cmd api.AmendConsolidation) (api.Receipt, error) {
	r, err := application.AmendConsolidation{Deps: m.deps(cmd.TenantID, cmd.CommandID), Locations: m.locations}.Handle(ctx,
		application.AmendConsolidationCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID),
			ConsolidationID: domain.ConsolidationID(cmd.ConsolidationID), ExpectedRevision: cmd.ExpectedRevision, Origin: cmd.Origin,
			Destination: cmd.Destination, PlannedDeparture: stored(cmd.PlannedDeparture), HouseCutOff: stored(cmd.HouseCutOff),
			Reason: cmd.Reason, OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) CloseConsolidation(ctx context.Context, cmd api.CloseConsolidation) (api.Receipt, error) {
	r, err := application.CloseConsolidation{Deps: m.deps(cmd.TenantID, cmd.CommandID), Intents: m.intents}.Handle(ctx,
		application.CloseConsolidationCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID),
			ConsolidationID: domain.ConsolidationID(cmd.ConsolidationID), ExpectedRevision: cmd.ExpectedRevision, OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) ReopenConsolidation(ctx context.Context, cmd api.ReopenConsolidation) (api.Receipt, error) {
	r, err := application.ReopenConsolidation{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx,
		application.ReopenConsolidationCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID),
			ConsolidationID: domain.ConsolidationID(cmd.ConsolidationID), ExpectedRevision: cmd.ExpectedRevision, Reason: cmd.Reason,
			OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) CancelConsolidation(ctx context.Context, cmd api.CancelConsolidation) (api.Receipt, error) {
	r, err := application.CancelConsolidation{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx,
		application.CancelConsolidationCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID),
			ConsolidationID: domain.ConsolidationID(cmd.ConsolidationID), ExpectedRevision: cmd.ExpectedRevision, Reason: cmd.Reason,
			OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) AttachHouse(ctx context.Context, cmd api.AttachHouse) (api.Receipt, error) {
	r, err := application.AttachHouse{Deps: m.deps(cmd.TenantID, cmd.CommandID), Intents: m.intents}.Handle(ctx,
		application.AttachHouseCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID),
			ConsolidationID: domain.ConsolidationID(cmd.ConsolidationID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
			ExpectedMembership: cmd.ExpectedMembershipRevision, OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) DetachHouse(ctx context.Context, cmd api.DetachHouse) (api.Receipt, error) {
	r, err := application.DetachHouse{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx,
		application.DetachHouseCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID),
			ConsolidationID: domain.ConsolidationID(cmd.ConsolidationID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
			ExpectedMembership: cmd.ExpectedMembershipRevision, Reason: domain.Reason(cmd.Reason), ReasonText: cmd.ReasonText,
			OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) TransferHouse(ctx context.Context, cmd api.TransferHouse) (api.Receipt, error) {
	r, err := application.TransferHouse{Deps: m.deps(cmd.TenantID, cmd.CommandID), Intents: m.intents}.Handle(ctx,
		application.TransferHouseCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID),
			From: domain.ConsolidationID(cmd.ConsolidationID), To: domain.ConsolidationID(cmd.ToConsolidationID),
			ShipmentID: domain.ShipmentID(cmd.ShipmentID), ExpectedMembership: cmd.ExpectedMembershipRevision,
			Reason: domain.Reason(cmd.Reason), ReasonText: cmd.ReasonText, OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) RefreshHouseIntent(ctx context.Context, cmd api.RefreshHouseIntent) (api.Receipt, error) {
	r, err := application.RefreshHouseIntent{Deps: m.deps(cmd.TenantID, cmd.CommandID), Intents: m.intents}.Handle(ctx,
		application.RefreshHouseIntentCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID),
			ConsolidationID: domain.ConsolidationID(cmd.ConsolidationID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
			ExpectedMembership: cmd.ExpectedMembershipRevision, OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) EndMembershipForCancelledShipment(ctx context.Context, cmd api.EndMembershipForCancelledShipment) (api.Receipt, error) {
	r, err := application.EndMembershipForCancelledShipment{Deps: m.deps(cmd.TenantID, cmd.CommandID), Intents: m.intents}.Handle(ctx,
		application.EndMembershipCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID),
			ConsolidationID: domain.ConsolidationID(cmd.ConsolidationID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
			ExpectedMembership: cmd.ExpectedMembershipRevision, OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) GetConsolidation(ctx context.Context, q api.GetConsolidation) (api.Consolidation, error) {
	c, revision, houses, err := m.reads(q.TenantID).Consolidation(ctx, domain.TenantID(q.TenantID), domain.ConsolidationID(q.ConsolidationID))
	if err != nil {
		return api.Consolidation{}, classify(err)
	}
	st := c.State()
	out := api.Consolidation{TenantID: string(st.TenantID), ConsolidationID: string(st.ConsolidationID), Revision: revision,
		Mode: string(st.Mode), Kind: string(st.Kind), OperatingLegalEntityID: st.OperatingLegalEntityID, State: string(st.State),
		Current: version(c.Current())}
	for _, v := range st.Versions {
		out.Versions = append(out.Versions, version(v))
	}
	for _, h := range houses {
		e := h.Entry
		out.Members = append(out.Members, api.Member{ShipmentID: string(h.ShipmentID), EntryNo: e.No, MembershipRevision: h.MembershipRevision,
			FULRevision: e.FULRevision, PlanVersion: e.PlanVersion, Packs: e.Scope.Packs, GrossWeight: e.Scope.GrossWeight,
			Volume: e.Scope.Volume, AttachedAt: e.At})
	}
	return out, nil
}

func (m *Module) GetConsolMembership(ctx context.Context, q api.GetConsolMembership) (api.ConsolMembership, error) {
	ms, revision, c, consolRevision, err := m.reads(q.TenantID).Membership(ctx, domain.TenantID(q.TenantID), domain.ShipmentID(q.ShipmentID))
	if err != nil {
		return api.ConsolMembership{}, classify(err)
	}
	st := ms.State()
	out := api.ConsolMembership{TenantID: string(st.TenantID), ShipmentID: string(st.ShipmentID), Stage: st.Stage, Revision: revision}
	for _, e := range st.Entries {
		out.History = append(out.History, entry(e))
	}
	if live, ok := ms.Live(); ok && c != nil {
		cs, v := c.State(), c.Current()
		out.Live, out.Entry = true, entry(live)
		out.Consolidation = api.ConsolidationSnapshot{ConsolidationID: string(cs.ConsolidationID), Revision: consolRevision, Mode: string(cs.Mode),
			Kind: string(cs.Kind), OperatingLegalEntityID: cs.OperatingLegalEntityID, State: string(cs.State), Origin: v.Route.Origin,
			Destination: v.Route.Destination, OriginIATA: v.Route.OriginIATA, DestinationIATA: v.Route.DestinationIATA,
			LocationEdition: v.Route.LocationEdition, PlannedDeparture: v.PlannedDeparture, HouseCutOff: v.HouseCutOff}
	}
	return out, nil
}

func (m *Module) ListOpenConsolidations(ctx context.Context, q api.ListOpenConsolidations) ([]api.ConsolidationSummary, error) {
	list, err := m.reads(q.TenantID).Open(ctx, domain.TenantID(q.TenantID), application.ListFilter{Mode: domain.Mode(q.Mode), Origin: q.Origin,
		Destination: q.Destination, Operator: q.OperatingLegalEntityID, DepartureFrom: q.DepartureFrom, DepartureUntil: q.DepartureUntil, Limit: q.Limit})
	if err != nil {
		return nil, classify(err)
	}
	out := make([]api.ConsolidationSummary, 0, len(list))
	for _, s := range list {
		v := s.State.Versions[0]
		out = append(out, api.ConsolidationSummary{ConsolidationID: string(s.State.ConsolidationID), Revision: s.Revision, Mode: string(s.State.Mode),
			Kind: string(s.State.Kind), OperatingLegalEntityID: s.State.OperatingLegalEntityID, Origin: v.Route.Origin, Destination: v.Route.Destination,
			OriginIATA: v.Route.OriginIATA, DestinationIATA: v.Route.DestinationIATA, PlannedDeparture: v.PlannedDeparture, HouseCutOff: v.HouseCutOff,
			LiveHouses: s.LiveHouses})
	}
	return out, nil
}

func version(v domain.Version) api.ConsolidationVersion {
	return api.ConsolidationVersion{Version: v.No, Change: string(v.Change), State: string(v.State), Origin: v.Route.Origin,
		Destination: v.Route.Destination, OriginIATA: v.Route.OriginIATA, DestinationIATA: v.Route.DestinationIATA,
		LocationEdition: v.Route.LocationEdition, PlannedDeparture: v.PlannedDeparture, HouseCutOff: v.HouseCutOff, Reason: v.Reason,
		WrittenAt: v.At, EnteredAt: v.EnteredAt}
}

func entry(e domain.Entry) api.MembershipEntry {
	out := api.MembershipEntry{EntryNo: e.No, ConsolidationID: string(e.ConsolidationID), Start: string(e.Start), PredecessorNo: e.Predecessor,
		FULRevision: e.FULRevision, PlanVersion: e.PlanVersion, Packs: e.Scope.Packs, GrossWeight: e.Scope.GrossWeight, Volume: e.Scope.Volume,
		AttachedAt: e.At, EnteredAt: e.EnteredAt, End: string(e.End), Reason: string(e.Reason), EndedAt: e.EndedAt, SuccessorNo: e.Successor}
	for _, p := range e.Parts {
		out.Parts = append(out.Parts, api.EntryPart{ObligationID: p.ObligationID, ObligationVersion: p.ObligationVersion, OrderID: p.OrderID,
			Packs: p.Packs, PackageType: p.PackageType, GrossWeight: p.GrossWeight, Volume: p.Volume})
	}
	return out
}

func (m *Module) reads(tenant string) application.Reads {
	return application.Reads{Tx: bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Consolidations: postgres.ConsolidationStore{DB: m.db}, Memberships: postgres.MembershipStore{DB: m.db}}
}

func (m *Module) deps(tenant, commandID string) application.Deps {
	return application.Deps{
		Tx:             bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Consolidations: postgres.ConsolidationStore{DB: m.db},
		Memberships:    postgres.MembershipStore{DB: m.db},
		Receipts:       postgres.CommandReceipts{DB: m.db},
		Outbox:         postgres.Outbox{Writer: m.outbox, CommandID: commandID, RecordedAt: m.clock.Now()},
		Auth:           authorizer{m.auth},
		Clock:          m.clock,
		Window:         m.window,
	}
}

func scope(tenant, actor string) application.Scope {
	return application.Scope{TenantID: domain.TenantID(tenant), ActorID: actor}
}

// stored truncates to the microsecond precision of timestamptz, so a consolidation reads back exactly as it
// was written.
func stored(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Microsecond)
}

func toReceipt(r application.Receipt, err error) (api.Receipt, error) {
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	return api.Receipt{CommandID: r.CommandID, ConsolidationID: string(r.ConsolidationID), ShipmentID: string(r.ShipmentID),
		NewRevision: r.NewRevision, EntryNo: r.EntryNo}, nil
}

type authorizer struct{ api.Authorizer }

func (a authorizer) RequireConsolidationAction(ctx context.Context, s application.Scope, action application.Action, id domain.ConsolidationID) error {
	return a.AuthorizeConsolidationAction(ctx, string(s.TenantID), s.ActorID, string(action), string(id))
}

// classify wraps known PHY, kernel and Bento causes in their api error kind and passes anything else through.
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
	case errors.Is(err, domain.ErrInvalidConsolidation), errors.Is(err, domain.ErrInvalidRoute), errors.Is(err, domain.ErrInvalidSchedule),
		errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrInvalidChange), errors.Is(err, domain.ErrNotOpen),
		errors.Is(err, domain.ErrHousesAttached), errors.Is(err, domain.ErrNoHouses), errors.Is(err, domain.ErrInvalidIntent),
		errors.Is(err, domain.ErrIntentNotPlanned), errors.Is(err, domain.ErrModeMismatch), errors.Is(err, domain.ErrRouteMismatch),
		errors.Is(err, domain.ErrWindowMismatch), errors.Is(err, domain.ErrOtherLegalEntity), errors.Is(err, domain.ErrInvalidMembership),
		errors.Is(err, domain.ErrAlreadyAttached), errors.Is(err, domain.ErrNotAttached), errors.Is(err, domain.ErrSameConsolidation),
		errors.Is(err, domain.ErrInvalidReason), errors.Is(err, domain.ErrNothingToRefresh), errors.Is(err, domain.ErrShipmentNotCancelled),
		errors.Is(err, domain.ErrMemberStale), errors.Is(err, domain.ErrMemberNotAllocated), errors.Is(err, application.ErrInvalidQuery),
		errors.Is(err, application.ErrUnknownShipment), errors.Is(err, application.ErrLocationRefused), errors.Is(err, kernel.ErrOutsideRecordWindow):
		kind = api.ErrRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
