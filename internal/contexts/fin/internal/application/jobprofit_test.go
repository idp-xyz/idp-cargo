package application

import (
	"context"
	"errors"
	"strings"
	"testing"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func cny(n int64) kernel.Money { return kernel.Money{Currency: "CNY", Minor: n} }

// A job's share of the pool of a consolidation its house left is a former share: it is not in the job's cost and is
// listed apart; its share of another pool and a loss it bears are in it.
func TestAFormerSharePendingIsNotInTheJobsCostAndALossIs(t *testing.T) {
	shares := []PoolShare{
		{PoolID: "P-1", ConsolidationID: "C-1", LineNo: 1, Amount: cny(50_000), Local: 50_000},
		{PoolID: "P-1", ConsolidationID: "C-1", LineNo: 2, Amount: cny(7_000), Local: 7_000},
		{PoolID: "P-2", ConsolidationID: "C-2", LineNo: 1, Amount: cny(10_001), Local: 10_001},
	}
	losses := []PoolLoss{{PoolID: "P-3", LineNo: 1, Voucher: "CLAIM-1", Amount: cny(10_000), Local: 10_000}}
	left := fin.CostBasis{ConsolidationID: "C-1", FormerHouses: []fin.FormerHouseFacts{{ShipmentID: "SH-H03"}}}
	live := fin.CostBasis{ConsolidationID: "C-2", Houses: []fin.HouseFacts{{ShipmentID: "SH-H03"}}}
	got := SplitFormerShares(shares, losses, "SH-H03", map[string]fin.CostBasis{"C-1": left, "C-2": live})
	if len(got.Former) != 2 || got.Former[0].LineNo != 1 || got.Former[1].LineNo != 2 || got.FormerTotal != 57_000 || len(got.Borne) != 1 || got.Borne[0].PoolID != "P-2" ||
		len(got.Losses) != 1 || got.LossTotal != 10_000 || got.Cost != 20_001 {
		t.Fatalf("the split: %+v", got)
	}
	// the same shares of another house of the consolidation: it did not leave, and bears them
	other := SplitFormerShares(shares, nil, "SH-H01", map[string]fin.CostBasis{"C-1": left, "C-2": live})
	if len(other.Former) != 0 || other.Cost != 67_001 || other.FormerTotal != 0 {
		t.Fatalf("a house that did not leave: %+v", other)
	}
	// a share whose consolidation was not read is borne; PHY's answer is the caller's to get
	if unread := SplitFormerShares(shares, nil, "SH-H03", nil); len(unread.Former) != 0 || unread.Cost != 67_001 {
		t.Fatalf("no basis read: %+v", unread)
	}
}

// With no PHY wired a job that holds a pool share is answered ErrMissingDependency (a defensive check: the module
// itself refuses to start without PHY); a job with no share asks nothing.
func TestFormerBasesWithNoPHYAreRefusedOnlyForAJobWithAShare(t *testing.T) {
	d := Deps{}
	if _, err := d.FormerBases(context.Background(), "T", []PoolShare{{PoolID: "P-1", ConsolidationID: "C-1"}}); !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("a share, no PHY: %v", err)
	}
	if b, err := d.FormerBases(context.Background(), "T", nil); err != nil || len(b) != 0 {
		t.Fatalf("no share, no PHY: %v %v", b, err)
	}
}

// The refusal of a cancel names every holding: a share in an open pool and a loss in a closed one, the second with the
// way out that starts by reopening the pool.
func TestTheRefusalOfACancelNamesEveryHolding(t *testing.T) {
	err := holdingsRefusal("JOB-C", []PoolHolding{{PoolID: "P-1", LineNo: 1}, {PoolID: "P-2", LineNo: 3, Loss: true, Closed: true}})
	if !errors.Is(err, fin.ErrJobBearsPoolCost) {
		t.Fatal(err)
	}
	m := err.Error()
	for _, part := range []string{"job JOB-C bears a share on line 1 of pool P-1", "a re-routing loss on line 3 of pool P-2", "reopen the pool first (ReopenConsolCostPool), then have finance change the attribution"} {
		if !strings.Contains(m, part) {
			t.Fatalf("%q does not say %q", m, part)
		}
	}
	if strings.Count(m, "reopen the pool first") != 1 {
		t.Fatalf("only the closed pool needs reopening: %q", m)
	}
}
