package domain

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// WouldRecordEvidence answers by the comparison RecordEvidence makes and changes nothing: for every kind of evidence
// it agrees with what RecordEvidence then does.
func TestWouldRecordEvidenceIsTheComparisonOfRecordEvidence(t *testing.T) {
	s, route := planned(t)
	at := s.Latest().At.Add(time.Hour)
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	if err := s.RecordConsolAttachment(Membership{ConsolidationID: "C-1", EntryNo: 1, Revision: 1, AttachedAt: at, Mode: ModeSea,
		Origin: route.Origin, Destination: route.Destination}, "ops", at); err != nil {
		t.Fatal(err)
	}
	would := func(label string, e LoadEvidence, want bool) {
		t.Helper()
		before, pending := s.State(), len(s.Pending())
		got, err := s.WouldRecordEvidence(e)
		if err != nil || got != want {
			t.Fatalf("%s: would record %v, %v; want %v", label, got, err, want)
		}
		if len(s.State().Evidence) != len(before.Evidence) || len(s.Pending()) != pending {
			t.Fatalf("%s: the question changed the shipment", label)
		}
	}
	// no evidence yet: even an empty one is recorded as the first version
	would("empty evidence on a shipment with none", LoadEvidence{ConsolidationID: "C-1"}, true)
	received := LoadEvidence{Lots: 1, ReceiptComplete: true, Received: CargoScope{Packs: 4, GrossWeight: kg(410)}, ConsolidationID: "C-1"}
	would("received, none recorded", received, true)
	if added, err := s.RecordEvidence(received, "ops", at); err != nil || !added {
		t.Fatalf("record: %v %v", added, err)
	}
	would("the same facts again", received, false)
	tendered := received
	tendered.Tendered, tendered.Departed = CargoScope{Packs: 4, GrossWeight: kg(412)}, 3
	would("tendered and departed", tendered, true)
	sealed := received
	sealed.Stuffings = []Stuffing{{ExecutionID: "X-1", LoadUnitNo: 1, TypeCode: "22G1", Scope: CargoScope{Packs: 4, GrossWeight: kg(412)}, Lines: []int{1},
		Seal: "S1", SealedAt: at}}
	would("a sealed stuffing", sealed, true)
	if added, err := s.RecordEvidence(sealed, "ops", at); err != nil || !added {
		t.Fatalf("record sealed: %v %v", added, err)
	}
	resealed := sealed
	resealed.Stuffings = []Stuffing{sealed.Stuffings[0]}
	resealed.Stuffings[0].Seal, resealed.Stuffings[0].SealedAt = "S2", at.Add(time.Hour)
	would("the same stuffing sealed again", resealed, true)
	would("the stuffing as recorded", sealed, false)
	// the plan changed since: the same facts derive another status
	derived := tendered
	derived.Departed = 4
	would("departed", derived, true)

	// invalid or foreign evidence is refused as RecordEvidence refuses it
	if _, err := s.WouldRecordEvidence(LoadEvidence{ConsolidationID: "C-9", Tendered: CargoScope{Packs: 1}}); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("another consolidation: %v", err)
	}
	if _, err := s.WouldRecordEvidence(LoadEvidence{Received: CargoScope{Packs: -1}}); !errors.Is(err, ErrInvalidEvidence) {
		t.Fatalf("invalid evidence: %v", err)
	}
	cancelled, _ := planned(t)
	_ = cancelled.Cancel("withdrawn", "ops", cancelled.Latest().At.Add(time.Hour))
	if got, err := cancelled.WouldRecordEvidence(received); got || !errors.Is(err, ErrShipmentCancelled) {
		t.Fatalf("a cancelled shipment records nothing: %v %v", got, err)
	}
}
