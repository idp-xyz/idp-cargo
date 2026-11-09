package application

import (
	"context"
	"time"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

type AmendShipmentPlanCommand struct {
	CommandID        string
	Scope            Scope
	ShipmentID       ful.ShipmentID
	ExpectedRevision int64
	Route            ful.Route
	Reason           string
	OccurredAt       time.Time
}

// AmendShipmentPlan writes a new plan version with a changed route and a reason.
type AmendShipmentPlan struct{ Deps }

func (h AmendShipmentPlan) Handle(ctx context.Context, c AmendShipmentPlanCommand) (Receipt, error) {
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionAmendShipmentPlan, c.ShipmentID); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, c.ShipmentID, c.ExpectedRevision, func(s *ful.Shipment) error {
		return s.AmendPlan(c.Route, c.Reason, c.Scope.ActorID, c.OccurredAt)
	})
}
