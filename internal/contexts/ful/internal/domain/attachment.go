package domain

import (
	"errors"
	"fmt"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// StageMain is the main carriage of a shipment's route, the only execution stage of FUL V0.4 in the China
// export slice (N1); pre-carriage and on-forwarding stages come later.
const StageMain = "MAIN"

var (
	ErrConsolPlanMismatch = errors.New("ful: the consolidation does not fit the shipment's plan")
	ErrNotAttached        = errors.New("ful: the shipment is not attached to the consolidation")
)

// AttachmentEnd says why a consol attachment ended, as PHY ended the membership entry behind it.
type AttachmentEnd string

const (
	AttachmentDetached          AttachmentEnd = "DETACHED"
	AttachmentTransferred       AttachmentEnd = "TRANSFERRED"
	AttachmentRefreshed         AttachmentEnd = "REFRESHED" // PHY wrote a new entry in the same consolidation
	AttachmentShipmentCancelled AttachmentEnd = "SHIPMENT_CANCELLED"
)

func (e AttachmentEnd) valid() bool {
	switch e {
	case AttachmentDetached, AttachmentTransferred, AttachmentRefreshed, AttachmentShipmentCancelled:
		return true
	}
	return false
}

// Membership is the live PHY membership entry of a shipment as FUL reads it from ConsolMembership v1
// (relationship 8): the consolidation, the entry and the membership revision, when PHY attached the entry, and
// the consolidation's mode and route.
type Membership struct {
	ConsolidationID string
	EntryNo         int
	Revision        int64
	AttachedAt      time.Time
	Mode            TransportMode
	Origin          kernel.LocationCode
	Destination     kernel.LocationCode
}

// ConsolAttachment (FUL V0.4) records that PHY attached the shipment to a consolidation on a stage: the
// consolidation and the membership entry and revision FUL read. FUL never decides it (decision N1-PHY,
// OD-1: PHY holds the membership and commits first). Rows are immutable but for their end, written once; at
// most one per stage is current.
type ConsolAttachment struct {
	Stage              string
	ConsolidationID    string
	MembershipEntryNo  int
	MembershipRevision int64
	AttachedFrom       time.Time // when PHY attached the entry (business time)
	RecordedBy         string
	RecordedAt         time.Time // FUL's business time
	EnteredAt          time.Time // when FUL recorded it (T1)
	EndedBy            string
	EndedAt            time.Time
	EndReason          AttachmentEnd
	EndEnteredAt       time.Time
}

// Current reports whether the attachment has not ended.
func (a ConsolAttachment) Current() bool { return a.EndedAt.IsZero() }

// RecordConsolAttachment records the live membership entry PHY reports for the shipment. The consolidation
// must have the shipment's mode and route. A current attachment to another consolidation ends as
// TRANSFERRED, one to an earlier entry of the same consolidation as REFRESHED; the same entry twice is
// refused.
func (s *Shipment) RecordConsolAttachment(m Membership, by string, at time.Time) error {
	if s.state.Status == ShipmentCancelled {
		return ErrInvalidTransition
	}
	if !identifier(m.ConsolidationID) || m.EntryNo < 1 || m.Revision < 1 || m.AttachedAt.IsZero() || at.Before(m.AttachedAt) {
		return fmt.Errorf("%w: membership entry", ErrInvalidChange)
	}
	r := s.Latest().Route
	if m.Mode != r.Mode || m.Origin != r.Origin || m.Destination != r.Destination {
		return fmt.Errorf("%w: consolidation %s runs %s %s-%s, the plan %s %s-%s", ErrConsolPlanMismatch, m.ConsolidationID,
			m.Mode, m.Origin.Code, m.Destination.Code, r.Mode, r.Origin.Code, r.Destination.Code)
	}
	if err := s.changeable(by, at); err != nil {
		return err
	}
	if i := s.attached(StageMain); i >= 0 {
		current := s.state.Attachments[i]
		end := AttachmentTransferred
		switch {
		case current.ConsolidationID == m.ConsolidationID && current.MembershipEntryNo == m.EntryNo:
			return fmt.Errorf("%w: entry %d in %s is recorded already", ErrInvalidChange, m.EntryNo, m.ConsolidationID)
		case current.ConsolidationID == m.ConsolidationID:
			end = AttachmentRefreshed
		}
		s.endAttachment(i, end, by, at)
	}
	s.state.Attachments = append(s.state.Attachments, ConsolAttachment{Stage: StageMain, ConsolidationID: m.ConsolidationID,
		MembershipEntryNo: m.EntryNo, MembershipRevision: m.Revision, AttachedFrom: m.AttachedAt.UTC(), RecordedBy: by, RecordedAt: at.UTC()})
	s.recordConsol(eventConsolAttachmentRecorded, at, m.ConsolidationID)
	return nil
}

// RecordConsolDetachment ends the current attachment to a consolidation the shipment has left in PHY, with
// how PHY ended its entry there.
func (s *Shipment) RecordConsolDetachment(consolidation string, end AttachmentEnd, by string, at time.Time) error {
	if s.state.Status == ShipmentCancelled {
		return ErrInvalidTransition
	}
	if !end.valid() || end == AttachmentRefreshed {
		return fmt.Errorf("%w: end %q", ErrInvalidChange, end)
	}
	i := s.attached(StageMain)
	if i < 0 || s.state.Attachments[i].ConsolidationID != consolidation {
		return fmt.Errorf("%w: %s", ErrNotAttached, consolidation)
	}
	if err := s.changeable(by, at); err != nil {
		return err
	}
	s.endAttachment(i, end, by, at)
	s.recordConsol(eventConsolDetachmentRecorded, at, consolidation)
	return nil
}

// Attachment returns the current consol attachment of a stage.
func (s *Shipment) Attachment(stage string) (ConsolAttachment, bool) {
	if i := s.attached(stage); i >= 0 {
		return s.state.Attachments[i], true
	}
	return ConsolAttachment{}, false
}

func (s *Shipment) attached(stage string) int {
	for i, a := range s.state.Attachments {
		if a.Stage == stage && a.Current() {
			return i
		}
	}
	return -1
}

func (s *Shipment) endAttachment(i int, end AttachmentEnd, by string, at time.Time) {
	a := &s.state.Attachments[i]
	a.EndedBy, a.EndedAt, a.EndReason = by, at.UTC(), end
	if i < s.attachments {
		s.endedAttachments = append(s.endedAttachments, i)
	}
}

func (a ConsolAttachment) valid() bool {
	ended := !a.Current()
	return a.Stage == StageMain && identifier(a.ConsolidationID) && a.MembershipEntryNo > 0 && a.MembershipRevision > 0 &&
		!a.AttachedFrom.IsZero() && !blank(a.RecordedBy) && !a.RecordedAt.Before(a.AttachedFrom) &&
		ended != blank(a.EndedBy) && ended == a.EndReason.valid() && (!ended || !a.EndedAt.Before(a.RecordedAt))
}

func (s *Shipment) validateAttachments() error {
	current := map[string]bool{}
	for i, a := range s.state.Attachments {
		if !a.valid() || (a.Current() && (current[a.Stage] || s.state.Status == ShipmentCancelled)) {
			return fmt.Errorf("%w: consol attachment %d", ErrInvalidShipment, i+1)
		}
		current[a.Stage] = current[a.Stage] || a.Current()
	}
	return nil
}

// AddedAttachments returns the attachments recorded since the shipment was restored; EndedAttachments the
// stored ones ended since, by position from 1. Both for persistence.
func (s *Shipment) AddedAttachments() []ConsolAttachment {
	return append([]ConsolAttachment(nil), s.state.Attachments[s.attachments:]...)
}

func (s *Shipment) EndedAttachments() map[int]ConsolAttachment {
	ended := map[int]ConsolAttachment{}
	for _, i := range s.endedAttachments {
		ended[i+1] = s.state.Attachments[i]
	}
	return ended
}

func (s *Shipment) recordConsol(kind string, at time.Time, consolidation string) {
	s.pending = append(s.pending, Event{Type: kind, At: at.UTC(), TenantID: s.state.TenantID, SubjectID: string(s.state.ShipmentID),
		Stage: StageMain, ConsolidationID: consolidation})
}
