package domain

import (
	"fmt"
	"regexp"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// IssuanceAuthority is a legal entity's authority to issue a kind of document at an issue place (OD-10 A): for a
// HAWB the forwarder's standing as consolidator, for a MAWB its agency for the airline. DOC issues only under
// an authority in force (DOC_ISSUANCE_BLOCKED otherwise). Once recorded it changes only by being withdrawn.
type IssuanceAuthority struct {
	TenantID        TenantID
	AuthorityID     string
	LegalEntityID   string
	Kind            Kind
	IssuePlace      string // UN/LOCODE
	AirlinePartyID  string // a MAWB authority's airline
	Qualification   string // the filing, registration or agency agreement reference
	Validity        kernel.ValidityInterval
	EvidenceRef     string
	RecordedBy      string
	RecordedAt      time.Time
	EnteredAt       time.Time
	WithdrawnBy     string
	WithdrawnAt     time.Time
	WithdrawReason  string
	WithdrawEntered time.Time
	Version         int64 // 1 when recorded, 2 once withdrawn
	pending         []Event
}

var locode = regexp.MustCompile(`^[A-Z]{2}[A-Z2-9]{3}$`)

const (
	eventAuthorityRecorded  = "IssuanceAuthorityRecorded.v1"
	eventAuthorityWithdrawn = "IssuanceAuthorityWithdrawn.v1"
)

// RecordIssuanceAuthority records an authority with its evidence. A MAWB authority names its airline.
func RecordIssuanceAuthority(a IssuanceAuthority, by string, at time.Time) (*IssuanceAuthority, error) {
	a.RecordedBy, a.RecordedAt, a.Version = by, at.UTC(), 1
	a.Validity = kernel.ValidityInterval{From: a.Validity.From.UTC(), Until: utc(a.Validity.Until)}
	if err := a.validate(); err != nil {
		return nil, err
	}
	a.pending = []Event{{Type: eventAuthorityRecorded, At: at.UTC(), TenantID: a.TenantID, DocumentID: a.AuthorityID, Kind: string(a.Kind), Version: 1}}
	return &a, nil
}

func (a *IssuanceAuthority) validate() error {
	switch {
	case !identifier(string(a.TenantID)) || !identifier(a.AuthorityID) || !identifier(a.LegalEntityID) || !a.Kind.valid() || !locode.MatchString(a.IssuePlace):
		return fmt.Errorf("%w: authority, legal entity, kind and issue place (UN/LOCODE)", ErrInvalidAuthority)
	case (a.Kind == KindMAWB) != identifier(a.AirlinePartyID):
		return fmt.Errorf("%w: an airline agency names its airline; a house authority names none", ErrInvalidAuthority)
	case !text(a.Qualification) || !text(a.EvidenceRef) || blank(a.RecordedBy) || a.RecordedAt.IsZero() || a.Validity.From.IsZero() ||
		(!a.Validity.Until.IsZero() && !a.Validity.Until.After(a.Validity.From)):
		return fmt.Errorf("%w: qualification, evidence, author and validity", ErrInvalidAuthority)
	case !a.WithdrawnAt.IsZero() && (blank(a.WithdrawnBy) || !text(a.WithdrawReason) || a.WithdrawnAt.Before(a.RecordedAt)):
		return fmt.Errorf("%w: withdrawal", ErrInvalidAuthority)
	}
	return nil
}

// Withdraw ends the authority from an instant on, with a reason.
func (a *IssuanceAuthority) Withdraw(reason, by string, at time.Time) error {
	if !a.WithdrawnAt.IsZero() {
		return ErrInvalidTransition
	}
	if !text(reason) || blank(by) || at.Before(a.RecordedAt) {
		return fmt.Errorf("%w: a withdrawal needs its author, a reason and a time not before the authority was recorded", ErrInvalidAuthority)
	}
	a.WithdrawnBy, a.WithdrawnAt, a.WithdrawReason, a.Version = by, at.UTC(), reason, 2
	a.pending = append(a.pending, Event{Type: eventAuthorityWithdrawn, At: at.UTC(), TenantID: a.TenantID, DocumentID: a.AuthorityID,
		Kind: string(a.Kind), Version: 2})
	return nil
}

// Covers reports whether the authority lets its legal entity issue a document of a kind at a place, for an
// airline, at an instant: in its validity and before any withdrawal.
func (a IssuanceAuthority) Covers(legalEntity string, kind Kind, place, airline string, at time.Time) bool {
	return a.LegalEntityID == legalEntity && a.Kind == kind && a.IssuePlace == place && (kind != KindMAWB || a.AirlinePartyID == airline) &&
		a.Validity.Contains(at) && (a.WithdrawnAt.IsZero() || at.Before(a.WithdrawnAt))
}

// Stamp records when DOC entered the authority or its withdrawal (T1).
func (a *IssuanceAuthority) Stamp(entered time.Time) {
	if a.EnteredAt.IsZero() {
		a.EnteredAt = entered.UTC()
	}
	if !a.WithdrawnAt.IsZero() && a.WithdrawEntered.IsZero() {
		a.WithdrawEntered = entered.UTC()
	}
}

// RestoreIssuanceAuthority rebuilds a stored authority.
func RestoreIssuanceAuthority(a IssuanceAuthority) (*IssuanceAuthority, error) {
	a.pending = nil
	if err := a.validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

func (a *IssuanceAuthority) Pending() []Event { return append([]Event(nil), a.pending...) }
func (a *IssuanceAuthority) Acknowledge()     { a.pending = nil }
