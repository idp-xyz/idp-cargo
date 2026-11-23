//go:build integration

package bootstrap

import (
	"slices"
	"strings"
	"testing"
	"time"

	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.95: the owner's box A from start to finish with the real ORG, RATE, COM, FUL, PHY, BOOK, SEA, DOC and FIN, and the
// one query that answers what became of the reworked container. C-1 is an own-container sea LCL consolidation with
// container 1 (PHY's stuffing X-1, SEA's visit CV-1) holding H01, H02 and H03 of 10, 15 and 5 W/M; freight CNY
// 3,000.00. Every step is a command of a context. The hours are the facts' business times and the order of the commands
// is the day's.

// newBoxADay is box A with SEA's movement OM-1 of C-1 opened, CV-1 assigned to container 1 and DOC over the same
// database: PHY's container sealed with SEAL-X-1 and FIN's pool P-1 with the freight line, nothing of SEA or DOC read
// yet.
func newBoxADay(t *testing.T, prefix string) *loadingDay {
	t.Helper()
	d := &loadingDay{reworkDay: wrapReworkDay(t, boxA(t, prefix)), visit: map[string]int64{}}
	r, err := d.sea.AssignContainer(d.ctx, seaapi.AssignContainer{CommandID: "assign-CV-1", TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-1", MovementID: "OM-1", LoadUnitNo: 1,
		ContainerNumber: "csqu3054383", SizeType: "22G1", TareKg: 2180, MaxGrossKg: 30480, PlateSource: seaapi.PlateCSC, OccurredAt: d.hours(12)})
	d.must("assign CV-1", err)
	d.visit["CV-1"] = r.NewRevision
	d.docs = d.withDocuments()
	return d
}

// vgmAgain asserts a VGM of the visit under its own command, determined and submitted at the hour.
func (d *loadingDay) vgmAgain(visit, command string, kilos int64, hour int) error {
	d.t.Helper()
	r, err := d.sea.AssertVgm(d.ctx, seaapi.AssertVgm{CommandID: command, TenantID: d.tenant, ActorID: "ops-1", VisitID: visit, ExpectedRevision: d.visit[visit],
		Mass: kernel.Quantity{Unit: "KGM", Amount: kilos}, Method: seaapi.VgmMethod2, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1, DeterminedAt: d.hours(hour),
		Signatory: "Ada", OccurredAt: d.hours(hour)})
	if err == nil {
		d.visit[visit] = r.NewRevision
	}
	return err
}

// shown is SEA's facts without the published bytes, to print.
func shown(f seaapi.OceanDocFacts) seaapi.OceanDocFacts {
	f.Content = nil
	return f
}

// reportOf reads the report of container 1 of C-1 from the day's real contexts.
func (d *loadingDay) reportOf() ReworkedContainerReport {
	d.t.Helper()
	r, err := NewReworkedContainerReader(d.phy, d.sea, d.dm(), d.fin).ReworkedContainerReport(d.ctx, ReworkedContainerQuery{TenantID: d.tenant, ConsolidationID: "C-1", LoadUnitNo: 1})
	d.must("the report of container 1", err)
	return r
}

// TestBoxAIsRunFromStartToFinishWithEveryContextAndTheReportAnswers is the owner's acceptance (step 7 of "a house
// leaves a sealed container"): eight steps, each with the figures and the states it leaves.
func TestBoxAIsRunFromStartToFinishWithEveryContextAndTheReportAnswers(t *testing.T) {
	d := newBoxADay(t, "cargo-box-a-acceptance-")

	// 1. Orders, plans, C-1 with container 1; H01, H02 and H03 received, loaded, the container sealed with S1; FIN's
	// pool of C-1 with the freight line CNY 3,000.00, previewed and submitted: 1,000.00, 1,500.00 and 500.00.
	c1, err := d.phy.GetConsolidation(d.ctx, phyapi.GetConsolidation{TenantID: d.tenant, ConsolidationID: "C-1"})
	d.must("C-1", err)
	if c1.State != phyapi.StateClosed || len(c1.Members) != 3 {
		t.Fatalf("step 1, C-1: %+v", c1)
	}
	x1, err := d.phy.GetLoadExecution(d.ctx, phyapi.GetLoadExecution{TenantID: d.tenant, ExecutionID: "X-1"})
	d.must("X-1", err)
	if b := d.basis(); x1.State != phyapi.ExecutionClosed || x1.CloseEvidence.Reference != "SEAL-X-1" || len(b.Houses) != 3 || len(b.PackingLists) != 1 || b.PackingLists[0].UnitNo != 1 ||
		len(b.PackingLists[0].Lines) != 3 || len(b.PackingLists[0].Reworks) != 0 || b.LoadUnits[0].Status != phyapi.UnitStatusClosed {
		t.Fatalf("step 1, the sealed container: %+v %+v", x1, b)
	}
	first, err := d.previewBy("fin-1", "fin-pre-1")
	d.must("step 1, the first preview", err)
	_, err = d.submitBy("fin-1", "fin-sub-1", first.Version)
	d.must("step 1, the first submission", err)
	if p := d.pool(); len(p.Apportionments) != 1 || p.Apportionments[0].Stage != "ACTUAL" || !sameFigures(d.sharesOf(p.Apportionments[0]), 100_000, 150_000, 50_000) ||
		p.Lines[0].Cost.Minor != 300_000 {
		t.Fatalf("step 1, the pool: %+v", p)
	}
	kept, err := d.previewBy("fin-1", "fin-pre-kept")
	d.must("step 1, a preview kept for later", err)

	// 2. SEA: C-1's movement with the container stuffed and its VGM, finalized. DOC: drafts of the three house bills.
	d.must("FUL takes the evidence", d.fulTakes(16, "SH-H01", "SH-H02", "SH-H03"))
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		d.must("refresh "+s, d.refresh(s, 17))
	}
	d.must("SEA's seal", d.boxSeal("CV-1", "seal-x-1", 17))
	d.must("stuffing done", d.boxStuffingDone("CV-1", 18))
	d.must("VGM v1", d.boxVGM("CV-1", 2900, 19))
	d.masterBill(18)
	d.finalizeOf("OM-1", "fin-1", 20)
	if f, v := d.facts("OM-1"), d.cvOf("CV-1"); f.Version != 1 || f.Status != seaapi.DocFactsFinal || len(f.Houses) != 3 || len(f.Containers) != 1 || f.Containers[0].SealNumber != "SEAL-X-1" ||
		f.Containers[0].VGMAssertionID != "CV-1:1" || f.Containers[0].Packs != 18 || v.Status != "VGM_SUBMITTED" || sealList(v) != "SEAL-X-1/SHIPPER" {
		t.Fatalf("step 2, SEA's facts: %+v %+v", shown(f), v)
	}
	h01, h02, h03 := d.prepare("SH-H01", "OM-1", 21), d.prepare("SH-H02", "OM-1", 21), d.prepare("SH-H03", "OM-1", 21)
	for _, id := range []string{h01, h02, h03} {
		if doc, v := d.bill(id); doc.State != docapi.StateApproved || doc.MovementID != "OM-1" || v.Doc.Source.Version != 1 || len(v.Doc.Containers) != 1 || v.Doc.Containers[0].SealNumber != "SEAL-X-1" {
			t.Fatalf("step 2, the draft %s: %+v %+v", id, doc, v)
		}
	}

	// 3. ops-1 asks for a rework of container 1. During it FIN's pool is in work (a preview, a submission and the close
	// are refused naming the container; the pending item is IN_WORK) and SEA's pending items say that FUL is behind PHY
	// for each house: the signal the rework itself causes, absent before it. The refusal of a finalization of a FINAL
	// movement is not that signal and is not caused by the rework.
	// The business times of the rework are early on purpose: the two containers are to be loaded more than a day apart
	// (hours 22 and 47) inside the window the recording time allows, and container 1 can be loaded only after its
	// reseal.
	if got := d.reworkDay.pending(); got != "" {
		t.Fatalf("step 3, SEA's pending items before the rework: %q", got)
	}
	d.reworkBy("rework-1", "ops-1", 17)
	inWork := func(what string, err error) {
		t.Helper()
		said(t, what, err, "FIN_POOL_IN_WORK", "load unit 1 (stuffing X-1)", "wait for PHY to seal the container again or to give its stuffing up")
	}
	_, err = d.previewBy("fin-1", "fin-pre-open")
	inWork("step 3, a preview", err)
	_, err = d.submitBy("fin-1", "fin-sub-open", kept.Version)
	inWork("step 3, the submission of the preview kept", err)
	inWork("step 3, the close", d.closeP1("fin-close-open"))
	if p := d.pool(); !p.InWork || len(p.WorkUnits) != 1 || p.WorkUnits[0] != (finapi.WorkUnit{UnitNo: 1, ExecutionID: "X-1"}) || len(p.Apportionments) != 1 {
		t.Fatalf("step 3, the pool in work: %+v", p)
	}
	if got := d.oceanDay.pending(); len(got) != 2 || got[0].Kind != finapi.PendingInWork || got[0].UnitNo != 1 || got[1].Kind != finapi.PendingCostNotFinal {
		t.Fatalf("step 3, FIN's pending items, IN_WORK and the freight not matched yet: %+v", got)
	}
	if got := d.reworkDay.pending(); got != "FUL_BEHIND_PHY, FUL_BEHIND_PHY, FUL_BEHIND_PHY" {
		t.Fatalf("step 3, SEA's pending items during the rework: %q", got)
	}
	if it := d.pendingItem("FUL_BEHIND_PHY"); !it.Blocking || it.Scope != seaapi.PendingScopeHouse || !strings.HasPrefix(it.SubjectID, "SH-H") || !strings.Contains(it.Message, "FUL has not recorded PHY's latest facts") {
		t.Fatalf("step 3, the item: %+v", it)
	}
	// SEA's movement is FINAL (version 1), so a finalization is refused for its state, which a rework does not cause;
	// the refusal the rework causes, "FUL has not recorded PHY's latest facts", comes once the movement is OPEN again
	// (step 6)
	refusedWith(t, "step 3, SEA's finalization of a FINAL movement", d.finalize("fin-f0", 23), "invalid state transition")
	if f, m := d.facts("OM-1"), d.movement(); f.Version != 1 || f.Status != seaapi.DocFactsFinal || m.State != seaapi.StateFinal || !d.due("SH-H01") || !d.due("SH-H02") {
		t.Fatalf("step 3, SEA's movement: %+v %+v, FUL's evidence due: %v", shown(f), m.State, d.due("SH-H01"))
	}

	// 4. H03 is unloaded whole; the container is sealed again with a new seal S2; a new VGM.
	d.unloadForRework("H03", 18)
	d.resealAt("reseal-1", 19)
	d.must("VGM v2", d.vgmAgain("CV-1", "vgm-2-CV-1", 2_800, 20))
	b := d.basis()
	if len(b.Houses) != 3 || b.Houses[2].ShipmentID != "SH-H03" || b.Houses[2].LoadedPacks != 0 || b.Houses[0].LoadedPacks != 6 || b.Houses[1].LoadedPacks != 6 ||
		b.PackingLists[0].ExecutionState != phyapi.ExecutionClosed || len(b.PackingLists[0].Lines) != 2 || len(b.PackingLists[0].Reworks) != 1 {
		t.Fatalf("step 4, PHY's basis: %+v", b)
	}
	if rw := b.PackingLists[0].Reworks[0]; rw.AskedBy != "ops-1" || rw.End != phyapi.ReworkSealed || rw.BrokenSeal.Reference != "SEAL-X-1" || rw.NewSealReference != "SEAL-reseal-1" || !rw.EndedAt.Equal(d.hours(19)) {
		t.Fatalf("step 4, the rework: %+v", rw)
	}
	if p := d.pool(); p.InWork {
		t.Fatalf("step 4, the pool is still in work: %+v", p)
	}
	// the report read now, with H03 unloaded and still attached to C-1 with nothing loaded: H03 is listed, with the
	// rework that unloaded it, where it is now, its draft bill and its movement and no container in SEA
	mid := d.reportOf()
	if len(mid.Houses) != 3 || mid.Houses[0].ShipmentID != "SH-H01" || mid.Houses[1].ShipmentID != "SH-H02" || mid.Houses[2].ShipmentID != "SH-H03" {
		t.Fatalf("step 4, the report's houses: %+v", mid.Houses)
	}
	if h := mid.Houses[2]; h.InContainer || h.Left != nil || h.UnloadedBy == nil || *h.UnloadedBy != (phyapi.HouseUnloadedBy{ExecutionID: "X-1", ReworkNo: 1, UnitNo: 1, AskedBy: "ops-1"}) ||
		h.Consolidation.ConsolidationID != "C-1" || h.Consolidation.EntryNo != 1 || !h.Loaded.Nothing || len(h.Movements.Found) != 1 || h.Movements.Found[0].MovementID != "OM-1" ||
		!h.Movements.Found[0].Associated || !h.Movements.Found[0].NoVisit || len(h.Documents.Documents) != 1 || h.Documents.Documents[0].State != string(docapi.StateApproved) ||
		!h.Documents.Documents[0].OnBoardAt.IsZero() {
		t.Fatalf("step 4, H03 in the report: %+v", h)
	}
	if mid.Houses[0].UnloadedBy != nil || !mid.Houses[0].InContainer || len(mid.Recoveries) != 0 || mid.Costs.State != finapi.PoolOpen {
		t.Fatalf("step 4, the report: %+v", mid)
	}

	// 5. ops-9 transfers H03 to C-2 (container 2, its own pool, the freight apportioned with H03 in it); FUL records
	// the attachment.
	d.newConsolidation2()
	_, err = d.phy.TransferHouse(d.ctx, phyapi.TransferHouse{CommandID: "transfer-h03", TenantID: d.tenant, ActorID: "ops-9", ConsolidationID: "C-1", ToConsolidationID: "C-2",
		ShipmentID: "SH-H03", ExpectedMembershipRevision: 1, Reason: phyapi.ReasonWrongConsol, OccurredAt: d.hours(21)})
	d.must("step 5, transfer H03", err)
	plan, err := d.ful.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: "SH-H03"})
	d.must("FUL's plan of H03", err)
	if plan.ConsolidationID != "C-1" {
		t.Fatalf("step 5, FUL still records H03 in C-1: %+v", plan)
	}
	_, err = d.ful.RecordConsolAttachment(d.ctx, fulapi.RecordConsolAttachment{CommandID: "ful-attach-c2", TenantID: d.tenant, ActorID: "ops-1", ShipmentID: "SH-H03",
		ExpectedRevision: plan.Revision, ConsolidationID: "C-2", OccurredAt: d.hours(22)})
	d.must("step 5, FUL records the attachment", err)
	if plan, err = d.ful.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: "SH-H03"}); err != nil || plan.ConsolidationID != "C-2" || plan.MembershipEntryNo != 2 {
		t.Fatalf("step 5, FUL after the transfer: %+v %v", plan, err)
	}
	m, err := d.phy.GetConsolMembership(d.ctx, phyapi.GetConsolMembership{TenantID: d.tenant, ShipmentID: "SH-H03"})
	if err != nil || !m.Live || m.Entry.ConsolidationID != "C-2" || m.History[0].End != phyapi.EntryTransferred || m.History[0].ReworkExecutionID != "X-1" || m.History[0].ReworkNo != 1 {
		t.Fatalf("step 5, the membership of H03: %+v %v", m, err)
	}
	_, err = d.fin.OpenConsolCostPool(d.ctx, finapi.OpenConsolCostPool{CommandID: "fin-pool2", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-2", ConsolidationID: "C-2", OccurredAt: d.hours(33)})
	d.must("open P-2", err)
	_, err = d.fin.RecordConsolCost(d.ctx, finapi.RecordConsolCost{CommandID: "fin-cost2", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-2", ExpectedRevision: 1,
		ChargeCode: "OCEAN_FREIGHT", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: 100_000}},
		Reason: "the line's freight for the second container", OccurredAt: d.hours(33)})
	d.must("cost of P-2", err)
	p2, err := d.fin.GetConsolCostPool(d.ctx, finapi.GetConsolCostPool{TenantID: d.tenant, PoolID: "P-2"})
	d.must("P-2", err)
	_, err = previewAndSubmit(d.ctx, d.fin, finapi.PreviewConsolCostApportionment{CommandID: "fin-apportion-p2", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-2",
		ExpectedRevision: p2.Revision, LineNos: []int{1}, OccurredAt: d.hours(34)})
	d.must("apportion P-2", err)
	if p2, err = d.fin.GetConsolCostPool(d.ctx, finapi.GetConsolCostPool{TenantID: d.tenant, PoolID: "P-2"}); err != nil || len(p2.Apportionments) != 1 ||
		len(p2.Apportionments[0].Lines[0].Shares) != 1 || p2.Apportionments[0].Lines[0].Shares[0].ShipmentID != "SH-H03" || p2.Apportionments[0].Lines[0].Shares[0].Amount.Minor != 100_000 {
		t.Fatalf("step 5, P-2's apportionment: %+v %v", p2.Apportionments, err)
	}

	// 6. SEA: C-1 is refreshed and finalized with S2 and two houses; C-2 is finalized; the containers are loaded at
	// different times (container 1 at hour 32, container 2 at hour 40). DOC: H01 and H02 are issued on C-1 dated by
	// container 1's loading; H03's draft moves to C-2 and is issued dated by container 2's loading.
	d.endH03(20)
	if m := d.movement(); m.State != seaapi.StateOpen || m.DocFactsVersion != 1 {
		t.Fatalf("step 6, ending H03's association reopens the movement: %+v", m.State)
	}
	refusedWith(t, "step 6, SEA's finalization while FUL is behind PHY", d.finalize("fin-f0-open", 20), "FUL has not recorded PHY's latest facts of")
	d.must("FUL takes the resealed evidence", d.fulTakes(20, "SH-H01", "SH-H02"))
	d.must("refresh H01", d.refresh("SH-H01", 21))
	d.visit["CV-1"] = d.cvOf("CV-1").Revision
	d.must("refresh H02", d.refresh("SH-H02", 21))
	if got := sealList(d.cvOf("CV-1")); got != "SEAL-X-1/SHIPPER SEAL-RESEAL-1/SHIPPER" {
		t.Fatalf("step 6, the seals of CV-1: %s", got)
	}
	d.load("CV-1", 22)
	d.finalize("fin-2", 22)
	f1 := d.facts("OM-1")
	if c := f1.Containers[0]; f1.Version != 2 || f1.Status != seaapi.DocFactsFinal || len(f1.Houses) != 2 || f1.Houses[0].ShipmentID != "SH-H01" || f1.Houses[1].ShipmentID != "SH-H02" ||
		c.SealNumber != "SEAL-RESEAL-1" || c.VGMAssertionID != "CV-1:2" || c.Packs != 12 || c.GrossWeightGrams != 612_000 || !c.OnBoardAt.Equal(d.hours(22)) || !f1.OnBoardAt.Equal(d.hours(22)) {
		t.Fatalf("step 6, OM-1's version 2: %+v", shown(f1))
	}
	d.secondMovementAt(47)
	f2 := d.facts("OM-2")
	if c := f2.Containers[0]; f2.Version != 1 || f2.Status != seaapi.DocFactsFinal || len(f2.Houses) != 1 || f2.Houses[0].ShipmentID != "SH-H03" || c.Packs != 6 || !c.OnBoardAt.Equal(d.hours(47)) {
		t.Fatalf("step 6, OM-2's version 1: %+v", shown(f2))
	}
	for _, id := range []string{h01, h02} {
		d.refreshBill(id, 2, "", 27)
		d.approve(id, 3, 28)
		d.must("issue "+id, d.issue(id, 4, 29))
		if doc, v := d.bill(id); doc.State != docapi.StateIssued || doc.MovementID != "OM-1" || !v.Doc.OnBoardAt.Equal(d.hours(22)) || v.Doc.Source.Version != 2 ||
			v.Doc.Containers[0].SealNumber != "SEAL-RESEAL-1" {
			t.Fatalf("step 6, the bill %s: %+v %+v", id, doc, v.Doc)
		}
	}
	d.refreshBill(h03, 2, "OM-2", 47)
	d.approve(h03, 3, 47)
	d.must("issue H03's bill", d.issue(h03, 4, 47))
	if doc, v := d.bill(h03); doc.State != docapi.StateIssued || doc.MovementID != "OM-2" || doc.ConsolidationID != "C-2" || !v.Doc.OnBoardAt.Equal(d.hours(47)) || v.Doc.Source.Version != 1 {
		t.Fatalf("step 6, H03's bill: %+v %+v", doc, v.Doc)
	}
	// the containers were loaded on different days in the business day the bills print (UTC+8): hours 22 and 47 are 25
	// hours apart, so the two dates differ whatever the time of day the test runs
	utc8 := time.FixedZone("UTC+8", 8*3600)
	_, billA := d.bill(h01)
	_, billB := d.bill(h03)
	dayA, dayB := billA.Doc.OnBoardAt.In(utc8).Format(time.DateOnly), billB.Doc.OnBoardAt.In(utc8).Format(time.DateOnly)
	if dayA == dayB || dayA != d.hours(22).In(utc8).Format(time.DateOnly) || dayB != d.hours(47).In(utc8).Format(time.DateOnly) || billB.Doc.OnBoardAt.Sub(billA.Doc.OnBoardAt) < 24*time.Hour {
		t.Fatalf("step 6, the bills' on-board days: %s and %s", dayA, dayB)
	}
	d.wantPending("OM-1 after the issue", "OM-1", "")
	d.wantPending("OM-2 after the issue", "OM-2", "")

	// 7. FIN on C-1: H03's 500.00 is pending, not in H03's profit and listed; FORMER_SHARE_PENDING names ops-1 and
	// ops-9. The effect of a preview with a loss of 100.00 and an exception cost of 200.00 of the carrier (the
	// reference is the carrier) under CLAIM-1: ops-1 and ops-9 are refused as the one who submits and fin-1 is
	// accepted: H01 1,080.00, H02 1,620.00, H03 bears the loss. The creditor invoice of the freight is matched final
	// and C-1's pool closes.
	d.settleFreight()
	h03p := d.jobProfit("H03")
	if h03p.PoolCost.Minor != 100_000 || h03p.FormerSharesPending.Minor != 50_000 || len(h03p.FormerShares) != 1 || h03p.FormerShares[0].PoolID != "P-1" || h03p.ReroutingLoss.Minor != 0 {
		t.Fatalf("step 7, H03 before the attribution: %+v", h03p)
	}
	got := d.oceanDay.pending()
	if len(got) != 1 || got[0].Kind != finapi.PendingFormerSharePending || got[0].ShipmentID != "SH-H03" || got[0].Amount.Minor != 50_000 || got[0].Actor != finapi.ActorFinance {
		t.Fatalf("step 7, FIN's pending items: %+v", got)
	}
	for _, part := range []string{"CNY 500.00", "ops-1", "ops-9"} {
		if !strings.Contains(got[0].Message, part) {
			t.Fatalf("step 7, the item does not say %q: %q", part, got[0].Message)
		}
	}
	carrier := statement("H03", 10_000, 20_000, finapi.PartyCarrier)
	carrier.PartyReference = "line-1"
	pre, err := d.previewWith("ops-2", "fin-pre-attr", carrier)
	d.must("step 7, the preview with the attribution", err)
	e := d.effect(pre.Version)
	type row struct {
		house, nowKind, kind string
		now, will, profit    int64
	}
	var rows []row
	for _, h := range e.Houses {
		rows = append(rows, row{h.ShipmentID, h.NowKind, h.PreviewKind, h.Now.Minor, h.Preview.Minor, h.ProfitChange.Minor})
	}
	if want := []row{{"SH-H01", "SHARE", "SHARE", 100_000, 108_000, -8_000}, {"SH-H02", "SHARE", "SHARE", 150_000, 162_000, -12_000}, {"SH-H03", "FORMER_SHARE_PENDING", "LOSS", 50_000, 10_000, -10_000}}; !slices.Equal(rows, want) || len(e.Exceptions) != 1 || e.Exceptions[0].Party != finapi.PartyCarrier || e.Exceptions[0].After.Minor != 20_000 || !e.Current {
		t.Fatalf("step 7, the effect: %+v, want %+v", e, want)
	}
	_, err = d.submitBy("ops-1", "fin-sub-ops-1", pre.Version)
	said(t, "step 7, ops-1 asked for the rework", err, "FIN_ATTRIBUTION_CONFIRMER", "ops-1 asked for rework 1")
	_, err = d.submitBy("ops-9", "fin-sub-ops-9", pre.Version)
	said(t, "step 7, ops-9 transferred the house", err, "FIN_ATTRIBUTION_CONFIRMER", "ops-9 ended the place of house SH-H03")
	_, err = d.submitBy("fin-1", "fin-sub-fin-1", pre.Version)
	d.must("step 7, fin-1's submission", err)
	a := d.pool().Apportionments[1]
	if !sameFigures(d.sharesOf(a), 108_000, 162_000) || a.By != "fin-1" || len(a.Lines[0].Attributions) != 1 || a.Lines[0].Attributions[0].Loss.Minor != 10_000 ||
		a.Lines[0].Attributions[0].Exception.Minor != 20_000 || a.Lines[0].Attributions[0].Party != finapi.PartyCarrier || a.Lines[0].Attributions[0].PartyReference != "line-1" {
		t.Fatalf("step 7, the apportionment: %+v", a)
	}
	if h03p = d.jobProfit("H03"); h03p.ReroutingLoss.Minor != 10_000 || h03p.PoolCost.Minor != 110_000 || h03p.FormerSharesPending.Minor != 0 {
		t.Fatalf("step 7, H03 after the attribution: %+v", h03p)
	}
	if got = d.oceanDay.pending(); len(got) != 0 {
		t.Fatalf("step 7, FIN's pending items before the close: %+v", got)
	}
	d.must("step 7, the close of C-1's pool", d.closeP1("fin-close"))
	if p := d.pool(); p.State != finapi.PoolClosed {
		t.Fatalf("step 7, the pool: %s", p.State)
	}

	// 8. The report of container 1.
	r := d.reportOf()
	if len(r.Stuffings) != 1 || r.Stuffings[0].ExecutionID != "X-1" || r.Stuffings[0].GivenUp || len(r.Stuffings[0].Reworks) != 1 {
		t.Fatalf("step 8, the stuffings: %+v", r.Stuffings)
	}
	if rw := r.Stuffings[0].Reworks[0]; rw.ReworkNo != 1 || rw.AskedBy != "ops-1" || rw.Reason != "rework rework-1" || !rw.AskedAt.Equal(d.hours(17)) || rw.BrokenSeal.Reference != "SEAL-X-1" ||
		rw.End != phyapi.ReworkSealed || !rw.EndedAt.Equal(d.hours(19)) || rw.NewSealReference != "SEAL-reseal-1" {
		t.Fatalf("step 8, the rework: %+v", rw)
	}
	if len(r.Houses) != 3 {
		t.Fatalf("step 8, the houses: %+v", r.Houses)
	}
	for i, house := range []string{"SH-H01", "SH-H02"} {
		h := r.Houses[i]
		if h.ShipmentID != house || !h.InContainer || h.Left != nil || h.Consolidation.ConsolidationID != "C-1" || h.Consolidation.InNone || len(h.Loaded.Units) != 1 || h.Loaded.Units[0].UnitNo != 1 ||
			h.Loaded.Units[0].ExecutionID != "X-1" || h.Loaded.Units[0].Packs != 6 || len(h.Movements.Found) != 1 || len(h.Documents.Documents) != 1 {
			t.Fatalf("step 8, %s: %+v", house, h)
		}
		mv, doc := h.Movements.Found[0], h.Documents.Documents[0]
		if mv.MovementID != "OM-1" || mv.State != seaapi.StateFinal || !mv.Associated || mv.Visit == nil || mv.Visit.VisitID != "CV-1" || mv.Visit.Status != seaapi.VisitLoaded || !mv.Visit.OnBoard ||
			!mv.Visit.OnBoardAt.Equal(d.hours(22)) || doc.Kind != docapi.KindHBL || doc.State != string(docapi.StateIssued) || doc.Number == "" || !doc.OnBoardAt.Equal(d.hours(22)) {
			t.Fatalf("step 8, %s in SEA and DOC: %+v %+v", house, mv, doc)
		}
	}
	h := r.Houses[2]
	if h.ShipmentID != "SH-H03" || h.InContainer || h.Left == nil || h.Left.End != phyapi.FormerTransferred || h.Left.TransferredTo != "C-2" || h.Left.EndedBy != "ops-9" || !h.Left.EndedAt.Equal(d.hours(21)) ||
		h.Left.ReworkAskedBy != "ops-1" || h.Consolidation.ConsolidationID != "C-2" || h.Consolidation.EntryNo != 2 || len(h.Loaded.Units) != 1 || h.Loaded.Units[0].ExecutionID != "X-3" ||
		h.Loaded.Units[0].UnitNo != 1 || h.Loaded.Units[0].Packs != 6 || len(h.Movements.Found) != 1 || len(h.Documents.Documents) != 1 {
		t.Fatalf("step 8, H03: %+v", h)
	}
	if mv, doc := h.Movements.Found[0], h.Documents.Documents[0]; mv.MovementID != "OM-2" || mv.State != seaapi.StateFinal || !mv.Associated || mv.Visit == nil || mv.Visit.VisitID != "CV-3" ||
		!mv.Visit.OnBoard || !mv.Visit.OnBoardAt.Equal(d.hours(47)) || doc.State != string(docapi.StateIssued) || doc.MovementID != "OM-2" || !doc.OnBoardAt.Equal(d.hours(47)) {
		t.Fatalf("step 8, H03 in SEA and DOC: %+v %+v", mv, doc)
	}
	// the freight line: 1,080.00 + 1,620.00 + a loss of 100.00 + an exception cost of 200.00 (carrier) = 3,000.00
	c := r.Costs
	if c.NoPool || c.Unanswered != "" || c.PoolID != "P-1" || c.State != finapi.PoolClosed || len(c.Lines) != 1 {
		t.Fatalf("step 8, the costs: %+v", c)
	}
	l := c.Lines[0]
	if l.LineNo != 1 || l.Cost.Minor != 300_000 || l.Local.Minor != 300_000 || l.ApportionmentNo != 2 || len(l.Shares) != 2 || l.Shares[0].ShipmentID != "SH-H01" || l.Shares[0].Amount.Minor != 108_000 ||
		l.Shares[1].ShipmentID != "SH-H02" || l.Shares[1].Amount.Minor != 162_000 || len(l.Losses) != 1 || l.Losses[0] != (LossBorne{UnitNo: 1, ShipmentID: "SH-H03", JobID: "JOB-H03",
		Amount: kernel.Money{Currency: "CNY", Minor: 10_000}, Local: kernel.Money{Currency: "CNY", Minor: 10_000}, Voucher: "CLAIM-1", DepartedAt: d.hours(21)}) ||
		len(l.Exceptions) != 1 || l.Exceptions[0].Party != finapi.PartyCarrier || l.Exceptions[0].Reference != "line-1" || l.Exceptions[0].Amount.Minor != 20_000 || l.Exceptions[0].Voucher != "CLAIM-1" ||
		l.ToAllocate.Minor != 0 || len(l.FormerPending) != 0 || l.Unapportioned.Minor != 0 || l.UnapportionedLo.Minor != 0 {
		t.Fatalf("step 8, the freight line: %+v", l)
	}
	if tt := c.Totals; tt.Pooled.Minor != 300_000 || tt.Apportioned.Minor != 270_000 || tt.Losses.Minor != 10_000 || tt.Exceptions.Minor != 20_000 || tt.ExceptionsPending.Minor != 0 ||
		tt.ToAllocated.Minor != 0 || tt.FormerSharesPending.Minor != 0 || tt.Unapportioned.Minor != 0 || !tt.Conserved {
		t.Fatalf("step 8, the totals: %+v", tt)
	}
	// to recover: CNY 200.00 from the carrier, CNY 100.00 that may be charged to H03's customer (not decided)
	if len(r.Recoveries) != 2 || r.Recoveries[0].Kind != RecoverFromParty || r.Recoveries[0].Party != finapi.PartyCarrier || r.Recoveries[0].Reference != "line-1" || r.Recoveries[0].Amount.Minor != 20_000 ||
		r.Recoveries[1].Kind != RecoverChargeToCustomer || r.Recoveries[1].ShipmentID != "SH-H03" || r.Recoveries[1].Amount.Minor != 10_000 || r.Recoveries[1].Voucher != "CLAIM-1" ||
		!strings.Contains(r.Recoveries[1].Note, "the system does not decide") || !strings.Contains(r.RecoveriesNote, "Claims are not built") {
		t.Fatalf("step 8, what there is to recover: %+v", r.Recoveries)
	}
	if len(r.Pending) != 0 || r.PendingNoPool || r.PendingUnanswered != "" {
		t.Fatalf("step 8, FIN's pending list of C-1: %+v", r.Pending)
	}
}

// reportOfBoxA reads the report of container 1 of C-1 from PHY and FIN over the day, with no SEA and no DOC wired.
func reportOfBoxA(d *oceanDay) (ReworkedContainerReport, error) {
	d.t.Helper()
	return NewReworkedContainerReader(d.phy, nil, nil, d.fin).ReworkedContainerReport(d.ctx, ReworkedContainerQuery{TenantID: d.tenant, ConsolidationID: "C-1", LoadUnitNo: 1})
}

// A house unloaded in a rework and only detached: it is in no consolidation, and its former share of 500.00 is pending,
// so the report lists it as nobody's and as something nothing is decided on. With no SEA and no DOC wired their parts
// say so. Finance then attributes a loss of 100.00 and an exception cost of 200.00 whose party is pending: the report
// lists it as a responsibility to be established, and FIN's pending list says the same.
func TestAHouseOnlyDetachedIsInNoConsolidationAndItsShareIsPendingWithPHYAndFIN(t *testing.T) {
	d := boxA(t, "cargo-box-a-report-detached-")
	first, err := d.previewBy("fin-1", "fin-pre-1")
	d.must("the first preview", err)
	_, err = d.submitBy("fin-1", "fin-sub-1", first.Version)
	d.must("the first submission", err)
	d.settleFreight()
	d.reworkBy("rework-1", "ops-1", 22)
	d.unloadForRework("H03", 23)
	d.resealAt("reseal-1", 24)
	_, err = d.phy.DetachHouse(d.ctx, phyapi.DetachHouse{CommandID: "detach-h03", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: "SH-H03",
		ExpectedMembershipRevision: 1, Reason: phyapi.ReasonSpaceShortage, OccurredAt: d.hours(26)})
	d.must("detach H03", err)

	r, err := reportOfBoxA(d)
	d.must("the report", err)
	if len(r.Houses) != 3 || r.Houses[0].ShipmentID != "SH-H01" || !r.Houses[0].InContainer || r.Houses[1].ShipmentID != "SH-H02" || !r.Houses[1].InContainer {
		t.Fatalf("the houses: %+v", r.Houses)
	}
	h := r.Houses[2]
	if h.ShipmentID != "SH-H03" || h.InContainer || h.Left == nil || h.Left.End != phyapi.FormerDetached || h.Left.Reason != phyapi.ReasonSpaceShortage || h.Left.EndedBy != "ops-1" ||
		!h.Left.EndedAt.Equal(d.hours(26)) || h.Left.ReworkAskedBy != "ops-1" || !h.Consolidation.InNone || len(h.Loaded.Units) != 0 || h.Movements.Unanswered != "SEA is not wired" ||
		h.Documents.Unanswered != "DOC is not wired" {
		t.Fatalf("H03: %+v", h)
	}
	l := r.Costs.Lines[0]
	if len(r.Costs.Lines) != 1 || len(l.Shares) != 2 || l.Shares[0].Amount.Minor != 100_000 || l.Shares[1].Amount.Minor != 150_000 || len(l.FormerPending) != 1 || l.FormerPending[0].ShipmentID != "SH-H03" ||
		l.FormerPending[0].Amount.Minor != 50_000 || l.Unapportioned.Minor != 0 || r.Costs.Totals.FormerSharesPending.Minor != 50_000 || r.Costs.Totals.Apportioned.Minor != 250_000 || !r.Costs.Totals.Conserved {
		t.Fatalf("the costs with the former share pending: %+v", r.Costs)
	}
	if len(r.Recoveries) != 1 || r.Recoveries[0].Kind != RecoverFormerSharePend || r.Recoveries[0].ShipmentID != "SH-H03" || r.Recoveries[0].Amount.Minor != 50_000 {
		t.Fatalf("what there is to recover: %+v", r.Recoveries)
	}
	if k := pendingKinds(r.Pending); !slices.Equal(k, []string{finapi.PendingFormerSharePending}) || r.Pending[0].Amount.Minor != 50_000 {
		t.Fatalf("FIN's pending items: %+v", r.Pending)
	}

	d.must("the attribution", d.attribute("fin-attr", "fin-1", statement("H03", 10_000, 20_000, finapi.PartyPending)))
	if r, err = reportOfBoxA(d); err != nil {
		t.Fatal(err)
	}
	if got := r.Costs.Lines[0]; len(got.FormerPending) != 0 || len(got.Exceptions) != 1 || got.Exceptions[0].Party != finapi.PartyPending || got.Exceptions[0].Amount.Minor != 20_000 ||
		r.Costs.Totals.ExceptionsPending.Minor != 20_000 || !r.Costs.Totals.Conserved {
		t.Fatalf("the costs after the attribution: %+v", r.Costs)
	}
	if len(r.Recoveries) != 2 || r.Recoveries[0].Kind != RecoverToEstablish || r.Recoveries[0].Amount.Minor != 20_000 || r.Recoveries[1].Kind != RecoverChargeToCustomer || r.Recoveries[1].Amount.Minor != 10_000 {
		t.Fatalf("what there is to recover after the attribution: %+v", r.Recoveries)
	}
	if k := pendingKinds(r.Pending); !slices.Equal(k, []string{finapi.PendingExceptionCause}) {
		t.Fatalf("FIN's pending items after the attribution: %+v", r.Pending)
	}
}

// A house whose shipment was cancelled after it was unloaded in a rework: PHY lists it as having left because of the
// cancellation, it is in no consolidation, and its former share is pending as the share of any house that left.
func TestAHouseWhoseShipmentWasCancelledIsListedWithPHYAndFIN(t *testing.T) {
	d := boxA(t, "cargo-box-a-report-cancelled-")
	first, err := d.previewBy("fin-1", "fin-pre-1")
	d.must("the first preview", err)
	_, err = d.submitBy("fin-1", "fin-sub-1", first.Version)
	d.must("the first submission", err)
	d.reworkBy("rework-1", "ops-1", 22)
	d.unloadForRework("H03", 23)
	d.resealAt("reseal-1", 24)
	ful := fulOf(t)
	plan, err := ful.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: "SH-H03"})
	d.must("FUL's plan of H03", err)
	_, err = ful.CancelShipment(d.ctx, fulapi.CancelShipment{CommandID: "cancel-h03", TenantID: d.tenant, ActorID: "planner-1", ShipmentID: "SH-H03", ExpectedRevision: plan.Revision,
		Reason: "the customer withdrew", OccurredAt: d.hours(25)})
	d.must("cancel H03", err)
	_, err = d.phy.EndMembershipForCancelledShipment(d.ctx, phyapi.EndMembershipForCancelledShipment{CommandID: "end-h03", TenantID: d.tenant, ActorID: "ops-9", ConsolidationID: "C-1",
		ShipmentID: "SH-H03", ExpectedMembershipRevision: 1, OccurredAt: d.hours(26)})
	d.must("end the membership of H03", err)

	r, err := reportOfBoxA(d)
	d.must("the report", err)
	if len(r.Houses) != 3 {
		t.Fatalf("the houses: %+v", r.Houses)
	}
	if h := r.Houses[2]; h.ShipmentID != "SH-H03" || h.Left == nil || h.Left.End != phyapi.FormerCancelled || h.Left.Reason != phyapi.ReasonShipmentCancelled || h.Left.EndedBy != "ops-9" ||
		!h.Consolidation.InNone || h.InContainer {
		t.Fatalf("H03: %+v", r.Houses[2])
	}
	if l := r.Costs.Lines[0]; len(l.FormerPending) != 1 || l.FormerPending[0].ShipmentID != "SH-H03" || l.FormerPending[0].Amount.Minor != 50_000 || len(l.Shares) != 2 || !r.Costs.Totals.Conserved {
		t.Fatalf("the costs: %+v", r.Costs)
	}
}

// A container given up: rework 1 (ask-1) ends sealed and rework 2 (ask-2) unloads the other houses and gives the
// stuffing up, before anybody is detached. The report lists both reworks of the stuffing given up, no stuffing in force
// and the three houses, all still attached to C-1 with nothing loaded, each with the rework that unloaded it (H03
// rework 1, H01 and H02 rework 2). The freight no apportionment covers is unapportioned.
func TestAContainerGivenUpIsReportedWithItsReworksWithPHYAndFIN(t *testing.T) {
	d := boxA(t, "cargo-box-a-report-givenup-")
	d.reworkBy("rework-1", "ask-1", 22)
	d.unloadForRework("H03", 23)
	d.resealAt("reseal-1", 24)
	d.reworkBy("rework-2", "ask-2", 25)
	d.unloadForRework("H01", 26)
	d.unloadForRework("H02", 27)
	_, err := d.phy.CancelLoadExecution(d.ctx, phyapi.CancelLoadExecution{CommandID: "give-up", TenantID: d.tenant, ActorID: "cfs-2", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Reason: "the booking is rolled", OccurredAt: d.hours(28)})
	d.must("give the stuffing up", err)

	r, err := reportOfBoxA(d)
	d.must("the report", err)
	if len(r.Stuffings) != 1 || r.Stuffings[0].ExecutionID != "X-1" || !r.Stuffings[0].GivenUp || len(r.Stuffings[0].Reworks) != 2 || r.Stuffings[0].Reworks[0].AskedBy != "ask-1" ||
		r.Stuffings[0].Reworks[0].End != phyapi.ReworkSealed || r.Stuffings[0].Reworks[1].AskedBy != "ask-2" || r.Stuffings[0].Reworks[1].End != phyapi.ReworkGivenUp {
		t.Fatalf("the stuffings: %+v", r.Stuffings)
	}
	if len(r.Houses) != 3 {
		t.Fatalf("the houses: %+v", r.Houses)
	}
	for i, h := range []struct {
		house string
		no    int
		by    string
	}{{"SH-H01", 2, "ask-2"}, {"SH-H02", 2, "ask-2"}, {"SH-H03", 1, "ask-1"}} {
		x := r.Houses[i]
		if x.ShipmentID != h.house || x.InContainer || x.Left != nil || x.UnloadedBy == nil || *x.UnloadedBy != (phyapi.HouseUnloadedBy{ExecutionID: "X-1", ReworkNo: h.no, UnitNo: 1, AskedBy: h.by}) ||
			x.Consolidation.ConsolidationID != "C-1" || !x.Loaded.Nothing {
			t.Fatalf("%s: %+v", h.house, x)
		}
	}
	if l := r.Costs.Lines[0]; l.ApportionmentNo != 0 || l.Unapportioned.Minor != 300_000 || len(r.Recoveries) != 0 || r.Costs.Totals.Unapportioned.Minor != 300_000 || !r.Costs.Totals.Conserved {
		t.Fatalf("the costs: %+v", r.Costs)
	}
}

// The report of a consolidation or a load unit that PHY does not have is an error that says which.
func TestTheReportOfAnUnknownContainerFailsWithPHYAndFIN(t *testing.T) {
	d := boxA(t, "cargo-box-a-report-unknown-")
	reader := NewReworkedContainerReader(d.phy, nil, nil, d.fin)
	_, err := reader.ReworkedContainerReport(d.ctx, ReworkedContainerQuery{TenantID: d.tenant, ConsolidationID: "C-9", LoadUnitNo: 1})
	said(t, "an unknown consolidation", err, "unknown consolidation or load unit", "C-9")
	_, err = reader.ReworkedContainerReport(d.ctx, ReworkedContainerQuery{TenantID: d.tenant, ConsolidationID: "C-1", LoadUnitNo: 2})
	said(t, "an unknown load unit", err, "unknown consolidation or load unit", "no load unit 2")
	_, err = reader.ReworkedContainerReport(d.ctx, ReworkedContainerQuery{TenantID: uniqueTenant("cargo-other-tenant-"), ConsolidationID: "C-1", LoadUnitNo: 1})
	said(t, "another tenant", err, "unknown consolidation or load unit")
}

// Two containers, each reworked, with the real ORG, RATE, COM, FUL, PHY and FIN: C-1 holds H01 and H02 in container 1
// (X-1) and H03 and H04 in container 2 (X-2), and H05 is received and never loaded. A rework of X-1 unloads H02, which
// is detached; a rework of X-2 unloads H04, which is detached; H05 is shut out. Container 1's report lists H01 and H02
// only, container 2's lists H03 and H04 only, and H05 is in neither.
func TestEachContainerReportListsOnlyItsOwnHousesWithPHYAndFIN(t *testing.T) {
	weights := map[string]int64{"H01": 300, "H02": 300, "H03": 300, "H04": 300, "H05": 300}
	volumes := map[string]int64{"H01": 1_000_000, "H02": 1_000_000, "H03": 1_000_000, "H04": 1_000_000, "H05": 1_000_000}
	d := newOceanDay(t, "cargo-box-a-report-two-", weights, volumes, []string{"22G1", "22G1"}, nil)
	d.stuff("X-1", 1)
	d.load("X-1", "H01", 6, 0)
	d.load("X-1", "H02", 6, 0)
	d.stuff("X-2", 2)
	d.load("X-2", "H03", 6, 0)
	d.load("X-2", "H04", 6, 0)
	d.seal("X-1")
	d.seal("X-2")
	// the freight of CNY 3,000.00 is apportioned over the four loaded houses, H05 named out
	_, err0 := d.fin.RecordConsolCost(d.ctx, finapi.RecordConsolCost{CommandID: "fin-freight", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision,
		ChargeCode: "OCEAN_FREIGHT", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: 300_000}},
		Reason: "the line's freight for the containers", OccurredAt: d.hours(16)})
	d.must("the freight", err0)
	first, err0 := d.previewBy("fin-1", "fin-pre-1", "SH-H05")
	d.must("the first preview", err0)
	_, err0 = d.submitBy("fin-1", "fin-sub-1", first.Version)
	d.must("the first submission", err0)

	d.reworkBy("rework-1", "ops-1", 22)
	d.unloadForRework("H02", 23)
	d.resealAt("reseal-1", 24)
	detach := func(house string, hour int) {
		t.Helper()
		_, err := d.phy.DetachHouse(d.ctx, phyapi.DetachHouse{CommandID: "detach-" + house, TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: "SH-" + house,
			ExpectedMembershipRevision: 1, Reason: phyapi.ReasonSpaceShortage, OccurredAt: d.hours(hour)})
		d.must("detach "+house, err)
	}
	detach("H02", 26)

	_, err := d.phy.OpenStuffingForRework(d.ctx, phyapi.OpenStuffingForRework{CommandID: "rework-2", TenantID: d.tenant, ActorID: "ops-2", ExecutionID: "X-2",
		ExpectedRevision: d.revision["X-2"], Reason: "rework 2", OccurredAt: d.hours(27)})
	d.must("open the rework of X-2", err)
	d.revision["X-2"]++
	_, err = d.phy.CloseCargoLot(d.ctx, phyapi.CloseCargoLot{CommandID: "close-lot-H04", TenantID: d.tenant, ActorID: "cfs-1", LotID: "L-H04", ExpectedRevision: 3, OccurredAt: d.hours(16)})
	d.must("close the lot of H04", err)
	_, err = d.phy.UnloadHouseForRework(d.ctx, phyapi.UnloadHouseForRework{CommandID: "unload-H04", TenantID: d.tenant, ActorID: "wh-1", ExecutionID: "X-2",
		ExpectedRevision: d.revision["X-2"], ShipmentID: "SH-H04", Evidence: phyapi.Evidence{Source: "CFS-WMS", Reference: "UNSTUFF-H04", OccurredAt: d.hours(28)}, OccurredAt: d.hours(28)})
	d.must("unload H04", err)
	d.revision["X-2"]++
	_, err = d.phy.CloseLoadExecution(d.ctx, phyapi.CloseLoadExecution{CommandID: "reseal-2", TenantID: d.tenant, ActorID: "cfs-2", ExecutionID: "X-2",
		ExpectedRevision: d.revision["X-2"], Evidence: d.evidence(phyapi.EvidenceSeal, "SEAL-reseal-2", 29), OccurredAt: d.hours(29)})
	d.must("seal X-2 again", err)
	detach("H04", 30)
	detach("H05", 31)

	housesOf := func(unit int) (out []string, r ReworkedContainerReport) {
		t.Helper()
		r, err := NewReworkedContainerReader(d.phy, nil, nil, d.fin).ReworkedContainerReport(d.ctx, ReworkedContainerQuery{TenantID: d.tenant, ConsolidationID: "C-1", LoadUnitNo: unit})
		d.must("the report of unit "+string(rune('0'+unit)), err)
		for _, h := range r.Houses {
			out = append(out, h.ShipmentID)
		}
		return out, r
	}
	b := d.basis()
	if len(b.FormerHouses) != 3 || b.FormerHouses[2].ShipmentID != "SH-H05" || b.FormerHouses[2].ReworkFrom != phyapi.ReworkFromNone || b.FormerHouses[0].ReworkUnitNo != 1 || b.FormerHouses[1].ReworkUnitNo != 2 {
		t.Fatalf("PHY's former houses: %+v", b.FormerHouses)
	}
	got1, r1 := housesOf(1)
	got2, r2 := housesOf(2)
	if !slices.Equal(got1, []string{"SH-H01", "SH-H02"}) || !slices.Equal(got2, []string{"SH-H03", "SH-H04"}) {
		t.Fatalf("container 1: %v, container 2: %v", got1, got2)
	}
	if len(r1.Costs.Lines) != 1 || len(r1.Costs.Lines[0].FormerPending) != 2 || r1.Costs.Lines[0].FormerPending[0].UnitNo != 1 || r1.Costs.Lines[0].FormerPending[1].UnitNo != 2 ||
		len(r1.Recoveries) != 1 || len(r2.Recoveries) != 1 || r1.Recoveries[0].ShipmentID != "SH-H02" || r2.Recoveries[0].ShipmentID != "SH-H04" {
		t.Fatalf("the costs before the attribution: %+v %+v", r1.Costs, r1.Recoveries)
	}
	// finance attributes: H02 all to the remaining cargo, H04 a loss of 100.00 and an exception cost of 200.00 of the
	// carrier. The pool's costs are the same in both reports and tag each with its unit; container 1 recovers nothing
	// of H04's and container 2 recovers H04's.
	carrier := statement("H04", 10_000, 20_000, finapi.PartyCarrier)
	carrier.PartyReference = "line-1"
	d.must("the attribution", d.attribute("fin-attr", "fin-1", statement("H02", 0, 0, ""), carrier))
	_, r1 = housesOf(1)
	_, r2 = housesOf(2)
	for _, r := range []ReworkedContainerReport{r1, r2} {
		l := r.Costs.Lines[0]
		if len(l.Losses) != 1 || l.Losses[0].ShipmentID != "SH-H04" || l.Losses[0].UnitNo != 2 || len(l.Exceptions) != 1 || l.Exceptions[0].UnitNo != 2 || l.Exceptions[0].Reference != "line-1" ||
			len(l.FormerPending) != 0 || !r.Costs.Totals.Conserved || r.Costs.Totals.Losses.Minor != 10_000 || r.Costs.Totals.Exceptions.Minor != 20_000 {
			t.Fatalf("the pool's costs in the report of unit %d: %+v", r.LoadUnitNo, r.Costs)
		}
	}
	if len(r1.Recoveries) != 0 {
		t.Fatalf("container 1 recovers what container 2's house gives rise to: %+v", r1.Recoveries)
	}
	if len(r2.Recoveries) != 2 || r2.Recoveries[0].Kind != RecoverFromParty || r2.Recoveries[0].ShipmentID != "SH-H04" || r2.Recoveries[0].UnitNo != 2 || r2.Recoveries[0].Amount.Minor != 20_000 ||
		r2.Recoveries[1].Kind != RecoverChargeToCustomer || r2.Recoveries[1].Amount.Minor != 10_000 {
		t.Fatalf("container 2's recoveries: %+v", r2.Recoveries)
	}
	if len(r1.Stuffings) != 1 || r1.Stuffings[0].ExecutionID != "X-1" || r1.Stuffings[0].Reworks[0].AskedBy != "ops-1" || len(r2.Stuffings) != 1 || r2.Stuffings[0].ExecutionID != "X-2" ||
		r2.Stuffings[0].Reworks[0].AskedBy != "ops-2" {
		t.Fatalf("the stuffings: %+v %+v", r1.Stuffings, r2.Stuffings)
	}
}

// secondMovementAt builds OM-2 on C-2 with H03 alone in CV-3, after PHY's transfer of H03 to C-2 and FUL's recording of
// it, as V2.90's secondMovement does but with the business times of this day: the stuffing X-3 sealed at hour 33, the
// container taken as stuffed at 36, loaded at the hour given and OM-2 finalized then.
func (d *loadingDay) secondMovementAt(loadHour int) {
	d.t.Helper()
	ctx := d.ctx
	slice{tenant: d.tenant, at: d.at, departs: d.at.Add(72 * time.Hour)}.bookSea(d.t, d.book, "BK-2", "C-2", "EVER GIVEN", "9811000", "0124E", d.at.Add(96*time.Hour))
	_, err := d.sea.OpenOceanMovement(ctx, seaapi.OpenOceanMovement{CommandID: "open-2", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-2", ConsolidationID: "C-2",
		BookingID: "BK-2", OccurredAt: d.hours(30)})
	d.must("open OM-2", err)
	m2 := func() int64 {
		m, err := d.sea.GetOceanMovement(ctx, seaapi.GetOceanMovement{TenantID: d.tenant, MovementID: "OM-2"})
		d.must("OM-2", err)
		return m.Revision
	}
	c2, err := d.phy.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: d.tenant, ConsolidationID: "C-2"})
	d.must("C-2", err)
	_, err = d.phy.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close-c2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", ExpectedRevision: c2.Revision,
		OccurredAt: d.hours(30)})
	d.must("close C-2", err)
	_, err = d.phy.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "stuff-x3", TenantID: d.tenant, ActorID: "cfs-1", ExecutionID: "X-3", ConsolidationID: "C-2",
		Kind: phyapi.ExecutionStuffing, Facility: d.cfs, LoadUnitNo: 1, OccurredAt: d.hours(31)})
	d.must("open X-3", err)
	_, err = d.phy.StartLoading(ctx, phyapi.StartLoading{CommandID: "start-x3", TenantID: d.tenant, ActorID: "cfs-1", ExecutionID: "X-3", ExpectedRevision: 1, OccurredAt: d.hours(31)})
	d.must("start X-3", err)
	d.revision["X-3"] = 2
	d.loadAt("X-3", "H03", 6, 32)
	d.sealAt("X-3", 33)
	_, err = d.sea.AssociateHouseBill(ctx, seaapi.AssociateHouseBill{CommandID: "hbl-h03-om2", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-2", ExpectedRevision: m2(),
		ShipmentID: "SH-H03", OccurredAt: d.hours(33)})
	d.must("associate H03 in OM-2", err)
	d.must("FUL takes the evidence", d.fulTakes(34, "SH-H03"))
	_, err = d.sea.RefreshHouseBillFacts(ctx, seaapi.RefreshHouseBillFacts{CommandID: "refresh-h03-om2", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-2", ExpectedRevision: m2(),
		ShipmentID: "SH-H03", OccurredAt: d.hours(34)})
	d.must("refresh H03 in OM-2", err)
	a, err := d.sea.AssignContainer(ctx, seaapi.AssignContainer{CommandID: "assign-CV-3", TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-3", MovementID: "OM-2", LoadUnitNo: 1,
		ContainerNumber: "msku1234565", SizeType: "22G1", TareKg: 2180, MaxGrossKg: 30480, PlateSource: seaapi.PlateCSC, OccurredAt: d.hours(35)})
	d.must("assign CV-3", err)
	d.visit["CV-3"] = a.NewRevision
	d.must("seal CV-3", d.boxSeal("CV-3", "seal-x-3", 35))
	d.must("stuffing done CV-3", d.boxStuffingDone("CV-3", 36))
	d.must("VGM CV-3", d.boxVGM("CV-3", 2900, 36))
	_, err = d.sea.RecordMasterBill(ctx, seaapi.RecordMasterBill{CommandID: "mbl-2", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-2", ExpectedRevision: m2(), Number: "linu2611000999",
		Kind: seaapi.MasterBillCarrierMBL, Evidence: seaapi.Evidence{SHA256: strings.Repeat("9d", 32), StorageID: "mail/mbl2.pdf", MediaType: "application/pdf", CapturedAt: d.hours(36)},
		OccurredAt: d.hours(36)})
	d.must("master bill of OM-2", err)
	d.load("CV-3", loadHour)
	d.finalizeOf("OM-2", "fin-om2", loadHour)
}

// A line whose cost is revised after it was apportioned, through FIN's commands: the freight of CNY 3,000.00 is
// apportioned 1,000.00, 1,500.00 and 500.00 and then revised to CNY 3,100.00. The report says the 100.00 no
// apportionment covers is unapportioned, in both currencies, and the totals still conserve.
func TestALineRevisedAfterItsApportionmentHasItsRestUnapportionedWithPHYAndFIN(t *testing.T) {
	d := boxA(t, "cargo-box-a-report-revised-")
	first, err := d.previewBy("fin-1", "fin-pre-1")
	d.must("the first preview", err)
	_, err = d.submitBy("fin-1", "fin-sub-1", first.Version)
	d.must("the first submission", err)
	_, err = d.fin.ReviseConsolCost(d.ctx, finapi.ReviseConsolCost{CommandID: "fin-revise", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision, LineNo: 1,
		ExpectedVersion: 1, Value: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: 310_000}}, Reason: "the line charged more", OccurredAt: d.later()})
	d.must("revise the freight", err)
	r, err := reportOfBoxA(d)
	d.must("the report", err)
	l := r.Costs.Lines[0]
	if l.Cost.Minor != 310_000 || l.Local.Minor != 310_000 || l.ApportionmentNo != 1 || len(l.Shares) != 3 || l.Unapportioned.Minor != 10_000 || l.UnapportionedLo.Minor != 10_000 ||
		r.Costs.Totals.Unapportioned.Minor != 10_000 || r.Costs.Totals.Pooled.Minor != 310_000 || !r.Costs.Totals.Conserved {
		t.Fatalf("the line revised after its apportionment: %+v", r.Costs)
	}
}
