//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/foundation/numbering"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func kernelCurrency(s string) kernel.CurrencyCode { return kernel.CurrencyCode(s) }

// billing tracks the policy's and the job's revisions through a test.
type billing struct {
	*fixture
	policy int64
	job    int64
}

func (b *billing) date(h int) string { return b.hours(h).UTC().Format(time.DateOnly) }

func (b *billing) rate(t *testing.T, id, kind string, rate int64) {
	t.Helper()
	r, err := b.module.RecordExchangeRate(context.Background(), api.RecordExchangeRate{CommandID: id, TenantID: b.tenant, ActorID: "acct-1",
		LegalEntityID: "LE", ExpectedRevision: b.policy, RateType: kind, From: "USD", Rate: rate, Scale: 4, FromDigits: 2, ToDigits: 2,
		ValidFrom: b.at.AddDate(0, 0, -1), ValidUntil: b.at.AddDate(0, 1, 0), Source: kind + " source", OccurredAt: b.hours(0)})
	if err != nil {
		t.Fatal(err)
	}
	b.policy = r.NewRevision
}

// policyChange takes the receipt of a policy command: policyChange(t)(b.tax(...)).
func (b *billing) policyChange(t *testing.T) func(api.Receipt, error) {
	return func(r api.Receipt, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		b.policy = r.NewRevision
	}
}

func (b *billing) tax(id, code, treatment string, rateBP int, from string) (api.Receipt, error) {
	return b.module.SetChargeTaxTreatment(context.Background(), api.SetChargeTaxTreatment{CommandID: id, TenantID: b.tenant, ActorID: "tax-1",
		LegalEntityID: "LE", ExpectedRevision: b.policy, ChargeCode: code, Treatment: treatment, RateBP: rateBP, EffectiveFrom: from, OccurredAt: b.hours(0)})
}

func (b *billing) series(id, kind, template string, first int64) (api.Receipt, error) {
	return b.module.RegisterInvoiceSeries(context.Background(), api.RegisterInvoiceSeries{CommandID: id, TenantID: b.tenant, ActorID: "acct-1",
		LegalEntityID: "LE", ExpectedRevision: b.policy, Kind: kind, Template: template, FirstNumber: first, OccurredAt: b.hours(0)})
}

func (b *billing) debit(id, job string, h int, debtor string, currency string, lines ...int) api.IssueDebitNote {
	return api.IssueDebitNote{CommandID: "issue-" + id, TenantID: b.tenant, ActorID: "bill-1", JobID: job, ExpectedRevision: b.job, InvoiceID: id,
		DebtorPartyID: debtor, LineNos: lines, Currency: kernelCurrency(currency), InvoiceDate: b.date(h), OccurredAt: b.hours(h)}
}

func (b *billing) issued(t *testing.T, cmd api.IssueDebitNote) api.Receipt {
	t.Helper()
	r, err := b.module.IssueDebitNote(context.Background(), cmd)
	if err != nil {
		t.Fatalf("%s: %v", cmd.InvoiceID, err)
	}
	b.job = r.NewRevision
	return r
}

func (b *billing) credit(id, debit string, h int, lines ...int) api.IssueCreditNote {
	return api.IssueCreditNote{CommandID: "credit-" + id, TenantID: b.tenant, ActorID: "bill-1", JobID: "JOB-A", ExpectedRevision: b.job, InvoiceID: id,
		DebitNoteID: debit, LineNos: lines, Reason: "billed in error", InvoiceDate: b.date(h), OccurredAt: b.hours(h)}
}

func (b *billing) profile(id, debtor, kind string, expected int) (api.Receipt, error) {
	return b.module.SetBillingProfile(context.Background(), api.SetBillingProfile{CommandID: id, TenantID: b.tenant, ActorID: "bill-1", DebtorPartyID: debtor,
		ExpectedVersion: expected, BuyerKind: kind, RegisteredAddress: "上海市浦东新区世纪大道100号", BankName: "中国银行上海分行", BankAccount: "4567 8901 2345",
		EvidenceRef: "billing details confirmed by the customer", OccurredAt: b.hours(0)})
}

func houseDocument(shipment, number string, issued time.Time) docapi.TransportDocumentSnapshot {
	return docapi.TransportDocumentSnapshot{DocumentID: "DOC-" + shipment, Kind: "HAWB", Version: 1, VersionStatus: docapi.VersionIssued, Number: number,
		MasterNumber: "781-12345675", ShipmentID: shipment, LegalEntityID: "LE", Origin: "CNSHA", Destination: "USJFK", IssuedAt: issued}
}

// ready gives the job of newBilling what a note needs besides the HAWB: the charge codes' tax treatments, the series
// DN{seq:6} and CN{seq:6}, and the billing profiles of party-a and agent-ny.
func (b *billing) ready(t *testing.T) {
	t.Helper()
	from := b.at.AddDate(0, 0, -5).Format(time.DateOnly)
	b.policyChange(t)(b.tax("tax-freight", "AIR_FREIGHT", api.TaxExemptIntlFreight, 0, from))
	b.policyChange(t)(b.tax("tax-doc", "DOC_FEE", api.TaxTaxable, 600, from))
	b.policyChange(t)(b.tax("tax-dest", "DEST_FEE", api.TaxPassThrough, 0, from))
	b.policyChange(t)(b.series("series-dn", api.KindDebitNote, "DN{seq:6}", 1))
	b.policyChange(t)(b.series("series-cn", api.KindCreditNote, "CN{seq:6}", 1))
	for debtor, kind := range map[string]string{"party-a": api.BuyerCNEnterprise, "agent-ny": api.BuyerForeign} {
		if _, err := b.profile("profile-"+debtor, debtor, kind, 0); err != nil {
			t.Fatal(err)
		}
	}
}

// rewrite changes one row of the tenant as the migrator role with the session's triggers off, to put a stored row into
// a shape FIN does not write; $1 is the tenant.
func (b *billing) rewrite(t *testing.T, sql string) {
	t.Helper()
	dsn := os.Getenv("IDP_CARGO_MIGRATOR_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("IDP_CARGO_MIGRATOR_DSN required in CI; skipping forbidden")
		}
		t.Skip("set IDP_CARGO_MIGRATOR_DSN to rewrite stored rows")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	tag, err := tx.Exec(ctx, sql, b.tenant)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("rewrite: %d rows, %v", tag.RowsAffected(), err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// newBilling opens the policy with the three USD rate types (ESTIMATE 7.1234, INVOICE 7.2000, ACCOUNTING 7.1000) and the
// job of SH-A priced from Q-1 (AIR_FREIGHT USD 1,200.00 and DOC_FEE CNY 300.00 for party-a) with a manual DEST_FEE of
// USD 200.00 for agent-ny.
func newBilling(t *testing.T) *billing {
	t.Helper()
	ctx := context.Background()
	b := &billing{fixture: newFixture(t)}
	if _, err := b.module.OpenFinancePolicy(ctx, api.OpenFinancePolicy{CommandID: "policy", TenantID: b.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: b.hours(0)}); err != nil {
		t.Fatal(err)
	}
	b.policy = 1
	b.rate(t, "rate-estimate", api.RateEstimate, 71_234)
	b.rate(t, "rate-invoice", api.RateInvoice, 72_000)
	b.rate(t, "rate-accounting", api.RateAccounting, 71_000)
	if _, err := b.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-a", TenantID: b.tenant, ActorID: "fin-1", JobID: "JOB-A", ShipmentID: "SH-A",
		OccurredAt: b.hours(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.module.ApplyPricingBasis(ctx, api.ApplyPricingBasis{CommandID: "apply-a", TenantID: b.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: 1, QuoteID: "Q-1", OrderID: "O-A", OccurredAt: b.hours(2)}); err != nil {
		t.Fatal(err)
	}
	r, err := b.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "line-dest", TenantID: b.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: 2, ChargeCode: "DEST_FEE", Sell: &api.ChargeSide{PartyID: "agent-ny", Amount: usd(20_000)}, Reason: "collect at destination",
		OccurredAt: b.hours(2)})
	if err != nil {
		t.Fatal(err)
	}
	b.job = r.NewRevision
	return b
}

// Debit notes bill a job's sides on the issued house document, under the debtor's billing profile, the charge codes'
// tax treatments and the legal entity's series; credit notes reverse them line by line; the billed sides cannot be
// revised or cancelled; exposure and open items follow; and the database refuses changes past FIN.
func TestDebitAndCreditNotesOnPostgres(t *testing.T) {
	ctx := context.Background()
	b := newBilling(t)
	rejected := func(name string, err error, code string) {
		t.Helper()
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), code) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	dn1 := b.debit("DN-1", "JOB-A", 5, "party-a", "CNY", 1, 2)
	_, err := b.module.IssueDebitNote(ctx, dn1)
	rejected("no issued house document", err, "FIN_NO_ISSUED_DOCUMENT")
	b.w.mu.Lock()
	b.w.documents["SH-A"] = houseDocument("SH-A", "SHA0000001", b.hours(6))
	b.w.mu.Unlock()
	_, err = b.module.IssueDebitNote(ctx, dn1)
	rejected("a HAWB issued after the note's time", err, "FIN_NO_ISSUED_DOCUMENT")
	b.w.mu.Lock()
	b.w.documents["SH-A"] = houseDocument("SH-A", "SHA0000001", b.hours(4))
	b.w.mu.Unlock()
	_, err = b.module.IssueDebitNote(ctx, dn1)
	rejected("no billing profile", err, "FIN_NO_BILLING_PROFILE")

	_, err = b.profile("profile-foreign", "party-a", api.BuyerForeign, 0)
	rejected("a party registered in CN as a foreign buyer", err, "Chinese enterprise")
	r, err := b.profile("profile-a", "party-a", api.BuyerCNEnterprise, 0)
	if err != nil || r.Version != 1 || r.SubjectID != "party-a" {
		t.Fatalf("profile: %+v %v", r, err)
	}
	if again, err := b.profile("profile-a", "party-a", api.BuyerCNEnterprise, 0); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err = b.module.SetBillingProfile(ctx, api.SetBillingProfile{CommandID: "profile-stale", TenantID: b.tenant, ActorID: "bill-1",
		DebtorPartyID: "party-a", ExpectedVersion: 0, BuyerKind: api.BuyerCNEnterprise, EvidenceRef: "x", OccurredAt: b.hours(0)}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a stale profile version: %v", err)
	}
	profile, err := b.module.GetBillingProfile(ctx, api.GetBillingProfile{TenantID: b.tenant, DebtorPartyID: "party-a"})
	if err != nil || len(profile.Versions) != 1 || profile.Versions[0].Title != "上海甲贸易有限公司" || profile.Versions[0].TaxpayerNumber != "91310000MA1FL8TQ2X" {
		t.Fatalf("profile read: %+v %v", profile, err)
	}

	_, err = b.module.IssueDebitNote(ctx, dn1)
	rejected("no tax treatment", err, "FIN_TAX_TREATMENT_MISSING")
	from := b.at.AddDate(0, 0, -5).Format(time.DateOnly)
	b.policyChange(t)(b.tax("tax-freight", "AIR_FREIGHT", api.TaxExemptIntlFreight, 0, from))
	b.policyChange(t)(b.tax("tax-doc", "DOC_FEE", api.TaxTaxable, 600, from))
	b.policyChange(t)(b.tax("tax-dest", "DEST_FEE", api.TaxPassThrough, 0, from))
	_, err = b.tax("tax-bad", "DOC_FEE", api.TaxTaxable, 0, b.date(48))
	rejected("taxable without a rate", err, "basis points")

	_, err = b.module.IssueDebitNote(ctx, dn1)
	rejected("no series", err, "FIN_NO_SERIES")
	for template, why := range map[string]string{"DN{rand:4}{seq:6}": "a random token", "dn{seq:6}": "lower case", "DN{seq:3}": "a narrow sequence",
		"DNDNDNDNDN{yyyy}{mm}{seq:10}": "over twenty characters", "DN{mm}{seq:5}": "a month without its year"} {
		_, err = b.series("series-bad", api.KindDebitNote, template, 1)
		rejected(why, err, "")
	}
	b.policyChange(t)(b.series("series-dn", api.KindDebitNote, "DN{seq:6}", 1))
	_, err = b.series("series-d", api.KindCreditNote, "D{seq:6}", 1)
	rejected("prefixes starting one another", err, "start one another")
	b.policyChange(t)(b.series("series-cn", api.KindCreditNote, "CN{seq:6}", 1))

	if _, err = b.module.SetCreditTerms(ctx, api.SetCreditTerms{CommandID: "terms-a", TenantID: b.tenant, ActorID: "credit-1", LegalEntityID: "LE",
		DebtorPartyID: "party-a", Limit: cny(100_000_000), PaymentDays: 30, PaymentBasis: api.TermsFromMonthEnd, OccurredAt: b.hours(0)}); err != nil {
		t.Fatal(err)
	}
	wrongDate := dn1
	wrongDate.CommandID, wrongDate.InvoiceDate = "issue-DN-1-date", b.date(72)
	_, err = b.module.IssueDebitNote(ctx, wrongDate)
	rejected("an invoice date that is not the issue's", err, "invoice date")
	r = b.issued(t, dn1)
	if r.Number != "DN000001" || r.SubjectID != "JOB-A" {
		t.Fatalf("debit note: %+v", r)
	}
	if again, err := b.module.IssueDebitNote(ctx, dn1); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	note, err := b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: "DN-1"})
	invoiceDay, _ := time.Parse(time.DateOnly, b.date(5))
	due := time.Date(invoiceDay.Year(), invoiceDay.Month()+1, 0, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 30).Format(time.DateOnly)
	// USD 1,200.00 at INVOICE 7.2000 = CNY 8,640.00, and CNY 300.00
	if err != nil || note.Number != "DN000001" || note.Total != cny(894_000) || note.LocalTotal != cny(894_000) || note.Open != cny(894_000) ||
		!note.Document.IssuedAt.Equal(b.hours(4)) ||
		note.DueDate != due || note.TermsVersion != 1 || note.Document.Number != "SHA0000001" || note.Document.MasterNumber != "781-12345675" ||
		note.BillingProfile.Title != "上海甲贸易有限公司" || len(note.Lines) != 2 || note.Lines[0].FX == nil || note.Lines[0].FX.RateType != api.RateInvoice ||
		len(note.TaxTotals) != 2 || note.TaxTotals[1].RateBP != 600 || note.ShipmentID != "SH-A" {
		t.Fatalf("debit note read: %+v %v", note, err)
	}
	job, err := b.module.GetFreightJob(ctx, api.GetFreightJob{TenantID: b.tenant, JobID: "JOB-A"})
	if err != nil || len(job.Notes) != 1 || job.Lines[0].Sell[1].Status != api.StatusInvoiced || job.Lines[0].Sell[1].InvoiceID != "DN-1" ||
		job.Lines[0].Sell[1].Local != cny(864_000) {
		t.Fatalf("job after the debit note: %+v %v", job, err)
	}
	account, err := b.module.GetCreditAccount(ctx, api.GetCreditAccount{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: "party-a"})
	if err != nil || account.Billed != cny(894_000) || account.Unbilled != cny(0) || account.Exposure != cny(894_000) {
		t.Fatalf("exposure: %+v %v", account, err)
	}
	_, err = b.module.ReviseChargeSide(ctx, api.ReviseChargeSide{CommandID: "revise-billed", TenantID: b.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: b.job, LineNo: 1, Side: api.SideSell, ExpectedSideVersion: 2, Value: api.ChargeSide{Amount: usd(1)}, Reason: "x", OccurredAt: b.hours(6)})
	rejected("revising a billed side", err, "FIN_INVOICED")
	_, err = b.module.CancelFreightJob(ctx, api.CancelFreightJob{CommandID: "cancel-billed", TenantID: b.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: b.job, Reason: "shipment cancelled", OccurredAt: b.hours(6)})
	rejected("cancelling a billed job", err, "FIN_INVOICED")
	stale := b.debit("DN-X", "JOB-A", 6, "party-a", "CNY", 1)
	stale.ExpectedRevision = b.job - 1
	if _, err = b.module.IssueDebitNote(ctx, stale); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a stale job revision: %v", err)
	}

	usdNote := b.debit("DN-2", "JOB-A", 6, "agent-ny", "USD", 3)
	_, err = b.module.IssueDebitNote(ctx, usdNote)
	rejected("the agent without a profile", err, "FIN_NO_BILLING_PROFILE")
	if _, err = b.profile("profile-ny", "agent-ny", api.BuyerForeign, 0); err != nil {
		t.Fatal(err)
	}
	if r = b.issued(t, usdNote); r.Number != "DN000002" {
		t.Fatalf("USD debit note: %+v", r)
	}
	if note, err = b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: "DN-2"}); err != nil || note.Total != usd(20_000) ||
		note.LocalTotal != cny(142_000) || note.Lines[0].FX.RateType != api.RateAccounting || note.DueDate != b.date(6) || note.TermsVersion != 0 ||
		note.BillingProfile.Title != "NY Forwarding Inc." || note.BillingProfile.TaxpayerNumber != "" {
		t.Fatalf("USD debit note read: %+v %v", note, err)
	}

	r, err = b.module.IssueCreditNote(ctx, b.credit("CN-1", "DN-1", 7, 2))
	if err != nil || r.Number != "CN000001" {
		t.Fatalf("credit note: %+v %v", r, err)
	}
	b.job = r.NewRevision
	_, err = b.module.IssueCreditNote(ctx, b.credit("CN-X", "DN-1", 7, 2))
	rejected("a line reversed twice", err, "reversed already")
	if note, err = b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: "DN-1"}); err != nil || note.State != api.NoteIssued ||
		note.Open != cny(864_000) || len(note.CreditNotes) != 1 {
		t.Fatalf("partly reversed: %+v %v", note, err)
	}
	if note, err = b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: "CN-1"}); err != nil || note.Kind != api.KindCreditNote ||
		note.Total != cny(-30_000) || note.CreditsInvoiceID != "DN-1" || note.Lines[0].CreditsLineNo != 2 || note.Reason != "billed in error" ||
		note.WriteOff {
		t.Fatalf("credit note read: %+v %v", note, err)
	}

	b.w.mu.Lock()
	renamed := b.w.identities["party-a"]
	renamed.LegalName, renamed.Revision = "上海甲国际贸易有限公司", 5
	b.w.identities["party-a"] = renamed
	b.w.mu.Unlock()
	_, err = b.module.IssueDebitNote(ctx, b.debit("DN-3", "JOB-A", 8, "party-a", "CNY", 2))
	rejected("ORG renamed the debtor", err, "FIN_BILLING_PROFILE_STALE")
	if r, err = b.profile("profile-a2", "party-a", api.BuyerCNEnterprise, 1); err != nil || r.Version != 2 {
		t.Fatalf("second profile version: %+v %v", r, err)
	}
	if r = b.issued(t, b.debit("DN-3", "JOB-A", 8, "party-a", "CNY", 2)); r.Number != "DN000003" {
		t.Fatalf("billed again: %+v", r)
	}
	if note, err = b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: "DN-1"}); err != nil ||
		note.BillingProfile.Version != 1 || note.BillingProfile.Title != "上海甲贸易有限公司" {
		t.Fatalf("an issued note keeps its profile version: %+v %v", note.BillingProfile, err)
	}
	items, err := b.module.ListOpenDebtorItems(ctx, api.ListOpenDebtorItems{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: "party-a"})
	if err != nil || len(items) != 2 || items[0].InvoiceID != "DN-1" || items[1].InvoiceID != "DN-3" || items[1].BillingProfile.Version != 2 {
		t.Fatalf("open items: %+v %v", items, err)
	}
	if r, err = b.module.IssueCreditNote(ctx, b.credit("CN-2", "DN-1", 9, 1)); err != nil || r.Number != "CN000002" {
		t.Fatalf("the last line reversed: %+v %v", r, err)
	}
	b.job = r.NewRevision
	if items, err = b.module.ListOpenDebtorItems(ctx, api.ListOpenDebtorItems{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: "party-a"}); err != nil ||
		len(items) != 1 || items[0].InvoiceID != "DN-3" {
		t.Fatalf("open items after the debit note was cancelled: %+v %v", items, err)
	}
	// the database refuses to cancel a job with an issued debit note, even past FIN
	if err = b.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, sql := range []string{
			`UPDATE cargo_fin.job_basis SET live=false WHERE job_id='JOB-A'`,
			`UPDATE cargo_fin.freight_job SET state='CANCELLED',cancel_reason='x',cancelled_by='x',cancelled_at=now(),cancel_entered_at=now(),
               revision=revision+1 WHERE job_id='JOB-A'`,
		} {
			if _, err := e.Exec(ctx, sql); err != nil {
				return err
			}
		}
		return nil
	}); err == nil {
		t.Fatal("a job with an issued debit note cancelled")
	} else if _, constraint := sqlState(err); constraint != "freight_job_invoices_consistent" {
		t.Fatalf("cancelling a billed job past FIN: %v", err)
	}
	if note, err = b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: "DN-1"}); err != nil || note.State != api.NoteCancelled ||
		note.Open != cny(0) {
		t.Fatalf("cancelled by credit notes: %+v %v", note, err)
	}

	_, err = b.tax("tax-doc-early", "DOC_FEE", api.TaxTaxable, 900, b.date(8))
	rejected("a treatment from a billed date", err, "FIN_TAX_TREATMENT_IN_USE")
	b.policyChange(t)(b.tax("tax-doc-later", "DOC_FEE", api.TaxTaxable, 900, b.date(48)))
	// every AIR_FREIGHT line billed is reversed, so its treatment may be corrected for the dates billed
	b.policyChange(t)(b.tax("tax-freight-fix", "AIR_FREIGHT", api.TaxPassThrough, 0, b.date(5)))
	_, err = b.module.EndExchangeRate(ctx, api.EndExchangeRate{CommandID: "end-invoice-rate", TenantID: b.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: b.policy, EntryNo: 2, ValidUntil: b.hours(4), Reason: "wrong rate", OccurredAt: b.hours(9)})
	rejected("ending the INVOICE rate a debit note used", err, "FIN_RATE_IN_USE")

	// the ESTIMATE rate ends just after the estimates were converted (at 2), the one CN-2 returned at 9 included; the
	// USD debit note to the agent (ACCOUNTING rate) is reversed after the end: the side returns to its estimate before
	// billing, at the estimate's own conversion (ESTIMATE 7.1234, CNY 1,424.68)
	endEstimate := func(id string, until int) (api.Receipt, error) {
		return b.module.EndExchangeRate(ctx, api.EndExchangeRate{CommandID: id, TenantID: b.tenant, ActorID: "acct-1", LegalEntityID: "LE",
			ExpectedRevision: b.policy, EntryNo: 1, ValidUntil: b.hours(until), Reason: "estimate rates move to the monthly table", OccurredAt: b.hours(10)})
	}
	_, err = endEstimate("end-estimate-rate-2", 2)
	rejected("ending the ESTIMATE rate at the estimates' conversion", err, "FIN_RATE_IN_USE")
	b.policyChange(t)(endEstimate("end-estimate-rate", 3))
	if r, err = b.module.IssueCreditNote(ctx, b.credit("CN-3", "DN-2", 11, 1)); err != nil || r.Number != "CN000003" {
		t.Fatalf("returned after the ESTIMATE rate ended: %+v %v", r, err)
	}
	b.job = r.NewRevision
	if note, err = b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: "CN-3"}); err != nil || note.Total != usd(-20_000) ||
		note.LocalTotal != cny(-142_000) || note.Lines[0].FX.RateType != api.RateAccounting || note.WriteOff {
		t.Fatalf("credit note of the USD note: %+v %v", note, err)
	}
	job, err = b.module.GetFreightJob(ctx, api.GetFreightJob{TenantID: b.tenant, JobID: "JOB-A"})
	if err != nil {
		t.Fatal(err)
	}
	estimate, back := job.Lines[2].Sell[0], job.Lines[2].Sell[len(job.Lines[2].Sell)-1]
	if back.Status != api.StatusEstimated || back.Amount != usd(20_000) || back.Local != cny(142_468) || back.FX == nil || back.FX.EntryNo != 1 ||
		back.FX.RateType != api.RateEstimate || !back.ConvertedAt.Equal(estimate.RecordedAt) || !back.RecordedAt.Equal(b.hours(11)) {
		t.Fatalf("the agent's charge back at its estimate: %+v", job.Lines[2].Sell)
	}
	// billed again and waived: the write-off leaves nothing to bill
	if r = b.issued(t, b.debit("DN-4", "JOB-A", 12, "agent-ny", "USD", 3)); r.Number != "DN000004" {
		t.Fatalf("the agent billed again: %+v", r)
	}
	waive := b.credit("CN-4", "DN-4", 13, 1)
	waive.WriteOff, waive.Reason = true, "collect charge waived by the agent"
	if r, err = b.module.IssueCreditNote(ctx, waive); err != nil || r.Number != "CN000004" {
		t.Fatalf("written off: %+v %v", r, err)
	}
	b.job = r.NewRevision
	if note, err = b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: "CN-4"}); err != nil || note.Total != usd(-20_000) ||
		note.LocalTotal != cny(-142_000) || !note.WriteOff {
		t.Fatalf("the write-off: %+v %v", note, err)
	}
	if job, err = b.module.GetFreightJob(ctx, api.GetFreightJob{TenantID: b.tenant, JobID: "JOB-A"}); err != nil ||
		job.Lines[2].Sell[len(job.Lines[2].Sell)-1].Amount != usd(0) || job.Lines[2].Sell[len(job.Lines[2].Sell)-1].FX != nil {
		t.Fatalf("the agent's charge written off: %+v %v", job.Lines[2].Sell, err)
	}
	if items, err = b.module.ListOpenDebtorItems(ctx, api.ListOpenDebtorItems{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: "agent-ny"}); err != nil ||
		len(items) != 0 {
		t.Fatalf("the agent's open items: %+v %v", items, err)
	}

	types := b.outboxTypes(t)
	for _, want := range []string{"ChargeTaxTreatmentSet", "InvoiceSeriesRegistered", "BillingProfileSet", "DebtorInvoiceIssued", "CreditNoteIssued",
		"DebtorInvoiceCancelled"} {
		if !slices.Contains(types, want) {
			t.Errorf("no %s event in %v", want, types)
		}
	}

	for name, sql := range map[string]string{
		"renumber a note":           `UPDATE cargo_fin.debtor_invoice SET number='DN999999' WHERE invoice_id='DN-3'`,
		"reissue a cancelled note":  `UPDATE cargo_fin.debtor_invoice SET state='ISSUED' WHERE invoice_id='DN-1'`,
		"delete a note line":        `DELETE FROM cargo_fin.debtor_invoice_line WHERE invoice_id='DN-3'`,
		"wind a sequence back":      `UPDATE cargo_fin.numbering_sequence SET next_val=next_val-1 WHERE doc_type='DEBIT_NOTE/LE'`,
		"delete a sequence":         `DELETE FROM cargo_fin.numbering_sequence WHERE doc_type='DEBIT_NOTE/LE'`,
		"change a series":           `UPDATE cargo_fin.invoice_series SET first_number=2 WHERE kind='DEBIT_NOTE'`,
		"jump a sequence":           `UPDATE cargo_fin.numbering_sequence SET next_val=next_val+2 WHERE doc_type='DEBIT_NOTE/LE'`,
		"start a stray sequence":    `INSERT INTO cargo_fin.numbering_sequence (scope,doc_type,period,next_val) VALUES ($1,'DEBIT_NOTE/LE-9','',1)`,
		"start past the seed":       `INSERT INTO cargo_fin.numbering_sequence (scope,doc_type,period,next_val) VALUES ($1,'DEBIT_NOTE/LE','2099',7)`,
		"another tenant's sequence": `INSERT INTO cargo_fin.numbering_sequence (scope,doc_type,period,next_val) VALUES ('another-tenant','DEBIT_NOTE/LE','',1)`,
		"change a profile":          `UPDATE cargo_fin.billing_profile_version SET title='x' WHERE debtor_party_id='party-a'`,
		"change a tax treatment":    `UPDATE cargo_fin.charge_tax_treatment SET rate_bp=1300 WHERE charge_code='DOC_FEE'`,
	} {
		err := b.within(t, func(ctx context.Context, e bentopg.Executor) error {
			var args []any
			if strings.Contains(sql, "$1") {
				args = append(args, b.tenant)
			}
			_, err := e.Exec(ctx, sql, args...)
			return err
		})
		if code, _ := sqlState(err); code == "" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err = b.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `UPDATE cargo_fin.numbering_sequence SET next_val=next_val+1 WHERE doc_type='DEBIT_NOTE/LE'`)
		return err
	}); err == nil {
		t.Fatal("a number drawn without a note")
	} else if _, constraint := sqlState(err); constraint != "invoice_series_consistent" {
		t.Fatalf("a number drawn without a note: %v", err)
	}
	// a conversion time belongs to an estimate with a rate, and is not after the version's own time
	for name, sql := range map[string]string{
		"a conversion time on a billed side": `INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_id,party_revision,currency,
            amount_minor,local_minor,rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,status,invoice_id,invoice_line_no,reason,
            recorded_by,recorded_at,entered_at,converted_at) SELECT tenant_id,job_id,line_no,side,4,party_id,party_revision,currency,amount_minor,local_minor,
            rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,'INVOICED','DN-1',1,'x','x',now(),now(),converted_at
            FROM cargo_fin.charge_side_version WHERE job_id='JOB-A' AND line_no=1 AND version_no=3`,
		"converted after it was recorded": `INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_id,party_revision,currency,
            amount_minor,local_minor,rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,status,reason,recorded_by,recorded_at,entered_at,
            converted_at) SELECT tenant_id,job_id,line_no,side,4,party_id,party_revision,currency,amount_minor,local_minor,rate_entry_no,rate_type,rate,scale,
            from_digits,to_digits,rate_source,status,'x','x',converted_at,now(),converted_at+interval '1 second'
            FROM cargo_fin.charge_side_version WHERE job_id='JOB-A' AND line_no=1 AND version_no=3`,
		"a conversion time without a rate": `INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_id,party_revision,currency,
            amount_minor,local_minor,status,reason,recorded_by,recorded_at,entered_at,converted_at) SELECT tenant_id,job_id,line_no,side,5,party_id,
            party_revision,currency,amount_minor,local_minor,'ESTIMATED','x','x',now(),now(),now()
            FROM cargo_fin.charge_side_version WHERE job_id='JOB-A' AND line_no=2 AND version_no=3`,
	} {
		err := b.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, sql)
			return err
		})
		if _, constraint := sqlState(err); constraint != "charge_side_converted" {
			t.Errorf("%s: %v", name, err)
		}
	}
	// the returned AIR_FREIGHT estimate (version 3) copied as a revision recorded now, keeping its conversion at 2 when
	// the ESTIMATE rate was in force: only a credit note's return keeps an earlier conversion
	if err = b.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, sql := range []string{
			`UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE job_id='JOB-A'`,
			`INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_id,party_revision,currency,amount_minor,local_minor,
               rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,status,reason,recorded_by,recorded_at,entered_at,converted_at)
             SELECT tenant_id,job_id,line_no,side,4,party_id,party_revision,currency,amount_minor,local_minor,rate_entry_no,rate_type,rate,scale,from_digits,
               to_digits,rate_source,status,'revised past FIN','x',now(),now(),converted_at
             FROM cargo_fin.charge_side_version WHERE job_id='JOB-A' AND line_no=1 AND version_no=3 AND converted_at IS NOT NULL`,
		} {
			if tag, err := e.Exec(ctx, sql); err != nil {
				return err
			} else if tag.RowsAffected() != 1 {
				return fmt.Errorf("%s: %d rows", sql[:40], tag.RowsAffected())
			}
		}
		return nil
	}); err == nil {
		t.Fatal("a revised estimate converted before it was recorded")
	} else if _, constraint := sqlState(err); constraint != "freight_job_consistent" {
		t.Fatalf("a revised estimate converted before it was recorded: %v", err)
	}
	// A forged copy of DN-3 with its number drawn and the job written, billing a side version that does not exist:
	// every other check passes, the job's note check does not.
	if err = b.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, sql := range []string{
			`WITH s AS (UPDATE cargo_fin.numbering_sequence SET next_val=next_val+1 WHERE doc_type='DEBIT_NOTE/LE' RETURNING next_val AS seq, 'DN' AS prefix,
               6 AS width)
             INSERT INTO cargo_fin.debtor_invoice (tenant_id,invoice_id,kind,job_id,legal_entity_id,number,number_seq,debtor_party_id,debtor_revision,
               profile_version,currency,invoice_date,due_date,payment_days,payment_basis,terms_version,document_id,document_version,document_number,
               master_number,document_issued_at,total_minor,local_total_minor,state,issued_by,issued_at,entered_at)
             SELECT n.tenant_id,'DN-FORGED',n.kind,n.job_id,n.legal_entity_id,s.prefix||lpad(s.seq::text,s.width,'0'),s.seq,n.debtor_party_id,
               n.debtor_revision,n.profile_version,n.currency,n.invoice_date,n.due_date,n.payment_days,n.payment_basis,n.terms_version,n.document_id,
               n.document_version,n.document_number,n.master_number,n.document_issued_at,n.total_minor,n.local_total_minor,n.state,n.issued_by,n.issued_at,
               n.entered_at
             FROM cargo_fin.debtor_invoice n, s WHERE n.invoice_id='DN-3'`,
			`INSERT INTO cargo_fin.debtor_invoice_line (tenant_id,invoice_id,line_no,job_id,job_line_no,side_version,charge_code,currency,amount_minor,
               invoice_minor,local_minor,tax_version,treatment,rate_bp)
             SELECT tenant_id,'DN-FORGED',line_no,job_id,job_line_no,side_version+5,charge_code,currency,amount_minor,invoice_minor,local_minor,tax_version,
               treatment,rate_bp FROM cargo_fin.debtor_invoice_line WHERE invoice_id='DN-3'`,
			`UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE job_id='JOB-A'`,
		} {
			if _, err := e.Exec(ctx, sql); err != nil {
				return err
			}
		}
		return nil
	}); err == nil {
		t.Fatal("a forged debit note committed")
	} else if _, constraint := sqlState(err); constraint != "freight_job_invoices_consistent" {
		t.Fatalf("a forged debit note: %v", err)
	}
	// a copy of DN-3 numbered off its template, with the sequence value drawn, is refused at commit (the number check
	// itself is TestDatabaseRendersNumbersAsThePlatform's)
	if err = b.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `WITH q AS (UPDATE cargo_fin.numbering_sequence SET next_val=next_val+1 WHERE doc_type='DEBIT_NOTE/LE' AND period=''
              RETURNING next_val AS seq)
            INSERT INTO cargo_fin.debtor_invoice (tenant_id,invoice_id,kind,job_id,legal_entity_id,number,number_seq,debtor_party_id,debtor_revision,
              profile_version,currency,invoice_date,due_date,payment_days,payment_basis,terms_version,document_id,document_version,document_number,
              master_number,document_issued_at,total_minor,local_total_minor,state,issued_by,issued_at,entered_at)
            SELECT n.tenant_id,'DN-OFF-TEMPLATE',n.kind,n.job_id,n.legal_entity_id,'DN-NOT-A-TEMPLATE',q.seq,n.debtor_party_id,n.debtor_revision,
              n.profile_version,n.currency,n.invoice_date,n.due_date,n.payment_days,n.payment_basis,n.terms_version,n.document_id,n.document_version,
              n.document_number,n.master_number,n.document_issued_at,n.total_minor,n.local_total_minor,n.state,n.issued_by,n.issued_at,n.entered_at
            FROM cargo_fin.debtor_invoice n, q WHERE n.invoice_id='DN-3'`)
		if err != nil {
			return err
		}
		if _, err = e.Exec(ctx, `INSERT INTO cargo_fin.debtor_invoice_line (tenant_id,invoice_id,line_no,job_id,job_line_no,side_version,charge_code,currency,
              amount_minor,invoice_minor,local_minor,tax_version,treatment,rate_bp) SELECT tenant_id,'DN-OFF-TEMPLATE',line_no,job_id,job_line_no,side_version+7,
              charge_code,currency,amount_minor,invoice_minor,local_minor,tax_version,treatment,rate_bp FROM cargo_fin.debtor_invoice_line WHERE invoice_id='DN-3'`); err != nil {
			return err
		}
		_, err = e.Exec(ctx, `UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE job_id='JOB-A'`)
		return err
	}); err == nil {
		t.Fatal("a note numbered off its template committed")
	} else if _, constraint := sqlState(err); constraint != "freight_job_invoices_consistent" {
		t.Fatalf("a note numbered off its template: %v", err)
	}
}

// Debit notes of eight jobs of the legal entity issued at once draw eight numbers without gaps; two notes on one job
// at the same revision: one wins. A series past its width refuses the note and draws nothing.
func TestDebitNotesDrawNumbersConcurrently(t *testing.T) {
	ctx := context.Background()
	b := newBilling(t)
	from := b.at.AddDate(0, 0, -5).Format(time.DateOnly)
	b.policyChange(t)(b.tax("tax-truck", "TRUCKING", api.TaxTaxable, 900, from))
	b.policyChange(t)(b.series("series-dn", api.KindDebitNote, "DN{seq:6}", 101))
	if _, err := b.profile("profile-a", "party-a", api.BuyerCNEnterprise, 0); err != nil {
		t.Fatal(err)
	}
	const n = 8
	revisions := make([]int64, n)
	b.w.mu.Lock()
	for i := range n {
		shipment := fmt.Sprintf("SH-%d", i)
		b.w.drivers[shipment] = fulapi.CostDriver{TenantID: b.tenant, ShipmentID: shipment, Kind: fulapi.DriverShipmentPlanned, Revision: 1,
			Status: fulapi.StatusPlanned, PlanVersion: 1, TransportMode: "AIR", LegalEntityID: "LE", CustomerPartyID: "party-a", Packs: 1}
		b.w.documents[shipment] = houseDocument(shipment, fmt.Sprintf("SHA000010%d", i), b.hours(2))
	}
	b.w.mu.Unlock()
	for i := range n {
		job := fmt.Sprintf("JOB-%d", i)
		if _, err := b.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-" + job, TenantID: b.tenant, ActorID: "fin-1", JobID: job,
			ShipmentID: fmt.Sprintf("SH-%d", i), OccurredAt: b.hours(1)}); err != nil {
			t.Fatal(err)
		}
		r, err := b.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "line-" + job, TenantID: b.tenant, ActorID: "fin-1", JobID: job,
			ExpectedRevision: 1, ChargeCode: "TRUCKING", Sell: &api.ChargeSide{PartyID: "party-a", Amount: cny(int64(10_000 * (i + 1)))}, Reason: "pickup",
			OccurredAt: b.hours(2)})
		if err != nil {
			t.Fatal(err)
		}
		revisions[i] = r.NewRevision
	}
	numbers := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			b2 := *b
			b2.job = revisions[i]
			r, err := b.module.IssueDebitNote(ctx, b2.debit(fmt.Sprintf("DN-%d", i), fmt.Sprintf("JOB-%d", i), 3, "party-a", "CNY", 1))
			numbers[i], errs[i] = r.Number, err
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("note %d: %v", i, err)
		}
	}
	slices.Sort(numbers)
	for i, number := range numbers {
		if want := fmt.Sprintf("DN%06d", 101+i); number != want {
			t.Fatalf("numbers %v: want %s at %d", numbers, want, i)
		}
	}
	if policy, err := b.module.GetFinancePolicy(ctx, api.GetFinancePolicy{TenantID: b.tenant, LegalEntityID: "LE"}); err != nil || len(policy.Series) != 1 ||
		len(policy.Series[0].Last) != 1 || policy.Series[0].Last[0].Last != 100+n || policy.Series[0].Template != "DN{seq:6}" || len(policy.TaxTreatments) != 1 {
		t.Fatalf("policy after %d notes: %+v %v", n, policy.Series, err)
	}

	// two notes on JOB-0's second line at the same revision
	r, err := b.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "line-JOB-0-2", TenantID: b.tenant, ActorID: "fin-1", JobID: "JOB-0",
		ExpectedRevision: revisions[0] + 1, ChargeCode: "TRUCKING", Sell: &api.ChargeSide{PartyID: "party-a", Amount: cny(5_000)}, Reason: "waiting time",
		OccurredAt: b.hours(4)})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	won, conflicts := 0, 0
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b2 := *b
			b2.job = r.NewRevision
			_, err := b.module.IssueDebitNote(ctx, b2.debit(fmt.Sprintf("DN-0-%d", i), "JOB-0", 5, "party-a", "CNY", 2))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, api.ErrConflict):
				conflicts++
			default:
				t.Errorf("note %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || conflicts != 1 {
		t.Fatalf("won %d, conflicts %d", won, conflicts)
	}

	// a series whose next number does not fit refuses the note and draws nothing
	c := newBilling(t)
	c.policyChange(t)(c.tax("tax-freight", "AIR_FREIGHT", api.TaxExemptIntlFreight, 0, from))
	c.policyChange(t)(c.tax("tax-doc", "DOC_FEE", api.TaxTaxable, 600, from))
	c.policyChange(t)(c.series("series-dn", api.KindDebitNote, "D{seq:4}", 9_999))
	if _, err = c.profile("profile-a", "party-a", api.BuyerCNEnterprise, 0); err != nil {
		t.Fatal(err)
	}
	c.w.mu.Lock()
	c.w.documents["SH-A"] = houseDocument("SH-A", "SHA0000001", c.hours(2))
	c.w.mu.Unlock()
	if r = c.issued(t, c.debit("DN-1", "JOB-A", 5, "party-a", "CNY", 1)); r.Number != "D9999" {
		t.Fatalf("last number: %+v", r)
	}
	if _, err = c.module.IssueDebitNote(ctx, c.debit("DN-2", "JOB-A", 6, "party-a", "CNY", 2)); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "FIN_SERIES_EXHAUSTED") {
		t.Fatalf("past the width: %v", err)
	}
	var next int64
	if err = c.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT next_val FROM cargo_fin.numbering_sequence WHERE doc_type='DEBIT_NOTE/LE'`).Scan(&next)
	}); err != nil || next != 9_999 {
		t.Fatalf("next number %d: %v", next, err)
	}
}

// A tax treatment set while debit notes billing its code are issued: the policy lock orders them, so either the
// treatment commits first and every note bills under it, or a note commits first and the treatment is refused as in
// use; never a note under a treatment that a later version replaces for its date.
func TestTaxTreatmentRacesDebitNotes(t *testing.T) {
	ctx := context.Background()
	b := newBilling(t)
	from := b.at.AddDate(0, 0, -5).Format(time.DateOnly)
	b.policyChange(t)(b.tax("tax-truck", "TRUCKING", api.TaxTaxable, 600, from))
	b.policyChange(t)(b.series("series-dn", api.KindDebitNote, "DN{seq:6}", 1))
	if _, err := b.profile("profile-a", "party-a", api.BuyerCNEnterprise, 0); err != nil {
		t.Fatal(err)
	}
	const n = 6
	revisions := make([]int64, n)
	b.w.mu.Lock()
	for i := range n {
		shipment := fmt.Sprintf("SH-%d", i)
		b.w.drivers[shipment] = fulapi.CostDriver{TenantID: b.tenant, ShipmentID: shipment, Kind: fulapi.DriverShipmentPlanned, Revision: 1,
			Status: fulapi.StatusPlanned, PlanVersion: 1, TransportMode: "AIR", LegalEntityID: "LE", CustomerPartyID: "party-a", Packs: 1}
		b.w.documents[shipment] = houseDocument(shipment, fmt.Sprintf("SHA000020%d", i), b.hours(2))
	}
	b.w.mu.Unlock()
	for i := range n {
		job := fmt.Sprintf("JOB-%d", i)
		if _, err := b.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-" + job, TenantID: b.tenant, ActorID: "fin-1", JobID: job,
			ShipmentID: fmt.Sprintf("SH-%d", i), OccurredAt: b.hours(1)}); err != nil {
			t.Fatal(err)
		}
		r, err := b.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "line-" + job, TenantID: b.tenant, ActorID: "fin-1", JobID: job,
			ExpectedRevision: 1, ChargeCode: "TRUCKING", Sell: &api.ChargeSide{PartyID: "party-a", Amount: cny(10_000)}, Reason: "pickup",
			OccurredAt: b.hours(2)})
		if err != nil {
			t.Fatal(err)
		}
		revisions[i] = r.NewRevision
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	var taxErr error
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			b2 := *b
			b2.job = revisions[i]
			_, errs[i] = b.module.IssueDebitNote(ctx, b2.debit(fmt.Sprintf("DN-%d", i), fmt.Sprintf("JOB-%d", i), 3, "party-a", "CNY", 1))
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, taxErr = b.tax("tax-truck-9", "TRUCKING", api.TaxTaxable, 900, b.date(3))
	}()
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("note %d: %v", i, err)
		}
	}
	versions := map[int]int{}
	for i := range n {
		note, err := b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: fmt.Sprintf("DN-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		versions[note.Lines[0].TaxVersion]++
	}
	switch {
	case taxErr == nil && versions[2] == n:
		t.Logf("the treatment committed first: %d notes at 9%%", n)
	case errors.Is(taxErr, api.ErrRejected) && strings.Contains(taxErr.Error(), "FIN_TAX_TREATMENT_IN_USE") && versions[1] == n:
		t.Logf("a note committed first: the treatment refused, %d notes at 6%%", n)
	default:
		t.Fatalf("treatment %v, notes per version %v", taxErr, versions)
	}
}

// A monthly template restarts its sequence each month: notes take the period of their invoice dates, so notes dated in
// two months draw from two sequences, each without gaps, and the policy shows the last number of each.
func TestNumbersFollowTheInvoiceDatePeriod(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	m, err := finmodule.New(f.db, f.w, f.w, f.w, f.w, allow{}, finmodule.WithRecordWindow(kernel.RecordWindow{MaxBackdate: 100 * 24 * time.Hour,
		Skew: 5 * time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	f.module = m
	now := time.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 12, 0, 0, 0, time.UTC)
	may := first.AddDate(0, -1, -1) // the last day of the month before last, at noon
	june := first.AddDate(0, -1, 1) // the second day of last month
	b := &billing{fixture: f, policy: 1}
	at := func(d time.Time, h int) time.Time { return d.Add(time.Duration(h) * time.Hour) }
	if _, err = m.OpenFinancePolicy(ctx, api.OpenFinancePolicy{CommandID: "policy", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: at(may, -6)}); err != nil {
		t.Fatal(err)
	}
	b.policyChange(t)(m.SetChargeTaxTreatment(ctx, api.SetChargeTaxTreatment{CommandID: "tax", TenantID: f.tenant, ActorID: "tax-1", LegalEntityID: "LE",
		ExpectedRevision: b.policy, ChargeCode: "TRUCKING", Treatment: api.TaxTaxable, RateBP: 900, EffectiveFrom: may.AddDate(0, 0, -5).Format(time.DateOnly),
		OccurredAt: at(may, -6)}))
	series := api.RegisterInvoiceSeries{CommandID: "series", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE", ExpectedRevision: b.policy,
		Kind: api.KindDebitNote, Template: "DN{yyyy}{mm}-{seq:4}", FirstNumber: 5, OccurredAt: at(may, -6)}
	if _, err = m.RegisterInvoiceSeries(ctx, series); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "names the date of its period") {
		t.Fatalf("a first number above 1 without its period: %v", err)
	}
	// numbering moves from another system in May: May's notes continue from 5
	series.CommandID, series.FirstPeriodDate = "series-may", may.Format(time.DateOnly)
	b.policyChange(t)(m.RegisterInvoiceSeries(ctx, series))
	if _, err = b.profile("profile-a", "party-a", api.BuyerCNEnterprise, 0); err != nil {
		t.Fatal(err)
	}
	dates := []time.Time{may, june, june}
	var numbers []string
	f.w.mu.Lock()
	for i := range dates {
		shipment := fmt.Sprintf("SH-%d", i)
		f.w.drivers[shipment] = fulapi.CostDriver{TenantID: f.tenant, ShipmentID: shipment, Kind: fulapi.DriverShipmentPlanned, Revision: 1,
			Status: fulapi.StatusPlanned, PlanVersion: 1, TransportMode: "AIR", LegalEntityID: "LE", CustomerPartyID: "party-a", Packs: 1}
		f.w.documents[shipment] = houseDocument(shipment, fmt.Sprintf("SHA000030%d", i), at(dates[i], -2))
	}
	f.w.mu.Unlock()
	for i, d := range dates {
		job := fmt.Sprintf("JOB-%d", i)
		if _, err = m.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-" + job, TenantID: f.tenant, ActorID: "fin-1", JobID: job,
			ShipmentID: fmt.Sprintf("SH-%d", i), OccurredAt: at(d, -4)}); err != nil {
			t.Fatal(err)
		}
		r, err := m.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "line-" + job, TenantID: f.tenant, ActorID: "fin-1", JobID: job, ExpectedRevision: 1,
			ChargeCode: "TRUCKING", Sell: &api.ChargeSide{PartyID: "party-a", Amount: cny(10_000)}, Reason: "pickup", OccurredAt: at(d, -3)})
		if err != nil {
			t.Fatal(err)
		}
		r, err = m.IssueDebitNote(ctx, api.IssueDebitNote{CommandID: "issue-" + job, TenantID: f.tenant, ActorID: "bill-1", JobID: job,
			ExpectedRevision: r.NewRevision, InvoiceID: "DN-" + job, DebtorPartyID: "party-a", LineNos: []int{1}, Currency: "CNY",
			InvoiceDate: d.Format(time.DateOnly), OccurredAt: d})
		if err != nil {
			t.Fatalf("%s: %v", job, err)
		}
		numbers = append(numbers, r.Number)
	}
	want := []string{"DN" + may.Format("200601") + "-0005", "DN" + june.Format("200601") + "-0001", "DN" + june.Format("200601") + "-0002"}
	if !slices.Equal(numbers, want) {
		t.Fatalf("numbers %v, want %v", numbers, want)
	}
	policy, err := m.GetFinancePolicy(ctx, api.GetFinancePolicy{TenantID: f.tenant, LegalEntityID: "LE"})
	if err != nil || len(policy.Series) != 1 || !slices.Equal(policy.Series[0].Last, []api.NumberPeriod{{Period: may.Format("200601"), Last: 5},
		{Period: june.Format("200601"), Last: 2}}) {
		t.Fatalf("policy series: %+v %v", policy.Series, err)
	}
	note, err := m.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: f.tenant, InvoiceID: "DN-JOB-2"})
	if err != nil || note.Number != want[2] {
		t.Fatalf("note %+v %v", note, err)
	}
}

// The database renders Cargo's template subset as the platform does: for templates of every date token and dates at
// the edges of months, quarters and years, render_number and number_period agree with the Go renderer, and a sequence
// value wider than its template renders nothing.
func TestDatabaseRendersNumbersAsThePlatform(t *testing.T) {
	f := newFixture(t)
	templates := []string{"DN{seq:6}", "DN{yy}{seq:5}", "DN-{yyyy}-{seq:4}", "DN{yyyy}{mm}-{seq:4}", "CN{yy}Q{q}{seq:4}", "D{seq:4}-X", "INV{yy}{mm}{seq:6}A"}
	dates := []time.Time{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(2099, 10, 1, 0, 0, 0, 0, time.UTC)}
	compared := 0
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, raw := range templates {
			tmpl, err := numbering.Parse(raw)
			if err != nil {
				return err
			}
			for _, d := range dates {
				for _, seq := range []int64{1, 42, 9_999} {
					want, err := tmpl.Render(seq, d)
					if err != nil {
						return err
					}
					var number, period string
					if err = e.QueryRow(ctx, `SELECT cargo_fin.render_number($1, $2, $3::date), cargo_fin.number_period($1, $3::date)`, raw, seq,
						d.Format(time.DateOnly)).Scan(&number, &period); err != nil {
						return err
					}
					if number != want || period != tmpl.Period(d) {
						t.Errorf("%s %d %s: database %q %q, Go %q %q", raw, seq, d.Format(time.DateOnly), number, period, want, tmpl.Period(d))
					}
					compared++
				}
			}
		}
		var wide *string
		if err := e.QueryRow(ctx, `SELECT cargo_fin.render_number('DN{seq:4}', 12345, date '2026-11-12')`).Scan(&wide); err != nil || wide != nil {
			t.Errorf("a sequence wider than its template: %v %v", wide, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if compared != len(templates)*len(dates)*3 {
		t.Fatalf("compared %d", compared)
	}
}

// A debit note bills the house document version valid at its business time; the review list shows the open debit
// notes whose document was amended, cancelled or replaced since, until a credit note closes them.
func TestDebitNotesFollowTheHouseDocument(t *testing.T) {
	ctx := context.Background()
	b := newBilling(t)
	b.ready(t)
	v1 := houseDocument("SH-A", "SHA0000001", b.hours(3))
	v2 := v1
	v2.Version, v2.IssuedAt, v2.SHA256 = 2, b.hours(6), "v2"
	v1.VersionStatus = docapi.VersionSuperseded
	b.w.mu.Lock()
	b.w.documents["SH-A"], b.w.earlier["SH-A"] = v2, []docapi.TransportDocumentSnapshot{v1}
	b.w.mu.Unlock()
	changes := func() []api.DocumentChange {
		t.Helper()
		out, err := b.module.ListDebitNotesOnChangedDocuments(ctx, api.ListDebitNotesOnChangedDocuments{TenantID: b.tenant, LegalEntityID: "LE"})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// entered after the amendment, the note of hour 4 bills version 1; the note of hour 7 bills version 2
	b.issued(t, b.debit("DN-1", "JOB-A", 4, "party-a", "CNY", 1, 2))
	b.issued(t, b.debit("DN-2", "JOB-A", 7, "agent-ny", "USD", 3))
	note, err := b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: "DN-1"})
	if err != nil || note.Document.Version != 1 || !note.Document.IssuedAt.Equal(b.hours(3)) {
		t.Fatalf("a backdated note: %+v %v", note.Document, err)
	}
	if note, err = b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: "DN-2"}); err != nil || note.Document.Version != 2 {
		t.Fatalf("a note after the amendment: %+v %v", note.Document, err)
	}
	if got := changes(); len(got) != 1 || got[0].InvoiceID != "DN-1" || got[0].Number != "DN000001" || got[0].Change != api.DocumentAmended ||
		got[0].Billed.Version != 1 || got[0].Current == nil || got[0].Current.Version != 2 || got[0].ShipmentID != "SH-A" || got[0].DebtorPartyID != "party-a" ||
		got[0].JobID != "JOB-A" {
		t.Fatalf("amended: %+v", got)
	}
	if got, err := b.module.ListDebitNotesOnChangedDocuments(ctx, api.ListDebitNotesOnChangedDocuments{TenantID: b.tenant, LegalEntityID: "LE-2"}); err != nil ||
		len(got) != 0 {
		t.Fatalf("another legal entity: %+v %v", got, err)
	}

	// the HAWB cancelled: both notes, nothing valid now
	b.w.mu.Lock()
	delete(b.w.documents, "SH-A")
	b.w.mu.Unlock()
	if got := changes(); len(got) != 2 || got[0].InvoiceID != "DN-1" || got[1].InvoiceID != "DN-2" || got[0].Change != api.DocumentCancelled ||
		got[1].Change != api.DocumentCancelled || got[0].Current != nil || got[1].Billed.Version != 2 {
		t.Fatalf("cancelled: %+v", got)
	}
	_, err = b.module.IssueDebitNote(ctx, b.debit("DN-X", "JOB-A", 8, "party-a", "CNY", 2))
	if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_NO_ISSUED_DOCUMENT") {
		t.Fatalf("billing a cancelled HAWB: %v", err)
	}

	// another HAWB issued for the shipment: replaced; a credit note closes DN-1
	replacement := houseDocument("SH-A", "SHA0000002", b.hours(8))
	replacement.DocumentID = "DOC-SH-A-2"
	b.w.mu.Lock()
	b.w.documents["SH-A"], b.w.earlier["SH-A"] = replacement, nil
	b.w.mu.Unlock()
	if got := changes(); len(got) != 2 || got[0].Change != api.DocumentReplaced || got[1].Change != api.DocumentReplaced ||
		got[0].Current == nil || got[0].Current.DocumentID != "DOC-SH-A-2" || got[0].Current.Number != "SHA0000002" || got[0].Billed.DocumentID != "DOC-SH-A" {
		t.Fatalf("replaced: %+v", got)
	}
	r, err := b.module.IssueCreditNote(ctx, b.credit("CN-1", "DN-1", 9, 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	b.job = r.NewRevision
	if got := changes(); len(got) != 1 || got[0].InvoiceID != "DN-2" {
		t.Fatalf("after the credit note: %+v", got)
	}
	// billed again on the new HAWB: nothing to review for it
	b.issued(t, b.debit("DN-3", "JOB-A", 10, "party-a", "CNY", 1, 2))
	if got := changes(); len(got) != 1 || got[0].InvoiceID != "DN-2" {
		t.Fatalf("billed on the new HAWB: %+v", got)
	}
}

var errRolledBack = errors.New("rolled back")

// At commit the database holds a credit note's return to the estimate it returns to (clause (h) of the job's note
// check): the estimate billed with its conversion and conversion time, or zero for a write-off. A return FIN V0.2 wrote,
// with its conversion but no conversion time, stays valid and the job goes on; no new return is written that way.
func TestReturnedEstimatesAreCheckedAtCommit(t *testing.T) {
	ctx := context.Background()
	b := newBilling(t)
	b.ready(t)
	b.w.mu.Lock()
	b.w.documents["SH-A"] = houseDocument("SH-A", "SHA0000001", b.hours(3))
	b.w.mu.Unlock()
	credited := func(cmd api.IssueCreditNote) {
		t.Helper()
		r, err := b.module.IssueCreditNote(ctx, cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd.InvoiceID, err)
		}
		b.job = r.NewRevision
	}
	// line 3 (USD 200.00 to the agent) billed and returned; line 2 (CNY 300.00) billed and written off
	b.issued(t, b.debit("DN-1", "JOB-A", 4, "agent-ny", "USD", 3))
	b.issued(t, b.debit("DN-2", "JOB-A", 4, "party-a", "CNY", 2))
	credited(b.credit("CN-1", "DN-1", 5, 1))
	waive := b.credit("CN-2", "DN-2", 5, 1)
	waive.WriteOff = true
	credited(waive)

	// the job's commit checks on its rows as they are, nothing kept
	check := func() error {
		err := b.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, sql := range []string{`UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE job_id='JOB-A'`, `SET CONSTRAINTS ALL IMMEDIATE`} {
				if _, err := e.Exec(ctx, sql); err != nil {
					return err
				}
			}
			return errRolledBack
		})
		if errors.Is(err, errRolledBack) {
			return nil
		}
		return err
	}
	if err := check(); err != nil {
		t.Fatalf("the job as FIN wrote it: %v", err)
	}
	const usdReturn = ` WHERE tenant_id=$1 AND job_id='JOB-A' AND line_no=3 AND side='SELL' AND version_no=3`
	const waived = ` WHERE tenant_id=$1 AND job_id='JOB-A' AND line_no=2 AND side='SELL' AND version_no=3`
	const fromEstimate = `amount_minor=p.amount_minor, local_minor=p.local_minor, rate_entry_no=p.rate_entry_no, rate_type=p.rate_type, rate=p.rate,
        scale=p.scale, from_digits=p.from_digits, to_digits=p.to_digits, rate_source=p.rate_source
        FROM cargo_fin.charge_side_version p WHERE p.tenant_id=w.tenant_id AND p.job_id=w.job_id AND p.line_no=w.line_no AND p.side=w.side AND p.version_no=1
        AND w.tenant_id=$1 AND w.job_id='JOB-A' AND w.side='SELL' AND w.version_no=3 AND w.line_no=`
	for _, c := range []struct{ name, wrong, right string }{
		{"a return converted at another time than its estimate",
			`UPDATE cargo_fin.charge_side_version SET converted_at=converted_at+interval '1 microsecond'` + usdReturn,
			`UPDATE cargo_fin.charge_side_version SET converted_at=converted_at-interval '1 microsecond'` + usdReturn},
		{"a return at zero without a write-off",
			`UPDATE cargo_fin.charge_side_version SET amount_minor=0, local_minor=0, rate_entry_no=NULL, rate_type=NULL, rate=NULL, scale=NULL,
               from_digits=NULL, to_digits=NULL, rate_source=NULL, converted_at=NULL` + usdReturn,
			`UPDATE cargo_fin.charge_side_version w SET converted_at=p.recorded_at, ` + fromEstimate + `3`},
		{"a write-off returned at its amount",
			`UPDATE cargo_fin.charge_side_version w SET ` + fromEstimate + `2`,
			`UPDATE cargo_fin.charge_side_version SET amount_minor=0, local_minor=0` + waived},
	} {
		b.rewrite(t, c.wrong)
		if _, constraint := sqlState(check()); constraint != "freight_job_invoices_consistent" {
			t.Fatalf("%s: %v", c.name, check())
		}
		b.rewrite(t, c.right)
		if err := check(); err != nil {
			t.Fatalf("%s, put back: %v", c.name, err)
		}
	}

	// the return as FIN V0.2 wrote it (the ESTIMATE rate in force when it was recorded, no conversion time) stays valid:
	// the side is billed again and credited, and returns to that estimate, converted when it was recorded
	b.rewrite(t, `UPDATE cargo_fin.charge_side_version SET converted_at=NULL`+usdReturn)
	if err := check(); err != nil {
		t.Fatalf("a return FIN V0.2 wrote: %v", err)
	}
	b.issued(t, b.debit("DN-3", "JOB-A", 6, "agent-ny", "USD", 3))
	// a new return written that way is refused, with the job written and before any other check
	err := b.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, sql := range []string{
			`INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_id,party_revision,currency,amount_minor,local_minor,
               rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,status,reason,recorded_by,recorded_at,entered_at)
             SELECT tenant_id,job_id,line_no,side,5,party_id,party_revision,currency,amount_minor,local_minor,rate_entry_no,rate_type,rate,scale,from_digits,
               to_digits,rate_source,status,'returned past FIN','x',now(),now()
             FROM cargo_fin.charge_side_version WHERE job_id='JOB-A' AND line_no=3 AND version_no=3`,
			`UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE job_id='JOB-A'`,
		} {
			if tag, err := e.Exec(ctx, sql); err != nil {
				return err
			} else if tag.RowsAffected() != 1 {
				return fmt.Errorf("%d rows", tag.RowsAffected())
			}
		}
		return nil
	})
	if _, constraint := sqlState(err); constraint != "charge_side_return_converted" {
		t.Fatalf("a new return without its conversion time: %v", err)
	}
	credited(b.credit("CN-3", "DN-3", 7, 1))
	job, err := b.module.GetFreightJob(ctx, api.GetFreightJob{TenantID: b.tenant, JobID: "JOB-A"})
	if err != nil {
		t.Fatal(err)
	}
	sell := job.Lines[2].Sell
	old, back := sell[2], sell[len(sell)-1]
	if len(sell) != 5 || !old.ConvertedAt.IsZero() || !old.RecordedAt.Equal(b.hours(5)) || back.Status != api.StatusEstimated || back.Amount != usd(20_000) ||
		back.Local != cny(142_468) || back.FX == nil || *back.FX != *old.FX || !back.ConvertedAt.Equal(old.RecordedAt) || !back.RecordedAt.Equal(b.hours(7)) {
		t.Fatalf("returned to the V0.2 return: %+v", sell)
	}
}
