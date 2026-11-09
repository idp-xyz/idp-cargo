package application

import (
	"context"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

type CancelCustomerOrderCommand struct {
	CommandID        string
	Scope            Scope
	OrderID          com.OrderID
	ExpectedRevision int64
	OccurredAt       time.Time
}

// CancelCustomerOrder cancels an order none of whose obligations a shipment holds; its active obligations
// are revoked (C6).
type CancelCustomerOrder struct{ Deps }

func (h CancelCustomerOrder) Handle(ctx context.Context, c CancelCustomerOrderCommand) (Receipt, error) {
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if _, err = h.admit(c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	if err = h.authorize(ctx, c.Scope, ActionCancelCustomerOrder, c.OrderID); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, c.OrderID, func(order *com.CustomerOrder, revision int64) error {
		if revision != c.ExpectedRevision {
			return com.ErrConflict
		}
		return order.Cancel(c.Scope.ActorID, c.OccurredAt)
	})
}
