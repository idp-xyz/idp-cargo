//go:build integration

package bootstrap

import (
	"strings"
	"testing"
	"time"

	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.103, stage 2c: the sea line from the order to the profit with both boxes of the owner's scheme of 2026-11-18, with
// the real ORG, RATE, COM, FUL, PHY, BOOK, SEA, DOC and FIN on PostgreSQL and every setting at its default. All amounts
// are CNY.
//
// Box A (container 1, PHY's stuffing X-1, SEA's visit CV-1) holds H01, H02 and H03 of 10, 15 and 5 W/M, ocean freight
// 3,000.00. Box B (container 2, X-2, CV-2) holds H04 and H05 of 20 and 10 W/M, ocean freight 2,400.00. Both are
// lines of C-1's pool P-1, each apportioned by CHARGEABLE with the houses of the other box named out. The document fee
// of 300.00 is a line of P-1 by SHIPMENT: 60.00 for each of the five houses.
//
// H03 leaves box A after it was sealed and before departure (a rework asked by ops-1, the container sealed again, H03
// transferred to C-2 by ops-9, where it sails alone in container 3). Finance attributes H03's former shares: all of
// box A's freight goes to the cargo that remains (1,200.00 and 1,800.00), and H03's 60.00 of the document fee is its
// own loss, so the others keep 60.00 each. H03 bears C-2's own freight of 850.00 and a rework handling charge of 180.00,
// both lines of C-2's pool P-2 apportioned to H03 alone. Box B's storage fee of 300.00 arrives later, a line of P-1 by
// CHARGEABLE with box A's houses named out: 200.00 and 100.00 by 20:10.
//
// NOT IN THIS STEP: the owner's scheme rolls box B, as a whole container, to a later voyage; that is V2.105's
// container_roll_integration_test.go. Here box B sails on the same voyage as box A, in the same movement, and its
// storage fee is a cost line of P-1 on box B's houses.
//
// The revenue (chosen here, 9,200.00 in all, and the same before and after H03 leaves): each house's OCEAN_FREIGHT sell
// and DOC_FEE sell of 100.00:
//
//	H01 1,700.00 + 100.00 = 1,800.00     H02 2,400.00 + 100.00 = 2,500.00     H03 800.00 + 100.00 = 900.00
//	H04 2,500.00 + 100.00 = 2,600.00     H05 1,300.00 + 100.00 = 1,400.00
//
// FIN has no query for the profit of a whole consolidation that covers a house in another consolidation, so the
// consolidation's profit is the sum of the five houses' job profits, and FIN's economics of C-1 (the pool) is asserted
// for what it does answer: the pool's cost, what is apportioned, the losses and the pending shares, and the profit of
// the houses it lists.

var seaLineHouses = []string{"H01", "H02", "H03", "H04", "H05"}

// seaLineSell is each house's revenue lines in minor units: the freight and the document fee.
var seaLineSell = map[string][2]int64{"H01": {170_000, 10_000}, "H02": {240_000, 10_000}, "H03": {80_000, 10_000}, "H04": {250_000, 10_000}, "H05": {130_000, 10_000}}

// newSeaLineDay builds the day to the sealed boxes: C-1 of two 22G1 containers, H01, H02 and H03 stuffed into the first
// and H04 and H05 into the second, both sealed; SEA's movement OM-1 with the five houses associated and CV-1 and CV-2
// assigned; FIN's pool P-1 with the three cost lines of the scheme; the five houses' revenue lines; and DOC over the same
// database with FIN bound to it, so that FIN can ask DOC for the house bill a debit note needs.
func newSeaLineDay(t *testing.T, prefix string) *loadingDay {
	t.Helper()
	weights := map[string]int64{"H01": 312, "H02": 300, "H03": 288, "H04": 400, "H05": 350}
	volumes := map[string]int64{"H01": 10_000_000, "H02": 15_000_000, "H03": 5_000_000, "H04": 20_000_000, "H05": 10_000_000}
	o := newOceanDay(t, prefix, weights, volumes, []string{"22G1", "22G1"}, nil)
	o.stuff("X-1", 1)
	for _, h := range []string{"H01", "H02", "H03"} {
		o.load("X-1", h, 6, 0)
	}
	o.stuff("X-2", 2)
	for _, h := range []string{"H04", "H05"} {
		o.load("X-2", h, 6, 0)
	}
	o.seal("X-1")
	o.seal("X-2")
	d := &loadingDay{reworkDay: wrapReworkDay(t, o), visit: map[string]int64{}}
	for _, s := range []string{"SH-H04", "SH-H05"} {
		_, err := d.sea.AssociateHouseBill(d.ctx, seaapi.AssociateHouseBill{CommandID: "hbl-" + s, TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1",
			ExpectedRevision: d.movement().Revision, ShipmentID: s, OccurredAt: d.hours(11)})
		d.must("associate "+s, err)
	}
	for i, box := range []struct{ visit, number string }{{"CV-1", "csqu3054383"}, {"CV-2", "temu1234565"}} {
		r, err := d.sea.AssignContainer(d.ctx, seaapi.AssignContainer{CommandID: "assign-" + box.visit, TenantID: d.tenant, ActorID: "ops-1", VisitID: box.visit, MovementID: "OM-1",
			LoadUnitNo: i + 1, ContainerNumber: box.number, SizeType: "22G1", TareKg: 2180, MaxGrossKg: 30480, PlateSource: seaapi.PlateCSC, OccurredAt: d.hours(12)})
		d.must("assign "+box.visit, err)
		d.visit[box.visit] = r.NewRevision
	}

	// FIN with DOC's snapshots bound, over the same database, in place of the ocean day's (which sees no documents)
	snapshots := &DocumentSnapshots{}
	fin, err := d.platform.FIN(d.org, d.rate, d.ful, snapshots, d.phy, allowAll{})
	d.must("FIN", err)
	d.fin = fin
	d.docs = d.withDocuments()
	snap, ok := d.docs.doc.(docapi.TransportDocumentSnapshotQueries)
	if !ok {
		t.Fatal("DOC does not answer FIN's snapshot query")
	}
	snapshots.Bind(snap)
	// the shippers of the two houses withDocuments does not know
	d.seedParties("shipper-h04", "shipper-h05")
	for _, party := range []string{"shipper-h04", "shipper-h05"} {
		_, err = d.org.ApproveRole(d.ctx, orgapi.ApproveRole{CommandID: "role-" + party, TenantID: d.tenant, ActorID: "org-officer", LegalEntityID: "LE", PartyID: party,
			ExpectedRevision: 1, RoleCode: "SHIPPER", ValidFrom: d.at, EvidenceRef: "SHIPPER-agreement", OccurredAt: d.at})
		d.must("role "+party, err)
		_, err = d.org.AddPartyAddress(d.ctx, orgapi.AddPartyAddress{CommandID: "office-" + party, TenantID: d.tenant, ActorID: "org-officer", PartyID: party, AddressID: "HQ-" + party,
			Address:    orgapi.Address{ShortCode: "HQ", Purposes: []string{orgapi.AddressOffice}, Name: "Office of " + party, Lines: []string{"1 Harbour Road"}, City: "Hamburg", CountryCode: "DE"},
			DefaultFor: []string{orgapi.AddressOffice}})
		d.must("office "+party, err)
	}

	// the pool's three lines and the houses' revenue lines
	cost := func(id, code, desc, method string, minor int64) {
		_, err := d.fin.RecordConsolCost(d.ctx, finapi.RecordConsolCost{CommandID: id, TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision,
			ChargeCode: code, Description: desc, Method: method, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: minor}},
			Reason: desc, OccurredAt: d.hours(16)})
		d.must(desc, err)
	}
	cost("fin-cost-a", "OCEAN_FREIGHT", "ocean freight of box A", finapi.MethodChargeable, 300_000)
	cost("fin-cost-b", "OCEAN_FREIGHT", "ocean freight of box B", finapi.MethodChargeable, 240_000)
	cost("fin-cost-doc", "DOC_FEE", "document fee", finapi.MethodShipment, 30_000)
	for _, h := range seaLineHouses {
		for i, code := range []string{"OCEAN_FREIGHT", "DOC_FEE"} {
			_, err := d.fin.RecordChargeLine(d.ctx, finapi.RecordChargeLine{CommandID: "fin-sell-" + h + "-" + code, TenantID: d.tenant, ActorID: "fin-1", JobID: "JOB-" + h,
				ExpectedRevision: d.jobRevision(h), ChargeCode: code, Sell: &finapi.ChargeSide{PartyID: "party-" + strings.ToLower(h), Amount: kernel.Money{Currency: "CNY", Minor: seaLineSell[h][i]}},
				Reason: "the customer's price", OccurredAt: d.hours(12)})
			d.must("sell "+code+" of "+h, err)
		}
	}
	return d
}

func (d *loadingDay) jobRevision(house string) int64 {
	d.t.Helper()
	j, err := d.fin.GetFreightJob(d.ctx, finapi.GetFreightJob{TenantID: d.tenant, JobID: "JOB-" + house})
	d.must("JOB-"+house, err)
	return j.Revision
}

// apportion previews the lines of P-1 as fin-1 and submits the preview, with the houses named out and the
// attributions stated.
func (d *loadingDay) apportionLines(id string, lines []int, notUsed []string, attributions ...finapi.AttributionStatement) error {
	d.t.Helper()
	_, err := previewAndSubmit(d.ctx, d.fin, finapi.PreviewConsolCostApportionment{CommandID: id, TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision,
		LineNos: lines, NotUsed: notUsed, Attributions: attributions, OccurredAt: d.later()})
	return err
}

// wantJobs asserts each house's revenue, cost and profit as FIN's job profit tells them (CNY minor units): the cost is
// all pool cost, the jobs having no cost line of their own; and the totals of the five houses.
func (d *loadingDay) wantJobs(stage string, bears map[string]int64, wantCost, wantProfit int64) {
	d.t.Helper()
	var cost, profit, revenue int64
	for _, h := range seaLineHouses {
		p := d.jobProfit(h)
		rev := seaLineSell[h][0] + seaLineSell[h][1]
		if p.Revenue.Minor != rev || p.Revenue.Currency != "CNY" || p.Cost.Minor != 0 || p.PoolCost.Minor != bears[h] || p.Profit.Minor != rev-bears[h] || p.FormerSharesPending.Minor != 0 {
			d.t.Fatalf("%s, JOB-%s: want revenue %d, pool cost %d, profit %d, got %+v", stage, h, rev, bears[h], rev-bears[h], p)
		}
		revenue += p.Revenue.Minor
		cost += p.PoolCost.Minor + p.Cost.Minor
		profit += p.Profit.Minor
	}
	if revenue != 920_000 || cost != wantCost || profit != wantProfit || revenue-cost != profit {
		d.t.Fatalf("%s, the five houses: revenue %d, cost %d, profit %d, want 920000, %d, %d", stage, revenue, cost, profit, wantCost, wantProfit)
	}
}

// shareOf is the amount of the pool's line the house's job bears (not pending), 0 when it has no share.
func (d *loadingDay) shareOf(house, pool string, line int) int64 {
	d.t.Helper()
	for _, s := range d.jobProfit(house).PoolShares {
		if s.PoolID == pool && s.LineNo == line && !s.Pending {
			return s.Amount.Minor
		}
	}
	return 0
}

func (d *loadingDay) wantShares(stage string, line int, want map[string]int64) {
	d.t.Helper()
	for _, h := range seaLineHouses {
		if got := d.shareOf(h, "P-1", line); got != want[h] {
			d.t.Fatalf("%s, line %d: %s bears %d, want %d", stage, line, h, got, want[h])
		}
	}
}

func TestTheSeaLineRunsFromTheOrderToTheProfitWithBothBoxesOfTheOwnersScheme(t *testing.T) {
	d := newSeaLineDay(t, "cargo-sea-line-")

	// 1. The orders, the houses and the consolidation are built (every house has its order, its shipment, its place in
	// C-1 and its job). Both boxes are sealed in PHY. Finance apportions P-1's three lines, each its own command: box A's
	// freight over the houses of box A, box B's over the houses of box B, the document fee equally over the five.
	c1, err := d.phy.GetConsolidation(d.ctx, phyapi.GetConsolidation{TenantID: d.tenant, ConsolidationID: "C-1"})
	d.must("C-1", err)
	if c1.State != phyapi.StateClosed || len(c1.Members) != 5 {
		t.Fatalf("step 1, C-1: %+v", c1)
	}
	if b := d.basis(); len(b.Houses) != 5 || len(b.PackingLists) != 2 || b.LoadUnits[0].Status != phyapi.UnitStatusClosed || b.LoadUnits[1].Status != phyapi.UnitStatusClosed ||
		len(b.PackingLists[0].Lines) != 3 || len(b.PackingLists[1].Lines) != 2 {
		t.Fatalf("step 1, PHY's basis: %+v", b)
	}
	d.must("box A's freight", d.apportionLines("fin-app-a", []int{1}, []string{"SH-H04", "SH-H05"}))
	d.must("box B's freight", d.apportionLines("fin-app-b", []int{2}, []string{"SH-H01", "SH-H02", "SH-H03"}))
	d.must("the document fee", d.apportionLines("fin-app-doc", []int{3}, nil))
	d.wantShares("step 1", 1, map[string]int64{"H01": 100_000, "H02": 150_000, "H03": 50_000})
	d.wantShares("step 1", 2, map[string]int64{"H04": 160_000, "H05": 80_000})
	d.wantShares("step 1", 3, map[string]int64{"H01": 6_000, "H02": 6_000, "H03": 6_000, "H04": 6_000, "H05": 6_000})
	// before H03 leaves: cost 5,700.00 and gross profit 3,500.00
	d.wantJobs("step 1", map[string]int64{"H01": 106_000, "H02": 156_000, "H03": 56_000, "H04": 166_000, "H05": 86_000}, 570_000, 350_000)
	before := d.consolProfit()
	if before.Pooled.Minor != 570_000 || before.Apportioned.Minor != 570_000 || before.Unapportioned.Minor != 0 || before.Losses.Minor != 0 || before.FormerSharesPending.Minor != 0 ||
		len(before.Houses) != 5 {
		t.Fatalf("step 1, FIN's economics of C-1: %+v", before)
	}
	var consolProfit int64
	for _, h := range before.Houses {
		consolProfit += h.Profit.Minor
	}
	if consolProfit != 350_000 {
		t.Fatalf("step 1, the profit of the houses FIN lists in C-1: %d, want 350000: %+v", consolProfit, before.Houses)
	}

	// 2. SEA: both containers stuffed and sealed, their VGMs, the master bill; the movement is finalized with the five
	// houses and both containers. DOC: drafts of the five house bills, approved.
	d.must("FUL takes the evidence", d.fulTakes(16, "SH-H01", "SH-H02", "SH-H03", "SH-H04", "SH-H05"))
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03", "SH-H04", "SH-H05"} {
		d.must("refresh "+s, d.refresh(s, 17))
	}
	d.visit["CV-1"], d.visit["CV-2"] = d.cvOf("CV-1").Revision, d.cvOf("CV-2").Revision
	d.must("SEA's seal of box A", d.boxSeal("CV-1", "seal-x-1", 17))
	d.must("SEA's seal of box B", d.boxSeal("CV-2", "seal-x-2", 17))
	for _, v := range []string{"CV-1", "CV-2"} {
		d.must("stuffing done "+v, d.boxStuffingDone(v, 18))
	}
	d.must("VGM of box A", d.boxVGM("CV-1", 2900, 19))
	d.must("VGM of box B", d.boxVGM("CV-2", 3000, 19))
	d.masterBill(18)
	d.finalizeOf("OM-1", "fin-1", 20)
	if f := d.facts("OM-1"); f.Version != 1 || f.Status != seaapi.DocFactsFinal || len(f.Houses) != 5 || len(f.Containers) != 2 || f.Containers[0].SealNumber != "SEAL-X-1" ||
		f.Containers[1].SealNumber != "SEAL-X-2" || f.Containers[0].Packs != 18 || f.Containers[1].Packs != 12 {
		t.Fatalf("step 2, SEA's facts: %+v", shown(f))
	}
	bills := map[string]string{}
	for _, h := range seaLineHouses {
		bills[h] = d.prepare("SH-"+h, "OM-1", 21)
		if doc, v := d.bill(bills[h]); doc.State != docapi.StateApproved || doc.MovementID != "OM-1" || v.Doc.Source.Version != 1 || len(v.Doc.Containers) != 1 {
			t.Fatalf("step 2, the draft of %s: %+v %+v", h, doc, v)
		}
	}

	// 3. ops-1 asks for a rework of box A: H03 is unloaded whole, the container is sealed again and a new VGM is asserted.
	// FIN's pool is in work meanwhile (a preview is refused) and box B is untouched.
	d.reworkBy("rework-1", "ops-1", 17)
	_, err = d.previewBy("fin-1", "fin-pre-open")
	said(t, "step 3, a preview during the rework", err, "FIN_POOL_IN_WORK", "load unit 1 (stuffing X-1)")
	d.unloadForRework("H03", 18)
	d.resealAt("reseal-1", 19)
	d.must("VGM v2 of box A", d.vgmAgain("CV-1", "vgm-2-CV-1", 2_800, 20))
	if b := d.basis(); b.Houses[2].ShipmentID != "SH-H03" || b.Houses[2].LoadedPacks != 0 || b.Houses[0].LoadedPacks != 6 || b.Houses[3].LoadedPacks != 6 || b.Houses[4].LoadedPacks != 6 ||
		len(b.PackingLists[0].Reworks) != 1 || len(b.PackingLists[1].Reworks) != 0 || len(b.PackingLists[0].Lines) != 2 || b.PackingLists[1].ExecutionState != phyapi.ExecutionClosed {
		t.Fatalf("step 3, PHY's basis: %+v", b)
	}
	if p := d.pool(); p.InWork {
		t.Fatalf("step 3, the pool is still in work: %+v", p)
	}

	// 4. ops-9 transfers H03 to C-2 (a container of its own, a pool of its own): C-2's freight of 850.00 and the rework
	// handling charge of 180.00 are both lines of P-2, apportioned to H03, which is the only house there. FUL records
	// the attachment.
	d.newConsolidation2()
	_, err = d.phy.TransferHouse(d.ctx, phyapi.TransferHouse{CommandID: "transfer-h03", TenantID: d.tenant, ActorID: "ops-9", ConsolidationID: "C-1", ToConsolidationID: "C-2",
		ShipmentID: "SH-H03", ExpectedMembershipRevision: 1, Reason: phyapi.ReasonWrongConsol, OccurredAt: d.hours(21)})
	d.must("step 4, transfer H03", err)
	plan, err := d.ful.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: "SH-H03"})
	d.must("FUL's plan of H03", err)
	_, err = d.ful.RecordConsolAttachment(d.ctx, fulapi.RecordConsolAttachment{CommandID: "ful-attach-c2", TenantID: d.tenant, ActorID: "ops-1", ShipmentID: "SH-H03",
		ExpectedRevision: plan.Revision, ConsolidationID: "C-2", OccurredAt: d.hours(22)})
	d.must("step 4, FUL records the attachment", err)
	_, err = d.fin.OpenConsolCostPool(d.ctx, finapi.OpenConsolCostPool{CommandID: "fin-pool2", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-2", ConsolidationID: "C-2", OccurredAt: d.hours(33)})
	d.must("open P-2", err)
	for i, line := range []struct {
		code, desc string
		minor      int64
	}{{"OCEAN_FREIGHT", "H03's own freight", 85_000}, {"REWORK_HANDLING", "rework handling charge", 18_000}} {
		_, err = d.fin.RecordConsolCost(d.ctx, finapi.RecordConsolCost{CommandID: "fin-cost2-" + line.code, TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-2", ExpectedRevision: int64(1 + i),
			ChargeCode: line.code, Description: line.desc, Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: line.minor}},
			Reason: line.desc, OccurredAt: d.hours(33)})
		d.must(line.desc, err)
	}
	p2, err := d.fin.GetConsolCostPool(d.ctx, finapi.GetConsolCostPool{TenantID: d.tenant, PoolID: "P-2"})
	d.must("P-2", err)
	_, err = previewAndSubmit(d.ctx, d.fin, finapi.PreviewConsolCostApportionment{CommandID: "fin-apportion-p2", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-2",
		ExpectedRevision: p2.Revision, LineNos: []int{1, 2}, OccurredAt: d.hours(34)})
	d.must("apportion P-2", err)
	if p2, err = d.fin.GetConsolCostPool(d.ctx, finapi.GetConsolCostPool{TenantID: d.tenant, PoolID: "P-2"}); err != nil || len(p2.Apportionments) != 1 || len(p2.Apportionments[0].Lines) != 2 ||
		p2.Apportionments[0].Lines[0].Shares[0].Amount.Minor != 85_000 || p2.Apportionments[0].Lines[1].Shares[0].Amount.Minor != 18_000 {
		t.Fatalf("step 4, P-2's apportionment: %+v %v", p2.Apportionments, err)
	}
	// H03's former shares of P-1 (500.00 of box A's freight and 60.00 of the document fee) are pending and nobody's
	// cost: C-1's pool lists them, the job lists them apart.
	if got := d.oceanDay.pending(); !strings.Contains(strings.Join(pendingKinds(got), ","), finapi.PendingFormerSharePending) {
		t.Fatalf("step 4, FIN's pending items: %+v", got)
	}
	if h03 := d.jobProfit("H03"); h03.FormerSharesPending.Minor != 56_000 || len(h03.FormerShares) != 2 || h03.PoolCost.Minor != 103_000 {
		t.Fatalf("step 4, H03 before the attribution: %+v", h03)
	}
	if c := d.consolProfit(); c.FormerSharesPending.Minor != 56_000 || c.Pooled.Minor != 570_000 {
		t.Fatalf("step 4, C-1's economics before the attribution: %+v", c)
	}

	// 5. SEA: C-1 is refreshed and finalized again with S2, four houses and both containers, which are loaded (gate-in
	// and loading on board); C-2 is finalized with H03 in container 3. DOC: the four bills of C-1 are refreshed on the
	// new facts, approved and issued; H03's draft moves to C-2 and is issued there.
	d.endH03(20)
	refusedWith(t, "step 5, SEA's finalization while FUL is behind PHY", d.finalize("fin-f0-open", 20), "FUL has not recorded PHY's latest facts of")
	d.must("FUL takes the resealed evidence", d.fulTakes(20, "SH-H01", "SH-H02"))
	d.must("refresh H01", d.refresh("SH-H01", 21))
	d.visit["CV-1"] = d.cvOf("CV-1").Revision
	d.must("refresh H02", d.refresh("SH-H02", 21))
	d.load("CV-1", 22)
	d.load("CV-2", 22)
	d.finalize("fin-2", 22)
	f1 := d.facts("OM-1")
	if c := f1.Containers[0]; f1.Version != 2 || f1.Status != seaapi.DocFactsFinal || len(f1.Houses) != 4 || len(f1.Containers) != 2 || c.SealNumber != "SEAL-RESEAL-1" || c.Packs != 12 ||
		!c.OnBoardAt.Equal(d.hours(22)) || f1.Containers[1].SealNumber != "SEAL-X-2" || f1.Containers[1].Packs != 12 || !f1.Containers[1].OnBoardAt.Equal(d.hours(22)) {
		t.Fatalf("step 5, OM-1's version 2: %+v", shown(f1))
	}
	d.secondMovementAt(44)
	if f2 := d.facts("OM-2"); f2.Version != 1 || f2.Status != seaapi.DocFactsFinal || len(f2.Houses) != 1 || f2.Houses[0].ShipmentID != "SH-H03" || !f2.Containers[0].OnBoardAt.Equal(d.hours(44)) {
		t.Fatalf("step 5, OM-2's version 1: %+v", shown(f2))
	}
	for _, h := range []string{"H01", "H02", "H04", "H05"} {
		id := bills[h]
		d.refreshBill(id, 2, "", 27)
		d.approve(id, 3, 28)
		d.must("issue "+id, d.issue(id, 4, 29))
		if doc, v := d.bill(id); doc.State != docapi.StateIssued || doc.MovementID != "OM-1" || v.Doc.Source.Version != 2 || !v.Doc.OnBoardAt.Equal(d.hours(22)) {
			t.Fatalf("step 5, the bill %s: %+v %+v", id, doc, v.Doc)
		}
	}
	d.refreshBill(bills["H03"], 2, "OM-2", 44)
	d.approve(bills["H03"], 3, 44)
	d.must("issue H03's bill", d.issue(bills["H03"], 4, 45))
	if doc, v := d.bill(bills["H03"]); doc.State != docapi.StateIssued || doc.MovementID != "OM-2" || doc.ConsolidationID != "C-2" || !v.Doc.OnBoardAt.Equal(d.hours(44)) {
		t.Fatalf("step 5, H03's bill: %+v %+v", doc, v.Doc)
	}
	d.wantPending("OM-1 after the issue", "OM-1", "")
	d.wantPending("OM-2 after the issue", "OM-2", "")

	// 6. Box B's storage fee of 300.00 is a cost line of P-1, by CHARGEABLE with box A's houses named out: H04 200.00 and
	// H05 100.00 by 20:10. Finance then attributes H03's former shares in one preview: box A's freight all to the cargo
	// that remains (H03 no loss, no exception), the document fee's 60.00 as H03's own loss. The first preview's checks
	// are made on what it would change.
	_, err = d.fin.RecordConsolCost(d.ctx, finapi.RecordConsolCost{CommandID: "fin-cost-storage", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision,
		ChargeCode: "STORAGE", Description: "storage of box B", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: 30_000}},
		Reason: "storage of box B at the terminal", OccurredAt: d.later()})
	d.must("the storage fee", err)
	d.must("the storage fee is apportioned", d.apportionLines("fin-app-storage", []int{4}, []string{"SH-H01", "SH-H02"}))
	d.wantShares("step 6", 4, map[string]int64{"H04": 20_000, "H05": 10_000})
	attrA := statement("H03", 0, 0, "")
	attrDoc := statement("H03", 6_000, 0, "")
	attrDoc.LineNo = 3
	attrDoc.Voucher = "REWORK-1"
	d.must("finance attributes H03's former shares", d.apportionLines("fin-attr", []int{1, 2, 3}, nil, attrA, attrDoc))
	// what is left is the creditor's invoices, not matched yet (the pool is not closed in this scenario)
	for _, it := range d.oceanDay.pending() {
		if it.Kind != finapi.PendingCostNotFinal {
			t.Fatalf("step 6, FIN's pending items after the attribution: %+v", it)
		}
	}
	d.wantShares("step 6", 1, map[string]int64{"H01": 120_000, "H02": 180_000})
	d.wantShares("step 6", 2, map[string]int64{"H04": 160_000, "H05": 80_000})
	d.wantShares("step 6", 3, map[string]int64{"H01": 6_000, "H02": 6_000, "H04": 6_000, "H05": 6_000})
	d.wantShares("step 6", 4, map[string]int64{"H04": 20_000, "H05": 10_000})
	if got := d.shareOf("H03", "P-2", 1) + d.shareOf("H03", "P-2", 2); got != 103_000 {
		t.Fatalf("step 6, H03 bears of P-2: %d", got)
	}
	// after H03 left: cost 7,030.00 and gross profit 2,170.00 (the revenue is the same 9,200.00)
	after := map[string]int64{"H01": 126_000, "H02": 186_000, "H03": 109_000, "H04": 186_000, "H05": 96_000}
	d.wantJobs("step 6", after, 703_000, 217_000)
	if h03 := d.jobProfit("H03"); h03.ReroutingLoss.Minor != 6_000 || h03.FormerSharesPending.Minor != 0 || h03.Profit.Minor != -19_000 {
		t.Fatalf("step 6, H03 after the attribution: %+v", h03)
	}
	c1p := d.consolProfit()
	if c1p.Pooled.Minor != 600_000 || c1p.Apportioned.Minor != 594_000 || c1p.Losses.Minor != 6_000 || c1p.Exceptions.Minor != 0 || c1p.FormerSharesPending.Minor != 0 ||
		c1p.Unapportioned.Minor != 0 || c1p.ToAllocated.Minor != 0 {
		t.Fatalf("step 6, FIN's economics of C-1: %+v", c1p)
	}
	// C-1's economics list the five houses of its apportionments with their job profits, but H03's is read through P-1
	// alone: its 60.00 loss, not the 1,030.00 of P-2 (and 840.00 of profit it therefore shows is not H03's). So FIN
	// does not answer the consolidation's profit after H03 left, and it is the sum of the five jobs asserted above.
	if len(c1p.Houses) != 5 || c1p.Houses[2].ShipmentID != "SH-H03" || c1p.Houses[2].PoolCost.Minor != 6_000 || c1p.Houses[0].Profit.Minor != 54_000 || c1p.Houses[1].Profit.Minor != 64_000 ||
		c1p.Houses[3].Profit.Minor != 74_000 || c1p.Houses[4].Profit.Minor != 44_000 {
		t.Fatalf("step 6, the houses FIN lists in C-1: %+v", c1p.Houses)
	}

	// 7. FIN bills the five houses on their issued house bills (debit notes of CNY, the charge lines of each job):
	// each note is on its own house's bill, and the profits stay what they were.
	d.billTheHouses(bills)
	d.wantJobs("step 7", after, 703_000, 217_000)
	for _, h := range seaLineHouses {
		if p := d.jobProfit(h); p.Invoiced.Minor != seaLineSell[h][0]+seaLineSell[h][1] {
			t.Fatalf("step 7, JOB-%s invoiced %d: %+v", h, p.Invoiced.Minor, p)
		}
	}
}

// billTheHouses sets what a debit note needs besides a document (the tax treatments, the series, the billing profiles
// and the credit terms) and issues one debit note on each job's two sell lines, on the house's own bill.
func (d *loadingDay) billTheHouses(bills map[string]string) {
	d.t.Helper()
	rev := int64(2)
	step := func(what string, r finapi.Receipt, err error) {
		d.t.Helper()
		d.must(what, err)
		rev = r.NewRevision
	}
	taxFrom := d.at.AddDate(0, 0, -1).Format(time.DateOnly)
	for _, code := range []string{"OCEAN_FREIGHT", "DOC_FEE"} {
		r, err := d.fin.SetChargeTaxTreatment(d.ctx, finapi.SetChargeTaxTreatment{CommandID: "fin-tax-" + code, TenantID: d.tenant, ActorID: "tax-1", LegalEntityID: "LE", ExpectedRevision: rev,
			ChargeCode: code, Treatment: finapi.TaxExemptIntlFreight, EffectiveFrom: taxFrom, OccurredAt: d.hours(30)})
		step("tax "+code, r, err)
	}
	r, err := d.fin.RegisterInvoiceSeries(d.ctx, finapi.RegisterInvoiceSeries{CommandID: "fin-series-dn", TenantID: d.tenant, ActorID: "acct-1", LegalEntityID: "LE", ExpectedRevision: rev,
		Kind: finapi.KindDebitNote, Template: "SEADN{seq:6}", FirstNumber: 1, OccurredAt: d.hours(30)})
	step("series", r, err)
	for _, h := range seaLineHouses {
		party := "party-" + strings.ToLower(h)
		_, err := d.fin.SetBillingProfile(d.ctx, finapi.SetBillingProfile{CommandID: "fin-profile-" + h, TenantID: d.tenant, ActorID: "bill-1", DebtorPartyID: party,
			BuyerKind: finapi.BuyerForeign, EvidenceRef: "account opening form AO-2026-" + h, OccurredAt: d.hours(30)})
		d.must("billing profile of "+party, err)
		_, err = d.fin.SetCreditTerms(d.ctx, finapi.SetCreditTerms{CommandID: "fin-terms-" + h, TenantID: d.tenant, ActorID: "credit-1", LegalEntityID: "LE", DebtorPartyID: party,
			Limit: kernel.Money{Currency: "CNY", Minor: 100_000_000}, PaymentDays: 30, PaymentBasis: finapi.TermsFromMonthEnd, OccurredAt: d.hours(30)})
		d.must("credit terms of "+party, err)
	}
	for i, h := range seaLineHouses {
		hour := 46
		if h != "H03" {
			hour = 31
		}
		id := "DN-" + h
		receipt, err := d.fin.IssueDebitNote(d.ctx, finapi.IssueDebitNote{CommandID: "fin-" + id, TenantID: d.tenant, ActorID: "bill-1", JobID: "JOB-" + h, ExpectedRevision: d.jobRevision(h),
			InvoiceID: id, DebtorPartyID: "party-" + strings.ToLower(h), LineNos: []int{1, 2}, Currency: "CNY", InvoiceDate: d.hours(hour).Format(time.DateOnly), OccurredAt: d.hours(hour)})
		d.must("debit note "+id, err)
		note, err := d.fin.GetDebtorInvoice(d.ctx, finapi.GetDebtorInvoice{TenantID: d.tenant, InvoiceID: id})
		bill, _ := d.bill(bills[h])
		if err != nil || receipt.SubjectID != "JOB-"+h || note.Kind != finapi.KindDebitNote || note.ShipmentID != "SH-"+h || note.Total.Minor != seaLineSell[h][0]+seaLineSell[h][1] ||
			note.Total.Currency != "CNY" || note.Document.DocumentID != bills[h] || note.Document.Number != bill.Number || len(note.Lines) != 2 {
			d.t.Fatalf("the debit note %d of %s: %+v %v", i+1, h, note, err)
		}
	}
}
