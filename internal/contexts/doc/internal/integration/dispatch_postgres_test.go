//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
)

// countries is the registration country of the parties dispatches go to.
var countries = map[string]string{"customer-O-A": "CN", "agent-ny": "US", "agent-hk": "HK", "carrier-cn": "CN"}

func (w *world) GetPartyIdentity(_ context.Context, q orgapi.GetPartyIdentity) (orgapi.PartyIdentity, error) {
	c, ok := countries[q.PartyID]
	if !ok {
		return orgapi.PartyIdentity{}, fmt.Errorf("%w: stub", orgapi.ErrNotFound)
	}
	return orgapi.PartyIdentity{PartyID: q.PartyID, Revision: 1, Status: "ACTIVE", LegalName: "Party " + q.PartyID, CountryCode: c}, nil
}

// GetContactTransferRegister answers from the register state and the reservations the test sets.
func (w *world) GetContactTransferRegister(_ context.Context, q orgapi.GetContactTransferRegister) (orgapi.ContactTransferRegister, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := orgapi.ContactTransferRegister{Year: 2026, Revision: 3, Persons: 79_000, State: orgapi.TransferOK}
	if w.register != "" {
		r.State = w.register
	}
	if w.safeguard != "" {
		r.Safeguard = &orgapi.TransferSafeguard{SafeguardID: "SG-1", Kind: w.safeguard, Reference: "CAC filing 2026-001"}
	}
	r.DispatchKeys = append([]string(nil), w.reserved[q.DispatchID]...)
	return r, nil
}

// contacts gives O-A's shipper an ORG contact consenting to both and its consignee an order contact consenting to be
// provided only; the customer is customer-O-A.
func (w *world) contacts(state string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	o := w.orders["O-A"]
	o.CustomerPartyID, o.Contacts = "customer-O-A", comapi.ContactsStatus{State: state}
	for i, p := range o.Parties {
		o.Parties[i].HasContact = true
		switch p.Role {
		case comapi.PartyShipper:
			o.Parties[i].Contact = &comapi.FrozenContact{Source: comapi.FromORG, ORGContactID: "C-SH", ORGVersion: 2,
				Contact: comapi.Contact{Name: "张三", Mobile: "+86 138 0000 0000", Email: "zhang@example.cn"},
				Consent: &comapi.ContactConsent{NoticeVersion: "PI-2026-01", ProvideToOthers: true, CrossBorder: true, Basis: comapi.ConsentSubjectConfirmed,
					EvidenceRef: "consent/C-SH", ORGRevision: 2}}
		case comapi.PartyConsignee:
			o.Parties[i].Contact = &comapi.FrozenContact{Source: comapi.FromOrder, Contact: comapi.Contact{Name: "John Receiver", Phone: "+1 718 555 0100"},
				Consent: &comapi.ContactConsent{NoticeVersion: "PI-2026-01", ProvideToOthers: true, Basis: comapi.ConsentCustomerUndertaking,
					EvidenceRef: "order/O-A"}}
		}
		if state != comapi.ContactsInUse {
			o.Parties[i].Contact = nil
		}
	}
	w.orders["O-A"] = o
}

// A HAWB copied to the ordering party as a draft, then issued and dispatched to the destination agent abroad with
// only the consenting contact, sent once ORG holds the reservation; the register's block; a copy refused once a contact
// is gone; the provision records; and what the database refuses past DOC.
func TestDocumentDispatchOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.reserved = map[string][]string{}
	f.authority(t, "AUTH-H", api.KindHAWB, "", 0)
	f.w.finalize(1, true, 318_000)
	f.w.order("O-A", 1)
	f.w.contacts(comapi.ContactsInUse)
	if _, err := f.module.DraftTransportDocument(ctx, api.DraftTransportDocument{CommandID: "draft-h1", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-H1",
		Kind: api.KindHAWB, MovementID: "AM-1", ShipmentID: "SH-A", PaymentIndicator: api.PaymentPrepaid, OccurredAt: f.hours(1)}); err != nil {
		t.Fatal(err)
	}
	preview, err := f.module.RenderDocumentVersion(ctx, api.RenderDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-H1"})
	if err != nil || preview.Status != api.VersionDraft || preview.Template != api.TemplateAWB || !bytes.HasPrefix(preview.PDF, []byte("%PDF-1.4")) ||
		!bytes.Contains(preview.PDF, []byte("(DRAFT - NOT VALID)")) || bytes.Contains(preview.PDF, []byte("<5F204E09>")) || len(preview.SHA256) != 64 {
		t.Fatalf("draft preview: %+v %v", preview.Status, err)
	}
	prepare := func(command, dispatch, recipient, role string, version int, h int) (api.Receipt, error) {
		return f.module.PrepareDocumentDispatch(ctx, api.PrepareDocumentDispatch{CommandID: command, TenantID: f.tenant, ActorID: "doc-1", DispatchID: dispatch,
			DocumentID: "DOC-H1", Version: version, RecipientParty: recipient, RecipientRole: role, Channel: api.ChannelEmailPDF, OccurredAt: f.hours(h)})
	}
	if _, err = prepare("dsp-draft-agent", "DSP-0", "agent-ny", api.RecipientDestinationAgent, 0, 2); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a draft to the agent: %v", err)
	}
	if _, err = prepare("dsp-fake-customer", "DSP-0", "agent-ny", api.RecipientOrderingParty, 0, 2); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("the agent claiming to be the ordering party: %v", err)
	}
	r, err := prepare("dsp-draft", "DSP-1", "customer-O-A", api.RecipientOrderingParty, 0, 2)
	if err != nil || r.DocumentID != "DSP-1" || r.Version != 1 || r.NewRevision != 1 {
		t.Fatalf("a draft to the ordering party: %+v %v", r, err)
	}
	if again, err := prepare("dsp-draft", "DSP-1", "customer-O-A", api.RecipientOrderingParty, 0, 2); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	d1, err := f.module.GetDispatch(ctx, api.GetDispatch{TenantID: f.tenant, DispatchID: "DSP-1"})
	if err != nil || d1.State != api.DispatchPrepared || d1.VersionStatus != api.VersionDraft || len(d1.Contacts) != 2 || d1.Contacts[0].Classification != api.ContactNotProvided ||
		d1.Contacts[0].Status != api.ContactIncluded || d1.Contacts[0].Key != "ORG:C-SH" || d1.Contacts[1].Key != "COM:O-A:CONSIGNEE:1" ||
		len(d1.CrossBorderKeys) != 0 || d1.Recipient.Country != "CN" || d1.OrderingParty != "customer-O-A" {
		t.Fatalf("draft dispatch: %+v %v", d1, err)
	}
	copy1, err := f.module.GetDispatchCopy(ctx, api.GetDispatchCopy{TenantID: f.tenant, ActorID: "doc-1", DispatchID: "DSP-1"})
	if err != nil || !bytes.Contains(copy1.PDF, []byte("<5F204E09>")) || !bytes.Contains(copy1.PDF, []byte("(CONSIGNEE: John Receiver, +1 718 555 0100)")) ||
		!bytes.Contains(copy1.PDF, []byte("(DRAFT - NOT VALID)")) {
		t.Fatalf("the ordering party's copy has every contact: %v", err)
	}

	// approve and issue; the draft copy still renders as it was sent
	if _, err = f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "approve-h1", TenantID: f.tenant, ActorID: "doc-2", DocumentID: "DOC-H1",
		ExpectedRevision: 1, OccurredAt: f.hours(3)}); err != nil {
		t.Fatal(err)
	}
	f.w.order("O-A", 1, "DOC-H1")
	f.w.contacts(comapi.ContactsInUse)
	if _, err = f.module.IssueTransportDocument(ctx, api.IssueTransportDocument{CommandID: "issue-h1", TenantID: f.tenant, ActorID: "doc-2", DocumentID: "DOC-H1",
		ExpectedRevision: 2, CreditOverrideReason: "FIN not in service", OccurredAt: f.hours(4)}); err != nil {
		t.Fatal(err)
	}
	if again, err := f.module.GetDispatchCopy(ctx, api.GetDispatchCopy{TenantID: f.tenant, ActorID: "doc-1", DispatchID: "DSP-1"}); err != nil ||
		!bytes.Equal(again.PDF, copy1.PDF) {
		t.Fatalf("the draft copy after issue: %v", err)
	}
	if r, err = prepare("dsp-agent", "DSP-2", "agent-ny", api.RecipientDestinationAgent, 0, 5); err != nil || r.Version != 1 {
		t.Fatalf("issued copy to the agent: %+v %v", r, err)
	}
	d2, err := f.module.GetDispatch(ctx, api.GetDispatch{TenantID: f.tenant, DispatchID: "DSP-2"})
	if err != nil || d2.VersionStatus != api.VersionIssued || d2.Recipient.Country != "US" || d2.Contacts[0].Classification != api.ContactCrossBorder ||
		d2.Contacts[0].LegalPath != api.PathThresholdExempt || d2.Contacts[0].Status != api.ContactIncluded || d2.Contacts[0].Consent == nil ||
		d2.Contacts[0].Consent.ORGRevision != 2 || d2.Contacts[1].Status != api.ContactOmitted || d2.Contacts[1].OmitReason != api.OmitNoTransferConsent ||
		!slices.Equal(d2.CrossBorderKeys, []string{"ORG:C-SH"}) || d2.RegisterYear != 2026 || d2.RegisterState != orgapi.TransferOK {
		t.Fatalf("agent dispatch: %+v %v", d2, err)
	}
	copy2, err := f.module.GetDispatchCopy(ctx, api.GetDispatchCopy{TenantID: f.tenant, ActorID: "doc-1", DispatchID: "DSP-2"})
	if err != nil || !bytes.Contains(copy2.PDF, []byte("<5F204E09>")) || bytes.Contains(copy2.PDF, []byte("John Receiver")) ||
		bytes.Contains(copy2.PDF, []byte("NOT VALID")) || copy2.Dispatch.CopySHA256 != d2.CopySHA256 {
		t.Fatalf("the agent's copy carries the consenting contact only: %v", err)
	}
	outcome := func(command, dispatch, outcome, reference string, revision int64, h int) (api.Receipt, error) {
		return f.module.RecordDispatchOutcome(ctx, api.RecordDispatchOutcome{CommandID: command, TenantID: f.tenant, ActorID: "doc-1", DispatchID: dispatch,
			ExpectedRevision: revision, Outcome: outcome, Reference: reference, OccurredAt: f.hours(h)})
	}
	if _, err = outcome("sent-early", "DSP-2", api.DispatchSent, "smtp:250 2.0.0 queued", 1, 6); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "DOC_TRANSFER_NOT_RESERVED") {
		t.Fatalf("sent before ORG holds the reservation: %v", err)
	}
	f.w.mu.Lock()
	f.w.reserved["DSP-2"] = []string{"ORG:C-SH"}
	f.w.mu.Unlock()
	if _, err = outcome("unknown-2", "DSP-2", api.DispatchUnknown, "", 1, 6); err != nil {
		t.Fatal(err)
	}
	if r, err = outcome("sent-2", "DSP-2", api.DispatchSent, "smtp:250 2.0.0 queued", 2, 7); err != nil || r.NewRevision != 3 {
		t.Fatalf("sent: %+v %v", r, err)
	}
	if _, err = outcome("failed-2", "DSP-2", api.DispatchFailed, "bounce", 3, 8); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an outcome after sent: %v", err)
	}

	// the register blocked: nobody's contact goes abroad; a carrier in China still gets the consenting contacts
	f.w.mu.Lock()
	f.w.register = orgapi.TransferBlocked
	f.w.mu.Unlock()
	if _, err = prepare("dsp-blocked", "DSP-3", "agent-hk", api.RecipientDestinationAgent, 0, 9); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "DOC_TRANSFER_BLOCKED") {
		t.Fatalf("Hong Kong while the register is blocked: %v", err)
	}
	if _, err = prepare("dsp-carrier", "DSP-4", "carrier-cn", api.RecipientCarrier, 1, 9); err != nil {
		t.Fatal(err)
	}
	d4, _ := f.module.GetDispatch(ctx, api.GetDispatch{TenantID: f.tenant, DispatchID: "DSP-4"})
	if d4.Contacts[0].Classification != api.ContactProvision || d4.Contacts[0].LegalPath != api.PathConsent || d4.Contacts[1].Status != api.ContactIncluded ||
		d4.RegisterState != orgapi.TransferBlocked {
		t.Fatalf("carrier dispatch: %+v", d4.Contacts)
	}
	if _, err = outcome("failed-4", "DSP-4", api.DispatchFailed, "smtp:550 5.1.1 unknown mailbox", 1, 10); err != nil {
		t.Fatalf("failed: %v", err)
	}
	f.w.mu.Lock()
	f.w.register, f.w.safeguard = orgapi.TransferOK, orgapi.SafeguardStandardContract
	f.w.mu.Unlock()
	if _, err = prepare("dsp-hk", "DSP-5", "agent-hk", api.RecipientDestinationAgent, 0, 11); err != nil {
		t.Fatal(err)
	}
	if d5, _ := f.module.GetDispatch(ctx, api.GetDispatch{TenantID: f.tenant, DispatchID: "DSP-5"}); d5.Contacts[0].LegalPath != api.PathStandardContract {
		t.Fatalf("under the tenant's standard contract: %+v", d5.Contacts[0])
	}

	// the order closed: its contacts are restricted, the copies can no longer be made
	f.w.contacts(comapi.ContactsRestricted)
	if _, err = f.module.GetDispatchCopy(ctx, api.GetDispatchCopy{TenantID: f.tenant, ActorID: "doc-1", DispatchID: "DSP-2"}); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "DOC_COPY_CHANGED") {
		t.Fatalf("a copy after the contacts were restricted: %v", err)
	}
	if _, err = prepare("dsp-closed", "DSP-6", "agent-ny", api.RecipientDestinationAgent, 0, 12); err != nil {
		t.Fatal(err)
	}
	if d6, _ := f.module.GetDispatch(ctx, api.GetDispatch{TenantID: f.tenant, DispatchID: "DSP-6"}); d6.Contacts[0].OmitReason != api.OmitErased ||
		d6.Contacts[0].Key != "COM:O-A:SHIPPER:1" || len(d6.CrossBorderKeys) != 0 {
		t.Fatalf("contacts of a closed order are left off: %+v", d6.Contacts)
	}

	// lists and the provision records
	if list, err := f.module.ListDocumentDispatches(ctx, api.ListDocumentDispatches{TenantID: f.tenant, DocumentID: "DOC-H1"}); err != nil || len(list) != 5 {
		t.Fatalf("dispatches of DOC-H1: %d %v", len(list), err)
	}
	period := api.ListContactProvisions{TenantID: f.tenant, ActorID: "intruder", From: f.hours(0), Until: f.hours(24)}
	if _, err = f.module.ListContactProvisions(ctx, period); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("provision records to someone not allowed: %v", err)
	}
	period.ActorID = "privacy-officer"
	if got, err := f.module.ListContactProvisions(ctx, period); err != nil || len(got) != 5 || got[1].DispatchID != "DSP-2" || got[1].State != api.DispatchSent {
		t.Fatalf("provision records: %v", err)
	}
	want := []string{"IssuanceAuthorityRecorded", "TransportDocumentDrafted", "DocumentDispatchPrepared", "TransportDocumentApproved", "TransportDocumentIssued",
		"DocumentDispatchPrepared", "DocumentDispatchOutcomeUnknown", "DocumentDispatched", "DocumentDispatchPrepared", "DocumentDispatchFailed",
		"DocumentDispatchPrepared", "DocumentDispatchPrepared"}
	if got := f.outboxTypes(t); !slices.Equal(got, want) {
		t.Fatalf("outbox %v", got)
	}

	// no name, phone or e-mail in DOC's tables or events
	var leaks int
	if err = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT (SELECT count(*) FROM cargo_doc.document_dispatch x WHERE x.tenant_id=$1 AND to_jsonb(x)::text ~ '张三|John|5550100|example')
            + (SELECT count(*) FROM cargo_doc.dispatch_contact c WHERE c.tenant_id=$1 AND to_jsonb(c)::text ~ '张三|John|5550100|example')
            + (SELECT count(*) FROM bento.outbox o WHERE o.scope=$1 AND o.payload::text ~ '张三|John|5550100|example')`, f.tenant).Scan(&leaks)
	}); err != nil || leaks != 0 {
		t.Fatalf("personal data in DOC's records: %d %v", leaks, err)
	}

	// past DOC on the application role
	for name, tc := range map[string]struct {
		sql   string
		state string
		check string // the constraint that refuses it, when it matters which
	}{
		"contact rewritten": {`UPDATE cargo_doc.dispatch_contact SET status='INCLUDED',omit_reason=NULL WHERE tenant_id=$1 AND dispatch_id='DSP-2' AND contact_no=2`, "42501|23001", ""},
		"outcome rewritten": {`UPDATE cargo_doc.dispatch_outcome SET outcome='FAILED' WHERE tenant_id=$1`, "42501|23001", ""},
		"recipient changed": {`UPDATE cargo_doc.document_dispatch SET recipient_party_id='agent-hk' WHERE tenant_id=$1 AND dispatch_id='DSP-6'`, "42501|23001", ""},
		"sent reopened":     {`UPDATE cargo_doc.document_dispatch SET state='PREPARED' WHERE tenant_id=$1 AND dispatch_id='DSP-2'`, "23001", ""},
		"state behind DOC":  {`UPDATE cargo_doc.document_dispatch SET state='SENT' WHERE tenant_id=$1 AND dispatch_id='DSP-6'`, "23514", "document_dispatch_consistent"},
		"contact added late": {`INSERT INTO cargo_doc.dispatch_contact (tenant_id,dispatch_id,contact_no,contact_key,party_role,order_id,classification,legal_path,status)
            VALUES ($1,'DSP-6',9,'ORG:C-X','SHIPPER','O-A','NOT_PROVIDED','NONE','INCLUDED')`, "23514", "dispatch_records_with_dispatch"},
		"abroad without consent": {`INSERT INTO cargo_doc.dispatch_contact (tenant_id,dispatch_id,contact_no,contact_key,party_role,order_id,classification,legal_path,status)
            VALUES ($1,'DSP-6',9,'ORG:C-X','SHIPPER','O-A','CROSS_BORDER','CONSENT_WITH_THRESHOLD_EXEMPTION','INCLUDED')`, "23514", "dispatch_contact_path"},
		"draft to the agent": {`INSERT INTO cargo_doc.document_dispatch (tenant_id,dispatch_id,document_id,version_no,kind,version_status,recipient_party_id,recipient_country,
            recipient_role,channel,copy_sha256,template,register_year,register_state,state,prepared_by,prepared_at,entered_at) VALUES ($1,'DSP-X','DOC-H1',1,'HAWB',
            'DRAFT','agent-ny','US','DESTINATION_AGENT','EMAIL_PDF',repeat('a',64),'IDP-AWB-V1',2026,'OK','PREPARED','x',now(),now())`, "23514", "document_dispatch_draft"},
		"dispatch deleted": {`DELETE FROM cargo_doc.document_dispatch WHERE tenant_id=$1 AND dispatch_id='DSP-6'`, "42501|23001", ""},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		var pgErr *pgconn.PgError
		if state := sqlState(err); state == "" || !strings.Contains(tc.state, state) || tc.check != "" && (!errors.As(err, &pgErr) || pgErr.ConstraintName != tc.check) {
			t.Errorf("%s: %v (state %s), want %s %s", name, err, state, tc.state, tc.check)
		}
		t.Logf("%s refused: %v", name, err)
	}
}
