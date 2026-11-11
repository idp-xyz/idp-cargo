package doc

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var at = time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)

func TestEventsMatchPublishedContract(t *testing.T) {
	e := domain.Event{Type: api.TransportDocumentIssued, At: at, TenantID: "T", DocumentID: "DOC-H1", Kind: "HAWB", Version: 2, ShipmentID: "SH-A",
		ConsolidationID: "C-1", MovementID: "AM-1"}
	envelope, err := postgres.DOCEnvelope("cmd-1", e, at)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tenant_id":"T","subject_id":"DOC-H1","kind":"HAWB","version":2,"shipment_id":"SH-A","consolidation_id":"C-1","movement_id":"AM-1"}`
	if envelope.Source != api.Source || string(envelope.Payload) != want || envelope.PartitionKey != "T:DOC-H1" {
		t.Fatalf("envelope: %s %s", envelope.Payload, envelope.PartitionKey)
	}
	var payload api.EventPayload
	if err = json.Unmarshal(envelope.Payload, &payload); err != nil || payload.Version != 2 {
		t.Fatalf("payload %+v %v", payload, err)
	}
	again, _ := postgres.DOCEnvelope("cmd-1", e, at.Add(time.Hour))
	other, _ := postgres.DOCEnvelope("cmd-2", e, at)
	if envelope.ID != again.ID || envelope.ID == other.ID {
		t.Fatal("event ids")
	}
}

// TestTheModelEmitsThePublishedEventTypes runs every command of the model and checks that its events are exactly
// the types package api publishes.
func TestTheModelEmitsThePublishedEventTypes(t *testing.T) {
	airport := func(code, iata string) domain.Airport { return domain.Airport{Code: code, IATA: iata} }
	pvg, jfk := airport("CNPVG", "PVG"), airport("USJFK", "JFK")
	facts := func(v int) domain.Facts {
		return domain.Facts{MovementID: "AM-1", Version: v, SHA256: strings.Repeat("cd", 32), Final: true, ConsolidationID: "C-1", LegalEntityID: "LE",
			MAWB: "781-12345675", Origin: pvg, Destination: jfk, Flights: []domain.Flight{{Flight: "MU587", From: pvg, To: jfk, ETD: at}},
			Houses: []domain.FactsHouse{{ShipmentID: "SH-A", HAWB: "SHA0000001", Pieces: 6, GrossGrams: 318_000, Parts: []domain.FactsPart{{OrderID: "O-A",
				Packs: 6, PackageType: "CT", GoodsDescription: "Cartons"}}}}}
	}
	party := func(id, country string) *domain.Party {
		return &domain.Party{Named: true, PartyID: id, PartyRevision: 1, LegalName: id, CountryCode: country, Version: 1,
			Identifiers: []domain.Identifier{{Type: "CN_USCC", Country: "CN", Value: "91310000MA1FL4XQ3X"}}, Address: &domain.Address{Source: "ORG", CountryCode: country}}
	}
	orders := []domain.OrderParties{{OrderID: "O-A", Revision: 3, Shipper: party("S", "CN"), Consignee: party("C", "US"), Locks: []string{"DOC-H1"}}}
	issuer := domain.Issuer{LegalEntityID: "LE", Revision: 1, Name: "LE Co.", Jurisdiction: "CN", Verified: true}
	content := func(v int) domain.Content {
		c, err := domain.HouseContent("DOC-H1", "SH-A", facts(v), issuer, orders, domain.Entered{PaymentIndicator: domain.PaymentPrepaid})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	d, err := domain.DraftDocument("T", "DOC-H1", content(1), "doc-1", at)
	if err != nil {
		t.Fatal(err)
	}
	gate := func(v int) domain.IssueGate {
		return domain.IssueGate{Facts: facts(v), Orders: orders, Credit: domain.CreditDecision{Status: "NONE"}, OverrideReason: "FIN not in service",
			IssuerVerified: true, AuthorityID: "A", AuthorityVersion: 1}
	}
	if err = errors.Join(d.Refresh(content(2), "newer facts", "doc-1", at), d.Approve("doc-2", at), d.Issue(gate(2), "doc-2", at),
		d.OpenAmendment(content(3), "weight", "doc-1", at), d.Approve("doc-2", at), d.IssueAmendment(gate(3), "doc-2", at), d.OpenAmendment(content(3), "again", "doc-1", at),
		d.AbandonAmendment(orders, "withdrawn", "doc-1", at), d.Cancel("mistake", "doc-1", at)); err != nil {
		t.Fatal(err)
	}
	a, err := domain.RecordIssuanceAuthority(domain.IssuanceAuthority{TenantID: "T", AuthorityID: "A", LegalEntityID: "LE", Kind: domain.KindHAWB,
		IssuePlace: "CNPVG", Qualification: "forwarder filing", Validity: kernel.ValidityInterval{From: at}, EvidenceRef: "filing.pdf"}, "legal-1", at)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Withdraw("closed", "legal-1", at); err != nil {
		t.Fatal(err)
	}
	published := map[string]bool{api.TransportDocumentDrafted: true, api.TransportDocumentApproved: true, api.TransportDocumentIssued: true,
		api.TransportDocumentAmendmentOpened: true, api.TransportDocumentAmended: true, api.TransportDocumentAmendmentAbandoned: true,
		api.TransportDocumentCancelled: true, api.IssuanceAuthorityRecorded: true, api.IssuanceAuthorityWithdrawn: true}
	emitted := map[string]bool{}
	for _, e := range append(d.Pending(), a.Pending()...) {
		if !published[e.Type] {
			t.Errorf("event type %s is not published by package api", e.Type)
		}
		emitted[e.Type] = true
	}
	if len(emitted) != len(published) {
		t.Fatalf("emitted %v, published %v", emitted, published)
	}
	// the content type package api publishes reads every field the model writes
	v := d.Latest()
	var published1 api.DocumentContent
	if err = json.Unmarshal(v.Content, &published1); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(published1)
	if string(again) != string(v.Content) {
		t.Fatalf("api.DocumentContent loses fields:\n%s\n%s", v.Content, again)
	}
}

func TestClassifyMapsCausesToAPIKinds(t *testing.T) {
	for _, tc := range []struct{ cause, want error }{
		{application.ErrUnauthenticated, api.ErrUnauthenticated},
		{fmt.Errorf("%w: policy", application.ErrUnauthorized), api.ErrUnauthorized},
		{bento.ErrTenantScopeConflict, api.ErrUnauthorized},
		{application.ErrIdempotencyConflict, api.ErrIdempotencyConflict},
		{domain.ErrConflict, api.ErrConflict},
		{bentorepo.ErrConflict, api.ErrConflict},
		{bentorepo.ErrNotFound, api.ErrNotFound},
		{domain.ErrSourceStale, api.ErrRejected},
		{domain.ErrNotLocked, api.ErrRejected},
		{domain.ErrCreditUndecided, api.ErrRejected},
		{domain.ErrIssuanceBlocked, api.ErrRejected},
		{domain.ErrPartiesDiverge, api.ErrRejected},
		{domain.ErrNumberIssued, api.ErrRejected},
		{application.ErrUnknownSource, api.ErrRejected},
		{kernel.ErrOutsideRecordWindow, api.ErrRejected},
	} {
		got := classify(tc.cause)
		if !errors.Is(got, tc.want) || !errors.Is(got, tc.cause) {
			t.Errorf("classify(%v) = %v, want %v", tc.cause, got, tc.want)
		}
	}
	if other := errors.New("connection reset"); classify(other) != other {
		t.Error("unknown causes must pass through unchanged")
	}
}
