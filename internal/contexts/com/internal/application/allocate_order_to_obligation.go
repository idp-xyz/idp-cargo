package application

import (
	"context"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

type AllocateOrderToObligationCommand struct {
	CommandID        string
	Scope            Scope
	OrderID          com.OrderID
	ExpectedRevision int64
	LineNo           int
	ObligationID     com.ObligationID
	Measure          com.Measure
	OccurredAt       time.Time
}

// AllocateOrderToObligation commits packs of an order line, with their declared gross weight and volume,
// to a new fulfillment obligation. It asks no other context: the quote and the customer were confirmed
// when the order was recorded.
type AllocateOrderToObligation struct{ Deps }

func (h AllocateOrderToObligation) Handle(ctx context.Context, c AllocateOrderToObligationCommand) (Receipt, error) {
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionAllocateOrderToObligation, c.OrderID); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, c.OrderID, func(order *com.CustomerOrder, revision int64) error {
		if revision != c.ExpectedRevision {
			return com.ErrConflict
		}
		return order.Allocate(c.LineNo, c.ObligationID, c.Measure, c.Scope.ActorID, c.OccurredAt)
	})
}
