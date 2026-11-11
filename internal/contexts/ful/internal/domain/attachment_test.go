package domain

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// planned returns a confirmed SEA shipment from Shanghai to Hamburg.
func planned(t *testing.T) (*Shipment, Route) {
	t.Helper()
	at := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	ob := ObligationRef{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", CustomerPartyID: "P", LegalEntityID: "LE", Packs: 4,
		PackageType: "CT", GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 400}}
	s, err := CreateShipment("T", "SH-1", ob, "planner-1", at)
	if err != nil {
		t.Fatal(err)
	}
	route := Route{Mode: ModeSea, Origin: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHG"},
		Destination: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "DEHAM"},
		Departure:   kernel.ValidityInterval{From: at.Add(72 * time.Hour), Until: at.Add(120 * time.Hour)}, LocationEdition: "E1"}
	if err = errors.Join(s.AmendPlan(route, "booked", "planner-1", at), s.Confirm("planner-1", at)); err != nil {
		t.Fatal(err)
	}
	return s, route
}

// FUL V0.4: the shipment records the consolidation PHY attached it to and never decides it; a transfer or
// refresh ends the previous attachment, a cancelled shipment ends its own, and the history restores.
func TestConsolAttachmentRecordsPHYMembership(t *testing.T) {
	s, route := planned(t)
	at := s.Latest().At.Add(time.Hour)
	m := Membership{ConsolidationID: "C-1", EntryNo: 1, Revision: 1, AttachedAt: at, Mode: ModeSea, Origin: route.Origin, Destination: route.Destination}
	other := m
	other.Destination = kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "NLRTM"}
	if err := s.RecordConsolAttachment(other, "planner-1", at); !errors.Is(err, ErrConsolPlanMismatch) {
		t.Fatalf("a consolidation to another port: %v", err)
	}
	air := m
	air.Mode = ModeAir
	if err := s.RecordConsolAttachment(air, "planner-1", at); !errors.Is(err, ErrConsolPlanMismatch) {
		t.Fatalf("an AIR consolidation for a SEA shipment: %v", err)
	}
	if err := s.RecordConsolAttachment(m, "planner-1", at.Add(-time.Minute)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("recorded before PHY attached it: %v", err)
	}
	if err := s.RecordConsolAttachment(m, "planner-1", at); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordConsolAttachment(m, "planner-1", at); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("the same entry twice: %v", err)
	}
	refreshed := m
	refreshed.EntryNo, refreshed.Revision = 2, 2
	if err := s.RecordConsolAttachment(refreshed, "planner-1", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	moved := refreshed
	moved.ConsolidationID, moved.EntryNo, moved.Revision = "C-2", 3, 3
	if err := s.RecordConsolAttachment(moved, "planner-1", at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordConsolDetachment("C-1", AttachmentDetached, "planner-1", at.Add(3*time.Hour)); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("detach from a consolidation the shipment left: %v", err)
	}
	if err := s.RecordConsolDetachment("C-2", AttachmentRefreshed, "planner-1", at.Add(3*time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a refresh is no detachment: %v", err)
	}
	if err := s.Cancel("customer withdrew", "planner-1", at.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	st := s.State()
	if len(st.Attachments) != 3 || st.Attachments[0].EndReason != AttachmentRefreshed || st.Attachments[1].EndReason != AttachmentTransferred ||
		st.Attachments[2].EndReason != AttachmentShipmentCancelled {
		t.Fatalf("attachments: %+v", st.Attachments)
	}
	if _, ok := s.Attachment(StageMain); ok {
		t.Fatal("a cancelled shipment keeps no attachment")
	}
	s.Stamp(at.Add(5 * time.Hour))
	if _, err := RestoreShipment(s.State()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st = s.State()
	st.Attachments[2].EndedAt, st.Attachments[2].EndedBy, st.Attachments[2].EndReason = time.Time{}, "", ""
	if _, err := RestoreShipment(st); err == nil {
		t.Fatal("a cancelled shipment with a current attachment must not restore")
	}
}
