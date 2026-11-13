package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Quote is a quote as it stands in RATE, in any status: what RATE's own users read while they price, approve and
// accept it (V0.5 part 3c, the HTTP API). Unlike QuoteSnapshot it carries costs and the margin: other contexts read
// snapshots, and an inbound adapter shows costs and the margin only to whom may see them. Revision is what the next
// command on the quote expects. A ONE_OFF quote's totals are in the quote currency (each line converted at the quote's
// rates); an AGREEMENT prices by lane with its customer card and has no lines or totals.
type Quote struct {
	QuoteID         string
	Revision        int64
	Kind            string
	Status          string // AWAITING_APPROVAL, PRICED, ACCEPTED or EXPIRED
	LegalEntityID   string
	Customer        CustomerRef // the revision ORG confirmed at pricing, or at acceptance once accepted
	RecipientRole   string
	Currency        kernel.CurrencyCode
	Lines           []QuoteViewLine
	ExchangeRates   []ExchangeRate
	Terms           CommercialTerms
	Validity        kernel.ValidityInterval
	Scope           *QuoteScope
	Agreement       *AgreementRef
	SellTotal       kernel.Money
	CostTotal       kernel.Money
	MarginFloorBP   int
	ApprovalReasons []string
	Author          string
	Approval        *QuoteApproval
	Acceptance      *QuoteAcceptance
	ExpiredAt       time.Time
}

// QuoteViewLine is a line of a Quote: SnapshotLineV3 with its cost.
type QuoteViewLine struct {
	SnapshotLineV3
	Cost kernel.Money
}

// QuoteApproval is who approved a quote's price, when, on what reason, and whether the author did (R3).
type QuoteApproval struct {
	ApprovedBy   string
	ApprovedAt   time.Time
	ReasonRef    string
	SelfApproved bool
}

// QuoteAcceptance is the customer's acceptance as recorded (R5, T1).
type QuoteAcceptance struct {
	AcceptedBy   string
	AcceptedAt   time.Time
	RecordedAt   time.Time
	EvidenceKind string
	EvidenceRef  string
}

// GetQuote asks for a quote as it stands. TenantID must come from a verified principal.
type GetQuote struct {
	TenantID string
	QuoteID  string
}

// QuoteQueries is RATE's read side for its own users.
type QuoteQueries interface {
	GetQuote(ctx context.Context, q GetQuote) (Quote, error)
}
