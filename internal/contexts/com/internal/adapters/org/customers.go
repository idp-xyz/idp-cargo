// Package org is COM's anticorruption layer for ORG: it turns ORG's party-role answer into the customer's
// transaction snapshot.
package org

import (
	"context"
	"errors"
	"fmt"
	"time"

	app "go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
)

// CustomerRole is the ORG partner role a party needs to place orders.
const CustomerRole = "CUSTOMER"

// Customers confirms customers through ORG's query side. Call it outside COM's transactions.
type Customers struct{ ORG orgapi.Queries }

func (c Customers) VerifyCustomer(ctx context.Context, tenant com.TenantID, partyID string, entity com.LegalEntityID, at time.Time) (com.CustomerSnapshot, error) {
	role, err := c.ORG.ResolvePartyRole(ctx, orgapi.ResolvePartyRole{
		TenantID: string(tenant), PartyID: partyID, RoleCode: CustomerRole, LegalEntityID: string(entity), At: at,
	})
	if errors.Is(err, orgapi.ErrRejected) || errors.Is(err, orgapi.ErrNotFound) {
		// ORG's answer is kept as text only, so its error kinds never become COM's.
		return com.CustomerSnapshot{}, fmt.Errorf("%w: %v", app.ErrCustomerNotConfirmed, err)
	}
	if err != nil {
		return com.CustomerSnapshot{}, err
	}
	if role.Ref.IdentitySource != orgapi.IdentitySource || role.Ref.TenantID != string(tenant) || role.Ref.PartyID != partyID || role.RoleCode != CustomerRole {
		return com.CustomerSnapshot{}, fmt.Errorf("%w: ORG answered for another party or role", app.ErrCustomerNotConfirmed)
	}
	return com.CustomerSnapshot{PartyID: role.Ref.PartyID, PartyRevision: role.Ref.PartyRevision, LegalName: role.LegalName,
		CountryCode: role.CountryCode, RoleValidity: role.Validity, RoleEvidenceRef: role.EvidenceRef}, nil
}
