// Package http is AIR's inbound HTTP adapter (since V2.108): each route binds the body or the query string to AIR's own
// api command or query, with the tenant and actor of the verified principal, and holds no business rules. AIR
// authorizes its commands itself, before it replays one; the reads are "air:Read". A movement's ETag is its revision.
// After FUL relays a house's new facts, or reports it detached, the client refreshes or ends the house here (T6).
package http

import (
	"net/http"

	"go.idp.xyz/idp-cargo/internal/contexts/air/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
)

const bc = "air"

// Service is what the routes call.
type Service interface {
	api.Commands
	api.Queries
	api.AirDocFactsQueries
}

// Kinds maps AIR's error kinds to statuses.
var Kinds = httpapi.Errors{Unauthenticated: api.ErrUnauthenticated, Unauthorized: api.ErrUnauthorized, NotFound: api.ErrNotFound,
	Conflict: api.ErrConflict, IdempotencyConflict: api.ErrIdempotencyConflict, Rejected: api.ErrRejected}

// Register registers AIR's routes.
func Register(r *httpapi.Routes, s Service) {
	k := Kinds
	cmd := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "POST", Path: path, Context: bc, Calls: calls, Action: calls, Summary: summary}
	}
	read := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "GET", Path: path, Context: bc, Calls: calls, Summary: summary}
	}
	created := func(o httpapi.Op) httpapi.Op { o.Status = http.StatusCreated; return o }
	const m = "/v1/air-movements/{movement_id}"
	httpapi.Command(r, k, created(cmd("/v1/air-movements", "OpenAirMovement", "Open an air movement from a confirmed booking")), s.OpenAirMovement)
	httpapi.Query(r, k, read(m, "GetAirMovement", "An air movement: its flights, houses and MAWB"), s.GetAirMovement)
	httpapi.Query(r, k, read("/v1/air-movements", "FindAirMovementByMAWB", "A movement by its MAWB number (?mawb=)"), s.FindAirMovementByMAWB)
	httpapi.Command(r, k, cmd(m+"/link-carrier-booking", "LinkCarrierBooking", "Take the booking's newer capacity"), s.LinkCarrierBooking)
	httpapi.Command(r, k, cmd(m+"/house-waybills", "AssociateHouseWaybill", "Associate a house and draw its HAWB"), s.AssociateHouseWaybill)
	httpapi.Command(r, k, cmd(m+"/house-waybills/{shipment_id}/refresh", "RefreshHouseWaybillFacts", "Refresh a house from FUL's latest plan"), s.RefreshHouseWaybillFacts)
	httpapi.Command(r, k, cmd(m+"/house-waybills/{shipment_id}/end", "EndHouseWaybillAssociation", "End a house FUL reports detached"), s.EndHouseWaybillAssociation)
	httpapi.Command(r, k, cmd(m+"/finalize-doc-facts", "FinalizeAirDocFacts", "Finalize the facts DOC issues from"), s.FinalizeAirDocFacts)
	httpapi.Query(r, k, read(m+"/doc-facts/{version}", "GetAirDocFacts", "A version of the facts (0: the latest)"), s.GetAirDocFacts)
	httpapi.Command(r, k, cmd(m+"/cancel", "CancelAirMovement", "Cancel the movement"), s.CancelAirMovement)
	httpapi.Command(r, k, cmd("/v1/hawb-series/{legal_entity_id}", "RegisterHawbSeries", "Register a legal entity's HAWB series"), s.RegisterHawbSeries)
	httpapi.Query(r, k, read("/v1/hawb-series/{legal_entity_id}", "GetHawbSeries", "A legal entity's HAWB series"), s.GetHawbSeries)
	httpapi.Query(r, k, read("/v1/house-waybills/{legal_entity_id}/{hawb}", "FindHouseWaybill", "Where a HAWB number lives"), s.FindHouseWaybill)
}
