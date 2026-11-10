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

// AmendShipmentPlan writes a new plan version with a changed route and a reason. The route's locations are
// resolved against the location reference data first (F2), and its mode must fit what the orders of the
// planned obligations request (C11); both questions are asked outside FUL's transaction.
type AmendShipmentPlan struct {
	Deps
	Locations Locations
	Modes     RequestedModes
}

func (h AmendShipmentPlan) Handle(ctx context.Context, c AmendShipmentPlanCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionAmendShipmentPlan, c.ShipmentID); err != nil {
		return Receipt{}, err
	}
	route, err := resolveRoute(ctx, h.Locations, c.Route)
	if err != nil {
		return Receipt{}, err
	}
	var refs []ful.ObligationRef
	if route.Mode != "" {
		if refs, err = requestedModes(ctx, h.Deps, h.Modes, c.ShipmentID, c.ExpectedRevision, c.Scope); err != nil {
			return Receipt{}, err
		}
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedRevision, func(s *ful.Shipment) error {
		if err := s.AmendPlan(route, c.Reason, c.Scope.ActorID, c.OccurredAt); err != nil {
			return err
		}
		return s.CheckRequestedModes(refs)
	})
}
