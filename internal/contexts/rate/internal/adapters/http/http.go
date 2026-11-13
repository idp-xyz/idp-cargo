// Package http is RATE's inbound HTTP adapter: it turns requests into RATE's api commands and queries, with the
// tenant and actor of the verified principal, and holds no business rules. Paths are under /v1; JSON is snake_case.
package http

import (
	"math"
	nethttp "net/http"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// bc is RATE's name in permissions ("rate:<Action>", "rate:Read").
const bc = "rate"

// Service is what the adapter calls: RATE's catalogue, card and rating commands and queries.
type Service interface {
	api.RateCardCommands
	api.RateCardQueries
}

var kinds = httpapi.Errors{Unauthenticated: api.ErrUnauthenticated, Unauthorized: api.ErrUnauthorized, NotFound: api.ErrNotFound,
	Conflict: api.ErrConflict, IdempotencyConflict: api.ErrIdempotencyConflict, Rejected: api.ErrRejected}

// Register registers RATE's routes.
func Register(mux *nethttp.ServeMux, s Service) {
	h := handlers{s}
	mux.HandleFunc("GET /v1/charge-codes", h.listChargeCodes)
	mux.HandleFunc("POST /v1/charge-codes", h.defineChargeCode)
	mux.HandleFunc("GET /v1/charge-codes/{code}", h.getChargeCode)
	mux.HandleFunc("POST /v1/charge-codes/{code}/revise", h.reviseChargeCode)
	mux.HandleFunc("POST /v1/charge-codes/{code}/deactivate", h.deactivateChargeCode)
	mux.HandleFunc("POST /v1/charge-codes/{code}/reactivate", h.reactivateChargeCode)
}

type handlers struct{ s Service }

// command is what every command request has: its id, the principal, and the business time.
type command struct {
	id         string
	who        identity.Principal
	occurredAt time.Time
}

// begin reads the command id, the principal and the body's occurred_at, the business time. A command states its
// business time, and a retry sends it again unchanged: a time the server filled in would differ on each attempt, and
// the retry would be refused as another command under the same Idempotency-Key.
func begin(w nethttp.ResponseWriter, r *nethttp.Request, occurredAt *time.Time) (command, bool) {
	id, ok := httpapi.CommandID(w, r)
	if !ok {
		return command{}, false
	}
	who, ok := identity.PrincipalFrom(r.Context())
	if !ok {
		httpapi.WriteProblem(w, r, nethttp.StatusUnauthorized, "UNAUTHENTICATED", "")
		return command{}, false
	}
	if occurredAt == nil || occurredAt.IsZero() {
		httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "OCCURRED_AT_REQUIRED", "a command states its business time in occurred_at (RFC 3339)")
		return command{}, false
	}
	return command{id: id, who: who, occurredAt: occurredAt.UTC()}, true
}

// expectedVersion reads If-Match as a version.
func expectedVersion(w nethttp.ResponseWriter, r *nethttp.Request) (int, bool) {
	n, ok := httpapi.ExpectedRevision(w, r)
	if ok && n > math.MaxInt32 {
		httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", "If-Match names a version that does not exist")
		return 0, false
	}
	return int(n), ok
}

// reader authorizes a query ("rate:Read") and answers the principal.
func reader(w nethttp.ResponseWriter, r *nethttp.Request) (identity.Principal, bool) {
	p, err := identity.Require(r.Context(), bc, identity.ActionRead)
	if err != nil {
		httpapi.WriteProblem(w, r, nethttp.StatusForbidden, "FORBIDDEN", "")
		return identity.Principal{}, false
	}
	return p, true
}

func receipt(rc api.Receipt) httpapi.Receipt {
	return httpapi.Receipt{CommandID: rc.CommandID, SubjectID: rc.SubjectID, NewRevision: rc.NewRevision}
}
