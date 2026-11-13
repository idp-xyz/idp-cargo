package http

import (
	nethttp "net/http"
	"net/url"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
)

// chargeCode is a code of the catalogue at its current version (0 for a starter code the tenant has not changed).
type chargeCode struct {
	Code       string    `json:"code"`
	NameZH     string    `json:"name_zh"`
	NameEN     string    `json:"name_en"`
	Group      string    `json:"group"`
	Calculator string    `json:"calculator"`
	Unit       string    `json:"unit,omitempty"`
	Modes      []string  `json:"modes"`
	Active     bool      `json:"active"`
	Version    int       `json:"version"`
	SetBy      string    `json:"set_by,omitempty"`
	SetAt      time.Time `json:"set_at,omitzero"`
}

func chargeCodeView(c api.ChargeCode) chargeCode {
	modes := c.Modes
	if modes == nil {
		modes = []string{}
	}
	return chargeCode{Code: c.Code, NameZH: c.NameZH, NameEN: c.NameEN, Group: c.Group, Calculator: c.Calculator, Unit: c.Unit, Modes: modes,
		Active: c.Active, Version: c.Version, SetBy: c.SetBy, SetAt: c.SetAt}
}

// chargeCodeBody is what a definition or revision states.
type chargeCodeBody struct {
	NameZH     string     `json:"name_zh"`
	NameEN     string     `json:"name_en"`
	Group      string     `json:"group"`
	Calculator string     `json:"calculator"`
	Unit       string     `json:"unit"`
	Modes      []string   `json:"modes"`
	OccurredAt *time.Time `json:"occurred_at"`
}

// GET /v1/charge-codes[?include_inactive=true]
func (h handlers) listChargeCodes(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	var inactive bool
	switch r.URL.Query().Get("include_inactive") {
	case "", "false":
	case "true":
		inactive = true
	default:
		httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", "include_inactive is true or false")
		return
	}
	codes, err := h.s.ListChargeCodes(r.Context(), api.ListChargeCodes{TenantID: p.TenantID, IncludeInactive: inactive})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	out := make([]chargeCode, 0, len(codes))
	for _, c := range codes {
		out = append(out, chargeCodeView(c))
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, map[string]any{"charge_codes": out})
}

// GET /v1/charge-codes/{code}; ETag is the code's version.
func (h handlers) getChargeCode(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	c, err := h.s.GetChargeCode(r.Context(), api.GetChargeCode{TenantID: p.TenantID, Code: r.PathValue("code")})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.ETag(w, int64(c.Version))
	httpapi.WriteJSON(w, nethttp.StatusOK, chargeCodeView(c))
}

// POST /v1/charge-codes: DefineChargeCode; 201 with the code's Location.
func (h handlers) defineChargeCode(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Code string `json:"code"`
		chargeCodeBody
	}
	if !httpapi.Decode(w, r, &body) {
		return
	}
	c, ok := begin(w, r, body.OccurredAt)
	if !ok {
		return
	}
	rc, err := h.s.DefineChargeCode(r.Context(), api.DefineChargeCode{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID, Code: body.Code,
		NameZH: body.NameZH, NameEN: body.NameEN, Group: body.Group, Calculator: body.Calculator, Unit: body.Unit, Modes: body.Modes,
		OccurredAt: c.occurredAt})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteReceipt(w, nethttp.StatusCreated, receipt(rc), "/v1/charge-codes/"+url.PathEscape(rc.SubjectID))
}

// POST /v1/charge-codes/{code}/revise: ReviseChargeCode at If-Match's version.
func (h handlers) reviseChargeCode(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body chargeCodeBody
	if !httpapi.Decode(w, r, &body) {
		return
	}
	version, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	c, ok := begin(w, r, body.OccurredAt)
	if !ok {
		return
	}
	rc, err := h.s.ReviseChargeCode(r.Context(), api.ReviseChargeCode{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
		Code: r.PathValue("code"), ExpectedVersion: version, NameZH: body.NameZH, NameEN: body.NameEN, Group: body.Group, Calculator: body.Calculator,
		Unit: body.Unit, Modes: body.Modes, OccurredAt: c.occurredAt})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteReceipt(w, nethttp.StatusOK, receipt(rc), "")
}

// onlyTime is the body of a command that states nothing but its business time.
type onlyTime struct {
	OccurredAt *time.Time `json:"occurred_at"`
}

// POST /v1/charge-codes/{code}/deactivate: DeactivateChargeCode at If-Match's version.
func (h handlers) deactivateChargeCode(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.toggle(w, r, func(c command, code string, version int) (api.Receipt, error) {
		return h.s.DeactivateChargeCode(r.Context(), api.DeactivateChargeCode{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			Code: code, ExpectedVersion: version, OccurredAt: c.occurredAt})
	})
}

// POST /v1/charge-codes/{code}/reactivate: ReactivateChargeCode at If-Match's version.
func (h handlers) reactivateChargeCode(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.toggle(w, r, func(c command, code string, version int) (api.Receipt, error) {
		return h.s.ReactivateChargeCode(r.Context(), api.ReactivateChargeCode{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			Code: code, ExpectedVersion: version, OccurredAt: c.occurredAt})
	})
}

func (h handlers) toggle(w nethttp.ResponseWriter, r *nethttp.Request, run func(command, string, int) (api.Receipt, error)) {
	var body onlyTime
	if !httpapi.Decode(w, r, &body) {
		return
	}
	version, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	c, ok := begin(w, r, body.OccurredAt)
	if !ok {
		return
	}
	rc, err := run(c, r.PathValue("code"), version)
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteReceipt(w, nethttp.StatusOK, receipt(rc), "")
}
