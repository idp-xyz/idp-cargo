package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// testCaller is the routes' Caller in these tests: the caller's permissions ride on the context.
type testCaller struct{}

type callerKey struct{}

func (testCaller) Who(ctx context.Context) (string, string, bool) {
	_, ok := ctx.Value(callerKey{}).([]string)
	return "T", "ops-1", ok
}

func (testCaller) Allowed(ctx context.Context, bc, action string, named bool) bool {
	perms, _ := ctx.Value(callerKey{}).([]string)
	if named {
		return slices.Contains(perms, bc+":"+action)
	}
	return slices.Contains(perms, "*") || slices.Contains(perms, bc+":*") || slices.Contains(perms, bc+":"+action)
}

func TestSnake(t *testing.T) {
	for in, want := range map[string]string{"PartyID": "party_id", "ID": "id", "OrderIDs": "order_ids", "HBL": "hbl", "VesselIMO": "vessel_imo",
		"SHA256": "sha256", "ETD": "etd", "MAWBPrefix": "mawb_prefix", "GrossWeightGrams": "gross_weight_grams", "Line2Vessel": "line2_vessel",
		"CutOffs": "cut_offs", "URL": "url", "HBLs": "hbls", "IsOK": "is_ok", "VGMGrams": "vgm_grams", "Status": "status"} {
		if got := Snake(in); got != want {
			t.Errorf("Snake(%q) = %q, want %q", in, got, want)
		}
	}
}

type money struct {
	Currency string
	Minor    int64
}

type addAddress struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	Lines            []string
	Price            *money
	Note             string
	Platform         string
	OccurredAt       time.Time
}

type receipt struct {
	CommandID   string
	PartyID     string
	NewRevision int64
}

type getParty struct {
	TenantID string
	PartyID  string
	Limit    int
	Active   bool
}

type party struct {
	PartyID  string
	Revision int64
	Names    map[string]string
	When     time.Time
	Tags     []string
}

var testKinds = Errors{Unauthenticated: errors.New("u"), Unauthorized: errors.New("f"), NotFound: errors.New("n"), Conflict: errors.New("c"),
	IdempotencyConflict: errors.New("i"), Rejected: errors.New("r")}

func serve(t *testing.T, r *Routes, method, path, body string, headers map[string]string, perms ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if perms == nil {
		perms = []string{}
	}
	req = req.WithContext(context.WithValue(req.Context(), callerKey{}, perms))
	w := httptest.NewRecorder()
	r.Mux().ServeHTTP(w, req)
	return w
}

func TestACommandIsTheContextsOwnStructWithTheServersFieldsSet(t *testing.T) {
	r := NewRoutes(http.NewServeMux(), testCaller{})
	var got addAddress
	Command(r, testKinds, Op{Path: "/v1/parties/{party_id}/addresses", Context: "org", Calls: "AddPartyAddress", Action: "AddPartyAddress", Deny: []string{"platform"}},
		func(_ context.Context, c addAddress) (receipt, error) {
			got = c
			return receipt{CommandID: c.CommandID, PartyID: c.PartyID, NewRevision: 8}, nil
		})
	h := map[string]string{"Idempotency-Key": "cmd-1", "If-Match": `"7"`}
	w := serve(t, r, "POST", "/v1/parties/P-1/addresses", `{"lines":["1 Road"],"price":{"currency":"CNY","minor":10001},"note":"a\nb","occurred_at":"2026-11-26T10:00:00+08:00"}`, h)
	if w.Code != 200 || w.Header().Get("ETag") != `"8"` || strings.TrimSpace(w.Body.String()) != `{"command_id":"cmd-1","party_id":"P-1","new_revision":8}` {
		t.Fatalf("%d %s %s", w.Code, w.Header(), w.Body)
	}
	if got.TenantID != "T" || got.ActorID != "ops-1" || got.CommandID != "cmd-1" || got.ExpectedRevision != 7 || got.PartyID != "P-1" || got.Price.Minor != 10001 ||
		!got.OccurredAt.Equal(time.Date(2026, 11, 26, 2, 0, 0, 0, time.UTC)) || got.OccurredAt.Location() != time.UTC {
		t.Fatalf("bound: %+v", got)
	}
	for name, tc := range map[string]struct {
		body    string
		headers map[string]string
		code    int
		detail  string
	}{
		"a tenant in the body":  {`{"tenant_id":"X","occurred_at":"2026-11-26T10:00:00Z"}`, h, 400, `unknown field "tenant_id"`},
		"the actor":             {`{"actor_id":"X","occurred_at":"2026-11-26T10:00:00Z"}`, h, 400, `unknown field "actor_id"`},
		"the path's field":      {`{"party_id":"P-9","occurred_at":"2026-11-26T10:00:00Z"}`, h, 400, `unknown field "party_id"`},
		"a denied field":        {`{"platform":"x","occurred_at":"2026-11-26T10:00:00Z"}`, h, 400, `unknown field "platform"`},
		"an unknown field":      {`{"nope":1,"occurred_at":"2026-11-26T10:00:00Z"}`, h, 400, `unknown field "nope"`},
		"a nested unknown":      {`{"price":{"currency":"CNY","minor":1,"x":1},"occurred_at":"2026-11-26T10:00:00Z"}`, h, 400, `unknown field "price.x"`},
		"Go's name":             {`{"Note":"x","occurred_at":"2026-11-26T10:00:00Z"}`, h, 400, "snake_case"},
		"a key twice":           {`{"note":"x","note":"y","occurred_at":"2026-11-26T10:00:00Z"}`, h, 400, "twice"},
		"a control character":   {`{"note":"a\u0000","occurred_at":"2026-11-26T10:00:00Z"}`, h, 400, "control character"},
		"the wrong type":        {`{"note":1,"occurred_at":"2026-11-26T10:00:00Z"}`, h, 400, `"note" has the wrong type`},
		"a fraction for minor":  {`{"price":{"currency":"CNY","minor":1.5},"occurred_at":"2026-11-26T10:00:00Z"}`, h, 400, `"price.minor"`},
		"no business time":      {`{}`, h, 400, "OCCURRED_AT_REQUIRED"},
		"no Idempotency-Key":    {`{"occurred_at":"2026-11-26T10:00:00Z"}`, map[string]string{"If-Match": `"7"`}, 400, "IDEMPOTENCY_KEY_REQUIRED"},
		"no If-Match":           {`{"occurred_at":"2026-11-26T10:00:00Z"}`, map[string]string{"Idempotency-Key": "c"}, 428, "PRECONDITION_REQUIRED"},
		"not one object":        {`[1]`, h, 400, "one JSON object"},
		"a revision as a field": {`{"expected_revision":3,"occurred_at":"2026-11-26T10:00:00Z"}`, h, 400, `unknown field "expected_revision"`},
	} {
		w := serve(t, r, "POST", "/v1/parties/P-1/addresses", tc.body, tc.headers)
		if w.Code != tc.code || !strings.Contains(strings.ReplaceAll(w.Body.String(), `\"`, `"`), tc.detail) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

func TestAQueryTakesItsFieldsFromThePathAndTheQueryString(t *testing.T) {
	r := NewRoutes(http.NewServeMux(), testCaller{})
	var got getParty
	Query(r, testKinds, Op{Path: "/v1/parties/{party_id}", Context: "org", Calls: "GetParty"}, func(_ context.Context, q getParty) (party, error) {
		got = q
		if q.PartyID == "P-0" {
			return party{}, testKinds.NotFound
		}
		return party{PartyID: q.PartyID, Revision: 4, Names: map[string]string{"zh": "甲", "en": "A"}, When: time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)}, nil
	})
	w := serve(t, r, "GET", "/v1/parties/P-1?limit=5&active=true", "", nil, "org:Read")
	if w.Code != 200 || w.Header().Get("ETag") != `"4"` ||
		strings.TrimSpace(w.Body.String()) != `{"party_id":"P-1","revision":4,"names":{"en":"A","zh":"甲"},"when":"2026-11-26T00:00:00Z","tags":[]}` {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got.TenantID != "T" || got.Limit != 5 || !got.Active {
		t.Fatalf("bound: %+v", got)
	}
	for name, tc := range map[string]struct {
		path  string
		perms []string
		code  int
		want  string
	}{
		"no permission":      {"/v1/parties/P-1", nil, 403, "FORBIDDEN"},
		"another context's":  {"/v1/parties/P-1", []string{"com:Read"}, 403, "FORBIDDEN"},
		"the context's all":  {"/v1/parties/P-1", []string{"org:*"}, 200, "P-1"},
		"a tenant parameter": {"/v1/parties/P-1?tenant_id=X", []string{"org:Read"}, 400, `unknown parameter "tenant_id"`},
		"a number":           {"/v1/parties/P-1?limit=x", []string{"org:Read"}, 400, "whole number"},
		"twice":              {"/v1/parties/P-1?limit=1&limit=2", []string{"org:Read"}, 400, "once"},
		"not found":          {"/v1/parties/P-0", []string{"org:Read"}, 404, "NOT_FOUND"},
	} {
		w := serve(t, r, "GET", tc.path, "", nil, tc.perms...)
		if w.Code != tc.code || !strings.Contains(strings.ReplaceAll(w.Body.String(), `\"`, `"`), tc.want) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	// a named permission: the wildcards do not grant it
	Query(r, testKinds, Op{Path: "/v1/parties/{party_id}/contacts", Context: "org", Calls: "GetPartyContacts", Action: "ReadPartyContacts", Named: true},
		func(_ context.Context, q getParty) (party, error) { return party{PartyID: q.PartyID}, nil })
	if w := serve(t, r, "GET", "/v1/parties/P-1/contacts", "", nil, "org:*", "*"); w.Code != 403 {
		t.Fatalf("a named permission by wildcard: %d", w.Code)
	}
	if w := serve(t, r, "GET", "/v1/parties/P-1/contacts", "", nil, "org:ReadPartyContacts"); w.Code != 200 {
		t.Fatalf("a named permission: %d", w.Code)
	}
}

func TestTheOpenAPIDocumentIsMadeFromTheTable(t *testing.T) {
	r := NewRoutes(nil, nil)
	Command(r, testKinds, Op{Path: "/v1/parties/{party_id}/addresses", Context: "org", Calls: "AddPartyAddress", Action: "AddPartyAddress", Status: 201},
		func(context.Context, addAddress) (receipt, error) { return receipt{}, nil })
	Query(r, testKinds, Op{Path: "/v1/parties/{party_id}", Context: "org", Calls: "GetParty"}, func(context.Context, getParty) (party, error) { return party{}, nil })
	r.Legacy("sea", "GET /v1/ocean-movements/{movement}", "GetOceanMovement")
	a, err := OpenAPI("Cargo", "test", r.Ops())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := OpenAPI("Cargo", "test", r.Ops())
	if !bytes.Equal(a, b) {
		t.Fatal("not deterministic")
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Permission  string `json:"x-permission"`
			Parameters  []struct{ Name, In string }
			RequestBody struct {
				Content map[string]struct {
					Schema struct{ Properties map[string]any }
				}
			} `json:"requestBody"`
			Responses map[string]any
		}
	}
	if err = json.Unmarshal(a, &doc); err != nil {
		t.Fatal(err)
	}
	add := doc.Paths["/v1/parties/{party_id}/addresses"]["post"]
	props := add.RequestBody.Content["application/json"].Schema.Properties
	if add.OperationID != "org.AddPartyAddress" || add.Permission != "org:AddPartyAddress" || add.Responses["201"] == nil || len(add.Parameters) != 3 {
		t.Fatalf("the command: %+v", add)
	}
	for _, k := range []string{"tenant_id", "actor_id", "command_id", "expected_revision", "party_id"} {
		if props[k] != nil {
			t.Errorf("the body documents %s", k)
		}
	}
	if props["lines"] == nil || props["occurred_at"] == nil || props["price"] == nil {
		t.Errorf("the body: %v", props)
	}
	get := doc.Paths["/v1/parties/{party_id}"]["get"]
	if get.Permission != "org:Read" || len(get.Parameters) != 3 {
		t.Fatalf("the query: %+v", get)
	}
	if doc.Paths["/v1/ocean-movements/{movement}"]["get"].OperationID != "sea.GetOceanMovement" {
		t.Fatal("the legacy route")
	}
}
