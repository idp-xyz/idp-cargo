//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	seamodule "go.idp.xyz/idp-cargo/internal/contexts/sea"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// containerNumber is an owner code and serial with the ISO 6346 check digit, worked out here apart from SEA's code
// and the database's: letters count from 10 leaving out the multiples of 11, weights double from 1.
func containerNumber(owner string, serial int) string {
	body := fmt.Sprintf("%sU%06d", owner, serial)
	sum := 0
	for i, c := range body {
		v := int(c - '0')
		if c >= 'A' {
			v = int(c-'A') + 10
			for _, skip := range []int{11, 22, 33} {
				if v >= skip {
					v++
				}
			}
		}
		sum += v * (1 << i)
	}
	return fmt.Sprintf("%s%d", body, sum%11%10)
}

func (f *fixture) assign(visit, movement string, unit int, number string, hours int) (api.Receipt, error) {
	return f.module.AssignContainer(context.Background(), api.AssignContainer{CommandID: "assign-" + visit, TenantID: f.tenant, ActorID: "ops-1", VisitID: visit,
		MovementID: movement, LoadUnitNo: unit, ContainerNumber: number, SizeType: "22G1", OccurredAt: f.hours(hours)})
}

func (f *fixture) seal(command, visit string, revision int64, number, party string, hours int) (api.Receipt, error) {
	return f.module.RecordContainerSeal(context.Background(), api.RecordContainerSeal{CommandID: command, TenantID: f.tenant, ActorID: "cfs-1", VisitID: visit,
		ExpectedRevision: revision, Number: number, Party: party, AppliedAt: f.hours(hours), Source: "tally sheet", OccurredAt: f.hours(hours)})
}

func (f *fixture) stuffingDone(command, visit string, revision int64, reason string, hours int) (api.Receipt, error) {
	return f.module.RecordStuffingDone(context.Background(), api.RecordStuffingDone{CommandID: command, TenantID: f.tenant, ActorID: "cfs-1", VisitID: visit,
		ExpectedRevision: revision, DifferenceReason: reason, OccurredAt: f.hours(hours)})
}

func (f *fixture) release(command, visit string, revision int64, reason string, hours int) (api.Receipt, error) {
	return f.module.ReleaseContainer(context.Background(), api.ReleaseContainer{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", VisitID: visit,
		ExpectedRevision: revision, Reason: reason, OccurredAt: f.hours(hours)})
}

func (f *fixture) cancel(command, movement string, revision int64, hours int) (api.Receipt, error) {
	return f.module.CancelOceanMovement(context.Background(), api.CancelOceanMovement{CommandID: command, TenantID: f.tenant, ActorID: "ops-1",
		MovementID: movement, ExpectedRevision: revision, Reason: "the consolidation was given up", OccurredAt: f.hours(hours)})
}

func (f *fixture) visit(t *testing.T, id string) api.ContainerVisit {
	t.Helper()
	v, err := f.module.GetContainerVisit(context.Background(), api.GetContainerVisit{TenantID: f.tenant, VisitID: id})
	if err != nil {
		t.Fatalf("visit %s: %v", id, err)
	}
	return v
}

// coLoad makes a booking a co-loader's.
func (f *fixture) coLoad(booking string) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	b := f.w.bookings[booking]
	b.Kind, b.CarrierPartyID, b.CarrierRole = bookapi.KindSeaLCLCoLoad, "coloader-x", "CO_LOADER"
	f.w.bookings[booking] = b
}

// stuffing makes a shipment's plan carry one sealed stuffing as FUL relays it from PHY.
func (f *fixture) stuffing(shipment, execution string, unit int, sizeType, seal string) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	p := f.w.plans[shipment]
	p.EvidenceVersion, p.MeasuredBasis, p.MeasuredPacks, p.MeasuredGrossWeight = 1, fulapi.MeasuredTendered, 6, kg(318)
	p.Stuffings = []fulapi.ContainerStuffing{{ExecutionID: execution, LoadUnitNo: unit, TypeCode: sizeType, Packs: 6, GrossWeight: kg(318), LineNos: []int{1},
		Seal: seal, SealedAt: f.at.Add(45 * time.Minute)}}
	f.w.plans[shipment] = p
}

type outboxEvent struct {
	Type    string
	Subject string
	Payload api.EventPayload
	Raw     string
}

func (f *fixture) outbox(t *testing.T) []outboxEvent {
	t.Helper()
	q, err := f.db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := q.Query(context.Background(), `SELECT event_type, subject, payload::text FROM bento.outbox WHERE scope=$1 AND source=$2 ORDER BY enqueued_seq`,
		f.tenant, api.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []outboxEvent
	for rows.Next() {
		var e outboxEvent
		if err = rows.Scan(&e.Type, &e.Subject, &e.Raw); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal([]byte(e.Raw), &e.Payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestContainerVisitsOnPostgres follows two containers of an own-container movement from their assignment to the
// record that they are stuffed, one of them where PHY's record differs, and a third that is given back unused.
func TestContainerVisitsOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	f.w.booking("BK-2", "C-2", "EVER GLORY", "", "0124W", 1, f.hours(96))
	f.w.booking("BK-CO", "C-CO", "EVER GLORY", "", "0124W", 1, f.hours(96))
	f.coLoad("BK-CO")
	for _, id := range []string{"1", "2", "CO"} {
		if _, err := f.open("OM-"+id, "C-"+id, "BK-"+id); err != nil {
			t.Fatal(err)
		}
	}
	f.series(t)
	f.w.plan("SH-1", "C-1", 1)
	if _, err := f.associate("assoc-1", "OM-1", "SH-1", "", 1); err != nil {
		t.Fatal(err)
	}

	// Assignment.
	first := api.AssignContainer{CommandID: "assign-CV-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1", MovementID: "OM-1", LoadUnitNo: 1,
		ContainerNumber: " csqu3054383 ", SizeType: "22g1", TareKg: 2200, MaxGrossKg: 30480, PlateSource: api.PlateCSC, OccurredAt: f.hours(1)}
	refused := func(change func(*api.AssignContainer)) error {
		cmd := first
		cmd.CommandID, cmd.VisitID = "assign-refused", "CV-X"
		change(&cmd)
		_, err := f.module.AssignContainer(ctx, cmd)
		return err
	}
	for name, tc := range map[string]struct {
		change func(*api.AssignContainer)
		want   error
	}{
		"a movement SEA does not know":        {func(c *api.AssignContainer) { c.MovementID = "OM-NONE" }, api.ErrNotFound},
		"an actor without the right":          {func(c *api.AssignContainer) { c.ActorID = "intruder" }, api.ErrUnauthorized},
		"another tenant":                      {func(c *api.AssignContainer) { c.TenantID = f.tenant + "-other" }, api.ErrNotFound},
		"a wrong check digit":                 {func(c *api.AssignContainer) { c.ContainerNumber = "CSQU3054384" }, api.ErrRejected},
		"a co-loader's container":             {func(c *api.AssignContainer) { c.MovementID = "OM-CO" }, api.ErrRejected},
		"unit 0":                              {func(c *api.AssignContainer) { c.LoadUnitNo = 0 }, api.ErrRejected},
		"a tare without its source":           {func(c *api.AssignContainer) { c.PlateSource = "" }, api.ErrRejected},
		"before the movement was opened":      {func(c *api.AssignContainer) { c.OccurredAt = f.at.Add(-time.Minute) }, api.ErrRejected},
		"a business time a month ago":         {func(c *api.AssignContainer) { c.OccurredAt = time.Now().Add(-30 * 24 * time.Hour) }, api.ErrRejected},
		"a business time after SEA's clock":   {func(c *api.AssignContainer) { c.OccurredAt = time.Now().Add(time.Hour) }, api.ErrRejected},
		"no business time":                    {func(c *api.AssignContainer) { c.OccurredAt = time.Time{} }, api.ErrUnauthenticated},
		"no visit id":                         {func(c *api.AssignContainer) { c.VisitID = "" }, api.ErrRejected},
		"a size-type that is no ISO 6346 one": {func(c *api.AssignContainer) { c.SizeType = "20 FT" }, api.ErrRejected},
	} {
		if err := refused(tc.change); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	r, err := f.module.AssignContainer(ctx, first)
	if err != nil || r != (api.Receipt{CommandID: "assign-CV-1", VisitID: "CV-1", NewRevision: 1}) {
		t.Fatalf("assigned: %+v %v", r, err)
	}
	if again, err := f.module.AssignContainer(ctx, first); err != nil || again != r {
		t.Fatalf("replayed: %+v %v", again, err)
	}
	other := first
	other.LoadUnitNo = 2
	if _, err = f.module.AssignContainer(ctx, other); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the command id with another unit: %v", err)
	}
	if _, err = f.assign("CV-1B", "OM-2", 1, "CSQU3054383", 1); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "live visit") {
		t.Fatalf("the container on another movement: %v", err)
	}
	if _, err = f.assign("CV-1C", "OM-1", 1, "TEMU1234565", 1); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "load unit") {
		t.Fatalf("another container on the unit: %v", err)
	}
	if _, err = f.module.AssignContainer(ctx, api.AssignContainer{CommandID: "assign-id-twice", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		MovementID: "OM-1", LoadUnitNo: 7, ContainerNumber: "HLXU1234561", SizeType: "22G1", OccurredAt: f.hours(1)}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a visit id twice: %v", err)
	}
	v := f.visit(t, "CV-1")
	want := api.ContainerVisit{TenantID: f.tenant, VisitID: "CV-1", Revision: 1, MovementID: "OM-1", ConsolidationID: "C-1", LoadUnitNo: 1,
		ContainerNumber: "CSQU3054383", SizeType: "22G1", TareKg: 2200, MaxGrossKg: 30480, PlateSource: api.PlateCSC, Status: api.VisitAssigned,
		AssignedBy: "ops-1", AssignedAt: f.hours(1)}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("the visit: %+v", v)
	}
	if m := f.get(t, "OM-1"); m.Revision != 2 || len(m.Containers) != 1 || !reflect.DeepEqual(m.Containers[0], want) || m.Houses[0].Containers != nil {
		t.Fatalf("the movement keeps its revision and lists the container: %+v", m)
	}

	// Seals.
	if _, err = f.seal("seal-stale", "CV-1", 4, "CN-SEAL-1", api.SealShipper, 2); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a stale revision: %v", err)
	}
	if _, err = f.seal("seal-unknown", "CV-NONE", 1, "CN-SEAL-1", api.SealShipper, 2); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a visit SEA does not know: %v", err)
	}
	for name, cmd := range map[string]api.RecordContainerSeal{
		"a party in small letters":  {Number: "CN-SEAL-1", Party: "shipper", AppliedAt: f.hours(2), Source: "tally sheet", OccurredAt: f.hours(2)},
		"other signs":               {Number: "CN_SEAL_1", Party: api.SealShipper, AppliedAt: f.hours(2), Source: "tally sheet", OccurredAt: f.hours(2)},
		"applied after it is dated": {Number: "CN-SEAL-1", Party: api.SealShipper, AppliedAt: f.hours(3), Source: "tally sheet", OccurredAt: f.hours(2)},
		"no source":                 {Number: "CN-SEAL-1", Party: api.SealShipper, AppliedAt: f.hours(2), OccurredAt: f.hours(2)},
		"before the assignment":     {Number: "CN-SEAL-1", Party: api.SealShipper, AppliedAt: f.at, Source: "tally sheet", OccurredAt: f.at},
	} {
		cmd.CommandID, cmd.TenantID, cmd.ActorID, cmd.VisitID, cmd.ExpectedRevision = "seal-refused", f.tenant, "cfs-1", "CV-1", 1
		if _, err = f.module.RecordContainerSeal(ctx, cmd); !errors.Is(err, api.ErrRejected) {
			t.Errorf("a seal with %s: %v", name, err)
		}
	}
	sealed, err := f.seal("seal-1", "CV-1", 1, " cn-seal-1 ", api.SealShipper, 2)
	if err != nil || sealed != (api.Receipt{CommandID: "seal-1", VisitID: "CV-1", NewRevision: 2, SealNo: 1}) {
		t.Fatalf("sealed: %+v %v", sealed, err)
	}
	if again, err := f.seal("seal-1", "CV-1", 1, " cn-seal-1 ", api.SealShipper, 2); err != nil || again != sealed {
		t.Fatalf("the seal replayed: %+v %v", again, err)
	}
	if _, err = f.seal("seal-1-again", "CV-1", 2, "CN-SEAL-1", api.SealCarrier, 2); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("the current seal again: %v", err)
	}

	// Stuffing done: the house must carry PHY's sealed stuffing of the unit first.
	if _, err = f.stuffingDone("done-early", "CV-1", 2, "", 3); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "refresh the houses") {
		t.Fatalf("before FUL relays the stuffing: %v", err)
	}
	f.stuffing("SH-1", "X-1", 1, "22G1", "cn-seal-1") // PHY keeps the seal as it was keyed
	if _, err = f.module.RefreshHouseBillFacts(ctx, api.RefreshHouseBillFacts{CommandID: "refresh-1", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 2, ShipmentID: "SH-1", OccurredAt: f.hours(3)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.stuffingDone("done-reason", "CV-1", 2, "to be safe", 3); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "only when") {
		t.Fatalf("a reason without a difference: %v", err)
	}
	if _, err = f.module.RecordStuffingDone(ctx, api.RecordStuffingDone{CommandID: "done-before-seal", TenantID: f.tenant, ActorID: "cfs-1", VisitID: "CV-1",
		ExpectedRevision: 2, OccurredAt: f.at.Add(40 * time.Minute)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("stuffing done before PHY sealed: %v", err)
	}
	done, err := f.stuffingDone("done-1", "CV-1", 2, "", 3)
	if err != nil || done != (api.Receipt{CommandID: "done-1", VisitID: "CV-1", NewRevision: 3}) {
		t.Fatalf("stuffing done: %+v %v", done, err)
	}
	if again, err := f.stuffingDone("done-1", "CV-1", 2, "", 3); err != nil || again != done {
		t.Fatalf("stuffing done replayed: %+v %v", again, err)
	}
	// one command id is one command: a release with a stuffing's id and the same fields is no replay
	if _, err = f.module.ReleaseContainer(ctx, api.ReleaseContainer{CommandID: "done-1", TenantID: f.tenant, ActorID: "cfs-1", VisitID: "CV-1",
		ExpectedRevision: 2, OccurredAt: f.hours(3)}); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("a release under a stuffing's command id: %v", err)
	}
	if _, err = f.release("release-stuffed", "CV-1", 3, "sent back", 4); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "holds cargo") {
		t.Fatalf("a stuffed container released: %v", err)
	}
	// customs open the container and seal it again
	if r, err = f.seal("seal-customs", "CV-1", 3, "CUS-778", api.SealCustoms, 5); err != nil || r.SealNo != 2 || r.NewRevision != 4 {
		t.Fatalf("the customs seal: %+v %v", r, err)
	}
	v = f.visit(t, "CV-1")
	want.Revision, want.Status, want.ExecutionID, want.StuffedAt, want.SealNumber, want.SealParty = 4, api.VisitStuffed, "X-1", f.hours(3), "CUS-778", api.SealCustoms
	want.Seals = []api.ContainerSeal{
		{SealNo: 2, Number: "CUS-778", Party: api.SealCustoms, AppliedAt: f.hours(5), Source: "tally sheet", RecordedBy: "cfs-1", RecordedAt: f.hours(5)},
		{SealNo: 1, Number: "CN-SEAL-1", Party: api.SealShipper, AppliedAt: f.hours(2), Source: "tally sheet", RecordedBy: "cfs-1", RecordedAt: f.hours(2)}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("stuffed and sealed again: %+v", v)
	}
	m := f.get(t, "OM-1")
	if m.Revision != 3 || len(m.Containers) != 1 || !reflect.DeepEqual(m.Containers[0], want) || len(m.Houses) != 1 || len(m.Houses[0].Containers) != 1 ||
		m.Houses[0].Containers[0].ContainerNumber != "CSQU3054383" || m.Houses[0].Containers[0].Seal != "cn-seal-1" {
		t.Fatalf("the house names its container: %+v", m)
	}
	if _, err = f.cancel("cancel-held", "OM-1", 3, 6); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a movement with a house and a container cancelled: %v", err)
	}

	// A second container, where PHY's record differs: the seal as a sentence, and the size-type PHY planned.
	f.w.plan("SH-2", "C-1", 1)
	f.stuffing("SH-2", "X-2", 2, "42G1", "sealed by Zhang at gate 3")
	if _, err = f.associateAt("assoc-2", "OM-1", "SH-2", "", 3, 6); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.AssignContainer(ctx, api.AssignContainer{CommandID: "assign-CV-2", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-2",
		MovementID: "OM-1", LoadUnitNo: 2, ContainerNumber: "TEMU1234565", SizeType: "42G0", OccurredAt: f.hours(4)}); err != nil {
		t.Fatalf("a container dated before the movement's last change: %v", err)
	}
	if _, err = f.seal("seal-2", "CV-2", 1, "ZX 4471", api.SealCarrier, 5); err != nil {
		t.Fatal(err)
	}
	if _, err = f.stuffingDone("done-2-bare", "CV-2", 2, "", 6); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), `seal "sealed by Zhang at gate 3", which is none of the container's seals; unit 2 is a 42G1 in PHY, the container a 42G0`) {
		t.Fatalf("PHY's record differs: %v", err)
	}
	if _, err = f.stuffingDone("done-2", "CV-2", 2, "the tally sheet names seal ZX 4471; the line released a 42G0", 6); err != nil {
		t.Fatal(err)
	}
	if v = f.visit(t, "CV-2"); v.Status != api.VisitStuffed || v.ExecutionID != "X-2" || v.TareKg != 0 || v.PlateSource != "" ||
		v.DifferenceReason != "the tally sheet names seal ZX 4471; the line released a 42G0" || v.Revision != 3 {
		t.Fatalf("stuffed with its reason: %+v", v)
	}
	m = f.get(t, "OM-1")
	if len(m.Containers) != 2 || m.Containers[0].VisitID != "CV-1" || m.Containers[1].VisitID != "CV-2" || len(m.Houses) != 2 ||
		m.Houses[1].Containers[0].ContainerNumber != "TEMU1234565" || m.Houses[0].Containers[0].ContainerNumber != "CSQU3054383" {
		t.Fatalf("two containers: %+v", m)
	}

	// A container given back unused, and its movement cancelled after that.
	if _, err = f.assign("CV-3", "OM-2", 1, "MSKU1234565", 1); err != nil {
		t.Fatal(err)
	}
	if _, err = f.cancel("cancel-2-held", "OM-2", 1, 2); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "containers assigned") {
		t.Fatalf("a movement with a container cancelled: %v", err)
	}
	if _, err = f.release("release-bare", "CV-3", 1, " ", 2); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a release without a reason: %v", err)
	}
	released, err := f.release("release-3", "CV-3", 1, "the booking was cut to one container", 3)
	if err != nil || released != (api.Receipt{CommandID: "release-3", VisitID: "CV-3", NewRevision: 2}) {
		t.Fatalf("released: %+v %v", released, err)
	}
	if again, err := f.release("release-3", "CV-3", 1, "the booking was cut to one container", 3); err != nil || again != released {
		t.Fatalf("the release replayed: %+v %v", again, err)
	}
	if _, err = f.seal("seal-released", "CV-3", 2, "S-9", api.SealShipper, 4); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a seal on a released container: %v", err)
	}
	if _, err = f.cancel("cancel-2-early", "OM-2", 1, 2); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "last container was released") {
		t.Fatalf("a cancellation dated before the release: %v", err)
	}
	if r, err = f.cancel("cancel-2", "OM-2", 1, 3); err != nil || r.MovementID != "OM-2" || r.VisitID != "" || r.NewRevision != 2 {
		t.Fatalf("cancelled once its container is released: %+v %v", r, err)
	}
	if _, err = f.assign("CV-3B", "OM-2", 1, "HLXU1234561", 4); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a container for a cancelled movement: %v", err)
	}
	// the released number is free for another visit; the released visit keeps its history
	if _, err = f.assign("CV-4", "OM-1", 3, "MSKU1234565", 7); err != nil {
		t.Fatalf("the released container assigned again: %v", err)
	}
	list, err := f.module.ListContainerVisits(ctx, api.ListContainerVisits{TenantID: f.tenant, MovementID: "OM-2"})
	if err != nil || len(list) != 1 || list[0].Status != api.VisitReleased || list[0].ReleaseReason != "the booking was cut to one container" ||
		!list[0].ReleasedAt.Equal(f.hours(3)) || list[0].ContainerNumber != "MSKU1234565" {
		t.Fatalf("the cancelled movement's visits: %+v %v", list, err)
	}
	if m = f.get(t, "OM-2"); m.State != api.StateCancelled || len(m.Containers) != 0 {
		t.Fatalf("a released container is not the movement's: %+v", m)
	}
	if list, err = f.module.ListContainerVisits(ctx, api.ListContainerVisits{TenantID: f.tenant, MovementID: "OM-1"}); err != nil || len(list) != 3 ||
		list[0].VisitID != "CV-1" || list[1].VisitID != "CV-2" || list[2].VisitID != "CV-4" {
		t.Fatalf("in the order they were assigned: %+v %v", list, err)
	}
	if list, err = f.module.ListContainerVisits(ctx, api.ListContainerVisits{TenantID: f.tenant, MovementID: "OM-NONE"}); err != nil || len(list) != 0 {
		t.Fatalf("a movement SEA does not know: %+v %v", list, err)
	}

	// Queries.
	if live, err := f.module.FindLiveContainerVisit(ctx, api.FindLiveContainerVisit{TenantID: f.tenant, ContainerNumber: " msku1234565 "}); err != nil ||
		live.VisitID != "CV-4" {
		t.Fatalf("the live visit of a number: %+v %v", live, err)
	}
	if _, err = f.module.FindLiveContainerVisit(ctx, api.FindLiveContainerVisit{TenantID: f.tenant, ContainerNumber: "HLXU1234561"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a number without a visit: %v", err)
	}
	if _, err = f.module.FindLiveContainerVisit(ctx, api.FindLiveContainerVisit{TenantID: f.tenant, ContainerNumber: "MSKU1234566"}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a number with a wrong check digit: %v", err)
	}
	if _, err = f.module.GetContainerVisit(ctx, api.GetContainerVisit{TenantID: f.tenant + "-other", VisitID: "CV-1"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("another tenant's visit: %v", err)
	}
	if list, err = f.module.ListContainerVisits(ctx, api.ListContainerVisits{TenantID: f.tenant + "-other", MovementID: "OM-1"}); err != nil || len(list) != 0 {
		t.Fatalf("another tenant's movement: %+v %v", list, err)
	}

	// Events: identifiers only, the visit as subject, its movement named.
	var got []string
	for _, e := range f.outbox(t) {
		if !strings.HasPrefix(e.Subject, "CV-") {
			continue
		}
		got = append(got, fmt.Sprintf("%s %s seal %d %s", e.Type, e.Subject, e.Payload.SealNo, e.Payload.Reason))
		movement, consolidation := "OM-1", "C-1"
		if e.Subject == "CV-3" {
			movement, consolidation = "OM-2", "C-2"
		}
		if e.Payload != (api.EventPayload{TenantID: f.tenant, SubjectID: e.Subject, ConsolidationID: consolidation, MovementID: movement,
			SealNo: e.Payload.SealNo, Reason: e.Payload.Reason}) {
			t.Errorf("payload of %s: %s", e.Type, e.Raw)
		}
		for _, secret := range []string{"CSQU", "TEMU", "MSKU", "SEAL", "CUS-778", "ZX 4471", "tally", "booking was cut", "Zhang"} {
			if strings.Contains(e.Raw, secret) {
				t.Errorf("%s carries %q: %s", e.Type, secret, e.Raw)
			}
		}
	}
	wantEvents := []string{
		"ContainerAssigned.v1 CV-1 seal 0 ", "SealRecorded.v1 CV-1 seal 1 ", "ContainerEventRecorded.v1 CV-1 seal 0 STUFFED", "SealRecorded.v1 CV-1 seal 2 ",
		"ContainerAssigned.v1 CV-2 seal 0 ", "SealRecorded.v1 CV-2 seal 1 ", "ContainerEventRecorded.v1 CV-2 seal 0 STUFFED",
		"ContainerAssigned.v1 CV-3 seal 0 ", "ContainerReleased.v1 CV-3 seal 0 ", "ContainerAssigned.v1 CV-4 seal 0 "}
	if !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("the visits' events:\n%s", strings.Join(got, "\n"))
	}

	// Read on a session in another time zone: the same visits.
	cfg, err := pgxpool.ParseConfig(os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "Asia/Shanghai"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	shanghaiDB, err := bentopg.NewDB(pool)
	if err != nil {
		t.Fatal(err)
	}
	shanghai, err := seamodule.New(shanghaiDB, f.w, f.w, allow{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"CV-1", "CV-2", "CV-3"} {
		there, err := shanghai.GetContainerVisit(ctx, api.GetContainerVisit{TenantID: f.tenant, VisitID: id})
		if err != nil || !reflect.DeepEqual(there, f.visit(t, id)) {
			t.Fatalf("%s read in Asia/Shanghai: %+v %v", id, there, err)
		}
	}
}

// TestContainerVisitDatabaseRules writes past SEA. OM-1 is open with house SH-1 in PHY's stuffing X-1 of unit 1:
// CV-1 is stuffed there, CV-2 is assigned to unit 2 with a seal, CV-4 to unit 3 without one. OM-2 is cancelled, its
// CV-3 released. OM-CO is a co-load.
func TestContainerVisitDatabaseRules(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	f.w.booking("BK-2", "C-2", "EVER GLORY", "", "0124W", 1, f.hours(96))
	f.w.booking("BK-CO", "C-CO", "EVER GLORY", "", "0124W", 1, f.hours(96))
	f.coLoad("BK-CO")
	for _, id := range []string{"1", "2", "CO"} {
		if _, err := f.open("OM-"+id, "C-"+id, "BK-"+id); err != nil {
			t.Fatal(err)
		}
	}
	f.series(t)
	f.w.plan("SH-1", "C-1", 1)
	f.stuffing("SH-1", "X-1", 1, "22G1", "CN-SEAL-1")
	steps := []func() (api.Receipt, error){
		func() (api.Receipt, error) { return f.associate("assoc-1", "OM-1", "SH-1", "", 1) },
		func() (api.Receipt, error) { return f.assign("CV-1", "OM-1", 1, "CSQU3054383", 1) },
		func() (api.Receipt, error) { return f.seal("seal-1", "CV-1", 1, "CN-SEAL-1", api.SealShipper, 2) },
		func() (api.Receipt, error) { return f.stuffingDone("done-1", "CV-1", 2, "", 3) },
		func() (api.Receipt, error) { return f.assign("CV-2", "OM-1", 2, "TEMU1234565", 1) },
		func() (api.Receipt, error) { return f.seal("seal-2", "CV-2", 1, "S-2", api.SealCarrier, 3) },
		func() (api.Receipt, error) { return f.assign("CV-4", "OM-1", 3, "HLXU1234561", 1) },
		func() (api.Receipt, error) { return f.assign("CV-3", "OM-2", 1, "MSKU1234565", 1) },
		func() (api.Receipt, error) { return f.release("release-3", "CV-3", 1, "not used", 2) },
		func() (api.Receipt, error) { return f.cancel("cancel-2", "OM-2", 1, 2) },
	}
	for i, step := range steps {
		if _, err := step(); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
	}

	// What never changes: refused to the application role for want of the privilege (42501), and to the migrator, who
	// holds every privilege, by the triggers (23001).
	migrator := f.migrator(t)
	for name, sql := range map[string]string{
		"a seal changed":               `UPDATE cargo_sea.container_seal SET number='OTHER' WHERE tenant_id=$1`,
		"a seal deleted":               `DELETE FROM cargo_sea.container_seal WHERE tenant_id=$1`,
		"a visit deleted":              `DELETE FROM cargo_sea.container_visit WHERE tenant_id=$1 AND visit_id='CV-4'`,
		"the container number changed": `UPDATE cargo_sea.container_visit SET container_number='MSKU0000080' WHERE tenant_id=$1 AND visit_id='CV-4'`,
		"the load unit changed":        `UPDATE cargo_sea.container_visit SET load_unit_no=9 WHERE tenant_id=$1 AND visit_id='CV-4'`,
		"the plate data changed":       `UPDATE cargo_sea.container_visit SET tare_kg=1,max_gross_kg=2,plate_source='CARRIER' WHERE tenant_id=$1 AND visit_id='CV-4'`,
		"the writing transaction":      `UPDATE cargo_sea.container_visit SET written_xact=pg_current_xact_id() WHERE tenant_id=$1 AND visit_id='CV-4'`,
	} {
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

	// What the application role may write, and the rule that refuses each write: at once or at commit.
	visit := `INSERT INTO cargo_sea.container_visit (tenant_id,visit_id,movement_id,consolidation_id,load_unit_no,container_number,size_type,status,
        assigned_by,assigned_at,entered_at%s) VALUES ($1,'CV-9','%s','%s',%d,'%s','22G1','%s','x',now(),now()%s)`
	// a seal written with a step of its visit's revision; %s: visit, seal number, number, party, applied and recorded
	sealStep := `WITH v AS (UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='%[1]s' RETURNING visit_id)
        INSERT INTO cargo_sea.container_seal (tenant_id,visit_id,seal_no,number,party,applied_at,source,recorded_by,recorded_at,entered_at)
        SELECT $1,visit_id,%[2]d,'%[3]s','%[4]s',%[5]s,'x','x',%[6]s,now() FROM v`
	stuffed := `UPDATE cargo_sea.container_visit SET status='STUFFED',execution_id='%s',stuffed_by='x',stuffed_at=now(),revision=revision+1
        WHERE tenant_id=$1 AND visit_id='%s'`
	receipt := `INSERT INTO cargo_sea.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision,result_seal_no)
        VALUES ($1,'past',repeat('a',64),'%s','x',1,%s)`
	for name, tc := range map[string]struct {
		sql   string
		state string
		in    string // the constraint, or part of the message
	}{
		"a wrong check digit":           {fmt.Sprintf(visit, "", "OM-1", "C-1", 9, "CSQU3054384", "ASSIGNED", ""), "23514", "container_visit_number"},
		"a number in small letters":     {fmt.Sprintf(visit, "", "OM-1", "C-1", 9, "msku0000080", "ASSIGNED", ""), "23514", "container_visit_number"},
		"a tare without its source":     {fmt.Sprintf(visit, ",tare_kg,max_gross_kg", "OM-1", "C-1", 9, "MSKU0000080", "ASSIGNED", ",2200,30480"), "23514", "container_visit_plate"},
		"a tare over the maximum gross": {fmt.Sprintf(visit, ",tare_kg,max_gross_kg,plate_source", "OM-1", "C-1", 9, "MSKU0000080", "ASSIGNED", ",30480,2200,'CSC_PLATE'"), "23514", "container_visit_plate"},
		"a visit that starts stuffed": {fmt.Sprintf(visit, ",execution_id,stuffed_by,stuffed_at", "OM-1", "C-1", 9, "MSKU0000080", "STUFFED", ",'X-1','x',now()"), "23001",
			"starts ASSIGNED"},
		"a visit that starts at revision 2":     {fmt.Sprintf(visit, ",revision", "OM-1", "C-1", 9, "MSKU0000080", "ASSIGNED", ",2"), "23001", "starts ASSIGNED"},
		"a second live visit of a container":    {fmt.Sprintf(visit, "", "OM-1", "C-1", 9, "CSQU3054383", "ASSIGNED", ""), "23505", "container_visit_live_number"},
		"a second container on a unit":          {fmt.Sprintf(visit, "", "OM-1", "C-1", 1, "MSKU0000080", "ASSIGNED", ""), "23505", "container_visit_live_unit"},
		"a container on a co-load":              {fmt.Sprintf(visit, "", "OM-CO", "C-CO", 1, "MSKU0000080", "ASSIGNED", ""), "23514", "own-container movement"},
		"a container for another consolidation": {fmt.Sprintf(visit, "", "OM-1", "C-9", 9, "MSKU0000080", "ASSIGNED", ""), "23514", "own-container movement"},
		"a container on a cancelled movement":   {fmt.Sprintf(visit, "", "OM-2", "C-2", 2, "MSKU0000080", "ASSIGNED", ""), "23514", "not open holds no container"},
		"a container on no movement":            {fmt.Sprintf(visit, "", "OM-NONE", "C-1", 1, "MSKU0000080", "ASSIGNED", ""), "23503", "container_visit_tenant_id_movement_id_fkey"},
		"stuffed where no house is":             {fmt.Sprintf(stuffed, "X-1", "CV-2"), "23514", "carries a sealed stuffing of its load unit"},
		"stuffed with a stuffing no house has":  {fmt.Sprintf(stuffed, "X-7", "CV-2"), "23514", "carries a sealed stuffing of its load unit"},
		"stuffed without a seal":                {fmt.Sprintf(stuffed, "X-1", "CV-4"), "23514", "a stuffed container has a seal"},
		"stuffed without its stuffing": {`UPDATE cargo_sea.container_visit SET status='STUFFED',stuffed_by='x',stuffed_at=now(),revision=revision+1
            WHERE tenant_id=$1 AND visit_id='CV-2'`, "23514", "container_visit_stuffing"},
		"a difference on an unused container": {`UPDATE cargo_sea.container_visit SET difference_reason='x',revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-2'`,
			"23001", "leaves ASSIGNED once"},
		"a stuffed container assigned again": {`UPDATE cargo_sea.container_visit SET status='ASSIGNED',execution_id=NULL,stuffed_by=NULL,stuffed_at=NULL,
            revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1'`, "23001", "leaves ASSIGNED once"},
		"a stuffed container released": {`UPDATE cargo_sea.container_visit SET status='RELEASED',release_reason='x',released_by='x',released_at=now(),
            execution_id=NULL,stuffed_by=NULL,stuffed_at=NULL,revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1'`, "23001", "leaves ASSIGNED once"},
		"a stuffing kept by another": {`UPDATE cargo_sea.container_visit SET execution_id='X-7',revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1'`,
			"23001", "leaves ASSIGNED once"},
		"a released container assigned again": {`UPDATE cargo_sea.container_visit SET status='ASSIGNED',release_reason=NULL,released_by=NULL,released_at=NULL,
            revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-3'`, "23001", "leaves ASSIGNED once"},
		"a released container stepped": {`UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-3'`, "23001",
			"never once released"},
		"a revision skipped": {`UPDATE cargo_sea.container_visit SET revision=revision+2 WHERE tenant_id=$1 AND visit_id='CV-2'`, "23001", "by one"},
		"a revision kept":    {`UPDATE cargo_sea.container_visit SET updated_at=now() WHERE tenant_id=$1 AND visit_id='CV-2'`, "23001", "by one"},
		"a release that names a stuffing": {`UPDATE cargo_sea.container_visit SET status='RELEASED',release_reason='x',released_by='x',released_at=now(),
            execution_id='X-1',revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-2'`, "23001", "leaves ASSIGNED once"},
		"released before it was assigned": {`UPDATE cargo_sea.container_visit SET status='RELEASED',release_reason='x',released_by='x',
            released_at=assigned_at - interval '1 hour',revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-2'`, "23514", "container_visit_release"},
		"released without a reason": {`UPDATE cargo_sea.container_visit SET status='RELEASED',released_by='x',released_at=now(),revision=revision+1
            WHERE tenant_id=$1 AND visit_id='CV-2'`, "23514", "container_visit_release"},
		"a seal without a step of its visit": {`INSERT INTO cargo_sea.container_seal (tenant_id,visit_id,seal_no,number,party,applied_at,source,recorded_by,
            recorded_at,entered_at) VALUES ($1,'CV-2',2,'S-NEW','SHIPPER',now(),'x','x',now(),now())`, "23514", "a step of its visit's revision"},
		"a seal on a released container": {`INSERT INTO cargo_sea.container_seal (tenant_id,visit_id,seal_no,number,party,applied_at,source,recorded_by,
            recorded_at,entered_at) VALUES ($1,'CV-3',1,'S-NEW','SHIPPER',now(),'x','x',now(),now())`, "23514", "a released container takes no seal"},
		"a seal out of number":      {fmt.Sprintf(sealStep, "CV-2", 5, "S-NEW", "SHIPPER", "now()", "now()"), "23514", "numbered without gaps"},
		"the current seal again":    {fmt.Sprintf(sealStep, "CV-2", 2, "S-2", "SHIPPER", "now()", "now()"), "23514", "a further seal has another number"},
		"a seal number taken twice": {fmt.Sprintf(sealStep, "CV-2", 1, "S-NEW", "SHIPPER", "now()", "now()"), "23505", "container_seal_pkey"},
		"a seal recorded before the one it replaces": {fmt.Sprintf(sealStep, "CV-2", 2, "S-NEW", "SHIPPER", "now() - interval '22 hours 30 minutes'",
			"now() - interval '22 hours 30 minutes'"), "23514", "recorded not before the one it replaces"},
		"a seal recorded before the assignment": {fmt.Sprintf(sealStep, "CV-4", 1, "S-NEW", "SHIPPER", "now() - interval '30 hours'", "now() - interval '30 hours'"),
			"23514", "not before the container was assigned"},
		"a seal applied after it is recorded": {fmt.Sprintf(sealStep, "CV-2", 2, "S-NEW", "SHIPPER", "now()", "now() - interval '1 minute'"), "23514",
			"container_seal_applied"},
		"the forwarder's seal":           {fmt.Sprintf(sealStep, "CV-2", 2, "S-NEW", "FORWARDER", "now()", "now()"), "23514", "container_seal_party_check"},
		"a seal number in small letters": {fmt.Sprintf(sealStep, "CV-2", 2, "s-new", "SHIPPER", "now()", "now()"), "23514", "container_seal_number_check"},
		"a seal for no visit": {`INSERT INTO cargo_sea.container_seal (tenant_id,visit_id,seal_no,number,party,applied_at,source,recorded_by,recorded_at,
            entered_at) VALUES ($1,'CV-NONE',1,'S-NEW','SHIPPER',now(),'x','x',now(),now())`, "23503", "container_seal_tenant_id_visit_id_fkey"},
		"cancelled with its containers": {`UPDATE cargo_sea.ocean_movement SET state='CANCELLED',cancel_reason='x',cancelled_by='x',cancelled_at=now(),
            revision=revision+1 WHERE tenant_id=$1 AND movement_id='OM-1'`, "23514", "ocean_movement_holds_no_container"},
		"a receipt of a movement naming a seal": {fmt.Sprintf(receipt, "OceanMovement", "1"), "23514", "command_receipt_seal_of_visit"},
		"a receipt of an unknown kind":          {fmt.Sprintf(receipt, "VgmAssertion", "NULL"), "23514", "command_receipt_result_type_check"},
	} {
		err := f.past(t, tc.sql)
		if state := sqlState(err); state != tc.state || !strings.Contains(refusal(err), tc.in) {
			t.Errorf("%s: %v (state %s, %s), want %s %s", name, err, state, refusal(err), tc.state, tc.in)
		}
		t.Logf("%s refused: %v", name, err)
	}

	// nothing above changed a visit, and no visit or seal was added
	for id, want := range map[string]struct {
		status   string
		revision int64
		seals    int
	}{"CV-1": {api.VisitStuffed, 3, 1}, "CV-2": {api.VisitAssigned, 2, 1}, "CV-3": {api.VisitReleased, 2, 0}, "CV-4": {api.VisitAssigned, 1, 0}} {
		if v := f.visit(t, id); v.Status != want.status || v.Revision != want.revision || len(v.Seals) != want.seals {
			t.Errorf("%s after the refused writes: %+v", id, v)
		}
	}
	if _, err := f.module.GetContainerVisit(ctx, api.GetContainerVisit{TenantID: f.tenant, VisitID: "CV-9"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a visit written past SEA: %v", err)
	}
	if m := f.get(t, "OM-1"); m.State != api.StateOpen || m.Revision != 2 || len(m.Containers) != 3 {
		t.Fatalf("the movement after the refused writes: %+v", m)
	}
	// another tenant sees and changes nothing of it
	other := scope.TenantTransactor{DB: f.db, Tenant: f.tenant + "-other"}
	if err := other.WithinTransaction(ctx, func(txCtx context.Context) error {
		e, err := f.db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		var visits, seals int
		if err = e.QueryRow(txCtx, `SELECT (SELECT count(*) FROM cargo_sea.container_visit), (SELECT count(*) FROM cargo_sea.container_seal)`).Scan(&visits, &seals); err != nil {
			return err
		}
		if visits != 0 || seals != 0 {
			return fmt.Errorf("another tenant reads %d visits and %d seals", visits, seals)
		}
		tag, err := e.Exec(txCtx, `UPDATE cargo_sea.container_visit SET revision=revision+1`)
		if err == nil && tag.RowsAffected() != 0 {
			err = fmt.Errorf("another tenant changed %d visits", tag.RowsAffected())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestVisitsRaceForOneContainer assigns one container to eight movements at once: one visit holds it.
func TestVisitsRaceForOneContainer(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	ids := []string{"A", "B", "C", "D", "E", "F", "G", "H"}
	for _, id := range ids {
		f.w.booking("BK-"+id, "C-"+id, "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
		if _, err := f.open("OM-"+id, "C-"+id, "BK-"+id); err != nil {
			t.Fatal(err)
		}
	}
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.assign("CV-"+id, "OM-"+id, 1, "CSQU3054383", 1)
		}()
	}
	wg.Wait()
	won := ""
	for i, err := range errs {
		switch {
		case err == nil && won == "":
			won = "CV-" + ids[i]
		case err == nil:
			t.Fatalf("%s and CV-%s both hold the container", won, ids[i])
		case !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "live visit"):
			t.Fatalf("a losing assignment: %v", err)
		}
	}
	if live, err := f.module.FindLiveContainerVisit(ctx, api.FindLiveContainerVisit{TenantID: f.tenant, ContainerNumber: "CSQU3054383"}); err != nil ||
		live.VisitID != won {
		t.Fatalf("the visit that holds the container: %+v %v, want %s", live, err, won)
	}
}

// TestVisitsRaceForOneLoadUnit assigns eight containers to one load unit at once: one is taken.
func TestVisitsRaceForOneLoadUnit(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.assign(fmt.Sprintf("CV-%d", i), "OM-1", 1, containerNumber("RAC", 100+i), 1)
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "load unit"):
			t.Fatalf("a losing assignment: %v", err)
		}
	}
	if m := f.get(t, "OM-1"); won != 1 || len(m.Containers) != 1 || m.Containers[0].LoadUnitNo != 1 {
		t.Fatalf("%d containers took the unit: %+v", won, m.Containers)
	}
	if list, err := f.module.ListContainerVisits(ctx, api.ListContainerVisits{TenantID: f.tenant, MovementID: "OM-1"}); err != nil || len(list) != 1 {
		t.Fatalf("the visits written: %+v %v", list, err)
	}
}

// TestAContainerAndACancellationCannotPassEachOther writes a container past SEA and has its checks run before the
// commit: SEA's cancellation of the movement waits for that transaction and is then refused for the container. The
// other way round, a cancellation written past SEA and not yet committed makes SEA's assignment wait, and refuses it.
func TestAContainerAndACancellationCannotPassEachOther(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"1", "2"} {
		f.w.booking("BK-"+id, "C-"+id, "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
		if _, err := f.open("OM-"+id, "C-"+id, "BK-"+id); err != nil {
			t.Fatal(err)
		}
	}
	race := func(first string, immediate bool, second func() error) (passed bool, err error) {
		finished := make(chan error, 1)
		if err := f.within(t, func(txCtx context.Context, e bentopg.Executor) error {
			if _, err := e.Exec(txCtx, first, f.tenant); err != nil {
				return err
			}
			if immediate {
				if _, err := e.Exec(txCtx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
					return err
				}
			}
			go func() { finished <- second() }()
			select {
			case err := <-finished: // the second did not wait for this transaction
				passed = true
				finished <- err
			case <-time.After(500 * time.Millisecond):
			}
			return nil
		}); err != nil {
			t.Fatalf("the write past SEA: %v", err)
		}
		return passed, <-finished
	}
	passed, err := race(`INSERT INTO cargo_sea.container_visit (tenant_id,visit_id,movement_id,consolidation_id,load_unit_no,container_number,size_type,status,
        assigned_by,assigned_at,entered_at) VALUES ($1,'CV-PAST','OM-1','C-1',1,'CSQU3054383','22G1','ASSIGNED','x',now(),now())`, true,
		func() error { _, err := f.cancel("cancel-1", "OM-1", 1, 1); return err })
	if passed || !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "containers assigned") {
		t.Fatalf("the cancellation passed the container (%v): %v", passed, err)
	}
	if m := f.get(t, "OM-1"); m.State != api.StateOpen || len(m.Containers) != 1 || m.Containers[0].VisitID != "CV-PAST" {
		t.Fatalf("the movement: %+v", m)
	}
	passed, err = race(`UPDATE cargo_sea.ocean_movement SET state='CANCELLED',cancel_reason='x',cancelled_by='x',cancelled_at=now(),revision=revision+1
        WHERE tenant_id=$1 AND movement_id='OM-2'`, false,
		func() error { _, err := f.assign("CV-2", "OM-2", 1, "TEMU1234565", 1); return err })
	if passed || !errors.Is(err, api.ErrRejected) {
		t.Fatalf("the assignment passed the cancellation (%v): %v", passed, err)
	}
	if m := f.get(t, "OM-2"); m.State != api.StateCancelled || len(m.Containers) != 0 {
		t.Fatalf("the cancelled movement: %+v", m)
	}
}
