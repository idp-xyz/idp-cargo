//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
)

// V2.102 batch F (criterion 4): a weekly review records its conclusion, ACCEPTED or NOT_ACCEPTED, with a note, and the lines it covers, which
// are the list at that time; the week's list says, for each line, the review that covers it or that none does; one query says what still waits
// for review, over a legal entity and a range of weeks.

func (u usedWorld) waiting(from, to string) api.AfterTheFactPending {
	u.t.Helper()
	p, err := u.module.ListAfterTheFactPending(u.ctx, api.ListAfterTheFactPending{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", FromWeek: from, ToWeek: to})
	if err != nil {
		u.t.Fatal(err)
	}
	return p
}

func (u usedWorld) reviewRows(table, where string) int {
	u.t.Helper()
	var n int
	err := u.fixture.within(u.t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT count(*) FROM cargo_fin.`+table+` WHERE tenant_id=$1 `+where, u.tenant).Scan(&n)
	})
	if err != nil {
		u.t.Fatal(err)
	}
	return n
}

func TestAReviewRecordsItsConclusionAndTheLinesItCovers(t *testing.T) {
	u, week, start := afterWorld(t, 5)
	end := start.AddDate(0, 0, 7)
	u.atTime("one", 1, start.Add(time.Hour))
	u.atTime("two", 2, start.Add(2*time.Hour))
	after := u.onClock(end.Add(time.Hour))

	// the conclusion is one of the two, and a NOT_ACCEPTED says why
	for name, c := range map[string]struct{ conclusion, note, say string }{
		"no conclusion":             {"", "x", "ACCEPTED or NOT_ACCEPTED"},
		"another conclusion":        {"REJECTED", "x", "ACCEPTED or NOT_ACCEPTED"},
		"NOT_ACCEPTED with no note": {"NOT_ACCEPTED", "", "says why"},
		"NOT_ACCEPTED with a blank": {"NOT_ACCEPTED", " \n", "says why"},
		"a blank note on ACCEPTED":  {"ACCEPTED", "  ", "1 to 2000 characters when given"},
		"a note of 2001 characters": {"ACCEPTED", strings.Repeat("n", 2001), "1 to 2000 characters when given"},
	} {
		cmd := after.markCmd("bad-"+strings.ReplaceAll(name, " ", "-"), "head-1", week)
		cmd.Conclusion, cmd.Note = c.conclusion, c.note
		_, err := after.module.MarkAfterTheFactReviewed(after.ctx, cmd)
		after.refused(name, err, c.say)
	}
	if n := after.reviewRows("after_the_fact_review", ""); n != 0 {
		t.Fatalf("a refused review wrote %d rows", n)
	}
	cmd := after.markCmd("review-1", "head-1", week)
	cmd.Conclusion, cmd.Note = "NOT_ACCEPTED", "the voucher of line 2 is missing"
	r1 := after.must(after.module.MarkAfterTheFactReviewed(after.ctx, cmd))
	if r1.NewRevision != 1 || r1.Version != 2 {
		t.Fatalf("the receipt: %+v", r1)
	}
	if n, m := after.reviewRows("after_the_fact_review_line", "AND review_no = 1"), after.reviewRows("after_the_fact_review", ""); n != 2 || m != 1 {
		t.Fatalf("the review covers %d lines (want 2), %d reviews", n, m)
	}
	page := u.onClock(end.Add(2*time.Hour)).listPage(week, 10, "")
	if m := page.Review; !m.Reviewed || m.Conclusion != "NOT_ACCEPTED" || m.Note != "the voucher of line 2 is missing" || m.ReviewNo != 1 || m.ReviewedBy != "head-1" || !m.Matches {
		t.Fatalf("the mark: %+v", m)
	}
	if len(page.Lines) != 2 {
		t.Fatalf("the list: %+v", page.Lines)
	}
	for _, l := range page.Lines {
		if !l.Covered || l.CoveredByReview != 1 || l.CoveredBy != "head-1" || l.CoveredConclusion != "NOT_ACCEPTED" || !l.CoveredAt.Equal(page.Review.ReviewedAt) {
			t.Errorf("line %d of %s: %+v", l.LineNo, l.ShipmentID, l)
		}
	}
	// a line committed after the review: listed, and no review covers it
	u.atTime("late", 3, end.Add(-2*time.Microsecond))
	page = u.listPage(week, 10, "")
	covered := map[int]bool{}
	for _, l := range page.Lines {
		covered[l.LineNo] = l.Covered
		if l.LineNo == 3 && (l.Covered || l.CoveredByReview != 0 || l.CoveredBy != "" || l.CoveredConclusion != "") {
			t.Errorf("the late line is covered: %+v", l)
		}
	}
	if len(page.Lines) != 3 || !covered[1] || !covered[2] || covered[3] || page.Review.Matches {
		t.Fatalf("the list after the late line: %v, %+v", covered, page.Review)
	}
	// reviewed again, without a note for ACCEPTED: the latest review that covers a line is the one told, and the earlier review keeps its own lines
	cmd2 := after.markCmd("review-2", "head-2", week)
	cmd2.Conclusion, cmd2.Note = "ACCEPTED", ""
	r2 := after.must(after.module.MarkAfterTheFactReviewed(after.ctx, cmd2))
	if r2.NewRevision != 2 || r2.Version != 3 {
		t.Fatalf("the second receipt: %+v", r2)
	}
	page = u.listPage(week, 10, "")
	if m := page.Review; m.Conclusion != "ACCEPTED" || m.Note != "" || m.ReviewNo != 2 || !m.Matches {
		t.Fatalf("the second mark: %+v", m)
	}
	for _, l := range page.Lines {
		if !l.Covered || l.CoveredByReview != 2 || l.CoveredBy != "head-2" || l.CoveredConclusion != "ACCEPTED" {
			t.Errorf("line %d: %+v", l.LineNo, l)
		}
	}
	if n1, n2 := after.reviewRows("after_the_fact_review_line", "AND review_no = 1"), after.reviewRows("after_the_fact_review_line", "AND review_no = 2"); n1 != 2 || n2 != 3 {
		t.Fatalf("the lines each review covers: %d and %d, want 2 and 3", n1, n2)
	}
	// paging keeps the coverage of each line
	first := u.listPage(week, 2, "")
	next := u.listPage(week, 2, first.NextAfter)
	if len(first.Lines) != 2 || len(next.Lines) != 1 || !first.Lines[1].Covered || !next.Lines[0].Covered {
		t.Fatalf("the pages: %+v %+v", first.Lines, next.Lines)
	}
}

func TestWhatStillWaitsForReviewIsEveryUncoveredLineOfAnEndedWeekAndEveryEndedWeekWithNoReview(t *testing.T) {
	u, week, start := afterWorld(t, 5)
	end := start.AddDate(0, 0, 7)
	weekB, weekC := domain.WeekOf(start.AddDate(0, 0, -7)), domain.WeekOf(start.AddDate(0, 0, -14))
	nextWeek := domain.WeekOf(end)
	u.atTime("b1", 3, start.AddDate(0, 0, -7).Add(time.Hour)) // week B has one line, week C none
	u.atTime("one", 1, start.Add(time.Hour))
	u.atTime("two", 2, start.Add(2*time.Hour))
	after := u.onClock(end.Add(time.Hour))

	// every ended week of the range with no review, and every listed line of an ended week that no review covers; the week that has not ended is left out
	p := after.waiting(weekC, nextWeek)
	if p.LegalEntityID != "LE" || p.FromWeek != weekC || p.ToWeek != nextWeek || p.AsOf.IsZero() || len(p.WeeksWithoutReview) != 3 || len(p.Lines) != 3 {
		t.Fatalf("what waits: %+v", p)
	}
	for i, want := range []struct {
		week  string
		lines int
	}{{weekC, 0}, {weekB, 1}, {week, 2}} {
		if w := p.WeeksWithoutReview[i]; w.Week != want.week || w.ListedLines != want.lines || !w.WeekEnd.Equal(w.WeekStart.AddDate(0, 0, 7)) {
			t.Errorf("week %d without a review: %+v, want %v", i, w, want)
		}
	}
	for i, want := range []string{weekB, week, week} {
		if l := p.Lines[i]; l.Week != want || l.Covered || l.SubmittedBy != "fin-2" || len(l.Reasons) == 0 {
			t.Errorf("pending line %d: %+v, want week %s", i, l, want)
		}
	}
	// a week of the range that has not ended yet is not asked of the reviewer
	if only := after.waiting(nextWeek, nextWeek); len(only.WeeksWithoutReview) != 0 || len(only.Lines) != 0 {
		t.Fatalf("a week that has not ended: %+v", only)
	}

	// week A is reviewed: its lines are covered; a line committed after the review is not, and its week still has a review
	cmd := after.markCmd("review-a", "head-1", week)
	after.must(after.module.MarkAfterTheFactReviewed(after.ctx, cmd))
	u.atTime("late", 4, end.Add(-2*time.Microsecond))
	p = after.waiting(weekC, nextWeek)
	if len(p.WeeksWithoutReview) != 2 || p.WeeksWithoutReview[0].Week != weekC || p.WeeksWithoutReview[1].Week != weekB || len(p.Lines) != 2 || p.Lines[0].Week != weekB ||
		p.Lines[1].Week != week || p.Lines[1].LineNo != 4 || p.Lines[1].Covered {
		t.Fatalf("after week A's review and a late line: %+v", p)
	}
	// reviewing A again and B leaves C, which has nothing listed
	again := after.markCmd("review-a2", "head-2", week)
	after.must(after.module.MarkAfterTheFactReviewed(after.ctx, again))
	after.must(after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("review-b", "head-1", weekB)))
	if p = after.waiting(weekC, nextWeek); len(p.WeeksWithoutReview) != 1 || p.WeeksWithoutReview[0].Week != weekC || p.WeeksWithoutReview[0].ListedLines != 0 || len(p.Lines) != 0 {
		t.Fatalf("after every review: %+v", p)
	}

	// the query's own refusals, each with its way out
	ask := api.ListAfterTheFactPending{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", FromWeek: weekC, ToWeek: nextWeek}
	for name, c := range map[string]func(*api.ListAfterTheFactPending){
		"a week that is not one":        func(q *api.ListAfterTheFactPending) { q.FromWeek = "2026-W54" },
		"the last before the first":     func(q *api.ListAfterTheFactPending) { q.FromWeek, q.ToWeek = nextWeek, weekC },
		"a range of 54 weeks":           func(q *api.ListAfterTheFactPending) { q.ToWeek = domain.WeekOf(start.AddDate(0, 0, 7*54)) },
		"no legal entity":               func(q *api.ListAfterTheFactPending) { q.LegalEntityID = "" },
		"a legal entity with no policy": func(q *api.ListAfterTheFactPending) { q.LegalEntityID = "LE-9" },
	} {
		q := ask
		c(&q)
		if _, err := after.module.ListAfterTheFactPending(after.ctx, q); !errors.Is(err, api.ErrRejected) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := after.module.ListAfterTheFactPending(after.ctx, api.ListAfterTheFactPending{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", FromWeek: weekC,
		ToWeek: domain.WeekOf(start.AddDate(0, 0, 7*52-14))}); err != nil {
		t.Errorf("a range of 53 weeks: %v", err)
	}
	// the permissions: the review's, by name, and ReadProfit, which the lines need
	build := func(allowed onlyActions) *finmodule.Module {
		m, err := finmodule.New(u.db, u.w, u.w, u.w, u.w, u.w, allowed, finmodule.WithClock(fixedClock{end.Add(time.Hour)}))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for name, c := range map[string]struct {
		m  *finmodule.Module
		ok bool
	}{"ReadProfit alone": {build(onlyActions{api.ActionReadProfit}), false}, "ReviewAfterTheFact alone": {build(onlyActions{api.ActionReviewAfterTheFact}), false},
		"both": {build(onlyActions{api.ActionReadProfit, api.ActionReviewAfterTheFact}), true}} {
		_, err := c.m.ListAfterTheFactPending(u.ctx, ask)
		if (err == nil) != c.ok || (!c.ok && !errors.Is(err, api.ErrUnauthorized)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := after.module.ListAfterTheFactPending(after.ctx, api.ListAfterTheFactPending{TenantID: u.tenant, LegalEntityID: "LE", FromWeek: weekC, ToWeek: nextWeek}); !errors.Is(err, api.ErrUnauthenticated) {
		t.Errorf("no actor: %v", err)
	}
}

// What the database keeps of the lines a review covers, each by a raw write on its own: the lines are written with their review, of its legal
// entity, stated by an apportionment of its week (not carried forward) and listed for a reason, and a review has as many as its size says, at commit.
// The world is the list's (TestTheAfterTheFactListOnPostgres): ap2 is listed, ap4 has a second person and a voucher, ap7 carries an attribution
// forward, and LE2 has an attribution of its own in the same week.
func TestTheDatabaseKeepsTheLinesAReviewCovers(t *testing.T) {
	u, week, start := afterWorld(t, 5)
	h := func(n int) time.Time { return start.Add(time.Duration(n) * time.Hour) }
	u.must(u.stateLine("ap2", "fin-2", "fin-2", h(1), 1, houseC(1, 5_000, 0, "", "CLAIM-1")))
	u.atTime("ap3", 2, h(2))
	u.must(u.stateLine("ap4", "ops-2", "fin-2", h(3), 3, houseC(3, 1_000, 0, "", "CLAIM-3")))
	u.must(u.stateLine("ap5", "ops-1", "ops-1", h(4), 4))
	u.must(u.stateLine("ap6", "ops-2", "fin-2", h(5), 5))
	u.must(u.stateLine("ap7", "ops-2", "ops-2", h(6), 1))
	u.otherEntity(h(7))
	other, err := u.module.GetAfterTheFactReview(u.ctx, api.GetAfterTheFactReview{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE2", Week: week, Limit: 10})
	if err != nil || len(other.Lines) != 1 {
		t.Fatalf("LE2's list: %+v, %v", other, err)
	}
	z := other.Lines[0]
	monday := start.UTC().Format("2006-01-02 15:04:05")
	weekB, mondayB := domain.WeekOf(start.AddDate(0, 0, 7)), start.AddDate(0, 0, 7).UTC().Format("2006-01-02 15:04:05")
	hex := strings.Repeat("a", 64)
	review := func(entity, wk, startAt string, no, size int) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.after_the_fact_review (tenant_id,legal_entity_id,week,review_no,week_start,reviewed_by,conclusion,note,list_size,list_digest,reviewed_at)
            VALUES ($1,'%s','%s',%d,'%s+00','head-1','ACCEPTED','a note',%d,'%s','2099-01-01 00:00:00+00')`, entity, wk, no, startAt, size, hex)
	}
	line := func(entity, wk, startAt string, no int, pool string, apportionment, lineNo int, shipment string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.after_the_fact_review_line (tenant_id,legal_entity_id,week,review_no,week_start,pool_id,apportionment_no,line_no,shipment_id)
            VALUES ($1,'%s','%s',%d,'%s+00','%s',%d,%d,'%s')`, entity, wk, no, startAt, pool, apportionment, lineNo, shipment)
	}
	check := func(name string, sql string, state, constraint string) {
		t.Helper()
		err := u.rawRun(sql)
		if got, named := sqlState(err); err == nil || got != state || named != constraint {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, got, named, state, constraint)
		}
	}
	ap2 := line("LE", week, monday, 1, "P-U", 2, 1, "SH-C")
	check("a review that covers fewer lines than its size", review("LE", week, monday, 1, 1), "23514", "after_the_fact_review_lines")
	check("a review that covers more lines than its size", review("LE", week, monday, 1, 0)+";\n"+ap2, "23514", "after_the_fact_review_lines")
	check("a line written apart from its review", ap2, "23514", "after_the_fact_line_with_review")
	check("a line stated in another week than the review's", review("LE", weekB, mondayB, 1, 1)+";\n"+line("LE", weekB, mondayB, 1, "P-U", 2, 1, "SH-C"), "23514", "after_the_fact_line_week")
	check("a line of another legal entity than the review's", review("LE", week, monday, 1, 1)+";\n"+line("LE", week, monday, 1, z.PoolID, z.ApportionmentNo, z.LineNo, z.ShipmentID), "23514", "after_the_fact_line_entity")
	check("a line carried forward, not stated", review("LE", week, monday, 1, 1)+";\n"+line("LE", week, monday, 1, "P-U", 7, 1, "SH-C"), "23514", "after_the_fact_line_carried")
	check("a line that is not on the list: a second person, a voucher, not by default", review("LE", week, monday, 1, 1)+";\n"+line("LE", week, monday, 1, "P-U", 4, 3, "SH-C"), "23514", "after_the_fact_line_reason")
	check("a line of an attribution that does not exist", review("LE", week, monday, 1, 1)+";\n"+line("LE", week, monday, 1, "P-U", 99, 1, "SH-C"), "23514", "after_the_fact_line_week")
	if n := u.reviewRows("after_the_fact_review", ""); n != 0 {
		t.Fatalf("a refused write left %d reviews", n)
	}
	// the same line, written as the domain writes it, with the review and as many as its size says, is accepted
	if err := u.rawRun(review("LE", week, monday, 1, 1) + ";\n" + ap2); err != nil {
		t.Fatalf("a review with the lines it says: %v", err)
	}
	if n := u.reviewRows("after_the_fact_review_line", ""); n != 1 {
		t.Fatalf("%d lines", n)
	}
	for _, stmt := range []string{`UPDATE cargo_fin.after_the_fact_review_line SET week = week WHERE tenant_id=$1`, `DELETE FROM cargo_fin.after_the_fact_review_line WHERE tenant_id=$1`,
		`UPDATE cargo_fin.after_the_fact_review SET conclusion = 'NOT_ACCEPTED' WHERE tenant_id=$1`} {
		if err := u.rawRun(stmt); err == nil {
			t.Errorf("%s: no refusal", stmt)
		} else if state, _ := sqlState(err); state != "23001" && state != "42501" {
			t.Errorf("%s: %v (state %s)", stmt, err, state)
		}
	}
	// the review's conclusion and note
	row := func(no int, conclusion, note string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.after_the_fact_review (tenant_id,legal_entity_id,week,review_no,week_start,reviewed_by,conclusion,note,list_size,list_digest,reviewed_at)
            VALUES ($1,'LE','%s',%d,'%s+00','head-1',%s,%s,0,'%s','2099-01-01 00:00:00+00')`, weekB, no, mondayB, conclusion, note, hex)
	}
	check("a conclusion that is not one of the two", row(1, "'REJECTED'", "'x'"), "23514", "after_the_fact_review_conclusion")
	check("no conclusion", row(1, "NULL", "'x'"), "23502", "")
	check("NOT_ACCEPTED without a note", row(1, "'NOT_ACCEPTED'", "NULL"), "23514", "after_the_fact_review_note_required")
	check("a note of 2001 characters", row(1, "'ACCEPTED'", "repeat('n',2001)"), "23514", "after_the_fact_review_note_length")
	if err := u.rawRun(row(1, "'ACCEPTED'", "NULL")); err != nil {
		t.Fatalf("an ACCEPTED review with no note: %v", err)
	}
	var enabled, forced bool
	err = u.fixture.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
          WHERE n.nspname = 'cargo_fin' AND c.relname = 'after_the_fact_review_line'`).Scan(&enabled, &forced)
	})
	if err != nil || !enabled || !forced {
		t.Errorf("row security enabled %v, forced %v, %v", enabled, forced, err)
	}
}
