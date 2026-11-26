// Package http is ORG's inbound HTTP adapter (since V2.106): each route binds the body or the query string to ORG's
// own api command or query (httpapi.Command, httpapi.Query), with the tenant and actor of the verified principal, and
// holds no business rules. ORG authorizes its commands itself, before it replays one; the reads are "org:Read", except
// those ORG authorizes itself because they name the reader: a party's contacts ("org:ReadPartyContacts", by name only,
// each read logged with its purpose), the log of those reads and the onboardings. A party's ETag is its revision; an
// address book's is the book's. The onboarding commands are the platform's (internal/bootstrap); the record of a
// module's progress and ResolvePartyRole stay in process.
package http

import (
	"net/http"

	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
)

// bc is ORG's name in permissions ("org:<Action>").
const bc = "org"

// Service is what the routes call.
type Service interface {
	api.Commands
	api.MasterDataCommands
	api.MasterDataQueries
	api.PartyIdentityCommands
	api.PartyIdentityQueries
	api.AddressBookCommands
	api.AddressBookQueries
	api.ContactAccessQueries
	api.LegalEntityQueries
	api.ContactTransferRegisterCommands
	api.ContactTransferRegisterQueries
	api.OnboardingQueries
}

// Kinds maps ORG's error kinds to statuses.
var Kinds = httpapi.Errors{Unauthenticated: api.ErrUnauthenticated, Unauthorized: api.ErrUnauthorized, NotFound: api.ErrNotFound,
	Conflict: api.ErrConflict, IdempotencyConflict: api.ErrIdempotencyConflict, Rejected: api.ErrRejected}

// Register registers ORG's routes.
func Register(r *httpapi.Routes, s Service) {
	k := Kinds
	op := func(method, path, calls, action, summary string) httpapi.Op {
		return httpapi.Op{Method: method, Path: path, Context: bc, Calls: calls, Action: action, Summary: summary}
	}
	created := func(o httpapi.Op) httpapi.Op { o.Status = http.StatusCreated; return o }
	checked := func(o httpapi.Op) httpapi.Op { o.ContextChecks, o.Action = true, ""; return o }

	// parties
	httpapi.Command(r, k, created(op("POST", "/v1/parties", "RegisterParty", api.ActionRegisterParty, "Register a business party (DRAFT)")), s.RegisterParty)
	httpapi.Query(r, k, op("GET", "/v1/parties", "ListParties", "", "List parties, a page at a time"), s.ListParties)
	httpapi.Query(r, k, op("GET", "/v1/parties/{party_id}", "GetPartyIdentity", "", "A party: names, status, identifiers"), s.GetPartyIdentity)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/activate", "ActivateParty", api.ActionActivateParty, "Activate a DRAFT party with the evidence of who it is"), s.ActivateParty)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/legal-name", "CorrectPartyLegalName", api.ActionCorrectPartyLegalName, "Correct the legal name"), s.CorrectPartyLegalName)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/suspend", "SuspendParty", api.ActionSuspendParty, "Suspend an active party"), s.SuspendParty)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/archive", "ArchiveParty", api.ActionArchiveParty, "Archive a suspended party whose roles ended"), s.ArchiveParty)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/english-name", "SetPartyEnglishName", api.ActionSetPartyEnglishName, "Set the English name"), s.SetPartyEnglishName)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/identifiers", "RecordPartyIdentifier", api.ActionRecordPartyIdentifier, "Record an identifier (or that it has none)"), s.RecordPartyIdentifier)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/identifiers/retire", "RetirePartyIdentifier", api.ActionRetirePartyIdentifier, "Retire an identifier"), s.RetirePartyIdentifier)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/roles", "ApproveRole", "ApproveRole", "Approve a partner role for a legal entity"), s.ApproveRole)

	// the address book: its revision is the book's
	httpapi.Query(r, k, op("GET", "/v1/parties/{party_id}/addresses", "GetPartyAddresses", "", "A party's addresses"), s.GetPartyAddresses)
	httpapi.Query(r, k, op("GET", "/v1/parties/{party_id}/addresses/{address_id}", "GetPartyAddress", "", "One address, at a version (0: the latest)"), s.GetPartyAddress)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/addresses", "AddPartyAddress", api.ActionAddPartyAddress, "Add an address"), s.AddPartyAddress)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/addresses/{address_id}/change", "ChangePartyAddress", api.ActionChangePartyAddress, "Record the next version of an address"), s.ChangePartyAddress)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/addresses/{address_id}/deactivate", "DeactivatePartyAddress", api.ActionDeactivatePartyAddress, "Deactivate an address"), s.DeactivatePartyAddress)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/addresses/{address_id}/default", "SetDefaultPartyAddress", api.ActionSetDefaultPartyAddress, "Make an address the default for a purpose"), s.SetDefaultPartyAddress)
	httpapi.Query(r, k, checked(op("GET", "/v1/parties/{party_id}/contacts", "GetPartyContacts", "", "A party's contacts: org:ReadPartyContacts by name, each read logged with its purpose")), s.GetPartyContacts)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/contacts", "AddPartyContact", api.ActionAddPartyContact, "Add a contact"), s.AddPartyContact)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/contacts/{contact_id}/change", "ChangePartyContact", api.ActionChangePartyContact, "Record the next version of a contact"), s.ChangePartyContact)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/contacts/{contact_id}/deactivate", "DeactivatePartyContact", api.ActionDeactivatePartyContact, "Deactivate a contact"), s.DeactivatePartyContact)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/contacts/{contact_id}/erase", "ErasePartyContact", api.ActionErasePartyContact, "Erase a contact's personal data (by name only)"), s.ErasePartyContact)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/contacts/{contact_id}/default", "SetDefaultPartyContact", api.ActionSetDefaultPartyContact, "Make a contact the default for a purpose"), s.SetDefaultPartyContact)
	httpapi.Command(r, k, op("POST", "/v1/parties/{party_id}/contacts/{contact_id}/consents", "RecordContactConsent", api.ActionRecordContactConsent, "Record a contact's consent"), s.RecordContactConsent)
	httpapi.Query(r, k, checked(op("GET", "/v1/parties/{party_id}/contact-access", "ListContactAccess", "", "Who read the party's contacts: org:ReadContactAccessLog by name")), s.ListContactAccess)

	// the tenant's legal entities
	httpapi.Command(r, k, created(op("POST", "/v1/legal-entities", "RegisterLegalEntity", api.ActionRegisterLegalEntity, "Register one of the tenant's companies (DRAFT)")), s.RegisterLegalEntity)
	httpapi.Query(r, k, op("GET", "/v1/legal-entities", "ListLegalEntities", "", "The tenant's legal entities"), s.ListLegalEntities)
	httpapi.Query(r, k, op("GET", "/v1/legal-entities/{legal_entity_id}", "GetLegalEntity", "", "A legal entity"), s.GetLegalEntity)
	httpapi.Command(r, k, op("POST", "/v1/legal-entities/{legal_entity_id}/verify", "VerifyLegalEntity", api.ActionVerifyLegalEntity, "Verify a DRAFT legal entity"), s.VerifyLegalEntity)

	// the contact transfer register (PIPL): the client reserves before DOC dispatches and releases after (T6)
	httpapi.Command(r, k, op("POST", "/v1/contact-transfers/reservations", "ReserveContactTransfers", api.ActionReserveContactTransfers, "Reserve the contacts a dispatch transfers abroad"), s.ReserveContactTransfers)
	httpapi.Command(r, k, op("POST", "/v1/contact-transfers/releases", "ReleaseContactTransfers", api.ActionReleaseContactTransfers, "Release a dispatch's reservation"), s.ReleaseContactTransfers)
	httpapi.Command(r, k, op("POST", "/v1/contact-transfers/safeguards", "RecordTransferSafeguard", api.ActionRecordTransferSafeguard, "Record a transfer safeguard"), s.RecordTransferSafeguard)
	httpapi.Query(r, k, op("GET", "/v1/contact-transfers/{year}", "GetContactTransferRegister", "", "The register of a year: persons transferred and the safeguard in force"), s.GetContactTransferRegister)

	// the onboardings, read by a person (the commands are the platform's routes)
	httpapi.Query(r, k, checked(op("GET", "/v1/onboardings", "ListOnboardings", "", "The tenant's onboardings: org:ReadOnboarding")), s.ListOnboardings)
	httpapi.Query(r, k, checked(op("GET", "/v1/onboardings/{onboarding_id}", "GetOnboarding", "", "An onboarding: org:ReadOnboarding")), s.GetOnboarding)
}
