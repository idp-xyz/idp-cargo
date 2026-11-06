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

// GetQuoteSnapshot asks for the snapshot of an accepted quote. TenantID must come from a verified
// principal.
type GetQuoteSnapshot struct {
	TenantID string
	QuoteID  string
}

// Queries is RATE's read side for other contexts. An unknown quote yields ErrNotFound; a quote that is
// not accepted yields ErrRejected.
type Queries interface {
	GetQuoteSnapshot(ctx context.Context, q GetQuoteSnapshot) (QuoteSnapshot, error)
}
