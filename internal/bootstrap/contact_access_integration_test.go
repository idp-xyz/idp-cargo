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

// privacyAuthorizer: only the data protection officer reads the access logs and restricted order contacts;
// everything else is allowed.
type privacyAuthorizer struct{ allowAll }

func (privacyAuthorizer) AuthorizePartyAction(_ context.Context, _, actor, action, _ string) error {
	if action == orgapi.ActionReadContactAccessLog && actor != "dpo" {
		return errors.New("test policy: the access log is for the data protection officer")
	}
	return nil
}

func (privacyAuthorizer) AuthorizeOrderAction(_ context.Context, _, actor, action, _ string) error {
	if (action == comapi.ActionReadContactAccessLog || action == comapi.ActionReadRestrictedOrderContacts) && actor != "dpo" {
		return errors.New("test policy: " + action + " is for the data protection officer")
	}
	return nil
}

// ORG V0.5 and COM V0.8 with the real contexts (privacy assessment V1.2; P4, P6, P7): ORG records a contact's
// notice and consent; COM carries them onto the order with the contact; every read of contacts in ORG and COM
// states its purpose and is logged without contact details; the data protection officer reads the logs;
// restricted contacts are read only for a retained purpose; the database refuses rewrites of the logs and
// consents and keeps a read's restriction and time honest.
func TestContactConsentAndAccessLogWithRealContexts(t *testing.T) {
	ctx := context.Background()
	platform := openPlatform(t)
	orgModule, err := platform.ORG(privacyAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := platform.RATE(orgModule, allowAll{}, marginFloor(2000))
	if err != nil {
		t.Fatal(err)
	}
	comModule, err := platform.COM(orgModule, rateModule, privacyAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-privacy-")
	seedORG(t, platform, tenant, "customer", "shipper-co", "consignee-co")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	for party, role := range map[string]string{"customer": "CUSTOMER", "shipper-co": "SHIPPER", "consignee-co": "CONSIGNEE"} {
		if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-" + party, TenantID: tenant, ActorID: "org-officer",
			LegalEntityID: "LE", PartyID: party, ExpectedRevision: 1, RoleCode: role, ValidFrom: at, EvidenceRef: role + "-agreement", OccurredAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	for _, party := range []string{"shipper-co", "consignee-co"} {
		if _, err = orgModule.AddPartyAddress(ctx, orgapi.AddPartyAddress{CommandID: "addr-" + party, TenantID: tenant, ActorID: "org-officer",
			PartyID: party, AddressID: party + "-HQ", Address: orgapi.Address{ShortCode: "HQ", Purposes: []string{orgapi.AddressOffice}, Name: "Fixture HQ",
				Lines: []string{"1 HQ Road"}, City: "Shanghai", CountryCode: "CN"}, DefaultFor: []string{orgapi.AddressOffice}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = orgModule.AddPartyContact(ctx, orgapi.AddPartyContact{CommandID: "contact-shipper", TenantID: tenant, ActorID: "org-officer",
		PartyID: "shipper-co", ExpectedRevision: 1, ContactID: "S-DOCS",
		Contact:    orgapi.Contact{Purposes: []string{orgapi.ContactDocuments}, Name: "Test Person A", Email: "docs-a@example.test"},
		DefaultFor: []string{orgapi.ContactDocuments}}); err != nil {
		t.Fatal(err)
	}
	consent := orgapi.ContactConsent{NoticeVersion: "PI-RULES-2026.1", ProvideToOthers: true, CrossBorder: true,
		Basis: orgapi.ConsentCustomerUndertaking, EvidenceRef: "Service contract SC-2026-001 clause 12"}
	invalid := consent
	invalid.Basis = "IMPLIED"
	if _, err = orgModule.RecordContactConsent(ctx, orgapi.RecordContactConsent{CommandID: "consent-bad", TenantID: tenant, ActorID: "org-officer",
		PartyID: "shipper-co", ExpectedRevision: 2, ContactID: "S-DOCS", Consent: invalid}); !errors.Is(err, orgapi.ErrRejected) {
		t.Fatalf("a consent of an unknown basis: %v", err)
	}
	if r, err := orgModule.RecordContactConsent(ctx, orgapi.RecordContactConsent{CommandID: "consent", TenantID: tenant, ActorID: "org-officer",
		PartyID: "shipper-co", ExpectedRevision: 2, ContactID: "S-DOCS", Consent: consent}); err != nil || r.NewRevision != 3 || r.EntryVersion != 1 {
		t.Fatalf("record the consent: %+v %v", r, err)
	}
	if _, err = orgModule.GetPartyContacts(ctx, orgapi.GetPartyContacts{TenantID: tenant, ActorID: "org-officer", PartyID: "shipper-co"}); !errors.Is(err, orgapi.ErrRejected) {
		t.Fatalf("contacts without a purpose: %v", err)
	}
	contacts, err := orgModule.GetPartyContacts(ctx, orgapi.GetPartyContacts{TenantID: tenant, ActorID: "org-officer", PartyID: "shipper-co", Purpose: kernel.PurposeOperations})
	if err != nil || len(contacts.Contacts) != 1 || contacts.Contacts[0].Consent == nil || contacts.Contacts[0].Consent.Revision != 3 || !contacts.Contacts[0].Consent.CrossBorder {
		t.Fatalf("contacts with their consent: %+v %v", contacts, err)
	}

	if _, err = rateModule.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "quote", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1", LegalEntityID: "LE",
		CustomerPartyID: "customer", Currency: "USD", Lines: []rateapi.QuoteLine{{ChargeCode: "AIR_FREIGHT", Sell: usd(100_000), Cost: usd(50_000)}},
		Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err = rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1",
		ExpectedRevision: 1, EvidenceKind: rateapi.EvidenceEmail, EvidenceRef: "mail", OccurredAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	kg := kernel.Quantity{Unit: "KGM", Amount: 500}
	if _, err = comModule.RecordCustomerOrder(ctx, comapi.RecordCustomerOrder{CommandID: "record", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1",
		QuoteID: "Q-1", CustomerPartyID: "customer", OccurredAt: at.Add(2 * time.Hour), Transport: comapi.Transport{Mode: comapi.ModeSea, Movement: comapi.PortToPort},
		Lines:   []comapi.OrderLine{{Description: "Pallets of machine parts", Packs: 10, PackageType: "PX", GrossWeight: kg}},
		Parties: []comapi.OrderParty{{Role: comapi.PartyShipper, PartyID: "shipper-co"}, {Role: comapi.PartyConsignee, PartyID: "consignee-co"}}}); err != nil {
		t.Fatal(err)
	}
	site := comapi.ContactConsent{NoticeVersion: "PI-RULES-2026.1", ProvideToOthers: true, Basis: comapi.ConsentSubjectConfirmed, EvidenceRef: "signed notice N-7"}
	if r, err := comModule.SetOrderParty(ctx, comapi.SetOrderParty{CommandID: "consignee-contact", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1",
		ExpectedRevision: 1, Reason: "site contact for the delivery", OccurredAt: at.Add(3 * time.Hour),
		Party: comapi.OrderParty{Role: comapi.PartyConsignee, PartyID: "consignee-co", Contact: comapi.ContactChoice{Use: comapi.ContactOverride,
			Override: &comapi.Contact{Name: "Test Person B", Phone: "+49 40 5555 0002"}, Consent: &site, Reason: "the site manager receives the goods"}}}); err != nil || r.NewRevision != 2 {
		t.Fatalf("a contact given on the order with its consent: %+v %v", r, err)
	}
	if _, err = comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, ActorID: "doc-officer", OrderID: "O-1", WithContacts: true}); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("order contacts without a purpose: %v", err)
	}
	parties, err := comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, ActorID: "doc-officer", OrderID: "O-1", WithContacts: true,
		Purpose: kernel.PurposeDocuments})
	if err != nil || len(parties.Parties) != 2 {
		t.Fatalf("order contacts: %+v %v", parties, err)
	}
	for _, p := range parties.Parties {
		c := p.Contact
		switch p.Role {
		case comapi.PartyShipper:
			if c == nil || c.Consent == nil || c.Consent.ORGRevision != 3 || !c.Consent.CrossBorder || c.Consent.Basis != orgapi.ConsentCustomerUndertaking {
				t.Fatalf("the shipper's contact carries ORG's consent: %+v", c)
			}
		case comapi.PartyConsignee:
			if c == nil || c.Consent == nil || c.Consent.ORGRevision != 0 || c.Consent.CrossBorder || c.Consent.EvidenceRef != "signed notice N-7" {
				t.Fatalf("the consignee's contact given on the order keeps its consent: %+v", c)
			}
		}
	}

	// Close the order: its contacts are restricted and read only for a retained purpose.
	if _, err = comModule.AllocateOrderToObligation(ctx, comapi.AllocateOrderToObligation{CommandID: "allocate", TenantID: tenant, ActorID: "planner-1",
		OrderID: "O-1", ExpectedRevision: 2, LineNo: 1, ObligationID: "OB-1", Packs: 10, GrossWeight: kg, OccurredAt: at.Add(4 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = comModule.CloseCustomerOrder(ctx, comapi.CloseCustomerOrder{CommandID: "close", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1",
		ExpectedRevision: 3, OccurredAt: at.Add(5 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, ActorID: "dpo", OrderID: "O-1", WithContacts: true,
		Purpose: kernel.PurposeDocuments}); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("restricted contacts for documents: %v", err)
	}
	if _, err = comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, ActorID: "dpo", OrderID: "O-1", WithContacts: true,
		Purpose: kernel.PurposeLegalClaim}); err != nil {
		t.Fatalf("restricted contacts for a legal claim: %v", err)
	}

	// The logs: who read which contacts, for what purpose; never the details.
	if _, err = orgModule.ListContactAccess(ctx, orgapi.ListContactAccess{TenantID: tenant, ActorID: "org-officer", Limit: 10}); !errors.Is(err, orgapi.ErrUnauthorized) {
		t.Fatalf("the ORG log for another actor: %v", err)
	}
	orgLog, err := orgModule.ListContactAccess(ctx, orgapi.ListContactAccess{TenantID: tenant, ActorID: "dpo", PartyID: "shipper-co", Limit: 10})
	if err != nil || len(orgLog) != 2 {
		t.Fatalf("ORG log: %+v %v", orgLog, err)
	}
	if newest := orgLog[0]; newest.ActorID != "cs-1" || newest.Purpose != kernel.PurposeOrderParty || len(newest.ContactIDs) != 1 || newest.ContactIDs[0] != "S-DOCS" {
		t.Fatalf("COM carried the contact for cs-1: %+v", newest)
	}
	if oldest := orgLog[1]; oldest.ActorID != "org-officer" || oldest.Purpose != kernel.PurposeOperations || oldest.Action != orgapi.ActionReadPartyContacts {
		t.Fatalf("the officer's read: %+v", oldest)
	}
	if byOfficer, err := orgModule.ListContactAccess(ctx, orgapi.ListContactAccess{TenantID: tenant, ActorID: "dpo", ReadBy: "org-officer", Limit: 10}); err != nil || len(byOfficer) != 1 {
		t.Fatalf("ORG log by actor: %+v %v", byOfficer, err)
	}
	if _, err = comModule.ListContactAccess(ctx, comapi.ListContactAccess{TenantID: tenant, ActorID: "doc-officer", Limit: 10}); !errors.Is(err, comapi.ErrUnauthorized) {
		t.Fatalf("the COM log for another actor: %v", err)
	}
	comLog, err := comModule.ListContactAccess(ctx, comapi.ListContactAccess{TenantID: tenant, ActorID: "dpo", OrderID: "O-1", Limit: 10})
	if err != nil || len(comLog) != 2 {
		t.Fatalf("COM log: %+v %v", comLog, err)
	}
	if r := comLog[0]; !r.Restricted || r.Action != comapi.ActionReadRestrictedOrderContacts || r.Purpose != kernel.PurposeLegalClaim || r.Contacts != 2 || r.ActorID != "dpo" {
		t.Fatalf("the restricted read: %+v", r)
	}
	if r := comLog[1]; r.Restricted || r.Purpose != kernel.PurposeDocuments || r.Contacts != 2 || r.ActorID != "doc-officer" {
		t.Fatalf("the documents read: %+v", r)
	}

	// Writes past the contexts.
	fixture, err := platform.newDB()
	if err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) error {
		return (bento.TenantTransactor{DB: fixture, Tenant: tenant}).WithinTransaction(ctx, func(txCtx context.Context) error {
			e, err := fixture.RequireExecutor(txCtx)
			if err != nil {
				return err
			}
			_, err = e.Exec(txCtx, sql, append([]any{tenant}, args...)...)
			return err
		})
	}
	for name, tc := range map[string]struct{ sql, state string }{
		"rewrite an ORG read":  {`UPDATE cargo_org.party_contact_access SET purpose='ACCOUNTS' WHERE tenant_id=$1`, "42501"},
		"delete an ORG read":   {`DELETE FROM cargo_org.party_contact_access WHERE tenant_id=$1`, "42501"},
		"rewrite a consent":    {`UPDATE cargo_org.party_contact_consent SET cross_border=false WHERE tenant_id=$1`, "42501"},
		"a read for marketing": {`INSERT INTO cargo_org.party_contact_access (tenant_id,party_id,actor_id,action,purpose,contact_ids) VALUES ($1,'shipper-co','sql','ReadPartyContacts','MARKETING','{}')`, "23514"},
		"a consent without its change record": {`INSERT INTO cargo_org.party_contact_consent (tenant_id,party_id,contact_id,revision,notice_version,provide_to_others,
            cross_border,basis,evidence_ref) VALUES ($1,'shipper-co','S-DOCS',3,'v2',false,false,'SUBJECT_CONFIRMED','by hand')`, "23505"},
		"a consent at a revision with no record": {`INSERT INTO cargo_org.party_contact_consent (tenant_id,party_id,contact_id,revision,notice_version,provide_to_others,
            cross_border,basis,evidence_ref) VALUES ($1,'shipper-co','S-DOCS',99,'v2',false,false,'SUBJECT_CONFIRMED','by hand')`, "23514"},
		"rewrite a COM read": {`UPDATE cargo_com.order_contact_access SET purpose='ACCOUNTS' WHERE tenant_id=$1`, "42501"},
		"an unrestricted read of the closed order": {`INSERT INTO cargo_com.order_contact_access (tenant_id,order_id,actor_id,action,restricted,purpose,contacts)
            VALUES ($1,'O-1','sql','ReadOrderContacts',false,'DOCUMENTS',2)`, "23514"},
		"a restricted read for documents": {`INSERT INTO cargo_com.order_contact_access (tenant_id,order_id,actor_id,action,restricted,purpose,contacts)
            VALUES ($1,'O-1','sql','ReadRestrictedOrderContacts',true,'DOCUMENTS',2)`, "23514"},
		"rewrite a frozen consent": {`UPDATE cargo_com.order_party_contact SET consent_cross_border=true WHERE tenant_id=$1`, "42501"},
	} {
		if got := sqlState(exec(tc.sql)); got != tc.state {
			t.Errorf("%s: SQLSTATE %s, want %s", name, got, tc.state)
		}
	}
	if err = exec(`INSERT INTO cargo_org.party_contact_access (tenant_id,party_id,actor_id,action,purpose,contact_ids,recorded_at)
        VALUES ($1,'shipper-co','sql','ReadPartyContacts','DOCUMENTS','{}',now()-interval '5 years')`); err != nil {
		t.Fatal(err)
	}
	if backdated, err := orgModule.ListContactAccess(ctx, orgapi.ListContactAccess{TenantID: tenant, ActorID: "dpo", ReadBy: "sql", Limit: 10}); err != nil ||
		len(backdated) != 1 || time.Since(backdated[0].RecordedAt) > time.Hour {
		t.Fatalf("a read is timed by the database, not by its writer: %+v %v", backdated, err)
	}

	// No contact details or consent evidence travel in events.
	q, err := fixture.ReadExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var consentEvents, leaks int
	if err = q.QueryRow(ctx, `SELECT count(*) FILTER (WHERE event_type='PartyContactConsentRecorded.v1'),
        count(*) FILTER (WHERE payload::text LIKE '%Test Person%' OR payload::text LIKE '%example.test%' OR payload::text LIKE '%SC-2026%' OR payload::text LIKE '%N-7%')
        FROM bento.outbox WHERE scope=$1`, tenant).Scan(&consentEvents, &leaks); err != nil || consentEvents != 1 || leaks != 0 {
		t.Fatalf("%d consent events, %d events with contact data or evidence: %v", consentEvents, leaks, err)
	}
}
