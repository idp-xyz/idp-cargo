//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	"go.idp.xyz/idp-cargo/internal/contexts/com"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/contexts/org"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/rate"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func iataCode(code string) kernel.LocationCode {
	return kernel.LocationCode{Scheme: kernel.IATAAirport, Code: code}
}

// airline gives party airline-mu the IATA designator MU and AWB prefix 781 in ORG, and approves it as the legal
// entity's CARRIER.
func (s slice) airline(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for i, code := range [][2]string{{orgapi.IdentifierIATAAirline, "MU"}, {orgapi.IdentifierIATAAWBPrefix, "781"}} {
		if _, err := s.org.RecordPartyIdentifier(ctx, orgapi.RecordPartyIdentifier{CommandID: "mu-" + code[0], TenantID: s.tenant, ActorID: "md-1",
			PartyID: "airline-mu", ExpectedRevision: int64(1 + i), Type: code[0], Country: orgapi.InternationalIssuer, Value: code[1], EvidenceRef: "IATA"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.org.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "mu-carrier", TenantID: s.tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "airline-mu", ExpectedRevision: 3, RoleCode: orgapi.RoleCarrier, ValidFrom: s.at.Add(-time.Hour), EvidenceRef: "contract", OccurredAt: s.at}); err != nil {
		t.Fatal(err)
	}
}

// bookAir books an AIR consolidation with MU for 20 pieces and 1,000 kg on MU587 PVG-JFK departing etd, and
// records the airline's confirmation with the MAWB: requested and sent at 10 hours, confirmed at 11 hours after
// s.at.
func (s slice) bookAir(t *testing.T, book bookapi.Commands, id, consolidation, mawb string, etd time.Time) {
	t.Helper()
	ctx, tenant := context.Background(), s.tenant
	hours := func(h int) time.Time { return s.at.Add(time.Duration(h) * time.Hour) }
	evidence := func(h int) bookapi.Evidence {
		return bookapi.Evidence{SHA256: strings.Repeat("5a", 32), StorageID: "mail/mu.eml", MediaType: "message/rfc822", CapturedAt: hours(h)}
	}
	kg := kernel.Quantity{Unit: "KGM", Amount: 1000}
	if _, err := book.DraftCarrierBooking(ctx, bookapi.DraftCarrierBooking{CommandID: "draft-" + id, TenantID: tenant, ActorID: "ops-1", BookingID: id,
		Kind: bookapi.KindAirConsol, LegalEntityID: "LE", CarrierPartyID: "airline-mu", ConsolidationID: consolidation,
		Request: bookapi.Request{Legs: []bookapi.RequestedLeg{{Loading: iataCode("PVG"), Discharge: iataCode("JFK"), Flight: "MU587", DepartureDate: etd.Format(time.DateOnly)}},
			Capacity: bookapi.Capacity{Pieces: 20, GrossWeight: kg}}, OccurredAt: hours(10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := book.RecordBookingRequestSent(ctx, bookapi.RecordBookingRequestSent{CommandID: "sent-" + id, TenantID: tenant, ActorID: "ops-1",
		BookingID: id, ExpectedRevision: 1, Channel: bookapi.ChannelCarrierPortal, Evidence: evidence(10), OccurredAt: hours(10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := book.RecordCarrierConfirmation(ctx, bookapi.RecordCarrierConfirmation{CommandID: "confirm-" + id, TenantID: tenant, ActorID: "ops-1",
		BookingID: id, ExpectedRevision: 2, Answer: bookapi.Answer{Channel: bookapi.ChannelEmail, Evidence: evidence(11),
			References: []bookapi.CarrierReference{{Type: bookapi.ReferenceMAWBNumber, Value: mawb}}},
		Lines: []bookapi.CapacityLine{{Capacity: bookapi.Capacity{Pieces: 20, GrossWeight: kg}, Legs: []bookapi.ConfirmedLeg{{Loading: iataCode("PVG"),
			Discharge: iataCode("JFK"), Flight: "MU587", ETD: etd, ETA: etd.Add(14 * time.Hour)}}}}, OccurredAt: hours(11)}); err != nil {
		t.Fatal(err)
	}
}

// slice holds the real contexts a test plans shipments with.
type slice struct {
	org     *org.Module
	rate    *rate.Module
	com     *com.Module
	ful     *ful.Module
	tenant  string
	at      time.Time
	departs time.Time
}

// planAir plans one AIR shipment SH-<suffix> for a customer party: 6 cartons of 300 kg from Shanghai Pudong to
// New York JFK, departing within two days of departs; the shipment ends PLANNED at revision 3.
func (s slice) planAir(t *testing.T, party, suffix string) {
	t.Helper()
	ctx, tenant, at, departs := context.Background(), s.tenant, s.at, s.departs
	orgModule, rateModule, comModule, fulModule := s.org, s.rate, s.com, s.ful
	iata := func(code string) kernel.LocationCode {
		return kernel.LocationCode{Scheme: kernel.IATAAirport, Code: code}
	}
	t.Helper()
	kg := kernel.Quantity{Unit: "KGM", Amount: 300}
	if err := errors.Join(
		func() error {
			_, err := orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-" + suffix, TenantID: tenant, ActorID: "org-officer",
				LegalEntityID: "LE", PartyID: party, ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: at, EvidenceRef: "contract", OccurredAt: at})
			return err
		}(),
		func() error {
			_, err := rateModule.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "quote-" + suffix, TenantID: tenant, ActorID: "sales-1",
				QuoteID: "Q-" + suffix, LegalEntityID: "LE", CustomerPartyID: party, Currency: "USD",
				Lines:    []rateapi.QuoteLine{{ChargeCode: "AIR_FREIGHT", Sell: usd(100_000), Cost: usd(50_000)}},
				Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(time.Minute)})
			return err
		}(),
		func() error {
			_, err := rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-" + suffix, TenantID: tenant, ActorID: "sales-1",
				QuoteID: "Q-" + suffix, ExpectedRevision: 1, EvidenceKind: rateapi.EvidenceEmail, EvidenceRef: "mail", OccurredAt: at.Add(time.Hour)})
			return err
		}(),
		func() error {
			_, err := comModule.RecordCustomerOrder(ctx, comapi.RecordCustomerOrder{CommandID: "order-" + suffix, TenantID: tenant, ActorID: "cs-1",
				OrderID: "O-" + suffix, QuoteID: "Q-" + suffix, CustomerPartyID: party, OccurredAt: at.Add(2 * time.Hour),
				Transport: comapi.Transport{Mode: comapi.ModeAir},
				Lines:     []comapi.OrderLine{{Description: "Cartons of electronics", Packs: 6, PackageType: "CT", GrossWeight: kg}}})
			return err
		}(),
		func() error {
			_, err := comModule.AllocateOrderToObligation(ctx, comapi.AllocateOrderToObligation{CommandID: "allocate-" + suffix, TenantID: tenant,
				ActorID: "planner-1", OrderID: "O-" + suffix, ExpectedRevision: 1, LineNo: 1, ObligationID: "OB-" + suffix, Packs: 6,
				GrossWeight: kg, OccurredAt: at.Add(3 * time.Hour)})
			return err
		}(),
		func() error {
			_, err := comModule.ReserveObligationForFulfillment(ctx, comapi.ReserveObligationForFulfillment{CommandID: "reserve-" + suffix,
				TenantID: tenant, ActorID: "fulfilment", ObligationID: "OB-" + suffix, ObligationVersion: 1, ShipmentID: "SH-" + suffix,
				OccurredAt: at.Add(4 * time.Hour)})
			return err
		}(),
		func() error {
			_, err := fulModule.CreateShipmentFromObligation(ctx, fulapi.CreateShipmentFromObligation{CommandID: "create-" + suffix, TenantID: tenant,
				ActorID: "planner-1", ShipmentID: "SH-" + suffix, ObligationID: "OB-" + suffix, ObligationVersion: 1, OccurredAt: at.Add(5 * time.Hour)})
			return err
		}(),
		func() error {
			_, err := fulModule.AmendShipmentPlan(ctx, fulapi.AmendShipmentPlan{CommandID: "plan-" + suffix, TenantID: tenant, ActorID: "planner-1",
				ShipmentID: "SH-" + suffix, ExpectedRevision: 1, TransportMode: fulapi.ModeAir, Origin: iata("PVG"), Destination: iata("JFK"),
				DepartureWindow: kernel.ValidityInterval{From: departs, Until: departs.Add(48 * time.Hour)}, Reason: "flight booked",
				OccurredAt: at.Add(6 * time.Hour)})
			return err
		}(),
		func() error {
			_, err := fulModule.ConfirmShipmentPlan(ctx, fulapi.ConfirmShipmentPlan{CommandID: "confirm-" + suffix, TenantID: tenant,
				ActorID: "planner-2", ShipmentID: "SH-" + suffix, ExpectedRevision: 2, OccurredAt: at.Add(7 * time.Hour)})
			return err
		}(),
	); err != nil {
		t.Fatal(err)
	}
}
