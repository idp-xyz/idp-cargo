package application

import (
	"context"
	"fmt"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

type AllocateOrderToObligationCommand struct {
	CommandID        string
	Scope            Scope
	OrderID          com.OrderID
	ExpectedRevision int64
	LineNo           int
	ObligationID     com.ObligationID
	Quantity         kernel.Quantity
	OccurredAt       time.Time
}

// AllocateOrderToObligation commits part or all of an order line to a new fulfillment obligation. It asks
// no other context: the quote and the customer were confirmed when the order was recorded.
type AllocateOrderToObligation struct{ Deps }

func (h AllocateOrderToObligation) Handle(ctx context.Context, c AllocateOrderToObligationCommand) (Receipt, error) {
	if err := h.check(ctx, c.Scope, c.CommandID, c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	hash, err := requestHash(c)
	if err != nil {
		return Receipt{}, err
	}
	if r, ok, err := h.replay(ctx, c.Scope.TenantID, c.CommandID, hash); err != nil || ok {
		return r, err
	}
	if err = h.Auth.RequireOrderAction(ctx, c.Scope, ActionAllocateOrderToObligation, c.OrderID); err != nil {
		return Receipt{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (*com.CustomerOrder, int64, error) {
		order, revision, err := h.Orders.Load(txCtx, c.Scope.TenantID, c.OrderID)
		if err != nil {
			return nil, 0, err
		}
		if revision != c.ExpectedRevision {
			return nil, 0, com.ErrConflict
		}
		if err = order.Allocate(c.LineNo, c.ObligationID, c.Quantity, c.Scope.ActorID, c.OccurredAt); err != nil {
			return nil, 0, err
		}
		newRevision, err := h.Orders.Save(txCtx, order, revision)
		return order, newRevision, err
	})
}
