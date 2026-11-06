// Package api is the ORG bounded context's contract for other Cargo contexts, transports and the AI
// capability layer. It must not expose ORG domain types.
package api

// Source identifies ORG as the producer of an integration event envelope.
const Source = "idp-cargo.org"

// PartnerRoleApproved is published when a partner role is approved for a business party.
const PartnerRoleApproved = "PartnerRoleApproved.v1"

// EventPayload is the payload of every ORG integration event. It carries identifiers only; legal
// identifiers and personal data never leave ORG in events.
type EventPayload struct {
	TenantID  string `json:"tenant_id"`
	SubjectID string `json:"subject_id"`
}
