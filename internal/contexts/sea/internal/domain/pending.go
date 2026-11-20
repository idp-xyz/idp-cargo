package domain

import (
	"fmt"
	"time"
)

// A PendingItem is something that stands between a movement and its finalization, or that an operator should look at.
// Finalize refuses while a blocking item is open and names the first; nothing else of SEA refuses for want of what PHY
// reported (a refresh writes what it can), so the items are the one list of what is to be re-read or recorded. Each
// message says who does what. Kind is the code, Scope says whether the item is about a HOUSE or a CONTAINER, SubjectID
// is the shipment or the container's number.
type PendingItem struct {
	Kind      string
	Scope     string
	SubjectID string
	Message   string
	Blocking  bool
}

const (
	ScopeHouse     = "HOUSE"
	ScopeContainer = "CONTAINER"

	// Blocking kinds.
	PendHousePlanNotRead      = "HOUSE_PLAN_NOT_READ"
	PendHouseNotReported      = "HOUSE_NOT_REPORTED"
	PendFULBehind             = "FUL_BEHIND_PHY"
	PendHouseStale            = "HOUSE_STALE"
	PendContainerNoSeal       = "CONTAINER_NO_SEAL"
	PendHousesDisagree        = "HOUSES_DISAGREE_ON_SEAL"
	PendSealNotOnContainer    = "PHY_SEAL_NOT_ON_CONTAINER"
	PendLaterShipperSeal      = "LATER_SHIPPER_SEAL"
	PendGatedBeforeReseal     = "GATED_IN_BEFORE_RESEAL"
	PendNoVGM                 = "NO_VGM_BY_CUTOFF"
	PendVGMBeforeReseal       = "VGM_BEFORE_RESEAL"
	PendHouseNotStuffed       = "HOUSE_NOT_STUFFED"
	PendHouseUnknownContainer = "HOUSE_NAMES_UNKNOWN_CONTAINER"
	PendHousePacks            = "HOUSE_PACKS_NOT_STUFFED"

	// Warnings: they do not block.
	WarnVGMEnteredBeforeSealKnown = "VGM_ENTERED_BEFORE_SEAL_KNOWN"
	WarnResealDatedBeforeStuffing = "RESEAL_DATED_BEFORE_STUFFING"
)

func houseStage(kind string) bool {
	return kind == PendHousePlanNotRead || kind == PendHouseNotReported || kind == PendFULBehind || kind == PendHouseStale
}

// PendingItems lists everything pending on the movement, as Finalize reads it: FUL's plans of the live houses (read
// just before, with EvidenceBehind) against what SEA froze, then, on an own-container movement, each live container,
// then each live house's packs in those containers, then the warnings. Blocking items come in the order Finalize
// reports them.
func (m *Movement) PendingItems(plans []Plan, visits []VisitState) []PendingItem {
	items := m.houseItems(plans)
	if m.state.Kind != KindOwn {
		return items
	}
	live := liveVisits(visits)
	cont, warns := m.containerItems(m.Houses(), live)
	return append(append(items, cont...), warns...)
}

// CheckHouses checks FUL's plans of the movement's live houses against what SEA froze, as Finalize and the stuffing of
// a container need them: the first blocking item of the houses is the refusal, which wraps refusal.
func (m *Movement) CheckHouses(plans []Plan, refusal error) error {
	for _, it := range m.houseItems(plans) {
		if it.Blocking {
			return fmt.Errorf("%w: %s", refusal, it.Message)
		}
	}
	return nil
}

// houseItems: each live house's plan must have been read, FUL must still report the house attached as SEA froze it, on
// an own-container movement FUL's evidence of the house must be PHY's current one (F0: FUL then has recorded PHY's
// latest facts, which a rework or reseal may have changed), and its facts must be those SEA froze (STALE_HOUSE).
func (m *Movement) houseItems(plans []Plan) []PendingItem {
	known := map[string]Plan{}
	for _, p := range plans {
		known[p.ShipmentID] = p
	}
	var out []PendingItem
	add := func(h Entry, kind, msg string) {
		out = append(out, PendingItem{Kind: kind, Scope: ScopeHouse, SubjectID: h.ShipmentID, Message: msg, Blocking: true})
	}
	for _, h := range m.Houses() {
		p, found := known[h.ShipmentID]
		if !found {
			add(h, PendHousePlanNotRead, fmt.Sprintf("FUL's plan of %s was not read", h.ShipmentID))
			continue
		}
		if err := m.admits(p); err != nil {
			add(h, PendHouseNotReported, fmt.Sprintf("STALE_HOUSE: FUL no longer reports %s %s as SEA froze it (%v); refresh the house, or end its association", h.HBL, h.ShipmentID, err))
			continue
		}
		if m.state.Kind == KindOwn && p.EvidenceBehind {
			add(h, PendFULBehind, fmt.Sprintf("FUL has not recorded PHY's latest facts of %s %s; record them in FUL, and refresh the house if SEA then reports it STALE_HOUSE", h.HBL, h.ShipmentID))
			continue
		}
		if f, err := facts(p); err != nil || p.PlanVersion != h.PlanVersion || !sameMeasures(f, h.Facts) {
			add(h, PendHouseStale, fmt.Sprintf("STALE_HOUSE: FUL reports newer facts of %s %s; refresh it first", h.HBL, h.ShipmentID))
		}
	}
	return out
}

// containerItems checks every live container. P is PHY's seal of the container's stuffing as the movement's live houses
// carry it (seal and sealed-at). After the container has a seal: F1 the houses agree on P, F2 P is one of the
// container's seals and only a customs or carrier seal follows it, F3 the container did not gate in or load before PHY
// sealed it again, F4 its latest VGM was submitted, and for a resealed container determined, not before P. A container
// no live house names has no P and none of the four apply.
func (m *Movement) containerItems(houses []Entry, visits []VisitState) (blocking, warnings []PendingItem) {
	cut, hasCut := m.VGMCutOff()
	byUnit := map[int]VisitState{}
	for _, v := range visits {
		byUnit[v.LoadUnitNo] = v
		add := func(kind, msg string) {
			blocking = append(blocking, PendingItem{Kind: kind, Scope: ScopeContainer, SubjectID: v.Number, Message: msg, Blocking: true})
		}
		if v.Status == VisitAssigned || len(v.Seals) == 0 {
			add(PendContainerNoSeal, fmt.Sprintf("container %s has no seal; record its seal (RecordContainerSeal) and take the stuffing (RecordStuffingDone)", v.Number))
			continue
		}
		p, carried, disagree := phySeal(v, houses)
		if disagree != "" {
			add(PendHousesDisagree, disagree)
			carried = false
		}
		resealed := false
		if carried {
			resealed = v.resealed(p, m.sealHistory(v.ExecutionID))
			m.sealItems(v, p, resealed, add)
		}
		ready := vgmReady(v, cut, hasCut)
		if !ready {
			add(PendNoVGM, fmt.Sprintf("container %s has no VGM submitted by the cut-off or accepted by the carrier; assert one (AssertVgm) or record the carrier's answer", v.Number))
		}
		if carried && ready {
			last := v.Assertions[len(v.Assertions)-1]
			if last.SubmittedAt.Before(p.SealedAt) {
				add(PendVGMBeforeReseal, fmt.Sprintf("container %s has no VGM submitted after PHY sealed it (%s): its latest was submitted %s; assert a new one (AssertVgm)",
					v.Number, p.SealedAt.UTC().Format(time.RFC3339), last.SubmittedAt.UTC().Format(time.RFC3339)))
			} else if resealed && last.DeterminedAt.Before(p.SealedAt) {
				add(PendVGMBeforeReseal, fmt.Sprintf("the VGM of container %s has its mass determined (%s) before PHY sealed the container again (%s); assert one determined after it (AssertVgm)",
					v.Number, last.DeterminedAt.UTC().Format(time.RFC3339), p.SealedAt.UTC().Format(time.RFC3339)))
			}
			warnings = append(warnings, v.warnings(p, last, resealed)...)
		}
	}
	for _, h := range houses {
		add := func(kind, msg string) {
			blocking = append(blocking, PendingItem{Kind: kind, Scope: ScopeHouse, SubjectID: h.ShipmentID, Message: msg, Blocking: true})
		}
		if len(h.Facts.Stuffings) == 0 {
			add(PendHouseNotStuffed, fmt.Sprintf("%s %s is not stuffed in a container; PHY has not sealed its cargo in one, or FUL and SEA have not read it yet", h.HBL, h.ShipmentID))
			continue
		}
		var packs int64
		bad := false
		for _, s := range h.Facts.Stuffings {
			v, ok := byUnit[s.LoadUnitNo]
			if !ok || v.ExecutionID != s.ExecutionID {
				add(PendHouseUnknownContainer, fmt.Sprintf("%s %s names a stuffing of unit %d that is not one of the movement's containers; assign the container and take its stuffing", h.HBL, h.ShipmentID, s.LoadUnitNo))
				bad = true
				break
			}
			packs += s.Packs
		}
		if !bad && packs != h.Facts.Packs {
			add(PendHousePacks, fmt.Sprintf("%s %s has %d packs and %d stuffed", h.HBL, h.ShipmentID, h.Facts.Packs, packs))
		}
	}
	return blocking, warnings
}

// phySeal is P, PHY's stuffing of the visit's execution as the live houses carry it: the stuffing with its seal and
// sealed-at, and whether any live house carries it. The houses agree (F1) when each carries the same seal, by its
// letters and digits, and the same sealed-at; otherwise the message says to refresh the houses.
func phySeal(v VisitState, houses []Entry) (Stuffing, bool, string) {
	var p Stuffing
	var from string
	for _, h := range houses {
		for _, s := range h.Facts.Stuffings {
			if s.ExecutionID != v.ExecutionID {
				continue
			}
			if from == "" {
				p, from = s, h.HBL
			} else if sealKey(s.Seal) != sealKey(p.Seal) || !s.SealedAt.Equal(p.SealedAt) {
				return Stuffing{}, false, fmt.Sprintf("the houses %s and %s carry different seals of PHY's stuffing %s (%q sealed %s, %q sealed %s); refresh the houses",
					from, h.HBL, v.ExecutionID, p.Seal, p.SealedAt.UTC().Format(time.RFC3339), s.Seal, s.SealedAt.UTC().Format(time.RFC3339))
			}
		}
	}
	return p, from != "", ""
}

// sealHistory is every stuffing of the execution that any entry of the movement carried, the ended entries (REFRESHED,
// DETACHED, TRANSFERRED) included: what FUL relayed of PHY's seal each time SEA read a house.
func (m *Movement) sealHistory(execution string) []Stuffing {
	var out []Stuffing
	for _, e := range m.state.Entries {
		for _, s := range e.Facts.Stuffings {
			if s.ExecutionID == execution {
				out = append(out, s)
			}
		}
	}
	return out
}

// sealChanged reports whether an entry of the history carried the execution's stuffing with a seal (by sealKey) or a
// sealed-at other than p's: PHY sealed it again since, whatever business times and difference reason say.
func sealChanged(p Stuffing, history []Stuffing) bool {
	for _, h := range history {
		if sealKey(h.Seal) != sealKey(p.Seal) || !h.SealedAt.Equal(p.SealedAt) {
			return true
		}
	}
	return false
}

// resealed reports whether PHY sealed the container's stuffing again after SEA took the container as stuffed, with p
// the seal the live houses carry. For a visit that has what SEA took (StuffedPhySeal and its sealed-at, kept since
// V2.88) it is exactly this: p differs from it by seal key or by sealed-at; not the business times, not the entries,
// not the difference reason (stated against the seal SEA took, which PHY's reseal replaces). For a visit stuffed before
// this version, which has none, the rule of V2.88's first corrections stands (legacyResealed), a known limit.
func (v VisitState) resealed(p Stuffing, history []Stuffing) bool {
	if v.StuffedPhySeal != "" {
		return sealKey(p.Seal) != sealKey(v.StuffedPhySeal) || !p.SealedAt.Equal(v.StuffedPhySealedAt)
	}
	return v.legacyResealed(p, history)
}

// followable reports whether PHY's stuffing s is to be followed onto the visit, whose seals do not hold its key: the
// container is resealed (with what SEA took, as resealed says; for a visit stuffed before V2.88: PHY's sealed-at is
// after StuffedAt, or the visit has no difference reason, or the movement's history shows the seal changed).
func (v VisitState) followable(s Stuffing, history []Stuffing) bool {
	if v.StuffedPhySeal != "" {
		return v.resealed(s, history)
	}
	return s.SealedAt.After(v.StuffedAt) || v.DifferenceReason == "" || sealChanged(s, history)
}

// legacyResealed is the rule for a visit that has not what SEA took: PHY's sealed-at is after StuffedAt; or an entry of
// the movement carried the stuffing with another seal or sealed-at (sealChanged); or the container has no difference
// reason and none of its seals with p's key was recorded at a business time not after StuffedAt. With a difference
// reason and no change in the history a reseal dated before StuffedAt cannot be told from the difference the reason was
// stated for. The history is wrong when PHY resealed before SEA's stuffing: the reason for the stored seal.
func (v VisitState) legacyResealed(p Stuffing, history []Stuffing) bool {
	if p.SealedAt.After(v.StuffedAt) || sealChanged(p, history) {
		return true
	}
	if v.DifferenceReason != "" {
		return false
	}
	key := sealKey(p.Seal)
	for _, s := range v.Seals {
		if key != "" && sealKey(s.Number) == key && !s.At.After(v.StuffedAt) {
			return false
		}
	}
	return true
}

// warnings are what does not block. SEA trusts the business times it is given; it also keeps when it entered each seal
// and each VGM assertion (the system clock). A resealed container whose latest VGM was entered before SEA entered the
// seal with P's key was keyed in before SEA learned of PHY's new seal: its mass may have been determined before the
// reseal. A reseal that is dated before StuffedAt was told only by the seal PHY holds now being none the container held
// when it was taken: the times are out of order.
func (v VisitState) warnings(p Stuffing, last VgmAssertion, resealed bool) []PendingItem {
	if !resealed || p.SealedAt.After(v.StuffedAt) {
		return nil // in the ordinary order the determination time already blocks an old weighing
	}
	var out []PendingItem
	key := sealKey(p.Seal)
	for i := len(v.Seals) - 1; i >= 0; i-- {
		if s := v.Seals[i]; key != "" && sealKey(s.Number) == key {
			if last.EnteredAt.Before(s.EnteredAt) {
				out = append(out, PendingItem{Kind: WarnVGMEnteredBeforeSealKnown, Scope: ScopeContainer, SubjectID: v.Number, Message: fmt.Sprintf(
					"the VGM of container %s was entered (%s) before SEA learned of PHY's new seal (entered %s), and the reseal is dated before the stuffing: check that its mass was determined after the reseal",
					v.Number, last.EnteredAt.UTC().Format(time.RFC3339), s.EnteredAt.UTC().Format(time.RFC3339))})
			}
			break
		}
	}
	return append(out, PendingItem{Kind: WarnResealDatedBeforeStuffing, Scope: ScopeContainer, SubjectID: v.Number, Message: fmt.Sprintf(
		"PHY's seal of container %s (%q) is dated %s, not after the stuffing SEA took (%s), yet PHY sealed it again: the times are out of order; check them",
		v.Number, p.Seal, p.SealedAt.UTC().Format(time.RFC3339), v.StuffedAt.UTC().Format(time.RFC3339))})
}

func (v VisitState) lastChange() time.Time {
	last := later(later(v.AssignedAt, v.StuffedAt), v.ReleasedAt)
	for _, s := range v.Seals {
		last = later(last, s.At)
	}
	for _, a := range v.Assertions {
		last = later(later(last, a.At), a.RespondedAt)
	}
	for _, e := range v.Events {
		last = later(last, e.At)
	}
	return last
}

// sealItems are F2 and F3 of a container whose stuffing PHY sealed with p. F2: p's seal is one of the container's
// seals, and every seal recorded after the latest one with p's key is a CUSTOMS or CARRIER seal; a container taken as
// stuffed with a difference reason, whose stuffing PHY has not sealed again since, keeps the rule it was taken under
// and is not asked. F3: a container PHY sealed again after SEA took it as stuffed did not gate in, nor was it loaded,
// before PHY sealed it again: by its own gate-in or loading events, or by the carrier's loading reports that name its
// number or name no container (a report naming another container is that container's). A time equal to PHY's is not
// before it.
func (m *Movement) sealItems(v VisitState, p Stuffing, resealed bool, add func(kind, msg string)) {
	if v.DifferenceReason == "" || resealed {
		key, last := sealKey(p.Seal), -1
		for i, s := range v.Seals {
			if key != "" && sealKey(s.Number) == key {
				last = i
			}
		}
		switch {
		case last < 0:
			add(PendSealNotOnContainer, sealMissingMessage(v, p))
		default:
			for _, s := range v.Seals[last+1:] {
				if s.Party != SealCustoms && s.Party != SealCarrier {
					add(PendLaterShipperSeal, fmt.Sprintf("container %s has the %s seal %s recorded after PHY's seal %s; only a customs or carrier seal may follow it: record PHY's seal %s again, or have customs or the carrier seal it",
						v.Number, s.Party, s.Number, v.Seals[last].Number, v.Seals[last].Number))
					break
				}
			}
		}
	}
	if !resealed {
		return
	}
	what := ""
	for _, e := range v.Events {
		switch {
		case e.Kind == EventGateIn && e.OccurredAt.Before(p.SealedAt):
			what = "gate-in"
		case e.Kind == EventLoaded && e.OccurredAt.Before(p.SealedAt) && what == "":
			what = "loading"
		}
	}
	for _, c := range m.state.CarrierFacts {
		if what == "" && c.Kind == CarrierLoaded && (c.ContainerNumber == "" || c.ContainerNumber == v.Number) && c.OccurredAt.Before(p.SealedAt) {
			what = "the carrier's report of its loading"
		}
	}
	if what != "" {
		add(PendGatedBeforeReseal, fmt.Sprintf("container %s gated in or was loaded before PHY sealed it again (%s), by its %s, and its return from the terminal is not recorded yet (the next step records it)",
			v.Number, p.SealedAt.UTC().Format(time.RFC3339), what))
	}
}

// sealMissingMessage says why PHY's seal is not on the container and what to do: SEA records it by itself on a refresh
// when its form fits and the business time allows; otherwise it is recorded by hand, or PHY seals the container again.
func sealMissingMessage(v VisitState, p Stuffing) string {
	base := fmt.Sprintf("container %s has none of the seals PHY sealed the stuffing %s with (%q)", v.Number, v.ExecutionID, p.Seal)
	times := fmt.Sprintf("at a business time not before PHY sealed it (%s) and not before the container's last change (%s)", p.SealedAt.UTC().Format(time.RFC3339),
		v.lastChange().UTC().Format(time.RFC3339))
	switch _, class := sealForSEA(p.Seal); class {
	case sealImpossible:
		return base + "; SEA's seal form cannot hold it, with no form that has the same letters and digits (letters A-Z, digits, 35 at most): PHY has to seal the container again with a seal of letters A-Z and digits"
	case sealByHand:
		return base + "; SEA's seal form cannot hold it as PHY keyed it: record the container's seal by hand in that form with the same letters and digits (" + sealKey(p.Seal) + ") " + times
	}
	return base + "; refresh a house that carries it (the refresh records it when its business time allows) or record it by hand " + times
}
