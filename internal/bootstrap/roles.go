package bootstrap

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
)

// Roles are the default permission sets of a forwarder's jobs (the owner's decision of 2026-11-26 for stage 3), by
// which development tokens are minted for internal acceptance (cargo-api -mint-token -role). Each version of stage 3
// adds its context's permissions. The identity platform will hold the real roles; these are its starting point, and
// they can be changed. A permission granted by name only is listed by name: the wildcards never include it.
//
//   - ops: the operator: parties' address books and contacts, quotes, ocean movements and containers.
//   - warehouse: the warehouse (CFS) staff: receiving, stuffing and sealing in PHY, and the stuffing and seal in SEA.
//   - docs: the documentation staff: contacts for a dispatch, the contact transfer register's reservations.
//   - finance: the finance staff: jobs, receivables, payables, credit, fapiao, the profit and cost reviews.
//   - finance-manager: the finance manager: as the staff, and by name the approval of one's own variance, the
//     after-the-fact review, the review of settings exceptions and the credit override of a document.
//   - privacy: the privacy officer: who read a contact, erasures, consents, transfer safeguards.
//   - admin: the customer's administrator: master data (parties, roles, legal entities), and the owner's confirmation
//     of an onboarding.
//   - consultant: the implementation consultant who proposes an onboarding.
var Roles = map[string][]string{
	"ops": {"org:Read", "org:AddPartyAddress", "org:ChangePartyAddress", "org:DeactivatePartyAddress", "org:SetDefaultPartyAddress",
		"org:AddPartyContact", "org:ChangePartyContact", "org:DeactivatePartyContact", "org:SetDefaultPartyContact", "org:ReadPartyContacts",
		"rate:*", "sea:*", "com:*", "com:ReadOrderContacts", "ful:*", "phy:*", "book:*", "air:*", "doc:Read", "fin:Read"},
	"warehouse": {"org:Read", "sea:Read", "sea:RecordStuffingDone", "sea:RecordContainerSeal", "com:Read", "ful:Read", "phy:*", "book:Read", "air:Read", "doc:Read"},
	"docs": {"org:Read", "org:ReadPartyContacts", "org:ReserveContactTransfers", "org:ReleaseContactTransfers", "sea:Read",
		"com:Read", "com:ReadOrderContacts", "com:LockOrderParties", "com:ReleaseOrderPartyLock", "ful:Read", "phy:Read", "book:Read", "air:Read", "doc:*"},
	"finance": {"org:Read", "rate:Read", "sea:Read", "com:Read", "ful:Read", "phy:Read", "book:Read", "air:Read", "doc:Read", "fin:*"},
	"finance-manager": {"org:Read", "rate:Read", "sea:Read", "com:Read", "ful:Read", "phy:Read", "book:Read", "air:Read", "doc:Read", "doc:OverrideCreditCheck", "fin:*",
		"fin:ApproveOwnCostVariance", "fin:ReviewAfterTheFact", "fin:ReviewSettingsExceptions"},
	"privacy": {"org:Read", "org:ReadContactAccessLog", "org:ErasePartyContact", "org:RecordContactConsent", "org:RecordTransferSafeguard",
		"org:ReadPartyContacts", "com:Read", "com:ReadContactAccessLog", "com:EraseOrderContacts", "com:ReadRestrictedOrderContacts", "doc:Read", "doc:ReadProvisionRecords"},
	"admin":      {"org:*", "org:ConfirmOnboarding", "fin:ApproveSettings", "sea:ApproveSettings", "com:Read", "com:SetOrderPolicy"},
	"consultant": {"org:Read", "org:ReadOnboarding", "org:ProposeOnboarding", "fin:ConfigureSettings", "sea:ConfigureSettings"},
}

// RoleNames answers the roles' names in order.
func RoleNames() []string {
	out := make([]string, 0, len(Roles))
	for k := range Roles {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RolePermissions answers the permissions of the roles named, with extra ones, each once.
func RolePermissions(roles []string, extra []string) ([]string, error) {
	var out []string
	for _, r := range roles {
		perms, ok := Roles[r]
		if !ok {
			return nil, fmt.Errorf("bootstrap: no role %q; the roles are %v", r, RoleNames())
		}
		out = append(out, perms...)
	}
	out = append(out, extra...)
	slices.Sort(out)
	return slices.Compact(out), nil
}

// permissionForm is a role's permission: "<context>:<Action>" or "<context>:*".
var permissionForm = regexp.MustCompile(`^[a-z]+:([A-Z][A-Za-z]*|\*)$`)
