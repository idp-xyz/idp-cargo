//go:build integration

package integration

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

func (b *billing) fapiao(id, debtor, kind string, h int, notes ...string) api.RequestFapiao {
	return api.RequestFapiao{CommandID: "request-" + id, TenantID: b.tenant, ActorID: "bill-1", FapiaoID: id, LegalEntityID: "LE", DebtorPartyID: debtor,
		Type: kind, InvoiceIDs: notes, OccurredAt: b.hours(h)}
}

func (b *billing) fapiaoRecord(t *testing.T, id string) api.FapiaoRecord {
	t.Helper()
	r, err := b.module.GetFapiaoRecord(context.Background(), api.GetFapiaoRecord{TenantID: b.tenant, FapiaoID: id})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (b *billing) fapiaoIssued(id string, expected int64, h int, number string) api.RecordFapiaoIssued {
	return api.RecordFapiaoIssued{CommandID: "issued-" + id + "-" + number, TenantID: b.tenant, ActorID: "bill-2", FapiaoID: id, ExpectedRevision: expected,
		Fapiao: api.FapiaoEvidence{Number: number, Date: b.date(h), EvidenceRef: "sha256:" + number}, OccurredAt: b.hours(h)}
}

// Fapiao records cover issued debit notes of party-a with the buyer data of its billing profile: lines per tax
// treatment and rate with their VAT, disbursements left off; a covered note is neither covered again nor credited until
// its fapiao fails or is red-lettered; numbers are recorded once per legal entity; and the database refuses changes
// past FIN.
func TestFapiaoRecordsOnPostgres(t *testing.T) {
	ctx := context.Background()
	b := billedTwice(t)
	rejected := func(name string, err error, code string) {
		t.Helper()
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), code) {
			t.Fatalf("%s: %v, want %s", name, err, code)
		}
	}
	// a disbursement billed to party-a: DN-3 bills line 4, CNY 150.00 of duty paid for it
	r, err := b.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "line-duty", TenantID: b.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: b.job, ChargeCode: "DEST_FEE", Sell: &api.ChargeSide{PartyID: "party-a", Amount: cny(15_000)}, Reason: "duty paid for the customer",
		OccurredAt: b.hours(4)})
	if err != nil {
		t.Fatal(err)
	}
	b.job = r.NewRevision
	b.issued(t, b.debit("DN-3", "JOB-A", 5, "party-a", "CNY", 4))

	_, err = b.module.RequestFapiao(ctx, b.fapiao("FP-S", "party-a", api.FapiaoSpecial, 6, "DN-1"))
	rejected("a special fapiao on exempt freight", err, "FIN_NOT_FAPIAOABLE")
	_, err = b.module.RequestFapiao(ctx, b.fapiao("FP-O", "party-a", api.FapiaoOrdinary, 6, "DN-1", "DN-2"))
	rejected("another debtor's note", err, "FIN_NOT_FAPIAOABLE")
	_, err = b.module.RequestFapiao(ctx, b.fapiao("FP-N", "agent-ny", api.FapiaoOrdinary, 6, "DN-2"))
	rejected("a foreign debtor", err, "FIN_NOT_FAPIAOABLE")
	_, err = b.module.RequestFapiao(ctx, b.fapiao("FP-D", "party-a", api.FapiaoSpecial, 6, "DN-3"))
	rejected("a disbursement only", err, "FIN_NOT_FAPIAOABLE")
	if _, err = b.module.RequestFapiao(ctx, b.fapiao("FP-X", "party-a", api.FapiaoOrdinary, 6, "DN-1", "DN-X")); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown note: %v", err)
	}
	if _, err = b.module.RequestFapiao(ctx, b.fapiao("FP-E", "party-a", api.FapiaoOrdinary, 6)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("no notes: %v", err)
	}
	if _, err = b.module.RequestFapiao(ctx, b.fapiao("FP-T", "party-a", api.FapiaoOrdinary, 6, "DN-1", "DN-1")); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "named twice") {
		t.Fatalf("a note named twice: %v", err)
	}
	b.w.mu.Lock()
	renamed := b.w.identities["party-a"]
	renamed.LegalName = "上海甲国际贸易有限公司"
	b.w.identities["party-a"] = renamed
	b.w.mu.Unlock()
	_, err = b.module.RequestFapiao(ctx, b.fapiao("FP-R", "party-a", api.FapiaoOrdinary, 6, "DN-1"))
	rejected("a profile naming the debtor as ORG did before", err, "FIN_BILLING_PROFILE_STALE")
	b.w.mu.Lock()
	renamed.LegalName = "上海甲贸易有限公司"
	b.w.identities["party-a"] = renamed
	b.w.mu.Unlock()

	request := b.fapiao("FP-1", "party-a", api.FapiaoOrdinary, 6, "DN-3", "DN-1")
	if r, err = b.module.RequestFapiao(ctx, request); err != nil || r.SubjectID != "FP-1" || r.NewRevision != 1 {
		t.Fatalf("requested: %+v %v", r, err)
	}
	if again, err := b.module.RequestFapiao(ctx, request); err != nil || again != r {
		t.Fatalf("replayed: %+v %v", again, err)
	}
	changed := request
	changed.Type = api.FapiaoSpecial
	if _, err = b.module.RequestFapiao(ctx, changed); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the command id reused: %v", err)
	}
	f := b.fapiaoRecord(t, "FP-1")
	// 8,640.00 exempt freight; 300.00 at 6% with VAT 16.98 (30,000 × 600 / 10,600 = 1,698.11); the 150.00 duty left off
	if f.State != api.FapiaoRecordRequested || f.Revision != 1 || f.Gross != cny(894_000) || f.VAT != cny(1_698) || len(f.Lines) != 2 ||
		f.Lines[0] != (api.FapiaoLine{Treatment: api.TaxExemptIntlFreight, Gross: cny(864_000), Net: cny(864_000), VAT: cny(0)}) ||
		f.Lines[1] != (api.FapiaoLine{Treatment: api.TaxTaxable, RateBP: 600, Gross: cny(30_000), Net: cny(28_302), VAT: cny(1_698)}) ||
		len(f.Excluded) != 1 || f.Excluded[0].Amount != cny(15_000) || len(f.Notes) != 2 || f.Notes[0].InvoiceID != "DN-1" || len(f.Notes[0].Amounts) != 2 ||
		f.Notes[1].Number != "DN000003" || f.BuyerTitle != "上海甲贸易有限公司" || f.TaxpayerNumber != "91310000MA1FL8TQ2X" ||
		f.BankAccount != "4567 8901 2345" || f.ProfileVersion != 1 || f.DebtorRevision != 4 || f.Issued != nil {
		t.Fatalf("FP-1: %+v", f)
	}
	_, err = b.module.RequestFapiao(ctx, b.fapiao("FP-2", "party-a", api.FapiaoOrdinary, 6, "DN-1"))
	rejected("a note covered", err, "FIN_FAPIAO_COVERS")
	_, err = b.module.IssueCreditNote(ctx, b.credit("CN-1", "DN-1", 6, 2))
	rejected("a covered note credited", err, "FIN_FAPIAO_COVERS")

	early := b.fapiaoIssued("FP-1", 1, 7, "26312000000012345678")
	early.Fapiao.Date = b.at.AddDate(0, 0, -1).Format("2006-01-02")
	_, err = b.module.RecordFapiaoIssued(ctx, early)
	rejected("a fapiao dated before its notes", err, "dated before debit note")
	short := b.fapiaoIssued("FP-1", 1, 7, "1234567")
	_, err = b.module.RecordFapiaoIssued(ctx, short)
	rejected("a short number", err, "8 to 20 digits")
	if _, err = b.module.RecordFapiaoIssued(ctx, b.fapiaoIssued("FP-1", 2, 7, "26312000000012345678")); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a stale revision: %v", err)
	}
	if r, err = b.module.RecordFapiaoIssued(ctx, b.fapiaoIssued("FP-1", 1, 7, "26312000000012345678")); err != nil || r.NewRevision != 2 {
		t.Fatalf("issued: %+v %v", r, err)
	}
	if f = b.fapiaoRecord(t, "FP-1"); f.State != api.FapiaoRecordIssued || f.Issued == nil || f.Issued.Number != "26312000000012345678" ||
		f.Issued.Date != b.date(7) || f.Issued.RecordedBy != "bill-2" || f.Notes[0].Released {
		t.Fatalf("FP-1 issued: %+v", f)
	}
	_, err = b.module.RecordFapiaoFailed(ctx, api.RecordFapiaoFailed{CommandID: "fail-FP-1", TenantID: b.tenant, ActorID: "bill-2", FapiaoID: "FP-1",
		ExpectedRevision: 2, Reason: "too late", OccurredAt: b.hours(7)})
	rejected("an issued fapiao failing", err, "is ISSUED")
	_, err = b.module.IssueCreditNote(ctx, b.credit("CN-1", "DN-1", 7, 2))
	rejected("an issued note credited", err, "FIN_FAPIAO_COVERS")
	red := api.RecordRedLetter{CommandID: "red-FP-1", TenantID: b.tenant, ActorID: "bill-2", FapiaoID: "FP-1", ExpectedRevision: 2,
		RedLetter: api.FapiaoEvidence{Number: "26312000000012345678", Date: b.date(8), EvidenceRef: "sha256:red"}, ConfirmationRef: "HZ-2026-0001",
		Reason: "document fee billed in error", OccurredAt: b.hours(8)}
	_, err = b.module.RecordRedLetter(ctx, red)
	rejected("a red letter with the fapiao's number", err, "its own number")
	red.CommandID, red.RedLetter.Number = "red-FP-1-b", "26312000000012349999"
	if r, err = b.module.RecordRedLetter(ctx, red); err != nil || r.NewRevision != 3 {
		t.Fatalf("red letter: %+v %v", r, err)
	}
	if f = b.fapiaoRecord(t, "FP-1"); f.State != api.FapiaoRecordRedLettered || f.RedLetter == nil || f.RedLetter.ConfirmationRef != "HZ-2026-0001" ||
		f.RedReason != "document fee billed in error" || !f.Notes[0].Released || !f.Notes[1].Released {
		t.Fatalf("FP-1 red-lettered: %+v", f)
	}

	// the document fee credited once the red letter is recorded; a new record covers what DN-1 still bills
	if r, err = b.module.IssueCreditNote(ctx, b.credit("CN-1", "DN-1", 9, 2)); err != nil {
		t.Fatal(err)
	}
	b.job = r.NewRevision
	if _, err = b.module.RequestFapiao(ctx, b.fapiao("FP-3", "party-a", api.FapiaoOrdinary, 9, "DN-1")); err != nil {
		t.Fatal(err)
	}
	if f = b.fapiaoRecord(t, "FP-3"); f.Gross != cny(864_000) || f.VAT != cny(0) || len(f.Lines) != 1 || len(f.Notes[0].Amounts) != 1 {
		t.Fatalf("FP-3: %+v", f)
	}
	for _, number := range []string{"26312000000012345678", "26312000000012349999"} {
		_, err = b.module.RecordFapiaoIssued(ctx, b.fapiaoIssued("FP-3", 1, 10, number))
		rejected("a number recorded already", err, "FIN_FAPIAO_NUMBER_USED")
	}
	if _, err = b.module.RecordFapiaoFailed(ctx, api.RecordFapiaoFailed{CommandID: "fail-FP-3", TenantID: b.tenant, ActorID: "bill-2", FapiaoID: "FP-3",
		ExpectedRevision: 1, Reason: "the buyer asked for a special fapiao", OccurredAt: b.hours(10)}); err != nil {
		t.Fatal(err)
	}
	if f = b.fapiaoRecord(t, "FP-3"); f.State != api.FapiaoRecordFailed || f.FailReason != "the buyer asked for a special fapiao" || !f.Notes[0].Released {
		t.Fatalf("FP-3 failed: %+v", f)
	}
	// a note credited in full is no longer one to put on a fapiao
	if r, err = b.module.IssueCreditNote(ctx, b.credit("CN-2", "DN-1", 10, 1)); err != nil {
		t.Fatal(err)
	}
	b.job = r.NewRevision
	_, err = b.module.RequestFapiao(ctx, b.fapiao("FP-4", "party-a", api.FapiaoOrdinary, 10, "DN-1"))
	rejected("a cancelled note", err, "FIN_NOT_FAPIAOABLE")
	if records, err := b.module.ListFapiaoRecords(ctx, api.ListFapiaoRecords{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: "party-a"}); err != nil ||
		len(records) != 2 || records[0].FapiaoID != "FP-1" || records[1].FapiaoID != "FP-3" || records[0].State != api.FapiaoRecordRedLettered {
		t.Fatalf("records: %+v %v", records, err)
	}
	counts := map[string]int{}
	for _, kind := range b.outboxTypes(t) {
		counts[kind]++
	}
	if counts["FapiaoIssued"] != 1 || counts["FapiaoRedLettered"] != 1 {
		t.Errorf("outbox: %v", counts)
	}

	// DN-4 bills a document fee of 100.00 that FP-5 covers, requested
	if r, err = b.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "line-doc", TenantID: b.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: b.job, ChargeCode: "DOC_FEE", Sell: &api.ChargeSide{PartyID: "party-a", Amount: cny(10_000)}, Reason: "second document set",
		OccurredAt: b.hours(10)}); err != nil {
		t.Fatal(err)
	}
	b.job = r.NewRevision
	b.issued(t, b.debit("DN-4", "JOB-A", 10, "party-a", "CNY", 5))
	if _, err = b.module.RequestFapiao(ctx, b.fapiao("FP-5", "party-a", api.FapiaoSpecial, 10, "DN-4")); err != nil {
		t.Fatal(err)
	}
	if f = b.fapiaoRecord(t, "FP-5"); f.Type != api.FapiaoSpecial || f.Gross != cny(10_000) || f.VAT != cny(566) {
		t.Fatalf("FP-5: %+v", f)
	}

	record := `INSERT INTO cargo_fin.fapiao_record (tenant_id,fapiao_id,legal_entity_id,debtor_party_id,debtor_revision,fapiao_type,currency,profile_version,
        buyer_title,taxpayer_number,buyer_address,buyer_phone,bank_name,bank_account,gross_minor,vat_minor,state,requested_by,requested_at,entered_at)
        SELECT $1,'FP-9','LE','party-a',4,'ORDINARY','CNY',1,title,taxpayer_number,registered_address,registered_phone,bank_name,bank_account,GROSS,0,
        'REQUESTED','x',now(),now() FROM cargo_fin.billing_profile_version WHERE tenant_id=$1 AND debtor_party_id='party-a' AND version_no=1;
INSERT INTO cargo_fin.fapiao_covered_note (tenant_id,fapiao_id,invoice_id) VALUES ($1,'FP-9','DN-3');
INSERT INTO cargo_fin.fapiao_covered_amount (tenant_id,fapiao_id,invoice_id,treatment,rate_bp,local_minor) VALUES ($1,'FP-9','DN-3','TREATMENT',0,15000)`
	for name, tc := range map[string]struct {
		sql, state, check string
	}{
		"a record's buyer rewritten":    {`UPDATE cargo_fin.fapiao_record SET buyer_title='x' WHERE tenant_id=$1 AND fapiao_id='FP-1'`, "42501", ""},
		"a record deleted":              {`DELETE FROM cargo_fin.fapiao_record WHERE tenant_id=$1 AND fapiao_id='FP-1'`, "42501", ""},
		"a failed record issued after":  {`UPDATE cargo_fin.fapiao_record SET state='ISSUED' WHERE tenant_id=$1 AND fapiao_id='FP-3'`, "23001", ""},
		"a released note covered again": {`UPDATE cargo_fin.fapiao_covered_note SET released=false WHERE tenant_id=$1 AND fapiao_id='FP-3'`, "23001", ""},
		"an amount rewritten":           {`UPDATE cargo_fin.fapiao_covered_amount SET local_minor=1 WHERE tenant_id=$1`, "42501|23001", ""},
		"a note added to an old record": {`INSERT INTO cargo_fin.fapiao_covered_note (tenant_id,fapiao_id,invoice_id) VALUES ($1,'FP-3','DN-3')`, "23514",
			"fapiao_written_once"},
		"a number without its record": {`INSERT INTO cargo_fin.fapiao_number (tenant_id,legal_entity_id,number,fapiao_id,kind) VALUES ($1,'LE','11112222','FP-3','ISSUED')`,
			"23514", "fapiao_written_once"},
		"totals not the notes'": {strings.ReplaceAll(strings.Replace(record, "GROSS", "100", 1), "TREATMENT", "PASS_THROUGH_DISBURSEMENT"), "23514",
			"fapiao_record_consistent"},
		"amounts not the note's": {strings.ReplaceAll(strings.Replace(record, "GROSS", "15000", 1), "TREATMENT", "EXEMPT_INTL_FREIGHT_FORWARDING"), "23514",
			"fapiao_covers_issued_notes"},
		"a covered note credited": {`SET CONSTRAINTS cargo_fin.debtor_invoice_fapiao IMMEDIATE;
INSERT INTO cargo_fin.debtor_invoice (tenant_id,invoice_id,kind,job_id,legal_entity_id,number,number_seq,debtor_party_id,debtor_revision,profile_version,
        currency,invoice_date,credits_invoice_id,reason,total_minor,local_total_minor,state,issued_by,issued_at,entered_at) VALUES ($1,'CN-9','CREDIT_NOTE',
        'JOB-A','LE','CN900009',9,'party-a',4,1,'CNY',current_date,'DN-4','x',-10000,-10000,'ISSUED','x',now(),now())`, "23514", "fapiao_covers_debit_note"},
	} {
		statements := strings.Split(tc.sql, ";\n")
		err := b.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range statements {
				var args []any
				if strings.Contains(s, "$1") {
					args = append(args, b.tenant)
				}
				if _, err := e.Exec(ctx, s, args...); err != nil {
					return err
				}
			}
			return nil
		})
		state, constraint := sqlState(err)
		if state == "" || !strings.Contains(tc.state, state) || tc.check != "" && constraint != tc.check {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, state, constraint, tc.state, tc.check)
		}
	}
}

// fapiaoRace runs two commands at once on a fresh billedTwice world, eight rounds, and requires exactly one to commit;
// lost checks the loser's error. One side starts up to 3 ms later, the first in odd rounds and the second in even ones,
// so that each wins some rounds although one of them asks ORG before its transaction.
func fapiaoRace(t *testing.T, first, second func(b *billing) error, lost func(round int, firstLost bool, err error)) {
	t.Helper()
	var losses [2]int
	for round := range 8 {
		b := billedTwice(t)
		var errs [2]error
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		for i, fn := range []func(*billing) error{first, second} {
			go func() {
				defer wg.Done()
				<-start
				if i == round%2 {
					time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
				}
				errs[i] = fn(b)
			}()
		}
		close(start)
		wg.Wait()
		switch {
		case (errs[0] == nil) == (errs[1] == nil):
			t.Fatalf("round %d: %v / %v", round, errs[0], errs[1])
		case errs[0] != nil:
			losses[0]++
			lost(round, true, errs[0])
		default:
			losses[1]++
			lost(round, false, errs[1])
		}
	}
	t.Logf("the first lost %d rounds, the second %d", losses[0], losses[1])
}

// Two fapiao requests on DN-1 at once: one covers it, the other hears FIN_FAPIAO_COVERS.
func TestFapiaoRequestsRaceForOneNote(t *testing.T) {
	request := func(id string) func(b *billing) error {
		return func(b *billing) error {
			_, err := b.module.RequestFapiao(context.Background(), b.fapiao(id, "party-a", api.FapiaoOrdinary, 6, "DN-1"))
			return err
		}
	}
	fapiaoRace(t, request("FP-1"), request("FP-2"), func(round int, _ bool, err error) {
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_FAPIAO_COVERS") {
			t.Fatalf("round %d: %v", round, err)
		}
	})
}

// A fapiao request on DN-1 races a credit note reversing all of DN-1: the request first leaves the credit note refused
// (FIN_FAPIAO_COVERS); the credit note first leaves nothing to put on a fapiao (FIN_NOT_FAPIAOABLE).
func TestFapiaoRequestRacesACreditNote(t *testing.T) {
	request := func(b *billing) error {
		_, err := b.module.RequestFapiao(context.Background(), b.fapiao("FP-1", "party-a", api.FapiaoOrdinary, 6, "DN-1"))
		return err
	}
	credit := func(b *billing) error {
		_, err := b.module.IssueCreditNote(context.Background(), b.credit("CN-1", "DN-1", 6, 1, 2))
		return err
	}
	fapiaoRace(t, request, credit, func(round int, requestLost bool, err error) {
		want := "FIN_FAPIAO_COVERS"
		if requestLost {
			want = "FIN_NOT_FAPIAOABLE"
		}
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), want) {
			t.Fatalf("round %d: %v, want %s", round, err, want)
		}
	})
}
