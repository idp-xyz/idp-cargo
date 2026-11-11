//go:build integration

package integration

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fulmodule "go.idp.xyz/idp-cargo/internal/contexts/ful"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// phyStub answers ConsolMembership v1 (SH-1 live in C-1, a SEA consolidation from Shanghai to Hamburg) and
// PhysicalLoadEvidence v1 as the test sets it.
type phyStub struct {
	mu       sync.Mutex
	at       time.Time
	evidence phyapi.PhysicalLoadEvidence
}

func (p *phyStub) GetConsolMembership(_ context.Context, q phyapi.GetConsolMembership) (phyapi.ConsolMembership, error) {
	entry := phyapi.MembershipEntry{EntryNo: 1, ConsolidationID: "C-1", AttachedAt: p.at}
	return phyapi.ConsolMembership{TenantID: q.TenantID, ShipmentID: q.ShipmentID, Stage: phyapi.StageMain, Revision: 1, Live: true, Entry: entry,
		Consolidation: phyapi.ConsolidationSnapshot{ConsolidationID: "C-1", Mode: phyapi.ModeSea, Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM")},
		History:       []phyapi.MembershipEntry{entry}}, nil
}

func (p *phyStub) GetPhysicalLoadEvidence(_ context.Context, q phyapi.GetPhysicalLoadEvidence) (phyapi.PhysicalLoadEvidence, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.evidence
	e.TenantID, e.ShipmentID = q.TenantID, q.ShipmentID
	return e, nil
}

func (p *phyStub) set(e phyapi.PhysicalLoadEvidence) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evidence = e
}

// TestLoadEvidenceOnPostgres records PHY's verified evidence of a shipment: a version per change of PHY's
// facts, the same facts again keeping the revision, the execution status and the measures relayed to AIR,
// departed packs final for planning, and the database refusing writes past FUL.
func TestLoadEvidenceOnPostgres(t *testing.T) {
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
	record := func(command string, expected int64) (api.Receipt, error) {
		return f.module.RecordVerifiedLoadEvidence(ctx, api.RecordVerifiedLoadEvidence{CommandID: command, TenantID: f.tenant, ActorID: "ops-1",
			ShipmentID: "SH-1", ExpectedRevision: expected, OccurredAt: f.at.Add(5 * time.Hour)})
	}
	plan := func() api.AirOperationPlan {
		t.Helper()
		p, err := f.module.GetAirOperationPlan(ctx, api.GetShipmentPlanSnapshot{TenantID: f.tenant, ShipmentID: "SH-1"})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := plan(); p.EvidenceVersion != 0 || p.ExecutionStatus != api.ExecutionNotStarted || p.MeasuredBasis != "" {
		t.Fatalf("before any evidence: %+v", p)
	}
	phy.set(phyapi.PhysicalLoadEvidence{Lots: []phyapi.LotEvidence{{LotID: "L-1", Completed: true, ReceivedPacks: 4,
		Measurement: phyapi.Measurement{Version: 1, GrossWeight: kg(205)}}}})
	first, err := record("evidence-1", 4)
	if err != nil || first.NewRevision != 5 {
		t.Fatalf("received: %+v %v", first, err)
	}
	if again, err := record("evidence-1", 4); err != nil || again != first {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if same, err := record("evidence-2", 5); err != nil || same.NewRevision != 5 {
		t.Fatalf("the same facts again keep the revision: %+v %v", same, err)
	}
	if p := plan(); p.EvidenceVersion != 1 || p.MeasuredBasis != api.MeasuredReceived || p.MeasuredPacks != 4 || p.MeasuredGrossWeight != kg(205) ||
		p.Revision != 5 || p.PlanVersion != 2 {
		t.Fatalf("received measures relayed: %+v", p)
	}
	tendered := phy.evidence
	tendered.Tenders = []phyapi.TenderEvidence{{ExecutionID: "X-1", ConsolidationID: "C-1", LoadedPacks: 4, GrossWeight: kg(207), Departed: 4}}
	phy.set(tendered)
	if r, err := record("evidence-3", 5); err != nil || r.NewRevision != 6 {
		t.Fatalf("tendered and departed: %+v %v", r, err)
	}
	if p := plan(); p.EvidenceVersion != 2 || p.MeasuredBasis != api.MeasuredTendered || p.MeasuredGrossWeight != kg(207) ||
		p.ExecutionStatus != api.ExecutionDeparted {
		t.Fatalf("tendered measures and the execution status: %+v", p)
	}
	if _, err = f.module.CancelShipment(ctx, api.CancelShipment{CommandID: "cancel", TenantID: f.tenant, ActorID: "planner-1", ShipmentID: "SH-1",
		ExpectedRevision: 6, Reason: "withdrawn", OccurredAt: f.at.Add(6 * time.Hour)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a departed shipment is not cancelled: %v", err)
	}
	types := f.outboxTypes(t)
	if !slices.Contains(types, api.ShipmentLoadEvidenceRecorded) || !slices.Contains(types, api.ShipmentExecutionStatusChanged) {
		t.Fatalf("events: %v", types)
	}
	for _, tc := range []struct {
		sql        string
		state      string
		constraint string
	}{
		{`UPDATE cargo_ful.shipment_load_evidence SET departed=0 WHERE tenant_id=$1`, "42501", ""},
		{`DELETE FROM cargo_ful.shipment_load_evidence WHERE tenant_id=$1`, "42501", ""},
		{`INSERT INTO cargo_ful.shipment_load_evidence (tenant_id,shipment_id,evidence_no,digest,lots,receipt_complete,received_packs,received_weight,
            received_volume,tendered_packs,tendered_weight,tendered_volume,departed,arrived,offloaded,open_discrepancies,execution_status,recorded_by,
            recorded_at,entered_at,departed_final) VALUES ($1,'SH-1',9,repeat('a',64),0,false,0,0,0,0,0,0,0,0,0,0,'NOT_STARTED','x',now(),now(),0)`, "23514", ""},
		{"", "23514", "shipment_departed_final"}, // a departed shipment cancelled behind FUL's back, below
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			if tc.sql != "" {
				_, err := e.Exec(ctx, tc.sql, f.tenant)
				return err
			}
			if _, err := e.Exec(ctx, `UPDATE cargo_ful.shipment_consol_attachment SET ended_by='x', ended_at=now(), end_reason='SHIPMENT_CANCELLED',
                end_entered_at=now() WHERE tenant_id=$1`, f.tenant); err != nil {
				return err
			}
			if _, err := e.Exec(ctx, `UPDATE cargo_ful.shipment_obligation SET released_by='x', released_at=now() WHERE tenant_id=$1 AND released_at IS NULL`,
				f.tenant); err != nil {
				return err
			}
			_, err := e.Exec(ctx, `UPDATE cargo_ful.shipment SET status='CANCELLED', cancelled_by='x', cancelled_at=now(), cancel_reason='x', revision=revision+1
                WHERE tenant_id=$1`, f.tenant)
			return err
		})
		var pgErr *pgconn.PgError
		if sqlState(err) != tc.state || (tc.constraint != "" && (!errors.As(err, &pgErr) || pgErr.ConstraintName != tc.constraint)) {
			t.Errorf("%q: %v (state %s), want %s %s", tc.sql, err, sqlState(err), tc.state, tc.constraint)
		}
	}
}
