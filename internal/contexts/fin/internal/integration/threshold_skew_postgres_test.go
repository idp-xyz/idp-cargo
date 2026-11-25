//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// FIN V2.100, after the second review: a lagging or a leading application server, as the database sees it.

// skewClock is the real clock a distance away.
type skewClock struct{ d time.Duration }

func (c skewClock) Now() time.Time { return time.Now().Add(c.d) }

func (th *thresholds) onClock(d time.Duration) *finmodule.Module {
	th.t.Helper()
	m, err := finmodule.New(th.db, th.w, th.w, th.w, th.w, th.w, allow{}, finmodule.WithClock(skewClock{d}))
	if err != nil {
		th.t.Fatal(err)
	}
	return m
}

func (th *thresholds) matchEntered(invoice string) time.Time {
	th.t.Helper()
	var entered time.Time
	if err := th.within(th.t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT match_entered_at FROM cargo_fin.creditor_invoice WHERE tenant_id=$1 AND invoice_id=$2`, th.tenant, invoice).Scan(&entered)
	}); err != nil {
		th.t.Fatal(err)
	}
	return entered
}

// A lowering stamped by a server whose clock is a second behind is not put in force before a match that has already
// committed: it is stamped at the later of the clock and the latest match's system time.
func TestALoweringOnALaggingServerIsNotDatedBeforeAMatchOnPostgres(t *testing.T) {
	th := thresholdWorld(t)
	th.must(th.set("acct-1", 50_000, 100))
	th.must(th.module.ApproveCostVarianceThreshold(th.ctx, th.approveCmd("ctrl-1", 2)))
	time.Sleep(1500 * time.Millisecond) // the lagging clock is then still after the approval
	th.must(th.module.RecordCreditorInvoice(th.ctx, th.record("CI-T1", "trucker", "T1", 5, "CNY", line("PICKUP", 170_000))))
	th.must(th.module.MatchCreditorInvoice(th.ctx, th.match("CI-T1", "ap-1", 5, onJob("JOB-A", 1, 1, 170_000, true))))
	entered := th.matchEntered("CI-T1")
	if i := th.invoice("CI-T1"); i.ThresholdVersion != 2 || i.ApprovalRequired {
		t.Fatalf("the match read version 2: %+v", i)
	}
	th.must(th.onClock(-time.Second).SetCostVarianceThreshold(th.ctx, th.setCmd("acct-1", 10_000, 100)))
	if v := th.version(3); v.Status != "APPROVED" || v.InForceAt.Before(entered) {
		t.Fatalf("a lowering in force from %s, before the match of %s", v.InForceAt, entered)
	}
}

// An approval stamped by a server a second behind is not put in force before a match that has committed and read
// the older version.
func TestAnApprovalOnALaggingServerIsNotDatedBeforeAMatchOnPostgres(t *testing.T) {
	th := thresholdWorld(t)
	th.must(th.set("acct-1", 50_000, 100))
	time.Sleep(1500 * time.Millisecond)
	th.must(th.module.RecordCreditorInvoice(th.ctx, th.record("CI-T1", "trucker", "T1", 5, "CNY", line("PICKUP", 170_000))))
	th.must(th.module.MatchCreditorInvoice(th.ctx, th.match("CI-T1", "ap-1", 5, onJob("JOB-A", 1, 1, 170_000, true))))
	entered := th.matchEntered("CI-T1")
	if i := th.invoice("CI-T1"); i.ThresholdVersion != 1 {
		t.Fatalf("the match read version 1: %+v", i)
	}
	th.must(th.onClock(-time.Second).ApproveCostVarianceThreshold(th.ctx, th.approveCmd("ctrl-1", 2)))
	if v := th.version(2); v.Status != "APPROVED" || v.InForceAt.Before(entered) {
		t.Fatalf("an approval in force from %s, before the match of %s", v.InForceAt, entered)
	}
}

// A server whose clock is five minutes off the database's is refused with the cause and the way out, as a refusal and
// not a raw database error, for an approval, a setting and a match ahead; nothing is written. A match on a server five
// minutes behind is read at the latest approval in force instead (it cannot be dated before it), which is within the
// minute, and is made.
func TestAServerFiveMinutesOffIsToldToSynchroniseItsClockOnPostgres(t *testing.T) {
	th := thresholdWorld(t)
	th.must(th.set("acct-1", 50_000, 100))
	th.must(th.module.RecordCreditorInvoice(th.ctx, th.record("CI-T1", "trucker", "T1", 5, "CNY", line("PICKUP", 170_000))))
	way := "synchronise the clocks of FIN's servers"
	for _, skew := range []time.Duration{5 * time.Minute, -5 * time.Minute} {
		m := th.onClock(skew)
		_, approve := m.ApproveCostVarianceThreshold(th.ctx, th.approveCmd("ctrl-1", 2))
		if !errors.Is(approve, api.ErrRejected) || !strings.Contains(approve.Error(), way) || strings.Contains(approve.Error(), "SQLSTATE") {
			t.Errorf("%v: an approval: %v", skew, approve)
		}
		if v := th.version(2); v.Status != "PROPOSED" {
			t.Fatalf("%v: a refused approval changed the version: %+v", skew, v)
		}
	}
	ahead := th.onClock(5 * time.Minute)
	_, err := ahead.MatchCreditorInvoice(th.ctx, th.match("CI-T1", "ap-1", 5, onJob("JOB-A", 1, 1, 170_000, true)))
	if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), way) || strings.Contains(err.Error(), "SQLSTATE") {
		t.Errorf("a match five minutes ahead: %v", err)
	}
	if i := th.invoice("CI-T1"); i.State != api.CreditorRecorded {
		t.Fatalf("a refused match wrote: %+v", i)
	}
	// a setting while a version waits is refused for that; with nothing waiting, on the clock
	th.must(th.module.RefuseCostVarianceThreshold(th.ctx, th.refuseCmd("ctrl-1", 2, "no")))
	for _, skew := range []time.Duration{5 * time.Minute, -5 * time.Minute} {
		_, err = th.onClock(skew).SetCostVarianceThreshold(th.ctx, th.setCmd("acct-1", 1, 1))
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), way) || strings.Contains(err.Error(), "SQLSTATE") {
			t.Errorf("a setting %v off: %v", skew, err)
		}
	}
	if n := len(th.policy().VarianceThresholds); n != 2 {
		t.Errorf("a refused setting wrote a version: %d versions", n)
	}
	// behind, a match is read at the latest approval in force, not before it
	th.must(th.set("acct-1", 50_000, 100))
	th.must(th.module.ApproveCostVarianceThreshold(th.ctx, th.approveCmd("ctrl-1", 3)))
	if _, err = th.onClock(-5*time.Minute).MatchCreditorInvoice(th.ctx, th.match("CI-T1", "ap-1", 5, onJob("JOB-A", 1, 1, 170_000, true))); err != nil {
		t.Fatalf("a match five minutes behind: %v", err)
	}
	if i := th.invoice("CI-T1"); i.ThresholdVersion != 3 || !th.matchEntered("CI-T1").Equal(th.version(3).InForceAt) {
		t.Fatalf("a match five minutes behind read version %d at %s, the approval being at %s", i.ThresholdVersion, th.matchEntered("CI-T1"), th.version(3).InForceAt)
	}
}
