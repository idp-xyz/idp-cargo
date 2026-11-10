package application

import (
	"context"
	"errors"
	"testing"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// dirFake is ORG as COM sees it: defaults by purpose, named addresses, and contacts behind an authorization.
type dirFake struct {
	defaults  map[string]com.AddressSnapshot // by purpose
	named     map[string]com.AddressSnapshot // by address id
	contact   *com.ContactSnapshot
	refuse    error // for contacts
	confirmed []string
}

func (d *dirFake) ConfirmedParty(_ context.Context, _ com.TenantID, party, role string, _ com.LegalEntityID, _ time.Time) (com.PartyRecord, error) {
	d.confirmed = append(d.confirmed, role)
	return com.PartyRecord{PartyID: party, PartyRevision: 2, LegalName: "Fixture", CountryCode: "CN", RoleCode: role,
		RoleValidity: kernel.ValidityInterval{From: time.Unix(0, 0)}, RoleEvidenceRef: "e"}, nil
}

func (d *dirFake) ActiveParty(_ context.Context, _ com.TenantID, party string) (com.PartyRecord, error) {
	d.confirmed = append(d.confirmed, "ACTIVE")
	return com.PartyRecord{PartyID: party, PartyRevision: 1, LegalName: "Fixture", CountryCode: "CN"}, nil
}

func (d *dirFake) Address(_ context.Context, _ com.TenantID, _, id, purpose string) (com.AddressSnapshot, bool, error) {
	if id != "" {
		a, ok := d.named[id]
		if !ok {
			return com.AddressSnapshot{}, false, ErrPartyNotConfirmed
		}
		return a, true, nil
	}
	a, ok := d.defaults[purpose]
	return a, ok, nil
}

func (d *dirFake) Contact(context.Context, com.TenantID, string, string, string) (com.ContactSnapshot, bool, error) {
	if d.refuse != nil {
		return com.ContactSnapshot{}, false, d.refuse
	}
	if d.contact == nil {
		return com.ContactSnapshot{}, false, nil
	}
	return *d.contact, true, nil
}

func orgAt(id string) com.AddressSnapshot {
	return com.AddressSnapshot{Source: com.FromORG, ORGAddressID: id, ShortCode: id, ORGVersion: 3, Name: "Fixture", Lines: []string{"1 Road"}, City: "Shanghai", CountryCode: "CN"}
}

func TestResolvePartyCarriesDefaultsAndOverrides(t *testing.T) {
	s := Scope{TenantID: "T", ActorID: "cs-1"}
	dir := &dirFake{defaults: map[string]com.AddressSnapshot{PurposeOffice: orgAt("HQ"), PurposePickup: orgAt("WH")},
		named:   map[string]com.AddressSnapshot{"OTHER": orgAt("OTHER")},
		contact: &com.ContactSnapshot{Source: com.FromORG, ORGContactID: "K", ORGVersion: 1, Name: "Test Person A", Email: "a@example.test"}}
	door := com.Transport{Mode: com.ModeRoad, Movement: com.DoorToPort}
	p, err := resolveParty(context.Background(), dir, s, "LE", time.Now(), door, PartyInput{Role: com.PartyShipper, PartyID: "S"})
	if err != nil || p.Address.ORGAddressID != "HQ" || p.Pickup == nil || p.Pickup.ORGAddressID != "WH" || p.Contact == nil || dir.confirmed[0] != com.PartyShipper {
		t.Fatalf("defaults: %+v %v", p, err)
	}
	override := &com.AddressSnapshot{Name: "Site", Lines: []string{"9 Port Road"}, City: "Ningbo", CountryCode: "CN"}
	p, err = resolveParty(context.Background(), dir, s, "LE", time.Now(), com.Transport{}, PartyInput{Role: com.PartyShipper, PartyID: "S",
		Address: AddressChoice{Override: override, Reason: "site office"}, Contact: ContactChoice{Use: ContactNone}})
	if err != nil || p.Address.Source != com.FromOrder || p.Address.ORGAddressID != "HQ" || p.Address.OverriddenBy != "cs-1" || p.Contact != nil || p.Pickup != nil {
		t.Fatalf("override naming the default it replaces: %+v %v", p.Address, err)
	}
	p, err = resolveParty(context.Background(), dir, s, "LE", time.Now(), com.Transport{}, PartyInput{Role: com.PartyNotify, PartyID: "N",
		Address: AddressChoice{AddressID: "OTHER"}})
	if err != nil || p.Address.ORGAddressID != "OTHER" || dir.confirmed[len(dir.confirmed)-1] != "ACTIVE" {
		t.Fatalf("notify party with a named address: %+v %v", p, err)
	}
	for name, tc := range map[string]struct {
		dir  *dirFake
		in   PartyInput
		t    com.Transport
		want error
	}{
		"no default office address": {&dirFake{}, PartyInput{Role: com.PartyShipper, PartyID: "S"}, com.Transport{}, com.ErrAddressRequired},
		"door delivery without a default": {&dirFake{defaults: map[string]com.AddressSnapshot{PurposeOffice: orgAt("HQ")}},
			PartyInput{Role: com.PartyConsignee, PartyID: "C"}, com.Transport{Movement: com.PortToDoor}, com.ErrAddressRequired},
		"override without a reason": {dir, PartyInput{Role: com.PartyShipper, PartyID: "S", Address: AddressChoice{Override: override}}, com.Transport{}, com.ErrInvalidParty},
		"contacts refused by ORG": {&dirFake{defaults: map[string]com.AddressSnapshot{PurposeOffice: orgAt("HQ")}, refuse: ErrUnauthorized},
			PartyInput{Role: com.PartyShipper, PartyID: "S"}, com.Transport{}, ErrUnauthorized},
		"TO ORDER with an ORG address": {dir, PartyInput{Role: com.PartyConsignee, ToOrder: "TO ORDER", Address: AddressChoice{AddressID: "OTHER"}}, com.Transport{}, com.ErrInvalidParty},
		"TO ORDER delivery from ORG":   {dir, PartyInput{Role: com.PartyConsignee, ToOrder: "TO ORDER", Delivery: &AddressChoice{AddressID: "OTHER"}}, com.Transport{}, com.ErrInvalidParty},
		"pickup on a consignee":        {dir, PartyInput{Role: com.PartyConsignee, PartyID: "C", Pickup: &AddressChoice{}}, com.Transport{}, com.ErrInvalidParty},
		"no party":                     {dir, PartyInput{Role: com.PartyShipper}, com.Transport{}, com.ErrInvalidParty},
	} {
		if _, err := resolveParty(context.Background(), tc.dir, s, "LE", time.Now(), tc.t, tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if _, err := resolveParties(context.Background(), nil, s, "LE", time.Now(), com.Transport{}, []PartyInput{{Role: com.PartyShipper, PartyID: "S"}}); !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("parties without a directory: %v", err)
	}
}
