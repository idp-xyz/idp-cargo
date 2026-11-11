// Package fin assembles the FIN (Freight Job Economics) bounded context, V0.1: finance policies, freight jobs with
// estimated charges, and credit accounts with CreditStatus v1 for DOC. Code outside FIN uses it through package api;
// everything under internal/ is private to FIN.
package fin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulacl "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/adapters/ful"
	orgacl "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/adapters/org"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/adapters/postgres"
	rateacl "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/adapters/rate"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Module wires FIN's use cases to PostgreSQL through IDP-Bento-Go, to FUL's CostDriver v1, RATE's PricingBasis v1 and
// ORG's legal entities and party roles.
type Module struct {
	db        *bentopg.DB
	outbox    *bentooutbox.Store
	shipments application.Shipments
	pricing   application.Pricing
	directory application.Directory
	auth      api.Authorizer
	clock     bento.SystemClock
	window    kernel.RecordWindow
}

// Option configures a Module.
type Option func(*Module)

// WithRecordWindow sets how far a command's business time may lie before the time FIN records it (T1).
func WithRecordWindow(w kernel.RecordWindow) Option { return func(m *Module) { m.window = w } }

var (
	_ api.Commands            = (*Module)(nil)
	_ api.Queries             = (*Module)(nil)
	_ api.CreditStatusQueries = (*Module)(nil)
)

// New assembles FIN. ful answers CostDriver v1, rate PricingBasis v1 and org LegalEntityRef v1 and PartyRef v1; FIN
// writes to none of them.
func New(db *bentopg.DB, org orgacl.ORG, rate rateapi.PricingBasisQueries, ful fulapi.CostDriverQueries, auth api.Authorizer, opts ...Option) (*Module, error) {
	if db == nil || org == nil || rate == nil || ful == nil || auth == nil {
		return nil, errors.New("fin: module needs a database, ORG, RATE's pricing bases, FUL's cost drivers and an authorizer")
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	m := &Module{db: db, outbox: outbox, shipments: fulacl.Shipments{FUL: ful}, pricing: rateacl.Pricing{RATE: rate}, directory: orgacl.Directory{ORG: org},
		auth: auth, window: kernel.DefaultRecordWindow}
	for _, opt := range opts {
		opt(m)
	}
	if !m.window.Valid() {
		return nil, errors.New("fin: the record window must not be negative")
	}
	return m, nil
}

func stored(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Microsecond)
}

func (m *Module) command(id, tenant, actor, action, subject string, expected int64, at time.Time, payload any) application.Command {
	return application.Command{CommandID: id, Scope: application.Scope{TenantID: domain.TenantID(tenant), ActorID: actor}, Action: application.Action(action),
		Subject: subject, ExpectedRevision: expected, OccurredAt: stored(at), Payload: payload}
}

func account(legalEntity, debtor string) string { return legalEntity + "/" + debtor }

func (m *Module) OpenFinancePolicy(ctx context.Context, cmd api.OpenFinancePolicy) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).OpenPolicy(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionOpenFinancePolicy,
		cmd.LegalEntityID, 0, cmd.OccurredAt, cmd), cmd.FunctionalCurrency, cmd.BookCode)
	return receipt(r, err)
}

func (m *Module) RecordExchangeRate(ctx context.Context, cmd api.RecordExchangeRate) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordRate(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordExchangeRate,
		cmd.LegalEntityID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), domain.RateEntry{Type: cmd.RateType, From: cmd.From, Rate: cmd.Rate, Scale: cmd.Scale,
		FromDigits: cmd.FromDigits, ToDigits: cmd.ToDigits, ValidFrom: stored(cmd.ValidFrom), ValidUntil: stored(cmd.ValidUntil), Source: cmd.Source})
	return receipt(r, err)
}

func (m *Module) OpenFreightJob(ctx context.Context, cmd api.OpenFreightJob) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).OpenJob(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionOpenFreightJob, cmd.JobID, 0,
		cmd.OccurredAt, cmd), cmd.ShipmentID)
	return receipt(r, err)
}

func (m *Module) ApplyPricingBasis(ctx context.Context, cmd api.ApplyPricingBasis) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ApplyBasis(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionApplyPricingBasis, cmd.JobID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.QuoteID, cmd.OrderID)
	return receipt(r, err)
}

func side(s *api.ChargeSide) *domain.SideInput {
	if s == nil {
		return nil
	}
	return &domain.SideInput{PartyID: s.PartyID, Amount: s.Amount}
}

func (m *Module) RecordChargeLine(ctx context.Context, cmd api.RecordChargeLine) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordLine(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordChargeLine, cmd.JobID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), application.LineInput{ChargeCode: cmd.ChargeCode, Description: cmd.Description, Sell: side(cmd.Sell),
		Cost: side(cmd.Cost), Reason: cmd.Reason})
	return receipt(r, err)
}

func (m *Module) ReviseChargeSide(ctx context.Context, cmd api.ReviseChargeSide) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Revise(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionReviseChargeSide, cmd.JobID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), application.ReviseInput{LineNo: cmd.LineNo, Side: cmd.Side, Expected: cmd.ExpectedSideVersion,
		Value: domain.SideInput{PartyID: cmd.Value.PartyID, Amount: cmd.Value.Amount}, Reason: cmd.Reason})
	return receipt(r, err)
}

func (m *Module) CancelFreightJob(ctx context.Context, cmd api.CancelFreightJob) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).CancelJob(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionCancelFreightJob, cmd.JobID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Reason)
	return receipt(r, err)
}

func (m *Module) SetCreditTerms(ctx context.Context, cmd api.SetCreditTerms) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).SetTerms(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionSetCreditTerms,
		account(cmd.LegalEntityID, cmd.DebtorPartyID), cmd.ExpectedRevision, cmd.OccurredAt, cmd), application.TermsInput{LegalEntityID: cmd.LegalEntityID,
		Debtor: cmd.DebtorPartyID, Limit: cmd.Limit, Days: cmd.PaymentDays, Basis: cmd.PaymentBasis})
	return receipt(r, err)
}

func (m *Module) PlaceCreditHold(ctx context.Context, cmd api.PlaceCreditHold) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Hold(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionPlaceCreditHold,
		account(cmd.LegalEntityID, cmd.DebtorPartyID), cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.LegalEntityID, cmd.DebtorPartyID, true, cmd.Code, cmd.Reason)
	return receipt(r, err)
}

func (m *Module) ReleaseCreditHold(ctx context.Context, cmd api.ReleaseCreditHold) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Hold(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionReleaseCreditHold,
		account(cmd.LegalEntityID, cmd.DebtorPartyID), cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.LegalEntityID, cmd.DebtorPartyID, false, "", cmd.Reason)
	return receipt(r, err)
}

func (m *Module) GetFinancePolicy(ctx context.Context, q api.GetFinancePolicy) (api.FinancePolicy, error) {
	var p *domain.FinancePolicy
	var revision int64
	err := m.deps(q.TenantID, "").Reader(ctx, func(txCtx context.Context) (err error) {
		p, revision, err = postgres.PolicyStore{DB: m.db}.Read(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID)
		return err
	})
	if err != nil {
		return api.FinancePolicy{}, classify(err)
	}
	st := p.State()
	out := api.FinancePolicy{LegalEntityID: st.LegalEntityID, Revision: revision, FunctionalCurrency: st.FunctionalCurrency, BookCode: st.BookCode}
	for _, e := range st.Rates {
		out.Rates = append(out.Rates, api.ExchangeRateEntry{EntryNo: e.No, RateType: e.Type, From: e.From, Rate: e.Rate, Scale: e.Scale, FromDigits: e.FromDigits,
			ToDigits: e.ToDigits, ValidFrom: e.ValidFrom, ValidUntil: e.ValidUntil, Source: e.Source, RecordedBy: e.By, RecordedAt: e.At})
	}
	return out, nil
}

func (m *Module) job(ctx context.Context, tenant, id string) (*domain.FreightJob, int64, error) {
	var j *domain.FreightJob
	var revision int64
	err := m.deps(tenant, "").Reader(ctx, func(txCtx context.Context) (err error) {
		j, revision, err = postgres.JobStore{DB: m.db}.Read(txCtx, domain.TenantID(tenant), id)
		return err
	})
	return j, revision, err
}

func (m *Module) GetFreightJob(ctx context.Context, q api.GetFreightJob) (api.FreightJob, error) {
	j, revision, err := m.job(ctx, q.TenantID, q.JobID)
	if err != nil {
		return api.FreightJob{}, classify(err)
	}
	return jobView(j, revision), nil
}

func sideViews(vs []domain.SideVersion, currency kernel.CurrencyCode) []api.SideVersion {
	var out []api.SideVersion
	for _, v := range vs {
		sv := api.SideVersion{Version: v.No, PartyID: v.PartyID, Amount: v.Amount, Local: kernel.Money{Currency: currency, Minor: v.Local}, Status: v.Status,
			Reason: v.Reason, RecordedBy: v.By, RecordedAt: v.At}
		if fx := v.FX; fx != nil {
			sv.FX = &api.FxSnapshot{EntryNo: fx.EntryNo, RateType: fx.Type, Rate: fx.Rate, Scale: fx.Scale, FromDigits: fx.FromDigits, ToDigits: fx.ToDigits,
				Source: fx.Source}
		}
		out = append(out, sv)
	}
	return out
}

func jobView(j *domain.FreightJob, revision int64) api.FreightJob {
	st := j.State()
	out := api.FreightJob{TenantID: string(st.TenantID), JobID: st.JobID, Revision: revision, LegalEntityID: st.LegalEntityID, ShipmentID: st.ShipmentID,
		Kind: st.Kind, FunctionalCurrency: st.FunctionalCurrency, BookCode: st.BookCode, CustomerPartyID: st.CustomerPartyID, PlanVersion: st.PlanVersion,
		State: st.State, OpenedAt: st.OpenedAt, CancelReason: st.CancelReason, CancelledAt: st.CancelledAt}
	for _, b := range st.Bases {
		out.Bases = append(out.Bases, api.AppliedBasis{BasisNo: b.No, BasisID: b.BasisID, QuoteID: b.QuoteID, QuoteRevision: b.QuoteRevision, OrderID: b.OrderID,
			AppliedBy: b.By, AppliedAt: b.At})
	}
	for _, l := range st.Lines {
		out.Lines = append(out.Lines, api.ChargeLine{LineNo: l.No, ChargeCode: l.ChargeCode, Description: l.Description, Source: l.Source, BasisNo: l.BasisNo,
			QuoteLineNo: l.QuoteLineNo, Reason: l.Reason, Sell: sideViews(l.Sell, st.FunctionalCurrency), Cost: sideViews(l.Cost, st.FunctionalCurrency)})
	}
	return out
}

func (m *Module) GetJobProfit(ctx context.Context, q api.GetJobProfit) (api.JobProfit, error) {
	j, revision, err := m.job(ctx, q.TenantID, q.JobID)
	if err != nil {
		return api.JobProfit{}, classify(err)
	}
	p := j.Profit()
	money := func(n int64) kernel.Money { return kernel.Money{Currency: p.Currency, Minor: n} }
	out := api.JobProfit{JobID: q.JobID, Revision: revision, Revenue: money(p.Revenue), Cost: money(p.Cost), Profit: money(p.Profit)}
	for _, c := range p.Lines {
		out.ByCode = append(out.ByCode, api.ChargeCodeProfit{ChargeCode: c.ChargeCode, Revenue: money(c.Revenue), Cost: money(c.Cost), Profit: money(c.Profit)})
	}
	return out, nil
}

func (m *Module) ListJobsForShipment(ctx context.Context, q api.ListJobsForShipment) ([]api.FreightJob, error) {
	var jobs []*domain.FreightJob
	var revisions []int64
	err := m.deps(q.TenantID, "").Reader(ctx, func(txCtx context.Context) (err error) {
		jobs, revisions, err = postgres.JobStore{DB: m.db}.ForShipment(txCtx, domain.TenantID(q.TenantID), q.ShipmentID)
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	out := []api.FreightJob{}
	for i, j := range jobs {
		out = append(out, jobView(j, revisions[i]))
	}
	return out, nil
}

func accountView(a *domain.CreditAccount, revision, exposure int64) api.CreditAccount {
	st := a.State()
	out := api.CreditAccount{LegalEntityID: st.LegalEntityID, DebtorPartyID: st.DebtorPartyID, Revision: revision, Status: a.Status(),
		Exposure: kernel.Money{Currency: st.Currency, Minor: exposure}}
	for _, t := range st.Terms {
		out.Terms = append(out.Terms, api.CreditTerms{Version: t.No, Limit: t.Limit, PaymentDays: t.PaymentDays, PaymentBasis: t.PaymentBasis, SetBy: t.By,
			SetAt: t.At})
	}
	for _, h := range st.Holds {
		out.Holds = append(out.Holds, api.CreditHold{HoldNo: h.No, Code: h.Code, Reason: h.Reason, PlacedBy: h.By, PlacedAt: h.At, ReleasedBy: h.ReleasedBy,
			ReleasedAt: h.ReleasedAt, ReleaseReason: h.ReleaseReason})
	}
	if t, ok := a.CurrentTerms(); ok {
		out.OverLimit = exposure > t.Limit.Minor
	}
	return out
}

func (m *Module) GetCreditAccount(ctx context.Context, q api.GetCreditAccount) (api.CreditAccount, error) {
	d := m.deps(q.TenantID, "")
	var a *domain.CreditAccount
	var revision, exposure int64
	err := d.Reader(ctx, func(txCtx context.Context) (err error) {
		if a, revision, err = d.Credit.Read(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.DebtorPartyID); err != nil {
			return err
		}
		exposure, err = d.Exposure(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.DebtorPartyID)
		return err
	})
	if err != nil {
		return api.CreditAccount{}, classify(err)
	}
	return accountView(a, revision, exposure), nil
}

func (m *Module) ListCreditExposure(ctx context.Context, q api.ListCreditExposure) ([]api.CreditAccount, error) {
	d := m.deps(q.TenantID, "")
	out := []api.CreditAccount{}
	err := d.Reader(ctx, func(txCtx context.Context) error {
		accounts, revisions, err := d.Credit.List(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID)
		if err != nil {
			return err
		}
		jobs, err := d.Jobs.OpenOf(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, "")
		if err != nil {
			return err
		}
		for i, a := range accounts {
			var exposure int64
			for _, j := range jobs {
				exposure += j.SellEstimate(a.State().DebtorPartyID)
			}
			out = append(out, accountView(a, revisions[i], exposure))
		}
		return nil
	})
	if err != nil {
		return nil, classify(err)
	}
	slices.SortStableFunc(out, func(a, b api.CreditAccount) int {
		switch {
		case a.OverLimit && !b.OverLimit:
			return -1
		case b.OverLimit && !a.OverLimit:
			return 1
		}
		return 0
	})
	return out, nil
}

// GetCreditStatus answers CreditStatus v1 for DOC.
func (m *Module) GetCreditStatus(ctx context.Context, q api.GetCreditStatus) (api.CreditStatus, error) {
	if q.TenantID == "" || q.LegalEntityID == "" || q.DebtorPartyID == "" {
		return api.CreditStatus{}, classify(application.ErrInvalidQuery)
	}
	d := m.deps(q.TenantID, "")
	var a *domain.CreditAccount
	var revision int64
	err := d.Reader(ctx, func(txCtx context.Context) (err error) {
		a, revision, err = d.Credit.Read(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.DebtorPartyID)
		return err
	})
	out := api.CreditStatus{LegalEntityID: q.LegalEntityID, DebtorPartyID: q.DebtorPartyID}
	if errors.Is(err, bentorepo.ErrNotFound) {
		out.Status = api.CreditNoAccount
		return out, nil
	}
	if err != nil {
		return api.CreditStatus{}, classify(err)
	}
	out.Status, out.Revision, out.DecisionID = a.Status(), revision, fmt.Sprintf("%s@%d", a.SubjectID(), revision)
	if h, ok := a.OpenHold(); ok {
		out.HoldCode, out.HoldReason = h.Code, h.Reason
	}
	return out, nil
}

func (m *Module) deps(tenant, commandID string) application.Deps {
	return application.Deps{
		Tx:        bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Policies:  postgres.PolicyStore{DB: m.db},
		Jobs:      postgres.JobStore{DB: m.db},
		Credit:    postgres.CreditStore{DB: m.db},
		Receipts:  postgres.CommandReceipts{DB: m.db},
		Outbox:    postgres.Outbox{Writer: m.outbox, CommandID: commandID, RecordedAt: m.clock.Now()},
		Auth:      authorizer{m.auth},
		Clock:     m.clock,
		Window:    m.window,
		Shipments: m.shipments,
		Pricing:   m.pricing,
		Directory: m.directory,
	}
}

func receipt(r application.Receipt, err error) (api.Receipt, error) {
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	return api.Receipt{CommandID: r.CommandID, SubjectID: r.SubjectID, NewRevision: r.NewRevision, Version: r.Version}, nil
}

type authorizer struct{ api.Authorizer }

func (a authorizer) RequireFinAction(ctx context.Context, s application.Scope, action application.Action, subject string) error {
	return a.AuthorizeFinAction(ctx, string(s.TenantID), s.ActorID, string(action), subject)
}

// classify wraps known FIN, kernel and Bento causes in their api error kind and passes anything else through.
func classify(err error) error {
	var kind error
	switch {
	case errors.Is(err, application.ErrUnauthenticated):
		kind = api.ErrUnauthenticated
	case errors.Is(err, application.ErrUnauthorized), errors.Is(err, bento.ErrTenantScopeConflict):
		kind = api.ErrUnauthorized
	case errors.Is(err, application.ErrIdempotencyConflict):
		kind = api.ErrIdempotencyConflict
	case errors.Is(err, domain.ErrConflict), errors.Is(err, bentorepo.ErrConflict), errors.Is(err, bentorepo.ErrAlreadyExists), postgres.Conflict(err):
		kind = api.ErrConflict
	case errors.Is(err, bentorepo.ErrNotFound):
		kind = api.ErrNotFound
	case errors.Is(err, domain.ErrInvalid), errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrRateMissing),
		errors.Is(err, domain.ErrRateOverlap), errors.Is(err, domain.ErrBasisApplied), errors.Is(err, domain.ErrNotPriceable),
		errors.Is(err, domain.ErrNotOpenable), errors.Is(err, domain.ErrPartyRole), errors.Is(err, domain.ErrSideVersion),
		errors.Is(err, application.ErrInvalidQuery), errors.Is(err, application.ErrUnknownSource), errors.Is(err, application.ErrNoPolicy),
		errors.Is(err, application.ErrNotVerified), errors.Is(err, kernel.ErrOutsideRecordWindow), errors.Is(err, kernel.ErrAmountOverflow):
		kind = api.ErrRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
