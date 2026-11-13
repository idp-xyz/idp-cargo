// Package rate assembles the RATE (Rate, Quote & Commercial Rules) bounded context. Code outside RATE
// uses it through package api; everything under internal/ is private to RATE.
package rate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	ratehttp "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/adapters/http"
	orgacl "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/adapters/org"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
	refapi "go.idp.xyz/idp-cargo/internal/reference/api"
)

// Module wires RATE's use cases to PostgreSQL through IDP-Bento-Go and to ORG's query side.
type Module struct {
	db        *bentopg.DB
	outbox    *bentooutbox.Store
	org       orgapi.Queries
	auth      api.Authorizer
	margins   api.MarginPolicy
	clock     bento.SystemClock
	window    kernel.RecordWindow
	locations refapi.Locations
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

// RegisterHTTP registers RATE's inbound HTTP routes; the composition root calls it.
func (m *Module) RegisterHTTP(mux *http.ServeMux) { ratehttp.Register(mux, m) }

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
		lines[i] = domain.Line{ChargeCode: domain.ChargeCode(l.ChargeCode), Sell: l.Sell, Cost: l.Cost, Source: domain.LineSource(l.Source)}
		if l.SnapshotID != "" || l.OverrideReason != "" {
			lines[i].Rated = &domain.RatedRef{SnapshotID: l.SnapshotID, OverrideReason: l.OverrideReason}
		}
	}
	var quoteScope *domain.QuoteScope
	if sc := cmd.Scope; sc != nil {
		quoteScope = &domain.QuoteScope{Mode: domain.Mode(sc.Mode), ContainerMode: domain.ContainerMode(sc.ContainerMode), Origin: sc.Origin,
			Destination: sc.Destination, Via: sc.Via, Carrier: sc.Carrier, Cargo: domain.Cargo{Pieces: sc.Cargo.Pieces, GrossGrams: sc.Cargo.GrossGrams,
				VolumeCM3: sc.Cargo.VolumeCM3, Documents: sc.Cargo.Documents}}
	}
	var rates []domain.ExchangeRate
	for _, r := range cmd.ExchangeRates {
		rates = append(rates, domain.ExchangeRate{From: r.From, Rate: r.Rate, Scale: r.Scale, FromDigits: r.FromDigits,
			QuoteDigits: r.QuoteDigits, Source: r.Source, AsOf: stored(r.AsOf)})
	}
	var agreement *domain.AgreementRef
	if a := cmd.Agreement; a != nil {
		agreement = &domain.AgreementRef{RateCardID: domain.RateCardID(a.RateCardID), Version: a.Version}
	}
	t := cmd.Terms
	receipt, err := application.CreateQuote{
		Deps: m.deps(cmd.TenantID, cmd.CommandID), Customers: orgacl.Customers{ORG: m.org}, Margins: margins{m.margins},
		Codes: postgres.ChargeCodeStore{DB: m.db}, Snapshots: postgres.SnapshotStore{DB: m.db}, Cards: postgres.RateCardStore{DB: m.db},
		Agreements: postgres.QuoteStore{DB: m.db},
	}.Handle(ctx, application.CreateQuoteCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), QuoteID: domain.QuoteID(cmd.QuoteID),
		LegalEntityID: domain.LegalEntityID(cmd.LegalEntityID), CustomerPartyID: cmd.CustomerPartyID,
		Recipient: domain.RecipientRole(cmd.RecipientRole), Currency: cmd.Currency, Lines: lines, ExchangeRates: rates,
		Terms:      domain.CommercialTerms{IncotermsRule: t.IncotermsRule, IncotermsVersion: t.IncotermsVersion, NamedPlace: t.NamedPlace, ServiceLevel: t.ServiceLevel},
		Validity:   kernel.ValidityInterval{From: stored(cmd.Validity.From), Until: stored(cmd.Validity.Until)},
		QuoteScope: quoteScope, Kind: domain.QuoteKind(cmd.Kind), Agreement: agreement, OccurredAt: stored(cmd.OccurredAt),
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
	receipt, err := application.AcceptQuote{Deps: m.deps(cmd.TenantID, cmd.CommandID), Customers: orgacl.Customers{ORG: m.org},
		Cards: postgres.RateCardStore{DB: m.db}, Agreements: postgres.QuoteStore{DB: m.db}}.Handle(ctx, application.AcceptQuoteCommand{
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

// oneOff reads an accepted ONE_OFF quote: an AGREEMENT has no fixed lines or total, so QuoteSnapshot v1 and v2 and a
// QUOTE_ACCEPTED basis do not answer for it (QuoteSnapshotV3 does, and its shipments are rated with its customer card).
func (m *Module) oneOff(ctx context.Context, q api.GetQuoteSnapshot) (*domain.Quote, int64, error) {
	quote, revision, err := m.accepted(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	if quote.Kind() == domain.KindAgreement {
		return nil, 0, fmt.Errorf("%w: quote %s is an AGREEMENT: QuoteSnapshotV3 answers for it, and its shipments are rated with its customer card",
			api.ErrRejected, q.QuoteID)
	}
	return quote, revision, nil
}

// GetPricingBasis answers PricingBasis v1: of purpose QUOTE_ACCEPTED, built from the accepted revision, or a rated
// basis as issued.
func (m *Module) GetPricingBasis(ctx context.Context, q api.GetPricingBasis) (api.PricingBasis, error) {
	if q.BasisID != "" {
		if q.Shipment != nil || q.QuoteID != "" {
			return api.PricingBasis{}, fmt.Errorf("%w: a basis id, or a quote with an optional shipment", api.ErrRejected)
		}
		return m.ratedBasis(ctx, q)
	}
	quote, revision, err := m.oneOff(ctx, api.GetQuoteSnapshot{TenantID: q.TenantID, QuoteID: q.QuoteID})
	if err != nil {
		return api.PricingBasis{}, err
	}
	s, a := quote.State(), quote.State().Acceptance
	out := api.PricingBasis{TenantID: string(s.TenantID), BasisID: fmt.Sprintf("QUOTE:%s@%d", s.QuoteID, revision), Purpose: api.BasisQuoteAccepted,
		QuoteID: string(s.QuoteID), QuoteRevision: revision, LegalEntityID: string(s.LegalEntityID),
		Customer: api.CustomerRef{PartyID: s.Customer.PartyID, PartyRevision: a.CustomerRevision}, Currency: s.Currency, AcceptedAt: a.AcceptedAt}
	shipped := make([]domain.ShippedLine, len(s.Lines))
	for i, l := range s.Lines {
		shipped[i] = domain.ShippedLine{LineNo: i + 1, ChargeCode: l.ChargeCode, Sell: l.Sell, Cost: l.Cost}
	}
	if sh := q.Shipment; sh != nil {
		if sh.ShipmentID == "" || sh.ShipmentID != strings.TrimSpace(sh.ShipmentID) || strings.ContainsAny(sh.ShipmentID, "/@ \t\n") ||
			utf8.RuneCountInString(sh.ShipmentID) > 128 || sh.PlanVersion < 1 {
			return api.PricingBasis{}, fmt.Errorf("%w: a shipment id (no blanks, '/' or '@') and its plan version", api.ErrRejected)
		}
		c := sh.Cargo
		if err = m.reader(ctx, q.TenantID, func(txCtx context.Context) (err error) {
			shipped, err = application.QuoteOnShipment{Snapshots: postgres.SnapshotStore{DB: m.db}, Cards: postgres.RateCardStore{DB: m.db}}.Lines(txCtx, quote,
				domain.Cargo{Pieces: c.Pieces, GrossGrams: c.GrossGrams, VolumeCM3: c.VolumeCM3, Documents: c.Documents})
			return err
		}); err != nil {
			return api.PricingBasis{}, err
		}
		out.BasisID = quoteBasisID(fmt.Sprintf("QUOTE:%s@%d/%s@%d", s.QuoteID, revision, sh.ShipmentID, sh.PlanVersion))
		out.SubjectKind, out.SubjectID, out.SubjectVersion = "SHIPMENT", sh.ShipmentID, sh.PlanVersion
	}
	for _, l := range shipped {
		out.Lines = append(out.Lines, api.PricingLine{LineNo: l.LineNo, ChargeCode: string(l.ChargeCode), Sell: l.Sell, Cost: l.Cost,
			Discount: l.Sell.Minor < 0, Source: api.SourceQuote, HasSell: true, HasCost: l.Cost.Minor != 0, Reevaluated: l.Reevaluated,
			Unit: string(l.Unit), Quantity: l.Quantity})
	}
	for _, r := range s.ExchangeRates {
		out.ExchangeRates = append(out.ExchangeRates, api.ExchangeRate{From: r.From, Rate: r.Rate, Scale: r.Scale, FromDigits: r.FromDigits,
			QuoteDigits: r.QuoteDigits, Source: r.Source, AsOf: r.AsOf})
	}
	return out, nil
}

// quoteBasisID keeps a re-evaluated quote's basis id within 128 characters, as FIN stores it: a longer one is hashed.
func quoteBasisID(id string) string {
	if utf8.RuneCountInString(id) <= 128 {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "QUOTE:" + hex.EncodeToString(sum[:20])
}

func (m *Module) GetQuoteSnapshotV2(ctx context.Context, q api.GetQuoteSnapshot) (api.QuoteSnapshotV2, error) {
	quote, revision, err := m.oneOff(ctx, q)
	if err != nil {
		return api.QuoteSnapshotV2{}, err
	}
	return snapshotV2(quote, revision)
}

// snapshotV2 is QuoteSnapshotV2 of an accepted quote read at revision.
func snapshotV2(quote *domain.Quote, revision int64) (api.QuoteSnapshotV2, error) {
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
	quote, revision, err := m.oneOff(ctx, q)
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
	return api.Receipt{CommandID: r.CommandID, QuoteID: string(r.QuoteID), SubjectID: string(r.QuoteID), NewRevision: r.NewRevision}, nil
}

type authorizer struct{ api.Authorizer }

func (a authorizer) RequireQuoteAction(ctx context.Context, s application.Scope, action application.QuoteAction, id domain.QuoteID) error {
	return a.AuthorizeQuoteAction(ctx, string(s.TenantID), s.ActorID, string(action), string(id))
}

type margins struct{ policy api.MarginPolicy }

func (m margins) MarginFloorBP(ctx context.Context, tenant domain.TenantID, entity domain.LegalEntityID) (int, error) {
	return m.policy.MarginFloorBP(ctx, string(tenant), string(entity))
}

// RateCardDiscountBP asks the policy's DiscountPolicy, when it has one; otherwise no sell below the rated one passes.
func (m margins) RateCardDiscountBP(ctx context.Context, tenant domain.TenantID, entity domain.LegalEntityID) (int, error) {
	if p, ok := m.policy.(api.DiscountPolicy); ok {
		return p.RateCardDiscountBP(ctx, string(tenant), string(entity))
	}
	return 0, nil
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
	case errors.Is(err, domain.ErrConflict), errors.Is(err, bentorepo.ErrConflict), errors.Is(err, bentorepo.ErrAlreadyExists), postgres.Conflict(err):
		kind = api.ErrConflict
	case errors.Is(err, bentorepo.ErrNotFound):
		kind = api.ErrNotFound
	case errors.Is(err, domain.ErrInvalidQuote), errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrSelfApproval),
		errors.Is(err, domain.ErrOutsideValidity), errors.Is(err, application.ErrCustomerNotConfirmed),
		errors.Is(err, application.ErrQuoteNotAccepted), errors.Is(err, kernel.ErrCurrencyMismatch), errors.Is(err, kernel.ErrAmountOverflow),
		errors.Is(err, kernel.ErrOutsideRecordWindow), errors.Is(err, domain.ErrInvalidChargeCode), errors.Is(err, domain.ErrChargeCodeUnknown),
		errors.Is(err, domain.ErrChargeCodeExists), errors.Is(err, domain.ErrInvalidRateCard), errors.Is(err, domain.ErrVersionOverlap),
		errors.Is(err, domain.ErrNotPublishable), errors.Is(err, domain.ErrInvalidEvaluation), errors.Is(err, application.ErrPartyNotConfirmed),
		errors.Is(err, application.ErrLocationRefused), errors.Is(err, application.ErrValidFromTooEarly), errors.Is(err, domain.ErrInvalidBasis),
		errors.Is(err, application.ErrVersionQuoted), errors.Is(err, domain.ErrLaneTaken):
		kind = api.ErrRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
