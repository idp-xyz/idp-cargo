package application

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var now = time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)

type txFake struct{ depth int }

func (x *txFake) WithinTransaction(ctx context.Context, fn func(context.Context) error) error {
	x.depth++
	defer func() { x.depth-- }()
	return fn(ctx)
}

var errNoShipment = errors.New("shipment not found")

type storeFake struct {
	tx        *txFake
	shipments map[ful.ShipmentID]ful.ShipmentState
	revs      map[ful.ShipmentID]int64
	outsideTx bool // PlannedElsewhere was called outside a transaction
}

// PlannedElsewhere sums the live parts other stored shipments plan of an obligation.
func (s *storeFake) PlannedElsewhere(_ context.Context, _ ful.TenantID, id ful.ObligationID, _ int64, except ful.ShipmentID) (ful.CargoScope, error) {
	s.outsideTx = s.outsideTx || s.tx.depth == 0
	var sum ful.CargoScope
	for sid, st := range s.shipments {
		for _, p := range st.Obligations {
			if sid == except || p.ObligationID != id || !p.Planned() {
				continue
			}
			sum.Packs += p.Part.Packs
			sum.GrossWeight = kernel.Quantity{Unit: p.Part.GrossWeight.Unit, Amount: sum.GrossWeight.Amount + p.Part.GrossWeight.Amount}
		}
	}
	return sum, nil
}

func (s *storeFake) Load(_ context.Context, _ ful.TenantID, id ful.ShipmentID) (*ful.Shipment, int64, error) {
	st, ok := s.shipments[id]
	if !ok {
		return nil, 0, errNoShipment
	}
	shipment, err := ful.RestoreShipment(st)
	return shipment, s.revs[id], err
}
func (s *storeFake) Read(ctx context.Context, tenant ful.TenantID, id ful.ShipmentID) (*ful.Shipment, int64, error) {
	return s.Load(ctx, tenant, id)
}
func (s *storeFake) Insert(_ context.Context, shipment *ful.Shipment) (int64, error) {
	s.shipments[shipment.ID()], s.revs[shipment.ID()] = shipment.State(), 1
	return 1, nil
}
func (s *storeFake) Save(_ context.Context, shipment *ful.Shipment, expected int64) (int64, error) {
	if s.revs[shipment.ID()] != expected {
		return 0, ful.ErrConflict
	}
	s.shipments[shipment.ID()] = shipment.State()
	s.revs[shipment.ID()]++
	return s.revs[shipment.ID()], nil
}

type receiptsFake map[string]Receipt

func (r receiptsFake) Find(_ context.Context, _ ful.TenantID, id string) (Receipt, bool, error) {
	got, ok := r[id]
	return got, ok, nil
}
func (r receiptsFake) Save(_ context.Context, _ ful.TenantID, got Receipt) error {
	r[got.CommandID] = got
	return nil
}

type outboxFake struct{ events []string }

func (o *outboxFake) EnqueueFULEvent(_ context.Context, e ful.Event) error {
	o.events = append(o.events, e.Type)
	return nil
}

type authFake struct{ calls int }

func (a *authFake) RequireShipmentAction(context.Context, Scope, ShipmentAction, ful.ShipmentID) error {
	a.calls++
	return nil
}

// comFake answers like COM's anticorruption layer: the listed obligations are plannable for SH-1.
type comFake struct {
	tx       *txFake
	refuse   map[ful.ObligationID]bool
	holders  map[ful.ShipmentID]bool // shipments COM reports as holding every obligation; SH-1 when empty
	packs    int64                   // packs per obligation, 50 kg each; 2 when zero
	calls    []string
	insideTx bool
}

// RequestedMode reports no requested mode: the fake orders leave the mode to FUL.
func (c *comFake) RequestedMode(context.Context, ful.TenantID, ful.ObligationID, int64) (Requested, error) {
	return Requested{}, nil
}

func (c *comFake) Plannable(_ context.Context, _ ful.TenantID, id ful.ObligationID, version int64, shipment ful.ShipmentID) (ful.ObligationRef, error) {
	c.calls = append(c.calls, string(id))
	c.insideTx = c.insideTx || c.tx.depth > 0
	held := shipment == "SH-1"
	if len(c.holders) > 0 {
		held = c.holders[shipment]
	}
	if c.refuse[id] || !held {
		return ful.ObligationRef{}, fmt.Errorf("%w: %s", ErrNotPlannable, id)
	}
	packs := c.packs
	if packs == 0 {
		packs = 2
	}
	return ful.ObligationRef{ObligationID: id, ObligationVersion: version, OrderID: "O-1", CustomerPartyID: "P", LegalEntityID: "LE",
		Packs: packs, PackageType: "PX", GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 50 * packs}}, nil
}

type fixture struct {
	deps   Deps
	store  *storeFake
	outbox *outboxFake
	com    *comFake
}

func newFixture() *fixture {
	tx := &txFake{}
	f := &fixture{store: &storeFake{tx: tx, shipments: map[ful.ShipmentID]ful.ShipmentState{}, revs: map[ful.ShipmentID]int64{}},
		outbox: &outboxFake{}, com: &comFake{tx: tx, refuse: map[ful.ObligationID]bool{}}}
	f.deps = Deps{Tx: tx, Shipments: f.store, Receipts: receiptsFake{}, Outbox: f.outbox, Auth: &authFake{}, Clock: clockFake{},
		Window: kernel.DefaultRecordWindow}
	return f
}

// clockFake records commands a day after the fixtures' business times, inside the record window (T1).
type clockFake struct{}

func (clockFake) Now() time.Time { return now.Add(24 * time.Hour) }

// locationsFake resolves every UN/LOCODE to itself in a test edition, with an IATA code for AIR.
type locationsFake struct{}

func (locationsFake) Resolve(_ context.Context, mode ful.TransportMode, l kernel.LocationCode) (ResolvedLocation, error) {
	r := ResolvedLocation{Code: l, Edition: "TEST-EDITION"}
	if mode == ful.ModeAir {
		r.IATA = l.Code[2:]
	}
	return r, nil
}

var planner = Scope{TenantID: "T", ActorID: "planner-1"}

func (f *fixture) create(command string) (Receipt, error) {
	return CreateShipmentFromObligation{Deps: f.deps, Obligations: f.com}.Handle(context.Background(), CreateShipmentFromObligationCommand{
		CommandID: command, Scope: planner, ShipmentID: "SH-1", ObligationID: "OB-1", ObligationVersion: 1, OccurredAt: now})
}

func (f *fixture) confirm(command string, revision int64) (Receipt, error) {
	return ConfirmShipmentPlan{Deps: f.deps, Obligations: f.com}.Handle(context.Background(), ConfirmShipmentPlanCommand{
		CommandID: command, Scope: planner, ShipmentID: "SH-1", ExpectedRevision: revision, OccurredAt: now.Add(3 * time.Hour)})
}

func TestCreateAsksCOMOutsideTransactionsAndReplays(t *testing.T) {
	f := newFixture()
	r, err := f.create("create-1")
	if err != nil || r.NewRevision != 1 || r.ShipmentID != "SH-1" || f.store.shipments["SH-1"].Status != ful.ShipmentDraft {
		t.Fatalf("create: %+v, %v", r, err)
	}
	if again, err := f.create("create-1"); err != nil || again != r || len(f.com.calls) != 1 {
		t.Fatalf("replay = %+v, %v; COM calls %v", again, err, f.com.calls)
	}
	if f.com.insideTx {
		t.Fatal("COM was asked inside a FUL transaction")
	}
	f.com.refuse["OB-1"] = true
	if _, err := (CreateShipmentFromObligation{Deps: f.deps, Obligations: f.com}).Handle(context.Background(), CreateShipmentFromObligationCommand{
		CommandID: "create-2", Scope: planner, ShipmentID: "SH-2", ObligationID: "OB-1", ObligationVersion: 1, OccurredAt: now}); !errors.Is(err, ErrNotPlannable) {
		t.Fatalf("an obligation COM does not hold for the shipment: err = %v", err)
	}
	if len(f.store.shipments) != 1 || fmt.Sprint(f.outbox.events) != "[ShipmentCreated.v1]" {
		t.Fatalf("shipments %d, events %v", len(f.store.shipments), f.outbox.events)
	}
}

func TestConfirmRechecksEveryPlannedObligationWithCOM(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	if _, err := f.create("create-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := (AddObligationToShipment{Deps: f.deps, Obligations: f.com}).Handle(ctx, AddObligationToShipmentCommand{CommandID: "add-1",
		Scope: planner, ShipmentID: "SH-1", ExpectedRevision: 1, ObligationID: "OB-2", ObligationVersion: 1, OccurredAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	route := ful.Route{Mode: ful.ModeSea, Origin: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHA"},
		Destination: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "DEHAM"}, Departure: kernel.ValidityInterval{From: now.Add(72 * time.Hour), Until: now.Add(96 * time.Hour)}}
	if _, err := (AmendShipmentPlan{Deps: f.deps, Locations: locationsFake{}, Modes: f.com}).Handle(ctx, AmendShipmentPlanCommand{CommandID: "amend-1", Scope: planner, ShipmentID: "SH-1",
		ExpectedRevision: 2, Route: route, Reason: "schedule", OccurredAt: now.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f.com.calls = nil
	if _, err := f.confirm("confirm-0", 2); !errors.Is(err, ful.ErrConflict) || len(f.com.calls) != 0 {
		t.Fatalf("stale revision: err = %v, COM calls %v", err, f.com.calls)
	}
	f.com.refuse["OB-2"] = true
	if _, err := f.confirm("confirm-1", 3); !errors.Is(err, ErrNotPlannable) || f.store.shipments["SH-1"].Status != ful.ShipmentDraft {
		t.Fatalf("COM no longer holds OB-2 for SH-1: err = %v", err)
	}
	delete(f.com.refuse, "OB-2")
	f.com.calls = nil
	r, err := f.confirm("confirm-2", 3)
	if err != nil || r.NewRevision != 4 || f.store.shipments["SH-1"].Status != ful.ShipmentPlanned || fmt.Sprint(f.com.calls) != "[OB-1 OB-2]" || f.com.insideTx {
		t.Fatalf("confirm: %+v, %v; COM calls %v (inside a transaction %v)", r, err, f.com.calls, f.com.insideTx)
	}
	release := ReleaseObligationFromShipmentCommand{CommandID: "release-1", Scope: planner, ShipmentID: "SH-1", ExpectedRevision: 4,
		ObligationID: "OB-1", ObligationVersion: 1, OccurredAt: now.Add(4 * time.Hour)}
	if r, err = (ReleaseObligationFromShipment{Deps: f.deps}).Handle(ctx, release); err != nil || f.store.shipments["SH-1"].Status != ful.ShipmentDraft {
		t.Fatalf("release: %+v, %v", r, err)
	}
	cancel := CancelShipmentCommand{CommandID: "cancel-1", Scope: planner, ShipmentID: "SH-1", ExpectedRevision: 4, Reason: "withdrawn",
		OccurredAt: now.Add(5 * time.Hour)}
	if _, err = (CancelShipment{Deps: f.deps}).Handle(ctx, cancel); !errors.Is(err, ful.ErrConflict) {
		t.Fatalf("cancel at a stale revision: err = %v", err)
	}
	cancel.CommandID, cancel.ExpectedRevision = "cancel-2", 5
	if _, err = (CancelShipment{Deps: f.deps}).Handle(ctx, cancel); err != nil || f.store.shipments["SH-1"].Status != ful.ShipmentCancelled {
		t.Fatalf("cancel: %v", err)
	}
	want := "[ShipmentCreated.v1 ShipmentObligationAdded.v1 ShipmentPlanAmended.v1 ShipmentPlanConfirmed.v1 ShipmentObligationReleased.v1 ShipmentCancelled.v1]"
	if got := fmt.Sprint(f.outbox.events); got != want {
		t.Fatalf("events %s", got)
	}
}

// F1: one obligation of 10 pallets is split over shipments; what other shipments plan is read in the
// command's transaction, a command without a part takes what is left, and parts can be replanned.
func TestPartsOfOneObligationAcrossShipments(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	f.com.packs, f.com.holders = 10, map[ful.ShipmentID]bool{"SH-1": true, "SH-2": true, "SH-3": true}
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	create := func(command string, shipment ful.ShipmentID, part ful.CargoScope) (Receipt, error) {
		return CreateShipmentFromObligation{Deps: f.deps, Obligations: f.com}.Handle(ctx, CreateShipmentFromObligationCommand{
			CommandID: command, Scope: planner, ShipmentID: shipment, ObligationID: "OB-1", ObligationVersion: 1, Part: part, OccurredAt: now})
	}
	change := func(command string, shipment ful.ShipmentID, revision int64, part ful.CargoScope) (Receipt, error) {
		return ChangeShipmentPart{Deps: f.deps}.Handle(ctx, ChangeShipmentPartCommand{CommandID: command, Scope: planner, ShipmentID: shipment,
			ExpectedRevision: revision, ObligationID: "OB-1", ObligationVersion: 1, Part: part, Reason: "MU587 loaded 6 of 10", OccurredAt: now.Add(time.Hour)})
	}
	if _, err := create("create-1", "SH-1", ful.CargoScope{Packs: 6, GrossWeight: kg(300)}); err != nil {
		t.Fatal(err)
	}
	if _, err := create("create-2", "SH-2", ful.CargoScope{}); err != nil {
		t.Fatal(err)
	}
	if got := f.store.shipments["SH-2"].Plans[0].Scope; got != (ful.CargoScope{Packs: 4, GrossWeight: kg(200)}) {
		t.Fatalf("a command without a part takes what is left: %+v", got)
	}
	if _, err := create("create-3", "SH-3", ful.CargoScope{}); !errors.Is(err, ErrPlannedElsewhere) {
		t.Fatalf("nothing left: err = %v", err)
	}
	if _, err := create("create-4", "SH-3", ful.CargoScope{Packs: 1, GrossWeight: kg(50)}); !errors.Is(err, ful.ErrInvalidPart) {
		t.Fatalf("a part beyond what is left: err = %v", err)
	}
	if _, err := change("change-0", "SH-2", 1, ful.CargoScope{Packs: 5, GrossWeight: kg(250)}); !errors.Is(err, ful.ErrInvalidPart) {
		t.Fatalf("growing before the other shrinks: err = %v", err)
	}
	if _, err := change("change-1", "SH-1", 1, ful.CargoScope{Packs: 4, GrossWeight: kg(200)}); err != nil {
		t.Fatal(err)
	}
	r, err := change("change-2", "SH-2", 1, ful.CargoScope{Packs: 6, GrossWeight: kg(300)})
	if err != nil || r.NewRevision != 2 {
		t.Fatalf("growing into what was freed: %+v, %v", r, err)
	}
	if again, err := change("change-2", "SH-2", 1, ful.CargoScope{Packs: 6, GrossWeight: kg(300)}); err != nil || again != r {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if f.store.outsideTx {
		t.Fatal("what other shipments plan was read outside the command's transaction")
	}
	want := "[ShipmentCreated.v1 ShipmentCreated.v1 ShipmentPartChanged.v1 ShipmentPartChanged.v1]"
	if got := fmt.Sprint(f.outbox.events); got != want {
		t.Fatalf("events %s", got)
	}
}
