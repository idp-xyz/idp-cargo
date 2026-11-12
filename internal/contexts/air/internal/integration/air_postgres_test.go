//go:build integration

// Package integration runs AIR against PostgreSQL 16 with stand-ins for BOOK's CarrierBooking v1 and FUL's
// AirOperationPlan v1. The real contexts together are tested by the composition root.
package integration

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"reflect"
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
	// templates whose numbers are no HAWB numbers, or of the platform's tokens Cargo does not take, are refused
	for _, template := range []string{"SH-{seq:7}", "SHANGHAI{seq:6}", "SHA{rand:4}{seq:6}", "sha{seq:7}", "{seq:7}", "SHA{yy}{seq:6}"} {
		if _, err := f.module.RegisterHawbSeries(context.Background(), api.RegisterHawbSeries{CommandID: "series-" + template, TenantID: f.tenant,
			ActorID: "ops-1", LegalEntityID: "LE", Template: template, FirstNumber: 1, OccurredAt: f.at}); !errors.Is(err, api.ErrRejected) {
			t.Fatalf("template %s: %v", template, err)
		}
	}
	if _, err := f.module.RegisterHawbSeries(context.Background(), api.RegisterHawbSeries{CommandID: "series", TenantID: f.tenant, ActorID: "ops-1",
		LegalEntityID: "LE", Template: "SHA{seq:7}", FirstNumber: 1, OccurredAt: f.at}); err != nil {
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
	if s, err := f.module.GetHawbSeries(ctx, api.GetHawbSeries{TenantID: f.tenant, LegalEntityID: "LE"}); err != nil || s.Template != "SHA{seq:7}" ||
		len(s.Last) != 1 || s.Last[0] != (api.NumberPeriod{Period: "", Last: 1}) {
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
		"series fixed":       {`UPDATE cargo_air.hawb_series SET first_number=2 WHERE tenant_id=$1`, "42501|23001"},
		"sequence backwards": {`UPDATE cargo_air.numbering_sequence SET next_val=0 WHERE scope=$1`, "23001"},
		"sequence deleted":   {`DELETE FROM cargo_air.numbering_sequence WHERE scope=$1`, "42501|23001"},
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
	if s, err := f.module.GetHawbSeries(context.Background(), api.GetHawbSeries{TenantID: f.tenant, LegalEntityID: "LE"}); err != nil || len(s.Last) != 1 ||
		s.Last[0].Last != workers {
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

// measured makes a shipment's plan carry PHY's tendered measures as FUL relays them: 318 kg and 1 m3, screened
// and cleared, with the order line's goods description.
func (w *world) measured(shipment string, evidence int, reference string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.plans[shipment]
	p.EvidenceVersion, p.MeasuredBasis, p.MeasuredPacks, p.SecurityStatus = evidence, fulapi.MeasuredTendered, 6, fulapi.SecurityCleared
	p.MeasuredGrossWeight, p.MeasuredVolume = kernel.Quantity{Unit: "KGM", Amount: 318}, kernel.Quantity{Unit: "MTQ", Amount: 1}
	p.Parts = append([]fulapi.LoadIntentPart(nil), p.Parts...)
	p.Parts[0].GoodsDescription = "Auto parts in cartons"
	p.Screenings = []fulapi.SecurityScreening{{LotID: "LOT-" + shipment, Outcome: "PASSED", Screener: "PVG cargo security station", Method: "XRY",
		Reference: reference, ScreenedAt: w.at.Add(30 * time.Minute)}}
	w.plans[shipment] = p
}

// AIR V0.3: finalize the movement's facts for DOC, supersede them with a change, refuse stale houses, finalize the
// next version; versions are immutable and a FINAL movement still holds its consolidation.
func TestDocFactsOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "781-12345675", 1, f.at.Add(72*time.Hour))
	if _, err := f.open("AM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	f.w.plan("SH-1", "C-1", 2)
	f.w.plan("SH-2", "C-1", 2)
	if _, err := f.associate("assoc-1", "AM-1", "SH-1", "", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.associate("assoc-2", "AM-1", "SH-2", "", 2); err != nil {
		t.Fatal(err)
	}
	finalize := func(command string, revision int64, hours int) (api.Receipt, error) {
		return f.module.FinalizeAirDocFacts(ctx, api.FinalizeAirDocFacts{CommandID: command, TenantID: f.tenant, ActorID: "ops-2", MovementID: "AM-1",
			ExpectedRevision: revision, OccurredAt: f.at.Add(time.Duration(hours) * time.Hour)})
	}
	refresh := func(command, shipment string, revision int64, hours int) error {
		_, err := f.module.RefreshHouseWaybillFacts(ctx, api.RefreshHouseWaybillFacts{CommandID: command, TenantID: f.tenant, ActorID: "ops-1",
			MovementID: "AM-1", ExpectedRevision: revision, ShipmentID: shipment, OccurredAt: f.at.Add(time.Duration(hours) * time.Hour)})
		return err
	}
	if _, err := finalize("fin-planned", 3, 2); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("planned houses: %v", err)
	}
	if _, err := f.module.GetAirDocFacts(ctx, api.GetAirDocFacts{TenantID: f.tenant, MovementID: "AM-1"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("never finalized: %v", err)
	}
	f.w.measured("SH-1", 2, "XR-0001")
	f.w.measured("SH-2", 2, "XR-0002")
	if err := errors.Join(refresh("refresh-1", "SH-1", 3, 2), refresh("refresh-2", "SH-2", 4, 2)); err != nil {
		t.Fatal(err)
	}
	r, err := finalize("fin-1", 5, 3)
	if err != nil || r.FactsVersion != 1 || r.NewRevision != 6 {
		t.Fatalf("finalize: %+v %v", r, err)
	}
	if again, err := finalize("fin-1", 5, 3); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	m := f.get(t, "AM-1")
	if m.State != api.StateFinal || m.DocFactsVersion != 1 || m.Houses[0].Parts[0].GoodsDescription != "Auto parts in cartons" ||
		len(m.Houses[1].Screenings) != 1 || m.Houses[1].Screenings[0].Reference != "XR-0002" {
		t.Fatalf("movement: %+v", m)
	}
	v1, err := f.module.GetAirDocFacts(ctx, api.GetAirDocFacts{TenantID: f.tenant, MovementID: "AM-1"})
	if err != nil || v1.Version != 1 || v1.Status != api.DocFactsFinal || v1.MAWB != "781-12345675" || v1.MAWBSource != api.MAWBSourceBookingConfirmation ||
		v1.SecurityStatus != api.SecuritySPX || v1.Pieces != 12 || v1.GrossWeight.Amount != 636_000 || v1.Volume != (kernel.Quantity{Unit: "CMQ", Amount: 2_000_000}) ||
		v1.ChargeableWeight.Amount != 636_000 || len(v1.Flights) != 1 || v1.Origin.IATA != "PVG" || len(v1.Houses) != 2 || len(v1.SHA256) != 64 {
		t.Fatalf("version 1: %+v %v", v1, err)
	}
	if h := v1.Houses[0]; h.HAWB != "SHA0000001" || h.Basis != api.BasisTendered || h.EvidenceVersion != 2 || h.ChargeableWeight.Amount != 318_000 ||
		h.SecurityStatus != api.SecuritySPX || h.Screenings[0].Screener != "PVG cargo security station" || !h.Screenings[0].ScreenedAt.Equal(f.at.Add(30*time.Minute)) ||
		h.Parts[0].GoodsDescription != "Auto parts in cartons" || h.Parts[0].PackageType != "CT" {
		t.Fatalf("house of version 1: %+v", h)
	}
	if _, err = finalize("fin-again", 6, 3); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("finalized already: %v", err)
	}
	f.w.booking("BK-1B", "C-1", "781-23456786", 1, f.at.Add(72*time.Hour))
	if _, err = f.open("AM-1B", "C-1", "BK-1B"); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a FINAL movement holds its consolidation: %v", err)
	}

	// The security station reissues SH-2's screening reference: refreshed, the movement is OPEN and version 1 superseded.
	f.w.measured("SH-2", 3, "XR-0002-R")
	if err = refresh("refresh-2b", "SH-2", 6, 4); err != nil {
		t.Fatal(err)
	}
	if v, err := f.module.GetAirDocFacts(ctx, api.GetAirDocFacts{TenantID: f.tenant, MovementID: "AM-1", Version: 1}); err != nil ||
		v.Status != api.DocFactsSuperseded || v.SHA256 != v1.SHA256 || v.Houses[1].Screenings[0].Reference != "XR-0002" {
		t.Fatalf("superseded: %+v %v", v, err)
	}
	if m = f.get(t, "AM-1"); m.State != api.StateOpen {
		t.Fatalf("reopened: %s", m.State)
	}
	// FUL re-plans SH-1 meanwhile: AIR's house is stale until refreshed.
	f.w.plan("SH-1", "C-1", 3)
	f.w.measured("SH-1", 4, "XR-0001")
	if _, err = finalize("fin-stale", 7, 5); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "STALE_HOUSE") {
		t.Fatalf("stale house: %v", err)
	}
	if err = refresh("refresh-1b", "SH-1", 7, 5); err != nil {
		t.Fatal(err)
	}
	if r, err = finalize("fin-2", 8, 6); err != nil || r.FactsVersion != 2 {
		t.Fatalf("version 2: %+v %v", r, err)
	}
	v2, err := f.module.GetAirDocFacts(ctx, api.GetAirDocFacts{TenantID: f.tenant, MovementID: "AM-1"})
	if err != nil || v2.Version != 2 || v2.Status != api.DocFactsFinal || v2.Houses[0].PlanVersion != 3 || v2.Houses[1].Screenings[0].Reference != "XR-0002-R" {
		t.Fatalf("version 2: %+v %v", v2, err)
	}
	if _, err = f.module.GetAirDocFacts(ctx, api.GetAirDocFacts{TenantID: f.tenant, MovementID: "AM-1", Version: 3}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("no version 3: %v", err)
	}
	// From FINAL, a house associated and then ended: version 2 is superseded once, and version 3 finalized.
	f.w.plan("SH-3", "C-1", 2)
	if _, err = f.module.AssociateHouseWaybill(ctx, api.AssociateHouseWaybill{CommandID: "assoc-3", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "AM-1", ExpectedRevision: 9, ShipmentID: "SH-3", OccurredAt: f.at.Add(7 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.module.GetAirDocFacts(ctx, api.GetAirDocFacts{TenantID: f.tenant, MovementID: "AM-1"}); v.Version != 2 || v.Status != api.DocFactsSuperseded {
		t.Fatalf("associated from FINAL: %+v", v)
	}
	f.w.plan("SH-3", "C-2", 2)
	if _, err = f.module.EndHouseWaybillAssociation(ctx, api.EndHouseWaybillAssociation{CommandID: "end-3", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "AM-1", ExpectedRevision: 10, ShipmentID: "SH-3", OccurredAt: f.at.Add(7 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if r, err = finalize("fin-3", 11, 8); err != nil || r.FactsVersion != 3 || r.NewRevision != 12 {
		t.Fatalf("version 3: %+v %v", r, err)
	}
	v3, err := f.module.GetAirDocFacts(ctx, api.GetAirDocFacts{TenantID: f.tenant, MovementID: "AM-1"})
	if err != nil || v3.Status != api.DocFactsFinal || len(v3.Houses) != 2 || fmt.Sprintf("%x", sha256.Sum256(v3.Content)) != v3.SHA256 {
		t.Fatalf("version 3: %+v %v", v3, err)
	}
	var stored string
	if err = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT string_agg(content::jsonb->>'mawb' || ':' || content_sha256 || ':' || movement_revision, ',' ORDER BY version_no)
          FROM cargo_air.air_doc_facts_version WHERE tenant_id=$1`, f.tenant).Scan(&stored)
	}); err != nil || stored != "781-12345675:"+v1.SHA256+":6,781-12345675:"+v2.SHA256+":9,781-12345675:"+v3.SHA256+":12" {
		t.Fatalf("stored versions: %s %v", stored, err)
	}
	want := []string{api.AirMovementOpened, api.MawbAssigned, api.FlightSegmentsLinked, api.HawbSeriesRegistered, api.HouseWaybillAssociated,
		api.HouseWaybillAssociated, api.HouseWaybillFactsRefreshed, api.HouseWaybillFactsRefreshed, api.AirDocFactsFinalized,
		api.AirDocFactsSuperseded, api.HouseWaybillFactsRefreshed, api.HouseWaybillFactsRefreshed, api.AirDocFactsFinalized,
		api.AirDocFactsSuperseded, api.HouseWaybillAssociated, api.HouseWaybillAssociationEnded, api.AirDocFactsFinalized}
	if got := f.outboxTypes(t); !slices.Equal(got, want) {
		t.Fatalf("outbox %v", got)
	}

	// Read on a session in another time zone: the same bytes, the same reading, still current.
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
	shanghai, err := airmodule.New(shanghaiDB, f.w, f.w, allow{})
	if err != nil {
		t.Fatal(err)
	}
	there, err := shanghai.GetAirDocFacts(ctx, api.GetAirDocFacts{TenantID: f.tenant, MovementID: "AM-1"})
	if err != nil || !reflect.DeepEqual(there, v3) {
		t.Fatalf("read in Asia/Shanghai: %+v %v", there, err)
	}
	if m, err := shanghai.GetAirMovement(ctx, api.GetAirMovement{TenantID: f.tenant, MovementID: "AM-1"}); err != nil || m.State != api.StateFinal ||
		!reflect.DeepEqual(m.Houses, f.get(t, "AM-1").Houses) {
		t.Fatalf("movement read in Asia/Shanghai: %v", err)
	}

	// Past AIR on the application role: versions and screenings are immutable; a version needs a FINAL movement at
	// its revision, and a FINAL movement its version; a house's screenings and parts are written with its entry.
	f.w.booking("BK-3", "C-3", "781-34567890", 1, f.at.Add(72*time.Hour))
	if _, err = f.open("AM-3", "C-3", "BK-3"); err != nil {
		t.Fatal(err)
	}
	version := func(movement string, no, revision int) string {
		return fmt.Sprintf(`INSERT INTO cargo_air.air_doc_facts_version (tenant_id,movement_id,version_no,content,content_sha256,movement_revision,
            finalized_by,finalized_at,entered_at) SELECT $1,'%[1]s',%[2]d,c,encode(sha256(convert_to(c,'UTF8')),'hex'),%[3]d,'x',now(),now()
            FROM (VALUES ('{"version":%[2]d,"movement_id":"%[1]s","houses":[{}]}')) t(c)`, movement, no, revision)
	}
	entry := `(SELECT max(entry_no) FROM cargo_air.house_waybill WHERE tenant_id=$1 AND movement_id='AM-1' AND ended_at IS NULL)`
	for name, tc := range map[string]struct {
		sql   string
		state string
	}{
		"version immutable":         {`UPDATE cargo_air.air_doc_facts_version SET finalized_by='x' WHERE tenant_id=$1`, "42501|23001"},
		"version deleted":           {`DELETE FROM cargo_air.air_doc_facts_version WHERE tenant_id=$1`, "42501|23001"},
		"screening immutable":       {`UPDATE cargo_air.house_waybill_screening SET reference='x' WHERE tenant_id=$1`, "42501|23001"},
		"FINAL without facts":       {`UPDATE cargo_air.air_movement SET state='FINAL' WHERE tenant_id=$1 AND movement_id='AM-3'`, "23514"},
		"facts of an OPEN movement": {version("AM-3", 1, 1), "23514"},
		"a version beside FINAL":    {version("AM-1", 4, 99), "23514"},
		"digest of other bytes": {`INSERT INTO cargo_air.air_doc_facts_version (tenant_id,movement_id,version_no,content,content_sha256,movement_revision,
            finalized_by,finalized_at,entered_at) VALUES ($1,'AM-3',1,'{"version":1,"movement_id":"AM-3","houses":[{}]}',repeat('a',64),1,'x',now(),now())`, "23514"},
		"FINAL changed behind AIR": {`UPDATE cargo_air.air_movement SET revision=revision+1 WHERE tenant_id=$1 AND movement_id='AM-1'`, "23514"},
		"screening added later": {`INSERT INTO cargo_air.house_waybill_screening (tenant_id,movement_id,entry_no,screening_no,lot_id,outcome,screener,method,
            reference,screened_at) VALUES ($1,'AM-1',` + entry + `,9,'L-X','PASSED','x','XRY','r',now())`, "23514"},
		"part added later": {`INSERT INTO cargo_air.house_waybill_part (tenant_id,movement_id,entry_no,part_no,obligation_id,obligation_version,order_id,packs,
            package_type,gross_weight_unit,gross_weight) VALUES ($1,'AM-1',` + entry + `,9,'OB-X',1,'O-X',1,'CT','KGM',1)`, "23514"},
		"FINAL cancelled": {`UPDATE cargo_air.air_movement SET state='CANCELLED',mawb_status='VOIDED',cancel_reason='x',cancelled_by='x',cancelled_at=now()
            WHERE tenant_id=$1 AND movement_id='AM-1'`, "23001"},
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
