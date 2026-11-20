package application

import (
	"context"
	"errors"
	"testing"
	"time"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// evidenceFake answers PHY's evidence as set and notes whether it was asked inside a transaction.
type evidenceFake struct {
	tx            *txFake
	evidence      ful.LoadEvidence
	err           error
	calls         int
	consolidation string
	air           bool
	insideTx      bool
	during        func() // runs while PHY is asked
}

func (e *evidenceFake) Evidence(_ context.Context, _ ful.TenantID, _ ful.ShipmentID, consolidation string, air bool) (ful.LoadEvidence, error) {
	e.calls++
	e.insideTx = e.insideTx || e.tx.depth > 0
	e.consolidation, e.air = consolidation, air
	if e.during != nil {
		e.during()
	}
	out := e.evidence
	out.ConsolidationID = consolidation
	if air && out.Lots > 0 {
		out.Security = ful.SecurityPending // as the adapter derives it for air only
	}
	return out, e.err
}

func attachedShipment(t *testing.T, f *fixture, cancel bool) {
	t.Helper()
	attachedShipmentIn(t, f, cancel, false)
}

func attachedShipmentIn(t *testing.T, f *fixture, cancel, air bool) {
	t.Helper()
	ob := ful.ObligationRef{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", CustomerPartyID: "P", LegalEntityID: "LE", Packs: 4, PackageType: "CT",
		GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 400}}
	s, err := ful.CreateShipment("T", "SH-1", ob, "planner-1", now)
	if err != nil {
		t.Fatal(err)
	}
	route := ful.Route{Mode: ful.ModeSea, Origin: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHG"}, Destination: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "DEHAM"},
		Departure: kernel.ValidityInterval{From: now.Add(72 * time.Hour), Until: now.Add(120 * time.Hour)}, LocationEdition: "E1"}
	if air {
		route = ful.Route{Mode: ful.ModeAir, Origin: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHA"}, Destination: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "USNYC"},
			Departure: route.Departure, OriginIATA: "PVG", DestinationIATA: "JFK", LocationEdition: "E1"}
	}
	if err = errors.Join(s.AmendPlan(route, "booked", "planner-1", now), s.Confirm("planner-1", now),
		s.RecordConsolAttachment(ful.Membership{ConsolidationID: "C-1", EntryNo: 1, Revision: 1, AttachedAt: now, Mode: route.Mode, Origin: route.Origin,
			Destination: route.Destination}, "ops", now)); err != nil {
		t.Fatal(err)
	}
	if cancel {
		if err = s.Cancel("withdrawn", "ops", now); err != nil {
			t.Fatal(err)
		}
	}
	f.store.shipments[s.ID()], f.store.revs[s.ID()] = s.State(), 3
}

// GetEvidenceCurrency reads the shipment for its consolidation, asks PHY outside a transaction, compares by the
// domain's comparison and writes nothing: the revision and the stored shipment stay.
func TestGetEvidenceCurrencyAsksLikeTheCommandAndWritesNothing(t *testing.T) {
	f := newFixture()
	attachedShipment(t, f, false)
	phy := &evidenceFake{tx: f.store.tx, evidence: ful.LoadEvidence{Lots: 1, ReceiptComplete: true, Received: ful.CargoScope{Packs: 4,
		GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 410}}}}
	h := GetEvidenceCurrency{Tx: f.deps.Tx, Shipments: f.store, Evidence: phy}
	got, err := h.Handle(context.Background(), "T", "SH-1")
	if err != nil || !got.NewVersionDue || got.EvidenceVersion != 0 || got.Revision != 3 || got.Status != ful.ShipmentPlanned {
		t.Fatalf("no evidence recorded yet: %+v %v", got, err)
	}
	if phy.calls != 1 || phy.insideTx || phy.consolidation != "C-1" || phy.air {
		t.Fatalf("PHY is asked once, outside a transaction, for the attached consolidation: %+v", phy)
	}
	// FUL records it (the command's own path): the question then says nothing is due
	rec := RecordVerifiedLoadEvidence{Deps: f.deps, Evidence: phy}
	if _, err = rec.Handle(context.Background(), RecordVerifiedLoadEvidenceCommand{CommandID: "ev-1", Scope: planner, ShipmentID: "SH-1", ExpectedRevision: 3,
		OccurredAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	revision := f.store.revs["SH-1"]
	if got, err = h.Handle(context.Background(), "T", "SH-1"); err != nil || got.NewVersionDue || got.EvidenceVersion != 1 {
		t.Fatalf("recorded: %+v %v", got, err)
	}
	phy.evidence.Tendered = ful.CargoScope{Packs: 4, GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 412}}
	if got, err = h.Handle(context.Background(), "T", "SH-1"); err != nil || !got.NewVersionDue || got.EvidenceVersion != 1 {
		t.Fatalf("PHY has newer facts: %+v %v", got, err)
	}
	if f.store.revs["SH-1"] != revision || len(f.store.shipments["SH-1"].Evidence) != 1 {
		t.Fatalf("the question wrote: revision %d, %d versions", f.store.revs["SH-1"], len(f.store.shipments["SH-1"].Evidence))
	}

	// PHY not answering is PHY's error, not an answer
	phy.err = ErrEvidenceUnavailable
	if _, err = h.Handle(context.Background(), "T", "SH-1"); !errors.Is(err, ErrEvidenceUnavailable) {
		t.Fatalf("PHY unavailable: %v", err)
	}
	if _, err = (GetEvidenceCurrency{Tx: f.deps.Tx, Shipments: f.store}).Handle(context.Background(), "T", "SH-1"); !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("without PHY's evidence: %v", err)
	}
	if _, err = h.Handle(context.Background(), "T", "SH-NO_SUCH"); err == nil {
		t.Fatal("an unknown shipment")
	}
}

func TestGetEvidenceCurrencyOfACancelledShipmentIsNotDueAndAsksNoOne(t *testing.T) {
	f := newFixture()
	attachedShipment(t, f, true)
	phy := &evidenceFake{tx: f.store.tx}
	got, err := (GetEvidenceCurrency{Tx: f.deps.Tx, Shipments: f.store, Evidence: phy}).Handle(context.Background(), "T", "SH-1")
	if err != nil || got.NewVersionDue || got.Status != ful.ShipmentCancelled || phy.calls != 0 {
		t.Fatalf("cancelled: %+v %v, PHY asked %d times", got, err, phy.calls)
	}
}

// The consolidation FUL records is the one PHY is asked for: when the attachment changes while PHY is asked, the answer
// is the conflict the command gives, not a comparison of evidence read for another consolidation.
func TestGetEvidenceCurrencyIsAConflictWhenTheAttachmentChangesWhilePHYIsAsked(t *testing.T) {
	f := newFixture()
	attachedShipment(t, f, false)
	phy := &evidenceFake{tx: f.store.tx, evidence: ful.LoadEvidence{Lots: 1, ReceiptComplete: true, Received: ful.CargoScope{Packs: 4, GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 410}}}}
	phy.during = func() {
		s, err := ful.RestoreShipment(f.store.shipments["SH-1"])
		if err != nil {
			t.Error(err)
			return
		}
		route := s.Latest().Route
		if err = s.RecordConsolAttachment(ful.Membership{ConsolidationID: "C-2", EntryNo: 2, Revision: 2, AttachedAt: now, Mode: ful.ModeSea, Origin: route.Origin,
			Destination: route.Destination}, "ops", now); err != nil {
			t.Error(err)
			return
		}
		f.store.shipments["SH-1"] = s.State()
	}
	if _, err := (GetEvidenceCurrency{Tx: f.deps.Tx, Shipments: f.store, Evidence: phy}).Handle(context.Background(), "T", "SH-1"); !errors.Is(err, ful.ErrConflict) {
		t.Fatalf("the attachment changed while PHY was asked: %v", err)
	}
}

// The air flag reaches PHY's evidence (the adapter sums the screenings and the security state for air only) and the
// answer depends on it: evidence recorded for an air shipment as the command records it is current for the query, and a
// query that read PHY as for sea would see other evidence and say a new version is due.
func TestGetEvidenceCurrencyReadsAnAirShipmentAsAir(t *testing.T) {
	f := newFixture()
	attachedShipmentIn(t, f, false, true)
	phy := &evidenceFake{tx: f.store.tx, evidence: ful.LoadEvidence{Lots: 1, ReceiptComplete: true, Received: ful.CargoScope{Packs: 4, GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 410}}}}
	if _, err := (RecordVerifiedLoadEvidence{Deps: f.deps, Evidence: phy}).Handle(context.Background(), RecordVerifiedLoadEvidenceCommand{CommandID: "ev-air", Scope: planner,
		ShipmentID: "SH-1", ExpectedRevision: 3, OccurredAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if !phy.air || f.store.shipments["SH-1"].Evidence[0].Evidence.Security != ful.SecurityPending {
		t.Fatalf("the command read PHY for air: %v %+v", phy.air, f.store.shipments["SH-1"].Evidence[0].Evidence)
	}
	phy.air = false
	got, err := (GetEvidenceCurrency{Tx: f.deps.Tx, Shipments: f.store, Evidence: phy}).Handle(context.Background(), "T", "SH-1")
	if err != nil || got.NewVersionDue || !phy.air {
		t.Fatalf("the query reads PHY for air and finds the recorded evidence current: %+v %v air %v", got, err, phy.air)
	}
}
