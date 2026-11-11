//go:build integration

// Package integration runs BOOK against PostgreSQL 16 with stand-ins for ORG and the location reference data.
// The real ORG and BOOK together are tested by the composition root.
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
	bookmodule "go.idp.xyz/idp-cargo/internal/contexts/book"
	"go.idp.xyz/idp-cargo/internal/contexts/book/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
	refapi "go.idp.xyz/idp-cargo/internal/reference/api"
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

// orgStub confirms MU as a CARRIER with its IATA designator and AWB prefix, COSCO as a CARRIER without airline
// codes, and CL as a CO_LOADER; it refuses anything else.
type orgStub struct{}

func (orgStub) ResolvePartyRole(_ context.Context, q orgapi.ResolvePartyRole) (orgapi.PartyRole, error) {
	role := map[string]string{"MU": orgapi.RoleCarrier, "COSCO": orgapi.RoleCarrier, "CL": orgapi.RoleCoLoader}[q.PartyID]
	if role == "" || role != q.RoleCode || q.LegalEntityID != "LE" {
		return orgapi.PartyRole{}, fmt.Errorf("%w: stub", orgapi.ErrRejected)
	}
	answer := orgapi.PartyRole{Ref: orgapi.PartyRef{TenantID: q.TenantID, PartyID: q.PartyID, PartyRevision: 4, IdentitySource: orgapi.IdentitySource},
		RoleCode: role, Validity: kernel.ValidityInterval{From: q.At.Add(-time.Hour)}, EvidenceRef: "contract"}
	if q.PartyID == "MU" {
		answer.Identifiers = []orgapi.PartyIdentifier{{Type: orgapi.IdentifierIATAAirline, Country: "ZZ", Value: "MU"},
			{Type: orgapi.IdentifierIATAAWBPrefix, Country: "ZZ", Value: "781"}}
	}
	return answer, nil
}

// locationStub resolves every well-formed UN/LOCODE to itself, with its last three letters as IATA code for AIR.
type locationStub struct{}

func (locationStub) ResolveLocation(_ context.Context, q refapi.ResolveLocation) (refapi.Location, error) {
	if q.Scheme != refapi.SchemeUNLOCODE || len(q.Code) != 5 {
		return refapi.Location{}, refapi.ErrUnknownLocation
	}
	loc := refapi.Location{UNLocode: q.Code, CountryCode: q.Code[:2], Edition: "TEST-EDITION", Via: refapi.ViaUNLOCODE}
	if q.Mode == refapi.ModeAir {
		loc.IATAAirport = q.Code[2:]
	}
	return loc, nil
}

type allow struct{}

func (allow) AuthorizeBookingAction(_ context.Context, _, actor, _, _ string) error {
	if actor == "intruder" {
		return errors.New("forbidden")
	}
	return nil
}

type fixture struct {
	module *bookmodule.Module
	db     *bentopg.DB
	tenant string
	at     time.Time
	etd    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	at := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	f := &fixture{db: testDB(t), at: at, etd: at.Add(72 * time.Hour), tenant: "cargo-book-" + time.Now().UTC().Format("20060102T150405.000000000")}
	m, err := bookmodule.New(f.db, orgStub{}, allow{}, bookmodule.WithLocations(locationStub{}))
	if err != nil {
		t.Fatal(err)
	}
	f.module = m
	return f
}

func unlocode(code string) kernel.LocationCode {
	return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}
}

var evidence = api.Evidence{SHA256: strings.Repeat("0f", 32), StorageID: "obj/booking.pdf", MediaType: "application/pdf"}

func (f *fixture) evidence(hours float64) api.Evidence {
	e := evidence
	e.CapturedAt = f.at.Add(time.Duration(hours * float64(time.Hour)))
	return e
}

func (f *fixture) answer(hours float64, refs ...api.CarrierReference) api.Answer {
	return api.Answer{Channel: api.ChannelEmail, Evidence: f.evidence(hours), CarrierTime: f.at.Add(time.Duration(hours * float64(time.Hour))), References: refs}
}

func (f *fixture) draft(t *testing.T, id, consolidation string) {
	t.Helper()
	if _, err := f.module.DraftCarrierBooking(context.Background(), api.DraftCarrierBooking{CommandID: "draft-" + id, TenantID: f.tenant, ActorID: "ops-1",
		BookingID: id, Kind: api.KindAirConsol, LegalEntityID: "LE", CarrierPartyID: "MU", ConsolidationID: consolidation,
		Request: api.Request{Legs: []api.RequestedLeg{{Loading: unlocode("CNPVG"), Discharge: unlocode("USJFK"), Flight: "MU587",
			DepartureDate: f.etd.Format(time.DateOnly)}}, Capacity: api.Capacity{Pieces: 20, GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 1000}}},
		OccurredAt: f.at}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.RecordBookingRequestSent(context.Background(), api.RecordBookingRequestSent{CommandID: "sent-" + id, TenantID: f.tenant,
		ActorID: "ops-1", BookingID: id, ExpectedRevision: 1, Channel: api.ChannelCarrierPortal, Evidence: f.evidence(1), OccurredAt: f.at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) lines(pieces int64, etd time.Time) []api.CapacityLine {
	return []api.CapacityLine{{Capacity: api.Capacity{Pieces: pieces, GrossWeight: kernel.Quantity{Unit: "KGM", Amount: pieces * 50}},
		Legs: []api.ConfirmedLeg{{Loading: unlocode("CNPVG"), Discharge: unlocode("USJFK"), Flight: "MU587", ETD: etd, ETA: etd.Add(14 * time.Hour),
			CutOffs: []api.CutOff{{Kind: api.CutOffCargoAcceptance, At: etd.Add(-6 * time.Hour), TimeZone: "Asia/Shanghai"}}}}}}
}

func (f *fixture) confirm(id, mawb string, revision int64) api.RecordCarrierConfirmation {
	return api.RecordCarrierConfirmation{CommandID: "confirm-" + id, TenantID: f.tenant, ActorID: "ops-2", BookingID: id, ExpectedRevision: revision,
		Answer: f.answer(2, api.CarrierReference{Type: api.ReferenceMAWBNumber, Value: mawb}), Lines: f.lines(20, f.etd), OccurredAt: f.at.Add(2 * time.Hour)}
}

func (f *fixture) get(t *testing.T, id string) api.CarrierBooking {
	t.Helper()
	b, err := f.module.GetCarrierBooking(context.Background(), api.GetCarrierBooking{TenantID: f.tenant, BookingID: id})
	if err != nil {
		t.Fatalf("booking %s: %v", id, err)
	}
	return b
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

func TestAirBookingOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.module.DraftCarrierBooking(ctx, api.DraftCarrierBooking{CommandID: "draft-cosco", TenantID: f.tenant, ActorID: "ops-1", BookingID: "B-X",
		Kind: api.KindAirConsol, LegalEntityID: "LE", CarrierPartyID: "COSCO", ConsolidationID: "C-1", Request: api.Request{Legs: []api.RequestedLeg{{
			Loading: unlocode("CNPVG"), Discharge: unlocode("USJFK"), DepartureDate: "2026-11-20"}}, Capacity: api.Capacity{Pieces: 1,
			GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 1}}}, OccurredAt: f.at}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an AIR booking with a carrier without airline codes: %v", err)
	}
	if _, err := f.module.DraftCarrierBooking(ctx, api.DraftCarrierBooking{CommandID: "draft-unknown", TenantID: f.tenant, ActorID: "ops-1", BookingID: "B-X",
		Kind: api.KindSeaLCLCoLoad, LegalEntityID: "LE", CarrierPartyID: "COSCO", ConsolidationID: "C-1", Request: api.Request{Legs: []api.RequestedLeg{{
			Loading: unlocode("CNSHG"), Discharge: unlocode("DEHAM"), DepartureDate: "2026-11-20"}}, Capacity: api.Capacity{
			GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 1}, Volume: kernel.Quantity{Unit: "DMQ", Amount: 1}}}, OccurredAt: f.at}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a co-load with a carrier ORG does not know as CO_LOADER: %v", err)
	}
	f.draft(t, "B-1", "C-1")
	if b := f.get(t, "B-1"); b.Status != api.StatusRequested || b.AwaitingCarrierSince.IsZero() || b.CarrierAWBPrefix != "781" || b.Request.Legs[0].Loading.IATA != "PVG" {
		t.Fatalf("sent: %+v", b)
	}
	awaiting, err := f.module.ListBookingsAwaitingCarrier(ctx, api.ListBookingsAwaitingCarrier{TenantID: f.tenant, SentBefore: f.at.Add(90 * time.Minute)})
	if err != nil || len(awaiting) != 1 || awaiting[0].BookingID != "B-1" {
		t.Fatalf("awaiting: %+v %v", awaiting, err)
	}
	bad := f.confirm("B-1", "781-12345674", 2)
	bad.CommandID = "confirm-bad"
	if _, err = f.module.RecordCarrierConfirmation(ctx, bad); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a MAWB with a wrong check digit: %v", err)
	}
	r, err := f.module.RecordCarrierConfirmation(ctx, f.confirm("B-1", "78112345675", 2))
	if err != nil || r.NewRevision != 3 {
		t.Fatalf("confirm: %+v %v", r, err)
	}
	if again, err := f.module.RecordCarrierConfirmation(ctx, f.confirm("B-1", "78112345675", 2)); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	b := f.get(t, "B-1")
	if b.Status != api.StatusConfirmed || b.MAWBNumber != "781-12345675" || b.Capacity.Version != 1 || b.Capacity.Reason != api.CapacityInitial ||
		b.ConsumerID != "C-1" || len(b.Capacity.Lines) != 1 || b.Capacity.Lines[0].Legs[0].CutOffs[0].Location != unlocode("CNPVG") ||
		b.ConfirmationLevel != api.VerificationOperatorEntered || !b.AwaitingCarrierSince.IsZero() {
		t.Fatalf("confirmed: %+v", b)
	}
	found, err := f.module.FindBookingByCarrierReference(ctx, api.FindBookingByCarrierReference{TenantID: f.tenant, CarrierPartyID: "MU",
		Type: api.ReferenceMAWBNumber, Value: "781 1234 5675"})
	if err != nil || found.BookingID != "B-1" {
		t.Fatalf("find by MAWB: %+v %v", found, err)
	}
	later := f.etd.Add(24 * time.Hour)
	if _, err = f.module.RecordCarrierChange(ctx, api.RecordCarrierChange{CommandID: "change", TenantID: f.tenant, ActorID: "ops-1", BookingID: "B-1",
		ExpectedRevision: 3, Reason: api.CapacityScheduleChange, Answer: f.answer(3), Lines: f.lines(20, later), OccurredAt: f.at.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	b = f.get(t, "B-1")
	if b.Capacity.Version != 2 || !slices.Equal(b.SupersededCapacity, []int{1}) || !b.Capacity.Lines[0].Legs[0].ETD.Equal(later) {
		t.Fatalf("schedule change: %+v", b.Capacity)
	}
	consol, err := f.module.ListCapacityForConsumer(ctx, api.ListCapacityForConsumer{TenantID: f.tenant, ConsolidationID: "C-1"})
	if err != nil || len(consol) != 1 || consol[0].Capacity.Version != 2 {
		t.Fatalf("capacity for C-1: %+v %v", consol, err)
	}
	due, err := f.module.ListCutOffsDue(ctx, api.ListCutOffsDue{TenantID: f.tenant, From: f.at, Until: later})
	if err != nil || len(due) != 1 || due[0].CapacityVersion != 2 || due[0].Flight != "MU587" || due[0].CutOff.TimeZone != "Asia/Shanghai" {
		t.Fatalf("cut-offs due: %+v %v", due, err)
	}
	if _, err = f.module.RequestBookingCancellation(ctx, api.RequestBookingCancellation{CommandID: "cancel", TenantID: f.tenant, ActorID: "ops-1",
		BookingID: "B-1", ExpectedRevision: 4, Reason: "consolidation cancelled", Channel: api.ChannelEmail, Evidence: f.evidence(4),
		OccurredAt: f.at.Add(4 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.RecordCancellationConfirmed(ctx, api.RecordCancellationConfirmed{CommandID: "cancelled", TenantID: f.tenant, ActorID: "ops-1",
		BookingID: "B-1", ExpectedRevision: 5, Answer: f.answer(5), OccurredAt: f.at.Add(5 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if b = f.get(t, "B-1"); b.Status != api.StatusCancelled || b.ConsumerID != "" || len(b.References) != 0 {
		t.Fatalf("cancelled: %+v", b)
	}
	if _, err = f.module.FindBookingByCarrierReference(ctx, api.FindBookingByCarrierReference{TenantID: f.tenant, CarrierPartyID: "MU",
		Type: api.ReferenceMAWBNumber, Value: "781-12345675"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a cancelled booking's MAWB is no longer live: %v", err)
	}
	f.draft(t, "B-2", "C-1")
	if _, err = f.module.RecordCarrierConfirmation(ctx, f.confirm("B-2", "781-12345675", 2)); err != nil {
		t.Fatalf("the MAWB of a cancelled booking may be booked again: %v", err)
	}
	want := []string{api.CarrierBookingDrafted, api.BookingRequested, api.BookingConfirmed, api.BookingCapacityAllocated, api.BookingCapacityChanged,
		api.BookingCancellationRequested, api.CarrierBookingCancelled, api.BookingCapacityReleased,
		api.CarrierBookingDrafted, api.BookingRequested, api.BookingConfirmed, api.BookingCapacityAllocated}
	if got := f.outboxTypes(t); !slices.Equal(got, want) {
		t.Fatalf("outbox %v", got)
	}
}

// TestDatabaseRules writes past BOOK on the application role: request versions, answers and capacity are
// immutable, a reference never comes back to life, an ended booking does not change, and the commit check refuses
// a second active allocation.
func TestDatabaseRules(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.draft(t, "B-1", "C-1")
	if _, err := f.module.RecordCarrierConfirmation(ctx, f.confirm("B-1", "781-12345675", 2)); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		sql   string
		state string
	}{
		"request immutable":  {`UPDATE cargo_book.booking_request_version SET reason='x' WHERE tenant_id=$1`, "42501|23001"},
		"response immutable": {`UPDATE cargo_book.carrier_response SET reason_text='x' WHERE tenant_id=$1`, "42501|23001"},
		"capacity immutable": {`UPDATE cargo_book.capacity_leg SET flight='MU588' WHERE tenant_id=$1`, "42501|23001"},
		"no delete":          {`DELETE FROM cargo_book.capacity_allocation WHERE tenant_id=$1`, "42501|23001"},
		"carrier fixed":      {`UPDATE cargo_book.carrier_booking SET carrier_party_id='CA' WHERE tenant_id=$1`, "42501|23001"},
		"second allocation": {`INSERT INTO cargo_book.capacity_allocation (tenant_id,booking_id,allocation_no,version_no,capacity_version,consumer_kind,
            consumer_id,status,reason,written_by,written_at,entered_at) VALUES ($1,'B-1',2,1,1,'CONSOL','C-9','ACTIVE','x','x',now(),now());
            UPDATE cargo_book.carrier_booking SET revision=revision+1 WHERE tenant_id=$1`, "23514"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, stmt := range strings.Split(tc.sql, ";") {
				if _, err := e.Exec(ctx, stmt, f.tenant); err != nil {
					return err
				}
			}
			return nil
		})
		if state := sqlState(err); state == "" || !strings.Contains(tc.state, state) {
			t.Errorf("%s: %v (state %s), want %s", name, err, state, tc.state)
		}
		t.Logf("%s refused: %v", name, err)
	}
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `UPDATE cargo_book.carrier_reference SET live=false WHERE tenant_id=$1`, f.tenant)
		if err != nil {
			return err
		}
		_, err = e.Exec(ctx, `UPDATE cargo_book.carrier_reference SET live=true WHERE tenant_id=$1`, f.tenant)
		return err
	}); sqlState(err) != "23001" {
		t.Errorf("a reference comes back to life: %v", err)
	}
}

// TestBookingsRaceForOneMAWB confirms eight bookings of one airline with the same MAWB number at once: one
// commits, the others are refused because a live booking holds it.
func TestBookingsRaceForOneMAWB(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	const workers = 8
	for i := range workers {
		f.draft(t, fmt.Sprintf("B-%d", i), fmt.Sprintf("C-%d", i))
	}
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.module.RecordCarrierConfirmation(ctx, f.confirm(fmt.Sprintf("B-%d", i), "781-12345675", 2))
		}()
	}
	close(start)
	wg.Wait()
	committed, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, api.ErrRejected):
			refused++
		}
	}
	if committed != 1 || refused != workers-1 {
		t.Fatalf("%d committed, %d refused: %v", committed, refused, errs)
	}
}

// TestReassignmentsRaceOnOneBooking reassigns one booking to eight consolidations at the same revision: one
// commits, the others conflict, and one allocation stays active.
func TestReassignmentsRaceOnOneBooking(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.draft(t, "B-1", "C-1")
	if _, err := f.module.RecordCarrierConfirmation(ctx, f.confirm("B-1", "781-12345675", 2)); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.module.ReassignBookingCapacity(ctx, api.ReassignBookingCapacity{CommandID: fmt.Sprintf("reassign-%d", i), TenantID: f.tenant,
				ActorID: "ops-1", BookingID: "B-1", ExpectedRevision: 3, ConsolidationID: fmt.Sprintf("C-N%d", i), Reason: "consolidation replaced",
				OccurredAt: f.at.Add(3 * time.Hour)})
		}()
	}
	close(start)
	wg.Wait()
	committed, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, api.ErrConflict):
			conflicts++
		}
	}
	if committed != 1 || conflicts != workers-1 {
		t.Fatalf("%d committed, %d conflicts: %v", committed, conflicts, errs)
	}
	var active int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT count(*) FROM cargo_book.capacity_allocation a WHERE tenant_id=$1 AND status='ACTIVE'
            AND version_no=(SELECT max(version_no) FROM cargo_book.capacity_allocation x WHERE x.tenant_id=a.tenant_id AND x.booking_id=a.booking_id
              AND x.allocation_no=a.allocation_no)`, f.tenant).Scan(&active)
	}); err != nil || active != 1 {
		t.Fatalf("%d active allocations, %v", active, err)
	}
}
