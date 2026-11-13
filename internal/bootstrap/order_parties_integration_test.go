//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// contactsAuthorizer allows every COM action but reading the contacts frozen on orders, which only the
// documentation officer may (privacy assessment V1.1, control 6).
type contactsAuthorizer struct{ allowAll }

func (contactsAuthorizer) AuthorizeOrderAction(_ context.Context, _, actor, action, _ string) error {
	if action == comapi.ActionReadOrderContacts && actor != "doc-officer" {
		return errors.New("test policy: only the documentation officer reads order contacts")
	}
	return nil
}

// COM V0.6 with the real contexts (C9, C11-C14): an order carries its shipper, consignee and notify party
// from ORG with their identifiers, default addresses and contact; a door movement carries the pickup address;
// overrides stay on the order; ORG changes never reach a frozen order; a TO ORDER consignee binds the order to
// SEA; contacts are read with their own authorization and never travel in events.
func TestOrderPartiesWithRealContexts(t *testing.T) {
	ctx := context.Background()
	platform := openPlatform(t)
	orgModule, err := platform.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := platform.RATE(orgModule, allowAll{}, marginFloor(2000))
	if err != nil {
		t.Fatal(err)
	}
	comModule, err := platform.COM(orgModule, rateModule, contactsAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-com06-")
	seedORG(t, platform, tenant, "customer", "shipper-co", "consignee-co", "notify-co")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	for party, role := range map[string]string{"customer": "CUSTOMER", "shipper-co": "SHIPPER", "consignee-co": "CONSIGNEE"} {
		if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-" + party, TenantID: tenant, ActorID: "org-officer",
			LegalEntityID: "LE", PartyID: party, ExpectedRevision: 1, RoleCode: role, ValidFrom: at, EvidenceRef: role + "-agreement", OccurredAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = orgModule.RecordPartyIdentifier(ctx, orgapi.RecordPartyIdentifier{CommandID: "uscc-shipper", TenantID: tenant, ActorID: "org-officer",
		PartyID: "shipper-co", ExpectedRevision: 2, Type: orgapi.IdentifierCNUSCC, Country: "CN", Value: "91350100M000100Y43", EvidenceRef: "registry"}); err != nil {
		t.Fatal(err)
	}
	if _, err = orgModule.SetPartyEnglishName(ctx, orgapi.SetPartyEnglishName{CommandID: "en-shipper", TenantID: tenant, ActorID: "org-officer",
		PartyID: "shipper-co", ExpectedRevision: 3, EnglishName: "Sample Trading Co., Ltd.", EvidenceRef: "licence"}); err != nil {
		t.Fatal(err)
	}
	address := func(code string, purposes ...string) orgapi.Address {
		return orgapi.Address{ShortCode: code, Purposes: purposes, Name: "Fixture " + code, Lines: []string{"1 " + code + " Road"}, City: "Shanghai",
			CountryCode: "CN", UNLocode: "CNSHA", Phone: "+86 21 5555 0000"}
	}
	book := []struct {
		party, id, code, purpose string
	}{
		{"shipper-co", "S-HQ", "HQ", orgapi.AddressOffice}, {"shipper-co", "S-WH", "WH", orgapi.AddressPickup},
		{"consignee-co", "C-HQ", "HQ", orgapi.AddressOffice}, {"notify-co", "N-HQ", "HQ", orgapi.AddressOffice},
	}
	revisions := map[string]int64{}
	for _, b := range book {
		r, err := orgModule.AddPartyAddress(ctx, orgapi.AddPartyAddress{CommandID: "addr-" + b.id, TenantID: tenant, ActorID: "org-officer",
			PartyID: b.party, ExpectedRevision: revisions[b.party], AddressID: b.id, Address: address(b.code, b.purpose), DefaultFor: []string{b.purpose}})
		if err != nil {
			t.Fatalf("address %s: %v", b.id, err)
		}
		revisions[b.party] = r.NewRevision
	}
	if _, err = orgModule.AddPartyContact(ctx, orgapi.AddPartyContact{CommandID: "contact-shipper", TenantID: tenant, ActorID: "org-officer",
		PartyID: "shipper-co", ExpectedRevision: revisions["shipper-co"], ContactID: "S-DOCS",
		Contact:    orgapi.Contact{Purposes: []string{orgapi.ContactDocuments}, Name: "Test Person A", Email: "docs-a@example.test"},
		DefaultFor: []string{orgapi.ContactDocuments}}); err != nil {
		t.Fatal(err)
	}
	revisions["shipper-co"]++
	if _, err = rateModule.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "quote", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1", LegalEntityID: "LE",
		CustomerPartyID: "customer", Currency: "USD", Lines: []rateapi.QuoteLine{{ChargeCode: "AIR_FREIGHT", Sell: usd(100_000), Cost: usd(50_000)}},
		Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err = rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1",
		ExpectedRevision: 1, EvidenceKind: rateapi.EvidenceEmail, EvidenceRef: "mail", OccurredAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = comModule.SetOrderPolicy(ctx, comapi.SetOrderPolicy{CommandID: "policy", TenantID: tenant, ActorID: "lead", LegalEntityID: "LE", MaxOrdersPerQuote: 5}); err != nil {
		t.Fatal(err)
	}
	pallets := comapi.OrderLine{Description: "Pallets of machine parts", Packs: 10, PackageType: "PX", GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 500}}
	order := func(id string, transport comapi.Transport, parties ...comapi.OrderParty) comapi.RecordCustomerOrder {
		return comapi.RecordCustomerOrder{CommandID: "record-" + id, TenantID: tenant, ActorID: "cs-1", OrderID: id, QuoteID: "Q-1",
			CustomerPartyID: "customer", Lines: []comapi.OrderLine{pallets}, OccurredAt: at.Add(2 * time.Hour), Transport: transport, Parties: parties}
	}
	shipper := comapi.OrderParty{Role: comapi.PartyShipper, PartyID: "shipper-co"}
	consignee := comapi.OrderParty{Role: comapi.PartyConsignee, PartyID: "consignee-co"}
	notify := comapi.OrderParty{Role: comapi.PartyNotify, PartyID: "notify-co"}
	if _, err = comModule.RecordCustomerOrder(ctx, order("O-1", comapi.Transport{Mode: comapi.ModeSea, Movement: comapi.DoorToPort}, shipper, consignee, notify)); err != nil {
		t.Fatalf("order with parties: %v", err)
	}
	parties, err := comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, OrderID: "O-1"})
	if err != nil || len(parties.Parties) != 3 || parties.Transport.Movement != comapi.DoorToPort {
		t.Fatalf("order parties: %+v %v", parties, err)
	}
	s := parties.Parties[0]
	if s.Role != comapi.PartyShipper || s.EnglishName != "Sample Trading Co., Ltd." || len(s.Identifiers) != 1 || s.Identifiers[0].Value != "91350100M000100Y43" ||
		s.Address.ORGAddressID != "S-HQ" || s.Address.Address.Phone != "+86 21 5555 0000" || s.Pickup == nil || s.Pickup.ORGAddressID != "S-WH" ||
		!s.HasContact || s.Contact != nil || s.RoleCode != "SHIPPER" || s.Overridden {
		t.Fatalf("shipper = %+v", s)
	}
	if n := parties.Parties[2]; n.Role != comapi.PartyNotify || n.RoleCode != "" || n.HasContact {
		t.Fatalf("notify party = %+v", n)
	}
	if _, err = comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, ActorID: "cs-1", OrderID: "O-1", WithContacts: true, Purpose: kernel.PurposeDocuments}); !errors.Is(err, comapi.ErrUnauthorized) {
		t.Fatalf("contacts for an actor not allowed: %v", err)
	}
	withContacts, err := comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, ActorID: "doc-officer", OrderID: "O-1", WithContacts: true, Purpose: kernel.PurposeDocuments})
	if err != nil || withContacts.Parties[0].Contact == nil || withContacts.Parties[0].Contact.Contact.Name != "Test Person A" || withContacts.Parties[0].Contact.ORGContactID != "S-DOCS" {
		t.Fatalf("contacts for the documentation officer: %+v %v", withContacts.Parties, err)
	}

	// A door delivery needs the consignee's delivery address; ORG has no default, so the order gives one.
	if _, err = comModule.RecordCustomerOrder(ctx, order("O-2", comapi.Transport{Mode: comapi.ModeSea, Movement: comapi.PortToDoor}, consignee)); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("door delivery without an address: %v", err)
	}
	site := comapi.Address{Name: "Fixture Site Office", Lines: []string{"Plot 9, Port Road"}, City: "Rotterdam", CountryCode: "NL"}
	delivering := consignee
	delivering.Delivery = &comapi.AddressChoice{Override: &site, Reason: "delivery to the construction site"}
	if _, err = comModule.RecordCustomerOrder(ctx, order("O-2", comapi.Transport{Mode: comapi.ModeSea, Movement: comapi.PortToDoor}, delivering)); err != nil {
		t.Fatalf("door delivery given on the order: %v", err)
	}
	o2, err := comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, OrderID: "O-2"})
	if err != nil || !o2.Parties[0].Overridden || o2.Parties[0].Delivery.Source != comapi.FromOrder || o2.Parties[0].Delivery.OverriddenBy != "cs-1" {
		t.Fatalf("overridden delivery: %+v %v", o2.Parties, err)
	}
	if _, err = comModule.RecordCustomerOrder(ctx, order("O-3", comapi.Transport{}, comapi.OrderParty{Role: comapi.PartyConsignee, PartyID: "notify-co"})); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("a consignee without the ORG role: %v", err)
	}
	if _, err = comModule.RecordCustomerOrder(ctx, order("O-3", comapi.Transport{}, comapi.OrderParty{Role: comapi.PartyNotify, PartyID: "nobody"})); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("an unknown notify party: %v", err)
	}

	// ORG moves the shipper's office; the frozen order keeps the address it was recorded with.
	moved := address("HQ", orgapi.AddressOffice)
	moved.Lines = []string{"2 HQ Road"}
	if _, err = orgModule.ChangePartyAddress(ctx, orgapi.ChangePartyAddress{CommandID: "move-S-HQ", TenantID: tenant, ActorID: "org-officer",
		PartyID: "shipper-co", ExpectedRevision: revisions["shipper-co"], AddressID: "S-HQ", Address: moved, Reason: "office moved"}); err != nil {
		t.Fatal(err)
	}
	if again, err := comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, OrderID: "O-1"}); err != nil ||
		again.Parties[0].Address.Address.Lines[0] != "1 HQ Road" || again.Parties[0].Address.ORGVersion != 1 {
		t.Fatalf("frozen shipper address after the ORG change: %+v %v", again.Parties[0].Address, err)
	}

	// The consignee becomes TO ORDER on the SEA order; the order can then not go by AIR.
	if _, err = comModule.SetOrderParty(ctx, comapi.SetOrderParty{CommandID: "to-order-1", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1",
		ExpectedRevision: 1, Party: comapi.OrderParty{Role: comapi.PartyConsignee, ToOrder: "TO ORDER OF FIXTURE BANK"}, OccurredAt: at.Add(3 * time.Hour)}); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("replacing a party without a reason: %v", err)
	}
	if r, err := comModule.SetOrderParty(ctx, comapi.SetOrderParty{CommandID: "to-order-2", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1",
		ExpectedRevision: 1, Party: comapi.OrderParty{Role: comapi.PartyConsignee, ToOrder: "TO ORDER OF FIXTURE BANK"}, Reason: "letter of credit",
		OccurredAt: at.Add(3 * time.Hour)}); err != nil || r.NewRevision != 2 {
		t.Fatalf("TO ORDER consignee: %+v %v", r, err)
	}
	if _, err = comModule.ChangeOrderTransport(ctx, comapi.ChangeOrderTransport{CommandID: "air", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1",
		ExpectedRevision: 2, Transport: comapi.Transport{Mode: comapi.ModeAir, Movement: comapi.DoorToPort}, Reason: "urgent", OccurredAt: at.Add(4 * time.Hour)}); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("AIR with a TO ORDER consignee: %v", err)
	}
	if r, err := comModule.ChangeOrderTransport(ctx, comapi.ChangeOrderTransport{CommandID: "port", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1",
		ExpectedRevision: 2, Transport: comapi.Transport{Mode: comapi.ModeSea, Movement: comapi.PortToPort}, Reason: "customer delivers to the port",
		OccurredAt: at.Add(4 * time.Hour)}); err != nil || r.NewRevision != 3 {
		t.Fatalf("port to port: %+v %v", r, err)
	}

	// Writes past COM that break the rules are refused when the transaction commits.
	fixture, err := platform.newDB()
	if err != nil {
		t.Fatal(err)
	}
	err = (bento.TenantTransactor{DB: fixture, Tenant: tenant}).WithinTransaction(ctx, func(txCtx context.Context) error {
		e, err := fixture.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		_, err = e.Exec(txCtx, `INSERT INTO cargo_com.order_transport (tenant_id,order_id,version,transport_mode,movement_type,reason,changed_by,changed_at,entered_at)
            VALUES ($1,'O-1',3,'AIR','PORT_TO_PORT','by hand','sql',now(),now())`, tenant)
		return err
	})
	if sqlState(err) != "23514" {
		t.Fatalf("AIR written past COM with a TO ORDER consignee: %v", err)
	}
	for name, tc := range map[string]struct{ sql, state string }{
		"rewrite a party version":            {`UPDATE cargo_com.order_party SET legal_name='Other' WHERE tenant_id=$1`, "42501"},
		"write a name into a frozen contact": {`UPDATE cargo_com.order_party_contact SET name='Someone else' WHERE tenant_id=$1`, "23001"},
		"named party without an address": {`INSERT INTO cargo_com.order_party (tenant_id,order_id,role,version,kind,party_id,party_revision,legal_name,country_code,
            role_code,role_valid_from,role_evidence_ref,has_contact,overridden,reason,set_by,set_at,entered_at)
            VALUES ($1,'O-1','SHIPPER',2,'PARTY','shipper-co',4,'Fixture Corp','US','SHIPPER',now(),'e',false,false,'by hand','sql',now(),now())`, "23514"},
		"a contact flag without the contact": {`INSERT INTO cargo_com.order_party (tenant_id,order_id,role,version,kind,party_id,party_revision,legal_name,country_code,
            address,has_contact,overridden,reason,set_by,set_at,entered_at)
            VALUES ($1,'O-1','NOTIFY_PARTY',2,'PARTY','notify-co',1,'Fixture Corp','US','{"source":"ORG"}',true,false,'by hand','sql',now(),now())`, "23514"},
	} {
		err = (bento.TenantTransactor{DB: fixture, Tenant: tenant}).WithinTransaction(ctx, func(txCtx context.Context) error {
			e, err := fixture.RequireExecutor(txCtx)
			if err != nil {
				return err
			}
			_, err = e.Exec(txCtx, tc.sql, tenant)
			return err
		})
		if got := sqlState(err); got != tc.state {
			t.Errorf("%s: SQLSTATE %s (%v), want %s", name, got, err, tc.state)
		}
	}

	// Events name roles, versions and kinds; no names, addresses or contacts travel.
	q, err := fixture.ReadExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var partyEvents, overridden int
	if err = q.QueryRow(ctx, `SELECT count(*) FILTER (WHERE event_type='OrderPartySet.v1'), count(*) FILTER (WHERE payload->>'overridden'='true')
        FROM bento.outbox WHERE scope=$1 AND source='idp-cargo.com'`, tenant).Scan(&partyEvents, &overridden); err != nil {
		t.Fatal(err)
	}
	if partyEvents != 5 || overridden != 1 {
		t.Fatalf("%d party events (want 5), %d overridden (want 1)", partyEvents, overridden)
	}
	for _, needle := range []string{"Test Person", "example.test", "5555", "Fixture HQ", "FIXTURE BANK", "91350100"} {
		var n int
		if err = q.QueryRow(ctx, `SELECT count(*) FROM bento.outbox WHERE scope=$1 AND source='idp-cargo.com' AND payload::text LIKE '%'||$2||'%'`,
			tenant, needle).Scan(&n); err != nil || n != 0 {
			t.Fatalf("a COM event carries %q (%d, %v)", needle, n, err)
		}
	}
}
