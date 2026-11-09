package application

import (
	"context"
	"fmt"
	"time"

	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

type CreateQuoteCommand struct {
	CommandID       string
	Scope           Scope
	QuoteID         rate.QuoteID
	LegalEntityID   rate.LegalEntityID
	CustomerPartyID string
	Recipient       rate.RecipientRole `json:",omitzero"` // empty: CUSTOMER
	Currency        kernel.CurrencyCode
	Lines           []rate.Line
	ExchangeRates   []rate.ExchangeRate  `json:",omitzero"`
	Terms           rate.CommercialTerms `json:",omitzero"`
	Validity        kernel.ValidityInterval
	OccurredAt      time.Time
}

// CreateQuote prices a quote for a party that ORG confirms in the recipient role (R7) for the legal
// entity.
type CreateQuote struct {
	Deps
	Customers Customers
	Margins   MarginPolicy
}

func (h CreateQuote) Handle(ctx context.Context, c CreateQuoteCommand) (Receipt, error) {
	if err := h.check(ctx, c.Scope, c.CommandID, c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	if h.Customers == nil || h.Margins == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, err := requestHash(c)
	if err != nil {
		return Receipt{}, err
	}
	if r, ok, err := h.replay(ctx, c.Scope.TenantID, c.CommandID, hash); err != nil || ok {
		return r, err
	}
	if _, err = h.admit(c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	if err = h.Auth.RequireQuoteAction(ctx, c.Scope, ActionCreateQuote, c.QuoteID); err != nil {
		return Receipt{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	recipient := c.Recipient
	if recipient == "" {
		recipient = rate.RecipientCustomer
	}
	if !recipient.Valid() {
		return Receipt{}, fmt.Errorf("%w: recipient role %q", rate.ErrInvalidQuote, recipient)
	}
	customer, err := h.Customers.VerifyRecipient(ctx, c.Scope.TenantID, c.CustomerPartyID, c.LegalEntityID, recipient, c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	floor, err := h.Margins.MarginFloorBP(ctx, c.Scope.TenantID, c.LegalEntityID)
	if err != nil {
		return Receipt{}, err
	}
	quote, err := rate.PriceQuote(rate.QuoteDraft{
		TenantID: c.Scope.TenantID, QuoteID: c.QuoteID, LegalEntityID: c.LegalEntityID, Customer: customer, Recipient: recipient,
		Currency: c.Currency, Lines: c.Lines, ExchangeRates: c.ExchangeRates, Terms: c.Terms, Validity: c.Validity,
		MarginFloorBP: floor, Author: c.Scope.ActorID,
	}, c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (*rate.Quote, int64, error) {
		revision, err := h.Quotes.Insert(txCtx, quote)
		return quote, revision, err
	})
}
