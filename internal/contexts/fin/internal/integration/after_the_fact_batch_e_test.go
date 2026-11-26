//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
)

// V2.102 batch E (R2-F10): a command stamped inside a week can commit after the week's review, so that its line is listed and was
// never reviewed. A week is reviewed again when its list differs from the latest review's (size or digest) and is refused only
// when it is the list already reviewed. Each review keeps its own number, from 1 without a gap, its size and its digest; the bar on
// the reviewer applies to every line of the list; the query answers the latest review and whether it matches.
func TestAWeekWhoseListChangedIsReviewedAgain(t *testing.T) {
	u, week, start := afterWorld(t, 5)
	end := start.AddDate(0, 0, 7)
	u.atTime("one", 1, start.Add(time.Hour))
	after := u.onClock(end.Add(time.Hour))
	r1 := after.must(after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("review-1", "head-1", week)))
	if r1.NewRevision != 1 || r1.Version != 1 {
		t.Fatalf("the first review's receipt: %+v", r1)
	}
	// the list is the one already reviewed: nothing new, by anyone, whoever reviewed it before
	for _, who := range []string{"head-1", "head-2"} {
		_, err := after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("same-"+who, who, week))
		after.refused("the same list, by "+who, err, "FIN_AFTER_THE_FACT_REVIEWED", "nothing new to review", "review 1", "has not changed")
	}
	// a command stamped inside the week commits after the review: its line is listed and was never reviewed
	u.atTime("late", 2, end.Add(-2*time.Microsecond))
	m := u.listPage(week, 10, "").Review
	if !m.Reviewed || m.ReviewNo != 1 || m.ListSize != 1 || m.CurrentSize != 2 || m.Matches {
		t.Fatalf("the latest review does not match the list: %+v", m)
	}
	// the reviewer's bar applies to every line of the list, whoever reviewed before: fin-2 submitted the late line
	_, err := after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("bar", "fin-2", week))
	after.refused("a reviewer who submitted a line of the changed list", err, "FIN_AFTER_THE_FACT_REVIEWER", "another person reviews it")
	r2 := after.must(after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("review-2", "head-2", week)))
	if r2.NewRevision != 2 || r2.Version != 2 || r2.SubjectID != "LE/"+week {
		t.Fatalf("the second review's receipt: %+v", r2)
	}
	if m = u.listPage(week, 10, "").Review; !m.Reviewed || m.ReviewNo != 2 || m.ReviewedBy != "head-2" || m.ListSize != 2 || m.CurrentSize != 2 || !m.Matches {
		t.Fatalf("the query answers the latest review: %+v", m)
	}
	if again := after.must(after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("review-2", "head-2", week))); again != r2 {
		t.Fatalf("a replay of the second review: %+v, want %+v", again, r2)
	}
	_, err = after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("same-again", "head-1", week))
	after.refused("the second review's list again", err, "FIN_AFTER_THE_FACT_REVIEWED", "review 2", "nothing new to review")
	// a third review, by someone who reviewed the first, when the list changed again
	u.atTime("later", 3, end.Add(-time.Microsecond))
	r3 := after.must(after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("review-3", "head-1", week)))
	if r3.NewRevision != 3 || r3.Version != 3 {
		t.Fatalf("the third review's receipt: %+v", r3)
	}
	// each review keeps its own row, number, size and digest
	type row struct {
		no, size int
		by       string
		digest   string
	}
	var rows []row
	err = u.within(t, func(ctx context.Context, e bentopg.Executor) error {
		q, err := e.Query(ctx, `SELECT review_no, list_size, reviewed_by, list_digest FROM cargo_fin.after_the_fact_review WHERE tenant_id=$1 AND week=$2 ORDER BY review_no`, u.tenant, week)
		if err != nil {
			return err
		}
		defer q.Close()
		for q.Next() {
			var r row
			if err = q.Scan(&r.no, &r.size, &r.by, &r.digest); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return q.Err()
	})
	if err != nil || len(rows) != 3 || rows[0] != (row{1, 1, "head-1", rows[0].digest}) || rows[1].no != 2 || rows[1].size != 2 || rows[1].by != "head-2" || rows[2].no != 3 || rows[2].size != 3 ||
		rows[2].by != "head-1" || rows[0].digest == rows[1].digest || rows[1].digest == rows[2].digest || rows[0].digest == rows[2].digest {
		t.Fatalf("the reviews of the week: %+v, %v", rows, err)
	}
	if n := countOf(u.outboxTypes(t), "AfterTheFactReviewed"); n != 3 {
		t.Fatalf("%d events for three reviews", n)
	}
}

// What the database keeps of the numbering: a review is numbered the next after the latest of its legal entity and week, from 1
// without a gap, and is refused when its size and digest are those of the latest review; each week is numbered on its own.
func TestTheDatabaseNumbersTheReviewsOfAWeekAndRefusesTheListAlreadyReviewed(t *testing.T) {
	u, _, _ := afterWorld(t, 1)
	hexA, hexB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	insert := func(week, startAt string, no, size int, digest string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.after_the_fact_review (tenant_id,legal_entity_id,week,review_no,week_start,reviewed_by,note,list_size,list_digest,reviewed_at)
            VALUES ($1,'LE','%s',%d,'%s+00','head-1','a note',%d,'%s','2099-01-01 00:00:00+00')`, week, no, startAt, size, digest)
	}
	refused := func(name, sql, constraint string) {
		t.Helper()
		err := u.rawRun(sql)
		if state, got := sqlState(err); state != "23514" || got != constraint {
			t.Errorf("%s: %v (state %s, constraint %s), want 23514 %s", name, err, state, got, constraint)
		}
	}
	const w, monday, w2, monday2 = "2026-W48", "2026-11-23 00:00:00", "2026-W49", "2026-11-30 00:00:00"
	refused("a first review numbered 2", insert(w, monday, 2, 1, hexA), "after_the_fact_review_numbered")
	refused("a first review numbered 0", insert(w, monday, 0, 1, hexA), "after_the_fact_review_numbered")
	if err := u.rawRun(insert(w, monday, 1, 1, hexA)); err != nil {
		t.Fatal(err)
	}
	refused("a review numbered 3 after 1", insert(w, monday, 3, 2, hexB), "after_the_fact_review_numbered")
	refused("review 1 again", insert(w, monday, 1, 2, hexB), "after_the_fact_review_numbered")
	refused("the list already reviewed", insert(w, monday, 2, 1, hexA), "after_the_fact_review_list_changed")
	if err := u.rawRun(insert(w, monday, 2, 1, hexB)); err != nil { // the same size and another digest: the list changed
		t.Fatalf("a changed digest: %v", err)
	}
	if err := u.rawRun(insert(w, monday, 3, 2, hexB)); err != nil { // another size, the same digest as review 2
		t.Fatalf("a changed size: %v", err)
	}
	refused("the list of review 3 again", insert(w, monday, 4, 2, hexB), "after_the_fact_review_list_changed")
	if err := u.rawRun(insert(w, monday, 4, 1, hexA)); err != nil { // the list of review 1 is not the latest review's: it is a change
		t.Fatalf("a list that is not the latest review's: %v", err)
	}
	// another week is numbered on its own
	if err := u.rawRun(insert(w2, monday2, 1, 1, hexA)); err != nil {
		t.Fatalf("the first review of another week: %v", err)
	}
}
