package application

import (
	"context"
	"time"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

type AddObligationToShipmentCommand struct {
	CommandID         string
	Scope             Scope
	ShipmentID        ful.ShipmentID
	ExpectedRevision  int64
	ObligationID      ful.ObligationID
	ObligationVersion int64
	Part              ful.CargoScope `json:",omitzero"` // empty: all that other shipments leave of the obligation
	OccurredAt        time.Time
}

// AddObligationToShipment plans a part of one more obligation version that COM reports ACTIVE, current
// and reserved for this shipment.
type AddObligationToShipment struct {
	Deps
	Obligations Obligations
}

func (h AddObligationToShipment) Handle(ctx context.Context, c AddObligationToShipmentCommand) (Receipt, error) {
	if h.Obligations == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionAddObligationToShipment, c.ShipmentID); err != nil {
		return Receipt{}, err
	}
	ob, err := h.Obligations.Plannable(ctx, c.Scope.TenantID, c.ObligationID, c.ObligationVersion, c.ShipmentID)
	if err != nil {
		return Receipt{}, err
	}
	return h.changeIn(ctx, c.Scope, c.CommandID, hash, c.ShipmentID, c.ExpectedRevision, func(txCtx context.Context, s *ful.Shipment) error {
		elsewhere, err := h.Shipments.PlannedElsewhere(txCtx, c.Scope.TenantID, c.ObligationID, c.ObligationVersion, c.ShipmentID)
		if err != nil {
			return err
		}
		part, err := partToPlan(ob, c.Part, elsewhere)
		if err != nil {
			return err
		}
		return s.AddPart(ob, part, elsewhere, c.Scope.ActorID, c.OccurredAt)
	})
}
