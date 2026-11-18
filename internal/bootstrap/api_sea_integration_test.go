//go:build integration

package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// dig walks decoded JSON: a string names a member, a number an element. It answers nil where the path ends early.
func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			s, _ := v.([]any)
			if k >= len(s) {
				return nil
			}
			v = s[k]
		}
	}
	return v
}

// SEA's routes end to end on PostgreSQL 16, through cargo-api's handler with HS256 tokens. ORG, RATE, COM, FUL, PHY
// and BOOK have no routes yet, so the test runs their steps in process, as the API client would (T6), and every SEA
// step over HTTP: the HBL series, the movement from the booking, the house, the container visit with its seal,
// stuffing, verified gross mass and terminal events, the master bill, the published facts and their second version
// after the loading, a carrier fact, a roll and a cancellation. It checks the permissions, tenant isolation,
// Idempotency-Key replays, If-Match revisions, that a movement's ETag covers what the movement resource states, and
// that the signatory's name is read only with its own permission and a stated purpose, and is logged.
func TestHTTPSeaOnPostgres(t *testing.T) {
	ctx := context.Background()
	platform := openPlatform(t)
	orgModule, err := platform.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := platform.RATE(orgModule, allowAll{}, marginFloor(2000))
	if err != nil {
		t.Fatal(err)
	}
	comModule, err := platform.COM(orgModule, rateModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	fulModule, phyModule, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	bookModule, err := platform.BOOK(orgModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := APIConfig{TokenKey: []byte("api-test-key-of-thirty-two-bytes!!"), TokenIssuer: "idp-cargo-test", TokenAudience: "idp-cargo-api",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MarginFloorBP: 2000}
	h, err := platform.API(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := apiClient{t: t, srv: srv}

	tenant, other := uniqueTenant("cargo-sea-api-a-"), uniqueTenant("cargo-sea-api-b-")
	seedORG(t, platform, tenant, "party-l", "line-1")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	ts := func(h int) string { return hours(h).Format(time.RFC3339Nano) }
	occurred := func(h int) string { return `"occurred_at":"` + ts(h) + `"` }
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}
	must := func(_ phyapi.Receipt, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	mint := func(tenant, actor string, perms ...string) string {
		t.Helper()
		token, err := MintDevToken(cfg, tenant, actor, perms, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	// the operator holds every SEA command and the reads, and not the signatory's name
	ops := mint(tenant, "ops-1", "sea:Read", "sea:OpenOceanMovement", "sea:LinkCarrierBooking", "sea:RecordMasterBill", "sea:RegisterHblSeries",
		"sea:AssociateHouseBill", "sea:RefreshHouseBillFacts", "sea:EndHouseBillAssociation", "sea:FinalizeOceanDocFacts", "sea:RecordOceanCarrierFact",
		"sea:CancelOceanMovement", "sea:AssignContainer", "sea:RecordContainerSeal", "sea:RecordStuffingDone", "sea:ReleaseContainer", "sea:AssertVgm",
		"sea:RecordVgmResponse", "sea:RecordContainerEvent")
	reader := mint(tenant, "cs-1", "sea:Read")
	nobody := mint(tenant, "pricing-1", "rate:*")
	auditor := mint(tenant, "auditor-1", "sea:ReadVgmSignatory")
	dpo := mint(tenant, "dpo-1", "sea:ReadVgmSignatoryAccess")
	stranger := mint(other, "ops-9", "sea:*")

	post := func(token, key, path, ifMatch, body string) apiResponse {
		t.Helper()
		header := []string{"Idempotency-Key", key}
		if ifMatch != "" {
			header = append(header, "If-Match", ifMatch)
		}
		return c.do("POST", path, token, body, header...)
	}
	// done is a command that succeeded: its status and the revision it wrote, as ETag
	done := func(name string, r apiResponse, status int, etag string) apiResponse {
		t.Helper()
		if r.status != status || r.header.Get("ETag") != etag {
			t.Fatalf("%s: %d %q %v, want %d %s", name, r.status, r.header.Get("ETag"), r.body, status, etag)
		}
		return r
	}
	get := func(name, path, token, etag string) apiResponse {
		t.Helper()
		r := c.do("GET", path, token, "")
		if r.status != 200 || r.header.Get("ETag") != etag {
			t.Fatalf("%s: %d %q %v, want 200 %q", name, r.status, r.header.Get("ETag"), r.body, etag)
		}
		return r
	}
	// raw reads a response's bytes as they were sent
	raw := func(path, token string) (int, string) {
		t.Helper()
		req, err := http.NewRequest("GET", srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}
	type publishedFacts struct {
		Version     int             `json:"version"`
		Status      string          `json:"status"`
		SHA256      string          `json:"sha256"`
		FinalizedBy string          `json:"finalized_by"`
		Facts       json.RawMessage `json:"facts"`
	}
	// published reads a version of OM-1's facts and checks that sha256 is the digest of the bytes sent as facts
	published := func(path string) publishedFacts {
		t.Helper()
		status, body := raw(path, reader)
		var out publishedFacts
		if err := json.Unmarshal([]byte(body), &out); status != 200 || err != nil {
			t.Fatalf("%s: %d %s %v", path, status, body, err)
		}
		if sum := sha256.Sum256(out.Facts); hex.EncodeToString(sum[:]) != out.SHA256 || out.FinalizedBy != "ops-1" {
			t.Fatalf("%s: sha256 %s is not the digest of the facts sent: %s", path, out.SHA256, body)
		}
		if strings.Contains(body, "Ada") || strings.Contains(body, "Lovelace") || strings.Contains(body, "signatory") {
			t.Fatalf("%s names the signatory: %s", path, body)
		}
		return out
	}

	// The other contexts, in process: house L planned and attached to C-1, C-2 next, the line's role and both bookings.
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	contexts.planSea(t, "party-l", "L", comapi.ContainerLCL, fulapi.ContainerLCL)
	for id, departure := range map[string]time.Duration{"C-1": 6 * time.Hour, "C-2": 30 * time.Hour} {
		must(phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol-" + id, TenantID: tenant, ActorID: "ops-1", ConsolidationID: id,
			Mode: phyapi.ModeSea, Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"),
			PlannedDeparture: departs.Add(departure), HouseCutOff: departs.Add(departure - 12*time.Hour), OccurredAt: hours(8)}))
		must(phyModule.PlanLoadUnit(ctx, phyapi.PlanLoadUnit{CommandID: "plan-unit-" + id, TenantID: tenant, ActorID: "ops-1", ConsolidationID: id,
			ExpectedRevision: 1, Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: hours(8)}))
	}
	must(phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-SH-L", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ShipmentID: "SH-L", OccurredAt: hours(9)}))
	record(t, ctx, fulModule, tenant, "ful-attach-SH-L", "SH-L", "C-1", 3, hours(9))
	if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-line-1", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "line-1", ExpectedRevision: 1, RoleCode: orgapi.RoleCarrier, ValidFrom: at.Add(-time.Hour), EvidenceRef: "service contract", OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	contexts.bookSea(t, bookModule, "BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", departs.Add(6*time.Hour))
	contexts.bookSea(t, bookModule, "BK-2", "C-2", "EVER GLORY", "", "0124W", departs.Add(30*time.Hour))

	// Who may ask.
	c.expect("no token", c.do("GET", "/v1/ocean-movements/OM-1", "", ""), 401, "UNAUTHENTICATED")
	c.expect("a read with another context's permissions", c.do("GET", "/v1/ocean-movements/OM-1", nobody, ""), 403, "FORBIDDEN")
	c.expect("no movement yet", c.do("GET", "/v1/ocean-movements/OM-1", reader, ""), 404, "NOT_FOUND")

	// The HBL series and the movement.
	series := `{"legal_entity_id":"LE","template":"SHAL{seq:7}","first_number":1,` + occurred(11) + `}`
	c.expect("a series without permission", post(reader, "series-0", "/v1/hbl-series", "", series), 403, "FORBIDDEN")
	if r := done("the series", post(ops, "series", "/v1/hbl-series", "", series), 201, `"1"`); r.header.Get("Location") != "/v1/hbl-series/LE" ||
		r.body["subject_id"] != "LE" {
		t.Fatalf("the series: %v %v", r.header, r.body)
	}
	open := `{"movement_id":"OM-1","consolidation_id":"C-1","booking_id":"BK-1",` + occurred(11) + `}`
	c.expect("no Idempotency-Key", c.do("POST", "/v1/ocean-movements", ops, open), 400, "IDEMPOTENCY_KEY_REQUIRED")
	c.expect("no business time", post(ops, "open-0", "/v1/ocean-movements", "", `{"movement_id":"OM-1","consolidation_id":"C-1","booking_id":"BK-1"}`),
		400, "OCCURRED_AT_REQUIRED")
	c.expect("a tenant named in the body", post(ops, "open-0", "/v1/ocean-movements", "", strings.Replace(open, `{`, `{"tenant_id":"`+other+`",`, 1)),
		400, "MALFORMED_REQUEST")
	c.expect("opening without permission", post(reader, "open-0", "/v1/ocean-movements", "", open), 403, "FORBIDDEN")
	c.expect("a booking allocated to another consolidation", post(ops, "open-x", "/v1/ocean-movements", "",
		`{"movement_id":"OM-X","consolidation_id":"C-2","booking_id":"BK-1",`+occurred(11)+`}`), 422, "")
	created := done("opened", post(ops, "open-1", "/v1/ocean-movements", "", open), 201, `"1"`)
	if created.header.Get("Location") != "/v1/ocean-movements/OM-1" || created.body["command_id"] != "open-1" || created.body["subject_id"] != "OM-1" ||
		created.body["new_revision"] != 1.0 {
		t.Fatalf("opened: %v %v", created.header, created.body)
	}
	if again := done("replayed", post(ops, "open-1", "/v1/ocean-movements", "", open), 201, `"1"`); again.body["subject_id"] != "OM-1" {
		t.Fatalf("replayed: %v", again.body)
	}
	c.expect("the key reused for another body", post(ops, "open-1", "/v1/ocean-movements", "", strings.Replace(open, occurred(11), occurred(12), 1)),
		409, "IDEMPOTENCY_MISMATCH")

	// The house: its HBL drawn from the series.
	house := `{"shipment_id":"SH-L",` + occurred(11) + `}`
	c.expect("a change without If-Match", post(ops, "hbl-0", "/v1/ocean-movements/OM-1/house-bills", "", house), 428, "PRECONDITION_REQUIRED")
	c.expect("a stale revision", post(ops, "hbl-0", "/v1/ocean-movements/OM-1/house-bills", `"9"`, house), 409, "VERSION_CONFLICT")
	if r := done("the house", post(ops, "hbl-1", "/v1/ocean-movements/OM-1/house-bills", `"1"`, house), 200, `"2"`); r.body["hbl"] != "SHAL0000001" ||
		r.body["entry_no"] != 1.0 || r.body["subject_id"] != "OM-1" {
		t.Fatalf("the house: %v", r.body)
	}

	// PHY receives, stuffs and seals house L; FUL takes the evidence; SEA's refreshed entry keeps the sealed container.
	consolidation, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	if err != nil {
		t.Fatal(err)
	}
	must(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: consolidation.Revision, OccurredAt: hours(13)}))
	cfs := phyapi.Facility{Location: consolidation.Current.Origin, Code: "SHA-CFS1"}
	must(phyModule.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv", TenantID: tenant, ActorID: "cfs-1", LotID: "L-L", ShipmentID: "SH-L",
		Facility: cfs, Packs: 6, PackageType: "CT", Condition: phyapi.ConditionGood, Evidence: evidence("CFS-WMS", "GRN-L", 14), OccurredAt: hours(14)}))
	must(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh", TenantID: tenant, ActorID: "cfs-1", LotID: "L-L",
		ExpectedRevision: 1, GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 312}, Method: phyapi.MethodScale, Evidence: evidence("CFS-SCALE", "W-L", 14),
		OccurredAt: hours(14)}))
	must(phyModule.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete", TenantID: tenant, ActorID: "cfs-1", LotID: "L-L",
		ExpectedRevision: 2, OccurredAt: hours(14)}))
	must(phyModule.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "stuff", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
		ConsolidationID: "C-1", Kind: phyapi.ExecutionStuffing, Facility: cfs, LoadUnitNo: 1, OccurredAt: hours(15)}))
	must(phyModule.StartLoading(ctx, phyapi.StartLoading{CommandID: "start", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1", ExpectedRevision: 1,
		OccurredAt: hours(15)}))
	must(phyModule.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
		ExpectedRevision: 2, LotID: "L-L", Packs: 6, Evidence: evidence("CFS-TALLY", "T-L", 15), OccurredAt: hours(15)}))
	must(phyModule.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "seal", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
		ExpectedRevision: 3, Evidence: evidence(phyapi.EvidenceSeal, "CN-SEAL-77120", 15), OccurredAt: hours(15)}))
	if _, err = fulModule.RecordVerifiedLoadEvidence(ctx, fulapi.RecordVerifiedLoadEvidence{CommandID: "ful-stuffed", TenantID: tenant, ActorID: "ops-1",
		ShipmentID: "SH-L", ExpectedRevision: 4, OccurredAt: hours(16)}); err != nil {
		t.Fatal(err)
	}
	now := `{` + occurred(16) + `}`
	if r := done("refreshed", post(ops, "refresh-1", "/v1/ocean-movements/OM-1/house-bills/SH-L/refresh", `"2"`, now), 200, `"3"`); r.body["entry_no"] != 2.0 ||
		r.body["hbl"] != "SHAL0000001" {
		t.Fatalf("refreshed: %v", r.body)
	}
	c.expect("ending a house FUL still attaches", post(ops, "end-0", "/v1/ocean-movements/OM-1/house-bills/SH-L/end", `"3"`, now), 422, "")

	// The container visit: a first container is released unused; the second is sealed and stuffed.
	assign := func(visit, number string) string {
		return `{"visit_id":"` + visit + `","movement_id":"OM-1","load_unit_no":1,"container_number":"` + number +
			`","size_type":"22G1","tare_kg":2180,"max_gross_kg":30480,"plate_source":"CSC_PLATE",` + occurred(12) + `}`
	}
	c.expect("a container number with a wrong check digit", post(ops, "assign-x", "/v1/container-visits", "", assign("CV-X", "CSQU3054384")), 422, "")
	if r := done("the first container", post(ops, "assign-0", "/v1/container-visits", "", assign("CV-0", "MSCU1234566")), 201, `"1"`); r.header.Get("Location") !=
		"/v1/container-visits/CV-0" {
		t.Fatalf("the first container: %v", r.header)
	}
	done("released", post(ops, "release-0", "/v1/container-visits/CV-0/release", `"1"`, `{"reason":"the line swapped the container",`+occurred(12)+`}`), 200, `"2"`)
	if r := done("the container", post(ops, "assign-1", "/v1/container-visits", "", assign("CV-1", "csqu3054383")), 201, `"1"`); r.header.Get("Location") !=
		"/v1/container-visits/CV-1" || r.body["subject_id"] != "CV-1" || r.body["new_revision"] != 1.0 {
		t.Fatalf("the container: %v %v", r.header, r.body)
	}
	c.expect("stuffing done without a seal", post(ops, "stuffed-0", "/v1/container-visits/CV-1/stuffing-done", `"1"`, now), 422, "")
	if r := done("the seal", post(ops, "seal-1", "/v1/container-visits/CV-1/seals", `"1"`, `{"number":"cn-seal-77120","party":"SHIPPER","applied_at":"`+
		ts(15)+`","source":"CFS tally T-L",`+occurred(15)+`}`), 200, `"2"`); r.body["seal_no"] != 1.0 || r.body["subject_id"] != "CV-1" {
		t.Fatalf("the seal: %v", r.body)
	}
	done("stuffing done", post(ops, "stuffed-1", "/v1/container-visits/CV-1/stuffing-done", `"2"`, now), 200, `"3"`)
	if v := get("the visit", "/v1/container-visits/CV-1", reader, `"3"`); v.body["status"] != "STUFFED" || v.body["container_number"] != "CSQU3054383" ||
		v.body["seal_number"] != "CN-SEAL-77120" || v.body["execution_id"] != "X-1" || v.body["movement_id"] != "OM-1" ||
		dig(v.body, "seals", 0, "party") != "SHIPPER" {
		t.Fatalf("the visit: %v", v.body)
	}
	if v := get("by its number", "/v1/container-visits?container_number=csqu3054383", reader, `"3"`); v.body["visit_id"] != "CV-1" {
		t.Fatalf("by its number: %v", v.body)
	}
	c.expect("a released container has no live visit", c.do("GET", "/v1/container-visits?container_number=MSCU1234566", reader, ""), 404, "NOT_FOUND")
	c.expect("visits without a question", c.do("GET", "/v1/container-visits", reader, ""), 400, "MALFORMED_REQUEST")
	if l := c.do("GET", "/v1/ocean-movements/OM-1/container-visits", reader, ""); l.status != 200 || l.header.Get("ETag") != "" ||
		dig(l.body, "container_visits", 0, "status") != "RELEASED" || dig(l.body, "container_visits", 1, "visit_id") != "CV-1" ||
		dig(l.body, "container_visits", 2) != nil {
		t.Fatalf("the movement's visits: %d %v", l.status, l.body)
	}

	// The movement resource states what its revision covers: the visits moved, its revision and ETag did not, and it
	// names neither the visits nor a container number.
	m := get("the movement", "/v1/ocean-movements/OM-1", reader, `"3"`)
	_, hasContainers := m.body["containers"]
	_, hasFacts := m.body["carrier_facts"]
	stuffed, _ := dig(m.body, "houses", 0, "containers", 0).(map[string]any)
	_, hasNumber := stuffed["container_number"]
	if m.body["state"] != "OPEN" || m.body["revision"] != 3.0 || m.body["kind"] != "SEA_LCL_OWN" || m.body["carrier_party_id"] != "line-1" ||
		m.body["origin"] != "CNSHG" || m.body["destination"] != "DEHAM" || dig(m.body, "legs", 0, "vessel") != "EVER GIVEN" ||
		dig(m.body, "legs", 0, "cut_offs", 1) == nil || dig(m.body, "houses", 0, "hbl") != "SHAL0000001" || dig(m.body, "houses", 0, "basis") != "TENDERED" ||
		dig(m.body, "houses", 0, "containers", 0, "execution_id") != "X-1" || dig(m.body, "houses", 0, "containers", 0, "seal") != "CN-SEAL-77120" ||
		dig(m.body, "gross_weight", "unit") != "GRM" || dig(m.body, "gross_weight", "amount") != 312000.0 || hasContainers || hasFacts || hasNumber {
		t.Fatalf("the movement: %v", m.body)
	}

	// The master bill reference.
	bill := `{"number":"linu2611000457","kind":"CARRIER_MBL","evidence":{"sha256":"` + strings.Repeat("9c", 32) +
		`","storage_id":"mail/draft-mbl.pdf","media_type":"application/pdf","captured_at":"` + ts(17) + `"},` + occurred(17) + `}`
	if r := done("the master bill", post(ops, "mbl-1", "/v1/ocean-movements/OM-1/master-bills", `"3"`, bill), 200, `"4"`); r.body["bill_no"] != 1.0 {
		t.Fatalf("the master bill: %v", r.body)
	}
	if r := get("by the master bill", "/v1/ocean-movements?issuer_party_id=line-1&master_bill_number=LINU2611000457", reader, `"4"`); r.body["movement_id"] != "OM-1" ||
		dig(r.body, "master_bills", 0, "evidence", "storage_id") != "mail/draft-mbl.pdf" {
		t.Fatalf("by the master bill: %v", r.body)
	}
	c.expect("a number no movement holds", c.do("GET", "/v1/ocean-movements?issuer_party_id=line-1&master_bill_number=LINU0000000000", reader, ""), 404, "NOT_FOUND")
	c.expect("movements without an issuer", c.do("GET", "/v1/ocean-movements?master_bill_number=LINU2611000457", reader, ""), 400, "MALFORMED_REQUEST")

	// The verified gross mass: the name goes in, and comes back in no receipt and no visit.
	vgm := `{"mass":{"unit":"KGM","amount":2500},"method":"METHOD_1","responsible_party_id":"LE","responsible_party_revision":1,"determined_at":"` + ts(17) +
		`","signatory":"Ada Lovelace",` + occurred(17) + `}`
	if r := done("the VGM", post(ops, "vgm-1", "/v1/container-visits/CV-1/vgm-assertions", `"3"`, vgm), 200, `"4"`); r.body["assertion_no"] != 1.0 ||
		len(r.body) != 4 {
		t.Fatalf("the VGM: %v", r.body)
	}
	if status, body := raw("/v1/container-visits/CV-1", reader); status != 200 || !strings.Contains(body, `"mass_grams":2500000`) ||
		strings.Contains(body, "Ada") || strings.Contains(body, "Lovelace") || strings.Contains(body, "signatory") {
		t.Fatalf("the visit with its VGM: %d %s", status, body)
	}

	// The facts are published; the carrier's acceptance and the gate-in leave them FINAL.
	if r := done("finalized", post(ops, "fin-1", "/v1/ocean-movements/OM-1/finalize-doc-facts", `"4"`, `{`+occurred(18)+`}`), 200, `"5"`); r.body["facts_version"] != 1.0 {
		t.Fatalf("finalized: %v", r.body)
	}
	first := published("/v1/ocean-movements/OM-1/doc-facts")
	if first.Version != 1 || first.Status != "FINAL" || !strings.Contains(string(first.Facts), `"master_bill_number":"LINU2611000457"`) ||
		!strings.Contains(string(first.Facts), `"vgm_assertion_id":"CV-1:1"`) || strings.Contains(string(first.Facts), "on_board_at") {
		t.Fatalf("the first version: %+v %s", first, first.Facts)
	}
	if r := done("the carrier's answer", post(ops, "vgm-answer", "/v1/container-visits/CV-1/vgm-response", `"4"`,
		`{"response":"ACCEPTED","source":"carrier portal",`+occurred(18)+`}`), 200, `"5"`); r.body["assertion_no"] != 1.0 {
		t.Fatalf("the carrier's answer: %v", r.body)
	}
	event := func(kind, location, reference string, h int) string {
		return `{"kind":"` + kind + `","location":"` + location + `","happened_at":"` + ts(h) + `","source":"terminal","reference":"` + reference + `",` +
			occurred(h) + `}`
	}
	c.expect("a place that is no UN/LOCODE", post(ops, "gate-0", "/v1/container-visits/CV-1/events", `"5"`, event("GATE_IN_TERMINAL", "Shanghai", "GIN-0", 19)), 422, "")
	if r := done("gated in", post(ops, "gate-1", "/v1/container-visits/CV-1/events", `"5"`, event("GATE_IN_TERMINAL", "CNSHG", "GIN-1", 19)), 200, `"6"`); r.body["event_no"] != 1.0 {
		t.Fatalf("gated in: %v", r.body)
	}
	if m = get("still final", "/v1/ocean-movements/OM-1", reader, `"5"`); m.body["state"] != "FINAL" || m.body["doc_facts_version"] != 1.0 {
		t.Fatalf("still final: %v", m.body)
	}

	// The loading states an on-board date the published facts do not: the movement returns to OPEN, and its ETag moves
	// although the command was the visit's.
	if r := done("loaded", post(ops, "load-1", "/v1/container-visits/CV-1/events", `"6"`, event("LOADED_ON_BOARD", "CNSHG", "LD-1", 20)), 200, `"7"`); r.body["event_no"] != 2.0 {
		t.Fatalf("loaded: %v", r.body)
	}
	if m = get("reopened", "/v1/ocean-movements/OM-1", reader, `"6"`); m.body["state"] != "OPEN" {
		t.Fatalf("reopened: %v", m.body)
	}
	if again := published("/v1/ocean-movements/OM-1/doc-facts"); again.Version != 1 || again.Status != "SUPERSEDED" || again.SHA256 != first.SHA256 {
		t.Fatalf("the first version once superseded: %+v", again)
	}

	// A carrier fact that changes nothing published: it is recorded, and the movement's revision stays.
	fact := `{"kind":"DEPARTED","bill_number":"LINU2611000457","vessel":"EVER GIVEN","voyage":"0123W","source":"line","reference":"DEP-1","sha256":"` +
		strings.Repeat("cd", 32) + `","happened_at":"` + ts(21) + `",` + occurred(21) + `}`
	c.expect("a fact that names a container and a bill", post(ops, "fact-0", "/v1/ocean-movements/OM-1/carrier-facts", `"6"`,
		strings.Replace(fact, `"bill_number"`, `"container_number":"CSQU3054383","bill_number"`, 1)), 422, "")
	if r := done("the carrier fact", post(ops, "fact-1", "/v1/ocean-movements/OM-1/carrier-facts", `"6"`, fact), 200, `"6"`); r.body["fact_no"] != 1.0 ||
		r.body["new_revision"] != 6.0 {
		t.Fatalf("the carrier fact: %v", r.body)
	}
	if l := c.do("GET", "/v1/ocean-movements/OM-1/carrier-facts", reader, ""); l.status != 200 || l.header.Get("ETag") != "" ||
		dig(l.body, "carrier_facts", 0, "kind") != "DEPARTED" || dig(l.body, "carrier_facts", 0, "bill_number") != "LINU2611000457" ||
		dig(l.body, "carrier_facts", 0, "recorded_by") != "ops-1" || dig(l.body, "carrier_facts", 1) != nil {
		t.Fatalf("the carrier facts: %d %v", l.status, l.body)
	}
	if m = get("after the fact", "/v1/ocean-movements/OM-1", reader, `"6"`); m.body["carrier_facts"] != nil {
		t.Fatalf("after the fact: %v", m.body)
	}

	// The second version states the on-board date; the first stays as it was published.
	if r := done("finalized again", post(ops, "fin-2", "/v1/ocean-movements/OM-1/finalize-doc-facts", `"6"`, `{`+occurred(22)+`}`), 200, `"7"`); r.body["facts_version"] != 2.0 {
		t.Fatalf("finalized again: %v", r.body)
	}
	second := published("/v1/ocean-movements/OM-1/doc-facts")
	var stated struct {
		OnBoardAt time.Time `json:"on_board_at"`
	}
	if err = json.Unmarshal(second.Facts, &stated); err != nil || second.Version != 2 || second.Status != "FINAL" || !stated.OnBoardAt.Equal(hours(20)) {
		t.Fatalf("the second version: %+v %s %v", second, second.Facts, err)
	}
	if old := published("/v1/ocean-movements/OM-1/doc-facts/1"); old.Status != "SUPERSEDED" || old.SHA256 != first.SHA256 || string(old.Facts) != string(first.Facts) ||
		old.SHA256 == second.SHA256 {
		t.Fatalf("the first version: %+v", old)
	}
	c.expect("a version not published", c.do("GET", "/v1/ocean-movements/OM-1/doc-facts/3", reader, ""), 404, "NOT_FOUND")
	c.expect("version 0", c.do("GET", "/v1/ocean-movements/OM-1/doc-facts/0", reader, ""), 400, "MALFORMED_REQUEST")
	c.expect("facts without sea:Read", c.do("GET", "/v1/ocean-movements/OM-1/doc-facts", auditor, ""), 403, "FORBIDDEN")

	// The signatory's name: only with its own permission and one of the three purposes, and every read is logged.
	reads := "/v1/container-visits/CV-1/vgm-assertions/1/signatory-reads"
	audit := `{"purpose":"CUSTOMS_AUDIT"}`
	c.expect("the operator reads no signatory", c.do("POST", reads, ops, audit), 403, "FORBIDDEN")
	c.expect("nor does sea:Read", c.do("POST", reads, reader, audit), 403, "FORBIDDEN")
	c.expect("nor the reader of the log", c.do("POST", reads, dpo, audit), 403, "FORBIDDEN")
	c.expect("a purpose that is none of the three", c.do("POST", reads, auditor, `{"purpose":"MARKETING"}`), 422, "")
	c.expect("no purpose", c.do("POST", reads, auditor, `{}`), 422, "")
	c.expect("an assertion that does not exist", c.do("POST", "/v1/container-visits/CV-1/vgm-assertions/9/signatory-reads", auditor, audit), 404, "NOT_FOUND")
	c.expect("another tenant, whatever it holds", c.do("POST", reads, stranger, audit), 404, "NOT_FOUND")
	if r := c.do("POST", reads, auditor, audit); r.status != 200 || r.body["name"] != "Ada Lovelace" || len(r.body) != 1 {
		t.Fatalf("the signatory: %d %v", r.status, r.body)
	}
	c.expect("the log without its permission", c.do("GET", reads, auditor, ""), 403, "FORBIDDEN")
	c.expect("the log with sea:Read", c.do("GET", reads, reader, ""), 403, "FORBIDDEN")
	status, logged := raw(reads, dpo)
	var log struct {
		Reads []struct {
			ActorID string `json:"actor_id"`
			Purpose string `json:"purpose"`
		} `json:"signatory_reads"`
	}
	if err = json.Unmarshal([]byte(logged), &log); status != 200 || err != nil || len(log.Reads) != 1 || log.Reads[0].ActorID != "auditor-1" ||
		log.Reads[0].Purpose != "CUSTOMS_AUDIT" || strings.Contains(logged, "Ada") || strings.Contains(logged, "Lovelace") {
		t.Fatalf("the log of reads: %d %s %v", status, logged, err)
	}

	// Another tenant sees nothing of this one, with every SEA permission.
	c.expect("another tenant's movement", c.do("GET", "/v1/ocean-movements/OM-1", stranger, ""), 404, "NOT_FOUND")
	c.expect("another tenant's visit", c.do("GET", "/v1/container-visits/CV-1", stranger, ""), 404, "NOT_FOUND")
	c.expect("another tenant's facts", c.do("GET", "/v1/ocean-movements/OM-1/doc-facts", stranger, ""), 404, "NOT_FOUND")
	c.expect("another tenant's house bill", c.do("GET", "/v1/house-bills/LE/SHAL0000001", stranger, ""), 404, "NOT_FOUND")
	c.expect("another tenant's command", post(stranger, "mbl-x", "/v1/ocean-movements/OM-1/master-bills", `"7"`, bill), 404, "NOT_FOUND")
	if l := c.do("GET", "/v1/ocean-movements/OM-1/container-visits", stranger, ""); l.status == 200 && dig(l.body, "container_visits", 0) != nil {
		t.Fatalf("another tenant's visits: %v", l.body)
	}

	// The series and the HBL register.
	if s := c.do("GET", "/v1/hbl-series/LE", reader, ""); s.status != 200 || s.body["template"] != "SHAL{seq:7}" || s.body["first_number"] != 1.0 ||
		dig(s.body, "last", 0, "last") != 1.0 || s.header.Get("ETag") != "" {
		t.Fatalf("the series: %d %v", s.status, s.body)
	}
	c.expect("a legal entity without a series", c.do("GET", "/v1/hbl-series/LE-9", reader, ""), 404, "NOT_FOUND")
	if ref := c.do("GET", "/v1/house-bills/LE/SHAL0000001", reader, ""); ref.status != 200 || ref.body["shipment_id"] != "SH-L" ||
		ref.body["movement_id"] != "OM-1" || ref.body["entry_no"] != 2.0 {
		t.Fatalf("the HBL: %d %v", ref.status, ref.body)
	}
	c.expect("an HBL not drawn", c.do("GET", "/v1/house-bills/LE/SHAL0000009", reader, ""), 404, "NOT_FOUND")

	// The next consolidation's movement: the line rolls it to another vessel, then withdraws the sailing.
	done("the next movement", post(ops, "open-2", "/v1/ocean-movements", "", `{"movement_id":"OM-2","consolidation_id":"C-2","booking_id":"BK-2",`+occurred(12)+`}`),
		201, `"1"`)
	c.expect("no newer capacity to link", post(ops, "link-0", "/v1/ocean-movements/OM-2/link-carrier-booking", `"1"`, `{`+occurred(13)+`}`), 422, "")
	rolled := departs.Add(30*time.Hour + 7*24*time.Hour)
	if _, err = bookModule.RecordCarrierChange(ctx, bookapi.RecordCarrierChange{CommandID: "roll", TenantID: tenant, ActorID: "ops-1", BookingID: "BK-2",
		ExpectedRevision: 3, Reason: bookapi.CapacityRollover, Answer: bookapi.Answer{Channel: bookapi.ChannelEmail,
			Evidence: bookapi.Evidence{SHA256: strings.Repeat("6b", 32), StorageID: "mail/roll.eml", MediaType: "message/rfc822", CapturedAt: hours(18)}},
		Lines: []bookapi.CapacityLine{{Capacity: bookapi.Capacity{Equipment: []bookapi.Equipment{{SizeType: "22G1", Count: 1}}},
			Legs: []bookapi.ConfirmedLeg{{Loading: unlocode("CNSHA"), Discharge: unlocode("DEHAM"), Vessel: "EVER GOLDEN", Voyage: "0125W", ETD: rolled,
				ETA: rolled.Add(30 * 24 * time.Hour)}}}}, OccurredAt: hours(18)}); err != nil {
		t.Fatal(err)
	}
	done("relinked", post(ops, "link-1", "/v1/ocean-movements/OM-2/link-carrier-booking", `"1"`, `{`+occurred(18)+`}`), 200, `"2"`)
	if next := get("the rolled movement", "/v1/ocean-movements/OM-2", reader, `"2"`); next.body["capacity_version"] != 2.0 ||
		dig(next.body, "legs", 0, "vessel") != "EVER GOLDEN" || dig(next.body, "legs", 0, "current") != true ||
		dig(next.body, "legs", 1, "vessel") != "EVER GLORY" || dig(next.body, "legs", 1, "current") != false || dig(next.body, "houses", 0) != nil {
		t.Fatalf("the rolled movement: %v", next.body)
	}
	withdrawn := `{"reason":"the line withdrew the sailing",` + occurred(19) + `}`
	c.expect("a movement with a house is not cancelled", post(ops, "cancel-1", "/v1/ocean-movements/OM-1/cancel", `"7"`, withdrawn), 422, "")
	c.expect("cancelling without permission", post(reader, "cancel-0", "/v1/ocean-movements/OM-2/cancel", `"2"`, withdrawn), 403, "FORBIDDEN")
	done("cancelled", post(ops, "cancel-2", "/v1/ocean-movements/OM-2/cancel", `"2"`, withdrawn), 200, `"3"`)
	if next := get("the cancelled movement", "/v1/ocean-movements/OM-2", reader, `"3"`); next.body["state"] != "CANCELLED" {
		t.Fatalf("the cancelled movement: %v", next.body)
	}
}
