//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.90 through DOC's real commands and queries and PostgreSQL, with SEA, COM and the rest as the stand-in world of
// doc_postgres_test.go: a house bill dated by its own containers, the abandon of a moved amendment, the pending items.

// loadingFacts is SEA's facts of OM-1 with two containers, CV-1 loaded at hour 4 and CV-2 at second (not loaded when
// zero), SH-A in CV-1 and SH-L in CV-2. Each version has its own digest.
func (f *fixture) loadingFacts(version int, status string, second int) seaapi.OceanDocFacts {
	box := func(visit, number string, at int) seaapi.DocContainer {
		c := seaapi.DocContainer{VisitID: visit, Number: number, SizeType: "22G1", SealNumber: "SEAL-" + visit}
		if at > 0 {
			c.OnBoardAt = f.hours(at)
		}
		return c
	}
	house := func(shipment, hbl, order, visit, number string) seaapi.DocHouse {
		return seaapi.DocHouse{ShipmentID: shipment, HBL: hbl, Packs: 6, PackageType: "CT", GrossWeightGrams: 312000, VolumeCubicCentimetres: 1_000_000,
			OrderIDs: []string{order}, GoodsDescription: "Cartons",
			Containers: []seaapi.DocBreakdown{{VisitID: visit, ContainerNumber: number, Packs: 6, GrossWeightGrams: 312000, VolumeCubicCentimetres: 1_000_000}}}
	}
	out := seaapi.OceanDocFacts{MovementID: "OM-1", Version: version, Status: status, SHA256: strings.Repeat(string(rune('a'+version)), 64), ConsolidationID: "C-1",
		OperatingLegalEntityID: "LE", CarrierPartyID: "line-1", MasterBillNumber: "LINU2611000457", MasterBillKind: "CARRIER_MBL", Vessel: "EVER GIVEN", Voyage: "0123W",
		PlaceOfReceipt: "CNSHA", PortOfLoading: "CNSHG", PortOfDischarge: "DEHAM", LocationEdition: "E1", ContainerDates: true, OnBoardAt: f.hours(4),
		Containers: []seaapi.DocContainer{box("CV-1", "CSQU3054383", 4), box("CV-2", "TEMU1234565", second)},
		Houses:     []seaapi.DocHouse{house("SH-A", "SHAL0000002", "O-A", "CV-1", "CSQU3054383"), house("SH-L", "SHAL0000001", "O-L", "CV-2", "TEMU1234565")}}
	return out
}

func (f *fixture) setOcean(movement string, facts seaapi.OceanDocFacts) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.ocean[movement] = facts
}

// orderFor makes the order known to COM's stand-in, locked for the document.
func (f *fixture) orderFor(order, document string) {
	o := comapi.OrderParties{OrderID: order, Revision: 4, CustomerPartyID: "customer-" + order, Transport: comapi.Transport{Mode: comapi.ModeSea},
		Parties: []comapi.OrderPartyView{view(comapi.PartyShipper, "shipper-"+order, "CN", 1), view(comapi.PartyConsignee, "consignee-"+order, "DE", 1)}}
	f.w.mu.Lock()
	f.w.orders[order] = locked(o, document)
	f.w.mu.Unlock()
}

func (f *fixture) draftHBL(t *testing.T, id, shipment string, hour int) api.Receipt {
	t.Helper()
	r, err := f.module.DraftTransportDocument(context.Background(), api.DraftTransportDocument{CommandID: "draft-" + id, TenantID: f.tenant, ActorID: "doc-1", DocumentID: id,
		Kind: api.KindHBL, MovementID: "OM-1", ShipmentID: shipment, PaymentIndicator: api.PaymentPrepaid, OccurredAt: f.hours(hour)})
	if err != nil {
		t.Fatalf("draft %s: %v", id, err)
	}
	return r
}

func (f *fixture) approveDoc(t *testing.T, id string, revision int64, hour int) api.Receipt {
	t.Helper()
	r, err := f.module.ApproveTransportDocument(context.Background(), api.ApproveTransportDocument{CommandID: "ap-" + id + "-" + string(rune('a'+hour)), TenantID: f.tenant,
		ActorID: "doc-2", DocumentID: id, ExpectedRevision: revision, OccurredAt: f.hours(hour)})
	if err != nil {
		t.Fatalf("approve %s: %v", id, err)
	}
	return r
}

func (f *fixture) issueDoc(id string, revision int64, hour int) (api.Receipt, error) {
	return f.module.IssueTransportDocument(context.Background(), api.IssueTransportDocument{CommandID: "is-" + id + "-" + string(rune('a'+hour)), TenantID: f.tenant,
		ActorID: "doc-1", DocumentID: id, ExpectedRevision: revision, CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(hour)})
}

func (f *fixture) refreshDoc(t *testing.T, id string, revision int64, hour int) api.Receipt {
	t.Helper()
	r, err := f.module.RefreshDocumentDraft(context.Background(), api.RefreshDocumentDraft{CommandID: "re-" + id + "-" + string(rune('a'+hour)), TenantID: f.tenant,
		ActorID: "doc-1", DocumentID: id, ExpectedRevision: revision, Reason: "SEA's facts", OccurredAt: f.hours(hour)})
	if err != nil {
		t.Fatalf("refresh %s: %v", id, err)
	}
	return r
}

// pendingOf lists the pending items of the movement as "DOCUMENT KIND blocking", one string each, in the order given.
func (f *fixture) pendingOf(t *testing.T, movement string) []string {
	t.Helper()
	items, err := f.module.GetPendingItemsOfMovement(context.Background(), api.GetPendingItemsOfMovement{TenantID: f.tenant, MovementID: movement})
	if err != nil {
		t.Fatalf("pending items of %s: %v", movement, err)
	}
	var out []string
	for _, it := range items {
		if it.Message == "" || it.ShipmentID == "" || it.Number == "" {
			t.Fatalf("a pending item: %+v", it)
		}
		out = append(out, it.DocumentID+" "+it.Kind+" "+map[bool]string{true: "blocking", false: "open"}[it.Blocking])
	}
	return out
}

func same(got []string, want ...string) bool {
	return strings.Join(got, "|") == strings.Join(want, "|")
}

func (f *fixture) houseBillWorld(t *testing.T) {
	t.Helper()
	f.seaAuthority(t, api.KindHBL)
	f.orderFor("O-A", "DOC-A")
	f.orderFor("O-L", "DOC-L")
}

func TestAHouseBillIsDatedByItsOwnContainersOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.houseBillWorld(t)
	f.setOcean("OM-1", f.loadingFacts(1, seaapi.DocFactsFinal, 0))
	f.draftHBL(t, "DOC-A", "SH-A", 1)
	f.draftHBL(t, "DOC-L", "SH-L", 1)
	if got := f.pendingOf(t, "OM-1"); !same(got, "DOC-L HOUSE_NOT_ALL_LOADED blocking") {
		t.Fatalf("pending with CV-2 not loaded: %v", got)
	}
	f.approveDoc(t, "DOC-A", 1, 2)
	f.approveDoc(t, "DOC-L", 1, 2)

	// SH-A's container is loaded: its bill is issued, dated by the loading of CV-1; SH-L's is refused for CV-2
	issued, err := f.issueDoc("DOC-A", 2, 3)
	if err != nil || issued.NewRevision != 3 {
		t.Fatalf("SH-A: %+v %v", issued, err)
	}
	if v, err := f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-A"}); err != nil || !v.Doc.OnBoardAt.Equal(f.hours(4)) {
		t.Fatalf("SH-A's bill: %v %v", v.Doc.OnBoardAt, err)
	}
	_, err = f.issueDoc("DOC-L", 2, 3)
	if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "TEMU1234565") || !strings.Contains(err.Error(), "wait for its loading") {
		t.Fatalf("SH-L's bill with CV-2 not loaded: %v", err)
	}
	if d := f.get(t, "DOC-L"); d.State != api.StateApproved || d.Revision != 2 {
		t.Fatalf("a refusal changes nothing: %+v", d)
	}
	if got := f.pendingOf(t, "OM-1"); !same(got, "DOC-L HOUSE_NOT_ALL_LOADED blocking") {
		t.Fatalf("pending after SH-A's issue: %v", got)
	}

	// CV-2 is loaded on hour 6, SEA reopens the movement and finalizes again: while it is open, wait
	f.setOcean("OM-1", f.loadingFacts(1, seaapi.DocFactsSuperseded, 0))
	if got := f.pendingOf(t, "OM-1"); !same(got, "DOC-A SOURCE_NOT_FINAL open", "DOC-L SOURCE_NOT_FINAL blocking") {
		t.Fatalf("pending while SEA is open: %v", got)
	}
	f.setOcean("OM-1", f.loadingFacts(2, seaapi.DocFactsFinal, 6))
	if got := f.pendingOf(t, "OM-1"); !same(got, "DOC-L SOURCE_SUPERSEDED blocking") {
		t.Fatalf("pending after SEA finalized again: %v", got)
	}
	one, err := f.module.GetPendingItemsOfDocument(ctx, api.GetPendingItemsOfDocument{TenantID: f.tenant, DocumentID: "DOC-L"})
	if err != nil || len(one) != 1 || one[0].Kind != api.PendingSourceSuperseded || !one[0].Blocking || one[0].DocumentID != "DOC-L" {
		t.Fatalf("pending of one document: %+v %v", one, err)
	}
	if _, err = f.issueDoc("DOC-L", 2, 4); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("issued from superseded facts: %v", err)
	}
	f.refreshDoc(t, "DOC-L", 2, 5)
	f.approveDoc(t, "DOC-L", 3, 6)
	if got := f.pendingOf(t, "OM-1"); !same(got) {
		t.Fatalf("pending after the refresh: %v", got)
	}
	if issued, err = f.issueDoc("DOC-L", 4, 7); err != nil || issued.NewRevision != 5 {
		t.Fatalf("SH-L once CV-2 is loaded: %+v %v", issued, err)
	}
	if v, err := f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-L"}); err != nil || !v.Doc.OnBoardAt.Equal(f.hours(6)) {
		t.Fatalf("SH-L's bill is dated by CV-2: %v %v", v.Doc.OnBoardAt, err)
	}
	if v, err := f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-A"}); err != nil || !v.Doc.OnBoardAt.Equal(f.hours(4)) {
		t.Fatalf("SH-A's bill is still dated by CV-1: %v %v", v.Doc.OnBoardAt, err)
	}
	if got := f.pendingOf(t, "OM-1"); !same(got) {
		t.Fatalf("pending at the end: %v", got)
	}
	// SEA's facts say another date for CV-1 (a carrier's earlier report): DOC-A needs an amendment, and says what
	// changed
	earlier := f.loadingFacts(3, seaapi.DocFactsFinal, 6)
	earlier.Containers[0].OnBoardAt = f.hours(3)
	f.setOcean("OM-1", earlier)
	if got := f.pendingOf(t, "OM-1"); !same(got, "DOC-A ISSUED_CONTENT_CHANGED open") {
		t.Fatalf("pending after a date changed: %v", got)
	}
	if one, err := f.module.GetPendingItemsOfDocument(ctx, api.GetPendingItemsOfDocument{TenantID: f.tenant, DocumentID: "DOC-A"}); err != nil || len(one) != 1 ||
		!strings.Contains(one[0].Message, "the on-board date") || one[0].Blocking {
		t.Fatalf("what changed: %+v %v", one, err)
	}

	// SH-L's house is no longer in the facts of version 3: its bill, issued, only says so
	left := f.loadingFacts(3, seaapi.DocFactsFinal, 6)
	left.Houses = left.Houses[:1]
	f.setOcean("OM-1", left)
	if got := f.pendingOf(t, "OM-1"); !same(got, "DOC-L HOUSE_LEFT_MOVEMENT open") {
		t.Fatalf("pending after SH-L left: %v", got)
	}
	// a movement SEA has no facts of (unknown, or never finalized) has no bill: an empty list; an empty tenant or
	// movement is no query; and a document that is not there is not found
	if got, err := f.module.GetPendingItemsOfMovement(ctx, api.GetPendingItemsOfMovement{TenantID: f.tenant, MovementID: "NO_SUCH_MOVEMENT"}); err != nil || len(got) != 0 {
		t.Fatalf("an unknown movement: %v %v", got, err)
	}
	if _, err = f.module.GetPendingItemsOfMovement(ctx, api.GetPendingItemsOfMovement{TenantID: "", MovementID: "OM-1"}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an empty tenant: %v", err)
	}
	if _, err = f.module.GetPendingItemsOfMovement(ctx, api.GetPendingItemsOfMovement{TenantID: f.tenant, MovementID: ""}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an empty movement: %v", err)
	}
	if _, err = f.module.GetPendingItemsOfDocument(ctx, api.GetPendingItemsOfDocument{TenantID: f.tenant, DocumentID: "NO_SUCH_DOCUMENT"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown document: %v", err)
	}
	// a cancelled bill has none
	if _, err = f.module.CancelTransportDocument(ctx, api.CancelTransportDocument{CommandID: "cancel-l", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-L",
		ExpectedRevision: 5, Reason: "the house was detached", OccurredAt: f.hours(12)}); err != nil {
		t.Fatal(err)
	}
	if got := f.pendingOf(t, "OM-1"); !same(got) {
		t.Fatalf("pending after the cancellation: %v", got)
	}
}

// An amendment that moves an issued bill to another movement moves the document's row with it; abandoning it puts the
// row back on the movement of the valid version.
func TestAbandoningAnAmendmentThatMovedTheBillOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.houseBillWorld(t)
	f.setOcean("OM-1", f.loadingFacts(1, seaapi.DocFactsFinal, 6))
	f.draftHBL(t, "DOC-L", "SH-L", 1)
	f.approveDoc(t, "DOC-L", 1, 2)
	if _, err := f.issueDoc("DOC-L", 2, 3); err != nil {
		t.Fatal(err)
	}
	// SEA's facts of OM-2 carry the house, in another consolidation
	moved := f.loadingFacts(1, seaapi.DocFactsFinal, 6)
	moved.MovementID, moved.ConsolidationID = "OM-2", "C-2"
	moved.Houses = moved.Houses[1:]
	f.setOcean("OM-2", moved)
	if _, err := f.module.OpenDocumentAmendment(ctx, api.OpenDocumentAmendment{CommandID: "am-l", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-L",
		ExpectedRevision: 3, Reason: "the house moved to OM-2", MovementID: "OM-2", OccurredAt: f.hours(8)}); err != nil {
		t.Fatal(err)
	}
	if d := f.get(t, "DOC-L"); d.MovementID != "OM-2" || d.ConsolidationID != "C-2" || d.State != api.StateAmending {
		t.Fatalf("the amendment moved the row: %+v", d)
	}
	if _, err := f.module.AbandonDocumentAmendment(ctx, api.AbandonDocumentAmendment{CommandID: "ab-l", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-L",
		ExpectedRevision: 4, Reason: "the move was a mistake", OccurredAt: f.hours(9)}); err != nil {
		t.Fatal(err)
	}
	d := f.get(t, "DOC-L")
	if d.MovementID != "OM-1" || d.ConsolidationID != "C-1" || d.State != api.StateIssued || d.CurrentVersion != 1 || d.LatestVersion != 2 {
		t.Fatalf("abandoned: %+v", d)
	}
	// the row is what the pending query of each movement finds it by
	if got := f.pendingOf(t, "OM-1"); !same(got) {
		t.Fatalf("OM-1: %v", got)
	}
	if got := f.pendingOf(t, "OM-2"); !same(got) {
		t.Fatalf("OM-2: %v", got)
	}
	f.setOcean("OM-1", f.loadingFacts(2, seaapi.DocFactsFinal, 6))
	if got := f.pendingOf(t, "OM-1"); !same(got) {
		t.Fatalf("OM-1 after its facts were finalized again with nothing changed for the house: %v", got)
	}
	f.setOcean("OM-1", f.loadingFacts(3, seaapi.DocFactsFinal, 8))
	if got := f.pendingOf(t, "OM-1"); !same(got, "DOC-L ISSUED_CONTENT_CHANGED open") {
		t.Fatalf("OM-1 after the house's date changed: %v", got)
	}
}

// An issued bill follows its house to another movement by an amendment that names it (the revise path of a draft or an
// amendment, MovementID), and is issued there at the new movement's port of loading, under an authority at that place.
// The stand-in world is used for the port: through the real contexts SEA takes a house only into a movement of the
// route of its plan, so the port is the same there (the cross-context test).
func TestAHouseBillMovedToAnotherPortIsIssuedAtItsPlaceOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.houseBillWorld(t)
	f.setOcean("OM-1", f.loadingFacts(1, seaapi.DocFactsFinal, 6))
	f.draftHBL(t, "DOC-L", "SH-L", 1)
	f.approveDoc(t, "DOC-L", 1, 2)
	if _, err := f.issueDoc("DOC-L", 2, 3); err != nil {
		t.Fatal(err)
	}
	moved := f.loadingFacts(1, seaapi.DocFactsFinal, 9)
	moved.MovementID, moved.ConsolidationID, moved.PortOfLoading = "OM-2", "C-2", "CNNBG"
	moved.Houses = moved.Houses[1:]
	f.setOcean("OM-2", moved)
	if _, err := f.module.OpenDocumentAmendment(ctx, api.OpenDocumentAmendment{CommandID: "am-l", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-L",
		ExpectedRevision: 3, Reason: "the house moved to OM-2", MovementID: "OM-2", OccurredAt: f.hours(8)}); err != nil {
		t.Fatal(err)
	}
	f.approveDoc(t, "DOC-L", 4, 9)
	issue := func(hour int) error {
		_, err := f.module.IssueDocumentAmendment(ctx, api.IssueDocumentAmendment{CommandID: "is-am-" + string(rune('a'+hour)), TenantID: f.tenant, ActorID: "doc-1",
			DocumentID: "DOC-L", ExpectedRevision: 5, CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(hour)})
		return err
	}
	if err := issue(10); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "DOC_ISSUANCE_BLOCKED") || !strings.Contains(err.Error(), "CNNBG") {
		t.Fatalf("no authority at the new port: %v", err)
	}
	if _, err := f.module.RecordIssuanceAuthority(ctx, api.RecordIssuanceAuthority{CommandID: "auth-nbg", TenantID: f.tenant, ActorID: "legal-1", AuthorityID: "AUTH-NBG",
		LegalEntityID: "LE", Kind: api.KindHBL, IssuePlace: "CNNBG", Qualification: "simulated NVOCC filing", Validity: kernel.ValidityInterval{From: f.at},
		EvidenceRef: "evidence/NBG", OccurredAt: f.hours(10)}); err != nil {
		t.Fatal(err)
	}
	if err := issue(11); err != nil {
		t.Fatalf("with the authority: %v", err)
	}
	d := f.get(t, "DOC-L")
	v, err := f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-L"})
	if err != nil || d.MovementID != "OM-2" || d.ConsolidationID != "C-2" || d.Number != "SHAL0000001" || v.Doc.IssuePlace.Code != "CNNBG" || v.Doc.PortOfLoading != "CNNBG" ||
		v.Version != 2 || !v.Doc.OnBoardAt.Equal(f.hours(9)) {
		t.Fatalf("the moved bill: %+v %+v %v", d, v.Doc, err)
	}
}

// The store finds the not cancelled sea house bills of a movement by its consolidation's index and the movement: not a
// cancelled bill, not another kind of document that names the same movement and consolidation, not another movement.
func TestTheStoreListsTheHouseBillsOfAMovementOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.houseBillWorld(t)
	f.authority(t, "AUTH-H", api.KindHAWB, "", 0)
	f.setOcean("OM-1", f.loadingFacts(1, seaapi.DocFactsFinal, 6))
	f.draftHBL(t, "DOC-A", "SH-A", 1)
	f.draftHBL(t, "DOC-L", "SH-L", 1)
	if _, err := f.module.CancelTransportDocument(ctx, api.CancelTransportDocument{CommandID: "cancel-l", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-L",
		ExpectedRevision: 1, Reason: "the house was detached", OccurredAt: f.hours(2)}); err != nil {
		t.Fatal(err)
	}
	// an air waybill on the movement AM-1 of the consolidation C-1, which OM-1 names too
	f.w.finalize(1, true, 318_000)
	f.w.order("O-A", 1)
	if _, err := f.module.DraftTransportDocument(ctx, api.DraftTransportDocument{CommandID: "draft-h1", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-H1",
		Kind: api.KindHAWB, MovementID: "AM-1", ShipmentID: "SH-A", PaymentIndicator: api.PaymentPrepaid, OccurredAt: f.hours(1)}); err != nil {
		t.Fatal(err)
	}
	list := func(movement, consolidation string) string {
		t.Helper()
		var ids []string
		if err := f.within(t, func(txCtx context.Context, _ bentopg.Executor) error {
			found, err := postgres.DocumentStore{DB: f.db}.ListOfMovement(txCtx, domain.TenantID(f.tenant), movement, consolidation)
			for _, id := range found {
				ids = append(ids, string(id))
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return strings.Join(ids, " ")
	}
	if got := list("OM-1", "C-1"); got != "DOC-A" {
		t.Fatalf("the house bills of OM-1: %q", got)
	}
	if got := list("AM-1", "C-1"); got != "" {
		t.Fatalf("an air waybill is no house bill: %q", got)
	}
	if got := list("OM-2", "C-1"); got != "" {
		t.Fatalf("another movement: %q", got)
	}
}
