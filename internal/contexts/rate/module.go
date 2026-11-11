// Package rate assembles the RATE (Rate, Quote & Commercial Rules) bounded context. Code outside RATE
// uses it through package api; everything under internal/ is private to RATE.
package rate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	orgacl "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/adapters/org"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Module wires RATE's use cases to PostgreSQL through IDP-Bento-Go and to ORG's query side.
type Module struct {
	db      *bentopg.DB
	outbox  *bentooutbox.Store
	org     orgapi.Queries
	auth    api.Authorizer
	margins api.MarginPolicy
	clock   bento.SystemClock
	window  kernel.RecordWindow
}

// Option configures a module.
type Option func(*Module)

// WithRecordWindow sets how far a command's business time may lie before the time RATE records it, and how
// much clock skew after it is tolerated (T1). The default is kernel.DefaultRecordWindow: seven days and
// five minutes.
func WithRecordWindow(w kernel.RecordWindow) Option { return func(m *Module) { m.window = w } }

var (
	_ api.Commands            = (*Module)(nil)
	_ api.Queries             = (*Module)(nil)
	_ api.PricingBasisQueries = (*Module)(nil)
)

// New assembles RATE. org answers recipient checks; the composition root passes ORG's module.
func New(db *bentopg.DB, org orgapi.Queries, auth api.Authorizer, margins api.MarginPolicy, opts ...Option) (*Module, error) {
	if db == nil || org == nil || auth == nil || margins == nil {
		return nil, errors.New("rate: module needs a database, ORG queries, an authorizer and a margin policy")
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	m := &Module{db: db, outbox: outbox, org: org, auth: auth, margins: margins, window: kernel.DefaultRecordWindow}
	for _, opt := range opts {
		opt(m)
	}
	if !m.window.Valid() {
		return nil, errors.New("rate: the record window must not be negative")
	}
	return m, nil
}

func (m *Module) CreateQuote(ctx context.Context, cmd api.CreateQuote) (api.Receipt, error) {
	lines := make([]domain.Line, len(cmd.Lines))
	for i, l := range cmd.Lines {
		lines[i] = domain.Line{ChargeCode: domain.ChargeCode(l.ChargeCode), Sell: l.Sell, Cost: l.Cost}
	}
	var rates []domain.ExchangeRate
	for _, r := range cmd.ExchangeRates {
		rates = append(rates, domain.ExchangeRate{From: r.From, Rate: r.Rate, Scale: r.Scale, FromDigits: r.FromDigits,
			QuoteDigits: r.QuoteDigits, Source: r.Source, AsOf: stored(r.AsOf)})
	}
	t := cmd.Terms
	receipt, err := application.CreateQuote{
		Deps: m.deps(cmd.TenantID, cmd.CommandID), Customers: orgacl.Customers{ORG: m.org}, Margins: margins{m.margins},
	}.Handle(ctx, application.CreateQuoteCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), QuoteID: domain.QuoteID(cmd.QuoteID),
		LegalEntityID: domain.LegalEntityID(cmd.LegalEntityID), CustomerPartyID: cmd.CustomerPartyID,
		Recipient: domain.RecipientRole(cmd.RecipientRole), Currency: cmd.Currency, Lines: lines, ExchangeRates: rates,
		Terms:      domain.CommercialTerms{IncotermsRule: t.IncotermsRule, IncotermsVersion: t.IncotermsVersion, NamedPlace: t.NamedPlace, ServiceLevel: t.ServiceLevel},
		Validity:   kernel.ValidityInterval{From: stored(cmd.Validity.From), Until: stored(cmd.Validity.Until)},
		OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) ApproveQuotePrice(ctx context.Context, cmd api.ApproveQuotePrice) (api.Receipt, error) {
	receipt, err := application.ApproveQuotePrice{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.ApproveQuotePriceCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), QuoteID: domain.QuoteID(cmd.QuoteID),
		ExpectedRevision: cmd.ExpectedRevision, ReasonRef: cmd.ReasonRef, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) AcceptQuote(ctx context.Context, cmd api.AcceptQuote) (api.Receipt, error) {
	receipt, err := application.AcceptQuote{Deps: m.deps(cmd.TenantID, cmd.CommandID), Customers: orgacl.Customers{ORG: m.org}}.Handle(ctx, application.AcceptQuoteCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), QuoteID: domain.QuoteID(cmd.QuoteID),
		ExpectedRevision: cmd.ExpectedRevision, EvidenceKind: domain.EvidenceKind(cmd.EvidenceKind), EvidenceRef: cmd.EvidenceRef,
		OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) ExpireQuote(ctx context.Context, cmd api.ExpireQuote) (api.Receipt, error) {
	receipt, err := application.ExpireQuote{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.ExpireQuoteCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), QuoteID: domain.QuoteID(cmd.QuoteID),
		ExpectedRevision: cmd.ExpectedRevision, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) accepted(ctx context.Context, q api.GetQuoteSnapshot) (*domain.Quote, int64, error) {
	quote, revision, err := application.GetQuoteSnapshot{
		Tx: bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}, Quotes: postgres.QuoteReader{DB: m.db},
	}.Handle(ctx, domain.TenantID(q.TenantID), domain.QuoteID(q.QuoteID))
	if err != nil {
		return nil, 0, classify(err)
	}
	return quote, revision, nil
}

// GetPricingBasis answers PricingBasis v1 of purpose QUOTE_ACCEPTED, built from the accepted revision.
func (m *Module) GetPricingBasis(ctx context.Context, q api.GetPricingBasis) (api.PricingBasis, error) {
	quote, revision, err := m.accepted(ctx, api.GetQuoteSnapshot{TenantID: q.TenantID, QuoteID: q.QuoteID})
	if err != nil {
		return api.PricingBasis{}, err
	}
	s, a := quote.State(), quote.State().Acceptance
	out := api.PricingBasis{TenantID: string(s.TenantID), BasisID: fmt.Sprintf("QUOTE:%s@%d", s.QuoteID, revision), Purpose: api.BasisQuoteAccepted,
		QuoteID: string(s.QuoteID), QuoteRevision: revision, LegalEntityID: string(s.LegalEntityID),
		Customer: api.CustomerRef{PartyID: s.Customer.PartyID, PartyRevision: a.CustomerRevision}, Currency: s.Currency, AcceptedAt: a.AcceptedAt}
	for i, l := range s.Lines {
		out.Lines = append(out.Lines, api.PricingLine{LineNo: i + 1, ChargeCode: string(l.ChargeCode), Sell: l.Sell, Cost: l.Cost, Discount: l.Discount()})
	}
	for _, r := range s.ExchangeRates {
		out.ExchangeRates = append(out.ExchangeRates, api.ExchangeRate{From: r.From, Rate: r.Rate, Scale: r.Scale, FromDigits: r.FromDigits,
			QuoteDigits: r.QuoteDigits, Source: r.Source, AsOf: r.AsOf})
	}
	return out, nil
}

func (m *Module) GetQuoteSnapshotV2(ctx context.Context, q api.GetQuoteSnapshot) (api.QuoteSnapshotV2, error) {
	quote, revision, err := m.accepted(ctx, q)
	if err != nil {
		return api.QuoteSnapshotV2{}, err
	}
	total, _, err := quote.Totals()
	if err != nil {
		return api.QuoteSnapshotV2{}, classify(err)
	}
	converted, err := quote.Converted()
	if err != nil {
		return api.QuoteSnapshotV2{}, classify(err)
	}
	reasons, err := quote.ApprovalReasons()
	if err != nil {
		return api.QuoteSnapshotV2{}, classify(err)
	}
	s, a, t := quote.State(), quote.State().Acceptance, quote.State().Terms
	snapshot := api.QuoteSnapshotV2{
		TenantID: string(s.TenantID), QuoteID: string(s.QuoteID), QuoteRevision: revision, LegalEntityID: string(s.LegalEntityID),
		Customer: api.CustomerRef{PartyID: s.Customer.PartyID, PartyRevision: a.CustomerRevision}, RecipientRole: string(s.Recipient),
		Currency: s.Currency, Total: total, Rounding: domain.RoundingHalfUpPerLine, Validity: s.Validity,
		Terms:      api.CommercialTerms{IncotermsRule: t.IncotermsRule, IncotermsVersion: t.IncotermsVersion, NamedPlace: t.NamedPlace, ServiceLevel: t.ServiceLevel},
		AcceptedAt: a.AcceptedAt, AcceptanceRecordedAt: a.RecordedAt, AcceptanceEvidenceKind: string(a.EvidenceKind), AcceptanceEvidenceRef: a.EvidenceRef,
		ServiceLevelUnlisted: t.ServiceLevelUnlisted,
	}
	for i, l := range s.Lines {
		snapshot.Lines = append(snapshot.Lines, api.SnapshotLineV2{ChargeCode: string(l.ChargeCode), Amount: l.Sell, InQuoteCurrency: converted[i], Discount: l.Discount()})
	}
	for _, r := range s.ExchangeRates {
		snapshot.ExchangeRates = append(snapshot.ExchangeRates, api.ExchangeRate{From: r.From, Rate: r.Rate, Scale: r.Scale,
			FromDigits: r.FromDigits, QuoteDigits: r.QuoteDigits, Source: r.Source, AsOf: r.AsOf})
	}
	for _, r := range reasons {
		snapshot.PriceApprovalReasons = append(snapshot.PriceApprovalReasons, string(r))
	}
	if s.Approval != nil {
		snapshot.PriceApprovedBy, snapshot.PriceSelfApproved = s.Approval.ApprovedBy, s.Approval.SelfApproved
	}
	return snapshot, nil
}

// GetServiceLevel returns the listed service level with the code (C8).
func (m *Module) GetServiceLevel(ctx context.Context, q api.GetServiceLevel) (api.ServiceLevel, error) {
	if err := ctx.Err(); err != nil {
		return api.ServiceLevel{}, err
	}
	if strings.TrimSpace(q.TenantID) == "" {
		return api.ServiceLevel{}, fmt.Errorf("%w: no tenant", api.ErrUnauthenticated)
	}
	l, listed := domain.ListedServiceLevel(q.Code)
	if !listed {
		return api.ServiceLevel{}, fmt.Errorf("%w: service level %q is not on the list", api.ErrNotFound, q.Code)
	}
	return api.ServiceLevel{Code: l.Code, Name: l.Name}, nil
}

// ListServiceLevels returns the tenant's service-level list (C8).
func (m *Module) ListServiceLevels(ctx context.Context, q api.ListServiceLevels) ([]api.ServiceLevel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(q.TenantID) == "" {
		return nil, fmt.Errorf("%w: no tenant", api.ErrUnauthenticated)
	}
	list := make([]api.ServiceLevel, len(domain.DefaultServiceLevels))
	for i, l := range domain.DefaultServiceLevels {
		list[i] = api.ServiceLevel{Code: l.Code, Name: l.Name}
	}
	return list, nil
}

func (m *Module) GetQuoteSnapshot(ctx context.Context, q api.GetQuoteSnapshot) (api.QuoteSnapshot, error) {
	quote, revision, err := m.accepted(ctx, q)
	if err != nil {
		return api.QuoteSnapshot{}, err
	}
	total, _, err := quote.Totals()
	if err != nil {
		return api.QuoteSnapshot{}, classify(err)
	}
	s := quote.State()
	snapshot := api.QuoteSnapshot{
		TenantID: string(s.TenantID), QuoteID: string(s.QuoteID), QuoteRevision: revision, LegalEntityID: string(s.LegalEntityID),
		Customer: api.CustomerRef{PartyID: s.Customer.PartyID, PartyRevision: s.Acceptance.CustomerRevision},
		Total:    total, Validity: s.Validity, AcceptedAt: s.Acceptance.AcceptedAt, AcceptanceEvidenceRef: s.Acceptance.EvidenceRef,
	}
	for _, l := range s.Lines {
		snapshot.Lines = append(snapshot.Lines, api.SnapshotLine{ChargeCode: string(l.ChargeCode), Amount: l.Sell})
	}
	if s.Approval != nil {
		snapshot.PriceApprovedBy = s.Approval.ApprovedBy
	}
	return snapshot, nil
}

func (m *Module) deps(tenant, commandID string) application.Deps {
	return application.Deps{
		Tx:       bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Quotes:   postgres.QuoteStore{DB: m.db},
		Receipts: postgres.CommandReceipts{DB: m.db},
		Outbox:   postgres.Outbox{Writer: m.outbox, CommandID: commandID, RecordedAt: m.clock.Now()},
		Auth:     authorizer{m.auth},
		Clock:    m.clock,
		Window:   m.window,
	}
}

func scope(tenant, actor string) application.Scope {
	return application.Scope{TenantID: domain.TenantID(tenant), ActorID: actor}
}

// stored truncates to the microsecond precision of timestamptz, so a quote reads back exactly as it was
// priced.
func stored(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Microsecond)
}

func toReceipt(r application.Receipt, err error) (api.Receipt, error) {
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	return api.Receipt{CommandID: r.CommandID, QuoteID: string(r.QuoteID), NewRevision: r.NewRevision}, nil
}

type authorizer struct{ api.Authorizer }

func (a authorizer) RequireQuoteAction(ctx context.Context, s application.Scope, action application.QuoteAction, id domain.QuoteID) error {
	return a.AuthorizeQuoteAction(ctx, string(s.TenantID), s.ActorID, string(action), string(id))
}

type margins struct{ policy api.MarginPolicy }

func (m margins) MarginFloorBP(ctx context.Context, tenant domain.TenantID, entity domain.LegalEntityID) (int, error) {
	return m.policy.MarginFloorBP(ctx, string(tenant), string(entity))
}

// classify wraps known RATE, kernel and Bento causes in their api error kind and passes anything else
// through.
func classify(err error) error {
	var kind error
	switch {
	case errors.Is(err, application.ErrUnauthenticated):
		kind = api.ErrUnauthenticated
	case errors.Is(err, application.ErrUnauthorized), errors.Is(err, bento.ErrTenantScopeConflict):
		kind = api.ErrUnauthorized
	case errors.Is(err, application.ErrIdempotencyConflict):
		kind = api.ErrIdempotencyConflict
	case errors.Is(err, domain.ErrConflict), errors.Is(err, bentorepo.ErrConflict), errors.Is(err, bentorepo.ErrAlreadyExists):
		kind = api.ErrConflict
	case errors.Is(err, bentorepo.ErrNotFound):
		kind = api.ErrNotFound
	case errors.Is(err, domain.ErrInvalidQuote), errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrSelfApproval),
		errors.Is(err, domain.ErrOutsideValidity), errors.Is(err, application.ErrCustomerNotConfirmed),
		errors.Is(err, application.ErrQuoteNotAccepted), errors.Is(err, kernel.ErrCurrencyMismatch), errors.Is(err, kernel.ErrAmountOverflow),
		errors.Is(err, kernel.ErrOutsideRecordWindow):
		kind = api.ErrRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
