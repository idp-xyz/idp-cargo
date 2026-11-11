package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// PricingBasis v1 (RATE -> FIN): what a job's charges are estimated on. RATE is the authority for the prices,
// FIN for the actual charges; a basis is an estimate, never revenue or a payable. Purpose QUOTE_ACCEPTED is an
// accepted quote's sell and cost lines; SHIPMENT_ESTIMATE and CONSOL_COST_ESTIMATE come with rate cards (V0.5).
// Unlike QuoteSnapshot it carries the costs, because FIN owns job economics (R6).

// Pricing basis purposes.
const (
	BasisQuoteAccepted      = "QUOTE_ACCEPTED"
	BasisShipmentEstimate   = "SHIPMENT_ESTIMATE"
	BasisConsolCostEstimate = "CONSOL_COST_ESTIMATE"
)

// PricingLine is a basis line: its number, charge code, sell amount (negative for a discount, R9) and cost
// amount (zero when none), each in its own currency.
type PricingLine struct {
	LineNo     int
	ChargeCode string
	Sell       kernel.Money
	Cost       kernel.Money
	Discount   bool
}

// PricingBasis is PricingBasis v1. BasisID is QUOTE:<quote>@<revision> for an accepted quote. ExchangeRates are
// the quote's frozen rates, for pricing only: FIN converts at its own rates (R8).
type PricingBasis struct {
	TenantID      string
	BasisID       string
	Purpose       string
	QuoteID       string
	QuoteRevision int64
	LegalEntityID string
	Customer      CustomerRef
	Currency      kernel.CurrencyCode
	Lines         []PricingLine
	ExchangeRates []ExchangeRate
	AcceptedAt    time.Time
}

// GetPricingBasis asks for the basis of an accepted quote. TenantID must come from a verified principal. An
// unknown quote yields ErrNotFound; one that is not accepted ErrRejected.
type GetPricingBasis struct {
	TenantID string
	QuoteID  string
}

// PricingBasisQueries is RATE's read side for FIN.
type PricingBasisQueries interface {
	GetPricingBasis(ctx context.Context, q GetPricingBasis) (PricingBasis, error)
}
