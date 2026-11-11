package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// ContactTransferRegister v1 (ORG V0.7; decision N1-DOC OD-4 A, privacy decision P5): the tenant's count of the
// distinct persons whose contact details DOC and BOOK transfer abroad in a calendar year (China time), with the
// warning from 80,000 persons and the block from 100,000 without a standard contract or certification in force.
// Keys identify persons without personal data: ORG:<contact id> for an ORG contact, or COM:<order>:<role>:<version>
// for a contact frozen on an order. The API client reserves a dispatch's cross-border contacts here before sending
// (T6): the reservation is the atomic gate; a failed dispatch's reservation is released.

// Register states.
const (
	TransferOK      = "OK"
	TransferWarning = "WARNING"
	TransferBlocked = "BLOCKED"
)

// Safeguard kinds that lift the block (PIPL art. 38).
const (
	SafeguardStandardContract = "STANDARD_CONTRACT"
	SafeguardCertification    = "CERTIFICATION"
)

// Actions an Authorizer decides on (AuthorizePartyAction); the subject is the dispatch or the safeguard.
const (
	ActionReserveContactTransfers = "ReserveContactTransfers"
	ActionReleaseContactTransfers = "ReleaseContactTransfers"
	ActionRecordTransferSafeguard = "RecordTransferSafeguard"
)

// ContactTransfersReserved, ContactTransfersReleased and TransferSafeguardRecorded name the register year as
// contact-transfer-register:<year> (or :safeguards), the dispatch or safeguard as the entry and the persons counted
// as the entry version; never a key.
const (
	ContactTransfersReserved  = "ContactTransfersReserved.v1"
	ContactTransfersReleased  = "ContactTransfersReleased.v1"
	TransferSafeguardRecorded = "TransferSafeguardRecorded.v1"
)

// ReserveContactTransfers counts a dispatch's cross-border contact keys in the register of the year ORG records it
// in (China time), with the safeguard in force then; a key counted already that year is not counted again. Refused (ErrRejected) when the persons would reach
// 100,000 without a safeguard in force, or 1,000,000 at all, and when the dispatch holds a reservation already.
type ReserveContactTransfers struct {
	CommandID   string
	TenantID    string
	ActorID     string
	DispatchID  string
	ContactKeys []string
	OccurredAt  time.Time
}

// ReleaseContactTransfers releases the reservation of a dispatch that was not sent.
type ReleaseContactTransfers struct {
	CommandID  string
	TenantID   string
	ActorID    string
	DispatchID string
	OccurredAt time.Time
}

// RecordTransferSafeguard records a standard contract filed or a certification obtained, with its reference,
// validity and evidence.
type RecordTransferSafeguard struct {
	CommandID   string
	TenantID    string
	ActorID     string
	SafeguardID string
	Kind        string
	Reference   string
	Validity    kernel.ValidityInterval
	EvidenceRef string
	OccurredAt  time.Time
}

// TransferReceipt confirms a register command: the year, the persons counted after it and the state.
type TransferReceipt struct {
	CommandID   string
	Year        int
	Persons     int64
	State       string
	NewRevision int64
}

// GetContactTransferRegister asks for a year's register (0: the current year); DispatchID, if given, asks which
// keys that dispatch holds in force, in whichever year they count.
type GetContactTransferRegister struct {
	TenantID   string
	Year       int
	DispatchID string
}

// TransferSafeguard is a safeguard in force.
type TransferSafeguard struct {
	SafeguardID string
	Kind        string
	Reference   string
	Validity    kernel.ValidityInterval
}

// ContactTransferRegister is a year's register.
type ContactTransferRegister struct {
	Year         int
	Revision     int64
	Persons      int64
	State        string
	Safeguard    *TransferSafeguard
	DispatchKeys []string
}

type ContactTransferRegisterCommands interface {
	ReserveContactTransfers(ctx context.Context, cmd ReserveContactTransfers) (TransferReceipt, error)
	ReleaseContactTransfers(ctx context.Context, cmd ReleaseContactTransfers) (TransferReceipt, error)
	RecordTransferSafeguard(ctx context.Context, cmd RecordTransferSafeguard) (TransferReceipt, error)
}

type ContactTransferRegisterQueries interface {
	GetContactTransferRegister(ctx context.Context, q GetContactTransferRegister) (ContactTransferRegister, error)
}
