// Package rate is COM's anticorruption layer for RATE: it turns QuoteSnapshot v1 into the accepted quote
// an order relies on, with a hash of the exact terms.
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
	s, err := q.RATE.GetQuoteSnapshot(ctx, rateapi.GetQuoteSnapshot{TenantID: string(tenant), QuoteID: quoteID})
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
	hash, err := TermsHash(s)
	if err != nil {
		return com.AcceptedQuote{}, err
	}
	return com.AcceptedQuote{
		QuoteID: s.QuoteID, QuoteRevision: s.QuoteRevision, LegalEntityID: com.LegalEntityID(s.LegalEntityID),
		CustomerPartyID: s.Customer.PartyID, CustomerRevision: s.Customer.PartyRevision, Total: s.Total, Validity: s.Validity,
		AcceptedAt: s.AcceptedAt, EvidenceRef: s.AcceptanceEvidenceRef, TermsHash: hash,
	}, nil
}

type canonicalMoney struct {
	Currency string `json:"currency"`
	Minor    int64  `json:"minor"`
}

type canonicalLine struct {
	ChargeCode string         `json:"charge_code"`
	Amount     canonicalMoney `json:"amount"`
}

// canonicalSnapshot fixes the encoding that TermsHash covers, so the hash depends on the terms and not on
// how RATE's Go type evolves.
type canonicalSnapshot struct {
	Contract              string          `json:"contract"`
	TenantID              string          `json:"tenant_id"`
	QuoteID               string          `json:"quote_id"`
	QuoteRevision         int64           `json:"quote_revision"`
	LegalEntityID         string          `json:"legal_entity_id"`
	CustomerPartyID       string          `json:"customer_party_id"`
	CustomerRevision      int64           `json:"customer_party_revision"`
	Lines                 []canonicalLine `json:"lines"`
	Total                 canonicalMoney  `json:"total"`
	ValidFrom             string          `json:"valid_from"`
	ValidUntil            string          `json:"valid_until"`
	PriceApprovedBy       string          `json:"price_approved_by"`
	AcceptedAt            string          `json:"accepted_at"`
	AcceptanceEvidenceRef string          `json:"acceptance_evidence_ref"`
}

// TermsHash is the hex SHA-256 of the canonical JSON encoding of a QuoteSnapshot v1.
func TermsHash(s rateapi.QuoteSnapshot) (string, error) {
	c := canonicalSnapshot{
		Contract: "QuoteSnapshot.v1", TenantID: s.TenantID, QuoteID: s.QuoteID, QuoteRevision: s.QuoteRevision,
		LegalEntityID: s.LegalEntityID, CustomerPartyID: s.Customer.PartyID, CustomerRevision: s.Customer.PartyRevision,
		Lines: []canonicalLine{}, Total: canonicalMoney{Currency: string(s.Total.Currency), Minor: s.Total.Minor},
		ValidFrom: instant(s.Validity.From), ValidUntil: instant(s.Validity.Until), PriceApprovedBy: s.PriceApprovedBy,
		AcceptedAt: instant(s.AcceptedAt), AcceptanceEvidenceRef: s.AcceptanceEvidenceRef,
	}
	for _, l := range s.Lines {
		c.Lines = append(c.Lines, canonicalLine{ChargeCode: l.ChargeCode, Amount: canonicalMoney{Currency: string(l.Amount.Currency), Minor: l.Amount.Minor}})
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
