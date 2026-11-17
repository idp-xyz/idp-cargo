package book

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/application"
	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// stub answers GetCarrierBooking with a booking or an error; SEA asks BOOK nothing else.
type stub struct {
	bookapi.Queries
	booking bookapi.CarrierBooking
	err     error
}

func (s stub) GetCarrierBooking(context.Context, bookapi.GetCarrierBooking) (bookapi.CarrierBooking, error) {
	return s.booking, s.err
}

func unlocode(code string) kernel.LocationCode {
	return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}
}

var etd = time.Date(2026, 11, 20, 9, 0, 0, 123456789, time.FixedZone("CST", 8*3600))

// confirmed is a container booking of a shipping line for C-1 as BOOK answers it, with times finer than a microsecond
// in another zone and a vessel name entered with spaces around it.
func confirmed() bookapi.CarrierBooking {
	port := func(code string) bookapi.ResolvedLocation { return bookapi.ResolvedLocation{Code: unlocode(code)} }
	return bookapi.CarrierBooking{TenantID: "T", BookingID: "BK-1", Revision: 3, Kind: bookapi.KindSeaLCLConsolEquipment, Status: bookapi.StatusConfirmed,
		LegalEntityID: "LE", CarrierPartyID: "line-1", CarrierPartyRevision: 5, CarrierRole: "CARRIER", ConsumerKind: bookapi.ConsumerConsolidation,
		ConsumerID: "C-1",
		Request:    bookapi.RequestVersion{Version: 1, LocationEdition: "E1", Legs: []bookapi.ResolvedLeg{{Loading: port("CNSHG"), Discharge: port("DEHAM")}}},
		Capacity: bookapi.CapacityVersion{Version: 2, Lines: []bookapi.CapacityLineView{{Line: 1, Legs: []bookapi.CapacityLeg{{Loading: port("CNSHG"),
			Discharge: port("DEHAM"), Vessel: " EVER GIVEN ", VesselIMO: "9811000", Voyage: "0123W", ETD: etd, ETA: etd.Add(30 * 24 * time.Hour),
			CutOffs: []bookapi.CutOff{{Kind: bookapi.CutOffVGM, At: etd.Add(-48*time.Hour + 100*time.Nanosecond), TimeZone: "Asia/Shanghai",
				Location: unlocode("CNSHG")}}}}}}}}
}

func TestBookingAsSEALinksIt(t *testing.T) {
	ctx := context.Background()
	got, err := Bookings{BOOK: stub{booking: confirmed()}}.Booking(ctx, "T", "BK-1")
	if err != nil {
		t.Fatal(err)
	}
	stored := etd.UTC().Truncate(time.Microsecond)
	if got.BookingID != "BK-1" || got.Revision != 3 || got.Kind != sea.KindOwn || !got.InForce || got.ConsumerID != "C-1" || got.LegalEntityID != "LE" ||
		got.Carrier != (sea.Carrier{PartyID: "line-1", PartyRevision: 5, Role: "CARRIER"}) || got.Origin != unlocode("CNSHG") ||
		got.Destination != unlocode("DEHAM") || got.Edition != "E1" || got.CapacityVersion != 2 || got.Lines != 1 || len(got.Legs) != 1 {
		t.Fatalf("booking: %+v", got)
	}
	// the vessel's name without the spaces BOOK kept around it; instants to the microsecond, in UTC
	leg := got.Legs[0]
	if leg.Vessel != "EVER GIVEN" || leg.VesselIMO != "9811000" || leg.Voyage != "0123W" || leg.From != unlocode("CNSHG") || leg.To != unlocode("DEHAM") ||
		leg.ETD != stored || leg.ETA != stored.Add(30*24*time.Hour) || len(leg.CutOffs) != 1 ||
		leg.CutOffs[0] != (sea.CutOff{Kind: "VGM", At: stored.Add(-48 * time.Hour), TimeZone: "Asia/Shanghai", Location: unlocode("CNSHG")}) {
		t.Fatalf("leg: %+v", leg)
	}
	if _, err = sea.OpenMovement("T", "OM-1", "C-1", got, "ops", stored); err != nil {
		t.Fatalf("the booking opens a movement: %v", err)
	}

	// an ETA within the ETD's microsecond is the ETD once stored: the domain refuses it, not the database
	b := confirmed()
	b.Capacity.Lines[0].Legs[0].ETA = etd.Add(100 * time.Nanosecond)
	if got, err = (Bookings{BOOK: stub{booking: b}}).Booking(ctx, "T", "BK-1"); err != nil || got.Legs[0].ETA != got.Legs[0].ETD {
		t.Fatalf("an ETA within the ETD's microsecond: %+v %v", got.Legs, err)
	}
	if _, err = sea.OpenMovement("T", "OM-1", "C-1", got, "ops", stored); !errors.Is(err, sea.ErrInvalidBooking) {
		t.Fatalf("an ETA that is the ETD once stored: %v", err)
	}

	for name, tc := range map[string]struct {
		change func(*bookapi.CarrierBooking)
		check  func(sea.Booking) bool
	}{
		"a co-load": {func(b *bookapi.CarrierBooking) { b.Kind, b.CarrierRole = bookapi.KindSeaLCLCoLoad, "CO_LOADER" },
			func(got sea.Booking) bool { return got.Kind == sea.KindCoLoad && got.Carrier.Role == "CO_LOADER" }},
		"an air booking": {func(b *bookapi.CarrierBooking) { b.Kind = bookapi.KindAirConsol }, func(got sea.Booking) bool { return got.Kind == "" }},
		"a draft":        {func(b *bookapi.CarrierBooking) { b.Status = bookapi.StatusDraft }, func(got sea.Booking) bool { return !got.InForce }},
		"cancelled":      {func(b *bookapi.CarrierBooking) { b.Status = bookapi.StatusCancelled }, func(got sea.Booking) bool { return !got.InForce }},
		"a cancellation the carrier has not confirmed": {func(b *bookapi.CarrierBooking) { b.Status = bookapi.StatusCancellationRequested },
			func(got sea.Booking) bool { return got.InForce }},
		"without capacity": {func(b *bookapi.CarrierBooking) { b.Capacity = bookapi.CapacityVersion{} },
			func(got sea.Booking) bool { return !got.InForce && got.Lines == 0 && len(got.Legs) == 0 }},
		"capacity released": {func(b *bookapi.CarrierBooking) { b.ConsumerID = "" }, func(got sea.Booking) bool { return got.ConsumerID == "" }},
		"capacity of another kind of consumer": {func(b *bookapi.CarrierBooking) { b.ConsumerKind = "SHIPMENT" },
			func(got sea.Booking) bool { return got.ConsumerID == "" }},
		"two capacity lines": {func(b *bookapi.CarrierBooking) { b.Capacity.Lines = append(b.Capacity.Lines, b.Capacity.Lines[0]) },
			func(got sea.Booking) bool { return got.Lines == 2 && len(got.Legs) == 1 }},
	} {
		b := confirmed()
		tc.change(&b)
		if got, err := (Bookings{BOOK: stub{booking: b}}).Booking(ctx, "T", "BK-1"); err != nil || !tc.check(got) {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
}

// What BOOK answers for another tenant or booking, and BOOK's errors: SEA's own kinds, never BOOK's.
func TestBookingAnswersSEADoesNotTake(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(*bookapi.CarrierBooking){
		"another tenant":  func(b *bookapi.CarrierBooking) { b.TenantID = "T-2" },
		"another booking": func(b *bookapi.CarrierBooking) { b.BookingID = "BK-2" },
	} {
		b := confirmed()
		change(&b)
		if _, err := (Bookings{BOOK: stub{booking: b}}).Booking(ctx, "T", "BK-1"); !errors.Is(err, app.ErrUnknownBooking) {
			t.Errorf("%s: %v", name, err)
		}
	}
	kinds := []error{bookapi.ErrUnauthenticated, bookapi.ErrUnauthorized, bookapi.ErrNotFound, bookapi.ErrConflict, bookapi.ErrIdempotencyConflict,
		bookapi.ErrRejected}
	for _, kind := range kinds {
		_, err := Bookings{BOOK: stub{err: fmt.Errorf("%w: stub", kind)}}.Booking(ctx, "T", "BK-1")
		for _, k := range kinds {
			if errors.Is(err, k) {
				t.Errorf("%v reaches SEA's caller as BOOK's kind: %v", kind, err)
			}
		}
		unknown := kind == bookapi.ErrNotFound || kind == bookapi.ErrRejected
		if err == nil || errors.Is(err, app.ErrUnknownBooking) != unknown {
			t.Errorf("%v: unknown booking %v, want %v: %v", kind, !unknown, unknown, err)
		}
	}
	if _, err := (Bookings{BOOK: stub{err: context.Canceled}}).Booking(ctx, "T", "BK-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context passes unchanged: %v", err)
	}
}
