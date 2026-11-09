package org

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	app "go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

type orgStub struct {
	role  orgapi.PartyRole
	err   error
	asked *[]string
}

func (o orgStub) ResolvePartyRole(_ context.Context, q orgapi.ResolvePartyRole) (orgapi.PartyRole, error) {
	if o.asked != nil {
		*o.asked = append(*o.asked, q.RoleCode)
	}
	return o.role, o.err
}

func TestCustomersTranslatePartyRole(t *testing.T) {
	at := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	role := orgapi.PartyRole{
		Ref:       orgapi.PartyRef{TenantID: "T", PartyID: "P", PartyRevision: 4, IdentitySource: orgapi.IdentitySource, LegalEntityScope: "LE"},
		LegalName: "Fixture Corp", CountryCode: "US", RoleCode: com.RoleOverseasAgent,
		Validity: kernel.ValidityInterval{From: at, Until: at.Add(time.Hour)}, EvidenceRef: "customer-contract",
	}
	var asked []string
	got, err := Customers{ORG: orgStub{role: role, asked: &asked}}.VerifyOrderingParty(context.Background(), "T", "P", com.RoleOverseasAgent, "LE", at)
	want := com.CustomerSnapshot{PartyID: "P", PartyRevision: 4, LegalName: "Fixture Corp", CountryCode: "US", RoleCode: com.RoleOverseasAgent,
		RoleValidity: role.Validity, RoleEvidenceRef: "customer-contract"}
	if err != nil || got != want || len(asked) != 1 || asked[0] != com.RoleOverseasAgent {
		t.Fatalf("snapshot %+v, %v; ORG asked for %v", got, err, asked)
	}
	if _, err = (Customers{ORG: orgStub{role: role}}).VerifyOrderingParty(context.Background(), "T", "P", "CARRIER", "LE", at); !errors.Is(err, app.ErrCustomerNotConfirmed) {
		t.Fatalf("a role no order is placed in: err = %v", err)
	}
	repositoryNotFound := errors.New("repository: not found")
	foreign := role
	foreign.Ref.IdentitySource = "LEGACY"
	otherParty := role
	otherParty.Ref.PartyID = "P-2"
	otherRole := role
	otherRole.RoleCode = com.RoleCustomer
	for name, stub := range map[string]orgStub{
		"role not effective":       {err: fmt.Errorf("%w: role ended", orgapi.ErrRejected)},
		"party unknown to ORG":     {err: fmt.Errorf("%w: %w", orgapi.ErrNotFound, repositoryNotFound)},
		"reference not ORG-issued": {role: foreign},
		"answer for another party": {role: otherParty},
		"answer for another role":  {role: otherRole},
	} {
		_, err := Customers{ORG: stub}.VerifyOrderingParty(context.Background(), "T", "P", com.RoleOverseasAgent, "LE", at)
		if !errors.Is(err, app.ErrCustomerNotConfirmed) || errors.Is(err, orgapi.ErrNotFound) || errors.Is(err, repositoryNotFound) {
			t.Errorf("%s: err = %v, want only ErrCustomerNotConfirmed in its chain", name, err)
		}
	}
	reset := errors.New("connection reset")
	if _, err := (Customers{ORG: orgStub{err: reset}}).VerifyOrderingParty(context.Background(), "T", "P", com.RoleCustomer, "LE", at); !errors.Is(err, reset) {
		t.Fatalf("other failures pass through: err = %v", err)
	}
}
