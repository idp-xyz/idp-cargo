//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
)

// seaDocument is DOC's answer for a sea shipment's house document: a house bill of lading or a house sea waybill under
// the carrier's master bill, issued as version 1.
func seaDocument(kind, shipment, number string, issued time.Time) docapi.TransportDocumentSnapshot {
	return docapi.TransportDocumentSnapshot{DocumentID: "DOC-" + shipment, Kind: kind, Version: 1, VersionStatus: docapi.VersionIssued, Number: number,
		MasterNumber: "LINU2611000457", ShipmentID: shipment, ConsolidationID: "C-1", LegalEntityID: "LE", Origin: "CNSHG", Destination: "DEHAM", IssuedAt: issued}
}

// A freight job of a SEA shipment opens as SEA_EXPORT_LCL_HOUSE, and a debit note bills it on the house document DOC
// answers, whatever its kind: a house bill of lading or a house sea waybill. FIN keeps the document's id, version,
// number, master number and issue time as it does for a HAWB, and refuses what it refuses for a HAWB: no house
// document, a version that is not issued, a note before the document's issue time, a document of another shipment or
// legal entity.
func TestSeaJobsAreBilledOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	b := &billing{fixture: f}
	rejected := func(name string, err error, code string) {
		t.Helper()
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), code) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// FUL's stand-in reports the two shipments by sea; Q-1 is an ocean quote with a flat OCEAN_FREIGHT line
	f.w.mu.Lock()
	for _, s := range []string{"SH-A", "SH-B"} {
		d := f.w.drivers[s]
		d.TransportMode = "SEA"
		f.w.drivers[s] = d
	}
	basis := f.w.bases["Q-1"]
	basis.Lines = []rateapi.PricingLine{{LineNo: 1, ChargeCode: "OCEAN_FREIGHT", Sell: usd(100_000), Cost: usd(50_000)}, {LineNo: 2, ChargeCode: "DOC_FEE", Sell: cny(30_000)}}
	f.w.bases["Q-1"] = basis
	// the second shipment is another order on its own quote, the same ocean quote again
	second := basis
	second.BasisID, second.QuoteID = "QUOTE:Q-2@2", "Q-2"
	f.w.bases["Q-2"] = second
	d := f.w.drivers["SH-B"]
	d.Orders = []fulapi.CostOrder{{OrderID: "O-B", QuoteID: "Q-2", QuoteRevision: 2, Packs: 6}}
	f.w.drivers["SH-B"] = d
	f.w.mu.Unlock()
	if _, err := f.module.OpenFinancePolicy(ctx, api.OpenFinancePolicy{CommandID: "policy", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: f.hours(0)}); err != nil {
		t.Fatal(err)
	}
	b.policy = 1
	b.rate(t, "rate-estimate", api.RateEstimate, 71_234)
	b.rate(t, "rate-invoice", api.RateInvoice, 72_000)
	from := f.at.AddDate(0, 0, -5).Format(time.DateOnly)
	b.policyChange(t)(b.tax("tax-freight", "OCEAN_FREIGHT", api.TaxExemptIntlFreight, 0, from))
	b.policyChange(t)(b.tax("tax-doc", "DOC_FEE", api.TaxTaxable, 600, from))
	b.policyChange(t)(b.series("series-dn", api.KindDebitNote, "SEADN{seq:6}", 1))
	b.policyChange(t)(b.series("series-cn", api.KindCreditNote, "SEACN{seq:6}", 1))
	if _, err := b.profile("profile-party-a", "party-a", api.BuyerCNEnterprise, 0); err != nil {
		t.Fatal(err)
	}

	// the jobs open as SEA_EXPORT_LCL_HOUSE and take the quote's lines
	revision := map[string]int64{}
	for i, s := range []string{"A", "B"} {
		quote, order := "Q-1", "O-A"
		if s == "B" {
			quote, order = "Q-2", "O-B"
		}
		job := "JOB-" + s
		r, err := f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-" + s, TenantID: f.tenant, ActorID: "fin-1", JobID: job, ShipmentID: "SH-" + s,
			OccurredAt: f.hours(1 + i)})
		if err != nil {
			t.Fatalf("open %s: %v", job, err)
		}
		if r, err = f.module.ApplyPricingBasis(ctx, api.ApplyPricingBasis{CommandID: "apply-" + s, TenantID: f.tenant, ActorID: "fin-1", JobID: job,
			ExpectedRevision: r.NewRevision, QuoteID: quote, OrderID: order, OccurredAt: f.hours(2)}); err != nil {
			t.Fatalf("pricing basis of %s: %v", job, err)
		}
		revision[job] = r.NewRevision
		got, err := f.module.GetFreightJob(ctx, api.GetFreightJob{TenantID: f.tenant, JobID: job})
		if err != nil || got.Kind != api.KindSeaExportLCLHouse || got.State != api.JobOpen || got.ShipmentID != "SH-"+s || len(got.Lines) != 2 ||
			got.Lines[0].ChargeCode != "OCEAN_FREIGHT" {
			t.Fatalf("%s: %+v %v", job, got, err)
		}
	}
	profit, err := f.module.GetJobProfit(ctx, api.GetJobProfit{TenantID: f.tenant, JobID: "JOB-A"})
	// USD 1,000.00 at ESTIMATE 7.1234 = CNY 7,123.40, the CNY 300.00 fee; cost USD 500.00 = CNY 3,561.70
	if err != nil || profit.Revenue != cny(742_340) || profit.Cost != cny(356_170) || profit.Profit != cny(386_170) {
		t.Fatalf("the sea job's estimate: %+v %v", profit, err)
	}
	note := func(id, job string, h int, lines ...int) api.IssueDebitNote {
		cmd := b.debit(id, job, h, "party-a", "CNY", lines...)
		cmd.ExpectedRevision = revision[job]
		return cmd
	}
	attempt := func(cmd api.IssueDebitNote) error {
		_, err := f.module.IssueDebitNote(ctx, cmd)
		return err
	}
	setDocument := func(shipment string, d *docapi.TransportDocumentSnapshot) {
		f.w.mu.Lock()
		defer f.w.mu.Unlock()
		if d == nil {
			delete(f.w.documents, shipment)
			return
		}
		f.w.documents[shipment] = *d
	}
	// the longest numbers: an HBL number of 16 characters and a master bill number of 35
	hbl := seaDocument(docapi.KindHBL, "SH-A", "SHAL000000000001", f.hours(3))
	hbl.MasterNumber = strings.Repeat("M", 35)
	dnA := note("DN-A", "JOB-A", 5, 1, 2)

	// no house document yet: DOC does not know the shipment
	rejected("no house document", attempt(dnA), "FIN_NO_ISSUED_DOCUMENT")
	// a version that is not issued is not billed on, whatever DOC answers
	for _, status := range []string{docapi.VersionDraft, docapi.VersionApproved} {
		d := hbl
		d.VersionStatus = status
		setDocument("SH-A", &d)
		rejected("an HBL "+status, attempt(dnA), "FIN_NO_ISSUED_DOCUMENT")
	}
	// a document of another shipment or of another legal entity
	other := hbl
	other.ShipmentID = "SH-B"
	setDocument("SH-A", &other)
	rejected("a document of another shipment", attempt(dnA), "FIN_NO_ISSUED_DOCUMENT")
	other = hbl
	other.LegalEntityID = "LE-2"
	setDocument("SH-A", &other)
	rejected("a document of another legal entity", attempt(dnA), "FIN_NO_ISSUED_DOCUMENT")
	// a note whose business time is before the document's issue time. FIN asks DOC for the version valid at that
	// instant, and the stand-in, like DOC, answers not found for it: this case does not reach FIN's own comparison of
	// the note's time with the document's issue time, which a document DOC answers can never fail
	late := hbl
	late.IssuedAt = f.hours(6)
	setDocument("SH-A", &late)
	rejected("a bill issued after the note's business time", attempt(dnA), "FIN_NO_ISSUED_DOCUMENT")
	if job, err := f.module.GetFreightJob(ctx, api.GetFreightJob{TenantID: f.tenant, JobID: "JOB-A"}); err != nil || job.Revision != revision["JOB-A"] || len(job.Notes) != 0 {
		t.Fatalf("the refused notes left the job as it was: %+v %v", job, err)
	}

	// the house bill of lading, issued: the note freezes what DOC answered
	setDocument("SH-A", &hbl)
	r, err := f.module.IssueDebitNote(ctx, dnA)
	if err != nil || r.Number != "SEADN000001" || r.SubjectID != "JOB-A" {
		t.Fatalf("debit note on the HBL: %+v %v", r, err)
	}
	revision["JOB-A"] = r.NewRevision
	got, err := f.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: f.tenant, InvoiceID: "DN-A"})
	// USD 1,000.00 at INVOICE 7.2000 = CNY 7,200.00, and CNY 300.00
	if err != nil || got.Total != cny(750_000) || got.ShipmentID != "SH-A" || got.State != api.NoteIssued ||
		got.Document != (api.DocumentRef{DocumentID: "DOC-SH-A", Version: 1, Number: "SHAL000000000001", MasterNumber: strings.Repeat("M", 35), IssuedAt: f.hours(3)}) {
		t.Fatalf("the note on the HBL: %+v %v", got, err)
	}

	// a house sea waybill of the second shipment, with its own number under the same master bill
	waybill := seaDocument(docapi.KindHouseSeaWaybill, "SH-B", "SHAL0000002", f.hours(4))
	setDocument("SH-B", &waybill)
	r, err = f.module.IssueDebitNote(ctx, note("DN-B", "JOB-B", 5, 1))
	if err != nil || r.Number != "SEADN000002" || r.SubjectID != "JOB-B" {
		t.Fatalf("debit note on the sea waybill: %+v %v", r, err)
	}
	revision["JOB-B"] = r.NewRevision
	got, err = f.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: f.tenant, InvoiceID: "DN-B"})
	if err != nil || got.Total != cny(720_000) || got.ShipmentID != "SH-B" ||
		got.Document != (api.DocumentRef{DocumentID: "DOC-SH-B", Version: 1, Number: "SHAL0000002", MasterNumber: "LINU2611000457", IssuedAt: f.hours(4)}) {
		t.Fatalf("the note on the sea waybill: %+v %v", got, err)
	}

	// the first note is unchanged by the second
	if first, err := f.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: f.tenant, InvoiceID: "DN-A"}); err != nil || first.Document.Number != "SHAL000000000001" ||
		first.Number != "SEADN000001" {
		t.Fatalf("the first note after the second: %+v %v", first, err)
	}

	// What follows the note does not depend on the document's kind, and this part says so: a credit note against the
	// ocean debit note, a receipt allocated to it, and the debtor's statement, which shows the note under the HBL's
	// number, kept at its full length.
	b.job = revision["JOB-A"]
	cr, err := f.module.IssueCreditNote(ctx, b.credit("CN-A", "DN-A", 7, 2))
	if err != nil || cr.Number != "SEACN000001" {
		t.Fatalf("a credit note against the ocean debit note: %+v %v", cr, err)
	}
	// the note's CNY 7,500.00 less the credited fee of CNY 300.00 is CNY 7,200.00; the second note, of the same debtor
	// for the other shipment, stays open
	received := b.received(t, b.receipt("R-A", "party-a", 8, cny(720_000), cny(0), api.EvidenceBank, "BOC-20261112-0001"))
	if _, err = f.module.AllocateDebtorReceipt(ctx, b.allocation("alloc-A", "R-A", received, 9, to("DN-A", cny(720_000)))); err != nil {
		t.Fatalf("the receipt allocated to the ocean debit note: %v", err)
	}
	settled, err := f.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: f.tenant, InvoiceID: "DN-A"})
	if err != nil || settled.Open != cny(0) || settled.Settled != cny(720_000) || len(settled.Settlements) != 1 || settled.Document.Number != "SHAL000000000001" {
		t.Fatalf("the settled ocean debit note: %+v %v", settled, err)
	}
	statement, err := f.module.GetDebtorStatement(ctx, api.GetDebtorStatement{TenantID: f.tenant, LegalEntityID: "LE", DebtorPartyID: "party-a", From: b.date(-48), To: b.date(24)})
	if err != nil || len(statement.Currencies) != 1 || statement.Currencies[0].Closing != cny(720_000) || len(statement.Currencies[0].OpenItems) != 1 ||
		statement.Currencies[0].OpenItems[0].InvoiceID != "DN-B" || statement.Currencies[0].OpenItems[0].DocumentNumber != "SHAL0000002" {
		t.Fatalf("the statement: %+v %v", statement, err)
	}
	kinds := map[string]int{}
	for _, e := range statement.Currencies[0].Entries {
		kinds[e.Kind]++
		if e.ID == "DN-A" && (e.Kind != api.EntryDebitNote || e.Number != "SEADN000001" || e.DocumentNumber != "SHAL000000000001" || e.Amount != cny(750_000)) {
			t.Fatalf("the debit note's entry: %+v", e)
		}
	}
	if kinds[api.EntryDebitNote] != 2 || kinds[api.EntryCreditNote] != 1 || kinds[api.EntryReceipt] != 1 {
		t.Fatalf("the statement's entries: %+v", statement.Currencies[0].Entries)
	}
}
