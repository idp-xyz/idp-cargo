//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func (allowAll) AuthorizeSeaAction(context.Context, string, string, string, string) error {
	return nil
}

// bookSea books an own-container SEA consolidation with shipping line line-1 for one 22G1 from Shanghai to Hamburg on a
// vessel and voyage departing etd, and records the line's confirmation with the VGM and shipping instruction cut-offs:
// requested and sent at 10 hours, confirmed at 11 hours after s.at.
func (s slice) bookSea(t *testing.T, book bookapi.Commands, id, consolidation, vessel, imo, voyage string, etd time.Time) {
	t.Helper()
	ctx, tenant := context.Background(), s.tenant
	hours := func(h int) time.Time { return s.at.Add(time.Duration(h) * time.Hour) }
	evidence := func(h int) bookapi.Evidence {
		return bookapi.Evidence{SHA256: strings.Repeat("6b", 32), StorageID: "mail/line.eml", MediaType: "message/rfc822", CapturedAt: hours(h)}
	}
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	box := bookapi.Capacity{Equipment: []bookapi.Equipment{{SizeType: "22G1", Count: 1}}}
	if _, err := book.DraftCarrierBooking(ctx, bookapi.DraftCarrierBooking{CommandID: "draft-" + id, TenantID: tenant, ActorID: "ops-1", BookingID: id,
		Kind: bookapi.KindSeaLCLConsolEquipment, LegalEntityID: "LE", CarrierPartyID: "line-1", ConsolidationID: consolidation,
		Request: bookapi.Request{Legs: []bookapi.RequestedLeg{{Loading: unlocode("CNSHA"), Discharge: unlocode("DEHAM"), DepartureDate: etd.Format(time.DateOnly)}},
			Capacity: box}, OccurredAt: hours(10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := book.RecordBookingRequestSent(ctx, bookapi.RecordBookingRequestSent{CommandID: "sent-" + id, TenantID: tenant, ActorID: "ops-1",
		BookingID: id, ExpectedRevision: 1, Channel: bookapi.ChannelCarrierPortal, Evidence: evidence(10), OccurredAt: hours(10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := book.RecordCarrierConfirmation(ctx, bookapi.RecordCarrierConfirmation{CommandID: "confirm-" + id, TenantID: tenant, ActorID: "ops-1",
		BookingID: id, ExpectedRevision: 2, Answer: bookapi.Answer{Channel: bookapi.ChannelEmail, Evidence: evidence(11),
			References: []bookapi.CarrierReference{{Type: bookapi.ReferenceBookingNumber, Value: "LINE-" + id}}},
		Lines: []bookapi.CapacityLine{{Capacity: box, Legs: []bookapi.ConfirmedLeg{{Loading: unlocode("CNSHA"), Discharge: unlocode("DEHAM"), Vessel: vessel,
			VesselIMO: imo, Voyage: voyage, ETD: etd, ETA: etd.Add(30 * 24 * time.Hour), CutOffs: []bookapi.CutOff{
				{Kind: bookapi.CutOffVGM, At: etd.Add(-36 * time.Hour), TimeZone: "Asia/Shanghai"},
				{Kind: bookapi.CutOffSIDocuments, At: etd.Add(-48 * time.Hour), TimeZone: "Asia/Shanghai"}}}}}}, OccurredAt: hours(11)}); err != nil {
		t.Fatal(err)
	}
}

// Sea freight part 1d with the real ORG, RATE, COM, FUL, PHY, BOOK and SEA, as the API client runs the steps (T6): two
// LCL orders are planned as SEA houses of an own-container consolidation, which is booked with a shipping line; SEA opens
// the ocean movement from the booking (vessel, voyage and cut-offs) and draws an HBL for each house from the legal
// entity's series. House M is shut out at the CFS: PHY transfers it to the next consolidation and FUL records it; SEA
// ends it in the first movement and associates it in the second with the HBL it has (scenario CW-GAP-E2E-02). House L is
// received, stuffed into the 22G1 and sealed; FUL relays the sealed container in OceanOperationPlan v1 and SEA's
// refreshed entry keeps it. The line states its master bill number, then rolls the container to the next vessel under
// the same booking: SEA relinks the voyage, and the house and the master bill stay.
func TestHouseBillsUnderTheOceanMovementWithEveryContext(t *testing.T) {
	ctx := context.Background()
	platform := openPlatform(t)
	orgModule, err := platform.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := platform.RATE(orgModule, allowAll{}, marginFloor(2000))
	if err != nil {
		t.Fatal(err)
	}
	comModule, err := platform.COM(orgModule, rateModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	fulModule, phyModule, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	bookModule, err := platform.BOOK(orgModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	seaModule, err := platform.SEA(bookModule, fulModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-hbl-")
	seedORG(t, platform, tenant, "party-l", "party-m", "line-1")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}
	must := func(r phyapi.Receipt, err error) phyapi.Receipt {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	movement := func(id string) seaapi.OceanMovement {
		t.Helper()
		m, err := seaModule.GetOceanMovement(ctx, seaapi.GetOceanMovement{TenantID: tenant, MovementID: id})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	// Houses: two planned SEA LCL shipments, attached to C-1 in PHY and recorded in FUL; C-2 is next week's container.
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	contexts.planSea(t, "party-l", "L", comapi.ContainerLCL, fulapi.ContainerLCL)
	contexts.planSea(t, "party-m", "M", comapi.ContainerLCL, fulapi.ContainerLCL)
	for id, departure := range map[string]time.Duration{"C-1": 6 * time.Hour, "C-2": 30 * time.Hour} {
		must(phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol-" + id, TenantID: tenant, ActorID: "ops-1", ConsolidationID: id,
			Mode: phyapi.ModeSea, Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"),
			PlannedDeparture: departs.Add(departure), HouseCutOff: departs.Add(departure - 12*time.Hour), OccurredAt: hours(8)}))
		must(phyModule.PlanLoadUnit(ctx, phyapi.PlanLoadUnit{CommandID: "plan-unit-" + id, TenantID: tenant, ActorID: "ops-1", ConsolidationID: id,
			ExpectedRevision: 1, Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: hours(8)}))
	}
	for _, s := range []string{"SH-L", "SH-M"} {
		must(phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-" + s, TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
			ShipmentID: s, OccurredAt: hours(9)}))
		record(t, ctx, fulModule, tenant, "ful-attach-"+s, s, "C-1", 3, hours(9))
	}

	// The shipping line: its CARRIER role in ORG, then each consolidation's container booking, confirmed with the vessel.
	if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-line-1", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "line-1", ExpectedRevision: 1, RoleCode: orgapi.RoleCarrier, ValidFrom: at.Add(-time.Hour), EvidenceRef: "service contract", OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	etd := departs.Add(6 * time.Hour)
	contexts.bookSea(t, bookModule, "BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", etd)
	// BOOK keeps the second vessel's name as the operator entered it, with a space after it; SEA keeps the name
	contexts.bookSea(t, bookModule, "BK-2", "C-2", "EVER GLORY ", "", "0124W", departs.Add(30*time.Hour))

	// SEA: the movement from the booking, the series, an HBL drawn for each house.
	if _, err = seaModule.OpenOceanMovement(ctx, seaapi.OpenOceanMovement{CommandID: "open-1", TenantID: tenant, ActorID: "ops-1", MovementID: "OM-1",
		ConsolidationID: "C-1", BookingID: "BK-1", OccurredAt: hours(11)}); err != nil {
		t.Fatal(err)
	}
	if _, err = seaModule.OpenOceanMovement(ctx, seaapi.OpenOceanMovement{CommandID: "open-wrong", TenantID: tenant, ActorID: "ops-1", MovementID: "OM-X",
		ConsolidationID: "C-2", BookingID: "BK-1", OccurredAt: hours(11)}); !errors.Is(err, seaapi.ErrRejected) {
		t.Fatalf("a booking allocated to another consolidation: %v", err)
	}
	if _, err = seaModule.RegisterHblSeries(ctx, seaapi.RegisterHblSeries{CommandID: "series", TenantID: tenant, ActorID: "ops-1", LegalEntityID: "LE",
		Template: "SHAL{seq:7}", FirstNumber: 1, OccurredAt: hours(11)}); err != nil {
		t.Fatal(err)
	}
	for i, s := range []string{"SH-L", "SH-M"} {
		r, err := seaModule.AssociateHouseBill(ctx, seaapi.AssociateHouseBill{CommandID: "hbl-" + s, TenantID: tenant, ActorID: "ops-1", MovementID: "OM-1",
			ExpectedRevision: int64(1 + i), ShipmentID: s, OccurredAt: hours(11)})
		if err != nil || r.HBL != []string{"SHAL0000001", "SHAL0000002"}[i] {
			t.Fatalf("HBL of %s: %+v %v", s, r, err)
		}
	}
	m := movement("OM-1")
	// the cut-offs as BOOK keeps them: by kind, at the leg's port of loading when the line named no other place
	cutOffs := map[string]seaapi.CutOff{}
	if len(m.Legs) == 1 {
		for _, c := range m.Legs[0].CutOffs {
			cutOffs[c.Kind] = c
		}
	}
	if vgm := cutOffs[bookapi.CutOffVGM]; len(cutOffs) != 2 || !vgm.At.Equal(etd.Add(-36*time.Hour)) || vgm.TimeZone != "Asia/Shanghai" ||
		vgm.Location != unlocode("CNSHG") || !cutOffs[bookapi.CutOffSIDocuments].At.Equal(etd.Add(-48*time.Hour)) {
		t.Fatalf("the cut-offs of the booking: %+v", m.Legs)
	}
	if m.Kind != seaapi.KindSeaLCLOwn || m.CarrierPartyID != "line-1" || m.CarrierRole != seaapi.RoleCarrier || m.Origin != unlocode("CNSHG") ||
		m.Destination != unlocode("DEHAM") || m.LocationEdition != "UNLOCODE-2026-11-01-S1" || len(m.Legs) != 1 || m.Legs[0].Vessel != "EVER GIVEN" ||
		m.Legs[0].VesselIMO != "9811000" || m.Legs[0].Voyage != "0123W" || !m.Legs[0].ETD.Equal(etd) ||
		len(m.Houses) != 2 || m.Packs != 12 || m.GrossWeight != (kernel.Quantity{Unit: "GRM", Amount: 600_000}) || m.Houses[0].Basis != seaapi.BasisPlanned ||
		m.Houses[0].ContainerMode != "LCL" || m.Houses[0].Parts[0].PackageType != "CT" || m.Houses[0].Parts[0].GoodsDescription != "Cartons of furniture parts" ||
		m.Houses[0].WindowDrift {
		t.Fatalf("the movement and its houses: %+v", m)
	}

	// House M is shut out at the CFS: PHY transfers it to C-2 and FUL records it; SEA ends it under OM-1 and associates
	// it under C-2's movement with the HBL it has.
	if _, err = seaModule.EndHouseBillAssociation(ctx, seaapi.EndHouseBillAssociation{CommandID: "end-M-early", TenantID: tenant, ActorID: "ops-1",
		MovementID: "OM-1", ExpectedRevision: 3, ShipmentID: "SH-M", OccurredAt: hours(12)}); !errors.Is(err, seaapi.ErrRejected) {
		t.Fatalf("ending a house FUL still attaches: %v", err)
	}
	must(phyModule.TransferHouse(ctx, phyapi.TransferHouse{CommandID: "shut-out-M", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ToConsolidationID: "C-2", ShipmentID: "SH-M", ExpectedMembershipRevision: 1, Reason: phyapi.ReasonShutOut, OccurredAt: hours(12)}))
	record(t, ctx, fulModule, tenant, "ful-shut-out-M", "SH-M", "C-2", 4, hours(12))
	if _, err = seaModule.RefreshHouseBillFacts(ctx, seaapi.RefreshHouseBillFacts{CommandID: "refresh-M-gone", TenantID: tenant, ActorID: "ops-1",
		MovementID: "OM-1", ExpectedRevision: 3, ShipmentID: "SH-M", OccurredAt: hours(12)}); !errors.Is(err, seaapi.ErrRejected) {
		t.Fatalf("refreshing a house FUL attaches elsewhere: %v", err)
	}
	if _, err = seaModule.EndHouseBillAssociation(ctx, seaapi.EndHouseBillAssociation{CommandID: "end-M", TenantID: tenant, ActorID: "ops-1",
		MovementID: "OM-1", ExpectedRevision: 3, ShipmentID: "SH-M", OccurredAt: hours(12)}); err != nil {
		t.Fatal(err)
	}
	if _, err = seaModule.OpenOceanMovement(ctx, seaapi.OpenOceanMovement{CommandID: "open-2", TenantID: tenant, ActorID: "ops-1", MovementID: "OM-2",
		ConsolidationID: "C-2", BookingID: "BK-2", OccurredAt: hours(12)}); err != nil {
		t.Fatal(err)
	}
	if _, err = seaModule.AssociateHouseBill(ctx, seaapi.AssociateHouseBill{CommandID: "hbl-L-wrong", TenantID: tenant, ActorID: "ops-1", MovementID: "OM-2",
		ExpectedRevision: 1, ShipmentID: "SH-L", OccurredAt: hours(13)}); !errors.Is(err, seaapi.ErrRejected) {
		t.Fatalf("a house FUL attaches to another consolidation: %v", err)
	}
	r, err := seaModule.AssociateHouseBill(ctx, seaapi.AssociateHouseBill{CommandID: "hbl-M-2", TenantID: tenant, ActorID: "ops-1", MovementID: "OM-2",
		ExpectedRevision: 1, ShipmentID: "SH-M", OccurredAt: hours(13)})
	if err != nil || r.HBL != "SHAL0000002" {
		t.Fatalf("SH-M under C-2's movement keeps its HBL: %+v %v", r, err)
	}
	ref, err := seaModule.FindHouseBill(ctx, seaapi.FindHouseBill{TenantID: tenant, LegalEntityID: "LE", HBL: "SHAL0000002"})
	if err != nil || ref.ShipmentID != "SH-M" || ref.MovementID != "OM-2" {
		t.Fatalf("SHAL0000002: %+v %v", ref, err)
	}

	// House L: the cut-off, the receipt at the CFS, the stuffing into the 22G1 and the seal; FUL takes the evidence.
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	if err != nil {
		t.Fatal(err)
	}
	must(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: c.Revision, OccurredAt: hours(13)}))
	cfs := phyapi.Facility{Location: c.Current.Origin, Code: "SHA-CFS1"}
	must(phyModule.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv", TenantID: tenant, ActorID: "cfs-1", LotID: "L-L", ShipmentID: "SH-L",
		Facility: cfs, Packs: 6, PackageType: "CT", Condition: phyapi.ConditionGood, Evidence: evidence("CFS-WMS", "GRN-L", 14), OccurredAt: hours(14)}))
	must(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh", TenantID: tenant, ActorID: "cfs-1", LotID: "L-L",
		ExpectedRevision: 1, GrossWeight: kg(312), Method: phyapi.MethodScale, Evidence: evidence("CFS-SCALE", "W-L", 14), OccurredAt: hours(14)}))
	must(phyModule.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete", TenantID: tenant, ActorID: "cfs-1", LotID: "L-L",
		ExpectedRevision: 2, OccurredAt: hours(14)}))
	must(phyModule.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "stuff", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
		ConsolidationID: "C-1", Kind: phyapi.ExecutionStuffing, Facility: cfs, LoadUnitNo: 1, OccurredAt: hours(15)}))
	must(phyModule.StartLoading(ctx, phyapi.StartLoading{CommandID: "start", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1", ExpectedRevision: 1,
		OccurredAt: hours(15)}))
	must(phyModule.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
		ExpectedRevision: 2, LotID: "L-L", Packs: 6, Evidence: evidence("CFS-TALLY", "T-L", 15), OccurredAt: hours(15)}))
	must(phyModule.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "seal", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
		ExpectedRevision: 3, Evidence: evidence(phyapi.EvidenceSeal, "CN-SEAL-77120", 15), OccurredAt: hours(15)}))
	refresh := func(command string, revision int64) (seaapi.Receipt, error) {
		return seaModule.RefreshHouseBillFacts(ctx, seaapi.RefreshHouseBillFacts{CommandID: command, TenantID: tenant, ActorID: "ops-1", MovementID: "OM-1",
			ExpectedRevision: revision, ShipmentID: "SH-L", OccurredAt: hours(16)})
	}
	if _, err = refresh("refresh-L-early", 4); !errors.Is(err, seaapi.ErrRejected) {
		t.Fatalf("a refresh before FUL recorded the evidence: %v", err)
	}
	if _, err = fulModule.RecordVerifiedLoadEvidence(ctx, fulapi.RecordVerifiedLoadEvidence{CommandID: "ful-stuffed", TenantID: tenant, ActorID: "ops-1",
		ShipmentID: "SH-L", ExpectedRevision: 4, OccurredAt: hours(16)}); err != nil {
		t.Fatal(err)
	}
	if r, err = refresh("refresh-L", 4); err != nil || r.HBL != "SHAL0000001" || r.EntryNo != 3 {
		t.Fatalf("the refreshed house: %+v %v", r, err)
	}
	plan, err := fulModule.GetOceanOperationPlan(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-L"})
	if err != nil || len(plan.Stuffings) != 1 {
		t.Fatalf("FUL's OceanOperationPlan v1 of SH-L: %+v %v", plan, err)
	}
	m = movement("OM-1")
	want := seaapi.HouseContainer{ExecutionID: "X-1", LoadUnitNo: 1, TypeCode: "22G1", Packs: 6, GrossWeight: kg(312), LineNos: []int{1},
		Seal: "CN-SEAL-77120", SealedAt: hours(15)}
	if len(m.Houses) != 1 || m.Houses[0].ShipmentID != "SH-L" || m.Houses[0].HBL != "SHAL0000001" || m.Houses[0].Basis != seaapi.BasisTendered ||
		m.Houses[0].EvidenceVersion != plan.EvidenceVersion || m.Houses[0].Packs != 6 || m.Houses[0].GrossWeight != kg(312) ||
		len(m.Houses[0].Containers) != 1 || !reflect.DeepEqual(m.Houses[0].Containers[0], want) || m.Packs != 6 ||
		m.GrossWeight != (kernel.Quantity{Unit: "GRM", Amount: 312_000}) {
		t.Fatalf("house L in its sealed container: %+v", m.Houses)
	}

	// The container: SEA assigns the one the line released to PHY's unit 1, records the seal the CFS applied, with
	// the shipper as its party, and that the container is stuffed, against PHY's stuffing as FUL relays it. The house
	// then names its container. The movement's revision does not move: a visit has its own.
	if _, err = seaModule.RecordStuffingDone(ctx, seaapi.RecordStuffingDone{CommandID: "stuffed-unknown", TenantID: tenant, ActorID: "cfs-1", VisitID: "CV-1",
		ExpectedRevision: 1, OccurredAt: hours(16)}); !errors.Is(err, seaapi.ErrNotFound) {
		t.Fatalf("a container not assigned yet: %v", err)
	}
	if r, err = seaModule.AssignContainer(ctx, seaapi.AssignContainer{CommandID: "assign", TenantID: tenant, ActorID: "ops-1", VisitID: "CV-1",
		MovementID: "OM-1", LoadUnitNo: 1, ContainerNumber: "csqu3054383", SizeType: "22G1", TareKg: 2180, MaxGrossKg: 30480, PlateSource: seaapi.PlateCSC,
		OccurredAt: hours(12)}); err != nil || r.VisitID != "CV-1" || r.NewRevision != 1 {
		t.Fatalf("the container assigned: %+v %v", r, err)
	}
	if _, err = seaModule.RecordStuffingDone(ctx, seaapi.RecordStuffingDone{CommandID: "stuffed-unsealed", TenantID: tenant, ActorID: "cfs-1", VisitID: "CV-1",
		ExpectedRevision: 1, OccurredAt: hours(16)}); !errors.Is(err, seaapi.ErrRejected) {
		t.Fatalf("stuffing done without a seal: %v", err)
	}
	if r, err = seaModule.RecordContainerSeal(ctx, seaapi.RecordContainerSeal{CommandID: "sea-seal", TenantID: tenant, ActorID: "cfs-1", VisitID: "CV-1",
		ExpectedRevision: 1, Number: "cn-seal-77120", Party: seaapi.SealShipper, AppliedAt: hours(15), Source: "CFS tally T-L", OccurredAt: hours(15)}); err != nil ||
		r.SealNo != 1 {
		t.Fatalf("the seal: %+v %v", r, err)
	}
	if r, err = seaModule.RecordStuffingDone(ctx, seaapi.RecordStuffingDone{CommandID: "stuffed", TenantID: tenant, ActorID: "cfs-1", VisitID: "CV-1",
		ExpectedRevision: 2, OccurredAt: hours(16)}); err != nil || r.NewRevision != 3 {
		t.Fatalf("stuffing done: %+v %v", r, err)
	}
	m = movement("OM-1")
	if m.Revision != 5 || len(m.Containers) != 1 || m.Containers[0].Status != seaapi.VisitStuffed || m.Containers[0].ExecutionID != "X-1" ||
		m.Containers[0].ContainerNumber != "CSQU3054383" || m.Containers[0].SealNumber != "CN-SEAL-77120" || m.Containers[0].SealParty != seaapi.SealShipper ||
		m.Containers[0].DifferenceReason != "" || m.Containers[0].LoadUnitNo != 1 || m.Houses[0].Containers[0].ContainerNumber != "CSQU3054383" {
		t.Fatalf("house L in container CSQU3054383: %+v", m)
	}
	if next := movement("OM-2"); len(next.Containers) != 0 {
		t.Fatalf("the next movement has no container yet: %+v", next.Containers)
	}

	// The line states its master bill number; then it rolls the container to the next vessel under the same booking.
	bill := seaapi.Evidence{SHA256: strings.Repeat("9c", 32), StorageID: "mail/draft-mbl.pdf", MediaType: "application/pdf", CapturedAt: hours(17)}
	if _, err = seaModule.RecordMasterBill(ctx, seaapi.RecordMasterBill{CommandID: "mbl", TenantID: tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 5, Number: "linu2611000457", Kind: seaapi.MasterBillCarrierMBL, Evidence: bill, OccurredAt: hours(17)}); err != nil {
		t.Fatal(err)
	}
	rolled := etd.Add(7 * 24 * time.Hour)
	if _, err = bookModule.RecordCarrierChange(ctx, bookapi.RecordCarrierChange{CommandID: "roll", TenantID: tenant, ActorID: "ops-1", BookingID: "BK-1",
		ExpectedRevision: 3, Reason: bookapi.CapacityRollover, Answer: bookapi.Answer{Channel: bookapi.ChannelEmail,
			Evidence: bookapi.Evidence{SHA256: strings.Repeat("6b", 32), StorageID: "mail/roll.eml", MediaType: "message/rfc822", CapturedAt: hours(18)}},
		Lines: []bookapi.CapacityLine{{Capacity: bookapi.Capacity{Equipment: []bookapi.Equipment{{SizeType: "22G1", Count: 1}}},
			Legs: []bookapi.ConfirmedLeg{{Loading: unlocode("CNSHA"), Discharge: unlocode("DEHAM"), Vessel: "EVER GOLDEN", Voyage: "0125W", ETD: rolled,
				ETA: rolled.Add(30 * 24 * time.Hour)}}}}, OccurredAt: hours(18)}); err != nil {
		t.Fatal(err)
	}
	if _, err = seaModule.LinkCarrierBooking(ctx, seaapi.LinkCarrierBooking{CommandID: "relink", TenantID: tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 6, OccurredAt: hours(18)}); err != nil {
		t.Fatal(err)
	}
	m = movement("OM-1")
	if m.Revision != 7 || m.CapacityVersion != 2 || len(m.Legs) != 2 || !m.Legs[0].Current || m.Legs[0].Vessel != "EVER GOLDEN" || !m.Legs[0].ETD.Equal(rolled) ||
		len(m.Legs[0].CutOffs) != 0 || m.Legs[1].Vessel != "EVER GIVEN" || m.Legs[1].Current || len(m.Houses) != 1 || !m.Houses[0].WindowDrift ||
		m.MasterBillNumber != "LINU2611000457" || m.MasterBills[0].Evidence != bill {
		t.Fatalf("rolled to the next vessel: %+v", m)
	}
	byBill, err := seaModule.FindOceanMovementByMasterBill(ctx, seaapi.FindOceanMovementByMasterBill{TenantID: tenant, IssuerPartyID: "line-1",
		Number: "LINU2611000457"})
	if err != nil || byBill.MovementID != "OM-1" {
		t.Fatalf("by the master bill number: %+v %v", byBill, err)
	}
	if next := movement("OM-2"); len(next.Houses) != 1 || next.Houses[0].ShipmentID != "SH-M" || next.Houses[0].HBL != "SHAL0000002" ||
		next.Legs[0].Vessel != "EVER GLORY" || next.Houses[0].Basis != seaapi.BasisPlanned {
		t.Fatalf("the next movement with house M: %+v", next)
	}
}
