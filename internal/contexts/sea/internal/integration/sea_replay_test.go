//go:build integration

package integration

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	seamodule "go.idp.xyz/idp-cargo/internal/contexts/sea"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// refuse is an authorizer that lets nobody act: the policy of an actor whose right is gone.
type refuse struct{}

func (refuse) AuthorizeSeaAction(context.Context, string, string, string, string) error {
	return errors.New("forbidden")
}

// TestACommandIsAuthorizedBeforeItsReplay: a replay answers only an actor still allowed, and a command id in use
// tells no one else. Before, SEA looked the command id up first: an actor whose right was gone was answered the
// receipt, and an actor without the right could tell a used id (another payload) from an unused one (refused).
func TestACommandIsAuthorizedBeforeItsReplay(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	f.w.booking("BK-2", "C-1", "EVER GLORY", "", "0124W", 1, f.hours(96))
	opened, err := f.open("OM-1", "C-1", "BK-1")
	if err != nil {
		t.Fatal(err)
	}
	open := api.OpenOceanMovement{CommandID: "open-OM-1", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1", ConsolidationID: "C-1",
		BookingID: "BK-1", OccurredAt: f.at}
	if again, err := f.module.OpenOceanMovement(ctx, open); err != nil || again != opened {
		t.Fatalf("the replay of an actor still allowed: %+v %v", again, err)
	}
	refused := func(name string, m *seamodule.Module, cmd api.OpenOceanMovement) {
		t.Helper()
		if r, err := m.OpenOceanMovement(ctx, cmd); !errors.Is(err, api.ErrUnauthorized) || errors.Is(err, api.ErrIdempotencyConflict) || r != (api.Receipt{}) {
			t.Fatalf("%s: %+v %v, want only unauthorized", name, r, err)
		}
	}
	// another actor, who never had the right: the id in use and an id not in use are answered alike
	intruder, unused := open, open
	intruder.ActorID = "intruder"
	unused.ActorID, unused.CommandID, unused.MovementID = "intruder", "open-unused", "OM-2"
	refused("another actor, the id in use", f.module, intruder)
	refused("another actor, an id not in use", f.module, unused)
	// the same actor once its right is gone: the same payload is no longer answered the receipt, and another payload
	// is not told apart from it
	gone, err := seamodule.New(f.db, f.w, f.w, refuse{})
	if err != nil {
		t.Fatal(err)
	}
	other := open
	other.BookingID = "BK-2"
	refused("the same actor without the right, the same payload", gone, open)
	refused("the same actor without the right, another payload", gone, other)
	// with the right, another payload under the id is still another command
	if _, err = f.module.OpenOceanMovement(ctx, other); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("another payload under the id, allowed: %v", err)
	}
	// a visit's command takes the same path
	assigned, err := f.assign("CV-1", "OM-1", 1, "CSQU3054383", 1)
	if err != nil {
		t.Fatal(err)
	}
	assign := api.AssignContainer{CommandID: "assign-CV-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1", MovementID: "OM-1", LoadUnitNo: 1,
		ContainerNumber: "CSQU3054383", SizeType: "22G1", OccurredAt: f.hours(1)}
	if r, err := gone.AssignContainer(ctx, assign); !errors.Is(err, api.ErrUnauthorized) || r != (api.Receipt{}) {
		t.Fatalf("a visit's command without the right: %+v %v (the receipt is %+v)", r, err, assigned)
	}
}

// TestTheSameCommandAtOnceCommitsOnce sends one command eight times at once: it commits once and all eight answer
// its receipt. Before, the copies that lost the race answered a conflict although the command had committed. An
// association reads FUL's plan and the movement's revision before SEA's transaction, so a copy may prepare against
// the movement the first copy has already changed: it still answers the receipt.
func TestTheSameCommandAtOnceCommitsOnce(t *testing.T) {
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	f.w.plan("SH-1", "C-1", 2)
	atOnce := func(name string, send func() (api.Receipt, error)) api.Receipt {
		t.Helper()
		const copies = 8
		receipts, errs := make([]api.Receipt, copies), make([]error, copies)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range copies {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				receipts[i], errs[i] = send()
			}()
		}
		close(start)
		wg.Wait()
		for i := range copies {
			if errs[i] != nil || receipts[i] != receipts[0] {
				t.Fatalf("%s, copy %d: %+v %v, want the receipt %+v of every copy", name, i, receipts[i], errs[i], receipts[0])
			}
		}
		return receipts[0]
	}
	// a movement's command that reads before its transaction
	house := atOnce("the association", func() (api.Receipt, error) { return f.associate("assoc", "OM-1", "SH-1", "", 1) })
	if house.NewRevision != 2 || house.EntryNo != 1 || house.HBL != "SHAL0000001" {
		t.Fatalf("the association: %+v", house)
	}
	// a movement's command that does not
	bill := atOnce("the master bill", func() (api.Receipt, error) {
		return f.masterBill("mbl", "OM-1", "LINU2611000457", api.MasterBillCarrierMBL, 2, 1)
	})
	if bill.NewRevision != 3 || bill.BillNo != 1 {
		t.Fatalf("the master bill: %+v", bill)
	}
	// a visit's command
	visit := atOnce("the container", func() (api.Receipt, error) { return f.assign("CV-1", "OM-1", 1, "CSQU3054383", 1) })
	if visit.NewRevision != 1 || visit.VisitID != "CV-1" {
		t.Fatalf("the container: %+v", visit)
	}
	// each committed once: one house with the first number of the series, one master bill, one visit, one event each
	m := f.get(t, "OM-1")
	if m.Revision != 3 || len(m.Houses) != 1 || len(m.MasterBills) != 1 || len(m.Containers) != 1 {
		t.Fatalf("the movement: revision %d, %d houses, %d master bills, %d containers", m.Revision, len(m.Houses), len(m.MasterBills), len(m.Containers))
	}
	series, err := f.module.GetHblSeries(context.Background(), api.GetHblSeries{TenantID: f.tenant, LegalEntityID: "LE"})
	if err != nil || len(series.Last) != 1 || series.Last[0].Last != 1 {
		t.Fatalf("the series drew once: %+v %v", series, err)
	}
	types := f.outboxTypes(t)
	for _, kind := range []string{api.HouseBillAssociated, api.MasterBillRecorded} {
		if n := len(slices.DeleteFunc(slices.Clone(types), func(s string) bool { return s != kind })); n != 1 {
			t.Fatalf("%s enqueued %d times: %v", kind, n, types)
		}
	}
}
