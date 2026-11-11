package domain

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FUL V0.4, plan versus actual: PHY's verified evidence is recorded beside the plan when it changes, the
// execution status is derived from it alone, the measures go to AIR, and departed packs are final for planning.
func TestLoadEvidenceBesideThePlan(t *testing.T) {
	s, route := planned(t)
	at := s.Latest().At.Add(time.Hour)
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	if _, err := s.RecordEvidence(LoadEvidence{ConsolidationID: "C-1", Tendered: CargoScope{Packs: 1}}, "ops", at); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("tendered in a consolidation FUL does not record: %v", err)
	}
	if err := s.RecordConsolAttachment(Membership{ConsolidationID: "C-1", EntryNo: 1, Revision: 1, AttachedAt: at, Mode: ModeSea,
		Origin: route.Origin, Destination: route.Destination}, "ops", at); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Measured(); ok {
		t.Fatal("no measures before evidence")
	}
	received := LoadEvidence{Lots: 1, ReceiptComplete: true, Received: CargoScope{Packs: 4, GrossWeight: kg(410)}, ConsolidationID: "C-1"}
	if added, err := s.RecordEvidence(received, "ops", at); err != nil || !added {
		t.Fatalf("received: %v %v", added, err)
	}
	if added, err := s.RecordEvidence(received, "ops", at); err != nil || added {
		t.Fatalf("the same facts again change nothing: %v %v", added, err)
	}
	if basis, m, ok := s.Measured(); !ok || basis != BasisReceived || m.GrossWeight != kg(410) || s.Execution() != ExecutionNotStarted {
		t.Fatalf("received measures: %s %+v %s", basis, m, s.Execution())
	}
	tendered := received
	tendered.Tendered = CargoScope{Packs: 4, GrossWeight: kg(412)}
	tendered.Departed = 3
	if _, err := s.RecordEvidence(tendered, "ops", at); err != nil {
		t.Fatal(err)
	}
	if basis, m, _ := s.Measured(); basis != BasisTendered || m.GrossWeight != kg(412) || s.Execution() != ExecutionPartiallyDeparted {
		t.Fatalf("tendered and partly departed: %s %+v %s", basis, m, s.Execution())
	}
	types := map[string]int{}
	for _, e := range s.Pending() {
		types[e.Type]++
	}
	if types[eventLoadEvidenceRecorded] != 2 || types[eventExecutionStatusChanged] != 1 {
		t.Fatalf("events: %v", types)
	}

	// Departed packs are final for planning.
	if err := s.Cancel("customer withdrew", "ops", at); !errors.Is(err, ErrDeparted) {
		t.Fatalf("cancel after departure: %v", err)
	}
	moved := route
	moved.Departure.Until = moved.Departure.Until.Add(time.Hour)
	if err := s.AmendPlan(moved, "later window", "ops", at); !errors.Is(err, ErrDeparted) {
		t.Fatalf("amend after departure: %v", err)
	}
	if err := s.ChangePart("OB-1", 1, CargoScope{Packs: 2, GrossWeight: kg(200)}, CargoScope{}, "short", "ops", at); !errors.Is(err, ErrDeparted) {
		t.Fatalf("plan below the 3 departed: %v", err)
	}
	if err := s.ChangePart("OB-1", 1, CargoScope{Packs: 3, GrossWeight: kg(300)}, CargoScope{}, "one carton offloaded, rebooked", "ops", at); err != nil {
		t.Fatalf("plan the 3 departed: %v", err)
	}
	all := tendered
	all.Departed, all.Arrived = 3, 3
	if _, err := s.RecordEvidence(all, "ops", at); err != nil || s.Execution() != ExecutionArrived {
		t.Fatalf("all 3 planned packs arrived: %v %s", err, s.Execution())
	}
	restored, err := RestoreShipment(s.State())
	if err != nil || restored.Execution() != ExecutionArrived || restored.Departed() != 3 {
		t.Fatalf("restore: %v", err)
	}
	tampered := s.State()
	tampered.Evidence[0].Evidence.Received.Packs = 5
	if _, err = RestoreShipment(tampered); !errors.Is(err, ErrInvalidShipment) {
		t.Fatalf("evidence that does not match its digest: %v", err)
	}

	// After a transfer FUL relays the received measures until it records evidence in the new consolidation.
	s2, route2 := planned(t)
	at2 := s2.Latest().At.Add(time.Hour)
	m := Membership{ConsolidationID: "C-1", EntryNo: 1, Revision: 1, AttachedAt: at2, Mode: ModeSea, Origin: route2.Origin, Destination: route2.Destination}
	_ = s2.RecordConsolAttachment(m, "ops", at2)
	_, _ = s2.RecordEvidence(LoadEvidence{Lots: 1, ReceiptComplete: true, Received: CargoScope{Packs: 4, GrossWeight: kg(400)}, ConsolidationID: "C-1",
		Tendered: CargoScope{Packs: 4, GrossWeight: kg(400)}}, "ops", at2)
	m.ConsolidationID, m.EntryNo, m.Revision = "C-2", 2, 2
	_ = s2.RecordConsolAttachment(m, "ops", at2)
	if basis, _, _ := s2.Measured(); basis != BasisReceived {
		t.Fatalf("tendered in C-1, now attached to C-2: %s", basis)
	}
	cancelled, _ := planned(t)
	_ = cancelled.Cancel("withdrawn", "ops", cancelled.Latest().At.Add(time.Hour))
	if _, err = cancelled.RecordEvidence(received, "ops", at); !errors.Is(err, ErrShipmentCancelled) {
		t.Fatalf("a cancelled shipment records nothing: %v", err)
	}
}

func TestExecutionStatusIsDerived(t *testing.T) {
	for _, tc := range []struct {
		departed, arrived int64
		want              ExecutionStatus
	}{
		{0, 0, ExecutionNotStarted}, {2, 0, ExecutionPartiallyDeparted}, {4, 0, ExecutionDeparted}, {9, 0, ExecutionDeparted},
		{4, 1, ExecutionPartiallyArrived}, {4, 4, ExecutionArrived},
	} {
		if got := derive(4, LoadEvidence{Departed: tc.departed, Arrived: tc.arrived}); got != tc.want {
			t.Errorf("4 planned, %d departed, %d arrived: %s, want %s", tc.departed, tc.arrived, got, tc.want)
		}
	}
}
