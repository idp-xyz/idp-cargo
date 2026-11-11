package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func consent(provide, cross bool) *Consent {
	return &Consent{NoticeVersion: "PI-2026-01", ProvideToOthers: provide, CrossBorder: cross, Basis: "SUBJECT_CONFIRMED", EvidenceRef: "evidence/1", ORGRevision: 2}
}

func sources() []ContactSource {
	return []ContactSource{
		{Key: "ORG:C-SHIPPER", PartyRole: "SHIPPER", OrderID: "O-A", PartyVersion: 1, Consent: consent(true, true), Name: "张三"},
		{Key: "COM:O-A:CONSIGNEE:1", PartyRole: "CONSIGNEE", OrderID: "O-A", PartyVersion: 1, Consent: consent(true, false), Name: "John Doe"},
		{Key: "ORG:C-NOTIFY", PartyRole: "NOTIFY_PARTY", OrderID: "O-A", PartyVersion: 1, Name: "No consent"},
		{Key: "COM:O-A:SHIPPER:9", PartyRole: "SHIPPER", OrderID: "O-A", PartyVersion: 9, Erased: true},
	}
}

// Decide's matrix: the ordering party gets every contact but an erased one; a party in mainland China those consenting to
// be provided; one abroad those also consenting to go abroad, by the threshold exemption or the tenant's safeguard.
func TestDecideContactsForARecipient(t *testing.T) {
	type want struct{ class, path, status, reason string }
	for name, tc := range map[string]struct {
		r        Recipient
		register RegisterView
		want     []want
	}{
		"ordering party": {Recipient{PartyID: "customer", Country: "CN", Role: RecipientOrderingParty}, RegisterView{State: "OK"}, []want{
			{NotProvided, PathNone, ContactIncluded, ""}, {NotProvided, PathNone, ContactIncluded, ""}, {NotProvided, PathNone, ContactIncluded, ""},
			{NotProvided, PathNone, ContactOmitted, OmitErased}}},
		"carrier in China": {Recipient{PartyID: "airline", Country: "CN", Role: RecipientCarrier}, RegisterView{State: "OK"}, []want{
			{Provision, PathConsent, ContactIncluded, ""}, {Provision, PathConsent, ContactIncluded, ""}, {Provision, PathNone, ContactOmitted, OmitNoConsent},
			{Provision, PathNone, ContactOmitted, OmitErased}}},
		"agent abroad": {Recipient{PartyID: "agent-ny", Country: "US", Role: RecipientDestinationAgent}, RegisterView{State: "WARNING"}, []want{
			{CrossBorder, PathThresholdExempt, ContactIncluded, ""}, {CrossBorder, PathNone, ContactOmitted, OmitNoTransferConsent},
			{CrossBorder, PathNone, ContactOmitted, OmitNoConsent}, {CrossBorder, PathNone, ContactOmitted, OmitErased}}},
		"Hong Kong under a standard contract": {Recipient{PartyID: "agent-hk", Country: "HK", Role: RecipientDestinationAgent},
			RegisterView{State: "WARNING", SafeguardKind: "STANDARD_CONTRACT"}, []want{
				{CrossBorder, PathStandardContract, ContactIncluded, ""}, {CrossBorder, PathNone, ContactOmitted, OmitNoTransferConsent},
				{CrossBorder, PathNone, ContactOmitted, OmitNoConsent}, {CrossBorder, PathNone, ContactOmitted, OmitErased}}},
	} {
		got, err := Decide(tc.r, "customer", sources(), tc.register)
		if err != nil || len(got) != len(tc.want) {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
		for i, w := range tc.want {
			g := got[i]
			if g.No != i+1 || g.Classification != w.class || g.LegalPath != w.path || g.Status != w.status || g.OmitReason != w.reason || g.Key != sources()[i].Key {
				t.Errorf("%s contact %d: %+v, want %+v", name, i+1, g, w)
			}
		}
	}
	// Blocked: refused only when a contact would go abroad.
	if _, err := Decide(Recipient{PartyID: "agent-ny", Country: "US"}, "customer", sources(), RegisterView{State: "BLOCKED"}); !errors.Is(err, ErrTransferBlocked) {
		t.Fatalf("blocked register: %v", err)
	}
	if got, err := Decide(Recipient{PartyID: "agent-ny", Country: "US"}, "customer", sources()[1:], RegisterView{State: "BLOCKED"}); err != nil || len(got) != 3 {
		t.Fatalf("blocked register, nobody going abroad: %+v %v", got, err)
	}
	if got, err := Decide(Recipient{PartyID: "carrier", Country: "CN"}, "customer", sources(), RegisterView{State: "BLOCKED"}); err != nil || got[0].Status != ContactIncluded {
		t.Fatalf("blocked register, a recipient in China: %+v %v", got, err)
	}
}

func hour(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }

func issuedHouse(t *testing.T) *Document {
	t.Helper()
	d, err := DraftDocument("T", "DOC-H1", hawb(t, 1, orders()), "doc-1", hour(0))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Approve("doc-2", hour(1)); err != nil {
		t.Fatal(err)
	}
	return d
}

func dispatchState(d *Document, role, party, country string, contacts []DispatchContact) DispatchState {
	return DispatchState{DispatchID: "DSP-1", DocumentID: d.State().DocumentID, Version: 1, VersionStatus: d.Status(1), Recipient: Recipient{PartyID: party,
		Country: country, Role: role}, OrderingParty: "customer", Channel: ChannelEmailPDF, Contacts: contacts, CopySHA256: strings.Repeat("c", 64),
		Template: "IDP-AWB-V1", RegisterYear: 2026, RegisterState: "OK"}
}

// A dispatch: drafts to the ordering party only; an issued copy to anyone; SENT and UNKNOWN only with ORG's reservation; FAILED
// final; UNKNOWN followed by SENT.
func TestDispatchLifecycle(t *testing.T) {
	d := issuedHouse(t)
	decided, err := Decide(Recipient{PartyID: "agent-ny", Country: "US"}, "customer", sources(), RegisterView{State: "OK"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = PrepareDispatch(dispatchState(d, RecipientDestinationAgent, "agent-ny", "US", decided), d, "doc-1", hour(2)); !errors.Is(err, ErrNotDispatchable) {
		t.Fatalf("an approved draft to the agent: %v", err)
	}
	if _, err = PrepareDispatch(dispatchState(d, RecipientOrderingParty, "someone-else", "CN", nil), d, "doc-1", hour(2)); !errors.Is(err, ErrInvalidDispatch) {
		t.Fatalf("a draft to someone claiming to be the ordering party: %v", err)
	}
	stale := dispatchState(d, RecipientOrderingParty, "customer", "CN", nil)
	stale.VersionStatus = VersionDraft
	if _, err = PrepareDispatch(stale, d, "doc-1", hour(2)); !errors.Is(err, ErrConflict) {
		t.Fatalf("rendered as a draft, approved since: %v", err)
	}
	if x, err := PrepareDispatch(dispatchState(d, RecipientOrderingParty, "customer", "CN", nil), d, "doc-1", hour(2)); err != nil || x.Current() != DispatchPrepared {
		t.Fatalf("a draft to the ordering party for approval: %v", err)
	}
	gate := IssueGate{Facts: facts(1), Orders: orders("DOC-H1"), Credit: CreditDecision{Status: "NONE"}, OverrideReason: "FIN not in service",
		IssuerVerified: true, AuthorityID: "AUTH-H", AuthorityVersion: 1}
	if err = d.Issue(gate, "doc-2", hour(3)); err != nil {
		t.Fatal(err)
	}
	x, err := PrepareDispatch(dispatchState(d, RecipientDestinationAgent, "agent-ny", "US", decided), d, "doc-1", hour(4))
	if err != nil {
		t.Fatal(err)
	}
	if keys := x.CrossBorderKeys(); len(keys) != 1 || keys[0] != "ORG:C-SHIPPER" {
		t.Fatalf("cross-border keys %v", keys)
	}
	if err = x.RecordOutcome(DispatchSent, "smtp:250 queued", nil, "doc-1", hour(5)); !errors.Is(err, ErrTransferNotReserved) {
		t.Fatalf("sent before ORG's reservation: %v", err)
	}
	if err = x.RecordOutcome(DispatchSent, "", []string{"ORG:C-SHIPPER"}, "doc-1", hour(5)); !errors.Is(err, ErrInvalidDispatch) {
		t.Fatalf("sent without the channel's reference: %v", err)
	}
	if err = x.RecordOutcome(DispatchUnknown, "", []string{"ORG:C-SHIPPER"}, "doc-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	if err = x.RecordOutcome(DispatchUnknown, "", []string{"ORG:C-SHIPPER"}, "doc-1", hour(5)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unknown twice: %v", err)
	}
	if err = x.RecordOutcome(DispatchSent, "smtp:250 queued", []string{"ORG:C-SHIPPER", "ORG:OTHER"}, "doc-1", hour(6)); err != nil || x.Current() != DispatchSent {
		t.Fatalf("sent after unknown: %v", err)
	}
	if err = x.RecordOutcome(DispatchFailed, "bounce", nil, "doc-1", hour(7)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("an outcome after sent: %v", err)
	}
	var types []string
	for _, e := range x.Pending() {
		if e.DocumentID != "DSP-1" || e.DocumentRef != "DOC-H1" || e.Version != 1 {
			t.Fatalf("event %+v", e)
		}
		types = append(types, e.Type)
	}
	if strings.Join(types, ",") != "DocumentDispatchPrepared.v1,DocumentDispatchOutcomeUnknown.v1,DocumentDispatched.v1" {
		t.Fatalf("events %v", types)
	}
	back, err := RestoreDispatch(x.State())
	if err != nil || back.Current() != DispatchSent || len(back.AddedOutcomes()) != 0 {
		t.Fatalf("restored: %v", err)
	}
	failed, _ := PrepareDispatch(dispatchState(d, RecipientDestinationAgent, "agent-ny", "US", decided), d, "doc-1", hour(4))
	if err = failed.RecordOutcome(DispatchFailed, "smtp:550 mailbox unavailable", nil, "doc-1", hour(5)); err != nil || failed.Current() != DispatchFailed {
		t.Fatalf("failed without a reservation: %v", err)
	}
	if err = failed.RecordOutcome(DispatchSent, "retry", []string{"ORG:C-SHIPPER"}, "doc-1", hour(6)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("failed is final: %v", err)
	}
}
