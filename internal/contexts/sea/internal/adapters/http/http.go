// Package http is SEA's inbound HTTP adapter: it turns requests into SEA's api commands and queries, with the tenant
// and actor of the verified principal, and holds no business rules. Paths are under /v1; JSON is snake_case.
//
// A resource's ETag is the revision its commands expect in If-Match, and it covers everything the resource states.
// An ocean movement's revision does not move when a container visit changes or a carrier fact is recorded, so the
// movement resource states neither: the movement's container visits and its carrier facts are resources of their
// own, and a house's containers are given without the container number, which is the visit's. A container visit's
// revision covers its seals, VGM assertions and events.
package http

import (
	nethttp "net/http"
	"net/url"
	"strconv"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// bc is SEA's name in permissions ("sea:<Action>", "sea:Read").
const bc = "sea"

// Service is what the adapter calls: SEA's commands and queries, and the restricted read of a VGM signatory.
type Service interface {
	api.Commands
	api.Queries
	api.VgmQueries
}

var kinds = httpapi.Errors{Unauthenticated: api.ErrUnauthenticated, Unauthorized: api.ErrUnauthorized, NotFound: api.ErrNotFound,
	Conflict: api.ErrConflict, IdempotencyConflict: api.ErrIdempotencyConflict, Rejected: api.ErrRejected}

// Register registers SEA's routes.
func Register(mux httpapi.Mux, s Service) {
	h := handlers{s}
	mux.HandleFunc("POST /v1/ocean-movements", h.openMovement)
	mux.HandleFunc("GET /v1/ocean-movements", h.findMovement)
	mux.HandleFunc("GET /v1/ocean-movements/{movement}", h.getMovement)
	mux.HandleFunc("POST /v1/ocean-movements/{movement}/link-carrier-booking", h.linkCarrierBooking)
	mux.HandleFunc("POST /v1/ocean-movements/{movement}/master-bills", h.recordMasterBill)
	mux.HandleFunc("POST /v1/ocean-movements/{movement}/house-bills", h.associateHouseBill)
	mux.HandleFunc("POST /v1/ocean-movements/{movement}/house-bills/{shipment}/refresh", h.refreshHouseBill)
	mux.HandleFunc("POST /v1/ocean-movements/{movement}/house-bills/{shipment}/end", h.endHouseBill)
	mux.HandleFunc("POST /v1/ocean-movements/{movement}/finalize-doc-facts", h.finalizeDocFacts)
	mux.HandleFunc("GET /v1/ocean-movements/{movement}/doc-facts", h.getDocFacts)
	mux.HandleFunc("GET /v1/ocean-movements/{movement}/doc-facts/{version}", h.getDocFacts)
	mux.HandleFunc("POST /v1/ocean-movements/{movement}/carrier-facts", h.recordCarrierFact)
	mux.HandleFunc("GET /v1/ocean-movements/{movement}/carrier-facts", h.listCarrierFacts)
	mux.HandleFunc("GET /v1/ocean-movements/{movement}/container-visits", h.listContainerVisits)
	mux.HandleFunc("POST /v1/ocean-movements/{movement}/cancel", h.cancelMovement)

	mux.HandleFunc("POST /v1/hbl-series", h.registerHblSeries)
	mux.HandleFunc("GET /v1/hbl-series/{legal_entity}", h.getHblSeries)
	mux.HandleFunc("GET /v1/house-bills/{legal_entity}/{hbl}", h.findHouseBill)

	mux.HandleFunc("POST /v1/container-visits", h.assignContainer)
	mux.HandleFunc("GET /v1/container-visits", h.findContainerVisit)
	mux.HandleFunc("GET /v1/container-visits/{visit}", h.getContainerVisit)
	mux.HandleFunc("POST /v1/container-visits/{visit}/seals", h.recordSeal)
	mux.HandleFunc("POST /v1/container-visits/{visit}/stuffing-done", h.recordStuffingDone)
	mux.HandleFunc("POST /v1/container-visits/{visit}/release", h.releaseContainer)
	mux.HandleFunc("POST /v1/container-visits/{visit}/vgm-assertions", h.assertVgm)
	mux.HandleFunc("POST /v1/container-visits/{visit}/vgm-response", h.recordVgmResponse)
	mux.HandleFunc("POST /v1/container-visits/{visit}/events", h.recordContainerEvent)
	mux.HandleFunc("POST /v1/container-visits/{visit}/vgm-assertions/{version}/signatory-reads", h.readSignatory)
	mux.HandleFunc("GET /v1/container-visits/{visit}/vgm-assertions/{version}/signatory-reads", h.listSignatoryReads)
}

type handlers struct{ s Service }

// command is what every command request has: its id, the principal, and the business time.
type command struct {
	id         string
	who        identity.Principal
	occurredAt time.Time
}

// occurred is the business time every command's body states. A retry sends it again unchanged: a time the server
// filled in would differ on each attempt, and the retry would be refused as another command under the same
// Idempotency-Key.
type occurred struct {
	OccurredAt *time.Time `json:"occurred_at"`
}

func (o occurred) at() *time.Time { return o.OccurredAt }

// timed is a command's body.
type timed interface{ at() *time.Time }

// begin reads the command id, the principal and the body's business time.
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

// create runs a command that makes a resource: 201 with its receipt and the resource's Location.
func create[B timed](w nethttp.ResponseWriter, r *nethttp.Request, location func(api.Receipt) string, run func(c command, body B) (api.Receipt, error)) {
	var body B
	if !httpapi.Decode(w, r, &body) {
		return
	}
	c, ok := begin(w, r, body.at())
	if !ok {
		return
	}
	rc, err := run(c, body)
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	writeReceipt(w, nethttp.StatusCreated, rc, location(rc))
}

// change runs a command on a resource at the revision If-Match names: 200 with its receipt.
func change[B timed](w nethttp.ResponseWriter, r *nethttp.Request, run func(c command, revision int64, body B) (api.Receipt, error)) {
	var body B
	if !httpapi.Decode(w, r, &body) {
		return
	}
	revision, ok := httpapi.ExpectedRevision(w, r)
	if !ok {
		return
	}
	c, ok := begin(w, r, body.at())
	if !ok {
		return
	}
	rc, err := run(c, revision, body)
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	writeReceipt(w, nethttp.StatusOK, rc, "")
}

// reader authorizes a query ("sea:Read") and answers the principal.
func reader(w nethttp.ResponseWriter, r *nethttp.Request) (identity.Principal, bool) {
	p, err := identity.Require(r.Context(), bc, identity.ActionRead)
	if err != nil {
		httpapi.WriteProblem(w, r, nethttp.StatusForbidden, "FORBIDDEN", "")
		return identity.Principal{}, false
	}
	return p, true
}

// receipt is the response to a command. subject_id is the movement, the container visit or, for an HBL series, the
// legal entity; new_revision is that subject's. The numbers name what the command wrote.
type receipt struct {
	CommandID    string `json:"command_id"`
	SubjectID    string `json:"subject_id"`
	NewRevision  int64  `json:"new_revision"`
	EntryNo      int    `json:"entry_no,omitempty"`
	HBL          string `json:"hbl,omitempty"`
	BillNo       int    `json:"bill_no,omitempty"`
	SealNo       int    `json:"seal_no,omitempty"`
	AssertionNo  int    `json:"assertion_no,omitempty"`
	EventNo      int    `json:"event_no,omitempty"`
	FactNo       int    `json:"fact_no,omitempty"`
	FactsVersion int    `json:"facts_version,omitempty"`
}

// writeReceipt writes a command's receipt with the subject's revision as ETag; location, when given, is the created
// resource.
func writeReceipt(w nethttp.ResponseWriter, status int, rc api.Receipt, location string) {
	subject := rc.MovementID
	if rc.VisitID != "" {
		subject = rc.VisitID
	}
	httpapi.ETag(w, rc.NewRevision)
	if location != "" {
		w.Header().Set("Location", location)
	}
	httpapi.WriteJSON(w, status, receipt{CommandID: rc.CommandID, SubjectID: subject, NewRevision: rc.NewRevision, EntryNo: rc.EntryNo, HBL: rc.HBL,
		BillNo: rc.BillNo, SealNo: rc.SealNo, AssertionNo: rc.AssertionNo, EventNo: rc.EventNo, FactNo: rc.FactNo, FactsVersion: rc.FactsVersion})
}

// positive reads a path value that is a number from 1.
func positive(w nethttp.ResponseWriter, r *nethttp.Request, name string) (int, bool) {
	n, err := strconv.Atoi(r.PathValue(name))
	if err != nil || n < 1 {
		httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", name+" is a number from 1")
		return 0, false
	}
	return n, true
}

// quantity is a weight or a volume with its unit.
type quantity struct {
	Unit   string `json:"unit"`
	Amount int64  `json:"amount"`
}

func (q quantity) kernel() kernel.Quantity {
	return kernel.Quantity{Unit: kernel.UnitCode(q.Unit), Amount: q.Amount}
}

// quantityOf is nil for no quantity.
func quantityOf(q kernel.Quantity) *quantity {
	if q == (kernel.Quantity{}) {
		return nil
	}
	return &quantity{Unit: string(q.Unit), Amount: q.Amount}
}

// port is a UN/LOCODE as a request states it.
func port(code string) kernel.LocationCode {
	return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}
}

func movementPath(id string) string { return "/v1/ocean-movements/" + url.PathEscape(id) }
func visitPath(id string) string    { return "/v1/container-visits/" + url.PathEscape(id) }
