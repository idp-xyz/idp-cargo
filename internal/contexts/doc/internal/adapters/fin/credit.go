// Package fin stands in for FIN's CreditStatus v1 (FIN -> DOC) until FIN is built: it has no credit decision for
// anyone, so a house document is issued only with a credit override (C7, decision N1-DOC OD-3 option B).
package fin

import (
	"context"

	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

// NoCreditControl answers that FIN has no decision.
type NoCreditControl struct{}

var _ app.Credit = NoCreditControl{}

func (NoCreditControl) Decision(context.Context, doc.TenantID, []string) (doc.CreditDecision, error) {
	return doc.CreditDecision{Status: "NONE"}, nil
}
