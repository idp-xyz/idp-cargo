// Package httpapi holds what the contexts' inbound HTTP adapters share: RFC 9457 problem details, the mapping of a
// context's api error kinds to status codes (rule C5: kinds, never causes), strict JSON bodies, and the command
// headers: Idempotency-Key is the command id, If-Match "n" the expected revision, ETag "n" the revision written.
// The server, routing and middleware are in internal/transport/http; adapters hold no business rules.
package httpapi

import (
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

// businessCode finds an IDP business code in an error's text, written "<context>: CODE: what" (as in
// "fin: FIN_NOT_FAPIAOABLE: ...").
var businessCode = regexp.MustCompile(`(?:^|: )([A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+): `)

// Fail writes the problem for err: by the context's error kinds, a commit Bento could not
// confirm (503, retry with the same Idempotency-Key), and anything else as an internal error, logged with the
// correlation id and never described to the caller.
func Fail(w http.ResponseWriter, r *http.Request, kinds Errors, err error) {
	is := func(kind error) bool { return kind != nil && errors.Is(err, kind) }
	code := func(fallback string) string {
		if m := businessCode.FindStringSubmatch(err.Error()); m != nil {
			return m[1]
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
		WriteProblem(w, r, http.StatusNotFound, "NOT_FOUND", err.Error())
	case is(kinds.Conflict):
		WriteProblem(w, r, http.StatusConflict, code("VERSION_CONFLICT"), err.Error())
	case is(kinds.Rejected):
		WriteProblem(w, r, http.StatusUnprocessableEntity, code("BUSINESS_RULE_VIOLATION"), err.Error())
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

// Decode reads the request body into v: application/json, one object, no unknown fields, at most MaxBodyBytes. On a
// bad body it writes the problem and answers false.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		WriteProblem(w, r, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "send application/json")
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err == nil {
		if err = d.Decode(&struct{}{}); err == io.EOF {
			return true
		}
		err = errors.New("one JSON object only")
	}
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		WriteProblem(w, r, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", fmt.Sprintf("at most %d bytes", MaxBodyBytes))
		return false
	}
	WriteProblem(w, r, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
	return false
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

// ExpectedRevision reads If-Match "n", the revision or version the command expects (428 when it is missing).
func ExpectedRevision(w http.ResponseWriter, r *http.Request) (int64, bool) {
	v := r.Header.Get("If-Match")
	if v == "" {
		WriteProblem(w, r, http.StatusPreconditionRequired, "PRECONDITION_REQUIRED", `a change names the revision it expects in If-Match: "n"`)
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(v, `"`), `"`), 10, 64)
	if err != nil || n < 0 || !strings.HasPrefix(v, `"`) || !strings.HasSuffix(v, `"`) || len(v) < 3 {
		WriteProblem(w, r, http.StatusBadRequest, "MALFORMED_REQUEST", `If-Match is a revision in quotes: "n"`)
		return 0, false
	}
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
