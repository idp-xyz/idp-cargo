//go:build integration

package integration

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// Lines of one container on PostgreSQL (V2.104), over containerBasis: SH-A has 4 packs in unit 1, SH-B 2 packs in unit 2,
// and SH-C 1 pack in unit 1 and 2 in unit 2.

func (u usedWorld) recordIn(id, code, method string, unitNo int, amount int64) error {
	_, err := u.module.RecordConsolCost(u.ctx, api.RecordConsolCost{CommandID: id, TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool,
		ExpectedRevision: u.revision(), ChargeCode: code, Method: method, Cost: api.ChargeSide{PartyID: "airline-mu", Amount: cny(amount)},
		Reason: "the container's freight", OccurredAt: u.hours(3), LoadUnitNo: unitNo})
	return err
}

// unitShare is what a share of a line of one container should be: the house, its quantity in the container, its
// amount, whether it took part, and whether it was in the container.
type unitShare struct {
	house    string
	quantity int64
	amount   int64
	used, in bool
}

func (u usedWorld) checkUnitLine(name string, a api.PoolApportionment, line int, want ...unitShare) {
	u.t.Helper()
	for _, la := range a.Lines {
		if la.LineNo != line {
			continue
		}
		if len(la.Shares) != len(want) {
			u.t.Fatalf("%s: line %d has %d shares: %+v", name, line, len(la.Shares), la.Shares)
		}
		for i, w := range want {
			s := la.Shares[i]
			if s.ShipmentID != "SH-"+w.house || s.Quantity != w.quantity || s.Amount != cny(w.amount) || s.NotUsed == w.used || s.InUnit == nil || *s.InUnit != w.in {
				in := "nil"
				if s.InUnit != nil {
					in = map[bool]string{true: "in", false: "out"}[*s.InUnit]
				}
				u.t.Errorf("%s: line %d share %d is %+v (in the container: %s), want %+v", name, line, i+1, s, in, w)
			}
		}
		return
	}
	u.t.Fatalf("%s: line %d is not in apportionment %d", name, line, a.ApportionmentNo)
}

func TestLinesOfOneContainerOnPostgres(t *testing.T) {
	u := containerWorld(t, true)
	f := u.fixture
	u.must(api.Receipt{}, u.recordIn("cost-1", "SEA_FREIGHT", api.MethodPieces, 1, 5_000)) // unit 1: SH-A 4, SH-C 1
	u.must(api.Receipt{}, u.recordIn("cost-2", "SEA_FREIGHT", api.MethodPieces, 2, 4_000)) // unit 2: SH-B 2, SH-C 2
	u.record("cost-3", "DOCS", api.MethodShipment, 300)                                    // the whole consolidation
	p := u.ap.pool(u.pool)
	if p.Lines[0].LoadUnitNo != 1 || p.Lines[0].Versions[0].LoadUnitNo != 1 || p.Lines[1].LoadUnitNo != 2 || p.Lines[2].LoadUnitNo != 0 {
		t.Fatalf("lines: %+v", p.Lines)
	}
	// what a line of one container cannot be
	u.refusedWith("by container count", u.recordIn("count", "THC", api.MethodContainerCount, 1, 100), "divide it by SHIPMENT")
	u.refusedWith("container 1000", u.recordIn("far", "THC", api.MethodPieces, 1_000, 100), "numbered 1 to 999")

	a := u.apportion("apportion-1", 4, nil)
	u.checkUnitLine("unit 1", a, 1, unitShare{"A", 4, 4_000, true, true}, unitShare{"B", 0, 0, false, false}, unitShare{"C", 1, 1_000, true, true})
	u.checkUnitLine("unit 2", a, 2, unitShare{"A", 0, 0, false, false}, unitShare{"B", 2, 2_000, true, true}, unitShare{"C", 2, 2_000, true, true})
	u.checkLine("the whole consolidation", a, 3, api.MethodShipment, share(1, "A", 1, "SHP", 100), share(2, "B", 1, "SHP", 100), share(3, "C", 1, "SHP", 100))
	for job, want := range map[string]int64{"JOB-A": 4_100, "JOB-B": 2_100, "JOB-C": 3_100} {
		if got := u.poolCost(job); got != want {
			t.Errorf("pool cost of %s is %d, want %d", job, got, want)
		}
	}

	// finance leaves SH-C out of unit 1's line; SH-B, not in it, changes nothing there
	a = u.apportion("apportion-2", 5, []int{1}, "SH-B", "SH-C")
	u.checkUnitLine("SH-C left out", a, 1, unitShare{"A", 4, 5_000, true, true}, unitShare{"B", 0, 0, false, false}, unitShare{"C", 0, 0, false, true})

	// a container not sealed is refused, and nothing is stored
	loading := containerBasis(f, true)
	loading.LoadUnits[1].Status = phyapi.UnitStatusInUse
	f.w.costBases["CONSOL-K"] = loading
	before := u.revision()
	_, err := previewAndSubmit(u.ctx, f.module, u.apportionCmd("unsealed", 6, []int{2}))
	u.refusedWith("unit 2 loading", err, "FIN_POOL_NO_BASIS")
	if err != nil && !strings.Contains(err.Error(), "seal the container first") {
		t.Errorf("the refusal names no way out: %v", err)
	}
	if u.revision() != before {
		t.Fatal("a refused apportionment changed the pool")
	}
	f.w.costBases["CONSOL-K"] = containerBasis(f, true)

	// SH-C moves to unit 2 with all its 3 packs: by default its share follows its cargo, and the shares record the move
	moved := containerBasis(f, true)
	moved.Revision = 8
	moved.Houses[2].Units = []phyapi.HouseUnitLoad{{UnitNo: 2, LoadedPacks: 3}}
	f.w.costBases["CONSOL-K"] = moved
	if items := u.pending(); !hasPending(items, 2, "LINE_NOT_ON_CURRENT_BASIS") {
		t.Errorf("unit 2's line on the old containers is not pending: %+v", items)
	}
	// SH-C was left out of unit 1's line by finance: AllHouses takes the line's houses as they are now
	a = u.apportionAll("apportion-4", 8, []int{1, 2})
	u.checkUnitLine("unit 1 after the move", a, 1, unitShare{"A", 4, 5_000, true, true}, unitShare{"B", 0, 0, false, false}, unitShare{"C", 0, 0, false, false})
	u.checkUnitLine("unit 2 after the move", a, 2, unitShare{"A", 0, 0, false, false}, unitShare{"B", 2, 1_600, true, true}, unitShare{"C", 3, 2_400, true, true})

	// finance may instead attribute SH-C's former share of unit 1's line (the 1,000 it bore there) as its own loss
	f.w.costBases["CONSOL-K"] = containerBasis(f, true)
	u.apportionAll("back", 9, []int{1})
	f.w.costBases["CONSOL-K"] = moved
	c := u.attributeCmd("attribute", "fin-1", 10, api.AttributionStatement{LineNo: 1, ShipmentID: "SH-C", Loss: 1_000, Voucher: "MOVE-1", Reason: "moved at the shipper's request"})
	u.must(previewAndSubmit(u.ctx, f.module, c))
	last := u.ap.pool(u.pool).Apportionments
	la := last[len(last)-1].Lines[0]
	if len(la.Attributions) != 1 || la.Attributions[0].ShipmentID != "SH-C" || la.Attributions[0].Loss != cny(1_000) || la.Attributions[0].JobID != "JOB-C" {
		t.Fatalf("the attribution: %+v", la.Attributions)
	}
	u.checkUnitLine("unit 1 with SH-C's loss", last[len(last)-1], 1, unitShare{"A", 4, 4_000, true, true}, unitShare{"B", 0, 0, false, false},
		unitShare{"C", 0, 0, false, false})

	// a line moved to another container is a revision, and is divided there
	u.must(f.module.ReviseConsolCost(u.ctx, api.ReviseConsolCost{CommandID: "move", TenantID: f.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(),
		LineNo: 3, ExpectedVersion: 1, Value: api.ChargeSide{Amount: cny(300)}, Reason: "unit 2's documents", MoveToLoadUnit: true, LoadUnitNo: 2,
		OccurredAt: u.hours(11)}))
	if l := u.ap.pool(u.pool).Lines[2]; l.LoadUnitNo != 2 || len(l.Versions) != 2 || l.Versions[1].LoadUnitNo != 2 || l.Versions[0].LoadUnitNo != 0 {
		t.Fatalf("line 3 moved: %+v", l)
	}
	a = u.apportion("apportion-5", 12, []int{3})
	u.checkUnitLine("line 3 in unit 2", a, 3, unitShare{"A", 0, 0, false, false}, unitShare{"B", 1, 150, true, true}, unitShare{"C", 1, 150, true, true})
}

func hasPending(items []api.PoolPendingItem, line int, kind string) bool {
	for _, i := range items {
		if i.LineNo == line && i.Kind == kind {
			return true
		}
	}
	return false
}

// The database's rules of a line of one container (fin/000024), each by a raw write on the application role that only
// that rule refuses.
func TestTheDatabaseKeepsTheRulesOfALineOfOneContainerOnPostgres(t *testing.T) {
	u := containerWorld(t, true)
	u.must(api.Receipt{}, u.recordIn("cost-1", "SEA_FREIGHT", api.MethodPieces, 1, 5_000))
	u.apportion("apportion-1", 4, []int{1})

	const bump = `UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-K';
`
	version := func(line, unit string) string {
		return bump + `INSERT INTO cargo_fin.pool_cost_line (tenant_id,pool_id,line_no,charge_code,method,recorded_by,recorded_at,entered_at)
            VALUES ($1,'P-K',2,'X','` + line + `','x',now(),now());
INSERT INTO cargo_fin.pool_cost_version (tenant_id,pool_id,line_no,version_no,creditor_revision,currency,amount_minor,local_minor,status,recorded_by,
            recorded_at,entered_at,load_unit_no) VALUES ($1,'P-K',2,1,0,'CNY',0,0,'ACCRUED','x',now(),now(),` + unit + `)`
	}
	// an apportionment (no 2) of line 1, version 1 (unit 1, CNY 50.00), by method, with SH-A's and SH-C's shares as given
	const columns = `(tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,used,quantity,unit,amount_minor,local_minor,in_unit)`
	apportionment := func(method, aIn, bIn string, bUsed bool) string {
		bAmount, bQuantity, bUnit, bUsedText := "0", "0", "NULL", "false"
		if bUsed {
			bAmount, bQuantity, bUnit, bUsedText = "1000", "1", "'PCS'", "true"
		}
		aAmount := "5000"
		if bUsed {
			aAmount = "4000"
		}
		return bump + `INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at)
            VALUES ($1,'P-K',2,7,'ACTUAL','x',now(),now());
INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,amount_minor,local_minor)
            VALUES ($1,'P-K',2,1,1,'` + method + `',5000,5000);
INSERT INTO cargo_fin.pool_apportionment_share ` + columns + ` VALUES ($1,'P-K',2,1,1,'SH-A','JOB-A',true,4,'PCS',` + aAmount + `,` + aAmount + `,` + aIn + `);
INSERT INTO cargo_fin.pool_apportionment_share ` + columns + ` VALUES ($1,'P-K',2,1,2,'SH-B','JOB-B',` + bUsedText + `,` + bQuantity + `,` + bUnit + `,` + bAmount + `,` + bAmount + `,` + bIn + `)`
	}
	for name, tc := range map[string]struct{ sql, check string }{
		"container 0":                               {version("PIECES", "0"), "pool_cost_version_load_unit"},
		"container 1000":                            {version("PIECES", "1000"), "pool_cost_version_load_unit"},
		"a line by TEU in one container":            {version("TEU", "1"), "pool_cost_version_unit_method"},
		"divided by container count":                {apportionment("CONTAINER_COUNT", "true", "false", false), "pool_apportionment_line_unit_method"},
		"a share used that is not in the container": {apportionment("PIECES", "true", "false", true), "pool_apportionment_share_in_unit"},
		"a share that does not say":                 {apportionment("PIECES", "true", "NULL", false), "pool_share_in_unit_of_line"},
	} {
		err := u.rawRun(tc.sql)
		if state, constraint := sqlState(err); state != "23514" || constraint != tc.check {
			t.Errorf("%s: %v (state %s, constraint %s), want 23514 %s", name, err, state, constraint, tc.check)
		}
	}
	// a share of a line of the whole consolidation does not say either: line 2, of the whole consolidation, apportioned with a share that says
	whole := version("PIECES", "NULL") + `;
INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at) VALUES ($1,'P-K',2,7,'ACTUAL','x',now(),now());
INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,amount_minor,local_minor) VALUES ($1,'P-K',2,2,1,'PIECES',0,0);
INSERT INTO cargo_fin.pool_apportionment_share ` + columns + ` VALUES ($1,'P-K',2,2,1,'SH-A','JOB-A',true,4,'PCS',0,0,true)`
	if state, constraint := sqlState(u.rawRun(whole)); state != "23514" || constraint != "pool_share_in_unit_of_line" {
		t.Errorf("a share of a whole line that says: state %s constraint %s", state, constraint)
	}
	// the same apportionment written right is accepted and reads back
	if err := u.rawRun(apportionment("PIECES", "true", "false", false)); err != nil {
		t.Fatalf("written right: %v", err)
	}
	if a := u.ap.pool(u.pool).Apportionments[1]; a.Lines[0].Shares[0].InUnit == nil || !*a.Lines[0].Shares[0].InUnit || a.Lines[0].Shares[1].InUnit == nil ||
		*a.Lines[0].Shares[1].InUnit {
		t.Fatalf("reads back as %+v", a.Lines[0].Shares)
	}
}

func TestALineOfOneContainerNeedsASeaLCLPoolOnPostgres(t *testing.T) {
	u := containerWorld(t, true)
	f := u.fixture
	// an air pool of its own, opened as FIN opens one: the database refuses a version of its line in a container
	f.w.costBases["CONSOL-AIR"] = airConsol(f, "CONSOL-AIR")
	u.must(f.module.OpenConsolCostPool(u.ctx, api.OpenConsolCostPool{CommandID: "air-pool", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-AIR",
		ConsolidationID: "CONSOL-AIR", OccurredAt: f.hours(2)}))
	if p := u.ap.pool("P-AIR"); p.Mode != "AIR" {
		t.Fatalf("pool P-AIR is %s", p.Mode)
	}
	// FIN refuses a line of one container on that pool, naming the way out
	_, err := f.module.RecordConsolCost(u.ctx, api.RecordConsolCost{CommandID: "air-unit", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-AIR",
		ExpectedRevision: u.ap.pool("P-AIR").Revision, ChargeCode: "AIR_FREIGHT", Method: api.MethodChargeable, Cost: api.ChargeSide{Amount: cny(100)},
		Reason: "x", OccurredAt: f.hours(3), LoadUnitNo: 1})
	u.refusedWith("FIN on an air pool", err, "record it for the whole consolidation")
	sql := `UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-AIR';
INSERT INTO cargo_fin.pool_cost_line (tenant_id,pool_id,line_no,charge_code,method,recorded_by,recorded_at,entered_at) VALUES ($1,'P-AIR',1,'X','PIECES','x',now(),now());
INSERT INTO cargo_fin.pool_cost_version (tenant_id,pool_id,line_no,version_no,creditor_revision,currency,amount_minor,local_minor,status,recorded_by,
            recorded_at,entered_at,load_unit_no) VALUES ($1,'P-AIR',1,1,0,'CNY',0,0,'ACCRUED','x',now(),now(),1)`
	if state, constraint := sqlState(u.rawRun(sql)); state != "23514" || constraint != "pool_cost_version_unit_sea" {
		t.Errorf("an air pool's line in a container: state %s constraint %s, want 23514 pool_cost_version_unit_sea", state, constraint)
	}
	// the same line of the whole consolidation is accepted
	if err := u.rawRun(strings.Replace(sql, "now(),now(),1)", "now(),now(),NULL)", 1)); err != nil {
		t.Fatalf("the whole consolidation: %v", err)
	}
}
