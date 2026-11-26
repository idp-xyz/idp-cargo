package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAWeekIsAnISOWeekFromMondayToMonday(t *testing.T) {
	for _, c := range []struct {
		week       string
		start, end string
	}{
		{"2026-W48", "2026-11-23T00:00:00Z", "2026-11-30T00:00:00Z"},
		{"2026-W01", "2025-12-29T00:00:00Z", "2026-01-05T00:00:00Z"}, // week 1 starts in the year before
		{"2026-W53", "2026-12-28T00:00:00Z", "2027-01-04T00:00:00Z"}, // 2026 has 53 weeks
		{"2027-W01", "2027-01-04T00:00:00Z", "2027-01-11T00:00:00Z"},
		{"2024-W09", "2024-02-26T00:00:00Z", "2024-03-04T00:00:00Z"}, // across a leap day
	} {
		start, end, err := ParseWeek(c.week)
		if err != nil || start.Format(time.RFC3339) != c.start || end.Format(time.RFC3339) != c.end || start.Weekday() != time.Monday {
			t.Errorf("%s: %v to %v, %v; want %s to %s", c.week, start, end, err, c.start, c.end)
		}
		if got := WeekOf(start); got != c.week {
			t.Errorf("WeekOf(%v) = %s, want %s", start, got, c.week)
		}
		if got := WeekOf(end.Add(-time.Nanosecond)); got != c.week {
			t.Errorf("WeekOf(%v) = %s, want %s", end.Add(-time.Nanosecond), got, c.week)
		}
		if got := WeekOf(end); got == c.week {
			t.Errorf("the end of %s is in it", c.week)
		}
	}
	for _, bad := range []string{"", "2026-W00", "2026-W54", "2027-W53", "2026-48", "2026W48", "2026-w48", "26-W48", "2026-W4", " 2026-W48", "2026-W48 ", "2026-W048", "2026-W99"} {
		if _, _, err := ParseWeek(bad); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "YYYY-Www") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestAnAttributionIsListedForEachOfItsReasons(t *testing.T) {
	base := ReviewLine{Voucher: "CLAIM-1", PreviewedBy: "ops-2", SubmittedBy: "fin-2"}
	for _, c := range []struct {
		name string
		edit func(l *ReviewLine)
		want []string
	}{
		{"a second person and a voucher", func(*ReviewLine) {}, nil},
		{"one person", func(l *ReviewLine) { l.PreviewedBy = "fin-2" }, []string{ReasonNoSecondPerson}},
		{"no voucher", func(l *ReviewLine) { l.Voucher = "" }, []string{ReasonNoVoucher}},
		{"a blank voucher", func(l *ReviewLine) { l.Voucher = "  " }, []string{ReasonNoVoucher}},
		{"by default", func(l *ReviewLine) { l.ByDefault = true }, []string{ReasonMadeByDefault}},
		{"two reasons", func(l *ReviewLine) { l.PreviewedBy, l.Voucher = "fin-2", "" }, []string{ReasonNoSecondPerson, ReasonNoVoucher}},
		{"all three", func(l *ReviewLine) { l.PreviewedBy, l.Voucher, l.ByDefault = "fin-2", "", true }, []string{ReasonNoSecondPerson, ReasonNoVoucher, ReasonMadeByDefault}},
		{"no previewer is not one person", func(l *ReviewLine) { l.PreviewedBy, l.SubmittedBy = "", "" }, nil},
	} {
		l := base
		c.edit(&l)
		if got := l.Reasons(); strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTheDigestOfAListTellsAnyChange(t *testing.T) {
	line := ReviewLine{ReviewKey: ReviewKey{PoolID: "P-1", ApportionmentNo: 2, LineNo: 1, ShipmentID: "SH-C"}, ConsolidationID: "C-1", JobID: "JOB-C", Currency: "USD",
		FunctionalCurrency: "CNY", Loss: 100, LossLocal: 700, Exception: 50, ExceptionLocal: 350, Party: PartyCarrier, Voucher: "V", PreviewNo: 1, SubmittedBy: "a", PreviewedBy: "b",
		EnteredAt: time.Date(2026, 11, 24, 1, 2, 3, 4000, time.UTC)}
	digest := ListDigest([]ReviewLine{line})
	if len(digest) != 64 || digest != ListDigest([]ReviewLine{line}) || digest == ListDigest(nil) || ListDigest(nil) != ListDigest([]ReviewLine{}) {
		t.Fatalf("the digests: %s, %s", digest, ListDigest(nil))
	}
	for name, edit := range map[string]func(l *ReviewLine){
		"pool": func(l *ReviewLine) { l.PoolID = "P-2" }, "apportionment": func(l *ReviewLine) { l.ApportionmentNo++ }, "line": func(l *ReviewLine) { l.LineNo++ },
		"house": func(l *ReviewLine) { l.ShipmentID = "SH-D" }, "consolidation": func(l *ReviewLine) { l.ConsolidationID = "C-2" }, "job": func(l *ReviewLine) { l.JobID = "JOB-D" },
		"currency": func(l *ReviewLine) { l.Currency = "EUR" }, "functional": func(l *ReviewLine) { l.FunctionalCurrency = "HKD" }, "loss": func(l *ReviewLine) { l.Loss++ },
		"loss local": func(l *ReviewLine) { l.LossLocal++ }, "exception": func(l *ReviewLine) { l.Exception++ }, "exception local": func(l *ReviewLine) { l.ExceptionLocal++ },
		"party": func(l *ReviewLine) { l.Party = PartyWarehouse }, "voucher": func(l *ReviewLine) { l.Voucher = "W" }, "default": func(l *ReviewLine) { l.ByDefault = true },
		"preview": func(l *ReviewLine) { l.PreviewNo++ }, "submitter": func(l *ReviewLine) { l.SubmittedBy = "c" }, "previewer": func(l *ReviewLine) { l.PreviewedBy = "c" },
		"time": func(l *ReviewLine) { l.EnteredAt = l.EnteredAt.Add(time.Microsecond) },
	} {
		other := line
		edit(&other)
		if ListDigest([]ReviewLine{other}) == digest {
			t.Errorf("a changed %s gives the same digest", name)
		}
	}
	if ListDigest([]ReviewLine{line, line}) == digest || ListDigest([]ReviewLine{line, {}}) == ListDigest([]ReviewLine{{}, line}) {
		t.Error("the size or the order of the list does not change the digest")
	}
}

func TestAReviewIsMadeByAnotherPersonOnceTheWeekHasEnded(t *testing.T) {
	week := "2026-W48"
	start, end, _ := ParseWeek(week)
	lines := []ReviewLine{
		{ReviewKey: ReviewKey{PoolID: "P-1", ApportionmentNo: 2, LineNo: 1, ShipmentID: "SH-C"}, SubmittedBy: "fin-2", PreviewedBy: "ops-2"},
		{ReviewKey: ReviewKey{PoolID: "P-1", ApportionmentNo: 3, LineNo: 2, ShipmentID: "SH-D"}, SubmittedBy: "fin-3", PreviewedBy: "fin-3"},
	}
	review := func(lines []ReviewLine, done *AfterTheFactReviewState, reviewer, note string, at time.Time) (*AfterTheFactReview, error) {
		return ReviewAfterTheFact("t", "LE", week, lines, done, reviewer, "ACCEPTED", note, at)
	}
	r, err := review(lines, nil, "head-1", "checked the vouchers", end.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	st := r.State()
	if st.ListSize != 2 || st.ListDigest != ListDigest(lines) || st.ReviewedBy != "head-1" || !st.WeekStart.Equal(start) || st.Week != week {
		t.Fatalf("the mark: %+v", st)
	}
	if e := r.Pending(); len(e) != 1 || e[0].Type != "AfterTheFactReviewed.v1" || e[0].SubjectID != "LE/2026-W48" || e[0].LegalEntityID != "LE" {
		t.Fatalf("the event: %+v", e)
	}
	r.Acknowledge()
	if len(r.Pending()) != 0 {
		t.Fatal("an acknowledged event is pending")
	}
	if e, err := review(nil, nil, "head-1", "nothing to review", end); err != nil || e.State().ListSize != 0 || e.State().ListDigest != ListDigest(nil) {
		t.Fatalf("a week with an empty list, at the instant it ends: %+v %v", e, err)
	}
	for _, c := range []struct {
		name     string
		reviewer string
		want     string
	}{{"the submitter of the first line", "fin-2", "fin-2 submitted apportionment 2 of pool P-1, which lists line 1 of house SH-C"},
		{"the previewer of the first line", "ops-2", "ops-2 previewed apportionment 2 of pool P-1, which lists line 1 of house SH-C"},
		{"who submitted and previewed the second", "fin-3", "fin-3 submitted and previewed apportionment 3 of pool P-1, which lists line 2 of house SH-D"}} {
		_, err := review(lines, nil, c.reviewer, "x", end.Add(time.Hour))
		if !errors.Is(err, ErrAfterTheFactReviewer) || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "another person reviews it") {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	done := r.State()
	if _, err = review(lines, &done, "head-2", "again", end.Add(time.Hour)); !errors.Is(err, ErrAfterTheFactReviewed) || !strings.Contains(err.Error(), "reviewed by head-1") {
		t.Errorf("a second review: %v", err)
	}
	if _, err = review(lines, nil, "head-1", "early", start.Add(-time.Nanosecond)); !errors.Is(err, ErrAfterTheFactBegun) {
		t.Errorf("a week that has not begun: %v", err)
	}
	// a week is reviewed from the Monday after it at 00:00 UTC, whatever zone the time is written in: 08:00 on that Monday in
	// China, 7:59:59.999999999 a moment before
	shanghai := time.FixedZone("CST", 8*3600)
	if _, err = review(lines, nil, "head-1", "x", end.Add(-time.Nanosecond)); !errors.Is(err, ErrAfterTheFactNotEnded) || !strings.Contains(err.Error(), "wait for the Monday after it") {
		t.Errorf("a week that has not ended, a moment before it: %v", err)
	}
	if _, err = review(lines, nil, "head-1", "x", start.Add(24*time.Hour)); !errors.Is(err, ErrAfterTheFactNotEnded) {
		t.Errorf("a week that is running: %v", err)
	}
	if _, err = review(lines, nil, "head-1", "x", end.In(shanghai).Add(-time.Nanosecond)); !errors.Is(err, ErrAfterTheFactNotEnded) {
		t.Errorf("a week that has not ended, in China time: %v", err)
	}
	if r, err = review(lines, nil, "head-1", "x", end.In(shanghai)); err != nil || !r.State().ReviewedAt.Equal(end) || r.State().ReviewedAt.Location() != time.UTC {
		t.Errorf("a week that has just ended, in China time (08:00): %+v, %v", r, err)
	}
	if _, err = review(nil, nil, "head-1", "x", end.Add(-8*time.Hour+time.Second).In(shanghai)); !errors.Is(err, ErrAfterTheFactNotEnded) { // 00:00:01 Monday in China is Sunday 16:00:01 UTC
		t.Errorf("a week that ends at 00:00 UTC, not at 00:00 in China: %v", err)
	}
	long := strings.Repeat("n", 2000)
	if _, err = review(nil, nil, "head-1", long, end); err != nil {
		t.Errorf("a note of %d characters: %v", MaxReviewNote, err)
	}
	if _, err = review(nil, nil, "head-1", strings.Repeat("语", 2000), end); err != nil {
		t.Errorf("a note of %d characters of 3 bytes: %v", MaxReviewNote, err)
	}
	for name, note := range map[string]string{"blank": "  \n", "long": long + "n"} {
		if _, err = review(nil, nil, "head-1", note, end); !errors.Is(err, ErrInvalid) {
			t.Errorf("a %s note: %v", name, err)
		}
	}
	// batch F: the note is given or left out for ACCEPTED, and required for NOT_ACCEPTED; the conclusion is one of the two
	if r, err = ReviewAfterTheFact("t", "LE", week, nil, nil, "head-1", ConclusionAccepted, "", end); err != nil || r.State().Note != "" || r.State().Conclusion != ConclusionAccepted {
		t.Errorf("an ACCEPTED review needs no note: %v", err)
	}
	for name, note := range map[string]string{"empty": "", "blank": " \n", "long": long + "n"} {
		if _, err = ReviewAfterTheFact("t", "LE", week, nil, nil, "head-1", ConclusionNotAccepted, note, end); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "note") {
			t.Errorf("a NOT_ACCEPTED review with a %s note: %v", name, err)
		}
	}
	if r, err = ReviewAfterTheFact("t", "LE", week, nil, nil, "head-1", ConclusionNotAccepted, "the vouchers are missing", end); err != nil || r.State().Conclusion != ConclusionNotAccepted {
		t.Errorf("a NOT_ACCEPTED review with its note: %v", err)
	}
	for _, c := range []string{"", "accepted", "REJECTED", " ACCEPTED"} {
		if _, err = ReviewAfterTheFact("t", "LE", week, nil, nil, "head-1", c, "x", end); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "ACCEPTED or NOT_ACCEPTED") {
			t.Errorf("the conclusion %q: %v", c, err)
		}
	}
	// the review records the lines it covers: the list at that time, in order
	if r, err = review(lines, nil, "head-1", "x", end); err != nil || len(r.Lines()) != len(lines) || r.Lines()[0] != lines[0].ReviewKey || r.Lines()[1] != lines[1].ReviewKey {
		t.Errorf("the lines of the review: %v %v", r.Lines(), err)
	}
	if _, err = review(nil, nil, " ", "x", end); !errors.Is(err, ErrInvalid) {
		t.Errorf("a blank reviewer: %v", err)
	}
	if _, err = ReviewAfterTheFact("t", "LE", "2026-W54", nil, nil, "head-1", "ACCEPTED", "x", end); !errors.Is(err, ErrInvalid) {
		t.Errorf("a week that is not one: %v", err)
	}
}

// V2.102 batch E: a week is reviewed again when its list differs from the latest review's (size or digest) and is refused only
// when it is the list already reviewed; each review has the next number, from 1 without a gap.
func TestAWeekIsReviewedAgainOnlyWhenItsListChanged(t *testing.T) {
	week := "2026-W48"
	_, end, _ := ParseWeek(week)
	line := func(no int, by string) ReviewLine {
		return ReviewLine{ReviewKey: ReviewKey{PoolID: "P-1", ApportionmentNo: no, LineNo: 1, ShipmentID: "SH-C"}, SubmittedBy: by, PreviewedBy: by}
	}
	one := []ReviewLine{line(1, "fin-2")}
	review := func(lines []ReviewLine, done *AfterTheFactReviewState, reviewer string) (*AfterTheFactReview, error) {
		return ReviewAfterTheFact("t", "LE", week, lines, done, reviewer, "ACCEPTED", "read", end.Add(time.Hour))
	}
	r1, err := review(one, nil, "head-1")
	if err != nil || r1.State().ReviewNo != 1 {
		t.Fatalf("the first review: %+v, %v", r1, err)
	}
	done := r1.State()
	// the list the latest review signed, whoever asks: nothing new
	for _, who := range []string{"head-1", "head-2"} {
		if _, err = review(one, &done, who); !errors.Is(err, ErrAfterTheFactReviewed) || !strings.Contains(err.Error(), "nothing new to review") || !strings.Contains(err.Error(), "review 1") {
			t.Errorf("the same list, by %s: %v", who, err)
		}
	}
	// a line added: the list changed in size
	two := []ReviewLine{line(1, "fin-2"), line(2, "fin-3")}
	r2, err := review(two, &done, "head-1")
	if err != nil || r2.State().ReviewNo != 2 || r2.State().ListSize != 2 || r2.State().ReviewedBy != "head-1" {
		t.Fatalf("the second review, a line added: %+v, %v", r2, err)
	}
	if e := r2.Pending(); len(e) != 1 || e[0].Version != 2 {
		t.Fatalf("the second review's event: %+v", e)
	}
	// a line changed with the size the same: the list changed in digest, and the week is reviewed again
	done = r2.State()
	changed := []ReviewLine{line(1, "fin-2"), line(2, "fin-4")}
	if ListDigest(changed) == done.ListDigest || len(changed) != done.ListSize {
		t.Fatal("the fixture: a list of the same size and another digest")
	}
	r3, err := review(changed, &done, "head-2")
	if err != nil || r3.State().ReviewNo != 3 || r3.State().ListDigest != ListDigest(changed) {
		t.Fatalf("the third review, a line changed with the same size: %+v, %v", r3, err)
	}
	// the bar on the reviewer applies to every line of the list, whoever reviewed before
	done = r3.State()
	if _, err = review([]ReviewLine{line(1, "fin-2"), line(2, "fin-4"), line(3, "head-9")}, &done, "head-9"); !errors.Is(err, ErrAfterTheFactReviewer) {
		t.Errorf("a reviewer who submitted a line of the changed list: %v", err)
	}
	// the week must have ended whatever its reviews
	if _, err = ReviewAfterTheFact("t", "LE", week, two, &done, "head-1", "ACCEPTED", "read", end.Add(-time.Nanosecond)); !errors.Is(err, ErrAfterTheFactNotEnded) {
		t.Errorf("a week that has not ended: %v", err)
	}
}
