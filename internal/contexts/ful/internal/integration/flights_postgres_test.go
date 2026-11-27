//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fulmodule "go.idp.xyz/idp-cargo/internal/contexts/ful"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.111 on PostgreSQL: FUL keeps the shipment's pieces on each flight with its load evidence, reads them back, shows
// the pieces awaiting a later flight on its work list, and the database keeps the flights as written and adding up.
func TestEvidenceFlightsOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	phy := &phyStub{at: f.at.Add(4 * time.Hour)}
	module, err := fulmodule.New(f.db, f.com, shipmentPolicy{}, fulmodule.WithLocations(locationStub{}), fulmodule.WithConsolMemberships(phy),
		fulmodule.WithPhysicalEvidence(phy))
	if err != nil {
		t.Fatal(err)
	}
	f.module = module
	for _, step := range []func() (api.Receipt, error){
		func() (api.Receipt, error) {
			return f.module.CreateShipmentFromObligation(ctx, f.create("SH-1", "create", "OB-1"))
		},
		func() (api.Receipt, error) {
			return f.module.AmendShipmentPlan(ctx, f.amend("amend", 1, 3, "schedule"))
		},
		func() (api.Receipt, error) { return f.module.ConfirmShipmentPlan(ctx, f.confirm("confirm", 2)) },
		func() (api.Receipt, error) {
			return f.module.RecordConsolAttachment(ctx, api.RecordConsolAttachment{CommandID: "attach", TenantID: f.tenant, ActorID: "ops-1",
				ShipmentID: "SH-1", ExpectedRevision: 3, ConsolidationID: "C-1", OccurredAt: f.at.Add(4 * time.Hour)})
		},
	} {
		if _, err = step(); err != nil {
			t.Fatal(err)
		}
	}
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	record := func(command string, minutes int) {
		t.Helper()
		e, err := f.module.GetShipmentExecution(ctx, api.GetShipmentExecution{TenantID: f.tenant, ShipmentID: "SH-1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.module.RecordVerifiedLoadEvidence(ctx, api.RecordVerifiedLoadEvidence{CommandID: command, TenantID: f.tenant, ActorID: "ops-1",
			ShipmentID: "SH-1", ExpectedRevision: e.Revision, OccurredAt: f.at.Add(5*time.Hour + time.Duration(minutes)*time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	lots := []phyapi.LotEvidence{{LotID: "L-1", Completed: true, ReceivedPacks: 2, Measurement: phyapi.Measurement{Version: 1, GrossWeight: kg(205)}}}
	phy.set(phyapi.PhysicalLoadEvidence{Lots: lots, Tenders: []phyapi.TenderEvidence{{ExecutionID: "X-1", ConsolidationID: "C-1",
		State: phyapi.ExecutionClosed, LoadedPacks: 2, GrossWeight: kg(207), Departed: 1, Offloaded: 1,
		Flights: []phyapi.FlightPieces{{Transport: "MU587/01DEC", Departed: 1, Offloaded: 1}}}}})
	record("first-day", 1)
	e, err := f.module.GetShipmentExecution(ctx, api.GetShipmentExecution{TenantID: f.tenant, ShipmentID: "SH-1"})
	if err != nil || e.AwaitingLaterFlight != 1 || len(e.Flights) != 1 || e.Flights[0].Transport != "MU587/01DEC" || e.ExecutionStatus != api.ExecutionPartiallyDeparted {
		t.Fatalf("after the first day: %+v %v", e, err)
	}
	if got, err := f.module.ListPiecesAwaitingFlight(ctx, api.ListPiecesAwaitingFlight{TenantID: f.tenant}); err != nil || len(got) != 1 ||
		got[0].ShipmentID != "SH-1" || got[0].Pieces != 1 || got[0].ConsolidationID != "C-1" || len(got[0].Flights) != 1 {
		t.Fatalf("the work list: %+v %v", got, err)
	}
	phy.set(phyapi.PhysicalLoadEvidence{Lots: lots, Tenders: []phyapi.TenderEvidence{{ExecutionID: "X-1", ConsolidationID: "C-1",
		State: phyapi.ExecutionClosed, LoadedPacks: 2, GrossWeight: kg(207), Departed: 2, Offloaded: 1,
		Flights: []phyapi.FlightPieces{{Transport: "MU587/01DEC", Departed: 1, Offloaded: 1}, {Transport: "MU587/02DEC", Departed: 1}}}}})
	record("next-day", 2)
	if e, err = f.module.GetShipmentExecution(ctx, api.GetShipmentExecution{TenantID: f.tenant, ShipmentID: "SH-1"}); err != nil || e.AwaitingLaterFlight != 0 ||
		len(e.Flights) != 2 || e.Flights[1] != (api.FlightPieces{Transport: "MU587/02DEC", Departed: 1}) || e.ExecutionStatus != api.ExecutionDeparted {
		t.Fatalf("after the next day: %+v %v", e, err)
	}
	if got, err := f.module.ListPiecesAwaitingFlight(ctx, api.ListPiecesAwaitingFlight{TenantID: f.tenant, ConsolidationID: "C-1"}); err != nil || len(got) != 0 {
		t.Fatalf("the work list after the next day: %+v %v", got, err)
	}

	// the database keeps the flights as written, with their evidence version, adding up to its pieces
	newVersion := `INSERT INTO cargo_ful.shipment_load_evidence (tenant_id,shipment_id,evidence_no,digest,lots,receipt_complete,received_packs,received_weight,
        received_volume,consolidation_id,tendered_packs,tendered_weight,tendered_volume,departed,arrived,offloaded,open_discrepancies,execution_status,
        recorded_by,recorded_at,entered_at,departed_final) VALUES ($1,'SH-1',3,repeat('b',64),1,true,2,0,0,'C-1',2,0,0,2,0,1,0,'DEPARTED','x',now(),now(),2)`
	for _, tc := range []struct {
		why, constraint, state string
		sqls                   []string
	}{
		{"flights are immutable", "", "42501", []string{`UPDATE cargo_ful.shipment_load_evidence_flight SET departed=9 WHERE tenant_id=$1`}},
		{"flights are kept", "", "42501", []string{`DELETE FROM cargo_ful.shipment_load_evidence_flight WHERE tenant_id=$1`}},
		{"a flight is written with its evidence version", "shipment_load_evidence_flight_with_evidence", "23514",
			[]string{`INSERT INTO cargo_ful.shipment_load_evidence_flight (tenant_id,shipment_id,evidence_no,flight_no,transport,departed,arrived,offloaded)
              VALUES ($1,'SH-1',1,2,'MU587/03DEC',1,0,0)`}},
		{"a version's flights add up to its pieces", "shipment_load_evidence_flights_add_up", "23514", []string{newVersion,
			`INSERT INTO cargo_ful.shipment_load_evidence_flight (tenant_id,shipment_id,evidence_no,flight_no,transport,departed,arrived,offloaded)
              VALUES ($1,'SH-1',3,1,'MU587/01DEC',3,0,1)`}},
		{"a flight has pieces", "shipment_load_evidence_flight_pieces", "23514", []string{newVersion,
			`INSERT INTO cargo_ful.shipment_load_evidence_flight (tenant_id,shipment_id,evidence_no,flight_no,transport,departed,arrived,offloaded)
              VALUES ($1,'SH-1',3,1,'MU587/01DEC',0,0,0)`}},
		{"a flight once in a version", "shipment_load_evidence_flight_once", "23505", []string{newVersion,
			`INSERT INTO cargo_ful.shipment_load_evidence_flight (tenant_id,shipment_id,evidence_no,flight_no,transport,departed,arrived,offloaded)
              VALUES ($1,'SH-1',3,1,'MU587/01DEC',1,0,1)`,
			`INSERT INTO cargo_ful.shipment_load_evidence_flight (tenant_id,shipment_id,evidence_no,flight_no,transport,departed,arrived,offloaded)
              VALUES ($1,'SH-1',3,2,'MU587/01DEC',1,0,0)`}},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, sql := range tc.sqls {
				if _, err := e.Exec(ctx, sql, f.tenant); err != nil {
					return err
				}
			}
			return nil
		})
		var pgErr *pgconn.PgError
		if sqlState(err) != tc.state || (tc.constraint != "" && (!errors.As(err, &pgErr) || pgErr.ConstraintName != tc.constraint)) {
			t.Errorf("%s: %v, want %s %s", tc.why, err, tc.state, tc.constraint)
		}
	}
	// the same version with flights that add up is taken: the refusals above are the rules'
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, sql := range []string{newVersion, `INSERT INTO cargo_ful.shipment_load_evidence_flight (tenant_id,shipment_id,evidence_no,flight_no,transport,
            departed,arrived,offloaded) VALUES ($1,'SH-1',3,1,'MU587/01DEC',1,0,1)`, `INSERT INTO cargo_ful.shipment_load_evidence_flight (tenant_id,
            shipment_id,evidence_no,flight_no,transport,departed,arrived,offloaded) VALUES ($1,'SH-1',3,2,'MU587/02DEC',1,0,0)`} {
			if _, err := e.Exec(ctx, sql, f.tenant); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("a version with flights adding up: %v", err)
	}
}
