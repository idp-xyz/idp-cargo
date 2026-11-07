package application

import (
	"context"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

type ReleaseObligationReservationCommand struct {
	CommandID         string
	Scope             Scope
	ObligationID      com.ObligationID
	ObligationVersion int64
	ShipmentID        string
	OccurredAt        time.Time
}

// ReleaseObligationReservation clears the reservation a FUL shipment holds on an obligation version,
// after FUL released the obligation or cancelled the shipment (ObligationReservation v1).
type ReleaseObligationReservation struct{ Deps }

func (h ReleaseObligationReservation) Handle(ctx context.Context, c ReleaseObligationReservationCommand) (Receipt, error) {
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	order, err := h.orderOf(ctx, c.Scope.TenantID, c.ObligationID)
	if err != nil {
		return Receipt{}, err
	}
	if err = h.authorize(ctx, c.Scope, ActionReleaseObligationReservation, order); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, order, func(o *com.CustomerOrder, _ int64) error {
		return o.ReleaseReservation(c.ObligationID, c.ObligationVersion, c.ShipmentID, c.Scope.ActorID, c.OccurredAt)
	})
}
