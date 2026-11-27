//go:build integration

package integration

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	fulmodule "go.idp.xyz/idp-cargo/internal/contexts/ful"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/api"
)

// bookStub answers CarrierBooking v1 as the test sets it; BOOK's other reads are not FUL's.
type bookStub struct {
	bookapi.Queries
	mu       sync.Mutex
	bookings map[string]bookapi.CarrierBooking
}

func (b *bookStub) GetCarrierBooking(_ context.Context, q bookapi.GetCarrierBooking) (bookapi.CarrierBooking, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	got, ok := b.bookings[q.BookingID]
	if !ok {
		return bookapi.CarrierBooking{}, bookapi.ErrNotFound
	}
	got.TenantID = q.TenantID
	return got, nil
}

func (b *bookStub) set(c bookapi.CarrierBooking) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bookings[c.BookingID] = c
}

// seaBooking is a confirmed container booking of C-1 at a capacity version, its one line departing at etd.
func seaBooking(id, consolidation string, version int, etd time.Time) bookapi.CarrierBooking {
	return bookapi.CarrierBooking{BookingID: id, Revision: int64(version + 2), Kind: bookapi.KindSeaLCLConsolEquipment, Status: bookapi.StatusConfirmed,
		ConsumerKind: bookapi.ConsumerConsolidation, ConsumerID: consolidation,
		Capacity: bookapi.CapacityVersion{Version: version, Lines: []bookapi.CapacityLineView{{Line: 1, CarrierReference: "BKG-" + id,
			Legs: []bookapi.CapacityLeg{{Loading: bookapi.ResolvedLocation{Code: unlocode("CNSHA")}, Discharge: bookapi.ResolvedLocation{Code: unlocode("DEHAM")},
				Vessel: "EVER GIVEN", Voyage: "OM-1", ETD: etd, ETA: etd.Add(30 * 24 * time.Hour)}}}}}}
}

// V2.110 on PostgreSQL: the main carriage links to BOOK's capacity of its consolidation, again after the carrier's
// change; a departure outside the window awaits the customer's answer, which the work list and the operation plans
// show; the links, legs and answers are kept as written.
func TestCapacityLinksAndTheCustomersAnswerOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	phy := &phyStub{at: f.at.Add(4 * time.Hour)}
	book := &bookStub{bookings: map[string]bookapi.CarrierBooking{}}
	module, err := fulmodule.New(f.db, f.com, shipmentPolicy{}, fulmodule.WithLocations(locationStub{}), fulmodule.WithConsolMemberships(phy),
		fulmodule.WithPhysicalEvidence(phy), fulmodule.WithCarrierBookings(book))
	if err != nil {
		t.Fatal(err)
	}
	f.module = module
	if _, err = f.module.LinkCarrierCapacity(ctx, api.LinkCarrierCapacity{CommandID: "x", TenantID: f.tenant, ActorID: "ops-1", ShipmentID: "SH-1",
		BookingID: "BK-1", OccurredAt: f.at}); err == nil {
		t.Fatal("a link before the shipment exists")
	}
	for _, step := range []func() (api.Receipt, error){
		func() (api.Receipt, error) {
			return f.module.CreateShipmentFromObligation(ctx, f.create("SH-1", "create", "OB-1"))
		},
		func() (api.Receipt, error) {
			return f.module.AmendShipmentPlan(ctx, f.amend("amend", 1, 3, "schedule"))
		},
		func() (api.Receipt, error) { return f.module.ConfirmShipmentPlan(ctx, f.confirm("confirm", 2)) },
		func() (api.Receipt, error) {
			return f.module.RecordConsolAttachment(ctx, api.RecordConsolAttachment{CommandID: "attach", TenantID: f.tenant, ActorID: "ops-1",
				ShipmentID: "SH-1", ExpectedRevision: 3, ConsolidationID: "C-1", OccurredAt: f.at.Add(4 * time.Hour)})
		},
	} {
		if _, err = step(); err != nil {
			t.Fatal(err)
		}
	}
	window := f.amend("", 0, 3, "").DepartureWindow
	link := func(command string, expected int64, booking string, minutes int) (api.Receipt, error) {
		return f.module.LinkCarrierCapacity(ctx, api.LinkCarrierCapacity{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", ShipmentID: "SH-1",
			ExpectedRevision: expected, BookingID: booking, OccurredAt: f.at.Add(5*time.Hour + time.Duration(minutes)*time.Minute)})
	}
	answer := func(command string, expected int64, linkNo int, answer string, minutes int) (api.Receipt, error) {
		return f.module.RecordCustomerDateAnswer(ctx, api.RecordCustomerDateAnswer{CommandID: command, TenantID: f.tenant, ActorID: "ops-1",
			ShipmentID: "SH-1", ExpectedRevision: expected, LinkNo: linkNo, Answer: answer, EvidenceRef: "mail " + command,
			OccurredAt: f.at.Add(5*time.Hour + time.Duration(minutes)*time.Minute)})
	}
	execution := func() api.ShipmentExecution {
		t.Helper()
		got, err := f.module.GetShipmentExecution(ctx, api.GetShipmentExecution{TenantID: f.tenant, ShipmentID: "SH-1"})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	awaiting := func(consolidation string) []api.DateChangeAwaitingCustomer {
		t.Helper()
		got, err := f.module.ListDateChangesAwaitingCustomer(ctx, api.ListDateChangesAwaitingCustomer{TenantID: f.tenant, ConsolidationID: consolidation})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// what BOOK cannot give, or gives for another consolidation, is refused
	if _, err = link("missing", 4, "BK-0", 0); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an unknown booking: %v", err)
	}
	pending := seaBooking("BK-1", "C-1", 1, window.From.Add(time.Hour))
	pending.Status, pending.Capacity = bookapi.StatusRequested, bookapi.CapacityVersion{}
	book.set(pending)
	if _, err = link("requested", 4, "BK-1", 0); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a booking not confirmed: %v", err)
	}
	book.set(seaBooking("BK-9", "C-9", 1, window.From.Add(time.Hour)))
	if _, err = link("other", 4, "BK-9", 0); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a booking of another consolidation: %v", err)
	}

	// within the window: linked, nothing awaits; the same carriage again keeps the revision
	book.set(seaBooking("BK-1", "C-1", 1, window.From.Add(time.Hour)))
	first, err := link("link-1", 4, "BK-1", 1)
	if err != nil || first.NewRevision != 5 {
		t.Fatalf("link: %+v %v", first, err)
	}
	if again, err := link("link-1b", 5, "BK-1", 2); err != nil || again.NewRevision != 5 {
		t.Fatalf("the same carriage again: %+v %v", again, err)
	}
	if e := execution(); len(e.Links) != 1 || e.CurrentLink != 1 || e.AwaitingCustomer || e.Links[0].OutsideWindow || e.ConsolidationID != "C-1" ||
		e.Links[0].Legs[0].Vessel != "EVER GIVEN" || e.Links[0].CarrierReference != "BKG-BK-1" {
		t.Fatalf("after the first link: %+v", e)
	}
	if _, err = answer("early", 5, 1, api.AnswerAccepted, 3); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an answer to a departure within the window: %v", err)
	}

	// the carrier rolls the booking two days past the window: linked again, the customer is asked
	outside := window.Until.Add(48 * time.Hour)
	book.set(seaBooking("BK-1", "C-1", 2, outside))
	if r, err := link("link-2", 5, "BK-1", 4); err != nil || r.NewRevision != 6 {
		t.Fatalf("the rolled capacity: %+v %v", r, err)
	}
	if e := execution(); len(e.Links) != 2 || !e.AwaitingCustomer || e.Refused || !e.Links[1].OutsideWindow {
		t.Fatalf("awaiting: %+v", e)
	}
	if got := awaiting(""); len(got) != 1 || got[0].ShipmentID != "SH-1" || got[0].LinkNo != 2 || !got[0].Departure.Equal(outside) {
		t.Fatalf("the tenant's work list: %+v", got)
	}
	if got := awaiting("C-9"); len(got) != 0 {
		t.Fatalf("another consolidation's work list: %+v", got)
	}
	if _, err = answer("refused", 6, 2, api.AnswerRefused, 5); err != nil {
		t.Fatal(err)
	}
	if got := awaiting("C-1"); len(got) != 1 || !got[0].Refused {
		t.Fatalf("after a refusal: %+v", got)
	}
	if _, err = answer("accepted", 7, 2, api.AnswerAccepted, 6); err != nil {
		t.Fatal(err)
	}
	if got := awaiting("C-1"); len(got) != 0 {
		t.Fatalf("after an acceptance: %+v", got)
	}
	e := execution()
	if e.AwaitingCustomer || len(e.Links[1].Answers) != 2 || e.Links[1].Answers[0].Counts || !e.Links[1].Answers[1].Counts ||
		!e.AgreedWindow.Until.Equal(outside.Add(time.Microsecond)) || !e.DepartureWindow.Until.Equal(window.Until) {
		t.Fatalf("after the answers: %+v", e)
	}
	for _, read := range []func() (any, error){
		func() (any, error) {
			return f.module.GetOceanOperationPlan(ctx, api.GetShipmentPlanSnapshot{TenantID: f.tenant, ShipmentID: "SH-1"})
		},
		func() (any, error) {
			return f.module.GetAirOperationPlan(ctx, api.GetShipmentPlanSnapshot{TenantID: f.tenant, ShipmentID: "SH-1"})
		},
	} {
		got, err := read()
		if err != nil {
			t.Fatal(err)
		}
		var agreed, plan time.Time
		switch p := got.(type) {
		case api.OceanOperationPlan:
			agreed, plan = p.AgreedWindow.Until, p.DepartureWindow.Until
		case api.AirOperationPlan:
			agreed, plan = p.AgreedWindow.Until, p.DepartureWindow.Until
		}
		if !agreed.Equal(outside.Add(time.Microsecond)) || !plan.Equal(window.Until) {
			t.Fatalf("an operation plan's windows: agreed %s, plan %s", agreed, plan)
		}
	}
	if types := f.outboxTypes(t); slices.Index(types, api.ShipmentCapacityLinked) < 0 ||
		len(slices.DeleteFunc(slices.Clone(types), func(s string) bool { return s != api.ShipmentDateAnswerRecorded })) != 2 {
		t.Fatalf("events: %v", types)
	}

	// the database keeps the rows as written; where a rule names its constraint, the refusal is that rule's
	for _, tc := range []struct{ sql, state, why, constraint string }{
		{`UPDATE cargo_ful.shipment_capacity_link SET capacity_version=9 WHERE tenant_id=$1`, "42501", "a link is immutable", ""},
		{`DELETE FROM cargo_ful.shipment_capacity_link_leg WHERE tenant_id=$1`, "42501", "a link's legs are kept", ""},
		{`UPDATE cargo_ful.shipment_date_answer SET answer='REFUSED' WHERE tenant_id=$1`, "42501", "an answer is immutable", ""},
		{`INSERT INTO cargo_ful.shipment_capacity_link (tenant_id,shipment_id,link_no,stage,plan_no,booking_id,booking_revision,transport_mode,
          consolidation_id,capacity_version,line_no,linked_by,linked_at,entered_at) VALUES ($1,'SH-1',4,'MAIN',2,'BK-1',9,'SEA','C-1',3,1,'rogue',now(),now())`,
			"23514", "links are numbered one after another", "shipment_capacity_link_follows"},
		{`INSERT INTO cargo_ful.shipment_capacity_link (tenant_id,shipment_id,link_no,stage,plan_no,booking_id,booking_revision,transport_mode,
          consolidation_id,capacity_version,line_no,linked_by,linked_at,entered_at) VALUES ($1,'SH-1',3,'MAIN',2,'BK-9',9,'SEA','C-9',3,1,'rogue',now(),now())`,
			"23514", "a link is to the consolidation the shipment is attached to", "shipment_capacity_link_attached"},
		{`INSERT INTO cargo_ful.shipment_capacity_link (tenant_id,shipment_id,link_no,stage,plan_no,booking_id,booking_revision,transport_mode,
          consolidation_id,capacity_version,line_no,linked_by,linked_at,entered_at) VALUES ($1,'SH-1',3,'MAIN',2,'BK-1',9,'SEA','C-1',3,1,'rogue',now(),now())`,
			"23514", "a link has legs, written with it", "shipment_capacity_link_legs_with_link"},
		{`INSERT INTO cargo_ful.shipment_capacity_link_leg (tenant_id,shipment_id,link_no,leg_no,etd) VALUES ($1,'SH-1',1,2,now())`,
			"23514", "a link's legs are written with it", "shipment_capacity_link_legs_with_link"},
		{`INSERT INTO cargo_ful.shipment_capacity_link_leg (tenant_id,shipment_id,link_no,leg_no,etd,eta) VALUES ($1,'SH-1',1,2,now(),now() - interval '1 day')`,
			"23514", "a leg arrives after it departs", "shipment_capacity_link_leg_times"},
		{`INSERT INTO cargo_ful.shipment_date_answer (tenant_id,shipment_id,answer_no,link_no,answer,evidence_ref,answered_by,answered_at,entered_at)
          VALUES ($1,'SH-1',3,1,'ACCEPTED','mail','rogue',now(),now())`, "23514", "an answer is to the latest link", ""},
		{`INSERT INTO cargo_ful.shipment_date_answer (tenant_id,shipment_id,answer_no,link_no,answer,evidence_ref,answered_by,answered_at,entered_at)
          VALUES ($1,'SH-1',3,2,'ACCEPTED','mail','rogue',now() - interval '30 days',now())`, "23514", "an answer is not dated before its link", ""},
		{`INSERT INTO cargo_ful.shipment_date_answer (tenant_id,shipment_id,answer_no,link_no,answer,evidence_ref,answered_by,answered_at,entered_at)
          VALUES ($1,'SH-1',5,2,'ACCEPTED','mail','rogue',now(),now())`, "23514", "answers are numbered one after another", ""},
		{`INSERT INTO cargo_ful.shipment_date_answer (tenant_id,shipment_id,answer_no,link_no,answer,evidence_ref,answered_by,answered_at,entered_at)
          VALUES ($1,'SH-1',3,2,'MAYBE','mail','rogue',now(),now())`, "23514", "an answer is ACCEPTED or REFUSED", ""},
		{`INSERT INTO cargo_ful.shipment_date_answer (tenant_id,shipment_id,answer_no,link_no,answer,evidence_ref,answered_by,answered_at,entered_at)
          VALUES ($1,'SH-1',3,2,'ACCEPTED',' ','rogue',now(),now())`, "23514", "an answer names its evidence", ""},
		{`INSERT INTO cargo_ful.shipment_capacity_link (tenant_id,shipment_id,link_no,stage,plan_no,booking_id,booking_revision,transport_mode,
          consolidation_id,capacity_version,line_no,linked_by,linked_at,entered_at) VALUES ($1,'SH-1',3,'MAIN',2,'BK-1',9,'SEA','C-1',3,1,'rogue',now() - interval '60 days',now())`,
			"23514", "a link is not dated before the shipment's latest link or answer", "shipment_capacity_link_in_order"},
		{`INSERT INTO cargo_ful.shipment_date_answer (tenant_id,shipment_id,answer_no,link_no,answer,evidence_ref,window_from,window_until,answered_by,answered_at,entered_at)
          SELECT $1,'SH-1',3,2,'ACCEPTED','mail',departure_from,departure_until,'rogue',
            (SELECT linked_at FROM cargo_ful.shipment_capacity_link WHERE tenant_id=$1 AND shipment_id='SH-1' AND link_no=2) + interval '1 second',now()
          FROM cargo_ful.shipment_plan_version WHERE tenant_id=$1 AND shipment_id='SH-1' AND plan_no=2`,
			"23514", "an answer is not dated before the previous answer", "shipment_date_answer_in_order"},
		{`INSERT INTO cargo_ful.shipment_date_answer (tenant_id,shipment_id,answer_no,link_no,answer,evidence_ref,window_from,window_until,answered_by,answered_at,entered_at)
          VALUES ($1,'SH-1',3,2,'ACCEPTED','mail',now(),now() + interval '1 day','rogue',now(),now())`,
			"23514", "an answer names the plan's window, which it answered", "shipment_date_answer_window"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		var pgErr *pgconn.PgError
		if sqlState(err) != tc.state || (tc.constraint != "" && (!errors.As(err, &pgErr) || pgErr.ConstraintName != tc.constraint)) {
			t.Errorf("%s: err = %v, want SQLSTATE %s %s (%s)", tc.sql, err, tc.state, tc.constraint, tc.why)
		}
	}
	// the same answer with the plan's window and in order is taken: the refusals above are the rules'
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `INSERT INTO cargo_ful.shipment_date_answer (tenant_id,shipment_id,answer_no,link_no,answer,evidence_ref,window_from,window_until,
            answered_by,answered_at,entered_at) SELECT $1,'SH-1',3,2,'REFUSED','mail',departure_from,departure_until,'rogue',now(),now()
          FROM cargo_ful.shipment_plan_version WHERE tenant_id=$1 AND shipment_id='SH-1' AND plan_no=2`, f.tenant)
		return err
	}); err != nil {
		t.Fatalf("an answer in order, with the plan's window: %v", err)
	}

	// a cancelled shipment takes no link and no answer
	if _, err = f.module.CancelShipment(ctx, api.CancelShipment{CommandID: "cancel", TenantID: f.tenant, ActorID: "planner-1", ShipmentID: "SH-1",
		ExpectedRevision: 8, Reason: "withdrawn", OccurredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO cargo_ful.shipment_date_answer (tenant_id,shipment_id,answer_no,link_no,answer,evidence_ref,answered_by,answered_at,entered_at)
          VALUES ($1,'SH-1',3,2,'ACCEPTED','mail','rogue',now(),now())`,
		`INSERT INTO cargo_ful.shipment_capacity_link (tenant_id,shipment_id,link_no,stage,plan_no,booking_id,booking_revision,transport_mode,
          consolidation_id,capacity_version,line_no,linked_by,linked_at,entered_at) VALUES ($1,'SH-1',3,'MAIN',2,'BK-1',9,'SEA','C-1',3,1,'rogue',now(),now())`,
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, sql, f.tenant)
			return err
		})
		if sqlState(err) != "23514" {
			t.Errorf("cancelled: %s: err = %v", sql, err)
		}
	}
	if got := awaiting(""); len(got) != 0 {
		t.Fatalf("a cancelled shipment on the work list: %+v", got)
	}
}
