package application

import (
	"context"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

type RevokeObligationCommand struct {
	CommandID        string
	Scope            Scope
	OrderID          com.OrderID
	ExpectedRevision int64
	ObligationID     com.ObligationID
	OccurredAt       time.Time
}

// RevokeObligation revokes an active obligation that no shipment holds; its packs become allocatable
// again (C6).
type RevokeObligation struct{ Deps }

func (h RevokeObligation) Handle(ctx context.Context, c RevokeObligationCommand) (Receipt, error) {
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionRevokeObligation, c.OrderID); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, c.OrderID, func(order *com.CustomerOrder, revision int64) error {
		if revision != c.ExpectedRevision {
			return com.ErrConflict
		}
		return order.Revoke(c.ObligationID, c.Scope.ActorID, c.OccurredAt)
	})
}
