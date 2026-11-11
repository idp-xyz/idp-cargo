package domain

import (
	"errors"
	"fmt"
	"time"
)

// DOC V0.2, DocumentDispatch with the P5 records: every copy of a document that leaves the tenant is a dispatch to
// one recipient over one channel. Contacts appear on the copy only with the consent the recipient needs (PIPL
// arts. 23, 39; privacy assessment V1.2 sections 5 and 6); each contact's decision is recorded with identifiers
// only, never a name, phone or e-mail.

var (
	ErrNotDispatchable     = errors.New("doc: the version cannot be sent: an issued version, or a draft to the ordering party for approval")
	ErrTransferBlocked     = errors.New("doc: DOC_TRANSFER_BLOCKED: the tenant's register of persons whose contacts went abroad this year is blocked; a standard contract or certification lifts it")
	ErrTransferNotReserved = errors.New("doc: DOC_TRANSFER_NOT_RESERVED: ORG's register does not hold the dispatch's cross-border contacts; the API client reserves them before sending")
	ErrInvalidDispatch     = errors.New("doc: invalid dispatch")
	ErrCopyChanged         = errors.New("doc: DOC_COPY_CHANGED: the copy can no longer be made as recorded: a contact on it changed or was erased since")
)

// Recipient roles, channels, classifications, legal paths and contact statuses of a dispatch.
const (
	RecipientOrderingParty    = "ORDERING_PARTY"
	RecipientShipper          = "SHIPPER"
	RecipientConsignee        = "CONSIGNEE"
	RecipientNotifyParty      = "NOTIFY_PARTY"
	RecipientDestinationAgent = "DESTINATION_AGENT"
	RecipientCarrier          = "CARRIER"
	RecipientCustomsBroker    = "CUSTOMS_BROKER"

	ChannelEmailPDF      = "EMAIL_PDF"
	ChannelPrintHandover = "PRINT_HANDOVER"
	ChannelPortal        = "PORTAL"

	NotProvided = "NOT_PROVIDED" // the recipient is the ordering party
	Provision   = "PROVISION"    // another party in mainland China
	CrossBorder = "CROSS_BORDER" // a recipient abroad, Hong Kong, Macao and Taiwan included

	PathNone              = "NONE"
	PathConsent           = "CONSENT"
	PathThresholdExempt   = "CONSENT_WITH_THRESHOLD_EXEMPTION"
	PathStandardContract  = "STANDARD_CONTRACT"
	PathCertification     = "CERTIFICATION"
	ContactIncluded       = "INCLUDED"
	ContactOmitted        = "OMITTED"
	OmitNoConsent         = "NO_CONSENT_RECORDED"
	OmitNoProvideConsent  = "NO_CONSENT_TO_PROVIDE"
	OmitNoTransferConsent = "NO_CONSENT_TO_TRANSFER"
	OmitErased            = "ERASED"
)

func validRecipientRole(r string) bool {
	switch r {
	case RecipientOrderingParty, RecipientShipper, RecipientConsignee, RecipientNotifyParty, RecipientDestinationAgent, RecipientCarrier,
		RecipientCustomsBroker:
		return true
	}
	return false
}

func validChannel(c string) bool {
	return c == ChannelEmailPDF || c == ChannelPrintHandover || c == ChannelPortal
}

// Recipient is an ORG party with its registration country and its role for the dispatch.
type Recipient struct {
	PartyID string
	Country string
	Role    string
}

// Consent is a contact's consent as COM froze it (privacy decision P4).
type Consent struct {
	NoticeVersion   string
	ProvideToOthers bool
	CrossBorder     bool
	Basis           string
	EvidenceRef     string
	ORGRevision     int64
}

// ContactSource is a contact of a document party as COM holds it: its key, the party role and order, its consent
// and, for the copy only, the details, which DOC never stores.
type ContactSource struct {
	Key          string // ORG:<contact id> or COM:<order>:<role>:<party version>
	PartyRole    string
	OrderID      string
	PartyVersion int64
	Consent      *Consent
	Erased       bool
	Name         string
	Phone        string
	Mobile       string
	Email        string
}

// DispatchContact is the provision record of one contact: identifiers and consent only.
type DispatchContact struct {
	No             int
	Key            string
	PartyRole      string
	OrderID        string
	Consent        *Consent
	Classification string
	LegalPath      string
	Status         string
	OmitReason     string
}

// RegisterView is what DOC reads of ORG's ContactTransferRegister v1 when it prepares a dispatch.
type RegisterView struct {
	Year          int
	Persons       int64
	State         string // OK, WARNING or BLOCKED
	SafeguardKind string // STANDARD_CONTRACT, CERTIFICATION, or empty
	DispatchKeys  []string
}

// Decide decides each contact for a recipient (OD-5 A: a contact without the consent the recipient needs is left
// off the copy, with the reason): the ordering party itself is NOT_PROVIDED; a recipient in mainland China needs
// consent to provide to others; one abroad needs consent to transfer abroad too, and the register must not be
// blocked; the legal path abroad is the threshold exemption, or the tenant's safeguard once it is past the block.
func Decide(r Recipient, orderingParty string, contacts []ContactSource, register RegisterView) ([]DispatchContact, error) {
	var out []DispatchContact
	for i, c := range contacts {
		d := DispatchContact{No: i + 1, Key: c.Key, PartyRole: c.PartyRole, OrderID: c.OrderID, Consent: c.Consent, Status: ContactIncluded, LegalPath: PathNone}
		switch {
		case r.PartyID == orderingParty:
			d.Classification = NotProvided
		case r.Country == "CN":
			d.Classification = Provision
		default:
			d.Classification = CrossBorder
		}
		omit := func(reason string) { d.Status, d.OmitReason, d.LegalPath = ContactOmitted, reason, PathNone }
		switch {
		case c.Erased:
			omit(OmitErased)
		case d.Classification == NotProvided:
		case c.Consent == nil:
			omit(OmitNoConsent)
		case !c.Consent.ProvideToOthers:
			omit(OmitNoProvideConsent)
		case d.Classification == Provision:
			d.LegalPath = PathConsent
		case !c.Consent.CrossBorder:
			omit(OmitNoTransferConsent)
		default:
			if register.State == "BLOCKED" {
				return nil, ErrTransferBlocked
			}
			d.LegalPath = PathThresholdExempt
			switch register.SafeguardKind {
			case "STANDARD_CONTRACT":
				d.LegalPath = PathStandardContract
			case "CERTIFICATION":
				d.LegalPath = PathCertification
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// Dispatch states and outcomes.
const (
	DispatchPrepared = "PREPARED"
	DispatchSent     = "SENT"
	DispatchFailed   = "FAILED"
	DispatchUnknown  = "UNKNOWN"
)

// Outcome is an outcome recorded for a dispatch: the channel's reference or evidence.
type Outcome struct {
	No        int
	Outcome   string
	Reference string
	By        string
	At        time.Time
	EnteredAt time.Time
}

// DispatchState is a stored dispatch, for persistence.
type DispatchState struct {
	TenantID      TenantID
	DispatchID    string
	DocumentID    DocumentID
	Version       int
	Kind          Kind
	VersionStatus string // the version's status the copy shows
	Recipient     Recipient
	OrderingParty string // the ordering party of a house's first order, as COM reported it
	Channel       string
	Contacts      []DispatchContact
	CopySHA256    string
	Template      string
	RegisterYear  int
	RegisterState string
	PreparedBy    string
	PreparedAt    time.Time
	EnteredAt     time.Time
	Outcomes      []Outcome
}

// Dispatch is the aggregate root (DocumentDispatch).
type Dispatch struct {
	state    DispatchState
	outcomes int
	pending  []Event
}

const (
	eventDispatchPrepared = "DocumentDispatchPrepared.v1"
	eventDispatched       = "DocumentDispatched.v1"
	eventDispatchFailed   = "DocumentDispatchFailed.v1"
	eventDispatchUnknown  = "DocumentDispatchOutcomeUnknown.v1"
)

// PrepareDispatch records a copy of a document version to a recipient over a channel, with each contact's decision
// and the copy's SHA-256. The version must be the document's issued one, or a draft sent to the ordering party.
func PrepareDispatch(st DispatchState, document *Document, by string, at time.Time) (*Dispatch, error) {
	d := document.State()
	switch {
	case !identifier(st.DispatchID) || st.DocumentID != d.DocumentID || st.Version < 1 || st.Version > len(d.Versions) || !identifier(st.Recipient.PartyID) ||
		len(st.Recipient.Country) != 2 || !validRecipientRole(st.Recipient.Role) || !validChannel(st.Channel) || len(st.CopySHA256) != 64 ||
		blank(st.Template) || blank(by) || at.IsZero():
		return nil, ErrInvalidDispatch
	}
	if st.Recipient.Role == RecipientOrderingParty && (st.OrderingParty == "" || st.Recipient.PartyID != st.OrderingParty) {
		return nil, fmt.Errorf("%w: the recipient is not the ordering party", ErrInvalidDispatch)
	}
	status := document.Status(st.Version)
	switch status {
	case VersionIssued:
	case VersionDraft, VersionApproved:
		if st.Recipient.Role != RecipientOrderingParty {
			return nil, fmt.Errorf("%w: a draft goes to the ordering party only", ErrNotDispatchable)
		}
	default:
		return nil, ErrNotDispatchable
	}
	if st.VersionStatus != status {
		return nil, fmt.Errorf("%w: version %d is %s now", ErrConflict, st.Version, status)
	}
	st.TenantID, st.Kind, st.PreparedBy, st.PreparedAt, st.Outcomes = d.TenantID, d.Kind, by, at.UTC(), nil
	for i, c := range st.Contacts {
		if c.No != i+1 || blank(c.Key) || (c.Status == ContactOmitted) == blank(c.OmitReason) {
			return nil, ErrInvalidDispatch
		}
	}
	x := &Dispatch{state: st}
	x.record(eventDispatchPrepared, at)
	return x, nil
}

// CrossBorderKeys returns the keys of the contacts sent abroad on the copy, which ORG's register must hold before
// the copy is sent.
func (x *Dispatch) CrossBorderKeys() []string {
	var keys []string
	for _, c := range x.state.Contacts {
		if c.Classification == CrossBorder && c.Status == ContactIncluded {
			keys = append(keys, c.Key)
		}
	}
	return keys
}

// State returns the dispatch's state: PREPARED, or its last outcome.
func (x *Dispatch) Current() string {
	if n := len(x.state.Outcomes); n > 0 {
		return x.state.Outcomes[n-1].Outcome
	}
	return DispatchPrepared
}

// RecordOutcome records what the channel reported: SENT or UNKNOWN once ORG's register holds the dispatch's
// cross-border contacts (reserved lists the keys ORG reports for the dispatch), FAILED with its evidence. PREPARED
// and UNKNOWN take an outcome; SENT and FAILED are final.
func (x *Dispatch) RecordOutcome(outcome, reference string, reserved []string, by string, at time.Time) error {
	switch x.Current() {
	case DispatchPrepared:
	case DispatchUnknown:
		if outcome == DispatchUnknown {
			return ErrInvalidTransition
		}
	default:
		return ErrInvalidTransition
	}
	if outcome != DispatchSent && outcome != DispatchFailed && outcome != DispatchUnknown {
		return fmt.Errorf("%w: outcome %q", ErrInvalidDispatch, outcome)
	}
	if (outcome != DispatchUnknown && !text(reference)) || blank(by) || at.Before(x.lastChange()) {
		return fmt.Errorf("%w: a sent or failed outcome needs the channel's reference or evidence, its author and a time not before the dispatch", ErrInvalidDispatch)
	}
	if outcome != DispatchFailed {
		held := map[string]bool{}
		for _, k := range reserved {
			held[k] = true
		}
		for _, k := range x.CrossBorderKeys() {
			if !held[k] {
				return ErrTransferNotReserved
			}
		}
	}
	x.state.Outcomes = append(x.state.Outcomes, Outcome{No: len(x.state.Outcomes) + 1, Outcome: outcome, Reference: reference, By: by, At: at.UTC()})
	switch outcome {
	case DispatchSent:
		x.record(eventDispatched, at)
	case DispatchFailed:
		x.record(eventDispatchFailed, at)
	default:
		x.record(eventDispatchUnknown, at)
	}
	return nil
}

func (x *Dispatch) lastChange() time.Time {
	last := x.state.PreparedAt
	for _, o := range x.state.Outcomes {
		last = later(last, o.At)
	}
	return last
}

func (x *Dispatch) record(kind string, at time.Time) {
	st := x.state
	x.pending = append(x.pending, Event{Type: kind, At: at.UTC(), TenantID: st.TenantID, DocumentID: st.DispatchID, Kind: string(st.Kind), Version: st.Version,
		DocumentRef: string(st.DocumentID)})
}

// Stamp records when DOC entered what was written since the dispatch was loaded (T1).
func (x *Dispatch) Stamp(entered time.Time) {
	if x.state.EnteredAt.IsZero() {
		x.state.EnteredAt = entered.UTC()
	}
	for i := range x.state.Outcomes {
		if x.state.Outcomes[i].EnteredAt.IsZero() {
			x.state.Outcomes[i].EnteredAt = entered.UTC()
		}
	}
}

// RestoreDispatch rebuilds a stored dispatch.
func RestoreDispatch(st DispatchState) (*Dispatch, error) {
	st.Contacts = append([]DispatchContact(nil), st.Contacts...)
	st.Outcomes = append([]Outcome(nil), st.Outcomes...)
	for i, o := range st.Outcomes {
		if o.No != i+1 || blank(o.By) || o.At.IsZero() || (i > 0 && st.Outcomes[i-1].Outcome != DispatchUnknown) {
			return nil, fmt.Errorf("%w: outcome %d", ErrInvalidDispatch, i+1)
		}
	}
	return &Dispatch{state: st, outcomes: len(st.Outcomes)}, nil
}

// State returns a copy for persistence and read models.
func (x *Dispatch) State() DispatchState {
	st := x.state
	st.Contacts = append([]DispatchContact(nil), st.Contacts...)
	st.Outcomes = append([]Outcome(nil), st.Outcomes...)
	return st
}

// AddedOutcomes returns the outcomes recorded since the dispatch was loaded.
func (x *Dispatch) AddedOutcomes() []Outcome {
	return append([]Outcome(nil), x.state.Outcomes[x.outcomes:]...)
}

func (x *Dispatch) Pending() []Event { return append([]Event(nil), x.pending...) }

// Acknowledge is permitted only after the transaction holding the dispatch and its Outbox events commits.
func (x *Dispatch) Acknowledge() { x.pending, x.outcomes = nil, len(x.state.Outcomes) }
