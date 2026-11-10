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

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	orgmodule "go.idp.xyz/idp-cargo/internal/contexts/org"
	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// Simulated data only (M1, privacy assessment V1.1 control 8): no real person's name, phone or e-mail.
func officeAddress(code string) api.Address {
	return api.Address{ShortCode: code, Purposes: []string{api.AddressOffice, api.AddressBilling}, Name: "Fixture Trading Co., Ltd.",
		Lines: []string{"Room 1001, 88 Century Avenue", "Pudong New Area"}, City: "Shanghai", Postcode: "200120", CountryCode: "CN",
		UNLocode: "CNSHA", Local: &api.LocalAddress{Language: "zh", Name: "示例贸易有限公司", Lines: []string{"浦东新区世纪大道88号1001室"}, City: "上海"}}
}

func warehouseAddress(code string) api.Address {
	return api.Address{ShortCode: code, Purposes: []string{api.AddressPickup}, Name: "Fixture Warehouse",
		Lines: []string{"Gate 3, 1 Harbour Road"}, City: "Ningbo", Region: "Zhejiang", CountryCode: "CN"}
}

func deskContact() api.Contact {
	return api.Contact{Purposes: []string{api.ContactBooking, api.ContactDocuments}, Name: "Test Person A",
		Phone: "+86 21 5555 0100", Email: "desk-a@example.test"}
}

func addressModule(t *testing.T) (*orgmodule.Module, *bentopg.DB, org.TenantID) {
	t.Helper()
	db, _ := testDB(t)
	tenant := org.TenantID("cargo-addr-" + uniqueSuffix())
	seed(t, db, tenant, "party-a")
	module, err := orgmodule.New(db, testAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	return module, db, tenant
}

func countRows(t *testing.T, db *bentopg.DB, tenant org.TenantID, sql string, args ...any) int {
	t.Helper()
	var n int
	err := (scope.TenantTransactor{DB: db, Tenant: string(tenant)}).WithinTransaction(context.Background(), func(txCtx context.Context) error {
		q, err := db.ReadExecutor(txCtx)
		if err != nil {
			return err
		}
		return q.QueryRow(txCtx, sql, append([]any{string(tenant)}, args...)...).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// ORG keeps a party's addresses by purpose with a default per purpose and a version per change (C9); the
// commands replay by command id and classify into the api error kinds.
func TestAddressBookThroughModule(t *testing.T) {
	module, db, tenant := addressModule(t)
	ctx := context.Background()
	add := api.AddPartyAddress{CommandID: "addr-1", TenantID: string(tenant), ActorID: "test-clerk", PartyID: "party-a",
		AddressID: "A-HQ", Address: officeAddress("HQ"), DefaultFor: []string{api.AddressOffice, api.AddressBilling}}
	receipt, err := module.AddPartyAddress(ctx, add)
	if err != nil || receipt.NewRevision != 1 || receipt.EntryID != "A-HQ" || receipt.EntryVersion != 1 {
		t.Fatalf("add: %+v %v", receipt, err)
	}
	if replay, err := module.AddPartyAddress(ctx, add); err != nil || replay != receipt {
		t.Fatalf("replay: %+v %v, want %+v", replay, err, receipt)
	}
	tampered := add
	tampered.Address.City = "Suzhou"
	stale := add
	stale.CommandID, stale.AddressID, stale.Address = "addr-2", "A-WH", warehouseAddress("WH")
	sameCode := add
	sameCode.CommandID, sameCode.AddressID, sameCode.ExpectedRevision, sameCode.DefaultFor = "addr-3", "A-HQ2", 1, nil
	reused := add
	reused.CommandID, reused.ExpectedRevision, reused.Address = "addr-4", 1, warehouseAddress("WH")
	invalid := add
	invalid.CommandID, invalid.AddressID, invalid.ExpectedRevision = "addr-5", "A-X", 1
	invalid.Address.UNLocode = "USNYC"
	forbidden := add
	forbidden.CommandID, forbidden.ActorID = "addr-6", "intruder"
	unknown := add
	unknown.CommandID, unknown.PartyID = "addr-7", "no-such-party"
	for name, tc := range map[string]struct {
		cmd  api.AddPartyAddress
		want error
	}{
		"payload changed under the same command id": {tampered, api.ErrIdempotencyConflict},
		"stale expected revision":                   {stale, api.ErrConflict},
		"short code of another active address":      {sameCode, api.ErrRejected},
		"address id used before":                    {reused, api.ErrConflict},
		"UN/LOCODE outside the country":             {invalid, api.ErrRejected},
		"actor refused by the authorizer":           {forbidden, api.ErrUnauthorized},
		"unknown party":                             {unknown, api.ErrNotFound},
	} {
		if _, err := module.AddPartyAddress(ctx, tc.cmd); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}

	if _, err = module.AddPartyAddress(ctx, api.AddPartyAddress{CommandID: "addr-8", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 1, AddressID: "A-WH", Address: warehouseAddress("WH"), DefaultFor: []string{api.AddressPickup}}); err != nil {
		t.Fatal(err)
	}
	moved := officeAddress("HQ")
	moved.Purposes = []string{api.AddressOffice}
	moved.Lines = []string{"Room 2002, 88 Century Avenue", "Pudong New Area"}
	changed, err := module.ChangePartyAddress(ctx, api.ChangePartyAddress{CommandID: "addr-9", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 2, AddressID: "A-HQ", Address: moved, Reason: "moved to another floor; billing goes elsewhere"})
	if err != nil || changed.NewRevision != 3 || changed.EntryVersion != 2 {
		t.Fatalf("change: %+v %v", changed, err)
	}

	book, err := module.GetPartyAddresses(ctx, api.GetPartyAddresses{TenantID: string(tenant), PartyID: "party-a"})
	if err != nil || book.Revision != 3 || len(book.Addresses) != 2 {
		t.Fatalf("addresses: %+v %v", book, err)
	}
	hq := book.Addresses[0]
	if hq.AddressID != "A-HQ" || hq.Version != 2 || !slices.Equal(hq.DefaultFor, []string{api.AddressOffice}) ||
		hq.Local == nil || hq.Local.Name != "示例贸易有限公司" || hq.UNLocode != "CNSHA" || hq.Lines[0] != "Room 2002, 88 Century Avenue" {
		t.Fatalf("current HQ = %+v; billing must have stopped being its default with the purpose", hq)
	}
	if wh := book.Addresses[1]; wh.Region != "Zhejiang" || wh.Local != nil || !slices.Equal(wh.DefaultFor, []string{api.AddressPickup}) {
		t.Fatalf("warehouse = %+v", wh)
	}
	first, err := module.GetPartyAddress(ctx, api.GetPartyAddress{TenantID: string(tenant), PartyID: "party-a", AddressID: "A-HQ", Version: 1})
	if err != nil || first.Version != 1 || first.Lines[0] != "Room 1001, 88 Century Avenue" || len(first.Purposes) != 2 || first.BookRevision != 3 {
		t.Fatalf("version 1 = %+v %v", first, err)
	}
	if _, err = module.GetPartyAddress(ctx, api.GetPartyAddress{TenantID: string(tenant), PartyID: "party-a", AddressID: "A-HQ", Version: 3}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("version 3: %v", err)
	}
	if _, err = module.GetPartyAddress(ctx, api.GetPartyAddress{TenantID: string(tenant), PartyID: "party-a", AddressID: "nope"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown address: %v", err)
	}
	if empty, err := module.GetPartyAddresses(ctx, api.GetPartyAddresses{TenantID: string(tenant), PartyID: "no-such-party"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown party: %+v %v", empty, err)
	}

	deactivated, err := module.DeactivatePartyAddress(ctx, api.DeactivatePartyAddress{CommandID: "addr-10", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 3, AddressID: "A-HQ", Reason: "office closed"})
	if err != nil || deactivated.EntryVersion != 3 {
		t.Fatalf("deactivate: %+v %v", deactivated, err)
	}
	if _, err = module.SetDefaultPartyAddress(ctx, api.SetDefaultPartyAddress{CommandID: "addr-11", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 4, Purpose: api.AddressOffice, AddressID: "A-HQ"}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("default to an inactive address: %v", err)
	}
	if _, err = module.AddPartyAddress(ctx, api.AddPartyAddress{CommandID: "addr-12", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 4, AddressID: "A-HQ-NEW", Address: officeAddress("HQ")}); err != nil {
		t.Fatalf("the short code of an inactive address is free: %v", err)
	}
	if set, err := module.SetDefaultPartyAddress(ctx, api.SetDefaultPartyAddress{CommandID: "addr-13", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 5, Purpose: api.AddressBilling, AddressID: "A-HQ-NEW"}); err != nil || set.NewRevision != 6 {
		t.Fatalf("set default: %+v %v", set, err)
	}

	// The book's revision is its own: the party's identity revision, which PartyRef carries, is unchanged.
	if n := countRows(t, db, tenant, `SELECT revision FROM cargo_org.business_party WHERE tenant_id=$1 AND party_id='party-a'`); n != 1 {
		t.Fatalf("party revision = %d, want 1", n)
	}
	if n := countRows(t, db, tenant, `SELECT count(*) FROM cargo_org.party_address_book_change WHERE tenant_id=$1`); n != 6 {
		t.Fatalf("change records = %d, want one per revision", n)
	}
	if n := outboxRows(t, db, tenant); n != 6 {
		t.Fatalf("outbox rows = %d, want one per command", n)
	}
}

// Contacts are personal data: reading them needs its own authorization, and erasing one leaves no name,
// phone or e-mail in any version, event or receipt (privacy assessment V1.1, controls 1, 2 and 5).
func TestAddressBookContactsReadAuthorizationAndErasure(t *testing.T) {
	module, db, tenant := addressModule(t)
	ctx := context.Background()
	add := api.AddPartyContact{CommandID: "contact-1", TenantID: string(tenant), ActorID: "test-clerk", PartyID: "party-a",
		ContactID: "C-DESK", Contact: deskContact(), DefaultFor: []string{api.ContactBooking}}
	if r, err := module.AddPartyContact(ctx, add); err != nil || r.NewRevision != 1 || r.EntryVersion != 1 {
		t.Fatalf("add contact: %+v %v", r, err)
	}
	changed := deskContact()
	changed.Mobile = "+86 139 0000 0001"
	if _, err := module.ChangePartyContact(ctx, api.ChangePartyContact{CommandID: "contact-2", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 1, ContactID: "C-DESK", Contact: changed, Reason: "mobile added"}); err != nil {
		t.Fatal(err)
	}
	other := api.Contact{Purposes: []string{api.ContactAccounts}, Name: "Test Person B", Email: "accounts-b@example.test"}
	if _, err := module.AddPartyContact(ctx, api.AddPartyContact{CommandID: "contact-3", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 2, ContactID: "C-ACC", Contact: other}); err != nil {
		t.Fatal(err)
	}
	if _, err := module.SetDefaultPartyContact(ctx, api.SetDefaultPartyContact{CommandID: "contact-4", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 3, Purpose: api.ContactAccounts, ContactID: "C-ACC"}); err != nil {
		t.Fatal(err)
	}

	if _, err := module.GetPartyContacts(ctx, api.GetPartyContacts{TenantID: string(tenant), ActorID: "test-clerk", PartyID: "party-a"}); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("a clerk who maintains addresses may not read contacts: %v", err)
	}
	contacts, err := module.GetPartyContacts(ctx, api.GetPartyContacts{TenantID: string(tenant), ActorID: "test-approved", PartyID: "party-a"})
	if err != nil || contacts.Revision != 4 || len(contacts.Contacts) != 2 {
		t.Fatalf("contacts: %+v %v", contacts, err)
	}
	if acc, desk := contacts.Contacts[0], contacts.Contacts[1]; acc.ContactID != "C-ACC" || !slices.Equal(acc.DefaultFor, []string{api.ContactAccounts}) ||
		desk.Version != 2 || desk.Mobile != "+86 139 0000 0001" || !slices.Equal(desk.DefaultFor, []string{api.ContactBooking}) {
		t.Fatalf("contacts = %+v", contacts.Contacts)
	}
	addresses, err := module.GetPartyAddresses(ctx, api.GetPartyAddresses{TenantID: string(tenant), PartyID: "party-a"})
	if err != nil || addresses.Revision != 4 || len(addresses.Addresses) != 0 {
		t.Fatalf("addresses of a book with contacts only: %+v %v", addresses, err)
	}

	erased, err := module.ErasePartyContact(ctx, api.ErasePartyContact{CommandID: "contact-5", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 4, ContactID: "C-DESK", Reason: "the person left the customer"})
	if err != nil || erased.EntryVersion != 3 || erased.NewRevision != 5 {
		t.Fatalf("erase: %+v %v", erased, err)
	}
	contacts, err = module.GetPartyContacts(ctx, api.GetPartyContacts{TenantID: string(tenant), ActorID: "test-approved", PartyID: "party-a"})
	if err != nil {
		t.Fatal(err)
	}
	desk := contacts.Contacts[1]
	if desk.Status != api.StatusErased || desk.Name != "" || desk.Phone != "" || desk.Mobile != "" || desk.Email != "" || len(desk.DefaultFor) != 0 ||
		!slices.Equal(desk.Purposes, []string{api.ContactBooking, api.ContactDocuments}) {
		t.Fatalf("erased contact = %+v", desk)
	}
	if n := countRows(t, db, tenant, `SELECT count(*) FROM cargo_org.party_contact_version WHERE tenant_id=$1 AND contact_id='C-DESK'
        AND (name IS NOT NULL OR phone IS NOT NULL OR mobile IS NOT NULL OR email IS NOT NULL OR NOT erased)`); n != 0 {
		t.Fatalf("%d versions of the erased contact keep personal data", n)
	}
	if n := countRows(t, db, tenant, `SELECT count(*) FROM cargo_org.party_contact_version WHERE tenant_id=$1 AND contact_id='C-DESK'`); n != 3 {
		t.Fatalf("erasure keeps the versions: %d, want 3", n)
	}
	for _, needle := range []string{"Test Person", "example.test", "5555", "139 0000"} {
		if n := countRows(t, db, tenant, `SELECT count(*) FROM bento.outbox WHERE scope=$1 AND payload::text LIKE '%'||$2||'%'`, needle); n != 0 {
			t.Fatalf("an event payload carries %q", needle)
		}
		if n := countRows(t, db, tenant, `SELECT count(*) FROM cargo_org.command_receipt WHERE tenant_id=$1 AND
            (result_id LIKE '%'||$2||'%' OR coalesce(result_entry_id,'') LIKE '%'||$2||'%')`, needle); n != 0 {
			t.Fatalf("a receipt carries %q", needle)
		}
	}
	if _, err = module.ErasePartyContact(ctx, api.ErasePartyContact{CommandID: "contact-6", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 5, ContactID: "C-DESK", Reason: "again"}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("erase twice: %v", err)
	}
	if _, err = module.DeactivatePartyContact(ctx, api.DeactivatePartyContact{CommandID: "contact-7", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 5, ContactID: "C-ACC", Reason: "left"}); err != nil {
		t.Fatal(err)
	}
}

// An archived party's address book is closed.
func TestAddressBookOfArchivedPartyIsClosed(t *testing.T) {
	module, db, tenant := addressModule(t)
	execInTenant(t, db, tenant, `UPDATE cargo_org.business_party SET status='ARCHIVED' WHERE tenant_id=$1 AND party_id='party-a'`, string(tenant))
	_, err := module.AddPartyAddress(context.Background(), api.AddPartyAddress{CommandID: "arch-1", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", AddressID: "A1", Address: officeAddress("HQ")})
	if !errors.Is(err, api.ErrRejected) {
		t.Fatalf("archived party: %v", err)
	}
}

// Eight commands race on one address book revision: exactly one commits, the others conflict.
func TestAddressBookConcurrentCommandsOneCommits(t *testing.T) {
	module, db, tenant := addressModule(t)
	ctx := context.Background()
	race := func(round string, run func(i int) error) {
		t.Helper()
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Add(1)
			go func() { defer wg.Done(); errs[i] = run(i) }()
		}
		wg.Wait()
		committed, conflicts := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				committed++
			case errors.Is(err, api.ErrConflict):
				conflicts++
			default:
				t.Fatalf("%s: unexpected %v", round, err)
			}
		}
		if committed != 1 || conflicts != 7 {
			t.Fatalf("%s: %d committed, %d conflicts; want 1 and 7", round, committed, conflicts)
		}
	}
	race("first addresses of a party without a book", func(i int) error {
		_, err := module.AddPartyAddress(ctx, api.AddPartyAddress{CommandID: fmt.Sprintf("race-add-%d", i), TenantID: string(tenant),
			ActorID: "test-clerk", PartyID: "party-a", AddressID: fmt.Sprintf("A%d", i), Address: warehouseAddress(fmt.Sprintf("W%d", i)),
			DefaultFor: []string{api.AddressPickup}})
		return err
	})
	book, err := module.GetPartyAddresses(ctx, api.GetPartyAddresses{TenantID: string(tenant), PartyID: "party-a"})
	if err != nil || len(book.Addresses) != 1 {
		t.Fatalf("after the race: %+v %v", book, err)
	}
	winner := book.Addresses[0]
	race("changes at revision 1", func(i int) error {
		a := warehouseAddress(winner.ShortCode)
		a.Lines = []string{fmt.Sprintf("Gate %d, 1 Harbour Road", i+10)}
		_, err := module.ChangePartyAddress(ctx, api.ChangePartyAddress{CommandID: fmt.Sprintf("race-change-%d", i), TenantID: string(tenant),
			ActorID: "test-clerk", PartyID: "party-a", ExpectedRevision: 1, AddressID: winner.AddressID, Address: a, Reason: "gate"})
		return err
	})
	if n := countRows(t, db, tenant, `SELECT count(*) FROM cargo_org.party_address_version WHERE tenant_id=$1`); n != 2 {
		t.Fatalf("address versions = %d, want 2", n)
	}
	if n := outboxRows(t, db, tenant); n != 2 {
		t.Fatalf("outbox rows = %d, want 2", n)
	}
}

// The database refuses writes past ORG that rewrite history or leave an address book inconsistent.
func TestAddressBookDatabaseRefusesWritesPastORG(t *testing.T) {
	module, db, tenant := addressModule(t)
	ctx := context.Background()
	if _, err := module.AddPartyAddress(ctx, api.AddPartyAddress{CommandID: "db-1", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", AddressID: "A1", Address: officeAddress("HQ"), DefaultFor: []string{api.AddressOffice}}); err != nil {
		t.Fatal(err)
	}
	if _, err := module.AddPartyAddress(ctx, api.AddPartyAddress{CommandID: "db-2", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 1, AddressID: "A2", Address: warehouseAddress("WH")}); err != nil {
		t.Fatal(err)
	}
	if _, err := module.AddPartyContact(ctx, api.AddPartyContact{CommandID: "db-3", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 2, ContactID: "C1", Contact: deskContact()}); err != nil {
		t.Fatal(err)
	}
	inTenant := func(statements ...string) error {
		return (scope.TenantTransactor{DB: db, Tenant: string(tenant)}).WithinTransaction(ctx, func(txCtx context.Context) error {
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
	next := func(action, entry, purpose string) string {
		return `UPDATE cargo_org.party_address_book SET revision=4 WHERE tenant_id=$T AND party_id='party-a';
            INSERT INTO cargo_org.party_address_book_change(tenant_id,party_id,revision,action,entry_id,purpose,actor_id,reason,recorded_at)
            VALUES($T,'party-a',4,'` + action + `','` + entry + `',` + purpose + `,'sql','by hand',now())`
	}
	for name, tc := range map[string]struct {
		statements []string
		state      string
	}{
		"rewrite an address version":          {[]string{`UPDATE cargo_org.party_address_version SET city='Elsewhere' WHERE tenant_id=$T`}, "42501"},
		"delete a change record":              {[]string{`DELETE FROM cargo_org.party_address_book_change WHERE tenant_id=$T`}, "42501"},
		"write a name into a contact version": {[]string{`UPDATE cargo_org.party_contact_version SET name='Someone' WHERE tenant_id=$T`}, "23001"},
		"erase without the ERASED status": {[]string{`UPDATE cargo_org.party_contact_version SET name=NULL,phone=NULL,mobile=NULL,email=NULL,erased=true
            WHERE tenant_id=$T`}, "23514"},
		"default to an address without the purpose": {[]string{next("SET_DEFAULT_ADDRESS", "A2", "'OFFICE'"),
			`INSERT INTO cargo_org.party_address_default(tenant_id,party_id,purpose,revision,address_id) VALUES($T,'party-a','OFFICE',4,'A2')`}, "23514"},
		"deactivate the default address without clearing it": {[]string{next("DEACTIVATE_ADDRESS", "A1", "NULL"),
			`UPDATE cargo_org.party_address SET status='INACTIVE',version=2 WHERE tenant_id=$T AND address_id='A1'`,
			`INSERT INTO cargo_org.party_address_version SELECT tenant_id,party_id,address_id,2,4,'INACTIVE',short_code,purposes,name,lines,city,region,
               postcode,country_code,unlocode,residential,local_language,local_name,local_lines,local_city,local_region
               FROM cargo_org.party_address_version WHERE tenant_id=$T AND address_id='A1'`}, "23514"},
		"a version the address does not point to": {[]string{next("CHANGE_ADDRESS", "A2", "NULL"),
			`INSERT INTO cargo_org.party_address_version SELECT tenant_id,party_id,address_id,2,4,status,short_code,purposes,'Other',lines,city,region,
               postcode,country_code,unlocode,residential,local_language,local_name,local_lines,local_city,local_region
               FROM cargo_org.party_address_version WHERE tenant_id=$T AND address_id='A2'`}, "23514"},
		"a revision without its change record": {[]string{`UPDATE cargo_org.party_address_book SET revision=9 WHERE tenant_id=$T`}, "23514"},
	} {
		err := inTenant(tc.statements...)
		if got := sqlState(err); got != tc.state {
			t.Errorf("%s: SQLSTATE %s (%v), want %s", name, got, err, tc.state)
		}
	}
	// ORG still reads and maintains the book after the refused writes.
	if _, err := module.DeactivatePartyAddress(ctx, api.DeactivatePartyAddress{CommandID: "db-4", TenantID: string(tenant), ActorID: "test-clerk",
		PartyID: "party-a", ExpectedRevision: 3, AddressID: "A1", Reason: "closed"}); err != nil {
		t.Fatal(err)
	}
	// Other tenants see nothing of the book.
	other := org.TenantID("cargo-addr-other-" + uniqueSuffix())
	if n := countRows(t, db, other, `SELECT count(*) FROM cargo_org.party_address_version WHERE tenant_id<>$1`); n != 0 {
		t.Fatalf("another tenant reads %d address versions", n)
	}
}
