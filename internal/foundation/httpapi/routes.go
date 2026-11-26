package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Op describes one route: what it calls, who may call it and what it takes and answers. Routes is the table of
// every route; the OpenAPI document is made from it (OpenAPI), so the document and the routes cannot differ.
type Op struct {
	Method  string // GET or POST
	Path    string // with {name} parameters, which bind to the field of the same key
	Context string // the bounded context, as in its permissions ("org")
	Calls   string // the api method the route calls ("AddPartyAddress"); a hand-written route names it too
	Summary string
	// Action is the permission: for a command, the one the context checks ("AddPartyAddress", as "<context>:Action");
	// for a query, the one the route checks before calling (identity.ActionRead unless named). Named is true for a
	// permission granted by name only, which "<context>:*" and "*" do not grant. ContextChecks is true for a query the
	// context authorizes itself (it takes the actor), so the route checks nothing more.
	Action        string
	Named         bool
	ContextChecks bool
	Status        int      // the success status: 200 unless set (201 for a create)
	Deny          []string // body keys refused besides the server's own, such as a field only the platform sets
	Legacy        bool     // a hand-written route (RATE, SEA, FIN before V2.106) whose schemas are not described here

	command bool
	input   reflect.Type // the api command or query
	result  reflect.Type
}

// Routes registers routes on a ServeMux and keeps their table.
type Routes struct {
	mux    *http.ServeMux
	caller Caller
	ops    []Op
}

// Caller tells a route who calls it and what the caller may do: the verified principal the transport put on the
// request's context (foundation/identity), which this package does not import.
type Caller interface {
	// Who answers the caller's tenant and actor; ok is false without a verified principal.
	Who(ctx context.Context) (tenant, actor string, ok bool)
	// Allowed reports whether the caller holds "<context>:<action>"; named, whether it holds it by name only.
	Allowed(ctx context.Context, context, action string, named bool) bool
}

// NewRoutes registers on mux, with caller for the requests; a nil mux only keeps the table.
func NewRoutes(mux *http.ServeMux, caller Caller) *Routes { return &Routes{mux: mux, caller: caller} }

// Ops answers the table, sorted by path and method.
func (r *Routes) Ops() []Op {
	out := append([]Op(nil), r.ops...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// Mux is the ServeMux, for a hand-written adapter that registers with HandleFunc; it names its routes with Legacy.
func (r *Routes) Mux() *http.ServeMux { return r.mux }

// Mux is what a hand-written adapter registers its routes on.
type Mux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// LegacyMux is a Mux that registers a context's hand-written routes and records each in the table, its schemas not
// described (Op.Legacy).
func (r *Routes) LegacyMux(context string) Mux { return legacyMux{r: r, context: context} }

type legacyMux struct {
	r       *Routes
	context string
}

func (m legacyMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.r.Legacy(m.context, pattern, "")
	if m.r.mux != nil {
		m.r.mux.HandleFunc(pattern, handler)
	}
}

// Legacy records a hand-written route registered on Mux: its pattern ("GET /v1/..."), context and the api method.
func (r *Routes) Legacy(context, pattern, calls string) {
	method, path, _ := strings.Cut(pattern, " ")
	r.ops = append(r.ops, Op{Method: method, Path: path, Context: context, Calls: calls, Legacy: true})
}

// ActionRead is the permission of a context's queries, "<context>:Read" (identity.ActionRead).
const ActionRead = "Read"

// Server-set fields: a body or a query string never sends them.
const (
	fieldCommandID        = "CommandID"
	fieldTenantID         = "TenantID"
	fieldActorID          = "ActorID"
	fieldExpectedRevision = "ExpectedRevision"
	fieldOccurredAt       = "OccurredAt"
)

var pathParam = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)

// shape is how a route binds its input: the server's fields, the path parameters and the fields left to the body or
// the query string.
type shape struct {
	commandID, tenant, actor, revision, occurred []int
	path                                         map[string][]int // parameter -> field index
	server                                       map[string]bool  // keys the client may not send
}

func shapeOf(op Op, t reflect.Type) shape {
	if t.Kind() != reflect.Struct {
		panic(errNotStruct)
	}
	bindable(t, map[reflect.Type]bool{})
	s := shape{path: map[string][]int{}, server: map[string]bool{}}
	index := func(name string) []int {
		if f, ok := t.FieldByName(name); ok && len(f.Index) == 1 {
			if k := keyOf(t, name); k != "" {
				s.server[k] = true
			}
			return f.Index
		}
		return nil
	}
	s.commandID, s.tenant, s.actor = index(fieldCommandID), index(fieldTenantID), index(fieldActorID)
	if op.command {
		s.revision = index(fieldExpectedRevision)
		if f, ok := t.FieldByName(fieldOccurredAt); ok && f.Type == reflect.TypeFor[time.Time]() {
			s.occurred = f.Index
		}
	}
	byKey := map[string][]int{}
	for _, f := range fieldsOf(t) {
		byKey[f.key] = f.index
	}
	for _, m := range pathParam.FindAllStringSubmatch(op.Path, -1) {
		idx, ok := byKey[m[1]]
		if !ok {
			panic(fmt.Sprintf("httpapi: %s %s: {%s} is no field of %s", op.Method, op.Path, m[1], t))
		}
		s.path[m[1]], s.server[m[1]] = idx, true
	}
	for _, k := range op.Deny {
		if _, ok := byKey[k]; !ok {
			panic(fmt.Sprintf("httpapi: %s %s: denied %q is no field of %s", op.Method, op.Path, k, t))
		}
		s.server[k] = true
	}
	if op.command && s.commandID == nil {
		panic(fmt.Sprintf("httpapi: %s %s: %s has no CommandID", op.Method, op.Path, t))
	}
	return s
}

func (r *Routes) add(op Op, h http.HandlerFunc) {
	if op.Method != http.MethodGet && op.Method != http.MethodPost {
		panic("httpapi: a route is GET or POST")
	}
	for _, o := range r.ops {
		if o.Method == op.Method && o.Path == op.Path {
			panic(fmt.Sprintf("httpapi: %s %s twice", op.Method, op.Path))
		}
	}
	r.ops = append(r.ops, op)
	if r.mux != nil {
		r.mux.HandleFunc(op.Method+" "+op.Path, h)
	}
}

// Command registers a POST route that runs a context's command: the body is the command (snake_case JSON) without the
// fields the server sets. CommandID is the Idempotency-Key; TenantID and ActorID are the verified principal's;
// ExpectedRevision is If-Match; the path parameters set their fields; OccurredAt, the business time, is required. The
// response is the command's receipt, with NewRevision as ETag when it has one. The context authorizes the command
// itself (op.Action documents its permission), before it replays an earlier one.
func Command[C, R any](r *Routes, kinds Errors, op Op, call func(context.Context, C) (R, error)) {
	op.command, op.input, op.result = true, reflect.TypeFor[C](), reflect.TypeFor[R]()
	if op.Method == "" {
		op.Method = http.MethodPost
	}
	if op.Status == 0 {
		op.Status = http.StatusOK
	}
	s := shapeOf(op, op.input)
	r.add(op, func(w http.ResponseWriter, req *http.Request) {
		tenant, actor, ok := r.caller.Who(req.Context())
		if !ok {
			WriteProblem(w, req, http.StatusUnauthorized, "UNAUTHENTICATED", "")
			return
		}
		id, ok := CommandID(w, req)
		if !ok {
			return
		}
		var revision int64
		if s.revision != nil {
			if revision, ok = ExpectedRevision(w, req); !ok {
				return
			}
		}
		var in C
		v := reflect.ValueOf(&in)
		if !decodeRequest(w, req, v, s.server) {
			return
		}
		e := v.Elem()
		if !bindPath(w, req, e, s) {
			return
		}
		e.FieldByIndex(s.commandID).SetString(id)
		setString(e, s.tenant, tenant)
		setString(e, s.actor, actor)
		if s.revision != nil {
			e.FieldByIndex(s.revision).SetInt(revision)
		}
		if s.occurred != nil {
			at := e.FieldByIndex(s.occurred).Interface().(time.Time)
			if at.IsZero() {
				WriteProblem(w, req, http.StatusBadRequest, "OCCURRED_AT_REQUIRED", "a command states its business time in occurred_at (RFC 3339)")
				return
			}
			e.FieldByIndex(s.occurred).Set(reflect.ValueOf(at.UTC()))
		}
		out, err := call(req.Context(), in)
		if err != nil {
			Fail(w, req, kinds, err)
			return
		}
		writeResult(w, op.Status, reflect.ValueOf(out), "NewRevision")
	})
}

// Query registers a GET route that runs a context's query: the path parameters and the query string set its fields
// (unknown parameters are refused), TenantID and ActorID are the principal's. Unless the context authorizes the query
// itself (op.ContextChecks), the route requires op.Action ("<context>:Read" when empty) before it calls. The response
// is the result, with Revision as ETag when it has one.
func Query[Q, R any](r *Routes, kinds Errors, op Op, call func(context.Context, Q) (R, error)) {
	op.input, op.result = reflect.TypeFor[Q](), reflect.TypeFor[R]()
	if op.Method == "" {
		op.Method = http.MethodGet
	}
	if op.Status == 0 {
		op.Status = http.StatusOK
	}
	if op.Action == "" && !op.ContextChecks {
		op.Action = ActionRead
	}
	s := shapeOf(op, op.input)
	params := map[string][]int{}
	for _, f := range fieldsOf(op.input) {
		if !s.server[f.key] {
			params[f.key] = f.index
		}
	}
	r.add(op, func(w http.ResponseWriter, req *http.Request) {
		tenant, actor, ok := r.caller.Who(req.Context())
		if !ok {
			WriteProblem(w, req, http.StatusUnauthorized, "UNAUTHENTICATED", "")
			return
		}
		if !op.ContextChecks {
			if !r.caller.Allowed(req.Context(), op.Context, op.Action, op.Named) {
				WriteProblem(w, req, http.StatusForbidden, "FORBIDDEN", "")
				return
			}
		}
		var in Q
		e := reflect.ValueOf(&in).Elem()
		if !bindPath(w, req, e, s) {
			return
		}
		for name, values := range req.URL.Query() {
			idx, known := params[name]
			if !known || len(values) != 1 {
				detail := fmt.Sprintf("unknown parameter %q", name)
				if known {
					detail = fmt.Sprintf("parameter %q once", name)
				}
				WriteProblem(w, req, http.StatusBadRequest, "MALFORMED_REQUEST", detail)
				return
			}
			if err := setParam(e.FieldByIndex(idx), values[0], name); err != nil {
				WriteProblem(w, req, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
				return
			}
		}
		setString(e, s.tenant, tenant)
		setString(e, s.actor, actor)
		out, err := call(req.Context(), in)
		if err != nil {
			Fail(w, req, kinds, err)
			return
		}
		writeResult(w, http.StatusOK, reflect.ValueOf(out), "Revision")
	})
}

func setString(e reflect.Value, index []int, s string) {
	if index != nil {
		e.FieldByIndex(index).SetString(s)
	}
}

// decodeRequest reads a command's body: application/json, at most MaxBodyBytes, keys snake_case and unique, no
// unknown keys and none the server sets. An empty body is an empty object.
func decodeRequest(w http.ResponseWriter, r *http.Request, v reflect.Value, server map[string]bool) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		WriteProblem(w, r, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "send application/json")
		return false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		WriteProblem(w, r, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", fmt.Sprintf("at most %d bytes", MaxBodyBytes))
		return false
	}
	if err == nil {
		err = checkKeys(raw)
	}
	if err == nil {
		err = decodeBody(raw, v, server)
	}
	if err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "MALFORMED_REQUEST", describe(err))
		return false
	}
	return true
}

func bindPath(w http.ResponseWriter, r *http.Request, e reflect.Value, s shape) bool {
	for name, idx := range s.path {
		if err := setParam(e.FieldByIndex(idx), r.PathValue(name), name); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
			return false
		}
	}
	return true
}

// writeResult writes a result with status, and the int64 field revision as ETag when the result has one. A list is
// written as {"items": [...]}, so that a list can gain a field without breaking its readers.
func writeResult(w http.ResponseWriter, status int, v reflect.Value, revision string) {
	var out any = v.Interface()
	if v.Kind() == reflect.Slice {
		out = struct{ Items any }{out}
	}
	body, err := Encode(out)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if v.Kind() == reflect.Struct {
		if f := v.FieldByName(revision); f.IsValid() && f.Kind() == reflect.Int64 {
			ETag(w, f.Int())
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.Copy(w, bytes.NewReader(append(body, '\n')))
}
