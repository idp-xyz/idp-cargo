//go:build integration

package bootstrap

import (
	"strings"
	"testing"
	"time"

	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// PHY tells which houses left a consolidation and the reworks of a stuffing given up, and FIN reads them (V2.92), with
// the real ORG, RATE, COM, FUL, PHY and FIN, on the owner's box A.

// reworkBy opens X-1 for rework as who, at the hour given; the stuffing's revision is kept.
func (d *oceanDay) reworkBy(id, who string, hour int) {
	d.t.Helper()
	_, err := d.phy.OpenStuffingForRework(d.ctx, phyapi.OpenStuffingForRework{CommandID: id, TenantID: d.tenant, ActorID: who, ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Reason: "rework " + id, OccurredAt: d.hours(hour)})
	d.must("open "+id, err)
	d.revision["X-1"]++
}

// unloadForRework closes the lot of the house, as the warehouse does when all of a house is loaded, and unloads the
// house whole in the rework under way.
func (d *oceanDay) unloadForRework(house string, hour int) {
	d.t.Helper()
	_, err := d.phy.CloseCargoLot(d.ctx, phyapi.CloseCargoLot{CommandID: "close-lot-" + house + "-" + string(rune('a'+hour)), TenantID: d.tenant, ActorID: "cfs-1", LotID: "L-" + house,
		ExpectedRevision: 3, OccurredAt: d.hours(16)})
	d.must("close the lot of "+house, err)
	_, err = d.phy.UnloadHouseForRework(d.ctx, phyapi.UnloadHouseForRework{CommandID: "unload-" + house, TenantID: d.tenant, ActorID: "wh-1", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], ShipmentID: "SH-" + house, Evidence: phyapi.Evidence{Source: "CFS-WMS", Reference: "UNSTUFF-" + house, OccurredAt: d.hours(hour)},
		OccurredAt: d.hours(hour)})
	d.must("unload "+house, err)
	d.revision["X-1"]++
}

func (d *oceanDay) resealAt(id string, hour int) {
	d.t.Helper()
	_, err := d.phy.CloseLoadExecution(d.ctx, phyapi.CloseLoadExecution{CommandID: id, TenantID: d.tenant, ActorID: "cfs-2", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Evidence: d.evidence(phyapi.EvidenceSeal, "SEAL-"+id, hour), OccurredAt: d.hours(hour)})
	d.must("seal again "+id, err)
	d.revision["X-1"]++
}

// TestTheReworksOfAStuffingGivenUpAreConfirmedByAThirdPersonWithEveryContext is the V2.91 review's F3 scenario: rework
// 1, asked by ask-1, ends sealed; rework 2, asked by ask-2, unloads the other houses and gives the stuffing up. PHY's
// basis has no packing list and tells of the stuffing and both reworks; FIN refuses ask-1 and ask-2 and accepts a third
// person. Before V2.92 ask-1 was accepted, and the sealed rework was lost with the stuffing.
func TestTheReworksOfAStuffingGivenUpAreConfirmedByAThirdPersonWithEveryContext(t *testing.T) {
	d := boxA(t, "cargo-box-a-givenup-f3-")
	d.reworkBy("rework-1", "ask-1", 22)
	d.unloadForRework("H03", 23)
	d.resealAt("reseal-1", 24)
	b := d.basis()
	if len(b.PackingLists) != 1 || len(b.PackingLists[0].Reworks) != 1 || b.PackingLists[0].Reworks[0].AskedBy != "ask-1" || b.PackingLists[0].Reworks[0].End != phyapi.ReworkSealed ||
		len(b.GivenUpStuffings) != 0 {
		t.Fatalf("after rework 1: %+v", b)
	}
	d.reworkBy("rework-2", "ask-2", 25)
	d.unloadForRework("H01", 26)
	d.unloadForRework("H02", 27)
	_, err := d.phy.CancelLoadExecution(d.ctx, phyapi.CancelLoadExecution{CommandID: "give-up", TenantID: d.tenant, ActorID: "cfs-2", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Reason: "the booking is rolled", OccurredAt: d.hours(28)})
	d.must("give the stuffing up", err)

	b = d.basis()
	if len(b.PackingLists) != 0 || len(b.GivenUpStuffings) != 1 {
		t.Fatalf("after rework 2 was given up: %d packing lists, given up %+v", len(b.PackingLists), b.GivenUpStuffings)
	}
	g := b.GivenUpStuffings[0]
	if g.UnitNo != 1 || g.ExecutionID != "X-1" || len(g.Reworks) != 2 || g.Reworks[0].ReworkNo != 1 || g.Reworks[0].AskedBy != "ask-1" || g.Reworks[0].End != phyapi.ReworkSealed ||
		g.Reworks[1].ReworkNo != 2 || g.Reworks[1].AskedBy != "ask-2" || g.Reworks[1].End != phyapi.ReworkGivenUp {
		t.Fatalf("the stuffing given up: %+v", g)
	}

	pre, err := d.previewBy("ops-1", "fin-pre")
	d.must("a preview", err)
	for who, rework := range map[string]string{"ask-1": "rework 1 of load unit 1 (given-up stuffing X-1), which ended SEALED", "ask-2": "rework 2 of load unit 1 (given-up stuffing X-1), which ended GIVEN_UP"} {
		_, err = d.submitBy(who, "fin-sub-"+who, pre.Version)
		if err == nil || !strings.Contains(err.Error(), "FIN_CONFIRMER_ASKED_REWORK") || !strings.Contains(err.Error(), who+" asked for "+rework) ||
			!strings.Contains(err.Error(), "another finance user submits") {
			t.Fatalf("%s submits: %v", who, err)
		}
	}
	if p := d.pool(); len(p.Apportionments) != 0 {
		t.Fatalf("a refused submission was written: %+v", p.Apportionments)
	}
	if _, err = d.submitBy("fin-3", "fin-sub-3", pre.Version); err != nil {
		t.Fatalf("a third person: %v", err)
	}
	if p := d.pool(); len(p.Apportionments) != 1 || p.Apportionments[0].By != "fin-3" {
		t.Fatalf("the third person's apportionment: %+v", p.Apportionments)
	}
}

// TestAHouseTransferredAfterAReworkIsAFormerHouseOfItsConsolidationWithEveryContext: on box A, H03 is unloaded in a
// rework asked by ops-1, the container is sealed again and H03 is transferred to C-2. C-1's basis lists it with the
// rework, the person who asked for it, the target and the time; a preview made before the transfer is stale and says it
// read a house that left.
func TestAHouseTransferredAfterAReworkIsAFormerHouseOfItsConsolidationWithEveryContext(t *testing.T) {
	d := boxA(t, "cargo-box-a-former-")
	if b := d.basis(); len(b.FormerHouses) != 0 || len(b.GivenUpStuffings) != 0 || len(b.Houses) != 3 {
		t.Fatalf("before the rework: %+v", b)
	}
	d.reworkBy("rework-1", "ops-1", 22)
	d.unloadForRework("H03", 23)
	d.resealAt("reseal-1", 24)
	kept, err := d.previewBy("ops-2", "fin-pre-kept")
	d.must("the preview made before the transfer", err)

	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	departs := d.at.Add(72 * time.Hour)
	_, err = d.phy.CreateConsolidation(d.ctx, phyapi.CreateConsolidation{CommandID: "consol-2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", Mode: phyapi.ModeSea,
		Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"), PlannedDeparture: departs.Add(6 * time.Hour),
		HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: d.hours(8)})
	d.must("create C-2", err)
	_, err = d.phy.PlanLoadUnit(d.ctx, phyapi.PlanLoadUnit{CommandID: "plan-c2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", ExpectedRevision: 1,
		Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: d.hours(8)})
	d.must("plan the unit of C-2", err)
	_, err = d.phy.TransferHouse(d.ctx, phyapi.TransferHouse{CommandID: "transfer-h03", TenantID: d.tenant, ActorID: "ops-9", ConsolidationID: "C-1", ToConsolidationID: "C-2",
		ShipmentID: "SH-H03", ExpectedMembershipRevision: 1, Reason: phyapi.ReasonWrongConsol, OccurredAt: d.hours(26)})
	d.must("transfer H03", err)

	b := d.basis()
	if len(b.Houses) != 2 || len(b.FormerHouses) != 1 || len(b.GivenUpStuffings) != 0 || len(b.PackingLists) != 1 {
		t.Fatalf("C-1: houses %+v, former %+v", b.Houses, b.FormerHouses)
	}
	h := b.FormerHouses[0]
	want := phyapi.FormerHouse{ShipmentID: "SH-H03", End: phyapi.FormerTransferred, TransferredTo: "C-2", Reason: phyapi.ReasonWrongConsol, EndedBy: "ops-9", EndedAt: d.hours(26),
		EndedEnteredAt: h.EndedEnteredAt, ReworkFrom: phyapi.ReworkFromEntry, ReworkExecutionID: "X-1", ReworkNo: 1, ReworkUnitNo: 1, ReworkAskedBy: "ops-1"}
	if h != want || !h.EndedEnteredAt.After(h.EndedAt) {
		t.Fatalf("the former house %+v, want %+v", h, want)
	}
	c2, err := d.phy.GetPhysicalCostBasis(d.ctx, phyapi.GetPhysicalCostBasis{TenantID: d.tenant, ConsolidationID: "C-2"})
	d.must("C-2's basis", err)
	if len(c2.Houses) != 1 || c2.Houses[0].ShipmentID != "SH-H03" || len(c2.FormerHouses) != 0 {
		t.Fatalf("C-2: %+v", c2)
	}

	_, err = d.submitBy("fin-1", "fin-sub-kept", kept.Version)
	if err == nil || !strings.Contains(err.Error(), "FIN_PREVIEW_STALE") || !strings.Contains(err.Error(), `former."SH-H03"`) {
		t.Fatalf("the preview made before the transfer: %v", err)
	}
	fresh, err := d.previewBy("ops-2", "fin-pre-fresh")
	d.must("a preview after the transfer", err)
	if _, err = d.submitBy("fin-1", "fin-sub-fresh", fresh.Version); err != nil {
		t.Fatalf("a submission after the transfer: %v", err)
	}
}
