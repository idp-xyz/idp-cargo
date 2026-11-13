package http

import (
	"context"
	"math/big"
	nethttp "net/http"
	"net/url"
	"slices"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// QuoteService is what the quote routes call: RATE's quote commands, its quote read model and snapshots, the
// service-level list, and the rate snapshots its rated lines name.
type QuoteService interface {
	api.Commands
	api.Queries
	api.QuoteQueries
	GetRateSnapshot(ctx context.Context, q api.GetRateSnapshot) (api.RateSnapshot, error)
}

func registerQuotes(mux *nethttp.ServeMux, s QuoteService) {
	h := quoteHandlers{s}
	mux.HandleFunc("GET /v1/service-levels", h.listServiceLevels)
	mux.HandleFunc("POST /v1/quotes", h.createQuote)
	mux.HandleFunc("GET /v1/quotes/{quote}", h.getQuote)
	mux.HandleFunc("GET /v1/quotes/{quote}/snapshot", h.getSnapshot)
	mux.HandleFunc("POST /v1/quotes/{quote}/approve-price", h.approvePrice)
	mux.HandleFunc("POST /v1/quotes/{quote}/accept", h.accept)
	mux.HandleFunc("POST /v1/quotes/{quote}/expire", h.expire)
}

type quoteHandlers struct{ s QuoteService }

// seesQuoteMargin reports whether the principal may see a quote's cost total and margin: who may see supplier costs,
// and who approves prices, which turns on the margin. Line costs, which can be a supplier's exact rate, need seesCosts.
func seesQuoteMargin(p identity.Principal) bool {
	return seesCosts(p) || identity.Allowed(p, bc, api.ActionApproveQuotePrice)
}

// takesCosts refuses a quote whose rated lines name a rate snapshot that rated the BUY side, to one who may not see
// supplier costs: such a quote takes its costs from the snapshot, and whether it then needs approval would let its
// creator find the costs out, one quote at a time.
func (h quoteHandlers) takesCosts(w nethttp.ResponseWriter, r *nethttp.Request, p identity.Principal, snapshots []string) bool {
	if seesCosts(p) {
		return false
	}
	for _, id := range snapshots {
		s, err := h.s.GetRateSnapshot(r.Context(), api.GetRateSnapshot{TenantID: p.TenantID, SnapshotID: id})
		if err != nil {
			continue // RATE answers for a snapshot it does not have
		}
		if slices.Contains(s.Sides, api.SideBuy) {
			httpapi.WriteProblem(w, r, nethttp.StatusForbidden, "FORBIDDEN",
				"a quote taking supplier costs from a rate snapshot needs rate:ReadBuyRates")
			return true
		}
	}
	return false
}

func (m money) kernel() kernel.Money {
	return kernel.Money{Currency: kernel.CurrencyCode(m.Currency), Minor: m.Minor}
}

type exchangeRate struct {
	From        string    `json:"from"`
	Rate        int64     `json:"rate"`
	Scale       int       `json:"scale"`
	FromDigits  int       `json:"from_digits"`
	QuoteDigits int       `json:"quote_digits"`
	Source      string    `json:"source"`
	AsOf        time.Time `json:"as_of"`
}

type terms struct {
	IncotermsRule        string `json:"incoterms_rule,omitempty"`
	IncotermsVersion     string `json:"incoterms_version,omitempty"`
	NamedPlace           string `json:"named_place,omitempty"`
	ServiceLevel         string `json:"service_level,omitempty"`
	ServiceLevelUnlisted bool   `json:"service_level_unlisted,omitempty"` // free text recorded before the list (C8); never sent
}

type scope struct {
	Mode          string `json:"mode"`
	ContainerMode string `json:"container_mode"`
	Origin        string `json:"origin"`
	Destination   string `json:"destination"`
	Via           string `json:"via,omitempty"`
	Carrier       string `json:"carrier,omitempty"`
	Cargo         cargo  `json:"cargo"`
}

type agreementRef struct {
	RateCardID string `json:"rate_card_id"`
	Version    int    `json:"version"`
}

// GET /v1/service-levels: the service levels a quote's terms name.
func (h quoteHandlers) listServiceLevels(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	levels, err := h.s.ListServiceLevels(r.Context(), api.ListServiceLevels{TenantID: p.TenantID})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	out := make([]map[string]string, 0, len(levels))
	for _, l := range levels {
		out = append(out, map[string]string{"code": l.Code, "name": l.Name})
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, map[string]any{"service_levels": out})
}

// POST /v1/quotes: CreateQuote; 201 with the quote's Location. A line is MANUAL (sell and cost entered), RATED (its
// snapshot named; RATE takes the price from it) or RATED_OVERRIDDEN (the sell entered, with a reason).
func (h quoteHandlers) createQuote(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		QuoteID         string `json:"quote_id"`
		Kind            string `json:"kind"`
		LegalEntityID   string `json:"legal_entity_id"`
		CustomerPartyID string `json:"customer_party_id"`
		RecipientRole   string `json:"recipient_role"`
		Currency        string `json:"currency"`
		Lines           []struct {
			ChargeCode     string `json:"charge_code"`
			Sell           money  `json:"sell"`
			Cost           money  `json:"cost"`
			Source         string `json:"source"`
			SnapshotID     string `json:"snapshot_id"`
			OverrideReason string `json:"override_reason"`
		} `json:"lines"`
		ExchangeRates []exchangeRate `json:"exchange_rates"`
		Terms         terms          `json:"terms"`
		ValidFrom     time.Time      `json:"valid_from"`
		ValidUntil    time.Time      `json:"valid_until"`
		Scope         *scope         `json:"scope"`
		Agreement     *agreementRef  `json:"agreement"`
		OccurredAt    *time.Time     `json:"occurred_at"`
	}
	if !httpapi.Decode(w, r, &body) {
		return
	}
	c, ok := begin(w, r, body.OccurredAt)
	if !ok {
		return
	}
	var snapshots []string
	for _, l := range body.Lines {
		if l.SnapshotID != "" && !slices.Contains(snapshots, l.SnapshotID) {
			snapshots = append(snapshots, l.SnapshotID)
		}
	}
	if h.takesCosts(w, r, c.who, snapshots) {
		return
	}
	cmd := api.CreateQuote{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID, QuoteID: body.QuoteID, Kind: body.Kind,
		LegalEntityID: body.LegalEntityID, CustomerPartyID: body.CustomerPartyID, RecipientRole: body.RecipientRole,
		Currency: kernel.CurrencyCode(body.Currency), Terms: api.CommercialTerms{IncotermsRule: body.Terms.IncotermsRule,
			IncotermsVersion: body.Terms.IncotermsVersion, NamedPlace: body.Terms.NamedPlace, ServiceLevel: body.Terms.ServiceLevel},
		Validity: kernel.ValidityInterval{From: body.ValidFrom.UTC(), Until: body.ValidUntil.UTC()}, OccurredAt: c.occurredAt}
	for _, l := range body.Lines {
		cmd.Lines = append(cmd.Lines, api.QuoteLine{ChargeCode: l.ChargeCode, Sell: l.Sell.kernel(), Cost: l.Cost.kernel(), Source: l.Source,
			SnapshotID: l.SnapshotID, OverrideReason: l.OverrideReason})
	}
	for _, x := range body.ExchangeRates {
		cmd.ExchangeRates = append(cmd.ExchangeRates, api.ExchangeRate{From: kernel.CurrencyCode(x.From), Rate: x.Rate, Scale: x.Scale,
			FromDigits: x.FromDigits, QuoteDigits: x.QuoteDigits, Source: x.Source, AsOf: x.AsOf.UTC()})
	}
	if sc := body.Scope; sc != nil {
		cmd.Scope = &api.QuoteScope{Mode: sc.Mode, ContainerMode: sc.ContainerMode, Origin: sc.Origin, Destination: sc.Destination, Via: sc.Via,
			Carrier: sc.Carrier, Cargo: api.Cargo{Pieces: sc.Cargo.Pieces, GrossGrams: sc.Cargo.GrossGrams, VolumeCM3: sc.Cargo.VolumeCM3,
				Documents: sc.Cargo.Documents}}
	}
	if a := body.Agreement; a != nil {
		cmd.Agreement = &api.AgreementRef{RateCardID: a.RateCardID, Version: a.Version}
	}
	rc, err := h.s.CreateQuote(r.Context(), cmd)
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteReceipt(w, nethttp.StatusCreated, quoteReceipt(rc), "/v1/quotes/"+url.PathEscape(rc.QuoteID))
}

func quoteReceipt(rc api.Receipt) httpapi.Receipt {
	subject := rc.SubjectID
	if subject == "" {
		subject = rc.QuoteID
	}
	return httpapi.Receipt{CommandID: rc.CommandID, SubjectID: subject, NewRevision: rc.NewRevision}
}

type quoteLine struct {
	ChargeCode      string `json:"charge_code"`
	Sell            money  `json:"sell"`
	InQuoteCurrency money  `json:"in_quote_currency"`
	Discount        bool   `json:"discount"`
	Cost            *money `json:"cost,omitempty"`
	Source          string `json:"source"`
	SnapshotID      string `json:"snapshot_id,omitempty"`
	Basis           string `json:"basis"`
	Unit            string `json:"unit,omitempty"`
	Rate            int64  `json:"rate,omitempty"`
	RateScale       int    `json:"rate_scale,omitempty"`
	Quantity        int64  `json:"quantity,omitempty"`
	CodeVersion     *int   `json:"code_version,omitempty"`
	CodeUnlisted    bool   `json:"code_unlisted,omitempty"` // a free-text code of a quote priced before the catalogue
}

func exchangeRates(rates []api.ExchangeRate) []exchangeRate {
	out := make([]exchangeRate, 0, len(rates))
	for _, x := range rates {
		out = append(out, exchangeRate{From: string(x.From), Rate: x.Rate, Scale: x.Scale, FromDigits: x.FromDigits, QuoteDigits: x.QuoteDigits,
			Source: x.Source, AsOf: x.AsOf})
	}
	return out
}

func scopeView(sc *api.QuoteScope) *scope {
	if sc == nil {
		return nil
	}
	return &scope{Mode: sc.Mode, ContainerMode: sc.ContainerMode, Origin: sc.Origin, Destination: sc.Destination, Via: sc.Via, Carrier: sc.Carrier,
		Cargo: cargo{Pieces: sc.Cargo.Pieces, GrossGrams: sc.Cargo.GrossGrams, VolumeCM3: sc.Cargo.VolumeCM3, Documents: sc.Cargo.Documents}}
}

func agreementView(a *api.AgreementRef) *agreementRef {
	if a == nil {
		return nil
	}
	return &agreementRef{RateCardID: a.RateCardID, Version: a.Version}
}

func termsView(t api.CommercialTerms, unlisted bool) terms {
	return terms{IncotermsRule: t.IncotermsRule, IncotermsVersion: t.IncotermsVersion, NamedPlace: t.NamedPlace, ServiceLevel: t.ServiceLevel,
		ServiceLevelUnlisted: unlisted}
}

// marginBP is margin × 10000 / sell, truncated toward zero, exactly for any amounts.
func marginBP(margin, sell int64) int64 {
	n := new(big.Int).Mul(big.NewInt(margin), big.NewInt(10_000))
	return n.Quo(n, big.NewInt(sell)).Int64()
}

// GET /v1/quotes/{quote}: the quote as it stands; ETag its revision. The cost total and the margin only to a reader who
// may see supplier costs (rate:ReadBuyRates, rate:MaintainBuyRates) or approves prices (rate:ApproveQuotePrice); line
// costs only to the former.
func (h quoteHandlers) getQuote(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	q, err := h.s.GetQuote(r.Context(), api.GetQuote{TenantID: p.TenantID, QuoteID: r.PathValue("quote")})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	costs, margin := seesCosts(p), seesQuoteMargin(p)
	lines := make([]quoteLine, 0, len(q.Lines))
	for _, l := range q.Lines {
		line := quoteLine{ChargeCode: l.ChargeCode, Sell: moneyView(l.Amount), InQuoteCurrency: moneyView(l.InQuoteCurrency), Discount: l.Discount,
			Source: l.Source, SnapshotID: l.SnapshotID, Basis: l.Basis, Unit: l.Unit, Rate: l.Rate, RateScale: l.RateScale, Quantity: l.Quantity,
			CodeVersion: l.CodeVersion, CodeUnlisted: l.CodeUnlisted}
		if costs {
			c := moneyView(l.Cost)
			line.Cost = &c
		}
		lines = append(lines, line)
	}
	out := map[string]any{"quote_id": q.QuoteID, "revision": q.Revision, "kind": q.Kind, "status": q.Status, "legal_entity_id": q.LegalEntityID,
		"customer": map[string]any{"party_id": q.Customer.PartyID, "party_revision": q.Customer.PartyRevision}, "recipient_role": q.RecipientRole,
		"currency": string(q.Currency), "lines": lines, "exchange_rates": exchangeRates(q.ExchangeRates),
		"terms":      termsView(q.Terms, q.ServiceLevelUnlisted),
		"valid_from": q.Validity.From, "valid_until": q.Validity.Until, "approval_reasons": orEmpty(q.ApprovalReasons), "author": q.Author}
	if sc := scopeView(q.Scope); sc != nil {
		out["scope"] = sc
	}
	if a := agreementView(q.Agreement); a != nil {
		out["agreement"] = a
	}
	if q.Kind != api.QuoteAgreement {
		out["sell_total"] = moneyView(q.SellTotal)
		if margin {
			out["cost_total"], out["margin_floor_bp"] = moneyView(q.CostTotal), q.MarginFloorBP
			m := q.SellTotal.Minor - q.CostTotal.Minor
			out["margin"] = money{Currency: string(q.SellTotal.Currency), Minor: m}
			if q.SellTotal.Minor > 0 {
				out["margin_bp"] = marginBP(m, q.SellTotal.Minor)
			}
		}
	}
	if a := q.Approval; a != nil {
		out["approval"] = map[string]any{"approved_by": a.ApprovedBy, "approved_at": a.ApprovedAt, "reason_ref": a.ReasonRef,
			"self_approved": a.SelfApproved}
	}
	if a := q.Acceptance; a != nil {
		out["acceptance"] = map[string]any{"accepted_by": a.AcceptedBy, "accepted_at": a.AcceptedAt, "recorded_at": a.RecordedAt,
			"evidence_kind": a.EvidenceKind, "evidence_ref": a.EvidenceRef}
	}
	if !q.ExpiredAt.IsZero() {
		out["expired_at"] = q.ExpiredAt
	}
	httpapi.ETag(w, q.Revision)
	httpapi.WriteJSON(w, nethttp.StatusOK, out)
}

// GET /v1/quotes/{quote}/snapshot: QuoteSnapshotV3 of an accepted quote (no costs or margin; 422 before acceptance).
func (h quoteHandlers) getSnapshot(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	s, err := h.s.GetQuoteSnapshotV3(r.Context(), api.GetQuoteSnapshot{TenantID: p.TenantID, QuoteID: r.PathValue("quote")})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	lines := make([]quoteLine, 0, len(s.Lines))
	for _, l := range s.Lines {
		lines = append(lines, quoteLine{ChargeCode: l.ChargeCode, Sell: moneyView(l.Amount), InQuoteCurrency: moneyView(l.InQuoteCurrency),
			Discount: l.Discount, Source: l.Source, SnapshotID: l.SnapshotID, Basis: l.Basis, Unit: l.Unit, Rate: l.Rate, RateScale: l.RateScale,
			Quantity: l.Quantity, CodeVersion: l.CodeVersion, CodeUnlisted: l.CodeUnlisted})
	}
	out := map[string]any{"quote_id": s.QuoteID, "quote_revision": s.QuoteRevision, "kind": s.Kind, "legal_entity_id": s.LegalEntityID,
		"customer": map[string]any{"party_id": s.Customer.PartyID, "party_revision": s.Customer.PartyRevision}, "recipient_role": s.RecipientRole,
		"currency": string(s.Currency), "lines": lines, "exchange_rates": exchangeRates(s.ExchangeRates), "rounding": s.Rounding,
		"terms": termsView(s.Terms, s.ServiceLevelUnlisted), "valid_from": s.Validity.From, "valid_until": s.Validity.Until,
		"price_approval_reasons": orEmpty(s.PriceApprovalReasons), "price_approved_by": s.PriceApprovedBy, "price_self_approved": s.PriceSelfApproved,
		"accepted_at": s.AcceptedAt, "acceptance_recorded_at": s.AcceptanceRecordedAt, "acceptance_evidence_kind": s.AcceptanceEvidenceKind,
		"acceptance_evidence_ref": s.AcceptanceEvidenceRef}
	if s.Kind != api.QuoteAgreement {
		out["total"] = moneyView(s.Total)
	}
	if sc := scopeView(s.Scope); sc != nil {
		out["scope"] = sc
	}
	if a := agreementView(s.Agreement); a != nil {
		out["agreement"] = a
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, out)
}

// quoteCommand reads what a command on a quote has: its body, the quote's revision in If-Match and the envelope.
func quoteCommand(w nethttp.ResponseWriter, r *nethttp.Request, body any, occurredAt func() *time.Time) (command, int64, bool) {
	return cardCommand(w, r, body, occurredAt)
}

// POST /v1/quotes/{quote}/approve-price: ApproveQuotePrice at the quote's revision, with a reason reference.
func (h quoteHandlers) approvePrice(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		ReasonRef  string     `json:"reason_ref"`
		OccurredAt *time.Time `json:"occurred_at"`
	}
	c, revision, ok := quoteCommand(w, r, &body, func() *time.Time { return body.OccurredAt })
	if !ok {
		return
	}
	rc, err := h.s.ApproveQuotePrice(r.Context(), api.ApproveQuotePrice{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
		QuoteID: r.PathValue("quote"), ExpectedRevision: revision, ReasonRef: body.ReasonRef, OccurredAt: c.occurredAt})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteReceipt(w, nethttp.StatusOK, quoteReceipt(rc), "")
}

// POST /v1/quotes/{quote}/accept: AcceptQuote at the quote's revision, with the kind and reference of the evidence.
func (h quoteHandlers) accept(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		EvidenceKind string     `json:"evidence_kind"`
		EvidenceRef  string     `json:"evidence_ref"`
		OccurredAt   *time.Time `json:"occurred_at"`
	}
	c, revision, ok := quoteCommand(w, r, &body, func() *time.Time { return body.OccurredAt })
	if !ok {
		return
	}
	rc, err := h.s.AcceptQuote(r.Context(), api.AcceptQuote{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
		QuoteID: r.PathValue("quote"), ExpectedRevision: revision, EvidenceKind: body.EvidenceKind, EvidenceRef: body.EvidenceRef,
		OccurredAt: c.occurredAt})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteReceipt(w, nethttp.StatusOK, quoteReceipt(rc), "")
}

// POST /v1/quotes/{quote}/expire: ExpireQuote at the quote's revision, once its validity has passed.
func (h quoteHandlers) expire(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body onlyTime
	c, revision, ok := quoteCommand(w, r, &body, func() *time.Time { return body.OccurredAt })
	if !ok {
		return
	}
	rc, err := h.s.ExpireQuote(r.Context(), api.ExpireQuote{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
		QuoteID: r.PathValue("quote"), ExpectedRevision: revision, OccurredAt: c.occurredAt})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteReceipt(w, nethttp.StatusOK, quoteReceipt(rc), "")
}
