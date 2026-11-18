//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// migrator connects as the migrator role, which owns the tables, holds every privilege and bypasses row level
// security: what it is refused, a trigger refuses. nil when no DSN is set outside CI.
func (f *fixture) migrator(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv("IDP_CARGO_MIGRATOR_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("IDP_CARGO_MIGRATOR_DSN required in CI; skipping forbidden")
		}
		return nil
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// past runs one statement past SEA on the application role, in the tenant's scope, and commits.
func (f *fixture) past(t *testing.T, sql string) error {
	t.Helper()
	return f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, sql, f.tenant)
		return err
	})
}

// insertions used by several cases: a master bill reference, a booking link with one leg, and a house bill with its
// registered HBL and one part, each as one statement, so that only the rule under test can refuse it.
const (
	billColumns = `(tenant_id,movement_id,bill_no,bill_number,kind,issuer_party_id,evidence_sha256,evidence_storage_id,evidence_media_type,
        evidence_captured_at,status,recorded_by,recorded_at,entered_at)`
	// %s: movement, link number, booking, capacity version
	linkWithLeg = `WITH l AS (INSERT INTO cargo_sea.booking_link (tenant_id,movement_id,link_no,booking_id,booking_revision,capacity_version,linked_by,
        linked_at,entered_at) VALUES ($1,'%[1]s',%[2]d,'%[3]s',9,%[4]d,'x',now(),now()) RETURNING link_no)
        INSERT INTO cargo_sea.voyage_leg (tenant_id,movement_id,link_no,leg_no,vessel,voyage,from_code,to_code,etd)
        SELECT $1,'%[1]s',link_no,1,'LATER','1','CNSHG','DEHAM',now() FROM l`
	// %s: movement, entry number, shipment, HBL, and the columns and values of an end, if any
	houseWithPart = `WITH n AS (INSERT INTO cargo_sea.hbl_number (tenant_id,legal_entity_id,hbl,shipment_id,drawn,claimed_at)
          VALUES ($1,'LE','%[4]s','%[3]s',false,now()) RETURNING hbl),
        e AS (INSERT INTO cargo_sea.house_bill (tenant_id,movement_id,entry_no,shipment_id,hbl,ful_revision,plan_version,window_from,packs,
          gross_weight_unit,gross_weight,basis,start_kind,associated_by,associated_at,entered_at%[5]s)
          SELECT $1,'%[1]s',%[2]d,'%[3]s',hbl,1,1,now(),1,'KGM',1,'PLANNED','ASSOCIATED','x',now(),now()%[6]s FROM n RETURNING entry_no)
        INSERT INTO cargo_sea.house_bill_part (tenant_id,movement_id,entry_no,part_no,obligation_id,obligation_version,order_id,packs,package_type,
          gross_weight_unit,gross_weight) SELECT $1,'%[1]s',entry_no,1,'OB-X',1,'O-X',1,'CT','KGM',1 FROM e`
)

// TestDatabaseRules writes past SEA. OM-1 is open with a house in a sealed container and two master bill references;
// OM-2 is cancelled.
func TestDatabaseRules(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	f.w.booking("BK-2", "C-2", "EVER GLORY", "", "0124W", 1, f.hours(96))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	f.w.plan("SH-1", "C-1", 2)
	f.w.stuffed("SH-1", 1, "CN-SEAL-1")
	if _, err := f.associate("assoc", "OM-1", "SH-1", "", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.masterBill("mbl-1", "OM-1", "MAEU1", api.MasterBillCarrierMBL, 2, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.masterBill("mbl-2", "OM-1", "MAEU2", api.MasterBillCarrierMBL, 3, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.open("OM-2", "C-2", "BK-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.CancelOceanMovement(ctx, api.CancelOceanMovement{CommandID: "cancel-2", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-2",
		ExpectedRevision: 1, Reason: "vessel omitted the port", OccurredAt: f.hours(1)}); err != nil {
		t.Fatal(err)
	}

	// What never changes. The application role holds no privilege for these (42501); the migrator holds every
	// privilege and is refused by the triggers (23001).
	immutable := map[string]string{
		"a leg changed":            `UPDATE cargo_sea.voyage_leg SET vessel='OTHER' WHERE tenant_id=$1`,
		"a cut-off changed":        `UPDATE cargo_sea.voyage_cut_off SET cut_off_at=now() WHERE tenant_id=$1`,
		"a cut-off deleted":        `DELETE FROM cargo_sea.voyage_cut_off WHERE tenant_id=$1`,
		"a link changed":           `UPDATE cargo_sea.booking_link SET capacity_version=9 WHERE tenant_id=$1`,
		"the register changed":     `UPDATE cargo_sea.hbl_number SET shipment_id='SH-9' WHERE tenant_id=$1`,
		"a house's facts changed":  `UPDATE cargo_sea.house_bill SET packs=1 WHERE tenant_id=$1`,
		"a house deleted":          `DELETE FROM cargo_sea.house_bill WHERE tenant_id=$1`,
		"a part changed":           `UPDATE cargo_sea.house_bill_part SET packs=1 WHERE tenant_id=$1`,
		"a container changed":      `UPDATE cargo_sea.house_bill_stuffing SET seal='OTHER' WHERE tenant_id=$1`,
		"the series changed":       `UPDATE cargo_sea.hbl_series SET first_number=2 WHERE tenant_id=$1`,
		"the sequence deleted":     `DELETE FROM cargo_sea.numbering_sequence WHERE scope=$1`,
		"the carrier changed":      `UPDATE cargo_sea.ocean_movement SET carrier_party_id='line-2' WHERE tenant_id=$1 AND movement_id='OM-1'`,
		"a movement deleted":       `DELETE FROM cargo_sea.ocean_movement WHERE tenant_id=$1 AND movement_id='OM-1'`,
		"a master bill renumbered": `UPDATE cargo_sea.master_bill SET bill_number='MAEU9' WHERE tenant_id=$1 AND status='CURRENT'`,
		"a master bill deleted":    `DELETE FROM cargo_sea.master_bill WHERE tenant_id=$1`,
		"the writing transaction":  `UPDATE cargo_sea.house_bill SET created_xact=pg_current_xact_id() WHERE tenant_id=$1`,
	}
	migrator := f.migrator(t)
	for name, sql := range immutable {
		if err := f.past(t, sql); sqlState(err) != "42501" {
			t.Errorf("%s on the application role: %v (state %s), want 42501", name, err, sqlState(err))
		}
		if migrator == nil {
			continue
		}
		tx, err := migrator.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, sql, f.tenant)
		_ = tx.Rollback(ctx)
		if sqlState(err) != "23001" {
			t.Errorf("%s as the migrator: %v (state %s), want 23001", name, err, sqlState(err))
		}
	}

	// What the application role may write, and the rule that refuses each write: at once (a CHECK, a unique index, a
	// row trigger) or at commit.
	insertBill := `INSERT INTO cargo_sea.master_bill ` + billColumns
	nextBill := `WITH s AS (UPDATE cargo_sea.master_bill SET status='SUPERSEDED' WHERE tenant_id=$1 AND movement_id='OM-1' AND status='CURRENT' RETURNING bill_no)
        ` + insertBill + ` SELECT $1,'OM-1',bill_no+1,'MAEU3','%s','%s',repeat('a',64),'s','m',now(),'CURRENT','x',now(),now() FROM s`
	movement := `INSERT INTO cargo_sea.ocean_movement (tenant_id,movement_id,consolidation_id,kind,operating_legal_entity_id,carrier_party_id,
        carrier_party_revision,carrier_role,origin_code,destination_code,location_edition,state,booking_id)
        VALUES ($1,'OM-9','C-9','SEA_LCL_OWN','LE','x',1,'%s','CNSHG','DEHAM','E1','OPEN','BK-9')`
	ended := `,end_kind,ended_by,ended_at,end_entered_at`
	for name, tc := range map[string]struct {
		sql   string
		state string
		in    string // the constraint, or part of the message
	}{
		"the sequence backwards": {`UPDATE cargo_sea.numbering_sequence SET next_val=0 WHERE scope=$1`, "23001", "one number at a time"},
		"a sequence of no series": {`INSERT INTO cargo_sea.numbering_sequence (scope,doc_type,period,next_val) VALUES ($1,'HBL/LE-NONE','',1)`, "23001",
			"starts at 1 or at its series"},
		"a master bill made current again": {`UPDATE cargo_sea.master_bill SET status='CURRENT' WHERE tenant_id=$1 AND status='SUPERSEDED'`, "23001",
			"only leaves CURRENT"},
		"a superseded master bill voided": {`UPDATE cargo_sea.master_bill SET status='VOIDED' WHERE tenant_id=$1 AND status='SUPERSEDED'`, "23001",
			"only leaves CURRENT"},
		"a house written again without ending it": {`UPDATE cargo_sea.house_bill SET ended_at=NULL,end_kind=NULL,ended_by=NULL,end_entered_at=NULL
            WHERE tenant_id=$1`, "23001", "can only be ended, once"},
		"a cancelled movement changed": {`UPDATE cargo_sea.ocean_movement SET revision=revision+1 WHERE tenant_id=$1 AND movement_id='OM-2'`, "23001",
			"never once cancelled"},
		"a second current master bill": {insertBill + ` VALUES ($1,'OM-1',3,'MAEU3','CARRIER_MBL','line-1',repeat('a',64),'s','m',now(),'CURRENT','x',now(),now())`,
			"23505", "master_bill_one_current"},
		"a master bill out of number": {insertBill + ` VALUES ($1,'OM-1',9,'MAEU3','CARRIER_MBL','line-1',repeat('a',64),'s','m',now(),'SUPERSEDED','x',now(),now())`,
			"23514", "ocean_movement_consistent"},
		"a master bill of another issuer": {fmt.Sprintf(nextBill, "CARRIER_MBL", "line-2"), "23514", "ocean_movement_consistent"},
		"a co-load bill on a container":   {fmt.Sprintf(nextBill, "COLOAD_BILL", "line-1"), "23514", "ocean_movement_consistent"},
		"the current master bill voided on an open movement": {`UPDATE cargo_sea.master_bill SET status='VOIDED' WHERE tenant_id=$1 AND status='CURRENT'`,
			"23514", "ocean_movement_consistent"},
		"evidence captured after it was recorded": {insertBill + ` VALUES ($1,'OM-1',3,'MAEU3','CARRIER_MBL','line-1',repeat('a',64),'s','m',now(),
            'SUPERSEDED','x',now() - interval '1 hour',now())`, "23514", "master_bill_evidence_time"},
		"a master bill for a cancelled movement": {insertBill + ` VALUES ($1,'OM-2',1,'MAEU3','CARRIER_MBL','line-1',repeat('a',64),'s','m',now(),'VOIDED','x',
            now(),now())`, "23514", "ocean_movement_consistent"},
		"a house with an unregistered HBL": {`WITH e AS (INSERT INTO cargo_sea.house_bill (tenant_id,movement_id,entry_no,shipment_id,hbl,ful_revision,
            plan_version,window_from,packs,gross_weight_unit,gross_weight,basis,start_kind,associated_by,associated_at,entered_at)
            VALUES ($1,'OM-1',2,'SH-9','NOPE1',1,1,now(),1,'KGM',1,'PLANNED','ASSOCIATED','x',now(),now()) RETURNING entry_no)
            INSERT INTO cargo_sea.house_bill_part (tenant_id,movement_id,entry_no,part_no,obligation_id,obligation_version,order_id,packs,package_type,
            gross_weight_unit,gross_weight) SELECT $1,'OM-1',entry_no,1,'OB-X',1,'O-X',1,'CT','KGM',1 FROM e`, "23514", "house_bill_consistent"},
		"a house without a part": {`WITH n AS (INSERT INTO cargo_sea.hbl_number (tenant_id,legal_entity_id,hbl,shipment_id,drawn,claimed_at)
            VALUES ($1,'LE','PAST1','SH-9',false,now()) RETURNING hbl)
            INSERT INTO cargo_sea.house_bill (tenant_id,movement_id,entry_no,shipment_id,hbl,ful_revision,plan_version,window_from,packs,gross_weight_unit,
            gross_weight,basis,start_kind,associated_by,associated_at,entered_at)
            SELECT $1,'OM-1',2,'SH-9',hbl,1,1,now(),1,'KGM',1,'PLANNED','ASSOCIATED','x',now(),now() FROM n`, "23514", "house_bill_consistent"},
		"a house for a cancelled movement": {fmt.Sprintf(houseWithPart, "OM-2", 1, "SH-9", "PAST2", "", ""), "23514", "house_bill_consistent"},
		"an ended house for a cancelled movement": {fmt.Sprintf(houseWithPart, "OM-2", 1, "SH-9", "PAST3", ended, `,'DETACHED','x',now(),now()`), "23514",
			"house_bill_consistent"},
		"measures without evidence": {`INSERT INTO cargo_sea.house_bill (tenant_id,movement_id,entry_no,shipment_id,hbl,ful_revision,plan_version,window_from,
            packs,gross_weight_unit,gross_weight,basis,start_kind,associated_by,associated_at,entered_at)
            VALUES ($1,'OM-1',2,'SH-9','NOPE1',1,1,now(),1,'KGM',1,'TENDERED','ASSOCIATED','x',now(),now())`, "23514", "house_bill_measured"},
		"an FCL house": {`INSERT INTO cargo_sea.house_bill (tenant_id,movement_id,entry_no,shipment_id,hbl,ful_revision,plan_version,container_mode,
            window_from,packs,gross_weight_unit,gross_weight,basis,start_kind,associated_by,associated_at,entered_at)
            VALUES ($1,'OM-1',2,'SH-9','NOPE1',1,1,'FCL',now(),1,'KGM',1,'PLANNED','ASSOCIATED','x',now(),now())`, "23514", "container_mode"},
		"a leg added later": {`INSERT INTO cargo_sea.voyage_leg (tenant_id,movement_id,link_no,leg_no,vessel,voyage,from_code,to_code,etd)
            VALUES ($1,'OM-1',1,2,'LATER','1','DEHAM','NLRTM',now())`, "23514", "voyage_with_link"},
		"a wrong IMO check digit": {`INSERT INTO cargo_sea.voyage_leg (tenant_id,movement_id,link_no,leg_no,vessel,vessel_imo,voyage,from_code,to_code,etd)
            VALUES ($1,'OM-1',1,2,'LATER','9811001','1','DEHAM','NLRTM',now())`, "23514", "voyage_leg_imo"},
		"an IMO number of letters": {`INSERT INTO cargo_sea.voyage_leg (tenant_id,movement_id,link_no,leg_no,vessel,vessel_imo,voyage,from_code,to_code,etd)
            VALUES ($1,'OM-1',1,2,'LATER','IMO9811','1','DEHAM','NLRTM',now())`, "23514", "voyage_leg_imo"},
		"a vessel's name with a space after it": {`INSERT INTO cargo_sea.voyage_leg (tenant_id,movement_id,link_no,leg_no,vessel,voyage,from_code,to_code,etd)
            VALUES ($1,'OM-1',1,2,'LATER ','1','DEHAM','NLRTM',now())`, "23514", "voyage_leg_vessel_check"},
		"a cut-off added later": {`INSERT INTO cargo_sea.voyage_cut_off (tenant_id,movement_id,link_no,leg_no,cut_off_no,kind,cut_off_at,time_zone)
            VALUES ($1,'OM-1',1,1,9,'HAZARDOUS',now(),'Asia/Shanghai')`, "23514", "voyage_with_link"},
		"an AIR cut-off": {`INSERT INTO cargo_sea.voyage_cut_off (tenant_id,movement_id,link_no,leg_no,cut_off_no,kind,cut_off_at,time_zone)
            VALUES ($1,'OM-1',1,1,9,'CARGO_ACCEPTANCE',now(),'Asia/Shanghai')`, "23514", "voyage_cut_off_kind_check"},
		"a link without a leg": {`INSERT INTO cargo_sea.booking_link (tenant_id,movement_id,link_no,booking_id,booking_revision,capacity_version,linked_by,
            linked_at,entered_at) VALUES ($1,'OM-1',2,'BK-1',9,2,'x',now(),now())`, "23514", "ocean_movement_consistent"},
		"a link of another booking":       {fmt.Sprintf(linkWithLeg, "OM-1", 2, "BK-9", 2), "23514", "ocean_movement_consistent"},
		"a link out of number":            {fmt.Sprintf(linkWithLeg, "OM-1", 7, "BK-1", 2), "23514", "ocean_movement_consistent"},
		"a link for a cancelled movement": {fmt.Sprintf(linkWithLeg, "OM-2", 2, "BK-2", 2), "23514", "ocean_movement_consistent"},
		"a part added later": {`INSERT INTO cargo_sea.house_bill_part (tenant_id,movement_id,entry_no,part_no,obligation_id,obligation_version,order_id,packs,
            package_type,gross_weight_unit,gross_weight) VALUES ($1,'OM-1',1,9,'OB-X',1,'O-X',1,'CT','KGM',1)`, "23514", "house_bill_facts_with_entry"},
		"a container added later": {`INSERT INTO cargo_sea.house_bill_stuffing (tenant_id,movement_id,entry_no,stuffing_no,execution_id,load_unit_no,type_code,
            packs,line_nos,seal,sealed_at) VALUES ($1,'OM-1',1,9,'X-9',2,'42G1',1,'{1}','S-9',now())`, "23514", "house_bill_facts_with_entry"},
		// ending an entry rewrites its row: the facts are still refused from another transaction than the one that wrote it
		"a part added by the transaction that ends the entry": {`WITH e AS (UPDATE cargo_sea.house_bill SET end_kind='DETACHED',ended_by='x',ended_at=now(),
            end_entered_at=now() WHERE tenant_id=$1 AND movement_id='OM-1' AND entry_no=1 RETURNING entry_no)
            INSERT INTO cargo_sea.house_bill_part (tenant_id,movement_id,entry_no,part_no,obligation_id,obligation_version,order_id,packs,package_type,
            gross_weight_unit,gross_weight) SELECT $1,'OM-1',entry_no,9,'OB-X',1,'O-X',1,'CT','KGM',1 FROM e`, "23514", "house_bill_facts_with_entry"},
		"a container without load lines": {`INSERT INTO cargo_sea.house_bill_stuffing (tenant_id,movement_id,entry_no,stuffing_no,execution_id,load_unit_no,
            type_code,packs,line_nos,seal,sealed_at) VALUES ($1,'OM-1',1,9,'X-9',2,'42G1',1,'{}','S-9',now())`, "23514", "house_bill_stuffing_line_nos_check"},
		"cancelled with a house": {`UPDATE cargo_sea.ocean_movement SET state='CANCELLED',cancel_reason='x',cancelled_by='x',cancelled_at=now(),
            revision=revision+1 WHERE tenant_id=$1 AND movement_id='OM-1'`, "23514", "ocean_movement_consistent"},
		"a co-loader on an own container":     {fmt.Sprintf(movement, "CO_LOADER"), "23514", "ocean_movement_carrier_role"},
		"a movement without its booking link": {fmt.Sprintf(movement, "CARRIER"), "23514", "ocean_movement_consistent"},
	} {
		err := f.past(t, tc.sql)
		if state := sqlState(err); state != tc.state || !strings.Contains(refusal(err), tc.in) {
			t.Errorf("%s: %v (state %s, %s), want %s %s", name, err, state, refusal(err), tc.state, tc.in)
		}
		t.Logf("%s refused: %v", name, err)
	}

	// nothing above changed either movement
	if m := f.get(t, "OM-1"); m.Revision != 4 || m.State != api.StateOpen || len(m.Houses) != 1 || len(m.Houses[0].Containers) != 1 ||
		len(m.Houses[0].Parts) != 1 || m.MasterBillNumber != "MAEU2" || len(m.MasterBills) != 2 || len(m.Legs) != 1 || len(m.Legs[0].CutOffs) != 2 {
		t.Fatalf("the open movement after the refused writes: %+v", m)
	}
	if m := f.get(t, "OM-2"); m.Revision != 2 || m.State != api.StateCancelled || len(m.Houses) != 0 || len(m.MasterBills) != 0 || len(m.Legs) != 1 {
		t.Fatalf("the cancelled movement after the refused writes: %+v", m)
	}
	if ref, err := f.module.FindHouseBill(ctx, api.FindHouseBill{TenantID: f.tenant, LegalEntityID: "LE", HBL: "PAST1"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an HBL registered by a refused write: %+v %v", ref, err)
	}
	// another tenant sees and changes nothing of it
	other := scope.TenantTransactor{DB: f.db, Tenant: f.tenant + "-other"}
	if err := other.WithinTransaction(ctx, func(txCtx context.Context) error {
		e, err := f.db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		var movements, bills, houses, numbers int
		if err = e.QueryRow(txCtx, `SELECT (SELECT count(*) FROM cargo_sea.ocean_movement), (SELECT count(*) FROM cargo_sea.master_bill),
            (SELECT count(*) FROM cargo_sea.house_bill), (SELECT count(*) FROM cargo_sea.numbering_sequence)`).Scan(&movements, &bills, &houses, &numbers); err != nil {
			return err
		}
		if movements != 0 || bills != 0 || houses != 0 || numbers != 0 {
			return fmt.Errorf("another tenant reads %d movements, %d master bills, %d house bills, %d sequences", movements, bills, houses, numbers)
		}
		tag, err := e.Exec(txCtx, `UPDATE cargo_sea.master_bill SET status='SUPERSEDED' WHERE status='CURRENT'`)
		if err == nil && tag.RowsAffected() != 0 {
			err = fmt.Errorf("another tenant changed %d master bills", tag.RowsAffected())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAnEntryAndACancellationCannotPassEachOther writes a live house past SEA and has its checks run before the commit;
// SEA's cancellation of the movement then waits for that transaction and is refused for the house, instead of
// committing in between and leaving a cancelled movement with a live house.
func TestAnEntryAndACancellationCannotPassEachOther(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	cancelled := make(chan error, 1)
	passed := false
	if err := f.within(t, func(txCtx context.Context, e bentopg.Executor) error {
		if _, err := e.Exec(txCtx, fmt.Sprintf(houseWithPart, "OM-1", 1, "SH-PAST", "PAST1", "", ""), f.tenant); err != nil {
			return err
		}
		if _, err := e.Exec(txCtx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			return err
		}
		go func() {
			_, err := f.module.CancelOceanMovement(ctx, api.CancelOceanMovement{CommandID: "cancel", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
				ExpectedRevision: 1, Reason: "no cargo", OccurredAt: f.hours(1)})
			cancelled <- err
		}()
		select {
		case err := <-cancelled: // the cancellation did not wait for this transaction
			passed = true
			cancelled <- err
		case <-time.After(500 * time.Millisecond):
		}
		return nil
	}); err != nil {
		t.Fatalf("the house written past SEA: %v", err)
	}
	if err := <-cancelled; passed || !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "houses are associated") {
		t.Fatalf("the cancellation passed the entry (%v): %v", passed, err)
	}
	if m := f.get(t, "OM-1"); m.State != api.StateOpen || len(m.Houses) != 1 || m.Houses[0].HBL != "PAST1" || m.Revision != 1 {
		t.Fatalf("the movement: %+v", m)
	}
}
