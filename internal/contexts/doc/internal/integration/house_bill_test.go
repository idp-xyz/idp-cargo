//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// An HBL is drafted from SEA's finalized OceanDocFacts, approved and issued once those facts state an on-board
// date, under a simulated issuance authority. The number is the HBL SEA drew. A consignee to order and the
// originals are not in this slice.
func TestAHouseBillOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.mu.Lock()
	f.w.ocean["OM-1"] = seaapi.OceanDocFacts{MovementID: "OM-1", Version: 1, Status: seaapi.DocFactsFinal, SHA256: "ab" + repeat('c', 62),
		ConsolidationID: "C-1", OperatingLegalEntityID: "LE", CarrierPartyID: "line-1", MasterBillNumber: "LINU2611000457", MasterBillKind: "CARRIER_MBL",
		Vessel: "EVER GIVEN", Voyage: "0123W", PlaceOfReceipt: "CNSHA", PortOfLoading: "CNSHG", PortOfDischarge: "DEHAM", LocationEdition: "E1",
		Houses: []seaapi.DocHouse{{ShipmentID: "SH-L", HBL: "SHAL0000001", Packs: 6, PackageType: "CT", GrossWeightGrams: 312000,
			VolumeCubicCentimetres: 1_000_000, OrderIDs: []string{"O-L"}, GoodsDescription: "Cartons of electronics",
			Containers: []seaapi.DocBreakdown{{VisitID: "CV-1", ContainerNumber: "CSQU3054383", Packs: 6, GrossWeightGrams: 312000, VolumeCubicCentimetres: 1_000_000}}}},
		Containers: []seaapi.DocContainer{{VisitID: "CV-1", Number: "CSQU3054383", SizeType: "22G1", SealNumber: "CN-SEAL-1"}}}
	o := comapi.OrderParties{OrderID: "O-L", Revision: 4, CustomerPartyID: "customer-O-L", Transport: comapi.Transport{Mode: comapi.ModeSea},
		Parties: []comapi.OrderPartyView{view(comapi.PartyShipper, "shipper-O-L", "CN", 1), view(comapi.PartyConsignee, "consignee-O-L", "DE", 1)}}
	f.w.orders["O-L"] = o
	f.w.mu.Unlock()
	if _, err := f.module.RecordIssuanceAuthority(ctx, api.RecordIssuanceAuthority{CommandID: "auth-s", TenantID: f.tenant, ActorID: "legal-1",
		AuthorityID: "AUTH-S", LegalEntityID: "LE", Kind: api.KindHBL, IssuePlace: "CNSHG", Qualification: "simulated NVOCC filing",
		Validity: kernel.ValidityInterval{From: f.at}, EvidenceRef: "evidence/AUTH-S", OccurredAt: f.at}); err != nil {
		t.Fatal(err)
	}
	draft := api.DraftTransportDocument{CommandID: "draft-l", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-L", Kind: api.KindHBL,
		MovementID: "OM-1", ShipmentID: "SH-L", PaymentIndicator: api.PaymentPrepaid, OccurredAt: f.hours(1)}
	got, err := f.module.DraftTransportDocument(ctx, draft)
	if err != nil || got.NewRevision != 1 {
		t.Fatalf("draft: %+v %v", got, err)
	}
	doc, err := f.module.GetTransportDocument(ctx, api.GetTransportDocument{TenantID: f.tenant, DocumentID: "DOC-L"})
	if err != nil || doc.Number != "SHAL0000001" || doc.Kind != api.KindHBL {
		t.Fatalf("the draft: %+v %v", doc, err)
	}
	if _, err = f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "ap-l", TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: "DOC-L", ExpectedRevision: 1, OccurredAt: f.hours(2)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.IssueTransportDocument(ctx, api.IssueTransportDocument{CommandID: "is-l", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-L", ExpectedRevision: 2, CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(3)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("issued before the on-board date: %v", err)
	}
	f.w.mu.Lock()
	facts := f.w.ocean["OM-1"]
	facts.Version, facts.OnBoardAt = 2, f.hours(4)
	facts.SHA256 = "cd" + repeat('e', 62)
	f.w.ocean["OM-1"] = facts
	f.w.orders["O-L"] = locked(o, "DOC-L")
	f.w.mu.Unlock()
	if _, err = f.module.RefreshDocumentDraft(ctx, api.RefreshDocumentDraft{CommandID: "re-l", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-L",
		ExpectedRevision: 2, Reason: "the on-board date", OccurredAt: f.hours(5)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "ap-l2", TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: "DOC-L", ExpectedRevision: 3, OccurredAt: f.hours(6)}); err != nil {
		t.Fatal(err)
	}
	issued, err := f.module.IssueTransportDocument(ctx, api.IssueTransportDocument{CommandID: "is-l2", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-L", ExpectedRevision: 4, CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(7)})
	if err != nil || issued.NewRevision != 5 {
		t.Fatalf("issued: %+v %v", issued, err)
	}
	issuedVersion, err := f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-L"})
	if err != nil || issuedVersion.Doc.IssuerCapacity != "NVOCC_CARRIER" || issuedVersion.Doc.ReleaseType != "EXPRESS" ||
		issuedVersion.Doc.MasterBill != "LINU2611000457" || issuedVersion.Doc.Vessel != "EVER GIVEN" || issuedVersion.Doc.PortOfLoading != "CNSHG" ||
		!issuedVersion.Doc.OnBoardAt.Equal(f.hours(4)) || len(issuedVersion.Doc.Containers) != 1 || issuedVersion.Doc.Containers[0].SealNumber != "CN-SEAL-1" ||
		issuedVersion.Doc.Source.Contract != "OceanDocFacts v1" {
		t.Fatalf("the issued HBL: %+v %v", issuedVersion.Doc, err)
	}

	// An amendment after the published facts move, then a cancellation. The number stays with the shipment: its next
	// HBL issues it again, and another shipment cannot take it.
	f.w.mu.Lock()
	facts = f.w.ocean["OM-1"]
	facts.Version, facts.SHA256 = 3, "ef"+repeat('0', 62)
	facts.Houses[0].GoodsDescription = "Cartons of machinery"
	f.w.ocean["OM-1"] = facts
	f.w.mu.Unlock()
	if _, err = f.module.OpenDocumentAmendment(ctx, api.OpenDocumentAmendment{CommandID: "am-l", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-L",
		ExpectedRevision: 5, Reason: "the goods description", OccurredAt: f.hours(8)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "ap-am", TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: "DOC-L", ExpectedRevision: 6, OccurredAt: f.hours(9)}); err != nil {
		t.Fatal(err)
	}
	amended, err := f.module.IssueDocumentAmendment(ctx, api.IssueDocumentAmendment{CommandID: "is-am", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-L", ExpectedRevision: 7, CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(10)})
	if err != nil || amended.Version != 3 {
		t.Fatalf("the amendment: %+v %v", amended, err)
	}
	if v, err := f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-L", Version: 3}); err != nil ||
		v.Doc.Cargo.Lines[0].GoodsDescription != "Cartons of machinery" || v.Status != api.VersionIssued {
		t.Fatalf("the amended version: %+v %v", v.Doc.Cargo, err)
	}
	if _, err = f.module.CancelTransportDocument(ctx, api.CancelTransportDocument{CommandID: "cancel-l", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-L", ExpectedRevision: 8, Reason: "issued on the wrong consignee by mistake", OccurredAt: f.hours(11)}); err != nil {
		t.Fatal(err)
	}
	if d := f.get(t, "DOC-L"); d.State != api.StateCancelled || d.CurrentVersion != 0 {
		t.Fatalf("cancelled: %+v", d)
	}
	again := draft
	again.CommandID, again.DocumentID, again.OccurredAt = "draft-l2", "DOC-L2", f.hours(12)
	f.w.mu.Lock()
	f.w.orders["O-L"] = locked(o, "DOC-L2")
	f.w.mu.Unlock()
	if _, err = f.module.DraftTransportDocument(ctx, again); err != nil {
		t.Fatalf("the shipment's next HBL: %v", err)
	}
	if _, err = f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "ap-l3", TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: "DOC-L2", ExpectedRevision: 1, OccurredAt: f.hours(12)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.IssueTransportDocument(ctx, api.IssueTransportDocument{CommandID: "is-l3", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-L2", ExpectedRevision: 2, CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(12)}); err != nil {
		t.Fatalf("the shipment's number again: %v", err)
	}
	f.w.mu.Lock()
	taken := facts
	taken.Houses[0].ShipmentID = "SH-X"
	f.w.ocean["OM-1"] = taken
	ox := o
	ox.OrderID = "O-X"
	f.w.orders["O-X"] = ox
	f.w.mu.Unlock()
	if _, err = f.module.DraftTransportDocument(ctx, api.DraftTransportDocument{CommandID: "draft-x", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-X", Kind: api.KindHBL, MovementID: "OM-1", ShipmentID: "SH-X", PaymentIndicator: api.PaymentPrepaid, OccurredAt: f.hours(13)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("another shipment takes the number: %v", err)
	}
}

func repeat(r byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = r
	}
	return string(b)
}

func locked(o comapi.OrderParties, holder string) comapi.OrderParties {
	o.Locks = append(o.Locks, comapi.PartyLock{HolderID: holder, LockedBy: "doc-1"})
	return o
}
