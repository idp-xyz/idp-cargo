package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/google/jsonschema-go/jsonschema"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// listed is the catalogue as a client reads it: the tools/list result of the real handler, without an assertion (the
// gateway reads it without a caller).
func (r *rig) listed() (tools []map[string]any, raw json.RawMessage) {
	r.t.Helper()
	got := r.rpc(nil, "tools/list", map[string]any{}, "")
	res, _ := got.rpc["result"].(map[string]any)
	var envelope struct {
		Result struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(got.body), &envelope); err != nil || res == nil {
		r.t.Fatalf("tools/list: %d %s", got.status, got.body)
	}
	if err := json.Unmarshal(envelope.Result.Tools, &tools); err != nil {
		r.t.Fatal(err)
	}
	return tools, envelope.Result.Tools
}

// The catalogue is pinned: the gateway pins what its reviewers approved, and a change of a name, a description, a
// schema or an annotation is a drift for which it blocks the target until a new review (the guide's section 6.2). This
// is the reminder. When a declaration has to change on purpose, change the constant below with it and say so to the
// platform's operators, who must review the catalogue again (docs/api/fin-mcp.md).
// The digest is idp-mcp's canonical digest of the "tools" array of the tools/list result.
const pinnedCatalogueDigest = "sha256:737616495fa3f18b9cb4a31a685ce67a893243ce60978aa27f339d15a1ad16d3"

func TestTheCatalogueIsPinned(t *testing.T) {
	r := newRig(t, &fakeFIN{})
	_, raw := r.listed()
	sum, err := digestOf(raw)
	if err != nil {
		t.Fatal(err)
	}
	if sum != pinnedCatalogueDigest {
		t.Fatalf("the catalogue's digest is %s, pinned %s: a declaration changed, and the gateway would block the target until a new review", sum, pinnedCatalogueDigest)
	}
}

func TestTheThreeToolsAreReadOnlyAndClosed(t *testing.T) {
	r := newRig(t, &fakeFIN{})
	tools, _ := r.listed()
	if len(tools) != 3 {
		t.Fatalf("%d tools", len(tools))
	}
	// the SDK lists tools by name
	want := []string{"get_job_profit", "list_job_cost_reviews", "list_loss_jobs"}
	for i, tool := range tools {
		if tool["name"] != want[i] {
			t.Errorf("tool %d is %v, want %s", i, tool["name"], want[i])
		}
		ann, _ := tool["annotations"].(map[string]any)
		if ann["readOnlyHint"] != true || ann["openWorldHint"] != false || ann["destructiveHint"] == false {
			t.Errorf("%s: annotations %v", want[i], ann)
		}
		in, _ := tool["inputSchema"].(map[string]any)
		if in["additionalProperties"] != false || in["type"] != "object" {
			t.Errorf("%s: the input schema is open: %v", want[i], in)
		}
		if tool["outputSchema"] == nil || tool["title"] == nil || tool["description"] == nil {
			t.Errorf("%s: no output schema, title or description", want[i])
		}
		// every field is typed and bounded
		props, _ := in["properties"].(map[string]any)
		for name, p := range props {
			prop := p.(map[string]any)
			switch prop["type"] {
			case "string":
				if prop["enum"] == nil && (prop["maxLength"] == nil || prop["minLength"] == nil && name != "state" || prop["pattern"] == nil) {
					t.Errorf("%s.%s: a string without length bounds or a pattern: %v", want[i], name, prop)
				}
			case "integer":
				if prop["minimum"] == nil || prop["maximum"] == nil {
					t.Errorf("%s.%s: an integer without bounds", want[i], name)
				}
			default:
				t.Errorf("%s.%s: type %v", want[i], name, prop["type"])
			}
		}
	}
}

// Nothing in an input or a result names a tenant or a person: the tenant and the actor are the caller's, taken from the
// assertion, and no result carries a member's name.
func TestNoSchemaNamesATenantOrAPerson(t *testing.T) {
	r := newRig(t, &fakeFIN{})
	tools, _ := r.listed()
	person := regexp.MustCompile(`(?i)tenant|actor|person|user|member|subject|owner|_by$|party|name|email|requester|resolver`)
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			if props, ok := x["properties"].(map[string]any); ok {
				for name := range props {
					if person.MatchString(name) {
						t.Errorf("%s names %q", path, name)
					}
				}
			}
			for k, c := range x {
				walk(path+"/"+k, c)
			}
		case []any:
			for i, c := range x {
				walk(path+"/"+string(rune('0'+i)), c)
			}
		}
	}
	for _, tool := range tools {
		walk(tool["name"].(string)+"/input", tool["inputSchema"])
		walk(tool["name"].(string)+"/output", tool["outputSchema"])
	}
}

// printable reports whether s has only what a reviewer can read: no control, zero-width, bidirectional-control or tag
// character, no line break.
func printable(s string) bool {
	for _, c := range s {
		if !unicode.IsPrint(c) {
			return false
		}
	}
	return true
}

func TestTheCheckOfADescriptionSeesWhatItShould(t *testing.T) {
	for _, bad := range []string{"a\x00b", "a\nb", "a\tb", "a\u200bb", "a\u202eb", "a\u2028b", "a\U000e0041b", "a\u00adb", "a\ufeffb"} {
		if printable(bad) {
			t.Errorf("%q passes", bad)
		}
	}
	if !printable("List jobs (profit < 0) of a legal entity, in minor units: 100.01") {
		t.Error("a plain description fails")
	}
}

// Every description and title of the declarations, in the tool, in the input schema and in the output schema, is at
// most 1024 bytes without a control or invisible character.
func TestEveryDescriptionAndTitleIsShortAndPlain(t *testing.T) {
	r := newRig(t, &fakeFIN{})
	tools, _ := r.listed()
	count := 0
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				if s, ok := c.(string); ok && (k == "description" || k == "title") {
					count++
					if len(s) > 1024 || !printable(s) || strings.TrimSpace(s) != s || s == "" {
						t.Errorf("%s/%s: %d bytes, printable %v: %q", path, k, len(s), printable(s), s)
					}
				}
				walk(path+"/"+k, c)
			}
		case []any:
			for _, c := range x {
				walk(path, c)
			}
		}
	}
	for _, tool := range tools {
		walk(tool["name"].(string), tool)
	}
	if count < 30 {
		t.Fatalf("only %d descriptions and titles were walked", count)
	}
}

// What a result says is what its declaration says: each successful result of each tool validates against the output
// schema the catalogue carries.
func TestResultsMatchTheirDeclaredSchemas(t *testing.T) {
	fin := richFIN()
	r := newRig(t, fin)
	tools, _ := r.listed()
	schemas := map[string]*jsonschema.Schema{}
	for _, tool := range tools {
		raw, _ := json.Marshal(tool["outputSchema"])
		var s jsonschema.Schema
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		schemas[tool["name"].(string)] = &s
	}
	for tool, args := range map[string]map[string]any{
		"list_loss_jobs":        {"legal_entity_id": "LE"},
		"get_job_profit":        {"job_id": "JOB-1"},
		"list_job_cost_reviews": {"job_id": "JOB-1"},
	} {
		got := r.call(&alice, tool, args)
		if got.isError || got.data == nil {
			t.Fatalf("%s: %s", tool, got.body)
		}
		resolved, err := schemas[tool].Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := resolved.Validate(got.data); err != nil {
			t.Errorf("%s: the result does not match its schema: %v\n%v", tool, err, got.data)
		}
		if got.text == "" || len(got.text) > 400 {
			t.Errorf("%s: the text is %q", tool, got.text)
		}
	}
}

// richFIN answers every query with a job of legal entity LE that has every part.
func richFIN() *fakeFIN {
	return &fakeFIN{
		jobs: func(_ context.Context, q api.ListJobProfits) (api.JobProfitPage, error) {
			return api.JobProfitPage{LegalEntityID: q.LegalEntityID, AsOf: asOf, Rows: []api.JobProfitRow{lossJob(1)}, NextAfter: "JOB-1"}, nil
		},
		profit: func(_ context.Context, q api.GetJobProfit) (api.JobProfit, error) {
			return api.JobProfit{JobID: q.JobID, Revision: 3, ProfitVersion: strings.Repeat("ab", 32), Revenue: cny(12000), Cost: cny(0), PoolCost: cny(20001), Profit: cny(-8001),
				ProfitBasis: "SEPARATE", ByCode: []api.ChargeCodeProfit{{ChargeCode: "OCEAN_FREIGHT", Revenue: cny(12000), Profit: cny(12000)}},
				PoolShares:      []api.JobPoolShare{{PoolID: "P-2", LineNo: 1, ChargeCode: "OCEAN_FREIGHT", Method: "CHARGEABLE", Local: cny(10001)}},
				FormerShares:    []api.JobPoolShare{{PoolID: "P-3", LineNo: 1, ChargeCode: "OCEAN_FREIGHT", Method: "CHARGEABLE", Local: cny(5), Pending: true}},
				ReroutingLosses: []api.JobReroutingLoss{{PoolID: "P-1", LineNo: 1, Local: cny(10000), Voucher: "CLAIM-1"}}}, nil
		},
		requests: func(_ context.Context, q api.ListCostReviewRequests) (api.CostReviewPage, error) {
			open := api.CostReviewRequest{RequestID: "CRR-1", JobID: q.JobID, State: api.ReviewOpen, RequestedBy: "carol", RequestedAt: asOf, Reason: "too high", Revenue: cny(1),
				Cost: cny(2), Profit: cny(-1), ProfitBasis: "SEPARATE", ProfitVersion: "v"}
			done := open
			done.RequestID, done.State = "CRR-2", api.ReviewResolved
			done.Resolution = &api.CostReviewResolution{ResolvedBy: "dave", ResolvedAt: asOf, Outcome: "NO_CHANGE", Note: "right"}
			return api.CostReviewPage{Requests: []api.CostReviewRequest{open, done}}, nil
		},
	}
}

// The inputs: missing, extra (the tenant and the actor among them), wrong types, out of range and with control
// characters are refused, with the same sentence and before anything reaches FIN.
func TestInputsThatDoNotMatchTheSchemaAreRefusedBeforeFIN(t *testing.T) {
	fin := richFIN()
	r := newRig(t, fin)
	good := map[string]map[string]any{
		"list_loss_jobs":        {"legal_entity_id": "LE"},
		"get_job_profit":        {"job_id": "JOB-1"},
		"list_job_cost_reviews": {"job_id": "JOB-1"},
	}
	id := map[string]string{"list_loss_jobs": "legal_entity_id", "get_job_profit": "job_id", "list_job_cost_reviews": "job_id"}
	with := func(tool, key string, v any) map[string]any {
		a := map[string]any{}
		for k, x := range good[tool] {
			a[k] = x
		}
		if v == nil {
			delete(a, key)
		} else {
			a[key] = v
		}
		return a
	}
	for tool := range good {
		cases := map[string]map[string]any{
			"the id missing":          with(tool, id[tool], nil),
			"an empty id":             with(tool, id[tool], ""),
			"an id of a number":       with(tool, id[tool], 7),
			"an id with a space":      with(tool, id[tool], "A B"),
			"an id with a line break": with(tool, id[tool], "LE\n"),
			"an id with a NUL":        with(tool, id[tool], "LE\x00"),
			"an id with an escape":    with(tool, id[tool], "LE\x1b[0m"),
			"an id that starts badly": with(tool, id[tool], "-LE"),
			"an id too long":          with(tool, id[tool], strings.Repeat("A", 129)),
			"an id with a quote":      with(tool, id[tool], `LE"`),
			"a tenant":                with(tool, "tenant_id", "cargo-b"),
			"an actor":                with(tool, "actor_id", "carol"),
			"a requester":             with(tool, "requested_by", "carol"),
			"an unknown field":        with(tool, "colour", "red"),
			"the id in another case":  with(tool, strings.ToUpper(id[tool]), "LE"),
		}
		if tool != "get_job_profit" {
			for k, v := range map[string]any{"a limit of 0": 0, "a limit of 51": 51, "a limit of -1": -1, "a limit of text": "5", "a limit of 2.5": 2.5, "a null limit": nil} {
				a := with(tool, "limit", v)
				if v == nil {
					a["limit"] = nil
				}
				cases[k] = a
			}
			for k, v := range map[string]any{"a cursor of a number": 5, "an empty cursor": "", "a cursor without a dot": "AAAA", "a cursor with a line break": "AAAA.BB\nBB",
				"a cursor too long": strings.Repeat("A", 600) + "." + strings.Repeat("B", 600)} {
				cases[k] = with(tool, "cursor", v)
			}
		}
		if tool == "list_job_cost_reviews" {
			cases["a state that is none"] = with(tool, "state", "DONE")
			cases["a state in lower case"] = with(tool, "state", "open")
			cases["a state of a number"] = with(tool, "state", 1)
		}
		for name, args := range cases {
			got := r.call(&alice, tool, args)
			if !got.isError || got.text != msgInvalid {
				t.Errorf("%s, %s: isError %v, %q (%s)", tool, name, got.isError, got.text, got.body)
			}
		}
		// the arguments missing altogether, not an object, an array
		for name, body := range map[string]string{
			"no arguments":       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			"arguments of text":  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":"LE","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			"arguments an array": `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":["LE"],"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		} {
			got := r.raw(&alice, "tools/call", tool, []byte(body))
			if (!got.isError || got.text != msgInvalid) && got.rpc["error"] == nil {
				t.Errorf("%s, %s: %s", tool, name, got.body)
			}
		}
	}
	if n := fin.total(); n != 0 {
		t.Fatalf("FIN was asked %d times for input that does not match the schema", n)
	}
	// the log says that the arguments were refused, and none of what they held
	if n := strings.Count(r.logged(), "outcome=invalid_input"); n < 50 {
		t.Errorf("the log has %d lines for refused arguments", n)
	}
	for _, value := range []string{"A B", "[0m", strings.Repeat("A", 129), "cargo-b", "carol", "colour", "tenant_id", "validating", "maxLength", "unevaluated"} {
		if strings.Contains(r.logged(), value) {
			t.Errorf("the log holds %q of the refused arguments", value)
		}
	}
	// a good call passes, with the defaults
	if got := r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE"}); got.isError {
		t.Fatalf("a good call: %s", got.body)
	}
}

var asOf = time.Date(2026, 11, 25, 13, 33, 6, 752086000, time.UTC)

func lossJob(n int) api.JobProfitRow {
	id := fmt.Sprintf("JOB-%03d", n)
	return api.JobProfitRow{JobID: id, ShipmentID: "SH-" + id, State: "OPEN", Currency: "CNY", Revenue: cny(12000), Cost: cny(20001), Profit: cny(-8001), ProfitBasis: "SEPARATE",
		ProfitVersion: strings.Repeat("0", 64)}
}

func digestOf(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
