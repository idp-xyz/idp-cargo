// Package org is RATE's anticorruption layer for ORG: it turns ORG's party-role answer into RATE's
// reference to the party a quote is for, in one of the recipient roles of decision R7.
package org

import (
	"context"
	"errors"
	"fmt"
	"time"

	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/application"
	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
)

// Customers confirms quote recipients through ORG's query side. Call it outside RATE's transactions.
type Customers struct{ ORG orgapi.Queries }

// VerifyRecipient asks ORG whether the party holds the role, which must be one a quote may go to (R7),
// for the legal entity at the instant. RATE's recipient roles are ORG role codes of the same names.
func (c Customers) VerifyRecipient(ctx context.Context, tenant rate.TenantID, partyID string, entity rate.LegalEntityID, recipient rate.RecipientRole, at time.Time) (rate.CustomerRef, error) {
	if !recipient.Valid() {
		return rate.CustomerRef{}, fmt.Errorf("%w: %q is not a role a quote goes to", app.ErrCustomerNotConfirmed, recipient)
	}
	role, err := c.ORG.ResolvePartyRole(ctx, orgapi.ResolvePartyRole{
		TenantID: string(tenant), PartyID: partyID, RoleCode: string(recipient), LegalEntityID: string(entity), At: at,
	})
	if errors.Is(err, orgapi.ErrRejected) || errors.Is(err, orgapi.ErrNotFound) {
		// ORG's answer is kept as text only, so its error kinds never become RATE's: a party ORG does
		// not know rejects the quote, it does not make the quote "not found".
		return rate.CustomerRef{}, fmt.Errorf("%w: %v", app.ErrCustomerNotConfirmed, err)
	}
	if err != nil {
		return rate.CustomerRef{}, err
	}
	return rate.CustomerRef{PartyID: role.Ref.PartyID, PartyRevision: role.Ref.PartyRevision}, nil
}
