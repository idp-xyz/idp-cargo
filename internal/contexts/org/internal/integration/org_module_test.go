//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	orgmodule "go.idp.xyz/idp-cargo/internal/contexts/org"
	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

type testAuthorizer struct{}

func (testAuthorizer) AuthorizeRoleApproval(_ context.Context, _, actorID, _, _ string) error {
	if actorID != "test-approved" {
		return errors.New("test policy: forbidden")
	}
	return nil
}

// AuthorizePartyAction lets test-approved maintain address books and read contacts, and test-clerk
// maintain address books without reading contacts.
func (testAuthorizer) AuthorizePartyAction(_ context.Context, _, actorID, action, _ string) error {
	switch {
	case actorID == "test-approved":
		return nil
	case actorID == "test-clerk" && action != api.ActionReadPartyContacts:
		return nil
	}
	return errors.New("test policy: forbidden")
}

// The module is ORG's only entry point for other code; its errors must classify into api kinds.
func TestModuleApproveRoleThroughAPI(t *testing.T) {
	db, _ := testDB(t)
	tenant := org.TenantID("cargo-module-" + uniqueSuffix())
	seed(t, db, tenant, "party-m")
	module, err := orgmodule.New(db, testAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Microsecond)
	cmd := api.ApproveRole{
		CommandID: "module-1", TenantID: string(tenant), ActorID: "test-approved", LegalEntityID: "LE",
		PartyID: "party-m", ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: at,
		EvidenceRef: "module-evidence", OccurredAt: at,
	}
	receipt, err := module.ApproveRole(ctx, cmd)
	if err != nil || receipt.NewRevision != 2 {
		t.Fatalf("approve: receipt=%+v err=%v", receipt, err)
	}
	if replay, err := module.ApproveRole(ctx, cmd); err != nil || replay != receipt {
		t.Fatalf("replay: receipt=%+v err=%v, want %+v", replay, err, receipt)
	}

	tampered := cmd
	tampered.EvidenceRef = "tampered"
	stale := cmd
	stale.CommandID, stale.RoleCode = "module-2", "SUPPLIER"
	overlap := cmd
	overlap.CommandID, overlap.ExpectedRevision = "module-3", 2
	forbidden := cmd
	forbidden.CommandID, forbidden.ActorID, forbidden.ExpectedRevision, forbidden.RoleCode = "module-4", "intruder", 2, "SUPPLIER"
	missing := cmd
	missing.CommandID, missing.PartyID = "module-5", "no-such-party"
	for name, tc := range map[string]struct {
		cmd  api.ApproveRole
		want error
	}{
		"payload changed under the same command id": {tampered, api.ErrIdempotencyConflict},
		"stale expected revision":                   {stale, api.ErrConflict},
		"overlapping approved role":                 {overlap, api.ErrRejected},
		"actor refused by the authorizer":           {forbidden, api.ErrUnauthorized},
		"unknown party":                             {missing, api.ErrNotFound},
	} {
		if _, err := module.ApproveRole(ctx, tc.cmd); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if n := outboxRows(t, db, tenant); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
}
