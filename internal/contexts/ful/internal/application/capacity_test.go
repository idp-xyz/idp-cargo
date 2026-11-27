package application

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// linkedFake lists the stored shipment ids in order, a page after after at a time, as the store does.
type linkedFake struct {
	ids   []ful.ShipmentID
	pages int
}

func (l *linkedFake) LinkedShipments(_ context.Context, _ ful.TenantID, _ string, after ful.ShipmentID, limit int) ([]ful.ShipmentID, error) {
	l.pages++
	var out []ful.ShipmentID
	for _, id := range l.ids {
		if id > after && len(out) < limit {
			out = append(out, id)
		}
	}
	return out, nil
}

// linkedShipments stores n shipments of C-1, each linked to BK-1; those for which outside says so depart after their
// window and await the customer.
func linkedShipments(t *testing.T, n int, outside func(i int) bool) (*storeFake, *linkedFake) {
	t.Helper()
	store := &storeFake{tx: &txFake{}, shipments: map[ful.ShipmentID]ful.ShipmentState{}, revs: map[ful.ShipmentID]int64{}}
	linked := &linkedFake{}
	origin, destination := kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHG"}, kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "DEHAM"}
	window := kernel.ValidityInterval{From: now.Add(72 * time.Hour), Until: now.Add(120 * time.Hour)}
	for i := range n {
		id := ful.ShipmentID(fmt.Sprintf("SH-%04d", i))
		ob := ful.ObligationRef{ObligationID: ful.ObligationID("OB-" + string(id)), ObligationVersion: 1, OrderID: "O-1", CustomerPartyID: "P", LegalEntityID: "LE",
			Packs: 4, PackageType: "CT", GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 400}}
		s, err := ful.CreateShipment("T", id, ob, "planner-1", now)
		if err != nil {
			t.Fatal(err)
		}
		route := ful.Route{Mode: ful.ModeSea, Origin: origin, Destination: destination, Departure: window, LocationEdition: "E1"}
		etd := window.From.Add(time.Hour)
		if outside(i) {
			etd = window.Until.Add(48 * time.Hour)
		}
		capacity := ful.Capacity{BookingID: "BK-1", BookingRevision: 3, Mode: ful.ModeSea, ConsolidationID: "C-1", CapacityVersion: 2, Line: 1,
			Legs: []ful.CapacityLeg{{Loading: origin, Discharge: destination, Vessel: "EVER GLORY", Voyage: "0130W", ETD: etd}}}
		_, linkErr := func() (bool, error) {
			if err := s.AmendPlan(route, "booked", "planner-1", now); err != nil {
				return false, err
			}
			if err := s.Confirm("planner-1", now); err != nil {
				return false, err
			}
			if err := s.RecordConsolAttachment(ful.Membership{ConsolidationID: "C-1", EntryNo: 1, Revision: 1, AttachedAt: now, Mode: ful.ModeSea,
				Origin: origin, Destination: destination}, "ops-1", now); err != nil {
				return false, err
			}
			return s.LinkCapacity(capacity, "ops-1", now)
		}()
		if linkErr != nil {
			t.Fatal(linkErr)
		}
		store.shipments[id], store.revs[id] = s.State(), 5
		linked.ids = append(linked.ids, id)
	}
	return store, linked
}

// The work list reads the linked shipments a page at a time: shipments awaiting the customer past the first page are
// listed, by id, and the list stops at its 200.
func TestTheWorkListReadsEveryPageOfLinkedShipments(t *testing.T) {
	ctx := context.Background()
	store, linked := linkedShipments(t, 450, func(i int) bool { return i%3 == 0 })
	got, err := ListDateChangesAwaitingCustomer{Tx: store.tx, Shipments: store, Linked: linked}.Handle(ctx, "T", "C-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 150 || linked.pages != 3 || got[0].Shipment.ID() != "SH-0000" || got[149].Shipment.ID() != "SH-0447" ||
		!slices.IsSortedFunc(got, func(a, b AwaitingCustomer) int { return compare(a.Shipment.ID(), b.Shipment.ID()) }) {
		t.Fatalf("%d awaiting over %d pages, first %s", len(got), linked.pages, got[0].Shipment.ID())
	}
	store, linked = linkedShipments(t, 700, func(i int) bool { return i%2 == 0 })
	if got, err = (ListDateChangesAwaitingCustomer{Tx: store.tx, Shipments: store, Linked: linked}).Handle(ctx, "T", ""); err != nil {
		t.Fatal(err)
	}
	if len(got) != 200 || got[199].Shipment.ID() != "SH-0398" {
		t.Fatalf("the list's limit: %d, last %s", len(got), got[len(got)-1].Shipment.ID())
	}
}

func compare(a, b ful.ShipmentID) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
