package application

import (
	"context"
	"time"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

type ConfirmShipmentPlanCommand struct {
	CommandID        string
	Scope            Scope
	ShipmentID       ful.ShipmentID
	ExpectedRevision int64
	OccurredAt       time.Time
}

// ConfirmShipmentPlan confirms the latest plan version once COM re-confirms every planned obligation
// ACTIVE, at its planned version and reserved for this shipment. The obligations are read at the
// expected revision and the commit checks that revision again, so the confirmed plan is the one checked.
type ConfirmShipmentPlan struct {
	Deps
	Obligations Obligations
}

func (h ConfirmShipmentPlan) Handle(ctx context.Context, c ConfirmShipmentPlanCommand) (Receipt, error) {
	if h.Obligations == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionConfirmShipmentPlan, c.ShipmentID); err != nil {
		return Receipt{}, err
	}
	var planned []ful.ObligationRef
	err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		shipment, revision, err := h.Shipments.Read(txCtx, c.Scope.TenantID, c.ShipmentID)
		if err != nil {
			return err
		}
		if revision != c.ExpectedRevision {
			return ful.ErrConflict
		}
		planned = shipment.Planned()
		return nil
	})
	if err != nil {
		return Receipt{}, err
	}
	for _, ob := range planned {
		if _, err = h.Obligations.Plannable(ctx, c.Scope.TenantID, ob.ObligationID, ob.ObligationVersion, c.ShipmentID); err != nil {
			return Receipt{}, err
		}
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, c.ShipmentID, c.ExpectedRevision, func(s *ful.Shipment) error {
		return s.Confirm(c.Scope.ActorID, c.OccurredAt)
	})
}
