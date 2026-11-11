//go:build integration

// Package integration runs AIR against PostgreSQL 16 with stand-ins for BOOK's CarrierBooking v1 and FUL's
// AirOperationPlan v1. The real contexts together are tested by the composition root.
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	airmodule "go.idp.xyz/idp-cargo/internal/contexts/air"
	"go.idp.xyz/idp-cargo/internal/contexts/air/api"
	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func testDB(t *testing.T) *bentopg.DB {
	t.Helper()
	dsn := os.Getenv("IDP_CARGO_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("IDP_CARGO_POSTGRES_DSN required in CI; skipping forbidden")
		}
		t.Skip("set IDP_CARGO_POSTGRES_DSN to a PostgreSQL 16 application role")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var major int
	var bypass, super bool
	if err = pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int / 10000, rolbypassrls, rolsuper
        FROM pg_roles WHERE rolname = current_user`).Scan(&major, &bypass, &super); err != nil {
		t.Fatal(err)
	}
	if major != 16 || bypass || super {
		t.Fatalf("need PostgreSQL 16 and an application role without BYPASSRLS or superuser (major %d, bypass %v, super %v)", major, bypass, super)
	}
	db, err := bentopg.NewDB(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

func unlocode(code string) kernel.LocationCode {
	return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}
}

func airport(code, iata string) bookapi.ResolvedLocation {
	return bookapi.ResolvedLocation{Code: unlocode(code), IATA: iata}
}

// world stands in for BOOK and FUL: bookings by id and shipment plans by id, which tests change.
type world struct {
	mu       sync.Mutex
	at       time.Time
	bookings map[string]bookapi.CarrierBooking
	plans    map[string]fulapi.AirOperationPlan
}

func (w *world) GetCarrierBooking(_ context.Context, q bookapi.GetCarrierBooking) (bookapi.CarrierBooking, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b, ok := w.bookings[q.BookingID]
	if !ok {
		return bookapi.CarrierBooking{}, fmt.Errorf("%w: stub", bookapi.ErrNotFound)
	}
	b.TenantID = q.TenantID
	return b, nil
}

func (w *world) ListCapacityForConsumer(context.Context, bookapi.ListCapacityForConsumer) ([]bookapi.CarrierBooking, error) {
	return nil, errors.New("not used")
}

func (w *world) FindBookingByCarrierReference(context.Context, bookapi.FindBookingByCarrierReference) (bookapi.CarrierBooking, error) {
	return bookapi.CarrierBooking{}, errors.New("not used")
}

func (w *world) ListBookingsAwaitingCarrier(context.Context, bookapi.ListBookingsAwaitingCarrier) ([]bookapi.BookingSummary, error) {
	return nil, errors.New("not used")
}

func (w *world) ListCutOffsDue(context.Context, bookapi.ListCutOffsDue) ([]bookapi.CutOffDue, error) {
	return nil, errors.New("not used")
}

func (w *world) GetAirOperationPlan(_ context.Context, q fulapi.GetShipmentPlanSnapshot) (fulapi.AirOperationPlan, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.plans[q.ShipmentID]
	if !ok {
		return fulapi.AirOperationPlan{}, fmt.Errorf("%w: stub", fulapi.ErrNotFound)
	}
	p.TenantID = q.TenantID
	return p, nil
}

// booking is a confirmed AIR_CONSOL booking of MU for a consolidation, PVG-JFK on MU587.
func (w *world) booking(id, consolidation, mawb string, capacity int, etd time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.bookings[id] = bookapi.CarrierBooking{BookingID: id, Revision: int64(capacity + 2), Kind: bookapi.KindAirConsol, Status: bookapi.StatusConfirmed,
		LegalEntityID: "LE", CarrierPartyID: "MU", CarrierPartyRevision: 5, CarrierRole: "CARRIER", CarrierIATADesignator: "MU", CarrierAWBPrefix: "781",
		ConsumerKind: bookapi.ConsumerConsolidation, ConsumerID: consolidation, MAWBNumber: mawb,
		Request: bookapi.RequestVersion{Version: 1, LocationEdition: "TEST-EDITION", Legs: []bookapi.ResolvedLeg{{Loading: airport("CNPVG", "PVG"), Discharge: airport("USJFK", "JFK")}}},
		Capacity: bookapi.CapacityVersion{Version: capacity, Lines: []bookapi.CapacityLineView{{Line: 1, Legs: []bookapi.CapacityLeg{{Loading: airport("CNPVG", "PVG"),
			Discharge: airport("USJFK", "JFK"), Flight: "MU587", ETD: etd, ETA: etd.Add(14 * time.Hour)}}}}}}
}

// plan is a PLANNED AIR shipment PVG-JFK of 6 cartons, 300 kg and 2,400 dm3, attached to a consolidation.
func (w *world) plan(shipment, consolidation string, version int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	kg := kernel.Quantity{Unit: "KGM", Amount: 300}
	w.plans[shipment] = fulapi.AirOperationPlan{ShipmentID: shipment, Revision: int64(version + 2), Status: fulapi.StatusPlanned, PlanVersion: version,
		LegalEntityID: "LE", TransportMode: fulapi.ModeAir, Origin: unlocode("CNPVG"), Destination: unlocode("USJFK"), OriginIATA: "PVG", DestinationIATA: "JFK",
		DepartureWindow: kernel.ValidityInterval{From: w.at.Add(48 * time.Hour), Until: w.at.Add(120 * time.Hour)}, Packs: 6, GrossWeight: kg,
		Volume: kernel.Quantity{Unit: "DMQ", Amount: 2400}, Stage: fulapi.StageMain, ConsolidationID: consolidation, MembershipEntryNo: 1, MembershipRevision: 1,
		Parts: []fulapi.LoadIntentPart{{ObligationID: "OB-" + shipment, ObligationVersion: 1, OrderID: "O-" + shipment, Packs: 6, PackageType: "CT", GrossWeight: kg}}}
}

type allow struct{}

func (allow) AuthorizeAirAction(_ context.Context, _, actor, _, _ string) error {
	if actor == "intruder" {
		return errors.New("forbidden")
	}
	return nil
}

type fixture struct {
	module *airmodule.Module
	db     *bentopg.DB
	w      *world
	tenant string
	at     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	at := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	f := &fixture{db: testDB(t), at: at, tenant: "cargo-air-" + time.Now().UTC().Format("20060102T150405.000000000"),
		w: &world{at: at, bookings: map[string]bookapi.CarrierBooking{}, plans: map[string]fulapi.AirOperationPlan{}}}
	m, err := airmodule.New(f.db, f.w, f.w, allow{})
	if err != nil {
		t.Fatal(err)
	}
	f.module = m
	return f
}

func (f *fixture) open(id, consolidation, booking string) (api.Receipt, error) {
	return f.module.OpenAirMovement(context.Background(), api.OpenAirMovement{CommandID: "open-" + id, TenantID: f.tenant, ActorID: "ops-1", MovementID: id,
		ConsolidationID: consolidation, BookingID: booking, OccurredAt: f.at})
}

func (f *fixture) series(t *testing.T) {
	t.Helper()
	if _, err := f.module.RegisterHawbSeries(context.Background(), api.RegisterHawbSeries{CommandID: "series", TenantID: f.tenant, ActorID: "ops-1",
		LegalEntityID: "LE", Prefix: "SHA", Width: 7, Next: 1, OccurredAt: f.at}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) associate(command, movement, shipment, hawb string, revision int64) (api.Receipt, error) {
	return f.module.AssociateHouseWaybill(context.Background(), api.AssociateHouseWaybill{CommandID: command, TenantID: f.tenant, ActorID: "ops-1",
		MovementID: movement, ExpectedRevision: revision, ShipmentID: shipment, HAWB: hawb, OccurredAt: f.at.Add(time.Hour)})
}

func (f *fixture) get(t *testing.T, id string) api.AirMovement {
	t.Helper()
	m, err := f.module.GetAirMovement(context.Background(), api.GetAirMovement{TenantID: f.tenant, MovementID: id})
	if err != nil {
		t.Fatalf("movement %s: %v", id, err)
	}
	return m
}

func (f *fixture) within(t *testing.T, fn func(context.Context, bentopg.Executor) error) error {
	t.Helper()
	return (scope.TenantTransactor{DB: f.db, Tenant: f.tenant}).WithinTransaction(context.Background(), func(txCtx context.Context) error {
		e, err := f.db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		return fn(txCtx, e)
	})
}

func (f *fixture) outboxTypes(t *testing.T) []string {
	t.Helper()
	q, err := f.db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := q.Query(context.Background(), `SELECT event_type FROM bento.outbox WHERE scope=$1 AND source=$2 ORDER BY enqueued_seq`, f.tenant, api.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var kind string
		if err = rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		types = append(types, kind)
	}
	return types
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestAirMovementOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	etd := f.at.Add(72 * time.Hour)
	f.w.booking("BK-1", "C-1", "781-12345675", 1, etd)
	f.w.booking("BK-2", "C-2", "781-23456786", 1, etd.Add(24*time.Hour))
	if _, err := f.open("AM-X", "C-1", "BK-404"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an unknown booking: %v", err)
	}
	if _, err := f.open("AM-X", "C-9", "BK-1"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a booking for another consolidation: %v", err)
	}
	r, err := f.open("AM-1", "C-1", "BK-1")
	if err != nil || r.NewRevision != 1 {
		t.Fatalf("open: %+v %v", r, err)
	}
	if again, err := f.open("AM-1", "C-1", "BK-1"); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err = f.module.OpenAirMovement(ctx, api.OpenAirMovement{CommandID: "open-twice", TenantID: f.tenant, ActorID: "ops-1", MovementID: "AM-1B",
		ConsolidationID: "C-1", BookingID: "BK-1", OccurredAt: f.at}); !errors.Is(err, api.ErrConflict) && !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a second movement for one consolidation and MAWB: %v", err)
	}
	f.w.plan("SH-1", "C-1", 2)
	f.w.plan("SH-2", "C-1", 2)
	f.w.plan("SH-3", "C-1", 2)
	if _, err = f.associate("assoc-1-noseries", "AM-1", "SH-1", "", 1); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("draw without a series: %v", err)
	}
	f.series(t)
	r1, err := f.associate("assoc-1", "AM-1", "SH-1", "", 1)
	if err != nil || r1.HAWB != "SHA0000001" || r1.EntryNo != 1 {
		t.Fatalf("associate SH-1: %+v %v", r1, err)
	}
	if _, err = f.associate("assoc-2", "AM-1", "SH-2", "MANUAL01", 2); err != nil {
		t.Fatal(err)
	}
	if _, err = f.associate("assoc-3", "AM-1", "SH-3", "SHA0000001", 3); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("another shipment's HAWB: %v", err)
	}
	m := f.get(t, "AM-1")
	if m.MAWB != "781-12345675" || len(m.Houses) != 2 || m.Pieces != 12 || m.GrossWeight.Amount != 600_000 || m.ChargeableWeight.Amount != 800_000 ||
		m.Houses[0].ChargeableWeight.Amount != 400_000 || m.Houses[0].Basis != api.BasisPlanned || m.Houses[0].Parts[0].OrderID != "O-SH-1" || m.Houses[0].WindowDrift {
		t.Fatalf("movement: %+v", m)
	}
	byMAWB, err := f.module.FindAirMovementByMAWB(ctx, api.FindAirMovementByMAWB{TenantID: f.tenant, MAWB: "781-12345675"})
	if err != nil || byMAWB.MovementID != "AM-1" {
		t.Fatalf("by MAWB: %+v %v", byMAWB, err)
	}
	ref, err := f.module.FindHouseWaybill(ctx, api.FindHouseWaybill{TenantID: f.tenant, LegalEntityID: "LE", HAWB: "MANUAL01"})
	if err != nil || ref.ShipmentID != "SH-2" || ref.MovementID != "AM-1" {
		t.Fatalf("by HAWB: %+v %v", ref, err)
	}

	// The airline retimes the flight under the same MAWB; the houses stay and drift outside their windows.
	f.w.booking("BK-1", "C-1", "781-12345675", 2, f.at.Add(130*time.Hour))
	if _, err = f.module.LinkCarrierBooking(ctx, api.LinkCarrierBooking{CommandID: "link", TenantID: f.tenant, ActorID: "ops-1", MovementID: "AM-1",
		ExpectedRevision: 3, OccurredAt: f.at.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	m = f.get(t, "AM-1")
	if m.CapacityVersion != 2 || len(m.Flights) != 2 || !m.Flights[0].Current || m.Flights[1].Current || !m.Houses[0].WindowDrift {
		t.Fatalf("relinked: %+v", m.Flights)
	}

	// SH-1 is shortloaded in FUL and refreshed; SH-2 moves to C-2 and is associated there with its HAWB.
	f.w.plan("SH-1", "C-1", 3)
	if _, err = f.module.RefreshHouseWaybillFacts(ctx, api.RefreshHouseWaybillFacts{CommandID: "refresh", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "AM-1", ExpectedRevision: 4, ShipmentID: "SH-1", OccurredAt: f.at.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f.w.plan("SH-2", "C-2", 2)
	end, err := f.module.EndHouseWaybillAssociation(ctx, api.EndHouseWaybillAssociation{CommandID: "end-2", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "AM-1", ExpectedRevision: 5, ShipmentID: "SH-2", OccurredAt: f.at.Add(4 * time.Hour)})
	if err != nil || end.HAWB != "MANUAL01" {
		t.Fatalf("end SH-2: %+v %v", end, err)
	}
	if _, err = f.open("AM-2", "C-2", "BK-2"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.associate("assoc-2-again", "AM-2", "SH-2", "OTHER1", 1); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("another number for a shipment that has one: %v", err)
	}
	if moved, err := f.associate("assoc-2-moved", "AM-2", "SH-2", "", 1); err != nil || moved.HAWB != "MANUAL01" {
		t.Fatalf("the HAWB moves with the shipment: %+v %v", moved, err)
	}
	if _, err = f.module.CancelAirMovement(ctx, api.CancelAirMovement{CommandID: "cancel-busy", TenantID: f.tenant, ActorID: "ops-1", MovementID: "AM-1",
		ExpectedRevision: 6, Reason: "x", OccurredAt: f.at.Add(5 * time.Hour)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("cancel with a house: %v", err)
	}
	f.w.mu.Lock()
	p := f.w.plans["SH-1"]
	p.Status, p.ConsolidationID = fulapi.StatusCancelled, ""
	f.w.plans["SH-1"] = p
	f.w.mu.Unlock()
	if _, err = f.module.EndHouseWaybillAssociation(ctx, api.EndHouseWaybillAssociation{CommandID: "end-1", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "AM-1", ExpectedRevision: 6, ShipmentID: "SH-1", OccurredAt: f.at.Add(5 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.CancelAirMovement(ctx, api.CancelAirMovement{CommandID: "cancel", TenantID: f.tenant, ActorID: "ops-1", MovementID: "AM-1",
		ExpectedRevision: 7, Reason: "consolidation cancelled", OccurredAt: f.at.Add(6 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if m = f.get(t, "AM-1"); m.State != api.StateCancelled || m.MAWBStatus != "VOIDED" {
		t.Fatalf("cancelled: %+v", m)
	}
	if _, err = f.module.FindAirMovementByMAWB(ctx, api.FindAirMovementByMAWB{TenantID: f.tenant, MAWB: "781-12345675"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a voided MAWB: %v", err)
	}
	if s, err := f.module.GetHawbSeries(ctx, api.GetHawbSeries{TenantID: f.tenant, LegalEntityID: "LE"}); err != nil || s.Next != 2 {
		t.Fatalf("series: %+v %v", s, err)
	}
	want := []string{api.AirMovementOpened, api.MawbAssigned, api.FlightSegmentsLinked, api.HawbSeriesRegistered, api.HouseWaybillAssociated,
		api.HouseWaybillAssociated, api.FlightSegmentsLinked, api.HouseWaybillFactsRefreshed, api.HouseWaybillAssociationEnded,
		api.AirMovementOpened, api.MawbAssigned, api.FlightSegmentsLinked, api.HouseWaybillAssociated, api.HouseWaybillAssociationEnded,
		api.MawbVoided, api.AirMovementCancelled}
	if got := f.outboxTypes(t); !slices.Equal(got, want) {
		t.Fatalf("outbox %v", got)
	}
}

// TestDatabaseRules writes past AIR on the application role.
func TestDatabaseRules(t *testing.T) {
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "781-12345675", 1, f.at.Add(72*time.Hour))
	if _, err := f.open("AM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	f.w.plan("SH-1", "C-1", 2)
	if _, err := f.associate("assoc", "AM-1", "SH-1", "", 1); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		sql   string
		state string
	}{
		"segment immutable":  {`UPDATE cargo_air.flight_segment SET flight='MU588' WHERE tenant_id=$1`, "42501|23001"},
		"register immutable": {`UPDATE cargo_air.hawb_number SET shipment_id='SH-9' WHERE tenant_id=$1`, "42501|23001"},
		"house facts":        {`UPDATE cargo_air.house_waybill SET pieces=1 WHERE tenant_id=$1`, "42501|23001"},
		"series backwards":   {`UPDATE cargo_air.hawb_series SET next_seq=1 WHERE tenant_id=$1`, "23001"},
		"MAWB fixed":         {`UPDATE cargo_air.air_movement SET mawb_number='781-23456786' WHERE tenant_id=$1`, "42501|23001"},
		"unregistered HAWB": {`INSERT INTO cargo_air.house_waybill (tenant_id,movement_id,entry_no,shipment_id,hawb,ful_revision,plan_version,window_from,pieces,
            gross_weight_unit,gross_weight,chargeable_grams,chargeable_rule,basis,start_kind,associated_by,associated_at,entered_at)
            VALUES ($1,'AM-1',2,'SH-9','NOPE1',1,1,now(),1,'KGM',1,500,'R','PLANNED','ASSOCIATED','x',now(),now())`, "23514"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		if state := sqlState(err); state == "" || !strings.Contains(tc.state, state) {
			t.Errorf("%s: %v (state %s), want %s", name, err, state, tc.state)
		}
		t.Logf("%s refused: %v", name, err)
	}
}

// TestHouseWaybillsDrawConcurrently associates eight houses with eight movements at once, all drawing from one
// legal entity's series: every one commits with its own number, in order, with none lost.
func TestHouseWaybillsDrawConcurrently(t *testing.T) {
	f := newFixture(t)
	f.series(t)
	const workers = 8
	for i := range workers {
		c := fmt.Sprintf("C-%d", i)
		f.w.booking("BK-"+c, c, fmt.Sprintf("781-%07d%d", 1000000+i, (1000000+i)%7), 1, f.at.Add(72*time.Hour))
		if _, err := f.open("AM-"+c, c, "BK-"+c); err != nil {
			t.Fatal(err)
		}
		f.w.plan("SH-"+c, c, 2)
	}
	hawbs := make([]string, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := fmt.Sprintf("C-%d", i)
			<-start
			r, err := f.associate("assoc-"+c, "AM-"+c, "SH-"+c, "", 1)
			hawbs[i], errs[i] = r.HAWB, err
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("house %d: %v", i, err)
		}
	}
	slices.Sort(hawbs)
	for i, h := range hawbs {
		if want := fmt.Sprintf("SHA%07d", i+1); h != want {
			t.Fatalf("numbers %v", hawbs)
		}
	}
	if s, err := f.module.GetHawbSeries(context.Background(), api.GetHawbSeries{TenantID: f.tenant, LegalEntityID: "LE"}); err != nil || s.Next != workers+1 {
		t.Fatalf("series: %+v %v", s, err)
	}
}

// TestMovementsRaceForOneConsolidation opens eight movements for one consolidation and booking at once: one wins.
func TestMovementsRaceForOneConsolidation(t *testing.T) {
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "781-12345675", 1, f.at.Add(72*time.Hour))
	const workers = 8
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.open(fmt.Sprintf("AM-%d", i), "C-1", "BK-1")
		}()
	}
	close(start)
	wg.Wait()
	committed, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, api.ErrConflict), errors.Is(err, api.ErrRejected):
			refused++
		}
	}
	if committed != 1 || refused != workers-1 {
		t.Fatalf("%d committed, %d refused: %v", committed, refused, errs)
	}
}
