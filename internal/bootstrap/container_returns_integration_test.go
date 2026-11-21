//go:build integration

package bootstrap

import (
	"errors"
	"strings"
	"testing"

	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.89: a container taken back out of the terminal, with the real ORG, RATE, COM, FUL, PHY, BOOK and SEA, on the
// day of reworkDay (sea_follows_rework_integration_test.go): the owner's box A, three houses H01, H02 and H03 of 10, 15
// and 5 W/M in unit 1 of C-1; SEA takes the container as stuffed with PHY's first seal SEAL-X-1 at 18 and a VGM at 19;
// PHY opens the rework at 22, unloads H03 at 24, seals again at 25 (SEAL-X-1-NEW) and detaches H03 at 26.

// eventAt records a container fact that happened at one hour, recorded at another.
func (d *reworkDay) eventAt(command, kind, reference string, happened, recorded int) error {
	d.t.Helper()
	r, err := d.sea.RecordContainerEvent(d.ctx, seaapi.RecordContainerEvent{CommandID: command, TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: d.visit,
		Kind: kind, Location: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHG"}, HappenedAt: d.hours(happened), Source: "terminal", Reference: reference, OccurredAt: d.hours(recorded)})
	if err == nil {
		d.visit = r.NewRevision
	}
	return err
}

func (d *reworkDay) facts() seaapi.OceanDocFacts {
	d.t.Helper()
	facts, err := d.sea.GetOceanDocFacts(d.ctx, seaapi.GetOceanDocFacts{TenantID: d.tenant, MovementID: "OM-1"})
	d.must("the facts", err)
	return facts
}

// TestAContainerGatedInBeforeTheReworkIsTakenBackWithEveryContext is the owner's box A: the container gated in at 20;
// PHY opens it for rework, unloads H03 and seals it again; finalize is refused with F3, which names the way out; the
// return is recorded (dated 21, before the rework), the container takes a new VGM and gates in again; finalize
// publishes the new seal, H01 and H02 and the new VGM. The order is that of the V2.88 case: SEA reads the rework (the
// refreshes and the end of H03's association) before the container's return is recorded, because F3 is told only once
// SEA has the seal PHY sealed again.
func TestAContainerGatedInBeforeTheReworkIsTakenBackWithEveryContext(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-return-")
	d.stuffedWithTheFirstSeal()
	d.event("gate-in", seaapi.EventGateIn, "GIN-1", 20)
	d.wantPending("gated in, before the rework", "")
	d.openRework()
	d.wantPending("the rework opened", "FUL_BEHIND_PHY, FUL_BEHIND_PHY, FUL_BEHIND_PHY")
	d.must("FUL records the rework", d.fulTakes(23, "SH-H01", "SH-H02", "SH-H03"))
	d.wantPending("FUL read the rework", "HOUSE_STALE, HOUSE_STALE, HOUSE_STALE")
	d.unloadH03()
	d.sealAgain()
	d.detachH03()
	d.wantPending("PHY sealed again and detached H03", "FUL_BEHIND_PHY, FUL_BEHIND_PHY, HOUSE_STALE")
	d.fulTakesTheDetachment(27)
	d.must("FUL records the resealed evidence", d.fulTakes(28, "SH-H01", "SH-H02", "SH-H03"))
	d.wantPending("FUL read the reseal and the detachment", "HOUSE_STALE, HOUSE_STALE, HOUSE_NOT_REPORTED")
	d.endH03(29)
	d.wantPending("H03's association ended", "HOUSE_STALE, HOUSE_STALE")
	d.must("refresh H01", d.refresh("SH-H01", 30))
	d.must("refresh H02", d.refresh("SH-H02", 30))
	v := d.cv()
	if sealList(v) != "SEAL-X-1/SHIPPER SEAL-X-1-NEW/SHIPPER" || v.Status != seaapi.VisitGatedIn {
		t.Fatalf("the new seal is followed on the gated-in container: %s %s", sealList(v), v.Status)
	}
	d.visit = v.Revision
	d.wantPending("both houses refreshed", "GATED_IN_BEFORE_RESEAL, VGM_BEFORE_RESEAL")
	refusedWith(t, "F3", d.finalize("fin-f3", 31), "was at the terminal when PHY sealed it again", "RecordContainerEvent GATE_OUT_TERMINAL", "assert a new VGM", "gate it in again")
	if err := d.vgm("vgm-too-early", 2950, 31); err == nil {
		t.Fatal("a VGM on a gated-in container is refused")
	}
	// the return, dated 21 (before PHY opened the rework at 22), recorded at 31
	if err := d.eventAt("gate-out", seaapi.EventGateOut, "GOUT-1", 21, 31); err != nil {
		t.Fatal(err)
	}
	if v = d.cv(); v.Status != seaapi.VisitVGM || len(v.Assertions) != 1 || v.Events[0].Kind != seaapi.EventGateOut || v.Events[0].EventNo != 2 {
		t.Fatalf("the return: %+v", v)
	}
	d.wantPending("the container is back, its VGM is old", "VGM_BEFORE_RESEAL")
	refusedWith(t, "F4", d.finalize("fin-f4", 32), "no VGM submitted after PHY sealed it", "assert a new one")
	d.must("VGM v2", d.vgm("vgm-2", 2950, 32))
	d.wantPending("a new VGM", "")
	d.event("gate-in-2", seaapi.EventGateIn, "GIN-2", 33)
	if v = d.cv(); v.Status != seaapi.VisitGatedIn || len(v.Assertions) != 2 {
		t.Fatalf("gated in again: %+v", v)
	}
	d.wantPending("gated in again", "")
	d.must("finalize", d.finalize("fin-1", 34))
	facts := d.facts()
	c := facts.Containers[0]
	if facts.Version != 1 || facts.Status != seaapi.DocFactsFinal || len(facts.Containers) != 1 || len(facts.Houses) != 2 || c.SealNumber != "SEAL-X-1-NEW" ||
		c.SealParty != seaapi.SealShipper || c.VGMAssertionID != "CV-1:2" || c.VGMGrams != 2_950_000 || c.Packs != 12 || c.GrossWeightGrams != 612_000 ||
		facts.Houses[0].ShipmentID != "SH-H01" || facts.Houses[0].Packs != 6 || facts.Houses[0].GrossWeightGrams != 312_000 ||
		facts.Houses[1].ShipmentID != "SH-H02" || facts.Houses[1].Packs != 6 || facts.Houses[1].GrossWeightGrams != 300_000 {
		t.Fatalf("the published facts: %+v", facts)
	}
}

// TestAContainerLoadedBeforeTheReworkStaysRefusedWithEveryContext: a container already loaded on board is not taken
// back by SEA. Finalize says that it is handled outside the system, a return is refused, and so is a new VGM.
func TestAContainerLoadedBeforeTheReworkStaysRefusedWithEveryContext(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-return2-")
	d.stuffedWithTheFirstSeal()
	d.event("gate-in", seaapi.EventGateIn, "GIN-1", 20)
	d.event("loaded", seaapi.EventLoaded, "LOA-1", 21)
	d.openRework()
	d.must("FUL records the rework", d.fulTakes(23, "SH-H01", "SH-H02", "SH-H03"))
	d.reworkedAndRead()
	v := d.cv()
	if sealList(v) != "SEAL-X-1/SHIPPER SEAL-X-1-NEW/SHIPPER" || v.Status != seaapi.VisitLoaded {
		t.Fatalf("the new seal is followed on the loaded container: %s %s", sealList(v), v.Status)
	}
	d.visit = v.Revision
	d.wantPending("loaded, resealed", "GATED_IN_BEFORE_RESEAL, VGM_BEFORE_RESEAL")
	item := d.pendingItem(seaapi.PendingGatedBeforeReseal)
	refusedWith(t, "the pending item", errors.New(item.Message), "was loaded before PHY sealed it again", "by its loading", "handled outside the system")
	refusedWith(t, "F3", d.finalize("fin-f3", 31), "was loaded before PHY sealed it again", "handled outside the system")
	refusedWith(t, "a return of a loaded container", d.eventAt("gate-out", seaapi.EventGateOut, "GOUT-1", 21, 31), "invalid container event", "handled outside the system")
	if err := d.vgm("vgm-2", 2950, 31); err == nil {
		t.Fatal("a VGM on a loaded container is refused")
	}
	if got := d.cv(); got.Status != seaapi.VisitLoaded || got.Revision != v.Revision || len(got.Events) != 2 {
		t.Fatalf("nothing was written: %+v", got)
	}
	if m := d.movement(); m.State != seaapi.StateOpen || m.DocFactsVersion != 0 {
		t.Fatalf("the movement stays OPEN: %+v", m)
	}
}

// TestAReturnOnAFinalMovementKeepsItFinalWithEveryContext: the container gated in, the movement finalized (version 1,
// three houses); the return is recorded on the FINAL movement, which stays FINAL with its facts as they were; what
// follows PHY's rework reopens it as V2.88 does (SEA ends H03's association: version 1 is SUPERSEDED), and after the
// new VGM and the gate-in again, version 2 is published.
func TestAReturnOnAFinalMovementKeepsItFinalWithEveryContext(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-return3-")
	d.stuffedWithTheFirstSeal()
	d.event("gate-in", seaapi.EventGateIn, "GIN-1", 20)
	d.must("finalize", d.finalize("fin-1", 21))
	first := d.facts()
	if first.Version != 1 || first.Status != seaapi.DocFactsFinal || len(first.Houses) != 3 {
		t.Fatalf("version 1: %+v", first)
	}
	if err := d.eventAt("gate-out", seaapi.EventGateOut, "GOUT-1", 21, 22); err != nil {
		t.Fatal(err)
	}
	if v := d.cv(); v.Status != seaapi.VisitVGM || len(v.Events) != 2 {
		t.Fatalf("the return: %+v", v)
	}
	if m, again := d.movement(), d.facts(); m.State != seaapi.StateFinal || m.DocFactsVersion != 1 || again.Version != 1 || again.Status != seaapi.DocFactsFinal || again.Containers[0].SealNumber != "SEAL-X-1" {
		t.Fatalf("the movement stays FINAL with version 1: %+v %+v", m, again)
	}
	d.wantPending("returned on a final movement", "")
	// PHY's rework, read by FUL and by SEA; the end of H03's association reopens the movement
	d.openRework()
	d.must("FUL records the rework", d.fulTakes(23, "SH-H01", "SH-H02", "SH-H03"))
	d.unloadH03()
	d.sealAgain()
	d.detachH03()
	d.fulTakesTheDetachment(27)
	d.must("FUL records the resealed evidence", d.fulTakes(28, "SH-H01", "SH-H02", "SH-H03"))
	d.endH03(29)
	if m, old := d.movement(), d.facts(); m.State != seaapi.StateOpen || old.Version != 1 || old.Status != seaapi.DocFactsSuperseded {
		t.Fatalf("the rework reopens: %+v %+v", m, old)
	}
	d.must("refresh H01", d.refresh("SH-H01", 30))
	d.must("refresh H02", d.refresh("SH-H02", 30))
	d.visit = d.cv().Revision
	d.wantPending("both houses refreshed", "VGM_BEFORE_RESEAL")
	d.must("VGM v2", d.vgm("vgm-2", 2950, 31))
	d.event("gate-in-2", seaapi.EventGateIn, "GIN-2", 32)
	d.wantPending("a new VGM, gated in again", "")
	d.must("finalize again", d.finalize("fin-2", 33))
	facts := d.facts()
	if c := facts.Containers[0]; facts.Version != 2 || facts.Status != seaapi.DocFactsFinal || len(facts.Houses) != 2 || c.SealNumber != "SEAL-X-1-NEW" || c.VGMAssertionID != "CV-1:2" || c.Packs != 12 {
		t.Fatalf("version 2: %+v", facts)
	}
}

// TestACarrierReportNamingNoContainerWithOneContainerWithEveryContext: the carrier reports a loading of the master
// bill, with no container, at 21 (before PHY's reseal at 25). The movement has one live container, so the report can
// only be about it: it counts as naming it, and finalize is refused with the outside-the-system message, as for a named
// report. A second container assigned later makes the report a warning. (A two-container movement from the start is
// not built here: the day has one unit.)
func TestACarrierReportNamingNoContainerWithOneContainerWithEveryContext(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-return4-")
	d.stuffedWithTheFirstSeal()
	d.event("gate-in", seaapi.EventGateIn, "GIN-1", 20)
	_, err := d.sea.RecordOceanCarrierFact(d.ctx, seaapi.RecordOceanCarrierFact{CommandID: "carrier-load", TenantID: d.tenant, ActorID: "line-1", MovementID: "OM-1",
		ExpectedRevision: d.movement().Revision, Kind: seaapi.CarrierLoaded, BillNumber: "LINU2611000457", Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line",
		Reference: "LD-1", SHA256: "9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c", HappenedAt: d.hours(21), OccurredAt: d.hours(21)})
	d.must("the carrier's report", err)
	d.openRework()
	d.must("FUL records the rework", d.fulTakes(23, "SH-H01", "SH-H02", "SH-H03"))
	d.reworkedAndRead()
	d.visit = d.cv().Revision
	d.wantPending("a report naming no container, one container", "GATED_IN_BEFORE_RESEAL, VGM_BEFORE_RESEAL")
	// the container's own events come first: it was at the terminal at the reseal
	refusedWith(t, "F3", d.finalize("fin-f3", 31), "was at the terminal when PHY sealed it again", "RecordContainerEvent GATE_OUT_TERMINAL")
	if err = d.eventAt("gate-out", seaapi.EventGateOut, "GOUT-1", 21, 31); err != nil {
		t.Fatal(err)
	}
	// with its return recorded, SEA's records show no loading and no stay at the terminal: the carrier's report
	// conflicts
	d.wantPending("the return recorded", "GATED_IN_BEFORE_RESEAL, VGM_BEFORE_RESEAL")
	refusedWith(t, "the conflict", d.finalize("fin-f3b", 32), "the carrier's report of a loading", "naming no container, while this is the movement's only live container",
		"AcceptCarrierLoadingConflict", "handled outside the system")
	if m := d.movement(); m.State != seaapi.StateOpen || m.DocFactsVersion != 0 {
		t.Fatalf("the movement stays OPEN: %+v", m)
	}
	// a second live container assigned later: from then on the report only warns
	_, err = d.sea.AssignContainer(d.ctx, seaapi.AssignContainer{CommandID: "assign-2", TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-2", MovementID: "OM-1", LoadUnitNo: 2,
		ContainerNumber: "temu1234565", SizeType: "22G1", TareKg: 2180, MaxGrossKg: 30480, PlateSource: seaapi.PlateCSC, OccurredAt: d.hours(32)})
	d.must("a second container", err)
	item := d.pendingItem(seaapi.WarningCarrierLoadingBeforeReseal)
	if item.Blocking || item.SubjectID != "CSQU3054383" {
		t.Fatalf("the warning: %+v", item)
	}
	// the conflict item is gone with the second container: the report only warns
	if got := d.pending(); strings.Contains(got, "GATED_IN_BEFORE_RESEAL") {
		t.Fatalf("with two live containers the report only warns: %s", got)
	}
}

// TestAConflictWithTheCarriersReportIsReleasedWithEveryContext is the reviewer's K1 case: one container, never gated
// in; the carrier reports at 21 a loading of the master bill without naming a container; PHY reworks and seals again at
// 25. The report can only be about the movement's one container, so finalize is refused with the conflict message and
// the ways out; the operator, having checked, releases it with a reason, recorded with who and when; finalize then
// publishes the new seal, H01 and H02 and the new VGM.
func TestAConflictWithTheCarriersReportIsReleasedWithEveryContext(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-conflict-")
	d.stuffedWithTheFirstSeal()
	_, err := d.sea.RecordOceanCarrierFact(d.ctx, seaapi.RecordOceanCarrierFact{CommandID: "carrier-load", TenantID: d.tenant, ActorID: "line-1", MovementID: "OM-1",
		ExpectedRevision: d.movement().Revision, Kind: seaapi.CarrierLoaded, BillNumber: "LINU2611000457", Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line",
		Reference: "LD-1", SHA256: "9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c", HappenedAt: d.hours(21), OccurredAt: d.hours(21)})
	d.must("the carrier's report", err)
	d.openRework()
	d.must("FUL records the rework", d.fulTakes(23, "SH-H01", "SH-H02", "SH-H03"))
	d.reworkedAndRead()
	d.visit = d.cv().Revision
	d.must("VGM v2", d.vgm("vgm-2", 2950, 31))
	d.wantPending("the conflict", "GATED_IN_BEFORE_RESEAL")
	refusedWith(t, "finalize", d.finalize("fin-conflict", 32), "the carrier's report of a loading", "naming no container, while this is the movement's only live container",
		"release the conflict with a reason (AcceptCarrierLoadingConflict)", "handled outside the system")
	accept := func(command, reason string, hour int) (seaapi.Receipt, error) {
		return d.sea.AcceptCarrierLoadingConflict(d.ctx, seaapi.AcceptCarrierLoadingConflict{CommandID: command, TenantID: d.tenant, ActorID: "ops-9", VisitID: "CV-1",
			ExpectedRevision: d.visit, Reason: reason, OccurredAt: d.hours(hour)})
	}
	if _, err = accept("acc-0", "", 33); err == nil {
		t.Fatal("a release without a reason is refused")
	}
	r, err := accept("acc-1", "the line's agent confirmed that the box was still at the CFS", 33)
	d.must("the release", err)
	d.visit = r.NewRevision
	if r.ReleaseNo != 1 {
		t.Fatalf("the receipt: %+v", r)
	}
	v := d.cv()
	if len(v.Releases) != 1 || v.Releases[0].RecordedBy != "ops-9" || !v.Releases[0].RecordedAt.Equal(d.hours(33)) || v.Releases[0].PHYSeal != "SEAL-X-1-NEW" ||
		v.Releases[0].Reason != "the line's agent confirmed that the box was still at the CFS" {
		t.Fatalf("the release is kept: %+v", v.Releases)
	}
	d.wantPending("released", "CARRIER_LOADING_CONFLICT_RELEASED (warning)")
	if m := d.movement(); m.State != seaapi.StateOpen || m.DocFactsVersion != 0 {
		t.Fatalf("the release changes nothing of the movement: %+v", m)
	}
	d.must("finalize", d.finalize("fin-1", 34))
	facts := d.facts()
	c := facts.Containers[0]
	if facts.Version != 1 || facts.Status != seaapi.DocFactsFinal || len(facts.Houses) != 2 || c.SealNumber != "SEAL-X-1-NEW" || c.VGMAssertionID != "CV-1:2" || c.Packs != 12 || c.GrossWeightGrams != 612_000 {
		t.Fatalf("the published facts: %+v", facts)
	}
}
