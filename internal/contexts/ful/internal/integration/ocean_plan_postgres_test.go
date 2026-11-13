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

// TestOceanOperationPlanOnPostgres records the sealed container PHY reports for a SEA shipment and relays it in
// OceanOperationPlan v1 with the plan, the container mode, the goods description and the tendered measures; a
// shipment not planned by SEA is refused, and the stuffing rows are kept as FUL wrote them.
func TestOceanOperationPlanOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	phy := &phyStub{at: f.at.Add(4 * time.Hour)}
	module, err := fulmodule.New(f.db, f.com, shipmentPolicy{}, fulmodule.WithLocations(locationStub{}), fulmodule.WithConsolMemberships(phy),
		fulmodule.WithPhysicalEvidence(phy))
	if err != nil {
		t.Fatal(err)
	}
	f.module = module
	amend := f.amend("amend", 1, 3, "schedule")
	amend.ContainerMode = api.ContainerLCL
	for _, step := range []func() (api.Receipt, error){
		func() (api.Receipt, error) {
			return f.module.CreateShipmentFromObligation(ctx, f.create("SH-1", "create", "OB-1"))
		},
		func() (api.Receipt, error) { return f.module.AmendShipmentPlan(ctx, amend) },
		func() (api.Receipt, error) { return f.module.ConfirmShipmentPlan(ctx, f.confirm("confirm", 2)) },
		func() (api.Receipt, error) {
			return f.module.RecordConsolAttachment(ctx, api.RecordConsolAttachment{CommandID: "attach", TenantID: f.tenant, ActorID: "ops-1",
				ShipmentID: "SH-1", ExpectedRevision: 3, ConsolidationID: "C-1", OccurredAt: f.at.Add(4 * time.Hour)})
		},
		func() (api.Receipt, error) {
			return f.module.CreateShipmentFromObligation(context.WithValue(ctx, holderKey{}, "SH-2"), f.create("SH-2", "create-2", "OB-2"))
		},
	} {
		if _, err = step(); err != nil {
			t.Fatal(err)
		}
	}
	plan := func(shipment string) (api.OceanOperationPlan, error) {
		return f.module.GetOceanOperationPlan(ctx, api.GetShipmentPlanSnapshot{TenantID: f.tenant, ShipmentID: shipment})
	}
	if p, err := plan("SH-1"); err != nil || p.TransportMode != api.ModeSea || p.ContainerMode != api.ContainerLCL || p.ConsolidationID != "C-1" ||
		len(p.Parts) != 1 || p.Parts[0].GoodsDescription != "Pallets" || p.EvidenceVersion != 0 || len(p.Stuffings) != 0 ||
		p.ExecutionStatus != api.ExecutionNotStarted {
		t.Fatalf("before any evidence: %+v %v", p, err)
	}
	if _, err = plan("SH-2"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a shipment not planned by SEA: %v", err)
	}
	if _, err = plan("SH-404"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown shipment: %v", err)
	}

	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	sealed := f.at.Add(4*time.Hour + 30*time.Minute)
	phy.set(phyapi.PhysicalLoadEvidence{Lots: []phyapi.LotEvidence{{LotID: "L-1", Completed: true, ReceivedPacks: 4,
		Measurement: phyapi.Measurement{Version: 1, GrossWeight: kg(205)}}},
		Tenders: []phyapi.TenderEvidence{
			{ExecutionID: "X-S1", ConsolidationID: "C-1", Kind: phyapi.ExecutionStuffing, State: phyapi.ExecutionClosed, LoadUnitNo: 1, LoadUnitType: "22G1",
				LoadedPacks: 3, GrossWeight: kg(154), ClosedAt: sealed,
				CloseEvidence: phyapi.Evidence{Source: phyapi.EvidenceSeal, Reference: "CN-SEAL-0042", OccurredAt: sealed}},
			{ExecutionID: "X-S2", ConsolidationID: "C-1", Kind: phyapi.ExecutionStuffing, State: phyapi.ExecutionLoading, LoadUnitNo: 2,
				LoadUnitType: "42G1", LoadedPacks: 1, GrossWeight: kg(51)}, // not sealed yet
		}})
	if r, err := f.module.RecordVerifiedLoadEvidence(ctx, api.RecordVerifiedLoadEvidence{CommandID: "evidence", TenantID: f.tenant, ActorID: "ops-1",
		ShipmentID: "SH-1", ExpectedRevision: 4, OccurredAt: f.at.Add(5 * time.Hour)}); err != nil || r.NewRevision != 5 {
		t.Fatalf("evidence: %+v %v", r, err)
	}
	p, err := plan("SH-1")
	if err != nil || p.EvidenceVersion != 1 || p.MeasuredBasis != api.MeasuredTendered || p.MeasuredPacks != 3 || p.MeasuredGrossWeight != kg(154) ||
		len(p.Stuffings) != 1 {
		t.Fatalf("after the seal: %+v %v", p, err)
	}
	if s := p.Stuffings[0]; s != (api.ContainerStuffing{ExecutionID: "X-S1", LoadUnitNo: 1, TypeCode: "22G1", Packs: 3, GrossWeight: kg(154),
		Seal: "CN-SEAL-0042", SealedAt: sealed}) {
		t.Fatalf("the sealed container: %+v", s)
	}
	for _, tc := range []struct{ sql, state, constraint string }{
		{`UPDATE cargo_ful.shipment_load_evidence_stuffing SET seal='x' WHERE tenant_id=$1`, "42501", ""},
		{`DELETE FROM cargo_ful.shipment_load_evidence_stuffing WHERE tenant_id=$1`, "42501", ""},
		{`INSERT INTO cargo_ful.shipment_load_evidence_stuffing (tenant_id,shipment_id,evidence_no,stuffing_no,execution_id,load_unit_no,type_code,
            packs,gross_weight_unit,gross_weight,volume,seal,sealed_at) VALUES ($1,'SH-1',1,2,'X-9',3,'22G1',1,'KGM',1,0,'S',now())`, "23514",
			"shipment_load_evidence_stuffing_with_evidence"},
		{`INSERT INTO cargo_ful.shipment_load_evidence_stuffing (tenant_id,shipment_id,evidence_no,stuffing_no,execution_id,load_unit_no,type_code,
            packs,gross_weight_unit,gross_weight,volume,seal,sealed_at) VALUES ($1,'SH-1',1,3,'X-9',3,'40HC',1,'KGM',1,0,'S',now())`, "23514", ""},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		var pgErr *pgconn.PgError
		if sqlState(err) != tc.state || (tc.constraint != "" && (!errors.As(err, &pgErr) || pgErr.ConstraintName != tc.constraint)) {
			t.Errorf("%q: %v (state %s), want %s %s", tc.sql, err, sqlState(err), tc.state, tc.constraint)
		}
	}
}
