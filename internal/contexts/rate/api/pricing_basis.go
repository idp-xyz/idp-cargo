package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// PricingBasis v1 (RATE -> FIN): what a job's charges are estimated on. RATE is the authority for the prices,
// FIN for the actual charges; a basis is an estimate, never revenue or a payable. Purpose QUOTE_ACCEPTED is an
// accepted quote's sell and cost lines. SHIPMENT_ESTIMATE (a shipment rated on both sides for its customer) and
// CONSOL_COST_ESTIMATE (a consolidation rated on the BUY side) are issued when FIN asks RATE to rate (V0.5 part 2):
// built from a rate snapshot, immutable, a new estimate a new basis. Unlike QuoteSnapshot it carries the costs, because
// FIN owns job economics (R6).

// Pricing basis purposes.
const (
	BasisQuoteAccepted      = "QUOTE_ACCEPTED"
	BasisShipmentEstimate   = "SHIPMENT_ESTIMATE"
	BasisConsolCostEstimate = "CONSOL_COST_ESTIMATE"
)

// Line sources of a basis.
const (
	SourceQuote = "QUOTE"
	SourceRated = "RATED"
)

// PricingLine is a basis line: its number, charge code, sell amount (negative for a discount, R9) and cost
// amount, each in its own currency. A QUOTE line always sells and costs when Cost is not zero. A RATED line has the
// sides the evaluation rated (HasSell, HasCost; a side not rated is absent, never zero), and the supplier whose card
// priced the cost (CreditorPartyID).
type PricingLine struct {
	LineNo          int
	ChargeCode      string
	Sell            kernel.Money
	Cost            kernel.Money
	Discount        bool
	Source          string
	HasSell         bool
	HasCost         bool
	CreditorPartyID string
}

// PricingGap is a charge a rated basis lacks: RATE could not rate it (GapNoRate, GapCallForPricing, GapBaseMissing,
// GapBaseCurrency or GapNoQuantity), and FIN leaves it to a person.
type PricingGap struct {
	Side       string
	ChargeCode string
	Gap        string
}

// PricingBasis is PricingBasis v1. BasisID is QUOTE:<quote>@<revision> for an accepted quote, the id FIN chose for a
// rated one. ExchangeRates are the quote's frozen rates, for pricing only: FIN converts at its own rates (R8). A rated
// basis names its subject (the shipment and plan version, or the consolidation and revision), the snapshot it was
// built from and its gaps; Customer is empty for a CONSOL_COST_ESTIMATE.
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

	SubjectKind    string // SHIPMENT or CONSOLIDATION for a rated basis
	SubjectID      string
	SubjectVersion int64
	SnapshotID     string
	Gaps           []PricingGap
	IssuedAt       time.Time
}

// GetPricingBasis asks for the basis of an accepted quote (QuoteID) or a rated basis (BasisID). TenantID must come
// from a verified principal. An unknown quote or basis yields ErrNotFound; a quote that is not accepted ErrRejected.
type GetPricingBasis struct {
	TenantID string
	QuoteID  string
	BasisID  string
}

// PricingBasisQueries is RATE's read side for FIN.
type PricingBasisQueries interface {
	GetPricingBasis(ctx context.Context, q GetPricingBasis) (PricingBasis, error)
}

// RateShipmentEstimate rates a shipment at OccurredAt for its customer on both sides (SELL: the customer's card, then
// the tariff, OD-1 A; BUY: the given carrier's cards or the lowest total, OD-2 A) and issues PricingBasis v1 BasisID of
// purpose SHIPMENT_ESTIMATE, recording the rate snapshot (its id is the basis id). ORG confirms the customer in a role
// a quote goes to (R7). FIN calls it with the shipment's CostDriver v1 facts; a replay returns the same basis.
type RateShipmentEstimate struct {
	CommandID       string
	TenantID        string
	ActorID         string
	BasisID         string
	LegalEntityID   string
	CustomerPartyID string
	ShipmentID      string
	PlanVersion     int64
	Mode            string
	ContainerMode   string
	Origin          string
	Destination     string
	Via             string
	ServiceLevel    string
	Carrier         string
	Cargo           Cargo
	OccurredAt      time.Time
}

// RateConsolCost rates a consolidation's BUY side at OccurredAt (the given carrier's cards, else the lowest total) and
// issues PricingBasis v1 BasisID of purpose CONSOL_COST_ESTIMATE. FIN calls it with the consolidation's carrier, lane
// and cargo totals.
type RateConsolCost struct {
	CommandID             string
	TenantID              string
	ActorID               string
	BasisID               string
	LegalEntityID         string
	ConsolidationID       string
	ConsolidationRevision int64
	Mode                  string
	ContainerMode         string
	Origin                string
	Destination           string
	Via                   string
	ServiceLevel          string
	Carrier               string
	Cargo                 Cargo
	OccurredAt            time.Time
}

// PricingBasisCommands is RATE's command side for FIN: it rates shipments and consolidations into pricing bases.
type PricingBasisCommands interface {
	RateShipmentEstimate(ctx context.Context, cmd RateShipmentEstimate) (Receipt, error)
	RateConsolCost(ctx context.Context, cmd RateConsolCost) (Receipt, error)
}
