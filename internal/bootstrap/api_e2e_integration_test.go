//go:build integration

package bootstrap

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// M is a JSON object of a request body.
type M = map[string]any

// flow drives the API alone (stage 3, acceptance 4): every step is a request with a job role's token; nothing is
// written to the database directly and no module is called in process.
type flow struct {
	t      *testing.T
	c      apiClient
	cfg    APIConfig
	tenant string
	at     time.Time
	keys   int
	tokens map[string]string
}

func newFlow(t *testing.T, prefix string) *flow {
	t.Helper()
	p := openPlatform(t)
	cfg := APIConfig{TokenKey: []byte("api-test-key-of-thirty-two-bytes!!"), TokenIssuer: "idp-cargo-test", TokenAudience: "idp-cargo-api",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MarginFloorBP: 2000}
	h, err := p.API(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &flow{t: t, c: apiClient{t: t, srv: srv}, cfg: cfg, tenant: uniqueTenant(prefix), at: time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second),
		tokens: map[string]string{}}
}

// as is the token of a person holding the roles' default permissions.
func (f *flow) as(person string, roles ...string) string {
	f.t.Helper()
	key := person + "/" + strings.Join(roles, ",")
	if tok, ok := f.tokens[key]; ok {
		return tok
	}
	perms, err := RolePermissions(roles, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	tok, err := MintDevToken(f.cfg, f.tenant, person, perms, time.Hour)
	if err != nil {
		f.t.Fatal(err)
	}
	f.tokens[key] = tok
	return tok
}

// hours is the business time h hours after the flow's start, in RFC 3339.
func (f *flow) hours(h int) string {
	return f.at.Add(time.Duration(h) * time.Hour).Format(time.RFC3339)
}

// etag reads a resource's ETag.
func (f *flow) etag(tok, path string) string {
	f.t.Helper()
	r := f.c.do("GET", path, tok, "")
	if r.status != 200 || r.header.Get("ETag") == "" {
		f.t.Fatalf("GET %s: %d %v", path, r.status, r.body)
	}
	return r.header.Get("ETag")
}

// post sends a command (a fresh Idempotency-Key) and fails the test unless it succeeds; ifMatch "" sends none, and
// "GET <path>" sends the ETag that path reads now.
func (f *flow) post(tok, path, ifMatch string, body M) map[string]any {
	f.t.Helper()
	if rest, ok := strings.CutPrefix(ifMatch, "GET "); ok {
		ifMatch = f.etag(tok, rest)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	f.keys++
	headers := []string{"Idempotency-Key", fmt.Sprintf("k-%d", f.keys)}
	if ifMatch != "" {
		headers = append(headers, "If-Match", ifMatch)
	}
	r := f.c.do("POST", path, tok, string(raw), headers...)
	if r.status != 200 && r.status != 201 {
		f.t.Fatalf("POST %s (If-Match %s) %s: %d %v", path, ifMatch, raw, r.status, r.body)
	}
	return r.body
}

// refused sends a command that must be refused with the status.
func (f *flow) refused(name, tok, path, ifMatch string, body M, status int) {
	f.t.Helper()
	if rest, ok := strings.CutPrefix(ifMatch, "GET "); ok {
		ifMatch = f.etag(tok, rest)
	}
	raw, _ := json.Marshal(body)
	f.keys++
	headers := []string{"Idempotency-Key", fmt.Sprintf("k-%d", f.keys)}
	if ifMatch != "" {
		headers = append(headers, "If-Match", ifMatch)
	}
	if r := f.c.do("POST", path, tok, string(raw), headers...); r.status != status {
		f.t.Fatalf("%s: %d %v, want %d", name, r.status, r.body, status)
	}
}

func (f *flow) get(tok, path string) map[string]any {
	f.t.Helper()
	r := f.c.do("GET", path, tok, "")
	if r.status != 200 {
		f.t.Fatalf("GET %s: %d %v", path, r.status, r.body)
	}
	return r.body
}

func loc(code string) M  { return M{"scheme": "UNLOCODE", "code": code} }
func jcny(minor int64) M { return M{"currency": "CNY", "minor": minor} }
func kgm(amount int64) M { return M{"unit": "KGM", "amount": amount} }
func evidenceAt(source, ref, at string) M {
	return M{"source": source, "reference": ref, "occurred_at": at}
}

// The owner's scheme of 2026-11-18 through the API alone (stage 3, acceptance 4): C-1 of two 22G1 boxes, box A with
// H01, H02 and H03 (10, 15 and 5 W/M), box B with H04 and H05 (20 and 10 W/M); the parties, quotes, orders, shipments,
// the consolidation, the warehouse's receipts, the stuffing and the seals, the booking and the ocean movement, the
// freight of each box and the document fee apportioned. Then the carrier rolls box B to EVER GLORY a week later: a new
// booking, a movement of its own, the master bills and finalizations; box B's storage fee of 300.00 falls on H04 and
// H05 by 20:10. Each house's revenue, cost and profit are FIN's answer at the end.
func TestTheOwnersTwoBoxesRunThroughTheAPIAlone(t *testing.T) {
	f := newFlow(t, "cargo-e2e-sea-")
	admin, ops, wh, docs := f.as("ada", "admin"), f.as("otto", "ops"), f.as("walt", "warehouse"), f.as("dora", "docs")
	fin, manager := f.as("finn", "finance"), f.as("mia", "finance-manager")
	houses := []string{"H01", "H02", "H03", "H04", "H05"}
	weights := map[string]int64{"H01": 312, "H02": 300, "H03": 288, "H04": 400, "H05": 350}
	volumes := map[string]int64{"H01": 10_000_000, "H02": 15_000_000, "H03": 5_000_000, "H04": 20_000_000, "H05": 10_000_000}
	sell := map[string][2]int64{"H01": {170_000, 10_000}, "H02": {240_000, 10_000}, "H03": {80_000, 10_000}, "H04": {250_000, 10_000}, "H05": {130_000, 10_000}}
	box := map[string]int{"H01": 1, "H02": 1, "H03": 1, "H04": 2, "H05": 2}
	departs := f.at.Add(72 * time.Hour)
	lower := strings.ToLower

	// 1. ORG: the forwarder's company and the parties, with their roles and the shippers' and consignee's offices
	f.post(admin, "/v1/legal-entities", "", M{"legal_entity_id": "LE", "registered_name": "上海货代有限公司", "jurisdiction": "CN", "evidence_ref": "licence", "occurred_at": f.hours(0)})
	f.post(admin, "/v1/legal-entities/LE/verify", `"1"`, M{"occurred_at": f.hours(0)})
	party := func(id, name, country, role string) {
		f.post(admin, "/v1/parties", "", M{"party_id": id, "legal_name": name, "country_code": country, "occurred_at": f.hours(0)})
		f.post(admin, "/v1/parties/"+id+"/activate", `"1"`, M{"evidence_ref": "registry extract", "occurred_at": f.hours(0)})
		f.post(admin, "/v1/parties/"+id+"/roles", `"2"`, M{"legal_entity_id": "LE", "role_code": role, "valid_from": f.at.Add(-time.Hour).Format(time.RFC3339),
			"evidence_ref": "contract", "occurred_at": f.hours(0)})
		if role == "SHIPPER" || role == "CONSIGNEE" {
			f.post(ops, "/v1/parties/"+id+"/addresses", `"0"`, M{"address_id": "HQ", "address": M{"short_code": "HQ", "purposes": []string{"OFFICE"}, "name": name,
				"lines": []string{"1 Harbour Road"}, "city": map[string]string{"CN": "Shanghai", "DE": "Hamburg"}[country], "country_code": country}, "default_for": []string{"OFFICE"}})
		}
	}
	party("line-1", "Evergreen Line", "CN", "CARRIER")
	party("consignee-de", "Hamburg Import GmbH", "DE", "CONSIGNEE")
	for _, h := range houses {
		party("party-"+lower(h), "Customer "+h, "CN", "CUSTOMER")
		party("shipper-"+lower(h), "Shipper "+h, "CN", "SHIPPER")
	}

	// 2. RATE, COM and FUL: a quote, an order and a shipment for each house
	for _, h := range houses {
		q, o, ob, s := "Q-"+h, "O-"+h, "OB-"+h, "SH-"+h
		f.post(ops, "/v1/quotes", "", M{"quote_id": q, "legal_entity_id": "LE", "customer_party_id": "party-" + lower(h), "currency": "USD",
			"lines":      []M{{"charge_code": "OCEAN_FREIGHT", "sell": M{"currency": "USD", "minor": 60_000}, "cost": M{"currency": "USD", "minor": 0}}},
			"valid_from": f.hours(0), "valid_until": f.at.Add(14 * 24 * time.Hour).Format(time.RFC3339), "occurred_at": f.hours(0)})
		f.post(ops, "/v1/quotes/"+q+"/accept", `"1"`, M{"evidence_kind": "EMAIL", "evidence_ref": "mail", "occurred_at": f.hours(1)})
		f.post(ops, "/v1/orders", "", M{"order_id": o, "quote_id": q, "customer_party_id": "party-" + lower(h), "transport": M{"mode": "SEA", "container_mode": "LCL"},
			"lines": []M{{"description": "Cartons of furniture parts", "packs": 6, "package_type": "CT", "gross_weight": kgm(300)}}, "occurred_at": f.hours(2)})
		f.post(ops, "/v1/orders/"+o+"/obligations", `"1"`, M{"line_no": 1, "obligation_id": ob, "packs": 6, "gross_weight": kgm(300), "occurred_at": f.hours(3)})
		for role, id := range map[string]string{"SHIPPER": "shipper-" + lower(h), "CONSIGNEE": "consignee-de"} {
			f.post(ops, "/v1/orders/"+o+"/parties", "GET /v1/orders/"+o, M{"party": M{"role": role, "party_id": id}, "occurred_at": f.hours(3)})
		}
		f.post(ops, "/v1/obligations/"+ob+"/reservations", "", M{"obligation_version": 1, "shipment_id": s, "occurred_at": f.hours(4)})
		f.post(ops, "/v1/shipments", "", M{"shipment_id": s, "obligation_id": ob, "obligation_version": 1, "occurred_at": f.hours(5)})
		f.post(ops, "/v1/shipments/"+s+"/plan", `"1"`, M{"transport_mode": "SEA", "container_mode": "LCL", "origin": loc("CNSHA"), "destination": loc("DEHAM"),
			"departure_window": M{"from": departs.Format(time.RFC3339), "until": departs.Add(48 * time.Hour).Format(time.RFC3339)}, "reason": "vessel booked", "occurred_at": f.hours(6)})
		f.post(ops, "/v1/shipments/"+s+"/confirm", `"2"`, M{"occurred_at": f.hours(7)})
	}

	// 3. PHY: C-1 of two containers, the houses attached (and recorded in FUL), closed; the warehouse receives and measures
	f.post(ops, "/v1/consolidations", "", M{"consolidation_id": "C-1", "mode": "SEA", "kind": "SEA_LCL_OWN", "operating_legal_entity_id": "LE", "origin": loc("CNSHA"),
		"destination": loc("DEHAM"), "planned_departure": departs.Add(6 * time.Hour).Format(time.RFC3339), "house_cut_off": departs.Add(-6 * time.Hour).Format(time.RFC3339),
		"occurred_at": f.hours(8)})
	for i := range 2 {
		f.post(ops, "/v1/consolidations/C-1/load-units", fmt.Sprintf(`"%d"`, 1+i), M{"kind": "CONTAINER", "type_code": "22G1", "occurred_at": f.hours(8)})
	}
	for _, h := range houses {
		f.post(ops, "/v1/consolidations/C-1/houses", "", M{"shipment_id": "SH-" + h, "expected_membership_revision": 0, "occurred_at": f.hours(9)})
		f.post(ops, "/v1/shipments/SH-"+h+"/consol-attachments", "GET /v1/shipments/SH-"+h, M{"consolidation_id": "C-1", "occurred_at": f.hours(9)})
	}
	f.post(ops, "/v1/consolidations/C-1/close", "GET /v1/consolidations/C-1", M{"occurred_at": f.hours(13)})
	cfs := M{"location": loc("CNSHG"), "code": "SHA-CFS1"}
	for _, h := range houses {
		lot := "/v1/cargo-lots/L-" + h
		f.post(wh, lot+"/receipts", `"0"`, M{"shipment_id": "SH-" + h, "facility": cfs, "packs": 6, "package_type": "CT", "condition": "GOOD",
			"evidence": evidenceAt("CFS-WMS", "GRN-"+h, f.hours(14)), "occurred_at": f.hours(14)})
		f.post(wh, lot+"/measurements", `"1"`, M{"gross_weight": kgm(weights[h]), "volume": M{"unit": "CMQ", "amount": volumes[h]}, "method": "SCALE",
			"evidence": evidenceAt("CFS-SCALE", "W-"+h, f.hours(14)), "occurred_at": f.hours(14)})
		f.post(wh, lot+"/complete", `"2"`, M{"occurred_at": f.hours(14)})
	}

	// 4. PHY: box A and box B stuffed and sealed; FUL takes the evidence
	for x, unit := range map[string]int{"X-1": 1, "X-2": 2} {
		f.post(wh, "/v1/load-executions", "", M{"execution_id": x, "consolidation_id": "C-1", "kind": "STUFFING", "facility": cfs, "load_unit_no": unit, "occurred_at": f.hours(15)})
		f.post(wh, "/v1/load-executions/"+x+"/start", `"1"`, M{"occurred_at": f.hours(15)})
		for _, h := range houses {
			if box[h] == unit {
				f.post(wh, "/v1/load-executions/"+x+"/loaded", "GET /v1/load-executions/"+x, M{"lot_id": "L-" + h, "packs": 6, "evidence": evidenceAt("CFS-TALLY", "T-"+x+h, f.hours(15)),
					"occurred_at": f.hours(15)})
			}
		}
		f.post(wh, "/v1/load-executions/"+x+"/close", "GET /v1/load-executions/"+x, M{"evidence": evidenceAt("SEAL", "SEAL-"+x, f.hours(15)), "occurred_at": f.hours(15)})
	}
	for _, h := range houses {
		f.post(ops, "/v1/shipments/SH-"+h+"/load-evidence", "GET /v1/shipments/SH-"+h, M{"occurred_at": f.hours(16)})
	}

	// 5. BOOK and SEA: the booking of two boxes on EVER GIVEN, OM-1 with its houses and containers, sealed, stuffed, the
	// VGMs, the master bill
	booking := func(id string, vessel, voyage string, etd time.Time, hour int) {
		evidence := M{"sha256": strings.Repeat("6b", 32), "storage_id": "mail/line.eml", "media_type": "message/rfc822", "captured_at": f.hours(hour)}
		f.post(ops, "/v1/bookings", "", M{"booking_id": id, "kind": "SEA_LCL_CONSOL_EQUIPMENT", "legal_entity_id": "LE", "carrier_party_id": "line-1", "consolidation_id": "C-1",
			"request": M{"legs": []M{{"loading": loc("CNSHA"), "discharge": loc("DEHAM"), "departure_date": etd.Format(time.DateOnly)}},
				"capacity": M{"equipment": []M{{"size_type": "22G1", "count": 2}}}}, "occurred_at": f.hours(hour)})
		f.post(ops, "/v1/bookings/"+id+"/sent", `"1"`, M{"channel": "CARRIER_PORTAL", "evidence": evidence, "occurred_at": f.hours(hour)})
		f.post(ops, "/v1/bookings/"+id+"/confirmation", `"2"`, M{"answer": M{"channel": "EMAIL", "evidence": evidence,
			"references": []M{{"type": "BOOKING_NUMBER", "value": "LINE-" + id}}},
			"lines": []M{{"capacity": M{"equipment": []M{{"size_type": "22G1", "count": 2}}}, "legs": []M{{"loading": loc("CNSHA"), "discharge": loc("DEHAM"),
				"vessel": vessel, "voyage": voyage, "etd": etd.Format(time.RFC3339), "eta": etd.Add(30 * 24 * time.Hour).Format(time.RFC3339),
				"cut_offs": []M{{"kind": "VGM", "at": etd.Add(-36 * time.Hour).Format(time.RFC3339), "time_zone": "Asia/Shanghai"},
					{"kind": "SHIPPING_INSTRUCTIONS_DOCUMENTS", "at": etd.Add(-48 * time.Hour).Format(time.RFC3339), "time_zone": "Asia/Shanghai"}}}}}},
			"occurred_at": f.hours(hour)})
	}
	booking("BK-1", "EVER GIVEN", "0123W", f.at.Add(78*time.Hour), 10)
	f.post(ops, "/v1/ocean-movements", "", M{"movement_id": "OM-1", "consolidation_id": "C-1", "booking_id": "BK-1", "occurred_at": f.hours(11)})
	f.post(ops, "/v1/hbl-series", "", M{"legal_entity_id": "LE", "template": "SHAL{seq:7}", "first_number": 1, "occurred_at": f.hours(11)})
	om := func(id string) string { return "GET /v1/ocean-movements/" + id }
	for _, h := range houses {
		f.post(ops, "/v1/ocean-movements/OM-1/house-bills", om("OM-1"), M{"shipment_id": "SH-" + h, "occurred_at": f.hours(11)})
	}
	for i, c := range []struct{ visit, number string }{{"CV-1", "csqu3054383"}, {"CV-2", "temu1234565"}} {
		f.post(ops, "/v1/container-visits", "", M{"visit_id": c.visit, "movement_id": "OM-1", "load_unit_no": 1 + i, "container_number": c.number, "size_type": "22G1",
			"tare_kg": 2180, "max_gross_kg": 30480, "plate_source": "CSC_PLATE", "occurred_at": f.hours(12)})
	}
	cv := func(id string) string { return "GET /v1/container-visits/" + id }
	for i, v := range []string{"CV-1", "CV-2"} {
		f.post(ops, "/v1/container-visits/"+v+"/seals", cv(v), M{"number": fmt.Sprintf("seal-x-%d", i+1), "party": "SHIPPER", "applied_at": f.hours(15),
			"source": "tally sheet", "occurred_at": f.hours(17)})
		f.post(wh, "/v1/container-visits/"+v+"/stuffing-done", cv(v), M{"occurred_at": f.hours(18)})
		f.post(ops, "/v1/container-visits/"+v+"/vgm-assertions", cv(v), M{"mass": kgm([]int64{2900, 3000}[i]), "method": "METHOD_2", "responsible_party_id": "LE",
			"responsible_party_revision": 1, "determined_at": f.hours(19), "signatory": "Ada", "occurred_at": f.hours(19)})
	}
	bill := func(movement, number string, hour int) {
		f.post(ops, "/v1/ocean-movements/"+movement+"/master-bills", om(movement), M{"number": number, "kind": "CARRIER_MBL", "evidence": M{"sha256": strings.Repeat("9c", 32),
			"storage_id": "mail/mbl.pdf", "media_type": "application/pdf", "captured_at": f.hours(hour)}, "occurred_at": f.hours(hour)})
	}
	bill("OM-1", "linu2611000457", 18)

	// 6. FIN: the jobs and their revenue, the pool of C-1: each box's freight a line of its container, the document fee
	// by shipment, apportioned by one person and submitted by another
	f.post(fin, "/v1/fin/legal-entities/LE/policy", "", M{"functional_currency": "CNY", "book_code": "MAIN", "occurred_at": f.hours(10)})
	f.post(fin, "/v1/fin/legal-entities/LE/exchange-rates", "GET /v1/fin/legal-entities/LE/policy", M{"rate_type": "ESTIMATE", "from": "USD", "rate": 71_234, "scale": 4,
		"from_digits": 2, "to_digits": 2, "valid_from": f.hours(0), "valid_until": f.at.AddDate(0, 1, 0).Format(time.RFC3339), "source": "CFETS central parity", "occurred_at": f.hours(10)})
	for _, h := range houses {
		job := "/v1/fin/jobs/JOB-" + h
		f.post(fin, "/v1/fin/jobs", "", M{"job_id": "JOB-" + h, "shipment_id": "SH-" + h, "occurred_at": f.hours(10)})
		// the scheme's revenue is the two sell lines below (9,200.00 in all), so the quote's pricing basis is not applied
		for i, code := range []string{"OCEAN_FREIGHT", "DOC_FEE"} {
			f.post(fin, job+"/charge-lines", "GET "+job, M{"charge_code": code, "sell": M{"party_id": "party-" + lower(h), "amount": jcny(sell[h][i])},
				"reason": "the customer's price", "occurred_at": f.hours(12)})
		}
	}
	f.post(fin, "/v1/fin/pools", "", M{"pool_id": "P-1", "consolidation_id": "C-1", "occurred_at": f.hours(12)})
	for _, line := range []struct {
		code, method string
		unit         int
		minor        int64
	}{{"OCEAN_FREIGHT", "CHARGEABLE", 1, 300_000}, {"OCEAN_FREIGHT", "CHARGEABLE", 2, 240_000}, {"DOC_FEE", "SHIPMENT", 0, 30_000}} {
		body := M{"charge_code": line.code, "description": "the line's charge", "method": line.method, "cost": M{"party_id": "line-1", "amount": jcny(line.minor)},
			"reason": "the line's charge", "occurred_at": f.hours(16)}
		if line.unit > 0 {
			body["load_unit_no"] = line.unit
		}
		f.post(fin, "/v1/fin/pools/P-1/costs", "GET /v1/fin/pools/P-1", body)
	}
	preview := f.post(fin, "/v1/fin/pools/P-1/previews", "GET /v1/fin/pools/P-1", M{"line_nos": []int{1, 2, 3}, "occurred_at": f.hours(20)})
	f.post(manager, fmt.Sprintf("/v1/fin/pools/P-1/previews/%v/submit", preview["version"]), "GET /v1/fin/pools/P-1", M{"occurred_at": f.hours(20)})
	bears := func(stage string, want map[string]int64) {
		t.Helper()
		for _, h := range houses {
			p := f.get(fin, "/v1/fin/jobs/JOB-"+h+"/profit")
			cost := int64(dig(p, "pool_cost", "minor").(float64))
			if cost != want[h] {
				t.Fatalf("%s, %s bears %d, want %d: %v", stage, h, cost, want[h], p)
			}
		}
	}
	bears("before the roll", map[string]int64{"H01": 106_000, "H02": 156_000, "H03": 56_000, "H04": 166_000, "H05": 86_000})

	// 7. The carrier rolls box B: BK-2 on EVER GLORY a week later; OM-2 is opened and box B rolled there; box A sails in
	// OM-1, box B in OM-2, each finalized with its own master bill
	booking("BK-2", "EVER GLORY", "0130W", f.at.Add(240*time.Hour), 23)
	f.post(ops, "/v1/ocean-movements", "", M{"movement_id": "OM-2", "consolidation_id": "C-1", "booking_id": "BK-2", "occurred_at": f.hours(23)})
	f.refused("the documentation staff rolls a container", docs, "/v1/container-visits/CV-2/roll", cv("CV-2"), M{"to_movement_id": "OM-2", "reason": "rolled", "occurred_at": f.hours(24)}, 403)
	f.post(ops, "/v1/container-visits/CV-2/roll", cv("CV-2"), M{"to_movement_id": "OM-2", "reason": "shut out at the terminal; rolled to EVER GLORY 0130W", "occurred_at": f.hours(24)})
	om2 := f.get(docs, "/v1/ocean-movements/OM-2")
	if hs, _ := om2["houses"].([]any); len(hs) != 2 {
		t.Fatalf("OM-2's houses: %v", om2["houses"])
	}
	load := func(visit string, hour int) {
		for i, kind := range []string{"GATE_IN_TERMINAL", "LOADED_ON_BOARD"} {
			f.post(ops, "/v1/container-visits/"+visit+"/events", cv(visit), M{"kind": kind, "location": "CNSHG", "happened_at": f.hours(hour - 1 + i), "source": "terminal",
				"reference": kind + "-" + visit, "occurred_at": f.hours(hour - 1 + i)})
		}
	}
	load("CV-1", 26)
	f.post(ops, "/v1/ocean-movements/OM-1/finalize-doc-facts", om("OM-1"), M{"occurred_at": f.hours(26)})
	bill("OM-2", "linu2612000777", 30)
	load("CV-2", 40)
	f.post(ops, "/v1/ocean-movements/OM-2/finalize-doc-facts", om("OM-2"), M{"occurred_at": f.hours(40)})
	if facts := f.get(docs, "/v1/ocean-movements/OM-2/doc-facts"); dig(facts, "facts", "vessel") != "EVER GLORY" {
		t.Fatalf("OM-2's facts: %v", facts)
	}

	// 8. Box B's storage fee of 300.00, a line of container 2: H04 200.00, H05 100.00; each house's profit
	f.post(fin, "/v1/fin/pools/P-1/costs", "GET /v1/fin/pools/P-1", M{"charge_code": "STORAGE", "description": "storage of box B", "method": "CHARGEABLE", "load_unit_no": 2,
		"cost": M{"party_id": "line-1", "amount": jcny(30_000)}, "reason": "storage of box B until EVER GLORY", "occurred_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)})
	preview = f.post(fin, "/v1/fin/pools/P-1/previews", "GET /v1/fin/pools/P-1", M{"line_nos": []int{4}, "occurred_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)})
	f.post(manager, fmt.Sprintf("/v1/fin/pools/P-1/previews/%v/submit", preview["version"]), "GET /v1/fin/pools/P-1", M{"occurred_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)})
	bears("after the roll", map[string]int64{"H01": 106_000, "H02": 156_000, "H03": 56_000, "H04": 186_000, "H05": 96_000})
	var revenue, cost int64
	for _, h := range houses {
		p := f.get(fin, "/v1/fin/jobs/JOB-"+h+"/profit")
		revenue += int64(dig(p, "revenue", "minor").(float64))
		cost += int64(dig(p, "pool_cost", "minor").(float64))
	}
	if revenue != 920_000 || cost != 600_000 || revenue-cost != 320_000 {
		t.Fatalf("revenue %d, cost %d, profit %d", revenue, cost, revenue-cost)
	}
}
