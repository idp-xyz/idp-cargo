//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/com"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// movableClock is the system clock until a test moves it.
type movableClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *movableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() {
		return time.Now().UTC()
	}
	return c.at
}

func (c *movableClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = t
}

// retentionAuthorizer: the documentation officer reads the contacts of open orders, the data protection
// officer those of closed and cancelled ones, and only the privacy job erases them.
type retentionAuthorizer struct{}

func (retentionAuthorizer) AuthorizeOrderAction(_ context.Context, _, actor, action, _ string) error {
	allowed := map[string]string{comapi.ActionReadOrderContacts: "doc-officer", comapi.ActionReadRestrictedOrderContacts: "dpo",
		comapi.ActionEraseOrderContacts: "privacy-job"}
	if who, guarded := allowed[action]; guarded && actor != who {
		return errors.New("test policy: " + action + " is for " + who)
	}
	return nil
}

// COM V0.7 with the real contexts (P1): an allocated order is closed and no longer changes; its contacts
// are then restricted, kept 36 months, listed when due and erased every version at once, while the order,
// its revision and ORG's own contact stay. The database refuses what COM refuses.
func TestOrderContactsRetainedAndErasedWithRealContexts(t *testing.T) {
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
	clock := &movableClock{}
	comModule, err := platform.COM(orgModule, rateModule, retentionAuthorizer{}, com.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = platform.COM(orgModule, rateModule, retentionAuthorizer{}, com.WithContactRetention(24)); err == nil {
		t.Fatal("a retention of 24 months")
	}
	tenant := uniqueTenant("cargo-com07-")
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
	if _, err = rateModule.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "quote", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1", LegalEntityID: "LE",
		CustomerPartyID: "customer", Currency: "USD", Lines: []rateapi.QuoteLine{{ChargeCode: "FREIGHT", Sell: usd(100_000), Cost: usd(50_000)}},
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
	kg := kernel.Quantity{Unit: "KGM", Amount: 500}
	record := func(id string) {
		t.Helper()
		if _, err := comModule.RecordCustomerOrder(ctx, comapi.RecordCustomerOrder{CommandID: "record-" + id, TenantID: tenant, ActorID: "cs-1", OrderID: id,
			QuoteID: "Q-1", CustomerPartyID: "customer", OccurredAt: at.Add(2 * time.Hour), Transport: comapi.Transport{Mode: comapi.ModeSea, Movement: comapi.PortToPort},
			Lines:   []comapi.OrderLine{{Description: "Pallets of machine parts", Packs: 10, PackageType: "PX", GrossWeight: kg}},
			Parties: []comapi.OrderParty{{Role: comapi.PartyShipper, PartyID: "shipper-co"}, {Role: comapi.PartyConsignee, PartyID: "consignee-co"}}}); err != nil {
			t.Fatalf("record %s: %v", id, err)
		}
	}
	allocate := func(id string) {
		t.Helper()
		if _, err := comModule.AllocateOrderToObligation(ctx, comapi.AllocateOrderToObligation{CommandID: "allocate-" + id, TenantID: tenant, ActorID: "planner-1",
			OrderID: id, ExpectedRevision: 1, LineNo: 1, ObligationID: "OB-" + id, Packs: 10, GrossWeight: kg, OccurredAt: at.Add(3 * time.Hour)}); err != nil {
			t.Fatalf("allocate %s: %v", id, err)
		}
	}
	read := func(actor string, contacts bool) (comapi.OrderParties, error) {
		purpose := kernel.PurposeDocuments
		if actor == "dpo" {
			purpose = kernel.PurposeCustomsAudit
		}
		return comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, ActorID: actor, OrderID: "O-1", WithContacts: contacts, Purpose: purpose})
	}
	closeAt := at.Add(4 * time.Hour)
	closing := comapi.CloseCustomerOrder{CommandID: "close-1", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1", ExpectedRevision: 1, OccurredAt: closeAt}

	record("O-1")
	if _, err = comModule.CloseCustomerOrder(ctx, closing); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("closing an order with its line unallocated: %v", err)
	}
	allocate("O-1")
	if open, err := read("doc-officer", true); err != nil || open.Contacts.State != comapi.ContactsInUse || open.Parties[0].Contact == nil {
		t.Fatalf("contacts of the open order: %+v %v", open, err)
	}
	closing.CommandID, closing.ExpectedRevision = "close-2", 2
	if r, err := comModule.CloseCustomerOrder(ctx, closing); err != nil || r.NewRevision != 3 {
		t.Fatalf("close: %+v %v", r, err)
	}
	if _, err = comModule.SetOrderParty(ctx, comapi.SetOrderParty{CommandID: "late-party", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1", ExpectedRevision: 3,
		Party: comapi.OrderParty{Role: comapi.PartyConsignee, ToOrder: "TO ORDER"}, Reason: "late", OccurredAt: closeAt}); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("a party for the closed order: %v", err)
	}
	if _, err = comModule.CancelCustomerOrder(ctx, comapi.CancelCustomerOrder{CommandID: "late-cancel", TenantID: tenant, ActorID: "cs-1", OrderID: "O-1",
		ExpectedRevision: 3, OccurredAt: closeAt}); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("cancelling the closed order: %v", err)
	}

	// Restricted: the documentation officer no longer sees the contacts; the data protection officer does.
	if _, err = read("doc-officer", true); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("contacts of the closed order for documents: %v", err)
	}
	if _, err = comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, ActorID: "doc-officer", OrderID: "O-1", WithContacts: true,
		Purpose: kernel.PurposeCustomsAudit}); !errors.Is(err, comapi.ErrUnauthorized) {
		t.Fatalf("contacts of the closed order for the documentation officer: %v", err)
	}
	until := closeAt.AddDate(3, 0, 0)
	if closeAt.Month() == time.February && closeAt.Day() == 29 {
		until = until.AddDate(0, 0, -1)
	}
	restricted, err := read("dpo", true)
	if err != nil || restricted.Contacts.State != comapi.ContactsRestricted || !restricted.Contacts.RetainedUntil.Equal(until) ||
		restricted.Parties[0].Contact == nil || restricted.Parties[0].Contact.Contact.Name != "Test Person A" || restricted.Revision != 3 {
		t.Fatalf("restricted contacts for the data protection officer: %+v %v", restricted, err)
	}
	if plain, err := read("cs-1", false); err != nil || len(plain.Parties) != 2 || plain.Parties[0].Contact != nil {
		t.Fatalf("parties without contacts: %+v %v", plain, err)
	}

	// Not due yet; the privacy job alone lists and erases.
	if _, err = comModule.ListOrdersDueForContactErasure(ctx, comapi.ListOrdersDueForContactErasure{TenantID: tenant, ActorID: "cs-1", Limit: 10}); !errors.Is(err, comapi.ErrUnauthorized) {
		t.Fatalf("listing for another actor: %v", err)
	}
	if due, err := comModule.ListOrdersDueForContactErasure(ctx, comapi.ListOrdersDueForContactErasure{TenantID: tenant, ActorID: "privacy-job", Limit: 10}); err != nil || len(due.Orders) != 0 {
		t.Fatalf("due now: %+v %v", due, err)
	}
	erase := comapi.EraseOrderContacts{CommandID: "erase-early", TenantID: tenant, ActorID: "privacy-job", OrderID: "O-1", OccurredAt: time.Now().UTC()}
	if _, err = comModule.EraseOrderContacts(ctx, erase); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("erasing within the retention: %v", err)
	}

	// Three years on.
	clock.set(until)
	due, err := comModule.ListOrdersDueForContactErasure(ctx, comapi.ListOrdersDueForContactErasure{TenantID: tenant, ActorID: "privacy-job", Limit: 10})
	if err != nil || len(due.Orders) != 1 || due.Orders[0].OrderID != "O-1" || !due.AsOf.Equal(until) {
		t.Fatalf("due at the end of the retention: %+v %v", due, err)
	}
	erase.CommandID, erase.OccurredAt = "erase", until
	if r, err := comModule.EraseOrderContacts(ctx, erase); err != nil || r.NewRevision != 3 {
		t.Fatalf("erase: %+v %v", r, err)
	}
	erased, err := read("dpo", true)
	if err != nil || erased.Contacts.State != comapi.ContactsErased || !erased.Contacts.ErasedAt.Equal(until) || erased.Revision != 3 ||
		!erased.Parties[0].HasContact || erased.Parties[0].Contact != nil || erased.Parties[0].LegalName == "" {
		t.Fatalf("after the erasure: %+v %v", erased, err)
	}
	if due, err = comModule.ListOrdersDueForContactErasure(ctx, comapi.ListOrdersDueForContactErasure{TenantID: tenant, ActorID: "privacy-job", Limit: 10}); err != nil || len(due.Orders) != 0 {
		t.Fatalf("due after the erasure: %+v %v", due, err)
	}
	if contacts, err := orgModule.GetPartyContacts(ctx, orgapi.GetPartyContacts{TenantID: tenant, ActorID: "org-officer", PartyID: "shipper-co",
		Purpose: kernel.PurposeOperations}); err != nil ||
		len(contacts.Contacts) != 1 || contacts.Contacts[0].Contact.Name != "Test Person A" {
		t.Fatalf("ORG keeps its own contact: %+v %v", contacts, err)
	}
	clock.set(time.Time{})

	// Writes past COM are refused: O-2 is cancelled with its contacts retained, O-3 only recorded, O-4 allocated.
	record("O-2")
	if _, err = comModule.CancelCustomerOrder(ctx, comapi.CancelCustomerOrder{CommandID: "cancel-2", TenantID: tenant, ActorID: "cs-1", OrderID: "O-2",
		ExpectedRevision: 1, OccurredAt: closeAt}); err != nil {
		t.Fatal(err)
	}
	record("O-3")
	record("O-4")
	allocate("O-4")
	fixture, err := platform.newDB()
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ sql, state string }{
		"reopen the closed order": {`UPDATE cargo_com.customer_order SET status='ALLOCATED',closed_by=NULL,closed_at=NULL,revision=revision+1
            WHERE tenant_id=$1 AND order_id='O-1'`, "23001"},
		"a reservation on the closed order": {`INSERT INTO cargo_com.obligation_reservation (tenant_id,order_id,reservation_no,obligation_id,obligation_version,
            shipment_id,reserved_by,reserved_at) VALUES ($1,'O-1',1,'OB-O-1',1,'S-SQL','sql',now())`, "23001"},
		"write a name back into an erased contact": {`UPDATE cargo_com.order_party_contact SET name='Test Person A' WHERE tenant_id=$1 AND order_id='O-1'`, "23001"},
		"a second erasure": {`INSERT INTO cargo_com.order_contact_erasure (tenant_id,order_id,erased_by,erased_at,entered_at)
            VALUES ($1,'O-1','sql',now(),now()+interval '10 years')`, "23505"},
		"shorten a retention":        {`UPDATE cargo_com.order_contact_retention SET retained_until=now() WHERE tenant_id=$1`, "42501"},
		"erase within the retention": {`INSERT INTO cargo_com.order_contact_erasure (tenant_id,order_id,erased_by,erased_at,entered_at) VALUES ($1,'O-2','sql',now(),now())`, "23514"},
		"clear contacts without an erasure": {`UPDATE cargo_com.order_party_contact SET name=NULL,phone=NULL,mobile=NULL,email=NULL,erased=true
            WHERE tenant_id=$1 AND order_id='O-2'`, "23001"},
		"a party for the cancelled order": {`INSERT INTO cargo_com.order_transport (tenant_id,order_id,version,transport_mode,reason,changed_by,changed_at,entered_at)
            VALUES ($1,'O-2',2,'AIR','by hand','sql',now(),now())`, "23001"},
		"close an order with its line unallocated": {`UPDATE cargo_com.customer_order SET status='CLOSED',closed_by='sql',closed_at=now(),revision=revision+1
            WHERE tenant_id=$1 AND order_id='O-3'`, "23001"},
		"close an order without a retention": {`UPDATE cargo_com.customer_order SET status='CLOSED',closed_by='sql',closed_at=now(),revision=revision+1
            WHERE tenant_id=$1 AND order_id='O-4'`, "23514"},
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

	// The closure and the erasure travel as events that name the order only.
	q, err := fixture.ReadExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var closed, erasedEvents int
	if err = q.QueryRow(ctx, `SELECT count(*) FILTER (WHERE event_type='CustomerOrderClosed.v1'), count(*) FILTER (WHERE event_type='CustomerOrderContactsErased.v1')
        FROM bento.outbox WHERE scope=$1 AND source='idp-cargo.com'`, tenant).Scan(&closed, &erasedEvents); err != nil || closed != 1 || erasedEvents != 1 {
		t.Fatalf("%d closure and %d erasure events: %v", closed, erasedEvents, err)
	}
	var leaks int
	if err = q.QueryRow(ctx, `SELECT count(*) FROM bento.outbox WHERE scope=$1 AND source='idp-cargo.com' AND (payload::text LIKE '%Test Person%' OR payload::text LIKE '%example.test%')`,
		tenant).Scan(&leaks); err != nil || leaks != 0 {
		t.Fatalf("%d COM events carry contact data: %v", leaks, err)
	}
}
