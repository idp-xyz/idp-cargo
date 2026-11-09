package domain

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var (
	at       = time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	recordAt = at.Add(2 * time.Hour)
)

func kg(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }

func dm3(n int64) kernel.Quantity { return kernel.Quantity{Unit: "DMQ", Amount: n} }

func lb(n int64) kernel.Quantity { return kernel.Quantity{Unit: "LBR", Amount: n} }

// packs returns n packs weighing w kilograms, without a volume.
func packs(n, w int64) Measure { return Measure{Packs: n, GrossWeight: kg(w)} }

// packsWithVolume returns n packs weighing w kilograms and filling v cubic decimetres.
func packsWithVolume(n, w, v int64) Measure {
	return Measure{Packs: n, GrossWeight: kg(w), Volume: dm3(v)}
}

func line(description string, pkg PackageType, m Measure) OrderLine {
	return OrderLine{Description: description, PackageType: pkg, Measure: m}
}

func draft(lines ...OrderLine) OrderDraft {
	return OrderDraft{
		TenantID: "T", OrderID: "O-1", RecordedBy: "sales-1", Lines: lines,
		Quote: AcceptedQuote{QuoteID: "Q-1", QuoteRevision: 3, LegalEntityID: "LE", CustomerPartyID: "P", CustomerRevision: 2,
			RecipientRole: RoleCustomer, Total: kernel.Money{Currency: "USD", Minor: 100_000},
			Validity:   kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)},
			AcceptedAt: at.Add(time.Hour), EvidenceRef: "signed-quote", TermsContract: QuoteSnapshotV2, TermsHash: strings.Repeat("ab", 32)},
		Customer: CustomerSnapshot{PartyID: "P", PartyRevision: 2, LegalName: "Fixture Corp", CountryCode: "US", RoleCode: RoleCustomer,
			RoleValidity: kernel.ValidityInterval{From: at, Until: at.Add(30 * 24 * time.Hour)}, RoleEvidenceRef: "customer-contract"},
		Policy: BuiltInOrderPolicy(),
	}
}

var (
	palletLine = line("Pallets of machine parts", "PX", packsWithVolume(10, 500, 12_000))
	cartonLine = line("Cartons of spare parts", "CT", packs(12, 60))
)

func recorded(t *testing.T) *CustomerOrder {
	t.Helper()
	o, err := RecordOrder(draft(palletLine, cartonLine), 0, recordAt, recordAt)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// eventList lists the pending events as type/obligation/version/shipment, leaving out empty parts.
func eventList(o *CustomerOrder) string {
	var got []string
	for _, e := range o.Pending() {
		s := e.Type
		if e.ObligationID != "" {
			s += fmt.Sprintf("/%s/%d", e.ObligationID, e.ObligationVersion)
		}
		if e.ShipmentID != "" {
			s += "/" + e.ShipmentID
		}
		got = append(got, s)
	}
	return fmt.Sprint(got)
}

func TestRecordOrderFreezesTheQuoteAndTheCustomer(t *testing.T) {
	d := draft(palletLine, cartonLine)
	o, err := RecordOrder(d, 0, recordAt, recordAt)
	if err != nil {
		t.Fatal(err)
	}
	d.Lines[0].Measure = packs(1, 1)
	s := o.State()
	want := palletLine
	want.LineNo = 1
	if o.Status() != OrderRecorded || !s.RecordedAt.Equal(recordAt) || s.Customer.LegalName != "Fixture Corp" ||
		s.Lines[0] != want || s.Lines[1].LineNo != 2 {
		t.Fatalf("state %+v", s)
	}
	s.Lines[1].Measure = packs(1, 1)
	if o.State().Lines[1].Measure != packs(12, 60) {
		t.Fatal("State must return a copy")
	}
	if got := eventList(o); got != "[CustomerOrderRecorded.v1]" {
		t.Fatalf("events %s", got)
	}
}

func TestRecordOrderRejectsMismatchesWithTheQuote(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*OrderDraft)
		at   time.Time
		want error
	}{
		"another customer than the quote's":     {func(d *OrderDraft) { d.Customer.PartyID = "OTHER" }, recordAt, ErrQuoteMismatch},
		"ORG revision older than at acceptance": {func(d *OrderDraft) { d.Customer.PartyRevision = 1 }, recordAt, ErrQuoteMismatch},
		"recorded before the acceptance":        {func(*OrderDraft) {}, at.Add(30 * time.Minute), ErrOutsideQuoteValidity},
		"recorded when the validity ends":       {func(*OrderDraft) {}, at.Add(14 * 24 * time.Hour), ErrOutsideQuoteValidity},
		"customer role not yet in force": {func(d *OrderDraft) {
			d.Customer.RoleValidity = kernel.ValidityInterval{From: recordAt.Add(time.Hour)}
		}, recordAt, ErrInvalidOrder},
		"quote without a terms hash": {func(d *OrderDraft) { d.Quote.TermsHash = "not-a-digest" }, recordAt, ErrInvalidOrder},
		"quote without a total":      {func(d *OrderDraft) { d.Quote.Total.Minor = 0 }, recordAt, ErrInvalidOrder},
		"customer without a name":    {func(d *OrderDraft) { d.Customer.LegalName = " " }, recordAt, ErrInvalidOrder},
	} {
		d := draft(palletLine)
		tc.edit(&d)
		if _, err := RecordOrder(d, 0, tc.at, tc.at); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestRecordOrderRejectsInvalidLines(t *testing.T) {
	for name, lines := range map[string][]OrderLine{
		"no lines":                     nil,
		"zero packs":                   {line("Pallets", "PX", packs(0, 500))},
		"negative packs":               {line("Pallets", "PX", packs(-1, 500))},
		"zero gross weight":            {line("Pallets", "PX", packs(10, 0))},
		"weight unit not upper case":   {line("Pallets", "PX", Measure{Packs: 10, GrossWeight: kernel.Quantity{Unit: "kgm", Amount: 5}})},
		"volume unit without a volume": {line("Pallets", "PX", Measure{Packs: 10, GrossWeight: kg(500), Volume: dm3(0)})},
		"volume without a unit":        {line("Pallets", "PX", Measure{Packs: 10, GrossWeight: kg(500), Volume: kernel.Quantity{Amount: 5}})},
		"package type of one letter":   {line("Pallets", "P", packs(10, 500))},
		"package type in lower case":   {line("Pallets", "px", packs(10, 500))},
		"package type of three":        {line("Pallets", "PXX", packs(10, 500))},
		"blank description":            {line(" ", "PX", packs(10, 500))},
		"description too long":         {line(strings.Repeat("é", maxText+1), "PX", packs(10, 500))},
	} {
		if _, err := RecordOrder(draft(lines...), 0, recordAt, recordAt); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := RecordOrder(draft(line("Drums of resin", "1A", packs(4, 800))), 0, recordAt, recordAt); err != nil {
		t.Errorf("numeric package type 1A (steel drum): %v", err)
	}
}

func TestAllocationStaysWithinEachLine(t *testing.T) {
	o := recorded(t)
	later := recordAt.Add(time.Hour)
	if err := o.Allocate(1, "OB-1", packsWithVolume(4, 200, 5_000), "planner-1", later); err != nil {
		t.Fatal(err)
	}
	if rest, err := o.Remaining(1); err != nil || rest != packsWithVolume(6, 300, 7_000) || o.Status() != OrderPartiallyAllocated {
		t.Fatalf("remaining %+v, %v; status %s", rest, err, o.Status())
	}
	before := o.State()
	allocate := func(lineNo int, id ObligationID, m Measure) func() error {
		return func() error { return o.Allocate(lineNo, id, m, "planner-1", later) }
	}
	for name, tc := range map[string]struct {
		run  func() error
		want error
	}{
		"more packs than remain":           {allocate(1, "OB-2", packsWithVolume(7, 1, 1)), ErrOverAllocation},
		"more weight than remains":         {allocate(1, "OB-2", packsWithVolume(1, 301, 1)), ErrOverAllocation},
		"more volume than remains":         {allocate(1, "OB-2", packsWithVolume(1, 1, 7_001)), ErrOverAllocation},
		"weight in another unit":           {allocate(1, "OB-2", Measure{Packs: 1, GrossWeight: kernel.Quantity{Unit: "LBR", Amount: 1}, Volume: dm3(1)}), kernel.ErrUnitMismatch},
		"no volume on a line with volume":  {allocate(1, "OB-2", packs(1, 1)), kernel.ErrUnitMismatch},
		"volume on a line without volume":  {allocate(2, "OB-2", packsWithVolume(1, 1, 1)), kernel.ErrUnitMismatch},
		"no such line":                     {allocate(3, "OB-2", packs(1, 1)), ErrInvalidAllocation},
		"obligation id reused":             {allocate(2, "OB-1", packs(1, 1)), ErrInvalidAllocation},
		"zero packs":                       {allocate(2, "OB-2", packs(0, 1)), ErrInvalidAllocation},
		"zero weight":                      {allocate(2, "OB-2", packs(1, 0)), ErrInvalidAllocation},
		"blank obligation id":              {allocate(2, " ", packs(1, 1)), ErrInvalidAllocation},
		"obligation id longer than stored": {allocate(2, ObligationID(strings.Repeat("x", maxID+1)), packs(1, 1)), ErrInvalidAllocation},
		"before the order":                 {func() error { return o.Allocate(2, "OB-2", packs(1, 1), "planner-1", recordAt.Add(-time.Second)) }, ErrInvalidAllocation},
		"nobody allocating":                {func() error { return o.Allocate(2, "OB-2", packs(1, 1), " ", later) }, ErrInvalidAllocation},
	} {
		if err := tc.run(); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if !reflect.DeepEqual(o.State(), before) || len(o.Pending()) != 2 {
		t.Fatalf("refused allocations changed the order: %+v, events %d", o.State(), len(o.Pending()))
	}
	if err := o.Allocate(1, "OB-2", packsWithVolume(6, 250, 6_000), "planner-1", later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := o.Allocate(2, "OB-3", packs(12, 60), "planner-2", later.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if o.Status() != OrderAllocated {
		t.Fatalf("all packs allocated, status %s", o.Status())
	}
	if rest, _ := o.Remaining(1); rest != packsWithVolume(0, 50, 1_000) {
		t.Fatalf("weight and volume the planner did not declare stay with the line: %+v", rest)
	}
	if err := o.Allocate(1, "OB-4", packs(1, 1), "planner-1", later); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("allocating a fully allocated order: err = %v", err)
	}
	if got, want := eventList(o), "[CustomerOrderRecorded.v1 FulfillmentObligationAllocated.v1/OB-1/1 "+
		"FulfillmentObligationAllocated.v1/OB-2/1 FulfillmentObligationAllocated.v1/OB-3/1]"; got != want {
		t.Fatalf("events %s", got)
	}
	ob, ok := o.Obligation("OB-3", 1)
	if !ok || ob.LineNo != 2 || ob.Measure != packs(12, 60) || ob.Status != ObligationActive || ob.AllocatedBy != "planner-2" {
		t.Fatalf("obligation OB-3: %+v, %v", ob, ok)
	}
	if _, ok := o.Obligation("OB-3", 2); ok {
		t.Fatal("OB-3 has no version 2")
	}
	if added := o.Added(); len(added) != 3 {
		t.Fatalf("added %+v", added)
	}
	o.Acknowledge()
	if len(o.Added()) != 0 || len(o.Pending()) != 0 {
		t.Fatal("Acknowledge must clear pending events and added obligations")
	}
}

func TestRestoreOrderChecksConsistency(t *testing.T) {
	o := recorded(t)
	steps := []error{
		o.Allocate(1, "OB-1", packsWithVolume(10, 500, 12_000), "planner-1", recordAt.Add(time.Hour)),
		o.Allocate(2, "OB-2", packs(5, 25), "planner-1", recordAt.Add(time.Hour)),
		o.Revoke("OB-2", "planner-1", recordAt.Add(2*time.Hour)),
		o.Reserve("OB-1", 1, "S-1", "fulfilment", recordAt.Add(3*time.Hour)),
		o.ReleaseReservation("OB-1", 1, "S-1", "fulfilment", recordAt.Add(4*time.Hour)),
		o.Reserve("OB-1", 1, "S-2", "fulfilment", recordAt.Add(5*time.Hour)),
	}
	if err := errors.Join(steps...); err != nil {
		t.Fatal(err)
	}
	valid := o.State()
	restored, err := RestoreOrder(valid)
	if err != nil || !reflect.DeepEqual(restored.State(), valid) || len(restored.Added()) != 0 || len(restored.AddedReservations()) != 0 ||
		len(restored.Pending()) != 0 {
		t.Fatalf("round trip: %+v, %v", restored, err)
	}
	ob := func(id ObligationID, version int64, line int, m Measure) Obligation {
		return Obligation{ObligationID: id, Version: version, LineNo: line, Measure: m, Status: ObligationActive, AllocatedBy: "x", AllocatedAt: recordAt}
	}
	for name, edit := range map[string]func(*OrderState){
		"status behind the obligations":   func(s *OrderState) { s.Status = OrderRecorded },
		"status ahead of the obligations": func(s *OrderState) { s.Status = OrderAllocated },
		"unknown status":                  func(s *OrderState) { s.Status = "CLOSED" },
		"cancelled with an active obligation": func(s *OrderState) {
			s.Status, s.CancelledBy, s.CancelledAt = OrderCancelled, "x", recordAt.Add(6*time.Hour)
			s.Reservations[1].ReleasedBy, s.Reservations[1].ReleasedAt = "x", recordAt.Add(6*time.Hour)
		},
		"cancellation without who":   func(s *OrderState) { s.CancelledAt = recordAt.Add(6 * time.Hour) },
		"over-allocated line":        func(s *OrderState) { s.Obligations = append(s.Obligations, ob("OB-3", 1, 1, packsWithVolume(1, 1, 1))) },
		"obligation in another unit": func(s *OrderState) { s.Obligations[0].Measure.GrossWeight.Unit = "LBR" },
		"duplicate version":          func(s *OrderState) { s.Obligations = append(s.Obligations, s.Obligations[0]) },
		"obligation before the order": func(s *OrderState) {
			s.Obligations[1].AllocatedAt, s.Obligations[2].AllocatedAt = recordAt.Add(-time.Minute), recordAt.Add(-time.Minute)
		},
		"revocation without its allocation": func(s *OrderState) { s.Obligations = append(s.Obligations[:1], s.Obligations[2]) },
		"revocation changing the measure":   func(s *OrderState) { s.Obligations[2].Measure = packs(4, 25) },
		"revocation without who":            func(s *OrderState) { s.Obligations[2].RevokedBy = "" },
		"version 1 revoked":                 func(s *OrderState) { s.Obligations[1].Status = ObligationRevoked },
		"version after a revocation": func(s *OrderState) {
			v3 := s.Obligations[2]
			v3.Version = 3
			s.Obligations = append(s.Obligations, v3)
		},
		"reservation numbering":            func(s *OrderState) { s.Reservations[1].No = 5 },
		"reservation of a revoked version": func(s *OrderState) { s.Reservations[0].ObligationID, s.Reservations[0].ObligationVersion = "OB-2", 2 },
		"one shipment holds one obligation twice": func(s *OrderState) {
			s.Reservations[0].ReleasedBy, s.Reservations[0].ReleasedAt = "", time.Time{}
			s.Reservations[0].ShipmentID = s.Reservations[1].ShipmentID
		},
		"release without who":        func(s *OrderState) { s.Reservations[0].ReleasedBy = "" },
		"released before reserved":   func(s *OrderState) { s.Reservations[0].ReleasedAt = recordAt },
		"reservation without a ship": func(s *OrderState) { s.Reservations[1].ShipmentID = " " },
		"line numbers out of order":  func(s *OrderState) { s.Lines[0].LineNo, s.Lines[1].LineNo = 2, 1 },
	} {
		s := o.State()
		edit(&s)
		if _, err := RestoreOrder(s); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
