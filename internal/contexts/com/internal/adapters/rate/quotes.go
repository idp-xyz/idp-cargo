// Package rate is COM's anticorruption layer for RATE: it turns QuoteSnapshot v2 into the accepted quote
// an order relies on, with a hash of the exact terms, and asks RATE whether a service level is listed.
package rate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	app "go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
)

// Quotes reads accepted quotes through RATE's query side. Call it outside COM's transactions.
type Quotes struct{ RATE rateapi.Queries }

func (q Quotes) AcceptedQuote(ctx context.Context, tenant com.TenantID, quoteID string) (com.AcceptedQuote, error) {
	s, err := q.RATE.GetQuoteSnapshotV2(ctx, rateapi.GetQuoteSnapshot{TenantID: string(tenant), QuoteID: quoteID})
	if errors.Is(err, rateapi.ErrRejected) || errors.Is(err, rateapi.ErrNotFound) {
		// RATE's answer is kept as text only, so its error kinds never become COM's: an unknown quote
		// rejects the order, it does not make the order "not found".
		return com.AcceptedQuote{}, fmt.Errorf("%w: %v", app.ErrQuoteNotAccepted, err)
	}
	if err != nil {
		return com.AcceptedQuote{}, err
	}
	if s.TenantID != string(tenant) || s.QuoteID != quoteID {
		return com.AcceptedQuote{}, fmt.Errorf("%w: RATE answered for another quote", app.ErrQuoteNotAccepted)
	}
	hash, err := TermsHashV2(s)
	if err != nil {
		return com.AcceptedQuote{}, err
	}
	t := s.Terms
	return com.AcceptedQuote{
		QuoteID: s.QuoteID, QuoteRevision: s.QuoteRevision, LegalEntityID: com.LegalEntityID(s.LegalEntityID),
		CustomerPartyID: s.Customer.PartyID, CustomerRevision: s.Customer.PartyRevision, RecipientRole: s.RecipientRole,
		Total: s.Total, Validity: s.Validity, AcceptedAt: s.AcceptedAt, EvidenceRef: s.AcceptanceEvidenceRef,
		Terms: com.CommercialTerms{IncotermsRule: t.IncotermsRule, IncotermsVersion: t.IncotermsVersion, NamedPlace: t.NamedPlace,
			ServiceLevel: t.ServiceLevel},
		TermsContract: com.QuoteSnapshotV2, TermsHash: hash,
	}, nil
}

// ServiceLevels asks RATE whether a service level is listed (C8). Call it outside COM's transactions.
type ServiceLevels struct{ RATE rateapi.Queries }

func (l ServiceLevels) Listed(ctx context.Context, tenant com.TenantID, code string) error {
	got, err := l.RATE.GetServiceLevel(ctx, rateapi.GetServiceLevel{TenantID: string(tenant), Code: code})
	if errors.Is(err, rateapi.ErrNotFound) || errors.Is(err, rateapi.ErrRejected) {
		return fmt.Errorf("%w: %v", app.ErrServiceLevelNotListed, err)
	}
	if err != nil {
		return err
	}
	if got.Code != code {
		return fmt.Errorf("%w: RATE answered for %q", app.ErrServiceLevelNotListed, got.Code)
	}
	return nil
}

type canonicalMoney struct {
	Currency string `json:"currency"`
	Minor    int64  `json:"minor"`
}

type canonicalLine struct {
	ChargeCode      string         `json:"charge_code"`
	Amount          canonicalMoney `json:"amount"`
	InQuoteCurrency canonicalMoney `json:"in_quote_currency"`
	Discount        bool           `json:"discount"`
}

type canonicalRate struct {
	From        string `json:"from"`
	Rate        int64  `json:"rate"`
	Scale       int    `json:"scale"`
	FromDigits  int    `json:"from_digits"`
	QuoteDigits int    `json:"quote_digits"`
	Source      string `json:"source"`
	AsOf        string `json:"as_of"`
}

type canonicalTerms struct {
	IncotermsRule    string `json:"incoterms_rule"`
	IncotermsVersion string `json:"incoterms_version"`
	NamedPlace       string `json:"named_place"`
	ServiceLevel     string `json:"service_level"`
	Unlisted         bool   `json:"service_level_unlisted"`
}

// canonicalSnapshot fixes the encoding that TermsHashV2 covers, so the hash depends on the terms and not on
// how RATE's Go type evolves. It leaves out when RATE recorded the acceptance, which is not a term.
type canonicalSnapshot struct {
	Contract               string          `json:"contract"`
	TenantID               string          `json:"tenant_id"`
	QuoteID                string          `json:"quote_id"`
	QuoteRevision          int64           `json:"quote_revision"`
	LegalEntityID          string          `json:"legal_entity_id"`
	CustomerPartyID        string          `json:"customer_party_id"`
	CustomerRevision       int64           `json:"customer_party_revision"`
	RecipientRole          string          `json:"recipient_role"`
	Currency               string          `json:"currency"`
	Lines                  []canonicalLine `json:"lines"`
	Total                  canonicalMoney  `json:"total"`
	ExchangeRates          []canonicalRate `json:"exchange_rates"`
	Rounding               string          `json:"rounding"`
	Terms                  canonicalTerms  `json:"terms"`
	ValidFrom              string          `json:"valid_from"`
	ValidUntil             string          `json:"valid_until"`
	PriceApprovalReasons   []string        `json:"price_approval_reasons"`
	PriceApprovedBy        string          `json:"price_approved_by"`
	PriceSelfApproved      bool            `json:"price_self_approved"`
	AcceptedAt             string          `json:"accepted_at"`
	AcceptanceEvidenceKind string          `json:"acceptance_evidence_kind"`
	AcceptanceEvidenceRef  string          `json:"acceptance_evidence_ref"`
}

func money(currency string, minor int64) canonicalMoney {
	return canonicalMoney{Currency: currency, Minor: minor}
}

// TermsHashV2 is the hex SHA-256 of the canonical JSON encoding of a QuoteSnapshot v2.
func TermsHashV2(s rateapi.QuoteSnapshotV2) (string, error) {
	c := canonicalSnapshot{
		Contract: com.QuoteSnapshotV2, TenantID: s.TenantID, QuoteID: s.QuoteID, QuoteRevision: s.QuoteRevision,
		LegalEntityID: s.LegalEntityID, CustomerPartyID: s.Customer.PartyID, CustomerRevision: s.Customer.PartyRevision,
		RecipientRole: s.RecipientRole, Currency: string(s.Currency), Lines: []canonicalLine{},
		Total: money(string(s.Total.Currency), s.Total.Minor), ExchangeRates: []canonicalRate{}, Rounding: s.Rounding,
		Terms: canonicalTerms{IncotermsRule: s.Terms.IncotermsRule, IncotermsVersion: s.Terms.IncotermsVersion, NamedPlace: s.Terms.NamedPlace,
			ServiceLevel: s.Terms.ServiceLevel, Unlisted: s.ServiceLevelUnlisted},
		ValidFrom: instant(s.Validity.From), ValidUntil: instant(s.Validity.Until), PriceApprovalReasons: append([]string{}, s.PriceApprovalReasons...),
		PriceApprovedBy: s.PriceApprovedBy, PriceSelfApproved: s.PriceSelfApproved, AcceptedAt: instant(s.AcceptedAt),
		AcceptanceEvidenceKind: s.AcceptanceEvidenceKind, AcceptanceEvidenceRef: s.AcceptanceEvidenceRef,
	}
	for _, l := range s.Lines {
		c.Lines = append(c.Lines, canonicalLine{ChargeCode: l.ChargeCode, Amount: money(string(l.Amount.Currency), l.Amount.Minor),
			InQuoteCurrency: money(string(l.InQuoteCurrency.Currency), l.InQuoteCurrency.Minor), Discount: l.Discount})
	}
	for _, r := range s.ExchangeRates {
		c.ExchangeRates = append(c.ExchangeRates, canonicalRate{From: string(r.From), Rate: r.Rate, Scale: r.Scale, FromDigits: r.FromDigits,
			QuoteDigits: r.QuoteDigits, Source: r.Source, AsOf: instant(r.AsOf)})
	}
	blob, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

func instant(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
