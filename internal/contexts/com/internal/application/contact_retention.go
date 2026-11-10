package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

type CloseCustomerOrderCommand struct {
	CommandID        string
	Scope            Scope
	OrderID          com.OrderID
	ExpectedRevision int64
	OccurredAt       time.Time
}

// CloseCustomerOrder records that the work on an order whose lines are all allocated is finished. The order
// no longer changes, and its contacts are retained, restricted, for Deps.RetentionMonths (P1).
type CloseCustomerOrder struct{ Deps }

func (h CloseCustomerOrder) Handle(ctx context.Context, c CloseCustomerOrderCommand) (Receipt, error) {
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if _, err = h.admit(c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	if err = h.authorize(ctx, c.Scope, ActionCloseCustomerOrder, c.OrderID); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, c.OrderID, func(order *com.CustomerOrder, revision int64) error {
		if revision != c.ExpectedRevision {
			return com.ErrConflict
		}
		return order.Close(c.Scope.ActorID, c.OccurredAt, h.retentionMonths())
	})
}

type EraseOrderContactsCommand struct {
	CommandID  string
	Scope      Scope
	OrderID    com.OrderID
	OccurredAt time.Time
}

// EraseOrderContacts erases the contacts frozen on a closed or cancelled order once their retention has
// ended, as COM's clock tells (P1). It changes neither the order nor its revision; the receipt names the
// revision the order stays at.
type EraseOrderContacts struct{ Deps }

func (h EraseOrderContacts) Handle(ctx context.Context, c EraseOrderContactsCommand) (Receipt, error) {
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	entered, err := h.admit(c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	if err = h.authorize(ctx, c.Scope, ActionEraseOrderContacts, c.OrderID); err != nil {
		return Receipt{}, err
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (*com.CustomerOrder, int64, error) {
		order, revision, err := h.Orders.Load(txCtx, c.Scope.TenantID, c.OrderID)
		if err != nil {
			return nil, 0, err
		}
		if err = order.EraseContacts(c.Scope.ActorID, c.OccurredAt, entered); err != nil {
			return nil, 0, err
		}
		return order, revision, h.Orders.Erase(txCtx, order)
	})
}

// DueOrder is an order whose contacts are due for erasure: its retention ended at RetainedUntil, and it
// still holds contacts.
type DueOrder struct {
	OrderID       com.OrderID
	RetainedUntil time.Time
}

// RetentionReader lists the orders of a tenant whose contacts are due for erasure at asOf, earliest first.
type RetentionReader interface {
	DueForContactErasure(ctx context.Context, tenant com.TenantID, asOf time.Time, limit int) ([]DueOrder, error)
}

// MaxDueOrders bounds one page of ListOrdersDueForContactErasure.
const MaxDueOrders = 1000

// ListOrdersDueForContactErasure lists the orders whose contacts may be erased now, for a job that erases
// them; callers need the authorization to erase them (P1). It returns the instant it judged by.
type ListOrdersDueForContactErasure struct {
	Tx     TxRunner
	Orders RetentionReader
	Auth   Authorization
	Clock  Clock
}

func (h ListOrdersDueForContactErasure) Handle(ctx context.Context, s Scope, limit int) ([]DueOrder, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return nil, time.Time{}, err
	}
	if h.Tx == nil || h.Orders == nil || h.Auth == nil || h.Clock == nil {
		return nil, time.Time{}, ErrMissingDependency
	}
	if s.TenantID == "" || strings.TrimSpace(s.ActorID) == "" {
		return nil, time.Time{}, ErrUnauthenticated
	}
	if limit < 1 || limit > MaxDueOrders {
		return nil, time.Time{}, fmt.Errorf("%w: limit %d, 1 to %d", ErrInvalidQuery, limit, MaxDueOrders)
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(s.TenantID) {
		return nil, time.Time{}, ErrUnauthorized
	}
	if err := h.Auth.RequireOrderAction(ctx, s, ActionEraseOrderContacts, ""); err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	asOf := h.Clock.Now().UTC().Truncate(time.Microsecond)
	var due []DueOrder
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		due, err = h.Orders.DueForContactErasure(txCtx, s.TenantID, asOf, limit)
		return err
	})
	if err != nil {
		return nil, time.Time{}, err
	}
	return due, asOf, nil
}
