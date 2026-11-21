package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"go.idp.xyz/idp-cargo/internal/kernel"
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
	ErrInvalidVGM       = errors.New("sea: invalid VGM")
	ErrInvalidEvent     = errors.New("sea: invalid container event")
)

// VisitStatus is a container visit's state. ASSIGNED: the container is assigned to a planned load unit of the
// movement's consolidation. STUFFED: PHY's stuffing of the unit is sealed, as FUL relays it. RELEASED: the container
// was given back unused. VGM_SUBMITTED, GATED_IN and LOADED come with the VGM and the container events; a gated-in
// container taken back out of the terminal is VGM_SUBMITTED again.
type VisitStatus string

const (
	VisitAssigned VisitStatus = "ASSIGNED"
	VisitStuffed  VisitStatus = "STUFFED"
	VisitVGM      VisitStatus = "VGM_SUBMITTED"
	VisitGatedIn  VisitStatus = "GATED_IN"
	VisitLoaded   VisitStatus = "LOADED"
	VisitReleased VisitStatus = "RELEASED"
)

// VGM methods (SOLAS): weighing the packed container, or adding the cargo, the packing and the tare.
const (
	VgmMethod1 = "METHOD_1"
	VgmMethod2 = "METHOD_2"

	VgmAccepted = "ACCEPTED"
	VgmRejected = "REJECTED"

	// Container event kinds. STUFFED is recorded by StuffingDone, not by RecordContainerEvent.
	EventEmptyReleased = "EMPTY_RELEASED"
	EventEmptyPickedUp = "EMPTY_PICKED_UP"
	EventArrivedAtCFS  = "ARRIVED_AT_CFS"
	EventGateIn        = "GATE_IN_TERMINAL"
	EventGateOut       = "GATE_OUT_TERMINAL"
	EventLoaded        = "LOADED_ON_BOARD"
	EventRolled        = "ROLLED"
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
	otherRuns     = regexp.MustCompile(`[^0-9A-Z]+`)
	asciiKey      = regexp.MustCompile(`^[0-9A-Z]+$`)
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
	// StuffedPhySeal and StuffedPhySealedAt are PHY's seal of the stuffing as FUL relayed it when SEA took the
	// container as stuffed (V2.88): resealed is a difference from them. Empty on a visit stuffed before this version.
	StuffedPhySeal     string
	StuffedPhySealedAt time.Time
	ReleaseReason      string
	ReleasedBy         string
	ReleasedAt         time.Time
	Assertions         []VgmAssertion
	Events             []VisitEvent
	// Releases are the operator's releases of a conflict with a carrier's loading report (V2.89), only added.
	Releases   []ConflictRelease
	AssignedBy string
	AssignedAt time.Time
	EnteredAt  time.Time
}

// VgmAssertion is one immutable statement of the container's verified gross mass. Signatory is the authorised
// person's name: it is personal data, kept only until the visit is stored, and empty when a visit is read back.
// The name lives in its own table and is returned only by the restricted read.
type VgmAssertion struct {
	Version           int
	MassGrams         int64
	Method            string
	PartyID           string
	PartyRevision     int64
	DeterminedAt      time.Time
	SubmittedAt       time.Time
	LateReason        string
	Signatory         string
	By                string
	At                time.Time
	EnteredAt         time.Time
	Response          string
	ResponseSource    string
	RespondedAt       time.Time
	ResponseBy        string
	ResponseEnteredAt time.Time
}

// VisitEvent is a carrier or terminal fact about the container. It never moves a house. A source reference is
// recorded once: the same message again changes nothing, and the same reference with other content is refused.
type VisitEvent struct {
	No         int
	Kind       string
	Location   kernel.LocationCode
	OccurredAt time.Time
	Source     string
	Reference  string
	By         string
	At         time.Time
	EnteredAt  time.Time
}

func (e VisitEvent) valid() bool {
	return containerEventKind(e.Kind) && e.Kind != ContainerStuffedEvent && port(e.Location) && !e.OccurredAt.IsZero() &&
		!e.OccurredAt.After(e.At) && !e.At.IsZero() && short(e.Source, 64) && short(e.Reference, 128) && !blank(e.By)
}

func containerEventKind(kind string) bool {
	switch kind {
	case EventEmptyReleased, EventEmptyPickedUp, EventArrivedAtCFS, ContainerStuffedEvent, EventGateIn, EventGateOut, EventLoaded, EventRolled:
		return true
	}
	return false
}

// Visit is the aggregate root (ContainerVisit). Seals, VGM assertions and container events are only added.
type Visit struct {
	state      VisitState
	releases   int
	seals      int
	assertions int
	events     int
	responded  int // the version whose carrier response was recorded since the visit was loaded; 0 otherwise
	repeated   int // the event a repeated source reference matched; 0 otherwise
	pending    []Event
}

const (
	eventContainerAssigned = "ContainerAssigned.v1"
	eventSealRecorded      = "SealRecorded.v1"
	eventContainerEvent    = "ContainerEventRecorded.v1"
	eventContainerReleased = "ContainerReleased.v1"
	eventVgmAsserted       = "VgmAsserted.v1"
	eventVgmResponse       = "VgmResponseRecorded.v1"
	eventConflictReleased  = "CarrierLoadingConflictReleased.v1"

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
	v.record(eventContainerAssigned, at, 0, 0, 0, "")
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
	v.record(eventSealRecorded, at, s.No, 0, 0, "")
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
	if m == nil || m.ID() != v.state.MovementID || m.state.TenantID != v.state.TenantID || m.state.State != StateOpen {
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
	v.state.StuffedPhySeal, v.state.StuffedPhySealedAt = s.Seal, s.SealedAt.UTC()
	v.record(eventContainerEvent, at, 0, 0, 0, ContainerStuffedEvent)
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
	if !v.holds(sealKey(s.Seal)) {
		out = append(out, fmt.Sprintf("PHY closed the stuffing of unit %d with seal %q, which is none of the container's seals", s.LoadUnitNo, s.Seal))
	}
	if s.TypeCode != v.state.SizeType {
		out = append(out, fmt.Sprintf("unit %d is a %s in PHY, the container a %s", s.LoadUnitNo, s.TypeCode, v.state.SizeType))
	}
	return out
}

// holds reports whether one of the visit's seals, current or replaced, has the key; an empty key is no seal's.
func (v *Visit) holds(key string) bool {
	for _, seal := range v.state.Seals {
		if key != "" && sealKey(seal.Number) == key {
			return true
		}
	}
	return false
}

// FollowSeal takes PHY's new seal of the stuffing the container was taken as stuffed with: when its seal is none of the
// visit's seals and either PHY sealed the stuffing after StuffedAt or the visit has no difference reason, the seal is
// recorded as a SHIPPER seal applied at PHY's sealed-at, with a source that says it was followed from PHY's stuffing
// (and keeps PHY's text as it was). Without a difference reason StuffingDone required PHY's seal of that time to be one
// of the visit's seals, and a visit's seals are never removed, so a PHY seal the visit does not hold is one PHY applied
// since, whatever business time it was recorded with; with a difference reason a reseal dated before StuffedAt cannot
// be told from the difference the reason was stated for, and is not followed. PHY keeps free text: it is recorded
// upper-cased and trimmed when that fits SEA's seal form, else with each run of other characters replaced by one space
// when that fits and has the same sealKey. A seal that cannot be taken (its form, or a business time before PHY sealed
// it or before the container's last change) is not recorded and is not an error: the house's entry is written all the
// same, and the seal is a pending item of the movement (PendSealNotOnContainer) until it is recorded by hand or a later
// refresh finds the time allows it. A visit with no execution (ASSIGNED, RELEASED), another execution's stuffing and a
// seal the visit holds are not followed. It reports whether a seal was recorded.
func (v *Visit) FollowSeal(s Stuffing, by string, at time.Time) (bool, error) {
	return v.followSeal(s, nil, by, at)
}

// followSeal is FollowSeal with the history of the movement's entries: every stuffing of the execution any entry of the
// movement, live or ended, carried. When one of them has a seal (by sealKey) or a sealed-at other than s's, PHY sealed
// the stuffing again, whatever the business times and the difference reason say, and the seal is followed.
func (v *Visit) followSeal(s Stuffing, history []Stuffing, by string, at time.Time) (bool, error) {
	if v.state.ExecutionID != s.ExecutionID || v.holds(sealKey(s.Seal)) || !v.state.followable(s, history) {
		return false, nil
	}
	number, class := sealForSEA(s.Seal)
	if class != sealFits {
		return false, nil
	}
	source := fmt.Sprintf("followed from PHY's stuffing %s, sealed %s, as FUL relays it (PHY's text %q)", s.ExecutionID, s.SealedAt.UTC().Format(time.RFC3339), s.Seal)
	return v.RecordSeal(number, SealShipper, s.SealedAt, source, by, at) == nil, nil
}

// sealClass says what SEA's seal form makes of PHY's free text.
type sealClass int

const (
	sealFits       sealClass = iota // recorded as the number returned
	sealByHand                      // no cleaned form has the same key, but the key itself fits: an operator records it
	sealImpossible                  // no form has the same key: a letter outside A-Z, or too many letters and digits
)

// sealForSEA is the number SEA records for PHY's text: the text upper-cased and trimmed when that fits SEA's form, else
// with each run of characters other than A-Z and 0-9 replaced by one space when that fits and has the same sealKey.
func sealForSEA(text string) (string, sealClass) {
	number := strings.ToUpper(strings.TrimSpace(text))
	if sealNumberForm(number) {
		return number, sealFits
	}
	key := sealKey(text)
	if cleaned := strings.TrimSpace(otherRuns.ReplaceAllString(number, " ")); sealNumberForm(cleaned) && sealKey(cleaned) == key {
		return cleaned, sealFits
	}
	if key == "" || len(key) > 35 || !asciiKey.MatchString(key) {
		return "", sealImpossible
	}
	return "", sealByHand
}

func sealNumberForm(n string) bool { return len(n) <= 35 && sealForm.MatchString(n) }

// FollowSeals offers each stuffing of a house's entry to each of the movement's visits (FollowSeal), without the
// movement's history of entries. It stops at the first refusal.
func FollowSeals(e Entry, visits []*Visit, by string, at time.Time) error {
	return followSeals(e, nil, visits, by, at)
}

// FollowSeals offers each stuffing of a house's entry (the house's newest, as written by Associate or Refresh) to each
// of the movement's visits: when SEA writes a house's entry, PHY's new seal of a container SEA has taken as stuffed is
// recorded on it. The movement's history of entries, live and ended, tells a reseal the business times cannot show.
func (m *Movement) FollowSeals(e Entry, visits []*Visit, by string, at time.Time) error {
	return followSeals(e, m, visits, by, at)
}

func followSeals(e Entry, m *Movement, visits []*Visit, by string, at time.Time) error {
	for _, s := range e.Facts.Stuffings {
		var history []Stuffing
		if m != nil {
			history = m.sealHistory(s.ExecutionID)
		}
		for _, v := range visits {
			if _, err := v.followSeal(s, history, by, at); err != nil {
				return fmt.Errorf("house %s: %w", e.HBL, err)
			}
		}
	}
	return nil
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
	v.record(eventContainerReleased, at, 0, 0, 0, "")
	return nil
}

func (v *Visit) changeable(by string, at time.Time) error {
	if blank(by) || at.IsZero() || at.Before(v.LastChange()) {
		return fmt.Errorf("%w: author and business time not before the visit's last change", ErrInvalidChange)
	}
	return nil
}

// LastChange is the latest business time recorded on the visit.
func (v *Visit) LastChange() time.Time { return v.state.lastChange() }

func (v *Visit) record(kind string, at time.Time, seal, assertion, event int, reason string) {
	v.pending = append(v.pending, Event{Type: kind, At: at.UTC(), TenantID: v.state.TenantID, SubjectID: string(v.state.VisitID),
		ConsolidationID: v.state.ConsolidationID, MovementID: string(v.state.MovementID), SealNo: seal, AssertionNo: assertion, EventNo: event, Reason: reason})
}

// AssertVGM states the container's verified gross mass. It is the shipper's assertion, not a sum of the houses'
// declared weights. The mass is kept in grams. Where the plate's tare is known, the mass is not below it. The
// business time is when the assertion is submitted to the carrier; when that is after the current voyage's VGM
// cut-off, a late reason is required, and without a cut-off or while still before it a reason is refused.
// signatory is the authorised person's name and is not kept on the visit once it is stored.
func (v *Visit) AssertVGM(m *Movement, mass kernel.Quantity, method, party string, partyRevision int64, determined time.Time, lateReason, signatory, by string, at time.Time) error {
	if v.state.Status != VisitStuffed && v.state.Status != VisitVGM {
		return ErrInvalidTransition
	}
	if m == nil || m.ID() != v.state.MovementID || m.state.TenantID != v.state.TenantID || (m.state.State != StateOpen && m.state.State != StateFinal) {
		return ErrInvalidVGM
	}
	grams, ok := Grams(mass)
	cut, hasCut := m.VGMCutOff()
	switch {
	case !ok || grams < 1:
		return fmt.Errorf("%w: the mass is a positive weight in grams, kilograms or tonnes", ErrInvalidVGM)
	case v.state.Plate.TareKg > 0 && grams < v.state.Plate.TareKg*1000:
		return fmt.Errorf("%w: %d g is below the tare of %d kg", ErrInvalidVGM, grams, v.state.Plate.TareKg)
	case method != VgmMethod1 && method != VgmMethod2:
		return fmt.Errorf("%w: the method is METHOD_1 (the packed container weighed) or METHOD_2 (cargo, packing and tare)", ErrInvalidVGM)
	case !identifier(party) || partyRevision < 1:
		return fmt.Errorf("%w: the responsible party and its revision", ErrInvalidVGM)
	case determined.IsZero() || determined.After(at):
		return fmt.Errorf("%w: the mass is determined not after it is submitted", ErrInvalidVGM)
	case hasCut && at.After(cut) && !text(lateReason):
		return fmt.Errorf("%w: submitted after the VGM cut-off %s; state why it is late", ErrInvalidVGM, cut.Format(time.RFC3339))
	case (!hasCut || !at.After(cut)) && lateReason != "":
		return fmt.Errorf("%w: a late reason is taken only after the VGM cut-off", ErrInvalidVGM)
	case !short(signatory, 100):
		return fmt.Errorf("%w: the authorised signatory's name", ErrInvalidVGM)
	}
	if err := v.changeable(by, at); err != nil {
		return err
	}
	a := VgmAssertion{Version: len(v.state.Assertions) + 1, MassGrams: grams, Method: method, PartyID: party, PartyRevision: partyRevision,
		DeterminedAt: determined.UTC(), SubmittedAt: at.UTC(), LateReason: lateReason, Signatory: signatory, By: by, At: at.UTC()}
	v.state.Assertions = append(v.state.Assertions, a)
	v.state.Status = VisitVGM
	v.record(eventVgmAsserted, at, 0, a.Version, 0, "")
	return nil
}

// RecordVgmResponse records the carrier's answer to the latest assertion, once. The mass and the signatory stay.
func (v *Visit) RecordVgmResponse(response, source, by string, at time.Time) error {
	if v.state.Status != VisitVGM || len(v.state.Assertions) == 0 {
		return ErrInvalidTransition
	}
	i := len(v.state.Assertions) - 1
	a := &v.state.Assertions[i]
	switch {
	case a.Response != "":
		return fmt.Errorf("%w: version %d already has the carrier's answer", ErrInvalidChange, a.Version)
	case response != VgmAccepted && response != VgmRejected:
		return fmt.Errorf("%w: the carrier accepts or rejects", ErrInvalidVGM)
	case !text(source) || at.Before(a.SubmittedAt):
		return fmt.Errorf("%w: the carrier's source, not before the assertion was submitted", ErrInvalidVGM)
	}
	if err := v.changeable(by, at); err != nil {
		return err
	}
	a.Response, a.ResponseSource, a.RespondedAt, a.ResponseBy = response, source, at.UTC(), by
	v.responded = a.Version
	v.record(eventVgmResponse, at, 0, a.Version, 0, "")
	return nil
}

// RecordContainerEvent records a carrier or terminal fact about the container. It never moves a house. A container
// is gated in only once its VGM is submitted, and loaded on board only once it is gated in. A gated-in container is
// taken back out of the terminal (GATE_OUT_TERMINAL, the full container returned to the forwarder's warehouse), which
// returns the visit to VGM_SUBMITTED; a loaded container is not taken back here. Stuffing is not recorded here. The
// same source reference with the same content changes nothing and returns its number; with other content it is
// refused. The number is the event's place in the visit's history, and added reports whether this call recorded it.
func (v *Visit) RecordContainerEvent(kind string, where kernel.LocationCode, occurred time.Time, source, reference, by string, at time.Time) (int, bool, error) {
	if v.state.Status == VisitReleased {
		return 0, false, ErrInvalidTransition
	}
	occurred = occurred.UTC()
	switch {
	case kind == ContainerStuffedEvent:
		return 0, false, fmt.Errorf("%w: stuffing is recorded when the container is sealed", ErrInvalidEvent)
	case !containerEventKind(kind) || !port(where) || occurred.IsZero() || occurred.After(at) || !short(source, 64) || !short(reference, 128):
		return 0, false, ErrInvalidEvent
	}
	for _, e := range v.state.Events {
		if e.Source == source && e.Reference == reference {
			if e.Kind == kind && e.Location == where && e.OccurredAt.Equal(occurred) {
				v.repeated = e.No
				return e.No, false, nil
			}
			return 0, false, fmt.Errorf("%w: %s %s was recorded with other content", ErrInvalidChange, source, reference)
		}
	}
	switch kind {
	case EventGateIn:
		if v.state.Status != VisitVGM && v.state.Status != VisitGatedIn && v.state.Status != VisitLoaded {
			return 0, false, fmt.Errorf("%w: a container is gated in once its VGM is submitted", ErrInvalidEvent)
		}
	case EventGateOut:
		switch v.state.Status {
		case VisitGatedIn:
			for i := len(v.state.Events) - 1; i >= 0; i-- {
				if g := v.state.Events[i]; g.Kind == EventGateIn {
					if occurred.Before(g.OccurredAt) {
						return 0, false, fmt.Errorf("%w: the return is dated %s, before the container gated in (%s): give the time it was taken back out",
							ErrInvalidEvent, occurred.Format(time.RFC3339), g.OccurredAt.Format(time.RFC3339))
					}
					break
				}
			}
		case VisitLoaded:
			return 0, false, fmt.Errorf("%w: a container loaded on board is not taken back out of the terminal here; that is handled outside the system", ErrInvalidEvent)
		default:
			return 0, false, fmt.Errorf("%w: a container is taken back out of the terminal only while it is gated in", ErrInvalidEvent)
		}
	case EventLoaded:
		if v.state.Status != VisitGatedIn && v.state.Status != VisitLoaded {
			return 0, false, fmt.Errorf("%w: a container is loaded once it is gated in", ErrInvalidEvent)
		}
	}
	if err := v.changeable(by, at); err != nil {
		return 0, false, err
	}
	e := VisitEvent{No: len(v.state.Events) + 1, Kind: kind, Location: where, OccurredAt: occurred, Source: source, Reference: reference, By: by, At: at.UTC()}
	v.state.Events = append(v.state.Events, e)
	switch kind {
	case EventGateIn:
		if v.state.Status == VisitVGM {
			v.state.Status = VisitGatedIn
		}
	case EventGateOut:
		v.state.Status = VisitVGM
	case EventLoaded:
		v.state.Status = VisitLoaded
	}
	v.record(eventContainerEvent, at, 0, 0, e.No, kind)
	return e.No, true, nil
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
	for i := v.assertions; i < len(v.state.Assertions); i++ {
		v.state.Assertions[i].EnteredAt = entered.UTC()
	}
	for i := v.events; i < len(v.state.Events); i++ {
		v.state.Events[i].EnteredAt = entered.UTC()
	}
	for i := v.releases; i < len(v.state.Releases); i++ {
		v.state.Releases[i].EnteredAt = entered.UTC()
	}
	if v.responded > 0 {
		v.state.Assertions[v.responded-1].ResponseEnteredAt = entered.UTC()
	}
}

// RestoreVisit rebuilds a stored visit without emitting events.
func RestoreVisit(st VisitState) (*Visit, error) {
	st.Seals = append([]VisitSeal(nil), st.Seals...)
	st.Assertions = append([]VgmAssertion(nil), st.Assertions...)
	st.Events = append([]VisitEvent(nil), st.Events...)
	st.Releases = append([]ConflictRelease(nil), st.Releases...)
	for i := range st.Assertions {
		st.Assertions[i].Signatory = ""
	}
	v := &Visit{state: st, seals: len(st.Seals), assertions: len(st.Assertions), events: len(st.Events), releases: len(st.Releases)}
	if err := v.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidVisit, err)
	}
	return v, nil
}

func (v *Visit) validate() error {
	st := v.state
	stuffed, submitted := st.Status == VisitStuffed, st.Status == VisitVGM
	gated, loaded, released := st.Status == VisitGatedIn, st.Status == VisitLoaded, st.Status == VisitReleased
	onward := submitted || gated || loaded
	packed := stuffed || onward
	switch {
	case !identifier(string(st.TenantID)) || !identifier(string(st.VisitID)) || !identifier(string(st.MovementID)) || !identifier(st.ConsolidationID) ||
		st.LoadUnitNo < 1 || blank(st.AssignedBy) || st.AssignedAt.IsZero():
		return fmt.Errorf("identity")
	case !ValidContainerNumber(st.Number) || !sizeTypeForm.MatchString(st.SizeType) || !st.Plate.valid():
		return fmt.Errorf("container")
	case st.Status != VisitAssigned && !packed && !released:
		return fmt.Errorf("status %s", st.Status)
	case packed != identifier(st.ExecutionID) || packed == blank(st.StuffedBy) || packed == st.StuffedAt.IsZero() || (packed && len(st.Seals) == 0) ||
		(packed && st.StuffedAt.Before(st.AssignedAt)):
		return fmt.Errorf("stuffing")
	case st.DifferenceReason != "" && (!packed || !text(st.DifferenceReason)):
		return fmt.Errorf("difference reason")
	case (st.StuffedPhySeal == "") != st.StuffedPhySealedAt.IsZero() || (st.StuffedPhySeal != "" && (!packed || !text(st.StuffedPhySeal) || st.StuffedPhySealedAt.After(st.StuffedAt))):
		return fmt.Errorf("stuffed seal")
	case onward != (len(st.Assertions) > 0):
		return fmt.Errorf("vgm")
	case released != text(st.ReleaseReason) || released == blank(st.ReleasedBy) || released == st.ReleasedAt.IsZero() ||
		(released && st.ReleasedAt.Before(st.AssignedAt)):
		return fmt.Errorf("release")
	}
	for i, a := range st.Assertions {
		answered := a.Response != ""
		switch {
		case a.Version != i+1 || a.MassGrams < 1 || (a.Method != VgmMethod1 && a.Method != VgmMethod2) || !identifier(a.PartyID) || a.PartyRevision < 1 ||
			a.DeterminedAt.IsZero() || a.DeterminedAt.After(a.SubmittedAt) || a.SubmittedAt.Before(st.StuffedAt) || blank(a.By) || (a.LateReason != "" && !text(a.LateReason)):
			return fmt.Errorf("assertion %d", i+1)
		case answered && (a.Response != VgmAccepted && a.Response != VgmRejected || !text(a.ResponseSource) || a.RespondedAt.Before(a.SubmittedAt) || blank(a.ResponseBy)):
			return fmt.Errorf("assertion %d response", i+1)
		case !answered && (a.ResponseSource != "" || !a.RespondedAt.IsZero() || a.ResponseBy != ""):
			return fmt.Errorf("assertion %d response", i+1)
		}
	}
	for i, s := range st.Seals {
		if s.No != i+1 || !s.valid() || s.At.Before(st.AssignedAt) || (i > 0 && (st.Seals[i-1].Number == s.Number || s.At.Before(st.Seals[i-1].At))) {
			return fmt.Errorf("seal %d", i+1)
		}
	}
	for i, r := range st.Releases {
		if r.No != i+1 || !packed || !r.valid() || r.At.Before(st.StuffedAt) || (i > 0 && r.At.Before(st.Releases[i-1].At)) {
			return fmt.Errorf("release %d", i+1)
		}
	}
	lastIn, lastOut, lastLoad, lastTerminal := -1, -1, -1, ""
	seen := map[string]struct{}{}
	for i, e := range st.Events {
		if e.No != i+1 || !e.valid() || e.At.Before(st.AssignedAt) {
			return fmt.Errorf("event %d", i+1)
		}
		if _, ok := seen[e.Source+"\x00"+e.Reference]; ok {
			return fmt.Errorf("event %d", i+1)
		}
		seen[e.Source+"\x00"+e.Reference] = struct{}{}
		switch e.Kind {
		case EventGateIn:
			lastIn = i
		case EventGateOut:
			if lastTerminal != EventGateIn {
				return fmt.Errorf("event %d", i+1)
			}
			lastOut = i
		case EventLoaded:
			lastLoad = i
		}
		if e.Kind == EventGateIn || e.Kind == EventGateOut {
			lastTerminal = e.Kind
		}
	}
	// A container is gated in or loaded exactly when its latest gate-in comes after its latest gate-out, loaded exactly
	// when a loading is recorded, and no gate-out comes after a loading. A gate-out follows a gate-in with no gate-out
	// between them.
	if (gated || loaded) != (lastIn > lastOut) || loaded != (lastLoad >= 0) || (lastLoad >= 0 && lastOut > lastLoad) {
		return fmt.Errorf("event")
	}
	return nil
}

// State returns a copy of the visit for persistence and read models.
func (v *Visit) State() VisitState {
	st := v.state
	st.Seals = append([]VisitSeal(nil), st.Seals...)
	st.Assertions = append([]VgmAssertion(nil), st.Assertions...)
	st.Events = append([]VisitEvent(nil), st.Events...)
	st.Releases = append([]ConflictRelease(nil), st.Releases...)
	return st
}

func (v *Visit) ID() VisitID      { return v.state.VisitID }
func (v *Visit) Pending() []Event { return append([]Event(nil), v.pending...) }

// AddedReleases returns the releases recorded since the visit was loaded.
func (v *Visit) AddedReleases() []ConflictRelease { return v.state.Releases[v.releases:] }

// AddedSeals returns the seals recorded since the visit was loaded.
func (v *Visit) AddedSeals() []VisitSeal { return v.state.Seals[v.seals:] }

// AddedAssertions returns the VGM assertions recorded since the visit was loaded. Their signatory names are still set.
func (v *Visit) AddedAssertions() []VgmAssertion { return v.state.Assertions[v.assertions:] }

// RespondedAssertion is the version whose carrier response was recorded since the visit was loaded, or 0.
func (v *Visit) RespondedAssertion() int { return v.responded }

// RejectedResponse reports that this command recorded the carrier's rejection of an assertion.
func (v *Visit) RejectedResponse() bool {
	n := v.responded
	return n > 0 && n <= len(v.state.Assertions) && v.state.Assertions[n-1].Response == VgmRejected
}

// AddedEvents returns the container events recorded since the visit was loaded.
func (v *Visit) AddedEvents() []VisitEvent { return v.state.Events[v.events:] }

// RepeatedEvent is the event a repeated source reference matched since the visit was loaded, or 0.
func (v *Visit) RepeatedEvent() int { return v.repeated }

// Acknowledge is permitted only after the transaction holding the visit and its Outbox events commits.
func (v *Visit) Acknowledge() {
	v.pending, v.seals, v.assertions, v.events, v.responded, v.repeated = nil, len(v.state.Seals), len(v.state.Assertions), len(v.state.Events), 0, 0
}
