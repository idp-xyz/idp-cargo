//go:build integration

// Package integration runs SEA against PostgreSQL 16 with stand-ins for BOOK's CarrierBooking v1 and FUL's
// OceanOperationPlan v1. The real contexts together are tested by the composition root.
package integration

import (
	"context"
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
	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	seamodule "go.idp.xyz/idp-cargo/internal/contexts/sea"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
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

func port(code string) bookapi.ResolvedLocation {
	return bookapi.ResolvedLocation{Code: unlocode(code)}
}

func kg(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }

// world stands in for BOOK and FUL: bookings by id and shipment plans by id, which tests change.
type world struct {
	mu       sync.Mutex
	at       time.Time
	bookings map[string]bookapi.CarrierBooking
	plans    map[string]fulapi.OceanOperationPlan
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

func (w *world) GetOceanOperationPlan(_ context.Context, q fulapi.GetShipmentPlanSnapshot) (fulapi.OceanOperationPlan, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.plans[q.ShipmentID]
	if !ok {
		return fulapi.OceanOperationPlan{}, fmt.Errorf("%w: stub", fulapi.ErrNotFound)
	}
	p.TenantID = q.TenantID
	return p, nil
}

// booking is a confirmed container booking of shipping line line-1 for a consolidation, Shanghai to Hamburg on one
// vessel, with the VGM and shipping instruction cut-offs.
func (w *world) booking(id, consolidation, vessel, imo, voyage string, capacity int, etd time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.bookings[id] = bookapi.CarrierBooking{BookingID: id, Revision: int64(capacity + 2), Kind: bookapi.KindSeaLCLConsolEquipment, Status: bookapi.StatusConfirmed,
		LegalEntityID: "LE", CarrierPartyID: "line-1", CarrierPartyRevision: 5, CarrierRole: "CARRIER", ConsumerKind: bookapi.ConsumerConsolidation,
		ConsumerID: consolidation,
		Request:    bookapi.RequestVersion{Version: 1, LocationEdition: "TEST-EDITION", Legs: []bookapi.ResolvedLeg{{Loading: port("CNSHG"), Discharge: port("DEHAM")}}},
		Capacity: bookapi.CapacityVersion{Version: capacity, Lines: []bookapi.CapacityLineView{{Line: 1, Legs: []bookapi.CapacityLeg{{Loading: port("CNSHG"),
			Discharge: port("DEHAM"), Vessel: vessel, VesselIMO: imo, Voyage: voyage, ETD: etd, ETA: etd.Add(30 * 24 * time.Hour), CutOffs: []bookapi.CutOff{
				{Kind: bookapi.CutOffVGM, At: etd.Add(-48 * time.Hour), TimeZone: "Asia/Shanghai"},
				{Kind: bookapi.CutOffSIDocuments, At: etd.Add(-72 * time.Hour), TimeZone: "Asia/Shanghai", Location: unlocode("CNSHG")}}}}}}}}
}

// plan is a PLANNED SEA LCL shipment Shanghai to Hamburg of 6 cartons, 300 kg and 2,400 dm3, attached to a
// consolidation.
func (w *world) plan(shipment, consolidation string, version int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.plans[shipment] = fulapi.OceanOperationPlan{ShipmentID: shipment, Revision: int64(version + 2), Status: fulapi.StatusPlanned, PlanVersion: version,
		LegalEntityID: "LE", TransportMode: fulapi.ModeSea, ContainerMode: fulapi.ContainerLCL, Origin: unlocode("CNSHG"), Destination: unlocode("DEHAM"),
		DepartureWindow: kernel.ValidityInterval{From: w.at.Add(48 * time.Hour), Until: w.at.Add(120 * time.Hour)}, Packs: 6, GrossWeight: kg(300),
		Volume: kernel.Quantity{Unit: "DMQ", Amount: 2400}, Stage: fulapi.StageMain, ConsolidationID: consolidation, MembershipEntryNo: 1, MembershipRevision: 1,
		Parts: []fulapi.LoadIntentPart{{ObligationID: "OB-" + shipment, ObligationVersion: 1, OrderID: "O-" + shipment, Packs: 6, PackageType: "CT",
			GrossWeight: kg(300), GoodsDescription: "Furniture parts in cartons"}}}
}

// stuffed makes a shipment's plan carry PHY's sealed 22G1 with its six packs, 318 kg, as FUL relays it.
func (w *world) stuffed(shipment string, evidence int, seal string) fulapi.ContainerStuffing {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.plans[shipment]
	s := fulapi.ContainerStuffing{ExecutionID: "X-1", LoadUnitNo: 1, TypeCode: "22G1", Packs: 6, GrossWeight: kg(318), LineNos: []int{1, 3},
		Seal: seal, SealSHA256: strings.Repeat("c4", 32), SealedAt: w.at.Add(30 * time.Minute)}
	p.EvidenceVersion, p.MeasuredBasis, p.MeasuredPacks, p.MeasuredGrossWeight = evidence, fulapi.MeasuredTendered, 6, kg(318)
	p.Stuffings = []fulapi.ContainerStuffing{s}
	w.plans[shipment] = p
	return s
}

type allow struct{}

func (allow) AuthorizeSeaAction(_ context.Context, _, actor, _, _ string) error {
	if actor == "intruder" {
		return errors.New("forbidden")
	}
	return nil
}

type fixture struct {
	module *seamodule.Module
	db     *bentopg.DB
	w      *world
	tenant string
	at     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	at := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	f := &fixture{db: testDB(t), at: at, tenant: "cargo-sea-" + time.Now().UTC().Format("20060102T150405.000000000"),
		w: &world{at: at, bookings: map[string]bookapi.CarrierBooking{}, plans: map[string]fulapi.OceanOperationPlan{}}}
	m, err := seamodule.New(f.db, f.w, f.w, allow{})
	if err != nil {
		t.Fatal(err)
	}
	f.module = m
	return f
}

func (f *fixture) hours(h int) time.Time { return f.at.Add(time.Duration(h) * time.Hour) }

func (f *fixture) evidence() api.Evidence {
	return api.Evidence{SHA256: strings.Repeat("7e", 32), StorageID: "mail/draft-bl.pdf", MediaType: "application/pdf", CapturedAt: f.at}
}

func (f *fixture) open(id, consolidation, booking string) (api.Receipt, error) {
	return f.module.OpenOceanMovement(context.Background(), api.OpenOceanMovement{CommandID: "open-" + id, TenantID: f.tenant, ActorID: "ops-1", MovementID: id,
		ConsolidationID: consolidation, BookingID: booking, OccurredAt: f.at})
}

func (f *fixture) series(t *testing.T) {
	t.Helper()
	// templates whose numbers are no HBL numbers, or of the platform's tokens Cargo does not take, are refused
	for _, template := range []string{"SH-{seq:7}", "SHANGHAILCL{seq:6}", "SHA{rand:4}{seq:6}", "sha{seq:7}", "{seq:7}", "SHA{yy}{seq:6}"} {
		if _, err := f.module.RegisterHblSeries(context.Background(), api.RegisterHblSeries{CommandID: "series-" + template, TenantID: f.tenant,
			ActorID: "ops-1", LegalEntityID: "LE", Template: template, FirstNumber: 1, OccurredAt: f.at}); !errors.Is(err, api.ErrRejected) {
			t.Fatalf("template %s: %v", template, err)
		}
	}
	if _, err := f.module.RegisterHblSeries(context.Background(), api.RegisterHblSeries{CommandID: "series", TenantID: f.tenant, ActorID: "ops-1",
		LegalEntityID: "LE", Template: "SHAL{seq:7}", FirstNumber: 1, OccurredAt: f.at}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) associate(command, movement, shipment, hbl string, revision int64) (api.Receipt, error) {
	return f.associateAt(command, movement, shipment, hbl, revision, 1)
}

func (f *fixture) associateAt(command, movement, shipment, hbl string, revision int64, hours int) (api.Receipt, error) {
	return f.module.AssociateHouseBill(context.Background(), api.AssociateHouseBill{CommandID: command, TenantID: f.tenant, ActorID: "ops-1",
		MovementID: movement, ExpectedRevision: revision, ShipmentID: shipment, HBL: hbl, OccurredAt: f.hours(hours)})
}

func (f *fixture) masterBill(command, movement, number, kind string, revision int64, hours int) (api.Receipt, error) {
	return f.module.RecordMasterBill(context.Background(), api.RecordMasterBill{CommandID: command, TenantID: f.tenant, ActorID: "ops-1",
		MovementID: movement, ExpectedRevision: revision, Number: number, Kind: kind, Evidence: f.evidence(), OccurredAt: f.hours(hours)})
}

func (f *fixture) get(t *testing.T, id string) api.OceanMovement {
	t.Helper()
	m, err := f.module.GetOceanMovement(context.Background(), api.GetOceanMovement{TenantID: f.tenant, MovementID: id})
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

// refusal is the database's message with the constraint it names, which a RAISE states apart from its message.
func refusal(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Message + " [" + pgErr.ConstraintName + "]"
	}
	return ""
}

func TestOceanMovementOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	etd := f.hours(72)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, etd)
	f.w.booking("BK-2", "C-2", "MSC OSCAR", "", "FA448W", 1, etd.Add(7*24*time.Hour))
	if _, err := f.open("OM-X", "C-1", "BK-404"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an unknown booking: %v", err)
	}
	if _, err := f.open("OM-X", "C-9", "BK-1"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a booking for another consolidation: %v", err)
	}
	if _, err := f.module.OpenOceanMovement(ctx, api.OpenOceanMovement{CommandID: "open-intruder", TenantID: f.tenant, ActorID: "intruder", MovementID: "OM-X",
		ConsolidationID: "C-1", BookingID: "BK-1", OccurredAt: f.at}); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("an actor without the right: %v", err)
	}
	r, err := f.open("OM-1", "C-1", "BK-1")
	if err != nil || r.NewRevision != 1 {
		t.Fatalf("open: %+v %v", r, err)
	}
	if again, err := f.open("OM-1", "C-1", "BK-1"); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err = f.module.OpenOceanMovement(ctx, api.OpenOceanMovement{CommandID: "open-OM-1", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ConsolidationID: "C-1", BookingID: "BK-2", OccurredAt: f.at}); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("a command id with another payload: %v", err)
	}
	if _, err = f.module.OpenOceanMovement(ctx, api.OpenOceanMovement{CommandID: "open-twice", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1B",
		ConsolidationID: "C-1", BookingID: "BK-1", OccurredAt: f.at}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a second movement for one consolidation: %v", err)
	}
	m := f.get(t, "OM-1")
	if m.Kind != api.KindSeaLCLOwn || m.CarrierPartyID != "line-1" || m.CarrierRole != api.RoleCarrier || m.Origin != unlocode("CNSHG") ||
		m.Destination != unlocode("DEHAM") || m.LocationEdition != "TEST-EDITION" || m.State != api.StateOpen || len(m.Legs) != 1 ||
		m.Legs[0].Vessel != "EVER GIVEN" || m.Legs[0].VesselIMO != "9811000" || m.Legs[0].Voyage != "0123W" || !m.Legs[0].ETD.Equal(etd) ||
		len(m.Legs[0].CutOffs) != 2 || m.Legs[0].CutOffs[0] != (api.CutOff{Kind: "VGM", At: etd.Add(-48 * time.Hour), TimeZone: "Asia/Shanghai"}) ||
		m.Legs[0].CutOffs[1].Location != unlocode("CNSHG") || m.MasterBillNumber != "" || len(m.MasterBills) != 0 {
		t.Fatalf("opened: %+v", m)
	}
	if _, err = f.module.GetOceanMovement(ctx, api.GetOceanMovement{TenantID: f.tenant + "-other", MovementID: "OM-1"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("another tenant reads the movement: %v", err)
	}

	// Houses: an HBL drawn from the series, one given, and neither twice.
	f.w.plan("SH-1", "C-1", 2)
	f.w.plan("SH-2", "C-1", 2)
	f.w.plan("SH-3", "C-1", 2)
	if _, err = f.associate("assoc-1-noseries", "OM-1", "SH-1", "", 1); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("draw without a series: %v", err)
	}
	f.series(t)
	if _, err = f.associate("assoc-1-stale", "OM-1", "SH-1", "", 7); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a stale revision: %v", err)
	}
	r1, err := f.associate("assoc-1", "OM-1", "SH-1", "", 1)
	if err != nil || r1.HBL != "SHAL0000001" || r1.EntryNo != 1 || r1.NewRevision != 2 {
		t.Fatalf("associate SH-1: %+v %v", r1, err)
	}
	if _, err = f.associate("assoc-2", "OM-1", "SH-2", "MANUAL01", 2); err != nil {
		t.Fatal(err)
	}
	if _, err = f.associate("assoc-3", "OM-1", "SH-3", "SHAL0000001", 3); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("another shipment's HBL: %v", err)
	}
	f.w.mu.Lock()
	fcl := f.w.plans["SH-3"]
	fcl.ContainerMode = fulapi.ContainerFCL
	f.w.plans["SH-3"] = fcl
	f.w.mu.Unlock()
	if _, err = f.associate("assoc-3-fcl", "OM-1", "SH-3", "", 3); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an FCL house in an LCL movement: %v", err)
	}
	m = f.get(t, "OM-1")
	if len(m.Houses) != 2 || m.Packs != 12 || m.GrossWeight != (kernel.Quantity{Unit: "GRM", Amount: 600_000}) ||
		m.Volume != (kernel.Quantity{Unit: "CMQ", Amount: 4_800_000}) || m.Houses[0].Basis != api.BasisPlanned || m.Houses[0].ContainerMode != "LCL" ||
		m.Houses[0].Parts[0].OrderID != "O-SH-1" || m.Houses[0].Parts[0].GoodsDescription != "Furniture parts in cartons" || m.Houses[0].WindowDrift ||
		len(m.Houses[0].Containers) != 0 {
		t.Fatalf("movement: %+v", m)
	}
	ref, err := f.module.FindHouseBill(ctx, api.FindHouseBill{TenantID: f.tenant, LegalEntityID: "LE", HBL: "MANUAL01"})
	if err != nil || ref.ShipmentID != "SH-2" || ref.MovementID != "OM-1" || ref.EntryNo != 2 {
		t.Fatalf("by HBL: %+v %v", ref, err)
	}
	if _, err = f.module.FindHouseBill(ctx, api.FindHouseBill{TenantID: f.tenant, LegalEntityID: "LE", HBL: "NOSUCH1"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown HBL: %v", err)
	}

	// The master bill reference as the line states it, normalized; a number is current on one movement per issuer.
	if _, err = f.masterBill("mbl-coload", "OM-1", "MAEU123456789", api.MasterBillCoLoad, 3, 2); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a co-load bill on an own container: %v", err)
	}
	rb, err := f.masterBill("mbl-1", "OM-1", "  maeu123456789 ", api.MasterBillCarrierMBL, 3, 2)
	if err != nil || rb.BillNo != 1 || rb.NewRevision != 4 {
		t.Fatalf("master bill: %+v %v", rb, err)
	}
	if again, err := f.masterBill("mbl-1", "OM-1", "  maeu123456789 ", api.MasterBillCarrierMBL, 3, 2); err != nil || again != rb {
		t.Fatalf("replay: %+v %v", again, err)
	}
	byBill, err := f.module.FindOceanMovementByMasterBill(ctx, api.FindOceanMovementByMasterBill{TenantID: f.tenant, IssuerPartyID: "line-1", Number: "maeu123456789"})
	if err != nil || byBill.MovementID != "OM-1" || byBill.MasterBillNumber != "MAEU123456789" || byBill.MasterBillKind != api.MasterBillCarrierMBL {
		t.Fatalf("by master bill: %+v %v", byBill, err)
	}
	if _, err = f.module.FindOceanMovementByMasterBill(ctx, api.FindOceanMovementByMasterBill{TenantID: f.tenant, IssuerPartyID: "line-2",
		Number: "MAEU123456789"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("another issuer's number: %v", err)
	}
	if _, err = f.open("OM-2", "C-2", "BK-2"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.masterBill("mbl-2-taken", "OM-2", "MAEU123456789", api.MasterBillCarrierMBL, 1, 2); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "another movement") {
		t.Fatalf("a number current on another movement: %v", err)
	}
	if rb, err = f.masterBill("mbl-1b", "OM-1", "MAEU987654321", api.MasterBillCarrierSeaWaybill, 4, 3); err != nil || rb.BillNo != 2 {
		t.Fatalf("a further number: %+v %v", rb, err)
	}
	if _, err = f.masterBill("mbl-2", "OM-2", "MAEU123456789", api.MasterBillCarrierMBL, 1, 3); err != nil {
		t.Fatalf("a superseded number is free again: %v", err)
	}
	m = f.get(t, "OM-1")
	if m.MasterBillNumber != "MAEU987654321" || m.MasterBillKind != api.MasterBillCarrierSeaWaybill || len(m.MasterBills) != 2 ||
		m.MasterBills[0].Status != api.MasterBillCurrent || m.MasterBills[1].Status != api.MasterBillSuperseded || m.MasterBills[1].Number != "MAEU123456789" ||
		m.MasterBills[0].Evidence != f.evidence() || m.MasterBills[0].RecordedBy != "ops-1" || !m.MasterBills[0].RecordedAt.Equal(f.hours(3)) {
		t.Fatalf("master bills: %+v", m.MasterBills)
	}

	// The line rolls the container to the next vessel under the same booking; the houses stay and drift outside
	// their windows.
	f.w.booking("BK-1", "C-1", "MSC OSCAR", "", "FA449W", 2, f.hours(130))
	if _, err = f.module.LinkCarrierBooking(ctx, api.LinkCarrierBooking{CommandID: "link", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 5, OccurredAt: f.hours(4)}); err != nil {
		t.Fatal(err)
	}
	m = f.get(t, "OM-1")
	if m.CapacityVersion != 2 || len(m.Legs) != 2 || !m.Legs[0].Current || m.Legs[0].Vessel != "MSC OSCAR" || m.Legs[0].VesselIMO != "" || m.Legs[1].Current ||
		m.Legs[1].Vessel != "EVER GIVEN" || len(m.Legs[1].CutOffs) != 2 || !m.Houses[0].WindowDrift || m.MasterBillNumber != "MAEU987654321" {
		t.Fatalf("rolled: %+v", m.Legs)
	}

	// SH-1 is stuffed and sealed; FUL relays the container, and the refreshed entry keeps it with the PHY evidence.
	// SH-2 moves to C-2 and is associated there with its HBL.
	f.w.plan("SH-1", "C-1", 3)
	container := f.w.stuffed("SH-1", 2, "CN-SEAL-77120")
	rr, err := f.module.RefreshHouseBillFacts(ctx, api.RefreshHouseBillFacts{CommandID: "refresh", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "OM-1", ExpectedRevision: 6, ShipmentID: "SH-1", OccurredAt: f.hours(5)})
	if err != nil || rr.HBL != "SHAL0000001" || rr.EntryNo != 3 {
		t.Fatalf("refresh: %+v %v", rr, err)
	}
	if _, err = f.module.RefreshHouseBillFacts(ctx, api.RefreshHouseBillFacts{CommandID: "refresh-again", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "OM-1", ExpectedRevision: 7, ShipmentID: "SH-1", OccurredAt: f.hours(5)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("nothing to refresh: %v", err)
	}
	m = f.get(t, "OM-1")
	h := m.Houses[len(m.Houses)-1]
	want := api.HouseContainer{ExecutionID: container.ExecutionID, LoadUnitNo: 1, TypeCode: "22G1", Packs: 6, GrossWeight: kg(318), LineNos: []int{1, 3},
		Seal: "CN-SEAL-77120", SealSHA256: container.SealSHA256, SealedAt: container.SealedAt}
	if h.ShipmentID != "SH-1" || h.EntryNo != 3 || h.Basis != api.BasisTendered || h.EvidenceVersion != 2 || h.PlanVersion != 3 || h.GrossWeight != kg(318) ||
		h.Volume != (kernel.Quantity{}) || h.Start != api.EntryRefreshed || len(h.Containers) != 1 || !reflect.DeepEqual(h.Containers[0], want) {
		t.Fatalf("the stuffed house: %+v", h)
	}
	if m.GrossWeight != (kernel.Quantity{Unit: "GRM", Amount: 618_000}) || m.Volume != (kernel.Quantity{Unit: "CMQ", Amount: 2_400_000}) {
		t.Fatalf("totals: %+v %+v", m.GrossWeight, m.Volume)
	}
	f.w.plan("SH-2", "C-2", 2)
	end, err := f.module.EndHouseBillAssociation(ctx, api.EndHouseBillAssociation{CommandID: "end-2", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "OM-1", ExpectedRevision: 7, ShipmentID: "SH-2", OccurredAt: f.hours(6)})
	if err != nil || end.HBL != "MANUAL01" || end.EntryNo != 2 {
		t.Fatalf("end SH-2: %+v %v", end, err)
	}
	if ref, err = f.module.FindHouseBill(ctx, api.FindHouseBill{TenantID: f.tenant, LegalEntityID: "LE", HBL: "MANUAL01"}); err != nil ||
		ref.ShipmentID != "SH-2" || ref.MovementID != "" {
		t.Fatalf("an HBL between movements: %+v %v", ref, err)
	}
	if _, err = f.associateAt("assoc-2-again", "OM-2", "SH-2", "OTHER1", 2, 6); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "MANUAL01") {
		t.Fatalf("another number for a shipment that has one: %v", err)
	}
	if _, err = f.associateAt("assoc-2-early", "OM-2", "SH-2", "", 2, 1); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a business time before the movement's last change: %v", err)
	}
	if moved, err := f.associateAt("assoc-2-moved", "OM-2", "SH-2", "", 2, 6); err != nil || moved.HBL != "MANUAL01" || moved.NewRevision != 3 {
		t.Fatalf("the HBL moves with the shipment: %+v %v", moved, err)
	}
	if _, err = f.module.CancelOceanMovement(ctx, api.CancelOceanMovement{CommandID: "cancel-busy", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 8, Reason: "x", OccurredAt: f.hours(7)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("cancel with a house: %v", err)
	}
	f.w.mu.Lock()
	p := f.w.plans["SH-1"]
	p.Status, p.ConsolidationID = fulapi.StatusCancelled, ""
	f.w.plans["SH-1"] = p
	f.w.mu.Unlock()
	if _, err = f.module.EndHouseBillAssociation(ctx, api.EndHouseBillAssociation{CommandID: "end-1", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "OM-1", ExpectedRevision: 8, ShipmentID: "SH-1", OccurredAt: f.hours(7)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.CancelOceanMovement(ctx, api.CancelOceanMovement{CommandID: "cancel", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 9, Reason: "consolidation cancelled", OccurredAt: f.hours(8)}); err != nil {
		t.Fatal(err)
	}
	if m = f.get(t, "OM-1"); m.State != api.StateCancelled || m.MasterBillNumber != "" || m.MasterBills[0].Status != api.MasterBillVoided ||
		m.MasterBills[1].Status != api.MasterBillSuperseded || len(m.Houses) != 0 || m.Revision != 10 {
		t.Fatalf("cancelled: %+v", m)
	}
	if _, err = f.module.FindOceanMovementByMasterBill(ctx, api.FindOceanMovementByMasterBill{TenantID: f.tenant, IssuerPartyID: "line-1",
		Number: "MAEU987654321"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a voided master bill: %v", err)
	}
	if _, err = f.masterBill("mbl-cancelled", "OM-1", "MAEU555", api.MasterBillCarrierMBL, 10, 9); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a master bill on a cancelled movement: %v", err)
	}
	// a cancelled movement frees its consolidation for another
	f.w.booking("BK-1C", "C-1", "COSCO STAR", "", "011W", 1, etd.Add(14*24*time.Hour))
	if _, err = f.open("OM-1C", "C-1", "BK-1C"); err != nil {
		t.Fatalf("a new movement after the cancelled one: %v", err)
	}
	if s, err := f.module.GetHblSeries(ctx, api.GetHblSeries{TenantID: f.tenant, LegalEntityID: "LE"}); err != nil || s.Template != "SHAL{seq:7}" ||
		len(s.Last) != 1 || s.Last[0] != (api.NumberPeriod{Period: "", Last: 1}) {
		t.Fatalf("series: %+v %v", s, err)
	}
	if _, err = f.module.GetHblSeries(ctx, api.GetHblSeries{TenantID: f.tenant, LegalEntityID: "LE-2"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("no series: %v", err)
	}
	wantEvents := []string{api.OceanMovementOpened, api.VoyageLegsLinked, api.HblSeriesRegistered, api.HouseBillAssociated, api.HouseBillAssociated,
		api.MasterBillRecorded, api.OceanMovementOpened, api.VoyageLegsLinked, api.MasterBillRecorded, api.MasterBillRecorded, api.VoyageLegsLinked,
		api.HouseBillFactsRefreshed, api.HouseBillAssociationEnded, api.HouseBillAssociated, api.HouseBillAssociationEnded, api.OceanMovementCancelled,
		api.OceanMovementOpened, api.VoyageLegsLinked}
	if got := f.outboxTypes(t); !slices.Equal(got, wantEvents) {
		t.Fatalf("outbox %v", got)
	}

	// Read on a session in another time zone: the same movement.
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
	for _, id := range []string{"OM-1", "OM-2"} {
		there, err := shanghai.GetOceanMovement(ctx, api.GetOceanMovement{TenantID: f.tenant, MovementID: id})
		if err != nil || !reflect.DeepEqual(there, f.get(t, id)) {
			t.Fatalf("%s read in Asia/Shanghai: %+v %v", id, there, err)
		}
	}
}

// TestCoLoadMovementOnPostgres opens the movement of a co-load: the co-loader is its carrier and only the co-loader's
// bill is its master bill reference.
func TestCoLoadMovementOnPostgres(t *testing.T) {
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	f.w.mu.Lock()
	b := f.w.bookings["BK-1"]
	b.Kind, b.CarrierPartyID, b.CarrierRole = bookapi.KindSeaLCLCoLoad, "coloader-x", "CO_LOADER"
	f.w.bookings["BK-1"] = b
	air := b
	air.Kind, air.CarrierRole, air.ConsumerID = bookapi.KindAirConsol, "CARRIER", "C-2"
	f.w.bookings["BK-AIR"] = air
	mixed := b
	mixed.CarrierRole, mixed.ConsumerID = "CARRIER", "C-3"
	f.w.bookings["BK-MIXED"] = mixed
	f.w.mu.Unlock()
	if _, err := f.open("OM-AIR", "C-2", "BK-AIR"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an air booking: %v", err)
	}
	if _, err := f.open("OM-MIXED", "C-3", "BK-MIXED"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a co-load of a CARRIER: %v", err)
	}
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.masterBill("mbl", "OM-1", "MAEU123456789", api.MasterBillCarrierMBL, 1, 1); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a carrier's bill on a co-load: %v", err)
	}
	if _, err := f.masterBill("coload-bill", "OM-1", "sha-ham/2611-0042", api.MasterBillCoLoad, 1, 1); err != nil {
		t.Fatal(err)
	}
	if m := f.get(t, "OM-1"); m.Kind != api.KindSeaLCLCoLoad || m.CarrierRole != api.RoleCoLoader || m.CarrierPartyID != "coloader-x" ||
		m.MasterBillNumber != "SHA-HAM/2611-0042" || m.MasterBillKind != api.MasterBillCoLoad {
		t.Fatalf("co-load: %+v", m)
	}
}

// TestDatabaseRules writes past SEA on the application role.
func TestDatabaseRules(t *testing.T) {
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
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
	nextBill := `WITH s AS (UPDATE cargo_sea.master_bill SET status='SUPERSEDED' WHERE tenant_id=$1 AND movement_id='OM-1' AND status='CURRENT' RETURNING bill_no)
        INSERT INTO cargo_sea.master_bill (tenant_id,movement_id,bill_no,bill_number,kind,issuer_party_id,evidence_sha256,evidence_storage_id,
        evidence_media_type,evidence_captured_at,status,recorded_by,recorded_at,entered_at)
        SELECT $1,'OM-1',bill_no+1,'MAEU3','%s','%s',repeat('a',64),'s','m',now(),'CURRENT','x',now(),now() FROM s`
	for name, tc := range map[string]struct {
		sql   string
		state string
		in    string // part of the error text, such as the constraint
	}{
		"leg immutable":      {`UPDATE cargo_sea.voyage_leg SET vessel='OTHER' WHERE tenant_id=$1`, "42501|23001", ""},
		"cut-off immutable":  {`UPDATE cargo_sea.voyage_cut_off SET cut_off_at=now() WHERE tenant_id=$1`, "42501|23001", ""},
		"cut-off deleted":    {`DELETE FROM cargo_sea.voyage_cut_off WHERE tenant_id=$1`, "42501|23001", ""},
		"register immutable": {`UPDATE cargo_sea.hbl_number SET shipment_id='SH-9' WHERE tenant_id=$1`, "42501|23001", ""},
		"house facts":        {`UPDATE cargo_sea.house_bill SET packs=1 WHERE tenant_id=$1`, "42501|23001", ""},
		"container facts":    {`UPDATE cargo_sea.house_bill_stuffing SET seal='OTHER' WHERE tenant_id=$1`, "42501|23001", ""},
		"series fixed":       {`UPDATE cargo_sea.hbl_series SET first_number=2 WHERE tenant_id=$1`, "42501|23001", ""},
		"sequence backwards": {`UPDATE cargo_sea.numbering_sequence SET next_val=0 WHERE scope=$1`, "23001", "one number at a time"},
		"sequence deleted":   {`DELETE FROM cargo_sea.numbering_sequence WHERE scope=$1`, "42501|23001", ""},
		"sequence of no series": {`INSERT INTO cargo_sea.numbering_sequence (scope,doc_type,period,next_val) VALUES ($1,'HBL/LE-NONE','',1)`, "23001",
			"starts at 1 or at its series"},
		"carrier fixed":            {`UPDATE cargo_sea.ocean_movement SET carrier_party_id='line-2' WHERE tenant_id=$1`, "42501|23001", ""},
		"movement deleted":         {`DELETE FROM cargo_sea.ocean_movement WHERE tenant_id=$1`, "42501|23001", ""},
		"master bill renumbered":   {`UPDATE cargo_sea.master_bill SET bill_number='MAEU9' WHERE tenant_id=$1`, "42501|23001", ""},
		"master bill made current": {`UPDATE cargo_sea.master_bill SET status='CURRENT' WHERE tenant_id=$1 AND status='SUPERSEDED'`, "23001|23505", ""},
		"a second current master bill": {`INSERT INTO cargo_sea.master_bill (tenant_id,movement_id,bill_no,bill_number,kind,issuer_party_id,evidence_sha256,
            evidence_storage_id,evidence_media_type,evidence_captured_at,status,recorded_by,recorded_at,entered_at)
            VALUES ($1,'OM-1',3,'MAEU3','CARRIER_MBL','line-1',repeat('a',64),'s','m',now(),'CURRENT','x',now(),now())`, "23505", "master_bill_one_current"},
		"a master bill out of number": {`INSERT INTO cargo_sea.master_bill (tenant_id,movement_id,bill_no,bill_number,kind,issuer_party_id,evidence_sha256,
            evidence_storage_id,evidence_media_type,evidence_captured_at,status,recorded_by,recorded_at,entered_at)
            VALUES ($1,'OM-1',9,'MAEU3','CARRIER_MBL','line-1',repeat('a',64),'s','m',now(),'SUPERSEDED','x',now(),now())`, "23514", "ocean_movement_consistent"},
		"a master bill of another issuer": {fmt.Sprintf(nextBill, "CARRIER_MBL", "line-2"), "23514", "ocean_movement_consistent"},
		"a co-load bill on a container":   {fmt.Sprintf(nextBill, "COLOAD_BILL", "line-1"), "23514", "ocean_movement_consistent"},
		"the current master bill voided on an open movement": {`UPDATE cargo_sea.master_bill SET status='VOIDED' WHERE tenant_id=$1 AND status='CURRENT'`,
			"23514", "ocean_movement_consistent"},
		"evidence captured after it was recorded": {`INSERT INTO cargo_sea.master_bill (tenant_id,movement_id,bill_no,bill_number,kind,issuer_party_id,
            evidence_sha256,evidence_storage_id,evidence_media_type,evidence_captured_at,status,recorded_by,recorded_at,entered_at)
            VALUES ($1,'OM-1',3,'MAEU3','CARRIER_MBL','line-1',repeat('a',64),'s','m',now(),'SUPERSEDED','x',now() - interval '1 hour',now())`, "23514",
			"master_bill_evidence_time"},
		"unregistered HBL": {`INSERT INTO cargo_sea.house_bill (tenant_id,movement_id,entry_no,shipment_id,hbl,ful_revision,plan_version,window_from,packs,
            gross_weight_unit,gross_weight,basis,start_kind,associated_by,associated_at,entered_at)
            VALUES ($1,'OM-1',2,'SH-9','NOPE1',1,1,now(),1,'KGM',1,'PLANNED','ASSOCIATED','x',now(),now())`, "23514", "house_bill_consistent"},
		"measures without evidence": {`INSERT INTO cargo_sea.house_bill (tenant_id,movement_id,entry_no,shipment_id,hbl,ful_revision,plan_version,window_from,
            packs,gross_weight_unit,gross_weight,basis,start_kind,associated_by,associated_at,entered_at)
            VALUES ($1,'OM-1',2,'SH-9','NOPE1',1,1,now(),1,'KGM',1,'TENDERED','ASSOCIATED','x',now(),now())`, "23514", "house_bill_measured"},
		"an FCL house": {`INSERT INTO cargo_sea.house_bill (tenant_id,movement_id,entry_no,shipment_id,hbl,ful_revision,plan_version,container_mode,
            window_from,packs,gross_weight_unit,gross_weight,basis,start_kind,associated_by,associated_at,entered_at)
            VALUES ($1,'OM-1',2,'SH-9','NOPE1',1,1,'FCL',now(),1,'KGM',1,'PLANNED','ASSOCIATED','x',now(),now())`, "23514", "container_mode"},
		"leg added later": {`INSERT INTO cargo_sea.voyage_leg (tenant_id,movement_id,link_no,leg_no,vessel,voyage,from_code,to_code,etd)
            VALUES ($1,'OM-1',1,2,'LATER','1','DEHAM','NLRTM',now())`, "23514", "voyage_with_link"},
		"a wrong IMO check digit": {`INSERT INTO cargo_sea.voyage_leg (tenant_id,movement_id,link_no,leg_no,vessel,vessel_imo,voyage,from_code,to_code,etd)
            VALUES ($1,'OM-1',1,2,'LATER','9811001','1','DEHAM','NLRTM',now())`, "23514", "voyage_leg_imo"},
		"an IMO number of letters": {`INSERT INTO cargo_sea.voyage_leg (tenant_id,movement_id,link_no,leg_no,vessel,vessel_imo,voyage,from_code,to_code,etd)
            VALUES ($1,'OM-1',1,2,'LATER','IMO9811','1','DEHAM','NLRTM',now())`, "23514", "voyage_leg_imo"},
		"cut-off added later": {`INSERT INTO cargo_sea.voyage_cut_off (tenant_id,movement_id,link_no,leg_no,cut_off_no,kind,cut_off_at,time_zone)
            VALUES ($1,'OM-1',1,1,9,'HAZARDOUS',now(),'Asia/Shanghai')`, "23514", "voyage_with_link"},
		"an AIR cut-off": {`INSERT INTO cargo_sea.voyage_cut_off (tenant_id,movement_id,link_no,leg_no,cut_off_no,kind,cut_off_at,time_zone)
            VALUES ($1,'OM-1',1,1,9,'CARGO_ACCEPTANCE',now(),'Asia/Shanghai')`, "23514", "kind"},
		"part added later": {`INSERT INTO cargo_sea.house_bill_part (tenant_id,movement_id,entry_no,part_no,obligation_id,obligation_version,order_id,packs,
            package_type,gross_weight_unit,gross_weight) VALUES ($1,'OM-1',1,9,'OB-X',1,'O-X',1,'CT','KGM',1)`, "23514", "house_bill_facts_with_entry"},
		"container added later": {`INSERT INTO cargo_sea.house_bill_stuffing (tenant_id,movement_id,entry_no,stuffing_no,execution_id,load_unit_no,type_code,
            packs,line_nos,seal,sealed_at) VALUES ($1,'OM-1',1,9,'X-9',2,'42G1',1,'{1}','S-9',now())`, "23514", "house_bill_facts_with_entry"},
		"a container without load lines": {`INSERT INTO cargo_sea.house_bill_stuffing (tenant_id,movement_id,entry_no,stuffing_no,execution_id,load_unit_no,
            type_code,packs,line_nos,seal,sealed_at) VALUES ($1,'OM-1',1,9,'X-9',2,'42G1',1,'{}','S-9',now())`, "23514", "line_nos"},
		"cancelled with a house": {`UPDATE cargo_sea.ocean_movement SET state='CANCELLED',cancel_reason='x',cancelled_by='x',cancelled_at=now()
            WHERE tenant_id=$1 AND movement_id='OM-1'`, "23514", "ocean_movement_consistent"},
		"a co-loader on an own container": {`INSERT INTO cargo_sea.ocean_movement (tenant_id,movement_id,consolidation_id,kind,operating_legal_entity_id,
            carrier_party_id,carrier_party_revision,carrier_role,origin_code,destination_code,location_edition,state,booking_id)
            VALUES ($1,'OM-9','C-9','SEA_LCL_OWN','LE','x',1,'CO_LOADER','CNSHG','DEHAM','E1','OPEN','BK-9')`, "23514", "ocean_movement_carrier_role"},
		"a movement without its booking link": {`INSERT INTO cargo_sea.ocean_movement (tenant_id,movement_id,consolidation_id,kind,operating_legal_entity_id,
            carrier_party_id,carrier_party_revision,carrier_role,origin_code,destination_code,location_edition,state,booking_id)
            VALUES ($1,'OM-9','C-9','SEA_LCL_OWN','LE','x',1,'CARRIER','CNSHG','DEHAM','E1','OPEN','BK-9')`, "23514", "ocean_movement_consistent"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		if state := sqlState(err); state == "" || !strings.Contains(tc.state, state) || !strings.Contains(refusal(err), tc.in) {
			t.Errorf("%s: %v (state %s, %s), want %s %s", name, err, state, refusal(err), tc.state, tc.in)
		}
		t.Logf("%s refused: %v", name, err)
	}
	// nothing above changed the movement
	if m := f.get(t, "OM-1"); m.Revision != 4 || m.State != api.StateOpen || len(m.Houses) != 1 || len(m.Houses[0].Containers) != 1 ||
		m.MasterBillNumber != "MAEU2" || len(m.MasterBills) != 2 || len(m.Legs) != 1 || len(m.Legs[0].CutOffs) != 2 {
		t.Fatalf("the movement after the refused writes: %+v", m)
	}
	// another tenant sees and changes nothing of it
	other := scope.TenantTransactor{DB: f.db, Tenant: f.tenant + "-other"}
	if err := other.WithinTransaction(context.Background(), func(txCtx context.Context) error {
		e, err := f.db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		var movements, bills, houses int
		if err = e.QueryRow(txCtx, `SELECT (SELECT count(*) FROM cargo_sea.ocean_movement), (SELECT count(*) FROM cargo_sea.master_bill),
            (SELECT count(*) FROM cargo_sea.house_bill)`).Scan(&movements, &bills, &houses); err != nil {
			return err
		}
		if movements != 0 || bills != 0 || houses != 0 {
			return fmt.Errorf("another tenant reads %d movements, %d master bills, %d house bills", movements, bills, houses)
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

// TestHouseBillsDrawConcurrently associates eight houses with eight movements at once, all drawing from one legal
// entity's series: every one commits with its own number, in order, with none lost.
func TestHouseBillsDrawConcurrently(t *testing.T) {
	f := newFixture(t)
	f.series(t)
	const workers = 8
	for i := range workers {
		c := fmt.Sprintf("C-%d", i)
		f.w.booking("BK-"+c, c, "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
		if _, err := f.open("OM-"+c, c, "BK-"+c); err != nil {
			t.Fatal(err)
		}
		f.w.plan("SH-"+c, c, 2)
	}
	hbls := make([]string, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := fmt.Sprintf("C-%d", i)
			<-start
			r, err := f.associate("assoc-"+c, "OM-"+c, "SH-"+c, "", 1)
			hbls[i], errs[i] = r.HBL, err
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("house %d: %v", i, err)
		}
	}
	slices.Sort(hbls)
	for i, h := range hbls {
		if want := fmt.Sprintf("SHAL%07d", i+1); h != want {
			t.Fatalf("numbers %v", hbls)
		}
	}
	if s, err := f.module.GetHblSeries(context.Background(), api.GetHblSeries{TenantID: f.tenant, LegalEntityID: "LE"}); err != nil || len(s.Last) != 1 ||
		s.Last[0].Last != workers {
		t.Fatalf("series: %+v %v", s, err)
	}
}

// TestMovementsRaceForOneConsolidation opens eight movements for one consolidation and booking at once: one wins.
func TestMovementsRaceForOneConsolidation(t *testing.T) {
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	const workers = 8
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.open(fmt.Sprintf("OM-%d", i), "C-1", "BK-1")
		}()
	}
	close(start)
	wg.Wait()
	committed, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, api.ErrConflict):
			refused++
		}
	}
	if committed != 1 || refused != workers-1 {
		t.Fatalf("%d committed, %d refused: %v", committed, refused, errs)
	}
}

// TestMovementsRaceForOneMasterBillNumber records one number of one issuer on eight movements at once: it is current
// on one.
func TestMovementsRaceForOneMasterBillNumber(t *testing.T) {
	f := newFixture(t)
	const workers = 8
	for i := range workers {
		c := fmt.Sprintf("C-%d", i)
		f.w.booking("BK-"+c, c, "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
		if _, err := f.open("OM-"+c, c, "BK-"+c); err != nil {
			t.Fatal(err)
		}
	}
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.masterBill(fmt.Sprintf("mbl-%d", i), fmt.Sprintf("OM-C-%d", i), "MAEU123456789", api.MasterBillCarrierMBL, 1, 1)
		}()
	}
	close(start)
	wg.Wait()
	committed, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, api.ErrRejected) && strings.Contains(err.Error(), "another movement"):
			refused++
		}
	}
	if committed != 1 || refused != workers-1 {
		t.Fatalf("%d committed, %d refused: %v", committed, refused, errs)
	}
	holder, err := f.module.FindOceanMovementByMasterBill(context.Background(), api.FindOceanMovementByMasterBill{TenantID: f.tenant, IssuerPartyID: "line-1",
		Number: "MAEU123456789"})
	if err != nil || holder.MasterBillNumber != "MAEU123456789" || holder.Revision != 2 {
		t.Fatalf("the holder: %+v %v", holder, err)
	}
}
