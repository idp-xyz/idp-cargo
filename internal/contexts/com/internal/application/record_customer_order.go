package application

import (
	"context"
	"fmt"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

type RecordCustomerOrderCommand struct {
	CommandID       string
	Scope           Scope
	OrderID         com.OrderID
	QuoteID         string
	CustomerPartyID string
	Lines           []com.OrderLine
	OccurredAt      time.Time
}

// RecordCustomerOrder records an order against a quote RATE has accepted, for the quote's customer as
// ORG confirms it at the instant.
type RecordCustomerOrder struct {
	Deps
	Quotes    Quotes
	Customers Customers
}

func (h RecordCustomerOrder) Handle(ctx context.Context, c RecordCustomerOrderCommand) (Receipt, error) {
	if h.Quotes == nil || h.Customers == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionRecordCustomerOrder, c.OrderID); err != nil {
		return Receipt{}, err
	}
	quote, err := h.Quotes.AcceptedQuote(ctx, c.Scope.TenantID, c.QuoteID)
	if err != nil {
		return Receipt{}, err
	}
	if quote.CustomerPartyID != c.CustomerPartyID {
		return Receipt{}, fmt.Errorf("%w: quote %s was accepted by another customer", com.ErrQuoteMismatch, c.QuoteID)
	}
	customer, err := h.Customers.VerifyCustomer(ctx, c.Scope.TenantID, c.CustomerPartyID, quote.LegalEntityID, c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	order, err := com.RecordOrder(com.OrderDraft{
		TenantID: c.Scope.TenantID, OrderID: c.OrderID, Quote: quote, Customer: customer, Lines: c.Lines, RecordedBy: c.Scope.ActorID,
	}, c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (*com.CustomerOrder, int64, error) {
		revision, err := h.Orders.Insert(txCtx, order)
		return order, revision, err
	})
}
