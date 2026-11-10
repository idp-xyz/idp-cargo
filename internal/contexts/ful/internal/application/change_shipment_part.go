package application

import (
	"context"
	"time"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

type ChangeShipmentPartCommand struct {
	CommandID         string
	Scope             Scope
	ShipmentID        ful.ShipmentID
	ExpectedRevision  int64
	ObligationID      ful.ObligationID
	ObligationVersion int64
	Part              ful.CargoScope
	Reason            string
	OccurredAt        time.Time
}

// ChangeShipmentPart replans the part of an obligation version a shipment plans, with a reason (F1), such
// as a carrier's shortload. It asks COM nothing: the shipment already holds COM's reservation, and COM
// refuses to revoke a reserved obligation. What other shipments plan of the obligation is read under the
// obligation's lock in the command's transaction.
type ChangeShipmentPart struct{ Deps }

func (h ChangeShipmentPart) Handle(ctx context.Context, c ChangeShipmentPartCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionChangeShipmentPart, c.ShipmentID); err != nil {
		return Receipt{}, err
	}
	return h.changeIn(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedRevision, func(txCtx context.Context, s *ful.Shipment) error {
		elsewhere, err := h.Shipments.PlannedElsewhere(txCtx, c.Scope.TenantID, c.ObligationID, c.ObligationVersion, c.ShipmentID)
		if err != nil {
			return err
		}
		return s.ChangePart(c.ObligationID, c.ObligationVersion, c.Part, elsewhere, c.Reason, c.Scope.ActorID, c.OccurredAt)
	})
}
