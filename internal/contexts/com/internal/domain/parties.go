package domain

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// TransportMode is the transport mode the customer asks for (C11), as SAP TM forwarding orders carry it.
type TransportMode string

const (
	ModeAir  TransportMode = "AIR"
	ModeSea  TransportMode = "SEA"
	ModeRoad TransportMode = "ROAD"
	ModeRail TransportMode = "RAIL"
)

// MovementType says where the forwarder's responsibility starts and ends (C11, C14): at the shipper's or the
// consignee's door, or at the port of loading or discharge.
type MovementType string

const (
	DoorToDoor MovementType = "DOOR_TO_DOOR"
	DoorToPort MovementType = "DOOR_TO_PORT"
	PortToDoor MovementType = "PORT_TO_DOOR"
	PortToPort MovementType = "PORT_TO_PORT"
)

// Transport is the requested transport mode and movement type of an order; either may be absent.
type Transport struct {
	Mode     TransportMode
	Movement MovementType
}

func (t Transport) valid() bool {
	switch t.Mode {
	case "", ModeAir, ModeSea, ModeRoad, ModeRail:
	default:
		return false
	}
	switch t.Movement {
	case "", DoorToDoor, DoorToPort, PortToDoor, PortToPort:
		return true
	}
	return false
}

// PickupAtDoor reports whether the movement starts at the shipper's door, so the order needs a pickup address.
func (t Transport) PickupAtDoor() bool { return t.Movement == DoorToDoor || t.Movement == DoorToPort }

// DeliveryAtDoor reports whether the movement ends at the consignee's door, so the order needs a delivery address.
func (t Transport) DeliveryAtDoor() bool { return t.Movement == DoorToDoor || t.Movement == PortToDoor }

// TransportVersion is one version of an order's transport: version 1 when given with the order, later ones
// with a reason (C11, C12).
type TransportVersion struct {
	Transport
	Version   int64
	Reason    string
	ChangedBy string
	ChangedAt time.Time
	EnteredAt time.Time
}

// Order party roles (C9). The ordering party is the order's Customer snapshot, not one of these.
const (
	PartyShipper   = "SHIPPER"
	PartyConsignee = "CONSIGNEE"
	PartyNotify    = "NOTIFY_PARTY"
)

// PartyKind says whether an order party is an ORG party or, for the consignee of an ocean bill, the words
// TO ORDER (C9).
type PartyKind string

const (
	KindParty   PartyKind = "PARTY"
	KindToOrder PartyKind = "TO_ORDER"
)

// AddressSource says where a frozen address comes from: ORG's address book, or the order itself, which
// overrides ORG without writing back (C9).
type AddressSource string

const (
	FromORG   AddressSource = "ORG"
	FromOrder AddressSource = "ORDER"
)

var (
	ErrInvalidParty    = errors.New("com: invalid order party")
	ErrPartyRule       = errors.New("com: the order parties break a rule of the transport")
	ErrAddressRequired = errors.New("com: the order party needs an address that ORG does not provide by default")
)

var toOrderForm = regexp.MustCompile(`^TO ORDER( OF \S.*)?$`)

// PartyIdentifier is an identifier of an ORG party, frozen with the party (ORG V0.4).
type PartyIdentifier struct {
	Type    string
	Country string
	Value   string
	None    bool
}

// PartyRecord is an ORG party as frozen on the order: its revision, names, registration country and
// identifiers, and, for shipper and consignee, the role ORG confirmed for the quote's legal entity (C9). A
// notify party holds no role (C13).
type PartyRecord struct {
	PartyID         string
	PartyRevision   int64
	LegalName       string
	EnglishName     string
	CountryCode     string
	Identifiers     []PartyIdentifier
	RoleCode        string
	RoleValidity    kernel.ValidityInterval
	RoleEvidenceRef string
}

// LocalAddress is the frozen local-language version of an address.
type LocalAddress struct {
	Language string
	Name     string
	Lines    []string
	City     string
	Region   string
}

// AddressSnapshot is an address frozen on the order. From ORG it names the ORG address, short code and
// version it copies; from the order it is an override with who and why, naming the ORG address it replaced
// when there was one. ORG never learns of an override (C9).
type AddressSnapshot struct {
	Source         AddressSource
	ORGAddressID   string
	ShortCode      string
	ORGVersion     int64
	Name           string
	Lines          []string
	City           string
	Region         string
	Postcode       string
	CountryCode    string
	UNLocode       string
	Residential    bool
	Phone          string
	Email          string
	Local          *LocalAddress
	OverriddenBy   string
	OverrideReason string
}

// ContactSnapshot is a contact frozen on the order: personal data, stored apart from the order and never in
// events (privacy assessment V1.1, controls 4-7). From ORG it names the ORG contact and version.
type ContactSnapshot struct {
	Source         AddressSource
	ORGContactID   string
	ORGVersion     int64
	Name           string
	Phone          string
	Mobile         string
	Email          string
	OverriddenBy   string
	OverrideReason string
}

// OrderParty is one version of an order party. A named party has its ORG record and a documentary address;
// a TO ORDER consignee has only its text. The shipper may have a pickup and the consignee a delivery
// address (C14); a named party may have a contact. Contact is nil on stored versions read without contacts.
type OrderParty struct {
	Role        string
	Version     int64
	Kind        PartyKind
	ToOrderText string
	Party       PartyRecord
	Address     *AddressSnapshot
	Pickup      *AddressSnapshot
	Delivery    *AddressSnapshot
	Contact     *ContactSnapshot
	HasContact  bool
	Overridden  bool
	Reason      string
	SetBy       string
	SetAt       time.Time
	EnteredAt   time.Time
}

// SetParty records the next version of one order party (C9, C12): until the order is cancelled, with a reason
// when it replaces an earlier version. The parties must fit the order's transport (C11, C14).
func (o *CustomerOrder) SetParty(p OrderParty, by string, at, entered time.Time) error {
	if o.state.Status == OrderCancelled {
		return fmt.Errorf("%w: the order is cancelled", ErrInvalidTransition)
	}
	if blank(by) || at.IsZero() || entered.IsZero() {
		return ErrInvalidChange
	}
	current, has := o.Party(p.Role)
	p.Version, p.SetBy, p.SetAt, p.EnteredAt = 1, by, at.UTC(), entered.UTC()
	if has {
		p.Version = current.Version + 1
		if blank(p.Reason) {
			return fmt.Errorf("%w: replacing the %s needs a reason", ErrInvalidChange, p.Role)
		}
	}
	p, err := p.normalized()
	if err != nil {
		return err
	}
	parties := o.currentParties()
	parties[p.Role] = p
	if err = partyRules(o.Transport(), parties); err != nil {
		return err
	}
	o.state.Parties = append(o.state.Parties, p)
	o.recordParty(p, at)
	return nil
}

// ChangeTransport records the next version of the order's transport with a reason (C11). The order's parties
// must still fit: a TO ORDER consignee only on SEA, and a pickup or delivery address where the movement
// starts or ends at a door (C14).
func (o *CustomerOrder) ChangeTransport(t Transport, reason, by string, at, entered time.Time) error {
	if o.state.Status == OrderCancelled {
		return fmt.Errorf("%w: the order is cancelled", ErrInvalidTransition)
	}
	if blank(by) || blank(reason) || utf8.RuneCountInString(reason) > maxText || at.IsZero() || entered.IsZero() {
		return ErrInvalidChange
	}
	if !t.valid() {
		return fmt.Errorf("%w: transport %+v", ErrInvalidChange, t)
	}
	if t == o.Transport() {
		return fmt.Errorf("%w: the transport is unchanged", ErrInvalidChange)
	}
	if err := partyRules(t, o.currentParties()); err != nil {
		return err
	}
	v := TransportVersion{Transport: t, Version: int64(len(o.state.TransportVersions)) + 1, Reason: strings.TrimSpace(reason),
		ChangedBy: by, ChangedAt: at.UTC(), EnteredAt: entered.UTC()}
	o.state.TransportVersions = append(o.state.TransportVersions, v)
	e := o.event(eventTransportChanged, at)
	e.TransportVersion = v.Version
	o.pending = append(o.pending, e)
	return nil
}

// Transport returns the order's current transport; zero when none was given.
func (o *CustomerOrder) Transport() Transport {
	if n := len(o.state.TransportVersions); n > 0 {
		return o.state.TransportVersions[n-1].Transport
	}
	return Transport{}
}

// Party returns the current version of an order party.
func (o *CustomerOrder) Party(role string) (OrderParty, bool) {
	for i := len(o.state.Parties) - 1; i >= 0; i-- {
		if o.state.Parties[i].Role == role {
			return o.state.Parties[i], true
		}
	}
	return OrderParty{}, false
}

// Parties returns the current version of every order party, shipper first.
func (o *CustomerOrder) Parties() []OrderParty {
	var out []OrderParty
	for _, role := range []string{PartyShipper, PartyConsignee, PartyNotify} {
		if p, ok := o.Party(role); ok {
			out = append(out, p)
		}
	}
	return out
}

// AddedParties and AddedTransport return the versions written since the order was recorded or restored.
func (o *CustomerOrder) AddedParties() []OrderParty {
	return append([]OrderParty(nil), o.state.Parties[o.storedParties:]...)
}

func (o *CustomerOrder) AddedTransport() []TransportVersion {
	return append([]TransportVersion(nil), o.state.TransportVersions[o.storedTransport:]...)
}

func (o *CustomerOrder) currentParties() map[string]OrderParty {
	out := map[string]OrderParty{}
	for _, p := range o.state.Parties {
		out[p.Role] = p
	}
	return out
}

func (o *CustomerOrder) recordParty(p OrderParty, at time.Time) {
	e := o.event(eventPartySet, at)
	e.PartyRole, e.PartyVersion, e.PartyKind, e.Overridden = p.Role, p.Version, string(p.Kind), p.Overridden
	o.pending = append(o.pending, e)
}

// partyRules are the rules the parties and the transport keep together (C9, C11, C14): a TO ORDER consignee
// only on a SEA order and with a notify party; on an AIR order a named consignee; a pickup address for the
// shipper and a delivery address for the consignee where the movement starts or ends at a door.
func partyRules(t Transport, parties map[string]OrderParty) error {
	consignee, hasConsignee := parties[PartyConsignee]
	if hasConsignee && consignee.Kind == KindToOrder {
		if t.Mode != ModeSea {
			return fmt.Errorf("%w: a TO ORDER consignee needs a SEA order, the order is %q", ErrPartyRule, t.Mode)
		}
		if _, ok := parties[PartyNotify]; !ok {
			return fmt.Errorf("%w: a TO ORDER consignee needs a notify party", ErrPartyRule)
		}
	}
	if shipper, ok := parties[PartyShipper]; ok && t.PickupAtDoor() && shipper.Pickup == nil {
		return fmt.Errorf("%w: a %s movement needs the shipper's pickup address", ErrPartyRule, t.Movement)
	}
	if hasConsignee && t.DeliveryAtDoor() && consignee.Delivery == nil {
		return fmt.Errorf("%w: a %s movement needs the consignee's delivery address", ErrPartyRule, t.Movement)
	}
	return nil
}

// normalized checks one order party version on its own.
func (p OrderParty) normalized() (OrderParty, error) {
	bad := func(what string) (OrderParty, error) {
		return OrderParty{}, fmt.Errorf("%w: %s %s", ErrInvalidParty, p.Role, what)
	}
	p.Reason = strings.TrimSpace(p.Reason)
	if utf8.RuneCountInString(p.Reason) > maxText {
		return bad("reason")
	}
	switch p.Role {
	case PartyShipper, PartyConsignee, PartyNotify:
	default:
		return bad("role")
	}
	if p.Pickup != nil && p.Role != PartyShipper || p.Delivery != nil && p.Role != PartyConsignee {
		return bad("pickup addresses belong to the shipper, delivery addresses to the consignee")
	}
	overridden := false
	for _, a := range []*AddressSnapshot{p.Address, p.Pickup, p.Delivery} {
		if a == nil {
			continue
		}
		if err := a.check(); err != nil {
			return OrderParty{}, fmt.Errorf("%w: %s: %v", ErrInvalidParty, p.Role, err)
		}
		overridden = overridden || a.Source == FromOrder
	}
	switch p.Kind {
	case KindToOrder:
		p.ToOrderText = strings.Join(strings.Fields(strings.ToUpper(p.ToOrderText)), " ")
		if p.Role != PartyConsignee || !toOrderForm.MatchString(p.ToOrderText) || utf8.RuneCountInString(p.ToOrderText) > maxText {
			return bad("TO ORDER is the consignee's only, as TO ORDER or TO ORDER OF a holder")
		}
		if p.Party.PartyID != "" || p.Address != nil || p.Contact != nil || p.HasContact {
			return bad("a TO ORDER consignee is text only")
		}
		if p.Delivery != nil && p.Delivery.Source != FromOrder {
			return bad("a TO ORDER consignee's delivery address is given on the order")
		}
	case KindParty:
		r := p.Party
		if p.ToOrderText != "" || blank(r.PartyID) || r.PartyRevision < 1 || !text(r.LegalName) || !countryCode(r.CountryCode) ||
			utf8.RuneCountInString(r.EnglishName) > 200 {
			return bad("ORG party record")
		}
		if p.Role == PartyNotify {
			if r.RoleCode != "" || r.RoleEvidenceRef != "" {
				return bad("a notify party holds no ORG role (C13)")
			}
		} else if r.RoleCode != p.Role || !r.RoleValidity.Valid() || blank(r.RoleEvidenceRef) {
			return bad("ORG role confirmation")
		}
		for _, id := range r.Identifiers {
			if blank(id.Type) || !countryCode(id.Country) || id.None == (id.Value != "") {
				return bad("identifier")
			}
		}
		if p.Address == nil {
			return bad("documentary address")
		}
		if c := p.Contact; c != nil {
			if err := c.check(); err != nil {
				return OrderParty{}, fmt.Errorf("%w: %s: %v", ErrInvalidParty, p.Role, err)
			}
			overridden = overridden || c.Source == FromOrder
			p.HasContact = true
		}
	default:
		return bad("kind")
	}
	p.Overridden = overridden
	return p, nil
}

func (a *AddressSnapshot) check() error {
	switch a.Source {
	case FromORG:
		if blank(a.ORGAddressID) || a.ORGVersion < 1 || blank(a.ShortCode) || a.OverriddenBy != "" || a.OverrideReason != "" {
			return errors.New("an ORG address names the address, short code and version, and nothing overrides it")
		}
	case FromOrder:
		if blank(a.OverriddenBy) || blank(a.OverrideReason) || utf8.RuneCountInString(a.OverrideReason) > maxText {
			return errors.New("an address given on the order records who overrode and why")
		}
		if (a.ORGAddressID == "") != (a.ORGVersion == 0) {
			return errors.New("an override names the ORG address it replaced with its version, or none")
		}
	default:
		return errors.New("address source")
	}
	if !fieldOK(a.Name, 200) || len(a.Lines) == 0 || len(a.Lines) > 4 || !fieldOK(a.City, 100) || !countryCode(a.CountryCode) ||
		!optionalField(a.Region, 100) || !optionalField(a.Postcode, 20) || !optionalField(a.Phone, 40) || !optionalField(a.Email, 254) {
		return errors.New("address fields")
	}
	for _, l := range a.Lines {
		if !fieldOK(l, 100) {
			return errors.New("address line")
		}
	}
	if a.UNLocode != "" && (!(kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: a.UNLocode}).Valid() || a.UNLocode[:2] != a.CountryCode) {
		return errors.New("UN/LOCODE")
	}
	if a.Residential && (a.Phone != "" || a.Email != "") {
		return errors.New("a residential address carries no phone or e-mail")
	}
	if l := a.Local; l != nil {
		if blank(l.Language) || !fieldOK(l.Name, 200) || len(l.Lines) == 0 || len(l.Lines) > 4 || !fieldOK(l.City, 100) || !optionalField(l.Region, 100) ||
			slices.ContainsFunc(l.Lines, func(s string) bool { return !fieldOK(s, 100) }) {
			return errors.New("local address")
		}
	}
	return nil
}

func (c *ContactSnapshot) check() error {
	switch c.Source {
	case FromORG:
		if blank(c.ORGContactID) || c.ORGVersion < 1 || c.OverriddenBy != "" || c.OverrideReason != "" {
			return errors.New("an ORG contact names the contact and version")
		}
	case FromOrder:
		if blank(c.OverriddenBy) || blank(c.OverrideReason) || utf8.RuneCountInString(c.OverrideReason) > maxText {
			return errors.New("a contact given on the order records who and why")
		}
	default:
		return errors.New("contact source")
	}
	if !fieldOK(c.Name, 200) || !optionalField(c.Phone, 40) || !optionalField(c.Mobile, 40) || !optionalField(c.Email, 254) || c.Phone+c.Mobile+c.Email == "" {
		return errors.New("contact fields")
	}
	return nil
}

func fieldOK(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return s == strings.TrimSpace(s) && n >= 1 && n <= max && !strings.ContainsFunc(s, unicode.IsControl)
}

func optionalField(s string, max int) bool { return s == "" || fieldOK(s, max) }
