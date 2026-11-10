// Package api is the ORG bounded context's contract for other Cargo contexts, transports and the AI
// capability layer. It must not expose ORG domain types.
package api

// Source identifies ORG as the producer of an integration event envelope.
const Source = "idp-cargo.org"

// PartnerRoleApproved is published when a partner role is approved for a business party.
const PartnerRoleApproved = "PartnerRoleApproved.v1"

// Address book events (C9), one per command. The subject is the party; the payload names the address or
// contact, its version, the address book revision and, when a default is set, the purpose.
const (
	PartyAddressAdded       = "PartyAddressAdded.v1"
	PartyAddressChanged     = "PartyAddressChanged.v1"
	PartyAddressDeactivated = "PartyAddressDeactivated.v1"
	PartyDefaultAddressSet  = "PartyDefaultAddressSet.v1"
	PartyContactAdded       = "PartyContactAdded.v1"
	PartyContactChanged     = "PartyContactChanged.v1"
	PartyContactDeactivated = "PartyContactDeactivated.v1"
	PartyContactErased      = "PartyContactErased.v1"
	PartyDefaultContactSet  = "PartyDefaultContactSet.v1"
)

// Party identity events (ORG V0.4). The payload's entry names the identifier's type and issuing country, such
// as CN_USCC:CN, never the code itself.
const (
	PartyIdentifierRecorded = "PartyIdentifierRecorded.v1"
	PartyIdentifierRetired  = "PartyIdentifierRetired.v1"
	PartyEnglishNameSet     = "PartyEnglishNameSet.v1"
)

// EventPayload is the payload of every ORG integration event. It carries identifiers only; legal
// identifiers, addresses and personal data never leave ORG in events. The entry fields are set by address
// book events only.
type EventPayload struct {
	TenantID     string `json:"tenant_id"`
	SubjectID    string `json:"subject_id"`
	EntryID      string `json:"entry_id,omitempty"`
	EntryVersion int64  `json:"entry_version,omitempty"`
	Revision     int64  `json:"revision,omitempty"`
	Purpose      string `json:"purpose,omitempty"`
}
