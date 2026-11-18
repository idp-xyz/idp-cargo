//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// seaContacts gives a sea order the contacts the world gives O-A: its shipper is ORG's shipper-O-A with a contact
// consenting to be provided and to go abroad, its consignee, when it is a named party, an order contact consenting to be
// provided only; the customer is customer-O-A. A consignee to order has no contact.
func (w *world) seaContacts(order string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	o := w.orders[order]
	o.CustomerPartyID, o.Contacts = "customer-O-A", comapi.ContactsStatus{State: comapi.ContactsInUse}
	for i, p := range o.Parties {
		switch {
		case p.Role == comapi.PartyShipper:
			o.Parties[i] = view(comapi.PartyShipper, "shipper-O-A", "CN", 1)
			o.Parties[i].HasContact = true
			o.Parties[i].Contact = &comapi.FrozenContact{Source: comapi.FromORG, ORGContactID: "C-SH", ORGVersion: 2,
				Contact: comapi.Contact{Name: "张三", Mobile: "+86 138 0000 0000", Email: "zhang@example.cn"},
				Consent: &comapi.ContactConsent{NoticeVersion: "PI-2026-01", ProvideToOthers: true, CrossBorder: true, Basis: comapi.ConsentSubjectConfirmed,
					EvidenceRef: "consent/C-SH", ORGRevision: 2}}
		case p.Role == comapi.PartyConsignee && p.Kind != comapi.KindToOrder:
			o.Parties[i].HasContact = true
			o.Parties[i].Contact = &comapi.FrozenContact{Source: comapi.FromOrder, Contact: comapi.Contact{Name: "John Receiver", Phone: "+1 718 555 0100"},
				Consent: &comapi.ContactConsent{NoticeVersion: "PI-2026-01", ProvideToOthers: true, Basis: comapi.ConsentCustomerUndertaking,
					EvidenceRef: "order/" + order}}
		}
	}
	w.orders[order] = o
}

// A container on the master bill's facts, so that the bills show one.
func (f *fixture) seaContainer() {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	facts := f.w.ocean["OM-R"]
	facts.Containers = []seaapi.DocContainer{{VisitID: "CV-1", Number: "CSQU3054383", SizeType: "22G1", SealNumber: "CN-SEAL-1"}}
	facts.Houses[0].Containers = []seaapi.DocBreakdown{{VisitID: "CV-1", ContainerNumber: "CSQU3054383", Packs: 4, GrossWeightGrams: 100000, VolumeCubicCentimetres: 500000}}
	f.w.ocean["OM-R"] = facts
}

func (f *fixture) render(t *testing.T, id string) api.RenderedDocument {
	t.Helper()
	r, err := f.module.RenderDocumentVersion(context.Background(), api.RenderDocumentVersion{TenantID: f.tenant, DocumentID: id})
	if err != nil {
		t.Fatalf("render %s: %v", id, err)
	}
	return r
}

// A house bill of lading is previewed as a draft and rendered again once issued; a house sea waybill likewise.
func TestAHouseBillIsRenderedOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newSeaFixture(t, 2)
	f.seaAuthority(t, api.KindHouseSeaWaybill)
	f.seaContainer()
	if _, err := f.module.DraftTransportDocument(ctx, seaDraft(f, "DOC-1", api.KindHBL, 1, api.ReleaseOriginals, 3)); err != nil {
		t.Fatal(err)
	}
	preview := f.render(t, "DOC-1")
	if preview.Status != api.VersionDraft || preview.Template != api.TemplateHBL || !bytes.HasPrefix(preview.PDF, []byte("%PDF-1.4")) ||
		!bytes.Contains(preview.PDF, []byte("(DRAFT - NOT VALID)")) || !bytes.Contains(preview.PDF, []byte("(BILL OF LADING)")) ||
		!bytes.Contains(preview.PDF, []byte("(Not issued)")) || !bytes.Contains(preview.PDF, []byte("(COPY - NOT NEGOTIABLE. Original bills of lading to be issued: 3)")) {
		t.Fatalf("draft preview: %s %s", preview.Status, preview.Template)
	}
	if _, err := f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "approve-1", TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: "DOC-1", ExpectedRevision: 1, OccurredAt: f.hours(3)}); err != nil {
		t.Fatal(err)
	}
	f.w.mu.Lock()
	f.w.orders["O-1"] = locked(f.w.orders["O-1"], "DOC-1")
	f.w.mu.Unlock()
	if _, err := f.module.IssueTransportDocument(ctx, api.IssueTransportDocument{CommandID: "issue-1", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-1",
		ExpectedRevision: 2, CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(4)}); err != nil {
		t.Fatal(err)
	}
	issued, again := f.render(t, "DOC-1"), f.render(t, "DOC-1")
	if issued.Status != api.VersionIssued || issued.Template != api.TemplateHBL || bytes.Contains(issued.PDF, []byte("NOT VALID")) || len(issued.SHA256) != 64 ||
		!bytes.Equal(issued.PDF, again.PDF) || issued.SHA256 != again.SHA256 || issued.SHA256 == preview.SHA256 {
		t.Fatalf("issued: %s %s %d", issued.Status, issued.Template, len(issued.SHA256))
	}
	for _, want := range []string{"(SHAR0000001)", "(Master bill of lading LINU2611000999)", "(EVER GIVEN 0456E)", "(CNSHG)", "(DEHAM)",
		"(CSQU3054383 22G1 seal CN-SEAL-1: 4 packages, 100 kg, 0.500 M3)", "(Issued on 2026-", "at CNSHG)", "template IDP-HBL-V1"} {
		if !bytes.Contains(issued.PDF, []byte(want)) {
			t.Errorf("the issued bill lacks %s", want)
		}
	}

	if _, err := f.module.DraftTransportDocument(ctx, seaDraft(f, "DOC-S", api.KindHouseSeaWaybill, 2, "", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "approve-s", TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: "DOC-S", ExpectedRevision: 1, OccurredAt: f.hours(3)}); err != nil {
		t.Fatal(err)
	}
	f.w.mu.Lock()
	f.w.orders["O-2"] = locked(f.w.orders["O-2"], "DOC-S")
	f.w.mu.Unlock()
	if p := f.render(t, "DOC-S"); p.Status != api.VersionApproved || p.Template != api.TemplateHBL || !bytes.Contains(p.PDF, []byte("(SEA WAYBILL)")) ||
		!bytes.Contains(p.PDF, []byte("(APPROVED - NOT VALID)")) {
		t.Fatalf("approved sea waybill: %s %s", p.Status, p.Template)
	}
	if _, err := f.module.IssueTransportDocument(ctx, api.IssueTransportDocument{CommandID: "issue-s", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-S",
		ExpectedRevision: 2, CreditOverrideReason: "the account is opened by hand", OccurredAt: f.hours(4)}); err != nil {
		t.Fatal(err)
	}
	w, wAgain := f.render(t, "DOC-S"), f.render(t, "DOC-S")
	if w.Status != api.VersionIssued || w.Template != api.TemplateHBL || bytes.Contains(w.PDF, []byte("NOT VALID")) || len(w.SHA256) != 64 ||
		!bytes.Equal(w.PDF, wAgain.PDF) || !bytes.Contains(w.PDF, []byte("(SEA WAYBILL)")) ||
		!bytes.Contains(w.PDF, []byte("(NOT NEGOTIABLE. Delivery to the named consignee.)")) || bytes.Contains(w.PDF, []byte("Original bills of lading")) {
		t.Fatalf("issued sea waybill: %s %s", w.Status, w.Template)
	}
}

// An issued HBL is dispatched as a HAWB is: contacts of its first order, a copy to the ordering party and one to a
// recipient abroad with only the consenting contact, sent once ORG holds the reservation. An HBL to order carries no
// consignee contact.
func TestAHouseBillIsDispatchedOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newSeaFixture(t, 3)
	f.seaAuthority(t, api.KindHouseSeaWaybill)
	f.w.reserved, f.w.crossNow = map[string][]string{}, true
	f.toOrder("O-2", "TO ORDER OF BANK OF CHINA", true)
	f.w.seaContacts("O-1")
	f.w.seaContacts("O-2")
	f.w.seaContacts("O-3")
	f.issueSea(t, "DOC-1", api.KindHBL, 1, api.ReleaseOriginals, 3)
	f.issueSea(t, "DOC-2", api.KindHBL, 2, api.ReleaseOriginals, 3)
	f.issueSea(t, "DOC-3", api.KindHouseSeaWaybill, 3, "", 0)
	prepare := func(document, dispatch, recipient, role string) {
		t.Helper()
		if _, err := f.module.PrepareDocumentDispatch(ctx, api.PrepareDocumentDispatch{CommandID: "cmd-" + dispatch, TenantID: f.tenant, ActorID: "doc-1",
			DispatchID: dispatch, DocumentID: document, RecipientParty: recipient, RecipientRole: role, Channel: api.ChannelEmailPDF, OccurredAt: f.hours(5)}); err != nil {
			t.Fatalf("dispatch %s: %v", dispatch, err)
		}
	}
	get := func(dispatch string) api.DocumentDispatch {
		t.Helper()
		d, err := f.module.GetDispatch(ctx, api.GetDispatch{TenantID: f.tenant, DispatchID: dispatch})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	role := func(d api.DocumentDispatch, role string) *api.DispatchContact {
		for i, c := range d.Contacts {
			if c.PartyRole == role {
				return &d.Contacts[i]
			}
		}
		return nil
	}
	sum := func(pdf []byte) string { s := sha256.Sum256(pdf); return hex.EncodeToString(s[:]) }

	// to the ordering party: the shipper's contact is provided to it, the consignee's comes back as it gave it
	prepare("DOC-1", "DSP-1", "customer-O-A", api.RecipientOrderingParty)
	d1 := get("DSP-1")
	s1, c1 := role(d1, comapi.PartyShipper), role(d1, comapi.PartyConsignee)
	if d1.Kind != api.KindHBL || d1.Template != api.TemplateHBL || d1.VersionStatus != api.VersionIssued || s1 == nil || s1.Classification != api.ContactProvision ||
		s1.Status != api.ContactIncluded || c1 == nil || c1.Classification != api.ContactNotProvided || c1.Status != api.ContactIncluded {
		t.Fatalf("the ordering party's dispatch: %+v", d1)
	}
	copy1, err := f.module.GetDispatchCopy(ctx, api.GetDispatchCopy{TenantID: f.tenant, ActorID: "doc-1", DispatchID: "DSP-1"})
	if err != nil || sum(copy1.PDF) != d1.CopySHA256 || copy1.Dispatch.Template != api.TemplateHBL || !bytes.Contains(copy1.PDF, []byte("<5F204E09>")) ||
		!bytes.Contains(copy1.PDF, []byte("(CONSIGNEE: John Receiver, +1 718 555 0100)")) || !bytes.Contains(copy1.PDF, []byte("(BILL OF LADING)")) {
		t.Fatalf("the ordering party's copy: %v", err)
	}

	// to the recipient abroad: only the contact that consents to go abroad
	prepare("DOC-1", "DSP-2", "agent-ny", api.RecipientDestinationAgent)
	d2 := get("DSP-2")
	s2, c2 := role(d2, comapi.PartyShipper), role(d2, comapi.PartyConsignee)
	if d2.Template != api.TemplateHBL || s2 == nil || s2.Classification != api.ContactCrossBorder || s2.Status != api.ContactIncluded ||
		c2 == nil || c2.Status != api.ContactOmitted || c2.OmitReason != api.OmitNoTransferConsent || len(d2.CrossBorderKeys) != 1 {
		t.Fatalf("the agent's dispatch: %+v", d2)
	}
	if _, err = f.module.GetDispatchCopy(ctx, api.GetDispatchCopy{TenantID: f.tenant, ActorID: "doc-1", DispatchID: "DSP-2"}); err == nil ||
		!strings.Contains(err.Error(), "DOC_TRANSFER_NOT_RESERVED") {
		t.Fatalf("a copy abroad before ORG holds its contacts: %v", err)
	}
	outcome := func(outcome string, revision int64) error {
		_, err := f.module.RecordDispatchOutcome(ctx, api.RecordDispatchOutcome{CommandID: "out-" + outcome, TenantID: f.tenant, ActorID: "doc-1", DispatchID: "DSP-2",
			ExpectedRevision: revision, Outcome: outcome, Reference: "smtp:250 queued", OccurredAt: f.hours(6)})
		return err
	}
	if err = outcome(api.DispatchSent, 1); err == nil || !strings.Contains(err.Error(), "DOC_TRANSFER_NOT_RESERVED") {
		t.Fatalf("sent before ORG holds the reservation: %v", err)
	}
	f.w.mu.Lock()
	f.w.reserved["DSP-2"] = d2.CrossBorderKeys
	f.w.mu.Unlock()
	copy2, err := f.module.GetDispatchCopy(ctx, api.GetDispatchCopy{TenantID: f.tenant, ActorID: "doc-1", DispatchID: "DSP-2"})
	if err != nil || sum(copy2.PDF) != d2.CopySHA256 || !bytes.Contains(copy2.PDF, []byte("<5F204E09>")) || bytes.Contains(copy2.PDF, []byte("John Receiver")) {
		t.Fatalf("the agent's copy: %v", err)
	}
	if err = outcome(api.DispatchSent, 1); err != nil {
		t.Fatalf("sent: %v", err)
	}
	if d := get("DSP-2"); d.State != api.DispatchSent {
		t.Fatalf("outcome: %s", d.State)
	}

	// an HBL to order: no consignee contact, to the ordering party or any other
	prepare("DOC-2", "DSP-3", "customer-O-A", api.RecipientOrderingParty)
	d3 := get("DSP-3")
	if role(d3, comapi.PartyConsignee) != nil || role(d3, comapi.PartyShipper) == nil || len(d3.Contacts) != 1 {
		t.Fatalf("an HBL to order has a consignee contact: %+v", d3.Contacts)
	}
	copy3, err := f.module.GetDispatchCopy(ctx, api.GetDispatchCopy{TenantID: f.tenant, ActorID: "doc-1", DispatchID: "DSP-3"})
	if err != nil || !bytes.Contains(copy3.PDF, []byte("(TO ORDER OF BANK OF CHINA)")) || bytes.Contains(copy3.PDF, []byte("CONSIGNEE:")) ||
		bytes.Contains(copy3.PDF, []byte("John Receiver")) {
		t.Fatalf("the copy of a bill to order: %v", err)
	}

	// a house sea waybill to the ordering party: its own kind, the same template, the contacts of its order
	prepare("DOC-3", "DSP-4", "customer-O-A", api.RecipientOrderingParty)
	d4 := get("DSP-4")
	if d4.Kind != api.KindHouseSeaWaybill || d4.Template != api.TemplateHBL || d4.VersionStatus != api.VersionIssued || role(d4, comapi.PartyShipper) == nil ||
		role(d4, comapi.PartyConsignee) == nil {
		t.Fatalf("the sea waybill's dispatch: %+v", d4)
	}
	copy4, err := f.module.GetDispatchCopy(ctx, api.GetDispatchCopy{TenantID: f.tenant, ActorID: "doc-1", DispatchID: "DSP-4"})
	if err != nil || sum(copy4.PDF) != d4.CopySHA256 || !bytes.Contains(copy4.PDF, []byte("(SEA WAYBILL)")) || !bytes.Contains(copy4.PDF, []byte("<5F204E09>")) ||
		!bytes.Contains(copy4.PDF, []byte("(CONSIGNEE: John Receiver, +1 718 555 0100)")) {
		t.Fatalf("the sea waybill's copy: %v", err)
	}

	// the provision records are the dispatches', with identifiers only
	period := api.ListContactProvisions{TenantID: f.tenant, ActorID: "privacy-officer", From: f.hours(0), Until: f.hours(24)}
	got, err := f.module.ListContactProvisions(ctx, period)
	if err != nil || len(got) != 4 || got[1].DispatchID != "DSP-2" || got[1].State != api.DispatchSent {
		t.Fatalf("provision records: %+v %v", got, err)
	}
	if _, err = f.module.GetDispatchCopy(ctx, api.GetDispatchCopy{TenantID: f.tenant, ActorID: "doc-1", DispatchID: "DSP-X"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown dispatch: %v", err)
	}
}
