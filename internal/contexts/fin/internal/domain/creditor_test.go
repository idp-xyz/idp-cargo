package domain

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// accountingPolicy is policy with an ACCOUNTING rate for USD: 7.2000 (OD-5 B, a monthly rate).
func accountingPolicy(t *testing.T) *FinancePolicy {
	t.Helper()
	p := policy(t)
	if err := p.RecordRate(RateEntry{Type: RateAccounting, From: "USD", Rate: 72_000, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: t0.AddDate(0, 0, -1),
		ValidUntil: t0.AddDate(0, 1, 0), Source: "monthly rate 2026-11"}, "acct-1", hour(0)); err != nil {
		t.Fatal(err)
	}
	return p
}

// jobAccrual is a job's cost side of 950.00 USD (6,767.23 CNY at the ESTIMATE rate) naming no creditor; poolAccrual a
// pool line of 1,000.00 USD (7,123.40 CNY) of CARRIER-1.
func jobAccrual() AccrualTarget {
	return AccrualTarget{Kind: TargetJobCost, ID: "JOB-1", LineNo: 1, LegalEntityID: "LE", Open: true, ChargeCode: "AIR_FREIGHT", Version: 1,
		Amount: usd(95_000), Local: 676_723, Status: StatusAccrued}
}

func poolAccrual() AccrualTarget {
	return AccrualTarget{Kind: TargetPoolCost, ID: "POOL-1", LineNo: 1, LegalEntityID: "LE", Open: true, ChargeCode: "AIR_FREIGHT",
		CreditorPartyID: "CARRIER-1", Version: 1, Amount: usd(100_000), Local: 712_340, Status: StatusAccrued}
}

func invoiceInput(lines ...CreditorLineInput) CreditorInvoiceInput {
	return CreditorInvoiceInput{ID: "CI-1", LegalEntityID: "LE", CreditorPartyID: "CARRIER-1", CreditorRevision: 3, SupplierInvoiceNo: "MU-2026-0042",
		InvoiceDate: t0.Truncate(24 * time.Hour), Currency: "USD", TaxDocumentKind: TaxDocForeignInvoice, TaxDocumentNumber: "DN 42", Lines: lines}
}

func recorded(t *testing.T, lines ...CreditorLineInput) *CreditorInvoice {
	t.Helper()
	c, err := RecordCreditorInvoice("T", invoiceInput(lines...), "ap-1", hour(4))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func to(line int, target AccrualTarget, amount int64, final bool) MatchItem {
	return MatchItem{LineNo: line, Target: target.Key(), Amount: amount, Final: final}
}

func TestRecordCreditorInvoiceChecks(t *testing.T) {
	line := CreditorLineInput{ChargeCode: "AIR_FREIGHT", Amount: 60_000}
	for name, change := range map[string]func(*CreditorInvoiceInput){
		"a number ending with a space": func(in *CreditorInvoiceInput) { in.SupplierInvoiceNo = "MU-42 " },
		"a date after the day":         func(in *CreditorInvoiceInput) { in.InvoiceDate = in.InvoiceDate.AddDate(0, 0, 2) },
		"a date over a year back":      func(in *CreditorInvoiceInput) { in.InvoiceDate = in.InvoiceDate.AddDate(0, 0, -368) },
		"a date that is no date":       func(in *CreditorInvoiceInput) { in.InvoiceDate = hour(1) },
		"a tax kind without a number":  func(in *CreditorInvoiceInput) { in.TaxDocumentNumber = "" },
		"an unknown tax kind":          func(in *CreditorInvoiceInput) { in.TaxDocumentKind = "RECEIPT" },
		"no lines":                     func(in *CreditorInvoiceInput) { in.Lines = nil },
		"a line of nothing":            func(in *CreditorInvoiceInput) { in.Lines = []CreditorLineInput{{ChargeCode: "X", Amount: 0}} },
		"a negative line":              func(in *CreditorInvoiceInput) { in.Lines = []CreditorLineInput{{ChargeCode: "X", Amount: -5}} },
		"an unknown currency":          func(in *CreditorInvoiceInput) { in.Currency = "XYZ" },
		"no creditor revision":         func(in *CreditorInvoiceInput) { in.CreditorRevision = 0 },
		"lines beyond the bound": func(in *CreditorInvoiceInput) {
			in.Lines = []CreditorLineInput{{ChargeCode: "X", Amount: MaxAmount}, {ChargeCode: "Y", Amount: 1}}
		},
	} {
		in := invoiceInput(line)
		change(&in)
		if _, err := RecordCreditorInvoice("T", in, "ap-1", hour(4)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	c := recorded(t, line, CreditorLineInput{ChargeCode: "FUEL", SupplierRef: "784-12345675", Amount: 40_000})
	if st := c.State(); st.State != CreditorRecorded || st.Total() != 100_000 || len(st.Lines) != 2 || st.Lines[1].No != 2 || st.Lines[1].SupplierRef != "784-12345675" {
		t.Fatalf("recorded: %+v", st)
	}
	if ev := c.Pending(); len(ev) != 1 || ev[0].Type != "CreditorInvoiceRecorded.v1" || ev[0].CreditorPartyID != "CARRIER-1" {
		t.Fatalf("events: %+v", ev)
	}
}

// A match: every line in full, each accrual once; a match not final consumes within the open accrual; a final match
// carries the variance against the accrual in both currencies; matches convert at the ACCOUNTING rate.
func TestMatchCreditorInvoice(t *testing.T) {
	p := accountingPolicy(t)
	lines := []CreditorLineInput{{ChargeCode: "AIR_FREIGHT", Amount: 60_000}, {ChargeCode: "FUEL", Amount: 40_000}}
	job, pool := jobAccrual(), poolAccrual()
	targets := []AccrualTarget{job, pool}
	fuel := to(2, job, 40_000, true)
	fuel.Reason = "the carrier bills the fuel the quote's freight included"

	for name, tc := range map[string]struct {
		items   []MatchItem
		targets []AccrualTarget
		want    error
	}{
		"a line short":                     {[]MatchItem{to(1, pool, 59_999, false), fuel}, targets, ErrMatchIncomplete},
		"an accrual twice":                 {[]MatchItem{to(1, pool, 30_000, false), {LineNo: 1, Target: pool.Key(), Amount: 30_000}, fuel}, targets, ErrMatchIncomplete},
		"another charge code, no reason":   {[]MatchItem{to(1, pool, 60_000, false), to(2, job, 40_000, true)}, targets, ErrMatchChargeCode},
		"more than the accrual has open":   {[]MatchItem{{LineNo: 1, Target: pool.Key(), Amount: 60_000}, fuel}, withConsumed(targets, 1, 50_000), ErrMatchExceeds},
		"an accrual in another currency":   {[]MatchItem{to(1, pool, 60_000, false), fuel}, withCurrency(targets, 1, "CNY"), ErrMatchCurrency},
		"an accrual of another creditor":   {[]MatchItem{to(1, pool, 60_000, false), fuel}, withCreditor(targets, 1, "CARRIER-2"), ErrMatchCreditor},
		"an accrual matched final":         {[]MatchItem{to(1, pool, 60_000, false), fuel}, withFinal(targets, 1), ErrAccrualClosed},
		"an accrual released":              {[]MatchItem{to(1, pool, 60_000, false), fuel}, withStatus(targets, 1, StatusReleased), ErrAccrualClosed},
		"an accrual of another entity":     {[]MatchItem{to(1, pool, 60_000, false), fuel}, withEntity(targets, 1, "LE2"), ErrMatchTarget},
		"an accrual of a closed pool":      {[]MatchItem{to(1, pool, 60_000, false), fuel}, withClosed(targets, 1), ErrMatchTarget},
		"an accrual FIN does not know":     {[]MatchItem{to(1, pool, 60_000, false), fuel}, targets[:1], ErrMatchTarget},
		"a match of nothing":               {[]MatchItem{to(1, pool, 0, false), fuel}, targets, ErrInvalid},
		"a target of no kind":              {[]MatchItem{{LineNo: 1, Target: TargetKey{Kind: "SHARE", ID: "X", LineNo: 1}, Amount: 60_000}, fuel}, targets, ErrInvalid},
		"a line the invoice does not have": {[]MatchItem{to(3, pool, 60_000, false), fuel}, targets, ErrInvalid},
	} {
		c := recorded(t, lines...)
		if err := c.Match(tc.items, tc.targets, p, "ap-1", hour(5)); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}

	c := recorded(t, lines...)
	if err := c.Match([]MatchItem{to(1, pool, 60_000, false), fuel}, targets, p, "ap-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	st := c.State()
	if st.State != CreditorMatched || st.MatchedBy != "ap-1" || !st.ApprovalRequired || st.ThresholdNo != 0 || len(st.Matches) != 2 {
		t.Fatalf("matched: %+v", st)
	}
	m1, m2 := st.Matches[0], st.Matches[1]
	if m1.No != 1 || m1.Target != pool.Key() || m1.TargetVersion != 1 || m1.Final || m1.Local != 432_000 || m1.FX == nil || m1.FX.Type != RateAccounting ||
		m1.AccrualAmount != 100_000 || m1.AccrualLocal != 712_340 || m1.Variance != 0 || m1.VarianceLocal != 0 {
		t.Fatalf("match 1: %+v", m1)
	}
	if m2.No != 2 || !m2.Final || m2.Local != 288_000 || m2.Variance != -55_000 || m2.VarianceLocal != 288_000-676_723 || m2.Reason == "" {
		t.Fatalf("match 2: %+v", m2)
	}
	if err := c.Match([]MatchItem{to(1, pool, 60_000, false), fuel}, targets, p, "ap-1", hour(5)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("matched twice: %v", err)
	}

	// no ACCOUNTING rate for the currency: refused, never another rate type
	eur := invoiceInput(CreditorLineInput{ChargeCode: "AIR_FREIGHT", Amount: 1_000})
	eur.Currency = "EUR"
	ce, err := RecordCreditorInvoice("T", eur, "ap-1", hour(4))
	if err != nil {
		t.Fatal(err)
	}
	if err = ce.Match([]MatchItem{to(1, pool, 1_000, true)}, withCurrency(targets, 1, "EUR"), p, "ap-1", hour(5)); !errors.Is(err, ErrRateMissing) {
		t.Fatalf("no ACCOUNTING rate for EUR: %v", err)
	}
}

func withConsumed(ts []AccrualTarget, i int, amount int64) []AccrualTarget {
	out := append([]AccrualTarget(nil), ts...)
	out[i].Consumed = Consumption{Amount: amount, Local: amount * 72 / 10}
	return out
}

func withCurrency(ts []AccrualTarget, i int, c kernel.CurrencyCode) []AccrualTarget {
	out := append([]AccrualTarget(nil), ts...)
	out[i].Amount.Currency = c
	return out
}

func withCreditor(ts []AccrualTarget, i int, creditor string) []AccrualTarget {
	out := append([]AccrualTarget(nil), ts...)
	out[i].CreditorPartyID = creditor
	return out
}

func withFinal(ts []AccrualTarget, i int) []AccrualTarget {
	out := append([]AccrualTarget(nil), ts...)
	out[i].Consumed = Consumption{Amount: 1, Local: 7, Final: true}
	return out
}

func withStatus(ts []AccrualTarget, i int, status string) []AccrualTarget {
	out := append([]AccrualTarget(nil), ts...)
	out[i].Status = status
	return out
}

func withEntity(ts []AccrualTarget, i int, entity string) []AccrualTarget {
	out := append([]AccrualTarget(nil), ts...)
	out[i].LegalEntityID = entity
	return out
}

func withClosed(ts []AccrualTarget, i int) []AccrualTarget {
	out := append([]AccrualTarget(nil), ts...)
	out[i].Open = false
	return out
}

// OD-9 B: a final variance needs approval beyond the greater of the threshold's amount and its rate of the accrual's
// local amount; without a threshold, any variance does; a variance of exchange only is a variance too.
func TestVarianceThreshold(t *testing.T) {
	p := accountingPolicy(t)
	small := AccrualTarget{Kind: TargetJobCost, ID: "JOB-1", LineNo: 2, LegalEntityID: "LE", Open: true, ChargeCode: "AIR_FREIGHT", Version: 1,
		Amount: usd(10_000), Local: 71_234, Status: StatusAccrued}
	match := func(amount int64) *CreditorInvoice {
		c := recorded(t, CreditorLineInput{ChargeCode: "AIR_FREIGHT", Amount: amount})
		if err := c.Match([]MatchItem{to(1, small, amount, true)}, []AccrualTarget{small}, p, "ap-1", hour(5)); err != nil {
			t.Fatal(err)
		}
		return c
	}
	// none set: the exchange difference alone (72,000 - 71,234 = 766) needs approval
	if c := match(10_000); !c.State().ApprovalRequired || c.State().Matches[0].VarianceLocal != 766 || c.State().Matches[0].Variance != 0 {
		t.Fatalf("no threshold: %+v", c.State().Matches[0])
	}
	if err := p.SetVarianceThreshold(10_000, 100, "acct-1", hour(3)); err != nil {
		t.Fatal(err)
	}
	if err := p.SetVarianceThreshold(5_000, 50, "acct-1", hour(2)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a threshold set before the one in force: %v", err)
	}
	for _, bad := range []struct {
		amount int64
		bp     int
	}{{-1, 0}, {0, 10_001}, {MaxAmount + 1, 0}, {0, -1}} {
		if err := p.SetVarianceThreshold(bad.amount, bad.bp, "acct-1", hour(3)); !errors.Is(err, ErrInvalid) {
			t.Errorf("threshold %+v: %v", bad, err)
		}
	}
	if c := match(10_000); c.State().ApprovalRequired || c.State().ThresholdNo != 1 {
		t.Fatalf("766 within the greater of 10,000 and 1%% of 71,234: %+v", c.State())
	}
	if c := match(12_000); !c.State().ApprovalRequired || c.State().Matches[0].VarianceLocal != 86_400-71_234 {
		t.Fatalf("15,166 beyond 10,000: %+v", c.State())
	}
	if p.ThresholdAt(hour(2)) != nil || p.ThresholdAt(hour(3)).No != 1 {
		t.Fatal("the threshold is in force from its time")
	}
	limit := &VarianceThreshold{Amount: 0, RateBP: 10_000}
	if Exceeded(limit, MaxAmount, MaxAmount) || !Exceeded(limit, -MaxAmount-1, MaxAmount) || Exceeded(nil, 0, 5) || !Exceeded(nil, -1, 5) {
		t.Fatal("Exceeded at the bounds")
	}
}

// The approver is not the matcher unless allowed to approve their own; disputes interrupt and return; a matched invoice
// is disputed before it is cancelled; disputes and cancellations need the accruals' pools and jobs open.
func TestApprovalDisputeAndCancel(t *testing.T) {
	p := accountingPolicy(t)
	pool := poolAccrual()
	matched := func() *CreditorInvoice {
		c := recorded(t, CreditorLineInput{ChargeCode: "AIR_FREIGHT", Amount: 110_000})
		if err := c.Match([]MatchItem{to(1, pool, 110_000, true)}, []AccrualTarget{pool}, p, "ap-1", hour(5)); err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := matched()
	if err := c.ApproveVariance("carrier's surcharge confirmed", false, "ap-1", hour(6)); !errors.Is(err, ErrVarianceApproval) {
		t.Fatalf("the matcher approving their own: %v", err)
	}
	if err := c.ApproveVariance("", false, "ctrl-1", hour(6)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an approval without a reason: %v", err)
	}
	if err := c.ApproveVariance("carrier's surcharge confirmed", true, "ap-1", hour(6)); err != nil {
		t.Fatal(err)
	}
	if st := c.State(); !st.SelfApproved || st.AwaitingApproval() || st.ApprovedBy != "ap-1" {
		t.Fatalf("self approval: %+v", st)
	}
	if err := c.ApproveVariance("again", false, "ctrl-1", hour(7)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("approved twice: %v", err)
	}
	c = matched()
	if err := c.ApproveVariance("checked", false, "ctrl-1", hour(6)); err != nil || c.State().SelfApproved {
		t.Fatalf("another person approves: %v %+v", err, c.State())
	}

	c = matched()
	if err := c.Cancel("wrong", []AccrualTarget{pool}, "ap-1", hour(6)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a matched invoice cancelled: %v", err)
	}
	if err := c.Dispute("rate disputed", withClosed([]AccrualTarget{pool}, 0), "ap-1", hour(6)); !errors.Is(err, ErrTargetClosed) {
		t.Fatalf("a dispute while the pool is closed: %v", err)
	}
	if err := c.Dispute("rate disputed", []AccrualTarget{pool}, "ap-1", hour(6)); err != nil {
		t.Fatal(err)
	}
	if err := c.ApproveVariance("x", false, "ctrl-1", hour(7)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("an approval while disputed: %v", err)
	}
	if err := c.ResolveDispute("carrier corrected nothing; the rate stands", "ap-1", hour(7)); err != nil || c.State().State != CreditorMatched {
		t.Fatalf("resolved: %v %+v", err, c.State())
	}
	if err := c.Dispute("rate disputed again", []AccrualTarget{pool}, "ap-1", hour(8)); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreCreditorInvoice(c.State())
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.ResolveDispute("credit note promised", "ap-1", hour(8)); err != nil {
		t.Fatal(err)
	}
	if _, disputes, resolved := restored.Added(); len(disputes) != 0 || len(resolved) != 1 || resolved[0].No != 2 {
		t.Fatalf("a stored dispute resolved: %+v %+v", disputes, resolved)
	}
	if err = c.Cancel("the carrier withdrew the invoice", withClosed([]AccrualTarget{pool}, 0), "ap-1", hour(9)); !errors.Is(err, ErrTargetClosed) {
		t.Fatalf("cancelled while the pool is closed: %v", err)
	}
	if err = c.Cancel("the carrier withdrew the invoice", []AccrualTarget{pool}, "ap-1", hour(9)); err != nil {
		t.Fatal(err)
	}
	if st := c.State(); st.State != CreditorCancelled || len(st.Disputes) != 2 || !st.Disputes[1].Open() {
		t.Fatalf("cancelled while disputed: %+v", st)
	}
	if err = c.ResolveDispute("x", "ap-1", hour(9)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a cancelled invoice changes: %v", err)
	}
	var kinds []string
	for _, e := range c.Pending() {
		kinds = append(kinds, e.Type)
	}
	if len(kinds) != 6 || kinds[5] != "CreditorInvoiceCancelled.v1" || kinds[2] != "CreditorInvoiceDisputed.v1" || kinds[3] != "CreditorInvoiceDisputeResolved.v1" {
		t.Fatalf("events: %v", kinds)
	}
	if err = c.Dispute("x", nil, "ap-1", hour(3)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a change before the invoice's last: %v", err)
	}
}

func TestAccrualCost(t *testing.T) {
	part := Consumption{Amount: 40_000, Local: 288_000}
	if s := AccrualStatus(StatusAccrued, Consumption{}); s != StatusAccrued {
		t.Fatal(s)
	}
	if s := AccrualStatus(StatusAccrued, part); s != StatusPartlyMatched {
		t.Fatal(s)
	}
	if s := AccrualStatus(StatusAccrued, Consumption{Amount: 1, Final: true}); s != StatusMatched {
		t.Fatal(s)
	}
	if s := AccrualStatus(StatusReleased, part); s != StatusReleased {
		t.Fatal(s)
	}
	// 288,000 matched plus 55,000 of 95,000 USD still accrued at the estimate: 676,723 × 55/95 = 391,787
	if amount, local := CurrentCost(95_000, 676_723, StatusAccrued, part); amount != 95_000 || local != 679_787 {
		t.Fatalf("partly matched: %d %d", amount, local)
	}
	if amount, local := CurrentCost(95_000, 676_723, StatusAccrued, Consumption{Amount: 99_000, Local: 712_800, Final: true}); amount != 99_000 || local != 712_800 {
		t.Fatalf("matched final above the accrual: %d %d", amount, local)
	}
	if amount, local := CurrentCost(0, 0, StatusReleased, part); amount != 40_000 || local != 288_000 {
		t.Fatalf("released: %d %d", amount, local)
	}
	if amount, local := CurrentCost(0, 0, StatusAccrued, Consumption{}); amount != 0 || local != 0 || Open(0, StatusAccrued, Consumption{}) != 0 {
		t.Fatalf("a zero accrual: %d %d", amount, local)
	}
}

// A job's cost side: revisions stay above what invoices consume and in their currency; a release ends the accrual; a
// job with matched or released cost sides is not cancelled; its profit splits matched and accrued cost.
func TestJobAccruals(t *testing.T) {
	p := accountingPolicy(t)
	j := job(t, p)
	if err := j.RecordLine("AIR_FREIGHT", "", &SideInput{PartyID: "party-a", PartyRevision: 3, Amount: cny(800_000)},
		&SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: usd(95_000)}, "booked", p, "fin-1", hour(2)); err != nil {
		t.Fatal(err)
	}
	j.Matched(map[int]Consumption{1: {Amount: 40_000, Local: 288_000}})
	if j.CostStatus(1) != StatusPartlyMatched {
		t.Fatal(j.CostStatus(1))
	}
	pr := j.Profit()
	if pr.Cost != 679_787 || pr.Matched != 288_000 || pr.Accrued != 391_787 || pr.Profit != 800_000-679_787 {
		t.Fatalf("profit: %+v", pr)
	}
	if err := j.ReviseSide(1, SideCost, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: usd(39_999)}, "lower", p, "fin-1", hour(3)); !errors.Is(err, ErrAccrualMatched) {
		t.Fatalf("below what is matched: %v", err)
	}
	if err := j.ReviseSide(1, SideCost, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(700_000)}, "in CNY", p, "fin-1", hour(3)); !errors.Is(err, ErrAccrualMatched) {
		t.Fatalf("into another currency once matched: %v", err)
	}
	if err := j.ReviseSide(1, SideCost, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: usd(50_000)}, "carrier's rate fell", p, "fin-1", hour(3)); err != nil {
		t.Fatal(err)
	}
	if err := j.Cancel("shipment cancelled", "fin-1", hour(4)); !errors.Is(err, ErrAccrualMatched) {
		t.Fatalf("a job with a matched cost side cancelled: %v", err)
	}
	if err := j.ReleaseCost(1, 1, "nothing more", "fin-1", hour(4)); !errors.Is(err, ErrSideVersion) {
		t.Fatalf("a release at the wrong version: %v", err)
	}
	if err := j.ReleaseCost(1, 2, "no further invoice: the carrier confirmed", "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	cost := j.State().Lines[0].Cost
	if v := cost[2]; v.No != 3 || v.Status != StatusReleased || v.Amount != usd(0) || v.Local != 0 || v.FX != nil || v.PartyID != "CARRIER-1" {
		t.Fatalf("release: %+v", v)
	}
	if j.CostStatus(1) != StatusReleased || j.Profit().Cost != 288_000 || j.Profit().Accrued != 0 {
		t.Fatalf("released: %s %+v", j.CostStatus(1), j.Profit())
	}
	if err := j.ReviseSide(1, SideCost, 3, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: usd(1)}, "x", p, "fin-1", hour(5)); !errors.Is(err, ErrAccrualClosed) {
		t.Fatalf("revised after its release: %v", err)
	}
	if err := j.ReleaseCost(1, 3, "again", "fin-1", hour(5)); !errors.Is(err, ErrAccrualClosed) {
		t.Fatalf("released twice: %v", err)
	}
	j.Matched(nil) // the invoice was cancelled: the side stays released
	if err := j.Cancel("shipment cancelled", "fin-1", hour(5)); !errors.Is(err, ErrAccrualMatched) {
		t.Fatalf("a job with a released cost side cancelled: %v", err)
	}
	if _, _, sides, _ := j.Added(); len(sides) != 4 { // sell v1, cost v1 to v3
		t.Fatalf("added side versions: %d", len(sides))
	}
	if ev := j.Pending(); ev[len(ev)-1].Type != "AccrualReleased.v1" || ev[len(ev)-1].LineNo != 1 || ev[len(ev)-1].Side != SideCost || ev[len(ev)-1].Version != 3 {
		t.Fatalf("release event: %+v", ev[len(ev)-1])
	}
	if _, err := RestoreJob(j.State()); err != nil {
		t.Fatal(err)
	}
}

// A pool: revisions above what invoices consume, releases, apportionment of each line's current cost, closing once
// every line is final and apportioned on the current loaded basis, reopening.
func TestPoolMatchingAndClosing(t *testing.T) {
	p := pool(t, airBasis())
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodChargeable)
	recordCost(t, p, "CFS", cny(30_000), MethodPieces)
	recordCost(t, p, "SCREENING", cny(5_000), MethodPieces)
	p.Matched(map[int]Consumption{1: {Amount: 60_000, Local: 60_000}})
	if p.LineStatus(1) != StatusPartlyMatched || p.LineStatus(2) != StatusAccrued {
		t.Fatal(p.LineStatus(1), p.LineStatus(2))
	}
	if err := p.ReviseCost(1, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(59_999)}, "lower", policy(t), "ops-1", hour(4)); !errors.Is(err, ErrAccrualMatched) {
		t.Fatalf("below what is matched: %v", err)
	}
	if err := p.ReviseCost(1, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: usd(10_000)}, "in USD", policy(t), "ops-1", hour(4)); !errors.Is(err, ErrAccrualMatched) {
		t.Fatalf("into another currency once matched: %v", err)
	}
	if err := p.Close(airBasis(), "ops-1", hour(4)); !errors.Is(err, ErrPoolNotClosable) {
		t.Fatalf("closed while partly matched: %v", err)
	}
	// the carrier's final invoice: 85,000 in all; the CFS line released; the screening line never apportioned and released
	p.Matched(map[int]Consumption{1: {Amount: 85_000, Local: 85_000, Final: true}})
	if err := p.ReviseCost(1, 1, SideInput{Amount: cny(90_000)}, "x", policy(t), "ops-1", hour(4)); !errors.Is(err, ErrAccrualClosed) {
		t.Fatalf("revised once matched final: %v", err)
	}
	if err := p.ReleaseCost(1, 1, "x", "ops-1", hour(4)); !errors.Is(err, ErrAccrualClosed) {
		t.Fatalf("released once matched final: %v", err)
	}
	if err := p.Apportion(ApportionInput{LineNos: []int{1, 2}, Jobs: jobs()}, airBasis(), "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if err := p.ReleaseCost(2, 1, "the CFS waived its fee", "ops-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	if err := p.ReleaseCost(3, 1, "screened by the carrier", "ops-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	if v := p.State().Lines[1].Versions[1]; v.Status != StatusReleased || v.Amount != cny(0) || v.CreditorPartyID != "CARRIER-1" || p.LineStatus(2) != StatusReleased {
		t.Fatalf("released line: %+v", v)
	}
	_, la, _ := p.SharesOfLine(1)
	if la.Amount != 85_000 || la.Local != 85_000 || la.Shares[0].Amount != 51_000 || la.Shares[1].Amount != 34_000 {
		t.Fatalf("line 1 apportioned at its final cost 3:2: %+v", la)
	}
	// line 2 was apportioned at 30,000 before its release: its final cost is zero, so it is apportioned again
	if err := p.Close(airBasis(), "ops-1", hour(6)); !errors.Is(err, ErrPoolNotClosable) {
		t.Fatalf("closed with line 2 apportioned at 30,000: %v", err)
	}
	if err := p.Apportion(ApportionInput{LineNos: []int{2}, Jobs: jobs()}, airBasis(), "ops-1", hour(6)); err != nil {
		t.Fatal(err)
	}
	held := map[int]Consumption{1: {Amount: 85_000, Local: 85_000, Final: true, Held: true}}
	p.Matched(held)
	if err := p.Close(airBasis(), "ops-1", hour(6)); !errors.Is(err, ErrPoolNotClosable) {
		t.Fatalf("closed while an invoice is disputed or awaits approval: %v", err)
	}
	final := map[int]Consumption{1: {Amount: 85_000, Local: 85_000, Final: true}}
	p.Matched(final)
	for name, change := range map[string]func(*CostBasis){
		"a house not loaded": func(b *CostBasis) { b.Houses[1].LoadedPacks = 0 },
		"a house added": func(b *CostBasis) {
			b.Houses = append(b.Houses, HouseFacts{ShipmentID: "HS3", LoadedPacks: 1, LoadedGrossWeight: kg(1)})
		},
		"a house re-weighed":   func(b *CostBasis) { b.Houses[0].LoadedGrossWeight = kg(320) },
		"a house in its place": func(b *CostBasis) { b.Houses[1].ShipmentID = "HS9" },
	} {
		b := airBasis()
		b.Houses = append([]HouseFacts(nil), b.Houses...)
		change(&b)
		if err := p.Close(b, "ops-1", hour(6)); !errors.Is(err, ErrPoolNotClosable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	other := airBasis()
	other.ConsolidationID = "CONSOL-9"
	if err := p.Close(other, "ops-1", hour(6)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("another consolidation's basis: %v", err)
	}
	// once PHY cancelled the consolidation (or no house is left), the apportionments at the final costs stand
	for _, gone := range []func(*CostBasis){func(b *CostBasis) { b.State = "CANCELLED" }, func(b *CostBasis) { b.Houses = nil }} {
		b := airBasis()
		gone(&b)
		q, err := RestorePool(p.State())
		if err != nil {
			t.Fatal(err)
		}
		q.Matched(final)
		if err = q.Close(b, "ops-1", hour(7)); err != nil {
			t.Fatalf("closed without a basis left: %v", err)
		}
	}
	if err := p.Close(airBasis(), "ops-1", hour(7)); err != nil {
		t.Fatal(err)
	}
	if st := p.State(); st.State != PoolClosed || len(st.Closures) != 1 || st.Closures[0].By != "ops-1" {
		t.Fatalf("closed: %+v", st)
	}
	if err := p.RecordCost("X", "", "", MethodPieces, SideInput{Amount: cny(1)}, "late", policy(t), "ops-1", hour(8)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a closed pool takes a cost: %v", err)
	}
	restored, err := RestorePool(p.State())
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.Reopen("", "ops-1", hour(8)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reopened without a reason: %v", err)
	}
	if err = restored.Reopen("the carrier's late terminal charge (OD-10 A)", "ops-1", hour(8)); err != nil {
		t.Fatal(err)
	}
	if closures, reopened := restored.AddedClosures(); len(closures) != 0 || len(reopened) != 1 || restored.State().State != PoolOpen {
		t.Fatalf("a stored closure reopened: %+v %+v", closures, reopened)
	}
	if err = restored.Reopen("again", "ops-1", hour(9)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("an open pool reopened: %v", err)
	}
	var kinds []string
	for _, e := range p.Pending() {
		kinds = append(kinds, e.Type)
	}
	if kinds[len(kinds)-1] != "ConsolCostPoolClosed.v1" || kinds[len(kinds)-3] != "AccrualReleased.v1" {
		t.Fatalf("events: %v", kinds)
	}
	// a manual apportionment closes on the houses alone, whatever they weigh now
	m := pool(t, airBasis())
	recordCost(t, m, "AIRFREIGHT", cny(100_000), MethodChargeable)
	m.Matched(map[int]Consumption{1: {Amount: 100_000, Local: 100_000, Final: true}})
	if err := m.Apportion(ApportionInput{LineNos: []int{1}, Method: MethodManual, Reason: "agreed with the co-loader",
		Manual: []ManualShare{{ShipmentID: "HS1", Amount: 70_000}, {ShipmentID: "HS2", Amount: 30_000}}, Jobs: jobs()}, airBasis(), "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	reweighed := airBasis()
	reweighed.Houses = append([]HouseFacts(nil), reweighed.Houses...)
	reweighed.Houses[0].LoadedGrossWeight = kg(500)
	if err := m.Close(reweighed, "ops-1", hour(5)); err != nil {
		t.Fatal(err)
	}
}

// An invoice matched in part is not cancelled behind another invoice's final match of the accrual (whose variance
// counted it); it is once that invoice is cancelled, or when the accrual was released instead.
func TestCancelBehindAFinalMatch(t *testing.T) {
	p := accountingPolicy(t)
	job := jobAccrual()
	c := recorded(t, CreditorLineInput{ChargeCode: "AIR_FREIGHT", Amount: 60_000})
	if err := c.Match([]MatchItem{to(1, job, 60_000, false)}, []AccrualTarget{job}, p, "ap-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	if err := c.Dispute("the trucker withdrew it", []AccrualTarget{job}, "ap-1", hour(6)); err != nil {
		t.Fatal(err)
	}
	behind := job
	behind.Consumed = Consumption{Amount: 95_000, Local: 684_000, Final: true}
	if err := c.Cancel("withdrawn", []AccrualTarget{behind}, "ap-1", hour(7)); !errors.Is(err, ErrFinalBehind) {
		t.Fatalf("cancelled behind a final match: %v", err)
	}
	released := job
	released.Status, released.Consumed = StatusReleased, Consumption{Amount: 60_000, Local: 432_000}
	if err := c.Cancel("withdrawn", []AccrualTarget{released}, "ap-1", hour(7)); err != nil {
		t.Fatalf("cancelled on a released accrual: %v", err)
	}
}

// Once invoices consume part of an accrual it names no creditor or theirs.
func TestMatchedAccrualKeepsItsCreditor(t *testing.T) {
	p := accountingPolicy(t)
	j := job(t, p)
	if err := j.RecordLine("AIR_FREIGHT", "", nil, &SideInput{Amount: usd(95_000)}, "estimate", p, "fin-1", hour(2)); err != nil {
		t.Fatal(err)
	}
	j.Matched(map[int]Consumption{1: {Amount: 40_000, Local: 288_000, Creditors: []string{"CARRIER-1"}}})
	if err := j.ReviseSide(1, SideCost, 1, SideInput{PartyID: "CARRIER-2", PartyRevision: 1, Amount: usd(95_000)}, "x", p, "fin-1", hour(3)); !errors.Is(err, ErrAccrualMatched) {
		t.Fatalf("another creditor named: %v", err)
	}
	if err := j.ReviseSide(1, SideCost, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: usd(95_000)}, "the carrier", p, "fin-1", hour(3)); err != nil {
		t.Fatal(err)
	}
	pl := pool(t, airBasis())
	recordCost(t, pl, "AIRFREIGHT", cny(100_000), MethodChargeable)
	pl.Matched(map[int]Consumption{1: {Amount: 10_000, Local: 10_000, Creditors: []string{"CARRIER-1"}}})
	if err := pl.ReviseCost(1, 1, SideInput{PartyID: "CARRIER-2", PartyRevision: 1, Amount: cny(100_000)}, "x", p, "ops-1", hour(4)); !errors.Is(err, ErrAccrualMatched) {
		t.Fatalf("another creditor named on a pool line: %v", err)
	}
}
