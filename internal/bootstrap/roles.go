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
//   - warehouse: the warehouse (CFS) staff.
//   - docs: the documentation staff: contacts for a dispatch, the contact transfer register's reservations.
//   - finance: the finance staff: the job profit and a cost review request.
//   - finance-manager: the finance manager: resolving a cost review.
//   - privacy: the privacy officer: who read a contact, erasures, consents, transfer safeguards.
//   - admin: the customer's administrator: master data (parties, roles, legal entities), and the owner's confirmation
//     of an onboarding.
//   - consultant: the implementation consultant who proposes an onboarding.
var Roles = map[string][]string{
	"ops": {"org:Read", "org:AddPartyAddress", "org:ChangePartyAddress", "org:DeactivatePartyAddress", "org:SetDefaultPartyAddress",
		"org:AddPartyContact", "org:ChangePartyContact", "org:DeactivatePartyContact", "org:SetDefaultPartyContact", "org:ReadPartyContacts",
		"rate:*", "sea:*", "com:*", "com:ReadOrderContacts", "ful:*"},
	"warehouse": {"org:Read", "sea:Read", "com:Read", "ful:Read"},
	"docs": {"org:Read", "org:ReadPartyContacts", "org:ReserveContactTransfers", "org:ReleaseContactTransfers", "sea:Read",
		"com:Read", "com:ReadOrderContacts", "com:LockOrderParties", "com:ReleaseOrderPartyLock", "ful:Read"},
	"finance":         {"org:Read", "rate:Read", "sea:Read", "com:Read", "ful:Read", "fin:ReadProfit", "fin:RequestCostReview"},
	"finance-manager": {"org:Read", "rate:Read", "sea:Read", "com:Read", "ful:Read", "fin:ReadProfit", "fin:ResolveCostReview"},
	"privacy": {"org:Read", "org:ReadContactAccessLog", "org:ErasePartyContact", "org:RecordContactConsent", "org:RecordTransferSafeguard",
		"org:ReadPartyContacts", "com:Read", "com:ReadContactAccessLog", "com:EraseOrderContacts", "com:ReadRestrictedOrderContacts"},
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
