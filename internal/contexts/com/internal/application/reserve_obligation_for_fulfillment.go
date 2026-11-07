package application

import (
	"context"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

type ReserveObligationForFulfillmentCommand struct {
	CommandID         string
	Scope             Scope
	ObligationID      com.ObligationID
	ObligationVersion int64
	ShipmentID        string
	OccurredAt        time.Time
}

// ReserveObligationForFulfillment records that a FUL shipment holds an obligation version
// (ObligationReservation v1). FUL names the obligation, not the order, and carries no order revision: the
// order row lock orders the reservation against every other command on the order.
type ReserveObligationForFulfillment struct{ Deps }

func (h ReserveObligationForFulfillment) Handle(ctx context.Context, c ReserveObligationForFulfillmentCommand) (Receipt, error) {
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	order, err := h.orderOf(ctx, c.Scope.TenantID, c.ObligationID)
	if err != nil {
		return Receipt{}, err
	}
	if err = h.authorize(ctx, c.Scope, ActionReserveObligationForFulfillment, order); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, order, func(o *com.CustomerOrder, _ int64) error {
		return o.Reserve(c.ObligationID, c.ObligationVersion, c.ShipmentID, c.Scope.ActorID, c.OccurredAt)
	})
}
