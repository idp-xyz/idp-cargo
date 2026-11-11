package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Booking statuses.
const (
	StatusDraft                 = "DRAFT"
	StatusRequested             = "REQUESTED"
	StatusAcknowledged          = "ACKNOWLEDGED"
	StatusConfirmed             = "CONFIRMED"
	StatusChangeRequested       = "CHANGE_REQUESTED"
	StatusCancellationRequested = "CANCELLATION_REQUESTED"
	StatusRejected              = "REJECTED"
	StatusDeclinedByCarrier     = "DECLINED_BY_CARRIER"
	StatusCancelled             = "CANCELLED"
)

// VerificationOperatorEntered is how a carrier answer is verified in N1: entered by an operator with its
// evidence. Authenticated channels come with electronic booking later.
const VerificationOperatorEntered = "OPERATOR_ENTERED_WITH_EVIDENCE"

// ResolvedLocation is a location as BOOK froze it: UN/LOCODE and, for AIR, the IATA airport code.
type ResolvedLocation struct {
	Code kernel.LocationCode
	IATA string
}

// ResolvedLeg is a requested leg as BOOK froze it.
type ResolvedLeg struct {
	Loading       ResolvedLocation
	Discharge     ResolvedLocation
	Flight        string
	Vessel        string
	Voyage        string
	DepartureDate string
}

// RequestVersion is a version of the booking request.
type RequestVersion struct {
	Version            int
	ForwarderReference string
	Legs               []ResolvedLeg
	Capacity           Capacity
	HandlingCodes      []string
	LocationEdition    string
	Reason             string
	WrittenAt          time.Time
}

// CapacityLeg is a confirmed leg; ETD and ETA are the carrier's estimates.
type CapacityLeg struct {
	Loading   ResolvedLocation
	Discharge ResolvedLocation
	Flight    string
	Vessel    string
	VesselIMO string
	Voyage    string
	ETD       time.Time
	ETA       time.Time // zero when the carrier gave none
	CutOffs   []CutOff
}

// CapacityLineView is a confirmed line.
type CapacityLineView struct {
	Line             int
	Legs             []CapacityLeg
	Capacity         Capacity
	CarrierReference string
}

// CapacityVersion is an immutable capacity version.
type CapacityVersion struct {
	Version         int
	Reason          string
	LocationEdition string
	Lines           []CapacityLineView
	ConfirmedAt     time.Time // business time of the response that granted it
	RecordedAt      time.Time
}

// CarrierBooking is CarrierBooking v1 (relationships 5 and 6, BOOK -> AIR and SEA): the booking with its
// carrier, consumer, current request, current capacity version and live carrier references. It asserts no
// loading, departure or document issuance. AIR links an air consolidation's MAWB and flights from it.
type CarrierBooking struct {
	TenantID              string
	BookingID             string
	Revision              int64
	Kind                  string
	Status                string
	LegalEntityID         string
	CarrierPartyID        string
	CarrierPartyRevision  int64
	CarrierRole           string
	CarrierIATADesignator string // AIR
	CarrierAWBPrefix      string // AIR
	ConsumerKind          string // ConsumerConsolidation
	ConsumerID            string // empty once released
	Rebooks               string
	Request               RequestVersion
	Capacity              CapacityVersion // zero before confirmation
	SupersededCapacity    []int
	References            []CarrierReference // live ones
	MAWBNumber            string             // the live MAWB_NUMBER reference of an AIR_CONSOL booking
	ConfirmationLevel     string
	AwaitingCarrierSince  time.Time // when the pending request or cancellation was sent; zero when nothing is pending
}

// GetCarrierBooking asks for a booking. TenantID must come from a verified principal.
type GetCarrierBooking struct {
	TenantID  string
	BookingID string
}

// ListCapacityForConsumer asks for the confirmed bookings whose capacity is allocated to a consolidation.
type ListCapacityForConsumer struct {
	TenantID        string
	ConsolidationID string
}

// FindBookingByCarrierReference asks which live booking of a carrier holds a reference, such as a MAWB
// number. A reference no live booking holds yields ErrNotFound.
type FindBookingByCarrierReference struct {
	TenantID       string
	CarrierPartyID string
	Type           string
	Value          string
}

// ListBookingsAwaitingCarrier asks for bookings whose request, change or cancellation was sent before
// SentBefore and is not yet answered, oldest first, for the operator's work list. Limit 1 to 200; 0 is 50.
type ListBookingsAwaitingCarrier struct {
	TenantID   string
	SentBefore time.Time
	Limit      int
}

// ListCutOffsDue asks for cut-offs of current capacity versions of bookings still in force that fall in
// [From, Until), earliest first. Limit 1 to 500; 0 is 100.
type ListCutOffsDue struct {
	TenantID string
	From     time.Time
	Until    time.Time
	Limit    int
}

// BookingSummary is a row of a booking list.
type BookingSummary struct {
	BookingID      string
	Kind           string
	Status         string
	CarrierPartyID string
	ConsumerID     string
	PendingSince   time.Time
}

// CutOffDue is a row of ListCutOffsDue.
type CutOffDue struct {
	BookingID       string
	ConsumerID      string
	CapacityVersion int
	Line            int
	Leg             int
	Flight          string
	Voyage          string
	CutOff          CutOff
}

// Queries is BOOK's read side. An unknown booking yields ErrNotFound.
type Queries interface {
	GetCarrierBooking(ctx context.Context, q GetCarrierBooking) (CarrierBooking, error)
	ListCapacityForConsumer(ctx context.Context, q ListCapacityForConsumer) ([]CarrierBooking, error)
	FindBookingByCarrierReference(ctx context.Context, q FindBookingByCarrierReference) (CarrierBooking, error)
	ListBookingsAwaitingCarrier(ctx context.Context, q ListBookingsAwaitingCarrier) ([]BookingSummary, error)
	ListCutOffsDue(ctx context.Context, q ListCutOffsDue) ([]CutOffDue, error)
}
