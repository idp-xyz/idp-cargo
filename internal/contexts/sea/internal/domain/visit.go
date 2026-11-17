package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// VisitID identifies a container visit: one use of one container for one ocean movement. The same equipment number
// recurs in later visits.
type VisitID string

var (
	ErrInvalidVisit     = errors.New("sea: invalid container visit")
	ErrInvalidContainer = errors.New("sea: a container number has four letters, the fourth U, J or Z, six digits and the ISO 6346 check digit")
	ErrInvalidSeal      = errors.New("sea: invalid seal")
	ErrNotStuffed       = errors.New("sea: no live house of the movement carries one sealed stuffing of the visit's load unit, as FUL relays it")
	ErrStuffingDiffers  = errors.New("sea: PHY's stuffing of the unit differs from the container")
	ErrCargoInContainer = errors.New("sea: the container holds cargo")
)

// VisitStatus is a container visit's state. ASSIGNED: the container is assigned to a planned load unit of the
// movement's consolidation. STUFFED: PHY's stuffing of the unit is sealed, as FUL relays it. RELEASED: the container
// was given back unused. VGM_SUBMITTED, GATED_IN and LOADED come with the VGM and the container events.
type VisitStatus string

const (
	VisitAssigned VisitStatus = "ASSIGNED"
	VisitStuffed  VisitStatus = "STUFFED"
	VisitReleased VisitStatus = "RELEASED"
)

// Seal parties (CargoWise: SealPartyType) and plate sources.
const (
	SealCarrier = "CARRIER"
	SealShipper = "SHIPPER"
	SealCustoms = "CUSTOMS"

	PlateCSC     = "CSC_PLATE"
	PlateCarrier = "CARRIER"
)

var (
	containerForm = regexp.MustCompile(`^[A-Z]{3}[UJZ][0-9]{7}$`)
	sealForm      = regexp.MustCompile(`^[0-9A-Z]+([ /-][0-9A-Z]+)*$`)
)

// ValidContainerNumber reports whether a container number is of ISO 6346: an owner code of three letters, the
// equipment category U, J or Z, six digits and the check digit. Each letter stands for a number from 10 to 38 that
// leaves out the multiples of 11; each of the first ten characters is weighted by a power of two, from 1 for the
// first to 512 for the tenth; the check digit is the sum modulo 11, and 0 where that is 10.
func ValidContainerNumber(s string) bool {
	if !containerForm.MatchString(s) {
		return false
	}
	sum := 0
	for i := range 10 {
		v := int(s[i] - '0')
		if i < 4 {
			v = int(s[i]-'A') + 10
			v += (v - 1) / 10 // leaves out 11, 22 and 33
		}
		sum += v << i
	}
	return sum%11%10 == int(s[10]-'0')
}

// Plate is a container's own tare and maximum gross mass in kilograms, from its CSC plate or as the carrier states
// them; the load unit's limits in PHY are its type's defaults.
type Plate struct {
	TareKg     int64
	MaxGrossKg int64
	Source     string // PlateCSC or PlateCarrier
}

func (p Plate) valid() bool {
	if p == (Plate{}) {
		return true
	}
	return p.TareKg > 0 && p.MaxGrossKg > p.TareKg && (p.Source == PlateCSC || p.Source == PlateCarrier)
}

// VisitSeal is a seal applied to the container. A further seal replaces the current one, which stays as history.
type VisitSeal struct {
	No        int
	Number    string
	Party     string // SealCarrier, SealShipper or SealCustoms
	AppliedAt time.Time
	Source    string // where the seal was read from: a tally sheet, a photograph, a customs notice
	By        string
	At        time.Time
	EnteredAt time.Time
}

func (s VisitSeal) valid() bool {
	return len(s.Number) <= 35 && sealForm.MatchString(s.Number) && (s.Party == SealCarrier || s.Party == SealShipper || s.Party == SealCustoms) &&
		!s.AppliedAt.IsZero() && text(s.Source) && !blank(s.By) && !s.At.IsZero() && !s.AppliedAt.After(s.At)
}

// VisitState is a stored container visit, for persistence.
type VisitState struct {
	TenantID        TenantID
	VisitID         VisitID
	MovementID      MovementID
	ConsolidationID string
	LoadUnitNo      int // PHY's planned load unit of the consolidation the container is assigned to
	Number          string
	SizeType        string
	Plate           Plate
	Status          VisitStatus
	Seals           []VisitSeal
	ExecutionID     string // PHY's stuffing of the unit, once STUFFED
	// DifferenceReason is why the container was taken as stuffed although PHY's seal of the stuffing is none of the
	// visit's seals or PHY's size-type of the unit is not the visit's; empty without a difference.
	DifferenceReason string
	StuffedBy        string
	StuffedAt        time.Time
	ReleaseReason    string
	ReleasedBy       string
	ReleasedAt       time.Time
	AssignedBy       string
	AssignedAt       time.Time
	EnteredAt        time.Time
}

// Visit is the aggregate root (ContainerVisit). Seals are only added.
type Visit struct {
	state   VisitState
	seals   int
	pending []Event
}

const (
	eventContainerAssigned = "ContainerAssigned.v1"
	eventSealRecorded      = "SealRecorded.v1"
	eventContainerEvent    = "ContainerEventRecorded.v1"
	eventContainerReleased = "ContainerReleased.v1"

	// ContainerStuffedEvent is the kind of the container event a stuffing done records.
	ContainerStuffedEvent = "STUFFED"
)

// AssignContainer assigns a container to a planned load unit of an OPEN movement of the consolidator's own
// container (SEA_LCL_OWN; a co-loader's container is the co-loader's), by its ISO 6346 number and size-type, with
// its plate data when known. The visit has its own history: it starts not before the movement was opened, whatever
// was recorded on the movement since.
func AssignContainer(id VisitID, m *Movement, unit int, number, sizeType string, plate Plate, by string, at time.Time) (*Visit, error) {
	if !identifier(string(id)) || m == nil || blank(by) || at.IsZero() {
		return nil, ErrInvalidVisit
	}
	st := m.State()
	switch {
	case st.State != StateOpen:
		return nil, ErrInvalidTransition
	case st.Kind != KindOwn:
		return nil, fmt.Errorf("%w: containers are assigned on a %s movement only; a co-load's container is the co-loader's", ErrInvalidVisit, KindOwn)
	case !ValidContainerNumber(number):
		return nil, ErrInvalidContainer
	case unit < 1 || !sizeTypeForm.MatchString(sizeType) || !plate.valid():
		return nil, fmt.Errorf("%w: a load unit number, an ISO 6346 size-type and, when given, a tare below the maximum gross with its source", ErrInvalidVisit)
	case at.Before(m.openedAt()):
		return nil, fmt.Errorf("%w: a container is assigned not before the movement was opened", ErrInvalidChange)
	}
	v := &Visit{state: VisitState{TenantID: st.TenantID, VisitID: id, MovementID: st.MovementID, ConsolidationID: st.ConsolidationID, LoadUnitNo: unit,
		Number: number, SizeType: sizeType, Plate: plate, Status: VisitAssigned, AssignedBy: by, AssignedAt: at.UTC()}}
	v.record(eventContainerAssigned, at, 0, "")
	return v, nil
}

// RecordSeal records a seal applied to the container: its number, the party that applied it, when, and where it
// was read from. It replaces the current seal, which stays as history; the same number again is refused.
func (v *Visit) RecordSeal(number, party string, appliedAt time.Time, source, by string, at time.Time) error {
	if v.state.Status == VisitReleased {
		return ErrInvalidTransition
	}
	s := VisitSeal{No: len(v.state.Seals) + 1, Number: number, Party: party, AppliedAt: utc(appliedAt), Source: source, By: by, At: at.UTC()}
	switch {
	case !s.valid():
		return fmt.Errorf("%w: a number of capital letters and digits with single spaces, hyphens or slashes inside (35 characters at most), the party "+
			"CARRIER, SHIPPER or CUSTOMS, when it was applied (not after the business time) and its source", ErrInvalidSeal)
	case len(v.state.Seals) > 0 && v.Seal().Number == number:
		return fmt.Errorf("%w: %s is the container's seal", ErrInvalidChange, number)
	}
	if err := v.changeable(by, at); err != nil {
		return err
	}
	v.state.Seals = append(v.state.Seals, s)
	v.record(eventSealRecorded, at, s.No, "")
	return nil
}

// StuffingDone records that the container is stuffed and sealed: a seal is recorded, and the movement's live houses
// carry, as FUL relays it from PHY (OD-2), one sealed stuffing of the visit's load unit. The visit keeps PHY's
// stuffing.
//
// PHY keeps the seal it closed the stuffing with as free text, and the size-type of the unit it planned; neither is
// corrected there once the container is sealed. When that seal is none of the visit's seals (compared by their
// letters and digits, whatever the case and the separators) or that size-type is not the visit's, the container is
// taken as stuffed only with a reason, which the visit keeps; a container assigned to the wrong unit is released
// and assigned again instead. Without a difference no reason is taken.
func (v *Visit) StuffingDone(m *Movement, reason, by string, at time.Time) error {
	if v.state.Status != VisitAssigned {
		return ErrInvalidTransition
	}
	if m == nil || m.ID() != v.state.MovementID || m.state.TenantID != v.state.TenantID {
		return ErrInvalidVisit
	}
	if len(v.state.Seals) == 0 {
		return fmt.Errorf("%w: the container has no seal recorded", ErrNotStuffed)
	}
	s, err := v.stuffing(m)
	if err != nil {
		return err
	}
	differs := v.differences(s)
	switch {
	case len(differs) > 0 && !text(reason):
		return fmt.Errorf("%w: %s; state the reason, or release the container and assign it to its unit", ErrStuffingDiffers, strings.Join(differs, "; "))
	case len(differs) == 0 && reason != "":
		return fmt.Errorf("%w: a reason is taken only when PHY's seal or size-type differs from the container's", ErrInvalidChange)
	case at.Before(s.SealedAt):
		return fmt.Errorf("%w: stuffing is done not before PHY sealed the container", ErrInvalidChange)
	}
	if err = v.changeable(by, at); err != nil {
		return err
	}
	v.state.Status, v.state.ExecutionID, v.state.DifferenceReason, v.state.StuffedBy, v.state.StuffedAt = VisitStuffed, s.ExecutionID, reason, by, at.UTC()
	v.record(eventContainerEvent, at, 0, ContainerStuffedEvent)
	return nil
}

// stuffing finds PHY's stuffing of the visit's load unit among the live houses of the movement.
func (v *Visit) stuffing(m *Movement) (Stuffing, error) {
	var found Stuffing
	for _, e := range m.Houses() {
		for _, s := range e.Facts.Stuffings {
			if s.LoadUnitNo != v.state.LoadUnitNo {
				continue
			}
			if found.ExecutionID != "" && found.ExecutionID != s.ExecutionID {
				return Stuffing{}, fmt.Errorf("%w: the houses name two stuffings of unit %d, %s and %s", ErrNotStuffed, s.LoadUnitNo, found.ExecutionID,
					s.ExecutionID)
			}
			if found.ExecutionID == "" || s.SealedAt.After(found.SealedAt) {
				found = s
			}
		}
	}
	if found.ExecutionID == "" {
		return Stuffing{}, fmt.Errorf("%w: unit %d; refresh the houses when FUL has newer evidence", ErrNotStuffed, v.state.LoadUnitNo)
	}
	return found, nil
}

// differences states how PHY's stuffing differs from the container: its seal is none of the visit's seals, its
// unit's size-type is not the visit's.
func (v *Visit) differences(s Stuffing) []string {
	var out []string
	key, held := sealKey(s.Seal), false
	for _, seal := range v.state.Seals {
		held = held || (key != "" && sealKey(seal.Number) == key)
	}
	if !held {
		out = append(out, fmt.Sprintf("PHY closed the stuffing of unit %d with seal %q, which is none of the container's seals", s.LoadUnitNo, s.Seal))
	}
	if s.TypeCode != v.state.SizeType {
		out = append(out, fmt.Sprintf("unit %d is a %s in PHY, the container a %s", s.LoadUnitNo, s.TypeCode, v.state.SizeType))
	}
	return out
}

// sealKey is a seal number by its letters and digits alone, in capitals: the same seal however it was keyed.
func sealKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	return b.String()
}

// Release gives an assigned container back unused, with a reason. A stuffed container is not released.
func (v *Visit) Release(reason, by string, at time.Time) error {
	switch {
	case v.state.Status == VisitStuffed:
		return ErrCargoInContainer
	case v.state.Status != VisitAssigned:
		return ErrInvalidTransition
	case !text(reason):
		return fmt.Errorf("%w: a release needs a reason", ErrInvalidChange)
	}
	if err := v.changeable(by, at); err != nil {
		return err
	}
	v.state.Status, v.state.ReleaseReason, v.state.ReleasedBy, v.state.ReleasedAt = VisitReleased, reason, by, at.UTC()
	v.record(eventContainerReleased, at, 0, "")
	return nil
}

func (v *Visit) changeable(by string, at time.Time) error {
	if blank(by) || at.IsZero() || at.Before(v.LastChange()) {
		return fmt.Errorf("%w: author and business time not before the visit's last change", ErrInvalidChange)
	}
	return nil
}

// LastChange is the latest business time recorded on the visit.
func (v *Visit) LastChange() time.Time {
	last := later(later(v.state.AssignedAt, v.state.StuffedAt), v.state.ReleasedAt)
	for _, s := range v.state.Seals {
		last = later(last, s.At)
	}
	return last
}

func (v *Visit) record(kind string, at time.Time, seal int, reason string) {
	v.pending = append(v.pending, Event{Type: kind, At: at.UTC(), TenantID: v.state.TenantID, SubjectID: string(v.state.VisitID),
		ConsolidationID: v.state.ConsolidationID, MovementID: string(v.state.MovementID), SealNo: seal, Reason: reason})
}

// Seal returns the container's current seal; zero when none is recorded.
func (v *Visit) Seal() VisitSeal {
	if n := len(v.state.Seals); n > 0 {
		return v.state.Seals[n-1]
	}
	return VisitSeal{}
}

// Live reports whether the visit holds its container: any state but RELEASED.
func (v *Visit) Live() bool { return v.state.Status != VisitReleased }

// Stamp records when SEA entered what was added since the visit was loaded (T1).
func (v *Visit) Stamp(entered time.Time) {
	if v.state.EnteredAt.IsZero() {
		v.state.EnteredAt = entered.UTC()
	}
	for i := v.seals; i < len(v.state.Seals); i++ {
		v.state.Seals[i].EnteredAt = entered.UTC()
	}
}

// RestoreVisit rebuilds a stored visit without emitting events.
func RestoreVisit(st VisitState) (*Visit, error) {
	st.Seals = append([]VisitSeal(nil), st.Seals...)
	v := &Visit{state: st, seals: len(st.Seals)}
	if err := v.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidVisit, err)
	}
	return v, nil
}

func (v *Visit) validate() error {
	st := v.state
	stuffed, released := st.Status == VisitStuffed, st.Status == VisitReleased
	switch {
	case !identifier(string(st.TenantID)) || !identifier(string(st.VisitID)) || !identifier(string(st.MovementID)) || !identifier(st.ConsolidationID) ||
		st.LoadUnitNo < 1 || blank(st.AssignedBy) || st.AssignedAt.IsZero():
		return fmt.Errorf("identity")
	case !ValidContainerNumber(st.Number) || !sizeTypeForm.MatchString(st.SizeType) || !st.Plate.valid():
		return fmt.Errorf("container")
	case st.Status != VisitAssigned && !stuffed && !released:
		return fmt.Errorf("status %s", st.Status)
	case stuffed != identifier(st.ExecutionID) || stuffed == blank(st.StuffedBy) || stuffed == st.StuffedAt.IsZero() || (stuffed && len(st.Seals) == 0) ||
		(stuffed && st.StuffedAt.Before(st.AssignedAt)):
		return fmt.Errorf("stuffing")
	case st.DifferenceReason != "" && (!stuffed || !text(st.DifferenceReason)):
		return fmt.Errorf("difference reason")
	case released != text(st.ReleaseReason) || released == blank(st.ReleasedBy) || released == st.ReleasedAt.IsZero() ||
		(released && st.ReleasedAt.Before(st.AssignedAt)):
		return fmt.Errorf("release")
	}
	for i, s := range st.Seals {
		if s.No != i+1 || !s.valid() || s.At.Before(st.AssignedAt) || (i > 0 && (st.Seals[i-1].Number == s.Number || s.At.Before(st.Seals[i-1].At))) {
			return fmt.Errorf("seal %d", i+1)
		}
	}
	return nil
}

// State returns a copy of the visit for persistence and read models.
func (v *Visit) State() VisitState {
	st := v.state
	st.Seals = append([]VisitSeal(nil), st.Seals...)
	return st
}

func (v *Visit) ID() VisitID      { return v.state.VisitID }
func (v *Visit) Pending() []Event { return append([]Event(nil), v.pending...) }

// AddedSeals returns the seals recorded since the visit was loaded.
func (v *Visit) AddedSeals() []VisitSeal { return v.state.Seals[v.seals:] }

// Acknowledge is permitted only after the transaction holding the visit and its Outbox events commits.
func (v *Visit) Acknowledge() { v.pending, v.seals = nil, len(v.state.Seals) }
