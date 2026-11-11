// Package fin is DOC's anticorruption layer for FIN's CreditStatus v1 (FIN -> DOC; C7, OD-7 A): the credit of the
// ordering parties of a house document's orders with the issuing legal entity. FIN's error kinds are kept as text.
package fin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// Credit asks FIN for every debtor. Call it outside DOC's transactions.
type Credit struct{ FIN finapi.CreditStatusQueries }

var _ app.Credit = Credit{}

// Decision is HOLD when FIN holds any debtor, OK when it has an account without a hold for every one, and NONE (no
// decision: an override is needed) otherwise. The decision ids name the accounts and revisions decided on.
func (c Credit) Decision(ctx context.Context, tenant doc.TenantID, legalEntity string, debtors []string) (doc.CreditDecision, error) {
	if len(debtors) == 0 {
		return doc.CreditDecision{Status: "NONE"}, nil
	}
	status := "OK"
	var ids []string
	for _, debtor := range debtors {
		s, err := c.FIN.GetCreditStatus(ctx, finapi.GetCreditStatus{TenantID: string(tenant), LegalEntityID: legalEntity, DebtorPartyID: debtor})
		if errors.Is(err, finapi.ErrNotFound) || errors.Is(err, finapi.ErrRejected) {
			return doc.CreditDecision{}, fmt.Errorf("%w: credit of %s: %v", app.ErrUnknownSource, debtor, err.Error())
		}
		if err != nil {
			return doc.CreditDecision{}, err
		}
		if s.DebtorPartyID != debtor || s.LegalEntityID != legalEntity {
			return doc.CreditDecision{}, fmt.Errorf("%w: FIN answered for another debtor", app.ErrUnknownSource)
		}
		switch s.Status {
		case finapi.CreditOnHold:
			return doc.CreditDecision{Status: "HOLD", DecisionID: s.DecisionID}, nil
		case finapi.CreditOK:
			ids = append(ids, s.DecisionID)
		default:
			status = "NONE"
		}
	}
	if status != "OK" {
		return doc.CreditDecision{Status: status}, nil
	}
	return doc.CreditDecision{Status: "OK", DecisionID: strings.Join(ids, ",")}, nil
}
