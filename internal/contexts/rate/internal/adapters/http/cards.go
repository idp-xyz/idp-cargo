package http

import (
	nethttp "net/http"
	"net/url"
	"strconv"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// actionReadBuyRates lets a reader see supplier costs: BUY cards, and the BUY lines and candidates of snapshots.
// Costs and margins stay in RATE, so rate:Read alone shows the SELL side only; maintaining BUY rates sees them too.
const actionReadBuyRates = "ReadBuyRates"

// seesCosts reports whether the principal may see supplier costs.
func seesCosts(p identity.Principal) bool {
	return identity.Allowed(p, bc, actionReadBuyRates) || identity.Allowed(p, bc, api.ActionMaintainBuyRates)
}

// costsHidden refuses a BUY card to a reader who may not see costs.
func costsHidden(w nethttp.ResponseWriter, r *nethttp.Request, p identity.Principal, side string) bool {
	if side == api.SideBuy && !seesCosts(p) {
		httpapi.WriteProblem(w, r, nethttp.StatusForbidden, "FORBIDDEN", "supplier costs need rate:ReadBuyRates")
		return true
	}
	return false
}

func registerCards(mux *nethttp.ServeMux, h handlers) {
	mux.HandleFunc("GET /v1/rate-cards", h.listRateCards)
	mux.HandleFunc("POST /v1/rate-cards", h.createRateCard)
	mux.HandleFunc("GET /v1/rate-cards/{card}", h.getRateCard)
	mux.HandleFunc("POST /v1/rate-cards/{card}/versions", h.draftVersion)
	mux.HandleFunc("GET /v1/rate-cards/{card}/versions/{version}", h.getVersion)
	mux.HandleFunc("PUT /v1/rate-cards/{card}/versions/{version}/entries", h.setEntries)
	mux.HandleFunc("POST /v1/rate-cards/{card}/versions/{version}/publish", h.publishVersion)
	mux.HandleFunc("POST /v1/rate-cards/{card}/versions/{version}/withdraw", h.withdrawVersion)
	mux.HandleFunc("POST /v1/rate-evaluations", h.evaluateRates)
	mux.HandleFunc("GET /v1/rate-snapshots/{snapshot}", h.getSnapshot)
}

// money is an amount in minor units of its currency.
type money struct {
	Currency string `json:"currency"`
	Minor    int64  `json:"minor"`
}

func moneyView(m kernel.Money) money { return money{Currency: string(m.Currency), Minor: m.Minor} }

func moneys(ms []kernel.Money) []money {
	out := make([]money, 0, len(ms))
	for _, m := range ms {
		out = append(out, moneyView(m))
	}
	return out
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func at(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

// rateBreak is a band of a PER_UNIT line: from From base units on, Rate applies.
type rateBreak struct {
	From int64 `json:"from"`
	Rate int64 `json:"rate"`
}

// lineFields are what a rate line states (see api.RateLine).
type lineFields struct {
	ChargeCode     string      `json:"charge_code"`
	Currency       string      `json:"currency"`
	Calculator     string      `json:"calculator"`
	Unit           string      `json:"unit,omitempty"`
	Rate           int64       `json:"rate"`
	RateScale      int         `json:"rate_scale"`
	Minimum        int64       `json:"minimum"`
	Maximum        int64       `json:"maximum"`
	Step           int64       `json:"step"`
	MinQuantity    int64       `json:"min_quantity"`
	VolumeFactor   int64       `json:"volume_factor"`
	PercentBP      int64       `json:"percent_bp"`
	BaseCode       string      `json:"base_code,omitempty"`
	CallForPricing bool        `json:"call_for_pricing"`
	Breaks         []rateBreak `json:"breaks"`
}

// entryFields are what a lane states (see api.RateEntry); Edition is the location data's, set by RATE.
type entryFields struct {
	Mode          string     `json:"mode"`
	ContainerMode string     `json:"container_mode"`
	Origin        string     `json:"origin"`
	Destination   string     `json:"destination"`
	Via           string     `json:"via,omitempty"`
	Carrier       string     `json:"carrier,omitempty"`
	ServiceLevel  string     `json:"service_level"`
	Commodity     string     `json:"commodity"`
	TransitDays   int        `json:"transit_days"`
	Frequency     string     `json:"frequency,omitempty"`
	ValidFrom     *time.Time `json:"valid_from,omitempty"`
	ValidUntil    *time.Time `json:"valid_until,omitempty"`
}

// entryInput is an entry as sent: entries and lines are numbered in the order given.
type entryInput struct {
	entryFields
	Lines []lineFields `json:"lines"`
}

type lineView struct {
	No int `json:"no"`
	lineFields
}

type entryView struct {
	No      int    `json:"no"`
	Edition string `json:"edition,omitempty"`
	entryFields
	Lines []lineView `json:"lines"`
}

func (e entryInput) api() api.RateEntry {
	out := api.RateEntry{Mode: e.Mode, ContainerMode: e.ContainerMode, Origin: e.Origin, Destination: e.Destination, Via: e.Via, Carrier: e.Carrier,
		ServiceLevel: e.ServiceLevel, Commodity: e.Commodity, TransitDays: e.TransitDays, Frequency: e.Frequency, ValidFrom: at(e.ValidFrom),
		ValidUntil: at(e.ValidUntil)}
	for _, l := range e.Lines {
		line := api.RateLine{ChargeCode: l.ChargeCode, Currency: kernel.CurrencyCode(l.Currency), Calculator: l.Calculator, Unit: l.Unit, Rate: l.Rate,
			RateScale: l.RateScale, Minimum: l.Minimum, Maximum: l.Maximum, Step: l.Step, MinQuantity: l.MinQuantity, VolumeFactor: l.VolumeFactor,
			PercentBP: l.PercentBP, BaseCode: l.BaseCode, CallForPricing: l.CallForPricing}
		for _, b := range l.Breaks {
			line.Breaks = append(line.Breaks, api.RateBreak{From: b.From, Rate: b.Rate})
		}
		out.Lines = append(out.Lines, line)
	}
	return out
}

func optional(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func entryOut(e api.RateEntry) entryView {
	out := entryView{No: e.No, Edition: e.Edition, entryFields: entryFields{Mode: e.Mode, ContainerMode: e.ContainerMode, Origin: e.Origin,
		Destination: e.Destination, Via: e.Via, Carrier: e.Carrier, ServiceLevel: e.ServiceLevel, Commodity: e.Commodity, TransitDays: e.TransitDays,
		Frequency: e.Frequency, ValidFrom: optional(e.ValidFrom), ValidUntil: optional(e.ValidUntil)}, Lines: []lineView{}}
	for _, l := range e.Lines {
		breaks := make([]rateBreak, 0, len(l.Breaks))
		for _, b := range l.Breaks {
			breaks = append(breaks, rateBreak{From: b.From, Rate: b.Rate})
		}
		out.Lines = append(out.Lines, lineView{No: l.No, lineFields: lineFields{ChargeCode: l.ChargeCode, Currency: string(l.Currency),
			Calculator: l.Calculator, Unit: l.Unit, Rate: l.Rate, RateScale: l.RateScale, Minimum: l.Minimum, Maximum: l.Maximum, Step: l.Step,
			MinQuantity: l.MinQuantity, VolumeFactor: l.VolumeFactor, PercentBP: l.PercentBP, BaseCode: l.BaseCode, CallForPricing: l.CallForPricing,
			Breaks: breaks}})
	}
	return out
}

// versionHeader is a version without its entries.
type versionHeader struct {
	Version        int        `json:"version"`
	Status         string     `json:"status"`
	ValidFrom      time.Time  `json:"valid_from"`
	ValidUntil     *time.Time `json:"valid_until,omitempty"`
	Currency       string     `json:"currency,omitempty"`
	SourceRef      string     `json:"source_ref,omitempty"`
	FilingRef      string     `json:"filing_ref,omitempty"`
	Preparers      []string   `json:"preparers"`
	PreparedAt     time.Time  `json:"prepared_at,omitzero"`
	PublishedBy    string     `json:"published_by,omitempty"`
	PublishedAt    time.Time  `json:"published_at,omitzero"`
	ApprovedBy     string     `json:"approved_by,omitempty"`
	SelfApproved   bool       `json:"self_approved"`
	WithdrawnBy    string     `json:"withdrawn_by,omitempty"`
	WithdrawnAt    time.Time  `json:"withdrawn_at,omitzero"`
	WithdrawReason string     `json:"withdraw_reason,omitempty"`
}

func headerView(v api.RateCardVersionHeader) versionHeader {
	return versionHeader{Version: v.Version, Status: v.Status, ValidFrom: v.ValidFrom.UTC(), ValidUntil: optional(v.ValidUntil), Currency: string(v.Currency),
		SourceRef: v.SourceRef, FilingRef: v.FilingRef, Preparers: orEmpty(v.Preparers), PreparedAt: v.PreparedAt, PublishedBy: v.PublishedBy,
		PublishedAt: v.PublishedAt, ApprovedBy: v.ApprovedBy, SelfApproved: v.SelfApproved, WithdrawnBy: v.WithdrawnBy, WithdrawnAt: v.WithdrawnAt,
		WithdrawReason: v.WithdrawReason}
}

type rateCard struct {
	RateCardID    string          `json:"rate_card_id"`
	Side          string          `json:"side"`
	Scope         string          `json:"scope"`
	LegalEntityID string          `json:"legal_entity_id"`
	PartyID       string          `json:"party_id,omitempty"`
	PartyRevision int64           `json:"party_revision,omitempty"`
	Name          string          `json:"name"`
	CreatedBy     string          `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
	Revision      int64           `json:"revision"`
	Versions      []versionHeader `json:"versions"`
}

func cardView(c api.RateCard) rateCard {
	out := rateCard{RateCardID: c.RateCardID, Side: c.Side, Scope: c.Scope, LegalEntityID: c.LegalEntityID, PartyID: c.PartyID,
		PartyRevision: c.PartyRevision, Name: c.Name, CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt, Revision: c.Revision, Versions: []versionHeader{}}
	for _, v := range c.Versions {
		out.Versions = append(out.Versions, headerView(v))
	}
	return out
}

// GET /v1/rate-cards[?side=&scope=&legal_entity_id=&party_id=&valid_at=]; BUY cards only to a reader who may see
// costs.
func (h handlers) listRateCards(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	in := api.ListRateCards{TenantID: p.TenantID, Side: q.Get("side"), Scope: q.Get("scope"), LegalEntityID: q.Get("legal_entity_id"),
		PartyID: q.Get("party_id")}
	switch {
	case in.Side != "" && in.Side != api.SideSell && in.Side != api.SideBuy:
		httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", "side is SELL or BUY")
		return
	case in.Scope != "" && in.Scope != api.ScopeTariff && in.Scope != api.ScopeCustomer && in.Scope != api.ScopeSupplier:
		httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", "scope is TARIFF, CUSTOMER or SUPPLIER")
		return
	case costsHidden(w, r, p, in.Side):
		return
	}
	if v := q.Get("valid_at"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", "valid_at is an RFC 3339 time (percent-encode a + offset)")
			return
		}
		in.ValidAt = t.UTC()
	}
	cards, err := h.s.ListRateCards(r.Context(), in)
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	out := make([]rateCard, 0, len(cards))
	for _, c := range cards {
		if c.Side == api.SideBuy && !seesCosts(p) {
			continue
		}
		out = append(out, cardView(c))
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, map[string]any{"rate_cards": out})
}

// GET /v1/rate-cards/{card}; ETag is the card's revision, which its next command names in If-Match.
func (h handlers) getRateCard(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	c, err := h.s.GetRateCard(r.Context(), api.GetRateCard{TenantID: p.TenantID, RateCardID: r.PathValue("card")})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	if costsHidden(w, r, p, c.Side) {
		return
	}
	httpapi.ETag(w, c.Revision)
	httpapi.WriteJSON(w, nethttp.StatusOK, cardView(c))
}

// version reads the {version} path segment: a version number from 1.
func version(w nethttp.ResponseWriter, r *nethttp.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || n < 1 || strconv.Itoa(n) != r.PathValue("version") {
		httpapi.WriteProblem(w, r, nethttp.StatusNotFound, "NOT_FOUND", "no such version")
		return 0, false
	}
	return n, true
}

// GET /v1/rate-cards/{card}/versions/{version}: the version with its entries; ETag is the card's revision, which a
// command on the version names in If-Match.
func (h handlers) getVersion(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	n, ok := version(w, r)
	if !ok {
		return
	}
	card, err := h.s.GetRateCard(r.Context(), api.GetRateCard{TenantID: p.TenantID, RateCardID: r.PathValue("card")})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	if costsHidden(w, r, p, card.Side) {
		return
	}
	v, err := h.s.GetRateCardVersion(r.Context(), api.GetRateCardVersion{TenantID: p.TenantID, RateCardID: r.PathValue("card"), Version: n})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.ETag(w, v.CardRevision)
	entries := make([]entryView, 0, len(v.Entries))
	for _, e := range v.Entries {
		entries = append(entries, entryOut(e))
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, struct {
		RateCardID string `json:"rate_card_id"`
		versionHeader
		Entries []entryView `json:"entries"`
	}{v.RateCardID, headerView(v.RateCardVersionHeader), entries})
}

// POST /v1/rate-cards: CreateRateCard; 201 with the card's Location.
func (h handlers) createRateCard(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		RateCardID    string     `json:"rate_card_id"`
		Side          string     `json:"side"`
		Scope         string     `json:"scope"`
		LegalEntityID string     `json:"legal_entity_id"`
		PartyID       string     `json:"party_id"`
		Name          string     `json:"name"`
		OccurredAt    *time.Time `json:"occurred_at"`
	}
	if !httpapi.Decode(w, r, &body) {
		return
	}
	c, ok := begin(w, r, body.OccurredAt)
	if !ok {
		return
	}
	rc, err := h.s.CreateRateCard(r.Context(), api.CreateRateCard{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
		RateCardID: body.RateCardID, Side: body.Side, Scope: body.Scope, LegalEntityID: body.LegalEntityID, PartyID: body.PartyID, Name: body.Name,
		OccurredAt: c.occurredAt})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteReceipt(w, nethttp.StatusCreated, receipt(rc), "/v1/rate-cards/"+url.PathEscape(rc.SubjectID))
}

// cardCommand reads what a command on an existing card has: its body, the card's revision in If-Match and the
// envelope.
func cardCommand(w nethttp.ResponseWriter, r *nethttp.Request, body any, occurredAt func() *time.Time) (command, int64, bool) {
	if !httpapi.Decode(w, r, body) {
		return command{}, 0, false
	}
	revision, ok := httpapi.ExpectedRevision(w, r)
	if !ok {
		return command{}, 0, false
	}
	c, ok := begin(w, r, occurredAt())
	return c, revision, ok
}

// POST /v1/rate-cards/{card}/versions: DraftRateCardVersion at the card's revision in If-Match; 201, the card's
// Location (its versions list the new one).
func (h handlers) draftVersion(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		ValidFrom  *time.Time `json:"valid_from"`
		ValidUntil *time.Time `json:"valid_until"`
		Currency   string     `json:"currency"`
		SourceRef  string     `json:"source_ref"`
		FilingRef  string     `json:"filing_ref"`
		CopyFrom   int        `json:"copy_from"`
		OccurredAt *time.Time `json:"occurred_at"`
	}
	c, revision, ok := cardCommand(w, r, &body, func() *time.Time { return body.OccurredAt })
	if !ok {
		return
	}
	card := r.PathValue("card")
	rc, err := h.s.DraftRateCardVersion(r.Context(), api.DraftRateCardVersion{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
		RateCardID: card, ExpectedRevision: revision, ValidFrom: at(body.ValidFrom), ValidUntil: at(body.ValidUntil),
		Currency: kernel.CurrencyCode(body.Currency), SourceRef: body.SourceRef, FilingRef: body.FilingRef, CopyFrom: body.CopyFrom,
		OccurredAt: c.occurredAt})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteReceipt(w, nethttp.StatusCreated, receipt(rc), "/v1/rate-cards/"+url.PathEscape(card))
}

// PUT /v1/rate-cards/{card}/versions/{version}/entries: SetRateCardEntries replaces a DRAFT version's entries.
func (h handlers) setEntries(w nethttp.ResponseWriter, r *nethttp.Request) {
	n, ok := version(w, r)
	if !ok {
		return
	}
	var body struct {
		Entries    []entryInput `json:"entries"`
		OccurredAt *time.Time   `json:"occurred_at"`
	}
	c, revision, ok := cardCommand(w, r, &body, func() *time.Time { return body.OccurredAt })
	if !ok {
		return
	}
	entries := make([]api.RateEntry, 0, len(body.Entries))
	for _, e := range body.Entries {
		entries = append(entries, e.api())
	}
	rc, err := h.s.SetRateCardEntries(r.Context(), api.SetRateCardEntries{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
		RateCardID: r.PathValue("card"), ExpectedRevision: revision, Version: n, Entries: entries, OccurredAt: c.occurredAt})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteReceipt(w, nethttp.StatusOK, receipt(rc), "")
}

// POST /v1/rate-cards/{card}/versions/{version}/publish: PublishRateCardVersion.
func (h handlers) publishVersion(w nethttp.ResponseWriter, r *nethttp.Request) {
	n, ok := version(w, r)
	if !ok {
		return
	}
	var body onlyTime
	c, revision, ok := cardCommand(w, r, &body, func() *time.Time { return body.OccurredAt })
	if !ok {
		return
	}
	rc, err := h.s.PublishRateCardVersion(r.Context(), api.PublishRateCardVersion{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
		RateCardID: r.PathValue("card"), ExpectedRevision: revision, Version: n, OccurredAt: c.occurredAt})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteReceipt(w, nethttp.StatusOK, receipt(rc), "")
}

// POST /v1/rate-cards/{card}/versions/{version}/withdraw: WithdrawRateCardVersion with a reason.
func (h handlers) withdrawVersion(w nethttp.ResponseWriter, r *nethttp.Request) {
	n, ok := version(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason     string     `json:"reason"`
		OccurredAt *time.Time `json:"occurred_at"`
	}
	c, revision, ok := cardCommand(w, r, &body, func() *time.Time { return body.OccurredAt })
	if !ok {
		return
	}
	rc, err := h.s.WithdrawRateCardVersion(r.Context(), api.WithdrawRateCardVersion{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
		RateCardID: r.PathValue("card"), ExpectedRevision: revision, Version: n, Reason: body.Reason, OccurredAt: c.occurredAt})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteReceipt(w, nethttp.StatusOK, receipt(rc), "")
}

// cargo is what an evaluation rates: pieces, grams, cubic centimetres and documents.
type cargo struct {
	Pieces     int64 `json:"pieces"`
	GrossGrams int64 `json:"gross_grams"`
	VolumeCM3  int64 `json:"volume_cm3"`
	Documents  int64 `json:"documents"`
}

// POST /v1/rate-evaluations: EvaluateRates records a snapshot; 201 with the snapshot's Location.
func (h handlers) evaluateRates(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		SnapshotID      string     `json:"snapshot_id"`
		Purpose         string     `json:"purpose"`
		LegalEntityID   string     `json:"legal_entity_id"`
		Sides           []string   `json:"sides"`
		CustomerPartyID string     `json:"customer_party_id"`
		Carrier         string     `json:"carrier"`
		Mode            string     `json:"mode"`
		ContainerMode   string     `json:"container_mode"`
		Origin          string     `json:"origin"`
		Destination     string     `json:"destination"`
		Via             string     `json:"via"`
		ServiceLevel    string     `json:"service_level"`
		Commodity       string     `json:"commodity"`
		Cargo           cargo      `json:"cargo"`
		SubjectKind     string     `json:"subject_kind"`
		SubjectID       string     `json:"subject_id"`
		SubjectVersion  int64      `json:"subject_version"`
		OccurredAt      *time.Time `json:"occurred_at"`
	}
	if !httpapi.Decode(w, r, &body) {
		return
	}
	c, ok := begin(w, r, body.OccurredAt)
	if !ok {
		return
	}
	rc, err := h.s.EvaluateRates(r.Context(), api.EvaluateRates{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
		SnapshotID: body.SnapshotID, Purpose: body.Purpose, LegalEntityID: body.LegalEntityID, Sides: body.Sides, CustomerPartyID: body.CustomerPartyID,
		Carrier: body.Carrier, Mode: body.Mode, ContainerMode: body.ContainerMode, Origin: body.Origin, Destination: body.Destination, Via: body.Via,
		ServiceLevel: body.ServiceLevel, Commodity: body.Commodity, Cargo: api.Cargo{Pieces: body.Cargo.Pieces, GrossGrams: body.Cargo.GrossGrams,
			VolumeCM3: body.Cargo.VolumeCM3, Documents: body.Cargo.Documents}, SubjectKind: body.SubjectKind, SubjectID: body.SubjectID,
		SubjectVersion: body.SubjectVersion, OccurredAt: c.occurredAt})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteReceipt(w, nethttp.StatusCreated, receipt(rc), "/v1/rate-snapshots/"+url.PathEscape(rc.SubjectID))
}

type candidate struct {
	Side       string  `json:"side"`
	RateCardID string  `json:"rate_card_id"`
	Version    int     `json:"version"`
	EntryNo    int     `json:"entry_no"`
	Taken      bool    `json:"taken"`
	Reason     string  `json:"reason,omitempty"`
	Total      []money `json:"total"`
}

type snapshotLine struct {
	LineNo         int    `json:"line_no"`
	Side           string `json:"side"`
	ChargeCode     string `json:"charge_code"`
	RateCardID     string `json:"rate_card_id,omitempty"`
	Version        int    `json:"version,omitempty"`
	EntryNo        int    `json:"entry_no,omitempty"`
	RateLineNo     int    `json:"rate_line_no,omitempty"`
	Calculator     string `json:"calculator,omitempty"`
	Unit           string `json:"unit,omitempty"`
	Quantity       int64  `json:"quantity"`
	Charged        int64  `json:"charged"`
	BandFrom       int64  `json:"band_from"`
	Rate           int64  `json:"rate"`
	RateScale      int    `json:"rate_scale"`
	PercentBP      int64  `json:"percent_bp"`
	BaseAmount     int64  `json:"base_amount"`
	MinimumApplied bool   `json:"minimum_applied"`
	MaximumApplied bool   `json:"maximum_applied"`
	Amount         *money `json:"amount,omitempty"` // none on a gap: what RATE cannot rate is never zero
	Gap            string `json:"gap,omitempty"`
}

// GET /v1/rate-snapshots/{snapshot}: a recorded evaluation; its BUY lines and candidates only to a reader who may see
// costs.
func (h handlers) getSnapshot(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	s, err := h.s.GetRateSnapshot(r.Context(), api.GetRateSnapshot{TenantID: p.TenantID, SnapshotID: r.PathValue("snapshot")})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	costs := seesCosts(p)
	candidates := make([]candidate, 0, len(s.Candidates))
	for _, c := range s.Candidates {
		if c.Side == api.SideBuy && !costs {
			continue
		}
		candidates = append(candidates, candidate{Side: c.Side, RateCardID: c.RateCardID, Version: c.Version, EntryNo: c.EntryNo, Taken: c.Taken,
			Reason: c.Reason, Total: moneys(c.Total)})
	}
	lines := make([]snapshotLine, 0, len(s.Lines))
	for _, l := range s.Lines {
		if l.Side == api.SideBuy && !costs {
			continue
		}
		var amount *money
		if l.Gap == "" {
			m := moneyView(l.Amount)
			amount = &m
		}
		lines = append(lines, snapshotLine{LineNo: l.LineNo, Side: l.Side, ChargeCode: l.ChargeCode, RateCardID: l.RateCardID, Version: l.Version,
			EntryNo: l.EntryNo, RateLineNo: l.RateLineNo, Calculator: l.Calculator, Unit: l.Unit, Quantity: l.Quantity, Charged: l.Charged,
			BandFrom: l.BandFrom, Rate: l.Rate, RateScale: l.RateScale, PercentBP: l.PercentBP, BaseAmount: l.BaseAmount, MinimumApplied: l.MinimumApplied,
			MaximumApplied: l.MaximumApplied, Amount: amount, Gap: l.Gap})
	}
	out := map[string]any{"snapshot_id": s.SnapshotID, "purpose": s.Purpose, "legal_entity_id": s.LegalEntityID,
		"sides": orEmpty(s.Sides), "carrier": s.Carrier, "mode": s.Mode, "container_mode": s.ContainerMode, "origin": s.Origin, "destination": s.Destination, "via": s.Via,
		"edition": s.Edition, "service_level": s.ServiceLevel, "commodity": s.Commodity, "at": s.At, "cargo": cargo{Pieces: s.Cargo.Pieces,
			GrossGrams: s.Cargo.GrossGrams, VolumeCM3: s.Cargo.VolumeCM3, Documents: s.Cargo.Documents}, "subject_kind": s.SubjectKind,
		"subject_id": s.SubjectID, "subject_version": s.SubjectVersion, "engine_version": s.EngineVersion, "candidates": candidates, "lines": lines,
		"evaluated_by": s.EvaluatedBy, "recorded_at": s.RecordedAt}
	if s.Customer.PartyID != "" {
		out["customer"] = map[string]any{"party_id": s.Customer.PartyID, "party_revision": s.Customer.PartyRevision}
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, out)
}
