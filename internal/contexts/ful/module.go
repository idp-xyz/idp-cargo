// Package ful assembles the FUL (Freight Fulfillment) bounded context. Code outside FUL uses it through
// package api; everything under internal/ is private to FUL.
package ful

import (
	"context"
	"errors"
	"fmt"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	comacl "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/adapters/com"
	phyacl "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/adapters/phy"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/internal/adapters/postgres"
	refacl "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/adapters/reference"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
	refapi "go.idp.xyz/idp-cargo/internal/reference/api"
)

// Module wires FUL's use cases to PostgreSQL through IDP-Bento-Go and to COM's query side.
type Module struct {
	db        *bentopg.DB
	outbox    *bentooutbox.Store
	com       comapi.Queries
	auth      api.Authorizer
	clock     bento.SystemClock
	window    kernel.RecordWindow
	locations application.Locations         // location reference data (F2); nil until wired
	consols   application.ConsolMemberships // PHY's ConsolMembership v1 (V0.4); nil until wired
}

// Option configures a Module.
type Option func(*Module)

// WithRecordWindow sets how far a command's business time may lie before the time FUL records it, and how
// much clock skew after it is tolerated (T1). The default is kernel.DefaultRecordWindow.
func WithRecordWindow(w kernel.RecordWindow) Option { return func(m *Module) { m.window = w } }

// WithLocations gives FUL the location reference data that routes are resolved against (F2). Without it a
// plan cannot name locations.
func WithLocations(l refapi.Locations) Option {
	return func(m *Module) {
		if l != nil {
			m.locations = refacl.Locations{Ref: l}
		}
	}
}

// WithConsolMemberships gives FUL PHY's ConsolMembership v1 (relationship 8), which it reads before recording a
// consol attachment or detachment. Without it those commands are refused.
func WithConsolMemberships(q phyapi.ConsolMembershipQueries) Option {
	return func(m *Module) {
		if q != nil {
			m.consols = phyacl.Memberships{PHY: q}
		}
	}
}

var (
	_ api.Commands                 = (*Module)(nil)
	_ api.ConsolAttachmentCommands = (*Module)(nil)
	_ api.Queries                  = (*Module)(nil)
	_ api.LoadIntentQueries        = (*Module)(nil)
	_ api.AirOperationPlanQueries  = (*Module)(nil)
)

// New assembles FUL. com answers which obligation versions a shipment may plan; the composition root
// passes COM's module. FUL never writes to COM: the caller reserves and releases obligations through
// COM's ObligationReservation v1 (rule T6 in S1).
func New(db *bentopg.DB, com comapi.Queries, auth api.Authorizer, opts ...Option) (*Module, error) {
	if db == nil || com == nil || auth == nil {
		return nil, errors.New("ful: module needs a database, COM queries and an authorizer")
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	m := &Module{db: db, outbox: outbox, com: com, auth: auth, window: kernel.DefaultRecordWindow}
	for _, opt := range opts {
		opt(m)
	}
	if !m.window.Valid() {
		return nil, errors.New("ful: the record window must not be negative")
	}
	return m, nil
}

func (m *Module) CreateShipmentFromObligation(ctx context.Context, cmd api.CreateShipmentFromObligation) (api.Receipt, error) {
	receipt, err := application.CreateShipmentFromObligation{Deps: m.deps(cmd.TenantID, cmd.CommandID), Obligations: m.obligations()}.Handle(ctx,
		application.CreateShipmentFromObligationCommand{
			CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
			ObligationID: domain.ObligationID(cmd.ObligationID), ObligationVersion: cmd.ObligationVersion,
			Part: part(cmd.Packs, cmd.GrossWeight, cmd.Volume), OccurredAt: stored(cmd.OccurredAt),
		})
	return toReceipt(receipt, err)
}

func (m *Module) AddObligationToShipment(ctx context.Context, cmd api.AddObligationToShipment) (api.Receipt, error) {
	receipt, err := application.AddObligationToShipment{Deps: m.deps(cmd.TenantID, cmd.CommandID), Obligations: m.obligations()}.Handle(ctx,
		application.AddObligationToShipmentCommand{
			CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
			ExpectedRevision: cmd.ExpectedRevision, ObligationID: domain.ObligationID(cmd.ObligationID), ObligationVersion: cmd.ObligationVersion,
			Part: part(cmd.Packs, cmd.GrossWeight, cmd.Volume), OccurredAt: stored(cmd.OccurredAt),
		})
	return toReceipt(receipt, err)
}

func (m *Module) ChangeShipmentPart(ctx context.Context, cmd api.ChangeShipmentPart) (api.Receipt, error) {
	receipt, err := application.ChangeShipmentPart{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.ChangeShipmentPartCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
		ExpectedRevision: cmd.ExpectedRevision, ObligationID: domain.ObligationID(cmd.ObligationID), ObligationVersion: cmd.ObligationVersion,
		Part: part(cmd.Packs, cmd.GrossWeight, cmd.Volume), Reason: cmd.Reason, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) AmendShipmentPlan(ctx context.Context, cmd api.AmendShipmentPlan) (api.Receipt, error) {
	route := domain.Route{Mode: domain.TransportMode(cmd.TransportMode), Origin: cmd.Origin, Destination: cmd.Destination,
		Departure: kernel.ValidityInterval{From: stored(cmd.DepartureWindow.From), Until: stored(cmd.DepartureWindow.Until)}}
	receipt, err := application.AmendShipmentPlan{Deps: m.deps(cmd.TenantID, cmd.CommandID), Locations: m.locations, Modes: comacl.Obligations{COM: m.com}}.Handle(ctx, application.AmendShipmentPlanCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
		ExpectedRevision: cmd.ExpectedRevision, Route: route, Reason: cmd.Reason, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) ConfirmShipmentPlan(ctx context.Context, cmd api.ConfirmShipmentPlan) (api.Receipt, error) {
	receipt, err := application.ConfirmShipmentPlan{Deps: m.deps(cmd.TenantID, cmd.CommandID), Obligations: m.obligations()}.Handle(ctx,
		application.ConfirmShipmentPlanCommand{
			CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
			ExpectedRevision: cmd.ExpectedRevision, OccurredAt: stored(cmd.OccurredAt),
		})
	return toReceipt(receipt, err)
}

func (m *Module) ReleaseObligationFromShipment(ctx context.Context, cmd api.ReleaseObligationFromShipment) (api.Receipt, error) {
	receipt, err := application.ReleaseObligationFromShipment{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx,
		application.ReleaseObligationFromShipmentCommand{
			CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
			ExpectedRevision: cmd.ExpectedRevision, ObligationID: domain.ObligationID(cmd.ObligationID), ObligationVersion: cmd.ObligationVersion,
			OccurredAt: stored(cmd.OccurredAt),
		})
	return toReceipt(receipt, err)
}

func (m *Module) CancelShipment(ctx context.Context, cmd api.CancelShipment) (api.Receipt, error) {
	receipt, err := application.CancelShipment{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.CancelShipmentCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
		ExpectedRevision: cmd.ExpectedRevision, Reason: cmd.Reason, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) RecordConsolAttachment(ctx context.Context, cmd api.RecordConsolAttachment) (api.Receipt, error) {
	receipt, err := application.RecordConsolAttachment{Deps: m.deps(cmd.TenantID, cmd.CommandID), Memberships: m.consols}.Handle(ctx,
		application.RecordConsolAttachmentCommand{
			CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
			ExpectedRevision: cmd.ExpectedRevision, ConsolidationID: cmd.ConsolidationID, OccurredAt: stored(cmd.OccurredAt),
		})
	return toReceipt(receipt, err)
}

func (m *Module) RecordConsolDetachment(ctx context.Context, cmd api.RecordConsolDetachment) (api.Receipt, error) {
	receipt, err := application.RecordConsolDetachment{Deps: m.deps(cmd.TenantID, cmd.CommandID), Memberships: m.consols}.Handle(ctx,
		application.RecordConsolDetachmentCommand{
			CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
			ExpectedRevision: cmd.ExpectedRevision, ConsolidationID: cmd.ConsolidationID, OccurredAt: stored(cmd.OccurredAt),
		})
	return toReceipt(receipt, err)
}

// GetLoadIntent answers LoadIntent v1 for PHY: the shipment's current plan with the package type of every part
// and its current consol attachment on the main carriage.
func (m *Module) GetLoadIntent(ctx context.Context, q api.GetShipmentPlanSnapshot) (api.LoadIntent, error) {
	shipment, revision, err := m.shipment(ctx, q)
	if err != nil {
		return api.LoadIntent{}, err
	}
	return loadIntent(shipment, revision), nil
}

// loadIntent is LoadIntent v1 of a shipment read at a revision.
func loadIntent(shipment *domain.Shipment, revision int64) api.LoadIntent {
	st, plan := shipment.State(), shipment.Latest()
	intent := api.LoadIntent{
		TenantID: string(st.TenantID), ShipmentID: string(st.ShipmentID), Revision: revision, Status: string(st.Status),
		PlanVersion: plan.No, CustomerPartyID: st.CustomerPartyID, LegalEntityID: st.LegalEntityID,
		TransportMode: string(plan.Route.Mode), Origin: plan.Route.Origin, Destination: plan.Route.Destination,
		OriginIATA: plan.Route.OriginIATA, DestinationIATA: plan.Route.DestinationIATA, LocationEdition: plan.Route.LocationEdition,
		DepartureWindow: plan.Route.Departure, Stage: api.StageMain,
	}
	if n := len(st.Confirmations); n > 0 {
		intent.ConfirmedVersion = st.Confirmations[n-1].PlanNo
	}
	if st.Status != domain.ShipmentCancelled {
		intent.Packs, intent.GrossWeight, intent.Volume = plan.Scope.Packs, plan.Scope.GrossWeight, plan.Scope.Volume
		for _, p := range shipment.Parts() {
			intent.Parts = append(intent.Parts, api.LoadIntentPart{ObligationID: string(p.ObligationID), ObligationVersion: p.ObligationVersion,
				OrderID: p.OrderID, Packs: p.Part.Packs, PackageType: p.PackageType, GrossWeight: p.Part.GrossWeight, Volume: p.Part.Volume})
		}
	}
	if a, ok := shipment.Attachment(domain.StageMain); ok {
		intent.ConsolidationID, intent.MembershipEntryNo = a.ConsolidationID, a.MembershipEntryNo
	}
	return intent
}

// GetAirOperationPlan answers AirOperationPlan v1 for AIR: the LoadIntent of the shipment with the PHY membership
// revision of its current attachment.
func (m *Module) GetAirOperationPlan(ctx context.Context, q api.GetShipmentPlanSnapshot) (api.AirOperationPlan, error) {
	shipment, revision, err := m.shipment(ctx, q)
	if err != nil {
		return api.AirOperationPlan{}, err
	}
	intent := loadIntent(shipment, revision)
	plan := api.AirOperationPlan{TenantID: intent.TenantID, ShipmentID: intent.ShipmentID, Revision: intent.Revision, Status: intent.Status,
		PlanVersion: intent.PlanVersion, CustomerPartyID: intent.CustomerPartyID, LegalEntityID: intent.LegalEntityID, TransportMode: intent.TransportMode,
		Origin: intent.Origin, Destination: intent.Destination, OriginIATA: intent.OriginIATA, DestinationIATA: intent.DestinationIATA,
		LocationEdition: intent.LocationEdition, DepartureWindow: intent.DepartureWindow, Packs: intent.Packs, GrossWeight: intent.GrossWeight,
		Volume: intent.Volume, Parts: intent.Parts, Stage: intent.Stage, ConsolidationID: intent.ConsolidationID, MembershipEntryNo: intent.MembershipEntryNo}
	if a, ok := shipment.Attachment(domain.StageMain); ok && a.ConsolidationID == intent.ConsolidationID {
		plan.MembershipRevision = a.MembershipRevision
	}
	return plan, nil
}

func (m *Module) GetShipmentPlanSnapshot(ctx context.Context, q api.GetShipmentPlanSnapshot) (api.ShipmentPlanSnapshot, error) {
	shipment, revision, err := m.shipment(ctx, q)
	if err != nil {
		return api.ShipmentPlanSnapshot{}, err
	}
	st, plan := shipment.State(), shipment.Latest()
	snapshot := api.ShipmentPlanSnapshot{
		TenantID: string(st.TenantID), ShipmentID: string(st.ShipmentID), Revision: revision, Status: string(st.Status),
		PlanVersion: plan.No, CustomerPartyID: st.CustomerPartyID, LegalEntityID: st.LegalEntityID,
		TransportMode: string(plan.Route.Mode), Origin: plan.Route.Origin, Destination: plan.Route.Destination, DepartureWindow: plan.Route.Departure,
		PlannedAt: plan.At,
	}
	if n := len(st.Confirmations); n > 0 {
		snapshot.ConfirmedVersion = st.Confirmations[n-1].PlanNo
	}
	for _, ob := range shipment.Planned() {
		snapshot.Obligations = append(snapshot.Obligations, api.ObligationRef{ObligationID: string(ob.ObligationID),
			ObligationVersion: ob.ObligationVersion, OrderID: ob.OrderID})
	}
	if st.Status != domain.ShipmentCancelled {
		snapshot.Packs, snapshot.GrossWeight, snapshot.Volume = plan.Scope.Packs, plan.Scope.GrossWeight, plan.Scope.Volume
	}
	return snapshot, nil
}

func (m *Module) GetShipmentPlanSnapshotV2(ctx context.Context, q api.GetShipmentPlanSnapshot) (api.ShipmentPlanSnapshotV2, error) {
	shipment, revision, err := m.shipment(ctx, q)
	if err != nil {
		return api.ShipmentPlanSnapshotV2{}, err
	}
	st, plan := shipment.State(), shipment.Latest()
	snapshot := api.ShipmentPlanSnapshotV2{
		TenantID: string(st.TenantID), ShipmentID: string(st.ShipmentID), Revision: revision, Status: string(st.Status),
		PlanVersion: plan.No, CustomerPartyID: st.CustomerPartyID, LegalEntityID: st.LegalEntityID,
		TransportMode: string(plan.Route.Mode), Origin: plan.Route.Origin, Destination: plan.Route.Destination, DepartureWindow: plan.Route.Departure,
		PlannedAt: plan.At, OriginIATA: plan.Route.OriginIATA, DestinationIATA: plan.Route.DestinationIATA,
		LocationEdition: plan.Route.LocationEdition, EnteredAt: plan.EnteredAt,
	}
	if n := len(st.Confirmations); n > 0 {
		snapshot.ConfirmedVersion = st.Confirmations[n-1].PlanNo
	}
	for _, p := range shipment.Parts() {
		snapshot.Parts = append(snapshot.Parts, api.PlannedPart{ObligationID: string(p.ObligationID), ObligationVersion: p.ObligationVersion,
			OrderID: p.OrderID, Packs: p.Part.Packs, GrossWeight: p.Part.GrossWeight, Volume: p.Part.Volume,
			ObligationPacks: p.Packs, ObligationWeight: p.GrossWeight, ObligationVolume: p.Volume})
	}
	if st.Status != domain.ShipmentCancelled {
		snapshot.Packs, snapshot.GrossWeight, snapshot.Volume = plan.Scope.Packs, plan.Scope.GrossWeight, plan.Scope.Volume
	}
	return snapshot, nil
}

// shipment reads a shipment for a snapshot, without locking it.
func (m *Module) shipment(ctx context.Context, q api.GetShipmentPlanSnapshot) (*domain.Shipment, int64, error) {
	shipment, revision, err := application.GetShipmentPlan{
		Tx: bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}, Shipments: postgres.ShipmentStore{DB: m.db},
	}.Handle(ctx, domain.TenantID(q.TenantID), domain.ShipmentID(q.ShipmentID))
	if err != nil {
		return nil, 0, classify(err)
	}
	return shipment, revision, nil
}

// part is the part a command asks for; zero packs asks for all that other shipments leave.
func part(packs int64, weight, volume kernel.Quantity) domain.CargoScope {
	if packs == 0 && weight == (kernel.Quantity{}) && volume == (kernel.Quantity{}) {
		return domain.CargoScope{}
	}
	return domain.CargoScope{Packs: packs, GrossWeight: weight, Volume: volume}
}

func (m *Module) obligations() application.Obligations { return comacl.Obligations{COM: m.com} }

func (m *Module) deps(tenant, commandID string) application.Deps {
	return application.Deps{
		Tx:        bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Shipments: postgres.ShipmentStore{DB: m.db},
		Receipts:  postgres.CommandReceipts{DB: m.db},
		Outbox:    postgres.Outbox{Writer: m.outbox, CommandID: commandID, RecordedAt: m.clock.Now()},
		Auth:      authorizer{m.auth},
		Clock:     m.clock,
		Window:    m.window,
	}
}

func scope(tenant, actor string) application.Scope {
	return application.Scope{TenantID: domain.TenantID(tenant), ActorID: actor}
}

// stored truncates to the microsecond precision of timestamptz, so a shipment reads back exactly as it was
// written.
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
	return api.Receipt{CommandID: r.CommandID, ShipmentID: string(r.ShipmentID), NewRevision: r.NewRevision}, nil
}

type authorizer struct{ api.Authorizer }

func (a authorizer) RequireShipmentAction(ctx context.Context, s application.Scope, action application.ShipmentAction, id domain.ShipmentID) error {
	return a.AuthorizeShipmentAction(ctx, string(s.TenantID), s.ActorID, string(action), string(id))
}

// classify wraps known FUL, kernel and Bento causes in their api error kind and passes anything else
// through.
func classify(err error) error {
	var kind error
	switch {
	case errors.Is(err, application.ErrUnauthenticated):
		kind = api.ErrUnauthenticated
	case errors.Is(err, application.ErrUnauthorized), errors.Is(err, bento.ErrTenantScopeConflict):
		kind = api.ErrUnauthorized
	case errors.Is(err, application.ErrIdempotencyConflict):
		kind = api.ErrIdempotencyConflict
	case errors.Is(err, domain.ErrConflict), errors.Is(err, bentorepo.ErrConflict), errors.Is(err, bentorepo.ErrAlreadyExists),
		errors.Is(err, application.ErrPlannedElsewhere):
		kind = api.ErrConflict
	case errors.Is(err, bentorepo.ErrNotFound):
		kind = api.ErrNotFound
	case errors.Is(err, domain.ErrInvalidShipment), errors.Is(err, domain.ErrInvalidObligation), errors.Is(err, domain.ErrInvalidPart),
		errors.Is(err, domain.ErrOtherCustomer),
		errors.Is(err, domain.ErrAlreadyPlanned), errors.Is(err, domain.ErrNotPlanned), errors.Is(err, domain.ErrLastObligation),
		errors.Is(err, domain.ErrInvalidPlan), errors.Is(err, domain.ErrIncompletePlan), errors.Is(err, domain.ErrInvalidTransition),
		errors.Is(err, domain.ErrInvalidChange), errors.Is(err, application.ErrNotPlannable), errors.Is(err, application.ErrInvalidQuery),
		errors.Is(err, kernel.ErrUnitMismatch), errors.Is(err, kernel.ErrAmountOverflow),
		errors.Is(err, domain.ErrUnresolvedRoute), errors.Is(err, domain.ErrModeMismatch), errors.Is(err, application.ErrLocationRefused),
		errors.Is(err, kernel.ErrOutsideRecordWindow),
		errors.Is(err, domain.ErrConsolPlanMismatch), errors.Is(err, domain.ErrNotAttached), errors.Is(err, application.ErrMembershipMismatch):
		kind = api.ErrRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
