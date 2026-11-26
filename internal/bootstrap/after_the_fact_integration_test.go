//go:build integration

package bootstrap

import (
	"fmt"
	"strings"
	"testing"
	"time"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// FIN's after-the-fact review list, the LEAN profile's weekly compensation (V2.102, batch B), on the owner's box A with
// the real ORG, RATE, COM, FUL, PHY and FIN, under LEAN-like settings set through FIN's own settings commands, proposed
// by cfg-1 and approved by cfg-2: F1 UNRESTRICTED, F3 ONLY_WHEN_AMOUNT_MOVES and F8 DEFAULT_TO_REMAINING. H03 is unloaded
// in a rework ops-1 asked for and transferred to C-2 by ops-9; ops-1 previews and submits C-1's line 1 without stating H03,
// and F8 attributes H03 nothing, its CNY 500.00 going to the cargo that remains.
//
// The week's list shows that attribution with all its reasons: nobody else looked at it, it has no voucher and nobody
// stated it. ops-1 who made it may not review the week; a reviewer who did not marks it, and the week then reads as
// reviewed, by whom and when, with a list that still matches.
func TestTheDefaultOfOneUnrestrictedPersonIsOnTheWeeksReviewListWithEveryContext(t *testing.T) {
	d, v := boxAAfterTheTransfer(t, "cargo-box-a-lean-", leSetting("fin.F1", "UNRESTRICTED"), leSetting("fin.F3", "ONLY_WHEN_AMOUNT_MOVES"), leSetting("fin.F8", "DEFAULT_TO_REMAINING"))
	if v == 0 {
		t.Fatal("the settings are not in force")
	}
	pre, err := d.previewBy("ops-1", "lean-pre")
	d.must("ops-1 previews without stating H03", err)
	if _, err = d.submitBy("ops-1", "lean-sub", pre.Version); err != nil {
		t.Fatalf("ops-1 submits the default under F1 UNRESTRICTED: %v", err)
	}
	a := d.pool().Apportionments[1]
	if a.By != "ops-1" || a.PreviewNo != pre.Version || len(a.Lines[0].Attributions) != 1 || !a.Lines[0].Attributions[0].ByDefault {
		t.Fatalf("the default: %+v", a)
	}
	y, w := a.EnteredAt.UTC().ISOWeek()
	week := fmt.Sprintf("%04d-W%02d", y, w)
	read := func(week string) finapi.AfterTheFactPage {
		t.Helper()
		p, err := d.fin.GetAfterTheFactReview(d.ctx, finapi.GetAfterTheFactReview{TenantID: d.tenant, ActorID: "head-1", LegalEntityID: "LE", Week: week, Limit: 10})
		d.must("the list of "+week, err)
		return p
	}

	p := read(week)
	if len(p.Lines) != 1 || p.NextAfter != "" || p.Review.Reviewed {
		t.Fatalf("the week's list: %+v", p)
	}
	l := p.Lines[0]
	if l.PoolID != "P-1" || l.ConsolidationID != "C-1" || l.LineNo != 1 || l.ShipmentID != "SH-H03" || l.JobID != "JOB-H03" || l.ApportionmentNo != 2 || l.PreviewNo != pre.Version ||
		l.SubmittedBy != "ops-1" || l.PreviewedBy != "ops-1" || l.Voucher != "" || l.Party != "" || l.Loss.Minor != 0 || l.LossLocal.Minor != 0 || l.Exception.Minor != 0 ||
		l.ExceptionLocal.Minor != 0 || l.Loss.Currency != "CNY" || !l.EnteredAt.Equal(a.EnteredAt) ||
		strings.Join(l.Reasons, ",") != finapi.ReviewReasonNoSecondPerson+","+finapi.ReviewReasonNoVoucher+","+finapi.ReviewReasonMadeByDefault {
		t.Fatalf("the line: %+v", l)
	}

	// a week is reviewed once it has ended: the running week is refused, whoever asks, and nothing is marked (review it from the Monday)
	end := weekEnd(a.EnteredAt)
	running, err := d.fin.MarkAfterTheFactReviewed(d.ctx, finapi.MarkAfterTheFactReviewed{CommandID: "lean-review-running", TenantID: d.tenant, ActorID: "head-1", LegalEntityID: "LE",
		Week: week, Note: "reviewed while the week runs"})
	said(t, "head-1 reviewing the running week", err, "FIN_AFTER_THE_FACT_WEEK_NOT_ENDED", "wait for the Monday after it", "00:00 UTC")
	if running != (finapi.Receipt{}) || read(week).Review.Reviewed {
		t.Fatalf("a refused review marked the running week: %+v", read(week).Review)
	}
	// a second attribution of the same week, made after that refusal, is in the list the review signs
	st := statement("H03", 5_000, 5_000, finapi.PartyCarrier)
	st.Reason = "a correction"
	pre2, err := d.previewWith("ops-2", "lean-pre-2", st)
	d.must("ops-2 previews a correction", err)
	_, err = d.submitBy("ops-2", "lean-sub-2", pre2.Version)
	d.must("ops-2 submits it", err)
	if got := len(read(week).Lines); got != 2 {
		t.Fatalf("the week's list has %d lines, want 2", got)
	}

	// the person who made it does not review it; another does, once, after the week ended, and the list is the whole week's
	review := d.finAt(end.Add(time.Hour))
	mark := func(id, who string) (finapi.Receipt, error) {
		return review.MarkAfterTheFactReviewed(d.ctx, finapi.MarkAfterTheFactReviewed{CommandID: id, TenantID: d.tenant, ActorID: who, LegalEntityID: "LE", Week: week,
			Note: "the default of H03 is right: the line's whole cost stays with the cargo that remains"})
	}
	_, err = mark("lean-review-ops-1", "ops-1")
	said(t, "ops-1, who submitted and previewed it", err, "FIN_AFTER_THE_FACT_REVIEWER", "ops-1 submitted and previewed apportionment 2 of pool P-1", "line 1 of house SH-H03",
		"another person reviews it")
	if p = read(week); p.Review.Reviewed {
		t.Fatalf("a refused review marked the week: %+v", p.Review)
	}
	r, err := mark("lean-review", "head-1")
	d.must("head-1 reviews the week", err)
	if again, err := mark("lean-review", "head-1"); err != nil || again != r {
		t.Fatalf("the replay: %+v %v", again, err)
	}
	_, err = mark("lean-review-2", "head-2")
	said(t, "a second review", err, "FIN_AFTER_THE_FACT_REVIEWED", "reviewed by head-1")
	if r.SubjectID != "LE/"+week || r.Version != 2 {
		t.Fatalf("the receipt: %+v", r)
	}
	m := read(week).Review
	if !m.Reviewed || m.ReviewedBy != "head-1" || m.ReviewedAt.IsZero() || m.ListSize != 2 || m.CurrentSize != 2 || !m.Matches || m.CurrentDigest != m.ListDigest || len(m.ListDigest) != 64 {
		t.Fatalf("the week reviewed: %+v", m)
	}
}

// weekEnd is the Monday at 00:00 UTC after the week that contains the time.
func weekEnd(at time.Time) time.Time {
	day := at.UTC().Truncate(24 * time.Hour)
	return day.AddDate(0, 0, 7-(int(day.Weekday())+6)%7)
}
