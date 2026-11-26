// fin-mcp-check is the end-to-end check of cargo-mcp for tools/demo/fin-mcp.sh: it calls the running server over HTTP
// the way idp-mcp's gateway does, with assertions signed by the local gateway key of the script (a key pair made for
// local testing only), and prints what the server answered. It prints no key and no assertion.
// The steps: tools/list without an assertion; each of the three tools for a member of the role finance-analyst; the
// same call without an assertion (403), with an assertion it was given before (403), and for a role the configuration
// does not name (refused).
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.idp.xyz/idp-cargo/internal/transport/mcp"
	"go.idp.xyz/idp-cargo/internal/transport/mcp/mcptest"
)

type check struct {
	endpoint string
	signer   *mcptest.Signer
	id       int
	failed   int
}

type answer struct {
	status int
	rpc    map[string]any
	isErr  bool
	text   string
	data   map[string]any
}

func (c *check) post(method, name string, body []byte) answer {
	req, err := http.NewRequest(http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	req.Header = mcptest.Headers(method, name)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fin-mcp-check:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	a := answer{status: resp.StatusCode}
	_ = json.Unmarshal(raw, &a.rpc)
	if res, ok := a.rpc["result"].(map[string]any); ok {
		a.isErr, _ = res["isError"].(bool)
		if content, _ := res["content"].([]any); len(content) > 0 {
			a.text, _ = content[0].(map[string]any)["text"].(string)
		}
		a.data, _ = res["structuredContent"].(map[string]any)
	}
	if e, ok := a.rpc["error"].(map[string]any); ok {
		a.text, _ = e["message"].(string)
	}
	return a
}

func (c *check) call(caller mcp.Caller, tool string, args map[string]any) ([]byte, answer) {
	c.id++
	body := c.signer.Call(caller, c.id, "tools/call", map[string]any{"name": tool, "arguments": args})
	return body, c.post("tools/call", tool, body)
}

func (c *check) expect(step string, ok bool, a answer) {
	verdict := "ok  "
	if !ok {
		verdict, c.failed = "FAIL", c.failed+1
	}
	text := a.text
	if len(text) > 160 {
		text = text[:160] + "..."
	}
	fmt.Printf("%s %-62s HTTP %d  isError=%-5v %s\n", verdict, step, a.status, a.isErr, text)
}

func main() {
	endpoint := flag.String("endpoint", "http://127.0.0.1:8600/mcp", "the server's endpoint")
	keyFile := flag.String("key", "", "the local gateway's private key (PKCS #8 PEM)")
	gateway := flag.String("gateway-id", "local", "the gateway id the server accepts")
	tenant := flag.String("tenant", "eii-test", "the gateway tenant")
	role := flag.String("role", "finance-analyst", "the role the configuration grants fin:ReadProfit")
	entity := flag.String("legal-entity", "LE", "the legal entity the role sees")
	flag.Parse()
	pemBytes, err := os.ReadFile(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fin-mcp-check:", err)
		os.Exit(1)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		fmt.Fprintln(os.Stderr, "fin-mcp-check: the key file holds no PEM block")
		os.Exit(1)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(*ecdsa.PrivateKey)
	if err != nil || !ok {
		fmt.Fprintln(os.Stderr, "fin-mcp-check: the key is not an EC private key in PKCS #8")
		os.Exit(1)
	}
	c := &check{endpoint: *endpoint, signer: &mcptest.Signer{Key: key, Issuer: "idp-mcp-gateway:" + *gateway, Audience: *endpoint, Now: time.Now}}
	member := mcp.Caller{Subject: "analyst-1", Tenant: *tenant, Roles: []string{*role}, Client: "fin-mcp-check"}

	list := c.post("tools/list", "", mcptest.Body(1, "tools/list", map[string]any{}, ""))
	tools, _ := list.rpc["result"].(map[string]any)["tools"].([]any)
	names := []string{}
	for _, t := range tools {
		names = append(names, t.(map[string]any)["name"].(string))
	}
	fmt.Printf("tools/list without an assertion: HTTP %d, %d tools: %s\n", list.status, len(tools), strings.Join(names, ", "))
	c.expect("tools/list without an assertion", list.status == 200 && len(tools) == 3, list)

	losses := func() answer {
		_, a := c.call(member, "list_loss_jobs", map[string]any{"legal_entity_id": *entity, "limit": 5})
		return a
	}()
	rows, _ := losses.data["rows"].([]any)
	c.expect(fmt.Sprintf("list_loss_jobs %s limit 5: %d rows, next_cursor %v, as_of %v", *entity, len(rows), losses.data["next_cursor"] != nil, losses.data["as_of"]),
		!losses.isErr && losses.status == 200 && len(rows) > 0, losses)
	if len(rows) == 0 {
		fmt.Println("no loss-making job to read; the rest of the steps need one")
		os.Exit(1)
	}
	job, _ := rows[0].(map[string]any)["job_id"].(string)
	for _, r := range rows {
		row := r.(map[string]any)
		fmt.Printf("     row %v: profit %v, basis %v\n", row["job_id"], row["profit"], row["profit_basis"])
	}
	if cursor, _ := losses.data["next_cursor"].(string); cursor != "" {
		_, next := c.call(member, "list_loss_jobs", map[string]any{"legal_entity_id": *entity, "limit": 5, "cursor": cursor})
		more, _ := next.data["rows"].([]any)
		c.expect(fmt.Sprintf("list_loss_jobs with its next_cursor: %d more rows", len(more)), !next.isErr && next.status == 200, next)
	}
	_, profit := c.call(member, "get_job_profit", map[string]any{"job_id": job})
	c.expect(fmt.Sprintf("get_job_profit %s: profit %v, version %.12v...", job, profit.data["profit"], profit.data["profit_version"]),
		!profit.isErr && profit.data["profit_version"] == rows[0].(map[string]any)["profit_version"], profit)
	_, reviews := c.call(member, "list_job_cost_reviews", map[string]any{"job_id": job})
	requests, _ := reviews.data["requests"].([]any)
	c.expect(fmt.Sprintf("list_job_cost_reviews %s: %d requests", job, len(requests)), !reviews.isErr && reviews.status == 200, reviews)

	args := map[string]any{"name": "get_job_profit", "arguments": map[string]any{"job_id": job}}
	none := c.post("tools/call", "get_job_profit", mcptest.Body(90, "tools/call", args, ""))
	c.expect("get_job_profit without an assertion", none.status == 403, none)
	body, first := c.call(member, "get_job_profit", map[string]any{"job_id": job})
	c.expect("a call with a fresh assertion", first.status == 200 && !first.isErr, first)
	replayed := c.post("tools/call", "get_job_profit", body)
	c.expect("the same assertion sent again (replay)", replayed.status == 403, replayed)
	stranger := mcp.Caller{Subject: "analyst-2", Tenant: *tenant, Roles: []string{"not-configured"}}
	_, denied := c.call(stranger, "get_job_profit", map[string]any{"job_id": job})
	c.expect("a role the configuration does not name", denied.isErr && denied.text == "you are not allowed to do this in FIN", denied)
	_, otherTenant := c.call(mcp.Caller{Subject: "analyst-3", Tenant: "no-such-tenant", Roles: []string{*role}}, "list_loss_jobs", map[string]any{"legal_entity_id": *entity})
	c.expect("a gateway tenant the configuration does not name", otherTenant.isErr && otherTenant.text == "you are not allowed to do this in FIN", otherTenant)
	_, otherEntity := c.call(member, "list_loss_jobs", map[string]any{"legal_entity_id": "NOT-" + *entity})
	c.expect("a legal entity the role does not see", otherEntity.isErr && otherEntity.text == "you are not allowed to do this in FIN", otherEntity)
	_, extra := c.call(member, "get_job_profit", map[string]any{"job_id": job, "tenant_id": "other"})
	c.expect("a tenant in the arguments", extra.isErr && strings.Contains(extra.text, "not valid"), extra)
	if c.failed > 0 {
		fmt.Printf("%d steps failed\n", c.failed)
		os.Exit(1)
	}
	fmt.Println("all steps as expected")
}
