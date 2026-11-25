package fin

import (
	"context"
	"fmt"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.99: the profit pages and the cost review. The commands carry no business time: FIN records its own clock.

// maxPage is the largest page of ListJobProfits and ListCostReviewRequests; defaultScanWindow is how many jobs a
// loss-only page looks at before it answers (Module.scanWindow), so that one request cannot read a whole tenant.
const (
	maxPage           = 500
	defaultScanWindow = 2000
)

// authorizeRead asks the authorizer for ReadProfit of the actor, who must be named.
func (m *Module) authorizeRead(ctx context.Context, tenant, actor, subject string) error {
	if actor == "" {
		return classify(application.ErrUnauthenticated)
	}
	if err := m.auth.AuthorizeFinAction(ctx, tenant, actor, api.ActionReadProfit, subject); err != nil {
		return classify(fmt.Errorf("%w: %v", application.ErrUnauthorized, err))
	}
	return nil
}

// RequestCostReview asks for the review of a loss-making job.
func (m *Module) RequestCostReview(ctx context.Context, cmd api.RequestCostReview) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RequestCostReview(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRequestCostReview,
		cmd.JobID, 0, m.clock.Now(), cmd), application.ReviewAsk{JobID: cmd.JobID, ProfitVersion: cmd.ProfitVersion, Reason: cmd.Reason})
	return receipt(r, err)
}

// ResolveCostReview ends a cost review request.
func (m *Module) ResolveCostReview(ctx context.Context, cmd api.ResolveCostReview) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ResolveCostReview(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionResolveCostReview,
		cmd.RequestID, 0, m.clock.Now(), cmd), cmd.Outcome, cmd.Note)
	return receipt(r, err)
}

// ListJobProfits lists the profit of a legal entity's jobs that are not cancelled. FIN's rows are read in one
// snapshot; PHY's basis of each consolidation the jobs have a share in is read just after it, outside FIN's
// transaction (T3), once for each consolidation. A loss-only page looks at no more than the scan window of jobs.
func (m *Module) ListJobProfits(ctx context.Context, q api.ListJobProfits) (api.JobProfitPage, error) {
	if err := m.authorizeRead(ctx, q.TenantID, q.ActorID, q.LegalEntityID); err != nil {
		return api.JobProfitPage{}, err
	}
	if q.LegalEntityID == "" || q.Limit < 1 || q.Limit > maxPage {
		return api.JobProfitPage{}, classify(fmt.Errorf("%w: a legal entity and a page of 1 to %d", application.ErrInvalidQuery, maxPage))
	}
	window := q.Limit + 1
	if q.LossOnly {
		window = m.scanWindow
	}
	tenant := domain.TenantID(q.TenantID)
	d := m.deps(q.TenantID, "")
	var reads []application.ProfitRead
	var asOf time.Time
	err := d.Reader(ctx, func(txCtx context.Context) error {
		asOf = m.clock.Now().UTC().Truncate(time.Microsecond)
		rules, err := d.RuleSettingsOf(txCtx, tenant, q.LegalEntityID)
		if err != nil {
			return err
		}
		jobs, revisions, err := d.Jobs.PageOf(txCtx, tenant, q.LegalEntityID, q.After, window)
		if err != nil {
			return err
		}
		reads = make([]application.ProfitRead, len(jobs))
		for i, j := range jobs {
			if reads[i], err = d.ReadProfit(txCtx, tenant, j, revisions[i], rules); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return api.JobProfitPage{}, classify(err)
	}
	var shares []application.PoolShare
	for _, r := range reads {
		shares = append(shares, r.Shares...)
	}
	bases, err := d.FormerBases(ctx, tenant, shares)
	if err != nil {
		return api.JobProfitPage{}, classify(err)
	}
	page := api.JobProfitPage{LegalEntityID: q.LegalEntityID, AsOf: asOf, Rows: []api.JobProfitRow{}}
	for i, r := range reads {
		f := r.Figures(bases)
		if q.LossOnly && f.Profit >= 0 {
			continue
		}
		st := r.Job.State()
		money := func(n int64) kernel.Money { return kernel.Money{Currency: st.FunctionalCurrency, Minor: n} }
		page.Rows = append(page.Rows, api.JobProfitRow{JobID: st.JobID, ShipmentID: st.ShipmentID, CustomerPartyID: st.CustomerPartyID, State: st.State,
			Currency: st.FunctionalCurrency, Revenue: money(f.Revenue), Cost: money(f.Revenue - f.Profit), Profit: money(f.Profit), ProfitBasis: f.Basis,
			SettingsVersion: f.SettingsVersion, ProfitVersion: f.Version})
		if len(page.Rows) == q.Limit {
			if i < len(reads)-1 || len(reads) == window {
				page.NextAfter = st.JobID
			}
			return page, nil
		}
	}
	if len(reads) == window && len(reads) > 0 {
		page.NextAfter = reads[len(reads)-1].Job.State().JobID
	}
	return page, nil
}

func reviewView(r *domain.CostReview) api.CostReviewRequest {
	st := r.State()
	money := func(n int64) kernel.Money { return kernel.Money{Currency: st.Currency, Minor: n} }
	out := api.CostReviewRequest{RequestID: st.RequestID, JobID: st.JobID, ShipmentID: st.ShipmentID, LegalEntityID: st.LegalEntityID, RequestedBy: st.RequestedBy,
		RequestedAt: st.RequestedAt, Reason: st.Reason, Revenue: money(st.Revenue), Cost: money(st.Cost), Profit: money(st.Profit), ProfitBasis: st.ProfitBasis,
		SettingsVersion: st.SettingsVersion, ProfitVersion: st.ProfitVersion, State: api.ReviewOpen}
	if x := st.Resolution; x != nil {
		out.State = api.ReviewResolved
		out.Resolution = &api.CostReviewResolution{ResolvedBy: x.By, ResolvedAt: x.At, Outcome: x.Outcome, Note: x.Note}
	}
	return out
}

// GetCostReviewRequest reads a request with its resolution.
func (m *Module) GetCostReviewRequest(ctx context.Context, q api.GetCostReviewRequest) (api.CostReviewRequest, error) {
	if err := m.authorizeRead(ctx, q.TenantID, q.ActorID, q.RequestID); err != nil {
		return api.CostReviewRequest{}, err
	}
	d := m.deps(q.TenantID, "")
	var r *domain.CostReview
	err := d.Reader(ctx, func(txCtx context.Context) (err error) {
		r, err = d.CostReviews.Read(txCtx, domain.TenantID(q.TenantID), q.RequestID)
		return err
	})
	if err != nil {
		return api.CostReviewRequest{}, classify(err)
	}
	return reviewView(r), nil
}

// GetJobLegalEntity answers the legal entity of a job (FIN V2.101).
func (m *Module) GetJobLegalEntity(ctx context.Context, q api.GetJobLegalEntity) (string, error) {
	if err := m.authorizeRead(ctx, q.TenantID, q.ActorID, q.JobID); err != nil {
		return "", err
	}
	j, _, err := m.job(ctx, q.TenantID, q.JobID)
	if err != nil {
		return "", classify(err)
	}
	return j.State().LegalEntityID, nil
}

// ListCostReviewRequests lists requests in the order made.
func (m *Module) ListCostReviewRequests(ctx context.Context, q api.ListCostReviewRequests) (api.CostReviewPage, error) {
	if err := m.authorizeRead(ctx, q.TenantID, q.ActorID, q.JobID); err != nil {
		return api.CostReviewPage{}, err
	}
	filter := application.CostReviewFilter{JobID: q.JobID, Limit: q.Limit + 1}
	switch q.State {
	case "":
	case api.ReviewOpen, api.ReviewResolved:
		resolved := q.State == api.ReviewResolved
		filter.Resolved = &resolved
	default:
		return api.CostReviewPage{}, classify(fmt.Errorf("%w: the state is %s or %s", application.ErrInvalidQuery, api.ReviewOpen, api.ReviewResolved))
	}
	if q.Limit < 1 || q.Limit > maxPage {
		return api.CostReviewPage{}, classify(fmt.Errorf("%w: a page of 1 to %d", application.ErrInvalidQuery, maxPage))
	}
	if q.After != "" {
		var no int
		if _, err := fmt.Sscanf(q.After, "CRR-%d", &no); err != nil || domain.CostReviewID(no) != q.After {
			return api.CostReviewPage{}, classify(fmt.Errorf("%w: after is a request id, CRR-<n>", application.ErrInvalidQuery))
		}
		filter.After = no
	}
	d := m.deps(q.TenantID, "")
	var found []*domain.CostReview
	err := d.Reader(ctx, func(txCtx context.Context) (err error) {
		found, err = d.CostReviews.List(txCtx, domain.TenantID(q.TenantID), filter)
		return err
	})
	if err != nil {
		return api.CostReviewPage{}, classify(err)
	}
	page := api.CostReviewPage{Requests: []api.CostReviewRequest{}}
	for i, r := range found {
		if i == q.Limit {
			page.NextAfter = page.Requests[len(page.Requests)-1].RequestID
			break
		}
		page.Requests = append(page.Requests, reviewView(r))
	}
	return page, nil
}
