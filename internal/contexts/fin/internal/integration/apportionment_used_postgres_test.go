//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// usedWorld is LE's policy (CNY), three open jobs (JOB-A, JOB-B, JOB-C for SH-A, SH-B, SH-C) and the pool P-U of the air
// consolidation CONSOL-U, whose three houses are as PHY reports them: SH-A loaded 4 packs, 300 kg, 1.2 m³ (chargeable
// 300 kg); SH-B loaded 2 packs, 100 kg, 1.2 m³ (volumetric 200 kg); SH-C received but not loaded: planned 3 packs, 50 kg,
// 0.6 m³ (volumetric 100 kg), nothing loaded.
type usedWorld struct {
	ap
	pool string
}

func newUsedWorld(t *testing.T) usedWorld {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	f.w.costBases["CONSOL-U"] = phyapi.PhysicalCostBasis{TenantID: f.tenant, ConsolidationID: "CONSOL-U", Revision: 7, Mode: "AIR", Kind: "AIR_CONSOL",
		OperatingLegalEntityID: "LE", State: "DEPARTED", Houses: []phyapi.HouseCostBasis{
			ownHouse(f, "SH-A", 4, 300, 1_200_000, 4, 300, 1_200_000),
			ownHouse(f, "SH-B", 2, 100, 1_200_000, 2, 100, 1_200_000),
			ownHouse(f, "SH-C", 3, 50, 600_000, 0, 0, 0)}}
	f.w.drivers["SH-C"] = fulapi.CostDriver{TenantID: f.tenant, ShipmentID: "SH-C", Kind: fulapi.DriverShipmentPlanned, Revision: 3, Status: fulapi.StatusPlanned,
		PlanVersion: 2, TransportMode: "AIR", LegalEntityID: "LE", CustomerPartyID: "party-a", Packs: 3,
		Orders: []fulapi.CostOrder{{OrderID: "O-C", QuoteID: "Q-1", QuoteRevision: 2, Packs: 3}}}
	u := usedWorld{ap: ap{fixture: f, t: t, ctx: ctx}, pool: "P-U"}
	u.must(f.module.OpenFinancePolicy(ctx, api.OpenFinancePolicy{CommandID: "policy", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: f.hours(0)}))
	for _, h := range []string{"A", "B", "C"} {
		u.must(f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-" + h, TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-" + h,
			ShipmentID: "SH-" + h, OccurredAt: f.hours(1)}))
	}
	u.must(f.module.OpenConsolCostPool(ctx, api.OpenConsolCostPool{CommandID: "pool", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-U",
		ConsolidationID: "CONSOL-U", OccurredAt: f.hours(2)}))
	return u
}

func (u usedWorld) revision() int64 { return u.ap.pool(u.pool).Revision }

func (u usedWorld) record(id, code, method string, amount int64) {
	u.t.Helper()
	u.must(u.module.RecordConsolCost(u.ctx, api.RecordConsolCost{CommandID: id, TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool,
		ExpectedRevision: u.revision(), ChargeCode: code, Method: method, Cost: api.ChargeSide{PartyID: "airline-mu", Amount: cny(amount)},
		Reason: "entered for the consolidation", OccurredAt: u.hours(3)}))
}

func (u usedWorld) apportionCmd(id string, hour int, lines []int, notUsed ...string) api.PreviewConsolCostApportionment {
	return api.PreviewConsolCostApportionment{CommandID: id, TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(), LineNos: lines,
		NotUsed: notUsed, OccurredAt: u.hours(hour)}
}

// apportion makes an apportionment and returns it as the pool shows it.
func (u usedWorld) apportion(id string, hour int, lines []int, notUsed ...string) api.PoolApportionment {
	u.t.Helper()
	u.must(previewAndSubmit(u.ctx, u.module, u.apportionCmd(id, hour, lines, notUsed...)))
	p := u.ap.pool(u.pool)
	return p.Apportionments[len(p.Apportionments)-1]
}

// apportionAll apportions lines with every live house taking part (AllHouses).
func (u usedWorld) apportionAll(id string, hour int, lines []int) api.PoolApportionment {
	u.t.Helper()
	cmd := u.apportionCmd(id, hour, lines)
	cmd.AllHouses = true
	u.must(previewAndSubmit(u.ctx, u.module, cmd))
	p := u.ap.pool(u.pool)
	return p.Apportionments[len(p.Apportionments)-1]
}

// sell records a sell side of CNY amount on a job.
func (u usedWorld) sell(job string, amount int64) {
	u.t.Helper()
	u.must(u.module.RecordChargeLine(u.ctx, api.RecordChargeLine{CommandID: "sell-" + job, TenantID: u.tenant, ActorID: "fin-1", JobID: job,
		ExpectedRevision: 1, ChargeCode: "AIR_FREIGHT", Sell: &api.ChargeSide{PartyID: "party-a", Amount: cny(amount)}, Reason: "sold as per booking",
		OccurredAt: u.hours(2)}))
}

// share is a share of a house that took part; left is one that did not.
func share(no int, house string, quantity int64, unit string, amount int64) api.PoolShare {
	return api.PoolShare{ShareNo: no, ShipmentID: "SH-" + house, JobID: "JOB-" + house, Quantity: quantity, Unit: unit, Amount: cny(amount), Local: cny(amount)}
}

func left(no int, house string) api.PoolShare {
	return api.PoolShare{ShareNo: no, ShipmentID: "SH-" + house, JobID: "JOB-" + house, NotUsed: true, Amount: cny(0), Local: cny(0)}
}

func (u usedWorld) checkLine(name string, a api.PoolApportionment, line int, method string, want ...api.PoolShare) {
	u.t.Helper()
	for _, la := range a.Lines {
		if la.LineNo != line {
			continue
		}
		if la.Method != method || len(la.Shares) != len(want) {
			u.t.Fatalf("%s: line %d %s with %d shares: %+v", name, line, la.Method, len(la.Shares), la)
		}
		for i := range want {
			if la.Shares[i] != want[i] {
				u.t.Errorf("%s: line %d share %d is %+v, want %+v", name, line, i+1, la.Shares[i], want[i])
			}
		}
		return
	}
	u.t.Fatalf("%s: line %d is not in apportionment %d", name, line, a.ApportionmentNo)
}

func (u usedWorld) poolShares(job string) []api.JobPoolShare {
	u.t.Helper()
	p, err := u.module.GetJobProfit(u.ctx, api.GetJobProfit{TenantID: u.tenant, JobID: job})
	if err != nil {
		u.t.Fatal(err)
	}
	return p.PoolShares
}

func (u usedWorld) poolCost(job string) int64 {
	u.t.Helper()
	p, err := u.module.GetJobProfit(u.ctx, api.GetJobProfit{TenantID: u.tenant, JobID: job})
	if err != nil {
		u.t.Fatal(err)
	}
	return p.PoolCost.Minor
}

func lines(p []api.JobPoolShare) (out []int) {
	for _, s := range p {
		out = append(out, s.LineNo)
	}
	return out
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A house left out of an apportionment bears none of its lines, on PostgreSQL (V2.78): three houses of an air
// consolidation, SH-C received but never loaded.
//
//	line 1 AIR_FREIGHT 1,000.00 CHARGEABLE, SH-C left out: ACTUAL, SH-A 300,000 g and SH-B 200,000 g share it 3 : 2,
//	  60,000 / 40,000 minor; SH-C's share is not used and zero
//	line 2 CFS_HANDLING 300.00 PIECES over all three: PLANNED (SH-C is not loaded), 4 : 2 : 3 packs, 30,000 minor is
//	  13,333.33 / 6,666.67 / 10,000: the floors leave one minor unit, to SH-B (the largest remainder) -> 13,333 / 6,667 / 10,000
//	line 3 SCREENING 900.00 GROSS_WEIGHT, SH-C left out: loaded 300 kg : 100 kg of 90,000 -> 67,500 / 22,500
//	line 4 TERMINAL_FEE 100.01 SHIPMENT, SH-C left out: 10,001 over two is 5,000.5 each, the minor unit left goes to SH-A
//	  -> 5,001 / 5,000
//
// Each job carries its share, the job of SH-C only that of line 2. The pool does not close while line 2 divides a
// cost over a house not loaded; with SH-C left out of line 2 as well it closes. Reopened and apportioned with SH-C taking
// part, it is refused until SH-C is loaded and the line apportioned on that basis.
func TestHousesLeftOutOfAnApportionmentOnPostgres(t *testing.T) {
	u := newUsedWorld(t)
	f, ctx := u.fixture, u.ctx
	u.sell("JOB-A", 80_000)
	u.sell("JOB-C", 50_000)
	u.record("cost-1", "AIR_FREIGHT", api.MethodChargeable, 100_000)
	u.record("cost-2", "CFS_HANDLING", api.MethodPieces, 30_000)
	u.record("cost-3", "SCREENING", api.MethodGrossWeight, 90_000)
	u.record("cost-4", "TERMINAL_FEE", api.MethodShipment, 10_001)

	// the refusals leave the pool as it was
	before := u.revision()
	for name, tc := range map[string]struct {
		in   api.PreviewConsolCostApportionment
		text string
	}{
		"a house named twice":  {u.apportionCmd("twice", 4, []int{1}, "SH-C", "SH-C"), "left out twice"},
		"a house not live":     {u.apportionCmd("stranger", 4, []int{1}, "SH-Z"), "not a live house"},
		"every house left out": {u.apportionCmd("all", 4, []int{1}, "SH-A", "SH-B", "SH-C"), "every house is left out"},
		"a house left out of a retained cost": {func() api.PreviewConsolCostApportionment {
			in := u.apportionCmd("retained", 4, []int{1}, "SH-C")
			in.Method, in.Reason = api.MethodRetained, "dead freight"
			return in
		}(), "no houses to leave out"},
	} {
		if _, err := previewAndSubmit(ctx, f.module, tc.in); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), tc.text) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if u.revision() != before {
		t.Fatalf("a refused apportionment changed the pool")
	}

	// line 1 by CHARGEABLE leaving SH-C out
	cmd := u.apportionCmd("apportion-1", 4, []int{1}, "SH-C")
	r := u.must(previewAndSubmit(ctx, f.module, cmd))
	p := u.ap.pool(u.pool)
	one := p.Apportionments[0]
	if len(p.Apportionments) != 1 || one.Stage != api.BasisActual || one.BasisRevision != 7 || len(one.Lines) != 1 {
		t.Fatalf("apportionment 1: %+v", one)
	}
	u.checkLine("apportionment 1", one, 1, api.MethodChargeable, share(1, "A", 300_000, "GRM", 60_000), share(2, "B", 200_000, "GRM", 40_000), left(3, "C"))

	// the same command replayed answers the same receipt; the same command id with another NotUsed is the conflict
	if again, err := previewAndSubmit(ctx, f.module, cmd); err != nil || again != r {
		t.Fatalf("replay: %+v %v, want %+v", again, err, r)
	}
	other := cmd
	other.NotUsed = []string{"SH-B"}
	if _, err := previewAndSubmit(ctx, f.module, other); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the same command id with another NotUsed: %v", err)
	}
	none := cmd
	none.NotUsed = nil
	if _, err := previewAndSubmit(ctx, f.module, none); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the same command id with nothing left out: %v", err)
	}
	everyone := none
	everyone.AllHouses = true
	if _, err := previewAndSubmit(ctx, f.module, everyone); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the same command id with AllHouses: %v", err)
	}

	// line 2 over all three: PLANNED, SH-C takes part
	two := u.apportion("apportion-2", 5, []int{2})
	if two.Stage != api.BasisPlanned {
		t.Fatalf("a house taking part is not loaded: %+v", two)
	}
	u.checkLine("apportionment 2", two, 2, api.MethodPieces, share(1, "A", 4, "PCS", 13_333), share(2, "B", 2, "PCS", 6_667), share(3, "C", 3, "PCS", 10_000))

	// lines 3 and 4, by the lines' own methods, leaving SH-C out
	three := u.apportion("apportion-3", 6, []int{3, 4}, "SH-C")
	if three.Stage != api.BasisActual {
		t.Fatalf("the house left out does not hold the stage back: %+v", three)
	}
	u.checkLine("apportionment 3", three, 3, api.MethodGrossWeight, share(1, "A", 300_000, "GRM", 67_500), share(2, "B", 100_000, "GRM", 22_500), left(3, "C"))
	u.checkLine("apportionment 3", three, 4, api.MethodShipment, share(1, "A", 1, "SHP", 5_001), share(2, "B", 1, "SHP", 5_000), left(3, "C"))

	// each job carries its share; the job left out carries none of the lines that left it out
	if got := lines(u.poolShares("JOB-A")); !sameInts(got, []int{1, 2, 3, 4}) {
		t.Fatalf("JOB-A carries lines %v", got)
	}
	if got := lines(u.poolShares("JOB-C")); !sameInts(got, []int{2}) {
		t.Fatalf("JOB-C carries lines %v: %+v", got, u.poolShares("JOB-C"))
	}
	if s := u.poolShares("JOB-C")[0]; s.Method != api.MethodPieces || s.Stage != api.BasisPlanned || s.Quantity != 3 || s.Unit != "PCS" || s.Local != cny(10_000) {
		t.Fatalf("JOB-C's share of line 2: %+v", s)
	}
	if a, b, c := u.poolCost("JOB-A"), u.poolCost("JOB-B"), u.poolCost("JOB-C"); a != 145_834 || b != 74_167 || c != 10_000 {
		t.Fatalf("pool cost of the jobs: %d %d %d, want 145834 (60000+13333+67500+5001), 74167 (40000+6667+22500+5000) and 10000", a, b, c)
	}
	consol, err := f.module.GetConsolProfit(ctx, api.GetConsolProfit{TenantID: f.tenant, ConsolidationID: "CONSOL-U"})
	if err != nil || consol.Pooled != cny(230_001) || consol.Apportioned != cny(230_001) || consol.Unapportioned != cny(0) || len(consol.Houses) != 3 ||
		consol.Houses[0].PoolCost != cny(145_834) || consol.Houses[1].PoolCost != cny(74_167) || consol.Houses[2].PoolCost != cny(10_000) ||
		consol.Houses[0].Revenue != cny(80_000) || consol.Houses[2].Revenue != cny(50_000) {
		t.Fatalf("consol profit: %+v %v", consol, err)
	}

	// the carrier's invoice is matched final; the pool does not close while line 2 is PLANNED over a house not loaded
	u.must(f.module.RecordCreditorInvoice(ctx, api.RecordCreditorInvoice{CommandID: "invoice", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-U",
		LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "MU-U-1", InvoiceDate: f.hours(7).Format(time.DateOnly), Currency: "CNY",
		Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 100_000}, {ChargeCode: "CFS_HANDLING", Amount: 30_000},
			{ChargeCode: "SCREENING", Amount: 90_000}, {ChargeCode: "TERMINAL_FEE", Amount: 10_001}}, OccurredAt: f.hours(7)}))
	u.must(f.module.MatchCreditorInvoice(ctx, u.match("CI-U", "ap-1", 8, onPool("P-U", 1, 1, 100_000, true), onPool("P-U", 2, 2, 30_000, true),
		onPool("P-U", 3, 3, 90_000, true), onPool("P-U", 4, 4, 10_001, true))))
	closeCmd := func(id string, hour int) api.CloseConsolCostPool {
		return api.CloseConsolCostPool{CommandID: id, TenantID: f.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(), OccurredAt: f.hours(hour)}
	}
	if _, err := f.module.CloseConsolCostPool(ctx, closeCmd("close-1", 9)); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_POOL_NOT_CLOSABLE") {
		t.Fatalf("closed with line 2 over a house not loaded: %v", err)
	}

	// line 2 apportioned again leaving SH-C out: ACTUAL, 4 : 2 of 30,000; the pool closes with SH-C never loaded
	again := u.apportion("apportion-2b", 10, []int{2}, "SH-C")
	if again.Stage != api.BasisActual {
		t.Fatalf("apportionment 4: %+v", again)
	}
	u.checkLine("apportionment 4", again, 2, api.MethodPieces, share(1, "A", 4, "PCS", 20_000), share(2, "B", 2, "PCS", 10_000), left(3, "C"))
	u.must(f.module.CloseConsolCostPool(ctx, closeCmd("close-2", 11)))
	if p := u.ap.pool(u.pool); p.State != api.PoolClosed || len(p.Apportionments) != 4 {
		t.Fatalf("closed pool: %+v", p)
	}
	if got := u.poolShares("JOB-C"); len(got) != 0 || u.poolCost("JOB-C") != 0 {
		t.Fatalf("SH-C bears nothing: %+v", got)
	}
	// SH-C bears nothing of any line and is still a house of the consolidation: it is listed with pool cost zero and its own
	// revenue, and the houses' revenue is the consolidation's (80,000 + 0 + 50,000)
	consol, err = f.module.GetConsolProfit(ctx, api.GetConsolProfit{TenantID: f.tenant, ConsolidationID: "CONSOL-U"})
	var revenue int64
	for _, h := range consol.Houses {
		revenue += h.Revenue.Minor
	}
	if err != nil || consol.State != api.PoolClosed || consol.Pooled != cny(230_001) || consol.Apportioned != cny(230_001) || len(consol.Houses) != 3 ||
		consol.Houses[0].ShipmentID != "SH-A" || consol.Houses[0].PoolCost != cny(60_000+20_000+67_500+5_001) || consol.Houses[0].Profit != cny(80_000-152_501) ||
		consol.Houses[1].ShipmentID != "SH-B" || consol.Houses[1].PoolCost != cny(40_000+10_000+22_500+5_000) || consol.Houses[1].Profit != cny(-77_500) ||
		consol.Houses[2].ShipmentID != "SH-C" || consol.Houses[2].JobID != "JOB-C" || consol.Houses[2].PoolCost != cny(0) || consol.Houses[2].Revenue != cny(50_000) ||
		consol.Houses[2].Profit != cny(50_000) || revenue != 130_000 {
		t.Fatalf("consol profit of the closed pool: %+v %v", consol, err)
	}

	// reopened, line 1 apportioned again with SH-C taking part: PLANNED, and refused until SH-C is loaded and the line
	// apportioned on that basis. W/M is not used in air: chargeable grams 300,000 : 200,000 : 100,000 (SH-C planned:
	// volumetric 100,000 g over gross 50,000 g); 100,000 divides 50,000 / 33,333.33 / 16,666.67, the unit left goes to SH-C
	u.must(f.module.ReopenConsolCostPool(ctx, api.ReopenConsolCostPool{CommandID: "reopen", TenantID: f.tenant, ActorID: "fin-1", PoolID: u.pool,
		ExpectedRevision: u.revision(), Reason: "SH-C takes part in the freight after all", OccurredAt: f.hours(12)}))
	planned := u.apportionAll("apportion-1b", 13, []int{1})
	if planned.Stage != api.BasisPlanned {
		t.Fatalf("apportionment 5: %+v", planned)
	}
	u.checkLine("apportionment 5", planned, 1, api.MethodChargeable, share(1, "A", 300_000, "GRM", 50_000), share(2, "B", 200_000, "GRM", 33_333),
		share(3, "C", 100_000, "GRM", 16_667))
	notClosable := func(id string, hour int, why string) {
		t.Helper()
		if _, err := f.module.CloseConsolCostPool(ctx, closeCmd(id, hour)); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_POOL_NOT_CLOSABLE") {
			t.Fatalf("%s: %v", why, err)
		}
	}
	notClosable("close-3", 14, "closed with a house taking part not loaded")
	loaded := f.w.costBases["CONSOL-U"]
	loaded.Revision = 8
	loaded.Houses = append([]phyapi.HouseCostBasis(nil), loaded.Houses...)
	loaded.Houses[2] = ownHouse(f, "SH-C", 3, 50, 600_000, 3, 50, 600_000)
	f.w.costBases["CONSOL-U"] = loaded
	notClosable("close-4", 15, "closed on a line apportioned while SH-C was not loaded")
	actual := u.apportion("apportion-1c", 16, []int{1})
	if actual.Stage != api.BasisActual || actual.BasisRevision != 8 {
		t.Fatalf("apportionment 6: %+v", actual)
	}
	u.checkLine("apportionment 6", actual, 1, api.MethodChargeable, share(1, "A", 300_000, "GRM", 50_000), share(2, "B", 200_000, "GRM", 33_333),
		share(3, "C", 100_000, "GRM", 16_667))
	u.must(f.module.CloseConsolCostPool(ctx, closeCmd("close-5", 17)))
	if got := u.poolShares("JOB-C"); len(got) != 1 || got[0].LineNo != 1 || got[0].Local != cny(16_667) || got[0].Stage != api.BasisActual {
		t.Fatalf("JOB-C after taking part: %+v", got)
	}
}

// What the database refuses past FIN on the application role (V2.78): a share not used that carries a quantity, a unit
// or an amount, each written as a real apportionment is written (the pool row touched in the same transaction, a valid
// line, a share used beside it) so that only pool_apportionment_share_not_used refuses it; a line whose shares are all
// not used; a method or a unit outside the lists; a share's used changed; the cancellation of a job that has a share
// used, refused, and of one that has none, allowed. A line with one share used is the control that it is accepted.
func TestApportionmentSharesPastFINOnPostgres(t *testing.T) {
	u := newUsedWorld(t)
	f := u.fixture
	u.record("cost-1", "AIR_FREIGHT", api.MethodChargeable, 100_000)
	u.record("cost-2", "ZERO_FEE", api.MethodShipment, 0)
	u.apportion("apportion-1", 4, []int{1}, "SH-C")

	const bump = `UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-U';
`
	// a second apportionment of line 1 (CNY 1,000.00 each way) with its two shares: SH-A's, used, and SH-C's, as given
	const columns = `(tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,used,quantity,unit,amount_minor,local_minor)`
	second := func(used, unused string) string {
		return bump + `INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at)
            VALUES ($1,'P-U',2,7,'ACTUAL','x',now(),now());
INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,amount_minor,local_minor)
            VALUES ($1,'P-U',2,1,1,'SHIPMENT',100000,100000);
INSERT INTO cargo_fin.pool_apportionment_share ` + columns + ` VALUES ($1,'P-U',2,1,1,'SH-A','JOB-A',` + used + `);
INSERT INTO cargo_fin.pool_apportionment_share ` + columns + ` VALUES ($1,'P-U',2,1,2,'SH-C','JOB-C',` + unused + `)`
	}
	// the line of the all-not-used case is the cost line of zero, whose shares of zero conserve: only the rule that a line has
	// a share used refuses it; the amount cases put the line's money wholly on the share not used, which conserves too
	const cancel = `UPDATE cargo_fin.freight_job SET state='CANCELLED',cancel_reason='x',cancelled_by='x',cancelled_at=now(),cancel_entered_at=now(),
            revision=revision+1 WHERE tenant_id=$1 AND job_id=`
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
	for name, tc := range map[string]struct{ sql, state, check string }{
		"a share not used with a quantity":     {second(`true,1,'SHP',100000,100000`, `false,5,NULL,0,0`), "23514", "pool_apportionment_share_not_used"},
		"a share not used with a unit":         {second(`true,1,'SHP',100000,100000`, `false,0,'GRM',0,0`), "23514", "pool_apportionment_share_not_used"},
		"a share not used with an amount":      {second(`true,1,'SHP',0,100000`, `false,0,NULL,100000,0`), "23514", "pool_apportionment_share_not_used"},
		"a share not used with a local amount": {second(`true,1,'SHP',100000,0`, `false,0,NULL,0,100000`), "23514", "pool_apportionment_share_not_used"},
		"a unit outside the list":              {second(`true,1,'SHP',100000,100000`, `true,1,'NO_SUCH_UNIT',0,0`), "23514", "pool_apportionment_share_unit_check"},
		"a share's used changed":               {`UPDATE cargo_fin.pool_apportionment_share SET used=NOT used WHERE tenant_id=$1 AND shipment_id='SH-C'`, "42501|23001", ""},
		"a share not used deleted":             {`DELETE FROM cargo_fin.pool_apportionment_share WHERE tenant_id=$1 AND shipment_id='SH-C'`, "42501|23001", ""},
		"a method outside the list on a line": {`INSERT INTO cargo_fin.pool_cost_line (tenant_id,pool_id,line_no,charge_code,method,recorded_by,recorded_at,entered_at)
            VALUES ($1,'P-U',77,'X','NO_SUCH_METHOD','x',now(),now())`, "23514", "pool_cost_line_method_check"},
		"a method outside the list on an apportionment line": {`INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,
            line_version,method,amount_minor,local_minor) VALUES ($1,'P-U',1,88,1,'NO_SUCH_METHOD',0,0)`, "23514", "pool_apportionment_line_method_check"},
		"a line whose shares are all not used": {bump + `INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at)
            VALUES ($1,'P-U',2,7,'ACTUAL','x',now(),now());
INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,amount_minor,local_minor)
            VALUES ($1,'P-U',2,2,1,'SHIPMENT',0,0);
INSERT INTO cargo_fin.pool_apportionment_share ` + columns + ` VALUES ($1,'P-U',2,2,1,'SH-A','JOB-A',false,0,NULL,0,0);
INSERT INTO cargo_fin.pool_apportionment_share ` + columns + ` VALUES ($1,'P-U',2,2,2,'SH-C','JOB-C',false,0,NULL,0,0)`, "23514", "pool_apportionment_conserves"},
		"the job of a house with a share used cancelled": {cancel + `'JOB-B'`, "23514", "pool_share_job_open"},
	} {
		err := run(tc.sql)
		state, constraint := sqlState(err)
		if state == "" || !strings.Contains(tc.state, state) || tc.check != "" && constraint != tc.check {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, state, constraint, tc.state, tc.check)
		}
	}

	// the controls: the same line with its money on the share used and the other not used is accepted and reads back; and
	// the job whose only share is not used (SH-C's, under apportionment 2 as under apportionment 1) is cancelled
	if err := run(second(`true,1,'SHP',100000,100000`, `false,0,NULL,0,0`)); err != nil {
		t.Fatalf("a line with a share used and one not: %v", err)
	}
	p := u.ap.pool(u.pool)
	if len(p.Apportionments) != 2 || len(p.Apportionments[1].Lines) != 1 || len(p.Apportionments[1].Lines[0].Shares) != 2 ||
		p.Apportionments[1].Lines[0].Shares[0] != (api.PoolShare{ShareNo: 1, ShipmentID: "SH-A", JobID: "JOB-A", Quantity: 1, Unit: "SHP", Amount: cny(100_000), Local: cny(100_000)}) ||
		p.Apportionments[1].Lines[0].Shares[1] != (api.PoolShare{ShareNo: 2, ShipmentID: "SH-C", JobID: "JOB-C", NotUsed: true, Amount: cny(0), Local: cny(0)}) {
		t.Fatalf("pool after the control: %+v", p.Apportionments)
	}
	if err := run(cancel + `'JOB-C'`); err != nil {
		t.Fatalf("the job of a house with no share used: %v", err)
	}
}

// A house left out stays out of a line until someone says otherwise, on PostgreSQL (V2.78): lines apportioned again with
// nothing named keep their own lists (in one command), AllHouses takes every house back, another list replaces the
// line's, MANUAL takes the carried list, a house detached since is dropped from it, and the command's hash covers
// AllHouses as it covers NotUsed.
func TestHousesLeftOutStayOutOnPostgres(t *testing.T) {
	u := newUsedWorld(t)
	f := u.fixture
	u.record("cost-1", "AIR_FREIGHT", api.MethodShipment, 100_000)
	u.record("cost-2", "CFS_HANDLING", api.MethodShipment, 60_000)
	u.record("cost-3", "SCREENING", api.MethodManual, 90_000)
	left1 := u.apportion("apportion-1", 4, []int{1}, "SH-C")
	if left1.Lines[0].Shares[2] != left(3, "C") {
		t.Fatalf("setup: %+v", left1)
	}
	u.apportion("apportion-2", 4, []int{2}, "SH-B")
	// nothing named: each line keeps its own list, both in one command
	again := u.apportion("apportion-3", 5, []int{1, 2})
	u.checkLine("line 1 again", again, 1, api.MethodShipment, share(1, "A", 1, "SHP", 50_000), share(2, "B", 1, "SHP", 50_000), left(3, "C"))
	u.checkLine("line 2 again", again, 2, api.MethodShipment, share(1, "A", 1, "SHP", 30_000), left(2, "B"), share(3, "C", 1, "SHP", 30_000))
	// MANUAL takes the carried list: its shares name the houses that take part
	manual := func(id string, notUsed []string, shares ...api.ManualShare) api.PreviewConsolCostApportionment {
		in := u.apportionCmd(id, 5, []int{3}, notUsed...)
		in.Method, in.Manual, in.Reason = api.MethodManual, shares, "agreed"
		return in
	}
	ab := []api.ManualShare{{ShipmentID: "SH-A", Amount: 10_000}, {ShipmentID: "SH-B", Amount: 80_000}}
	if _, err := previewAndSubmit(u.ctx, f.module, manual("manual-0", []string{"SH-C"}, ab...)); err != nil {
		t.Fatal(err)
	}
	if _, err := previewAndSubmit(u.ctx, f.module, manual("manual-2", nil, ab...)); err != nil {
		t.Fatalf("manual with the carried list: %v", err)
	}
	if p := u.ap.pool(u.pool); !p.Apportionments[len(p.Apportionments)-1].Lines[0].Shares[2].NotUsed {
		t.Fatalf("manual did not carry the list: %+v", p.Apportionments[len(p.Apportionments)-1])
	}
	if _, err := previewAndSubmit(u.ctx, f.module, manual("manual-1", nil, api.ManualShare{ShipmentID: "SH-A", Amount: 10_000}, api.ManualShare{ShipmentID: "SH-B", Amount: 50_000},
		api.ManualShare{ShipmentID: "SH-C", Amount: 30_000})); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_MANUAL_SHARES") {
		t.Fatalf("manual naming a house of a line's carried list: %v", err)
	}
	// another list replaces the line's; AllHouses takes everyone in; both together are refused
	replaced := u.apportion("apportion-4", 6, []int{1}, "SH-A")
	u.checkLine("replaced", replaced, 1, api.MethodShipment, left(1, "A"), share(2, "B", 1, "SHP", 50_000), share(3, "C", 1, "SHP", 50_000))
	both := u.apportionCmd("apportion-both", 6, []int{1}, "SH-A")
	both.AllHouses = true
	if _, err := previewAndSubmit(u.ctx, f.module, both); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("AllHouses with NotUsed: %v", err)
	}
	all := u.apportionCmd("apportion-all", 7, []int{1, 2})
	all.AllHouses = true
	r := u.must(previewAndSubmit(u.ctx, f.module, all))
	p := u.ap.pool(u.pool)
	last := p.Apportionments[len(p.Apportionments)-1]
	u.checkLine("all", last, 1, api.MethodShipment, share(1, "A", 1, "SHP", 33_334), share(2, "B", 1, "SHP", 33_333), share(3, "C", 1, "SHP", 33_333))
	u.checkLine("all", last, 2, api.MethodShipment, share(1, "A", 1, "SHP", 20_000), share(2, "B", 1, "SHP", 20_000), share(3, "C", 1, "SHP", 20_000))
	// replay and idempotency of a command with AllHouses
	if again, err := previewAndSubmit(u.ctx, f.module, all); err != nil || again != r {
		t.Fatalf("replay with AllHouses: %+v %v, want %+v", again, err, r)
	}
	for name, change := range map[string]func(*api.PreviewConsolCostApportionment){
		"without AllHouses":   func(c *api.PreviewConsolCostApportionment) { c.AllHouses = false },
		"with a list instead": func(c *api.PreviewConsolCostApportionment) { c.AllHouses, c.NotUsed = false, []string{"SH-C"} },
		"with another line":   func(c *api.PreviewConsolCostApportionment) { c.LineNos = []int{1} },
	} {
		other := all
		change(&other)
		if _, err := previewAndSubmit(u.ctx, f.module, other); !errors.Is(err, api.ErrIdempotencyConflict) {
			t.Errorf("the same command id %s: %v", name, err)
		}
	}
	// a house detached since is dropped from the carried list: line 1 leaves SH-C out, then PHY reports SH-C gone
	u.apportion("apportion-5", 8, []int{1}, "SH-C")
	basis := f.w.costBases["CONSOL-U"]
	basis.Revision = 8
	basis.Houses = append([]phyapi.HouseCostBasis(nil), basis.Houses[:2]...)
	f.w.costBases["CONSOL-U"] = basis
	gone := u.apportion("apportion-6", 9, []int{1})
	u.checkLine("detached", gone, 1, api.MethodShipment, share(1, "A", 1, "SHP", 50_000), share(2, "B", 1, "SHP", 50_000))
	// every house still live left out: refused as when every house is named
	if _, err := previewAndSubmit(u.ctx, f.module, u.apportionCmd("apportion-7", 9, []int{1}, "SH-A")); err != nil {
		t.Fatal(err)
	}
	basis.Houses = basis.Houses[:1]
	f.w.costBases["CONSOL-U"] = basis
	if _, err := previewAndSubmit(u.ctx, f.module, u.apportionCmd("apportion-8", 10, []int{1})); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "every house still live was left out") {
		t.Fatalf("a carried list that leaves no house taking part: %v", err)
	}
}

// The job of a house left out of every line can be cancelled, that of a house with a share used cannot (V2.78). A house
// PHY still reports live needs an OPEN job for any apportionment that names it, so once the job is cancelled the next
// apportionment is refused with FIN_POOL_JOB_MISSING, as it is for any live house without an open job.
func TestTheJobOfAHouseLeftOutIsCancelledOnPostgres(t *testing.T) {
	u := newUsedWorld(t)
	f := u.fixture
	u.record("cost-1", "AIR_FREIGHT", api.MethodChargeable, 100_000)
	u.apportion("apportion-1", 4, []int{1}, "SH-C")
	cancel := func(job string, id string) error {
		j := u.job(job)
		_, err := f.module.CancelFreightJob(u.ctx, api.CancelFreightJob{CommandID: id, TenantID: f.tenant, ActorID: "fin-1", JobID: job, ExpectedRevision: j.Revision,
			Reason: "shipment left behind", OccurredAt: f.hours(5)})
		return err
	}
	// since V2.93 FIN refuses before it writes; the database's rule (pool_share_job_open) is the backstop
	if err := cancel("JOB-B", "cancel-b"); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_JOB_BEARS_POOL_COST") {
		t.Fatalf("the job of a house with a share used: %v", err)
	}
	if err := cancel("JOB-C", "cancel-c"); err != nil {
		t.Fatalf("the job of a house left out of every line: %v", err)
	}
	if j := u.job("JOB-C"); j.State != api.JobCancelled {
		t.Fatalf("JOB-C: %+v", j)
	}
	if _, err := previewAndSubmit(u.ctx, f.module, u.apportionCmd("apportion-2", 6, []int{1}, "SH-C")); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "FIN_POOL_JOB_MISSING") {
		t.Fatalf("the next apportionment names a house whose job is cancelled: %v", err)
	}
}

// Two lines leave different houses out and the pool closes over the houses each used (V2.78): every house loaded, line 1
// (by shipment) without SH-C and line 2 without SH-B. It is refused while a house used by either is not loaded, whichever
// the house, and closes when all are loaded.
func TestTwoLinesLeaveDifferentHousesOutOnPostgres(t *testing.T) {
	u := newUsedWorld(t)
	f, ctx := u.fixture, u.ctx
	setLoaded := func(house int, loaded bool) {
		b := f.w.costBases["CONSOL-U"]
		b.Houses = append([]phyapi.HouseCostBasis(nil), b.Houses...)
		if loaded {
			b.Houses[house] = ownHouse(f, b.Houses[house].ShipmentID, 3, 50, 600_000, 3, 50, 600_000)
		} else {
			b.Houses[house] = ownHouse(f, b.Houses[house].ShipmentID, 3, 50, 600_000, 0, 0, 0)
		}
		f.w.costBases["CONSOL-U"] = b
	}
	setLoaded(2, true)
	u.record("cost-1", "AIR_FREIGHT", api.MethodShipment, 100_000)
	u.record("cost-2", "CFS_HANDLING", api.MethodShipment, 60_000)
	one := u.apportion("apportion-1", 4, []int{1}, "SH-C")
	two := u.apportion("apportion-2", 4, []int{2}, "SH-B")
	if one.Stage != api.BasisActual || two.Stage != api.BasisActual {
		t.Fatalf("stages: %s %s", one.Stage, two.Stage)
	}
	u.checkLine("line 2", two, 2, api.MethodShipment, share(1, "A", 1, "SHP", 30_000), left(2, "B"), share(3, "C", 1, "SHP", 30_000))
	u.must(f.module.RecordCreditorInvoice(ctx, api.RecordCreditorInvoice{CommandID: "invoice", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-U",
		LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "MU-U-2", InvoiceDate: f.hours(5).Format(time.DateOnly), Currency: "CNY",
		Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 100_000}, {ChargeCode: "CFS_HANDLING", Amount: 60_000}}, OccurredAt: f.hours(5)}))
	u.must(f.module.MatchCreditorInvoice(ctx, u.match("CI-U", "ap-1", 6, onPool("P-U", 1, 1, 100_000, true), onPool("P-U", 2, 2, 60_000, true))))
	closeCmd := func(id string, hour int) error {
		_, err := f.module.CloseConsolCostPool(ctx, api.CloseConsolCostPool{CommandID: id, TenantID: f.tenant, ActorID: "fin-1", PoolID: u.pool,
			ExpectedRevision: u.revision(), OccurredAt: f.hours(hour)})
		return err
	}
	notClosable := func(id string, hour int, why string) {
		t.Helper()
		if err := closeCmd(id, hour); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_POOL_NOT_CLOSABLE") {
			t.Fatalf("%s: %v", why, err)
		}
	}
	setLoaded(2, false)
	notClosable("close-c", 7, "SH-C is used by line 2 and is not loaded")
	setLoaded(2, true)
	setLoaded(1, false)
	notClosable("close-b", 8, "SH-B is used by line 1 and is not loaded")
	setLoaded(1, true)
	if err := closeCmd("close-ok", 9); err != nil {
		t.Fatalf("every house loaded, each line over the houses it used: %v", err)
	}
}

// The basis of a line is that of the houses that take part (V2.78): a weight FIN cannot convert, or no volume at all, in
// the house left out is of no matter, and in a house that takes part it is refused.
func TestTheBasisIsThatOfTheHousesThatTakePartOnPostgres(t *testing.T) {
	u := newUsedWorld(t)
	f := u.fixture
	b := f.w.costBases["CONSOL-U"]
	b.Houses = append([]phyapi.HouseCostBasis(nil), b.Houses...)
	b.Houses[2].GrossWeight = kernel.Quantity{Unit: "LBR", Amount: 110} // SH-C is not loaded: its planned scope weighs
	f.w.costBases["CONSOL-U"] = b
	u.record("cost-1", "AIR_FREIGHT", api.MethodGrossWeight, 100_000)
	_, err := previewAndSubmit(u.ctx, f.module, u.apportionCmd("apportion-all", 4, []int{1}))
	if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_POOL_UNIT") {
		t.Fatalf("a weight in pounds in a house that takes part: %v", err)
	}
	a := u.apportion("apportion-1", 4, []int{1}, "SH-C")
	u.checkLine("gross weight", a, 1, api.MethodGrossWeight, share(1, "A", 300_000, "GRM", 75_000), share(2, "B", 100_000, "GRM", 25_000), left(3, "C"))
}
