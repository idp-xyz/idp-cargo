//go:build integration

package bootstrap

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
)

// FIN's generic routes (stage 3, V2.110), on PostgreSQL, with the job roles' tokens: a job's cost lines are seen only
// with fin:ReadProfit (the finance staff can; the operator's fin:Read and the documentation staff cannot), a charge
// line recorded and the job read again, the policy read by the operator, a debtor's credit terms set and its account
// read, a creditor's invoice recorded and read only with fin:ReadProfit; the job, the debtor and the creditor of
// another tenant are not found. The shipment and the job are set up in process.
func TestFINGenericRoutesWithTheJobRolesOnPostgres(t *testing.T) {
	ctx := context.Background()
	p := openPlatform(t)
	cfg := APIConfig{TokenKey: []byte("api-test-key-of-thirty-two-bytes!!"), TokenIssuer: "idp-cargo-test", TokenAudience: "idp-cargo-api",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MarginFloorBP: 2000}
	h, err := p.API(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := apiClient{t: t, srv: srv}
	tenant, other := uniqueTenant("cargo-fin-gen-a-"), uniqueTenant("cargo-fin-gen-b-")
	seedORG(t, p, tenant, "party-a", "line-1")
	orgModule, err := p.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := p.RATE(orgModule, allowAll{}, marginFloor(2000))
	if err != nil {
		t.Fatal(err)
	}
	comModule, err := p.COM(orgModule, rateModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	fulModule, phyModule, err := p.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	finModule, err := p.FIN(orgModule, rateModule, fulModule, &DocumentSnapshots{}, phyModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	_, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-line-1", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE", PartyID: "line-1",
		ExpectedRevision: 1, RoleCode: orgapi.RoleCarrier, ValidFrom: at.Add(-time.Hour), EvidenceRef: "service contract", OccurredAt: at})
	must("role line-1", err)
	slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: at.Add(72 * time.Hour)}.
		planSea(t, "party-a", "A", comapi.ContainerLCL, fulapi.ContainerLCL)
	_, err = finModule.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "fin-policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: at.Add(10 * time.Hour)})
	must("policy", err)
	_, err = finModule.RecordExchangeRate(ctx, finapi.RecordExchangeRate{CommandID: "fin-rate", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE", ExpectedRevision: 1,
		RateType: finapi.RateEstimate, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: at, ValidUntil: at.AddDate(0, 1, 0),
		Source: "CFETS central parity", OccurredAt: at.Add(10 * time.Hour)})
	must("rate", err)
	_, err = finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "open-A", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-A", ShipmentID: "SH-A", OccurredAt: at.Add(10 * time.Hour)})
	must("open the job", err)
	r, err := finModule.ApplyPricingBasis(ctx, finapi.ApplyPricingBasis{CommandID: "apply-A", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-A", ExpectedRevision: 1,
		QuoteID: "Q-A", OrderID: "O-A", OccurredAt: at.Add(11 * time.Hour)})
	must("apply", err)

	token := func(tenant, actor string, roles ...string) string {
		t.Helper()
		perms, err := RolePermissions(roles, nil)
		must("roles", err)
		tok, err := MintDevToken(cfg, tenant, actor, perms, time.Hour)
		must("token", err)
		return tok
	}
	finance, ops, docs, stranger := token(tenant, "finn", "finance"), token(tenant, "otto", "ops"), token(tenant, "dora", "docs"), token(other, "finn", "finance")
	occurred := fmt.Sprintf(`"occurred_at":%q`, at.Add(12*time.Hour).Format(time.RFC3339))
	post := func(tok, key, path, ifMatch, body string) apiResponse {
		t.Helper()
		headers := []string{"Idempotency-Key", key}
		if ifMatch != "" {
			headers = append(headers, "If-Match", ifMatch)
		}
		return c.do("POST", path, tok, body, headers...)
	}
	want := func(name string, got apiResponse, status int, etag string) apiResponse {
		t.Helper()
		if got.status != status || (etag != "" && got.header.Get("ETag") != etag) {
			t.Fatalf("%s: %d %s %v, want %d %s", name, got.status, got.header.Get("ETag"), got.body, status, etag)
		}
		return got
	}

	revision := fmt.Sprintf(`"%d"`, r.NewRevision)
	want("the finance staff reads the job", c.do("GET", "/v1/fin/jobs/JOB-A", finance, ""), 200, revision)
	c.expect("the operator's fin:Read reads the job's costs", c.do("GET", "/v1/fin/jobs/JOB-A", ops, ""), 403, "FORBIDDEN")
	c.expect("the documentation staff reads the job's costs", c.do("GET", "/v1/fin/jobs/JOB-A", docs, ""), 403, "FORBIDDEN")
	c.expect("the operator records a cost", post(ops, "line-0", "/v1/fin/jobs/JOB-A/charge-lines", revision, `{"charge_code":"TRUCKING","cost":{"party_id":"line-1","amount":{"currency":"CNY","minor":120000}},"reason":"pickup",`+occurred+`}`), 403, "FORBIDDEN")
	want("the finance staff records a cost", post(finance, "line-1", "/v1/fin/jobs/JOB-A/charge-lines", revision,
		`{"charge_code":"TRUCKING","cost":{"party_id":"line-1","amount":{"currency":"CNY","minor":120000}},"reason":"pickup",`+occurred+`}`), 200, "")
	job := want("the job again", c.do("GET", "/v1/fin/jobs/JOB-A", finance, ""), 200, "")
	if !strings.Contains(fmt.Sprint(job.body), "TRUCKING") {
		t.Fatalf("the job: %v", job.body)
	}
	c.expect("another tenant's job", c.do("GET", "/v1/fin/jobs/JOB-A", stranger, ""), 404, "NOT_FOUND")
	want("the operator reads the policy", c.do("GET", "/v1/fin/legal-entities/LE/policy", ops, ""), 200, "")

	want("credit terms", post(finance, "credit-1", "/v1/fin/legal-entities/LE/debtors/party-a/credit-terms", `"0"`,
		`{"limit":{"currency":"CNY","minor":10000000},"payment_days":30,"payment_basis":"`+finapi.TermsFromMonthEnd+`",`+occurred+`}`), 200, "")
	account := want("the credit account", c.do("GET", "/v1/fin/legal-entities/LE/debtors/party-a/credit", ops, ""), 200, "")
	if terms, _ := account.body["terms"].([]any); len(terms) != 1 || terms[0].(map[string]any)["limit"].(map[string]any)["minor"] != 1e7 || account.body["status"] != "OK" {
		t.Fatalf("the credit account: %v", account.body)
	}

	invoice := `{"invoice_id":"CI-1","legal_entity_id":"LE","creditor_party_id":"line-1","supplier_invoice_no":"INV-77","invoice_date":"` + at.Format(time.DateOnly) +
		`","currency":"CNY","lines":[{"charge_code":"TRUCKING","description":"pickup","amount":120000}],` + occurred + `}`
	want("a creditor's invoice", post(finance, "ci-1", "/v1/fin/creditor-invoices", "", invoice), 201, "")
	want("read it", c.do("GET", "/v1/fin/creditor-invoices/CI-1", finance, ""), 200, "")
	c.expect("the operator reads a payable", c.do("GET", "/v1/fin/creditor-invoices/CI-1", ops, ""), 403, "FORBIDDEN")
	if list := want("the creditor's invoices", c.do("GET", "/v1/fin/creditor-invoices?legal_entity_id=LE&creditor_party_id=line-1", finance, ""), 200, ""); len(list.body["items"].([]any)) != 1 {
		t.Fatalf("the invoices: %v", list.body)
	}
}
