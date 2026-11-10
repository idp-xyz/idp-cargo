package application

import (
	"context"
	"time"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

type CreateShipmentFromObligationCommand struct {
	CommandID         string
	Scope             Scope
	ShipmentID        ful.ShipmentID
	ObligationID      ful.ObligationID
	ObligationVersion int64
	Part              ful.CargoScope `json:",omitzero"` // empty: all that other shipments leave of the obligation
	OccurredAt        time.Time
}

// CreateShipmentFromObligation starts a shipment from a part of an obligation version that COM reports
// ACTIVE, current and reserved for this shipment; what other shipments plan of it is read under the
// obligation's lock, in the same transaction as the insert.
type CreateShipmentFromObligation struct {
	Deps
	Obligations Obligations
}

func (h CreateShipmentFromObligation) Handle(ctx context.Context, c CreateShipmentFromObligationCommand) (Receipt, error) {
	if h.Obligations == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionCreateShipmentFromObligation, c.ShipmentID); err != nil {
		return Receipt{}, err
	}
	ob, err := h.Obligations.Plannable(ctx, c.Scope.TenantID, c.ObligationID, c.ObligationVersion, c.ShipmentID)
	if err != nil {
		return Receipt{}, err
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (*ful.Shipment, int64, error) {
		elsewhere, err := h.Shipments.PlannedElsewhere(txCtx, c.Scope.TenantID, c.ObligationID, c.ObligationVersion, c.ShipmentID)
		if err != nil {
			return nil, 0, err
		}
		part, err := partToPlan(ob, c.Part, elsewhere)
		if err != nil {
			return nil, 0, err
		}
		shipment, err := ful.CreateShipmentForPart(c.Scope.TenantID, c.ShipmentID, ob, part, elsewhere, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return nil, 0, err
		}
		shipment.Stamp(entered)
		revision, err := h.Shipments.Insert(txCtx, shipment)
		return shipment, revision, err
	})
}
