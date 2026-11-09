// Package org is COM's anticorruption layer for ORG: it turns ORG's party-role answer into the ordering
// party's transaction snapshot.
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

// Customers confirms ordering parties through ORG's query side. Call it outside COM's transactions.
type Customers struct{ ORG orgapi.Queries }

// VerifyOrderingParty asks ORG whether the party may act in role, the role the quote went to (R7), for the
// legal entity at the instant.
func (c Customers) VerifyOrderingParty(ctx context.Context, tenant com.TenantID, partyID, role string, entity com.LegalEntityID, at time.Time) (com.CustomerSnapshot, error) {
	switch role {
	case com.RoleCustomer, com.RoleOverseasAgent, com.RoleShipper, com.RoleConsignee:
	default:
		return com.CustomerSnapshot{}, fmt.Errorf("%w: %q is not a role an order is placed in", app.ErrCustomerNotConfirmed, role)
	}
	answer, err := c.ORG.ResolvePartyRole(ctx, orgapi.ResolvePartyRole{
		TenantID: string(tenant), PartyID: partyID, RoleCode: role, LegalEntityID: string(entity), At: at,
	})
	if errors.Is(err, orgapi.ErrRejected) || errors.Is(err, orgapi.ErrNotFound) {
		// ORG's answer is kept as text only, so its error kinds never become COM's.
		return com.CustomerSnapshot{}, fmt.Errorf("%w: %v", app.ErrCustomerNotConfirmed, err)
	}
	if err != nil {
		return com.CustomerSnapshot{}, err
	}
	if answer.Ref.IdentitySource != orgapi.IdentitySource || answer.Ref.TenantID != string(tenant) || answer.Ref.PartyID != partyID || answer.RoleCode != role {
		return com.CustomerSnapshot{}, fmt.Errorf("%w: ORG answered for another party or role", app.ErrCustomerNotConfirmed)
	}
	return com.CustomerSnapshot{PartyID: answer.Ref.PartyID, PartyRevision: answer.Ref.PartyRevision, LegalName: answer.LegalName,
		CountryCode: answer.CountryCode, RoleCode: answer.RoleCode, RoleValidity: answer.Validity, RoleEvidenceRef: answer.EvidenceRef}, nil
}
