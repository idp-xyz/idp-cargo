package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var reviewAt = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func lossJob() (JobState, ReviewFigures) {
	return JobState{JobID: "JOB-1", ShipmentID: "SH-1", LegalEntityID: "LE", State: JobOpen, FunctionalCurrency: "CNY"},
		ReviewFigures{Currency: "CNY", Revenue: 1000, Cost: 1500, Profit: -500, ProfitBasis: "SEPARATE", SettingsVersion: 3, ProfitVersion: "v1"}
}

func TestACostReviewRequestRecordsTheFiguresItSaw(t *testing.T) {
	job, f := lossJob()
	r, err := RequestCostReview("t", 7, job, f, "v1", "", "the freight cost looks double", "alice", reviewAt)
	if err != nil {
		t.Fatal(err)
	}
	st := r.State()
	if st.RequestID != "CRR-7" || st.No != 7 || st.JobID != "JOB-1" || st.RequestedBy != "alice" || st.Profit != -500 || st.Cost != 1500 || st.ProfitVersion != "v1" || st.Resolution != nil {
		t.Fatalf("state: %+v", st)
	}
	if e := r.Pending(); len(e) != 1 || e[0].Type != "CostReviewRequested.v1" || e[0].SubjectID != "CRR-7" || e[0].JobID != "JOB-1" {
		t.Fatalf("events: %+v", e)
	}
}

func TestACostReviewRequestRefusals(t *testing.T) {
	job, f := lossJob()
	cancelled := job
	cancelled.State = JobCancelled
	zero := f
	zero.Profit, zero.Cost = 0, 1000
	gain := f
	gain.Profit, gain.Cost = 200, 800
	for _, c := range []struct {
		name   string
		job    JobState
		f      ReviewFigures
		seen   string
		open   string
		reason string
		want   error
		say    string
	}{
		{"cancelled", cancelled, f, "v1", "", "r", ErrReviewJobNotOpen, "ask about its shipment's open job"},
		{"zero profit", job, zero, "v1", "", "r", ErrReviewNotLoss, "not a loss, no review is needed"},
		{"profit", job, gain, "v1", "", "r", ErrReviewNotLoss, "no review"},
		{"stale", job, f, "v0", "", "r", ErrReviewStale, "read it again"},
		{"open", job, f, "v1", "CRR-2", "r", ErrReviewOpen, "request CRR-2 is open: wait for its resolution"},
		{"no reason", job, f, "v1", "", "  ", ErrInvalid, "reason"},
		{"long reason", job, f, "v1", "", strings.Repeat("x", 501), ErrInvalid, "reason"},
	} {
		_, err := RequestCostReview("t", 1, c.job, c.f, c.seen, c.open, c.reason, "alice", reviewAt)
		if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.say) {
			t.Errorf("%s: %v, want %v saying %q", c.name, err, c.want, c.say)
		}
	}
	if _, err := RequestCostReview("t", 1, job, f, "v1", "", strings.Repeat("x", 500), "alice", reviewAt); err != nil {
		t.Errorf("a reason of 500 characters: %v", err)
	}
}

func TestACostReviewIsResolvedOnceByAnotherPerson(t *testing.T) {
	job, f := lossJob()
	r, _ := RequestCostReview("t", 1, job, f, "v1", "", "why", "alice", reviewAt)
	r.Acknowledge()
	if err := r.Resolve(ReviewNoChange, "costs are right", "alice", reviewAt.Add(time.Hour)); !errors.Is(err, ErrReviewSameActor) || !strings.Contains(err.Error(), "another person") {
		t.Fatalf("the requester: %v", err)
	}
	if err := r.Resolve("DONE", "n", "bob", reviewAt.Add(time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unknown outcome: %v", err)
	}
	if err := r.Resolve(ReviewNoChange, "", "bob", reviewAt.Add(time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an empty note: %v", err)
	}
	if err := r.Resolve(ReviewNoChange, "x", "bob", reviewAt.Add(-time.Hour)); !errors.Is(err, ErrReviewClockBehind) || !strings.Contains(err.Error(), "synchronise the servers' clocks") {
		t.Fatalf("before the request: %v", err)
	}
	if err := r.Resolve(ReviewCostCorrected, "freight corrected", "bob", reviewAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if x := r.State().Resolution; x == nil || x.By != "bob" || x.Outcome != ReviewCostCorrected {
		t.Fatalf("resolution: %+v", x)
	}
	if e := r.Pending(); len(e) != 1 || e[0].Type != "CostReviewResolved.v1" {
		t.Fatalf("events: %+v", e)
	}
	if err := r.Resolve(ReviewNoChange, "again", "carol", reviewAt.Add(2*time.Hour)); !errors.Is(err, ErrReviewResolved) {
		t.Fatalf("a second resolution: %v", err)
	}
}
