// Package api is the published language of AIR (Air Freight) V0.1: the air movement of an air
// consolidation, its master air waybill (MAWB) number and flights taken from the airline's booking confirmation,
// and the house air waybill (HAWB) of every house. It imports only the standard library and the kernel.
//
// One air movement per air consolidation holds the MAWB and the flights (CargoWise: the consol's master and
// routing); a flight change confirmed under the same MAWB moves the whole movement and leaves the houses as they
// are, while another airline or another MAWB is a new movement. The MAWB number and flights come only from
// BOOK's CarrierBooking v1 (decision N1-PHY OD-7). A house is a FUL shipment attached to the consolidation, as FUL
// reports it in AirOperationPlan v1 (OD-2); AIR draws its HAWB number from the operating legal entity's series
// when it associates the house, and DOC prints it (decision N1-HAWB). Issuing the waybills stays with DOC.
package api

import (
	"context"
	"errors"
	"time"
)

// OpenAirMovement opens the air movement of an air consolidation from its booking: BOOK must report the booking
// AIR_CONSOL, in force with confirmed capacity, allocated to the consolidation and holding a MAWB number; the
// movement takes the airline, the operating legal entity, the route, the MAWB and the flights from it. One open
// movement per consolidation; a MAWB is assigned to one movement of the tenant. TenantID and ActorID must come
// from a verified principal; OccurredAt lies within the record window before AIR records the command (T1).
type OpenAirMovement struct {
	CommandID       string
	TenantID        string
	ActorID         string
	MovementID      string
	ConsolidationID string
	BookingID       string
	OccurredAt      time.Time
}

// LinkCarrierBooking takes a newer capacity version of the movement's booking (a schedule change confirmed by
// the airline under the same MAWB): the flights are replaced, the old ones kept as superseded; houses stay.
type LinkCarrierBooking struct {
	CommandID        string
	TenantID         string
	ActorID          string
	MovementID       string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// RegisterHawbSeries gives a legal entity its HAWB series: a prefix of one to four letters and digits followed
// by a zero-padded sequence of Width digits, from Next. One series per legal entity; prefix and width make
// numbers of at most twelve characters.
type RegisterHawbSeries struct {
	CommandID     string
	TenantID      string
	ActorID       string
	LegalEntityID string
	Prefix        string
	Width         int
	Next          int64
	OccurredAt    time.Time
}

// AssociateHouseWaybill associates a house with the movement. FUL's AirOperationPlan v1 must report the
// shipment PLANNED by AIR, attached to the movement's consolidation, on its route (OD-8) and of its operating
// legal entity (OD-10). The house keeps the HAWB number it had under the legal entity; otherwise HAWB is the
// number given, or empty to draw the next one from the entity's series. Its planned packs, gross weight and
// volume and its documented chargeable weight are frozen; PHY's measures replace them once relayed.
type AssociateHouseWaybill struct {
	CommandID        string
	TenantID         string
	ActorID          string
	MovementID       string
	ExpectedRevision int64
	ShipmentID       string
	HAWB             string
	OccurredAt       time.Time
}

// RefreshHouseWaybillFacts takes a newer FUL plan of a house (a shortload, a changed part) into a new entry.
type RefreshHouseWaybillFacts struct {
	CommandID        string
	TenantID         string
	ActorID          string
	MovementID       string
	ExpectedRevision int64
	ShipmentID       string
	OccurredAt       time.Time
}

// EndHouseWaybillAssociation ends a house's association once FUL reports the shipment no longer attached to the
// consolidation (detached or transferred) or cancelled. The HAWB number stays the shipment's and moves with it.
type EndHouseWaybillAssociation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	MovementID       string
	ExpectedRevision int64
	ShipmentID       string
	OccurredAt       time.Time
}

// CancelAirMovement cancels an open movement without houses, with a reason; its MAWB number is voided.
type CancelAirMovement struct {
	CommandID        string
	TenantID         string
	ActorID          string
	MovementID       string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// Receipt confirms a committed command. Replaying the same command returns the same receipt. House commands also
// name the house entry and its HAWB; RegisterHawbSeries names the legal entity in MovementID's place.
type Receipt struct {
	CommandID   string
	MovementID  string
	NewRevision int64
	EntryNo     int
	HAWB        string
}

// Commands is AIR's command entry point.
type Commands interface {
	OpenAirMovement(ctx context.Context, cmd OpenAirMovement) (Receipt, error)
	LinkCarrierBooking(ctx context.Context, cmd LinkCarrierBooking) (Receipt, error)
	RegisterHawbSeries(ctx context.Context, cmd RegisterHawbSeries) (Receipt, error)
	AssociateHouseWaybill(ctx context.Context, cmd AssociateHouseWaybill) (Receipt, error)
	RefreshHouseWaybillFacts(ctx context.Context, cmd RefreshHouseWaybillFacts) (Receipt, error)
	EndHouseWaybillAssociation(ctx context.Context, cmd EndHouseWaybillAssociation) (Receipt, error)
	CancelAirMovement(ctx context.Context, cmd CancelAirMovement) (Receipt, error)
}

// Actions an Authorizer decides on: the command names.
const (
	ActionOpenAirMovement            = "OpenAirMovement"
	ActionLinkCarrierBooking         = "LinkCarrierBooking"
	ActionRegisterHawbSeries         = "RegisterHawbSeries" // subject: the legal entity
	ActionAssociateHouseWaybill      = "AssociateHouseWaybill"
	ActionRefreshHouseWaybillFacts   = "RefreshHouseWaybillFacts"
	ActionEndHouseWaybillAssociation = "EndHouseWaybillAssociation"
	ActionCancelAirMovement          = "CancelAirMovement"
)

// Authorizer decides whether a verified actor may perform an action on a movement (or, for a series, a legal
// entity).
type Authorizer interface {
	AuthorizeAirAction(ctx context.Context, tenantID, actorID, action, subjectID string) error
}

// Error kinds returned by Commands and Queries, wrapped around the original cause.
var (
	ErrUnauthenticated     = errors.New("air: unauthenticated")
	ErrUnauthorized        = errors.New("air: unauthorized")
	ErrNotFound            = errors.New("air: not found")
	ErrConflict            = errors.New("air: conflicts with the stored state")
	ErrIdempotencyConflict = errors.New("air: command id reused with a different payload")
	ErrRejected            = errors.New("air: rejected by business rules")
)
