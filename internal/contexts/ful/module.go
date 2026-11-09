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
	"go.idp.xyz/idp-cargo/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Module wires FUL's use cases to PostgreSQL through IDP-Bento-Go and to COM's query side.
type Module struct {
	db     *bentopg.DB
	outbox *bentooutbox.Store
	com    comapi.Queries
	auth   api.Authorizer
	clock  bento.SystemClock
}

var (
	_ api.Commands = (*Module)(nil)
	_ api.Queries  = (*Module)(nil)
)

// New assembles FUL. com answers which obligation versions a shipment may plan; the composition root
// passes COM's module. FUL never writes to COM: the caller reserves and releases obligations through
// COM's ObligationReservation v1 (rule T6 in S1).
func New(db *bentopg.DB, com comapi.Queries, auth api.Authorizer) (*Module, error) {
	if db == nil || com == nil || auth == nil {
		return nil, errors.New("ful: module needs a database, COM queries and an authorizer")
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	return &Module{db: db, outbox: outbox, com: com, auth: auth}, nil
}

func (m *Module) CreateShipmentFromObligation(ctx context.Context, cmd api.CreateShipmentFromObligation) (api.Receipt, error) {
	receipt, err := application.CreateShipmentFromObligation{Deps: m.deps(cmd.TenantID, cmd.CommandID), Obligations: m.obligations()}.Handle(ctx,
		application.CreateShipmentFromObligationCommand{
			CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
			ObligationID: domain.ObligationID(cmd.ObligationID), ObligationVersion: cmd.ObligationVersion, OccurredAt: stored(cmd.OccurredAt),
		})
	return toReceipt(receipt, err)
}

func (m *Module) AddObligationToShipment(ctx context.Context, cmd api.AddObligationToShipment) (api.Receipt, error) {
	receipt, err := application.AddObligationToShipment{Deps: m.deps(cmd.TenantID, cmd.CommandID), Obligations: m.obligations()}.Handle(ctx,
		application.AddObligationToShipmentCommand{
			CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), ShipmentID: domain.ShipmentID(cmd.ShipmentID),
			ExpectedRevision: cmd.ExpectedRevision, ObligationID: domain.ObligationID(cmd.ObligationID), ObligationVersion: cmd.ObligationVersion,
			OccurredAt: stored(cmd.OccurredAt),
		})
	return toReceipt(receipt, err)
}

func (m *Module) AmendShipmentPlan(ctx context.Context, cmd api.AmendShipmentPlan) (api.Receipt, error) {
	route := domain.Route{Mode: domain.TransportMode(cmd.TransportMode), Origin: cmd.Origin, Destination: cmd.Destination,
		Departure: kernel.ValidityInterval{From: stored(cmd.DepartureWindow.From), Until: stored(cmd.DepartureWindow.Until)}}
	receipt, err := application.AmendShipmentPlan{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.AmendShipmentPlanCommand{
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

func (m *Module) GetShipmentPlanSnapshot(ctx context.Context, q api.GetShipmentPlanSnapshot) (api.ShipmentPlanSnapshot, error) {
	shipment, revision, err := application.GetShipmentPlan{
		Tx: bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}, Shipments: postgres.ShipmentStore{DB: m.db},
	}.Handle(ctx, domain.TenantID(q.TenantID), domain.ShipmentID(q.ShipmentID))
	if err != nil {
		return api.ShipmentPlanSnapshot{}, classify(err)
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

func (m *Module) obligations() application.Obligations { return comacl.Obligations{COM: m.com} }

func (m *Module) deps(tenant, commandID string) application.Deps {
	return application.Deps{
		Tx:        bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Shipments: postgres.ShipmentStore{DB: m.db},
		Receipts:  postgres.CommandReceipts{DB: m.db},
		Outbox:    postgres.Outbox{Writer: m.outbox, CommandID: commandID, RecordedAt: m.clock.Now()},
		Auth:      authorizer{m.auth},
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
	case errors.Is(err, domain.ErrInvalidShipment), errors.Is(err, domain.ErrInvalidObligation), errors.Is(err, domain.ErrOtherCustomer),
		errors.Is(err, domain.ErrAlreadyPlanned), errors.Is(err, domain.ErrNotPlanned), errors.Is(err, domain.ErrLastObligation),
		errors.Is(err, domain.ErrInvalidPlan), errors.Is(err, domain.ErrIncompletePlan), errors.Is(err, domain.ErrInvalidTransition),
		errors.Is(err, domain.ErrInvalidChange), errors.Is(err, application.ErrNotPlannable), errors.Is(err, application.ErrInvalidQuery),
		errors.Is(err, kernel.ErrUnitMismatch), errors.Is(err, kernel.ErrAmountOverflow):
		kind = api.ErrRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
