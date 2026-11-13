//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// planSea plans one SEA shipment SH-<suffix> for a customer party whose order requests container mode: 6 cartons of
// 300 kg from Shanghai to Hamburg, departing within two days of departs; the plan names planned as its container
// mode. The shipment ends PLANNED at revision 3.
func (s slice) planSea(t *testing.T, party, suffix, requested, planned string) {
	t.Helper()
	ctx, tenant, at, departs := context.Background(), s.tenant, s.at, s.departs
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	kg := kernel.Quantity{Unit: "KGM", Amount: 300}
	steps := []func() error{
		func() error {
			_, err := s.org.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-" + suffix, TenantID: tenant, ActorID: "org-officer",
				LegalEntityID: "LE", PartyID: party, ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: at, EvidenceRef: "contract", OccurredAt: at})
			return err
		},
		func() error {
			_, err := s.rate.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "quote-" + suffix, TenantID: tenant, ActorID: "sales-1",
				QuoteID: "Q-" + suffix, LegalEntityID: "LE", CustomerPartyID: party, Currency: "USD",
				Lines:    []rateapi.QuoteLine{{ChargeCode: "OCEAN_FREIGHT", Sell: usd(100_000), Cost: usd(50_000)}},
				Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(time.Minute)})
			return err
		},
		func() error {
			_, err := s.rate.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-" + suffix, TenantID: tenant, ActorID: "sales-1",
				QuoteID: "Q-" + suffix, ExpectedRevision: 1, EvidenceKind: rateapi.EvidenceEmail, EvidenceRef: "mail", OccurredAt: at.Add(time.Hour)})
			return err
		},
		func() error {
			_, err := s.com.RecordCustomerOrder(ctx, comapi.RecordCustomerOrder{CommandID: "order-" + suffix, TenantID: tenant, ActorID: "cs-1",
				OrderID: "O-" + suffix, QuoteID: "Q-" + suffix, CustomerPartyID: party, OccurredAt: at.Add(2 * time.Hour),
				Transport: comapi.Transport{Mode: comapi.ModeSea, ContainerMode: requested},
				Lines:     []comapi.OrderLine{{Description: "Cartons of furniture parts", Packs: 6, PackageType: "CT", GrossWeight: kg}}})
			return err
		},
		func() error {
			_, err := s.com.AllocateOrderToObligation(ctx, comapi.AllocateOrderToObligation{CommandID: "allocate-" + suffix, TenantID: tenant,
				ActorID: "planner-1", OrderID: "O-" + suffix, ExpectedRevision: 1, LineNo: 1, ObligationID: "OB-" + suffix, Packs: 6,
				GrossWeight: kg, OccurredAt: at.Add(3 * time.Hour)})
			return err
		},
		func() error {
			_, err := s.com.ReserveObligationForFulfillment(ctx, comapi.ReserveObligationForFulfillment{CommandID: "reserve-" + suffix,
				TenantID: tenant, ActorID: "fulfilment", ObligationID: "OB-" + suffix, ObligationVersion: 1, ShipmentID: "SH-" + suffix,
				OccurredAt: at.Add(4 * time.Hour)})
			return err
		},
		func() error {
			_, err := s.ful.CreateShipmentFromObligation(ctx, fulapi.CreateShipmentFromObligation{CommandID: "create-" + suffix, TenantID: tenant,
				ActorID: "planner-1", ShipmentID: "SH-" + suffix, ObligationID: "OB-" + suffix, ObligationVersion: 1, OccurredAt: at.Add(5 * time.Hour)})
			return err
		},
		func() error {
			_, err := s.ful.AmendShipmentPlan(ctx, fulapi.AmendShipmentPlan{CommandID: "plan-" + suffix, TenantID: tenant, ActorID: "planner-1",
				ShipmentID: "SH-" + suffix, ExpectedRevision: 1, TransportMode: fulapi.ModeSea, ContainerMode: planned, Origin: unlocode("CNSHA"),
				Destination: unlocode("DEHAM"), DepartureWindow: kernel.ValidityInterval{From: departs, Until: departs.Add(48 * time.Hour)},
				Reason: "vessel booked", OccurredAt: at.Add(6 * time.Hour)})
			return err
		},
		func() error {
			_, err := s.ful.ConfirmShipmentPlan(ctx, fulapi.ConfirmShipmentPlan{CommandID: "confirm-" + suffix, TenantID: tenant,
				ActorID: "planner-2", ShipmentID: "SH-" + suffix, ExpectedRevision: 2, OccurredAt: at.Add(7 * time.Hour)})
			return err
		},
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("planSea %s, step %d: %v", suffix, i, err)
		}
	}
}

// N1-PHY OD-9 with the real COM, FUL and PHY: the container mode an order requests is planned in FUL, which refuses
// a plan of another container mode, carried to PHY in LoadIntent v1, and an LCL consolidation refuses an FCL house.
// COM and the database refuse a container mode of another transport mode.
func TestContainerModeFromOrderToConsolidation(t *testing.T) {
	ctx := context.Background()
	platform := openPlatform(t)
	orgModule, err := platform.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := platform.RATE(orgModule, allowAll{}, marginFloor(2000))
	if err != nil {
		t.Fatal(err)
	}
	comModule, err := platform.COM(orgModule, rateModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	fulModule, phyModule, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-container-")
	seedORG(t, platform, tenant, "party-l", "party-f", "party-x", "party-n")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	contexts.planSea(t, "party-l", "L", comapi.ContainerLCL, fulapi.ContainerLCL)
	contexts.planSea(t, "party-f", "F", comapi.ContainerFCL, fulapi.ContainerFCL)

	if ob, err := comModule.GetObligationSnapshotV3(ctx, comapi.GetObligationSnapshot{TenantID: tenant, ObligationID: "OB-F", Version: 1}); err != nil ||
		ob.RequestedTransportMode != comapi.ModeSea || ob.RequestedContainerMode != comapi.ContainerFCL {
		t.Fatalf("COM's obligation snapshot: %+v %v", ob, err)
	}
	if p, err := comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, ActorID: "cs-1", OrderID: "O-L"}); err != nil ||
		p.Transport.ContainerMode != comapi.ContainerLCL {
		t.Fatalf("OrderParties v1: %+v %v", p.Transport, err)
	}
	for id, want := range map[string]string{"SH-L": fulapi.ContainerLCL, "SH-F": fulapi.ContainerFCL} {
		if intent, err := fulModule.GetLoadIntent(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: id}); err != nil ||
			intent.ContainerMode != want || intent.TransportMode != fulapi.ModeSea {
			t.Fatalf("LoadIntent v1 of %s: %+v %v", id, intent, err)
		}
	}

	// FUL plans an obligation only in the container mode its order requests
	if _, err = fulModule.AmendShipmentPlan(ctx, fulapi.AmendShipmentPlan{CommandID: "replan-L", TenantID: tenant, ActorID: "planner-1",
		ShipmentID: "SH-L", ExpectedRevision: 3, TransportMode: fulapi.ModeSea, ContainerMode: fulapi.ContainerFCL, Origin: unlocode("CNSHA"),
		Destination: unlocode("DEHAM"), DepartureWindow: kernel.ValidityInterval{From: departs, Until: departs.Add(48 * time.Hour)},
		Reason: "a full container", OccurredAt: at.Add(8 * time.Hour)}); !errors.Is(err, fulapi.ErrRejected) || !strings.Contains(err.Error(), "requests LCL") {
		t.Fatalf("an FCL plan for an order that requests LCL: %v", err)
	}
	if _, err = fulModule.AmendShipmentPlan(ctx, fulapi.AmendShipmentPlan{CommandID: "replan-F-none", TenantID: tenant, ActorID: "planner-1",
		ShipmentID: "SH-F", ExpectedRevision: 3, TransportMode: fulapi.ModeSea, Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"),
		DepartureWindow: kernel.ValidityInterval{From: departs, Until: departs.Add(48 * time.Hour)}, Reason: "the container mode left out",
		OccurredAt: at.Add(8 * time.Hour)}); !errors.Is(err, fulapi.ErrRejected) || !strings.Contains(err.Error(), "plans no container mode") {
		t.Fatalf("a plan dropping the container mode its order requests: %v", err)
	}
	// a shipment whose order requests no container mode is planned without one, then given one alone
	contexts.planSea(t, "party-n", "N", "", "")
	if _, err = fulModule.AmendShipmentPlan(ctx, fulapi.AmendShipmentPlan{CommandID: "replan-N", TenantID: tenant, ActorID: "planner-1",
		ShipmentID: "SH-N", ExpectedRevision: 3, TransportMode: fulapi.ModeSea, ContainerMode: fulapi.ContainerLCL, Origin: unlocode("CNSHA"),
		Destination: unlocode("DEHAM"), DepartureWindow: kernel.ValidityInterval{From: departs, Until: departs.Add(48 * time.Hour)},
		Reason: "consolidated LCL", OccurredAt: at.Add(8 * time.Hour)}); err != nil {
		t.Fatalf("the container mode alone amended: %v", err)
	}
	if intent, err := fulModule.GetLoadIntent(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: tenant, ShipmentID: "SH-N"}); err != nil ||
		intent.ContainerMode != fulapi.ContainerLCL || intent.PlanVersion != 3 {
		t.Fatalf("LoadIntent v1 of SH-N: %+v %v", intent, err)
	}
	if _, err = fulModule.AmendShipmentPlan(ctx, fulapi.AmendShipmentPlan{CommandID: "replan-L-air", TenantID: tenant, ActorID: "planner-1",
		ShipmentID: "SH-L", ExpectedRevision: 3, TransportMode: fulapi.ModeSea, ContainerMode: fulapi.ContainerLoose, Origin: unlocode("CNSHA"),
		Destination: unlocode("DEHAM"), DepartureWindow: kernel.ValidityInterval{From: departs, Until: departs.Add(48 * time.Hour)},
		Reason: "loose", OccurredAt: at.Add(8 * time.Hour)}); !errors.Is(err, fulapi.ErrRejected) {
		t.Fatalf("LOOSE planned on SEA: %v", err)
	}

	// COM: a container mode of another transport mode is refused, by COM and by the database
	contexts.planAir(t, "party-x", "X")
	if _, err = comModule.ChangeOrderTransport(ctx, comapi.ChangeOrderTransport{CommandID: "transport-X", TenantID: tenant, ActorID: "cs-1",
		OrderID: "O-X", ExpectedRevision: 3, Transport: comapi.Transport{Mode: comapi.ModeAir, ContainerMode: comapi.ContainerFCL},
		Reason: "a full container", OccurredAt: at.Add(8 * time.Hour)}); !errors.Is(err, comapi.ErrRejected) {
		t.Fatalf("FCL on an AIR order: %v", err)
	}
	if _, err = comModule.ChangeOrderTransport(ctx, comapi.ChangeOrderTransport{CommandID: "transport-X-loose", TenantID: tenant, ActorID: "cs-1",
		OrderID: "O-X", ExpectedRevision: 3, Transport: comapi.Transport{Mode: comapi.ModeAir, ContainerMode: comapi.ContainerLoose},
		Reason: "loose cargo consolidated", OccurredAt: at.Add(8 * time.Hour)}); err != nil {
		t.Fatalf("LOOSE on an AIR order: %v", err)
	}
	if p, err := comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, ActorID: "cs-1", OrderID: "O-X"}); err != nil ||
		p.Transport != (comapi.Transport{Mode: comapi.ModeAir, ContainerMode: comapi.ContainerLoose}) {
		t.Fatalf("the changed transport: %+v %v", p.Transport, err)
	}
	fixture, err := platform.newDB()
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ sql, constraint string }{
		"FCL on an AIR order": {`INSERT INTO cargo_com.order_transport (tenant_id,order_id,version,transport_mode,container_mode,reason,changed_by,
            changed_at,entered_at) VALUES ($1,'O-X',99,'AIR','FCL','x','x',now(),now())`, "order_transport_container_mode"},
		"a container mode on an order of no mode": {`INSERT INTO cargo_com.order_transport (tenant_id,order_id,version,movement_type,container_mode,
            reason,changed_by,changed_at,entered_at) VALUES ($1,'O-X',99,'PORT_TO_PORT','LCL','x','x',now(),now())`, "order_transport_container_mode"},
		"FCL on an AIR plan": {`INSERT INTO cargo_ful.shipment_plan_version (tenant_id,shipment_id,plan_no,change,transport_mode,container_mode,
            scope_packs,scope_weight_unit,scope_weight,reason,written_by,written_at,entered_at) VALUES ($1,'SH-X',99,'AMENDED','AIR','FCL',6,'KGM',300,
            'x','x',now(),now())`, "shipment_plan_version_container_mode"},
		"a container mode on a plan of no mode": {`INSERT INTO cargo_ful.shipment_plan_version (tenant_id,shipment_id,plan_no,change,container_mode,
            scope_packs,scope_weight_unit,scope_weight,reason,written_by,written_at,entered_at) VALUES ($1,'SH-X',99,'AMENDED','FCL',6,'KGM',300,
            'x','x',now(),now())`, "shipment_plan_version_container_mode"},
	} {
		err = (bento.TenantTransactor{DB: fixture, Tenant: tenant}).WithinTransaction(ctx, func(txCtx context.Context) error {
			e, err := fixture.RequireExecutor(txCtx)
			if err != nil {
				return err
			}
			_, err = e.Exec(txCtx, tc.sql, tenant)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), tc.constraint) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// PHY: the LCL consolidation takes the LCL house and refuses the FCL one
	if _, err = phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol-sea", TenantID: tenant, ActorID: "ops-1",
		ConsolidationID: "C-LCL", Mode: phyapi.ModeSea, Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"),
		Destination: unlocode("DEHAM"), PlannedDeparture: departs.Add(6 * time.Hour), HouseCutOff: departs.Add(-6 * time.Hour),
		OccurredAt: at.Add(8 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-F", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-LCL",
		ShipmentID: "SH-F", OccurredAt: at.Add(9 * time.Hour)}); !errors.Is(err, phyapi.ErrRejected) || !strings.Contains(err.Error(), "PHY_FCL_HOUSE") {
		t.Fatalf("an FCL house in an LCL consolidation: %v", err)
	}
	if _, err = phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-L", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-LCL",
		ShipmentID: "SH-L", OccurredAt: at.Add(9 * time.Hour)}); err != nil {
		t.Fatalf("an LCL house: %v", err)
	}
}
