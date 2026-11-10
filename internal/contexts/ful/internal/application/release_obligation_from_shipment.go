package application

import (
	"context"
	"time"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

type ReleaseObligationFromShipmentCommand struct {
	CommandID         string
	Scope             Scope
	ShipmentID        ful.ShipmentID
	ExpectedRevision  int64
	ObligationID      ful.ObligationID
	ObligationVersion int64
	OccurredAt        time.Time
}

// ReleaseObligationFromShipment stops planning an obligation version. It asks COM nothing: FUL releases
// first, and COM's reservation is released after it (the C6 handshake).
type ReleaseObligationFromShipment struct{ Deps }

func (h ReleaseObligationFromShipment) Handle(ctx context.Context, c ReleaseObligationFromShipmentCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionReleaseObligationFromShipment, c.ShipmentID); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedRevision, func(s *ful.Shipment) error {
		return s.ReleaseObligation(c.ObligationID, c.ObligationVersion, c.Scope.ActorID, c.OccurredAt)
	})
}
