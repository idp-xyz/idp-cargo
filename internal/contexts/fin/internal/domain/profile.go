package domain

import (
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// Buyer kinds of a billing profile (MD-21). A Chinese individual buyer carries personal data and waits for the
// privacy controls of the fapiao work (FB6).
const (
	BuyerCNEnterprise = "CN_ENTERPRISE"
	BuyerForeign      = "FOREIGN"
)

// PartyIdentity is what ORG answers of a debtor's identity (ORG keeps identifiers, FIN invoice data): its revision,
// status, names, registration country and active unified social credit code, if any.
type PartyIdentity struct {
	PartyID     string
	Revision    int64
	Status      string
	LegalName   string
	EnglishName string
	CountryCode string
	USCC        string
}

// invoiceTitle is the name a bill and a fapiao print for the party: the registered name for a Chinese enterprise, the
// English name of a foreign one when ORG has it.
func (id PartyIdentity) invoiceTitle(kind string) string {
	if kind == BuyerForeign && id.EnglishName != "" {
		return id.EnglishName
	}
	return id.LegalName
}

// ProfileInput is a billing profile version as asked: the buyer kind and the enterprise's registered invoice data
// (address, phone, bank and account; optional, never a person's contact), with the evidence it was checked against.
type ProfileInput struct {
	BuyerKind         string
	RegisteredAddress string
	RegisteredPhone   string
	BankName          string
	BankAccount       string
	EvidenceRef       string
}

// ProfileVersion is an immutable version of a debtor's billing profile: the invoice title and taxpayer number frozen
// from ORG at the party revision read, and the invoice data FIN keeps.
type ProfileVersion struct {
	No             int
	BuyerKind      string
	Title          string
	TaxpayerNumber string
	PartyRevision  int64
	ProfileInput
	By        string
	At        time.Time
	EnteredAt time.Time
}

var (
	phonePattern   = regexp.MustCompile(`^[0-9+()\- ]{3,30}$`)
	accountPattern = regexp.MustCompile(`^[0-9A-Z ]{4,40}$`)
)

// ProfileState is a stored profile, for persistence.
type ProfileState struct {
	TenantID      TenantID
	DebtorPartyID string
	Versions      []ProfileVersion
}

// BillingProfile is the aggregate root of a debtor's invoice data (MD-21, FIN; never ORG master data): versions that
// debit notes and fapiao records freeze.
type BillingProfile struct {
	state   ProfileState
	stored  int
	pending []Event
}

const eventProfileSet = "BillingProfileSet.v1"

// NewProfile starts a debtor's profile; its first version follows.
func NewProfile(tenant TenantID, debtor string) (*BillingProfile, error) {
	if tenant == "" || !identifier(debtor) {
		return nil, fmt.Errorf("%w: a profile names its debtor", ErrInvalid)
	}
	return &BillingProfile{state: ProfileState{TenantID: tenant, DebtorPartyID: debtor}}, nil
}

// Set records the next version from ORG's identity of the debtor: an ACTIVE party; a Chinese enterprise registered in
// CN with an active unified social credit code, which becomes the taxpayer number, and the registered name as title;
// a foreign buyer registered elsewhere, titled with its English name when ORG has one. A version equal to the current
// one is refused.
func (b *BillingProfile) Set(in ProfileInput, id PartyIdentity, by string, at time.Time) error {
	switch {
	case id.PartyID != b.state.DebtorPartyID || id.Revision < 1 || blank(by) || at.IsZero():
		return fmt.Errorf("%w: ORG answered for another party", ErrInvalid)
	case id.Status != "ACTIVE":
		return fmt.Errorf("%w: ORG reports party %s %s", ErrInvalid, id.PartyID, id.Status)
	case in.BuyerKind != BuyerCNEnterprise && in.BuyerKind != BuyerForeign:
		return fmt.Errorf("%w: the buyer kind is CN_ENTERPRISE or FOREIGN", ErrInvalid)
	case in.BuyerKind == BuyerCNEnterprise && (id.CountryCode != "CN" || id.USCC == ""):
		return fmt.Errorf("%w: a Chinese enterprise buyer is registered in CN with a unified social credit code in ORG", ErrInvalid)
	case in.BuyerKind == BuyerForeign && id.CountryCode == "CN":
		return fmt.Errorf("%w: a party registered in CN bills as a Chinese enterprise", ErrInvalid)
	case !text(in.EvidenceRef),
		in.RegisteredAddress != "" && (!text(in.RegisteredAddress) || utf8.RuneCountInString(in.RegisteredAddress) > 300),
		in.RegisteredPhone != "" && !phonePattern.MatchString(in.RegisteredPhone),
		in.BankName != "" && (!text(in.BankName) || utf8.RuneCountInString(in.BankName) > 200),
		in.BankAccount != "" && !accountPattern.MatchString(in.BankAccount),
		(in.BankName == "") != (in.BankAccount == ""):
		return fmt.Errorf("%w: a profile has its evidence; the registered address (300 characters), phone (digits, +, -, parentheses), and bank "+
			"with account (capital letters and digits) are optional, the bank and account together", ErrInvalid)
	}
	title := id.invoiceTitle(in.BuyerKind)
	if utf8.RuneCountInString(title) > 300 || blank(title) {
		return fmt.Errorf("%w: ORG's name of %s does not fit an invoice title", ErrInvalid, id.PartyID)
	}
	v := ProfileVersion{No: len(b.state.Versions) + 1, BuyerKind: in.BuyerKind, Title: title, PartyRevision: id.Revision, ProfileInput: in, By: by,
		At: at.UTC()}
	if in.BuyerKind == BuyerCNEnterprise {
		v.TaxpayerNumber = id.USCC
	}
	if c, ok := b.Current(); ok {
		if at.Before(c.At) {
			return fmt.Errorf("%w: business time before version %d", ErrInvalid, c.No)
		}
		if c.BuyerKind == v.BuyerKind && c.Title == v.Title && c.TaxpayerNumber == v.TaxpayerNumber && c.ProfileInput == v.ProfileInput {
			return fmt.Errorf("%w: version %d holds the same data", ErrInvalid, c.No)
		}
	}
	b.state.Versions = append(b.state.Versions, v)
	b.pending = append(b.pending, Event{Type: eventProfileSet, At: at.UTC(), TenantID: b.state.TenantID, SubjectID: b.state.DebtorPartyID,
		Version: v.No, DebtorPartyID: b.state.DebtorPartyID})
	return nil
}

// Current returns the latest version, if any.
func (b *BillingProfile) Current() (ProfileVersion, bool) {
	if n := len(b.state.Versions); n > 0 {
		return b.state.Versions[n-1], true
	}
	return ProfileVersion{}, false
}

// Matches reports whether a version still names the debtor as ORG does now: title and taxpayer number.
func (v ProfileVersion) Matches(id PartyIdentity) bool {
	taxpayer := ""
	if v.BuyerKind == BuyerCNEnterprise {
		taxpayer = id.USCC
	}
	return v.Title == id.invoiceTitle(v.BuyerKind) && v.TaxpayerNumber == taxpayer
}

// Stamp records when FIN entered the versions written since the profile was loaded (T1).
func (b *BillingProfile) Stamp(entered time.Time) {
	for i := range b.state.Versions {
		if b.state.Versions[i].EnteredAt.IsZero() {
			b.state.Versions[i].EnteredAt = entered.UTC()
		}
	}
}

// RestoreProfile rebuilds a stored profile.
func RestoreProfile(st ProfileState) (*BillingProfile, error) {
	st.Versions = append([]ProfileVersion(nil), st.Versions...)
	for i, v := range st.Versions {
		if v.No != i+1 {
			return nil, fmt.Errorf("%w: billing profile version %d", ErrInvalid, i+1)
		}
	}
	return &BillingProfile{state: st, stored: len(st.Versions)}, nil
}

func (b *BillingProfile) State() ProfileState {
	st := b.state
	st.Versions = append([]ProfileVersion(nil), st.Versions...)
	return st
}

// Added returns the versions written since the profile was loaded.
func (b *BillingProfile) Added() []ProfileVersion {
	return append([]ProfileVersion(nil), b.state.Versions[b.stored:]...)
}

func (b *BillingProfile) Pending() []Event { return append([]Event(nil), b.pending...) }
func (b *BillingProfile) Acknowledge()     { b.pending, b.stored = nil, len(b.state.Versions) }
