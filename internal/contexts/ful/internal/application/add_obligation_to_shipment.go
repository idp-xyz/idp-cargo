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
	OccurredAt        time.Time
}

// AddObligationToShipment plans one more obligation version that COM reports ACTIVE, current and reserved
// for this shipment.
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
	return h.change(ctx, c.Scope, c.CommandID, hash, c.ShipmentID, c.ExpectedRevision, func(s *ful.Shipment) error {
		return s.AddObligation(ob, c.Scope.ActorID, c.OccurredAt)
	})
}
