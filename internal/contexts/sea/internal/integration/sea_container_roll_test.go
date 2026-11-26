//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// A whole container rolled to a later vessel (V2.105; 验收标准_V2.105_整箱改到另一航次.md): C-1 sails in OM-1 with two
// sealed containers, SH-1 in CV-1 (unit 1) and SH-2 in CV-2 (unit 2), each with its VGM.
func newRollFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	numbers := []string{"CSQU3054383", "TEMU1234565"}
	for i, s := range []string{"SH-1", "SH-2"} {
		f.w.plan(s, "C-1", 1)
		f.stuffing(s, fmt.Sprintf("X-%d", i+1), i+1, "22G1", fmt.Sprintf("CN-SEAL-%d", i+1))
		if _, err := f.associate("assoc-"+s, "OM-1", s, "", f.get(t, "OM-1").Revision); err != nil {
			t.Fatal(err)
		}
		visit := fmt.Sprintf("CV-%d", i+1)
		r, err := f.assign(visit, "OM-1", i+1, numbers[i], 1)
		if err != nil {
			t.Fatal(err)
		}
		if r, err = f.seal("seal-"+visit, visit, r.NewRevision, fmt.Sprintf("CN-SEAL-%d", i+1), api.SealShipper, 2); err != nil {
			t.Fatal(err)
		}
		if r, err = f.stuffingDone("done-"+visit, visit, r.NewRevision, "", 3); err != nil {
			t.Fatal(err)
		}
		if _, err = f.module.AssertVgm(ctx, api.AssertVgm{CommandID: "vgm-" + visit, TenantID: f.tenant, ActorID: "ops-1", VisitID: visit, ExpectedRevision: r.NewRevision,
			Mass: kg(3700), Method: api.VgmMethod2, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1, DeterminedAt: f.hours(4), Signatory: "Ada", OccurredAt: f.hours(4)}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) roll(t *testing.T, command, visit, to, reason string, hours int) (api.Receipt, error) {
	t.Helper()
	return f.module.RollContainerToMovement(context.Background(), api.RollContainerToMovement{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", VisitID: visit,
		ExpectedRevision: f.visit(t, visit).Revision, ToMovementID: to, Reason: reason, OccurredAt: f.hours(hours)})
}

func houseIDs(m api.OceanMovement) []string {
	var out []string
	for _, h := range m.Houses {
		out = append(out, h.ShipmentID+"/"+h.Start)
	}
	return out
}

func TestAContainerRolledToANewBookingSailsInAMovementOfItsOwnOnPostgres(t *testing.T) {
	f := newRollFixture(t)
	// the carrier gives CV-2 a new booking on a later vessel, allocated to the same consolidation
	f.w.booking("BK-2", "C-1", "EVER GLORY", "", "0130W", 1, f.hours(240))
	if _, err := f.open("OM-2", "C-1", "BK-2"); err != nil {
		t.Fatalf("a second movement of C-1 on another booking: %v", err)
	}
	// a third movement on the same booking line is refused
	if _, err := f.module.OpenOceanMovement(context.Background(), api.OpenOceanMovement{CommandID: "open-OM-3", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "OM-3", ConsolidationID: "C-1", BookingID: "BK-2", OccurredAt: f.hours(5)}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("two movements on one booking line: %v", err)
	}
	// refused: no reason; to a movement of another consolidation; the same movement
	f.w.booking("BK-9", "C-9", "EVER GLORY", "", "0130W", 1, f.hours(240))
	if _, err := f.open("OM-9", "C-9", "BK-9"); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ to, reason string }{
		"no reason":                    {"OM-2", " "},
		"another consolidation's":      {"OM-9", "rolled"},
		"the movement it is in":        {"OM-1", "rolled"},
		"a movement SEA does not know": {"OM-NONE", "rolled"},
	} {
		if _, err := f.roll(t, "roll-refused-"+tc.to, "CV-2", tc.to, tc.reason, 6); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	cmd := api.RollContainerToMovement{CommandID: "roll-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-2", ExpectedRevision: f.visit(t, "CV-2").Revision,
		ToMovementID: "OM-2", Reason: "shut out at the terminal: the carrier rolled it to EVER GLORY 0130W", OccurredAt: f.hours(6)}
	r, err := f.module.RollContainerToMovement(context.Background(), cmd)
	if err != nil {
		t.Fatalf("the roll: %v", err)
	}
	if again, err := f.module.RollContainerToMovement(context.Background(), cmd); err != nil || again != r {
		t.Fatalf("replayed: %+v %v", again, err)
	}
	v := f.visit(t, "CV-2")
	if v.MovementID != "OM-2" || v.Status != api.VisitVGM || len(v.Rolls) != 1 || v.Rolls[0].FromMovementID != "OM-1" || v.Rolls[0].ToMovementID != "OM-2" ||
		v.Rolls[0].By != "ops-1" || !strings.Contains(v.Rolls[0].Reason, "shut out") || len(v.Seals) != 1 || len(v.Assertions) != 1 {
		t.Fatalf("CV-2: %+v", v)
	}
	one, two := f.get(t, "OM-1"), f.get(t, "OM-2")
	if got := houseIDs(one); len(got) != 1 || got[0] != "SH-1/ASSOCIATED" {
		t.Fatalf("OM-1's houses: %v", got)
	}
	if got := houseIDs(two); len(got) != 1 || got[0] != "SH-2/ROLLED_WITH_CONTAINER" || two.Houses[0].HBL == "" {
		t.Fatalf("OM-2's houses: %v", got)
	}
	if len(one.Containers) != 1 || one.Containers[0].VisitID != "CV-1" || len(two.Containers) != 1 || two.Containers[0].VisitID != "CV-2" {
		t.Fatalf("containers: %+v / %+v", one.Containers, two.Containers)
	}
	if two.Legs[0].Vessel != "EVER GLORY" || one.Legs[0].Vessel != "EVER GIVEN" {
		t.Fatalf("voyages: %+v / %+v", one.Legs, two.Legs)
	}
	types := strings.Join(f.outboxTypes(t), ",")
	if !strings.Contains(types, "ContainerRolledToMovement.v1") {
		t.Fatalf("events: %s", types)
	}
}

func TestAContainerRolledUnderTheSameBookingSailsOnItsNewLineOnPostgres(t *testing.T) {
	f := newRollFixture(t)
	// the same booking, a new capacity version with two lines: line 1 EVER GIVEN for CV-1, line 2 EVER GLORY for CV-2
	f.w.mu.Lock()
	b := f.w.bookings["BK-1"]
	b.Revision++
	b.Capacity.Version = 2
	later := b.Capacity.Lines[0]
	later.Line = 2
	later.Legs = append([]bookapi.CapacityLeg(nil), later.Legs...)
	later.Legs[0].Vessel, later.Legs[0].Voyage = "EVER GLORY", "0130W"
	later.Legs[0].ETD, later.Legs[0].ETA = f.hours(240), f.hours(240+30*24)
	b.Capacity.Lines = append(b.Capacity.Lines, later)
	f.w.bookings["BK-1"] = b
	f.w.mu.Unlock()
	if _, err := f.module.LinkCarrierBooking(context.Background(), api.LinkCarrierBooking{CommandID: "link-v2", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "OM-1", ExpectedRevision: f.get(t, "OM-1").Revision, OccurredAt: f.hours(5)}); err != nil {
		t.Fatalf("OM-1 takes version 2, its line 1: %v", err)
	}
	if _, err := f.module.OpenOceanMovement(context.Background(), api.OpenOceanMovement{CommandID: "open-OM-2", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "OM-2", ConsolidationID: "C-1", BookingID: "BK-1", BookingLine: 2, OccurredAt: f.hours(5)}); err != nil {
		t.Fatalf("OM-2 on line 2: %v", err)
	}
	if _, err := f.module.OpenOceanMovement(context.Background(), api.OpenOceanMovement{CommandID: "open-OM-3", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "OM-3", ConsolidationID: "C-1", BookingID: "BK-1", BookingLine: 3, OccurredAt: f.hours(5)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a line the booking does not have: %v", err)
	}
	if _, err := f.roll(t, "roll-1", "CV-2", "OM-2", "rolled by the carrier under the same booking", 6); err != nil {
		t.Fatalf("the roll: %v", err)
	}
	one, two := f.get(t, "OM-1"), f.get(t, "OM-2")
	if one.BookingLine != 1 || two.BookingLine != 2 || two.BookingID != "BK-1" || two.Legs[0].Vessel != "EVER GLORY" || one.Legs[0].Vessel != "EVER GIVEN" {
		t.Fatalf("lines: %+v / %+v", one, two)
	}
	if houseIDs(two)[0] != "SH-2/ROLLED_WITH_CONTAINER" || len(one.Houses) != 1 {
		t.Fatalf("houses: %v / %v", houseIDs(one), houseIDs(two))
	}
}

func TestAHouseInTwoContainersCannotRollWithOneOfThemOnPostgres(t *testing.T) {
	f := newRollFixture(t)
	// SH-3 has packs in both containers (refreshed with both stuffings)
	f.w.plan("SH-3", "C-1", 1)
	f.stuffing("SH-3", "X-1", 1, "22G1", "CN-SEAL-1")
	f.w.mu.Lock()
	p := f.w.plans["SH-3"]
	second := p.Stuffings[0]
	second.ExecutionID, second.LoadUnitNo, second.Seal = "X-2", 2, "CN-SEAL-2"
	p.Stuffings = append(p.Stuffings, second)
	f.w.plans["SH-3"] = p
	f.w.mu.Unlock()
	if _, err := f.associate("assoc-SH-3", "OM-1", "SH-3", "", f.get(t, "OM-1").Revision); err != nil {
		t.Fatal(err)
	}
	f.w.booking("BK-2", "C-1", "EVER GLORY", "", "0130W", 1, f.hours(240))
	if _, err := f.open("OM-2", "C-1", "BK-2"); err != nil {
		t.Fatal(err)
	}
	_, err := f.roll(t, "roll-split", "CV-2", "OM-2", "rolled", 6)
	if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "SH-3") || !strings.Contains(err.Error(), "unload them from one of the containers first") {
		t.Fatalf("a house in two containers: %v", err)
	}
	if v := f.visit(t, "CV-2"); v.MovementID != "OM-1" || len(v.Rolls) != 0 {
		t.Fatalf("CV-2 moved: %+v", v)
	}
}

// The database's rules of a roll (sea/000014), each by a raw write on the application role.
func TestTheDatabaseKeepsTheRulesOfARollOnPostgres(t *testing.T) {
	f := newRollFixture(t)
	f.w.booking("BK-2", "C-1", "EVER GLORY", "", "0130W", 1, f.hours(240))
	if _, err := f.open("OM-2", "C-1", "BK-2"); err != nil {
		t.Fatal(err)
	}
	run := func(sql string) error {
		return f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range strings.Split(sql, ";\n") {
				if _, err := e.Exec(ctx, s, f.tenant); err != nil {
					return err
				}
			}
			return nil
		})
	}
	const roll = `INSERT INTO cargo_sea.container_visit_roll (tenant_id,visit_id,roll_no,from_movement_id,to_movement_id,reason,recorded_by,recorded_at,entered_at)
        VALUES ($1,'CV-2',%d,'%s','%s','rolled','x',now(),now())`
	const move = `UPDATE cargo_sea.container_visit SET movement_id='%s', revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-2'`
	for name, tc := range map[string]struct{ sql, state, check string }{
		"a movement changed without its roll": {fmt.Sprintf(move, "OM-2"), "P0001", ""},
		"a roll from a movement it is not in": {fmt.Sprintf(roll, 1, "OM-2", "OM-1"), "23514", "is its next and starts from the movement the visit is in"},
		"a roll that is not the next":         {fmt.Sprintf(roll, 2, "OM-1", "OM-2"), "23514", "is its next and starts from the movement the visit is in"},
		"a roll to the movement it is in":     {fmt.Sprintf(roll, 1, "OM-1", "OM-1"), "23514", "container_visit_roll_elsewhere"},
		"a roll to another consolidation's":   {fmt.Sprintf(roll, 1, "OM-1", "OM-NONE") + ";\n" + fmt.Sprintf(move, "OM-NONE"), "23503", ""},
	} {
		err := run(tc.sql)
		state := sqlState(err)
		if err == nil || (tc.state != "P0001" && state != tc.state) || (tc.check != "" && !strings.Contains(err.Error(), tc.check)) {
			t.Errorf("%s: %v (state %s)", name, err, state)
		}
	}
	// a link of another line than its movement's
	link := `INSERT INTO cargo_sea.booking_link (tenant_id,movement_id,link_no,booking_id,booking_revision,capacity_version,linked_by,linked_at,entered_at,capacity_line)
        VALUES ($1,'OM-2',2,'BK-2',5,2,'x',now(),now(),2)`
	if err := run(`UPDATE cargo_sea.ocean_movement SET revision=revision+1 WHERE tenant_id=$1 AND movement_id='OM-2';
` + link); sqlState(err) != "23514" || !strings.Contains(err.Error(), "which is not the movement's") {
		t.Errorf("a link of another line: %v", err)
	}
	// written right, with its roll, the movement changes
	if err := run(fmt.Sprintf(roll, 1, "OM-1", "OM-2") + ";\n" + fmt.Sprintf(move, "OM-2")); err != nil {
		t.Fatalf("a roll with its movement: %v", err)
	}
	if v := f.visit(t, "CV-2"); v.MovementID != "OM-2" || len(v.Rolls) != 1 {
		t.Fatalf("read back: %+v", v)
	}
	// a roll is never changed
	if err := run(`UPDATE cargo_sea.container_visit_roll SET reason='other' WHERE tenant_id=$1 AND visit_id='CV-2'`); err == nil {
		t.Fatal("a roll was changed")
	}
	_ = time.Second
}
