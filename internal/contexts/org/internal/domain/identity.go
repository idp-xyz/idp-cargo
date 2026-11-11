package domain

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// IdentifierType is the kind of an official identifier of a party (MD-06): the codes customs declarations,
// manifests and invoices name a party by. ORG keeps the codes; qualifications such as an AEO certificate
// belong to CUS (MD-20) and tax treatment to FIN (MD-21).
type IdentifierType string

const (
	// IdentifierCNUSCC is the unified social credit code of a Chinese organization, GB 32100: 18 characters
	// with a check character.
	IdentifierCNUSCC IdentifierType = "CN_USCC"
	// IdentifierCNCustoms is the 10-character customs code of a Chinese consignor or consignee.
	IdentifierCNCustoms IdentifierType = "CN_CUSTOMS"
	// IdentifierAEO is an authorized economic operator code as customs declarations print it: the issuing
	// country followed by the operator's code, such as SG123456789012.
	IdentifierAEO IdentifierType = "AEO"
	// IdentifierEUEORI is an EU economic operator registration and identification number: a member state
	// code followed by up to 15 letters and digits.
	IdentifierEUEORI IdentifierType = "EU_EORI"
	// IdentifierUSEIN is a US employer identification number: nine digits.
	IdentifierUSEIN IdentifierType = "US_EIN"
	// IdentifierRegistration is any other national company registration code, the code a manifest gives
	// for a foreign party.
	IdentifierRegistration IdentifierType = "REGISTRATION"
	// IdentifierIATAAirline is an airline's two-character IATA designator, such as MU, which its flight
	// numbers carry. IATA, not a state, issues it, so its issuing country is InternationalIssuer.
	IdentifierIATAAirline IdentifierType = "IATA_AIRLINE"
	// IdentifierIATAAWBPrefix is an airline's three-digit IATA air waybill prefix, such as 781, the first part
	// of every MAWB number it issues; issuing country InternationalIssuer.
	IdentifierIATAAWBPrefix IdentifierType = "IATA_AWB_PREFIX"
	// IdentifierSCAC is an ocean or road carrier's Standard Carrier Alpha Code of two to four letters, such as
	// MAEU, issued by NMFTA; issuing country InternationalIssuer.
	IdentifierSCAC IdentifierType = "SCAC"
)

// InternationalIssuer is the issuing country of identifiers that international bodies issue: ZZ, a code ISO
// 3166-1 leaves to users. One active holder of such a value per tenant then holds worldwide.
const InternationalIssuer = "ZZ"

// Events of party identity. They name the identifier slot (type and issuing country), never the code: raw
// legal identifiers do not travel in integration events (V0.2 rule).
const (
	EventPartyIdentifierRecorded = "PartyIdentifierRecorded.v1"
	EventPartyIdentifierRetired  = "PartyIdentifierRetired.v1"
	EventPartyEnglishNameSet     = "PartyEnglishNameSet.v1"
)

var (
	ErrInvalidIdentifier = errors.New("org: invalid party identifier")
	ErrIdentifierExists  = errors.New("org: the party already has an active identifier of that type and country")
	ErrUnknownIdentifier = errors.New("org: the party has no active identifier of that type and country")
	ErrIdentifierInUse   = errors.New("org: the identifier belongs to another party")
)

const usccCharset = "0123456789ABCDEFGHJKLMNPQRTUWXY"

var (
	customsForm      = regexp.MustCompile(`^[0-9A-Z]{10}$`)
	prefixedForm     = regexp.MustCompile(`^[A-Z]{2}[0-9A-Z]{1,30}$`)
	eoriForm         = regexp.MustCompile(`^[A-Z]{2}[0-9A-Z]{1,15}$`)
	einForm          = regexp.MustCompile(`^[0-9]{9}$`)
	registrationForm = regexp.MustCompile(`^[0-9A-Z][0-9A-Z-]{0,34}$`)
	airlineForm      = regexp.MustCompile(`^([A-Z][0-9A-Z]|[0-9][A-Z])$`)
	awbPrefixForm    = regexp.MustCompile(`^[0-9]{3}$`)
	scacForm         = regexp.MustCompile(`^[A-Z]{2,4}$`)
)

// PartyIdentifier is one identifier of a party in one issuing country, or, with None, the recorded fact that
// the party has none of that type there (customs declarations and manifests then print NO). It needs evidence:
// ORG records identifiers after checking them (MD-06; a customs broker must check what its principal gives).
type PartyIdentifier struct {
	Type        IdentifierType
	Country     string
	Value       string
	None        bool
	EvidenceRef string
	RecordedBy  string
	RecordedAt  time.Time
}

// Key names an identifier slot of a party: one active entry per type and issuing country.
func (i PartyIdentifier) Key() string { return string(i.Type) + ":" + i.Country }

// IdentifierRetirement records that an active identifier stopped being the party's.
type IdentifierRetirement struct {
	Type      IdentifierType
	Country   string
	Reason    string
	RetiredBy string
	RetiredAt time.Time
}

func (t IdentifierType) international() bool {
	return t == IdentifierIATAAirline || t == IdentifierIATAAWBPrefix || t == IdentifierSCAC
}

// ValidUSCC reports whether a code is a unified social credit code with a correct check character (GB 32100).
func ValidUSCC(code string) bool {
	if len(code) != 18 {
		return false
	}
	sum, weight := 0, 1
	for i := 0; i < 17; i++ {
		v := strings.IndexByte(usccCharset, code[i])
		if v < 0 {
			return false
		}
		sum += v * weight
		weight = weight * 3 % 31
	}
	return code[17] == usccCharset[(31-sum%31)%31]
}

// normalized checks an identifier's form for its type and country.
func (i PartyIdentifier) normalized() (PartyIdentifier, error) {
	bad := func(what string) (PartyIdentifier, error) {
		return PartyIdentifier{}, fmt.Errorf("%w: %s %s: %s", ErrInvalidIdentifier, i.Type, i.Country, what)
	}
	i.Country, i.Value = strings.TrimSpace(i.Country), strings.ToUpper(strings.TrimSpace(i.Value))
	if len(i.Country) != 2 || !upperASCII(i.Country) {
		return bad("issuing country")
	}
	var ok bool
	if i.EvidenceRef, ok = text(i.EvidenceRef, 1, maxReasonRunes); !ok {
		return PartyIdentifier{}, fmt.Errorf("%w: evidence", ErrMissingEvidence)
	}
	switch i.Type {
	case IdentifierCNUSCC, IdentifierCNCustoms:
		if i.Country != "CN" {
			return bad("issued in CN only")
		}
	case IdentifierUSEIN:
		if i.Country != "US" {
			return bad("issued in US only")
		}
	case IdentifierIATAAirline, IdentifierIATAAWBPrefix, IdentifierSCAC:
		if i.Country != InternationalIssuer {
			return bad("issued internationally, country " + InternationalIssuer)
		}
	case IdentifierAEO, IdentifierEUEORI, IdentifierRegistration:
	default:
		return bad("unknown type")
	}
	if i.Country == InternationalIssuer && !i.Type.international() {
		return bad("issued by a state")
	}
	if i.None {
		if i.Value != "" {
			return bad("a recorded absence has no value")
		}
		return i, nil
	}
	switch i.Type {
	case IdentifierCNUSCC:
		ok = ValidUSCC(i.Value)
	case IdentifierCNCustoms:
		ok = customsForm.MatchString(i.Value)
	case IdentifierAEO:
		ok = prefixedForm.MatchString(i.Value) && i.Value[:2] == i.Country
	case IdentifierEUEORI:
		ok = eoriForm.MatchString(i.Value) && i.Value[:2] == i.Country
	case IdentifierUSEIN:
		ok = einForm.MatchString(i.Value)
	case IdentifierRegistration:
		ok = registrationForm.MatchString(i.Value)
	case IdentifierIATAAirline:
		ok = airlineForm.MatchString(i.Value)
	case IdentifierIATAAWBPrefix:
		ok = awbPrefixForm.MatchString(i.Value)
	case IdentifierSCAC:
		ok = scacForm.MatchString(i.Value)
	}
	if !ok {
		return bad("value")
	}
	return i, nil
}

// RecordIdentifier gives the party an identifier, or records that it has none of that type in that country.
// One active entry per type and country: retire the old one first to correct it. Whether another party
// holds the same value is checked by the caller and by the database. Archived parties are closed.
func (p *BusinessParty) RecordIdentifier(id PartyIdentifier, actor string, at time.Time) error {
	if p.status == PartyArchived || at.IsZero() || strings.TrimSpace(actor) == "" {
		return ErrInvalidTransition
	}
	id, err := id.normalized()
	if err != nil {
		return err
	}
	for _, x := range p.identifiers {
		if x.Key() == id.Key() {
			return fmt.Errorf("%w: %s", ErrIdentifierExists, id.Key())
		}
	}
	id.RecordedBy, id.RecordedAt = actor, at.UTC()
	p.identifiers = append(p.identifiers, id)
	p.newIdentifiers = append(p.newIdentifiers, id)
	p.recordEntry(EventPartyIdentifierRecorded, at, id.Key())
	return nil
}

// RetireIdentifier ends an active identifier of the party, with a reason, such as a code entered wrongly or
// a registration cancelled. The entry is kept as history.
func (p *BusinessParty) RetireIdentifier(t IdentifierType, country, reason, actor string, at time.Time) error {
	if p.status == PartyArchived || at.IsZero() || strings.TrimSpace(actor) == "" {
		return ErrInvalidTransition
	}
	reason, ok := text(reason, 1, maxReasonRunes)
	if !ok {
		return fmt.Errorf("%w: a reason is required", ErrMissingEvidence)
	}
	key := string(t) + ":" + country
	i := slices.IndexFunc(p.identifiers, func(x PartyIdentifier) bool { return x.Key() == key })
	if i < 0 {
		return fmt.Errorf("%w: %s", ErrUnknownIdentifier, key)
	}
	p.identifiers = slices.Delete(p.identifiers, i, i+1)
	p.newIdentifiers = slices.DeleteFunc(p.newIdentifiers, func(x PartyIdentifier) bool { return x.Key() == key })
	p.retirements = append(p.retirements, IdentifierRetirement{Type: t, Country: country, Reason: reason, RetiredBy: actor, RetiredAt: at.UTC()})
	p.recordEntry(EventPartyIdentifierRetired, at, key)
	return nil
}

// SetEnglishName records the party's name in English as international documents print it: printable ASCII,
// up to 200 characters; an empty name removes it. The legal name stays the registered one.
func (p *BusinessParty) SetEnglishName(name, evidence string, at time.Time) error {
	if p.status == PartyArchived || at.IsZero() {
		return ErrInvalidTransition
	}
	if strings.TrimSpace(evidence) == "" {
		return ErrMissingEvidence
	}
	name = strings.TrimSpace(name)
	if !EnglishNameValid(name) {
		return fmt.Errorf("%w: English name must be printable ASCII, up to 200 characters", ErrInvalidIdentity)
	}
	if name == p.englishName {
		return ErrNothingChanged
	}
	p.englishName = name
	p.record(EventPartyEnglishNameSet, at)
	return nil
}

// EnglishNameValid reports whether a trimmed English name is empty or printable ASCII up to 200 characters.
func EnglishNameValid(name string) bool {
	if len(name) > 200 || name != strings.TrimSpace(name) {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] > 0x7e {
			return false
		}
	}
	return true
}

// RestoreIdentity adds the stored English name and active identifiers to a party restored by RestoreParty.
func (p *BusinessParty) RestoreIdentity(english string, identifiers []PartyIdentifier) error {
	if !EnglishNameValid(english) {
		return ErrInvalidIdentity
	}
	seen := map[string]bool{}
	for _, raw := range identifiers {
		id, err := raw.normalized()
		if err != nil || seen[id.Key()] || strings.TrimSpace(raw.RecordedBy) == "" || raw.RecordedAt.IsZero() {
			return fmt.Errorf("%w: stored identifier %s", ErrInvalidIdentity, raw.Key())
		}
		seen[id.Key()] = true
		id.RecordedBy, id.RecordedAt = raw.RecordedBy, raw.RecordedAt
		p.identifiers = append(p.identifiers, id)
	}
	p.englishName = english
	return nil
}

func (p *BusinessParty) EnglishName() string { return p.englishName }

// Identifiers returns the party's active identifiers, ordered by type and country.
func (p *BusinessParty) Identifiers() []PartyIdentifier {
	out := slices.Clone(p.identifiers)
	slices.SortFunc(out, func(a, b PartyIdentifier) int { return strings.Compare(a.Key(), b.Key()) })
	return out
}

// Identifier returns the active identifier of a type and issuing country.
func (p *BusinessParty) Identifier(t IdentifierType, country string) (PartyIdentifier, bool) {
	i := slices.IndexFunc(p.identifiers, func(x PartyIdentifier) bool { return x.Type == t && x.Country == country })
	if i < 0 {
		return PartyIdentifier{}, false
	}
	return p.identifiers[i], true
}

// IdentityChanges returns the identifiers recorded and retired since the party was loaded, for the store.
func (p *BusinessParty) IdentityChanges() ([]PartyIdentifier, []IdentifierRetirement) {
	return slices.Clone(p.newIdentifiers), slices.Clone(p.retirements)
}

func (p *BusinessParty) recordEntry(kind string, at time.Time, entry string) {
	p.pending = append(p.pending, Event{Type: kind, At: at.UTC(), TenantID: p.tenantID, SubjectID: string(p.id), EntryID: entry})
}
