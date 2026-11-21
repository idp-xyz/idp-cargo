package domain

import (
	"fmt"
	"strings"
)

// Kinds of a house bill's pending items (V2.90). A pending item is read from the document and SEA's latest facts of its
// movement; DOC stores nothing for it and changes nothing by it.
const (
	PendingHouseNotAllLoaded = "HOUSE_NOT_ALL_LOADED"
	PendingSourceSuperseded  = "SOURCE_SUPERSEDED"
	PendingHouseLeft         = "HOUSE_LEFT_MOVEMENT"
	PendingSourceNotFinal    = "SOURCE_NOT_FINAL"
	PendingIssuedChanged     = "ISSUED_CONTENT_CHANGED"
)

// PendingItem is one pending item of a house bill. Blocking says whether the bill cannot be issued while it stands: it
// is false for a bill already issued, which has no issue waiting.
type PendingItem struct {
	Kind       string
	DocumentID DocumentID
	ShipmentID string
	Number     string
	Message    string
	Blocking   bool
}

// PendingMovement is the movement whose facts PendingItems reads: the one the version it considers was built on. ok is
// false for a document that has nothing pending by definition: not a sea house bill, or one that is cancelled or
// surrendered (its issued version cannot be amended, and a cancelled one is finished).
func (d *Document) PendingMovement() (movement string, ok bool) {
	v, ok := d.pendingVersion()
	return v.Doc.Source.MovementID, ok
}

// pendingVersion is the version a pending item is about: the draft or amendment's draft in progress, or the valid
// issued version.
func (d *Document) pendingVersion() (Version, bool) {
	if !d.state.Kind.SeaHouse() {
		return Version{}, false
	}
	switch d.state.State {
	case StateDraft, StateApproved, StateAmending:
		return *d.latest(), true
	case StateIssued:
		return d.Current()
	}
	return Version{}, false
}

// PendingItems lists what stands between the document and a bill that says what SEA now says, from SEA's latest facts f
// of PendingMovement. A house that is not in the facts has only HOUSE_LEFT_MOVEMENT. Otherwise, with the facts not
// FINAL there is only SOURCE_NOT_FINAL: what they say may change at SEA's next finalization. With them FINAL, a draft,
// an approved bill or an amendment's draft has SOURCE_SUPERSEDED when it was built on other facts, and
// HOUSE_NOT_ALL_LOADED when the facts give the house no on-board date (a container carrying it has no loading, or the
// facts state no loading at all). An issued bill has neither: SEA finalizing again is no reason to touch it, so it has
// ISSUED_CONTENT_CHANGED only when what the facts now say of the house differs from what the bill states
// (OceanFacts.HouseChanges), naming what. An issued bill's items never block: it has no issue waiting.
func (d *Document) PendingItems(f OceanFacts) []PendingItem {
	v, ok := d.pendingVersion()
	if !ok {
		return nil
	}
	st := d.state
	inProgress := st.State != StateIssued
	item := func(kind, message string, blocking bool) PendingItem {
		return PendingItem{Kind: kind, DocumentID: st.DocumentID, ShipmentID: st.ShipmentID, Number: st.Number, Message: message, Blocking: blocking && inProgress}
	}
	h, here := f.house(st.ShipmentID)
	if !here {
		message := fmt.Sprintf("house %s (HBL %s) is not in SEA's latest facts of movement %s, version %d: whoever drafted the bill refreshes it "+
			"onto the movement the house was transferred to, naming that movement, or cancels it when the house was only detached", st.ShipmentID, st.Number, f.MovementID, f.Version)
		if !inProgress {
			message = fmt.Sprintf("house %s (HBL %s) is not in SEA's latest facts of movement %s, version %d: the issued bill stays valid as it is; if the house was "+
				"transferred to another movement, an amendment naming that movement moves the bill there, and if it was only detached the bill is cancelled",
				st.ShipmentID, st.Number, f.MovementID, f.Version)
		}
		return []PendingItem{item(PendingHouseLeft, message, true)}
	}
	if !f.Final {
		return []PendingItem{item(PendingSourceNotFinal, fmt.Sprintf("SEA's movement %s is open: SEA's operator finalizes it, and the bill is refreshed or issued after that", f.MovementID), true)}
	}
	if !inProgress {
		if changes := f.HouseChanges(v.Doc, st.ShipmentID); len(changes) > 0 {
			return []PendingItem{item(PendingIssuedChanged, fmt.Sprintf("SEA's facts version %d state %s of house %s differently from the issued bill (built on version %d): "+
				"the bill needs an amendment", f.Version, strings.Join(changes, ", "), st.ShipmentID, v.Doc.Source.Version), false)}
		}
		return nil
	}
	var out []PendingItem
	if f.Version != v.Doc.Source.Version || f.SHA256 != v.Doc.Source.SHA256 {
		out = append(out, item(PendingSourceSuperseded, fmt.Sprintf("the bill was built on SEA's facts version %d and SEA's latest final version is %d: "+
			"whoever drafted the bill refreshes the draft", v.Doc.Source.Version, f.Version), true))
	}
	if at, notLoaded := f.HouseOnBoard(h); at.IsZero() {
		message := fmt.Sprintf("SEA's facts state no loading of any container carrying house %s (HBL %s), so no on-board date: SEA's operator records the loading, "+
			"by the container's event or the carrier's report, and finalizes the movement again, then the bill is refreshed; it is not issued before", st.ShipmentID, st.Number)
		if len(notLoaded) > 0 {
			message = fmt.Sprintf("container %s carrying house %s (HBL %s) has no loading in SEA's facts: SEA's operator records its loading "+
				"and finalizes the movement again, then the bill is refreshed; it is not issued before", strings.Join(notLoaded, ", "), st.ShipmentID, st.Number)
		}
		out = append(out, item(PendingHouseNotAllLoaded, message, true))
	}
	return out
}
