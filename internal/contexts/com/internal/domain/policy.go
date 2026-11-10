package domain

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidPolicy      = errors.New("com: invalid order policy")
	ErrQuoteFullyOrdered  = errors.New("com: the accepted quote already backs as many orders as the order policy allows")
	ErrPolicyVersionStale = errors.New("com: the order policy changed since it was read")
)

// PolicyScope says which order policy applies to an order (C1, C2).
type PolicyScope string

const (
	PolicyBuiltIn     PolicyScope = "BUILT_IN"     // no policy recorded: one order per quote, no grace
	PolicyTenant      PolicyScope = "TENANT"       // the tenant's default
	PolicyLegalEntity PolicyScope = "LEGAL_ENTITY" // the policy of the quote's legal entity
)

// Bounds of an order policy.
const (
	MaxOrdersPerQuoteLimit = 999
	MaxGraceDays           = 30
)

// OrderPolicy is how many orders one accepted quote may back (C1) and for how many days after the end of
// the quote's validity an order may still be recorded (C2). COM keeps a policy per legal entity with a
// tenant default; without either the built-in policy applies: one order and no grace. An order freezes the
// policy it was recorded under.
type OrderPolicy struct {
	Scope             PolicyScope
	LegalEntityID     LegalEntityID // set exactly for LEGAL_ENTITY
	Version           int64         // 0 for BUILT_IN
	MaxOrdersPerQuote int
	GraceDays         int
}

// BuiltInOrderPolicy is the policy when none is recorded: the rules before C1 and C2 were made configurable.
func BuiltInOrderPolicy() OrderPolicy {
	return OrderPolicy{Scope: PolicyBuiltIn, MaxOrdersPerQuote: 1}
}

func (p OrderPolicy) valid() bool {
	scoped := p.Scope == PolicyLegalEntity
	switch {
	case p.Scope != PolicyBuiltIn && p.Scope != PolicyTenant && !scoped, scoped != identifier(string(p.LegalEntityID)),
		!scoped && p.LegalEntityID != "", (p.Scope == PolicyBuiltIn) != (p.Version == 0), p.Version < 0,
		p.MaxOrdersPerQuote < 1, p.MaxOrdersPerQuote > MaxOrdersPerQuoteLimit, p.GraceDays < 0, p.GraceDays > MaxGraceDays:
		return false
	}
	return true
}

// lastInstant returns the end of the time in which an order on a quote valid until until may be recorded.
func (p OrderPolicy) lastInstant(until time.Time) time.Time {
	return until.Add(time.Duration(p.GraceDays) * 24 * time.Hour)
}

// PolicySetting is one recorded version of an order policy: of a legal entity, or the tenant default when
// LegalEntityID is empty. Versions are never changed; the latest one is in force from the time it was
// recorded.
type PolicySetting struct {
	TenantID TenantID
	OrderPolicy
	SetBy string
	SetAt time.Time
}

// SetOrderPolicy records the next version of the policy of a legal entity, or of the tenant default when
// entity is empty. latest is the version in force for the same scope, nil when there is none, and expected
// is the version the caller read (0 for none). The new version takes effect at at, the time COM records it.
func SetOrderPolicy(tenant TenantID, entity LegalEntityID, latest *PolicySetting, expected int64, maxOrders, graceDays int, by string, at time.Time) (PolicySetting, error) {
	scope := PolicyTenant
	if entity != "" {
		scope = PolicyLegalEntity
	}
	var version int64
	if latest != nil {
		if latest.TenantID != tenant || latest.LegalEntityID != entity || latest.Scope != scope {
			return PolicySetting{}, fmt.Errorf("%w: the latest version belongs to another scope", ErrInvalidPolicy)
		}
		if at.Before(latest.SetAt) {
			return PolicySetting{}, fmt.Errorf("%w: recorded before version %d", ErrInvalidPolicy, latest.Version)
		}
		version = latest.Version
	}
	if expected != version {
		return PolicySetting{}, fmt.Errorf("%w: version %d is in force, not %d", ErrPolicyVersionStale, version, expected)
	}
	s := PolicySetting{TenantID: tenant, SetBy: by, SetAt: at.UTC(), OrderPolicy: OrderPolicy{Scope: scope, LegalEntityID: entity,
		Version: version + 1, MaxOrdersPerQuote: maxOrders, GraceDays: graceDays}}
	if err := s.Validate(); err != nil {
		return PolicySetting{}, err
	}
	return s, nil
}

// Validate checks a recorded policy version.
func (s PolicySetting) Validate() error {
	if blank(string(s.TenantID)) || s.Scope == PolicyBuiltIn || !s.OrderPolicy.valid() || blank(s.SetBy) || s.SetAt.IsZero() {
		return fmt.Errorf("%w: at most %d orders per quote and %d days of grace", ErrInvalidPolicy, MaxOrdersPerQuoteLimit, MaxGraceDays)
	}
	return nil
}
