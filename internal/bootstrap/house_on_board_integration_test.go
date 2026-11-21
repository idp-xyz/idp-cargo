//go:build integration

package bootstrap

import (
	"strings"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.90: DOC dates a house bill by the loading of the house's own containers, with the real ORG, RATE, COM, FUL, PHY,
// BOOK, SEA, FIN and DOC. The movement OM-1 of C-1 has two containers: CV-1 (PHY's X-1) carries H01 and H02, CV-2
// (X-2) carries H03. CV-1 is loaded at hour 24 (the day 1 of the case), CV-2 at hour 31 (the day 3). The hours are
// the facts' business times; the order of the commands is the day's.

// loadingDay is the day: SEA's movement OM-1 with its two containers, DOC over the same database, and the revision of
// each container's visit.
type loadingDay struct {
	*reworkDay
	docs  docModule
	visit map[string]int64
}

// wrapReworkDay is newReworkDay's building of the modules over an ocean day, with the movement OM-1 opened at hour 11
// and the three houses associated, and no container assigned.
func wrapReworkDay(t *testing.T, d *oceanDay) *reworkDay {
	t.Helper()
	ctx := d.ctx
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
	return rd
}

func newLoadingDay(t *testing.T, prefix string) *loadingDay {
	t.Helper()
	o := newOceanDay(t, prefix, map[string]int64{"H01": 312, "H02": 300, "H03": 288}, map[string]int64{"H01": 10_000_000, "H02": 15_000_000, "H03": 5_000_000},
		[]string{"22G1", "22G1"}, []struct{ code, method string }{{"OCEAN_FREIGHT", finapi.MethodChargeable}})
	o.stuff("X-1", 1)
	o.load("X-1", "H01", 6, 0)
	o.load("X-1", "H02", 6, 0)
	o.stuff("X-2", 2)
	o.load("X-2", "H03", 6, 0)
	o.seal("X-1")
	o.seal("X-2")
	rd := wrapReworkDay(t, o)
	d := &loadingDay{reworkDay: rd, visit: map[string]int64{}}
	for i, box := range []struct{ visit, number string }{{"CV-1", "csqu3054383"}, {"CV-2", "temu1234565"}} {
		r, err := d.sea.AssignContainer(d.ctx, seaapi.AssignContainer{CommandID: "assign-" + box.visit, TenantID: d.tenant, ActorID: "ops-1", VisitID: box.visit, MovementID: "OM-1",
			LoadUnitNo: i + 1, ContainerNumber: box.number, SizeType: "22G1", TareKg: 2180, MaxGrossKg: 30480, PlateSource: seaapi.PlateCSC, OccurredAt: d.hours(12)})
		d.must("assign "+box.visit, err)
		d.visit[box.visit] = r.NewRevision
	}
	d.docs = d.withDocuments()
	// SEA reads PHY's stuffings, takes both containers as stuffed, records the master bill and a VGM for each
	d.must("FUL takes the evidence", d.fulTakes(16, "SH-H01", "SH-H02", "SH-H03"))
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03"} {
		d.must("refresh "+s, d.refresh(s, 17))
	}
	for _, v := range []string{"CV-1", "CV-2"} {
		x := strings.ToLower(strings.Replace(v, "CV-", "x-", 1))
		d.must("seal "+v, d.boxSeal(v, "seal-"+x, 17))
		d.must("stuffing done "+v, d.boxStuffingDone(v, 18))
		d.must("VGM "+v, d.boxVGM(v, 2900, 19))
	}
	d.masterBill(18)
	return d
}

// dm is DOC's whole module: the day's docModule holds it as the narrow interface its one command needs.
func (d *loadingDay) dm() interface {
	docapi.Commands
	docapi.Queries
	docapi.PendingQueries
} {
	return d.docs.doc.(interface {
		docapi.Commands
		docapi.Queries
		docapi.PendingQueries
	})
}

func (d *loadingDay) boxSeal(visit, number string, hour int) error {
	d.t.Helper()
	r, err := d.sea.RecordContainerSeal(d.ctx, seaapi.RecordContainerSeal{CommandID: "seal-" + visit, TenantID: d.tenant, ActorID: "cfs-1", VisitID: visit,
		ExpectedRevision: d.visit[visit], Number: number, Party: seaapi.SealShipper, AppliedAt: d.hours(15), Source: "tally sheet", OccurredAt: d.hours(hour)})
	if err == nil {
		d.visit[visit] = r.NewRevision
	}
	return err
}

func (d *loadingDay) boxStuffingDone(visit string, hour int) error {
	d.t.Helper()
	r, err := d.sea.RecordStuffingDone(d.ctx, seaapi.RecordStuffingDone{CommandID: "stuffed-" + visit, TenantID: d.tenant, ActorID: "cfs-1", VisitID: visit,
		ExpectedRevision: d.visit[visit], OccurredAt: d.hours(hour)})
	if err == nil {
		d.visit[visit] = r.NewRevision
	}
	return err
}

func (d *loadingDay) boxVGM(visit string, kilos int64, hour int) error {
	d.t.Helper()
	r, err := d.sea.AssertVgm(d.ctx, seaapi.AssertVgm{CommandID: "vgm-" + visit, TenantID: d.tenant, ActorID: "ops-1", VisitID: visit, ExpectedRevision: d.visit[visit],
		Mass: kernel.Quantity{Unit: "KGM", Amount: kilos}, Method: seaapi.VgmMethod2, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1, DeterminedAt: d.hours(hour),
		Signatory: "Ada", OccurredAt: d.hours(hour)})
	if err == nil {
		d.visit[visit] = r.NewRevision
	}
	return err
}

// load records the container's gate-in an hour before its loading, and the loading, at the hour.
func (d *loadingDay) load(visit string, hour int) {
	d.t.Helper()
	for _, e := range []struct {
		kind string
		at   int
	}{{seaapi.EventGateIn, hour - 1}, {seaapi.EventLoaded, hour}} {
		r, err := d.sea.RecordContainerEvent(d.ctx, seaapi.RecordContainerEvent{CommandID: e.kind + "-" + visit, TenantID: d.tenant, ActorID: "ops-1", VisitID: visit,
			ExpectedRevision: d.visit[visit], Kind: e.kind, Location: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHG"}, HappenedAt: d.hours(e.at), Source: "terminal",
			Reference: e.kind + "-" + visit, OccurredAt: d.hours(e.at)})
		d.must(e.kind+" "+visit, err)
		d.visit[visit] = r.NewRevision
	}
}

func (d *loadingDay) facts(movement string) seaapi.OceanDocFacts {
	d.t.Helper()
	f, err := d.sea.GetOceanDocFacts(d.ctx, seaapi.GetOceanDocFacts{TenantID: d.tenant, MovementID: movement})
	d.must("SEA's facts of "+movement, err)
	return f
}

func (d *loadingDay) finalizeOf(movement, command string, hour int) {
	d.t.Helper()
	m, err := d.sea.GetOceanMovement(d.ctx, seaapi.GetOceanMovement{TenantID: d.tenant, MovementID: movement})
	d.must("movement "+movement, err)
	_, err = d.sea.FinalizeOceanDocFacts(d.ctx, seaapi.FinalizeOceanDocFacts{CommandID: command, TenantID: d.tenant, ActorID: "ops-2", MovementID: movement,
		ExpectedRevision: m.Revision, OccurredAt: d.hours(hour)})
	d.must("finalize "+movement, err)
}

// prepare sets the parties of the house's order, drafts its HBL on the movement, approves it and locks the order for
// it.
func (d *loadingDay) prepare(shipment, movement string, hour int) string {
	d.t.Helper()
	house := strings.TrimPrefix(shipment, "SH-")
	order, id := "O-"+house, "DOC-HBL-"+house
	revision := func() int64 {
		p, err := d.com.GetOrderParties(d.ctx, comapi.GetOrderParties{TenantID: d.tenant, OrderID: order})
		d.must("order parties "+order, err)
		return p.Revision
	}
	for _, p := range []comapi.OrderParty{{Role: comapi.PartyShipper, PartyID: "shipper-" + strings.ToLower(house)}, {Role: comapi.PartyConsignee, PartyID: "consignee-de"}} {
		_, err := d.com.SetOrderParty(d.ctx, comapi.SetOrderParty{CommandID: "set-" + p.Role + "-" + house, TenantID: d.tenant, ActorID: "cs-1", OrderID: order,
			ExpectedRevision: revision(), Party: p, OccurredAt: d.hours(hour)})
		d.must("set "+p.Role+" of "+order, err)
	}
	_, err := d.dm().DraftTransportDocument(d.ctx, docapi.DraftTransportDocument{CommandID: "draft-" + id, TenantID: d.tenant, ActorID: "doc-1", DocumentID: id,
		Kind: docapi.KindHBL, MovementID: movement, ShipmentID: shipment, PaymentIndicator: docapi.PaymentPrepaid, Release: docapi.ReleaseExpress, OccurredAt: d.hours(hour)})
	d.must("draft "+id, err)
	d.approve(id, 1, hour)
	_, err = d.com.LockOrderParties(d.ctx, comapi.LockOrderParties{CommandID: "lock-" + id, TenantID: d.tenant, ActorID: "doc-2", OrderID: order, ExpectedRevision: revision(),
		HolderID: id, OccurredAt: d.hours(hour)})
	d.must("lock "+order, err)
	return id
}

func (d *loadingDay) approve(id string, revision int64, hour int) {
	d.t.Helper()
	_, err := d.dm().ApproveTransportDocument(d.ctx, docapi.ApproveTransportDocument{CommandID: "approve-" + id + "-" + string(rune('a'+hour)), TenantID: d.tenant, ActorID: "doc-2",
		DocumentID: id, ExpectedRevision: revision, OccurredAt: d.hours(hour)})
	d.must("approve "+id, err)
}

func (d *loadingDay) issue(id string, revision int64, hour int) error {
	d.t.Helper()
	_, err := d.dm().IssueTransportDocument(d.ctx, docapi.IssueTransportDocument{CommandID: "issue-" + id + "-" + string(rune('a'+hour)), TenantID: d.tenant, ActorID: "doc-2",
		DocumentID: id, ExpectedRevision: revision, CreditOverrideReason: "the controller approved the credit", OccurredAt: d.hours(hour)})
	return err
}

func (d *loadingDay) refreshBill(id string, revision int64, movement string, hour int) {
	d.t.Helper()
	_, err := d.dm().RefreshDocumentDraft(d.ctx, docapi.RefreshDocumentDraft{CommandID: "refresh-" + id + "-" + string(rune('a'+hour)), TenantID: d.tenant, ActorID: "doc-1",
		DocumentID: id, ExpectedRevision: revision, Reason: "SEA's latest facts", MovementID: movement, OccurredAt: d.hours(hour)})
	d.must("refresh "+id, err)
}

func (d *loadingDay) bill(id string) (docapi.TransportDocument, docapi.DocumentVersion) {
	d.t.Helper()
	doc, err := d.dm().GetTransportDocument(d.ctx, docapi.GetTransportDocument{TenantID: d.tenant, DocumentID: id})
	d.must("document "+id, err)
	v, err := d.dm().GetDocumentVersion(d.ctx, docapi.GetDocumentVersion{TenantID: d.tenant, DocumentID: id})
	d.must("version of "+id, err)
	return doc, v
}

// pendingList is DOC's pending items of the movement, one string each: "DOCUMENT KIND blocking|open".
func (d *loadingDay) pendingList(movement string) string {
	d.t.Helper()
	items, err := d.dm().GetPendingItemsOfMovement(d.ctx, docapi.GetPendingItemsOfMovement{TenantID: d.tenant, MovementID: movement})
	d.must("DOC's pending items of "+movement, err)
	var out []string
	for _, it := range items {
		if it.Message == "" {
			d.t.Fatalf("an item with no message: %+v", it)
		}
		state := "open"
		if it.Blocking {
			state = "blocking"
		}
		out = append(out, strings.TrimPrefix(it.DocumentID, "DOC-HBL-")+" "+it.Kind+" "+state)
	}
	return strings.Join(out, ", ")
}

func (d *loadingDay) wantPending(stage, movement, want string) {
	d.t.Helper()
	if got := d.pendingList(movement); got != want {
		d.t.Fatalf("%s: DOC's pending items of %s: %q, want %q", stage, movement, got, want)
	}
}

// TestAHouseBillIsDatedByItsOwnContainersWithEveryContext is the main case. CV-1 (H01, H02) is loaded at hour 24 and
// SEA finalizes (version 1: CV-1 states hour 24, CV-2 none, the movement hour 24): H01's bill is issued, dated hour 24;
// H03's cannot be issued while CV-2 is not loaded, and the refusal names CV-2. CV-2 is loaded at hour 31: SEA reopens
// the movement though its own date did not change, and finalizes again (version 2); H03's draft is refreshed and
// issued, dated hour 31, and H01's bill is as it was.
func TestAHouseBillIsDatedByItsOwnContainersWithEveryContext(t *testing.T) {
	d := newLoadingDay(t, "cargo-house-on-board-")
	d.load("CV-1", 24)
	d.finalizeOf("OM-1", "fin-1", 25)
	if f := d.facts("OM-1"); f.Version != 1 || !f.ContainerDates || !f.OnBoardAt.Equal(d.hours(24)) || len(f.Containers) != 2 || !f.Containers[0].OnBoardAt.Equal(d.hours(24)) ||
		!f.Containers[1].OnBoardAt.IsZero() || len(f.Houses) != 3 {
		t.Fatalf("version 1: %+v", f)
	}
	h01 := d.prepare("SH-H01", "OM-1", 26)
	h03 := d.prepare("SH-H03", "OM-1", 26)
	d.wantPending("H01 and H03 drafted", "OM-1", "H03 HOUSE_NOT_ALL_LOADED blocking")
	d.must("H01's bill", d.issue(h01, 2, 27))
	doc, v := d.bill(h01)
	if doc.State != docapi.StateIssued || !v.Doc.OnBoardAt.Equal(d.hours(24)) || v.Doc.Source.Version != 1 || len(v.Doc.Containers) != 1 || v.Doc.Containers[0].Number != "CSQU3054383" {
		t.Fatalf("H01's bill: %+v %+v", doc, v)
	}
	err := d.issue(h03, 2, 27)
	refusedWith(t, "H03's bill while CV-2 is not loaded", err, "DOC_INCOMPLETE", "TEMU1234565", "not all on board", "wait for its loading")
	if doc, _ = d.bill(h03); doc.State != docapi.StateApproved {
		t.Fatalf("a refusal changes nothing: %+v", doc)
	}
	d.wantPending("H01 issued, H03 refused", "OM-1", "H03 HOUSE_NOT_ALL_LOADED blocking")

	// CV-2 is loaded: the movement's date stands (hour 24), CV-2's is new, so SEA reopens the movement
	d.load("CV-2", 31)
	if m := d.movement(); m.State != seaapi.StateOpen {
		t.Fatalf("the loading of CV-2 left the movement %s", m.State)
	}
	d.wantPending("SEA reopened", "OM-1", "H01 SOURCE_NOT_FINAL open, H03 SOURCE_NOT_FINAL blocking")
	refusedWith(t, "H03's bill while SEA is open", d.issue(h03, 2, 32), "DOC_SOURCE_STALE")
	d.finalizeOf("OM-1", "fin-2", 32)
	if f := d.facts("OM-1"); f.Version != 2 || !f.OnBoardAt.Equal(d.hours(24)) || !f.Containers[0].OnBoardAt.Equal(d.hours(24)) || !f.Containers[1].OnBoardAt.Equal(d.hours(31)) {
		t.Fatalf("version 2: %+v", f)
	}
	d.wantPending("SEA finalized again", "OM-1", "H03 SOURCE_SUPERSEDED blocking")
	refusedWith(t, "H03's draft of version 1", d.issue(h03, 2, 33), "DOC_SOURCE_STALE")
	d.refreshBill(h03, 2, "", 33)
	d.approve(h03, 3, 34)
	d.wantPending("H03 refreshed", "OM-1", "")
	d.must("H03's bill", d.issue(h03, 4, 35))
	if doc, v = d.bill(h03); doc.State != docapi.StateIssued || !v.Doc.OnBoardAt.Equal(d.hours(31)) || v.Doc.Source.Version != 2 || v.Doc.Containers[0].Number != "TEMU1234565" {
		t.Fatalf("H03's bill: %+v %+v", doc, v)
	}
	if doc, v = d.bill(h01); doc.State != docapi.StateIssued || !v.Doc.OnBoardAt.Equal(d.hours(24)) || v.Doc.Source.Version != 1 {
		t.Fatalf("H01's bill is as it was: %+v %+v", doc, v)
	}
	d.wantPending("both issued", "OM-1", "")
	one, err := d.dm().GetPendingItemsOfDocument(d.ctx, docapi.GetPendingItemsOfDocument{TenantID: d.tenant, DocumentID: h03})
	if err != nil || len(one) != 0 {
		t.Fatalf("the pending items of H03's bill: %+v %v", one, err)
	}

	// The carrier reports that CV-2 was loaded at hour 26, before the loading at 31 that dated H03's bill: SEA reopens
	// (the container's date changes, the movement's does not) and finalizes again. H01's bill is untouched, so it has
	// no item; H03's date is now wrong against SEA's facts: it needs an amendment, and the item says what changed.
	_, err = d.sea.RecordOceanCarrierFact(d.ctx, seaapi.RecordOceanCarrierFact{CommandID: "report-cv2", TenantID: d.tenant, ActorID: "line-1", MovementID: "OM-1",
		ExpectedRevision: d.movement().Revision, Kind: seaapi.CarrierLoaded, ContainerNumber: "TEMU1234565", Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line",
		Reference: "LD-CV2", SHA256: strings.Repeat("cd", 32), HappenedAt: d.hours(26), OccurredAt: d.hours(36)})
	d.must("the carrier's report naming CV-2", err)
	if m := d.movement(); m.State != seaapi.StateOpen {
		t.Fatalf("the report reopens the movement: %s", m.State)
	}
	d.wantPending("the report", "OM-1", "H01 SOURCE_NOT_FINAL open, H03 SOURCE_NOT_FINAL open")
	d.finalizeOf("OM-1", "fin-3", 37)
	if f := d.facts("OM-1"); f.Version != 3 || !f.Containers[1].OnBoardAt.Equal(d.hours(26)) || !f.OnBoardAt.Equal(d.hours(24)) {
		t.Fatalf("version 3: %+v", f)
	}
	d.wantPending("SEA finalized again with the report", "OM-1", "H03 ISSUED_CONTENT_CHANGED open")
	if one, err = d.dm().GetPendingItemsOfDocument(d.ctx, docapi.GetPendingItemsOfDocument{TenantID: d.tenant, DocumentID: h03}); err != nil || len(one) != 1 ||
		!strings.Contains(one[0].Message, "the on-board date") || !strings.Contains(one[0].Message, "amendment") {
		t.Fatalf("what changed for H03's bill: %+v %v", one, err)
	}
}

// h03LeavesC1 is PHY's side of H03 leaving CV-2's container before it is loaded: the stuffing X-2 is opened for rework
// and H03 unloaded (a closed consolidation lets a house go only when it has no live packs in it).
func (d *loadingDay) h03Unloaded(open, unload int) {
	d.t.Helper()
	_, err := d.phy.OpenStuffingForRework(d.ctx, phyapi.OpenStuffingForRework{CommandID: "rework-x2", TenantID: d.tenant, ActorID: "ops-asker", ExecutionID: "X-2",
		ExpectedRevision: d.revision["X-2"], Reason: "H03 goes to the next consolidation", OccurredAt: d.hours(open)})
	d.must("open the rework of X-2", err)
	d.revision["X-2"]++
	_, err = d.phy.UnloadHouseForRework(d.ctx, phyapi.UnloadHouseForRework{CommandID: "unload-h03", TenantID: d.tenant, ActorID: "wh-1", ExecutionID: "X-2",
		ExpectedRevision: d.revision["X-2"], ShipmentID: "SH-H03", Evidence: phyapi.Evidence{Source: "CFS-WMS", Reference: "UNSTUFF-H03", OccurredAt: d.hours(unload)}, OccurredAt: d.hours(unload)})
	d.must("unload H03", err)
	d.revision["X-2"]++
}

// beforeCV2Loads is the day up to H03's leaving: CV-1 loaded at 24, SEA's version 1 at 25, H01's bill issued at 27 and
// H03's bill drafted and approved on OM-1, and H03 unloaded from X-2.
func (d *loadingDay) beforeCV2Loads() (h01, h03 string) {
	d.load("CV-1", 24)
	d.finalizeOf("OM-1", "fin-1", 25)
	h01, h03 = d.prepare("SH-H01", "OM-1", 26), d.prepare("SH-H03", "OM-1", 26)
	d.must("H01's bill", d.issue(h01, 2, 27))
	d.wantPending("before H03 leaves", "OM-1", "H03 HOUSE_NOT_ALL_LOADED blocking")
	d.h03Unloaded(28, 29)
	return h01, h03
}

// FUL reads PHY's membership of H03 and SEA ends its association in OM-1, which reopens the movement; SEA then
// finalizes OM-1 again (version 2, without H03), at hours 31 to 33.
func (d *loadingDay) h03LeftOM1(record func(), endHour, finalizeHour int) {
	d.t.Helper()
	record()
	d.endH03(endHour)
	d.finalize("fin-2", finalizeHour)
}

func (d *loadingDay) newConsolidation2() {
	d.t.Helper()
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	departs := d.at.Add(72 * time.Hour)
	_, err := d.phy.CreateConsolidation(d.ctx, phyapi.CreateConsolidation{CommandID: "consol-2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", Mode: phyapi.ModeSea,
		Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"), PlannedDeparture: departs.Add(6 * time.Hour),
		HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: d.hours(8)})
	d.must("create C-2", err)
	_, err = d.phy.PlanLoadUnit(d.ctx, phyapi.PlanLoadUnit{CommandID: "plan-c2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", ExpectedRevision: 1,
		Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: d.hours(8)})
	d.must("plan the unit of C-2", err)
}

func (d *loadingDay) cvOf(id string) seaapi.ContainerVisit {
	v, err := d.sea.GetContainerVisit(d.ctx, seaapi.GetContainerVisit{TenantID: d.tenant, VisitID: id})
	d.must("visit", err)
	return v
}

// secondMovement builds OM-2 on C-2 with H03 alone in CV-3, loaded at hour 40, finalized at 41, after PHY's transfer
// of H03 to C-2 and FUL's recording of it.
func (d *loadingDay) secondMovement() {
	d.t.Helper()
	ctx := d.ctx
	slice{tenant: d.tenant, at: d.at, departs: d.at.Add(72 * time.Hour)}.bookSea(d.t, d.book, "BK-2", "C-2", "EVER GIVEN", "9811000", "0124E", d.at.Add(96*time.Hour))
	_, err := d.sea.OpenOceanMovement(ctx, seaapi.OpenOceanMovement{CommandID: "open-2", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-2", ConsolidationID: "C-2",
		BookingID: "BK-2", OccurredAt: d.hours(34)})
	d.must("open OM-2", err)
	m2 := func() int64 {
		m, err := d.sea.GetOceanMovement(ctx, seaapi.GetOceanMovement{TenantID: d.tenant, MovementID: "OM-2"})
		d.must("OM-2", err)
		return m.Revision
	}
	// PHY: C-2 is closed and H03's lot, opened again by the unloading, is stuffed into X-3 and sealed
	c2, err := d.phy.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: d.tenant, ConsolidationID: "C-2"})
	d.must("C-2", err)
	_, err = d.phy.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close-c2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", ExpectedRevision: c2.Revision,
		OccurredAt: d.hours(34)})
	d.must("close C-2", err)
	_, err = d.phy.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "stuff-x3", TenantID: d.tenant, ActorID: "cfs-1", ExecutionID: "X-3", ConsolidationID: "C-2",
		Kind: phyapi.ExecutionStuffing, Facility: d.cfs, LoadUnitNo: 1, OccurredAt: d.hours(35)})
	d.must("open X-3", err)
	_, err = d.phy.StartLoading(ctx, phyapi.StartLoading{CommandID: "start-x3", TenantID: d.tenant, ActorID: "cfs-1", ExecutionID: "X-3", ExpectedRevision: 1, OccurredAt: d.hours(35)})
	d.must("start X-3", err)
	d.revision["X-3"] = 2
	d.loadAt("X-3", "H03", 6, 35)
	d.sealAt("X-3", 36)
	// SEA: the house is associated with the HBL it has, FUL takes PHY's evidence, the container is taken as stuffed
	r, err := d.sea.AssociateHouseBill(ctx, seaapi.AssociateHouseBill{CommandID: "hbl-h03-om2", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-2", ExpectedRevision: m2(),
		ShipmentID: "SH-H03", OccurredAt: d.hours(36)})
	d.must("associate H03 in OM-2", err)
	_ = r
	d.must("FUL takes the evidence", d.fulTakes(37, "SH-H03"))
	_, err = d.sea.RefreshHouseBillFacts(ctx, seaapi.RefreshHouseBillFacts{CommandID: "refresh-h03-om2", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-2", ExpectedRevision: m2(),
		ShipmentID: "SH-H03", OccurredAt: d.hours(37)})
	d.must("refresh H03 in OM-2", err)
	a, err := d.sea.AssignContainer(ctx, seaapi.AssignContainer{CommandID: "assign-CV-3", TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-3", MovementID: "OM-2", LoadUnitNo: 1,
		ContainerNumber: "msku1234565", SizeType: "22G1", TareKg: 2180, MaxGrossKg: 30480, PlateSource: seaapi.PlateCSC, OccurredAt: d.hours(37)})
	d.must("assign CV-3", err)
	d.visit["CV-3"] = a.NewRevision
	d.must("seal CV-3", d.boxSeal("CV-3", "seal-x-3", 37))
	d.must("stuffing done CV-3", d.boxStuffingDone("CV-3", 38))
	d.must("VGM CV-3", d.boxVGM("CV-3", 2900, 38))
	_, err = d.sea.RecordMasterBill(ctx, seaapi.RecordMasterBill{CommandID: "mbl-2", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-2", ExpectedRevision: m2(), Number: "linu2611000999",
		Kind: seaapi.MasterBillCarrierMBL, Evidence: seaapi.Evidence{SHA256: strings.Repeat("9d", 32), StorageID: "mail/mbl2.pdf", MediaType: "application/pdf", CapturedAt: d.hours(38)},
		OccurredAt: d.hours(38)})
	d.must("master bill of OM-2", err)
	d.load("CV-3", 40)
	d.finalizeOf("OM-2", "fin-om2", 41)
}

// TestAHouseThatMovedToAnotherMovementTakesItsBillWithEveryContext is the second case: H03 is transferred to C-2 before
// CV-2 is loaded. PHY unloads it, FUL records its new consolidation, SEA ends its association in OM-1 and finalizes
// again, and OM-2 (C-2, booked with the same line from the same port: SEA refuses a house whose plan's route is not the
// movement's) carries it in CV-3, loaded at hour 40. H03's approved draft is refreshed onto OM-2 with the revise path
// (MovementID) and issued there: the same HBL number, OM-2's port of loading as the issue place, dated by CV-3's
// loading.
func TestAHouseThatMovedToAnotherMovementTakesItsBillWithEveryContext(t *testing.T) {
	d := newLoadingDay(t, "cargo-house-on-board-move-")
	h01, h03 := d.beforeCV2Loads()
	_ = h01
	d.newConsolidation2()
	_, err := d.phy.TransferHouse(d.ctx, phyapi.TransferHouse{CommandID: "transfer-h03", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-1", ToConsolidationID: "C-2",
		ShipmentID: "SH-H03", ExpectedMembershipRevision: 1, Reason: phyapi.ReasonWrongConsol, OccurredAt: d.hours(30)})
	d.must("transfer H03", err)
	d.h03LeftOM1(func() {
		plan, err := d.ful.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: "SH-H03"})
		d.must("FUL's plan of H03", err)
		_, err = d.ful.RecordConsolAttachment(d.ctx, fulapi.RecordConsolAttachment{CommandID: "ful-attach-c2", TenantID: d.tenant, ActorID: "ops-1", ShipmentID: "SH-H03",
			ExpectedRevision: plan.Revision, ConsolidationID: "C-2", OccurredAt: d.hours(31)})
		d.must("FUL records the attachment to C-2", err)
	}, 32, 33)
	if f := d.facts("OM-1"); f.Version != 2 || len(f.Houses) != 2 || len(f.Containers) != 2 {
		t.Fatalf("OM-1's version 2: %+v", f)
	}
	d.wantPending("OM-1 without H03", "OM-1", "H03 HOUSE_LEFT_MOVEMENT blocking")
	d.secondMovement()
	f2 := d.facts("OM-2")
	if f2.Version != 1 || f2.PortOfLoading != "CNSHG" || len(f2.Houses) != 1 || f2.Houses[0].HBL == "" || !f2.Containers[0].OnBoardAt.Equal(d.hours(40)) {
		t.Fatalf("OM-2's version 1: %+v", f2)
	}
	hbl := f2.Houses[0].HBL
	ref, err := d.sea.FindHouseBill(d.ctx, seaapi.FindHouseBill{TenantID: d.tenant, LegalEntityID: "LE", HBL: hbl})
	if err != nil || ref.ShipmentID != "SH-H03" || ref.MovementID != "OM-2" {
		t.Fatalf("the HBL moved with the shipment: %+v %v", ref, err)
	}
	// DOC finds the document through the movement its row names, still OM-1; OM-2 has none yet
	d.wantPending("H03's draft not yet moved", "OM-1", "H03 HOUSE_LEFT_MOVEMENT blocking")
	d.wantPending("OM-2 before the refresh", "OM-2", "")
	one, err := d.dm().GetPendingItemsOfDocument(d.ctx, docapi.GetPendingItemsOfDocument{TenantID: d.tenant, DocumentID: h03})
	if err != nil || len(one) != 1 || one[0].Kind != docapi.PendingHouseLeft || !one[0].Blocking || !strings.Contains(one[0].Message, "transferred") {
		t.Fatalf("the pending items of H03's bill: %+v %v", one, err)
	}
	refusedWith(t, "H03's bill on OM-1", d.issue(h03, 2, 42), "DOC_SOURCE_STALE")
	_, err = d.dm().RefreshDocumentDraft(d.ctx, docapi.RefreshDocumentDraft{CommandID: "refresh-h03-same", TenantID: d.tenant, ActorID: "doc-1", DocumentID: h03, ExpectedRevision: 2,
		Reason: "again", OccurredAt: d.hours(42)})
	refusedWith(t, "a refresh of H03's bill on OM-1", err, "DOC_MASTER_MISMATCH")

	// the refresh onto OM-2 with the revise path: same number, new movement and consolidation, the new port
	d.refreshBill(h03, 2, "OM-2", 42)
	doc, v := d.bill(h03)
	if doc.State != docapi.StateDraft || doc.MovementID != "OM-2" || doc.ConsolidationID != "C-2" || doc.Number != hbl || v.Doc.Source.MovementID != "OM-2" || v.Doc.IssuePlace.Code != f2.PortOfLoading ||
		v.Doc.PortOfLoading != f2.PortOfLoading || !v.Doc.OnBoardAt.Equal(d.hours(40)) || v.Doc.Containers[0].Number != "MSKU1234565" || v.Doc.MasterBill != "LINU2611000999" {
		t.Fatalf("the refreshed draft: %+v %+v", doc, v.Doc)
	}
	d.wantPending("H03's draft on OM-2", "OM-2", "")
	d.wantPending("OM-1 after the move", "OM-1", "")
	d.approve(h03, 3, 43)
	d.must("H03's bill on OM-2", d.issue(h03, 4, 44))
	if doc, v = d.bill(h03); doc.State != docapi.StateIssued || doc.MovementID != "OM-2" || v.Doc.Number != hbl || !v.Doc.OnBoardAt.Equal(d.hours(40)) || v.Doc.IssuePlace.Code != f2.PortOfLoading {
		t.Fatalf("H03's bill: %+v %+v", doc, v.Doc)
	}
	d.wantPending("H03 issued on OM-2", "OM-2", "")
	if doc, v = d.bill(h01); doc.State != docapi.StateIssued || doc.MovementID != "OM-1" || !v.Doc.OnBoardAt.Equal(d.hours(24)) {
		t.Fatalf("H01's bill stays: %+v %+v", doc, v.Doc)
	}
}

// TestAHouseThatWasOnlyDetachedHasItsBillCancelledWithEveryContext is the third case: H03 is only detached from C-1
// (not transferred). PHY unloads and detaches it, FUL records the detachment, SEA ends its association and finalizes
// again; DOC lists the bill's house as having left, and the bill (a draft approved, never issued) is cancelled with the
// existing command. The HBL number stays with the shipment in SEA: it names SH-H03 and no movement. DOC has bound
// nothing, since the bill was never issued.
func TestAHouseThatWasOnlyDetachedHasItsBillCancelledWithEveryContext(t *testing.T) {
	d := newLoadingDay(t, "cargo-house-on-board-detach-")
	_, h03 := d.beforeCV2Loads()
	hbl := d.facts("OM-1").Houses[2].HBL
	if d.facts("OM-1").Houses[2].ShipmentID != "SH-H03" {
		t.Fatalf("the houses: %+v", d.facts("OM-1").Houses)
	}
	_, err := d.phy.DetachHouse(d.ctx, phyapi.DetachHouse{CommandID: "detach-h03", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: "SH-H03",
		ExpectedMembershipRevision: 1, Reason: phyapi.ReasonSpaceShortage, OccurredAt: d.hours(30)})
	d.must("detach H03", err)
	d.h03LeftOM1(func() { d.fulTakesTheDetachment(31) }, 32, 33)
	d.wantPending("H03 left", "OM-1", "H03 HOUSE_LEFT_MOVEMENT blocking")
	one, err := d.dm().GetPendingItemsOfDocument(d.ctx, docapi.GetPendingItemsOfDocument{TenantID: d.tenant, DocumentID: h03})
	if err != nil || len(one) != 1 || one[0].Kind != docapi.PendingHouseLeft || !one[0].Blocking || !strings.Contains(one[0].Message, "detached") || !strings.Contains(one[0].Message, "cancels it") {
		t.Fatalf("the pending items of H03's bill: %+v %v", one, err)
	}
	refusedWith(t, "H03's bill after it left", d.issue(h03, 2, 34), "DOC_SOURCE_STALE")
	_, err = d.dm().CancelTransportDocument(d.ctx, docapi.CancelTransportDocument{CommandID: "cancel-h03", TenantID: d.tenant, ActorID: "doc-1", DocumentID: h03, ExpectedRevision: 2,
		Reason: "H03 was detached from the consolidation", OccurredAt: d.hours(35)})
	d.must("cancel H03's bill", err)
	if doc, _ := d.bill(h03); doc.State != docapi.StateCancelled || doc.CurrentVersion != 0 {
		t.Fatalf("cancelled: %+v", doc)
	}
	d.wantPending("H03's bill cancelled", "OM-1", "")
	if got, err := d.dm().GetPendingItemsOfDocument(d.ctx, docapi.GetPendingItemsOfDocument{TenantID: d.tenant, DocumentID: h03}); err != nil || len(got) != 0 {
		t.Fatalf("a cancelled bill has none: %+v %v", got, err)
	}
	// CV-2 is now empty: no house has packs in it. Its loading changes no house's date, so SEA leaves the movement
	// FINAL and H02, fully loaded in CV-1, is drafted and issued on the facts as they are.
	d.load("CV-2", 36)
	if m := d.movement(); m.State != seaapi.StateFinal || m.DocFactsVersion != 2 {
		t.Fatalf("the loading of an empty container: %+v", m)
	}
	h02 := d.prepare("SH-H02", "OM-1", 37)
	d.must("H02's bill", d.issue(h02, 2, 38))
	if doc, v := d.bill(h02); doc.State != docapi.StateIssued || !v.Doc.OnBoardAt.Equal(d.hours(24)) || v.Doc.Source.Version != 2 {
		t.Fatalf("H02's bill: %+v %+v", doc, v.Doc)
	}
	ref, err := d.sea.FindHouseBill(d.ctx, seaapi.FindHouseBill{TenantID: d.tenant, LegalEntityID: "LE", HBL: hbl})
	if err != nil || ref.ShipmentID != "SH-H03" || ref.MovementID != "" {
		t.Fatalf("the number stays with the shipment, in no movement: %+v %v", ref, err)
	}
}

// When the containers load together a house's date is the movement's: both containers are loaded at hour 24, and the
// bills of H01 (CV-1) and H03 (CV-2) are dated as the facts' on-board date is, hour 24.
func TestWhenTheContainersLoadTogetherTheHousesDateIsTheMovementsWithEveryContext(t *testing.T) {
	d := newLoadingDay(t, "cargo-house-on-board-together-")
	d.load("CV-1", 24)
	d.load("CV-2", 24)
	d.finalizeOf("OM-1", "fin-1", 25)
	f := d.facts("OM-1")
	if !f.OnBoardAt.Equal(d.hours(24)) || !f.Containers[0].OnBoardAt.Equal(d.hours(24)) || !f.Containers[1].OnBoardAt.Equal(d.hours(24)) {
		t.Fatalf("the facts: %+v", f)
	}
	h01, h03 := d.prepare("SH-H01", "OM-1", 26), d.prepare("SH-H03", "OM-1", 26)
	d.wantPending("both drafted", "OM-1", "")
	d.must("H01's bill", d.issue(h01, 2, 27))
	d.must("H03's bill", d.issue(h03, 2, 27))
	for _, id := range []string{h01, h03} {
		if doc, v := d.bill(id); doc.State != docapi.StateIssued || !v.Doc.OnBoardAt.Equal(f.OnBoardAt) || v.Doc.Source.Version != 1 {
			t.Fatalf("%s: %+v %+v", id, doc, v.Doc)
		}
	}
	d.wantPending("both issued", "OM-1", "")
}
