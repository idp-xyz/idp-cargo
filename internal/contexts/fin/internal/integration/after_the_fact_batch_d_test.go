//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// V2.102 batch D: a week is reviewed only once it has ended, from the next Monday at 00:00 UTC by FIN's clock; the
// database checks it too, in UTC whatever the session's time zone. And the period of the exception report's actors:
// from included, to excluded.

// The week is reviewed from the instant it ends, not a microsecond before: the refusal has its own code and says to review
// from that Monday; a refused review marks nothing, and a line that is made after it is in the list the later review signs.
func TestAWeekIsNotReviewedBeforeItEndsOnPostgres(t *testing.T) {
	u, week, start := afterWorld(t, 2)
	end := start.AddDate(0, 0, 7)
	u.atTime("one", 1, start.Add(time.Hour))
	for name, at := range map[string]time.Time{"the week's first hour": start.Add(time.Hour), "a microsecond before it ends": end.Add(-time.Microsecond),
		"midnight of the Monday in China, 16:00 UTC on the Sunday": end.Add(-8 * time.Hour).In(time.FixedZone("CST", 8*3600))} {
		early := u.onClock(at)
		_, err := early.module.MarkAfterTheFactReviewed(early.ctx, early.markCmd("early-"+strings.ReplaceAll(name, " ", "-"), "head-1", week))
		early.refused(name, err, "FIN_AFTER_THE_FACT_WEEK_NOT_ENDED", "wait for the Monday after it", "00:00 UTC", "review it from then")
	}
	if p := u.listPage(week, 10, ""); p.Review.Reviewed {
		t.Fatalf("a refused review marked the week: %+v", p.Review)
	}
	// a line made after the refusals, still in the week, is in the list the review signs when the week has ended
	u.atTime("two", 2, end.Add(-time.Hour))
	after := u.onClock(end)
	r := after.must(after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("review", "head-1", week)))
	if r.Version != 2 {
		t.Fatalf("the review signed %d lines, want 2", r.Version)
	}
	if m := u.listPage(week, 10, "").Review; !m.Reviewed || m.ListSize != 2 || !m.Matches || !m.ReviewedAt.Equal(end) {
		t.Fatalf("the review: %+v", m)
	}
}

// The database refuses a review made before its week has ended, and reads the end in UTC: under a session in China, an
// instant written in China time or with no offset (which the session reads as China time) is judged as the instant it is.
func TestTheDatabaseChecksTheWeekHasEndedInUTCWhateverTheSessionZone(t *testing.T) {
	u, _, _ := afterWorld(t, 1)
	f := u.fixture
	row := func(week, startAt, reviewedAt string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.after_the_fact_review (tenant_id,legal_entity_id,week,review_no,week_start,reviewed_by,note,list_size,list_digest,reviewed_at)
            VALUES ($1,'LE','%s',1,'%s+00','head-1','a note',0,'%s','%s')`, week, startAt, strings.Repeat("a", 64), reviewedAt)
	}
	run := func(zone, sql string) error {
		return f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			if _, err := e.Exec(ctx, `SET LOCAL TIME ZONE '`+zone+`'`); err != nil {
				return err
			}
			_, err := e.Exec(ctx, sql, f.tenant)
			return err
		})
	}
	const w, monday = "2026-W49", "2026-11-30 00:00:00" // the week ends at 2026-12-07 00:00 UTC
	for name, c := range map[string]struct{ zone, reviewed string }{
		"a microsecond before the end, in UTC":                 {"UTC", "2026-12-06 23:59:59.999999+00"},
		"the same instant, written in China time":              {"Asia/Shanghai", "2026-12-07 07:59:59.999999+08"},
		"midnight of the Monday with no offset, read as China": {"Asia/Shanghai", "2026-12-07 00:00:00"},
		"the Sunday evening in China, eight hours early":       {"Asia/Shanghai", "2026-12-06 23:00:00"},
		"a second before the end in New York, with no offset":  {"America/New_York", "2026-12-06 18:59:59"},
	} {
		err := run(c.zone, row(w, monday, c.reviewed))
		if state, constraint := sqlState(err); state != "23514" || constraint != "after_the_fact_review_week_ended" {
			t.Errorf("%s: %v (state %s, constraint %s), want 23514 after_the_fact_review_week_ended", name, err, state, constraint)
		}
	}
	// seven days later in a zone that changes its clock is not seven days of instants: week 10 of 2026 ends at 2026-03-09 00:00 UTC
	// and New York changed to summer time on 2026-03-08, so a local "+ 7 days" would end it an hour early
	err := run("America/New_York", row("2026-W10", "2026-03-02 00:00:00", "2026-03-08 23:30:00+00"))
	if state, constraint := sqlState(err); state != "23514" || constraint != "after_the_fact_review_week_ended" {
		t.Errorf("half an hour before the end across a change of clocks: %v (state %s, constraint %s)", err, state, constraint)
	}
	for name, c := range map[string]struct{ zone, week, start, reviewed string }{
		"the end across a change of clocks":      {"America/New_York", "2026-W10", "2026-03-02 00:00:00", "2026-03-09 00:00:00+00"},
		"the end, in UTC":                        {"UTC", "2026-W49", monday, "2026-12-07 00:00:00+00"},
		"the same instant, in China time":        {"Asia/Shanghai", "2026-W50", "2026-12-07 00:00:00", "2026-12-14 08:00:00+08"},
		"no offset, read in New York as the end": {"America/New_York", "2026-W51", "2026-12-14 00:00:00", "2026-12-20 19:00:00"},
	} {
		if err := run(c.zone, row(c.week, c.start, c.reviewed)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The actors of the exception report are those who submitted an apportionment entered in the period, from included and
// to excluded, to the microsecond.
func TestThePeriodOfTheReportsWorkActorsIsFromIncludedToExcluded(t *testing.T) {
	u, _, start := afterWorld(t, 1)
	at := start.Add(time.Hour)
	u.atTime("one", 1, at)
	submitted := func(from, to time.Time) int {
		t.Helper()
		rep, err := u.module.GetFinExceptionReport(u.ctx, api.GetFinExceptionReport{TenantID: u.tenant, ActorID: "alice", From: from, To: to})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, w := range rep.Work {
			if w.ActorID == "fin-2" {
				n += w.Submitted
			}
		}
		return n
	}
	hour := time.Hour
	for name, c := range map[string]struct {
		from, to time.Time
		want     int
	}{
		"from is the entry time, so it is included": {at, at.Add(hour), 1},
		"from a microsecond later, so it is not":    {at.Add(time.Microsecond), at.Add(hour), 0},
		"to is the entry time, so it is excluded":   {at.Add(-hour), at, 0},
		"to a microsecond later, so it is included": {at.Add(-hour), at.Add(time.Microsecond), 1},
		"a period around it":                        {at.Add(-hour), at.Add(hour), 1},
	} {
		if got := submitted(c.from, c.to); got != c.want {
			t.Errorf("%s: %d submissions, want %d", name, got, c.want)
		}
	}
}
