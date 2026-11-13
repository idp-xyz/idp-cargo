// Package httpapi holds what the contexts' inbound HTTP adapters share: RFC 9457 problem details, the mapping of a
// context's api error kinds to status codes (rule C5: kinds, never causes), strict JSON bodies, and the command
// headers: Idempotency-Key is the command id, If-Match "n" the expected revision, ETag "n" the revision written.
// The server, routing and middleware are in internal/transport/http; adapters hold no business rules.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	bentoapp "go.idp.xyz/idp-bento-go/application"
)

// MaxBodyBytes is the largest request body read.
const MaxBodyBytes = 1 << 20

// Problem is an RFC 9457 problem detail with the IDP business code and the request's correlation id.
type Problem struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Detail        string `json:"detail,omitempty"`
	Instance      string `json:"instance,omitempty"`
	Code          string `json:"code"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

type correlationKey struct{}

// WithCorrelationID returns a context carrying the request's correlation id.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}

// CorrelationID answers the request's correlation id ("" outside a request).
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

// WriteProblem writes a problem of status with code.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	p := Problem{Type: "urn:idp-cargo:problem:" + strings.ToLower(code), Title: http.StatusText(status), Status: status, Detail: detail,
		Instance: r.URL.Path, Code: code, CorrelationID: CorrelationID(r.Context())}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="idp-cargo"`)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteJSON writes v as JSON with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Errors are a context's api error kinds, which Fail maps to status codes.
type Errors struct {
	Unauthenticated, Unauthorized, NotFound, Conflict, IdempotencyConflict, Rejected error
}

// businessCode is an IDP business code at the start of a sentinel error's text, "<context>: CODE: what" (as in
// "fin: FIN_NOT_FAPIAOABLE: ...").
var businessCode = regexp.MustCompile(`^[a-z]+: ([A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+): `)

// codeOf answers the business code of the first sentinel error (one that wraps nothing) in err's tree that names one.
// Only sentinels, whose text is the code's own, are read: text a wrapper adds may quote what a caller sent.
func codeOf(err error) string {
	switch e := err.(type) {
	case nil:
		return ""
	case interface{ Unwrap() []error }:
		for _, inner := range e.Unwrap() {
			if c := codeOf(inner); c != "" {
				return c
			}
		}
		return ""
	case interface{ Unwrap() error }:
		return codeOf(e.Unwrap())
	}
	if m := businessCode.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	return ""
}

// detail is what a problem says of err: its text, unless it carries PostgreSQL's own message, which is not for
// callers (the cause is logged with the correlation id).
func detail(r *http.Request, err error, fallback string) string {
	if _, fromDatabase := errors.AsType[*pgconn.PgError](err); fromDatabase {
		slog.InfoContext(r.Context(), "database refusal", "correlation_id", CorrelationID(r.Context()), "path", r.URL.Path, "error", err)
		return fallback
	}
	return err.Error()
}

// Fail writes the problem for err: by the context's error kinds, a commit Bento could not
// confirm (503, retry with the same Idempotency-Key), and anything else as an internal error, logged with the
// correlation id and never described to the caller.
func Fail(w http.ResponseWriter, r *http.Request, kinds Errors, err error) {
	is := func(kind error) bool { return kind != nil && errors.Is(err, kind) }
	code := func(fallback string) string {
		if c := codeOf(err); c != "" {
			return c
		}
		return fallback
	}
	switch {
	case is(kinds.Unauthenticated):
		WriteProblem(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "")
	case is(kinds.Unauthorized):
		WriteProblem(w, r, http.StatusForbidden, "FORBIDDEN", "")
	case is(kinds.IdempotencyConflict):
		WriteProblem(w, r, http.StatusConflict, "IDEMPOTENCY_MISMATCH", "the Idempotency-Key was used for another request")
	case is(kinds.NotFound):
		WriteProblem(w, r, http.StatusNotFound, "NOT_FOUND", detail(r, err, "no such resource"))
	case is(kinds.Conflict):
		WriteProblem(w, r, http.StatusConflict, code("VERSION_CONFLICT"), detail(r, err, "changed meanwhile; read it again"))
	case is(kinds.Rejected):
		WriteProblem(w, r, http.StatusUnprocessableEntity, code("BUSINESS_RULE_VIOLATION"), detail(r, err, "refused by a business rule"))
	case errors.Is(err, bentoapp.ErrCommitUncertain):
		w.Header().Set("Retry-After", "1")
		WriteProblem(w, r, http.StatusServiceUnavailable, "COMMIT_UNCERTAIN", "send the request again with the same Idempotency-Key")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		WriteProblem(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "")
	default:
		slog.ErrorContext(r.Context(), "request failed", "correlation_id", CorrelationID(r.Context()), "path", r.URL.Path, "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "INTERNAL", "")
	}
}

// Decode reads the request body into v: application/json, one object whose keys are snake_case and unique in each
// object, no unknown fields, at most MaxBodyBytes. On a bad body it writes the problem and answers false.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
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
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err = d.Decode(v); err == nil && d.More() {
			err = errors.New("send one JSON object")
		}
	}
	if err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "MALFORMED_REQUEST", describe(err))
		return false
	}
	return true
}

var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// checkKeys reads raw as one JSON object whose keys, at every depth, are snake_case and unique in their object:
// encoding/json would take "CODE" for "code" and the last of two equal keys.
func checkKeys(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	t, err := d.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') {
		return errors.New("send one JSON object")
	}
	if err = objectKeys(d); err != nil {
		return err
	}
	if _, err = d.Token(); err != io.EOF {
		return errors.New("send one JSON object")
	}
	return nil
}

// objectKeys reads the rest of an object whose '{' was read.
func objectKeys(d *json.Decoder) error {
	seen := map[string]bool{}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return err
		}
		key, _ := t.(string)
		if !snakeCase.MatchString(key) {
			return fmt.Errorf("field %q: fields are snake_case", key)
		}
		if seen[key] {
			return fmt.Errorf("field %q twice", key)
		}
		seen[key] = true
		if err = value(d); err != nil {
			return err
		}
	}
	_, err := d.Token() // '}'
	return err
}

func value(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch t {
	case json.Delim('{'):
		return objectKeys(d)
	case json.Delim('['):
		for d.More() {
			if err = value(d); err != nil {
				return err
			}
		}
		_, err = d.Token() // ']'
	}
	return err
}

// describe says what is wrong with a body without Go's type names.
func describe(err error) string {
	if e, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		return fmt.Sprintf("field %q has the wrong type", e.Field)
	}
	if e, ok := errors.AsType[*json.SyntaxError](err); ok {
		return fmt.Sprintf("not valid JSON at byte %d", e.Offset)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return "the JSON ends early"
	}
	msg := err.Error()
	if f, ok := strings.CutPrefix(msg, "json: unknown field "); ok {
		return "unknown field " + f
	}
	return msg
}

// commandID: what a context takes as a command id.
var commandID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// CommandID reads Idempotency-Key, the command id: a command sent again with it returns its first receipt, and with
// another body is refused (409 IDEMPOTENCY_MISMATCH). On a missing or malformed key it writes the problem.
func CommandID(w http.ResponseWriter, r *http.Request) (string, bool) {
	k := r.Header.Get("Idempotency-Key")
	switch {
	case k == "":
		WriteProblem(w, r, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "a command carries an Idempotency-Key")
		return "", false
	case !commandID.MatchString(k):
		WriteProblem(w, r, http.StatusBadRequest, "MALFORMED_REQUEST", "an Idempotency-Key is 1 to 128 letters, digits and . _ : -")
		return "", false
	}
	return k, true
}

var revisionTag = regexp.MustCompile(`^"(0|[1-9][0-9]{0,17})"$`)

// ExpectedRevision reads If-Match "n", the revision or version the command expects (428 when it is missing).
func ExpectedRevision(w http.ResponseWriter, r *http.Request) (int64, bool) {
	v := r.Header.Get("If-Match")
	if v == "" {
		WriteProblem(w, r, http.StatusPreconditionRequired, "PRECONDITION_REQUIRED", `a change names the revision it expects in If-Match: "n"`)
		return 0, false
	}
	m := revisionTag.FindStringSubmatch(v)
	if m == nil {
		WriteProblem(w, r, http.StatusBadRequest, "MALFORMED_REQUEST", `If-Match is a revision in quotes: "n"`)
		return 0, false
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n, true
}

// ETag sets the ETag of a revision or version.
func ETag(w http.ResponseWriter, n int64) { w.Header().Set("ETag", `"`+strconv.FormatInt(n, 10)+`"`) }

// Receipt is the response to a command: replaying the command returns it again.
type Receipt struct {
	CommandID   string `json:"command_id"`
	SubjectID   string `json:"subject_id"`
	NewRevision int64  `json:"new_revision"`
}

// WriteReceipt writes a command's receipt with status (201 for a create, 200 otherwise) and its revision as ETag;
// location, when given, is the created resource.
func WriteReceipt(w http.ResponseWriter, status int, rc Receipt, location string) {
	ETag(w, rc.NewRevision)
	if location != "" {
		w.Header().Set("Location", location)
	}
	WriteJSON(w, status, rc)
}
