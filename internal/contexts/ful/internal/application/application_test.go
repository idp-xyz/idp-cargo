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
	shipments map[ful.ShipmentID]ful.ShipmentState
	revs      map[ful.ShipmentID]int64
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
	calls    []string
	insideTx bool
}

func (c *comFake) Plannable(_ context.Context, _ ful.TenantID, id ful.ObligationID, version int64, shipment ful.ShipmentID) (ful.ObligationRef, error) {
	c.calls = append(c.calls, string(id))
	c.insideTx = c.insideTx || c.tx.depth > 0
	if c.refuse[id] || shipment != "SH-1" {
		return ful.ObligationRef{}, fmt.Errorf("%w: %s", ErrNotPlannable, id)
	}
	return ful.ObligationRef{ObligationID: id, ObligationVersion: version, OrderID: "O-1", CustomerPartyID: "P", LegalEntityID: "LE",
		Packs: 2, PackageType: "PX", GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 100}}, nil
}

type fixture struct {
	deps   Deps
	store  *storeFake
	outbox *outboxFake
	com    *comFake
}

func newFixture() *fixture {
	tx := &txFake{}
	f := &fixture{store: &storeFake{shipments: map[ful.ShipmentID]ful.ShipmentState{}, revs: map[ful.ShipmentID]int64{}},
		outbox: &outboxFake{}, com: &comFake{tx: tx, refuse: map[ful.ObligationID]bool{}}}
	f.deps = Deps{Tx: tx, Shipments: f.store, Receipts: receiptsFake{}, Outbox: f.outbox, Auth: &authFake{}}
	return f
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
	if _, err := (AmendShipmentPlan{Deps: f.deps}).Handle(ctx, AmendShipmentPlanCommand{CommandID: "amend-1", Scope: planner, ShipmentID: "SH-1",
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
