// Package http is FIN's inbound HTTP adapter (V2.99): it turns requests into FIN's api commands and queries, with the
// tenant and actor of the verified principal, and holds no business rules. Paths are under /v1/fin; JSON is snake_case.
// FIN's authorizer decides on each call (fin:ReadProfit, fin:RequestCostReview, fin:ResolveCostReview): the adapter
// names the token's tenant and actor, and a body that names a tenant or a person is refused as an unknown field.
package http

import (
	nethttp "net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Service is what the adapter calls: the commands and queries of the cost review and the profit.
type Service interface {
	api.Commands
	api.Queries
}

var kinds = httpapi.Errors{Unauthenticated: api.ErrUnauthenticated, Unauthorized: api.ErrUnauthorized, NotFound: api.ErrNotFound,
	Conflict: api.ErrConflict, IdempotencyConflict: api.ErrIdempotencyConflict, Rejected: api.ErrRejected}

// Register registers FIN's routes.
func Register(mux *nethttp.ServeMux, s Service) {
	h := handlers{s}
	mux.HandleFunc("GET /v1/fin/legal-entities/{legalEntityId}/job-profits", h.listJobProfits)
	mux.HandleFunc("GET /v1/fin/jobs/{jobId}/profit", h.getJobProfit)
	mux.HandleFunc("POST /v1/fin/cost-review-requests", h.requestCostReview)
	mux.HandleFunc("GET /v1/fin/cost-review-requests/{requestId}", h.getCostReviewRequest)
	mux.HandleFunc("GET /v1/fin/jobs/{jobId}/cost-review-requests", h.listCostReviewRequests)
	mux.HandleFunc("POST /v1/fin/cost-review-requests/{requestId}/resolution", h.resolveCostReview)
}

type handlers struct{ s Service }

type money struct {
	Currency string `json:"currency"`
	Minor    int64  `json:"minor"`
}

func moneyOf(m kernel.Money) money { return money{Currency: string(m.Currency), Minor: m.Minor} }

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// principal answers the verified principal, or writes the 401.
func principal(w nethttp.ResponseWriter, r *nethttp.Request) (identity.Principal, bool) {
	p, ok := identity.PrincipalFrom(r.Context())
	if !ok {
		httpapi.WriteProblem(w, r, nethttp.StatusUnauthorized, "UNAUTHENTICATED", "")
	}
	return p, ok
}

// pathID reads a path value that is an identifier.
func pathID(w nethttp.ResponseWriter, r *nethttp.Request, name string) (string, bool) {
	v := r.PathValue(name)
	if !identifier.MatchString(v) {
		httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", name+" is 1 to 128 letters, digits and . _ : / -")
		return "", false
	}
	return v, true
}

var requestID = regexp.MustCompile(`^CRR-[1-9][0-9]{0,8}$`)

// queryKnown refuses a query parameter the route does not define, naming it.
func queryKnown(w nethttp.ResponseWriter, r *nethttp.Request, known ...string) bool {
	var unknown []string
	for k := range r.URL.Query() {
		if !slices.Contains(known, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return true
	}
	slices.Sort(unknown)
	httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", "unknown query parameter "+strconv.Quote(unknown[0])+
		"; this route takes "+strconv.Quote(strings.Join(known, ", ")))
	return false
}

// text refuses a value with a control character (a NUL byte, an escape; with multiline false, a line break and a tab
// too) or bytes that are not UTF-8, naming the field, before it reaches the database.
func text(w nethttp.ResponseWriter, r *nethttp.Request, field, value string, multiline bool) bool {
	ok := utf8.ValidString(value)
	for _, c := range value {
		if unicode.IsControl(c) && !(multiline && (c == '\n' || c == '\r' || c == '\t')) {
			ok = false
		}
	}
	if !ok {
		httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", "field "+strconv.Quote(field)+" has a control character or is not UTF-8")
	}
	return ok
}

// page reads limit (1 to 500, default 100) and after, which must match cursor (the form of what a page gave).
func page(w nethttp.ResponseWriter, r *nethttp.Request, cursor *regexp.Regexp) (limit int, after string, ok bool) {
	limit = 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", "limit is a number from 1 to 500")
			return 0, "", false
		}
		limit = n
	}
	after = r.URL.Query().Get("after")
	if after != "" && !cursor.MatchString(after) {
		httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", "after is the next_after a page gave")
		return 0, "", false
	}
	return limit, after, true
}

type jobProfitRow struct {
	JobID           string `json:"job_id"`
	ShipmentID      string `json:"shipment_id"`
	CustomerPartyID string `json:"customer_party_id"`
	State           string `json:"state"`
	Currency        string `json:"currency"`
	Revenue         money  `json:"revenue"`
	Cost            money  `json:"cost"`
	Profit          money  `json:"profit"`
	ProfitBasis     string `json:"profit_basis"`
	SettingsVersion int    `json:"settings_version"`
	ProfitVersion   string `json:"profit_version"`
}

func (h handlers) listJobProfits(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	entity, ok := pathID(w, r, "legalEntityId")
	if !ok || !queryKnown(w, r, "loss_only", "limit", "after") {
		return
	}
	lossOnly := false
	switch v := r.URL.Query().Get("loss_only"); v {
	case "", "false":
	case "true":
		lossOnly = true
	default:
		httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", "loss_only is true or false")
		return
	}
	limit, after, ok := page(w, r, identifier)
	if !ok {
		return
	}
	got, err := h.s.ListJobProfits(r.Context(), api.ListJobProfits{TenantID: p.TenantID, ActorID: p.ActorID, LegalEntityID: entity, LossOnly: lossOnly, Limit: limit, After: after})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	rows := make([]jobProfitRow, 0, len(got.Rows))
	for _, x := range got.Rows {
		rows = append(rows, jobProfitRow{JobID: x.JobID, ShipmentID: x.ShipmentID, CustomerPartyID: x.CustomerPartyID, State: x.State, Currency: string(x.Currency),
			Revenue: moneyOf(x.Revenue), Cost: moneyOf(x.Cost), Profit: moneyOf(x.Profit), ProfitBasis: x.ProfitBasis, SettingsVersion: x.SettingsVersion,
			ProfitVersion: x.ProfitVersion})
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, map[string]any{"legal_entity_id": got.LegalEntityID, "as_of": got.AsOf, "rows": rows, "next_after": got.NextAfter})
}

func (h handlers) getJobProfit(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "jobId")
	if !ok || !queryKnown(w, r) {
		return
	}
	got, err := h.s.GetJobProfit(r.Context(), api.GetJobProfit{TenantID: p.TenantID, ActorID: p.ActorID, JobID: id})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	type code struct {
		ChargeCode string `json:"charge_code"`
		Revenue    money  `json:"revenue"`
		Cost       money  `json:"cost"`
		Profit     money  `json:"profit"`
	}
	type share struct {
		PoolID     string `json:"pool_id"`
		LineNo     int    `json:"line_no"`
		ChargeCode string `json:"charge_code"`
		Method     string `json:"method"`
		Local      money  `json:"local"`
		Pending    bool   `json:"pending"`
	}
	type loss struct {
		PoolID  string `json:"pool_id"`
		LineNo  int    `json:"line_no"`
		Local   money  `json:"local"`
		Voucher string `json:"voucher"`
	}
	byCode := []code{}
	for _, c := range got.ByCode {
		byCode = append(byCode, code{c.ChargeCode, moneyOf(c.Revenue), moneyOf(c.Cost), moneyOf(c.Profit)})
	}
	shares := []share{}
	for _, s := range append(append([]api.JobPoolShare{}, got.PoolShares...), got.FormerShares...) {
		shares = append(shares, share{s.PoolID, s.LineNo, s.ChargeCode, s.Method, moneyOf(s.Local), s.Pending})
	}
	losses := []loss{}
	for _, l := range got.ReroutingLosses {
		losses = append(losses, loss{l.PoolID, l.LineNo, moneyOf(l.Local), l.Voucher})
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, map[string]any{"job_id": got.JobID, "revision": got.Revision, "profit_version": got.ProfitVersion, "revenue": moneyOf(got.Revenue),
		"invoiced": moneyOf(got.Invoiced), "cost": moneyOf(got.Cost), "cost_matched": moneyOf(got.CostMatched), "cost_accrued": moneyOf(got.CostAccrued),
		"pool_cost": moneyOf(got.PoolCost), "profit": moneyOf(got.Profit), "profit_basis": got.ProfitBasis, "settings_version": got.SettingsVersion,
		"pending_in_profit": moneyOf(got.PendingInProfit), "former_shares_pending": moneyOf(got.FormerSharesPending), "rerouting_loss": moneyOf(got.ReroutingLoss),
		"by_charge_code": byCode, "pool_shares": shares, "rerouting_losses": losses})
}

type resolution struct {
	ResolvedBy string    `json:"resolved_by"`
	ResolvedAt time.Time `json:"resolved_at"`
	Outcome    string    `json:"outcome"`
	Note       string    `json:"note"`
}

type reviewRequest struct {
	RequestID       string      `json:"request_id"`
	JobID           string      `json:"job_id"`
	ShipmentID      string      `json:"shipment_id"`
	LegalEntityID   string      `json:"legal_entity_id"`
	State           string      `json:"state"`
	RequestedBy     string      `json:"requested_by"`
	RequestedAt     time.Time   `json:"requested_at"`
	Reason          string      `json:"reason"`
	Revenue         money       `json:"revenue"`
	Cost            money       `json:"cost"`
	Profit          money       `json:"profit"`
	ProfitBasis     string      `json:"profit_basis"`
	SettingsVersion int         `json:"settings_version"`
	ProfitVersion   string      `json:"profit_version"`
	Resolution      *resolution `json:"resolution"`
}

func reviewOf(x api.CostReviewRequest) reviewRequest {
	out := reviewRequest{RequestID: x.RequestID, JobID: x.JobID, ShipmentID: x.ShipmentID, LegalEntityID: x.LegalEntityID, State: x.State, RequestedBy: x.RequestedBy,
		RequestedAt: x.RequestedAt, Reason: x.Reason, Revenue: moneyOf(x.Revenue), Cost: moneyOf(x.Cost), Profit: moneyOf(x.Profit), ProfitBasis: x.ProfitBasis,
		SettingsVersion: x.SettingsVersion, ProfitVersion: x.ProfitVersion}
	if x.Resolution != nil {
		out.Resolution = &resolution{x.Resolution.ResolvedBy, x.Resolution.ResolvedAt, x.Resolution.Outcome, x.Resolution.Note}
	}
	return out
}

func (h handlers) getCostReviewRequest(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "requestId")
	if !ok || !queryKnown(w, r) {
		return
	}
	got, err := h.s.GetCostReviewRequest(r.Context(), api.GetCostReviewRequest{TenantID: p.TenantID, ActorID: p.ActorID, RequestID: id})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, reviewOf(got))
}

func (h handlers) listCostReviewRequests(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "jobId")
	if !ok || !queryKnown(w, r, "state", "limit", "after") {
		return
	}
	state := r.URL.Query().Get("state")
	if state != "" && state != api.ReviewOpen && state != api.ReviewResolved {
		httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", "state is OPEN or RESOLVED")
		return
	}
	limit, after, ok := page(w, r, requestID)
	if !ok {
		return
	}
	got, err := h.s.ListCostReviewRequests(r.Context(), api.ListCostReviewRequests{TenantID: p.TenantID, ActorID: p.ActorID, JobID: id, State: state, Limit: limit, After: after})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	out := make([]reviewRequest, 0, len(got.Requests))
	for _, x := range got.Requests {
		out = append(out, reviewOf(x))
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, map[string]any{"job_id": id, "requests": out, "next_after": got.NextAfter})
}

// receipt is the response to a command: the request the command made or ended, its revision (1 when requested, 2 when
// resolved) and, for a request, its number.
type receipt struct {
	CommandID   string `json:"command_id"`
	RequestID   string `json:"request_id"`
	NewRevision int64  `json:"new_revision"`
	RequestNo   int    `json:"request_no"`
}

func (h handlers) requestCostReview(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		JobID         string `json:"job_id"`
		ProfitVersion string `json:"profit_version"`
		Reason        string `json:"reason"`
	}
	if !httpapi.Decode(w, r, &body) || !text(w, r, "job_id", body.JobID, false) || !text(w, r, "profit_version", body.ProfitVersion, false) ||
		!text(w, r, "reason", body.Reason, true) {
		return
	}
	id, ok := httpapi.CommandID(w, r)
	if !ok {
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	rc, err := h.s.RequestCostReview(r.Context(), api.RequestCostReview{CommandID: id, TenantID: p.TenantID, ActorID: p.ActorID, JobID: body.JobID,
		ProfitVersion: body.ProfitVersion, Reason: body.Reason})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.ETag(w, rc.NewRevision)
	w.Header().Set("Location", "/v1/fin/cost-review-requests/"+url.PathEscape(rc.SubjectID))
	httpapi.WriteJSON(w, nethttp.StatusCreated, receipt{CommandID: rc.CommandID, RequestID: rc.SubjectID, NewRevision: rc.NewRevision, RequestNo: rc.Version})
}

func (h handlers) resolveCostReview(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Outcome string `json:"outcome"`
		Note    string `json:"note"`
	}
	if !httpapi.Decode(w, r, &body) || !text(w, r, "outcome", body.Outcome, false) || !text(w, r, "note", body.Note, true) {
		return
	}
	request, ok := pathID(w, r, "requestId")
	if !ok {
		return
	}
	id, ok := httpapi.CommandID(w, r)
	if !ok {
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	rc, err := h.s.ResolveCostReview(r.Context(), api.ResolveCostReview{CommandID: id, TenantID: p.TenantID, ActorID: p.ActorID, RequestID: request,
		Outcome: body.Outcome, Note: body.Note})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.ETag(w, rc.NewRevision)
	httpapi.WriteJSON(w, nethttp.StatusOK, receipt{CommandID: rc.CommandID, RequestID: rc.SubjectID, NewRevision: rc.NewRevision, RequestNo: rc.Version})
}
