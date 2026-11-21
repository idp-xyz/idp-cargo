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
)

// containerBasis is PhysicalCostBasis v1 of an own-container consolidation as the stand-in answers it: a 20-foot 22G1
// (10 tenths of a TEU) as unit 1 and a 40-foot 42G1 (20 tenths) as unit 2, both sealed; SH-A is in unit 1 only, SH-B in
// unit 2 only and SH-C in both. With stuffed false no house is in any unit yet (nothing is loaded for SH-C).
func containerBasis(f *fixture, stuffed bool) phyapi.PhysicalCostBasis {
	b := phyapi.PhysicalCostBasis{TenantID: f.tenant, ConsolidationID: "CONSOL-K", Revision: 7, Mode: "SEA", Kind: "SEA_LCL_OWN",
		OperatingLegalEntityID: "LE", State: "CLOSED", LoadUnits: []phyapi.PlannedLoadUnit{
			{UnitNo: 1, Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", TEUTenths: 10, Status: phyapi.UnitStatusClosed},
			{UnitNo: 2, Kind: phyapi.LoadUnitContainer, TypeCode: "42G1", TEUTenths: 20, Status: phyapi.UnitStatusClosed}},
		Houses: []phyapi.HouseCostBasis{
			ownHouse(f, "SH-A", 4, 300, 1_200_000, 4, 300, 1_200_000),
			ownHouse(f, "SH-B", 2, 100, 1_200_000, 2, 100, 1_200_000),
			ownHouse(f, "SH-C", 3, 50, 600_000, 3, 50, 600_000)}}
	if !stuffed {
		b.Houses[2] = ownHouse(f, "SH-C", 3, 50, 600_000, 0, 0, 0)
		return b
	}
	b.Houses[0].Units = []phyapi.HouseUnitLoad{{UnitNo: 1, LoadedPacks: 4}}
	b.Houses[1].Units = []phyapi.HouseUnitLoad{{UnitNo: 2, LoadedPacks: 2}}
	b.Houses[2].Units = []phyapi.HouseUnitLoad{{UnitNo: 1, LoadedPacks: 1}, {UnitNo: 2, LoadedPacks: 2}}
	return b
}

// containerWorld is LE's policy (CNY), three open jobs JOB-A, JOB-B, JOB-C for SH-A, SH-B, SH-C and the pool P-K of the
// consolidation CONSOL-K, whose houses are in the containers of containerBasis.
func containerWorld(t *testing.T, stuffed bool) usedWorld {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	f.w.costBases["CONSOL-K"] = containerBasis(f, stuffed)
	f.w.drivers["SH-C"] = fulapi.CostDriver{TenantID: f.tenant, ShipmentID: "SH-C", Kind: fulapi.DriverShipmentPlanned, Revision: 3, Status: fulapi.StatusPlanned,
		PlanVersion: 2, TransportMode: "SEA", LegalEntityID: "LE", CustomerPartyID: "party-a", Packs: 3,
		Orders: []fulapi.CostOrder{{OrderID: "O-C", QuoteID: "Q-1", QuoteRevision: 2, Packs: 3}}}
	u := usedWorld{ap: ap{fixture: f, t: t, ctx: ctx}, pool: "P-K"}
	u.must(f.module.OpenFinancePolicy(ctx, api.OpenFinancePolicy{CommandID: "policy", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: f.hours(0)}))
	for _, h := range []string{"A", "B", "C"} {
		u.must(f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-" + h, TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-" + h,
			ShipmentID: "SH-" + h, OccurredAt: f.hours(1)}))
	}
	u.must(f.module.OpenConsolCostPool(ctx, api.OpenConsolCostPool{CommandID: "pool", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-K",
		ConsolidationID: "CONSOL-K", OccurredAt: f.hours(2)}))
	return u
}

func (u usedWorld) refusedWith(name string, err error, code string) {
	u.t.Helper()
	if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), code) {
		u.t.Errorf("%s: %v, want %s", name, err, code)
	}
}

// Apportioning by container count and by TEU on PostgreSQL (V2.80): three houses in the two containers of
// containerBasis, SH-A in the 20-foot, SH-B in the 40-foot and SH-C in both: 1 : 1 : 2 containers and 10 : 20 : 30 tenths.
//
//	line 1 SEA_FREIGHT 100.01 CONTAINER_COUNT: 10,001 over 4 is 2,500.25 / 2,500.25 / 5,000.50; one unit is left, to SH-C
//	  (.50 is the largest remainder) -> 2,500 / 2,500 / 5,001
//	line 2 THC 100.01 TEU: 10,001 over 60 tenths is 1,666.83 / 3,333.67 / 5,000.50; two units are left, to SH-A (.83) and
//	  SH-B (.67) -> 1,667 / 3,334 / 5,000
//	line 3 DOCS 60.00 SHIPMENT as its default, apportioned by TEU as the command's method: 6,000 over 60 -> 1,000 / 2,000 / 3,000
//
// Before the stuffing a line by either method is refused and nothing is stored. The jobs carry the shares, the
// consolidation's profit agrees, a command replayed answers its receipt, and the pool closes once the invoice is matched.
// Reopened after PHY reports SH-C in the 20-foot container only, the close is refused until each line is apportioned again;
// the counts are then 1 : 1 : 1 and the tenths 10 : 20 : 10:
//
//	line 1 10,001 over 3 is 3,333.67 each; two units are left, to the first two -> 3,334 / 3,334 / 3,333
//	line 2 10,001 over 40 is 2,500.25 / 5,000.50 / 2,500.25; the unit left goes to SH-B -> 2,500 / 5,001 / 2,500
//	line 3 6,000 over 40 -> 1,500 / 3,000 / 1,500
func TestApportionmentByContainerCountAndTEUOnPostgres(t *testing.T) {
	u := containerWorld(t, false)
	f, ctx := u.fixture, u.ctx
	u.sell("JOB-A", 80_000)
	u.record("cost-1", "SEA_FREIGHT", api.MethodContainerCount, 10_001)
	u.record("cost-2", "THC", api.MethodTEU, 10_001)
	u.record("cost-3", "DOCS", api.MethodShipment, 6_000)
	p := u.ap.pool(u.pool)
	if len(p.Lines) != 3 || p.Lines[0].Method != api.MethodContainerCount || p.Lines[1].Method != api.MethodTEU || p.Lines[2].Method != api.MethodShipment {
		t.Fatalf("lines recorded with their default methods: %+v", p.Lines)
	}

	// nothing is stuffed: both methods are refused, alone or with a line by another method in the command, and nothing is stored
	before := u.revision()
	byTEU := u.apportionCmd("planned-teu", 4, []int{3})
	byTEU.Method = api.MethodTEU
	for name, in := range map[string]api.PreviewConsolCostApportionment{
		"count at PLANNED":                         u.apportionCmd("planned-count", 4, []int{1}),
		"TEU at PLANNED":                           u.apportionCmd("planned-tea", 4, []int{2}),
		"TEU as the method of a SHIPMENT line":     byTEU,
		"every line, with one by SHIPMENT amongst": u.apportionCmd("planned-all", 4, nil),
	} {
		_, err := previewAndSubmit(ctx, f.module, in)
		u.refusedWith(name, err, "FIN_POOL_NO_BASIS")
		if err != nil && !strings.Contains(err.Error(), "stuffed") {
			t.Errorf("%s: the refusal does not say that a house's containers are known once it is stuffed: %v", name, err)
		}
	}
	if u.revision() != before || len(u.ap.pool(u.pool).Apportionments) != 0 {
		t.Fatalf("a refused apportionment changed the pool")
	}
	// the existing methods still fall back to the planned scope at PLANNED
	if a := u.apportion("planned-shipment", 4, []int{3}); a.Stage != api.BasisPlanned {
		t.Fatalf("a line by SHIPMENT at PLANNED: %+v", a)
	}

	// stuffed, but unit 2 is still being loaded: refused (every container must be sealed), nothing stored
	loading := containerBasis(f, true)
	loading.LoadUnits[1].Status = phyapi.UnitStatusInUse
	f.w.costBases["CONSOL-K"] = loading
	before = u.revision()
	for _, lines := range [][]int{{1}, {2}} {
		_, err := previewAndSubmit(ctx, f.module, u.apportionCmd("unsealed", 5, lines))
		u.refusedWith("a unit still loading", err, "FIN_POOL_NO_BASIS")
		if err != nil && !strings.Contains(err.Error(), "load unit 2 is IN_USE") {
			t.Errorf("the refusal does not name the unit and its status: %v", err)
		}
	}
	if u.revision() != before || len(u.ap.pool(u.pool).Apportionments) != 1 {
		t.Fatalf("a refused apportionment changed the pool")
	}

	// stuffed and sealed
	f.w.costBases["CONSOL-K"] = containerBasis(f, true)
	cmd := u.apportionCmd("apportion-1", 5, []int{1, 2})
	r := u.must(previewAndSubmit(ctx, f.module, cmd))
	one := u.ap.pool(u.pool).Apportionments[1]
	if one.Stage != api.BasisActual || len(one.Lines) != 2 {
		t.Fatalf("apportionment 2: %+v", one)
	}
	u.checkLine("by count", one, 1, api.MethodContainerCount, share(1, "A", 1, "CNT", 2_500), share(2, "B", 1, "CNT", 2_500), share(3, "C", 2, "CNT", 5_001))
	u.checkLine("by TEU", one, 2, api.MethodTEU, share(1, "A", 10, "DTE", 1_667), share(2, "B", 20, "DTE", 3_334), share(3, "C", 30, "DTE", 5_000))
	if again, err := previewAndSubmit(ctx, f.module, cmd); err != nil || again != r {
		t.Fatalf("replay: %+v %v, want %+v", again, err, r)
	}
	if n := len(u.ap.pool(u.pool).Apportionments); n != 2 {
		t.Fatalf("the replay stored another apportionment: %d", n)
	}
	// line 3 by TEU as the command's method
	cmd3 := u.apportionCmd("apportion-3", 5, []int{3})
	cmd3.Method = api.MethodTEU
	u.must(previewAndSubmit(ctx, f.module, cmd3))
	three := u.ap.pool(u.pool).Apportionments[2]
	u.checkLine("line 3 by TEU", three, 3, api.MethodTEU, share(1, "A", 10, "DTE", 1_000), share(2, "B", 20, "DTE", 2_000), share(3, "C", 30, "DTE", 3_000))

	// each job carries its shares; the profit of the consolidation agrees
	for job, want := range map[string]int64{"JOB-A": 2_500 + 1_667 + 1_000, "JOB-B": 2_500 + 3_334 + 2_000, "JOB-C": 5_001 + 5_000 + 3_000} {
		if got := u.poolCost(job); got != want {
			t.Errorf("pool cost of %s is %d, want %d", job, got, want)
		}
	}
	if s := u.poolShares("JOB-C"); len(s) != 3 || s[0].Method != api.MethodContainerCount || s[0].Quantity != 2 || s[0].Unit != "CNT" || s[0].Stage != api.BasisActual ||
		s[1].Method != api.MethodTEU || s[1].Quantity != 30 || s[1].Unit != "DTE" || s[1].Local != cny(5_000) {
		t.Fatalf("JOB-C's shares: %+v", s)
	}
	consol, err := f.module.GetConsolProfit(ctx, api.GetConsolProfit{TenantID: f.tenant, ConsolidationID: "CONSOL-K"})
	if err != nil || consol.Pooled != cny(26_002) || consol.Apportioned != cny(26_002) || consol.Unapportioned != cny(0) || len(consol.Houses) != 3 ||
		consol.Houses[0].PoolCost != cny(5_167) || consol.Houses[1].PoolCost != cny(7_834) || consol.Houses[2].PoolCost != cny(13_001) {
		t.Fatalf("consol profit: %+v %v", consol, err)
	}

	// the invoice is matched final and the pool closes
	u.must(f.module.RecordCreditorInvoice(ctx, api.RecordCreditorInvoice{CommandID: "invoice", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-K",
		LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "K-1", InvoiceDate: f.hours(7).Format(time.DateOnly), Currency: "CNY",
		Lines:      []api.CreditorInvoiceLine{{ChargeCode: "SEA_FREIGHT", Amount: 10_001}, {ChargeCode: "THC", Amount: 10_001}, {ChargeCode: "DOCS", Amount: 6_000}},
		OccurredAt: f.hours(7)}))
	u.must(f.module.MatchCreditorInvoice(ctx, u.match("CI-K", "ap-1", 8, onPool("P-K", 1, 1, 10_001, true), onPool("P-K", 2, 2, 10_001, true),
		onPool("P-K", 3, 3, 6_000, true))))
	closeCmd := func(id string, hour int) api.CloseConsolCostPool {
		return api.CloseConsolCostPool{CommandID: id, TenantID: f.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(), OccurredAt: f.hours(hour)}
	}
	u.must(f.module.CloseConsolCostPool(ctx, closeCmd("close-1", 9)))
	if p := u.ap.pool(u.pool); p.State != api.PoolClosed {
		t.Fatalf("pool: %+v", p)
	}

	// PHY reports SH-C in the 20-foot container only: the close is refused until each line is apportioned again
	u.must(f.module.ReopenConsolCostPool(ctx, api.ReopenConsolCostPool{CommandID: "reopen", TenantID: f.tenant, ActorID: "fin-1", PoolID: u.pool,
		ExpectedRevision: u.revision(), Reason: "SH-C was moved out of the 40-foot container", OccurredAt: f.hours(10)}))
	moved := containerBasis(f, true)
	moved.Revision = 8
	moved.Houses[2].Units = []phyapi.HouseUnitLoad{{UnitNo: 1, LoadedPacks: 3}}
	f.w.costBases["CONSOL-K"] = moved
	notClosable := func(id string, hour int, why string) {
		t.Helper()
		_, err := f.module.CloseConsolCostPool(ctx, closeCmd(id, hour))
		u.refusedWith(why, err, "FIN_POOL_NOT_CLOSABLE")
	}
	notClosable("close-2", 11, "closed on the old containers")
	u.apportion("apportion-4", 12, []int{1, 2})
	notClosable("close-3", 13, "closed with line 3 still on the old containers")
	cmd3b := u.apportionCmd("apportion-5", 14, []int{3})
	cmd3b.Method = api.MethodTEU
	u.must(previewAndSubmit(ctx, f.module, cmd3b))
	last := u.ap.pool(u.pool).Apportionments
	u.checkLine("by count again", last[3], 1, api.MethodContainerCount, share(1, "A", 1, "CNT", 3_334), share(2, "B", 1, "CNT", 3_334), share(3, "C", 1, "CNT", 3_333))
	u.checkLine("by TEU again", last[3], 2, api.MethodTEU, share(1, "A", 10, "DTE", 2_500), share(2, "B", 20, "DTE", 5_001), share(3, "C", 10, "DTE", 2_500))
	u.checkLine("line 3 again", last[4], 3, api.MethodTEU, share(1, "A", 10, "DTE", 1_500), share(2, "B", 20, "DTE", 3_000), share(3, "C", 10, "DTE", 1_500))
	u.must(f.module.CloseConsolCostPool(ctx, closeCmd("close-4", 15)))
}

// What the database refuses and accepts past FIN on the application role (V2.80): a cost line and an apportionment line
// with a method outside the list (NO_SUCH_METHOD), a share with a unit outside the list (NO_SUCH_UNIT), each written as a real
// apportionment is written (the pool row touched in the same transaction, a valid line, a share used) so that only the
// named check refuses it; a line of each new method with its unit, written the same way, is accepted and reads back.
func TestContainerMethodsPastFINOnPostgres(t *testing.T) {
	u := containerWorld(t, true)
	f := u.fixture
	u.record("cost-1", "SEA_FREIGHT", api.MethodContainerCount, 100_000)
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
INSERT INTO cargo_fin.pool_apportionment_share ` + columns + ` VALUES ($1,'P-K',` + itoa(no) + `,1,1,'SH-A','JOB-A',true,1,'` + unit + `',100000,100000);
INSERT INTO cargo_fin.pool_apportionment_share ` + columns + ` VALUES ($1,'P-K',` + itoa(no) + `,1,2,'SH-C','JOB-C',false,0,NULL,0,0)`
	}
	// a cost line (the next, no 2) with its first version of zero, as a pool's lines are written
	costLine := func(method string) string {
		return bump + `INSERT INTO cargo_fin.pool_cost_line (tenant_id,pool_id,line_no,charge_code,method,recorded_by,recorded_at,entered_at)
            VALUES ($1,'P-K',2,'X','` + method + `','x',now(),now());
INSERT INTO cargo_fin.pool_cost_version (tenant_id,pool_id,line_no,version_no,creditor_revision,currency,amount_minor,local_minor,status,recorded_by,
            recorded_at,entered_at) VALUES ($1,'P-K',2,1,0,'CNY',0,0,'ACCRUED','x',now(),now())`
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
		"a method outside the list on a cost line":           {costLine("NO_SUCH_METHOD"), "pool_cost_line_method_check"},
		"a method outside the list on an apportionment line": {line(2, "NO_SUCH_METHOD", "CNT"), "pool_apportionment_line_method_check"},
		"a unit outside the list":                            {line(2, "CONTAINER_COUNT", "NO_SUCH_UNIT"), "pool_apportionment_share_unit_check"},
	} {
		err := run(tc.sql)
		if state, constraint := sqlState(err); state != "23514" || constraint != tc.check {
			t.Errorf("%s: %v (state %s, constraint %s), want 23514 %s", name, err, state, constraint, tc.check)
		}
	}
	// the new methods and units are accepted, and read back
	if err := run(line(2, "CONTAINER_COUNT", "CNT")); err != nil {
		t.Fatalf("a line by container count: %v", err)
	}
	if err := run(line(3, "TEU", "DTE")); err != nil {
		t.Fatalf("a line by TEU: %v", err)
	}
	p := u.ap.pool(u.pool)
	if len(p.Apportionments) != 3 {
		t.Fatalf("apportionments: %+v", p.Apportionments)
	}
	for i, want := range []struct{ method, unit string }{{api.MethodContainerCount, "CNT"}, {api.MethodTEU, "DTE"}} {
		la := p.Apportionments[i+1].Lines[0]
		if la.Method != want.method || la.Shares[0].Unit != want.unit || la.Shares[0].Amount != cny(100_000) || !la.Shares[1].NotUsed {
			t.Errorf("apportionment %d reads back as %+v", i+2, la)
		}
	}
	if err := run(costLine("TEU")); err != nil {
		t.Fatalf("a cost line by TEU: %v", err)
	}
	// and the next one, line 3, by container count
	if err := run(strings.ReplaceAll(costLine("CONTAINER_COUNT"), "'P-K',2,", "'P-K',3,")); err != nil {
		t.Fatalf("a cost line by container count: %v", err)
	}
}

func itoa(n int) string { return string(rune('0' + n)) }
