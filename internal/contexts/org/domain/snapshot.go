package domain

import "time"

// External PartyRef is a reference, NOT a shared aggregate instance.
type PartyRef struct {
	TenantID       TenantID
	PartyID        PartyID
	SourceRevision int64
}
type TransactionPartySnapshot struct {
	Ref         PartyRef
	LegalName   string
	Country     string
	CapturedAt  time.Time
	EvidenceRef string
}

// Snapshot returns a new immutable-by-convention value. Consumers must persist their own copy.
func (p *BusinessParty) Snapshot(revision int64, at time.Time, evidence string) (TransactionPartySnapshot, error) {
	if revision < 1 || at.IsZero() || evidence == "" || p.status != PartyActive {
		return TransactionPartySnapshot{}, ErrInvalidIdentity
	}
	return TransactionPartySnapshot{Ref: PartyRef{p.tenantID, p.id, revision}, LegalName: p.legalName, Country: p.countryCode, CapturedAt: at.UTC(), EvidenceRef: evidence}, nil
}
