package domain

import (
	"errors"
	"fmt"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.99: the cost review of a loss-making job. A person who sees a job's profit below zero asks for its costs to be
// reviewed, naming the profit version they saw; another person resolves the request. Neither step changes an amount.

// Refusals of the cost review, each with its way out.
var (
	ErrReviewJobNotOpen  = errors.New("fin: FIN_REVIEW_JOB_NOT_OPEN: the job is not OPEN; a cancelled job needs no review, ask about its shipment's open job")
	ErrReviewNotLoss     = errors.New("fin: FIN_REVIEW_NOT_A_LOSS: the job's profit is not below 0: not a loss, no review is needed")
	ErrReviewStale       = errors.New("fin: FIN_REVIEW_STALE: the profit changed since the version you saw: read it again (GetJobProfit) and request with its version")
	ErrReviewOpen        = errors.New("fin: FIN_REVIEW_OPEN: the job already has a cost review request")
	ErrReviewSameActor   = errors.New("fin: FIN_REVIEW_SAME_ACTOR: the person who requested a cost review does not resolve it: have another person with ResolveCostReview resolve it")
	ErrReviewClockBehind = errors.New("fin: FIN_REVIEW_CLOCK_BEHIND: the resolution is stamped earlier than its request, so the servers' clocks are apart: synchronise the servers' clocks (for example with NTP) and try again")
	ErrReviewResolved    = errors.New("fin: FIN_REVIEW_RESOLVED: the cost review request is resolved already: read it (GetCostReviewRequest); a new review is a new request")
)

// Outcomes of a resolution: finance corrected the costs through the ordinary commands, or found them right.
const (
	ReviewCostCorrected = "COST_CORRECTED"
	ReviewNoChange      = "NO_CHANGE"
)

// CostReviewResolution is how a request ended: who resolved it, the outcome, a note and when.
type CostReviewResolution struct {
	By      string
	Outcome string
	Note    string
	At      time.Time
}

// CostReviewState is a cost review request with its resolution, if it has one. The figures are the job's, in its
// functional currency, as the requester saw them; Cost is the job's cost and pool cost together, so that Revenue less
// Cost is Profit.
type CostReviewState struct {
	TenantID        TenantID
	No              int
	RequestID       string
	JobID           string
	ShipmentID      string
	LegalEntityID   string
	RequestedBy     string
	Reason          string
	Currency        kernel.CurrencyCode
	Revenue         int64
	Cost            int64
	Profit          int64
	ProfitBasis     string
	SettingsVersion int
	ProfitVersion   string
	RequestedAt     time.Time
	Resolution      *CostReviewResolution
}

// CostReview is a cost review request, the aggregate this step adds. Its rows are only inserted.
type CostReview struct {
	state   CostReviewState
	pending []Event
}

// CostReviewID is the id of the tenant's nth request.
func CostReviewID(no int) string { return fmt.Sprintf("CRR-%d", no) }

// ReviewFigures are the figures of a job's profit that a request records.
type ReviewFigures struct {
	Currency        kernel.CurrencyCode
	Revenue         int64
	Cost            int64
	Profit          int64
	ProfitBasis     string
	SettingsVersion int
	ProfitVersion   string
}

// RequestCostReview makes request number no for a job, as it stands now: the job is OPEN, its profit is below 0, the
// version the requester saw is the current one (figures.ProfitVersion), and no other request of the job is unresolved
// (open is its id, or empty). The caller holds the job's lock and read the figures under it.
func RequestCostReview(tenant TenantID, no int, job JobState, figures ReviewFigures, seen, open, reason, actor string, at time.Time) (*CostReview, error) {
	switch {
	case !text(reason):
		return nil, fmt.Errorf("%w: a reason is 1 to %d characters", ErrInvalid, maxText)
	case !identifier(actor) || at.IsZero() || no < 1 || seen == "":
		return nil, fmt.Errorf("%w: a cost review request needs a requester, a time, a number and the profit version seen", ErrInvalid)
	case job.State != JobOpen:
		return nil, fmt.Errorf("%w (job %s is %s)", ErrReviewJobNotOpen, job.JobID, job.State)
	case figures.Profit >= 0:
		return nil, fmt.Errorf("%w (job %s: profit %d %s)", ErrReviewNotLoss, job.JobID, figures.Profit, figures.Currency)
	case seen != figures.ProfitVersion:
		return nil, fmt.Errorf("%w (job %s)", ErrReviewStale, job.JobID)
	case open != "":
		return nil, fmt.Errorf("%w: request %s is open: wait for its resolution", ErrReviewOpen, open)
	}
	r := &CostReview{state: CostReviewState{TenantID: tenant, No: no, RequestID: CostReviewID(no), JobID: job.JobID, ShipmentID: job.ShipmentID,
		LegalEntityID: job.LegalEntityID, RequestedBy: actor, Reason: reason, Currency: figures.Currency, Revenue: figures.Revenue, Cost: figures.Cost,
		Profit: figures.Profit, ProfitBasis: figures.ProfitBasis, SettingsVersion: figures.SettingsVersion, ProfitVersion: figures.ProfitVersion, RequestedAt: at.UTC()}}
	r.pending = append(r.pending, Event{Type: "CostReviewRequested.v1", At: at.UTC(), TenantID: tenant, SubjectID: r.state.RequestID, LegalEntityID: job.LegalEntityID,
		Version: no, ShipmentID: job.ShipmentID, JobID: job.JobID})
	return r, nil
}

// RestoreCostReview rebuilds a stored request.
func RestoreCostReview(s CostReviewState) *CostReview { return &CostReview{state: s} }

// Resolve ends the request: by someone other than the requester, once.
func (r *CostReview) Resolve(outcome, note, actor string, at time.Time) error {
	switch {
	case outcome != ReviewCostCorrected && outcome != ReviewNoChange:
		return fmt.Errorf("%w: the outcome is %s or %s", ErrInvalid, ReviewCostCorrected, ReviewNoChange)
	case !text(note):
		return fmt.Errorf("%w: a note is 1 to %d characters", ErrInvalid, maxText)
	case !identifier(actor) || at.IsZero():
		return fmt.Errorf("%w: a resolution needs a resolver and a time", ErrInvalid)
	case r.state.Resolution != nil:
		return fmt.Errorf("%w (request %s, %s by %s)", ErrReviewResolved, r.state.RequestID, r.state.Resolution.Outcome, r.state.Resolution.By)
	case actor == r.state.RequestedBy:
		return fmt.Errorf("%w (request %s was made by %s)", ErrReviewSameActor, r.state.RequestID, actor)
	case at.Before(r.state.RequestedAt):
		return fmt.Errorf("%w (request %s was made at %s, this server's time is %s)", ErrReviewClockBehind, r.state.RequestID,
			r.state.RequestedAt.Format(time.RFC3339Nano), at.UTC().Format(time.RFC3339Nano))
	}
	r.state.Resolution = &CostReviewResolution{By: actor, Outcome: outcome, Note: note, At: at.UTC()}
	r.pending = append(r.pending, Event{Type: "CostReviewResolved.v1", At: at.UTC(), TenantID: r.state.TenantID, SubjectID: r.state.RequestID,
		LegalEntityID: r.state.LegalEntityID, Version: r.state.No, ShipmentID: r.state.ShipmentID, JobID: r.state.JobID})
	return nil
}

func (r *CostReview) State() CostReviewState { return r.state }
func (r *CostReview) Pending() []Event       { return append([]Event(nil), r.pending...) }
func (r *CostReview) Acknowledge()           { r.pending = nil }
