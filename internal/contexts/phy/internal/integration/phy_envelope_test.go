//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// TestEnvelopeAtCloseOnPostgres closes own-container consolidations against their planned units' envelope: a 22G1
// takes 28,230 kg of cargo. SH-B plans 600 cartons of 50 kg, 30,000 kg; SH-S 6 cartons, 300 kg.
func TestEnvelopeAtCloseOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.ful.sea["SH-B"], f.ful.sea["SH-S"], f.ful.packs["SH-B"] = true, true, 600
	for _, c := range []struct{ id, shipment string }{{"C-B", "SH-B"}, {"C-S", "SH-S"}} {
		f.createSea(t, c.id, api.KindSeaLCLOwn)
		ok(t, 2)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-"+c.id, c.id, 1, "22G1")))
		if _, err := f.module.AttachHouse(ctx, f.attach("attach-"+c.id, c.id, c.shipment, 0)); err != nil {
			t.Fatal(err)
		}
		f.ful.allocate(c.shipment, c.id)
	}
	f.createSea(t, "C-N", api.KindSeaLCLOwn) // no units
	f.ful.sea["SH-N"], f.ful.packs["SH-N"] = true, 600
	if _, err := f.module.AttachHouse(ctx, f.attach("attach-C-N", "C-N", "SH-N", 0)); err != nil {
		t.Fatal(err)
	}
	f.ful.allocate("SH-N", "C-N")

	big := f.closeCmd("close-B", "C-B", 2)
	if _, err := f.module.CloseConsolidation(ctx, big); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "PHY_ENVELOPE_EXCEEDED") ||
		!strings.Contains(err.Error(), "30000.000 kg gross, the 1 load units take 28230.000 kg") {
		t.Fatalf("30,000 kg into a 22G1 without a reason: %v", err)
	}
	big.CommandID, big.OverrunReason = "close-B-accepted", "the carrier's plate data allow 32,500 kg gross"
	ok(t, 3)(f.module.CloseConsolidation(ctx, big))
	if again, err := f.module.CloseConsolidation(ctx, big); err != nil || again.NewRevision != 3 {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if c := f.consolidation(t, "C-B"); c.Current.Change != "CLOSED" || c.Current.Reason != big.OverrunReason {
		t.Fatalf("C-B = %+v", c.Current)
	}
	small := f.closeCmd("close-S", "C-S", 2)
	small.OverrunReason = "just in case"
	if _, err := f.module.CloseConsolidation(ctx, small); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an overrun reason within the envelope: %v", err)
	}
	none := f.closeCmd("close-N", "C-N", 1)
	none.OverrunReason = "big"
	if _, err := f.module.CloseConsolidation(ctx, none); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an overrun reason without units: %v", err)
	}
	none.CommandID, none.OverrunReason = "close-N-plain", ""
	ok(t, 2)(f.module.CloseConsolidation(ctx, none))

	// The database keeps the same rule: a reason exactly when the plan exceeds the envelope.
	f.createSea(t, "C-D", api.KindSeaLCLOwn)
	ok(t, 2)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-C-D", "C-D", 1, "22G1")))
	f.ful.sea["SH-D"], f.ful.packs["SH-D"] = true, 600
	if _, err := f.module.AttachHouse(ctx, f.attach("attach-C-D", "C-D", "SH-D", 0)); err != nil {
		t.Fatal(err)
	}
	closeVersion := func(consolidation, reason string) error {
		return f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, `INSERT INTO cargo_phy.consolidation_version (tenant_id,consolidation_id,version_no,change,state,origin_code,
                destination_code,location_edition,planned_departure,house_cut_off,reason,written_by,written_at,entered_at)
                SELECT tenant_id,consolidation_id,version_no+1,'CLOSED','CLOSED',origin_code,destination_code,location_edition,planned_departure,
                  house_cut_off,nullif($3,''),'x',now(),now() FROM cargo_phy.consolidation_version WHERE tenant_id=$1 AND consolidation_id=$2
                ORDER BY version_no DESC LIMIT 1`, f.tenant, consolidation, reason)
			return err
		})
	}
	var pgErr *pgconn.PgError
	if err := closeVersion("C-D", ""); !errors.As(err, &pgErr) || pgErr.ConstraintName != "consolidation_close_envelope" {
		t.Fatalf("a close over the envelope without a reason in the database: %v", err)
	}
	if err := closeVersion("C-S", "just in case"); !errors.As(err, &pgErr) || pgErr.ConstraintName != "consolidation_close_envelope" {
		t.Fatalf("a reason within the envelope in the database: %v", err)
	}
}
