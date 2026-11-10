package application

import (
	"context"
	"time"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

type CancelShipmentCommand struct {
	CommandID        string
	Scope            Scope
	ShipmentID       ful.ShipmentID
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// CancelShipment cancels a shipment and releases every obligation it plans.
type CancelShipment struct{ Deps }

func (h CancelShipment) Handle(ctx context.Context, c CancelShipmentCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionCancelShipment, c.ShipmentID); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedRevision, func(s *ful.Shipment) error {
		return s.Cancel(c.Reason, c.Scope.ActorID, c.OccurredAt)
	})
}
