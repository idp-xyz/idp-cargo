package application

import (
	"context"
	"time"

	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

type AttachHouseCommand struct {
	CommandID          string
	Scope              Scope
	ConsolidationID    phy.ConsolidationID
	ShipmentID         phy.ShipmentID
	ExpectedMembership int64
	OccurredAt         time.Time
}

// AttachHouse reads FUL's LoadIntent v1 before PHY's transaction (T3) and attaches the shipment under the
// consolidation's FOR SHARE lock, which a concurrent close waits for.
type AttachHouse struct {
	Deps
	Intents LoadIntents
}

func (h AttachHouse) Handle(ctx context.Context, c AttachHouseCommand) (Receipt, error) {
	if h.Intents == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionAttachHouse, c.ConsolidationID); err != nil {
		return Receipt{}, err
	}
	intent, err := h.Intents.LoadIntent(ctx, c.Scope.TenantID, c.ShipmentID)
	if err != nil {
		return Receipt{}, err
	}
	return h.changeHouse(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedMembership, []phy.ConsolidationID{c.ConsolidationID},
		func(_ context.Context, m *phy.Membership, consols []*phy.Consolidation) error {
			return m.Attach(consols[0], intent, c.Scope.ActorID, c.OccurredAt)
		})
}

type DetachHouseCommand struct {
	CommandID          string
	Scope              Scope
	ConsolidationID    phy.ConsolidationID
	ShipmentID         phy.ShipmentID
	ExpectedMembership int64
	Reason             phy.Reason
	ReasonText         string
	OccurredAt         time.Time
}

// DetachHouse takes a house out of an open consolidation, or out of a closed one when it has no live packs in it: it
// was unloaded whole in the rework of one of its sealed containers (V2.86), or, in a sea consolidation, it was shut out
// before stuffing or taken out by ordinary reversals (V2.87).
type DetachHouse struct{ Deps }

func (h DetachHouse) Handle(ctx context.Context, c DetachHouseCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionDetachHouse, c.ConsolidationID); err != nil {
		return Receipt{}, err
	}
	return h.changeHouse(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedMembership, []phy.ConsolidationID{c.ConsolidationID},
		func(txCtx context.Context, m *phy.Membership, consols []*phy.Consolidation) error {
			if err := h.unloaded(txCtx, c.Scope.TenantID, consols[0].ID(), c.ShipmentID); err != nil {
				return err
			}
			rework, lastReversed, err := h.leavingFacts(txCtx, c.Scope.TenantID, consols[0], c.ShipmentID)
			if err != nil {
				return err
			}
			if rework != nil {
				return m.DetachAfterRework(consols[0], *rework, c.Reason, c.ReasonText, c.Scope.ActorID, c.OccurredAt)
			}
			return m.DetachAfterReversals(consols[0], lastReversed, c.Reason, c.ReasonText, c.Scope.ActorID, c.OccurredAt)
		})
}

type TransferHouseCommand struct {
	CommandID          string
	Scope              Scope
	From               phy.ConsolidationID
	To                 phy.ConsolidationID
	ShipmentID         phy.ShipmentID
	ExpectedMembership int64
	Reason             phy.Reason
	ReasonText         string
	OccurredAt         time.Time
}

// TransferHouse moves a house between two open consolidations in one transaction, or from a closed one when it has no
// live packs in it, on the same terms as DetachHouse (V2.86, V2.87); FUL's plan is read first for the attach checks on
// the target.
type TransferHouse struct {
	Deps
	Intents LoadIntents
}

func (h TransferHouse) Handle(ctx context.Context, c TransferHouseCommand) (Receipt, error) {
	if h.Intents == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionTransferHouse, c.From, c.To); err != nil {
		return Receipt{}, err
	}
	intent, err := h.Intents.LoadIntent(ctx, c.Scope.TenantID, c.ShipmentID)
	if err != nil {
		return Receipt{}, err
	}
	ids := []phy.ConsolidationID{c.From, c.To}
	if c.From == c.To {
		ids = ids[:1]
	}
	return h.changeHouse(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedMembership, ids,
		func(txCtx context.Context, m *phy.Membership, consols []*phy.Consolidation) error {
			if err := h.unloaded(txCtx, c.Scope.TenantID, consols[0].ID(), c.ShipmentID); err != nil {
				return err
			}
			rework, lastReversed, err := h.leavingFacts(txCtx, c.Scope.TenantID, consols[0], c.ShipmentID)
			if err != nil {
				return err
			}
			if rework != nil {
				return m.TransferAfterRework(consols[0], consols[len(consols)-1], intent, *rework, c.Reason, c.ReasonText, c.Scope.ActorID, c.OccurredAt)
			}
			return m.TransferAfterReversals(consols[0], consols[len(consols)-1], intent, lastReversed, c.Reason, c.ReasonText, c.Scope.ActorID, c.OccurredAt)
		})
}

type RefreshHouseIntentCommand struct {
	CommandID          string
	Scope              Scope
	ConsolidationID    phy.ConsolidationID
	ShipmentID         phy.ShipmentID
	ExpectedMembership int64
	OccurredAt         time.Time
}

// RefreshHouseIntent takes FUL's changed plan into the house's membership.
type RefreshHouseIntent struct {
	Deps
	Intents LoadIntents
}

func (h RefreshHouseIntent) Handle(ctx context.Context, c RefreshHouseIntentCommand) (Receipt, error) {
	if h.Intents == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionRefreshHouseIntent, c.ConsolidationID); err != nil {
		return Receipt{}, err
	}
	intent, err := h.Intents.LoadIntent(ctx, c.Scope.TenantID, c.ShipmentID)
	if err != nil {
		return Receipt{}, err
	}
	return h.changeHouse(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedMembership, []phy.ConsolidationID{c.ConsolidationID},
		func(txCtx context.Context, m *phy.Membership, consols []*phy.Consolidation) error {
			loaded, err := h.Executions.HouseLoaded(txCtx, c.Scope.TenantID, consols[0].ID(), c.ShipmentID)
			if err != nil {
				return err
			}
			if err = phy.Replannable(intent, loaded); err != nil {
				return err
			}
			return m.Refresh(consols[0], intent, c.Scope.ActorID, c.OccurredAt)
		})
}

type EndMembershipCommand struct {
	CommandID          string
	Scope              Scope
	ConsolidationID    phy.ConsolidationID
	ShipmentID         phy.ShipmentID
	ExpectedMembership int64
	OccurredAt         time.Time
}

// EndMembershipForCancelledShipment ends the membership of a shipment FUL reports cancelled.
type EndMembershipForCancelledShipment struct {
	Deps
	Intents LoadIntents
}

func (h EndMembershipForCancelledShipment) Handle(ctx context.Context, c EndMembershipCommand) (Receipt, error) {
	if h.Intents == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionEndMembershipForCancelledShipment, c.ConsolidationID); err != nil {
		return Receipt{}, err
	}
	intent, err := h.Intents.LoadIntent(ctx, c.Scope.TenantID, c.ShipmentID)
	if err != nil {
		return Receipt{}, err
	}
	return h.changeHouse(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedMembership, []phy.ConsolidationID{c.ConsolidationID},
		func(txCtx context.Context, m *phy.Membership, consols []*phy.Consolidation) error {
			if err := h.unloaded(txCtx, c.Scope.TenantID, consols[0].ID(), c.ShipmentID); err != nil {
				return err
			}
			return m.EndForCancelledShipment(consols[0], intent, c.Scope.ActorID, c.OccurredAt)
		})
}

// unloaded checks, under the house's ledger lock, that nothing of the house is loaded in the consolidation it
// leaves: it is unloaded first (reversal lines), so that conservation holds (PHY V0.2).
func (d Deps) unloaded(ctx context.Context, tenant phy.TenantID, consolidation phy.ConsolidationID, shipment phy.ShipmentID) error {
	loaded, err := d.Executions.HouseLoaded(ctx, tenant, consolidation, shipment)
	if err != nil {
		return err
	}
	return phy.Leavable(loaded)
}

// leavingFacts finds, under the house's ledger lock and inside the command's transaction, what a house leaving a CLOSED
// consolidation is checked against: the rework that unloaded it, or, when no rework of this consolidation did, the
// business time of its latest reversal here (the zero time when it was never loaded; V2.87). A rework also exists in
// the history of a consolidation that was emptied, its container given up and the consolidation reopened (OPEN again),
// and of a RECONCILED one; neither lets a house go for it. It returns nothing for any other state. A rework of another
// consolidation, or of another house, is not looked at: HouseRework reads this house's reversals in this consolidation.
func (d Deps) leavingFacts(ctx context.Context, tenant phy.TenantID, c *phy.Consolidation, shipment phy.ShipmentID) (*phy.ReworkRef, time.Time, error) {
	if c.Current().State != phy.StateClosed {
		return nil, time.Time{}, nil
	}
	rework, found, err := d.Executions.HouseRework(ctx, tenant, c.ID(), shipment)
	if err != nil {
		return nil, time.Time{}, err
	}
	if found {
		return &rework, time.Time{}, nil
	}
	last, _, err := d.Executions.HouseLastReversal(ctx, tenant, c.ID(), shipment)
	return nil, last, err
}
