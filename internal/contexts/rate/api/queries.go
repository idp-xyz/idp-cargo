package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// CustomerRef identifies the ORG party a quote was accepted for, at the revision ORG confirmed at
// acceptance.
type CustomerRef struct {
	PartyID       string
	PartyRevision int64
}

// SnapshotLine is one charge of an accepted quote, at its sell amount.
type SnapshotLine struct {
	ChargeCode string
	Amount     kernel.Money
}

// QuoteSnapshot is QuoteSnapshot v1: an accepted quote as RATE froze it. Later changes to rates or to
// the party never alter it, and it carries no costs or margins.
type QuoteSnapshot struct {
	TenantID              string
	QuoteID               string
	QuoteRevision         int64
	LegalEntityID         string
	Customer              CustomerRef
	Lines                 []SnapshotLine
	Total                 kernel.Money
	Validity              kernel.ValidityInterval
	PriceApprovedBy       string // empty when the margin met the floor
	AcceptedAt            time.Time
	AcceptanceEvidenceRef string
}

// SnapshotLineV2 is one charge of an accepted quote: its sell amount in its own currency, the amount in the
// quote currency at the frozen rate, and whether it is a discount (R8, R9).
type SnapshotLineV2 struct {
	ChargeCode      string
	Amount          kernel.Money
	InQuoteCurrency kernel.Money
	Discount        bool
}

// QuoteSnapshotV2 is QuoteSnapshot v1 with what the 2026-11-06 business review and the owner decisions of
// 2026-11-09 added: the recipient role (R7), lines in several currencies with the frozen exchange rates and
// rounding (R8), discount lines (R9), the reasons for and kind of price approval (R2, R3), the evidence kind
// and recording time of the acceptance (R5, T1), and the commercial terms (C8). Like v1 it carries no costs
// or margins. In v1 the lines keep their own currencies and Total is in the quote currency.
type QuoteSnapshotV2 struct {
	TenantID               string
	QuoteID                string
	QuoteRevision          int64
	LegalEntityID          string
	Customer               CustomerRef
	RecipientRole          string
	Currency               kernel.CurrencyCode
	Lines                  []SnapshotLineV2
	Total                  kernel.Money
	ExchangeRates          []ExchangeRate
	Rounding               string // "HALF_UP_PER_LINE"
	Terms                  CommercialTerms
	Validity               kernel.ValidityInterval
	PriceApprovalReasons   []string // BELOW_MARGIN_FLOOR, DISCOUNT, RATE_CARD_DISCOUNT (V0.5); empty when none was needed
	PriceApprovedBy        string   // empty when no approval was needed
	PriceSelfApproved      bool
	AcceptedAt             time.Time
	AcceptanceRecordedAt   time.Time // zero for quotes accepted before recording times were kept
	AcceptanceEvidenceKind string    // UNRECORDED for quotes accepted before evidence kinds were kept
	AcceptanceEvidenceRef  string
	// ServiceLevelUnlisted is true when Terms.ServiceLevel was recorded as free text before the
	// service-level list existed (C8); it is passed on as recorded.
	ServiceLevelUnlisted bool
}

// ServiceLevel is an entry of RATE's service-level list, the commercial catalogue of service levels a
// quote or an order may name (C8).
type ServiceLevel struct {
	Code string
	Name string
}

// GetServiceLevel asks for the listed service level with Code. TenantID must come from a verified
// principal.
type GetServiceLevel struct {
	TenantID string
	Code     string
}

// ListServiceLevels asks for a tenant's service-level list. S1 lists the same built-in codes for every
// tenant; tenants maintain their own list before the pilot.
type ListServiceLevels struct {
	TenantID string
}

// SnapshotLineV3 is a line of QuoteSnapshotV3: SnapshotLineV2 with where its price comes from (Source, and for a rated
// line the snapshot it was rated in), its basis (LUMP_SUM, or PER_UNIT with the unit, the rate in minor units times
// 10^-RateScale and the quantity it was priced on, which reproduce Amount), the catalogue version of its code (nil for a
// line priced before the catalogue), and whether the code was recorded unlisted before the catalogue. No costs, and
// nothing of how far an overridden line was moved from its rated sell.
type SnapshotLineV3 struct {
	SnapshotLineV2
	Source       string
	SnapshotID   string
	Basis        string
	Unit         string
	Rate         int64
	RateScale    int
	Quantity     int64
	CodeVersion  *int
	CodeUnlisted bool
}

// QuoteSnapshotV3 is QuoteSnapshotV2 with the quote's kind, its scope and the V0.5 line details. An AGREEMENT prices by
// lane with its customer card (Agreement, published by the acceptance), not by lines: it has no lines and an empty
// Total, and COM takes no commitment amount from it (OD-5); only V3 answers for it. Like v2 it carries no costs or
// margins; v2 and v1 stay published for ONE_OFF quotes.
type QuoteSnapshotV3 struct {
	QuoteSnapshotV2
	Kind      string
	Scope     *QuoteScope
	Agreement *AgreementRef
	Lines     []SnapshotLineV3
}

// QuoteSnapshotV3Queries answers QuoteSnapshotV3.
type QuoteSnapshotV3Queries interface {
	GetQuoteSnapshotV3(ctx context.Context, q GetQuoteSnapshot) (QuoteSnapshotV3, error)
}

// GetQuoteSnapshot asks for the snapshot of an accepted quote. TenantID must come from a verified
// principal.
type GetQuoteSnapshot struct {
	TenantID string
	QuoteID  string
}

// Queries is RATE's read side for other contexts. An unknown quote yields ErrNotFound; a quote that is
// not accepted yields ErrRejected. A service level that is not listed yields ErrNotFound.
type Queries interface {
	GetQuoteSnapshot(ctx context.Context, q GetQuoteSnapshot) (QuoteSnapshot, error)
	GetQuoteSnapshotV2(ctx context.Context, q GetQuoteSnapshot) (QuoteSnapshotV2, error)
	QuoteSnapshotV3Queries
	GetServiceLevel(ctx context.Context, q GetServiceLevel) (ServiceLevel, error)
	ListServiceLevels(ctx context.Context, q ListServiceLevels) ([]ServiceLevel, error)
}
