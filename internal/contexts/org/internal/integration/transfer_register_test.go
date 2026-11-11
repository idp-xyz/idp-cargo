//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	orgmodule "go.idp.xyz/idp-cargo/internal/contexts/org"
	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// registerModule runs ORG with thresholds small enough to reach: warning from 3 persons, block from 5, assessment from 8.
func registerModule(t *testing.T) (*orgmodule.Module, org.TenantID, func(string) error) {
	t.Helper()
	db, _ := testDB(t)
	tenant := org.TenantID("cargo-transfer-" + uniqueSuffix())
	module, err := orgmodule.New(db, testAuthorizer{}, orgmodule.WithTransferThresholds(3, 5, 8))
	if err != nil {
		t.Fatal(err)
	}
	sql := func(statement string) error {
		return (scope.TenantTransactor{DB: db, Tenant: string(tenant)}).WithinTransaction(context.Background(), func(txCtx context.Context) error {
			e, err := db.RequireExecutor(txCtx)
			if err != nil {
				return err
			}
			_, err = e.Exec(txCtx, strings.ReplaceAll(statement, "$T", "'"+string(tenant)+"'"))
			return err
		})
	}
	return module, tenant, sql
}

// The register counts each person once a year, warns, blocks without a safeguard and always at the assessment
// threshold, gives a dispatch's keys back, releases a dispatch once, and the database refuses what ORG would not.
func TestContactTransferRegisterOnPostgres(t *testing.T) {
	module, tenant, sql := registerModule(t)
	ctx := context.Background()
	now := time.Now().UTC()
	year := org.TransferYear(now)
	reserve := func(command, dispatch string, keys ...string) (api.TransferReceipt, error) {
		return module.ReserveContactTransfers(ctx, api.ReserveContactTransfers{CommandID: command, TenantID: string(tenant), ActorID: "test-approved",
			DispatchID: dispatch, ContactKeys: keys, OccurredAt: now})
	}
	r, err := reserve("r-1", "DSP-1", "ORG:p:a", "ORG:p:b", "ORG:p:a")
	if err != nil || r.Persons != 2 || r.State != api.TransferOK || r.Year != year || r.NewRevision != 2 {
		t.Fatalf("first dispatch: %+v %v", r, err)
	}
	if again, err := reserve("r-1", "DSP-1", "ORG:p:a", "ORG:p:b", "ORG:p:a"); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err = reserve("r-1", "DSP-1", "ORG:p:z"); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("a command id reused: %v", err)
	}
	if _, err = reserve("r-1b", "DSP-1", "ORG:p:c"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a dispatch reserving twice: %v", err)
	}
	if _, err = reserve("r-bad", "DSP-X", "John Doe +1 555"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a key carrying personal data: %v", err)
	}
	if _, err = module.ReserveContactTransfers(ctx, api.ReserveContactTransfers{CommandID: "r-x", TenantID: string(tenant), ActorID: "intruder",
		DispatchID: "DSP-X", ContactKeys: []string{"ORG:p:x"}, OccurredAt: now}); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("an actor not allowed: %v", err)
	}
	if r, err = reserve("r-2", "DSP-2", "ORG:p:b", "COM:O-1:CONSIGNEE:1"); err != nil || r.Persons != 3 || r.State != api.TransferWarning {
		t.Fatalf("a person counted once: %+v %v", r, err)
	}
	if _, err = reserve("r-3", "DSP-3", "ORG:p:d", "ORG:p:e"); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "TRANSFER_THRESHOLD") {
		t.Fatalf("the block without a safeguard: %v", err)
	}
	if r, err = reserve("r-3b", "DSP-3", "ORG:p:b"); err != nil || r.Persons != 3 {
		t.Fatalf("a person counted already passes the block: %+v %v", r, err)
	}
	reg, err := module.GetContactTransferRegister(ctx, api.GetContactTransferRegister{TenantID: string(tenant), DispatchID: "DSP-2"})
	if err != nil || reg.Persons != 3 || reg.State != api.TransferWarning || reg.Safeguard != nil || !slices.Equal(reg.DispatchKeys, []string{"COM:O-1:CONSIGNEE:1", "ORG:p:b"}) {
		t.Fatalf("register: %+v %v", reg, err)
	}
	safeguard := api.RecordTransferSafeguard{CommandID: "sg-1", TenantID: string(tenant), ActorID: "test-approved", SafeguardID: "SG-1",
		Kind: api.SafeguardStandardContract, Reference: "standard contract filed with the provincial CAC", Validity: kernel.ValidityInterval{From: now.Add(-time.Hour)},
		EvidenceRef: "dms://contracts/sg-1", OccurredAt: now}
	bad := safeguard
	bad.CommandID, bad.SafeguardID, bad.Kind = "sg-bad", "SG-BAD", "SECURITY_ASSESSMENT"
	if _, err = module.RecordTransferSafeguard(ctx, bad); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a safeguard Cargo does not support: %v", err)
	}
	if _, err = module.RecordTransferSafeguard(ctx, safeguard); err != nil {
		t.Fatal(err)
	}
	if r, err = reserve("r-4", "DSP-4", "ORG:p:d", "ORG:p:e"); err != nil || r.Persons != 5 || r.State != api.TransferWarning {
		t.Fatalf("past the block under the standard contract: %+v %v", r, err)
	}
	if _, err = reserve("r-5", "DSP-5", "ORG:p:f", "ORG:p:g", "ORG:p:h"); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "TRANSFER_ASSESSMENT_REQUIRED") {
		t.Fatalf("the assessment threshold: %v", err)
	}
	release := func(command, dispatch string) (api.TransferReceipt, error) {
		return module.ReleaseContactTransfers(ctx, api.ReleaseContactTransfers{CommandID: command, TenantID: string(tenant), ActorID: "test-approved",
			DispatchID: dispatch, OccurredAt: now.Add(time.Minute)})
	}
	if r, err = release("rel-1", "DSP-1"); err != nil || r.Persons != 4 {
		t.Fatalf("DSP-1 released, b still held by DSP-2: %+v %v", r, err)
	}
	if _, err = release("rel-1b", "DSP-1"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("released twice: %v", err)
	}
	if _, err = reserve("r-1c", "DSP-1", "ORG:p:x"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a released dispatch reserving again: %v", err)
	}
	if reg, _ = module.GetContactTransferRegister(ctx, api.GetContactTransferRegister{TenantID: string(tenant), DispatchID: "DSP-1"}); len(reg.DispatchKeys) != 0 ||
		reg.Safeguard == nil || reg.Safeguard.Kind != api.SafeguardStandardContract {
		t.Fatalf("a released dispatch holds nothing: %+v", reg)
	}

	// past ORG on the application role
	for name, tc := range map[string]struct {
		statement string
		state     string
	}{
		"a reservation outside the lock": {`INSERT INTO cargo_org.contact_transfer_dispatch (tenant_id,dispatch_id,year,reserved_by,reserved_at,entered_at)
            VALUES ($T,'DSP-9',` + fmt.Sprint(year) + `,'sql',now(),now()); INSERT INTO cargo_org.contact_transfer_reservation (tenant_id,year,dispatch_id,
            contact_key,reserved_by,reserved_at,entered_at) VALUES ($T,` + fmt.Sprint(year) + `,'DSP-9','ORG:p:q','sql',now(),now())`, "23514"},
		"a dispatch's marker removed": {`DELETE FROM cargo_org.contact_transfer_dispatch WHERE tenant_id=$T`, "42501"},
		"a release before the reservation, on ORG's clock": {`UPDATE cargo_org.contact_transfer_register SET revision=revision+1 WHERE tenant_id=$T;
            UPDATE cargo_org.contact_transfer_reservation SET released_by='sql',released_at=reserved_at,release_entered_at=entered_at-interval '1 second'
            WHERE tenant_id=$T AND dispatch_id='DSP-2'`, "23514"},
		"a reservation deleted":   {`DELETE FROM cargo_org.contact_transfer_reservation WHERE tenant_id=$T`, "42501"},
		"a release undone":        {`UPDATE cargo_org.contact_transfer_reservation SET released_at=NULL,released_by=NULL,release_entered_at=NULL WHERE tenant_id=$T AND dispatch_id='DSP-1'`, "23001"},
		"a key rewritten":         {`UPDATE cargo_org.contact_transfer_reservation SET contact_key='ORG:zz' WHERE tenant_id=$T`, "42501"},
		"a safeguard extended":    {`UPDATE cargo_org.contact_transfer_safeguard SET valid_until=NULL WHERE tenant_id=$T`, "42501"},
		"the register wound back": {`UPDATE cargo_org.contact_transfer_register SET revision=1 WHERE tenant_id=$T`, "23001"},
		"a key with a name in it": {`UPDATE cargo_org.contact_transfer_register SET revision=revision+1 WHERE tenant_id=$T;
            INSERT INTO cargo_org.contact_transfer_dispatch (tenant_id,dispatch_id,year,reserved_by,reserved_at,entered_at) VALUES ($T,'DSP-9',` + fmt.Sprint(year) +
			`,'sql',now(),now()); INSERT INTO cargo_org.contact_transfer_reservation (tenant_id,year,dispatch_id,contact_key,reserved_by,reserved_at,entered_at)
            VALUES ($T,` + fmt.Sprint(year) + `,'DSP-9','ORG:p:zhang.san@example.cn','sql',now(),now())`, "23514"},
	} {
		if got := sqlState(sql(tc.statement)); got != tc.state {
			t.Errorf("%s: SQLSTATE %s, want %s", name, got, tc.state)
		}
	}
}

// TestTransferReservationsRaceAtTheBlock reserves eight new persons at once with the block at 5: four commit.
func TestTransferReservationsRaceAtTheBlock(t *testing.T) {
	module, tenant, _ := registerModule(t)
	if _, err := module.ReserveContactTransfers(context.Background(), api.ReserveContactTransfers{CommandID: "seed", TenantID: string(tenant),
		ActorID: "test-approved", DispatchID: "DSP-0", ContactKeys: []string{"ORG:p:seed"}, OccurredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = module.ReserveContactTransfers(context.Background(), api.ReserveContactTransfers{CommandID: fmt.Sprintf("race-%d", i),
				TenantID: string(tenant), ActorID: "test-approved", DispatchID: fmt.Sprintf("DSP-%d", i+1), ContactKeys: []string{fmt.Sprintf("ORG:p:p%d", i)},
				OccurredAt: time.Now().UTC()})
		}()
	}
	close(start)
	wg.Wait()
	committed, blocked := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, api.ErrRejected) && strings.Contains(err.Error(), "TRANSFER_THRESHOLD"):
			blocked++
		}
	}
	reg, err := module.GetContactTransferRegister(context.Background(), api.GetContactTransferRegister{TenantID: string(tenant)})
	if committed != 3 || blocked != workers-3 || err != nil || reg.Persons != 4 || reg.State != api.TransferWarning || reg.Revision != 5 {
		t.Fatalf("%d committed, %d blocked, register %+v %v: %v", committed, blocked, reg, err, errs)
	}
}

// TestTransferReleaseToNoPersons releases a year's only reservation: the register counts none, and a replay answers
// as the release did.
func TestTransferReleaseToNoPersons(t *testing.T) {
	module, tenant, _ := registerModule(t)
	ctx := context.Background()
	if r, err := module.ReserveContactTransfers(ctx, api.ReserveContactTransfers{CommandID: "only", TenantID: string(tenant), ActorID: "test-approved",
		DispatchID: "DSP-1", ContactKeys: []string{"ORG:p:a"}, OccurredAt: time.Now().UTC()}); err != nil || r.Persons != 1 {
		t.Fatalf("reserve: %+v %v", r, err)
	}
	release := api.ReleaseContactTransfers{CommandID: "release", TenantID: string(tenant), ActorID: "test-approved", DispatchID: "DSP-1",
		OccurredAt: time.Now().UTC()}
	r, err := module.ReleaseContactTransfers(ctx, release)
	if err != nil || r.Persons != 0 || r.State != api.TransferOK {
		t.Fatalf("release to no persons: %+v %v", r, err)
	}
	if again, err := module.ReleaseContactTransfers(ctx, release); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if reg, err := module.GetContactTransferRegister(ctx, api.GetContactTransferRegister{TenantID: string(tenant), DispatchID: "DSP-1"}); err != nil ||
		reg.Persons != 0 || len(reg.DispatchKeys) != 0 {
		t.Fatalf("register: %+v %v", reg, err)
	}
}
