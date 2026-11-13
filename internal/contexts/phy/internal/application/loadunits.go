package application

import (
	"context"
	"time"

	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

// Equipment resolves a container size-type against the current edition of the equipment reference data (PB8) and
// answers its limits with the edition. It is called outside PHY's transactions.
type Equipment interface {
	ContainerType(ctx context.Context, code string) (phy.UnitLimits, error)
}

type PlanLoadUnitCommand struct {
	CommandID        string
	Scope            Scope
	ConsolidationID  phy.ConsolidationID
	ExpectedRevision int64
	Kind             phy.LoadUnitKind
	TypeCode         string
	OccurredAt       time.Time
}

// PlanLoadUnit plans a load unit for a consolidation, its limits frozen from the reference data with the edition.
// The receipt's item is the unit's number.
type PlanLoadUnit struct {
	Deps
	Equipment Equipment
}

func (h PlanLoadUnit) Handle(ctx context.Context, c PlanLoadUnitCommand) (Receipt, error) {
	if h.Equipment == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionPlanLoadUnit, c.ConsolidationID); err != nil {
		return Receipt{}, err
	}
	var limits phy.UnitLimits
	if c.Kind == phy.UnitContainer { // other kinds are refused by the consolidation
		if limits, err = h.Equipment.ContainerType(ctx, c.TypeCode); err != nil {
			return Receipt{}, err
		}
	}
	return h.changeConsolidationItem(ctx, c.Scope, c.CommandID, hash, entered, c.ConsolidationID, c.ExpectedRevision,
		func(_ context.Context, consol *phy.Consolidation) (int, error) {
			return consol.PlanLoadUnit(c.Kind, limits, c.Scope.ActorID, c.OccurredAt)
		})
}

type RemovePlannedLoadUnitCommand struct {
	CommandID        string
	Scope            Scope
	ConsolidationID  phy.ConsolidationID
	ExpectedRevision int64
	UnitNo           int
	Reason           string
	OccurredAt       time.Time
}

// RemovePlannedLoadUnit removes a planned load unit with a reason. The receipt's item is the unit's number.
type RemovePlannedLoadUnit struct{ Deps }

func (h RemovePlannedLoadUnit) Handle(ctx context.Context, c RemovePlannedLoadUnitCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionRemovePlannedLoadUnit, c.ConsolidationID); err != nil {
		return Receipt{}, err
	}
	return h.changeConsolidationItem(ctx, c.Scope, c.CommandID, hash, entered, c.ConsolidationID, c.ExpectedRevision,
		func(_ context.Context, consol *phy.Consolidation) (int, error) {
			return c.UnitNo, consol.RemovePlannedLoadUnit(c.UnitNo, c.Reason, c.Scope.ActorID, c.OccurredAt)
		})
}
