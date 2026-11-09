package application

import (
	"context"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

type AmendObligationCommand struct {
	CommandID        string
	Scope            Scope
	OrderID          com.OrderID
	ExpectedRevision int64
	ObligationID     com.ObligationID
	NewObligationID  com.ObligationID
	Measure          com.Measure
	OccurredAt       time.Time
}

// AmendObligation replaces an active obligation that no shipment holds by a new obligation on the same
// line: one command revokes the old one and allocates the new one (C6).
type AmendObligation struct{ Deps }

func (h AmendObligation) Handle(ctx context.Context, c AmendObligationCommand) (Receipt, error) {
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if _, err = h.admit(c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	if err = h.authorize(ctx, c.Scope, ActionAmendObligation, c.OrderID); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, c.OrderID, func(order *com.CustomerOrder, revision int64) error {
		if revision != c.ExpectedRevision {
			return com.ErrConflict
		}
		return order.Amend(c.ObligationID, c.NewObligationID, c.Measure, c.Scope.ActorID, c.OccurredAt)
	})
}
