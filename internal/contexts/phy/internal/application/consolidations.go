package application

import (
	"context"
	"fmt"
	"time"

	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

type CreateConsolidationCommand struct {
	CommandID        string
	Scope            Scope
	ConsolidationID  phy.ConsolidationID
	Mode             phy.Mode
	Kind             phy.Kind
	Operator         string
	Origin           kernel.LocationCode
	Destination      kernel.LocationCode
	PlannedDeparture time.Time
	HouseCutOff      time.Time
	OccurredAt       time.Time
}

// CreateConsolidation opens a consolidation on a route resolved against the location reference data.
type CreateConsolidation struct {
	Deps
	Locations Locations
}

func (h CreateConsolidation) Handle(ctx context.Context, c CreateConsolidationCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionCreateConsolidation, c.ConsolidationID); err != nil {
		return Receipt{}, err
	}
	route, err := resolveRoute(ctx, h.Locations, c.Mode, c.Origin, c.Destination)
	if err != nil {
		return Receipt{}, err
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (Receipt, aggregate, error) {
		consol, err := phy.CreateConsolidation(c.Scope.TenantID, c.ConsolidationID, c.Mode, c.Kind, c.Operator, route,
			c.PlannedDeparture, c.HouseCutOff, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		consol.Stamp(entered)
		revision, err := h.Consolidations.Insert(txCtx, consol)
		return Receipt{ConsolidationID: c.ConsolidationID, NewRevision: revision}, consol, err
	})
}

type AmendConsolidationCommand struct {
	CommandID        string
	Scope            Scope
	ConsolidationID  phy.ConsolidationID
	ExpectedRevision int64
	Origin           kernel.LocationCode `json:",omitzero"`
	Destination      kernel.LocationCode `json:",omitzero"`
	PlannedDeparture time.Time
	HouseCutOff      time.Time
	Reason           string
	OccurredAt       time.Time
}

// AmendConsolidation writes a new consolidation version; a new route is resolved first, and refused while
// houses are attached.
type AmendConsolidation struct {
	Deps
	Locations Locations
}

func (h AmendConsolidation) Handle(ctx context.Context, c AmendConsolidationCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionAmendConsolidation, c.ConsolidationID); err != nil {
		return Receipt{}, err
	}
	var route phy.Route
	if !c.Origin.IsZero() || !c.Destination.IsZero() {
		mode, err := h.mode(ctx, c.Scope.TenantID, c.ConsolidationID)
		if err != nil {
			return Receipt{}, err
		}
		if route, err = resolveRoute(ctx, h.Locations, mode, c.Origin, c.Destination); err != nil {
			return Receipt{}, err
		}
	}
	return h.changeConsolidation(ctx, c.Scope, c.CommandID, hash, entered, c.ConsolidationID, c.ExpectedRevision,
		func(txCtx context.Context, consol *phy.Consolidation) error {
			houses, err := h.Consolidations.LiveHouses(txCtx, c.Scope.TenantID, c.ConsolidationID)
			if err != nil {
				return err
			}
			return consol.Amend(route, c.PlannedDeparture, c.HouseCutOff, c.Reason, c.Scope.ActorID, c.OccurredAt, len(houses))
		})
}

// mode reads a consolidation's mode, which never changes, to resolve a new route for it.
func (h AmendConsolidation) mode(ctx context.Context, tenant phy.TenantID, id phy.ConsolidationID) (phy.Mode, error) {
	var mode phy.Mode
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		c, _, err := h.Consolidations.Read(txCtx, tenant, id)
		if err == nil {
			mode = c.Mode()
		}
		return err
	})
	return mode, err
}

type CloseConsolidationCommand struct {
	CommandID        string
	Scope            Scope
	ConsolidationID  phy.ConsolidationID
	ExpectedRevision int64
	OccurredAt       time.Time
}

// CloseConsolidation closes a consolidation after FUL re-confirmed every live house as PHY verified it (T3).
// The houses are read again under the consolidation's FOR UPDATE lock; if they changed meanwhile, the close
// is a conflict and the caller tries again.
type CloseConsolidation struct {
	Deps
	Intents LoadIntents
}

func (h CloseConsolidation) Handle(ctx context.Context, c CloseConsolidationCommand) (Receipt, error) {
	if h.Intents == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionCloseConsolidation, c.ConsolidationID); err != nil {
		return Receipt{}, err
	}
	var checked []House
	if err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		checked, err = h.Consolidations.LiveHouses(txCtx, c.Scope.TenantID, c.ConsolidationID)
		return err
	}); err != nil {
		return Receipt{}, err
	}
	for _, house := range checked {
		intent, err := h.Intents.LoadIntent(ctx, c.Scope.TenantID, house.ShipmentID)
		if err != nil {
			return Receipt{}, err
		}
		if err = house.Entry.Confirm(intent); err != nil {
			return Receipt{}, err
		}
	}
	return h.changeConsolidation(ctx, c.Scope, c.CommandID, hash, entered, c.ConsolidationID, c.ExpectedRevision,
		func(txCtx context.Context, consol *phy.Consolidation) error {
			houses, err := h.Consolidations.LiveHouses(txCtx, c.Scope.TenantID, c.ConsolidationID)
			if err != nil {
				return err
			}
			if !sameHouses(checked, houses) {
				return fmt.Errorf("%w: the houses changed while FUL was asked; try again", phy.ErrConflict)
			}
			var last time.Time
			for _, house := range houses {
				if house.Entry.At.After(last) {
					last = house.Entry.At
				}
			}
			return consol.Close(len(houses), last, c.Scope.ActorID, c.OccurredAt)
		})
}

func sameHouses(a, b []House) bool {
	if len(a) != len(b) {
		return false
	}
	entries := map[phy.ShipmentID]int{}
	for _, h := range a {
		entries[h.ShipmentID] = h.Entry.No
	}
	for _, h := range b {
		if no, ok := entries[h.ShipmentID]; !ok || no != h.Entry.No {
			return false
		}
	}
	return true
}

type ReopenConsolidationCommand struct {
	CommandID        string
	Scope            Scope
	ConsolidationID  phy.ConsolidationID
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// ReopenConsolidation reopens a closed consolidation with a reason.
type ReopenConsolidation struct{ Deps }

func (h ReopenConsolidation) Handle(ctx context.Context, c ReopenConsolidationCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionReopenConsolidation, c.ConsolidationID); err != nil {
		return Receipt{}, err
	}
	return h.changeConsolidation(ctx, c.Scope, c.CommandID, hash, entered, c.ConsolidationID, c.ExpectedRevision,
		func(_ context.Context, consol *phy.Consolidation) error {
			return consol.Reopen(c.Reason, c.Scope.ActorID, c.OccurredAt)
		})
}

type CancelConsolidationCommand struct {
	CommandID        string
	Scope            Scope
	ConsolidationID  phy.ConsolidationID
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// CancelConsolidation cancels an open consolidation without houses.
type CancelConsolidation struct{ Deps }

func (h CancelConsolidation) Handle(ctx context.Context, c CancelConsolidationCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionCancelConsolidation, c.ConsolidationID); err != nil {
		return Receipt{}, err
	}
	return h.changeConsolidation(ctx, c.Scope, c.CommandID, hash, entered, c.ConsolidationID, c.ExpectedRevision,
		func(txCtx context.Context, consol *phy.Consolidation) error {
			houses, err := h.Consolidations.LiveHouses(txCtx, c.Scope.TenantID, c.ConsolidationID)
			if err != nil {
				return err
			}
			return consol.Cancel(c.Reason, c.Scope.ActorID, c.OccurredAt, len(houses))
		})
}
