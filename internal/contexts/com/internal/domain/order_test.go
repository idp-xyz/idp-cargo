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

func pieces(n int64) kernel.Quantity { return kernel.Quantity{Unit: "H87", Amount: n} }

func line(description string, q kernel.Quantity) OrderLine {
	return OrderLine{Description: description, Quantity: q}
}

func draft(lines ...OrderLine) OrderDraft {
	return OrderDraft{
		TenantID: "T", OrderID: "O-1", RecordedBy: "sales-1", Lines: lines,
		Quote: AcceptedQuote{QuoteID: "Q-1", QuoteRevision: 3, LegalEntityID: "LE", CustomerPartyID: "P", CustomerRevision: 2,
			Total: kernel.Money{Currency: "USD", Minor: 100_000}, Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)},
			AcceptedAt: at.Add(time.Hour), EvidenceRef: "signed-quote", TermsHash: strings.Repeat("ab", 32)},
		Customer: CustomerSnapshot{PartyID: "P", PartyRevision: 2, LegalName: "Fixture Corp", CountryCode: "US",
			RoleValidity: kernel.ValidityInterval{From: at, Until: at.Add(30 * 24 * time.Hour)}, RoleEvidenceRef: "customer-contract"},
	}
}

func recorded(t *testing.T) *CustomerOrder {
	t.Helper()
	o, err := RecordOrder(draft(line("Pallets of machine parts", kg(500)), line("Cartons of spare parts", pieces(12))), recordAt)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestRecordOrderFreezesTheQuoteAndTheCustomer(t *testing.T) {
	d := draft(line("Pallets of machine parts", kg(500)), line("Cartons of spare parts", pieces(12)))
	o, err := RecordOrder(d, recordAt)
	if err != nil {
		t.Fatal(err)
	}
	d.Lines[0].Quantity = kg(1)
	s := o.State()
	if o.Status() != OrderRecorded || !s.RecordedAt.Equal(recordAt) || s.Customer.LegalName != "Fixture Corp" ||
		s.Lines[0] != (OrderLine{LineNo: 1, Description: "Pallets of machine parts", Quantity: kg(500)}) || s.Lines[1].LineNo != 2 {
		t.Fatalf("state %+v", s)
	}
	s.Lines[1].Quantity = pieces(1)
	if o.State().Lines[1].Quantity != pieces(12) {
		t.Fatal("State must return a copy")
	}
	if events := o.Pending(); len(events) != 1 || events[0].Type != "CustomerOrderRecorded.v1" || events[0].SubjectID != "O-1" || events[0].ObligationID != "" {
		t.Fatalf("events %+v", events)
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
		d := draft(line("Pallets of machine parts", kg(500)))
		tc.edit(&d)
		if _, err := RecordOrder(d, tc.at); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestRecordOrderRejectsInvalidLines(t *testing.T) {
	for name, lines := range map[string][]OrderLine{
		"no lines":             nil,
		"zero quantity":        {line("Pallets", kg(0))},
		"negative quantity":    {line("Pallets", kg(-5))},
		"unit not upper case":  {line("Pallets", kernel.Quantity{Unit: "kgm", Amount: 5})},
		"blank description":    {line(" ", kg(5))},
		"description too long": {line(strings.Repeat("é", maxText+1), kg(5))},
	} {
		if _, err := RecordOrder(draft(lines...), recordAt); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestAllocationStaysWithinEachLine(t *testing.T) {
	o := recorded(t)
	if err := o.Allocate(1, "OB-1", kg(200), "planner-1", recordAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rest, err := o.Remaining(1); err != nil || rest != kg(300) || o.Status() != OrderPartiallyAllocated {
		t.Fatalf("remaining %+v, %v; status %s", rest, err, o.Status())
	}
	before := o.State()
	for name, tc := range map[string]struct {
		run  func() error
		want error
	}{
		"more than remains":    {func() error { return o.Allocate(1, "OB-2", kg(301), "planner-1", recordAt.Add(time.Hour)) }, ErrOverAllocation},
		"another unit":         {func() error { return o.Allocate(1, "OB-2", pieces(1), "planner-1", recordAt.Add(time.Hour)) }, kernel.ErrUnitMismatch},
		"no such line":         {func() error { return o.Allocate(3, "OB-2", kg(1), "planner-1", recordAt.Add(time.Hour)) }, ErrInvalidAllocation},
		"obligation id reused": {func() error { return o.Allocate(1, "OB-1", kg(1), "planner-1", recordAt.Add(time.Hour)) }, ErrInvalidAllocation},
		"before the order":     {func() error { return o.Allocate(1, "OB-2", kg(1), "planner-1", recordAt.Add(-time.Second)) }, ErrInvalidAllocation},
		"zero quantity":        {func() error { return o.Allocate(1, "OB-2", kg(0), "planner-1", recordAt.Add(time.Hour)) }, ErrInvalidAllocation},
		"nobody allocating":    {func() error { return o.Allocate(1, "OB-2", kg(1), " ", recordAt.Add(time.Hour)) }, ErrInvalidAllocation},
		"blank obligation id":  {func() error { return o.Allocate(1, " ", kg(1), "planner-1", recordAt.Add(time.Hour)) }, ErrInvalidAllocation},
	} {
		if err := tc.run(); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if !reflect.DeepEqual(o.State(), before) || len(o.Pending()) != 2 {
		t.Fatalf("refused allocations changed the order: %+v, events %d", o.State(), len(o.Pending()))
	}
	if err := o.Allocate(1, "OB-2", kg(300), "planner-1", recordAt.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := o.Allocate(2, "OB-3", pieces(12), "planner-2", recordAt.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if o.Status() != OrderAllocated {
		t.Fatalf("status %s", o.Status())
	}
	if err := o.Allocate(1, "OB-4", kg(1), "planner-1", recordAt.Add(4*time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("allocating a fully allocated order: err = %v", err)
	}
	var got []string
	for _, e := range o.Pending() {
		got = append(got, fmt.Sprintf("%s/%s/%d", e.Type, e.ObligationID, e.ObligationVersion))
	}
	if want := "[CustomerOrderRecorded.v1//0 FulfillmentObligationAllocated.v1/OB-1/1 FulfillmentObligationAllocated.v1/OB-2/1 FulfillmentObligationAllocated.v1/OB-3/1]"; fmt.Sprint(got) != want {
		t.Fatalf("events %v", got)
	}
	if ob, ok := o.Obligation("OB-3", 1); !ok || ob.LineNo != 2 || ob.Quantity != pieces(12) || ob.AllocatedBy != "planner-2" {
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
	if err := o.Allocate(1, "OB-1", kg(500), "planner-1", recordAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	valid := o.State()
	restored, err := RestoreOrder(valid)
	if err != nil || !reflect.DeepEqual(restored.State(), valid) || len(restored.Added()) != 0 || len(restored.Pending()) != 0 {
		t.Fatalf("round trip: %+v, %v", restored, err)
	}
	for name, edit := range map[string]func(*OrderState){
		"status behind the allocations":   func(s *OrderState) { s.Status = OrderRecorded },
		"status ahead of the allocations": func(s *OrderState) { s.Status = OrderAllocated },
		"unknown status":                  func(s *OrderState) { s.Status = "CANCELLED" },
		"over-allocated line": func(s *OrderState) {
			s.Obligations = append(s.Obligations, Obligation{ObligationID: "OB-2", Version: 1, LineNo: 1, Quantity: kg(1), AllocatedBy: "x", AllocatedAt: recordAt})
		},
		"obligation in another unit": func(s *OrderState) { s.Obligations[0].Quantity = pieces(500) },
		"duplicate obligation id": func(s *OrderState) {
			s.Obligations = append(s.Obligations, Obligation{ObligationID: "OB-1", Version: 1, LineNo: 2, Quantity: pieces(1), AllocatedBy: "x", AllocatedAt: recordAt})
		},
		"obligation before the order": func(s *OrderState) { s.Obligations[0].AllocatedAt = recordAt.Add(-time.Minute) },
		"line numbers out of order":   func(s *OrderState) { s.Lines[0].LineNo, s.Lines[1].LineNo = 2, 1 },
	} {
		s := o.State()
		edit(&s)
		if _, err := RestoreOrder(s); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
