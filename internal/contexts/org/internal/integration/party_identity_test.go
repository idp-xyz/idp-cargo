//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	orgmodule "go.idp.xyz/idp-cargo/internal/contexts/org"
	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// 91350100M000100Y43 is the example code of GB 32100; usccFor completes simulated codes with their check
// character, so tests never use a real company's code.
func usccFor(first17 string) string {
	const cs = "0123456789ABCDEFGHJKLMNPQRTUWXY"
	sum, weight := 0, 1
	for i := 0; i < 17; i++ {
		sum += strings.IndexByte(cs, first17[i]) * weight
		weight = weight * 3 % 31
	}
	return first17 + string(cs[(31-sum%31)%31])
}

// A party's identity is maintained through the module: identifiers with evidence, a recorded absence, the
// English name, retirement and correction; each command is one party revision, and role queries return
// the identity in the same view as the revision.
func TestPartyIdentityThroughModule(t *testing.T) {
	db, _ := testDB(t)
	tenant := org.TenantID("cargo-ident-" + uniqueSuffix())
	seed(t, db, tenant, "party-i")
	module, err := orgmodule.New(db, testAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	identity, err := module.GetPartyIdentity(ctx, api.GetPartyIdentity{TenantID: string(tenant), PartyID: "party-i"})
	if err != nil || identity.Revision != 1 || len(identity.Identifiers) != 0 || identity.EnglishName != "" || identity.Status != "ACTIVE" {
		t.Fatalf("before: %+v %v", identity, err)
	}
	record := api.RecordPartyIdentifier{CommandID: "id-1", TenantID: string(tenant), ActorID: "test-clerk", PartyID: "party-i",
		ExpectedRevision: 1, Type: api.IdentifierCNUSCC, Country: "CN", Value: "91350100M000100Y43", EvidenceRef: "national enterprise credit system"}
	receipt, err := module.RecordPartyIdentifier(ctx, record)
	if err != nil || receipt.NewRevision != 2 {
		t.Fatalf("record: %+v %v", receipt, err)
	}
	if replay, err := module.RecordPartyIdentifier(ctx, record); err != nil || replay != receipt {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	tampered := record
	tampered.EvidenceRef = "other"
	stale := record
	stale.CommandID, stale.Type, stale.Value = "id-2", api.IdentifierCNCustoms, "3101234567"
	second := record
	second.CommandID, second.ExpectedRevision, second.Value = "id-3", 2, usccFor("91310115MA1K4ABCD")
	badCheck := record
	badCheck.CommandID, badCheck.ExpectedRevision, badCheck.Type, badCheck.Country, badCheck.Value = "id-4", 2, api.IdentifierAEO, "SG", "CN123"
	forbidden := record
	forbidden.CommandID, forbidden.ActorID, forbidden.ExpectedRevision = "id-5", "intruder", 2
	unknown := record
	unknown.CommandID, unknown.PartyID = "id-6", "no-such-party"
	for name, tc := range map[string]struct {
		cmd  api.RecordPartyIdentifier
		want error
	}{
		"payload changed under the same command id": {tampered, api.ErrIdempotencyConflict},
		"stale expected revision":                   {stale, api.ErrConflict},
		"second active USCC":                        {second, api.ErrRejected},
		"AEO code of another country":               {badCheck, api.ErrRejected},
		"actor refused by the authorizer":           {forbidden, api.ErrUnauthorized},
		"unknown party":                             {unknown, api.ErrNotFound},
	} {
		if _, err := module.RecordPartyIdentifier(ctx, tc.cmd); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if _, err = module.RecordPartyIdentifier(ctx, api.RecordPartyIdentifier{CommandID: "id-7", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-i", ExpectedRevision: 2, Type: api.IdentifierAEO, Country: "CN", None: true, EvidenceRef: "customs credit platform: no AEO"}); err != nil {
		t.Fatal(err)
	}
	if _, err = module.SetPartyEnglishName(ctx, api.SetPartyEnglishName{CommandID: "id-8", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-i", ExpectedRevision: 3, EnglishName: "Sample Logistics Co., Ltd.", EvidenceRef: "business licence"}); err != nil {
		t.Fatal(err)
	}
	if _, err = module.SetPartyEnglishName(ctx, api.SetPartyEnglishName{CommandID: "id-9", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-i", ExpectedRevision: 4, EnglishName: "示例物流", EvidenceRef: "licence"}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("non-ASCII English name: %v", err)
	}
	approved, err := module.ApproveRole(ctx, api.ApproveRole{CommandID: "id-10", TenantID: string(tenant), ActorID: "test-approved", LegalEntityID: "LE",
		PartyID: "party-i", ExpectedRevision: 4, RoleCode: "SHIPPER", ValidFrom: time.Now().UTC().Add(-time.Hour), EvidenceRef: "shipper-contract", OccurredAt: time.Now().UTC()})
	if err != nil || approved.NewRevision != 5 {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	role, err := module.ResolvePartyRole(ctx, api.ResolvePartyRole{TenantID: string(tenant), PartyID: "party-i", RoleCode: "SHIPPER", LegalEntityID: "LE", At: time.Now().UTC()})
	want := []api.PartyIdentifier{{Type: api.IdentifierAEO, Country: "CN", None: true}, {Type: api.IdentifierCNUSCC, Country: "CN", Value: "91350100M000100Y43"}}
	if err != nil || role.Ref.PartyRevision != 5 || role.EnglishName != "Sample Logistics Co., Ltd." || fmt.Sprint(role.Identifiers) != fmt.Sprint(want) {
		t.Fatalf("role answer: %+v %v", role, err)
	}
	if _, err = module.RetirePartyIdentifier(ctx, api.RetirePartyIdentifier{CommandID: "id-11", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-i", ExpectedRevision: 5, Type: api.IdentifierCNCustoms, Country: "CN", Reason: "typo"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("retire missing: %v", err)
	}
	if _, err = module.RetirePartyIdentifier(ctx, api.RetirePartyIdentifier{CommandID: "id-12", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-i", ExpectedRevision: 5, Type: api.IdentifierCNUSCC, Country: "CN", Reason: "code of the parent company entered by mistake"}); err != nil {
		t.Fatal(err)
	}
	corrected := usccFor("91310115MA1K4ABCD")
	if _, err = module.RecordPartyIdentifier(ctx, api.RecordPartyIdentifier{CommandID: "id-13", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-i", ExpectedRevision: 6, Type: api.IdentifierCNUSCC, Country: "CN", Value: strings.ToLower(corrected), EvidenceRef: "licence"}); err != nil {
		t.Fatal(err)
	}
	identity, err = module.GetPartyIdentity(ctx, api.GetPartyIdentity{TenantID: string(tenant), PartyID: "party-i"})
	if err != nil || identity.Revision != 7 || len(identity.Identifiers) != 2 || identity.Identifiers[1].Value != corrected {
		t.Fatalf("after correction: %+v %v", identity, err)
	}
	if n := countRows(t, db, tenant, `SELECT count(*) FROM cargo_org.party_identifier WHERE tenant_id=$1 AND retired_at IS NOT NULL AND retired_revision=6`); n != 1 {
		t.Fatalf("retired entries kept as history: %d", n)
	}
	for _, code := range []string{"91350100M000100Y43", corrected} {
		if n := countRows(t, db, tenant, `SELECT count(*) FROM bento.outbox WHERE scope=$1 AND payload::text LIKE '%'||$2||'%'`, code); n != 0 {
			t.Fatalf("an event carries the code %s", code)
		}
	}
	if n := outboxRows(t, db, tenant); n != 6 {
		t.Fatalf("outbox rows = %d, want one per committed command", n)
	}
}

// A code belongs to one party of a tenant. Eight parties racing to record the same code: one commits, the
// others conflict; a later attempt names the holder.
func TestIdentifierHeldByAnotherPartyConflicts(t *testing.T) {
	db, _ := testDB(t)
	tenant := org.TenantID("cargo-ident-race-" + uniqueSuffix())
	module, err := orgmodule.New(db, testAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		seed(t, db, tenant, org.PartyID(fmt.Sprintf("party-%d", i)))
	}
	code := usccFor("91440300MA5F00001")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = module.RecordPartyIdentifier(context.Background(), api.RecordPartyIdentifier{CommandID: fmt.Sprintf("race-%d", i),
				TenantID: string(tenant), ActorID: "test-clerk", PartyID: fmt.Sprintf("party-%d", i), ExpectedRevision: 1,
				Type: api.IdentifierCNUSCC, Country: "CN", Value: code, EvidenceRef: "registry"})
		}()
	}
	wg.Wait()
	committed, holder := 0, ""
	for i, err := range errs {
		switch {
		case err == nil:
			committed++
			holder = fmt.Sprintf("party-%d", i)
		case !errors.Is(err, api.ErrConflict):
			t.Fatalf("party-%d: %v", i, err)
		}
	}
	if committed != 1 {
		t.Fatalf("%d parties hold the code", committed)
	}
	other := "party-0"
	if holder == other {
		other = "party-1"
	}
	_, err = module.RecordPartyIdentifier(context.Background(), api.RecordPartyIdentifier{CommandID: "later", TenantID: string(tenant),
		ActorID: "test-clerk", PartyID: other, ExpectedRevision: 1, Type: api.IdentifierCNUSCC, Country: "CN", Value: code, EvidenceRef: "registry"})
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), holder) {
		t.Fatalf("later attempt: %v, want a conflict naming %s", err, holder)
	}
	// Another tenant may hold the same code: tenants are separate books.
	elsewhere := org.TenantID("cargo-ident-other-" + uniqueSuffix())
	seed(t, db, elsewhere, "party-x")
	if _, err = module.RecordPartyIdentifier(context.Background(), api.RecordPartyIdentifier{CommandID: "x", TenantID: string(elsewhere),
		ActorID: "test-clerk", PartyID: "party-x", ExpectedRevision: 1, Type: api.IdentifierCNUSCC, Country: "CN", Value: code, EvidenceRef: "registry"}); err != nil {
		t.Fatalf("other tenant: %v", err)
	}
}

// The database refuses identifiers with a wrong check character, rewrites of recorded identifiers, a second
// active entry, and English names outside printable ASCII, whatever writes them.
func TestPartyIdentityDatabaseRefusesWritesPastORG(t *testing.T) {
	db, _ := testDB(t)
	tenant := org.TenantID("cargo-ident-db-" + uniqueSuffix())
	seed(t, db, tenant, "party-d")
	module, err := orgmodule.New(db, testAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = module.RecordPartyIdentifier(context.Background(), api.RecordPartyIdentifier{CommandID: "d-1", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-d", ExpectedRevision: 1, Type: api.IdentifierCNUSCC, Country: "CN", Value: "91350100M000100Y43", EvidenceRef: "registry"}); err != nil {
		t.Fatal(err)
	}
	insert := func(id, typ, value string) string {
		return `INSERT INTO cargo_org.party_identifier(tenant_id,party_id,identifier_id,identifier_type,issuing_country,value,none,evidence_ref,recorded_by,recorded_at,recorded_revision)
          VALUES($T,'party-d',repeat('` + id + `',64),'` + typ + `','CN','` + value + `',false,'sql','sql',now(),9)`
	}
	exec := func(statements ...string) error {
		return (scope.TenantTransactor{DB: db, Tenant: string(tenant)}).WithinTransaction(context.Background(), func(txCtx context.Context) error {
			e, err := db.RequireExecutor(txCtx)
			if err != nil {
				return err
			}
			for _, s := range statements {
				if _, err = e.Exec(txCtx, strings.ReplaceAll(s, "$T", "'"+string(tenant)+"'")); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for name, tc := range map[string]struct {
		statements []string
		state      string
	}{
		"USCC with a wrong check character": {[]string{insert("a", "CN_CUSTOMS", "3101234567"), insert("b", "CN_USCC", "91350100M000100Y44")}, "23514"},
		"second active USCC":                {[]string{insert("c", "CN_USCC", usccFor("91310115MA1K4ABCD"))}, "23505"},
		"rewrite a recorded value":          {[]string{`UPDATE cargo_org.party_identifier SET value='91350100M000100Y43' WHERE tenant_id=$T`}, "42501"},
		"delete an identifier":              {[]string{`DELETE FROM cargo_org.party_identifier WHERE tenant_id=$T`}, "42501"},
		"undo a retirement": {[]string{`UPDATE cargo_org.party_identifier SET retired_by='sql',retired_at=now(),retire_reason='x',retired_revision=5 WHERE tenant_id=$T`,
			`UPDATE cargo_org.party_identifier SET retired_by=NULL,retired_at=NULL,retire_reason=NULL,retired_revision=NULL WHERE tenant_id=$T`}, "23001"},
		"English name in Chinese": {[]string{`UPDATE cargo_org.business_party SET english_name='示例物流' WHERE tenant_id=$T`}, "23514"},
	} {
		if got := sqlState(exec(tc.statements...)); got != tc.state {
			t.Errorf("%s: SQLSTATE %s, want %s", name, got, tc.state)
		}
	}
}

// An address carries the organization's phone and e-mail; a residential address carries neither.
func TestAddressPhoneAndEmailThroughModule(t *testing.T) {
	module, _, tenant := addressModule(t)
	ctx := context.Background()
	hq := officeAddress("HQ")
	hq.Phone, hq.Email = "+86 21 5555 0000", "shipping@example.test"
	if _, err := module.AddPartyAddress(ctx, api.AddPartyAddress{CommandID: "ph-1", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", AddressID: "A-HQ", Address: hq}); err != nil {
		t.Fatal(err)
	}
	got, err := module.GetPartyAddress(ctx, api.GetPartyAddress{TenantID: string(tenant), PartyID: "party-a", AddressID: "A-HQ"})
	if err != nil || got.Phone != "+86 21 5555 0000" || got.Email != "shipping@example.test" {
		t.Fatalf("address: %+v %v", got, err)
	}
	home := warehouseAddress("HOME")
	home.Residential, home.Phone = true, "+86 139 0000 0002"
	if _, err = module.AddPartyAddress(ctx, api.AddPartyAddress{CommandID: "ph-2", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 1, AddressID: "A-HOME", Address: home}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("residential with a phone: %v", err)
	}
}
