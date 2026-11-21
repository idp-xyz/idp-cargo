//go:build integration

package bootstrap

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/book"
	"go.idp.xyz/idp-cargo/internal/contexts/com"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	fulmodule "go.idp.xyz/idp-cargo/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/contexts/org"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/contexts/rate"
	seamodule "go.idp.xyz/idp-cargo/internal/contexts/sea"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.87: SEA follows a container's rework, with the real ORG, RATE, COM, FUL, PHY, BOOK and SEA (and DOC in the last
// case). The owner's box A: three houses H01, H02 and H03 of 10, 15 and 5 W/M in unit 1 of C-1 (PHY's stuffing X-1,
// sealed SEAL-X-1 at hour 15); SEA takes the container as stuffed with that seal and a VGM; PHY opens X-1 for rework at
// hour 22, unloads H03 at 24, seals again with SEAL-X-1-NEW at 25 and detaches H03 at 26. FUL and SEA read PHY only
// when they are made to, by hand, as ever.

// reworkDay is the day with SEA, FUL and PHY over the same database.
type reworkDay struct {
	*oceanDay
	platform *Platform
	org      *org.Module
	rate     *rate.Module
	com      *com.Module
	book     *book.Module
	ful      *fulmodule.Module
	sea      *seamodule.Module
	visit    int64 // CV-1's revision after the last command that named it
}

// newReworkDay builds the day to the first seal in PHY, with SEA's movement OM-1 opened at hour 11, the three houses
// associated (nothing of PHY's stuffing is read yet) and CV-1 assigned to unit 1 at hour 12.
func newReworkDay(t *testing.T, prefix string) *reworkDay {
	t.Helper()
	ctx := context.Background()
	d := sealedDay(t, prefix)
	platform := openPlatform(t)
	orgModule, err := platform.ORG(allowAll{})
	d.must("org", err)
	rateModule, err := platform.RATE(orgModule, allowAll{}, marginFloor(2000))
	d.must("rate", err)
	comModule, err := platform.COM(orgModule, rateModule, allowAll{})
	d.must("com", err)
	fulModule, _, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	d.must("ful", err)
	bookModule, err := platform.BOOK(orgModule, allowAll{})
	d.must("book", err)
	seaModule, err := platform.SEA(bookModule, fulModule, allowAll{})
	d.must("sea", err)
	rd := &reworkDay{oceanDay: d, platform: platform, org: orgModule, rate: rateModule, com: comModule, book: bookModule, ful: fulModule, sea: seaModule}
	contexts := slice{tenant: d.tenant, at: d.at, departs: d.at.Add(72 * time.Hour)}
	contexts.bookSea(t, bookModule, "BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", d.at.Add(78*time.Hour))
	_, err = seaModule.OpenOceanMovement(ctx, seaapi.OpenOceanMovement{CommandID: "open-1", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1", ConsolidationID: "C-1",
		BookingID: "BK-1", OccurredAt: d.hours(11)})
	d.must("open movement", err)
	_, err = seaModule.RegisterHblSeries(ctx, seaapi.RegisterHblSeries{CommandID: "series", TenantID: d.tenant, ActorID: "ops-1", LegalEntityID: "LE", Template: "SHAL{seq:7}",
		FirstNumber: 1, OccurredAt: d.hours(11)})
	d.must("series", err)
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		_, err = seaModule.AssociateHouseBill(ctx, seaapi.AssociateHouseBill{CommandID: "hbl-" + s, TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1",
			ExpectedRevision: rd.movement().Revision, ShipmentID: s, OccurredAt: d.hours(11)})
		d.must("associate "+s, err)
	}
	r, err := seaModule.AssignContainer(ctx, seaapi.AssignContainer{CommandID: "assign", TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-1", MovementID: "OM-1", LoadUnitNo: 1,
		ContainerNumber: "csqu3054383", SizeType: "22G1", TareKg: 2180, MaxGrossKg: 30480, PlateSource: seaapi.PlateCSC, OccurredAt: d.hours(12)})
	d.must("assign", err)
	rd.visit = r.NewRevision
	return rd
}

func (d *reworkDay) movement() seaapi.OceanMovement {
	d.t.Helper()
	m, err := d.sea.GetOceanMovement(d.ctx, seaapi.GetOceanMovement{TenantID: d.tenant, MovementID: "OM-1"})
	d.must("movement", err)
	return m
}

func (d *reworkDay) cv() seaapi.ContainerVisit {
	d.t.Helper()
	v, err := d.sea.GetContainerVisit(d.ctx, seaapi.GetContainerVisit{TenantID: d.tenant, VisitID: "CV-1"})
	d.must("visit", err)
	return v
}

// fulTakes asks FUL to record PHY's evidence of the shipments at the hour, by hand as ever; it returns the first
// error.
func (d *reworkDay) fulTakes(hour int, shipments ...string) error {
	d.t.Helper()
	for _, s := range shipments {
		p, err := d.ful.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: s})
		d.must("FUL's plan of "+s, err)
		if _, err = d.ful.RecordVerifiedLoadEvidence(d.ctx, fulapi.RecordVerifiedLoadEvidence{CommandID: "ful-ev-" + s + "-" + string(rune('a'+hour)), TenantID: d.tenant,
			ActorID: "ops-1", ShipmentID: s, ExpectedRevision: p.Revision, OccurredAt: d.hours(hour)}); err != nil {
			return err
		}
	}
	return nil
}

// due is FUL's answer to whether it would record a new evidence version of the shipment now.
func (d *reworkDay) due(shipment string) bool {
	d.t.Helper()
	c, err := d.ful.GetOceanEvidenceCurrency(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: shipment})
	d.must("FUL's evidence currency of "+shipment, err)
	return c.NewVersionDue
}

func (d *reworkDay) refresh(shipment string, hour int) error {
	d.t.Helper()
	_, err := d.sea.RefreshHouseBillFacts(d.ctx, seaapi.RefreshHouseBillFacts{CommandID: "refresh-" + shipment + "-" + string(rune('a'+hour)), TenantID: d.tenant, ActorID: "ops-1",
		MovementID: "OM-1", ExpectedRevision: d.movement().Revision, ShipmentID: shipment, OccurredAt: d.hours(hour)})
	return err
}

func (d *reworkDay) finalize(command string, hour int) error {
	d.t.Helper()
	_, err := d.sea.FinalizeOceanDocFacts(d.ctx, seaapi.FinalizeOceanDocFacts{CommandID: command, TenantID: d.tenant, ActorID: "ops-2", MovementID: "OM-1",
		ExpectedRevision: d.movement().Revision, OccurredAt: d.hours(hour)})
	return err
}

func (d *reworkDay) stuffingDone(command string, hour int) error {
	d.t.Helper()
	r, err := d.sea.RecordStuffingDone(d.ctx, seaapi.RecordStuffingDone{CommandID: command, TenantID: d.tenant, ActorID: "cfs-1", VisitID: "CV-1", ExpectedRevision: d.visit,
		OccurredAt: d.hours(hour)})
	if err == nil {
		d.visit = r.NewRevision
	}
	return err
}

func (d *reworkDay) vgm(command string, kilos int64, hour int) error {
	d.t.Helper()
	r, err := d.sea.AssertVgm(d.ctx, seaapi.AssertVgm{CommandID: command, TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: d.visit,
		Mass: kernel.Quantity{Unit: "KGM", Amount: kilos}, Method: seaapi.VgmMethod2, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1, DeterminedAt: d.hours(hour),
		Signatory: "Ada", OccurredAt: d.hours(hour)})
	if err == nil {
		d.visit = r.NewRevision
	}
	return err
}

func (d *reworkDay) seal(command, number, party string, applied, hour int) error {
	d.t.Helper()
	r, err := d.sea.RecordContainerSeal(d.ctx, seaapi.RecordContainerSeal{CommandID: command, TenantID: d.tenant, ActorID: "cfs-1", VisitID: "CV-1", ExpectedRevision: d.visit,
		Number: number, Party: party, AppliedAt: d.hours(applied), Source: "tally sheet", OccurredAt: d.hours(hour)})
	if err == nil {
		d.visit = r.NewRevision
	}
	return err
}

func (d *reworkDay) event(command, kind, reference string, hour int) {
	d.t.Helper()
	r, err := d.sea.RecordContainerEvent(d.ctx, seaapi.RecordContainerEvent{CommandID: command, TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: d.visit,
		Kind: kind, Location: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHG"}, HappenedAt: d.hours(hour), Source: "terminal", Reference: reference, OccurredAt: d.hours(hour)})
	d.must(kind, err)
	d.visit = r.NewRevision
}

func (d *reworkDay) masterBill(hour int) {
	d.t.Helper()
	_, err := d.sea.RecordMasterBill(d.ctx, seaapi.RecordMasterBill{CommandID: "mbl", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1", ExpectedRevision: d.movement().Revision,
		Number: "linu2611000457", Kind: seaapi.MasterBillCarrierMBL, Evidence: seaapi.Evidence{SHA256: strings.Repeat("9c", 32), StorageID: "mail/mbl.pdf", MediaType: "application/pdf",
			CapturedAt: d.hours(hour)}, OccurredAt: d.hours(hour)})
	d.must("master bill", err)
}

// SEA takes the container as stuffed with PHY's first seal: FUL takes the evidence of the three houses at hour 16, SEA
// refreshes them at 17, the container's seal SEAL-X-1 is recorded at 17, the stuffing taken at 18 and a VGM v1 of 2,900
// kg submitted at 19; the master bill reference is recorded at 18.
func (d *reworkDay) stuffedWithTheFirstSeal() {
	d.t.Helper()
	d.must("FUL takes the first evidence", d.fulTakes(16, "SH-H01", "SH-H02", "SH-H03"))
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		d.must("refresh "+s, d.refresh(s, 17))
	}
	d.must("SEA's seal", d.seal("sea-seal", "seal-x-1", seaapi.SealShipper, 15, 17))
	d.must("stuffing done", d.stuffingDone("stuffed", 18))
	d.masterBill(18)
	d.must("VGM v1", d.vgm("vgm-1", 2900, 19))
}

// PHY's rework, one step at a time: it opens at 22, unloads H03 at 24, seals again at 25 and detaches H03 at 26.
func (d *reworkDay) openRework() {
	d.t.Helper()
	_, err := d.phy.OpenStuffingForRework(d.ctx, phyapi.OpenStuffingForRework{CommandID: "rework-1", TenantID: d.tenant, ActorID: "ops-asker", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Reason: "the consignee of H03 withdrew its cargo", OccurredAt: d.hours(22)})
	d.must("open the rework", err)
	d.revision["X-1"]++
}

func (d *reworkDay) unloadH03() {
	d.t.Helper()
	_, err := d.phy.UnloadHouseForRework(d.ctx, phyapi.UnloadHouseForRework{CommandID: "unload-h03", TenantID: d.tenant, ActorID: "wh-1", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], ShipmentID: "SH-H03", Evidence: phyapi.Evidence{Source: "CFS-WMS", Reference: "UNSTUFF-H03", OccurredAt: d.hours(24)}, OccurredAt: d.hours(24)})
	d.must("unload H03", err)
	d.revision["X-1"]++
}

func (d *reworkDay) sealAgain() {
	d.t.Helper()
	_, err := d.phy.CloseLoadExecution(d.ctx, phyapi.CloseLoadExecution{CommandID: "reseal-1", TenantID: d.tenant, ActorID: "cfs-2", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Evidence: d.evidence(phyapi.EvidenceSeal, "SEAL-X-1-NEW", 25), OccurredAt: d.hours(25)})
	d.must("seal again", err)
	d.revision["X-1"]++
}

func (d *reworkDay) detachH03() {
	d.t.Helper()
	_, err := d.phy.DetachHouse(d.ctx, phyapi.DetachHouse{CommandID: "detach-h03", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: "SH-H03",
		ExpectedMembershipRevision: 1, Reason: phyapi.ReasonSpaceShortage, OccurredAt: d.hours(26)})
	d.must("detach H03", err)
}

// fulTakesTheDetachment is FUL recording PHY's membership of H03: attached to no consolidation any more.
func (d *reworkDay) fulTakesTheDetachment(hour int) {
	d.t.Helper()
	p, err := d.ful.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: "SH-H03"})
	d.must("FUL's plan of H03", err)
	_, err = d.ful.RecordConsolDetachment(d.ctx, fulapi.RecordConsolDetachment{CommandID: "ful-detach", TenantID: d.tenant, ActorID: "ops-1", ShipmentID: "SH-H03",
		ExpectedRevision: p.Revision, ConsolidationID: "C-1", OccurredAt: d.hours(hour)})
	d.must("FUL records the detachment", err)
}

func (d *reworkDay) endH03(hour int) {
	d.t.Helper()
	_, err := d.sea.EndHouseBillAssociation(d.ctx, seaapi.EndHouseBillAssociation{CommandID: "end-h03", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: d.movement().Revision, ShipmentID: "SH-H03", OccurredAt: d.hours(hour)})
	d.must("end the association of H03", err)
}

func refusedWith(t *testing.T, label string, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: accepted", label)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Fatalf("%s: %q does not say %q", label, err, p)
		}
	}
}

func sealList(v seaapi.ContainerVisit) string {
	var out []string
	for i := len(v.Seals) - 1; i >= 0; i-- {
		out = append(out, v.Seals[i].Number+"/"+v.Seals[i].Party)
	}
	return strings.Join(out, " ")
}

// TestSEAFollowsAContainersReworkWithEveryContext is case 1: SEA took the container as stuffed with S1 (SEAL-X-1) and a
// VGM v1 before the rework.
func TestSEAFollowsAContainersReworkWithEveryContext(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-follow-")
	d.stuffedWithTheFirstSeal()
	d.wantPending("before the rework", "")
	// FUL's evidence is PHY's current one until PHY changes it (the query FUL answers SEA)
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		if d.due(s) {
			t.Fatalf("FUL has recorded PHY's evidence of %s: nothing is due", s)
		}
	}

	// PHY opens the rework at 22: FUL has read nothing, and SEA refuses to finalize (F0)
	d.openRework()
	d.wantPending("the rework opened, FUL has read nothing", "FUL_BEHIND_PHY, FUL_BEHIND_PHY, FUL_BEHIND_PHY")
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		if !d.due(s) {
			t.Fatalf("PHY opened the rework: FUL would record a new version of %s", s)
		}
	}
	refusedWith(t, "F0 during the rework", d.finalize("fin-f0", 23), "FUL has not recorded PHY's latest facts of", "record them in FUL, and refresh the house if SEA then reports it STALE_HOUSE")
	if m := d.movement(); m.State != seaapi.StateOpen || m.DocFactsVersion != 0 {
		t.Fatalf("the movement stays OPEN: %+v", m)
	}
	// FUL records the evidence of the rework; the houses SEA froze are not what FUL reports now: STALE_HOUSE
	d.must("FUL records the rework", d.fulTakes(23, "SH-H01", "SH-H02", "SH-H03"))
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		if d.due(s) {
			t.Fatalf("FUL recorded the rework: %s is current", s)
		}
	}
	refusedWith(t, "STALE_HOUSE after FUL read the rework", d.finalize("fin-stale", 23), "STALE_HOUSE")
	d.wantPending("FUL read the rework", "HOUSE_STALE, HOUSE_STALE, HOUSE_STALE")

	// PHY unloads H03, seals again and detaches H03
	d.unloadH03()
	d.sealAgain()
	d.detachH03()
	d.wantPending("PHY sealed again and detached H03, FUL has read nothing of it", "FUL_BEHIND_PHY, FUL_BEHIND_PHY, HOUSE_STALE")
	// H01 and H02 are tendered again under the new seal; H03 has no packs in the container and reads as it did during
	// the rework (received, no stuffing) until FUL records its detachment, which changes the consolidation it asks PHY
	// for
	if !d.due("SH-H01") || !d.due("SH-H02") || d.due("SH-H03") {
		t.Fatalf("PHY sealed again and detached H03: due H01 %v, H02 %v, H03 %v", d.due("SH-H01"), d.due("SH-H02"), d.due("SH-H03"))
	}
	refusedWith(t, "F0 after the reseal", d.finalize("fin-f0-2", 27), "FUL has not recorded PHY's latest facts of")
	// FUL records the detachment and the evidence of the three houses
	d.fulTakesTheDetachment(27)
	if !d.due("SH-H03") {
		t.Fatal("FUL records H03 in no consolidation: its evidence is due again")
	}
	d.must("FUL records the resealed evidence", d.fulTakes(28, "SH-H01", "SH-H02", "SH-H03"))
	if d.due("SH-H01") || d.due("SH-H02") || d.due("SH-H03") {
		t.Fatal("FUL recorded everything")
	}
	d.wantPending("FUL read the reseal and the detachment", "HOUSE_STALE, HOUSE_STALE, HOUSE_NOT_REPORTED")
	refusedWith(t, "STALE_HOUSE after the reseal", d.finalize("fin-stale-2", 29), "STALE_HOUSE")
	// SEA ends H03's association: the container's seals have not moved yet, the container's stuffing is SEA's S1
	d.endH03(29)
	d.wantPending("H03's association ended", "HOUSE_STALE, HOUSE_STALE")
	before := d.cv()
	if sealList(before) != "SEAL-X-1/SHIPPER" {
		t.Fatalf("seals before the refresh: %s", sealList(before))
	}
	// refreshing H01 records S2 on the visit, followed from PHY's stuffing
	if err := d.refresh("SH-H01", 30); err != nil {
		t.Fatal(err)
	}
	v := d.cv()
	if sealList(v) != "SEAL-X-1/SHIPPER SEAL-X-1-NEW/SHIPPER" || v.Revision != before.Revision+1 {
		t.Fatalf("seals after H01's refresh: %s revision %d (was %d)", sealList(v), v.Revision, before.Revision)
	}
	s2 := v.Seals[0]
	if s2.SealNo != 2 || s2.Party != seaapi.SealShipper || !s2.AppliedAt.Equal(d.hours(25)) || s2.RecordedBy != "ops-1" || !s2.RecordedAt.Equal(d.hours(30)) ||
		!strings.Contains(s2.Source, "followed from PHY's stuffing X-1") {
		t.Fatalf("the followed seal: %+v", s2)
	}
	d.visit = v.Revision
	d.wantPending("H01 refreshed", "HOUSE_STALE, HOUSES_DISAGREE_ON_SEAL")
	// refreshing H02 finds the seal held and records nothing
	if err := d.refresh("SH-H02", 30); err != nil {
		t.Fatal(err)
	}
	if again := d.cv(); sealList(again) != sealList(v) || again.Revision != v.Revision {
		t.Fatalf("H02's refresh recorded a seal: %s revision %d", sealList(again), again.Revision)
	}
	d.wantPending("both houses refreshed", "VGM_BEFORE_RESEAL")
	// the VGM was submitted at 19, before PHY sealed again at 25 (F4)
	refusedWith(t, "F4", d.finalize("fin-f4", 31), "no VGM submitted after PHY sealed it", "assert a new one")
	d.must("VGM v2", d.vgm("vgm-2", 2950, 31))
	d.wantPending("after the new VGM", "")
	d.must("finalize", d.finalize("fin-1", 32))
	facts, err := d.sea.GetOceanDocFacts(d.ctx, seaapi.GetOceanDocFacts{TenantID: d.tenant, MovementID: "OM-1"})
	d.must("the facts", err)
	c := facts.Containers[0]
	if facts.Version != 1 || facts.Status != seaapi.DocFactsFinal || len(facts.Containers) != 1 || len(facts.Houses) != 2 || c.SealNumber != "SEAL-X-1-NEW" ||
		c.SealParty != seaapi.SealShipper || c.VGMAssertionID != "CV-1:2" || c.VGMGrams != 2_950_000 || c.Packs != 12 || c.GrossWeightGrams != 612_000 ||
		facts.Houses[0].ShipmentID != "SH-H01" || facts.Houses[0].Packs != 6 || facts.Houses[0].GrossWeightGrams != 312_000 ||
		facts.Houses[1].ShipmentID != "SH-H02" || facts.Houses[1].Packs != 6 || facts.Houses[1].GrossWeightGrams != 300_000 {
		t.Fatalf("the published facts: %+v", facts)
	}
	// a customs seal afterwards reopens the movement, and the next finalize publishes it
	d.must("customs seal", d.seal("sea-customs", "cus-1", seaapi.SealCustoms, 33, 33))
	if m := d.movement(); m.State != seaapi.StateOpen || m.DocFactsVersion != 1 {
		t.Fatalf("a seal reopens: %+v", m)
	}
	d.must("finalize again", d.finalize("fin-2", 34))
	facts, err = d.sea.GetOceanDocFacts(d.ctx, seaapi.GetOceanDocFacts{TenantID: d.tenant, MovementID: "OM-1"})
	d.must("the second facts", err)
	if c = facts.Containers[0]; facts.Version != 2 || c.SealNumber != "CUS-1" || c.SealParty != seaapi.SealCustoms || c.VGMAssertionID != "CV-1:2" || c.Packs != 12 {
		t.Fatalf("the second version: %+v", facts)
	}
}

// reworkedAndRead runs PHY's rework to its end and has FUL and SEA read it: H03 is unloaded, X-1 sealed again and H03
// detached; FUL takes the detachment at 27 and the evidence of the three houses at 28; SEA ends H03's association at 29
// and refreshes H01 and H02 at 30.
func (d *reworkDay) reworkedAndRead() {
	d.t.Helper()
	d.unloadH03()
	d.sealAgain()
	d.detachH03()
	d.fulTakesTheDetachment(27)
	d.must("FUL records the resealed evidence", d.fulTakes(28, "SH-H01", "SH-H02", "SH-H03"))
	d.endH03(29)
	d.must("refresh H01", d.refresh("SH-H01", 30))
	d.must("refresh H02", d.refresh("SH-H02", 30))
}

// TestAReworkBeforeSEATookTheStuffingWithEveryContext is case 2: PHY's rework happens before SEA's stuffing-done.
// RecordStuffingDone is refused while the rework is open and FUL has not read it, then while SEA has not read FUL,
// then while the houses carry no stuffing; after the reseal and with FUL and SEA having read it, the seal PHY resealed
// with is none of the container's seals, as today, and the container is taken as stuffed once it is recorded.
func TestAReworkBeforeSEATookTheStuffingWithEveryContext(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-follow2-")
	d.must("FUL takes the first evidence", d.fulTakes(16, "SH-H01", "SH-H02", "SH-H03"))
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		d.must("refresh "+s, d.refresh(s, 17))
	}
	d.must("SEA's seal", d.seal("sea-seal", "seal-x-1", seaapi.SealShipper, 15, 17))
	d.masterBill(18)
	d.openRework()
	refusedWith(t, "F0", d.stuffingDone("stuffed-0", 23), "FUL has not recorded PHY's latest facts")
	d.must("FUL records the rework", d.fulTakes(23, "SH-H01", "SH-H02", "SH-H03"))
	refusedWith(t, "STALE_HOUSE", d.stuffingDone("stuffed-1", 23), "STALE_HOUSE")
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		d.must("refresh "+s+" in the rework", d.refresh(s, 23))
	}
	refusedWith(t, "no stuffing while PHY has it open", d.stuffingDone("stuffed-2", 23), "refresh the houses when FUL has newer evidence")
	d.unloadH03()
	d.sealAgain()
	d.detachH03()
	d.fulTakesTheDetachment(27)
	d.must("FUL records the resealed evidence", d.fulTakes(28, "SH-H01", "SH-H02", "SH-H03"))
	d.endH03(29)
	d.must("refresh H01", d.refresh("SH-H01", 30))
	d.must("refresh H02", d.refresh("SH-H02", 30))
	if got := sealList(d.cv()); got != "SEAL-X-1/SHIPPER" {
		t.Fatalf("an ASSIGNED container is not followed: %s", got)
	}
	// as today: PHY's new seal is none of the container's seals
	refusedWith(t, "the new seal is not recorded", d.stuffingDone("stuffed-3", 31), `seal "SEAL-X-1-NEW"`, "none of the container's seals")
	d.must("SEA records PHY's new seal", d.seal("sea-seal-2", "seal-x-1-new", seaapi.SealShipper, 25, 31))
	d.must("stuffing done", d.stuffingDone("stuffed-4", 32))
	if v := d.cv(); v.Status != seaapi.VisitStuffed || v.DifferenceReason != "" {
		t.Fatalf("the container is stuffed: %+v", v)
	}
	d.must("VGM", d.vgm("vgm-1", 2950, 33))
	d.must("finalize", d.finalize("fin-1", 34))
	facts, err := d.sea.GetOceanDocFacts(d.ctx, seaapi.GetOceanDocFacts{TenantID: d.tenant, MovementID: "OM-1"})
	d.must("the facts", err)
	if c := facts.Containers[0]; facts.Version != 1 || len(facts.Houses) != 2 || c.SealNumber != "SEAL-X-1-NEW" || c.VGMAssertionID != "CV-1:1" || c.Packs != 12 || c.GrossWeightGrams != 612_000 {
		t.Fatalf("the facts: %+v", facts)
	}
}

// TestAContainerGatedInBeforeTheReworkIsStoppedWithEveryContext is case 3: the container gated in before PHY opened it.
// SEA follows S2, refuses to finalize with F3's message, and refuses a new VGM as it does on any gated-in container.
func TestAContainerGatedInBeforeTheReworkIsStoppedWithEveryContext(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-follow3-")
	d.stuffedWithTheFirstSeal()
	d.event("gate-in", seaapi.EventGateIn, "GIN-1", 20)
	d.openRework()
	d.must("FUL records the rework", d.fulTakes(23, "SH-H01", "SH-H02", "SH-H03"))
	d.reworkedAndRead()
	v := d.cv()
	if sealList(v) != "SEAL-X-1/SHIPPER SEAL-X-1-NEW/SHIPPER" || v.Status != seaapi.VisitGatedIn {
		t.Fatalf("S2 is followed on the gated-in container: %s %s", sealList(v), v.Status)
	}
	d.visit = v.Revision
	refusedWith(t, "F3", d.finalize("fin-f3", 31), "was at the terminal when PHY sealed it again", "RecordContainerEvent GATE_OUT_TERMINAL")
	if err := d.vgm("vgm-2", 2950, 31); err == nil || !strings.Contains(err.Error(), "invalid state transition") {
		t.Fatalf("a VGM on a gated-in container is refused as today: %v", err)
	}
	if m := d.movement(); m.State != seaapi.StateOpen || m.DocFactsVersion != 0 {
		t.Fatalf("the movement stays OPEN: %+v", m)
	}
}

// withDocuments builds DOC over the same database (with the day's FIN) and returns it, with the shippers and the
// consignee of the houses and the HBL authority recorded at hour 21.
func (d *reworkDay) withDocuments() docModule {
	d.t.Helper()
	air, err := d.platform.AIR(d.book, d.ful, allowAll{})
	d.must("air", err)
	sea := d.sea
	doc, err := d.platform.DOC(air, sea, d.com, d.org, d.fin, allowAll{})
	d.must("doc", err)
	// the parties of the orders: a shipper for each house, one consignee in Hamburg
	parties := []string{"shipper-h01", "shipper-h02", "shipper-h03", "consignee-de"}
	d.seedParties(parties...)
	for _, party := range parties {
		role := "SHIPPER"
		if party == "consignee-de" {
			role = "CONSIGNEE"
		}
		_, err = d.org.ApproveRole(d.ctx, orgapi.ApproveRole{CommandID: "role-" + party, TenantID: d.tenant, ActorID: "org-officer", LegalEntityID: "LE", PartyID: party,
			ExpectedRevision: 1, RoleCode: role, ValidFrom: d.at, EvidenceRef: role + "-agreement", OccurredAt: d.at})
		d.must("role "+party, err)
		_, err = d.org.AddPartyAddress(d.ctx, orgapi.AddPartyAddress{CommandID: "office-" + party, TenantID: d.tenant, ActorID: "org-officer", PartyID: party, AddressID: "HQ-" + party,
			Address:    orgapi.Address{ShortCode: "HQ", Purposes: []string{orgapi.AddressOffice}, Name: "Office of " + party, Lines: []string{"1 Harbour Road"}, City: "Hamburg", CountryCode: "DE"},
			DefaultFor: []string{orgapi.AddressOffice}})
		d.must("office "+party, err)
	}
	_, err = d.org.RecordPartyIdentifier(d.ctx, orgapi.RecordPartyIdentifier{CommandID: "eori-consignee", TenantID: d.tenant, ActorID: "org-officer", PartyID: "consignee-de",
		ExpectedRevision: 2, Type: orgapi.IdentifierEUEORI, Country: "DE", Value: "DE1234567890123", EvidenceRef: "customs letter"})
	d.must("consignee EORI", err)
	_, err = doc.RecordIssuanceAuthority(d.ctx, docapi.RecordIssuanceAuthority{CommandID: "auth-hbl", TenantID: d.tenant, ActorID: "legal-1", AuthorityID: "AUTH-HBL", LegalEntityID: "LE",
		Kind: docapi.KindHBL, IssuePlace: "CNSHG", Qualification: "HBL filing", Validity: kernel.ValidityInterval{From: d.at}, EvidenceRef: "filings/HBL", OccurredAt: d.hours(21)})
	d.must("authority", err)
	return docModule{doc: doc, d: d}
}

// docModule is DOC over the day, with the one command the case needs.
type docModule struct {
	doc interface {
		DraftTransportDocument(context.Context, docapi.DraftTransportDocument) (docapi.Receipt, error)
		ApproveTransportDocument(context.Context, docapi.ApproveTransportDocument) (docapi.Receipt, error)
		IssueTransportDocument(context.Context, docapi.IssueTransportDocument) (docapi.Receipt, error)
		GetTransportDocument(context.Context, docapi.GetTransportDocument) (docapi.TransportDocument, error)
		GetDocumentVersion(context.Context, docapi.GetDocumentVersion) (docapi.DocumentVersion, error)
		OpenDocumentAmendment(context.Context, docapi.OpenDocumentAmendment) (docapi.Receipt, error)
	}
	d *reworkDay
}

// issueHBL drafts, approves and issues the shipment's HBL, released EXPRESS, at the hour: the shipper and the consignee
// are set on the order and locked as the bill's holder.
func (m docModule) issueHBL(shipment string, hour int) {
	d := m.d
	d.t.Helper()
	house := strings.TrimPrefix(shipment, "SH-")
	order, id := "O-"+house, "DOC-HBL-"+house
	orderRevision := func() int64 {
		p, err := d.com.GetOrderParties(d.ctx, comapi.GetOrderParties{TenantID: d.tenant, OrderID: order})
		d.must("order parties "+order, err)
		return p.Revision
	}
	for _, p := range []comapi.OrderParty{{Role: comapi.PartyShipper, PartyID: "shipper-" + strings.ToLower(house)}, {Role: comapi.PartyConsignee, PartyID: "consignee-de"}} {
		_, err := d.com.SetOrderParty(d.ctx, comapi.SetOrderParty{CommandID: "set-" + p.Role + "-" + house, TenantID: d.tenant, ActorID: "cs-1", OrderID: order,
			ExpectedRevision: orderRevision(), Party: p, OccurredAt: d.hours(hour)})
		d.must("set "+p.Role+" of "+order, err)
	}
	_, err := m.doc.DraftTransportDocument(d.ctx, docapi.DraftTransportDocument{CommandID: "draft-" + id, TenantID: d.tenant, ActorID: "doc-1", DocumentID: id, Kind: docapi.KindHBL,
		MovementID: "OM-1", ShipmentID: shipment, PaymentIndicator: docapi.PaymentPrepaid, Release: docapi.ReleaseExpress, OccurredAt: d.hours(hour)})
	d.must("draft "+id, err)
	_, err = m.doc.ApproveTransportDocument(d.ctx, docapi.ApproveTransportDocument{CommandID: "approve-" + id, TenantID: d.tenant, ActorID: "doc-2", DocumentID: id, ExpectedRevision: 1,
		OccurredAt: d.hours(hour)})
	d.must("approve "+id, err)
	_, err = d.com.LockOrderParties(d.ctx, comapi.LockOrderParties{CommandID: "lock-" + id, TenantID: d.tenant, ActorID: "doc-2", OrderID: order, ExpectedRevision: orderRevision(),
		HolderID: id, OccurredAt: d.hours(hour)})
	d.must("lock "+order, err)
	_, err = m.doc.IssueTransportDocument(d.ctx, docapi.IssueTransportDocument{CommandID: "issue-" + id, TenantID: d.tenant, ActorID: "doc-2", DocumentID: id, ExpectedRevision: 2,
		CreditOverrideReason: "the controller approved the credit", OccurredAt: d.hours(hour + 1)})
	d.must("issue "+id, err)
}

// TestAReworkAfterSEAFinalizedAndDOCIssuedWithEveryContext is case 4: the movement is FINAL and DOC has issued the
// house bills of H01 and H03 before PHY's rework. The brief's rule F3 refuses a container that gated in or loaded
// before PHY sealed it again, and DOC issues only from facts that state an on-board date, which comes from such a
// loading: so a version n+1 can follow only when the container's loading, and its VGM, are dated after PHY's reseal
// (hour 25). They are here (VGM v1 at 26, gate-in at 27, loading at 28, finalize at 29, DOC issues at 30 and 31),
// recorded before PHY's rework runs in the order of the commands; the business times are the facts' and the order of
// the commands is the day's. 1. Nobody re-reads: PHY unloads H03, seals again and detaches it, and SEA and DOC keep the
// FINAL facts of before: version 1 current, three houses, SEAL-X-1; DOC's bills stay as issued from version 1. 2. FUL
// records the detachment and the evidence, SEA ends H03's association (the movement reopens: version 1 is SUPERSEDED)
// and DOC's amendment is refused (DOC_SOURCE_NOT_FINAL). 3. SEA refreshes H01, which records S2 on the container; the
// new finalize is version 2, with S2, H01 and H02.
//
// What today's DOC does with version 2 is asserted at the end. DOC's amendment of an issued bill from the new version
// is PR 4's: nothing of DOC is changed here.
func TestAReworkAfterSEAFinalizedAndDOCIssuedWithEveryContext(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-follow4-")
	documents := d.withDocuments()
	d.must("FUL takes the first evidence", d.fulTakes(16, "SH-H01", "SH-H02", "SH-H03"))
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		d.must("refresh "+s, d.refresh(s, 17))
	}
	d.must("SEA's seal", d.seal("sea-seal", "seal-x-1", seaapi.SealShipper, 15, 17))
	d.must("stuffing done", d.stuffingDone("stuffed", 18))
	d.masterBill(18)
	d.must("VGM v1", d.vgm("vgm-1", 2900, 26))
	d.event("gate-in", seaapi.EventGateIn, "GIN-1", 27)
	d.event("loaded", seaapi.EventLoaded, "LD-1", 28)
	d.must("finalize", d.finalize("fin-1", 29))
	documents.issueHBL("SH-H01", 30)
	documents.issueHBL("SH-H03", 30)
	hbl := func(id string) docapi.DocumentVersion {
		t.Helper()
		v, err := documents.doc.GetDocumentVersion(d.ctx, docapi.GetDocumentVersion{TenantID: d.tenant, DocumentID: id})
		d.must("the version of "+id, err)
		return v
	}
	if v := hbl("DOC-HBL-H01"); v.Status != docapi.VersionIssued || v.Doc.Source.Version != 1 || v.Doc.Cargo.Pieces != 6 || len(v.Doc.Containers) != 1 ||
		v.Doc.Containers[0].SealNumber != "SEAL-X-1" || !v.Doc.OnBoardAt.Equal(d.hours(28)) {
		t.Fatalf("the HBL of H01 as issued: %+v", v)
	}

	// PHY reworks the container; nobody reads it
	d.openRework()
	d.unloadH03()
	d.sealAgain()
	d.detachH03()
	facts, err := d.sea.GetOceanDocFacts(d.ctx, seaapi.GetOceanDocFacts{TenantID: d.tenant, MovementID: "OM-1"})
	d.must("the facts", err)
	if m := d.movement(); m.State != seaapi.StateFinal || m.DocFactsVersion != 1 || facts.Version != 1 || facts.Status != seaapi.DocFactsFinal || len(facts.Houses) != 3 ||
		facts.Containers[0].SealNumber != "SEAL-X-1" || facts.Containers[0].Packs != 18 || hbl("DOC-HBL-H01").Doc.Containers[0].SealNumber != "SEAL-X-1" ||
		hbl("DOC-HBL-H03").Status != docapi.VersionIssued {
		t.Fatalf("with nobody re-reading, the FINAL facts stay: %+v %+v", d.movement().State, facts)
	}
	if d.due("SH-H01") != true {
		t.Fatal("FUL has not read PHY's reseal")
	}

	// FUL and SEA read the detachment; SEA ends H03's association: the movement reopens, DOC cannot amend
	d.fulTakesTheDetachment(32)
	d.must("FUL records the resealed evidence", d.fulTakes(33, "SH-H01", "SH-H02", "SH-H03"))
	d.endH03(34)
	if m := d.movement(); m.State != seaapi.StateOpen || m.DocFactsVersion != 1 {
		t.Fatalf("ending H03's association reopens the movement: %+v", m.State)
	}
	_, err = documents.doc.OpenDocumentAmendment(d.ctx, docapi.OpenDocumentAmendment{CommandID: "amend-h01-early", TenantID: d.tenant, ActorID: "doc-1", DocumentID: "DOC-HBL-H01", ExpectedRevision: 3,
		Reason: "PHY resealed the container", OccurredAt: d.hours(35)})
	refusedWith(t, "an amendment while the facts are not FINAL", err, "DOC_SOURCE_NOT_FINAL")
	// SEA reads the reseal: H01's refresh records S2; H02's records nothing; the new version is 2
	d.must("refresh H01", d.refresh("SH-H01", 35))
	if got := sealList(d.cv()); got != "SEAL-X-1/SHIPPER SEAL-X-1-NEW/SHIPPER" {
		t.Fatalf("seals: %s", got)
	}
	d.must("refresh H02", d.refresh("SH-H02", 35))
	d.must("finalize again", d.finalize("fin-2", 36))
	facts, err = d.sea.GetOceanDocFacts(d.ctx, seaapi.GetOceanDocFacts{TenantID: d.tenant, MovementID: "OM-1"})
	d.must("the facts", err)
	if c := facts.Containers[0]; facts.Version != 2 || facts.Status != seaapi.DocFactsFinal || len(facts.Houses) != 2 || c.SealNumber != "SEAL-X-1-NEW" || c.VGMAssertionID != "CV-1:1" ||
		c.Packs != 12 || c.GrossWeightGrams != 612_000 || !facts.OnBoardAt.Equal(d.hours(28)) {
		t.Fatalf("version 2: %+v", facts)
	}

	// What today's DOC does with version 2: the bills it issued stay as issued from version 1; none is amended or
	// cancelled by SEA's new version, and H03's bill still stands for a house SEA no longer publishes. DOC's amendment
	// from version 2 is asked of H01 (released EXPRESS, so with no originals to return) and of H03.
	for _, id := range []string{"DOC-HBL-H01", "DOC-HBL-H03"} {
		v := hbl(id)
		if v.Status != docapi.VersionIssued || v.Doc.Source.Version != 1 || v.Doc.Containers[0].SealNumber != "SEAL-X-1" {
			t.Fatalf("%s after version 2: %+v", id, v)
		}
	}
	amended, err := documents.doc.OpenDocumentAmendment(d.ctx, docapi.OpenDocumentAmendment{CommandID: "amend-h01", TenantID: d.tenant, ActorID: "doc-1", DocumentID: "DOC-HBL-H01",
		ExpectedRevision: 3, Reason: "PHY resealed the container", OccurredAt: d.hours(37)})
	if err != nil || amended.Version != 2 {
		t.Fatalf("DOC opens an amendment of H01's bill from version 2: %+v %v", amended, err)
	}
	draft, err := documents.doc.GetDocumentVersion(d.ctx, docapi.GetDocumentVersion{TenantID: d.tenant, DocumentID: "DOC-HBL-H01", Version: 2})
	if d.must("the amendment's draft", err); draft.Doc.Source.Version != 2 || len(draft.Doc.Containers) != 1 || draft.Doc.Containers[0].SealNumber != "SEAL-X-1-NEW" ||
		draft.Doc.Containers[0].Packs != 6 || draft.Doc.Cargo.GrossGrams != 312_000 {
		t.Fatalf("the draft from version 2: %+v", draft)
	}
	// the bill of the house that left cannot be amended: AIR's and SEA's facts no longer show the shipment under the
	// master
	_, err = documents.doc.OpenDocumentAmendment(d.ctx, docapi.OpenDocumentAmendment{CommandID: "amend-h03", TenantID: d.tenant, ActorID: "doc-1", DocumentID: "DOC-HBL-H03",
		ExpectedRevision: 3, Reason: "the house left", OccurredAt: d.hours(37)})
	refusedWith(t, "an amendment of the bill of the house that left", err, "DOC_MASTER_MISMATCH", "SH-H03")
}

// seedParties adds ORG parties to the day's tenant, beside the legal entity it has.
func (d *reworkDay) seedParties(parties ...string) {
	d.t.Helper()
	fixture, err := d.platform.newDB()
	d.must("db", err)
	err = (bento.TenantTransactor{DB: fixture, Tenant: d.tenant}).WithinTransaction(d.ctx, func(txCtx context.Context) error {
		e, err := fixture.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		for _, party := range parties {
			if _, err = e.Exec(txCtx, `INSERT INTO cargo_org.business_party(tenant_id,party_id,legal_name,registration_country,status)
                VALUES($1,$2,'Fixture Corp','US','ACTIVE')`, d.tenant, party); err != nil {
				return err
			}
		}
		return nil
	})
	d.must("parties", err)
}

// ---- batch 2: what the independent review found, through the real contexts ----

// sealAgainWith is sealAgain with the seal's text as PHY keeps it, at the hour.
func (d *reworkDay) sealAgainWith(text string, hour int) {
	d.t.Helper()
	_, err := d.phy.CloseLoadExecution(d.ctx, phyapi.CloseLoadExecution{CommandID: "reseal-text", TenantID: d.tenant, ActorID: "cfs-2", ExecutionID: "X-1",
		ExpectedRevision: d.revision["X-1"], Evidence: d.evidence(phyapi.EvidenceSeal, text, hour), OccurredAt: d.hours(hour)})
	d.must("seal again with "+text, err)
	d.revision["X-1"]++
}

// vgmDetermined asserts a VGM whose mass was determined at one hour and submitted at another.
func (d *reworkDay) vgmDetermined(command string, kilos int64, determined, submitted int) error {
	d.t.Helper()
	r, err := d.sea.AssertVgm(d.ctx, seaapi.AssertVgm{CommandID: command, TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: d.visit,
		Mass: kernel.Quantity{Unit: "KGM", Amount: kilos}, Method: seaapi.VgmMethod2, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1, DeterminedAt: d.hours(determined),
		Signatory: "Ada", OccurredAt: d.hours(submitted)})
	if err == nil {
		d.visit = r.NewRevision
	}
	return err
}

// afterTheRework is PHY's rework with the seal text and time given, read by FUL (27, 28) and by SEA up to the refresh
// of the two houses at hour 30, in the order that leaves H03's association for last.
func (d *reworkDay) reworkWith(text string, sealed int) {
	d.t.Helper()
	d.openRework()
	d.unloadH03()
	d.sealAgainWith(text, sealed)
	d.detachH03()
	d.fulTakesTheDetachment(27)
	d.must("FUL records the resealed evidence", d.fulTakes(28, "SH-H01", "SH-H02", "SH-H03"))
}

// TestAResealRecordedAfterSEAsStuffingButDatedBeforeItIsFollowedWithEveryContext (review F1): SEA takes the stuffing at
// hour 30 with the first seal and a VGM at 31; PHY's rework is then recorded with business times 22 to 25, and FUL and
// SEA read it. Without a difference reason the seal PHY now keeps is one PHY applied since, whatever time it was
// recorded with: it is followed, and the container counts as resealed. What SEA then relies on is the times it is
// given: the VGM was determined at 31, after PHY's reseal at 25, so it stands; a container gated in at a time dated
// before PHY's reseal is refused (F3), the gate-in recorded after the stuffing with an earlier time of occurrence.
func TestAResealRecordedAfterSEAsStuffingButDatedBeforeItIsFollowedWithEveryContext(t *testing.T) {
	for _, gated := range []bool{false, true} {
		d := newReworkDay(t, "cargo-ocean-backdated-")
		d.must("FUL takes the first evidence", d.fulTakes(16, "SH-H01", "SH-H02", "SH-H03"))
		for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
			d.must("refresh "+s, d.refresh(s, 17))
		}
		d.must("SEA's seal", d.seal("sea-seal", "seal-x-1", seaapi.SealShipper, 15, 17))
		d.must("stuffing done", d.stuffingDone("stuffed", 30))
		d.masterBill(30)
		d.must("VGM v1", d.vgm("vgm-1", 2900, 31))
		if gated {
			// recorded at 32, occurred at 24: before PHY's reseal at 25
			r, err := d.sea.RecordContainerEvent(d.ctx, seaapi.RecordContainerEvent{CommandID: "gate-early", TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-1",
				ExpectedRevision: d.visit, Kind: seaapi.EventGateIn, Location: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHG"}, HappenedAt: d.hours(24), Source: "terminal",
				Reference: "GIN-1", OccurredAt: d.hours(32)})
			d.must("gate-in", err)
			d.visit = r.NewRevision
		}
		d.reworkWith("SEAL-X-1-NEW", 25)
		d.endH03(33)
		d.must("refresh H01", d.refresh("SH-H01", 34))
		if got := sealList(d.cv()); got != "SEAL-X-1/SHIPPER SEAL-X-1-NEW/SHIPPER" {
			t.Fatalf("gated %v: S2 is followed though PHY sealed it before SEA's StuffedAt: %s", gated, got)
		}
		d.visit = d.cv().Revision
		d.must("refresh H02", d.refresh("SH-H02", 34))
		if gated {
			refusedWith(t, "F3 after a backdated reseal", d.finalize("fin-f3", 35), "was at the terminal when PHY sealed it again", "GATE_OUT_TERMINAL")
			continue
		}
		// only warnings: SEA learned of the seal after the VGM was keyed in, and PHY's reseal is dated before the
		// stuffing
		d.wantPending("a backdated reseal", "VGM_ENTERED_BEFORE_SEAL_KNOWN (warning), RESEAL_DATED_BEFORE_STUFFING (warning)")
		d.must("finalize", d.finalize("fin-1", 35))
		facts, err := d.sea.GetOceanDocFacts(d.ctx, seaapi.GetOceanDocFacts{TenantID: d.tenant, MovementID: "OM-1"})
		d.must("the facts", err)
		if c := facts.Containers[0]; c.SealNumber != "SEAL-X-1-NEW" || c.VGMAssertionID != "CV-1:1" || c.VGMGrams != 2_900_000 || c.Packs != 12 {
			t.Fatalf("the VGM determined after the reseal's time stands: %+v", c)
		}
	}
}

// TestTheVGMIsDeterminedAfterTheResealWithEveryContext (review F4): after the reseal a VGM submitted later but with a
// mass determined before it (the old weighing, with H03 inside) is refused at finalize; one determined after it is
// accepted.
func TestTheVGMIsDeterminedAfterTheResealWithEveryContext(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-determined-")
	d.stuffedWithTheFirstSeal()
	d.openRework()
	d.reworkedAndRead()
	d.visit = d.cv().Revision
	d.must("the old weighing submitted again", d.vgmDetermined("vgm-old", 2900, 10, 31))
	refusedWith(t, "an old weighing", d.finalize("fin-old", 32), "mass determined", "before PHY sealed the container again", "assert one determined after it")
	d.must("a new weighing", d.vgmDetermined("vgm-new", 2950, 31, 32))
	d.must("finalize", d.finalize("fin-new", 33))
}

// TestPHYsSealTextSEAsFormDoesNotTakeWithEveryContext (review F2, F5): PHY keeps free text. "seal#9 new" is followed as
// "SEAL 9 NEW" and finalize passes F2; before H03's association is ended finalize says FUL no longer reports H03 as SEA
// froze it. A refresh at a business time before PHY sealed it says to use a later one. A text whose cleaned form is too
// long is refused with a message to record the seal by hand with the same letters and digits, after which the refresh
// records nothing and the movement finalizes; a letter outside A-Z is refused with the message that PHY has to seal
// again, and no command of SEA gets the movement past it (reasoned for the commands, run for the refusals).
func TestPHYsSealTextSEAsFormDoesNotTakeWithEveryContext(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-sealform-")
	d.stuffedWithTheFirstSeal()
	d.reworkWith("seal#9 new", 25)
	// a refresh at a business time before PHY sealed it is written, and the seal is a pending item that says which
	// times
	d.must("a refresh before PHY sealed", d.refresh("SH-H01", 24))
	d.must("a refresh before PHY sealed, H02", d.refresh("SH-H02", 24))
	if got := sealList(d.cv()); got != "SEAL-X-1/SHIPPER" {
		t.Fatalf("seals: %s", got)
	}
	// H03's association stands: FUL no longer reports it in the consolidation as SEA froze it
	refusedWith(t, "H03 still associated", d.finalize("fin-stale", 25), "STALE_HOUSE: FUL no longer reports", "SH-H03", "refresh the house, or end its association")
	d.endH03(25)
	it := d.pendingItem(seaapi.PendingSealNotOnContainer)
	if !it.Blocking || !strings.Contains(it.Message, "at a business time not before PHY sealed it (") || !strings.Contains(it.Message, "and not before the container's last change (") {
		t.Fatalf("the pending item: %+v", it)
	}
	// the later refresh follows it by itself, as the house's facts are the ones SEA froze
	d.must("refresh H01", d.refresh("SH-H01", 30))
	refusedWith(t, "nothing left to refresh", d.refresh("SH-H02", 30), "FUL reports the plan version, the measures and the containers SEA froze")
	v := d.cv()
	if sealList(v) != "SEAL-X-1/SHIPPER SEAL 9 NEW/SHIPPER" || !strings.Contains(v.Seals[0].Source, `(PHY's text "seal#9 new")`) {
		t.Fatalf("seals: %s %+v", sealList(v), v.Seals[0])
	}
	d.visit = v.Revision
	d.must("VGM v2", d.vgm("vgm-2", 2950, 33))
	d.must("finalize", d.finalize("fin-1", 34))
	facts, err := d.sea.GetOceanDocFacts(d.ctx, seaapi.GetOceanDocFacts{TenantID: d.tenant, MovementID: "OM-1"})
	d.must("the facts", err)
	if c := facts.Containers[0]; facts.Version != 1 || c.SealNumber != "SEAL 9 NEW" || c.VGMAssertionID != "CV-1:2" || c.Packs != 12 {
		t.Fatalf("published: %+v", facts)
	}
}

func TestPHYsSealTextThatNeedsTheSealRecordedByHandWithEveryContext(t *testing.T) {
	long := strings.TrimSuffix(strings.Repeat("AB.", 14), ".") // 41 characters, 28 letters
	key := strings.Repeat("AB", 14)
	d := newReworkDay(t, "cargo-ocean-sealhand-")
	d.stuffedWithTheFirstSeal()
	d.reworkWith(long, 25)
	d.endH03(29)
	// the refreshes are written; no seal is recorded; the pending item says what to do and Finalize names it
	d.must("refresh H01", d.refresh("SH-H01", 30))
	d.must("refresh H02", d.refresh("SH-H02", 30))
	if got := sealList(d.cv()); got != "SEAL-X-1/SHIPPER" {
		t.Fatalf("nothing is recorded: %s", got)
	}
	it := d.pendingItem(seaapi.PendingSealNotOnContainer)
	if !strings.Contains(it.Message, "record the container's seal by hand") || !strings.Contains(it.Message, key) {
		t.Fatalf("the pending item: %+v", it)
	}
	refusedWith(t, "finalize", d.finalize("fin-pending", 31), "record the container's seal by hand", key)
	d.must("the seal by hand", d.seal("sea-hand", strings.ToLower(key), seaapi.SealShipper, 25, 31))
	if got := d.pending(); got != "VGM_BEFORE_RESEAL" {
		t.Fatalf("pending after the seal by hand: %s", got)
	}
	d.must("VGM v2", d.vgm("vgm-2", 2950, 32))
	d.wantPending("after the new VGM", "")
	d.must("finalize", d.finalize("fin-1", 33))
}

func TestPHYsSealTextWithALetterOutsideAZStopsTheMovementWithEveryContext(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-sealletter-")
	d.stuffedWithTheFirstSeal()
	d.reworkWith("S\u00c9AL-X-9", 25)
	d.endH03(29)
	d.must("refresh H01", d.refresh("SH-H01", 30))
	d.must("refresh H02", d.refresh("SH-H02", 30))
	it := d.pendingItem(seaapi.PendingSealNotOnContainer)
	if !it.Blocking || !strings.Contains(it.Message, "PHY has to seal the container again") {
		t.Fatalf("the pending item: %+v", it)
	}
	refusedWith(t, "finalize", d.finalize("fin-1", 31), "PHY has to seal the container again")
	// the operator cannot record it by hand either: SEA's form has no such letter
	if err := d.seal("sea-hand", "S\u00c9AL-X-9", seaapi.SealShipper, 25, 31); err == nil {
		t.Fatal("SEA took a seal with a letter outside A-Z")
	}
	// the way out is PHY's: a new rework, sealed with letters A-Z and digits (reasoned: not run)
}

// pending is SEA's list of what stands between the movement and its finalization, as kinds (a warning marked).
func (d *reworkDay) pending() string {
	d.t.Helper()
	items, err := d.sea.GetPendingItems(d.ctx, seaapi.GetPendingItems{TenantID: d.tenant, MovementID: "OM-1"})
	d.must("pending items", err)
	var out []string
	for _, it := range items {
		k := it.Kind
		if !it.Blocking {
			k += " (warning)"
		}
		out = append(out, k)
	}
	return strings.Join(out, ", ")
}

func (d *reworkDay) pendingItem(kind string) seaapi.PendingItem {
	d.t.Helper()
	items, err := d.sea.GetPendingItems(d.ctx, seaapi.GetPendingItems{TenantID: d.tenant, MovementID: "OM-1"})
	d.must("pending items", err)
	for _, it := range items {
		if it.Kind == kind {
			return it
		}
	}
	d.t.Fatalf("no pending item %s in %s", kind, d.pending())
	return seaapi.PendingItem{}
}

func (d *reworkDay) wantPending(stage, want string) {
	d.t.Helper()
	if got := d.pending(); got != want {
		d.t.Fatalf("%s: pending %q, want %q", stage, got, want)
	}
}

// TestADifferenceReasonContainerResealedWithBackdatedTimesWithEveryContext (review G1): SEA took the container as
// stuffed at hour 30 with another seal than PHY's (a difference reason stated), and PHY's rework is recorded afterwards
// with business times 22 to 25. PHY's seal was SEAL-X-1 in the entries SEA froze at 17 and is SEAL-X-1-NEW now, which
// the movement's history shows whatever the business times say: the seal is followed, the container counts as resealed,
// F2 asks for PHY's seal and the movement finalizes with it. Only warnings remain, the times being out of order; the
// VGM determined at 31 stands.
func TestADifferenceReasonContainerResealedWithBackdatedTimesWithEveryContext(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-reason-")
	d.must("FUL takes the first evidence", d.fulTakes(16, "SH-H01", "SH-H02", "SH-H03"))
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		d.must("refresh "+s, d.refresh(s, 17))
	}
	d.must("SEA's other seal", d.seal("sea-seal", "other-seal-1", seaapi.SealShipper, 15, 17))
	r, err := d.sea.RecordStuffingDone(d.ctx, seaapi.RecordStuffingDone{CommandID: "stuffed", TenantID: d.tenant, ActorID: "cfs-1", VisitID: "CV-1", ExpectedRevision: d.visit,
		DifferenceReason: "seal re-keyed on the tally sheet", OccurredAt: d.hours(30)})
	d.must("stuffing with a difference reason", err)
	d.visit = r.NewRevision
	d.masterBill(30)
	d.must("VGM v1", d.vgm("vgm-1", 2900, 31))
	d.wantPending("before the rework", "")
	d.reworkWith("SEAL-X-1-NEW", 25)
	d.endH03(33)
	d.must("refresh H01", d.refresh("SH-H01", 34))
	if got := sealList(d.cv()); got != "OTHER-SEAL-1/SHIPPER SEAL-X-1-NEW/SHIPPER" {
		t.Fatalf("PHY's new seal is followed through the entry history: %s", got)
	}
	d.visit = d.cv().Revision
	refusedWith(t, "H02 not read yet", d.finalize("fin-early", 34), "STALE_HOUSE")
	d.must("refresh H02", d.refresh("SH-H02", 34))
	d.wantPending("both houses refreshed", "VGM_ENTERED_BEFORE_SEAL_KNOWN (warning), RESEAL_DATED_BEFORE_STUFFING (warning)")
	d.must("finalize", d.finalize("fin-1", 35))
	facts, err := d.sea.GetOceanDocFacts(d.ctx, seaapi.GetOceanDocFacts{TenantID: d.tenant, MovementID: "OM-1"})
	d.must("the facts", err)
	if c := facts.Containers[0]; c.SealNumber != "SEAL-X-1-NEW" || c.VGMAssertionID != "CV-1:1" || c.Packs != 12 {
		t.Fatalf("published: %+v", c)
	}
	// a difference reason whose PHY seal never changed keeps today's behaviour: not resealed, nothing pending
	e := newReworkDay(t, "cargo-ocean-reason2-")
	e.must("FUL takes the first evidence", e.fulTakes(16, "SH-H01", "SH-H02", "SH-H03"))
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		e.must("refresh "+s, e.refresh(s, 17))
	}
	e.must("SEA's other seal", e.seal("sea-seal", "other-seal-1", seaapi.SealShipper, 15, 17))
	r, err = e.sea.RecordStuffingDone(e.ctx, seaapi.RecordStuffingDone{CommandID: "stuffed", TenantID: e.tenant, ActorID: "cfs-1", VisitID: "CV-1", ExpectedRevision: e.visit,
		DifferenceReason: "seal re-keyed on the tally sheet", OccurredAt: e.hours(18)})
	e.must("stuffing with a difference reason", err)
	e.visit = r.NewRevision
	e.masterBill(18)
	e.must("VGM v1", e.vgm("vgm-1", 2900, 19))
	e.wantPending("PHY's seal never changed", "")
	e.must("finalize", e.finalize("fin-1", 20))
	facts, err = e.sea.GetOceanDocFacts(e.ctx, seaapi.GetOceanDocFacts{TenantID: e.tenant, MovementID: "OM-1"})
	if e.must("the facts", err); facts.Containers[0].SealNumber != "OTHER-SEAL-1" {
		t.Fatalf("published: %+v", facts.Containers[0])
	}
}

// TestAResealBeforeTheStuffingIsNoResealSinceWithEveryContext (review round 3, H1): PHY reseals before SEA's
// stuffing-done (case 2's shape). The entries SEA froze before hold the older seal, but SEA took the container with
// PHY's new seal, and nothing changed since: no warning, no block, no seal followed. With a difference reason stated
// against SEA's own seal (PHY's new text is wrong) the reason is kept and SEA's seal is published; without a reason the
// seal recorded by hand is.
func TestAResealBeforeTheStuffingIsNoResealSinceWithEveryContext(t *testing.T) {
	for _, reason := range []string{"", "seal re-keyed at the CFS"} {
		d := newReworkDay(t, "cargo-ocean-before-")
		d.must("FUL takes the first evidence", d.fulTakes(16, "SH-H01", "SH-H02", "SH-H03"))
		for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
			d.must("refresh "+s, d.refresh(s, 17))
		}
		d.must("SEA's seal", d.seal("sea-seal", "seal-x-1", seaapi.SealShipper, 15, 17))
		d.masterBill(18)
		d.reworkWith("SEAL-X-1-NEW", 25)
		d.endH03(29)
		d.must("refresh H01", d.refresh("SH-H01", 30))
		d.must("refresh H02", d.refresh("SH-H02", 30))
		if got := sealList(d.cv()); got != "SEAL-X-1/SHIPPER" {
			t.Fatalf("reason %q: an ASSIGNED container is not followed: %s", reason, got)
		}
		if reason == "" {
			d.must("PHY's new seal by hand", d.seal("sea-seal-2", "seal-x-1-new", seaapi.SealShipper, 25, 31))
			d.must("stuffing", d.stuffingDone("stuffed", 32))
		} else {
			r, err := d.sea.RecordStuffingDone(d.ctx, seaapi.RecordStuffingDone{CommandID: "stuffed", TenantID: d.tenant, ActorID: "cfs-1", VisitID: "CV-1", ExpectedRevision: d.visit,
				DifferenceReason: reason, OccurredAt: d.hours(32)})
			d.must("stuffing with a reason", err)
			d.visit = r.NewRevision
		}
		d.must("VGM", d.vgm("vgm-1", 2950, 33))
		d.wantPending("after the stuffing, PHY's seal as SEA took it", "")
		refusedWith(t, "nothing to refresh", d.refresh("SH-H01", 34), "FUL reports the plan version, the measures and the containers SEA froze")
		want := "SEAL-X-1-NEW"
		if reason != "" {
			want = "SEAL-X-1"
		}
		d.must("finalize", d.finalize("fin-1", 35))
		facts, err := d.sea.GetOceanDocFacts(d.ctx, seaapi.GetOceanDocFacts{TenantID: d.tenant, MovementID: "OM-1"})
		d.must("the facts", err)
		if c := facts.Containers[0]; c.SealNumber != want || c.VGMAssertionID != "CV-1:1" || c.Packs != 12 {
			t.Fatalf("reason %q: published %+v", reason, c)
		}
	}
}
