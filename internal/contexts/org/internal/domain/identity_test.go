package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// usccWithCheck completes 17 characters with their GB 32100 check character.
func usccWithCheck(first17 string) string {
	sum, weight := 0, 1
	for i := 0; i < 17; i++ {
		sum += strings.IndexByte(usccCharset, first17[i]) * weight
		weight = weight * 3 % 31
	}
	return first17 + string(usccCharset[(31-sum%31)%31])
}

func TestValidUSCC(t *testing.T) {
	// 91350100M000100Y43 is the example of GB 32100; the others are completed with their check character.
	for _, code := range []string{"91350100M000100Y43", usccWithCheck("91310115MA1K4ABCD"), usccWithCheck("52110000500000000")} {
		if !ValidUSCC(code) {
			t.Errorf("%s should be valid", code)
		}
	}
	valid := usccWithCheck("91310115MA1K4ABCD")
	for name, code := range map[string]string{
		"wrong check character":   valid[:17] + string(usccCharset[(strings.IndexByte(usccCharset, valid[17])+1)%31]),
		"letter I not in the set": "91350100I000100Y43",
		"letter O not in the set": "9135010OM000100Y43",
		"17 characters":           "91350100M000100Y4",
		"lower case":              "91350100m000100y43",
		"19 characters":           "91350100M000100Y431",
	} {
		if ValidUSCC(code) {
			t.Errorf("%s: %s accepted", name, code)
		}
	}
}

func TestRecordAndRetireIdentifiers(t *testing.T) {
	p := newActive(t)
	uscc := PartyIdentifier{Type: IdentifierCNUSCC, Country: "CN", Value: " 91350100m000100y43 ", EvidenceRef: "national enterprise credit system, checked 2026-11-10"}
	if err := p.RecordIdentifier(uscc, "clerk", at); err != nil {
		t.Fatal(err)
	}
	got, ok := p.Identifier(IdentifierCNUSCC, "CN")
	if !ok || got.Value != "91350100M000100Y43" || got.RecordedBy != "clerk" {
		t.Fatalf("recorded = %+v %v; values are trimmed and upper-cased", got, ok)
	}
	if err := p.RecordIdentifier(uscc, "clerk", at); !errors.Is(err, ErrIdentifierExists) {
		t.Fatalf("second USCC: %v", err)
	}
	if err := p.RecordIdentifier(PartyIdentifier{Type: IdentifierAEO, Country: "CN", None: true, EvidenceRef: "customs credit platform: no AEO"}, "clerk", at); err != nil {
		t.Fatalf("recorded absence: %v", err)
	}
	if err := p.RetireIdentifier(IdentifierCNUSCC, "CN", "", "clerk", at); !errors.Is(err, ErrMissingEvidence) {
		t.Fatalf("retire without a reason: %v", err)
	}
	if err := p.RetireIdentifier(IdentifierCNCustoms, "CN", "typo", "clerk", at); !errors.Is(err, ErrUnknownIdentifier) {
		t.Fatalf("retire missing: %v", err)
	}
	if err := p.RetireIdentifier(IdentifierCNUSCC, "CN", "entered for the wrong branch", "clerk", at); err != nil {
		t.Fatal(err)
	}
	if err := p.RecordIdentifier(PartyIdentifier{Type: IdentifierCNUSCC, Country: "CN", Value: usccWithCheck("91310115MA1K4ABCD"), EvidenceRef: "licence"}, "clerk", at); err != nil {
		t.Fatalf("record after retiring: %v", err)
	}
	added, retired := p.IdentityChanges()
	if len(added) != 2 || len(retired) != 1 || retired[0].Reason != "entered for the wrong branch" {
		t.Fatalf("changes: %+v %+v; the retired USCC recorded in this session is not written", added, retired)
	}
	ids := p.Identifiers()
	if len(ids) != 2 || ids[0].Type != IdentifierAEO || ids[1].Type != IdentifierCNUSCC {
		t.Fatalf("identifiers = %+v", ids)
	}
	for _, e := range p.Pending() {
		if strings.Contains(fmt.Sprint(e), "91350100") {
			t.Fatalf("event %+v carries the code", e)
		}
	}
}

func TestIdentifierForms(t *testing.T) {
	for name, id := range map[string]PartyIdentifier{
		"USCC with a bad check":     {Type: IdentifierCNUSCC, Country: "CN", Value: "91350100M000100Y44"},
		"USCC issued elsewhere":     {Type: IdentifierCNUSCC, Country: "HK", Value: "91350100M000100Y43"},
		"customs code of 9":         {Type: IdentifierCNCustoms, Country: "CN", Value: "310123456"},
		"AEO of another country":    {Type: IdentifierAEO, Country: "SG", Value: "CN123456789012"},
		"EORI too long":             {Type: IdentifierEUEORI, Country: "DE", Value: "DE1234567890123456"},
		"EIN with a dash":           {Type: IdentifierUSEIN, Country: "US", Value: "12-3456789"},
		"EIN outside US":            {Type: IdentifierUSEIN, Country: "CA", Value: "123456789"},
		"registration with a space": {Type: IdentifierRegistration, Country: "GB", Value: "AB 12"},
		"absence with a value":      {Type: IdentifierAEO, Country: "CN", Value: "CN1", None: true},
		"no value":                  {Type: IdentifierRegistration, Country: "GB"},
		"unknown type":              {Type: "PASSPORT", Country: "CN", Value: "E12345678"},
		"country in lower case":     {Type: IdentifierRegistration, Country: "gb", Value: "12345"},
	} {
		id.EvidenceRef = "evidence"
		if _, err := id.normalized(); !errors.Is(err, ErrInvalidIdentifier) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for name, id := range map[string]PartyIdentifier{
		"customs code":     {Type: IdentifierCNCustoms, Country: "CN", Value: "3101234567"},
		"AEO of Singapore": {Type: IdentifierAEO, Country: "SG", Value: "SG123456789012"},
		"EORI":             {Type: IdentifierEUEORI, Country: "DE", Value: "DE123456789012345"},
		"EIN":              {Type: IdentifierUSEIN, Country: "US", Value: "123456789"},
		"UK company":       {Type: IdentifierRegistration, Country: "GB", Value: "01234567"},
	} {
		id.EvidenceRef = "evidence"
		if _, err := id.normalized(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := (PartyIdentifier{Type: IdentifierUSEIN, Country: "US", Value: "123456789"}).normalized(); !errors.Is(err, ErrMissingEvidence) {
		t.Fatalf("no evidence: %v", err)
	}
}

func TestEnglishName(t *testing.T) {
	p := newActive(t)
	if err := p.SetEnglishName("A Logistics Co., Ltd.", "", at); !errors.Is(err, ErrMissingEvidence) {
		t.Fatalf("no evidence: %v", err)
	}
	if err := p.SetEnglishName("示例物流", "licence", at); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("Chinese characters: %v", err)
	}
	if err := p.SetEnglishName("  A Logistics Co., Ltd. ", "licence", at); err != nil || p.EnglishName() != "A Logistics Co., Ltd." {
		t.Fatalf("set: %q %v", p.EnglishName(), err)
	}
	if err := p.SetEnglishName("A Logistics Co., Ltd.", "licence", at); !errors.Is(err, ErrNothingChanged) {
		t.Fatalf("same name: %v", err)
	}
	if err := p.SetEnglishName("", "name withdrawn", at); err != nil || p.EnglishName() != "" {
		t.Fatalf("clear: %v", err)
	}
}

func TestArchivedPartyIdentityIsClosed(t *testing.T) {
	p := newActive(t)
	if err := p.Suspend("risk", at); err != nil {
		t.Fatal(err)
	}
	if err := p.RecordIdentifier(PartyIdentifier{Type: IdentifierUSEIN, Country: "US", Value: "123456789", EvidenceRef: "e"}, "clerk", at); err != nil {
		t.Fatalf("a suspended party keeps its identity maintained: %v", err)
	}
	if err := p.RetireIdentifier(IdentifierUSEIN, "US", "wrong", "clerk", at); err != nil {
		t.Fatal(err)
	}
	if err := p.Archive("closed", at); err != nil {
		t.Fatal(err)
	}
	if err := p.RecordIdentifier(PartyIdentifier{Type: IdentifierUSEIN, Country: "US", Value: "123456789", EvidenceRef: "e"}, "clerk", at); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("archived: %v", err)
	}
	if err := p.SetEnglishName("X", "e", at); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("archived name: %v", err)
	}
}

func TestRestoreIdentity(t *testing.T) {
	p, err := RestoreParty("T", "P", "示例物流有限公司", "CN", PartyActive, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored := PartyIdentifier{Type: IdentifierCNUSCC, Country: "CN", Value: "91350100M000100Y43", EvidenceRef: "e", RecordedBy: "clerk", RecordedAt: at}
	if err = p.RestoreIdentity("Sample Logistics Co., Ltd.", []PartyIdentifier{stored, stored}); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("two active USCC: %v", err)
	}
	p, _ = RestoreParty("T", "P", "示例物流有限公司", "CN", PartyActive, nil)
	if err = p.RestoreIdentity("Sample Logistics Co., Ltd.", []PartyIdentifier{stored}); err != nil || len(p.Pending()) != 0 {
		t.Fatalf("restore: %v", err)
	}
	if added, retired := p.IdentityChanges(); len(added)+len(retired) != 0 {
		t.Fatal("restored identifiers are not changes")
	}
}

func TestAddressPhoneAndEmail(t *testing.T) {
	a := office("HQ")
	a.Phone, a.Email = "+86 21 5555 0000", "shipping@example.test"
	if _, err := a.normalized(); err != nil {
		t.Fatalf("organization phone and e-mail: %v", err)
	}
	home := warehouse("HOME")
	home.Residential, home.Phone = true, "+86 139 0000 0000"
	if _, err := home.normalized(); !errors.Is(err, ErrInvalidAddress) {
		t.Fatalf("residential with a phone: %v", err)
	}
	a.Email = "shipping@"
	if _, err := a.normalized(); !errors.Is(err, ErrInvalidAddress) {
		t.Fatalf("bad e-mail: %v", err)
	}
}
