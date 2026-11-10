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
	Terms           com.CommercialTerms `json:",omitzero"` // completes what the quote left open (C8)
	OccurredAt      time.Time
	Transport       com.Transport `json:",omitzero"`  // requested transport mode and movement type (C11)
	Parties         []PartyInput  `json:",omitempty"` // shipper, consignee, notify party (C9)
}

// RecordCustomerOrder records an order against a quote RATE has accepted, for the quote's recipient in the
// role the quote went to, as ORG confirms it at the instant (R7). The order policy in force for the quote's
// legal entity bounds the orders the quote backs and the grace after its validity (C1, C2); the order
// freezes the quote's commercial terms, completed by those given (C8).
type RecordCustomerOrder struct {
	Deps
	Quotes        Quotes
	Customers     Customers
	Policies      Policies
	ServiceLevels ServiceLevels
	Directory     Directory // needed only for orders recorded with parties
}

func (h RecordCustomerOrder) Handle(ctx context.Context, c RecordCustomerOrderCommand) (Receipt, error) {
	if h.Quotes == nil || h.Customers == nil || h.Policies == nil || h.ServiceLevels == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	entered, err := h.admit(c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	if err = h.authorize(ctx, c.Scope, ActionRecordCustomerOrder, c.OrderID); err != nil {
		return Receipt{}, err
	}
	quote, err := h.Quotes.AcceptedQuote(ctx, c.Scope.TenantID, c.QuoteID)
	if err != nil {
		return Receipt{}, err
	}
	if quote.CustomerPartyID != c.CustomerPartyID {
		return Receipt{}, fmt.Errorf("%w: quote %s was accepted for another party", com.ErrQuoteMismatch, c.QuoteID)
	}
	party, err := h.Customers.VerifyOrderingParty(ctx, c.Scope.TenantID, c.CustomerPartyID, quote.RecipientRole, quote.LegalEntityID, c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	terms, err := com.AgreeTerms(quote.Terms, c.Terms)
	if err != nil {
		return Receipt{}, err
	}
	if terms.ServiceLevelSource == com.TermFromOrder {
		if err = h.ServiceLevels.Listed(ctx, c.Scope.TenantID, terms.ServiceLevel); err != nil {
			return Receipt{}, err
		}
	}
	parties, err := resolveParties(ctx, h.Directory, c.Scope, quote.LegalEntityID, c.OccurredAt, c.Transport, c.Parties)
	if err != nil {
		return Receipt{}, err
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (*com.CustomerOrder, int64, error) {
		policy, err := h.Policies.InForce(txCtx, c.Scope.TenantID, quote.LegalEntityID)
		if err != nil {
			return nil, 0, err
		}
		orders, err := h.Orders.OrdersOfQuote(txCtx, c.Scope.TenantID, c.QuoteID)
		if err != nil {
			return nil, 0, err
		}
		order, err := com.RecordOrder(com.OrderDraft{
			TenantID: c.Scope.TenantID, OrderID: c.OrderID, Quote: quote, Customer: party, Lines: c.Lines, Requested: c.Terms,
			Policy: policy, RecordedBy: c.Scope.ActorID, Transport: c.Transport, InitialParties: parties,
		}, orders, c.OccurredAt, entered)
		if err != nil {
			return nil, 0, err
		}
		revision, err := h.Orders.Insert(txCtx, order)
		return order, revision, err
	})
}
