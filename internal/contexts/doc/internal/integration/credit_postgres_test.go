//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// A HAWB is issued on FIN's credit decision (CreditStatus v1, C7): a hold refuses it even with an override reason; an
// account without a hold issues it without one; a debtor without an account still needs the override.
func TestHouseIssueAsksFINForCredit(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.credit = map[string]string{"customer-O-A": finapi.CreditOnHold}
	f.authority(t, "AUTH-H", api.KindHAWB, "", 0)
	f.w.finalize(1, true, 318_000)
	f.w.order("O-A", 1, "DOC-H1")
	if _, err := f.module.DraftTransportDocument(ctx, api.DraftTransportDocument{CommandID: "draft", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-H1",
		Kind: api.KindHAWB, MovementID: "AM-1", ShipmentID: "SH-A", PaymentIndicator: api.PaymentPrepaid, OccurredAt: f.hours(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "approve", TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: "DOC-H1", ExpectedRevision: 1, OccurredAt: f.hours(2)}); err != nil {
		t.Fatal(err)
	}
	issue := func(command, actor, reason string) error {
		_, err := f.module.IssueTransportDocument(ctx, api.IssueTransportDocument{CommandID: command, TenantID: f.tenant, ActorID: actor, DocumentID: "DOC-H1",
			ExpectedRevision: 2, CreditOverrideReason: reason, OccurredAt: f.hours(3)})
		return err
	}
	if err := issue("issue-held", "doc-2", "the customer pays cash"); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "DOC_CREDIT_HOLD") {
		t.Fatalf("a hold refuses even with an override: %v", err)
	}
	if err := issue("issue-held-junior", "doc-junior", "the customer pays cash"); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "DOC_CREDIT_HOLD") {
		t.Fatalf("a hold, not the override permission, refuses an actor who may not override: %v", err)
	}
	f.w.mu.Lock()
	f.w.credit["customer-O-A"] = finapi.CreditOK
	f.w.mu.Unlock()
	if err := issue("issue-ok", "doc-2", ""); err != nil {
		t.Fatalf("issued on FIN's decision: %v", err)
	}
	d := f.get(t, "DOC-H1")
	if v := d.Versions[0]; v.CreditBasis != api.CreditOK || v.CreditOverrideReason != "" {
		t.Fatalf("credit basis %+v", v)
	}
	var decision string
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT credit_decision_id FROM cargo_doc.document_version_issue WHERE tenant_id=$1 AND document_id='DOC-H1'`, f.tenant).Scan(&decision)
	}); err != nil || decision != "LE/customer-O-A@3" {
		t.Fatalf("decision %q %v", decision, err)
	}
}
