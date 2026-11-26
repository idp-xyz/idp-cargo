// Package http is BOOK's inbound HTTP adapter (since V2.108): each route binds the body or the query string to BOOK's
// own api command or query, with the tenant and actor of the verified principal, and holds no business rules. BOOK
// authorizes its commands itself, before it replays one; the reads are "book:Read". A booking's ETag is its revision.
// A booking is read by people as AIR and SEA read it; the operator records what the carrier answered, as BOOK records
// it (OD-6).
package http

import (
	"net/http"

	"go.idp.xyz/idp-cargo/internal/contexts/book/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
)

const bc = "book"

// Service is what the routes call.
type Service interface {
	api.Commands
	api.Queries
}

// Kinds maps BOOK's error kinds to statuses.
var Kinds = httpapi.Errors{Unauthenticated: api.ErrUnauthenticated, Unauthorized: api.ErrUnauthorized, NotFound: api.ErrNotFound,
	Conflict: api.ErrConflict, IdempotencyConflict: api.ErrIdempotencyConflict, Rejected: api.ErrRejected}

// Register registers BOOK's routes.
func Register(r *httpapi.Routes, s Service) {
	k := Kinds
	cmd := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "POST", Path: path, Context: bc, Calls: calls, Action: calls, Summary: summary}
	}
	read := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "GET", Path: path, Context: bc, Calls: calls, Summary: summary}
	}
	draft := cmd("/v1/bookings", "DraftCarrierBooking", "Draft a booking with a carrier")
	draft.Status = http.StatusCreated
	const b = "/v1/bookings/{booking_id}"
	httpapi.Command(r, k, draft, s.DraftCarrierBooking)
	httpapi.Query(r, k, read(b, "GetCarrierBooking", "A booking: its request, the carrier's answers and the capacity in force"), s.GetCarrierBooking)
	httpapi.Query(r, k, read("/v1/bookings", "ListCapacityForConsumer", "The confirmed bookings whose capacity is allocated to a consolidation"), s.ListCapacityForConsumer)
	httpapi.Query(r, k, read("/v1/booking-references", "FindBookingByCarrierReference", "A booking by the carrier's reference"), s.FindBookingByCarrierReference)
	httpapi.Query(r, k, read("/v1/bookings-awaiting-carrier", "ListBookingsAwaitingCarrier", "Requests sent and not answered"), s.ListBookingsAwaitingCarrier)
	httpapi.Query(r, k, read("/v1/cut-offs", "ListCutOffsDue", "Cut-offs due in a window"), s.ListCutOffsDue)
	httpapi.Command(r, k, cmd(b+"/request", "AmendBookingRequest", "Amend the request before it is sent"), s.AmendBookingRequest)
	httpapi.Command(r, k, cmd(b+"/sent", "RecordBookingRequestSent", "Record the request sent, with its evidence"), s.RecordBookingRequestSent)
	httpapi.Command(r, k, cmd(b+"/acknowledgement", "RecordCarrierAcknowledgement", "Record the carrier's acknowledgement"), s.RecordCarrierAcknowledgement)
	httpapi.Command(r, k, cmd(b+"/confirmation", "RecordCarrierConfirmation", "Record the carrier's confirmation: the capacity lines"), s.RecordCarrierConfirmation)
	httpapi.Command(r, k, cmd(b+"/rejection", "RecordCarrierRejection", "Record the carrier's rejection"), s.RecordCarrierRejection)
	httpapi.Command(r, k, cmd(b+"/carrier-change", "RecordCarrierChange", "Record a change the carrier made: schedule, rollover, reduction, split"), s.RecordCarrierChange)
	httpapi.Command(r, k, cmd(b+"/change-request", "RequestBookingChange", "Request a change of the booking"), s.RequestBookingChange)
	httpapi.Command(r, k, cmd(b+"/change-confirmed", "RecordChangeConfirmed", "Record the carrier's confirmation of a requested change"), s.RecordChangeConfirmed)
	httpapi.Command(r, k, cmd(b+"/change-refused", "RecordChangeRefused", "Record the carrier's refusal of a requested change"), s.RecordChangeRefused)
	httpapi.Command(r, k, cmd(b+"/decline", "RecordCarrierDecline", "Record that the carrier declined the booking"), s.RecordCarrierDecline)
	httpapi.Command(r, k, cmd(b+"/cancellation-request", "RequestBookingCancellation", "Request the cancellation"), s.RequestBookingCancellation)
	httpapi.Command(r, k, cmd(b+"/cancellation-confirmed", "RecordCancellationConfirmed", "Record the carrier's confirmation of the cancellation"), s.RecordCancellationConfirmed)
	httpapi.Command(r, k, cmd(b+"/withdraw", "WithdrawDraftBooking", "Withdraw a draft"), s.WithdrawDraftBooking)
	httpapi.Command(r, k, cmd(b+"/reassign", "ReassignBookingCapacity", "Reassign the capacity to another consolidation"), s.ReassignBookingCapacity)
}
