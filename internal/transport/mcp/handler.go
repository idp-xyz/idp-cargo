package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	segjson "github.com/segmentio/encoding/json"
)

// maxBody is the largest request body: the gateway's requests are small (the assertion is 8 KiB at most).
const maxBody = 1 << 20

// ErrAssertion is the only thing a refused caller is told.
var ErrAssertion = errors.New("the caller assertion is missing or invalid")

// NewHandler serves server over Streamable HTTP in its stateless form (the only one that speaks 2026-07-28), with JSON
// responses, behind the verifier. It answers POST only.
func NewHandler(server *sdk.Server, v *Verifier, logger *slog.Logger) http.Handler {
	inner := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: maxBody})
	return v.Require(inner, logger)
}

// Require is the middleware in front of the SDK's handler. tools/list and server/discover pass without an assertion
// (the gateway checks the catalogue without a caller); every other method needs one, which is verified against the
// raw body and puts the Caller on the request's context. A request that is not one JSON-RPC call (a batch, a
// body that is not an object or that repeats "method") is refused before it can be read two ways. A refusal answers
// 403 and runs nothing; its reason goes to the log only.
func (v *Verifier) Require(next http.Handler, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body)) // given back to the SDK
		msg, err := splitRequest(body)
		if err != nil {
			logger.Warn("request refused", "reason", err.Error(), "remote", r.RemoteAddr)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if msg.method == "server/discover" || msg.method == "tools/list" {
			next.ServeHTTP(w, r)
			return
		}
		var meta map[string]json.RawMessage
		_ = json.Unmarshal(msg.params["_meta"], &meta)
		var assertion string
		_ = json.Unmarshal(meta[MetaCallerAssertion], &assertion)
		caller, err := v.Verify(assertion, msg.method, msg.params)
		if err != nil {
			logger.Warn("caller assertion refused", "method", msg.method, "reason", err.Error(), "remote", r.RemoteAddr)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.id,
				"error": map[string]any{"code": -32600, "message": ErrAssertion.Error()}})
			return
		}
		if err := sameReading(body, msg); err != nil {
			logger.Warn("request refused", "method", msg.method, "reason", err.Error(), "remote", r.RemoteAddr)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithCaller(r.Context(), caller)))
	})
}

// sameReading reads the body a second time with the JSON library the SDK reads it with (v1.7.0 uses
// github.com/segmentio/encoding/json), into the shape the SDK reads a call into, and refuses a body in which the method,
// the tool's name or its arguments (as canonical JSON) are not what this middleware verified. The digest covers what
// encoding/json read; this is what ties it to what the SDK will run.
func sameReading(body []byte, msg request) error {
	var sdkRead struct {
		Method string `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := segjson.Unmarshal(body, &sdkRead); err != nil {
		return errors.New("the SDK's reader does not take the body: " + err.Error())
	}
	if sdkRead.Method != msg.method {
		return errors.New("the SDK's reader takes another method")
	}
	if msg.method != "tools/call" {
		return nil
	}
	var name string
	_ = json.Unmarshal(msg.params["name"], &name)
	if sdkRead.Params.Name != name {
		return errors.New("the SDK's reader takes another tool name")
	}
	canon := func(raw json.RawMessage) string {
		if len(bytes.TrimSpace(raw)) == 0 {
			raw = json.RawMessage("null")
		}
		c, err := Canonical(raw)
		if err != nil {
			return "!" + err.Error()
		}
		return string(c)
	}
	if canon(sdkRead.Params.Arguments) != canon(msg.params["arguments"]) {
		return errors.New("the SDK's reader takes other arguments")
	}
	return nil
}

type request struct {
	id     json.RawMessage
	method string
	params map[string]json.RawMessage
}

// splitRequest reads the one JSON-RPC call in body. The decision to ask for an assertion hangs on the method, so the
// method must read the same here and in the SDK: a body that repeats a key (noRepeatedKeys), or spells "method" or
// "params" in another case (encoding/json would take them for the same), is refused.
func splitRequest(body []byte) (request, error) {
	if err := noRepeatedKeys(body); err != nil {
		return request{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return request{}, errors.New("not a JSON-RPC call: the body is not one object")
	}
	top := map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return request{}, err
		}
		key, _ := t.(string)
		for _, known := range []string{"jsonrpc", "id", "method", "params"} {
			if key != known && strings.EqualFold(key, known) {
				return request{}, errors.New("a key of the call is spelled in another case")
			}
		}
		var v json.RawMessage
		if err = dec.Decode(&v); err != nil {
			return request{}, err
		}
		top[key] = v
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return request{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return request{}, errors.New("data after the call")
	}
	var out request
	if err := json.Unmarshal(top["method"], &out.method); err != nil || out.method == "" {
		return request{}, errors.New("no method")
	}
	if out.id = top["id"]; len(out.id) == 0 {
		out.id = json.RawMessage("null")
	}
	if raw := top["params"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.params); err != nil {
			return request{}, errors.New("params is not an object")
		}
	}
	return out, nil
}

// maxDepth is how deep the body of a call may nest: a call of the gateway is a few levels deep.
const maxDepth = 64

var errRepeated = errors.New("an object of the body repeats a key")

// noRepeatedKeys refuses a body in which an object, at any depth, holds a key twice. The digest the gateway signed covers
// the params as this server reads them, one value for each key, and the SDK reads them again: two readers that take
// another one of two equal keys would run what the digest does not cover.
func noRepeatedKeys(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	var value func(depth int) error
	value = func(depth int) error {
		if depth > maxDepth {
			return errors.New("the body nests too deep")
		}
		t, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		seen := map[string]bool{}
		for dec.More() {
			if d == '{' {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := k.(string)
				if seen[key] {
					return errRepeated
				}
				seen[key] = true
			}
			if err := value(depth + 1); err != nil {
				return err
			}
		}
		_, err = dec.Token() // the closing brace or bracket
		return err
	}
	return value(0)
}
