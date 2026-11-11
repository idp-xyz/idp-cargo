package api

import "context"

// Identifier types of a party (MD-06; ORG V0.4; the IATA and SCAC carrier codes since ORG V0.6). ORG keeps the codes; qualifications such as an AEO
// certificate belong to CUS and tax treatment to FIN.
const (
	IdentifierCNUSCC        = "CN_USCC"         // unified social credit code, GB 32100, 18 characters with a check character; CN
	IdentifierCNCustoms     = "CN_CUSTOMS"      // customs code of a Chinese consignor or consignee, 10 characters; CN
	IdentifierAEO           = "AEO"             // authorized economic operator code as declarations print it: issuing country + code
	IdentifierEUEORI        = "EU_EORI"         // EU EORI number: member state + up to 15 letters and digits
	IdentifierUSEIN         = "US_EIN"          // US employer identification number, nine digits; US
	IdentifierRegistration  = "REGISTRATION"    // any other national company registration code, as manifests give foreign parties
	IdentifierIATAAirline   = "IATA_AIRLINE"    // airline designator of two characters, such as MU; country InternationalIssuer
	IdentifierIATAAWBPrefix = "IATA_AWB_PREFIX" // airline's three-digit air waybill prefix, such as 781; country InternationalIssuer
	IdentifierSCAC          = "SCAC"            // carrier's Standard Carrier Alpha Code, two to four letters; country InternationalIssuer
)

// InternationalIssuer is the issuing country of the identifiers international bodies issue (IATA, NMFTA): ZZ,
// a code ISO 3166-1 leaves to users.
const InternationalIssuer = "ZZ"

// Partner role codes other contexts ask ResolvePartyRole about, besides those of RATE and COM.
const (
	RoleCarrier       = "CARRIER"        // an airline, shipping line or other carrier
	RoleCoLoader      = "CO_LOADER"      // another consolidator selling LCL or air consolidation space (BOOK OD-4)
	RoleOverseasAgent = "OVERSEAS_AGENT" // the destination agent a master air waybill or bill is consigned to (DOC)
)

// Party identity actions an Authorizer decides on.
const (
	ActionRecordPartyIdentifier = "RecordPartyIdentifier"
	ActionRetirePartyIdentifier = "RetirePartyIdentifier"
	ActionSetPartyEnglishName   = "SetPartyEnglishName"
)

// PartyIdentifier is an active identifier of a party in its issuing country. None records that the party
// has none of that type there; customs declarations and manifests then print NO.
type PartyIdentifier struct {
	Type    string
	Country string
	Value   string
	None    bool
}

// RecordPartyIdentifier gives a party an identifier, or records that it has none of that type in that
// country, with the evidence it was checked against. A party has one active entry per type and country;
// retire the old one to correct it. A value another party holds is a conflict, and the error names that
// party, to be merged. ExpectedRevision is the party revision the caller read (GetPartyIdentity): identity
// is part of the party, so these commands advance the revision PartyRef carries. TenantID and ActorID must
// come from a verified principal.
type RecordPartyIdentifier struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	Type             string
	Country          string
	Value            string
	None             bool
	EvidenceRef      string
}

// RetirePartyIdentifier ends an active identifier, with a reason; the entry stays as history.
type RetirePartyIdentifier struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	Type             string
	Country          string
	Reason           string
}

// SetPartyEnglishName records the party's name in English as international documents print it, printable
// ASCII up to 200 characters, with evidence; an empty name removes it. The legal name stays the registered one.
type SetPartyEnglishName struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	EnglishName      string
	EvidenceRef      string
}

// PartyIdentityCommands maintains parties' identifiers and English names. Errors: ErrConflict for a stale
// ExpectedRevision or a value another party holds, ErrNotFound for an unknown party or identifier,
// ErrRejected for an invalid identifier or name, a second active entry of a type and country, missing
// evidence or an archived party.
type PartyIdentityCommands interface {
	RecordPartyIdentifier(ctx context.Context, cmd RecordPartyIdentifier) (Receipt, error)
	RetirePartyIdentifier(ctx context.Context, cmd RetirePartyIdentifier) (Receipt, error)
	SetPartyEnglishName(ctx context.Context, cmd SetPartyEnglishName) (Receipt, error)
}

// GetPartyIdentity asks for a party's identity: names, registration country, status, active identifiers and
// the revision. TenantID must come from a verified principal.
type GetPartyIdentity struct {
	TenantID string
	PartyID  string
}

// PartyIdentity is a party's identity at Revision.
type PartyIdentity struct {
	PartyID     string
	Revision    int64
	Status      string
	LegalName   string
	EnglishName string
	CountryCode string
	Identifiers []PartyIdentifier
}

// PartyIdentityQueries is the read side of party identity.
type PartyIdentityQueries interface {
	GetPartyIdentity(ctx context.Context, q GetPartyIdentity) (PartyIdentity, error)
}
