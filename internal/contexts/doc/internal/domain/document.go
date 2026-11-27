package domain

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// State is a document's state.
type State string

const (
	StateDraft       State = "DRAFT"
	StateApproved    State = "APPROVED"
	StateIssued      State = "ISSUED"
	StateAmending    State = "AMENDING"
	StateSurrendered State = "SURRENDERED"
	StateCancelled   State = "CANCELLED"
)

// Version statuses, derived from what follows a version.
const (
	VersionDraft      = "DRAFT"
	VersionApproved   = "APPROVED"
	VersionIssued     = "ISSUED"
	VersionSuperseded = "SUPERSEDED"
	VersionDiscarded  = "DISCARDED"
	VersionCancelled  = "CANCELLED" // the version valid when the document was cancelled
)

// Approval of a version (OD-7 B: self-approval is allowed to designated roles and flagged).
type Approval struct {
	By           string
	At           time.Time
	SelfApproved bool
	EnteredAt    time.Time
}

// Credit bases of an issue (C7, OD-3 B).
const (
	CreditOK          = "FIN_OK"       // FIN's decision for the ordering party is OK
	CreditOverride    = "OVERRIDE"     // FIN has no decision; an authorized actor overrode with a reason
	CreditNotRequired = "NOT_REQUIRED" // a master has no ordering party
)

// Credit is what FIN said of the ordering party when DOC issued a house document.
type Credit struct {
	Basis      string
	DecisionID string
	By         string // the override's author
	Reason     string
}

// CreditDecision is FIN's credit decision as DOC reads it (CreditStatus v1); Status is OK, HOLD or NONE when FIN
// has none.
type CreditDecision struct {
	Status     string
	DecisionID string
}

// Issue of a version under an issuance authority in force.
type Issue struct {
	By               string
	At               time.Time
	AuthorityID      string
	AuthorityVersion int64
	Credit           Credit
	EnteredAt        time.Time
}

// Discard of a draft version an amendment abandoned.
type Discard struct {
	By        string
	At        time.Time
	Reason    string
	EnteredAt time.Time
}

// Version is one immutable version: its content as written, with approval, issue and discard recorded beside
// it, never in it.
type Version struct {
	No        int
	Content   []byte
	Doc       Content // read from Content
	SHA256    string
	Reason    string
	DraftedBy string
	DraftedAt time.Time
	EnteredAt time.Time
	Approval  *Approval
	Issue     *Issue
	Discard   *Discard
}

// DocumentState is a stored document, for persistence.
type DocumentState struct {
	TenantID        TenantID
	DocumentID      DocumentID
	Kind            Kind
	MovementID      string
	ConsolidationID string
	ShipmentID      string // a HAWB's shipment
	Number          string
	LegalEntityID   string
	State           State
	Versions        []Version
	CancelReason    string
	CancelledBy     string
	CancelledAt     time.Time
}

// Subject is what a document's number is issued for: a HAWB's shipment, a MAWB's movement.
func (st DocumentState) Subject() string {
	if st.Kind.House() {
		return st.ShipmentID
	}
	return st.MovementID
}

// Document is the aggregate root (TransportDocument).
type Document struct {
	state     DocumentState
	snapshot  []Version // the versions as persisted, with their records
	pending   []Event
	originals []Original // every original row, loaded and added, in order
	added     []Original // original rows written since the document was loaded
}

// Original is one immutable record of an original bill: the records of a serial are numbered from 1, and the one
// with the highest number is the original's status.
type Original struct {
	Version   int
	Serial    int
	Record    int
	Status    string
	By        string
	At        time.Time
	Evidence  string
	EnteredAt time.Time
}

// Event carries identifiers only.
type Event struct {
	Type            string
	At              time.Time
	TenantID        TenantID
	DocumentID      string
	Kind            string
	Version         int
	ShipmentID      string
	ConsolidationID string
	MovementID      string
	DocumentRef     string // a dispatch event's document
}

const (
	eventDrafted     = "TransportDocumentDrafted.v1"
	eventApproved    = "TransportDocumentApproved.v1"
	eventIssued      = "TransportDocumentIssued.v1"
	eventOpened      = "TransportDocumentAmendmentOpened.v1"
	eventAmended     = "TransportDocumentAmended.v1"
	eventAbandoned   = "TransportDocumentAmendmentAbandoned.v1"
	eventCancelled   = "TransportDocumentCancelled.v1"
	eventSurrendered = "OriginalBillsSurrendered.v1"
)

// DraftDocument drafts a document from its first version's content. A HAWB covers one FUL shipment and a MAWB
// one consolidation; the content's number is the one AIR holds for them.
func DraftDocument(tenant TenantID, id DocumentID, c Content, by string, at time.Time) (*Document, error) {
	if !identifier(string(tenant)) || !identifier(string(id)) || c.DocumentID != string(id) || !Kind(c.Kind).valid() || blank(c.Number) {
		return nil, ErrInvalidDocument
	}
	d := &Document{state: DocumentState{TenantID: tenant, DocumentID: id, Kind: Kind(c.Kind), MovementID: c.Source.MovementID,
		ConsolidationID: c.ConsolidationID, ShipmentID: c.ShipmentID, Number: c.Number, LegalEntityID: c.Issuer.LegalEntityID, State: StateDraft}}
	if d.state.Kind.House() == blank(d.state.ShipmentID) {
		return nil, ErrInvalidDocument
	}
	if err := d.addVersion(c, "", by, at); err != nil {
		return nil, err
	}
	d.record(eventDrafted, at)
	return d, nil
}

// addVersion writes the next version.
func (d *Document) addVersion(c Content, reason, by string, at time.Time) error {
	st := &d.state
	moved := c.Source.MovementID != st.MovementID || c.ConsolidationID != st.ConsolidationID
	if c.DocumentID != string(st.DocumentID) || Kind(c.Kind) != st.Kind || c.ShipmentID != st.ShipmentID || c.Number != st.Number ||
		c.Issuer.LegalEntityID != st.LegalEntityID || (moved && (!st.Kind.House() || len(st.Versions) == 0)) {
		return fmt.Errorf("%w: another shipment, number or issuer is another document, and so is another movement for a MAWB", ErrInvalidChange)
	}
	if len(st.Versions) > 0 && !text(reason) {
		return fmt.Errorf("%w: a version after the first needs a reason", ErrInvalidChange)
	}
	if err := d.changeable(by, at); err != nil {
		return err
	}
	content, err := json.Marshal(c)
	if err != nil {
		return err
	}
	st.Versions = append(st.Versions, Version{No: len(st.Versions) + 1, Content: content, Doc: c, SHA256: Digest(content), Reason: reason,
		DraftedBy: by, DraftedAt: at.UTC()})
	// a HAWB follows its shipment to the movement AIR now associates it with (a rollover): the amendment names the
	// new master, under the same number
	st.MovementID, st.ConsolidationID = c.Source.MovementID, c.ConsolidationID
	return nil
}

// Refresh writes a draft version from newer source facts, parties or entered fields: a draft or approved document
// returns to DRAFT and needs approval again; an amendment keeps AMENDING with the newer draft.
func (d *Document) Refresh(c Content, reason, by string, at time.Time) error {
	switch d.state.State {
	case StateDraft, StateApproved, StateAmending:
	default:
		return ErrInvalidTransition
	}
	if err := d.addVersion(c, reason, by, at); err != nil {
		return err
	}
	if d.state.State == StateApproved {
		d.state.State = StateDraft
	}
	d.record(eventDrafted, at)
	return nil
}

// Approve approves the draft version, or an amendment's draft, once it is complete for its kind. An approver who
// drafted it approves their own draft (OD-7 B), which the approval records. A draft becomes APPROVED; an
// amendment stays AMENDING with its draft approved.
func (d *Document) Approve(by string, at time.Time) error {
	v := d.latest()
	if !d.approvable() {
		return ErrInvalidTransition
	}
	if err := v.Doc.complete(); err != nil {
		return err
	}
	if err := d.changeable(by, at); err != nil {
		return err
	}
	v.Approval = &Approval{By: by, At: at.UTC(), SelfApproved: by == v.DraftedBy}
	if d.state.State == StateDraft {
		d.state.State = StateApproved
	}
	d.record(eventApproved, at)
	return nil
}

func (d *Document) approvable() bool {
	v := d.latest()
	return (d.state.State == StateDraft || d.state.State == StateAmending) && v.Approval == nil && v.Issue == nil && v.Discard == nil
}

// SelfApproval reports whether an actor would approve their own draft.
func (d *Document) SelfApproval(actor string) bool {
	return d.approvable() && d.latest().DraftedBy == actor
}

// IssueGate is what DOC checked before its transaction (T3) and found in its own: the facts and, for a house
// document, the order parties as they stand, the credit decision with the override asked for, and the issuance
// authority in force.
type IssueGate struct {
	Facts            Facts
	Ocean            OceanFacts
	Orders           []OrderParties
	Credit           CreditDecision
	OverrideReason   string
	IssuerVerified   bool // ORG reports the issuing legal entity VERIFIED at issue
	AuthorityID      string
	AuthorityVersion int64
}

// houseDrifts reports whether the facts' house of the shipment departs outside the window the customer agreed to, with
// its HBL or HAWB.
func (g IssueGate) houseDrifts(sea bool, shipment string) (string, bool) {
	if sea {
		for _, h := range g.Ocean.Houses {
			if h.ShipmentID == shipment {
				return h.HBL, h.WindowDrift
			}
		}
		return "", false
	}
	for _, h := range g.Facts.Houses {
		if h.ShipmentID == shipment {
			return h.HAWB, h.WindowDrift
		}
	}
	return "", false
}

// Issue issues the approved version.
func (d *Document) Issue(g IssueGate, by string, at time.Time) error {
	if d.state.State != StateApproved {
		return ErrInvalidTransition
	}
	if err := d.issue(g, by, at); err != nil {
		return err
	}
	d.state.State = StateIssued
	d.record(eventIssued, at)
	return nil
}

// OpenAmendment drafts the next version of an issued document; the issued version stays the valid document
// until the amendment is issued.
func (d *Document) OpenAmendment(c Content, reason, by string, at time.Time) error {
	if d.state.State != StateIssued {
		return ErrInvalidTransition
	}
	if err := d.originalsReturned(); err != nil {
		return err
	}
	if err := d.addVersion(c, reason, by, at); err != nil {
		return err
	}
	d.state.State = StateAmending
	d.record(eventOpened, at)
	return nil
}

// IssueAmendment issues the amendment's approved draft under the same number; the version it replaces is superseded.
func (d *Document) IssueAmendment(g IssueGate, by string, at time.Time) error {
	if d.state.State != StateAmending {
		return ErrInvalidTransition
	}
	if err := d.issue(g, by, at); err != nil {
		return err
	}
	d.state.State = StateIssued
	d.record(eventAmended, at)
	return nil
}

// AbandonAmendment discards the amendment's draft; the issued version stays. For a house document the order
// parties must be locked for the document again, unchanged since that version (C12).
func (d *Document) AbandonAmendment(orders []OrderParties, reason, by string, at time.Time) error {
	if d.state.State != StateAmending {
		return ErrInvalidTransition
	}
	if !text(reason) {
		return fmt.Errorf("%w: abandoning an amendment needs a reason", ErrInvalidChange)
	}
	if issued, ok := d.Current(); ok && d.state.Kind.House() {
		if err := d.locked(issued.Doc, orders); err != nil {
			return err
		}
	}
	if err := d.changeable(by, at); err != nil {
		return err
	}
	d.latest().Discard = &Discard{By: by, At: at.UTC(), Reason: reason}
	// the row follows the valid version again: the discarded draft may have moved it to another movement
	if issued, ok := d.Current(); ok {
		d.state.MovementID, d.state.ConsolidationID = issued.Doc.Source.MovementID, issued.Doc.ConsolidationID
	}
	// the originals returned for the amendment are handed back: the issued version is the document again
	if issued, ok := d.Current(); ok && issued.Doc.ReleaseType == ReleaseOriginals {
		for serial := 1; serial <= issued.Doc.OriginalCount; serial++ {
			if status, _ := d.originalStatus(issued.No, serial); status == OriginalReturned {
				d.addOriginal(Original{Version: issued.No, Serial: serial, Status: OriginalIssued, By: by, At: at.UTC(), Evidence: reason})
			}
		}
	}
	d.state.State = StateIssued
	d.record(eventAbandoned, at)
	return nil
}

// Cancel cancels a draft, approved, issued, amending or surrendered document with a reason; an amendment's draft
// is discarded with it. No original of the valid version may still be out: each is returned, or all were
// surrendered at origin. An issued number stays with its shipment or movement; the holder releases the order
// parties' lock in COM afterwards (C12).
func (d *Document) Cancel(reason, by string, at time.Time) error {
	switch d.state.State {
	case StateDraft, StateApproved, StateIssued, StateAmending, StateSurrendered:
	default:
		return ErrInvalidTransition
	}
	if cur, ok := d.Current(); ok && cur.Doc.ReleaseType == ReleaseOriginals {
		for serial := 1; serial <= cur.Doc.OriginalCount; serial++ {
			if status, _ := d.originalStatus(cur.No, serial); status != OriginalReturned && status != OriginalSurrendered {
				return fmt.Errorf("%w: original %d of version %d is still out; it is returned first", ErrOriginalsOut, serial, cur.No)
			}
		}
	}
	if !text(reason) {
		return fmt.Errorf("%w: a cancellation needs a reason", ErrInvalidChange)
	}
	if err := d.changeable(by, at); err != nil {
		return err
	}
	if d.state.State == StateAmending {
		d.latest().Discard = &Discard{By: by, At: at.UTC(), Reason: reason}
	}
	d.state.State, d.state.CancelReason, d.state.CancelledBy, d.state.CancelledAt = StateCancelled, reason, by, at.UTC()
	d.record(eventCancelled, at)
	return nil
}

// issue records the issue of the latest version after the gates: the version is approved and complete; the facts it
// was built on are still AIR's latest and FINAL; the issuer is still VERIFIED; a house document's orders report
// their parties locked for the document and unchanged; FIN's credit decision is OK, or absent and overridden with a
// reason (a master needs none); an authority is in force.
func (d *Document) issue(g IssueGate, by string, at time.Time) error {
	v := d.latest()
	src := v.Doc.Source
	if v.Approval == nil {
		return fmt.Errorf("%w: the version is not approved", ErrInvalidTransition)
	}
	if err := v.Doc.complete(); err != nil {
		return err
	}
	var notLoaded error
	if d.state.Kind.SeaHouse() {
		notLoaded = g.Ocean.houseNotLoaded(d.state.ShipmentID)
	}
	number, drifting := g.houseDrifts(d.state.Kind.SeaHouse(), d.state.ShipmentID)
	switch {
	case !g.IssuerVerified:
		return ErrIssuerNotVerified
	case d.state.Kind.SeaHouse() && (g.Ocean.MovementID != src.MovementID || !g.Ocean.Final || g.Ocean.Version != src.Version || g.Ocean.SHA256 != src.SHA256):
		return fmt.Errorf("%w: built on OceanDocFacts version %d, SEA's latest is %d (final %v)", ErrSourceStale, src.Version, g.Ocean.Version, g.Ocean.Final)
	case d.state.Kind.SeaHouse() && notLoaded != nil:
		return notLoaded
	case !d.state.Kind.SeaHouse() && (!g.Facts.Final || g.Facts.MovementID != src.MovementID || g.Facts.Version != src.Version || g.Facts.SHA256 != src.SHA256):
		return fmt.Errorf("%w: built on AirDocFacts version %d, AIR's latest is %d (final %v)", ErrSourceStale, src.Version, g.Facts.Version, g.Facts.Final)
	case d.state.Kind.House() && drifting:
		return fmt.Errorf("%w: house %s", ErrDateChangeUnanswered, number)
	case !identifier(g.AuthorityID) || g.AuthorityVersion < 1:
		return ErrIssuanceBlocked
	}
	credit := Credit{Basis: CreditNotRequired}
	if d.state.Kind.House() {
		if err := d.locked(v.Doc, g.Orders); err != nil {
			return err
		}
		switch g.Credit.Status {
		case "OK":
			credit = Credit{Basis: CreditOK, DecisionID: g.Credit.DecisionID}
		case "HOLD":
			return ErrCreditHold
		default:
			if !text(g.OverrideReason) {
				return ErrCreditUndecided
			}
			credit = Credit{Basis: CreditOverride, By: by, Reason: g.OverrideReason}
		}
	}
	if err := d.changeable(by, at); err != nil {
		return err
	}
	v.Issue = &Issue{By: by, At: at.UTC(), AuthorityID: g.AuthorityID, AuthorityVersion: g.AuthorityVersion, Credit: credit}
	if v.Doc.ReleaseType == ReleaseOriginals {
		for serial := 1; serial <= v.Doc.OriginalCount; serial++ {
			d.addOriginal(Original{Version: v.No, Serial: serial, Status: OriginalIssued, By: by, At: at.UTC()})
		}
	}
	return nil
}

// ReturnOriginal records that one original of the valid issued version was returned to the issuer, with evidence.
// An amendment of a document issued as originals waits until every original is returned, and so does a
// cancellation unless all were surrendered.
func (d *Document) ReturnOriginal(serial int, evidence, by string, at time.Time) error {
	cur, ok := d.Current()
	if d.state.State != StateIssued || !ok || cur.Doc.ReleaseType != ReleaseOriginals {
		return fmt.Errorf("%w: an original is returned on a document issued as originals", ErrInvalidTransition)
	}
	if status, _ := d.originalStatus(cur.No, serial); status != OriginalIssued {
		return fmt.Errorf("%w: original %d of version %d is not out", ErrInvalidTransition, serial, cur.No)
	}
	if !text(evidence) {
		return fmt.Errorf("%w: returning an original records the evidence", ErrInvalidChange)
	}
	if err := d.changeable(by, at); err != nil {
		return err
	}
	d.addOriginal(Original{Version: cur.No, Serial: serial, Status: OriginalReturned, By: by, At: at.UTC(), Evidence: evidence})
	return nil
}

// SurrenderOriginals records that every original of the valid version was surrendered to the issuer at origin
// (telex release), with evidence. An original that is already back with the issuer is surrendered with the rest:
// whether each is out or returned, the issuer ends up holding the whole set. The version stays the issued document;
// the document becomes SURRENDERED.
func (d *Document) SurrenderOriginals(evidence, by string, at time.Time) error {
	cur, ok := d.Current()
	if d.state.State != StateIssued || !ok || cur.Doc.ReleaseType != ReleaseOriginals {
		return fmt.Errorf("%w: originals are surrendered on an issued document that has them", ErrInvalidTransition)
	}
	if !text(evidence) {
		return fmt.Errorf("%w: a surrender records the evidence", ErrInvalidChange)
	}
	if err := d.changeable(by, at); err != nil {
		return err
	}
	for serial := 1; serial <= cur.Doc.OriginalCount; serial++ {
		d.addOriginal(Original{Version: cur.No, Serial: serial, Status: OriginalSurrendered, By: by, At: at.UTC(), Evidence: evidence})
	}
	d.state.State = StateSurrendered
	d.record(eventSurrendered, at)
	return nil
}

// originalsReturned refuses an amendment of an originals document until every original of the valid version is back.
func (d *Document) originalsReturned() error {
	cur, ok := d.Current()
	if !ok || cur.Doc.ReleaseType != ReleaseOriginals {
		return nil
	}
	if !d.originalsAll(cur.No, cur.Doc.OriginalCount, OriginalReturned) {
		return fmt.Errorf("%w: the originals of version %d are returned before it is amended", ErrOriginalsOut, cur.No)
	}
	return nil
}

// addOriginal writes the next record of an original.
func (d *Document) addOriginal(o Original) {
	o.Record = 1
	for _, x := range d.originals {
		if x.Version == o.Version && x.Serial == o.Serial && x.Record >= o.Record {
			o.Record = x.Record + 1
		}
	}
	d.originals = append(d.originals, o)
	d.added = append(d.added, o)
}

// originalStatus is the status of an original: that of its record with the highest number.
func (d *Document) originalStatus(version, serial int) (string, bool) {
	status, record := "", 0
	for _, o := range d.originals {
		if o.Version == version && o.Serial == serial && o.Record > record {
			status, record = o.Status, o.Record
		}
	}
	return status, record > 0
}

func (d *Document) originalsAll(version, count int, status string) bool {
	if count < 1 {
		return false
	}
	for serial := 1; serial <= count; serial++ {
		got, ok := d.originalStatus(version, serial)
		if !ok || got != status {
			return false
		}
	}
	return true
}

// RememberOriginals loads the original records already stored. It is called when the document is read.
func (d *Document) RememberOriginals(rows []Original) {
	d.originals = append([]Original(nil), rows...)
	d.added = nil
}

// Originals returns every original record, by version, serial and record number.
func (d *Document) Originals() []Original {
	out := append([]Original(nil), d.originals...)
	slices.SortStableFunc(out, func(a, b Original) int {
		return cmp.Or(cmp.Compare(a.Version, b.Version), cmp.Compare(a.Serial, b.Serial), cmp.Compare(a.Record, b.Record))
	})
	return out
}

// AddedOriginals returns the original rows written since the document was loaded.
func (d *Document) AddedOriginals() []Original { return append([]Original(nil), d.added...) }

// locked checks that COM reports every order of a house version with its parties locked for the document and
// unchanged since the version froze them (C12).
func (d *Document) locked(c Content, orders []OrderParties) error {
	byID := map[string]OrderParties{}
	for _, o := range orders {
		byID[o.OrderID] = o
	}
	for _, ref := range c.Orders {
		o, ok := byID[ref.OrderID]
		if !ok {
			return fmt.Errorf("%w: COM's parties of order %s were not read", ErrNotLocked, ref.OrderID)
		}
		held := false
		for _, h := range o.Locks {
			held = held || h == string(d.state.DocumentID)
		}
		if !held {
			return fmt.Errorf("%w: order %s", ErrNotLocked, ref.OrderID)
		}
		var notify int64
		if o.Notify != nil {
			notify = o.Notify.Version
		}
		if o.Shipper == nil || o.Consignee == nil || o.Shipper.Version != ref.ShipperVersion || o.Consignee.Version != ref.ConsigneeVersion ||
			notify != ref.NotifyVersion || o.Transport != ref.Transport {
			return fmt.Errorf("%w: order %s's parties or transport changed", ErrSourceStale, ref.OrderID)
		}
	}
	return nil
}

func (d *Document) latest() *Version { return &d.state.Versions[len(d.state.Versions)-1] }

// Current returns the valid issued version: the latest issued one.
func (d *Document) Current() (Version, bool) {
	for i := len(d.state.Versions) - 1; i >= 0; i-- {
		if d.state.Versions[i].Issue != nil {
			return d.state.Versions[i], true
		}
	}
	return Version{}, false
}

// Latest returns the latest version: the draft in progress, or the issued one.
func (d *Document) Latest() Version { return *d.latest() }

// Status returns a version's status.
func (d *Document) Status(no int) string {
	v := d.state.Versions[no-1]
	if v.Issue != nil {
		current, _ := d.Current()
		switch {
		case current.No != no:
			return VersionSuperseded
		case d.state.State == StateCancelled:
			return VersionCancelled
		}
		return VersionIssued
	}
	if no == len(d.state.Versions) && v.Discard == nil {
		switch {
		case d.state.State == StateApproved, d.state.State == StateAmending && v.Approval != nil:
			return VersionApproved
		case d.state.State == StateDraft, d.state.State == StateAmending:
			return VersionDraft
		}
	}
	return VersionDiscarded
}

func (d *Document) changeable(by string, at time.Time) error {
	if blank(by) || at.IsZero() || at.Before(d.lastChange()) {
		return fmt.Errorf("%w: author and business time not before the document's last change", ErrInvalidChange)
	}
	return nil
}

func (d *Document) lastChange() time.Time {
	last := d.state.CancelledAt
	for _, v := range d.state.Versions {
		last = later(last, v.DraftedAt)
		if v.Approval != nil {
			last = later(last, v.Approval.At)
		}
		if v.Issue != nil {
			last = later(last, v.Issue.At)
		}
		if v.Discard != nil {
			last = later(last, v.Discard.At)
		}
	}
	for _, o := range d.originals {
		last = later(last, o.At)
	}
	return last
}

func (d *Document) record(kind string, at time.Time) {
	st := d.state
	d.pending = append(d.pending, Event{Type: kind, At: at.UTC(), TenantID: st.TenantID, DocumentID: string(st.DocumentID), Kind: string(st.Kind),
		Version: len(st.Versions), ShipmentID: st.ShipmentID, ConsolidationID: st.ConsolidationID, MovementID: st.MovementID})
}

// Stamp records when DOC entered what was written since the document was loaded (T1).
func (d *Document) Stamp(entered time.Time) {
	for i := range d.originals {
		if d.originals[i].EnteredAt.IsZero() {
			d.originals[i].EnteredAt = entered.UTC()
		}
	}
	for i := range d.added {
		if d.added[i].EnteredAt.IsZero() {
			d.added[i].EnteredAt = entered.UTC()
		}
	}
	for i := range d.state.Versions {
		v := &d.state.Versions[i]
		if v.EnteredAt.IsZero() {
			v.EnteredAt = entered.UTC()
		}
		for _, at := range []*time.Time{approvalEntered(v), issueEntered(v), discardEntered(v)} {
			if at != nil && at.IsZero() {
				*at = entered.UTC()
			}
		}
	}
}

func approvalEntered(v *Version) *time.Time {
	if v.Approval == nil {
		return nil
	}
	return &v.Approval.EnteredAt
}

func issueEntered(v *Version) *time.Time {
	if v.Issue == nil {
		return nil
	}
	return &v.Issue.EnteredAt
}

func discardEntered(v *Version) *time.Time {
	if v.Discard == nil {
		return nil
	}
	return &v.Discard.EnteredAt
}

// RestoreDocument rebuilds a stored document from its versions' bytes, without events.
func RestoreDocument(st DocumentState) (*Document, error) {
	st.Versions = append([]Version(nil), st.Versions...)
	d := &Document{state: st}
	for i := range d.state.Versions {
		v := &d.state.Versions[i]
		var c Content
		if v.No != i+1 || len(v.Content) == 0 || Digest(v.Content) != v.SHA256 || json.Unmarshal(v.Content, &c) != nil {
			return nil, fmt.Errorf("%w: version %d content", ErrInvalidDocument, i+1)
		}
		v.Doc = c
	}
	if err := d.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDocument, err)
	}
	d.snapshot = append([]Version(nil), d.state.Versions...)
	return d, nil
}

func (d *Document) validate() error {
	st := d.state
	n := len(st.Versions)
	if !identifier(string(st.TenantID)) || !identifier(string(st.DocumentID)) || !st.Kind.valid() || blank(st.Number) || n == 0 ||
		(st.Kind.House()) == blank(st.ShipmentID) {
		return fmt.Errorf("identity")
	}
	for i, v := range st.Versions {
		c := v.Doc
		if c.DocumentID != string(st.DocumentID) || c.Kind != string(st.Kind) || c.Number != st.Number || c.ShipmentID != st.ShipmentID ||
			c.Issuer.LegalEntityID != st.LegalEntityID || (i == n-1 && !d.rowFollows(v)) ||
			(st.Kind == KindMAWB && c.Source.MovementID != st.MovementID) || blank(v.DraftedBy) || v.DraftedAt.IsZero() || (i > 0 && blank(v.Reason)) ||
			(v.Issue != nil && v.Discard != nil) || (v.Approval != nil && v.Approval.At.Before(v.DraftedAt)) ||
			(v.Issue != nil && v.Issue.At.Before(v.DraftedAt)) {
			return fmt.Errorf("version %d", i+1)
		}
	}
	for _, v := range st.Versions {
		if v.Issue != nil && v.Approval == nil {
			return fmt.Errorf("version %d issued unapproved", v.No)
		}
	}
	last := st.Versions[n-1]
	_, issued := d.Current()
	switch st.State {
	case StateDraft:
		if last.Issue != nil || last.Approval != nil || issued {
			return fmt.Errorf("DRAFT")
		}
	case StateApproved:
		if last.Issue != nil || last.Approval == nil || issued {
			return fmt.Errorf("APPROVED")
		}
	case StateIssued, StateSurrendered:
		if !issued || (last.Issue == nil && last.Discard == nil) {
			return fmt.Errorf("ISSUED")
		}
	case StateAmending:
		if !issued || last.Issue != nil || last.Discard != nil {
			return fmt.Errorf("AMENDING")
		}
	case StateCancelled:
		if blank(st.CancelReason) || blank(st.CancelledBy) || st.CancelledAt.IsZero() {
			return fmt.Errorf("CANCELLED")
		}
	default:
		return fmt.Errorf("state %s", st.State)
	}
	return nil
}

// rowFollows reports whether the document row's movement and consolidation are those of v, the latest version, or of
// the latest version that was not discarded: since V2.90 an abandoned amendment puts the row back on the valid
// version's movement, and a row written before that names the discarded draft's, so both are the row of a sound
// document.
func (d *Document) rowFollows(latest Version) bool {
	names := func(v Version) bool {
		return v.Doc.Source.MovementID == d.state.MovementID && v.Doc.ConsolidationID == d.state.ConsolidationID
	}
	if names(latest) {
		return true
	}
	for i := len(d.state.Versions) - 1; i >= 0; i-- {
		if v := d.state.Versions[i]; v.Discard == nil {
			return names(v)
		}
	}
	return false
}

// State returns a copy of the document for persistence and read models.
func (d *Document) State() DocumentState {
	st := d.state
	st.Versions = append([]Version(nil), st.Versions...)
	return st
}

func (d *Document) ID() DocumentID   { return d.state.DocumentID }
func (d *Document) Pending() []Event { return append([]Event(nil), d.pending...) }

// Added returns the versions written since the document was loaded, and the approvals, issues and discards
// written since on versions of any age.
func (d *Document) Added() (versions []Version, approvals, issues, discards []Version) {
	for i, v := range d.state.Versions {
		if i >= len(d.snapshot) {
			versions = append(versions, v)
		}
		var was Version
		if i < len(d.snapshot) {
			was = d.snapshot[i]
		}
		if v.Approval != nil && was.Approval == nil {
			approvals = append(approvals, v)
		}
		if v.Issue != nil && was.Issue == nil {
			issues = append(issues, v)
		}
		if v.Discard != nil && was.Discard == nil {
			discards = append(discards, v)
		}
	}
	return versions, approvals, issues, discards
}

// Acknowledge is permitted only after the transaction holding the document and its Outbox events commits.
func (d *Document) Acknowledge() {
	d.pending = nil
	d.added = nil
	d.snapshot = append([]Version(nil), d.state.Versions...)
}
