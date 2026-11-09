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
	OccurredAt        time.Time
}

// CreateShipmentFromObligation starts a shipment from an obligation version that COM reports ACTIVE,
// current and reserved for this shipment.
type CreateShipmentFromObligation struct {
	Deps
	Obligations Obligations
}

func (h CreateShipmentFromObligation) Handle(ctx context.Context, c CreateShipmentFromObligationCommand) (Receipt, error) {
	if h.Obligations == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
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
	shipment, err := ful.CreateShipment(c.Scope.TenantID, c.ShipmentID, ob, c.Scope.ActorID, c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (*ful.Shipment, int64, error) {
		revision, err := h.Shipments.Insert(txCtx, shipment)
		return shipment, revision, err
	})
}
