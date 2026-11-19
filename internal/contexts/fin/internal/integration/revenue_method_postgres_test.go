//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// revenueWorld is usedWorld with all three houses of the air consolidation loaded (the close needs the ACTUAL stage),
// the
// USD rates (ESTIMATE 7.1234, INVOICE 7.2000, ACCOUNTING 7.1000), the tax treatments and the DN series a debit note
// needs, and the billing profile of party-a.
func revenueWorld(t *testing.T) (usedWorld, *billing) {
	t.Helper()
	u := newUsedWorld(t)
	f := u.fixture
	f.w.costBases["CONSOL-U"] = phyapi.PhysicalCostBasis{TenantID: f.tenant, ConsolidationID: "CONSOL-U", Revision: 7, Mode: "AIR", Kind: "AIR_CONSOL",
		OperatingLegalEntityID: "LE", State: "DEPARTED", Houses: []phyapi.HouseCostBasis{
			ownHouse(f, "SH-A", 4, 300, 1_200_000, 4, 300, 1_200_000),
			ownHouse(f, "SH-B", 2, 100, 1_200_000, 2, 100, 1_200_000),
			ownHouse(f, "SH-C", 3, 50, 600_000, 3, 50, 600_000)}}
	b := &billing{fixture: f, policy: 1}
	b.rate(t, "rate-estimate", api.RateEstimate, 71_234)
	b.rate(t, "rate-invoice", api.RateInvoice, 72_000)
	b.rate(t, "rate-accounting", api.RateAccounting, 71_000)
	from := f.at.AddDate(0, 0, -5).Format(time.DateOnly)
	b.policyChange(t)(b.tax("tax-freight", "AIR_FREIGHT", api.TaxExemptIntlFreight, 0, from))
	b.policyChange(t)(b.tax("tax-doc", "DOC_FEE", api.TaxTaxable, 600, from))
	b.policyChange(t)(b.series("series-dn", api.KindDebitNote, "DN{seq:6}", 1))
	if _, err := b.profile("profile-party-a", "party-a", api.BuyerCNEnterprise, 0); err != nil {
		t.Fatal(err)
	}
	return u, b
}

// sellOn records a sell line of party-a on a job (at hour 2) and answers the job's revision after it.
func (u usedWorld) sellOn(job, code string, amount api.ChargeSide) int64 {
	u.t.Helper()
	return u.must(u.module.RecordChargeLine(u.ctx, api.RecordChargeLine{CommandID: fmt.Sprintf("sell-%s-%s-%d", job, code, amount.Amount.Minor),
		TenantID: u.tenant, ActorID: "fin-1", JobID: job, ExpectedRevision: u.job(job).Revision, ChargeCode: code, Sell: &amount, Reason: "sold as per booking",
		OccurredAt: u.hours(2)})).NewRevision
}

func party(amount api.ChargeSide) api.ChargeSide {
	amount.PartyID = "party-a"
	return amount
}

// reviseSell revises the first sell side of a job's line 1 to CNY amount at an hour.
func (u usedWorld) reviseSell(id, job string, version int, amount int64, hour int) api.Receipt {
	u.t.Helper()
	return u.must(u.module.ReviseChargeSide(u.ctx, api.ReviseChargeSide{CommandID: id, TenantID: u.tenant, ActorID: "fin-1", JobID: job,
		ExpectedRevision: u.job(job).Revision, LineNo: 1, Side: api.SideSell, ExpectedSideVersion: version, Value: party(api.ChargeSide{Amount: cny(amount)}),
		Reason: "price agreed again", OccurredAt: u.hours(hour)}))
}

// Apportioning by revenue on PostgreSQL (V2.81). Three houses of an air consolidation, loaded, whose jobs sell under
// AIR_FREIGHT: JOB-A USD 1,200.00 (at the ESTIMATE rate 7.1234 CNY 8,548.08, that is 854,808 minor), JOB-B CNY 3,000.00
// and JOB-C CNY 500.00; JOB-B also sells DOC_FEE CNY 999.99, which a line of AIR_FREIGHT does not count. Line 1
// AIR_FREIGHT CNY 1,000.01 and line 2 DOC_FEE CNY 100.00 are recorded with REVENUE as their default method.
//
//	line 1: 100,001 over 854,808 : 300,000 : 50,000 is 70,950.44 / 24,900.48 / 4,150.08; the floors leave one minor unit,
//	  to SH-B (.48 is the largest remainder) -> 70,950 / 24,901 / 4,150
//	line 2: only JOB-B sells DOC_FEE: 10,000 over 0 : 99,999 : 0 -> 0 / 10,000 / 0, the others with a zero share
//	line 3 STORAGE, which no job sells: refused (FIN_POOL_NO_BASIS) and nothing is stored
//
// The jobs carry the shares and the consolidation's profit agrees. A debit note for JOB-A at the INVOICE rate (7.2000,
// CNY 8,640.00 for the USD line) changes the profit of the job and not the revenue the apportionment used: the pool
// closes once the creditor invoice is matched. A sell side revised, a sell line added under the code and a sell side
// revised to zero each refuse the close (FIN_POOL_NOT_CLOSABLE) until the lines are apportioned again, and then it
// closes:
//
//	JOB-C revised to 600.00:    100,001 over 854,808 : 300,000 : 60,000 -> 70,366.39 / 24,695.51 / 4,939.10 -> 70,366 /
//	24,696 / 4,939
//	JOB-B sells CNY 600.00 more under the code on another line: 360,000 for SH-B (its shares add up to the line)
//	JOB-C revised to zero:      a zero share for SH-C, and 854,808 : 360,000 : 0 for the houses
func TestApportionmentByRevenueOnPostgres(t *testing.T) {
	u, b := revenueWorld(t)
	f, ctx := u.fixture, u.ctx
	u.sellOn("JOB-A", "AIR_FREIGHT", party(api.ChargeSide{Amount: usd(120_000)}))
	u.sellOn("JOB-B", "AIR_FREIGHT", party(api.ChargeSide{Amount: cny(300_000)}))
	u.sellOn("JOB-B", "DOC_FEE", party(api.ChargeSide{Amount: cny(99_999)}))
	u.sellOn("JOB-C", "AIR_FREIGHT", party(api.ChargeSide{Amount: cny(50_000)}))
	u.record("cost-1", "AIR_FREIGHT", api.MethodRevenue, 100_001)
	u.record("cost-2", "DOC_FEE", api.MethodRevenue, 10_000)
	u.record("cost-3", "STORAGE", api.MethodRevenue, 5_000)
	if p := u.ap.pool(u.pool); len(p.Lines) != 3 || p.Lines[0].Method != api.MethodRevenue || p.Lines[2].Method != api.MethodRevenue {
		t.Fatalf("lines recorded with REVENUE as their default: %+v", p.Lines)
	}

	// a line no job sells has no basis; the others are not affected by being named separately
	before := u.revision()
	_, err := f.module.ApportionConsolCosts(ctx, u.apportionCmd("no-basis", 4, []int{3}))
	u.refusedWith("a code no job sells", err, "FIN_POOL_NO_BASIS")
	if err != nil && !strings.Contains(err.Error(), "STORAGE") {
		t.Errorf("the refusal does not name the charge code: %v", err)
	}
	if u.revision() != before || len(u.ap.pool(u.pool).Apportionments) != 0 {
		t.Fatalf("a refused apportionment changed the pool")
	}
	// every line of the command by the command's method: the same refusal while line 3 is among them
	_, err = f.module.ApportionConsolCosts(ctx, u.apportionCmd("no-basis-all", 4, nil))
	u.refusedWith("every line, one of a code no job sells", err, "FIN_POOL_NO_BASIS")

	cmd := u.apportionCmd("apportion-1", 4, []int{1, 2})
	r := u.must(f.module.ApportionConsolCosts(ctx, cmd))
	one := u.ap.pool(u.pool).Apportionments[0]
	if one.Stage != api.BasisActual || len(one.Lines) != 2 {
		t.Fatalf("apportionment 1: %+v", one)
	}
	u.checkLine("line 1 by revenue", one, 1, api.MethodRevenue, share(1, "A", 854_808, "REV", 70_950), share(2, "B", 300_000, "REV", 24_901), share(3, "C", 50_000, "REV", 4_150))
	u.checkLine("line 2 by revenue", one, 2, api.MethodRevenue, share(1, "A", 0, "REV", 0), share(2, "B", 99_999, "REV", 10_000), share(3, "C", 0, "REV", 0))
	if again, err := f.module.ApportionConsolCosts(ctx, cmd); err != nil || again != r {
		t.Fatalf("replay: %+v %v, want %+v", again, err, r)
	}
	if n := len(u.ap.pool(u.pool).Apportionments); n != 1 {
		t.Fatalf("the replay stored another apportionment: %d", n)
	}

	// each job carries its shares; the profit of the consolidation agrees
	for job, want := range map[string]int64{"JOB-A": 70_950, "JOB-B": 24_901 + 10_000, "JOB-C": 4_150} {
		if got := u.poolCost(job); got != want {
			t.Errorf("pool cost of %s is %d, want %d", job, got, want)
		}
	}
	if s := u.poolShares("JOB-B"); len(s) != 2 || s[0].Method != api.MethodRevenue || s[0].Quantity != 300_000 || s[0].Unit != "REV" || s[0].Stage != api.BasisActual ||
		s[1].Quantity != 99_999 || s[1].Amount != cny(10_000) {
		t.Fatalf("JOB-B's shares: %+v", s)
	}
	consol, err := f.module.GetConsolProfit(ctx, api.GetConsolProfit{TenantID: f.tenant, ConsolidationID: "CONSOL-U"})
	if err != nil || consol.Pooled != cny(115_001) || consol.Apportioned != cny(110_001) || consol.Unapportioned != cny(5_000) || len(consol.Houses) != 3 ||
		consol.Houses[0].PoolCost != cny(70_950) || consol.Houses[1].PoolCost != cny(34_901) || consol.Houses[2].PoolCost != cny(4_150) {
		t.Fatalf("consol profit: %+v %v", consol, err)
	}
	// line 3 is not by anything the jobs sell: another method takes it, and the pool is whole
	cmd3 := u.apportionCmd("apportion-3", 5, []int{3})
	cmd3.Method = api.MethodShipment
	u.must(f.module.ApportionConsolCosts(ctx, cmd3))

	// a debit note for JOB-A at the INVOICE rate: the job's own revenue follows the invoice, the pool's basis does not
	f.w.mu.Lock()
	f.w.documents["SH-A"] = houseDocument("SH-A", "SHA0000001", f.hours(3))
	f.w.mu.Unlock()
	b.job = u.job("JOB-A").Revision
	b.issued(t, b.debit("DN-A", "JOB-A", 6, "party-a", "CNY", 1))
	if p, err := f.module.GetJobProfit(ctx, api.GetJobProfit{TenantID: f.tenant, JobID: "JOB-A"}); err != nil || p.Revenue != cny(864_000) {
		t.Fatalf("JOB-A's revenue follows the debit note's rate: %+v %v", p, err)
	}
	u.must(f.module.RecordCreditorInvoice(ctx, api.RecordCreditorInvoice{CommandID: "invoice", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-R",
		LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "R-1", InvoiceDate: f.hours(7).Format(time.DateOnly), Currency: "CNY",
		Lines:      []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 100_001}, {ChargeCode: "DOC_FEE", Amount: 10_000}, {ChargeCode: "STORAGE", Amount: 5_000}},
		OccurredAt: f.hours(7)}))
	u.must(f.module.MatchCreditorInvoice(ctx, u.match("CI-R", "ap-1", 8, onPool("P-U", 1, 1, 100_001, true), onPool("P-U", 2, 2, 10_000, true),
		onPool("P-U", 3, 3, 5_000, true))))
	closeCmd := func(id string, hour int) api.CloseConsolCostPool {
		return api.CloseConsolCostPool{CommandID: id, TenantID: f.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(), OccurredAt: f.hours(hour)}
	}
	u.must(f.module.CloseConsolCostPool(ctx, closeCmd("close-1", 9)))
	if p := u.ap.pool(u.pool); p.State != api.PoolClosed {
		t.Fatalf("an invoice issued for a house's job refused the close: %+v", p)
	}

	// the revenue changes: a sell side revised, a sell line added under the code, a sell side revised to zero. The close
	// is refused until the lines are apportioned again.
	hour := 10
	for _, c := range []struct {
		name   string
		change func(h int)
		want   []api.PoolShare
	}{
		{"JOB-C revised to 600.00", func(h int) { u.reviseSell("revise-c", "JOB-C", 1, 60_000, h) },
			[]api.PoolShare{share(1, "A", 854_808, "REV", 70_366), share(2, "B", 300_000, "REV", 24_696), share(3, "C", 60_000, "REV", 4_939)}},
		{"JOB-B sells under the code on another line", func(h int) { u.sellOn("JOB-B", "AIR_FREIGHT", party(api.ChargeSide{Amount: cny(60_000)})) },
			nil},
		{"JOB-C revised to zero", func(h int) { u.reviseSell("revise-c-zero", "JOB-C", 2, 0, h) }, nil},
	} {
		u.must(f.module.ReopenConsolCostPool(ctx, api.ReopenConsolCostPool{CommandID: "reopen-" + c.name, TenantID: f.tenant, ActorID: "fin-1", PoolID: u.pool,
			ExpectedRevision: u.revision(), Reason: "revenue changed", OccurredAt: f.hours(hour)}))
		c.change(hour + 1)
		_, err := f.module.CloseConsolCostPool(ctx, closeCmd("close-refused-"+c.name, hour+2))
		u.refusedWith(c.name+": closed on the old revenue", err, "FIN_POOL_NOT_CLOSABLE")
		if err != nil && (!strings.Contains(err.Error(), "revenue") || !strings.Contains(err.Error(), "apportion it again")) {
			t.Errorf("%s: the refusal does not say that the revenue changed: %v", c.name, err)
		}
		a := u.apportion("again-"+c.name, hour+3, []int{1, 2})
		if c.want != nil {
			u.checkLine(c.name, a, 1, api.MethodRevenue, c.want...)
		}
		var sum int64
		for _, s := range a.Lines[0].Shares {
			sum += s.Amount.Minor
		}
		if sum != 100_001 {
			t.Errorf("%s: the shares add up to %d", c.name, sum)
		}
		u.must(f.module.CloseConsolCostPool(ctx, closeCmd("close-"+c.name, hour+4)))
		if p := u.ap.pool(u.pool); p.State != api.PoolClosed {
			t.Fatalf("%s: apportioned again, the pool does not close: %+v", c.name, p)
		}
		hour += 5
	}
	last := u.ap.pool(u.pool).Apportionments
	last1 := last[len(last)-1].Lines[0].Shares
	if last1[2].Quantity != 0 || last1[2].Amount != cny(0) || last1[1].Quantity != 360_000 {
		t.Fatalf("after the last change: %+v", last1)
	}
}

// What the database refuses and accepts past FIN on the application role (V2.81): a cost line and an apportionment line
// with a method outside the list (NO_SUCH_METHOD) and a share with a unit outside the list (NO_SUCH_UNIT), each written
// as a
// real apportionment is written (the pool row touched in the same transaction, a valid line, a share used) so that only
// the
// named check refuses it; a line by REVENUE with a share of unit REV, written the same way, is accepted and reads back.
func TestRevenueMethodPastFINOnPostgres(t *testing.T) {
	u := containerWorld(t, true)
	f := u.fixture
	u.record("cost-1", "SEA_FREIGHT", api.MethodShipment, 100_000)
	u.apportion("apportion-1", 4, []int{1})

	const bump = `UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-K';
`
	const columns = `(tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,used,quantity,unit,amount_minor,local_minor)`
	// apportionment no of line 1 (CNY 1,000.00 each way) by method, with SH-A's share used in unit and SH-C's not used
	line := func(no int, method, unit string) string {
		return bump + `INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at)
            VALUES ($1,'P-K',` + itoa(no) + `,7,'ACTUAL','x',now(),now());
INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,amount_minor,local_minor)
            VALUES ($1,'P-K',` + itoa(no) + `,1,1,'` + method + `',100000,100000);
INSERT INTO cargo_fin.pool_apportionment_share ` + columns + ` VALUES ($1,'P-K',` + itoa(no) + `,1,1,'SH-A','JOB-A',true,854808,'` + unit + `',100000,100000);
INSERT INTO cargo_fin.pool_apportionment_share ` + columns + ` VALUES ($1,'P-K',` + itoa(no) + `,1,2,'SH-C','JOB-C',false,0,NULL,0,0)`
	}
	costLine := func(no int, method string) string {
		return bump + `INSERT INTO cargo_fin.pool_cost_line (tenant_id,pool_id,line_no,charge_code,method,recorded_by,recorded_at,entered_at)
            VALUES ($1,'P-K',` + itoa(no) + `,'X','` + method + `','x',now(),now());
INSERT INTO cargo_fin.pool_cost_version (tenant_id,pool_id,line_no,version_no,creditor_revision,currency,amount_minor,local_minor,status,recorded_by,
            recorded_at,entered_at) VALUES ($1,'P-K',` + itoa(no) + `,1,0,'CNY',0,0,'ACCRUED','x',now(),now())`
	}
	run := func(sql string) error {
		return f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range strings.Split(sql, ";\n") {
				if _, err := e.Exec(ctx, s, f.tenant); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for name, tc := range map[string]struct{ sql, check string }{
		"a method outside the list on a cost line":           {costLine(2, "NO_SUCH_METHOD"), "pool_cost_line_method_check"},
		"a method outside the list on an apportionment line": {line(2, "NO_SUCH_METHOD", "REV"), "pool_apportionment_line_method_check"},
		"a unit outside the list":                            {line(2, "REVENUE", "NO_SUCH_UNIT"), "pool_apportionment_share_unit_check"},
	} {
		err := run(tc.sql)
		if state, constraint := sqlState(err); state != "23514" || constraint != tc.check {
			t.Errorf("%s: %v (state %s, constraint %s), want 23514 %s", name, err, state, constraint, tc.check)
		}
	}
	// the method and its unit are accepted, and read back
	if err := run(line(2, "REVENUE", "REV")); err != nil {
		t.Fatalf("a line by revenue: %v", err)
	}
	p := u.ap.pool(u.pool)
	if len(p.Apportionments) != 2 {
		t.Fatalf("apportionments: %+v", p.Apportionments)
	}
	la := p.Apportionments[1].Lines[0]
	if la.Method != api.MethodRevenue || la.Shares[0].Unit != "REV" || la.Shares[0].Quantity != 854_808 || la.Shares[0].Amount != cny(100_000) || !la.Shares[1].NotUsed {
		t.Errorf("the apportionment reads back as %+v", la)
	}
	if err := run(costLine(2, "REVENUE")); err != nil {
		t.Fatalf("a cost line by revenue: %v", err)
	}
	if lines := u.ap.pool(u.pool).Lines; len(lines) != 2 || lines[1].Method != api.MethodRevenue {
		t.Fatalf("the cost line reads back as %+v", lines)
	}
}

// An apportionment by REVENUE races the revision of a sell side of one of the houses' jobs, ten rounds. Both commit
// (they
// change different aggregates), and whichever commits second sees the first: the order of the two events in the outbox
// is
// the order of the commits, since each is written after the job's lock is taken and the lock is held to the commit. If
// the
// revision came first the share records the revised revenue, if the apportionment came first the old revenue; never a
// revenue the job did not have at that moment.
func TestApportionByRevenueRacesASellRevision(t *testing.T) {
	ctx := context.Background()
	var revisionFirst, apportionFirst int
	for round := range 10 {
		u, _ := revenueWorld(t)
		f := u.fixture
		u.sellOn("JOB-A", "AIR_FREIGHT", party(api.ChargeSide{Amount: cny(60_000)}))
		u.sellOn("JOB-B", "AIR_FREIGHT", party(api.ChargeSide{Amount: cny(30_000)}))
		u.sellOn("JOB-C", "AIR_FREIGHT", party(api.ChargeSide{Amount: cny(10_000)}))
		u.record("cost-1", "AIR_FREIGHT", api.MethodRevenue, 90_000)
		apportion := u.apportionCmd("apportion", 4, nil)
		revise := api.ReviseChargeSide{CommandID: "revise", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-B", ExpectedRevision: u.job("JOB-B").Revision, LineNo: 1,
			Side: api.SideSell, ExpectedSideVersion: 1, Value: party(api.ChargeSide{Amount: cny(90_000)}), Reason: "price agreed again", OccurredAt: f.hours(4)}
		var apportionErr, reviseErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, apportionErr = f.module.ApportionConsolCosts(ctx, apportion)
		}()
		go func() {
			defer wg.Done()
			<-start
			time.Sleep(time.Duration(round) * 300 * time.Microsecond) // the later rounds let the apportionment get further first
			_, reviseErr = f.module.ReviseChargeSide(ctx, revise)
		}()
		close(start)
		wg.Wait()
		if apportionErr != nil || reviseErr != nil {
			t.Fatalf("round %d: apportion %v / revise %v", round, apportionErr, reviseErr)
		}
		types := f.outboxTypes(t)
		apportioned, revised := slices.Index(types, "ConsolCostsApportioned"), slices.Index(types, "ChargeSideRevised")
		if apportioned < 0 || revised < 0 {
			t.Fatalf("round %d: events %v", round, types)
		}
		got := u.ap.pool(u.pool).Apportionments[0].Lines[0].Shares
		want := int64(30_000)
		if revised < apportioned {
			want = 90_000
			revisionFirst++
		} else {
			apportionFirst++
		}
		if got[1].Quantity != want || got[0].Quantity != 60_000 || got[2].Quantity != 10_000 {
			t.Fatalf("round %d: the revision committed %s the apportionment, so SH-B's quantity should be %d: %+v", round,
				map[bool]string{true: "before", false: "after"}[revised < apportioned], want, got)
		}
		// the shares add up to the line, whichever came first
		if sum := got[0].Amount.Minor + got[1].Amount.Minor + got[2].Amount.Minor; sum != 90_000 {
			t.Fatalf("round %d: shares add up to %d", round, sum)
		}
		// and the close compares with the revenue now: after the apportionment first the pool is not closable on the old
		// figure
		if want == 30_000 {
			u.must(f.module.RecordCreditorInvoice(ctx, api.RecordCreditorInvoice{CommandID: "invoice", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-X",
				LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "X-1", InvoiceDate: f.hours(7).Format(time.DateOnly), Currency: "CNY",
				Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 90_000}}, OccurredAt: f.hours(7)}))
			u.must(f.module.MatchCreditorInvoice(ctx, u.match("CI-X", "ap-1", 8, onPool("P-U", 1, 1, 90_000, true))))
			_, err := f.module.CloseConsolCostPool(ctx, api.CloseConsolCostPool{CommandID: "close", TenantID: f.tenant, ActorID: "fin-1", PoolID: u.pool,
				ExpectedRevision: u.revision(), OccurredAt: f.hours(9)})
			u.refusedWith("the revision came after the apportionment", err, "FIN_POOL_NOT_CLOSABLE")
		}
	}
	t.Logf("%d rounds with the revision first, %d with the apportionment first", revisionFirst, apportionFirst)
}

// jobsPool is revenueWorld with the three jobs selling AIR_FREIGHT (CNY 600.00, 300.00 and 100.00) and one line of
// CNY 900.00 recorded with the given default method and matched final by the carrier's invoice; with apportioned the
// line
// is also apportioned, so that the pool can be closed.
func jobsPool(t *testing.T, method string, apportioned bool) usedWorld {
	t.Helper()
	u, _ := revenueWorld(t)
	f := u.fixture
	u.sellOn("JOB-A", "AIR_FREIGHT", party(api.ChargeSide{Amount: cny(60_000)}))
	u.sellOn("JOB-B", "AIR_FREIGHT", party(api.ChargeSide{Amount: cny(30_000)}))
	u.sellOn("JOB-C", "AIR_FREIGHT", party(api.ChargeSide{Amount: cny(10_000)}))
	u.record("cost-1", "AIR_FREIGHT", method, 90_000)
	u.must(f.module.RecordCreditorInvoice(u.ctx, api.RecordCreditorInvoice{CommandID: "invoice", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-W",
		LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "W-1", InvoiceDate: f.hours(7).Format(time.DateOnly), Currency: "CNY",
		Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 90_000}}, OccurredAt: f.hours(7)}))
	u.must(f.module.MatchCreditorInvoice(u.ctx, u.match("CI-W", "ap-1", 8, onPool("P-U", 1, 1, 90_000, true))))
	if apportioned {
		u.apportion("apportion", 5, nil)
	}
	return u
}

// holdingJob begins a transaction on a connection of its own and takes the row lock a change of the job takes
// (JobStore.Load: SELECT ... FOR NO KEY UPDATE, store.go), which it keeps until release is called.
func holdingJob(t *testing.T, u usedWorld, job string) (release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var one int
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, u.tenant); err == nil {
		err = tx.QueryRow(ctx, `SELECT 1 FROM cargo_fin.freight_job WHERE tenant_id=$1 AND job_id=$2 FOR NO KEY UPDATE`, u.tenant, job).Scan(&one)
	}
	if err != nil {
		t.Fatalf("holding the lock of %s: %v", job, err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback(ctx)
			conn.Close(ctx)
		}
	}
	t.Cleanup(release)
	return release
}

// returnsWithin tells whether a command running in a goroutine has answered within d.
func returnsWithin(done <-chan error, d time.Duration) (error, bool) {
	select {
	case err := <-done:
		return err, true
	case <-time.After(d):
		return nil, false
	}
}

// The close of a pool with a line by REVENUE waits for the locks of the houses' jobs. A second connection holds the row
// lock a change of a job takes (FOR NO KEY UPDATE) on JOB-B; the close started meanwhile must not return. 300 ms is
// safe
// because the figure cannot make a correct close fail: a close that waits for the lock waits however long it is held,
// and the test only looks for an answer within 300 ms, which a close that takes no lock gives in a few milliseconds.
// When the lock is released the close returns and succeeds. The control: a pool whose line is by SHIPMENT closes while
// the lock is held, as it did before this version (it takes no job lock). An apportionment by REVENUE waits the same
// way (no earlier test pins this for apportionments: TestApportionRacesJobCancel races the job's cancellation, not its
// lock).
//
// What it pins, truthfully: that the close and the apportionment wait for a job lock held by someone else. It does not
// show which of the two lock calls does it. The review found that it still passes when OpenJobsFor alone loses its
// lock,
// because the jobs' Load locks the row as well; it fails when both go. The order of the read and the lock is pinned by
// the two tests that follow.
func TestTheCloseByRevenueWaitsForTheJobs(t *testing.T) {
	ctx := context.Background()
	const wait = 300 * time.Millisecond
	closeCmd := func(u usedWorld) api.CloseConsolCostPool {
		return api.CloseConsolCostPool{CommandID: "close", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(), OccurredAt: u.hours(9)}
	}
	start := func(f func() error) <-chan error {
		done := make(chan error, 1)
		go func() { done <- f() }()
		return done
	}

	u := jobsPool(t, api.MethodRevenue, true)
	release := holdingJob(t, u, "JOB-B")
	done := start(func() error { _, err := u.module.CloseConsolCostPool(ctx, closeCmd(u)); return err })
	if err, returned := returnsWithin(done, wait); returned {
		t.Fatalf("the close of a pool with a line by REVENUE returned (%v) while a job's lock was held", err)
	}
	release()
	if err, returned := returnsWithin(done, 10*time.Second); !returned || err != nil {
		t.Fatalf("the close after the lock was released: %v (returned %v)", err, returned)
	}
	if p := u.ap.pool(u.pool); p.State != api.PoolClosed {
		t.Fatalf("the pool: %+v", p)
	}

	// the control: no line by REVENUE, no job lock
	c := jobsPool(t, api.MethodShipment, true)
	releaseC := holdingJob(t, c, "JOB-B")
	done = start(func() error { _, err := c.module.CloseConsolCostPool(ctx, closeCmd(c)); return err })
	if err, returned := returnsWithin(done, 10*time.Second); !returned || err != nil {
		t.Fatalf("a pool without a line by REVENUE did not close while a job's lock was held: %v (returned %v)", err, returned)
	}
	releaseC()

	// an apportionment by REVENUE waits for the lock too, and then divides on the revenue the job has
	a := jobsPool(t, api.MethodRevenue, false)
	releaseA := holdingJob(t, a, "JOB-B")
	done = start(func() error {
		_, err := a.module.ApportionConsolCosts(ctx, a.apportionCmd("apportion-held", 5, nil))
		return err
	})
	if err, returned := returnsWithin(done, wait); returned {
		t.Fatalf("an apportionment by REVENUE returned (%v) while a job's lock was held", err)
	}
	releaseA()
	if err, returned := returnsWithin(done, 10*time.Second); !returned || err != nil {
		t.Fatalf("the apportionment after the lock was released: %v (returned %v)", err, returned)
	}
}

// queuedBehindAHeldJob holds the lock of JOB-B, queues a revision of its sell side (to CNY 900.00) behind it, then the
// command run, and releases the lock once both are queued (400 ms each, so that the second is waiting on the row too:
// a command that waits for a row lock is not racing it). Postgres grants a waiting lock in the order it was asked, so
// the revision commits first. It answers what the two commands returned.
func queuedBehindAHeldJob(t *testing.T, u usedWorld, run func() error) (reviseErr, runErr error) {
	t.Helper()
	ctx := context.Background()
	f := u.fixture
	revise := api.ReviseChargeSide{CommandID: "revise", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-B", ExpectedRevision: u.job("JOB-B").Revision, LineNo: 1,
		Side: api.SideSell, ExpectedSideVersion: 1, Value: party(api.ChargeSide{Amount: cny(90_000)}), Reason: "price agreed again", OccurredAt: f.hours(4)}
	release := holdingJob(t, u, "JOB-B")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, reviseErr = f.module.ReviseChargeSide(ctx, revise) }()
	time.Sleep(400 * time.Millisecond)
	go func() { defer wg.Done(); runErr = run() }()
	time.Sleep(400 * time.Millisecond)
	release()
	wg.Wait()
	return reviseErr, runErr
}

// An apportionment by REVENUE reads the revenue under the jobs' lock. JOB-B's lock is held, a revision of its sell side
// (CNY 300.00 to 900.00) waits behind it and the apportionment behind the revision. Released, the revision commits
// first, so the apportionment must record 90,000 for SH-B: a revenue read before the lock would record the old 30,000,
// a figure that was not the job's when the apportionment committed. The race test cannot pin this: it accepts either
// order and only meets the narrow window by chance (it caught a scratch stale read once in fifty rounds); here the
// order is forced.
func TestAnApportionmentByRevenueReadsTheRevenueUnderTheJobsLock(t *testing.T) {
	u := jobsPool(t, api.MethodRevenue, false)
	f := u.fixture
	reviseErr, apportionErr := queuedBehindAHeldJob(t, u, func() error {
		_, err := f.module.ApportionConsolCosts(context.Background(), u.apportionCmd("apportion", 5, nil))
		return err
	})
	if reviseErr != nil || apportionErr != nil {
		t.Fatalf("revise %v, apportion %v", reviseErr, apportionErr)
	}
	types := f.outboxTypes(t)
	if rev, app := slices.Index(types, "ChargeSideRevised"), slices.Index(types, "ConsolCostsApportioned"); rev < 0 || app < 0 || rev > app {
		t.Fatalf("the revision should have committed first: %v", types)
	}
	if got := u.ap.pool(u.pool).Apportionments[0].Lines[0].Shares[1].Quantity; got != 90_000 {
		t.Fatalf("the apportionment recorded %d for SH-B, the revenue its job had when it committed was 90,000", got)
	}
}

// The close of a pool with a line by REVENUE reads the revenue under the jobs' lock. The pool is apportioned and its
// invoice matched; JOB-B's lock is held, a revision of its sell side waits behind it and the close behind the revision.
// Released, the revision commits first, so the close must be refused (FIN_POOL_NOT_CLOSABLE): a revenue read before the
// lock would be the old one and the pool would close on a figure that is no longer the job's.
func TestTheCloseByRevenueReadsTheRevenueUnderTheJobsLock(t *testing.T) {
	u := jobsPool(t, api.MethodRevenue, true)
	f := u.fixture
	closeCmd := api.CloseConsolCostPool{CommandID: "close", TenantID: f.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(), OccurredAt: f.hours(9)}
	reviseErr, closeErr := queuedBehindAHeldJob(t, u, func() error {
		_, err := f.module.CloseConsolCostPool(context.Background(), closeCmd)
		return err
	})
	if reviseErr != nil {
		t.Fatalf("the revision: %v", reviseErr)
	}
	u.refusedWith("the close queued behind a revision", closeErr, "FIN_POOL_NOT_CLOSABLE")
	if p := u.ap.pool(u.pool); p.State == api.PoolClosed {
		t.Fatalf("the pool closed on a revenue that was no longer the job's")
	}
}

// A pool whose first line is by SHIPMENT and whose second is by REVENUE is a pool with a line by REVENUE: its close
// reads
// the revenue, waits for the jobs and is refused when the second line's revenue changed.
func TestTheCloseOfAPoolWhoseSecondLineIsByRevenue(t *testing.T) {
	u := jobsPool(t, api.MethodShipment, false)
	f := u.fixture
	u.record("cost-2", "AIR_FREIGHT", api.MethodRevenue, 1_000)
	u.must(f.module.RecordCreditorInvoice(u.ctx, u.ap.record("CI-2", "airline-mu", "2", 7, "CNY", line("AIR_FREIGHT", 1_000))))
	u.must(f.module.MatchCreditorInvoice(u.ctx, u.match("CI-2", "ap-1", 8, onPool("P-U", 2, 1, 1_000, true))))
	u.apportion("apportion", 5, nil)
	closeCmd := func(id string, hour int) api.CloseConsolCostPool {
		return api.CloseConsolCostPool{CommandID: id, TenantID: f.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(), OccurredAt: f.hours(hour)}
	}
	// the close waits for a held job lock, though the first line is not by REVENUE
	release := holdingJob(t, u, "JOB-B")
	done := make(chan error, 1)
	go func() { _, err := f.module.CloseConsolCostPool(u.ctx, closeCmd("close-held", 9)); done <- err }()
	if err, returned := returnsWithin(done, 300*time.Millisecond); returned {
		t.Fatalf("the close returned (%v) while a job's lock was held", err)
	}
	release()
	if err, returned := returnsWithin(done, 10*time.Second); !returned || err != nil {
		t.Fatalf("the close after the lock was released: %v (returned %v)", err, returned)
	}
	// reopened, and JOB-C sells more: the second line's revenue changed, so the close is refused until it is apportioned
	// again
	u.must(f.module.ReopenConsolCostPool(u.ctx, api.ReopenConsolCostPool{CommandID: "reopen", TenantID: f.tenant, ActorID: "fin-1", PoolID: u.pool,
		ExpectedRevision: u.revision(), Reason: "revenue changed", OccurredAt: f.hours(10)}))
	u.reviseSell("revise-c", "JOB-C", 1, 20_000, 11)
	_, err := f.module.CloseConsolCostPool(u.ctx, closeCmd("close-refused", 13))
	u.refusedWith("the second line's revenue changed", err, "FIN_POOL_NOT_CLOSABLE")
	u.apportion("again", 12, []int{2})
	u.must(f.module.CloseConsolCostPool(u.ctx, closeCmd("close-again", 14)))
	if p := u.ap.pool(u.pool); p.State != api.PoolClosed {
		t.Fatalf("apportioned again, the pool does not close: %+v", p)
	}
}
