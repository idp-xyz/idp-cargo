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

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// newSeaFixture publishes OceanDocFacts of OM-R with an on-board date and one house per shipment, SH-1 on order
// O-1 and so on, every order with a named consignee and a notify party, and records the HBL authority at CNSHG.
func newSeaFixture(t *testing.T, shipments int) *fixture {
	t.Helper()
	f := newFixture(t)
	facts := seaapi.OceanDocFacts{MovementID: "OM-R", Version: 1, Status: seaapi.DocFactsFinal, SHA256: "aa" + repeat('b', 62), ConsolidationID: "C-R",
		OperatingLegalEntityID: "LE", CarrierPartyID: "line-1", MasterBillNumber: "LINU2611000999", MasterBillKind: "CARRIER_MBL", Vessel: "EVER GIVEN",
		Voyage: "0456E", PlaceOfReceipt: "CNSHA", PortOfLoading: "CNSHG", PortOfDischarge: "DEHAM", LocationEdition: "E1", OnBoardAt: f.hours(1)}
	f.w.mu.Lock()
	for i := 1; i <= shipments; i++ {
		order := fmt.Sprintf("O-%d", i)
		facts.Houses = append(facts.Houses, seaapi.DocHouse{ShipmentID: fmt.Sprintf("SH-%d", i), HBL: fmt.Sprintf("SHAR%07d", i), Packs: 4, PackageType: "CT",
			GrossWeightGrams: 100000, VolumeCubicCentimetres: 500000, OrderIDs: []string{order}, GoodsDescription: "Cartons of electronics"})
		f.w.orders[order] = comapi.OrderParties{OrderID: order, Revision: 4, CustomerPartyID: "customer-" + order, Transport: comapi.Transport{Mode: comapi.ModeSea},
			Parties: []comapi.OrderPartyView{view(comapi.PartyShipper, "shipper-"+order, "CN", 1), view(comapi.PartyConsignee, "consignee-"+order, "DE", 1),
				view(comapi.PartyNotify, "notify-"+order, "DE", 1)}}
	}
	f.w.ocean["OM-R"] = facts
	f.w.mu.Unlock()
	f.seaAuthority(t, api.KindHBL)
	return f
}

func (f *fixture) seaAuthority(t *testing.T, kind string) {
	t.Helper()
	if _, err := f.module.RecordIssuanceAuthority(context.Background(), api.RecordIssuanceAuthority{CommandID: "auth-" + kind, TenantID: f.tenant,
		ActorID: "legal-1", AuthorityID: "AUTH-" + kind, LegalEntityID: "LE", Kind: kind, IssuePlace: "CNSHG", Qualification: "simulated NVOCC filing",
		Validity: kernel.ValidityInterval{From: f.at}, EvidenceRef: "evidence/" + kind, OccurredAt: f.at}); err != nil {
		t.Fatal(err)
	}
}

// toOrder makes an order's consignee the text COM holds for a consignee to order.
func (f *fixture) toOrder(order, text string, notify bool) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	o := f.w.orders[order]
	o.Parties = []comapi.OrderPartyView{view(comapi.PartyShipper, "shipper-"+order, "CN", 1),
		{Role: comapi.PartyConsignee, Version: 2, Kind: comapi.KindToOrder, ToOrder: text}}
	if notify {
		o.Parties = append(o.Parties, view(comapi.PartyNotify, "notify-"+order, "DE", 1))
	}
	f.w.orders[order] = o
}

func seaDraft(f *fixture, id, kind string, shipment int, release string, originals int) api.DraftTransportDocument {
	return api.DraftTransportDocument{CommandID: "draft-" + id, TenantID: f.tenant, ActorID: "doc-1", DocumentID: id, Kind: kind, MovementID: "OM-R",
		ShipmentID: fmt.Sprintf("SH-%d", shipment), PaymentIndicator: api.PaymentPrepaid, Release: release, Originals: originals, OccurredAt: f.hours(2)}
}

// issueSea drafts, approves and issues a sea house document of a shipment, with its order locked for it: revision 3.
func (f *fixture) issueSea(t *testing.T, id, kind string, shipment int, release string, originals int) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.module.DraftTransportDocument(ctx, seaDraft(f, id, kind, shipment, release, originals)); err != nil {
		t.Fatalf("draft %s: %v", id, err)
	}
	if _, err := f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "approve-" + id, TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: id, ExpectedRevision: 1, OccurredAt: f.hours(3)}); err != nil {
		t.Fatalf("approve %s: %v", id, err)
	}
	order := fmt.Sprintf("O-%d", shipment)
	f.w.mu.Lock()
	f.w.orders[order] = locked(f.w.orders[order], id)
	f.w.mu.Unlock()
	if got, err := f.module.IssueTransportDocument(ctx, api.IssueTransportDocument{CommandID: "issue-" + id, TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: id, ExpectedRevision: 2, CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(4)}); err != nil || got.NewRevision != 3 {
		t.Fatalf("issue %s: %+v %v", id, got, err)
	}
}

// originals reads a version's originals as "serial:status" of each one's latest record, and the number of records.
func (f *fixture) originals(t *testing.T, id string, version int) (string, int) {
	t.Helper()
	rows, err := f.module.ListOriginals(context.Background(), api.ListOriginals{TenantID: f.tenant, DocumentID: id})
	if err != nil {
		t.Fatal(err)
	}
	latest, records := map[int]api.Original{}, 0
	for _, r := range rows {
		if r.Version != version {
			continue
		}
		records++
		if r.Record != latest[r.Serial].Record+1 {
			t.Fatalf("records of %s out of order: %+v", id, rows)
		}
		latest[r.Serial] = r
	}
	var out []string
	for serial := 1; serial <= len(latest); serial++ {
		out = append(out, fmt.Sprintf("%d:%s", serial, latest[serial].Status))
	}
	return strings.Join(out, " "), records
}

func refused(err error, code string) bool {
	return errors.Is(err, api.ErrRejected) && strings.Contains(err.Error(), code)
}

// An HBL issued as three originals: nothing is amended or cancelled while they are out; all three surrendered at
// origin make it SURRENDERED, its version still the issued one, which FIN's snapshot answers with the master bill;
// a surrendered bill is cancelled, and its number stays with the shipment.
func TestOriginalsSurrenderedAtOriginOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newSeaFixture(t, 1)
	f.issueSea(t, "DOC-1", api.KindHBL, 1, api.ReleaseOriginals, 3)
	if v, err := f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-1"}); err != nil ||
		v.Doc.ReleaseType != api.ReleaseOriginals || v.Doc.Originals != 3 {
		t.Fatalf("the bill: %+v %v", v.Doc, err)
	}
	if got, records := f.originals(t, "DOC-1", 1); got != "1:ISSUED 2:ISSUED 3:ISSUED" || records != 3 {
		t.Fatalf("issued: %s (%d records)", got, records)
	}
	if _, err := f.module.CancelTransportDocument(ctx, api.CancelTransportDocument{CommandID: "cancel-out", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-1", ExpectedRevision: 3, Reason: "issued by mistake", OccurredAt: f.hours(5)}); !refused(err, "DOC_ORIGINALS_OUT") {
		t.Fatalf("cancelled with the originals out: %v", err)
	}
	if _, err := f.module.OpenDocumentAmendment(ctx, api.OpenDocumentAmendment{CommandID: "amend-out", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-1", ExpectedRevision: 3, Reason: "the goods description", OccurredAt: f.hours(5)}); !refused(err, "DOC_ORIGINALS_OUT") {
		t.Fatalf("amended with the originals out: %v", err)
	}
	surrender := api.SurrenderOriginals{CommandID: "telex", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-1", ExpectedRevision: 3,
		OccurredAt: f.hours(6)}
	if _, err := f.module.SurrenderOriginals(ctx, surrender); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("surrendered without evidence: %v", err)
	}
	surrender.CommandID, surrender.Evidence = "telex-1", "telex release instruction TLX-881"
	got, err := f.module.SurrenderOriginals(ctx, surrender)
	if err != nil || got.NewRevision != 4 {
		t.Fatalf("surrendered: %+v %v", got, err)
	}
	if again, err := f.module.SurrenderOriginals(ctx, surrender); err != nil || again != got {
		t.Fatalf("the same command again: %+v %v", again, err)
	}
	if status, records := f.originals(t, "DOC-1", 1); status != "1:SURRENDERED_AT_ORIGIN 2:SURRENDERED_AT_ORIGIN 3:SURRENDERED_AT_ORIGIN" || records != 6 {
		t.Fatalf("surrendered: %s (%d records)", status, records)
	}
	d := f.get(t, "DOC-1")
	if d.State != api.StateSurrendered || d.CurrentVersion != 1 || d.Versions[0].Status != api.VersionIssued {
		t.Fatalf("the surrendered bill: %+v", d)
	}
	snapshot, err := f.module.GetHouseDocumentSnapshot(ctx, api.GetHouseDocumentSnapshot{TenantID: f.tenant, ShipmentID: "SH-1"})
	if err != nil || snapshot.DocumentID != "DOC-1" || snapshot.Kind != api.KindHBL || snapshot.Version != 1 || snapshot.VersionStatus != api.VersionIssued ||
		snapshot.Number != "SHAR0000001" || snapshot.MasterNumber != "LINU2611000999" {
		t.Fatalf("FIN's snapshot of a surrendered bill: %+v %v", snapshot, err)
	}
	if types := f.outboxTypes(t); !slices.ContainsFunc(types, func(s string) bool { return strings.HasPrefix(s, "OriginalBillsSurrendered") }) {
		t.Fatalf("outbox %v", types)
	}
	surrender.CommandID = "telex-2"
	surrender.ExpectedRevision = 4
	if _, err = f.module.SurrenderOriginals(ctx, surrender); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("surrendered twice: %v", err)
	}
	if _, err = f.module.ReturnOriginal(ctx, api.ReturnOriginal{CommandID: "return-late", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-1",
		ExpectedRevision: 4, Serial: 1, Evidence: "at the counter", OccurredAt: f.hours(7)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("returned once surrendered: %v", err)
	}
	if _, err = f.module.OpenDocumentAmendment(ctx, api.OpenDocumentAmendment{CommandID: "amend-late", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-1", ExpectedRevision: 4, Reason: "the goods description", OccurredAt: f.hours(7)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("amended once surrendered: %v", err)
	}
	if _, err = f.module.CancelTransportDocument(ctx, api.CancelTransportDocument{CommandID: "cancel-1", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-1", ExpectedRevision: 4, Reason: "issued by mistake", OccurredAt: f.hours(8)}); err != nil {
		t.Fatalf("a surrendered bill is cancelled: %v", err)
	}
	if d = f.get(t, "DOC-1"); d.State != api.StateCancelled {
		t.Fatalf("cancelled: %+v", d)
	}
	if _, err = f.module.GetHouseDocumentSnapshot(ctx, api.GetHouseDocumentSnapshot{TenantID: f.tenant, ShipmentID: "SH-1"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("FIN's snapshot of a cancelled bill: %v", err)
	}
	next := seaDraft(f, "DOC-1B", api.KindHBL, 1, api.ReleaseExpress, 0)
	next.OccurredAt = f.hours(9)
	if _, err = f.module.DraftTransportDocument(ctx, next); err != nil {
		t.Fatalf("the shipment's next bill: %v", err)
	}
}

// Originals returned one by one for an amendment. Abandoning it hands them back; issuing it gives the new version
// originals of its own and leaves the old ones returned.
func TestOriginalsReturnedForAnAmendmentOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newSeaFixture(t, 1)
	f.issueSea(t, "DOC-2", api.KindHBL, 1, api.ReleaseOriginals, 2)
	revision, hour := int64(3), 4
	done := func(err error) error { // every command is an hour after the one before; a committed one steps the revision
		if err == nil {
			revision++
		}
		return err
	}
	back := func(serial int, name string) error {
		hour++
		_, err := f.module.ReturnOriginal(ctx, api.ReturnOriginal{CommandID: name, TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-2",
			ExpectedRevision: revision, Serial: serial, Evidence: "handed in at the Shanghai counter", OccurredAt: f.hours(hour)})
		return done(err)
	}
	amend := func(name string) error {
		hour++
		_, err := f.module.OpenDocumentAmendment(ctx, api.OpenDocumentAmendment{CommandID: name, TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-2",
			ExpectedRevision: revision, Reason: "the goods description", OccurredAt: f.hours(hour)})
		return done(err)
	}
	if err := back(3, "return-3"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an original the bill has not: %v", err)
	}
	if err := back(1, "return-1"); err != nil {
		t.Fatal(err)
	}
	if err := back(1, "return-1-again"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("returned twice: %v", err)
	}
	if err := amend("amend-part"); !refused(err, "DOC_ORIGINALS_OUT") {
		t.Fatalf("amended with one original out: %v", err)
	}
	if err := errors.Join(back(2, "return-2"), amend("amend-1")); err != nil {
		t.Fatal(err)
	}
	hour++
	if _, err := f.module.AbandonDocumentAmendment(ctx, api.AbandonDocumentAmendment{CommandID: "abandon-1", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-2", ExpectedRevision: revision, Reason: "the shipper withdrew the change", OccurredAt: f.hours(hour)}); done(err) != nil {
		t.Fatal(err)
	}
	if status, records := f.originals(t, "DOC-2", 1); status != "1:ISSUED 2:ISSUED" || records != 6 || f.get(t, "DOC-2").State != api.StateIssued {
		t.Fatalf("after the abandoned amendment: %s (%d records)", status, records)
	}
	rows, _ := f.module.ListOriginals(ctx, api.ListOriginals{TenantID: f.tenant, DocumentID: "DOC-2"})
	if last := rows[len(rows)-1]; last.Record != 3 || last.Status != api.OriginalIssued || last.Evidence != "the shipper withdrew the change" || last.By != "doc-1" {
		t.Fatalf("handed back: %+v", last)
	}

	// the amendment issued: SEA publishes version 2 of the facts
	f.w.mu.Lock()
	facts := f.w.ocean["OM-R"]
	facts.Version, facts.SHA256 = 2, "cc"+repeat('d', 62)
	facts.Houses = slices.Clone(facts.Houses)
	facts.Houses[0].GoodsDescription = "Cartons of machinery"
	f.w.ocean["OM-R"] = facts
	f.w.mu.Unlock()
	if err := errors.Join(back(1, "return-1b"), back(2, "return-2b"), amend("amend-2")); err != nil {
		t.Fatal(err)
	}
	hour++
	if _, err := f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "approve-amend-2", TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: "DOC-2", ExpectedRevision: revision, OccurredAt: f.hours(hour)}); done(err) != nil {
		t.Fatal(err)
	}
	hour++
	amended, err := f.module.IssueDocumentAmendment(ctx, api.IssueDocumentAmendment{CommandID: "issue-amend-2", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-2", ExpectedRevision: revision, CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(hour)})
	if done(err) != nil || amended.Version != 3 {
		t.Fatalf("the amendment: %+v %v", amended, err)
	}
	old, oldRecords := f.originals(t, "DOC-2", 1)
	current, records := f.originals(t, "DOC-2", 3)
	if old != "1:RETURNED 2:RETURNED" || oldRecords != 8 || current != "1:ISSUED 2:ISSUED" || records != 2 {
		t.Fatalf("after the amendment: version 1 %s (%d), version 3 %s (%d)", old, oldRecords, current, records)
	}
	if _, err = f.module.SurrenderOriginals(ctx, api.SurrenderOriginals{CommandID: "telex-2", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-2",
		ExpectedRevision: revision, Evidence: "telex release instruction TLX-882", OccurredAt: f.hours(hour + 1)}); err != nil {
		t.Fatal(err)
	}
	if d := f.get(t, "DOC-2"); d.State != api.StateSurrendered || d.CurrentVersion != 3 {
		t.Fatalf("the amended bill surrendered: %+v", d)
	}
}

// Originals returned for an amendment that is then abandoned are handed back, and the document is issued again with
// the discarded draft as its latest version: every original is out, so they are surrendered at origin, and the
// surrendered bill is cancelled.
func TestOriginalsSurrenderedAfterAnAbandonedAmendmentOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newSeaFixture(t, 1)
	f.issueSea(t, "DOC-3", api.KindHBL, 1, api.ReleaseOriginals, 2) // revision 3, business time at hour 4
	// every command is an hour after the one before and at the revision the one before made
	for i, serial := range []int{1, 2} {
		if got, err := f.module.ReturnOriginal(ctx, api.ReturnOriginal{CommandID: fmt.Sprintf("return-3-%d", serial), TenantID: f.tenant, ActorID: "doc-1",
			DocumentID: "DOC-3", ExpectedRevision: int64(3 + i), Serial: serial, Evidence: "handed in at the Shanghai counter",
			OccurredAt: f.hours(5 + i)}); err != nil || got.NewRevision != int64(4+i) {
			t.Fatalf("returned original %d: %+v %v", serial, got, err)
		}
	}
	if got, err := f.module.OpenDocumentAmendment(ctx, api.OpenDocumentAmendment{CommandID: "amend-3", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-3", ExpectedRevision: 5, Reason: "the goods description", OccurredAt: f.hours(7)}); err != nil || got.NewRevision != 6 {
		t.Fatalf("the amendment opened: %+v %v", got, err)
	}
	if got, err := f.module.AbandonDocumentAmendment(ctx, api.AbandonDocumentAmendment{CommandID: "abandon-3", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-3", ExpectedRevision: 6, Reason: "the shipper withdrew the change", OccurredAt: f.hours(8)}); err != nil || got.NewRevision != 7 {
		t.Fatalf("the amendment abandoned: %+v %v", got, err)
	}
	if status, records := f.originals(t, "DOC-3", 1); status != "1:ISSUED 2:ISSUED" || records != 6 {
		t.Fatalf("after the abandoned amendment: %s (%d records)", status, records)
	}
	if d := f.get(t, "DOC-3"); d.State != api.StateIssued || d.CurrentVersion != 1 || len(d.Versions) != 2 || d.Versions[0].Status != api.VersionIssued ||
		d.Versions[1].Status != api.VersionDiscarded {
		t.Fatalf("the document after the abandoned amendment: %+v", d)
	}

	if got, err := f.module.SurrenderOriginals(ctx, api.SurrenderOriginals{CommandID: "telex-3", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-3",
		ExpectedRevision: 7, Evidence: "telex release instruction TLX-884", OccurredAt: f.hours(9)}); err != nil || got.NewRevision != 8 {
		t.Fatalf("surrendered after the abandoned amendment: %+v %v", got, err)
	}
	d := f.get(t, "DOC-3")
	if d.State != api.StateSurrendered || d.CurrentVersion != 1 {
		t.Fatalf("the surrendered bill: %+v", d)
	}
	if status, records := f.originals(t, "DOC-3", 1); status != "1:SURRENDERED_AT_ORIGIN 2:SURRENDERED_AT_ORIGIN" || records != 8 {
		t.Fatalf("surrendered: %s (%d records)", status, records)
	}
	snapshot, err := f.module.GetHouseDocumentSnapshot(ctx, api.GetHouseDocumentSnapshot{TenantID: f.tenant, ShipmentID: "SH-1"})
	if err != nil || snapshot.DocumentID != "DOC-3" || snapshot.Version != 1 || snapshot.VersionStatus != api.VersionIssued {
		t.Fatalf("FIN's snapshot of the surrendered bill: %+v %v", snapshot, err)
	}
	if _, err = f.module.CancelTransportDocument(ctx, api.CancelTransportDocument{CommandID: "cancel-3", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-3", ExpectedRevision: 8, Reason: "issued by mistake", OccurredAt: f.hours(10)}); err != nil {
		t.Fatalf("a surrendered bill is cancelled: %v", err)
	}
	if d = f.get(t, "DOC-3"); d.State != api.StateCancelled {
		t.Fatalf("cancelled: %+v", d)
	}
}

// An original that is already back with the issuer is surrendered with the rest: three originals, the second returned,
// then all three surrendered at origin, the second with its third record; FIN's snapshot answers, and the bill is
// cancelled.
func TestOriginalsSurrenderedWithOneAlreadyReturnedOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newSeaFixture(t, 1)
	f.issueSea(t, "DOC-4", api.KindHBL, 1, api.ReleaseOriginals, 3) // revision 3, business time at hour 4
	if got, err := f.module.ReturnOriginal(ctx, api.ReturnOriginal{CommandID: "return-4-2", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-4",
		ExpectedRevision: 3, Serial: 2, Evidence: "handed in at the Shanghai counter", OccurredAt: f.hours(5)}); err != nil || got.NewRevision != 4 {
		t.Fatalf("returned original 2: %+v %v", got, err)
	}
	if status, records := f.originals(t, "DOC-4", 1); status != "1:ISSUED 2:RETURNED 3:ISSUED" || records != 4 {
		t.Fatalf("after the return: %s (%d records)", status, records)
	}
	if got, err := f.module.SurrenderOriginals(ctx, api.SurrenderOriginals{CommandID: "telex-4", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-4",
		ExpectedRevision: 4, Evidence: "telex release instruction TLX-885", OccurredAt: f.hours(6)}); err != nil || got.NewRevision != 5 {
		t.Fatalf("surrendered with one original already returned: %+v %v", got, err)
	}
	if d := f.get(t, "DOC-4"); d.State != api.StateSurrendered || d.CurrentVersion != 1 || d.Versions[0].Status != api.VersionIssued {
		t.Fatalf("the surrendered bill: %+v", d)
	}
	if status, records := f.originals(t, "DOC-4", 1); status != "1:SURRENDERED_AT_ORIGIN 2:SURRENDERED_AT_ORIGIN 3:SURRENDERED_AT_ORIGIN" || records != 7 {
		t.Fatalf("surrendered: %s (%d records)", status, records)
	}
	rows, err := f.module.ListOriginals(ctx, api.ListOriginals{TenantID: f.tenant, DocumentID: "DOC-4"})
	if err != nil {
		t.Fatal(err)
	}
	records := map[int]int{}
	for _, r := range rows {
		records[r.Serial]++
	}
	if records[1] != 2 || records[2] != 3 || records[3] != 2 {
		t.Fatalf("records of the three originals: %v", records)
	}
	snapshot, err := f.module.GetHouseDocumentSnapshot(ctx, api.GetHouseDocumentSnapshot{TenantID: f.tenant, ShipmentID: "SH-1"})
	if err != nil || snapshot.DocumentID != "DOC-4" || snapshot.Version != 1 || snapshot.VersionStatus != api.VersionIssued {
		t.Fatalf("FIN's snapshot of the surrendered bill: %+v %v", snapshot, err)
	}
	if _, err = f.module.CancelTransportDocument(ctx, api.CancelTransportDocument{CommandID: "cancel-4", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-4",
		ExpectedRevision: 5, Reason: "issued by mistake", OccurredAt: f.hours(7)}); err != nil {
		t.Fatalf("a surrendered bill is cancelled: %v", err)
	}
}

// The consignee to order is the order's: DOC prints the text COM holds, on an HBL issued as originals whose order
// names a notify party, and nowhere else.
func TestAConsigneeToOrderOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newSeaFixture(t, 2)
	f.seaAuthority(t, api.KindHouseSeaWaybill)
	f.toOrder("O-1", "TO ORDER OF BANK OF CHINA", true)
	f.toOrder("O-2", "TO ORDER", false)
	for name, draft := range map[string]api.DraftTransportDocument{
		"on an EXPRESS bill":                seaDraft(f, "DOC-E", api.KindHBL, 1, api.ReleaseExpress, 0),
		"on a bill released as the default": seaDraft(f, "DOC-D", api.KindHBL, 1, "", 0),
		"on a sea waybill":                  seaDraft(f, "DOC-W", api.KindHouseSeaWaybill, 1, "", 0),
		"without a notify party":            seaDraft(f, "DOC-N", api.KindHBL, 2, api.ReleaseOriginals, 3),
	} {
		if _, err := f.module.DraftTransportDocument(ctx, draft); !refused(err, "DOC_INCOMPLETE") {
			t.Errorf("to order %s: %v", name, err)
		}
	}
	f.issueSea(t, "DOC-1", api.KindHBL, 1, api.ReleaseOriginals, 3)
	v, err := f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-1"})
	if err != nil || v.Doc.Consignee.ToOrder != "TO ORDER OF BANK OF CHINA" || v.Doc.Consignee.PartyID != "" || v.Doc.Consignee.LegalName != "" ||
		v.Doc.Notify == nil || v.Doc.Notify.PartyID != "notify-O-1" || v.Doc.Orders[0].ConsigneeVersion != 2 || v.Status != api.VersionIssued {
		t.Fatalf("the bill to order: %+v %v", v.Doc, err)
	}
	if !strings.Contains(string(v.Content), `"to_order":"TO ORDER OF BANK OF CHINA"`) {
		t.Fatalf("the content as written: %s", v.Content)
	}
	if got, _ := f.originals(t, "DOC-1", 1); got != "1:ISSUED 2:ISSUED 3:ISSUED" {
		t.Fatalf("originals: %s", got)
	}
}

// A house sea waybill: its own authority, a named consignee, no originals; and one sea house document per shipment.
func TestAHouseSeaWaybillOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newSeaFixture(t, 1)
	for name, draft := range map[string]api.DraftTransportDocument{
		"released as originals": seaDraft(f, "DOC-S", api.KindHouseSeaWaybill, 1, api.ReleaseOriginals, 3),
		"released as EXPRESS":   seaDraft(f, "DOC-S", api.KindHouseSeaWaybill, 1, api.ReleaseExpress, 0),
		"with originals":        seaDraft(f, "DOC-S", api.KindHouseSeaWaybill, 1, "", 1),
	} {
		if _, err := f.module.DraftTransportDocument(ctx, draft); !errors.Is(err, api.ErrRejected) {
			t.Errorf("a sea waybill %s: %v", name, err)
		}
	}
	if _, err := f.module.DraftTransportDocument(ctx, seaDraft(f, "DOC-S", api.KindHouseSeaWaybill, 1, "", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.DraftTransportDocument(ctx, seaDraft(f, "DOC-B", api.KindHBL, 1, "", 0)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("an HBL of a shipment that has a sea waybill: %v", err)
	}
	if _, err := f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "approve-s", TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: "DOC-S", ExpectedRevision: 1, OccurredAt: f.hours(3)}); err != nil {
		t.Fatal(err)
	}
	f.w.mu.Lock()
	f.w.orders["O-1"] = locked(f.w.orders["O-1"], "DOC-S")
	f.w.mu.Unlock()
	issue := api.IssueTransportDocument{CommandID: "issue-s", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-S", ExpectedRevision: 2,
		CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(4)}
	if _, err := f.module.IssueTransportDocument(ctx, issue); !refused(err, "DOC_ISSUANCE_BLOCKED") {
		t.Fatalf("issued under the HBL's authority: %v", err)
	}
	f.seaAuthority(t, api.KindHouseSeaWaybill)
	issue.CommandID = "issue-s2"
	if got, err := f.module.IssueTransportDocument(ctx, issue); err != nil || got.NewRevision != 3 {
		t.Fatalf("issued: %+v %v", got, err)
	}
	v, err := f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-S"})
	if err != nil || v.Doc.Kind != api.KindHouseSeaWaybill || v.Doc.ReleaseType != api.ReleaseSeaWaybill || v.Doc.IssuerCapacity != "NVOCC_CARRIER" ||
		v.Doc.Originals != 0 || v.Doc.Consignee.PartyID != "consignee-O-1" || v.Doc.Number != "SHAR0000001" {
		t.Fatalf("the sea waybill: %+v %v", v.Doc, err)
	}
	if rows, err := f.module.ListOriginals(ctx, api.ListOriginals{TenantID: f.tenant, DocumentID: "DOC-S"}); err != nil || len(rows) != 0 {
		t.Fatalf("a sea waybill with originals: %+v %v", rows, err)
	}
	if _, err = f.module.SurrenderOriginals(ctx, api.SurrenderOriginals{CommandID: "telex-s", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-S",
		ExpectedRevision: 3, Evidence: "telex", OccurredAt: f.hours(5)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a sea waybill surrendered: %v", err)
	}
	snapshot, err := f.module.GetHouseDocumentSnapshot(ctx, api.GetHouseDocumentSnapshot{TenantID: f.tenant, ShipmentID: "SH-1"})
	if err != nil || snapshot.Kind != api.KindHouseSeaWaybill || snapshot.MasterNumber != "LINU2611000999" {
		t.Fatalf("FIN's snapshot: %+v %v", snapshot, err)
	}
	// the sea waybill renders under the bill of lading's template
	if rendered, err := f.module.RenderDocumentVersion(ctx, api.RenderDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-S"}); err != nil ||
		rendered.Template != api.TemplateHBL || !strings.HasPrefix(string(rendered.PDF), "%PDF-1.4") {
		t.Fatalf("a sea waybill rendered: %+v %v", rendered.Template, err)
	}
	if _, err = f.module.CancelTransportDocument(ctx, api.CancelTransportDocument{CommandID: "cancel-s", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-S", ExpectedRevision: 3, Reason: "the shipper wants a bill of lading", OccurredAt: f.hours(6)}); err != nil {
		t.Fatal(err)
	}
	bill := seaDraft(f, "DOC-B", api.KindHBL, 1, api.ReleaseOriginals, 3)
	bill.CommandID, bill.OccurredAt = "draft-b2", f.hours(7)
	if _, err = f.module.DraftTransportDocument(ctx, bill); err != nil {
		t.Fatalf("an HBL once the sea waybill is cancelled: %v", err)
	}
}

// TestSeaHouseDocumentsRaceForOneShipment drafts four HBLs and four sea waybills of one shipment at once: one wins,
// and the others are answered a conflict.
func TestSeaHouseDocumentsRaceForOneShipment(t *testing.T) {
	f := newSeaFixture(t, 1)
	const workers = 8
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			kind := api.KindHBL
			if i%2 == 1 {
				kind = api.KindHouseSeaWaybill
			}
			_, errs[i] = f.module.DraftTransportDocument(context.Background(), seaDraft(f, fmt.Sprintf("DOC-R%d", i), kind, 1, "", 0))
		}()
	}
	close(start)
	wg.Wait()
	committed, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, api.ErrConflict):
			conflicts++
		}
	}
	if committed != 1 || conflicts != workers-1 {
		t.Fatalf("%d committed, %d conflicts: %v", committed, conflicts, errs)
	}
}

// Past DOC on the application role: original records are another tenant's to neither read nor write, are written
// only with their document, only in order, and never leave a document in a state its originals deny.
func TestOriginalsPastDOCOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newSeaFixture(t, 5)
	f.issueSea(t, "DOC-1", api.KindHBL, 1, api.ReleaseOriginals, 2)
	f.issueSea(t, "DOC-E", api.KindHBL, 2, api.ReleaseExpress, 0)
	count := func(g *fixture) (n int) {
		t.Helper()
		if err := g.within(t, func(ctx context.Context, e bentopg.Executor) error {
			return e.QueryRow(ctx, `SELECT count(*) FROM cargo_doc.original`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	other := newFixture(t)
	if mine, theirs := count(f), count(other); mine != 2 || theirs != 0 {
		t.Fatalf("original records in sight: %d of the tenant's own, %d from another tenant's scope", mine, theirs)
	}
	// Two more documents, each brought to its state by DOC's own commands: DOC-S surrendered at origin, and DOC-A
	// amending, both originals returned and the amendment approved.
	f.issueSea(t, "DOC-S", api.KindHBL, 3, api.ReleaseOriginals, 2)
	f.issueSea(t, "DOC-A", api.KindHBL, 4, api.ReleaseOriginals, 2)
	if _, err := f.module.SurrenderOriginals(ctx, api.SurrenderOriginals{CommandID: "telex-s", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-S",
		ExpectedRevision: 3, Evidence: "telex release instruction TLX-884", OccurredAt: f.hours(5)}); err != nil {
		t.Fatal(err)
	}
	for i, serial := range []int{1, 2} {
		if _, err := f.module.ReturnOriginal(ctx, api.ReturnOriginal{CommandID: fmt.Sprintf("return-a-%d", serial), TenantID: f.tenant, ActorID: "doc-1",
			DocumentID: "DOC-A", ExpectedRevision: int64(3 + i), Serial: serial, Evidence: "handed in at the Shanghai counter", OccurredAt: f.hours(5 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.module.OpenDocumentAmendment(ctx, api.OpenDocumentAmendment{CommandID: "amend-a", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-A",
		ExpectedRevision: 5, Reason: "the goods description", OccurredAt: f.hours(7)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "approve-a", TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: "DOC-A", ExpectedRevision: 6, OccurredAt: f.hours(8)}); err != nil {
		t.Fatal(err)
	}
	// DOC-WI is an issued house sea waybill: it has no originals, and its kind is what a record of originals meets
	f.seaAuthority(t, api.KindHouseSeaWaybill)
	f.issueSea(t, "DOC-WI", api.KindHouseSeaWaybill, 5, "", 0)
	var unforced int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE n.nspname = 'cargo_doc' AND c.relkind = 'r' AND NOT (c.relrowsecurity AND c.relforcerowsecurity
              AND EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid))`).Scan(&unforced)
	}); err != nil || unforced != 0 {
		t.Fatalf("%d tables of cargo_doc without forced row security and a policy: %v", unforced, err)
	}
	// every policy of cargo_doc has both a USING and a WITH CHECK expression, and each names the tenant the transaction is bound to
	var loose int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT count(*) FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE n.nspname = 'cargo_doc' AND (p.polqual IS NULL OR p.polwithcheck IS NULL
              OR pg_get_expr(p.polqual, p.polrelid) NOT LIKE '%app.tenant_id%' OR pg_get_expr(p.polwithcheck, p.polrelid) NOT LIKE '%app.tenant_id%')`).Scan(&loose)
	}); err != nil || loose != 0 {
		t.Fatalf("%d policies of cargo_doc without a USING and a WITH CHECK expression that name app.tenant_id: %v", loose, err)
	}
	// the records are immutable by a trigger as well as by the application role's grants: a BEFORE UPDATE OR DELETE
	// row trigger calling reject_row_change (tgtype bits: 1 row, 2 before, 8 delete, 16 update)
	var immutable int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT count(*) FROM pg_trigger t WHERE t.tgrelid = 'cargo_doc.original'::regclass AND NOT t.tgisinternal AND t.tgenabled = 'O'
            AND t.tgfoid = 'cargo_doc.reject_row_change'::regproc AND t.tgtype & 27 = 27 AND t.tgtype & 4 = 0`).Scan(&immutable)
	}); err != nil || immutable != 1 {
		t.Fatalf("%d BEFORE UPDATE OR DELETE triggers of cargo_doc.original that call reject_row_change: %v", immutable, err)
	}
	const (
		touch  = `UPDATE cargo_doc.transport_document SET revision=revision+1 WHERE tenant_id=$1 AND document_id='DOC-1'`
		record = `INSERT INTO cargo_doc.original (tenant_id,document_id,version_no,serial,record_no,status,actor,at,evidence,entered_at) VALUES `
		// the rows of a second issued version of a document, copied from its first as a write past DOC would (%s is the document)
		versionTwo = `INSERT INTO cargo_doc.document_version (tenant_id,document_id,version_no,content,content_sha256,source_movement_id,source_version,
            reason,drafted_by,drafted_at,entered_at)
            SELECT tenant_id,document_id,2,content,content_sha256,source_movement_id,source_version,'past DOC',drafted_by,now(),now()
            FROM cargo_doc.document_version WHERE tenant_id=$1 AND document_id='%s' AND version_no=1`
		// the same for a house sea waybill whose second version says it is an HBL of two originals
		versionAsBill = `INSERT INTO cargo_doc.document_version (tenant_id,document_id,version_no,content,content_sha256,source_movement_id,source_version,
            reason,drafted_by,drafted_at,entered_at)
            SELECT tenant_id,document_id,2,c,encode(sha256(convert_to(c,'UTF8')),'hex'),source_movement_id,source_version,'past DOC',drafted_by,now(),now()
            FROM (SELECT v.*, replace(replace(content,'"kind":"HOUSE_SEA_WAYBILL"','"kind":"HBL"'),'"release_type":"SEA_WAYBILL"',
              '"release_type":"ORIGINALS","originals":2') AS c
              FROM cargo_doc.document_version v WHERE tenant_id=$1 AND document_id='%s' AND version_no=1) x`
		approvalTwo = `INSERT INTO cargo_doc.document_version_approval (tenant_id,document_id,version_no,approved_by,approved_at,self_approved,entered_at)
            SELECT tenant_id,document_id,2,approved_by,now(),self_approved,now() FROM cargo_doc.document_version_approval
            WHERE tenant_id=$1 AND document_id='%s' AND version_no=1`
		issueTwo = `INSERT INTO cargo_doc.document_version_issue (tenant_id,document_id,version_no,issued_by,issued_at,authority_id,authority_version,
            credit_basis,credit_decision_id,credit_override_by,credit_override_reason,entered_at)
            SELECT tenant_id,document_id,2,issued_by,now(),authority_id,authority_version,credit_basis,credit_decision_id,credit_override_by,
              credit_override_reason,now()
            FROM cargo_doc.document_version_issue WHERE tenant_id=$1 AND document_id='%s' AND version_no=1`
		originalsTwo = `($1,'%[1]s',2,1,1,'ISSUED','x',now(),NULL,now()),($1,'%[1]s',2,2,1,'ISSUED','x',now(),NULL,now())`
	)
	touchOf := func(doc string) string {
		return fmt.Sprintf(`UPDATE cargo_doc.transport_document SET revision=revision+1 WHERE tenant_id=$1 AND document_id='%s'`, doc)
	}
	// the writes that draft, approve and issue a second version of a document, and that give it two originals
	draftTwo := func(doc string) []string {
		return []string{fmt.Sprintf(versionTwo, doc), fmt.Sprintf(approvalTwo, doc)}
	}
	issueTwoWithOriginals := func(doc string) []string {
		return []string{fmt.Sprintf(issueTwo, doc), record + fmt.Sprintf(originalsTwo, doc)}
	}
	for name, tc := range map[string]struct {
		as    *fixture
		sql   []string
		state string
		check string
	}{
		"a record changed, the application role lacking UPDATE": {f, []string{`UPDATE cargo_doc.original SET status='RETURNED' WHERE tenant_id=$1`}, "42501", ""},
		"a record removed, the application role lacking DELETE": {f, []string{`DELETE FROM cargo_doc.original WHERE tenant_id=$1`}, "42501", ""},
		"another tenant's, who cannot see the document": {other, []string{record + `('` + f.tenant + `','DOC-1',1,1,2,'RETURNED','x',now(),'x',now())`}, "23514",
			"original_record_follows"},
		"behind DOC":       {f, []string{record + `($1,'DOC-1',1,1,2,'RETURNED','x',now(),'x',now())`}, "23514", "document_records_with_document"},
		"issued twice":     {f, []string{touch, record + `($1,'DOC-1',1,1,2,'ISSUED','x',now(),'x',now())`}, "23514", "original_record_follows"},
		"a record skipped": {f, []string{touch, record + `($1,'DOC-1',1,1,3,'RETURNED','x',now(),'x',now())`}, "23514", "original_record_follows"},
		"a third original": {f, []string{touch, record + `($1,'DOC-1',1,3,1,'ISSUED','x',now(),NULL,now())`}, "23514", "original_record_follows"},
		"a tenth original": {f, []string{touch, record + `($1,'DOC-1',1,10,1,'ISSUED','x',now(),NULL,now())`}, "23514", "original_record_follows"},
		"without evidence": {f, []string{touch, record + `($1,'DOC-1',1,1,2,'RETURNED','x',now(),NULL,now())`}, "23514", "original_evidence"},
		"on an EXPRESS bill": {f, []string{`UPDATE cargo_doc.transport_document SET revision=revision+1 WHERE tenant_id=$1 AND document_id='DOC-E'`,
			record + `($1,'DOC-E',1,1,1,'ISSUED','x',now(),NULL,now())`}, "23514", "original_record_follows"},
		"of a version not issued": {f, []string{touch, record + `($1,'DOC-1',2,1,1,'ISSUED','x',now(),NULL,now())`}, "23503|23514", ""},
		"surrendered behind the state": {f, []string{touch, record + `($1,'DOC-1',1,1,2,'SURRENDERED_AT_ORIGIN','x',now(),'x',now()),
            ($1,'DOC-1',1,2,2,'SURRENDERED_AT_ORIGIN','x',now(),'x',now())`}, "23514", "transport_document_consistent"},
		"one of two surrendered": {f, []string{`UPDATE cargo_doc.transport_document SET state='SURRENDERED',revision=revision+1 WHERE tenant_id=$1 AND document_id='DOC-1'`,
			record + `($1,'DOC-1',1,1,2,'SURRENDERED_AT_ORIGIN','x',now(),'x',now())`}, "23514", "transport_document_consistent"},
		"SURRENDERED with the originals out": {f, []string{`UPDATE cargo_doc.transport_document SET state='SURRENDERED',revision=revision+1
            WHERE tenant_id=$1 AND document_id='DOC-1'`}, "23514", "transport_document_consistent"},
		"an EXPRESS bill SURRENDERED": {f, []string{`UPDATE cargo_doc.transport_document SET state='SURRENDERED',revision=revision+1
            WHERE tenant_id=$1 AND document_id='DOC-E'`}, "23514", "transport_document_consistent"},
		"CANCELLED with the originals out": {f, []string{`UPDATE cargo_doc.transport_document SET state='CANCELLED',cancel_reason='x',cancelled_by='x',
            cancelled_at=now(),revision=revision+1 WHERE tenant_id=$1 AND document_id='DOC-1'`}, "23514", "transport_document_consistent"},
		// an issued version is superseded only once every one of its originals is returned
		"a second issue over originals out": {f, slices.Concat([]string{touch}, draftTwo("DOC-1"), issueTwoWithOriginals("DOC-1")), "23514",
			"transport_document_consistent"},
		"a second issue over a surrendered bill": {f, slices.Concat([]string{`UPDATE cargo_doc.transport_document SET state='ISSUED',revision=revision+1
            WHERE tenant_id=$1 AND document_id='DOC-S'`}, draftTwo("DOC-S"), issueTwoWithOriginals("DOC-S")), "23514", "transport_document_consistent"},
		"an amendment issued over an original handed back": {f, slices.Concat([]string{`UPDATE cargo_doc.transport_document SET state='ISSUED',
            revision=revision+1 WHERE tenant_id=$1 AND document_id='DOC-A'`, record + `($1,'DOC-A',1,1,3,'ISSUED','x',now(),'x',now())`},
			issueTwoWithOriginals("DOC-A")), "23514", "transport_document_consistent"},
		// originals of a document that is not an HBL, though a version past DOC says it is
		"originals of a house sea waybill given a version that says HBL": {f, slices.Concat([]string{touchOf("DOC-WI"), fmt.Sprintf(versionAsBill, "DOC-WI"),
			fmt.Sprintf(approvalTwo, "DOC-WI")}, issueTwoWithOriginals("DOC-WI")), "23514", "original_record_follows"},
		// the records of an amending document (DOC-A: both originals returned, its draft version 2 approved) and of a
		// surrendered one (DOC-S), by the rule that refuses each
		"an original handed back while amending": {f, []string{touchOf("DOC-A"), record + `($1,'DOC-A',1,1,3,'ISSUED','x',now(),'x',now())`}, "23514",
			"transport_document_consistent"},
		"both originals surrendered while amending": {f, []string{touchOf("DOC-A"), record + `($1,'DOC-A',1,1,3,'SURRENDERED_AT_ORIGIN','x',now(),'x',now()),
            ($1,'DOC-A',1,2,3,'SURRENDERED_AT_ORIGIN','x',now(),'x',now())`}, "23514", "transport_document_consistent"},
		"a returned original returned again": {f, []string{touchOf("DOC-A"), record + `($1,'DOC-A',1,1,3,'RETURNED','x',now(),'x',now())`}, "23514",
			"original_record_follows"},
		"a record of the amendment's draft": {f, []string{touchOf("DOC-A"), record + `($1,'DOC-A',2,1,1,'ISSUED','x',now(),NULL,now())`}, "23514",
			"original_record_follows"},
		"a return after the surrender": {f, []string{touchOf("DOC-S"), record + `($1,'DOC-S',1,1,3,'RETURNED','x',now(),'x',now())`}, "23514",
			"original_record_follows"},
		"a hand back after the surrender": {f, []string{touchOf("DOC-S"), record + `($1,'DOC-S',1,1,3,'ISSUED','x',now(),'x',now())`}, "23514",
			"original_record_follows"},
	} {
		err := tc.as.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, sql := range tc.sql {
				args := []any{}
				if strings.Contains(sql, "$1") {
					args = append(args, tc.as.tenant)
				}
				if _, err := e.Exec(ctx, sql, args...); err != nil {
					return err
				}
			}
			return nil
		})
		var pgErr *pgconn.PgError
		if state := sqlState(err); state == "" || !strings.Contains(tc.state, state) || tc.check != "" && (!errors.As(err, &pgErr) || pgErr.ConstraintName != tc.check) {
			t.Errorf("%s: %v (state %s), want %s %s", name, err, state, tc.state, tc.check)
		}
		t.Logf("%s refused: %v", name, err)
	}
	// nothing above was written, and DOC's own commands still work on the document. The tenant's records are ten:
	// DOC-1's two (issued), DOC-E's none, DOC-S's four (issued, surrendered) and DOC-A's four (issued, returned).
	if got, records := f.originals(t, "DOC-1", 1); got != "1:ISSUED 2:ISSUED" || records != 2 || count(f) != 10 {
		t.Fatalf("after the refused writes: %s (%d records)", got, records)
	}
	if _, err := f.module.SurrenderOriginals(ctx, api.SurrenderOriginals{CommandID: "telex", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-1",
		ExpectedRevision: 3, Evidence: "telex release instruction TLX-883", OccurredAt: f.hours(5)}); err != nil {
		t.Fatal(err)
	}
}

// A record for an original beside the issue of the amendment that supersedes its version: one transaction writes the
// record, while version 1 is still the last issued, DOC issues the amendment in a transaction of its own, and the
// first transaction then touches the document and commits. The commit is refused, because at commit every original of
// a version that is no longer the last issued one is RETURNED; the amendment stays issued with its originals out.
func TestARecordBesideAnAmendmentIssuedAtOnceOnPostgres(t *testing.T) {
	f := newSeaFixture(t, 1)
	f.issueSea(t, "DOC-6", api.KindHBL, 1, api.ReleaseOriginals, 2) // revision 3, business time at hour 4
	for i, serial := range []int{1, 2} {
		if _, err := f.module.ReturnOriginal(context.Background(), api.ReturnOriginal{CommandID: fmt.Sprintf("return-6-%d", serial), TenantID: f.tenant,
			ActorID: "doc-1", DocumentID: "DOC-6", ExpectedRevision: int64(3 + i), Serial: serial, Evidence: "handed in at the Shanghai counter",
			OccurredAt: f.hours(5 + i)}); err != nil {
			t.Fatalf("returned original %d: %v", serial, err)
		}
	}
	if _, err := f.module.OpenDocumentAmendment(context.Background(), api.OpenDocumentAmendment{CommandID: "amend-6", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-6", ExpectedRevision: 5, Reason: "the goods description", OccurredAt: f.hours(7)}); err != nil {
		t.Fatalf("the amendment opened: %v", err)
	}
	if _, err := f.module.ApproveTransportDocument(context.Background(), api.ApproveTransportDocument{CommandID: "approve-amend-6", TenantID: f.tenant,
		ActorID: "doc-2", DocumentID: "DOC-6", ExpectedRevision: 6, OccurredAt: f.hours(8)}); err != nil {
		t.Fatalf("the amendment approved: %v", err)
	}
	err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		// version 1 is still the last issued one: the order trigger takes the record
		if _, err := e.Exec(ctx, `INSERT INTO cargo_doc.original (tenant_id,document_id,version_no,serial,record_no,status,actor,at,evidence,entered_at)
            VALUES ($1,'DOC-6',1,1,3,'ISSUED','x',now(),'x',now())`, f.tenant); err != nil {
			return err
		}
		// DOC issues the amendment in its own transaction while this one is open
		issueCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if got, err := f.module.IssueDocumentAmendment(issueCtx, api.IssueDocumentAmendment{CommandID: "issue-amend-6", TenantID: f.tenant, ActorID: "doc-1",
			DocumentID: "DOC-6", ExpectedRevision: 7, CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(9)}); err != nil || got.NewRevision != 8 {
			return fmt.Errorf("the amendment issued: %+v %v", got, err)
		}
		_, err := e.Exec(ctx, `UPDATE cargo_doc.transport_document SET revision=revision+1 WHERE tenant_id=$1 AND document_id='DOC-6'`, f.tenant)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "transport_document_consistent" {
		t.Fatalf("the record beside the amendment: %v, want 23514 transport_document_consistent", err)
	}
	if d := f.get(t, "DOC-6"); d.State != api.StateIssued || d.CurrentVersion != 2 || d.Revision != 8 {
		t.Fatalf("the amended bill: %+v", d)
	}
	if status, records := f.originals(t, "DOC-6", 1); status != "1:RETURNED 2:RETURNED" || records != 4 {
		t.Fatalf("version 1: %s (%d records)", status, records)
	}
	if status, records := f.originals(t, "DOC-6", 2); status != "1:ISSUED 2:ISSUED" || records != 2 {
		t.Fatalf("version 2: %s (%d records)", status, records)
	}
}

// A return is replayed: the same command again answers the same receipt and writes nothing; the same command id for
// another serial is an idempotency conflict; a new command at a stale revision is a conflict, a surrender too.
func TestReturnOriginalIsReplayedOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newSeaFixture(t, 1)
	f.issueSea(t, "DOC-7", api.KindHBL, 1, api.ReleaseOriginals, 2) // revision 3, business time at hour 4
	back := api.ReturnOriginal{CommandID: "return-7-1", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-7", ExpectedRevision: 3, Serial: 1,
		Evidence: "handed in at the Shanghai counter", OccurredAt: f.hours(5)}
	first, err := f.module.ReturnOriginal(ctx, back)
	if err != nil || first.NewRevision != 4 {
		t.Fatalf("returned: %+v %v", first, err)
	}
	if again, err := f.module.ReturnOriginal(ctx, back); err != nil || again != first {
		t.Fatalf("the same command again: %+v %v", again, err)
	}
	other := back
	other.Serial = 2
	if _, err = f.module.ReturnOriginal(ctx, other); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the same command id for another serial: %v", err)
	}
	other.CommandID = "return-7-2"
	if _, err = f.module.ReturnOriginal(ctx, other); !errors.Is(err, api.ErrConflict) || errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("a new command at a stale revision: %v", err)
	}
	if _, err = f.module.SurrenderOriginals(ctx, api.SurrenderOriginals{CommandID: "telex-7", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-7",
		ExpectedRevision: 3, Evidence: "telex release instruction TLX-886", OccurredAt: f.hours(6)}); !errors.Is(err, api.ErrConflict) || errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("a surrender at a stale revision: %v", err)
	}
	// the replay and the refusals wrote nothing
	if status, records := f.originals(t, "DOC-7", 1); status != "1:RETURNED 2:ISSUED" || records != 3 || f.get(t, "DOC-7").Revision != 4 {
		t.Fatalf("after the replays: %s (%d records)", status, records)
	}
	other.ExpectedRevision = 4
	if got, err := f.module.ReturnOriginal(ctx, other); err != nil || got.NewRevision != 5 {
		t.Fatalf("the other original at the revision: %+v %v", got, err)
	}
}

// A draft's release and number of originals change by a refresh, and an issued bill's by an amendment: a draft
// EXPRESS bill whose order's consignee becomes to order is refreshed as ORIGINALS of three, then approved and issued
// with three originals; a refresh with a number alone changes the count; an issued ORIGINALS bill amended to EXPRESS
// issues none; a number on an EXPRESS draft with no release naming ORIGINALS is refused.
func TestReleaseAndOriginalsChangeOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newSeaFixture(t, 4)
	refresh := func(id string, revision int64, release string, originals, hour int) error {
		_, err := f.module.RefreshDocumentDraft(ctx, api.RefreshDocumentDraft{CommandID: fmt.Sprintf("refresh-%s-%d-%s-%d", id, revision, release, originals),
			TenantID: f.tenant, ActorID: "doc-1", DocumentID: id, ExpectedRevision: revision, Reason: "the order changed", Release: release, Originals: originals,
			OccurredAt: f.hours(hour)})
		return err
	}
	latest := func(id string) api.DocumentVersion {
		t.Helper()
		v, err := f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: id})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	// an EXPRESS draft, then the order's consignee becomes to order: only ORIGINALS carries it
	if _, err := f.module.DraftTransportDocument(ctx, seaDraft(f, "DOC-X", api.KindHBL, 1, api.ReleaseExpress, 0)); err != nil {
		t.Fatal(err)
	}
	f.toOrder("O-1", "TO ORDER OF BANK OF CHINA", true)
	if err := refresh("DOC-X", 1, "", 0, 3); !refused(err, "DOC_INCOMPLETE") {
		t.Fatalf("refreshed as EXPRESS to a consignee to order: %v", err)
	}
	if err := refresh("DOC-X", 1, api.ReleaseOriginals, 3, 3); err != nil {
		t.Fatalf("refreshed as ORIGINALS of three: %v", err)
	}
	if _, err := f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "approve-DOC-X", TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: "DOC-X", ExpectedRevision: 2, OccurredAt: f.hours(4)}); err != nil {
		t.Fatal(err)
	}
	f.w.mu.Lock()
	f.w.orders["O-1"] = locked(f.w.orders["O-1"], "DOC-X")
	f.w.mu.Unlock()
	if _, err := f.module.IssueTransportDocument(ctx, api.IssueTransportDocument{CommandID: "issue-DOC-X", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-X",
		ExpectedRevision: 3, CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(5)}); err != nil {
		t.Fatal(err)
	}
	if v := latest("DOC-X"); v.Version != 2 || v.Doc.ReleaseType != api.ReleaseOriginals || v.Doc.Originals != 3 || v.Doc.Consignee.ToOrder != "TO ORDER OF BANK OF CHINA" {
		t.Fatalf("the bill to order: %+v", v)
	}
	if status, records := f.originals(t, "DOC-X", 2); status != "1:ISSUED 2:ISSUED 3:ISSUED" || records != 3 {
		t.Fatalf("issued: %s (%d records)", status, records)
	}

	// a number alone changes the count of a draft
	if _, err := f.module.DraftTransportDocument(ctx, seaDraft(f, "DOC-Y", api.KindHBL, 2, api.ReleaseOriginals, 3)); err != nil {
		t.Fatal(err)
	}
	if err := refresh("DOC-Y", 1, "", 2, 3); err != nil {
		t.Fatalf("refreshed with two originals: %v", err)
	}
	if v := latest("DOC-Y"); v.Version != 2 || v.Doc.ReleaseType != api.ReleaseOriginals || v.Doc.Originals != 2 {
		t.Fatalf("the draft with two originals: %+v", v)
	}

	// an issued ORIGINALS bill, its originals returned, amended to EXPRESS: the new version issues none
	f.issueSea(t, "DOC-Z", api.KindHBL, 3, api.ReleaseOriginals, 2)
	for i, serial := range []int{1, 2} {
		if _, err := f.module.ReturnOriginal(ctx, api.ReturnOriginal{CommandID: fmt.Sprintf("return-Z-%d", serial), TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-Z",
			ExpectedRevision: int64(3 + i), Serial: serial, Evidence: "handed in at the Shanghai counter", OccurredAt: f.hours(5 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.module.OpenDocumentAmendment(ctx, api.OpenDocumentAmendment{CommandID: "amend-Z", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-Z",
		ExpectedRevision: 5, Reason: "released as EXPRESS", Release: api.ReleaseExpress, OccurredAt: f.hours(7)}); err != nil {
		t.Fatalf("amended to EXPRESS: %v", err)
	}
	if _, err := f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "approve-amend-Z", TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: "DOC-Z", ExpectedRevision: 6, OccurredAt: f.hours(8)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.IssueDocumentAmendment(ctx, api.IssueDocumentAmendment{CommandID: "issue-amend-Z", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-Z",
		ExpectedRevision: 7, CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(9)}); err != nil {
		t.Fatal(err)
	}
	if d, v := f.get(t, "DOC-Z"), latest("DOC-Z"); d.State != api.StateIssued || d.CurrentVersion != 2 || v.Doc.ReleaseType != api.ReleaseExpress || v.Doc.Originals != 0 {
		t.Fatalf("the bill amended to EXPRESS: %+v %+v", d, v)
	}
	if status, records := f.originals(t, "DOC-Z", 1); status != "1:RETURNED 2:RETURNED" || records != 4 {
		t.Fatalf("version 1: %s (%d records)", status, records)
	}
	if status, records := f.originals(t, "DOC-Z", 2); status != "" || records != 0 {
		t.Fatalf("version 2 of an EXPRESS bill: %s (%d records)", status, records)
	}

	// a number on an EXPRESS draft, with no release that asks for originals
	if _, err := f.module.DraftTransportDocument(ctx, seaDraft(f, "DOC-W", api.KindHBL, 4, api.ReleaseExpress, 0)); err != nil {
		t.Fatal(err)
	}
	if err := refresh("DOC-W", 1, "", 2, 3); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("two originals on an EXPRESS draft: %v", err)
	}
}

// Originals belong to an HBL and their count is a count: a version written past DOC whose content has the member
// originals without being released as ORIGINALS, or is released as ORIGINALS without a count of one digit from 1 to 9,
// or is not an HBL's, is refused by the table of versions itself.
func TestOriginalsOfOtherKindsAndCountsPastDOCOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newSeaFixture(t, 2)
	f.issueSea(t, "DOC-E", api.KindHBL, 1, api.ReleaseExpress, 0)
	if _, err := f.module.DraftTransportDocument(ctx, seaDraft(f, "DOC-W", api.KindHouseSeaWaybill, 2, "", 0)); err != nil {
		t.Fatalf("a house sea waybill drafted: %v", err)
	}
	const (
		touch = `UPDATE cargo_doc.transport_document SET revision=revision+1 WHERE tenant_id=$1 AND document_id='%s'`
		// a second version that is the first with a part of its content replaced, and the digest of the result
		version = `INSERT INTO cargo_doc.document_version (tenant_id,document_id,version_no,content,content_sha256,source_movement_id,source_version,
            reason,drafted_by,drafted_at,entered_at)
            SELECT tenant_id,document_id,2,c,encode(sha256(convert_to(c,'UTF8')),'hex'),source_movement_id,source_version,'past DOC',drafted_by,now(),now()
            FROM (SELECT v.*, replace(content,'%[2]s','%[3]s') AS c FROM cargo_doc.document_version v
              WHERE tenant_id=$1 AND document_id='%[1]s' AND version_no=1) x`
		express   = `"release_type":"EXPRESS"`
		waybill   = `"release_type":"SEA_WAYBILL"`
		originals = `"release_type":"ORIGINALS"`
	)
	for name, tc := range map[string]struct{ doc, from, to string }{
		"an EXPRESS bill made ORIGINALS of 12":           {"DOC-E", express, originals + `,"originals":12`},
		"an EXPRESS bill made ORIGINALS of 0":            {"DOC-E", express, originals + `,"originals":0`},
		"an EXPRESS bill made ORIGINALS of abc":          {"DOC-E", express, originals + `,"originals":"abc"`},
		"an EXPRESS bill made ORIGINALS of the text 3":   {"DOC-E", express, originals + `,"originals":"3"`},
		"an EXPRESS bill made ORIGINALS without a count": {"DOC-E", express, originals},
		"an EXPRESS bill that counts 2 originals":        {"DOC-E", express, express + `,"originals":2`},
		"a house sea waybill made ORIGINALS of 2":        {"DOC-W", waybill, originals + `,"originals":2`},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, sql := range []string{fmt.Sprintf(touch, tc.doc), fmt.Sprintf(version, tc.doc, tc.from, tc.to)} {
				if _, err := e.Exec(ctx, sql, f.tenant); err != nil {
					return err
				}
			}
			return nil
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "document_version_originals" {
			t.Errorf("%s: %v, want 23514 document_version_originals", name, err)
		}
		t.Logf("%s refused: %v", name, err)
	}
	// nothing was written
	if d := f.get(t, "DOC-E"); len(d.Versions) != 1 || d.Revision != 3 {
		t.Fatalf("after the refused writes: %+v", d)
	}
	if d := f.get(t, "DOC-W"); len(d.Versions) != 1 || d.Revision != 1 {
		t.Fatalf("after the refused writes: %+v", d)
	}
}
