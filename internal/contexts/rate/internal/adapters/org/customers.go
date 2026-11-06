// Package org is RATE's anticorruption layer for ORG: it turns ORG's party-role answer into RATE's
// customer reference.
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

// CustomerRole is the ORG partner role that lets a party receive quotes.
const CustomerRole = "CUSTOMER"

// Customers confirms customers through ORG's query side. Call it outside RATE's transactions.
type Customers struct{ ORG orgapi.Queries }

func (c Customers) VerifyCustomer(ctx context.Context, tenant rate.TenantID, partyID string, entity rate.LegalEntityID, at time.Time) (rate.CustomerRef, error) {
	role, err := c.ORG.ResolvePartyRole(ctx, orgapi.ResolvePartyRole{
		TenantID: string(tenant), PartyID: partyID, RoleCode: CustomerRole, LegalEntityID: string(entity), At: at,
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
