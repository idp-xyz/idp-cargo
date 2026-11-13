package domain

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

func orgAddress(id, code string) *AddressSnapshot {
	return &AddressSnapshot{Source: FromORG, ORGAddressID: id, ShortCode: code, ORGVersion: 1, Name: "Fixture Trading Co., Ltd.",
		Lines: []string{"88 Century Avenue"}, City: "Shanghai", CountryCode: "CN", UNLocode: "CNSHA", Phone: "+86 21 5555 0000"}
}

func overrideAddress(replaced string) *AddressSnapshot {
	a := &AddressSnapshot{Source: FromOrder, Name: "Fixture Warehouse", Lines: []string{"Gate 3, 1 Harbour Road"}, City: "Ningbo",
		CountryCode: "CN", OverriddenBy: "ops-1", OverrideReason: "the customer asked for collection at its new warehouse"}
	if replaced != "" {
		a.ORGAddressID, a.ORGVersion = replaced, 2
	}
	return a
}

func named(role, party string) OrderParty {
	r := PartyRecord{PartyID: party, PartyRevision: 3, LegalName: "示例贸易有限公司", EnglishName: "Sample Trading Co., Ltd.", CountryCode: "CN",
		Identifiers: []PartyIdentifier{{Type: "CN_USCC", Country: "CN", Value: "91350100M000100Y43"}, {Type: "AEO", Country: "CN", None: true}}}
	if role != PartyNotify {
		r.RoleCode, r.RoleValidity, r.RoleEvidenceRef = role, kernel.ValidityInterval{From: at}, role+"-approval"
	}
	return OrderParty{Role: role, Kind: KindParty, Party: r, Address: orgAddress("A-"+party, "HQ")}
}

func toOrder(text string) OrderParty {
	return OrderParty{Role: PartyConsignee, Kind: KindToOrder, ToOrderText: text}
}

func TestRecordOrderWithPartiesAndTransport(t *testing.T) {
	d := draft(palletLine)
	d.Transport = Transport{Mode: ModeSea, Movement: PortToPort}
	contact := &ContactSnapshot{Source: FromORG, ORGContactID: "C-1", ORGVersion: 1, Name: "Test Person A", Email: "desk-a@example.test"}
	shipper := named(PartyShipper, "S")
	shipper.Contact = contact
	d.InitialParties = []OrderParty{shipper, toOrder(" to order of  the bank of fixtures "), named(PartyNotify, "N")}
	o, err := RecordOrder(d, 0, recordAt, recordAt)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := o.Party(PartyConsignee)
	if c.ToOrderText != "TO ORDER OF THE BANK OF FIXTURES" || c.Version != 1 {
		t.Fatalf("consignee = %+v", c)
	}
	s, _ := o.Party(PartyShipper)
	if !s.HasContact || s.Overridden || s.SetBy != "sales-1" {
		t.Fatalf("shipper = %+v", s)
	}
	if got := eventList(o); got != "[CustomerOrderRecorded.v1 OrderPartySet.v1 OrderPartySet.v1 OrderPartySet.v1]" {
		t.Fatalf("events = %s", got)
	}
	if o.Transport() != d.Transport || len(o.AddedTransport()) != 1 || len(o.AddedParties()) != 3 {
		t.Fatalf("transport %+v, %d transport and %d party versions to store", o.Transport(), len(o.AddedTransport()), len(o.AddedParties()))
	}
	for _, e := range o.Pending() {
		if e.Type == eventPartySet && (e.PartyRole == "" || e.PartyVersion != 1 || e.PartyKind == "") {
			t.Fatalf("party event %+v", e)
		}
	}
}

func TestPartyRulesOfTheTransport(t *testing.T) {
	for name, tc := range map[string]struct {
		transport Transport
		parties   []OrderParty
		want      error
	}{
		"TO ORDER without a notify party":   {Transport{Mode: ModeSea}, []OrderParty{toOrder("TO ORDER")}, ErrPartyRule},
		"TO ORDER on an air order":          {Transport{Mode: ModeAir}, []OrderParty{toOrder("TO ORDER"), named(PartyNotify, "N")}, ErrPartyRule},
		"TO ORDER without a transport mode": {Transport{}, []OrderParty{toOrder("TO ORDER"), named(PartyNotify, "N")}, ErrPartyRule},
		"door pickup without the address":   {Transport{Mode: ModeRoad, Movement: DoorToPort}, []OrderParty{named(PartyShipper, "S")}, ErrPartyRule},
		"door delivery without the address": {Transport{Mode: ModeAir, Movement: PortToDoor}, []OrderParty{named(PartyConsignee, "C")}, ErrPartyRule},
		"TO ORDER text not to order":        {Transport{Mode: ModeSea}, []OrderParty{toOrder("BEARER"), named(PartyNotify, "N")}, ErrInvalidParty},
		"TO ORDER for the shipper":          {Transport{Mode: ModeSea}, []OrderParty{{Role: PartyShipper, Kind: KindToOrder, ToOrderText: "TO ORDER"}}, ErrInvalidParty},
		"unknown transport mode":            {Transport{Mode: "PIPELINE"}, nil, ErrInvalidOrder},
	} {
		d := draft(palletLine)
		d.Transport, d.InitialParties = tc.transport, tc.parties
		if _, err := RecordOrder(d, 0, recordAt, recordAt); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	d := draft(palletLine)
	shipper := named(PartyShipper, "S")
	shipper.Pickup = overrideAddress("")
	consignee := named(PartyConsignee, "C")
	consignee.Delivery = orgAddress("D-C", "DOCK")
	d.Transport, d.InitialParties = Transport{Mode: ModeAir, Movement: DoorToDoor}, []OrderParty{shipper, consignee}
	o, err := RecordOrder(d, 0, recordAt, recordAt)
	if err != nil {
		t.Fatalf("door to door with both addresses: %v", err)
	}
	if s, _ := o.Party(PartyShipper); !s.Overridden {
		t.Fatal("a pickup address given on the order marks the version as overridden")
	}
}

func TestPartyRecordChecks(t *testing.T) {
	notifyWithRole := named(PartyNotify, "N")
	notifyWithRole.Party.RoleCode, notifyWithRole.Party.RoleEvidenceRef = PartyNotify, "x"
	shipperWithoutRole := named(PartyShipper, "S")
	shipperWithoutRole.Party.RoleCode = ""
	noAddress := named(PartyShipper, "S")
	noAddress.Address = nil
	silentOverride := named(PartyShipper, "S")
	silentOverride.Address = overrideAddress("")
	silentOverride.Address.OverrideReason = ""
	residentialPhone := named(PartyConsignee, "C")
	residentialPhone.Address.Residential = true
	pickupOnConsignee := named(PartyConsignee, "C")
	pickupOnConsignee.Pickup = orgAddress("P", "P")
	badIdentifier := named(PartyShipper, "S")
	badIdentifier.Party.Identifiers = []PartyIdentifier{{Type: "AEO", Country: "CN", Value: "CN1", None: true}}
	for name, p := range map[string]OrderParty{
		"notify party with an ORG role":     notifyWithRole,
		"shipper without the role":          shipperWithoutRole,
		"named party without an address":    noAddress,
		"override without a reason":         silentOverride,
		"residential address with a phone":  residentialPhone,
		"pickup address on the consignee":   pickupOnConsignee,
		"identifier both absent and valued": badIdentifier,
	} {
		d := draft(palletLine)
		d.InitialParties = []OrderParty{p}
		if _, err := RecordOrder(d, 0, recordAt, recordAt); !errors.Is(err, ErrInvalidParty) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSetPartyAndChangeTransport(t *testing.T) {
	o := recorded(t)
	o.Acknowledge()
	later := recordAt.Add(time.Hour)
	if err := o.SetParty(named(PartyShipper, "S"), "ops-1", later, later); err != nil {
		t.Fatal(err)
	}
	if err := o.SetParty(named(PartyShipper, "S2"), "ops-1", later, later); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("replacing without a reason: %v", err)
	}
	replacement := named(PartyShipper, "S2")
	replacement.Reason = "the goods ship from the parent company"
	if err := o.SetParty(replacement, "ops-1", later, later); err != nil {
		t.Fatal(err)
	}
	if s, _ := o.Party(PartyShipper); s.Version != 2 || s.Party.PartyID != "S2" {
		t.Fatalf("shipper = %+v", s)
	}
	if err := o.ChangeTransport(Transport{Mode: ModeSea, Movement: DoorToPort}, "door pickup agreed", "ops-1", later, later); !errors.Is(err, ErrPartyRule) {
		t.Fatalf("door pickup without the shipper's pickup address: %v", err)
	}
	withPickup := named(PartyShipper, "S2")
	withPickup.Reason, withPickup.Pickup = "pickup address for the door movement", orgAddress("PU", "PICK")
	if err := o.SetParty(withPickup, "ops-1", later, later); err != nil {
		t.Fatal(err)
	}
	if err := o.ChangeTransport(Transport{Mode: ModeSea, Movement: DoorToPort}, "door pickup agreed", "ops-1", later, later); err != nil {
		t.Fatal(err)
	}
	if err := o.ChangeTransport(Transport{Mode: ModeSea, Movement: DoorToPort}, "again", "ops-1", later, later); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("unchanged transport: %v", err)
	}
	if err := o.SetParty(toOrder("TO ORDER"), "ops-1", later, later); !errors.Is(err, ErrPartyRule) {
		t.Fatalf("TO ORDER before a notify party: %v", err)
	}
	if err := o.SetParty(named(PartyNotify, "N"), "ops-1", later, later); err != nil {
		t.Fatal(err)
	}
	if err := o.SetParty(toOrder("TO ORDER"), "ops-1", later, later); err != nil {
		t.Fatal(err)
	}
	if err := o.ChangeTransport(Transport{Mode: ModeAir, Movement: DoorToPort}, "switch to air", "ops-1", later, later); !errors.Is(err, ErrPartyRule) {
		t.Fatalf("air with a TO ORDER consignee: %v", err)
	}
	if len(o.AddedParties()) != 5 || len(o.AddedTransport()) != 1 {
		t.Fatalf("%d party and %d transport versions to store", len(o.AddedParties()), len(o.AddedTransport()))
	}
	state := o.State()
	state.Status = o.Status()
	restored, err := RestoreOrder(state)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(restored.AddedParties()) != 0 || restored.Transport() != o.Transport() || len(restored.Parties()) != 3 {
		t.Fatalf("restored: %+v", restored.Parties())
	}
	if err = o.Cancel("ops-1", later, MinContactRetentionMonths); err != nil {
		t.Fatal(err)
	}
	if err = o.SetParty(named(PartyNotify, "N2"), "ops-1", later, later); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("set party on a cancelled order: %v", err)
	}
}

// N1-PHY OD-9: an order requests LCL or FCL on SEA and LOOSE on AIR, with its mode; a change is a transport version.
func TestContainerModeOfTheOrder(t *testing.T) {
	for name, tr := range map[string]Transport{
		"FCL on an air order":          {Mode: ModeAir, ContainerMode: ContainerFCL},
		"LOOSE on a sea order":         {Mode: ModeSea, ContainerMode: ContainerLoose},
		"a container mode, no mode":    {ContainerMode: ContainerLCL},
		"a container mode on road":     {Mode: ModeRoad, ContainerMode: ContainerLCL},
		"an unknown container mode":    {Mode: ModeSea, ContainerMode: "BULK"},
		"a container mode in capitals": {Mode: ModeSea, ContainerMode: "lcl"},
	} {
		d := draft(palletLine)
		d.Transport = tr
		if _, err := RecordOrder(d, 0, recordAt, recordAt); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, tr := range []Transport{{Mode: ModeSea, ContainerMode: ContainerLCL}, {Mode: ModeSea, ContainerMode: ContainerFCL},
		{Mode: ModeAir, ContainerMode: ContainerLoose}, {Mode: ModeSea}} {
		d := draft(palletLine)
		d.Transport = tr
		o, err := RecordOrder(d, 0, recordAt, recordAt)
		if err != nil || o.Transport() != tr {
			t.Fatalf("%+v: %+v %v", tr, o, err)
		}
	}
}

// Commands recorded before the container mode hash as they did: an empty one is left out of the JSON.
func TestTransportHashesAsBefore(t *testing.T) {
	blob, err := json.Marshal(Transport{Mode: ModeSea, Movement: PortToPort})
	if err != nil || string(blob) != `{"Mode":"SEA","Movement":"PORT_TO_PORT"}` {
		t.Fatalf("%s %v", blob, err)
	}
}
