// Package com assembles the COM (Commercial Commitment) bounded context. Code outside COM uses it through
// package api; everything under internal/ is private to COM.
package com

import (
	"context"
	"errors"
	"fmt"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/com/api"
	orgacl "go.idp.xyz/idp-cargo/internal/contexts/com/internal/adapters/org"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/adapters/postgres"
	rateacl "go.idp.xyz/idp-cargo/internal/contexts/com/internal/adapters/rate"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Module wires COM's use cases to PostgreSQL through IDP-Bento-Go and to the query sides of ORG and RATE.
type Module struct {
	db     *bentopg.DB
	outbox *bentooutbox.Store
	org    orgapi.Queries
	rate   rateapi.Queries
	auth   api.Authorizer
	clock  bento.SystemClock
}

var (
	_ api.Commands               = (*Module)(nil)
	_ api.ObligationReservations = (*Module)(nil)
	_ api.Queries                = (*Module)(nil)
)

// New assembles COM. org answers customer checks and rate returns accepted quotes; the composition root
// passes their modules.
func New(db *bentopg.DB, org orgapi.Queries, rate rateapi.Queries, auth api.Authorizer) (*Module, error) {
	if db == nil || org == nil || rate == nil || auth == nil {
		return nil, errors.New("com: module needs a database, ORG queries, RATE queries and an authorizer")
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	return &Module{db: db, outbox: outbox, org: org, rate: rate, auth: auth}, nil
}

func (m *Module) RecordCustomerOrder(ctx context.Context, cmd api.RecordCustomerOrder) (api.Receipt, error) {
	lines := make([]domain.OrderLine, len(cmd.Lines))
	for i, l := range cmd.Lines {
		lines[i] = domain.OrderLine{Description: l.Description, PackageType: domain.PackageType(l.PackageType),
			Measure: domain.Measure{Packs: l.Packs, GrossWeight: l.GrossWeight, Volume: l.Volume}}
	}
	receipt, err := application.RecordCustomerOrder{
		Deps: m.deps(cmd.TenantID, cmd.CommandID), Quotes: rateacl.Quotes{RATE: m.rate}, Customers: orgacl.Customers{ORG: m.org},
	}.Handle(ctx, application.RecordCustomerOrderCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), OrderID: domain.OrderID(cmd.OrderID), QuoteID: cmd.QuoteID,
		CustomerPartyID: cmd.CustomerPartyID, Lines: lines, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) AllocateOrderToObligation(ctx context.Context, cmd api.AllocateOrderToObligation) (api.Receipt, error) {
	receipt, err := application.AllocateOrderToObligation{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.AllocateOrderToObligationCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), OrderID: domain.OrderID(cmd.OrderID),
		ExpectedRevision: cmd.ExpectedRevision, LineNo: cmd.LineNo, ObligationID: domain.ObligationID(cmd.ObligationID),
		Measure: domain.Measure{Packs: cmd.Packs, GrossWeight: cmd.GrossWeight, Volume: cmd.Volume}, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) RevokeObligation(ctx context.Context, cmd api.RevokeObligation) (api.Receipt, error) {
	receipt, err := application.RevokeObligation{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.RevokeObligationCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), OrderID: domain.OrderID(cmd.OrderID),
		ExpectedRevision: cmd.ExpectedRevision, ObligationID: domain.ObligationID(cmd.ObligationID), OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) AmendObligation(ctx context.Context, cmd api.AmendObligation) (api.Receipt, error) {
	receipt, err := application.AmendObligation{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.AmendObligationCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), OrderID: domain.OrderID(cmd.OrderID),
		ExpectedRevision: cmd.ExpectedRevision, ObligationID: domain.ObligationID(cmd.ObligationID),
		NewObligationID: domain.ObligationID(cmd.NewObligationID),
		Measure:         domain.Measure{Packs: cmd.Packs, GrossWeight: cmd.GrossWeight, Volume: cmd.Volume}, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) CancelCustomerOrder(ctx context.Context, cmd api.CancelCustomerOrder) (api.Receipt, error) {
	receipt, err := application.CancelCustomerOrder{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.CancelCustomerOrderCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), OrderID: domain.OrderID(cmd.OrderID),
		ExpectedRevision: cmd.ExpectedRevision, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) ReserveObligationForFulfillment(ctx context.Context, cmd api.ReserveObligationForFulfillment) (api.Receipt, error) {
	receipt, err := application.ReserveObligationForFulfillment{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx,
		application.ReserveObligationForFulfillmentCommand{
			CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), ObligationID: domain.ObligationID(cmd.ObligationID),
			ObligationVersion: cmd.ObligationVersion, ShipmentID: cmd.ShipmentID, OccurredAt: stored(cmd.OccurredAt),
		})
	return toReceipt(receipt, err)
}

func (m *Module) ReleaseObligationReservation(ctx context.Context, cmd api.ReleaseObligationReservation) (api.Receipt, error) {
	receipt, err := application.ReleaseObligationReservation{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx,
		application.ReleaseObligationReservationCommand{
			CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), ObligationID: domain.ObligationID(cmd.ObligationID),
			ObligationVersion: cmd.ObligationVersion, ShipmentID: cmd.ShipmentID, OccurredAt: stored(cmd.OccurredAt),
		})
	return toReceipt(receipt, err)
}

func (m *Module) GetObligationSnapshot(ctx context.Context, q api.GetObligationSnapshot) (api.ObligationSnapshot, error) {
	order, ob, err := m.obligation(ctx, q)
	if err != nil {
		return api.ObligationSnapshot{}, err
	}
	if ob.Status != domain.ObligationActive {
		return api.ObligationSnapshot{}, classify(fmt.Errorf("%w: version %d of obligation %s records its revocation, which ObligationSnapshot v1 does not show",
			bentorepo.ErrNotFound, ob.Version, ob.ObligationID))
	}
	s := order.State()
	l := s.Lines[ob.LineNo-1]
	return api.ObligationSnapshot{
		TenantID: string(s.TenantID), ObligationID: string(ob.ObligationID), ObligationVersion: ob.Version,
		OrderID: string(s.OrderID), OrderLineNo: ob.LineNo, Description: l.Description,
		Quantity:      kernel.Quantity{Unit: kernel.UnitCode("X" + string(l.PackageType)), Amount: ob.Measure.Packs},
		LegalEntityID: string(s.Quote.LegalEntityID), Customer: api.CustomerRef{PartyID: s.Customer.PartyID, PartyRevision: s.Customer.PartyRevision},
		QuoteID: s.Quote.QuoteID, QuoteRevision: s.Quote.QuoteRevision, AllocatedAt: ob.AllocatedAt,
	}, nil
}

func (m *Module) GetObligationSnapshotV2(ctx context.Context, q api.GetObligationSnapshot) (api.ObligationSnapshotV2, error) {
	order, ob, err := m.obligation(ctx, q)
	if err != nil {
		return api.ObligationSnapshotV2{}, err
	}
	s := order.State()
	l := s.Lines[ob.LineNo-1]
	latest, _ := order.Latest(ob.ObligationID)
	snapshot := api.ObligationSnapshotV2{
		TenantID: string(s.TenantID), ObligationID: string(ob.ObligationID), ObligationVersion: ob.Version, LatestVersion: latest.Version,
		Status: string(ob.Status), OrderID: string(s.OrderID), OrderLineNo: ob.LineNo, Description: l.Description,
		Packs: ob.Measure.Packs, PackageType: string(l.PackageType), GrossWeight: ob.Measure.GrossWeight, Volume: ob.Measure.Volume,
		LegalEntityID: string(s.Quote.LegalEntityID), Customer: api.CustomerRef{PartyID: s.Customer.PartyID, PartyRevision: s.Customer.PartyRevision},
		QuoteID: s.Quote.QuoteID, QuoteRevision: s.Quote.QuoteRevision, AllocatedAt: ob.AllocatedAt, RevokedAt: ob.RevokedAt,
	}
	if holders := holdersOf(order, ob); len(holders) == 1 {
		snapshot.HeldByShipmentID = holders[0]
	}
	return snapshot, nil
}

func (m *Module) GetObligationSnapshotV3(ctx context.Context, q api.GetObligationSnapshot) (api.ObligationSnapshotV3, error) {
	order, ob, err := m.obligation(ctx, q)
	if err != nil {
		return api.ObligationSnapshotV3{}, err
	}
	s := order.State()
	l := s.Lines[ob.LineNo-1]
	latest, _ := order.Latest(ob.ObligationID)
	return api.ObligationSnapshotV3{
		TenantID: string(s.TenantID), ObligationID: string(ob.ObligationID), ObligationVersion: ob.Version, LatestVersion: latest.Version,
		Status: string(ob.Status), OrderID: string(s.OrderID), OrderLineNo: ob.LineNo, Description: l.Description,
		Packs: ob.Measure.Packs, PackageType: string(l.PackageType), GrossWeight: ob.Measure.GrossWeight, Volume: ob.Measure.Volume,
		LegalEntityID: string(s.Quote.LegalEntityID), Customer: api.CustomerRef{PartyID: s.Customer.PartyID, PartyRevision: s.Customer.PartyRevision},
		QuoteID: s.Quote.QuoteID, QuoteRevision: s.Quote.QuoteRevision, AllocatedAt: ob.AllocatedAt, RevokedAt: ob.RevokedAt,
		HeldByShipmentIDs: holdersOf(order, ob),
	}, nil
}

// holdersOf returns the shipments holding an obligation version, in the order they reserved it.
func holdersOf(order *domain.CustomerOrder, ob domain.Obligation) []string {
	var holders []string
	for _, r := range order.Holders(ob.ObligationID) {
		if r.ObligationVersion == ob.Version {
			holders = append(holders, r.ShipmentID)
		}
	}
	return holders
}

func (m *Module) obligation(ctx context.Context, q api.GetObligationSnapshot) (*domain.CustomerOrder, domain.Obligation, error) {
	order, ob, err := application.GetObligationSnapshot{
		Tx: bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}, Orders: postgres.OrderReader{DB: m.db},
	}.Handle(ctx, domain.TenantID(q.TenantID), domain.ObligationID(q.ObligationID), q.Version)
	if err != nil {
		return nil, domain.Obligation{}, classify(err)
	}
	return order, ob, nil
}

func (m *Module) deps(tenant, commandID string) application.Deps {
	return application.Deps{
		Tx:       bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Orders:   postgres.OrderStore{DB: m.db},
		Receipts: postgres.CommandReceipts{DB: m.db},
		Outbox:   postgres.Outbox{Writer: m.outbox, CommandID: commandID, RecordedAt: m.clock.Now()},
		Auth:     authorizer{m.auth},
	}
}

func scope(tenant, actor string) application.Scope {
	return application.Scope{TenantID: domain.TenantID(tenant), ActorID: actor}
}

// stored truncates to the microsecond precision of timestamptz, so an order reads back exactly as it was
// recorded.
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
	return api.Receipt{CommandID: r.CommandID, OrderID: string(r.OrderID), NewRevision: r.NewRevision}, nil
}

type authorizer struct{ api.Authorizer }

func (a authorizer) RequireOrderAction(ctx context.Context, s application.Scope, action application.OrderAction, id domain.OrderID) error {
	return a.AuthorizeOrderAction(ctx, string(s.TenantID), s.ActorID, string(action), string(id))
}

// classify wraps known COM, kernel and Bento causes in their api error kind and passes anything else
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
		errors.Is(err, application.ErrQuoteAlreadyOrdered):
		kind = api.ErrConflict
	case errors.Is(err, bentorepo.ErrNotFound), errors.Is(err, domain.ErrUnknownObligation):
		kind = api.ErrNotFound
	case errors.Is(err, domain.ErrInvalidOrder), errors.Is(err, domain.ErrQuoteMismatch), errors.Is(err, domain.ErrOutsideQuoteValidity),
		errors.Is(err, domain.ErrInvalidAllocation), errors.Is(err, domain.ErrOverAllocation), errors.Is(err, domain.ErrInvalidTransition),
		errors.Is(err, domain.ErrInvalidChange), errors.Is(err, domain.ErrObligationNotActive), errors.Is(err, domain.ErrObligationReserved),
		errors.Is(err, domain.ErrNotReserved), errors.Is(err, application.ErrQuoteNotAccepted), errors.Is(err, application.ErrCustomerNotConfirmed),
		errors.Is(err, application.ErrInvalidQuery), errors.Is(err, kernel.ErrUnitMismatch), errors.Is(err, kernel.ErrAmountOverflow):
		kind = api.ErrRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
