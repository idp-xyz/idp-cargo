package application

import (
	"context"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

type AmendOrderLineCommand struct {
	CommandID        string
	Scope            Scope
	OrderID          com.OrderID
	ExpectedRevision int64
	LineNo           int
	Description      string
	PackageType      com.PackageType
	Measure          com.Measure
	Reason           string
	OccurredAt       time.Time
}

// AmendOrderLine corrects what the customer declared for an order line (C10): the line's next version,
// with a reason, without a new quote.
type AmendOrderLine struct{ Deps }

func (h AmendOrderLine) Handle(ctx context.Context, c AmendOrderLineCommand) (Receipt, error) {
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	entered, err := h.admit(c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	if err = h.authorize(ctx, c.Scope, ActionAmendOrderLine, c.OrderID); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, c.OrderID, func(order *com.CustomerOrder, revision int64) error {
		if revision != c.ExpectedRevision {
			return com.ErrConflict
		}
		return order.AmendLine(c.LineNo, c.Description, c.PackageType, c.Measure, c.Reason, c.Scope.ActorID, c.OccurredAt, entered)
	})
}

type AddOrderLineCommand struct {
	CommandID        string
	Scope            Scope
	OrderID          com.OrderID
	ExpectedRevision int64
	Description      string
	PackageType      com.PackageType
	Measure          com.Measure
	Reason           string
	OccurredAt       time.Time
}

// AddOrderLine adds a line to a recorded order, such as goods the customer added (C10).
type AddOrderLine struct{ Deps }

func (h AddOrderLine) Handle(ctx context.Context, c AddOrderLineCommand) (Receipt, error) {
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	entered, err := h.admit(c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	if err = h.authorize(ctx, c.Scope, ActionAddOrderLine, c.OrderID); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, c.OrderID, func(order *com.CustomerOrder, revision int64) error {
		if revision != c.ExpectedRevision {
			return com.ErrConflict
		}
		return order.AddLine(c.Description, c.PackageType, c.Measure, c.Reason, c.Scope.ActorID, c.OccurredAt, entered)
	})
}
