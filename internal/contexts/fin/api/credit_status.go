package api

import "context"

// CreditStatus v1 (FIN -> DOC; C7, MD-12, OD-7 A): FIN's credit decision for a debtor of a legal entity, which DOC
// asks before issuing a house document. FIN is the authority; DOC never judges credit itself.

// Credit status answers.
const (
	CreditOK        = "OK"         // an account without a hold
	CreditOnHold    = "ON_HOLD"    // a credit controller holds the debtor's credit
	CreditNoAccount = "NO_ACCOUNT" // FIN keeps no account for the debtor: no decision
)

// GetCreditStatus asks for a debtor's credit with a legal entity. TenantID must come from a verified principal.
type GetCreditStatus struct {
	TenantID      string
	LegalEntityID string
	DebtorPartyID string
}

// CreditStatus is CreditStatus v1. DecisionID names the account and the revision decided on
// (<legal entity>/<debtor>@<revision>), empty without an account; HoldCode and HoldReason explain a hold.
type CreditStatus struct {
	LegalEntityID string
	DebtorPartyID string
	Status        string
	DecisionID    string
	Revision      int64
	HoldCode      string
	HoldReason    string
}

// CreditStatusQueries is FIN's read side for DOC.
type CreditStatusQueries interface {
	GetCreditStatus(ctx context.Context, q GetCreditStatus) (CreditStatus, error)
}
