//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// SEA V2.88, migration 000010: what SEA took when it took the container as stuffed (PHY's seal as relayed and its
// sealed-at) is written with the stuffing by SEA's command, and checked by the database past SEA: set together, only at
// the step ASSIGNED to STUFFED, with a stuffing a live house of the movement carries, and never changed afterwards. A
// visit stuffed before the version has neither and still loads and takes commands.
func TestWhatSEATookAtStuffingOnPostgres(t *testing.T) {
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	for i, s := range []string{"SH-1", "SH-2", "SH-3"} {
		f.w.plan(s, "C-1", 1)
		f.stuffing(s, fmt.Sprintf("X-%d", i+1), i+1, "22G1", fmt.Sprintf("CN-SEAL-%d", i+1))
		if _, err := f.associate("assoc-"+s, "OM-1", s, "", f.get(t, "OM-1").Revision); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range []string{"CSQU3054383", "TEMU1234565", "HLXU1234561", "MSKU0000080"} {
		r, err := f.assign(fmt.Sprintf("CV-%d", i+1), "OM-1", i+1, c, 1)
		if err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			if _, err = f.seal(fmt.Sprintf("seal-%d", i+1), fmt.Sprintf("CV-%d", i+1), r.NewRevision, fmt.Sprintf("CN-SEAL-%d", i+1), api.SealShipper, 2); err != nil {
				t.Fatal(err)
			}
		}
	}
	// SEA's command writes what it took: the stuffing's seal as FUL relayed it and when it was sealed
	if _, err := f.stuffingDone("done-1", "CV-1", 2, "", 3); err != nil {
		t.Fatal(err)
	}
	read := func(visit string) (seal string, sealedAt time.Time, null bool) {
		t.Helper()
		var s *string
		var at *time.Time
		if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			return e.QueryRow(ctx, `SELECT stuffed_phy_seal, stuffed_phy_sealed_at FROM cargo_sea.container_visit WHERE tenant_id=$1 AND visit_id=$2`, f.tenant, visit).Scan(&s, &at)
		}); err != nil {
			t.Fatal(err)
		}
		if s == nil || at == nil {
			return "", time.Time{}, true
		}
		return *s, at.UTC(), false
	}
	if seal, at, null := read("CV-1"); null || seal != "CN-SEAL-1" || !at.Equal(f.at.Add(45*time.Minute)) {
		t.Fatalf("what SEA took: %q %v", seal, at)
	}
	if _, _, null := read("CV-2"); !null {
		t.Fatal("a container not yet stuffed has none")
	}
	const sealedOf = `(SELECT sealed_at FROM cargo_sea.house_bill_stuffing WHERE tenant_id=$1 AND execution_id='%s' LIMIT 1)`
	stuff := func(visit, exec, columns string) string {
		return fmt.Sprintf(`UPDATE cargo_sea.container_visit SET status='STUFFED',execution_id='%s',stuffed_by='x',stuffed_at=now()%s,revision=revision+1
          WHERE tenant_id=$1 AND visit_id='%s'`, exec, columns, visit)
	}
	for name, tc := range map[string]struct {
		sql   string
		state string
		in    string
	}{
		"a seal no live house carries": {stuff("CV-2", "X-2", `,stuffed_phy_seal='OTHER-SEAL',stuffed_phy_sealed_at=`+fmt.Sprintf(sealedOf, "X-2")), "23514",
			"with the seal and time it was taken with"},
		"another time than the house's": {stuff("CV-2", "X-2", `,stuffed_phy_seal='CN-SEAL-2',stuffed_phy_sealed_at=`+fmt.Sprintf(sealedOf, "X-2")+` - interval '1 minute'`), "23514",
			"with the seal and time it was taken with"},
		"the seal of another execution": {stuff("CV-2", "X-2", `,stuffed_phy_seal='CN-SEAL-3',stuffed_phy_sealed_at=`+fmt.Sprintf(sealedOf, "X-3")), "23514",
			"with the seal and time it was taken with"},
		"the seal without its time": {stuff("CV-2", "X-2", `,stuffed_phy_seal='CN-SEAL-2'`), "23514", "container_visit_stuffed_phy_seal"},
		"the time without its seal": {stuff("CV-2", "X-2", `,stuffed_phy_sealed_at=`+fmt.Sprintf(sealedOf, "X-2")), "23514", "container_visit_stuffed_phy_seal"},
		"a blank seal":              {stuff("CV-2", "X-2", `,stuffed_phy_seal='  ',stuffed_phy_sealed_at=`+fmt.Sprintf(sealedOf, "X-2")), "23514", "container_visit_stuffed_phy_seal"},
		"sealed after the container was taken": {stuff("CV-2", "X-2", `,stuffed_phy_seal='CN-SEAL-2',stuffed_phy_sealed_at=now() + interval '1 hour'`), "23514",
			"container_visit_stuffed_phy_seal"},
		"set on a visit that starts assigned": {`INSERT INTO cargo_sea.container_visit (tenant_id,visit_id,movement_id,consolidation_id,load_unit_no,container_number,size_type,
          status,assigned_by,assigned_at,entered_at,stuffed_phy_seal,stuffed_phy_sealed_at) VALUES ($1,'CV-9','OM-1','C-1',9,'MSKU1234565','22G1','ASSIGNED','x',now(),now(),
          'CN-SEAL-9','2020-01-01')`, "23514", "container_visit_stuffed_phy_seal"},
		"set on a container not stuffed": {`UPDATE cargo_sea.container_visit SET stuffed_phy_seal='CN-SEAL-4',stuffed_phy_sealed_at=now(),revision=revision+1
          WHERE tenant_id=$1 AND visit_id='CV-4'`, "23001", "leaves ASSIGNED once"},
		"changed after the stuffing": {`UPDATE cargo_sea.container_visit SET stuffed_phy_seal='CN-SEAL-9',revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1'`, "23001",
			"leaves ASSIGNED once"},
		"taken away after the stuffing": {`UPDATE cargo_sea.container_visit SET stuffed_phy_seal=NULL,stuffed_phy_sealed_at=NULL,revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1'`,
			"23001", "leaves ASSIGNED once"},
		"set with the status step of a VGM": {`UPDATE cargo_sea.container_visit SET status='VGM_SUBMITTED',stuffed_phy_seal='CN-SEAL-9',revision=revision+1
          WHERE tenant_id=$1 AND visit_id='CV-1'`, "23001", "leaves ASSIGNED once"},
	} {
		err := f.past(t, tc.sql)
		if state := sqlState(err); state != tc.state || !strings.Contains(refusal(err), tc.in) {
			t.Errorf("%s: %v (state %s, %s), want %s %s", name, err, state, refusal(err), tc.state, tc.in)
		}
	}
	if seal, _, null := read("CV-1"); null || seal != "CN-SEAL-1" {
		t.Fatalf("a refused write changed what SEA took: %q", seal)
	}
	// written past SEA with the right seal and time at the step: accepted
	if err := f.past(t, stuff("CV-2", "X-2", `,stuffed_phy_seal='CN-SEAL-2',stuffed_phy_sealed_at=`+fmt.Sprintf(sealedOf, "X-2"))); err != nil {
		t.Fatalf("the right seal and time at the step: %v", err)
	}
	if seal, _, null := read("CV-2"); null || seal != "CN-SEAL-2" {
		t.Fatalf("CV-2: %q", seal)
	}
	// stuffed before this version: neither column, at the same step; it loads, takes a VGM and finalizes like any
	if err := f.past(t, stuff("CV-3", "X-3", ``)); err != nil {
		t.Fatalf("a stuffing with neither column: %v", err)
	}
	if _, _, null := read("CV-3"); !null {
		t.Fatal("a visit stuffed before this version has none")
	}
	if v := f.visit(t, "CV-3"); v.Status != api.VisitStuffed || v.ExecutionID != "X-3" {
		t.Fatalf("the legacy visit: %+v", v)
	}
	if _, err := f.module.AssertVgm(context.Background(), api.AssertVgm{CommandID: "vgm-3", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-3", ExpectedRevision: f.visit(t, "CV-3").Revision,
		Mass: kg(3700), Method: api.VgmMethod2, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1, DeterminedAt: f.hours(4), LateReason: "keyed after the cut-off", Signatory: "Ada", OccurredAt: time.Now().UTC().Truncate(time.Microsecond)}); err != nil {
		t.Fatalf("a VGM on the legacy visit: %v", err)
	}
}

// A container stuffed before V2.88 (a row stuffed with neither column, a difference reason stated) follows PHY's new
// seal through the movement's history of entries, by the real commands: PHY's new seal has another key and the same
// sealed-at, dated before the stuffing, so only the history shows the reseal.
func TestALegacyVisitFollowsByTheEntryHistoryOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	f.w.plan("SH-1", "C-1", 1)
	f.stuffing("SH-1", "X-1", 1, "22G1", "CN-SEAL-1")
	if _, err := f.associate("assoc-1", "OM-1", "SH-1", "", 1); err != nil {
		t.Fatal(err)
	}
	r, err := f.assign("CV-1", "OM-1", 1, "CSQU3054383", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.seal("seal-1", "CV-1", r.NewRevision, "OTHER-SEAL", api.SealShipper, 2); err != nil {
		t.Fatal(err)
	}
	// stuffed before this version: no stored seal, with a difference reason
	if err = f.past(t, `UPDATE cargo_sea.container_visit SET status='STUFFED',execution_id='X-1',difference_reason='re-keyed',stuffed_by='x',stuffed_at=now(),
        revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1'`); err != nil {
		t.Fatal(err)
	}
	// PHY's seal is now another one, sealed at the same time as before
	f.stuffing("SH-1", "X-1", 1, "22G1", "CN-SEAL-2")
	if _, err = f.module.RefreshHouseBillFacts(ctx, api.RefreshHouseBillFacts{CommandID: "refresh-1", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: f.get(t, "OM-1").Revision, ShipmentID: "SH-1", OccurredAt: time.Now().UTC().Truncate(time.Microsecond)}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sealNumbers(f.visit(t, "CV-1")), " "); got != "OTHER-SEAL/SHIPPER CN-SEAL-2/SHIPPER" {
		t.Fatalf("the seal is followed through the history: %s", got)
	}
}
