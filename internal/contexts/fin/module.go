// Package fin assembles the FIN (Freight Job Economics) bounded context, V0.3: finance policies with tax treatments
// and number series, freight jobs with estimated charges and the debit and credit notes billing them, billing
// profiles, debtor receipts settling debit notes and debtor statements, and credit accounts with CreditStatus v1 for
// DOC. Code outside FIN uses it through package api;
// everything under internal/ is private to FIN.
package fin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	docacl "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/adapters/doc"
	fulacl "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/adapters/ful"
	orgacl "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/adapters/org"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/adapters/postgres"
	rateacl "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/adapters/rate"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/foundation/numbering"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Module wires FIN's use cases to PostgreSQL through IDP-Bento-Go, to FUL's CostDriver v1, RATE's PricingBasis v1,
// DOC's TransportDocumentSnapshot v1 and ORG's legal entities, party roles and party identity.
type Module struct {
	db        *bentopg.DB
	outbox    *bentooutbox.Store
	shipments application.Shipments
	pricing   application.Pricing
	directory application.Directory
	documents application.Documents
	auth      api.Authorizer
	clock     bento.SystemClock
	window    kernel.RecordWindow
	numbers   *numbering.Sequences
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

// New assembles FIN. ful answers CostDriver v1, rate PricingBasis v1, doc TransportDocumentSnapshot v1 and org
// LegalEntityRef v1, PartyRef v1 and party identity; FIN writes to none of them.
func New(db *bentopg.DB, org orgacl.ORG, rate rateapi.PricingBasisQueries, ful fulapi.CostDriverQueries, doc docapi.TransportDocumentSnapshotQueries,
	auth api.Authorizer, opts ...Option) (*Module, error) {
	if db == nil || org == nil || rate == nil || ful == nil || doc == nil || auth == nil {
		return nil, errors.New("fin: module needs a database, ORG, RATE's pricing bases, FUL's cost drivers, DOC's document snapshots and an authorizer")
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	m := &Module{db: db, outbox: outbox, shipments: fulacl.Shipments{FUL: ful}, pricing: rateacl.Pricing{RATE: rate}, directory: orgacl.Directory{ORG: org},
		documents: docacl.Documents{DOC: doc}, auth: auth, window: kernel.DefaultRecordWindow}
	if m.numbers, err = numbering.NewSequences(db, "cargo_fin"); err != nil {
		return nil, err
	}
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

func (m *Module) EndExchangeRate(ctx context.Context, cmd api.EndExchangeRate) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).EndRate(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionEndExchangeRate,
		cmd.LegalEntityID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.EntryNo, stored(cmd.ValidUntil), cmd.Reason)
	if postgres.RateInUse(err) {
		err = fmt.Errorf("%w: entry %d", domain.ErrRateInUse, cmd.EntryNo)
	}
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

func (m *Module) SetChargeTaxTreatment(ctx context.Context, cmd api.SetChargeTaxTreatment) (api.Receipt, error) {
	from, err := domain.ParseDate(cmd.EffectiveFrom)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).SetTax(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionSetChargeTaxTreatment,
		cmd.LegalEntityID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), domain.TaxTreatment{ChargeCode: cmd.ChargeCode, Treatment: cmd.Treatment,
		RateBP: cmd.RateBP, EffectiveFrom: from})
	if postgres.TaxInUse(err) {
		err = fmt.Errorf("%w: %s from %s", domain.ErrTaxInUse, cmd.ChargeCode, cmd.EffectiveFrom)
	}
	return receipt(r, err)
}

func (m *Module) RegisterInvoiceSeries(ctx context.Context, cmd api.RegisterInvoiceSeries) (api.Receipt, error) {
	var date time.Time
	if cmd.FirstPeriodDate != "" {
		var err error
		if date, err = domain.ParseDate(cmd.FirstPeriodDate); err != nil {
			return api.Receipt{}, classify(err)
		}
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RegisterSeries(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRegisterInvoiceSeries,
		cmd.LegalEntityID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), application.SeriesAsk{Kind: cmd.Kind, Template: cmd.Template,
		FirstNumber: cmd.FirstNumber, FirstPeriodDate: date})
	return receipt(r, err)
}

func (m *Module) SetBillingProfile(ctx context.Context, cmd api.SetBillingProfile) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).SetProfile(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionSetBillingProfile,
		cmd.DebtorPartyID, int64(cmd.ExpectedVersion), cmd.OccurredAt, cmd), domain.ProfileInput{BuyerKind: cmd.BuyerKind,
		RegisteredAddress: cmd.RegisteredAddress, RegisteredPhone: cmd.RegisteredPhone, BankName: cmd.BankName, BankAccount: cmd.BankAccount,
		EvidenceRef: cmd.EvidenceRef})
	return receipt(r, err)
}

func (m *Module) IssueDebitNote(ctx context.Context, cmd api.IssueDebitNote) (api.Receipt, error) {
	date, err := domain.ParseDate(cmd.InvoiceDate)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).IssueDebitNote(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionIssueDebitNote, cmd.JobID,
		cmd.ExpectedRevision, cmd.OccurredAt, cmd), application.DebitNoteAsk{ID: cmd.InvoiceID, Debtor: cmd.DebtorPartyID,
		LineNos: append([]int(nil), cmd.LineNos...), Currency: cmd.Currency, InvoiceDate: date})
	return receipt(r, err)
}

func (m *Module) IssueCreditNote(ctx context.Context, cmd api.IssueCreditNote) (api.Receipt, error) {
	date, err := domain.ParseDate(cmd.InvoiceDate)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).IssueCreditNote(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionIssueCreditNote,
		cmd.JobID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), domain.CreditNoteInput{ID: cmd.InvoiceID, DebitNoteID: cmd.DebitNoteID,
		LineNos: append([]int(nil), cmd.LineNos...), Reason: cmd.Reason, InvoiceDate: date, WriteOff: cmd.WriteOff})
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
			ToDigits: e.ToDigits, ValidFrom: e.ValidFrom, ValidUntil: e.ValidUntil, Source: e.Source, RecordedBy: e.By, RecordedAt: e.At, EndedBy: e.EndedBy,
			EndReason: e.EndReason})
	}
	for _, t := range st.Taxes {
		out.TaxTreatments = append(out.TaxTreatments, api.ChargeTaxTreatment{Version: t.No, ChargeCode: t.ChargeCode, Treatment: t.Treatment, RateBP: t.RateBP,
			EffectiveFrom: t.EffectiveFrom.Format(time.DateOnly), SetBy: t.By, SetAt: t.At})
	}
	for _, x := range st.Series {
		series := api.InvoiceSeries{Kind: x.Kind, Template: x.Template, FirstNumber: x.FirstNumber}
		if err = m.deps(q.TenantID, "").Reader(ctx, func(txCtx context.Context) error {
			last, err := m.numbers.Last(txCtx, q.TenantID, numbering.DocType(x.Kind, st.LegalEntityID))
			for period, n := range last {
				series.Last = append(series.Last, api.NumberPeriod{Period: period, Last: n})
			}
			return err
		}); err != nil {
			return api.FinancePolicy{}, classify(err)
		}
		slices.SortFunc(series.Last, func(a, b api.NumberPeriod) int { return strings.Compare(a.Period, b.Period) })
		out.Series = append(out.Series, series)
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
		out = append(out, api.SideVersion{Version: v.No, PartyID: v.PartyID, Amount: v.Amount, Local: kernel.Money{Currency: currency, Minor: v.Local},
			Status: v.Status, Reason: v.Reason, InvoiceID: v.InvoiceID, InvoiceLineNo: v.InvoiceLineNo, ConvertedAt: v.ConvertedAt, RecordedBy: v.By,
			RecordedAt: v.At, FX: fxView(v.FX)})
	}
	return out
}

func fxView(fx *domain.FxSnapshot) *api.FxSnapshot {
	if fx == nil {
		return nil
	}
	return &api.FxSnapshot{EntryNo: fx.EntryNo, RateType: fx.Type, Rate: fx.Rate, Scale: fx.Scale, FromDigits: fx.FromDigits, ToDigits: fx.ToDigits,
		Source: fx.Source}
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
	for _, n := range st.Notes {
		out.Notes = append(out.Notes, api.NoteSummary{InvoiceID: n.ID, Kind: n.Kind, Number: n.Number, DebtorPartyID: n.DebtorPartyID, State: n.State,
			Total: kernel.Money{Currency: n.Currency, Minor: n.Total}})
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
	out := api.JobProfit{JobID: q.JobID, Revision: revision, Revenue: money(p.Revenue), Invoiced: money(p.Invoiced), Cost: money(p.Cost),
		Profit: money(p.Profit)}
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

// onAccount groups what receipts have unallocated by debtor.
func onAccount(rows []application.OnAccount) map[string][]kernel.Money {
	out := map[string][]kernel.Money{}
	for _, r := range rows {
		out[r.DebtorPartyID] = append(out[r.DebtorPartyID], r.Amount)
	}
	return out
}

func accountView(a *domain.CreditAccount, revision, unbilled, billed int64, onAccount []kernel.Money) api.CreditAccount {
	st := a.State()
	exposure := unbilled + billed
	out := api.CreditAccount{LegalEntityID: st.LegalEntityID, DebtorPartyID: st.DebtorPartyID, Revision: revision, Status: a.Status(),
		Exposure: kernel.Money{Currency: st.Currency, Minor: exposure}, Unbilled: kernel.Money{Currency: st.Currency, Minor: unbilled},
		Billed: kernel.Money{Currency: st.Currency, Minor: billed}, OnAccount: onAccount}
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
	var revision, unbilled, billed int64
	var money []application.OnAccount
	err := d.Reader(ctx, func(txCtx context.Context) (err error) {
		if a, revision, err = d.Credit.Read(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.DebtorPartyID); err != nil {
			return err
		}
		if unbilled, billed, err = d.Exposure(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.DebtorPartyID); err != nil {
			return err
		}
		money, err = d.DebtorReceipts.OnAccount(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.DebtorPartyID)
		return err
	})
	if err != nil {
		return api.CreditAccount{}, classify(err)
	}
	return accountView(a, revision, unbilled, billed, onAccount(money)[q.DebtorPartyID]), nil
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
		unbilled, billed := map[string]int64{}, map[string]int64{}
		var debtors []string
		for _, j := range jobs {
			for _, d := range j.Debtors() {
				if _, ok := unbilled[d]; !ok {
					debtors = append(debtors, d)
				}
				u, b := j.Exposure(d)
				unbilled[d], billed[d] = unbilled[d]+u, billed[d]+b
			}
		}
		rows, err := d.DebtorReceipts.OnAccount(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, "")
		if err != nil {
			return err
		}
		money := onAccount(rows)
		held := map[string]bool{}
		for i, a := range accounts {
			debtor := a.State().DebtorPartyID
			held[debtor] = true
			out = append(out, accountView(a, revisions[i], unbilled[debtor], billed[debtor], money[debtor]))
		}
		policy, found, err := d.Policies.Find(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID)
		if err != nil || !found {
			return err
		}
		currency := policy.State().FunctionalCurrency
		for debtor := range money { // a debtor with only money on account is listed too, at no exposure
			if _, exposed := unbilled[debtor]; !exposed {
				debtors = append(debtors, debtor)
			}
		}
		slices.Sort(debtors)
		for _, d := range debtors {
			if total := unbilled[d] + billed[d]; !held[d] && (total != 0 || len(money[d]) > 0) {
				out = append(out, api.CreditAccount{LegalEntityID: q.LegalEntityID, DebtorPartyID: d, Status: api.CreditNoAccount,
					Exposure: kernel.Money{Currency: currency, Minor: total}, Unbilled: kernel.Money{Currency: currency, Minor: unbilled[d]},
					Billed: kernel.Money{Currency: currency, Minor: billed[d]}, OnAccount: money[d]})
			}
		}
		return nil
	})
	if err != nil {
		return nil, classify(err)
	}
	rank := func(a api.CreditAccount) int {
		switch {
		case a.OverLimit:
			return 0
		case a.Status == api.CreditNoAccount:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(out, func(a, b api.CreditAccount) int { return rank(a) - rank(b) })
	return out, nil
}

func profileVersionView(v domain.ProfileVersion) api.BillingProfileVersion {
	return api.BillingProfileVersion{Version: v.No, BuyerKind: v.BuyerKind, Title: v.Title, TaxpayerNumber: v.TaxpayerNumber, PartyRevision: v.PartyRevision,
		RegisteredAddress: v.RegisteredAddress, RegisteredPhone: v.RegisteredPhone, BankName: v.BankName, BankAccount: v.BankAccount, EvidenceRef: v.EvidenceRef,
		SetBy: v.By, SetAt: v.At}
}

func (m *Module) GetBillingProfile(ctx context.Context, q api.GetBillingProfile) (api.BillingProfile, error) {
	d := m.deps(q.TenantID, "")
	var b *domain.BillingProfile
	var found bool
	err := d.Reader(ctx, func(txCtx context.Context) (err error) {
		b, found, err = d.Profiles.Read(txCtx, domain.TenantID(q.TenantID), q.DebtorPartyID)
		return err
	})
	if err == nil && !found {
		err = fmt.Errorf("%w: billing profile %s", bentorepo.ErrNotFound, q.DebtorPartyID)
	}
	if err != nil {
		return api.BillingProfile{}, classify(err)
	}
	out := api.BillingProfile{DebtorPartyID: q.DebtorPartyID}
	for _, v := range b.State().Versions {
		out.Versions = append(out.Versions, profileVersionView(v))
	}
	return out, nil
}

func dateView(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.DateOnly)
}

// noteView shows a note of a job with the profile version it froze.
func noteView(j *domain.FreightJob, n domain.Note, profile *domain.BillingProfile) api.DebtorInvoice {
	st := j.State()
	money := func(v int64) kernel.Money { return kernel.Money{Currency: n.Currency, Minor: v} }
	local := func(v int64) kernel.Money { return kernel.Money{Currency: st.FunctionalCurrency, Minor: v} }
	out := api.DebtorInvoice{InvoiceID: n.ID, Kind: n.Kind, Number: n.Number, JobID: st.JobID, LegalEntityID: st.LegalEntityID, ShipmentID: st.ShipmentID,
		DebtorPartyID: n.DebtorPartyID, DebtorRevision: n.DebtorRevision, Currency: n.Currency, InvoiceDate: dateView(n.InvoiceDate),
		DueDate: dateView(n.DueDate), PaymentDays: n.PaymentDays, PaymentBasis: n.PaymentBasis, TermsVersion: n.TermsVersion,
		Document: api.DocumentRef(n.Document), CreditsInvoiceID: n.CreditsID, Reason: n.Reason, WriteOff: n.WriteOff, State: n.State, Total: money(n.Total),
		LocalTotal: local(n.LocalTotal), IssuedBy: n.By, IssuedAt: n.At}
	if profile != nil {
		for _, v := range profile.State().Versions {
			if v.No == n.ProfileVersion {
				out.BillingProfile = profileVersionView(v)
			}
		}
	}
	for _, t := range n.TaxTotals() {
		out.TaxTotals = append(out.TaxTotals, api.TaxTotal{Treatment: t.Treatment, RateBP: t.RateBP, Amount: money(t.Invoice), Local: local(t.Local)})
	}
	for _, l := range n.Lines {
		out.Lines = append(out.Lines, api.DebtorInvoiceLine{LineNo: l.No, JobLineNo: l.JobLineNo, SideVersion: l.SideVersion, CreditsLineNo: l.CreditsLineNo,
			ChargeCode: l.ChargeCode, Amount: l.Amount, Invoiced: money(l.Invoice), Local: local(l.Local), FX: fxView(l.FX), TaxVersion: l.Tax.No,
			Treatment: l.Tax.Treatment, RateBP: l.Tax.RateBP})
	}
	if n.Kind == domain.KindDebitNote {
		for _, c := range j.Credits(n.ID) {
			out.CreditNotes = append(out.CreditNotes, c.ID)
		}
		remaining, _ := j.Remaining(n.ID)
		out.Settled, out.Open = money(n.Settled), money(remaining-n.Settled)
	}
	return out
}

func (m *Module) GetDebtorInvoice(ctx context.Context, q api.GetDebtorInvoice) (api.DebtorInvoice, error) {
	d := m.deps(q.TenantID, "")
	var out api.DebtorInvoice
	err := d.Reader(ctx, func(txCtx context.Context) error {
		j, _, err := d.Jobs.OfNote(txCtx, domain.TenantID(q.TenantID), q.InvoiceID)
		if err != nil {
			return err
		}
		for _, n := range j.State().Notes {
			if n.ID == q.InvoiceID {
				profile, _, err := d.Profiles.Read(txCtx, domain.TenantID(q.TenantID), n.DebtorPartyID)
				if err != nil {
					return err
				}
				out = noteView(j, n, profile)
				if n.Kind != domain.KindDebitNote {
					continue
				}
				settlements, err := d.DebtorReceipts.Settlements(txCtx, domain.TenantID(q.TenantID), n.ID)
				if err != nil {
					return err
				}
				for _, s := range settlements {
					out.Settlements = append(out.Settlements, api.InvoiceSettlement{ReceiptID: s.ReceiptID, AllocationNo: s.AllocationNo,
						Amount: kernel.Money{Currency: n.Currency, Minor: s.Amount}, Date: dateView(s.Date)})
				}
			}
		}
		return nil
	})
	if err != nil {
		return api.DebtorInvoice{}, classify(err)
	}
	return out, nil
}

func (m *Module) ListOpenDebtorItems(ctx context.Context, q api.ListOpenDebtorItems) ([]api.DebtorInvoice, error) {
	d := m.deps(q.TenantID, "")
	out := []api.DebtorInvoice{}
	err := d.Reader(ctx, func(txCtx context.Context) error {
		jobs, err := d.Jobs.BilledTo(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.DebtorPartyID)
		if err != nil {
			return err
		}
		profile, _, err := d.Profiles.Read(txCtx, domain.TenantID(q.TenantID), q.DebtorPartyID)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			for _, n := range j.State().Notes {
				if n.Kind == domain.KindDebitNote && n.State == domain.NoteIssued && n.DebtorPartyID == q.DebtorPartyID {
					if v := noteView(j, n, profile); v.Open.Minor != 0 {
						out = append(out, v)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, classify(err)
	}
	slices.SortStableFunc(out, func(a, b api.DebtorInvoice) int { return strings.Compare(a.DueDate+a.Number, b.DueDate+b.Number) })
	return out, nil
}

// ListDebitNotesOnChangedDocuments compares the house document each open debit note of the legal entity billed with
// the valid one DOC answers now, asking DOC after FIN's read.
func (m *Module) ListDebitNotesOnChangedDocuments(ctx context.Context, q api.ListDebitNotesOnChangedDocuments) ([]api.DocumentChange, error) {
	d := m.deps(q.TenantID, "")
	var jobs []*domain.FreightJob
	if err := d.Reader(ctx, func(txCtx context.Context) (err error) {
		jobs, err = d.Jobs.WithOpenDebitNotes(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID)
		return err
	}); err != nil {
		return nil, classify(err)
	}
	out := []api.DocumentChange{}
	current := map[string]*api.DocumentRef{}
	for _, j := range jobs {
		st := j.State()
		now, asked := current[st.ShipmentID]
		if !asked {
			h, err := m.documents.HouseDocument(ctx, domain.TenantID(q.TenantID), st.ShipmentID, time.Time{})
			switch {
			case errors.Is(err, domain.ErrNoIssuedDocument):
			case err != nil:
				return nil, classify(err)
			default:
				ref := api.DocumentRef(h.DocumentRef)
				now = &ref
			}
			current[st.ShipmentID] = now
		}
		for _, n := range st.Notes {
			if n.Kind != domain.KindDebitNote || n.State != domain.NoteIssued {
				continue
			}
			c := api.DocumentChange{InvoiceID: n.ID, Number: n.Number, JobID: st.JobID, ShipmentID: st.ShipmentID, DebtorPartyID: n.DebtorPartyID,
				Billed: api.DocumentRef(n.Document), Current: now}
			switch {
			case now == nil:
				c.Change = api.DocumentCancelled
			case now.DocumentID != n.Document.DocumentID:
				c.Change = api.DocumentReplaced
			case now.Version != n.Document.Version:
				c.Change = api.DocumentAmended
			default:
				continue
			}
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b api.DocumentChange) int { return strings.Compare(a.Number, b.Number) })
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
		Documents: m.documents,
		Numbers:   m.numbers,
		Profiles:  postgres.ProfileStore{DB: m.db},

		DebtorReceipts: postgres.ReceiptStore{DB: m.db},
	}
}

func receipt(r application.Receipt, err error) (api.Receipt, error) {
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	return api.Receipt{CommandID: r.CommandID, SubjectID: r.SubjectID, NewRevision: r.NewRevision, Version: r.Version, Number: r.Number}, nil
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
		errors.Is(err, domain.ErrNotOpenable), errors.Is(err, domain.ErrPartyRole), errors.Is(err, domain.ErrSideVersion), errors.Is(err, domain.ErrRateInUse),
		errors.Is(err, application.ErrInvalidQuery), errors.Is(err, application.ErrUnknownSource), errors.Is(err, application.ErrNoPolicy),
		errors.Is(err, application.ErrNotVerified), errors.Is(err, kernel.ErrOutsideRecordWindow), errors.Is(err, kernel.ErrAmountOverflow),
		errors.Is(err, domain.ErrInvoiced), errors.Is(err, domain.ErrNoIssuedDocument), errors.Is(err, domain.ErrNoBillingProfile),
		errors.Is(err, domain.ErrProfileStale), errors.Is(err, domain.ErrTaxMissing), errors.Is(err, domain.ErrTaxInUse), errors.Is(err, domain.ErrNoSeries),
		errors.Is(err, domain.ErrSeriesExhausted), errors.Is(err, numbering.ErrSeeded), errors.Is(err, domain.ErrSettled),
		errors.Is(err, domain.ErrOverAllocated), errors.Is(err, domain.ErrNotAllocatable), errors.Is(err, domain.ErrEvidenceUsed):
		kind = api.ErrRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
