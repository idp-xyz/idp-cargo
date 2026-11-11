package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// ORG V0.7, ContactTransferRegister v1 (decision N1-DOC OD-4 A, privacy decision P5): the tenant's count of the
// distinct persons whose contact details were transferred abroad in a calendar year, kept next to contact consent
// so that DOC and BOOK count a person once and the block holds atomically. Keys identify persons (an ORG contact,
// or a contact frozen on a COM order); no name, phone or e-mail is ever kept here.

var (
	ErrInvalidTransfer    = errors.New("org: invalid contact transfer")
	ErrTransferThreshold  = errors.New("org: TRANSFER_THRESHOLD: the tenant would transfer the contacts of 100,000 persons abroad this year without a standard contract or certification in force")
	ErrTransferAssessment = errors.New("org: TRANSFER_ASSESSMENT_REQUIRED: 1,000,000 persons a year need a security assessment, which Cargo does not support")
)

// TransferState is the register's state for the tenant and year.
type TransferState string

const (
	TransferOK      TransferState = "OK"
	TransferWarning TransferState = "WARNING" // from the warning threshold: time to sign a standard contract or get certified
	TransferBlocked TransferState = "BLOCKED" // from the block threshold without a safeguard in force
)

// TransferThresholds are the counts of persons from which the register warns, blocks without a safeguard, and
// blocks always (Provisions on Promoting and Regulating Cross-border Data Flows, arts. 5, 7, 8; decision P5).
type TransferThresholds struct {
	Warning    int64
	Block      int64
	Assessment int64
}

// DefaultTransferThresholds are 80,000, 100,000 and 1,000,000 persons.
var DefaultTransferThresholds = TransferThresholds{Warning: 80_000, Block: 100_000, Assessment: 1_000_000}

func (t TransferThresholds) Valid() bool {
	return t.Warning > 0 && t.Warning <= t.Block && t.Block < t.Assessment
}

// State is the register's state at a count of persons.
func (t TransferThresholds) State(persons int64, safeguard bool) TransferState {
	switch {
	case persons >= t.Assessment, persons >= t.Block && !safeguard:
		return TransferBlocked
	case persons >= t.Warning:
		return TransferWarning
	}
	return TransferOK
}

// Admit decides whether added new persons may join the persons counted already: below the block threshold, or
// below the assessment threshold with a safeguard in force.
func (t TransferThresholds) Admit(persons, added int64, safeguard bool) (TransferState, error) {
	total := persons + added
	switch {
	case added < 0 || persons < 0:
		return "", ErrInvalidTransfer
	case added > 0 && total >= t.Assessment:
		return "", fmt.Errorf("%w (%d persons)", ErrTransferAssessment, total)
	case added > 0 && total >= t.Block && !safeguard:
		return "", fmt.Errorf("%w (%d persons)", ErrTransferThreshold, total)
	}
	return t.State(total, safeguard), nil
}

// chinaTime is the time zone the tenant's calendar year is counted in.
var chinaTime = time.FixedZone("UTC+8", 8*3600)

// TransferYear is the calendar year, in China time, a transfer at an instant counts in.
func TransferYear(at time.Time) int { return at.In(chinaTime).Year() }

// ValidContactKey reports whether a key identifies a person without carrying personal data: a short token
// without spaces, such as ORG:<contact id> or COM:<order>:<role>:<version>.
func ValidContactKey(k string) bool {
	if len(k) < 3 || len(k) > 256 {
		return false
	}
	for _, r := range k {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return strings.Contains(k, ":")
}

// Safeguard kinds that lift the block (PIPL art. 38).
const (
	SafeguardStandardContract = "STANDARD_CONTRACT"
	SafeguardCertification    = "CERTIFICATION"
)

// TransferSafeguard is a standard contract filed or a certification obtained by the tenant, with its validity.
type TransferSafeguard struct {
	SafeguardID string
	Kind        string
	Reference   string
	Validity    kernel.ValidityInterval
	EvidenceRef string
	RecordedBy  string
	RecordedAt  time.Time
	EnteredAt   time.Time
}

func (s TransferSafeguard) Valid() bool {
	return strings.TrimSpace(s.SafeguardID) != "" && len(s.SafeguardID) <= 128 &&
		(s.Kind == SafeguardStandardContract || s.Kind == SafeguardCertification) && strings.TrimSpace(s.Reference) != "" && len(s.Reference) <= 500 &&
		strings.TrimSpace(s.EvidenceRef) != "" && len(s.EvidenceRef) <= 500 && !s.Validity.From.IsZero() &&
		(s.Validity.Until.IsZero() || s.Validity.Until.After(s.Validity.From)) && strings.TrimSpace(s.RecordedBy) != "" && !s.RecordedAt.IsZero()
}
