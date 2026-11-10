package domain

import "testing"

// A contact frozen on an order may bring the consent recorded for the person: from ORG with the revision that
// recorded it, or given with a contact given on the order, without one (decisions P4, P7).
func TestContactConsentOnTheOrder(t *testing.T) {
	consent := func(revision int64) *ContactConsent {
		return &ContactConsent{NoticeVersion: "PI-RULES-2026.1", ProvideToOthers: true, Basis: ConsentCustomerUndertaking,
			EvidenceRef: "contract SC-1", ORGRevision: revision}
	}
	fromORG := func(c *ContactConsent) *ContactSnapshot {
		return &ContactSnapshot{Source: FromORG, ORGContactID: "K1", ORGVersion: 1, Name: "Test Person A", Email: "a@example.test", Consent: c}
	}
	onOrder := func(c *ContactConsent) *ContactSnapshot {
		return &ContactSnapshot{Source: FromOrder, OverriddenBy: "ops-1", OverrideReason: "site contact", Name: "Test Person B",
			Phone: "+86 21 5555 0001", Consent: c}
	}
	bad := func(edit func(*ContactConsent)) *ContactConsent { c := consent(3); edit(c); return c }
	for name, tc := range map[string]struct {
		k    *ContactSnapshot
		good bool
	}{
		"no consent":                            {fromORG(nil), true},
		"carried from ORG":                      {fromORG(consent(3)), true},
		"given on the order":                    {onOrder(consent(0)), true},
		"from ORG without its revision":         {fromORG(consent(0)), false},
		"on the order with an ORG revision":     {onOrder(consent(3)), false},
		"an unknown basis":                      {fromORG(bad(func(c *ContactConsent) { c.Basis = "IMPLIED" })), false},
		"a notice version with a space":         {fromORG(bad(func(c *ContactConsent) { c.NoticeVersion = "v 1" })), false},
		"evidence with surrounding white space": {fromORG(bad(func(c *ContactConsent) { c.EvidenceRef = " contract " })), false},
	} {
		if err := tc.k.check(); (err == nil) != tc.good {
			t.Errorf("%s: %v", name, err)
		}
	}
}
