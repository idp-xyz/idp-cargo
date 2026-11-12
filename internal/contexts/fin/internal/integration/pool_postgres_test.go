//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func kg(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
func cc(n int64) kernel.Quantity { return kernel.Quantity{Unit: "CMQ", Amount: n} }

// airConsol is PhysicalCostBasis v1 of an air consolidation operated by LE with the two house shipments:
// SH-A loaded 4 packs, 300 kg, 1.2 m³ (chargeable 300 kg) and SH-B loaded 2 packs, 100 kg, 1.2 m³
// (volumetric 200 kg), so CHARGEABLE divides 3:2 and PIECES 4:2.
func airConsol(f *fixture, id string) phyapi.PhysicalCostBasis {
	return phyapi.PhysicalCostBasis{TenantID: f.tenant, ConsolidationID: id, Revision: 7, Mode: "AIR", Kind: "AIR_CONSOL",
		OperatingLegalEntityID: "LE", State: "DEPARTED", Houses: []phyapi.HouseCostBasis{
			{ShipmentID: "SH-A", MembershipRevision: 2, Packs: 6, GrossWeight: kg(300), Volume: cc(1_200_000),
				LoadedPacks: 4, LoadedGrossWeight: kg(300), LoadedVolume: cc(1_200_000), LastLoadedAt: f.hours(1)},
			{ShipmentID: "SH-B", MembershipRevision: 2, Packs: 6, GrossWeight: kg(100), Volume: cc(1_200_000),
				LoadedPacks: 2, LoadedGrossWeight: kg(100), LoadedVolume: cc(1_200_000), LastLoadedAt: f.hours(1)}}}
}

// A consol cost pool on PostgreSQL (FB3): opened from PHY's PhysicalCostBasis v1 under the operating legal
// entity's policy, versioned cost lines converted at the ESTIMATE rate, apportionment by chargeable weight,
// pieces and by hand with supersession, the pool and profit views, and what the database refuses past FIN.
func TestConsolCostPoolOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.costBases["CONSOL-1"] = airConsol(f, "CONSOL-1")
	f.w.costBases["CONSOL-2"] = airConsol(f, "CONSOL-2")
	le2 := airConsol(f, "CONSOL-LE2")
	le2.OperatingLegalEntityID = "LE-2"
	f.w.costBases["CONSOL-LE2"] = le2
	fcl := airConsol(f, "CONSOL-FCL")
	fcl.Kind = "SEA_FCL"
	f.w.costBases["CONSOL-FCL"] = fcl

	if _, err := f.module.OpenFinancePolicy(ctx, api.OpenFinancePolicy{CommandID: "policy", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: f.hours(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.RecordExchangeRate(ctx, api.RecordExchangeRate{CommandID: "rate-usd", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: 1, RateType: api.RateEstimate, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2,
		ValidFrom: f.at.AddDate(0, 0, -1), ValidUntil: f.at.AddDate(0, 1, 0), Source: "CFETS central parity", OccurredAt: f.hours(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-a", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ShipmentID: "SH-A", OccurredAt: f.hours(1)}); err != nil {
		t.Fatal(err)
	}

	// opening the pool
	open := api.OpenConsolCostPool{CommandID: "pool", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1", ConsolidationID: "CONSOL-1", OccurredAt: f.hours(2)}
	unknown := open
	unknown.CommandID, unknown.PoolID, unknown.ConsolidationID = "pool-unknown", "P-U", "CONSOL-9"
	if _, err := f.module.OpenConsolCostPool(ctx, unknown); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a consolidation PHY does not know: %v", err)
	}
	crossEntity := open
	crossEntity.CommandID, crossEntity.PoolID, crossEntity.ConsolidationID = "pool-le2", "P-LE2", "CONSOL-LE2"
	if _, err := f.module.OpenConsolCostPool(ctx, crossEntity); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a consolidation operated by a legal entity not VERIFIED: %v", err)
	}
	wrongKind := open
	wrongKind.CommandID, wrongKind.PoolID, wrongKind.ConsolidationID = "pool-fcl", "P-FCL", "CONSOL-FCL"
	if _, err := f.module.OpenConsolCostPool(ctx, wrongKind); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an FCL consolidation pools no shared costs: %v", err)
	}
	r, err := f.module.OpenConsolCostPool(ctx, open)
	if err != nil || r.SubjectID != "P-1" || r.NewRevision != 1 {
		t.Fatalf("open pool: %+v %v", r, err)
	}
	if again, err := f.module.OpenConsolCostPool(ctx, open); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	second := open
	second.CommandID, second.PoolID = "pool-second", "P-1B"
	if _, err = f.module.OpenConsolCostPool(ctx, second); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a second pool of the consolidation and legal entity (UL-08): %v", err)
	}

	// cost lines: versioned accruals converted at the ESTIMATE rate
	record := api.RecordConsolCost{CommandID: "cost-freight", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: 1,
		ChargeCode: "AIR_FREIGHT", Description: "MAWB freight", SupplierRef: "784-12345675", Method: api.MethodChargeable,
		Cost: api.ChargeSide{PartyID: "airline-mu", Amount: usd(100_000)}, Reason: "MU's freight for the master", OccurredAt: f.hours(3)}
	stranger := record
	stranger.CommandID, stranger.Cost = "cost-stranger", api.ChargeSide{PartyID: "party-a", Amount: usd(1)}
	if _, err = f.module.RecordConsolCost(ctx, stranger); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a creditor without a creditor role: %v", err)
	}
	badMethod := record
	badMethod.CommandID, badMethod.Method = "cost-bad-method", "SPLIT"
	if _, err = f.module.RecordConsolCost(ctx, badMethod); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an unknown apportionment method: %v", err)
	}
	if r, err = f.module.RecordConsolCost(ctx, record); err != nil || r.NewRevision != 2 || r.Version != 1 {
		t.Fatalf("record: %+v %v", r, err)
	}
	revise := api.ReviseConsolCost{CommandID: "cost-revise", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: 2, LineNo: 1,
		ExpectedVersion: 1, Value: api.ChargeSide{Amount: cny(720_000)}, Reason: "settled in CNY with the GSA", OccurredAt: f.hours(4)}
	staleVersion := revise
	staleVersion.CommandID, staleVersion.ExpectedVersion = "cost-revise-stale", 2
	if _, err = f.module.ReviseConsolCost(ctx, staleVersion); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a revision at the wrong version (FIN_COST_VERSION): %v", err)
	}
	if r, err = f.module.ReviseConsolCost(ctx, revise); err != nil || r.NewRevision != 3 || r.Version != 2 {
		t.Fatalf("revise: %+v %v", r, err)
	}
	if r, err = f.module.RecordConsolCost(ctx, api.RecordConsolCost{CommandID: "cost-cfs", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: 3, ChargeCode: "CFS_HANDLING", Method: api.MethodPieces, Cost: api.ChargeSide{PartyID: "trucker", Amount: cny(50_000)},
		Reason: "CFS bill for the consol", OccurredAt: f.hours(5)}); err != nil || r.NewRevision != 4 || r.Version != 2 {
		t.Fatalf("second line: %+v %v", r, err)
	}

	// apportionment: every live house needs an OPEN job of the pool's legal entity
	apportion := api.ApportionConsolCosts{CommandID: "apportion-1", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: 4,
		OccurredAt: f.hours(6)}
	if _, err = f.module.ApportionConsolCosts(ctx, apportion); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_POOL_JOB_MISSING") {
		t.Fatalf("SH-B has no job yet: %v", err)
	}
	if _, err = f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-b", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-B",
		ShipmentID: "SH-B", OccurredAt: f.hours(6)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "sell-a", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: 1, ChargeCode: "AIR_FREIGHT", Sell: &api.ChargeSide{PartyID: "party-a", Amount: cny(800_000)}, Reason: "sold as per booking",
		OccurredAt: f.hours(6)}); err != nil {
		t.Fatal(err)
	}
	stale := apportion
	stale.CommandID, stale.ExpectedRevision = "apportion-stale", 3
	if _, err = f.module.ApportionConsolCosts(ctx, stale); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("an apportionment at a stale revision: %v", err)
	}
	if r, err = f.module.ApportionConsolCosts(ctx, apportion); err != nil || r.NewRevision != 5 || r.Version != 1 {
		t.Fatalf("apportion: %+v %v", r, err)
	}

	// a manual apportionment of line 2 supersedes its shares line by line
	manual := api.ApportionConsolCosts{CommandID: "apportion-manual", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: 5,
		LineNos: []int{2}, Method: api.MethodManual, Manual: []api.ManualShare{{ShipmentID: "SH-A", Amount: 10_000}, {ShipmentID: "SH-B", Amount: 40_000}},
		Reason: "the CFS charged SH-B's oversize piece", OccurredAt: f.hours(7)}
	short := manual
	short.CommandID, short.Manual = "apportion-short", []api.ManualShare{{ShipmentID: "SH-A", Amount: 10_000}, {ShipmentID: "SH-B", Amount: 39_999}}
	if _, err = f.module.ApportionConsolCosts(ctx, short); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_MANUAL_SHARES") {
		t.Fatalf("manual shares not summing to the line: %v", err)
	}
	strangerHouse := manual
	strangerHouse.CommandID, strangerHouse.Manual = "apportion-strange", []api.ManualShare{{ShipmentID: "SH-A", Amount: 10_000}, {ShipmentID: "SH-C", Amount: 40_000}}
	if _, err = f.module.ApportionConsolCosts(ctx, strangerHouse); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_MANUAL_SHARES") {
		t.Fatalf("a manual share of a house not in the consolidation: %v", err)
	}
	if r, err = f.module.ApportionConsolCosts(ctx, manual); err != nil || r.NewRevision != 6 || r.Version != 2 {
		t.Fatalf("manual apportion: %+v %v", r, err)
	}

	// a cancelled consolidation takes no apportionment
	cancelled := f.w.costBases["CONSOL-1"]
	cancelled.State = "CANCELLED"
	f.w.costBases["CONSOL-1"] = cancelled
	gone := apportion
	gone.CommandID, gone.ExpectedRevision = "apportion-gone", 6
	if _, err = f.module.ApportionConsolCosts(ctx, gone); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_POOL_NO_BASIS") {
		t.Fatalf("a cancelled consolidation: %v", err)
	}
	restored := f.w.costBases["CONSOL-1"]
	restored.State = "DEPARTED"
	f.w.costBases["CONSOL-1"] = restored

	// the pool view
	pool, err := f.module.GetConsolCostPool(ctx, api.GetConsolCostPool{TenantID: f.tenant, PoolID: "P-1"})
	if err != nil || pool.Revision != 6 || pool.Mode != "AIR" || pool.FunctionalCurrency != "CNY" || pool.BookCode != "MAIN" ||
		pool.LegalEntityID != "LE" || pool.State != "OPEN" || len(pool.Lines) != 2 || len(pool.Apportionments) != 2 {
		t.Fatalf("pool: %+v %v", pool, err)
	}
	if v := pool.Lines[0].Versions; len(v) != 2 || v[0].Amount != usd(100_000) || v[0].Local != cny(712_340) || v[0].CreditorPartyID != "airline-mu" ||
		v[0].Status != api.StatusAccrued || v[1].Amount != cny(720_000) || v[1].Local != cny(720_000) || v[1].CreditorPartyID != "airline-mu" ||
		v[1].Reason != "settled in CNY with the GSA" {
		t.Fatalf("line 1 versions: %+v", v)
	}
	first := pool.Apportionments[0]
	if first.Stage != api.BasisActual || first.BasisRevision != 7 || len(first.Lines) != 2 {
		t.Fatalf("apportionment 1: %+v", first)
	}
	chargeable := first.Lines[0]
	if chargeable.Method != api.MethodChargeable || chargeable.LineVersion != 2 || len(chargeable.Shares) != 2 ||
		chargeable.Shares[0] != (api.PoolShare{ShareNo: 1, ShipmentID: "SH-A", JobID: "JOB-A", Quantity: 300_000, Unit: "GRM", Amount: cny(432_000), Local: cny(432_000)}) ||
		chargeable.Shares[1] != (api.PoolShare{ShareNo: 2, ShipmentID: "SH-B", JobID: "JOB-B", Quantity: 200_000, Unit: "GRM", Amount: cny(288_000), Local: cny(288_000)}) {
		t.Fatalf("chargeable shares 3:2 of 720000: %+v", chargeable)
	}
	pieces := first.Lines[1]
	if pieces.Method != api.MethodPieces || pieces.LineVersion != 1 || len(pieces.Shares) != 2 ||
		pieces.Shares[0] != (api.PoolShare{ShareNo: 1, ShipmentID: "SH-A", JobID: "JOB-A", Quantity: 4, Unit: "PCS", Amount: cny(33_333), Local: cny(33_333)}) ||
		pieces.Shares[1] != (api.PoolShare{ShareNo: 2, ShipmentID: "SH-B", JobID: "JOB-B", Quantity: 2, Unit: "PCS", Amount: cny(16_667), Local: cny(16_667)}) {
		t.Fatalf("pieces shares by largest remainder: %+v", pieces)
	}
	handed := pool.Apportionments[1]
	if len(handed.Lines) != 1 || handed.Lines[0].LineNo != 2 || handed.Lines[0].Method != api.MethodManual ||
		handed.Lines[0].Reason != "the CFS charged SH-B's oversize piece" || len(handed.Lines[0].Shares) != 2 ||
		handed.Lines[0].Shares[0] != (api.PoolShare{ShareNo: 1, ShipmentID: "SH-A", JobID: "JOB-A", Quantity: 10_000, Amount: cny(10_000), Local: cny(10_000)}) {
		t.Fatalf("manual apportionment: %+v", handed)
	}
	if _, err = f.module.GetConsolCostPool(ctx, api.GetConsolCostPool{TenantID: f.tenant, PoolID: "P-9"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown pool: %v", err)
	}

	// a second pool at the PLANNED stage: CONSOL-2's SH-B has nothing loaded yet, so the attach plan scope weighs
	planned := f.w.costBases["CONSOL-2"]
	planned.Houses[1].LoadedPacks, planned.Houses[1].LoadedGrossWeight, planned.Houses[1].LoadedVolume = 0, kernel.Quantity{}, kernel.Quantity{}
	f.w.costBases["CONSOL-2"] = planned
	if _, err = f.module.OpenConsolCostPool(ctx, api.OpenConsolCostPool{CommandID: "pool-2", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-2",
		ConsolidationID: "CONSOL-2", OccurredAt: f.hours(8)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.RecordConsolCost(ctx, api.RecordConsolCost{CommandID: "cost-2", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-2",
		ExpectedRevision: 1, ChargeCode: "AIR_FREIGHT", Method: api.MethodChargeable, Cost: api.ChargeSide{Amount: cny(100_000)},
		Reason: "booked with the second carrier", OccurredAt: f.hours(9)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.ApportionConsolCosts(ctx, api.ApportionConsolCosts{CommandID: "apportion-2", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-2",
		ExpectedRevision: 2, OccurredAt: f.hours(10)}); err != nil {
		t.Fatal(err)
	}
	pool2, err := f.module.GetConsolCostPool(ctx, api.GetConsolCostPool{TenantID: f.tenant, PoolID: "P-2"})
	if err != nil || len(pool2.Apportionments) != 1 || pool2.Apportionments[0].Stage != api.BasisPlanned ||
		pool2.Apportionments[0].Lines[0].Shares[0].Local != cny(60_000) || pool2.Apportionments[0].Lines[0].Shares[1].Local != cny(40_000) {
		t.Fatalf("planned apportionment 3:2 of 100000: %+v %v", pool2, err)
	}

	// the job and consol profit views
	profit, err := f.module.GetJobProfit(ctx, api.GetJobProfit{TenantID: f.tenant, JobID: "JOB-A"})
	if err != nil || profit.Revenue != cny(800_000) || profit.Cost != cny(0) || profit.PoolCost != cny(502_000) || profit.Profit != cny(298_000) ||
		len(profit.PoolShares) != 3 {
		t.Fatalf("JOB-A profit with pool shares: %+v %v", profit, err)
	}
	if s := profit.PoolShares; s[0].PoolID != "P-1" || s[0].LineNo != 1 || s[0].Method != api.MethodChargeable || s[0].Stage != api.BasisActual ||
		s[0].Local != cny(432_000) || s[1].PoolID != "P-1" || s[1].LineNo != 2 || s[1].Method != api.MethodManual || s[1].Local != cny(10_000) ||
		s[2].PoolID != "P-2" || s[2].Stage != api.BasisPlanned || s[2].Local != cny(60_000) {
		t.Fatalf("JOB-A shares: %+v", s)
	}
	consol, err := f.module.GetConsolProfit(ctx, api.GetConsolProfit{TenantID: f.tenant, ConsolidationID: "CONSOL-1"})
	if err != nil || consol.PoolID != "P-1" || consol.Pooled != cny(770_000) || consol.Apportioned != cny(770_000) ||
		consol.Unapportioned != cny(0) || len(consol.Houses) != 2 {
		t.Fatalf("consol profit: %+v %v", consol, err)
	}
	houseA, houseB := consol.Houses[0], consol.Houses[1]
	if houseA.ShipmentID != "SH-A" || houseA.JobID != "JOB-A" || houseA.Revenue != cny(800_000) || houseA.PoolCost != cny(442_000) ||
		houseA.Profit != cny(358_000) || houseB.ShipmentID != "SH-B" || houseB.PoolCost != cny(328_000) || houseB.Profit != cny(-328_000) {
		t.Fatalf("houses: %+v / %+v", houseA, houseB)
	}
	if _, err = f.module.GetConsolProfit(ctx, api.GetConsolProfit{TenantID: f.tenant, ConsolidationID: "CONSOL-9"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a consolidation without a pool: %v", err)
	}

	// a job holding current pool shares cannot be cancelled; re-apportion without the house first
	if _, err = f.module.CancelFreightJob(ctx, api.CancelFreightJob{CommandID: "cancel-b", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-B",
		ExpectedRevision: 1, Reason: "x", OccurredAt: f.hours(11)}); err == nil {
		t.Fatal("JOB-B holds current shares of P-1 and P-2")
	} else if state, constraint := sqlState(err); state != "23514" || constraint != "pool_share_job_open" {
		t.Fatalf("refused for another reason: %v (state %s, constraint %s)", err, state, constraint)
	}
	if j, err := f.module.GetFreightJob(ctx, api.GetFreightJob{TenantID: f.tenant, JobID: "JOB-B"}); err != nil || j.State != api.JobOpen {
		t.Fatalf("JOB-B after the refused cancel: %+v %v", j, err)
	}

	want := []string{"FinancePolicyOpened", "ExchangeRateRecorded", "FreightJobOpened", "ConsolCostPoolOpened", "ConsolCostRecorded", "ConsolCostRevised",
		"ConsolCostRecorded", "FreightJobOpened", "ChargeLineRecorded", "ConsolCostsApportioned", "ConsolCostsApportioned", "ConsolCostPoolOpened",
		"ConsolCostRecorded", "ConsolCostsApportioned"}
	if got := f.outboxTypes(t); !slices.Equal(got, want) {
		t.Fatalf("outbox %v", got)
	}

	// past FIN on the application role
	for name, tc := range map[string]struct {
		sql, state, check string
	}{
		"a cost version rewritten": {`UPDATE cargo_fin.pool_cost_version SET amount_minor=1 WHERE tenant_id=$1`, "42501|23001", ""},
		"a share rewritten":        {`UPDATE cargo_fin.pool_apportionment_share SET amount_minor=amount_minor+1 WHERE tenant_id=$1`, "42501|23001", ""},
		"an apportionment deleted": {`DELETE FROM cargo_fin.pool_apportionment_share WHERE tenant_id=$1`, "42501|23001", ""},
		"a pool deleted":           {`DELETE FROM cargo_fin.consol_cost_pool WHERE tenant_id=$1 AND pool_id='P-2'`, "42501|23001", ""},
		"a version behind the pool": {`INSERT INTO cargo_fin.pool_cost_version (tenant_id,pool_id,line_no,version_no,creditor_revision,currency,
            amount_minor,local_minor,status,reason,recorded_by,recorded_at,entered_at) VALUES ($1,'P-1',1,3,0,'CNY',720000,720000,'ACCRUED','x','x',now(),now())`,
			"23514", "fin_records_with_parent"},
		"a local amount not its conversion": {`UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-1';
            INSERT INTO cargo_fin.pool_cost_version (tenant_id,pool_id,line_no,version_no,creditor_revision,currency,amount_minor,local_minor,status,reason,
            recorded_by,recorded_at,entered_at) VALUES ($1,'P-1',1,3,0,'CNY',720000,999999,'ACCRUED','x','x',now(),now())`,
			"23514", "consol_cost_pool_consistent"},
		"a share behind the pool": {`INSERT INTO cargo_fin.pool_apportionment_share (tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,
            quantity,unit,amount_minor,local_minor) VALUES ($1,'P-1',1,1,3,'SH-C','JOB-A',1,'GRM',0,0)`, "23514", "fin_records_with_parent"},
		"a conservation-breaking share": {`UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-1';
            INSERT INTO cargo_fin.pool_apportionment_share (tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,quantity,unit,amount_minor,
            local_minor) VALUES ($1,'P-1',1,1,3,'SH-C','JOB-A',1,'GRM',1,1)`, "23514", "pool_apportionment_conserves"},
		"a share whose job is another shipment's": {`UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-1';
            INSERT INTO cargo_fin.pool_apportionment_share (tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,quantity,unit,amount_minor,
            local_minor) VALUES ($1,'P-1',1,1,3,'SH-C','JOB-A',1,'GRM',0,0)`, "23514", "pool_share_job_open"},
		"a job with current shares cancelled past FIN": {`UPDATE cargo_fin.freight_job SET state='CANCELLED',cancel_reason='x',cancelled_by='x',
            cancelled_at=now(),cancel_entered_at=now(),revision=revision+1 WHERE tenant_id=$1 AND job_id='JOB-B'`, "23514", "pool_share_job_open"},
		"a second pool of the consolidation": {`INSERT INTO cargo_fin.consol_cost_pool (tenant_id,pool_id,consolidation_id,legal_entity_id,mode,
            functional_currency,book_code,state,opened_by,opened_at,entered_at) VALUES ($1,'P-9','CONSOL-1','LE','AIR','CNY','MAIN','OPEN','x',now(),now())`,
			"23505", "consol_cost_pool_one_per_consol"},
	} {
		statements := strings.Split(tc.sql, ";\n")
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range statements {
				if _, err := e.Exec(ctx, s, f.tenant); err != nil {
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

// TestApportionRacesJobCancel races an apportionment against the cancellation of a house's job, four times: exactly
// one commits. A cancellation first leaves the apportionment without the house's open job (FIN_POOL_JOB_MISSING); an
// apportionment first leaves the cancellation refused at commit (pool_share_job_open) since the job holds current
// shares.
func TestApportionRacesJobCancel(t *testing.T) {
	ctx := context.Background()
	for round := range 4 {
		f := newFixture(t)
		f.w.costBases["CONSOL-R"] = airConsol(f, "CONSOL-R")
		if _, err := f.module.OpenFinancePolicy(ctx, api.OpenFinancePolicy{CommandID: "policy", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
			FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: f.hours(0)}); err != nil {
			t.Fatal(err)
		}
		for _, j := range []struct{ job, shipment string }{{"JOB-A", "SH-A"}, {"JOB-B", "SH-B"}} {
			if _, err := f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-" + j.job, TenantID: f.tenant, ActorID: "fin-1", JobID: j.job,
				ShipmentID: j.shipment, OccurredAt: f.hours(1)}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.module.OpenConsolCostPool(ctx, api.OpenConsolCostPool{CommandID: "pool", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-R",
			ConsolidationID: "CONSOL-R", OccurredAt: f.hours(2)}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.module.RecordConsolCost(ctx, api.RecordConsolCost{CommandID: "cost", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-R",
			ExpectedRevision: 1, ChargeCode: "AIR_FREIGHT", Method: api.MethodChargeable, Cost: api.ChargeSide{Amount: cny(90_000)},
			Reason: "booked", OccurredAt: f.hours(3)}); err != nil {
			t.Fatal(err)
		}
		var apportionErr, cancelErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, apportionErr = f.module.ApportionConsolCosts(ctx, api.ApportionConsolCosts{CommandID: "apportion", TenantID: f.tenant, ActorID: "fin-1",
				PoolID: "P-R", ExpectedRevision: 2, OccurredAt: f.hours(4)})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, cancelErr = f.module.CancelFreightJob(ctx, api.CancelFreightJob{CommandID: "cancel", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-B",
				ExpectedRevision: 1, Reason: "shipment rolled", OccurredAt: f.hours(4)})
		}()
		close(start)
		wg.Wait()
		t.Logf("round %d: apportion %v / cancel %v", round, apportionErr, cancelErr)
		switch {
		case apportionErr == nil && cancelErr == nil:
			t.Fatalf("round %d: a cancelled job kept current pool shares", round)
		case apportionErr != nil && cancelErr != nil:
			t.Fatalf("round %d: both refused: %v / %v", round, apportionErr, cancelErr)
		case apportionErr != nil:
			if !errors.Is(apportionErr, api.ErrRejected) || !strings.Contains(apportionErr.Error(), "FIN_POOL_JOB_MISSING") {
				t.Fatalf("round %d: the apportionment refused for another reason: %v", round, apportionErr)
			}
		default:
			if state, constraint := sqlState(cancelErr); state != "23514" || constraint != "pool_share_job_open" {
				t.Fatalf("round %d: the cancellation refused for another reason: %v", round, cancelErr)
			}
		}
		// whoever lost, the pool keeps taking changes
		pool, err := f.module.GetConsolCostPool(ctx, api.GetConsolCostPool{TenantID: f.tenant, PoolID: "P-R"})
		if err != nil {
			t.Fatal(err)
		}
		if cancelErr == nil {
			if len(pool.Apportionments) != 0 {
				t.Fatalf("round %d: shares despite the cancellation: %+v", round, pool.Apportionments)
			}
			continue
		}
		if len(pool.Apportionments) != 1 || fmt.Sprintf("%s/%s", pool.Apportionments[0].Lines[0].Shares[0].ShipmentID,
			pool.Apportionments[0].Lines[0].Shares[1].ShipmentID) != "SH-A/SH-B" {
			t.Fatalf("round %d: apportionment: %+v", round, pool.Apportionments)
		}
	}
}
