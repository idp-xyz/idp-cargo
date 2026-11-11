// Package api is the published language of BOOK (Carrier Booking): booking commands, CarrierBooking v1, events
// and error kinds. It imports only the standard library and the kernel.
//
// In N1 the operator sends a booking request through the carrier's own channel (portal, e-mail, phone) and
// BOOK records that it was sent and what the carrier answered, each with its evidence (decision N1-BOOK, OD-1
// and OD-2). A request is never a confirmation, and a confirmation is never loading or departure. Each
// confirmation or carrier change writes an immutable capacity version, allocated to the booking's one
// consumer, a consolidation (OD-3). For an air consolidation the confirmation states the MAWB number the
// airline gives (decision N1-PHY OD-7), which AIR takes from here.
package api

import (
	"context"
	"errors"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Booking kinds of the China export slice.
const (
	KindAirConsol             = "AIR_CONSOL"               // an air consolidation's booking with the airline, under one MAWB
	KindSeaLCLConsolEquipment = "SEA_LCL_CONSOL_EQUIPMENT" // the consolidator's container booking with the shipping line
	KindSeaLCLCoLoad          = "SEA_LCL_CO_LOAD"          // LCL space bought from another consolidator (a CO_LOADER in ORG)
	ConsumerConsolidation     = "CONSOL"                   // the consumer of a booking's capacity: a PHY consolidation
)

// Location is a port or airport as UN/LOCODE, an alias carriers use, or for AIR an IATA airport code; BOOK
// resolves it against the location reference data and keeps the UN/LOCODE and, for AIR, the IATA code (F2).
type Location = kernel.LocationCode

// RequestedLeg is a leg the forwarder asks the carrier for: ports, the flight (AIR, such as MU5101) or
// vessel and voyage (SEA) when one is asked for, and the departure date in the port's local calendar.
type RequestedLeg struct {
	Loading       Location
	Discharge     Location
	Flight        string
	Vessel        string
	Voyage        string
	DepartureDate string // YYYY-MM-DD at the port of loading
}

// Equipment is a number of containers of one ISO 6346 size-type, such as 22G1 or 45G1.
type Equipment struct {
	SizeType string
	Count    int64
}

// Capacity is what a booking asks for or a carrier confirms: for AIR pieces, gross weight and volume; for a
// container booking equipment lines; for a co-load gross weight and volume. Never chargeable weight (I-04).
type Capacity struct {
	Pieces      int64
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity
	Equipment   []Equipment
}

// Request is a version of the booking request.
type Request struct {
	ForwarderReference string // the forwarder's own request reference, optional
	Legs               []RequestedLeg
	Capacity           Capacity
	HandlingCodes      []string // IATA or carrier handling codes (DG, PER, VAL...): codes only
}

// DraftCarrierBooking drafts a booking for a consolidation with a carrier. ORG must report the carrier in
// role CARRIER (CO_LOADER for SEA_LCL_CO_LOAD) for the legal entity at the business time; the role decision and
// the party revision are frozen. An AIR carrier needs its IATA designator and AWB prefix in ORG. Rebooks names
// the booking this one replaces, if any. TenantID and ActorID must come from a verified principal; OccurredAt
// lies within the record window before BOOK records the command (T1).
type DraftCarrierBooking struct {
	CommandID       string
	TenantID        string
	ActorID         string
	BookingID       string
	Kind            string
	LegalEntityID   string
	CarrierPartyID  string
	ConsolidationID string
	Rebooks         string
	Request         Request
	OccurredAt      time.Time
}

// AmendBookingRequest writes the next request version of a draft, with a reason.
type AmendBookingRequest struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	Request          Request
	Reason           string
	OccurredAt       time.Time
}

// Channels a request or a carrier answer travels by in N1.
const (
	ChannelCarrierPortal  = "CARRIER_PORTAL"
	ChannelEmail          = "EMAIL"
	ChannelPhone          = "PHONE"
	ChannelCoLoaderPortal = "CO_LOADER_PORTAL"
)

// Evidence references a stored document: the e-mail, PDF or screenshot the operator sent or received. BOOK
// keeps the reference and the SHA-256 of the content, not the content.
type Evidence struct {
	SHA256     string // 64 lower-case hex characters
	StorageID  string
	MediaType  string // such as application/pdf
	CapturedAt time.Time
}

// RecordBookingRequestSent records that the operator sent the current request through the carrier's channel.
// It never confirms anything. In V0.1 a request carries no personal contacts (decision N1-BOOK OD-5, option C
// as the default content).
type RecordBookingRequestSent struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	Channel          string
	Evidence         Evidence
	OccurredAt       time.Time // when it was sent
}

// Carrier reference types.
const (
	ReferenceBookingNumber = "BOOKING_NUMBER"
	ReferenceMAWBNumber    = "MAWB_NUMBER" // PPP-SSSSSSSS: the airline's prefix and a serial with its modulus-7 check digit
	ReferenceCoLoad        = "CO_LOAD_REFERENCE"
	ReferenceRequestAck    = "CARRIER_REQUEST_ACK"
)

// CarrierReference is a reference the carrier states, scoped by the carrier and its type.
type CarrierReference struct {
	Type  string
	Value string
}

// Answer is what the carrier said, as the operator records it: the channel, the evidence (required), the
// carrier's time as stated, its references and, for a refusal, its reason.
type Answer struct {
	Channel     string
	Evidence    Evidence
	CarrierTime time.Time
	References  []CarrierReference
	ReasonCode  string
	ReasonText  string
}

// Cut-off kinds.
const (
	CutOffCargoAcceptance   = "CARGO_ACCEPTANCE" // AIR: latest acceptance
	CutOffDocuments         = "DOCUMENTS"
	CutOffSecurity          = "SECURITY_SCREENING"
	CutOffManifest          = "MANIFEST"
	CutOffCYFullContainer   = "CY_FULL_CONTAINER"
	CutOffSIDocuments       = "SHIPPING_INSTRUCTIONS_DOCUMENTS"
	CutOffVGM               = "VGM"
	CutOffCustomsRelease    = "CUSTOMS_RELEASE"
	CutOffHazardous         = "HAZARDOUS"
	CutOffReceivingCommence = "RECEIVING_COMMENCES"
)

// CutOff is a deadline the carrier set, as an instant with the IANA time zone of the place, such as
// Asia/Shanghai, and the place.
type CutOff struct {
	Kind     string
	At       time.Time
	TimeZone string
	Location Location // zero: the leg's port of loading
}

// ConfirmedLeg is a leg the carrier confirmed: the flight (AIR) or vessel and voyage (SEA), the ports and the
// carrier-stated departure and arrival, which are estimates, never facts of departure (UL-17).
type ConfirmedLeg struct {
	Loading   Location
	Discharge Location
	Flight    string
	Vessel    string
	VesselIMO string
	Voyage    string
	ETD       time.Time
	ETA       time.Time
	CutOffs   []CutOff
}

// CapacityLine is a confirmed line: its legs, from the request's first port to its last, its capacity and
// the carrier's reference for the line where it gives one.
type CapacityLine struct {
	Legs             []ConfirmedLeg
	Capacity         Capacity
	CarrierReference string
}

// Capacity version reasons.
const (
	CapacityInitial             = "INITIAL"
	CapacityPartialConfirmation = "PARTIAL_CONFIRMATION"
	CapacityScheduleChange      = "SCHEDULE_CHANGE"
	CapacityRollover            = "ROLLOVER"
	CapacityCarrierReduced      = "CARRIER_REDUCED"
	CapacitySplit               = "SPLIT"
	CapacityRequestedChange     = "REQUESTED_CHANGE"
)

// RecordCarrierAcknowledgement records that the carrier received the request; no capacity is granted.
type RecordCarrierAcknowledgement struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	Answer           Answer
	OccurredAt       time.Time
}

// RecordCarrierConfirmation records the carrier's confirmation of the current request with its evidence
// (OD-2): capacity version 1, at most what was requested (PARTIAL_CONFIRMATION when less), allocated to the
// booking's consolidation (OD-3). An AIR_CONSOL confirmation states exactly one MAWB number of the airline's
// prefix. A carrier reference is unique among the tenant's live bookings of that carrier and type.
type RecordCarrierConfirmation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	Answer           Answer
	Lines            []CapacityLine
	OccurredAt       time.Time
}

// RecordCarrierRejection records that the carrier refused the request, with its reason; terminal.
type RecordCarrierRejection struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	Answer           Answer
	OccurredAt       time.Time
}

// RecordCarrierChange records a change the carrier made to a confirmed booking (a schedule change, a rollover
// before departure, reduced or split capacity): a new capacity version that supersedes the current one, which
// stays readable. The allocation moves to it.
type RecordCarrierChange struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	Reason           string // CapacityScheduleChange, CapacityRollover, CapacityCarrierReduced or CapacitySplit
	Answer           Answer
	Lines            []CapacityLine
	OccurredAt       time.Time
}

// RequestBookingChange records a changed request sent to the carrier (more or less capacity, another flight
// or vessel), with a reason; the current capacity stays in force until the carrier answers.
type RequestBookingChange struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	Request          Request
	Reason           string
	Channel          string
	Evidence         Evidence
	OccurredAt       time.Time
}

// RecordChangeConfirmed records the carrier's confirmation of a requested change: a new capacity version.
type RecordChangeConfirmed struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	Answer           Answer
	Lines            []CapacityLine
	OccurredAt       time.Time
}

// RecordChangeRefused records that the carrier refused a requested change; the capacity stays as it was.
type RecordChangeRefused struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	Answer           Answer
	OccurredAt       time.Time
}

// RecordCarrierDecline records that the carrier withdrew a confirmed booking; the allocation is released and
// the consumer rebooks. Terminal.
type RecordCarrierDecline struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	Answer           Answer
	OccurredAt       time.Time
}

// RequestBookingCancellation records a cancellation request sent to the carrier, with a reason. Capacity and
// allocation stay until the carrier confirms, because space may still be charged.
type RequestBookingCancellation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	Reason           string
	Channel          string
	Evidence         Evidence
	OccurredAt       time.Time
}

// RecordCancellationConfirmed records the carrier's confirmation of a cancellation; the allocation is
// released. Terminal.
type RecordCancellationConfirmed struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	Answer           Answer
	OccurredAt       time.Time
}

// WithdrawDraftBooking cancels a draft that was never sent, with a reason.
type WithdrawDraftBooking struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// ReassignBookingCapacity moves a confirmed booking's capacity to another consolidation, with a reason (the
// consolidation was replaced). BOOK does not verify the consumer; its owner links itself.
type ReassignBookingCapacity struct {
	CommandID        string
	TenantID         string
	ActorID          string
	BookingID        string
	ExpectedRevision int64
	ConsolidationID  string
	Reason           string
	OccurredAt       time.Time
}

// Receipt confirms a committed command. Replaying the same command returns the same receipt.
type Receipt struct {
	CommandID   string
	BookingID   string
	NewRevision int64
}

// Commands is BOOK's command entry point.
type Commands interface {
	DraftCarrierBooking(ctx context.Context, cmd DraftCarrierBooking) (Receipt, error)
	AmendBookingRequest(ctx context.Context, cmd AmendBookingRequest) (Receipt, error)
	RecordBookingRequestSent(ctx context.Context, cmd RecordBookingRequestSent) (Receipt, error)
	RecordCarrierAcknowledgement(ctx context.Context, cmd RecordCarrierAcknowledgement) (Receipt, error)
	RecordCarrierConfirmation(ctx context.Context, cmd RecordCarrierConfirmation) (Receipt, error)
	RecordCarrierRejection(ctx context.Context, cmd RecordCarrierRejection) (Receipt, error)
	RecordCarrierChange(ctx context.Context, cmd RecordCarrierChange) (Receipt, error)
	RequestBookingChange(ctx context.Context, cmd RequestBookingChange) (Receipt, error)
	RecordChangeConfirmed(ctx context.Context, cmd RecordChangeConfirmed) (Receipt, error)
	RecordChangeRefused(ctx context.Context, cmd RecordChangeRefused) (Receipt, error)
	RecordCarrierDecline(ctx context.Context, cmd RecordCarrierDecline) (Receipt, error)
	RequestBookingCancellation(ctx context.Context, cmd RequestBookingCancellation) (Receipt, error)
	RecordCancellationConfirmed(ctx context.Context, cmd RecordCancellationConfirmed) (Receipt, error)
	WithdrawDraftBooking(ctx context.Context, cmd WithdrawDraftBooking) (Receipt, error)
	ReassignBookingCapacity(ctx context.Context, cmd ReassignBookingCapacity) (Receipt, error)
}

// Actions an Authorizer decides on: the command names above.
const (
	ActionDraftCarrierBooking          = "DraftCarrierBooking"
	ActionAmendBookingRequest          = "AmendBookingRequest"
	ActionRecordBookingRequestSent     = "RecordBookingRequestSent"
	ActionRecordCarrierAcknowledgement = "RecordCarrierAcknowledgement"
	ActionRecordCarrierConfirmation    = "RecordCarrierConfirmation"
	ActionRecordCarrierRejection       = "RecordCarrierRejection"
	ActionRecordCarrierChange          = "RecordCarrierChange"
	ActionRequestBookingChange         = "RequestBookingChange"
	ActionRecordChangeConfirmed        = "RecordChangeConfirmed"
	ActionRecordChangeRefused          = "RecordChangeRefused"
	ActionRecordCarrierDecline         = "RecordCarrierDecline"
	ActionRequestBookingCancellation   = "RequestBookingCancellation"
	ActionRecordCancellationConfirmed  = "RecordCancellationConfirmed"
	ActionWithdrawDraftBooking         = "WithdrawDraftBooking"
	ActionReassignBookingCapacity      = "ReassignBookingCapacity"
)

// Authorizer decides whether a verified actor may perform an action on a booking. The identity and policy
// layer supplies it. The operator who sent a request may also record the carrier's answer; both are recorded
// (OD-6).
type Authorizer interface {
	AuthorizeBookingAction(ctx context.Context, tenantID, actorID, action, bookingID string) error
}

// Error kinds returned by Commands and Queries, wrapped around the original cause.
var (
	ErrUnauthenticated     = errors.New("book: unauthenticated")
	ErrUnauthorized        = errors.New("book: unauthorized")
	ErrNotFound            = errors.New("book: not found")
	ErrConflict            = errors.New("book: conflicts with the stored state")
	ErrIdempotencyConflict = errors.New("book: command id reused with a different payload")
	ErrRejected            = errors.New("book: rejected by business rules")
)
