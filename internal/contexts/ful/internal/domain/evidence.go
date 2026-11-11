package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// FUL V0.4, plan versus actual: what PHY verified of the shipment's cargo (PhysicalLoadEvidence v1,
// relationship 8) is kept next to the plan, never in it (UL-13). FUL records a new version only when PHY's
// facts changed, derives the execution status from it alone, and relays the measures to AIR (decision N1-PHY
// OD-2, option A). FUL never writes physical facts back.

var (
	ErrShipmentCancelled = errors.New("ful: SHIPMENT_CANCELLED: a cancelled shipment records no load evidence")
	ErrDeparted          = errors.New("ful: packs of the shipment have departed; departed quantity is final for planning")
	ErrInvalidEvidence   = errors.New("ful: invalid load evidence")
)

// ExecutionStatus is a shipment's execution status, derived only from PHY's verified evidence; plan versions,
// attachments, bookings and carrier estimates never set it (UL-17).
type ExecutionStatus string

const (
	ExecutionNotStarted        ExecutionStatus = "NOT_STARTED"
	ExecutionPartiallyDeparted ExecutionStatus = "PARTIALLY_DEPARTED"
	ExecutionDeparted          ExecutionStatus = "DEPARTED"
	ExecutionPartiallyArrived  ExecutionStatus = "PARTIALLY_ARRIVED"
	ExecutionArrived           ExecutionStatus = "ARRIVED"
)

func (e ExecutionStatus) valid() bool {
	switch e {
	case ExecutionNotStarted, ExecutionPartiallyDeparted, ExecutionDeparted, ExecutionPartiallyArrived, ExecutionArrived:
		return true
	}
	return false
}

// Security is the screening state of an AIR shipment's cargo as its lots report it: CLEARED when every lot's
// latest result is PASSED or NOT_REQUIRED, FAILED when one failed, PENDING otherwise; empty for other modes or
// before any cargo is received.
type Security string

const (
	SecurityPending Security = "PENDING"
	SecurityCleared Security = "CLEARED"
	SecurityFailed  Security = "FAILED"
)

func (s Security) valid() bool {
	return s == "" || s == SecurityPending || s == SecurityCleared || s == SecurityFailed
}

// Measure bases FUL relays to AIR.
const (
	BasisReceived = "RECEIVED" // the lots' latest measures once every lot completed receipt
	BasisTendered = "TENDERED" // what was tendered in the consolidation, at the lots' latest measures
)

// LoadEvidence is what FUL takes from PhysicalLoadEvidence v1 for a shipment, summed over its lots and over
// the tenders of the consolidation it is attached to. Received gross weight and volume are the lots' latest
// measures; they are zero when not measured, or when lots measured in different units.
type LoadEvidence struct {
	Lots              int
	ReceiptComplete   bool
	Received          CargoScope
	ConsolidationID   string // the consolidation the tendered, departed and arrived pieces belong to
	Tendered          CargoScope
	Departed          int64
	Arrived           int64
	Offloaded         int64
	Security          Security
	OpenDiscrepancies int
}

// Digest is the SHA-256 of the facts, so that the same facts are recorded once.
func (e LoadEvidence) Digest() string {
	blob, _ := json.Marshal(e)
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

func (e LoadEvidence) valid() bool {
	nonNegative := e.Lots >= 0 && e.Received.Packs >= 0 && e.Received.GrossWeight.Amount >= 0 && e.Received.Volume.Amount >= 0 &&
		e.Tendered.Packs >= 0 && e.Tendered.GrossWeight.Amount >= 0 && e.Tendered.Volume.Amount >= 0 && e.Departed >= 0 &&
		e.Arrived >= 0 && e.Offloaded >= 0 && e.OpenDiscrepancies >= 0
	return nonNegative && e.Security.valid() && (e.Lots > 0 || (e.Received == CargoScope{} && !e.ReceiptComplete)) &&
		(e.ConsolidationID == "" || identifier(e.ConsolidationID)) && (e.ConsolidationID != "" || (e.Tendered == CargoScope{} &&
		e.Departed == 0 && e.Arrived == 0 && e.Offloaded == 0))
}

// EvidenceVersion is one immutable version of the load evidence FUL recorded, with the execution status it
// derived and the departed packs final for planning: those departed, up to what the plan held then.
type EvidenceVersion struct {
	No        int
	Digest    string
	Evidence  LoadEvidence
	Execution ExecutionStatus
	Final     int64
	By        string
	At        time.Time
	EnteredAt time.Time
}

// RecordEvidence records PHY's verified evidence of the shipment as a new version when it differs from the
// latest one, and re-derives the execution status. The tendered pieces must belong to the consolidation FUL
// records the shipment attached to. It reports whether a version was added.
func (s *Shipment) RecordEvidence(e LoadEvidence, by string, at time.Time) (bool, error) {
	if s.state.Status == ShipmentCancelled {
		return false, ErrShipmentCancelled
	}
	if !e.valid() {
		return false, ErrInvalidEvidence
	}
	if e.ConsolidationID != "" {
		if a, ok := s.Attachment(StageMain); !ok || a.ConsolidationID != e.ConsolidationID {
			return false, fmt.Errorf("%w: %s", ErrNotAttached, e.ConsolidationID)
		}
	}
	digest := e.Digest()
	if v, ok := s.LatestEvidence(); ok && v.Digest == digest {
		return false, nil
	}
	if err := s.changeable(by, at); err != nil {
		return false, err
	}
	before := s.Execution()
	planned := s.Latest().Scope.Packs
	s.state.Evidence = append(s.state.Evidence, EvidenceVersion{No: len(s.state.Evidence) + 1, Digest: digest, Evidence: e,
		Execution: derive(planned, e), Final: min(e.Departed, planned), By: by, At: at.UTC()})
	execution := s.Execution()
	s.recordEvidence(eventLoadEvidenceRecorded, at, e.ConsolidationID)
	if execution != before {
		s.recordEvidence(eventExecutionStatusChanged, at, e.ConsolidationID)
	}
	return true, nil
}

// derive is the execution status of planned packs with the evidence's departed and arrived pieces, each
// capped at the plan: ARRIVED or PARTIALLY_ARRIVED once pieces arrived, else DEPARTED or PARTIALLY_DEPARTED once
// pieces departed, else NOT_STARTED.
func derive(planned int64, e LoadEvidence) ExecutionStatus {
	departed, arrived := min(e.Departed, planned), min(e.Arrived, planned)
	switch {
	case planned > 0 && arrived == planned:
		return ExecutionArrived
	case arrived > 0:
		return ExecutionPartiallyArrived
	case planned > 0 && departed == planned:
		return ExecutionDeparted
	case departed > 0:
		return ExecutionPartiallyDeparted
	}
	return ExecutionNotStarted
}

// LatestEvidence returns the latest evidence version.
func (s *Shipment) LatestEvidence() (EvidenceVersion, bool) {
	if n := len(s.state.Evidence); n > 0 {
		return s.state.Evidence[n-1], true
	}
	return EvidenceVersion{}, false
}

// Execution returns the execution status, NOT_STARTED until evidence is recorded.
func (s *Shipment) Execution() ExecutionStatus {
	if v, ok := s.LatestEvidence(); ok {
		return v.Execution
	}
	return ExecutionNotStarted
}

// Departed returns the packs PHY's latest evidence shows departed, up to what the plan held when FUL recorded
// it; they are final for planning.
func (s *Shipment) Departed() int64 {
	if v, ok := s.LatestEvidence(); ok {
		return v.Final
	}
	return 0
}

// Measured returns what FUL relays to AIR: what was tendered in the consolidation the shipment is attached to,
// once tendered and weighed; else the received measures once every lot completed receipt; else nothing.
func (s *Shipment) Measured() (string, CargoScope, bool) {
	v, ok := s.LatestEvidence()
	if !ok {
		return "", CargoScope{}, false
	}
	e := v.Evidence
	if a, attached := s.Attachment(StageMain); attached && e.ConsolidationID == a.ConsolidationID && e.Tendered.Packs > 0 &&
		e.Tendered.GrossWeight.Amount > 0 {
		return BasisTendered, e.Tendered, true
	}
	if e.ReceiptComplete && e.Received.Packs > 0 && e.Received.GrossWeight.Amount > 0 {
		return BasisReceived, e.Received, true
	}
	return "", CargoScope{}, false
}

// stillPlannable refuses a change that would take departed packs out of the plan.
func (s *Shipment) stillPlannable(packs int64) error {
	if departed := s.Departed(); packs < departed {
		return fmt.Errorf("%w: %d departed, the plan would keep %d", ErrDeparted, departed, packs)
	}
	return nil
}

func (s *Shipment) recordEvidence(kind string, at time.Time, consolidation string) {
	s.pending = append(s.pending, Event{Type: kind, At: at.UTC(), TenantID: s.state.TenantID, SubjectID: string(s.state.ShipmentID),
		Stage: StageMain, ConsolidationID: consolidation, EvidenceNo: len(s.state.Evidence)})
}

func (s *Shipment) validateEvidence() error {
	for i, v := range s.state.Evidence {
		if v.No != i+1 || len(v.Digest) != 64 || v.Digest != v.Evidence.Digest() || !v.Evidence.valid() || !v.Execution.valid() ||
			v.Final < 0 || v.Final > v.Evidence.Departed ||
			blank(v.By) || v.At.IsZero() || (i > 0 && v.At.Before(s.state.Evidence[i-1].At)) {
			return fmt.Errorf("%w: load evidence %d", ErrInvalidShipment, i+1)
		}
	}
	return nil
}

// AddedEvidence returns the evidence versions recorded since the shipment was restored, for persistence.
func (s *Shipment) AddedEvidence() []EvidenceVersion {
	return append([]EvidenceVersion(nil), s.state.Evidence[s.evidence:]...)
}
