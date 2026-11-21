//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// V2.89 batch 6 on PostgreSQL, migration 000011: a conflict between SEA's records and a carrier's loading report at a
// reseal is released by the operator with a reason. The day is the rework day of sea_rework_test.go: CV-1 stuffed at 3
// with a VGM at 4, PHY's reseal S2 at 5; the carrier reports at hour 4 that CSQU3054383 was loaded (fact 1), and the
// container never gated in.

func (f *reworkFixture) accept(command string, revision int64, reason string, hours int) (api.Receipt, error) {
	return f.module.AcceptCarrierLoadingConflict(context.Background(), api.AcceptCarrierLoadingConflict{CommandID: command, TenantID: f.tenant, ActorID: "ops-9",
		VisitID: "CV-1", ExpectedRevision: revision, Reason: reason, OccurredAt: f.hours(hours)})
}

// conflictDay builds the day to the conflict: the carrier's report (hour 4), PHY's reseal followed (6), a new VGM (7).
func (f *reworkFixture) conflictDay(t *testing.T) {
	t.Helper()
	f.day(t)
	ok(t)(f.carrierFact("load-fact", "OM-1", f.get(t, "OM-1").Revision, api.CarrierLoaded, "CSQU3054383", "", "LD-9", 4, 4))
	f.reseal()
	for _, s := range []string{"SH-1", "SH-2"} {
		ok(t)(f.refresh("refresh-"+s, s, 6))
	}
	ok(t)(f.vgm("vgm-2", f.visit(t, "CV-1").Revision, 7))
}

// Through SEA's commands: finalize is refused with the conflict message; the release is accepted with a reason, which
// the visit keeps with who and when; the item reads as a warning; the event is published with the release's number and
// nothing of the reason; the same command again is a replay; finalize then publishes.
func TestAConflictWithTheCarriersReportIsReleasedWithAReasonOnPostgres(t *testing.T) {
	f := newReworkFixture(t)
	f.conflictDay(t)
	if got := pendingKinds(f.pending(t)); got != "GATED_IN_BEFORE_RESEAL" {
		t.Fatalf("pending: %s", got)
	}
	_, err := f.finalize("fin-conflict", 8)
	if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "the carrier's report of a loading") || !strings.Contains(err.Error(), "naming this container") ||
		!strings.Contains(err.Error(), "AcceptCarrierLoadingConflict") || !strings.Contains(err.Error(), "handled outside the system") {
		t.Fatalf("the conflict: %v", err)
	}
	v := f.visit(t, "CV-1")
	// refused: no reason, a stale revision
	if _, err = f.accept("acc-0", v.Revision, "  ", 8); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("no reason: %v", err)
	}
	if _, err = f.accept("acc-0", v.Revision-1, "checked", 8); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a stale revision: %v", err)
	}
	if got := f.visit(t, "CV-1"); got.Revision != v.Revision || len(got.Releases) != 0 {
		t.Fatalf("a refused release wrote: %+v", got)
	}
	r, err := f.accept("acc-1", v.Revision, "the line confirmed that the box was not loaded", 8)
	if err != nil || r.ReleaseNo != 1 || r.NewRevision != v.Revision+1 || r.VisitID != "CV-1" {
		t.Fatalf("the release: %+v %v", r, err)
	}
	got := f.visit(t, "CV-1")
	if len(got.Releases) != 1 || got.Releases[0].ReleaseNo != 1 || got.Releases[0].FactNo != 1 || got.Releases[0].PHYSeal != "S2" || !got.Releases[0].PHYSealedAt.Equal(f.hours(5)) ||
		got.Releases[0].Reason != "the line confirmed that the box was not loaded" || got.Releases[0].RecordedBy != "ops-9" || !got.Releases[0].RecordedAt.Equal(f.hours(8)) {
		t.Fatalf("the visit keeps the release: %+v", got.Releases)
	}
	items := f.pending(t)
	if pendingKinds(items) != "CARRIER_LOADING_CONFLICT_RELEASED (warning)" || !strings.Contains(items[0].Message, "ops-9") || !strings.Contains(items[0].Message, "the line confirmed") {
		t.Fatalf("pending: %+v", items)
	}
	published := 0
	for _, e := range f.outbox(t) {
		if e.Type == api.CarrierLoadingConflictReleased {
			published++
			if e.Subject != "CV-1" || e.Payload.ReleaseNo != 1 || e.Payload.MovementID != "OM-1" || strings.Contains(e.Raw, "line confirmed") || strings.Contains(e.Raw, "S2") {
				t.Fatalf("the event: %+v %s", e.Payload, e.Raw)
			}
		}
	}
	if published != 1 {
		t.Fatalf("%d release events published", published)
	}
	outboxBefore := len(f.outbox(t))
	if again, err := f.accept("acc-1", v.Revision, "the line confirmed that the box was not loaded", 8); err != nil || again != r {
		t.Fatalf("the same command again: %+v %v", again, err)
	}
	if _, err = f.accept("acc-2", r.NewRevision, "again", 9); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a second release of the same report and seal: %v", err)
	}
	if len(f.outbox(t)) != outboxBefore || len(f.visit(t, "CV-1").Releases) != 1 {
		t.Fatal("a replay or a refusal wrote")
	}
	if m := f.get(t, "OM-1"); m.State != api.StateOpen || m.DocFactsVersion != 0 {
		t.Fatalf("the release changes nothing of the movement: %+v", m)
	}
	ok(t)(f.finalize("fin-ok", 10))
	facts, err := f.module.GetOceanDocFacts(context.Background(), api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
	if err != nil || facts.Version != 1 || facts.Status != api.DocFactsFinal || facts.Containers[0].SealNumber != "S2" {
		t.Fatalf("published: %+v %v", facts, err)
	}
}

// A later reseal needs its own release; the release of the first does not cover it.
func TestALaterResealNeedsItsOwnReleaseOnPostgres(t *testing.T) {
	f := newReworkFixture(t)
	f.conflictDay(t)
	ok(t)(f.accept("acc-1", f.visit(t, "CV-1").Revision, "checked", 8))
	for _, s := range []string{"SH-1", "SH-2"} {
		f.phySealed(s, "S3", 9, 3)
	}
	for _, s := range []string{"SH-1", "SH-2"} {
		ok(t)(f.refresh("refresh3-"+s, s, 10))
	}
	ok(t)(f.vgm("vgm-3", f.visit(t, "CV-1").Revision, 11))
	if got := pendingKinds(f.pending(t)); got != "GATED_IN_BEFORE_RESEAL" {
		t.Fatalf("a new seal: %s", got)
	}
	r := ok(t)(f.accept("acc-2", f.visit(t, "CV-1").Revision, "checked again", 12))
	if r.ReleaseNo != 2 || len(f.visit(t, "CV-1").Releases) != 2 {
		t.Fatalf("the second release: %+v", r)
	}
	ok(t)(f.finalize("fin-ok", 13))
}

// rawRelease is a release written past SEA; stepVisit is the visit's revision step it is written with.
func rawRelease(visit string, no, fact int, columns ...string) string {
	sealedAt := "'2026-01-01T00:00:00Z'"
	seal, reason, by := "'S2'", "'checked'", "'x'"
	for _, c := range columns {
		switch {
		case strings.HasPrefix(c, "seal="):
			seal = strings.TrimPrefix(c, "seal=")
		case strings.HasPrefix(c, "reason="):
			reason = strings.TrimPrefix(c, "reason=")
		case strings.HasPrefix(c, "by="):
			by = strings.TrimPrefix(c, "by=")
		case strings.HasPrefix(c, "at="):
			sealedAt = strings.TrimPrefix(c, "at=")
		}
	}
	return fmt.Sprintf(`INSERT INTO cargo_sea.carrier_conflict_release (tenant_id,visit_id,release_no,fact_no,phy_seal,phy_sealed_at,reason,recorded_by,recorded_at,entered_at)
        VALUES ($1,'%s',%d,%d,%s,%s,%s,%s,now(),now())`, visit, no, fact, seal, sealedAt, reason, by)
}

func stepVisit(visit string) string {
	return fmt.Sprintf(`UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='%s'`, visit)
}

// The database's rules of the release, written past SEA as the application role: each refuses by name.
func TestTheDatabaseRulesOfTheReleaseOnPostgres(t *testing.T) {
	f := newReworkFixture(t)
	f.day(t)
	ok(t)(f.carrierFact("load-fact", "OM-1", f.get(t, "OM-1").Revision, api.CarrierLoaded, "CSQU3054383", "", "LD-9", 4, 4))
	ok(t)(f.carrierFact("dep-fact", "OM-1", f.get(t, "OM-1").Revision, api.CarrierDeparted, "CSQU3054383", "", "DEP-9", 4, 4))
	ok(t)(f.carrierFact("load-fact-3", "OM-1", f.get(t, "OM-1").Revision, api.CarrierLoaded, "CSQU3054383", "", "LD-10", 4, 4))
	ok(t)(f.assign("CV-2", "OM-1", 2, "TEMU1234565", 5))
	for name, tc := range map[string]struct {
		statements []string
		state      string
		in         string
	}{
		"no step of the visit":                   {[]string{rawRelease("CV-1", 1, 1)}, "23514", "with a step of its visit's revision"},
		"a container not stuffed":                {[]string{rawRelease("CV-2", 1, 1), stepVisit("CV-2")}, "23514", "on a stuffed container"},
		"a fact that is no fact of the movement": {[]string{rawRelease("CV-1", 1, 99), stepVisit("CV-1")}, "23514", "of a loading the carrier reported"},
		"a fact that is no loading":              {[]string{rawRelease("CV-1", 1, 2), stepVisit("CV-1")}, "23514", "of a loading the carrier reported"},
		"a number with a gap":                    {[]string{rawRelease("CV-1", 2, 1), stepVisit("CV-1")}, "23514", "numbered without gaps"},
		"a visit that does not exist":            {[]string{rawRelease("CV-9", 1, 1)}, "23503", "carrier_conflict_release_tenant_id_visit_id_fkey"},
		"no reason":                              {[]string{rawRelease("CV-1", 1, 1, "reason='  '"), stepVisit("CV-1")}, "23514", "carrier_conflict_release_reason_check"},
		"a reason of 501 characters":             {[]string{rawRelease("CV-1", 1, 1, "reason='"+strings.Repeat("x", 501)+"'"), stepVisit("CV-1")}, "23514", "carrier_conflict_release_reason_check"},
		"no seal":                                {[]string{rawRelease("CV-1", 1, 1, "seal=' '"), stepVisit("CV-1")}, "23514", "carrier_conflict_release_phy_seal_check"},
		"nobody":                                 {[]string{rawRelease("CV-1", 1, 1, "by=' '"), stepVisit("CV-1")}, "23514", "carrier_conflict_release_recorded_by_check"},
		"a number of 0":                          {[]string{rawRelease("CV-1", 0, 1), stepVisit("CV-1")}, "23514", "carrier_conflict_release_release_no_check"},
		"a fact number of 0":                     {[]string{rawRelease("CV-1", 1, 0), stepVisit("CV-1")}, "23514", "carrier_conflict_release_fact_no_check"},
	} {
		err := f.rawTx(t, tc.statements...)
		if state := sqlState(err); state != tc.state || !strings.Contains(refusal(err), tc.in) {
			t.Errorf("%s: %v (state %s, %s), want %s %s", name, err, state, refusal(err), tc.state, tc.in)
		}
	}
	if got := f.visit(t, "CV-1"); len(got.Releases) != 0 {
		t.Fatalf("a refused write wrote: %+v", got.Releases)
	}
	// a release as SEA writes it: accepted; its number twice is refused by the key
	if err := f.rawTx(t, rawRelease("CV-1", 1, 1), stepVisit("CV-1")); err != nil {
		t.Fatalf("a release with a step of the visit: %v", err)
	}
	if err := f.rawTx(t, rawRelease("CV-1", 1, 1), stepVisit("CV-1")); sqlState(err) != "23505" {
		t.Fatalf("the same number twice: %v", err)
	}
	// one release at most of a report against a seal and its sealed-at, whatever its number
	if err := f.rawTx(t, rawRelease("CV-1", 2, 1), stepVisit("CV-1")); sqlState(err) != "23505" || !strings.Contains(refusal(err), "carrier_conflict_release_once") {
		t.Fatalf("the same report against the same seal twice: %v", err)
	}
	// what only the domain keeps: a release of no conflict (another seal than PHY's, a report dated after it) is
	// accepted
	if err := f.rawTx(t, rawRelease("CV-1", 2, 1, "seal='NO_SUCH_SEAL'"), stepVisit("CV-1")); err != nil {
		t.Fatalf("the database accepts a release the domain would refuse: %v", err)
	}
	// the key is the report, the seal and its sealed-at together: another report, or another sealed-at, is another
	// release
	if err := f.rawTx(t, rawRelease("CV-1", 3, 3), stepVisit("CV-1")); err != nil {
		t.Fatalf("the same seal against another report: %v", err)
	}
	if err := f.rawTx(t, rawRelease("CV-1", 4, 1, "at='2026-01-02T00:00:00Z'"), stepVisit("CV-1")); err != nil {
		t.Fatalf("the same report and seal sealed at another time: %v", err)
	}
	// immutable
	if conn := f.migrator(t); conn != nil {
		for _, sql := range []string{`UPDATE cargo_sea.carrier_conflict_release SET reason='other' WHERE tenant_id=$1`, `DELETE FROM cargo_sea.carrier_conflict_release WHERE tenant_id=$1`} {
			if _, err := conn.Exec(context.Background(), sql, f.tenant); err == nil || !strings.Contains(err.Error(), "cargo_sea") {
				t.Fatalf("a release is never changed or deleted: %v", err)
			}
		}
	}
	// a movement that is not OPEN: the release is refused (the movement is finalized without a conflict first)
	g := newReworkFixture(t)
	g.day(t)
	ok(t)(g.carrierFact("load-fact", "OM-1", g.get(t, "OM-1").Revision, api.CarrierLoaded, "CSQU3054383", "", "LD-9", 4, 4))
	ok(t)(g.finalize("fin-1", 6))
	if m := g.get(t, "OM-1"); m.State != api.StateFinal {
		t.Fatalf("the movement: %+v", m)
	}
	err := g.rawTx(t, rawRelease("CV-1", 1, 1), stepVisit("CV-1"))
	if sqlState(err) != "23514" || !strings.Contains(refusal(err), "only while the movement is open") {
		t.Fatalf("a release on a FINAL movement: %v", err)
	}
}

// A release racing a finalize of the same movement, deterministically: a second connection holds the movement's lock,
// so that the first command waits for it and the second waits behind (blockedByHolder counts only the sessions the
// test's own holder blocks). Release first: both are accepted and the movement is FINAL with the conflict released.
// Finalize first: it is refused with the conflict (nothing was released when it read), the release is then accepted,
// and a second finalize passes. Never a FINAL movement with an unreleased blocking item.
func TestAReleaseRacingAFinalizeOfTheSameMovementOnPostgres(t *testing.T) {
	for _, order := range [][]string{{"release", "finalize"}, {"finalize", "release"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			f := newReworkFixture(t)
			f.conflictDay(t)
			visit := f.visit(t, "CV-1")
			movement := f.get(t, "OM-1").Revision
			hold := f.holdMovement(t)
			commands := map[string]func() error{
				"release": func() error {
					_, err := f.accept("race-release", visit.Revision, "checked", 8)
					return err
				},
				"finalize": func() error {
					_, err := f.module.FinalizeOceanDocFacts(context.Background(), api.FinalizeOceanDocFacts{CommandID: "race-finalize", TenantID: f.tenant, ActorID: "ops-2",
						MovementID: "OM-1", ExpectedRevision: movement, OccurredAt: f.hours(9)})
					return err
				},
			}
			results := map[string]chan error{}
			for _, name := range order {
				hold.start(name, commands[name], results)
			}
			hold.release()
			outcome := map[string]error{}
			for _, name := range order {
				outcome[name] = result(t, name, results[name])
			}
			if outcome["release"] != nil {
				t.Fatalf("the release: %v", outcome["release"])
			}
			if order[0] == "release" {
				if outcome["finalize"] != nil {
					t.Fatalf("the finalize after the release: %v", outcome["finalize"])
				}
			} else {
				if !errors.Is(outcome["finalize"], api.ErrRejected) || !strings.Contains(outcome["finalize"].Error(), "AcceptCarrierLoadingConflict") {
					t.Fatalf("the finalize before the release: %v", outcome["finalize"])
				}
				if m := f.get(t, "OM-1"); m.State != api.StateOpen {
					t.Fatalf("the movement: %+v", m)
				}
				ok(t)(f.finalize("fin-again", 10))
			}
			m := f.get(t, "OM-1")
			if m.State != api.StateFinal || m.DocFactsVersion != 1 || len(f.visit(t, "CV-1").Releases) != 1 {
				t.Fatalf("the end: %+v, %d releases", m, len(f.visit(t, "CV-1").Releases))
			}
			for _, it := range f.pending(t) {
				if it.Blocking {
					t.Fatalf("a FINAL movement with a blocking item: %+v", it)
				}
			}
		})
	}
}

// A carrier report that names no container counts for the container only while it is the movement's one live container:
// with a second live container the release is refused (the item is a warning), with a released second visit it is
// accepted.
func TestANamelessReportIsReleasedOnlyWithOneLiveContainerOnPostgres(t *testing.T) {
	prepare := func(t *testing.T) *reworkFixture {
		f := newReworkFixture(t)
		f.day(t)
		ok(t)(f.carrierFact("load-fact", "OM-1", f.get(t, "OM-1").Revision, api.CarrierLoaded, "", "MAEU123456789", "LD-9", 4, 4))
		f.reseal()
		for _, s := range []string{"SH-1", "SH-2"} {
			ok(t)(f.refresh("refresh-"+s, s, 6))
		}
		ok(t)(f.vgm("vgm-2", f.visit(t, "CV-1").Revision, 7))
		return f
	}
	f := prepare(t)
	ok(t)(f.accept("acc-1", f.visit(t, "CV-1").Revision, "checked", 8))
	f = prepare(t)
	r := ok(t)(f.assign("CV-2", "OM-1", 2, "TEMU1234565", 7))
	if _, err := f.accept("acc-1", f.visit(t, "CV-1").Revision, "checked", 8); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("with a second live container: %v", err)
	}
	ok(t)(f.release("release-2", "CV-2", r.NewRevision, "not needed", 8))
	ok(t)(f.accept("acc-2", f.visit(t, "CV-1").Revision, "checked", 9))
}

// A release is of a loading the carrier reported on the visit's own movement (not a fact of another movement with that
// number), a receipt names a release only for a visit, and a tenant sees only its own releases.
func TestTheReleaseRulesOfFactReceiptAndTenantOnPostgres(t *testing.T) {
	f := newReworkFixture(t)
	f.day(t)
	ok(t)(f.carrierFact("load-fact", "OM-1", f.get(t, "OM-1").Revision, api.CarrierLoaded, "CSQU3054383", "", "LD-9", 4, 4))
	f.w.booking("BK-2", "C-2", "EVER GIVEN", "9811000", "0123X", 1, f.hours(72))
	ok(t)(f.open("OM-2", "C-2", "BK-2"))
	ok(t)(f.masterBill("bill-2", "OM-2", "MAEU987654321", api.MasterBillCarrierMBL, f.get(t, "OM-2").Revision, 2))
	for i := 1; i <= 3; i++ {
		ok(t)(f.carrierFact(fmt.Sprintf("load-2-%d", i), "OM-2", f.get(t, "OM-2").Revision, api.CarrierLoaded, "", "MAEU987654321", fmt.Sprintf("LD-B%d", i), 4, 4))
	}
	// fact 3 is a loading of OM-2; OM-1 has one fact
	err := f.rawTx(t, rawRelease("CV-1", 1, 3), stepVisit("CV-1"))
	if sqlState(err) != "23514" || !strings.Contains(refusal(err), "of a loading the carrier reported") {
		t.Fatalf("a fact of another movement: %v", err)
	}
	// a receipt names a release only for a visit
	err = f.rawTx(t, `INSERT INTO cargo_sea.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision,result_release_no)
        VALUES ($1,'raw-receipt','x','OceanMovement','OM-1',1,1)`)
	if sqlState(err) != "23514" || !strings.Contains(refusal(err), "command_receipt_release_of_visit") {
		t.Fatalf("a release number on a movement's receipt: %v", err)
	}
	// row level security: another tenant sees none
	if err = f.rawTx(t, rawRelease("CV-1", 1, 1), stepVisit("CV-1")); err != nil {
		t.Fatal(err)
	}
	count := func(tenant string) int {
		var n int
		if err := (scope.TenantTransactor{DB: f.db, Tenant: tenant}).WithinTransaction(context.Background(), func(txCtx context.Context) error {
			e, err := f.db.RequireExecutor(txCtx)
			if err != nil {
				return err
			}
			return e.QueryRow(txCtx, `SELECT count(*) FROM cargo_sea.carrier_conflict_release`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if own, other := count(f.tenant), count("some-other-tenant"); own != 1 || other != 0 {
		t.Fatalf("own %d, other %d", own, other)
	}
}
