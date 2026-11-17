//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fulmodule "go.idp.xyz/idp-cargo/internal/contexts/ful"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// TestOceanOperationPlanOnPostgres records the sealed container PHY reports for a SEA shipment and relays it in
// OceanOperationPlan v1 with the plan, the container mode, the goods description, the tendered measures and PHY's
// evidence ids; a shipment planned by another mode, or by none yet, is answered with its mode, and the stuffing rows
// are kept as FUL wrote them.
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
		func() (api.Receipt, error) {
			return f.module.CreateShipmentFromObligation(context.WithValue(ctx, holderKey{}, "SH-3"), f.create("SH-3", "create-3", "OB-3"))
		},
		func() (api.Receipt, error) {
			byAir := f.amend("amend-3", 1, 3, "by air")
			byAir.ShipmentID, byAir.TransportMode, byAir.Origin, byAir.Destination = "SH-3", api.ModeAir, unlocode("CNPVG"), unlocode("USJFK")
			return f.module.AmendShipmentPlan(context.WithValue(ctx, holderKey{}, "SH-3"), byAir)
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
	if p, err := plan("SH-2"); err != nil || p.TransportMode != "" || len(p.Stuffings) != 0 {
		t.Fatalf("a shipment planned by no mode yet: %+v %v", p, err)
	}
	if p, err := plan("SH-3"); err != nil || p.TransportMode != api.ModeAir || p.ContainerMode != "" {
		t.Fatalf("a shipment planned by AIR, so that SEA can end its association: %+v %v", p, err)
	}
	if _, err = plan("SH-404"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown shipment: %v", err)
	}

	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	sealed, closed := f.at.Add(4*time.Hour+20*time.Minute), f.at.Add(4*time.Hour+30*time.Minute) // sealed, then the stuffing closed
	digest := strings.Repeat("5e", 32)
	phy.set(phyapi.PhysicalLoadEvidence{Lots: []phyapi.LotEvidence{{LotID: "L-1", Completed: true, ReceivedPacks: 4,
		Measurement: phyapi.Measurement{Version: 1, GrossWeight: kg(205)}}},
		Tenders: []phyapi.TenderEvidence{
			{ExecutionID: "X-S1", ConsolidationID: "C-1", Kind: phyapi.ExecutionStuffing, State: phyapi.ExecutionClosed, LoadUnitNo: 1, LoadUnitType: "22G1",
				LoadedPacks: 3, GrossWeight: kg(154), ClosedAt: closed,
				Lines:         []phyapi.LoadLine{{LineNo: 1, LivePacks: 2}, {LineNo: 3, LivePacks: 1}},
				CloseEvidence: phyapi.Evidence{Source: phyapi.EvidenceSeal, Reference: "CN-SEAL-0042", SHA256: digest, OccurredAt: sealed}},
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
	want := api.ContainerStuffing{ExecutionID: "X-S1", LoadUnitNo: 1, TypeCode: "22G1", Packs: 3, GrossWeight: kg(154), LineNos: []int{1, 3},
		Seal: "CN-SEAL-0042", SealSHA256: digest, SealedAt: sealed}
	if s := p.Stuffings[0]; !reflect.DeepEqual(s, want) {
		t.Fatalf("the sealed container: %+v, want %+v", s, want)
	}
	stuffing := func(no int, typeCode, lines string) string {
		return fmt.Sprintf(`INSERT INTO cargo_ful.shipment_load_evidence_stuffing (tenant_id,shipment_id,evidence_no,stuffing_no,execution_id,load_unit_no,
            type_code,packs,gross_weight_unit,gross_weight,volume,line_nos,seal,sealed_at) VALUES ($1,'SH-1',1,%d,'X-9',3,'%s',1,'KGM',1,0,'%s','S',now())`,
			no, typeCode, lines)
	}
	for _, tc := range []struct{ sql, state, constraint string }{
		{`UPDATE cargo_ful.shipment_load_evidence_stuffing SET seal='x' WHERE tenant_id=$1`, "42501", ""},
		{`DELETE FROM cargo_ful.shipment_load_evidence_stuffing WHERE tenant_id=$1`, "42501", ""},
		{stuffing(2, "22G1", "{1}"), "23514", "shipment_load_evidence_stuffing_with_evidence"},
		{stuffing(3, "22g1", "{1}"), "23514", "shipment_load_evidence_stuffing_type_code_check"},
		{stuffing(4, "22G1", "{}"), "23514", "shipment_load_evidence_stuffing_line_nos_check"},
		{stuffing(5, "22G1", "{0}"), "23514", "shipment_load_evidence_stuffing_line_nos_check"},
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
	// Past the grants, the rows are kept even from the migrator.
	dsn := os.Getenv("IDP_CARGO_MIGRATOR_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("IDP_CARGO_MIGRATOR_DSN required in CI; skipping forbidden")
		}
		return
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, sql := range []string{`UPDATE cargo_ful.shipment_load_evidence_stuffing SET seal='x' WHERE tenant_id=$1`,
		`DELETE FROM cargo_ful.shipment_load_evidence_stuffing WHERE tenant_id=$1`} {
		if _, err := conn.Exec(ctx, sql, f.tenant); sqlState(err) != "23001" {
			t.Errorf("%q as the migrator: %v", sql, err)
		}
	}
}
