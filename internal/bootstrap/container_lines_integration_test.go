//go:build integration

package bootstrap

import (
	"testing"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// The owner's acceptance of V2.104 (验收标准_V2.104_按箱自动圈定.md), with every context on PostgreSQL: the two boxes of
// the owner's scheme (box A: H01, H02, H03; box B: H04, H05; both sealed), box A's and box B's freight each a line of its
// own container, the document fee a line of the whole consolidation. One apportionment of the three lines, with no house
// named out, gives what stage 2c gave by naming them: box A 1,000 / 1,500 / 500, box B 1,600 / 800, the document fee
// 60 each; cost 5,700 and gross profit 3,500.
func TestEachBoxsFreightIsDividedOverTheHousesInItWithNoHouseNamedOut(t *testing.T) {
	d := newSeaLineDay(t, "cargo-container-lines-")
	move := func(id string, line int, unitNo int, minor int64) {
		t.Helper()
		_, err := d.fin.ReviseConsolCost(d.ctx, finapi.ReviseConsolCost{CommandID: id, TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision,
			LineNo: line, ExpectedVersion: 1, Value: finapi.ChargeSide{Amount: kernel.Money{Currency: "CNY", Minor: minor}}, Reason: "the freight of one box",
			MoveToLoadUnit: true, LoadUnitNo: unitNo, OccurredAt: d.hours(16)})
		d.must(id, err)
	}
	move("fin-box-a", 1, 1, 300_000)
	move("fin-box-b", 2, 2, 240_000)
	p := d.pool()
	if p.Lines[0].LoadUnitNo != 1 || p.Lines[1].LoadUnitNo != 2 || p.Lines[2].LoadUnitNo != 0 || p.Lines[0].Versions[1].Reason != "the freight of one box" {
		t.Fatalf("the lines: %+v", p.Lines)
	}
	d.must("the three lines at once", d.apportionLines("fin-app-all", nil, nil))
	d.wantShares("box A", 1, map[string]int64{"H01": 100_000, "H02": 150_000, "H03": 50_000})
	d.wantShares("box B", 2, map[string]int64{"H04": 160_000, "H05": 80_000})
	d.wantShares("the document fee", 3, map[string]int64{"H01": 6_000, "H02": 6_000, "H03": 6_000, "H04": 6_000, "H05": 6_000})
	d.wantJobs("one apportionment", map[string]int64{"H01": 106_000, "H02": 156_000, "H03": 56_000, "H04": 166_000, "H05": 86_000}, 570_000, 350_000)

	// what FIN answers of each box's line: who was in it, with what it had there
	a := d.pool().Apportionments[0]
	for _, la := range a.Lines[:2] {
		for _, s := range la.Shares {
			inA := s.ShipmentID == "SH-H01" || s.ShipmentID == "SH-H02" || s.ShipmentID == "SH-H03"
			if s.InUnit == nil || *s.InUnit != (inA == (la.LineNo == 1)) || s.NotUsed == *s.InUnit {
				t.Errorf("line %d, %s: %+v", la.LineNo, s.ShipmentID, s)
			}
		}
	}
	for _, s := range a.Lines[2].Shares {
		if s.InUnit != nil {
			t.Errorf("the document fee, %s says whether it is in a container: %+v", s.ShipmentID, s)
		}
	}
	// by container count, a box's line is refused with the way out
	c := finapi.PreviewConsolCostApportionment{CommandID: "fin-app-count", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision,
		LineNos: []int{1}, Method: finapi.MethodContainerCount, OccurredAt: d.later()}
	if _, err := previewAndSubmit(d.ctx, d.fin, c); err == nil {
		t.Fatal("box A's freight divided by container count")
	}
}
