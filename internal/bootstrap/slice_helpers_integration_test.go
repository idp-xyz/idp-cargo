//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

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
				Lines:    []rateapi.QuoteLine{{ChargeCode: "FREIGHT", Sell: usd(100_000), Cost: usd(50_000)}},
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
