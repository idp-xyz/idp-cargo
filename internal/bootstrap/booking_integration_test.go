//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func (allowAll) AuthorizeBookingAction(context.Context, string, string, string, string) error {
	return nil
}

// BOOK V0.1 and ORG V0.6 with the real contexts and the S1 location reference data: ORG records an airline's
// IATA designator and AWB prefix (issued internationally, country ZZ) and approves it as CARRIER; the forwarder
// books the airline for a PHY air consolidation, records the request as sent and the airline's confirmation
// with the MAWB number, which BOOK checks against the airline's prefix and finds again by its number; a
// co-load needs a CO_LOADER, and the carriers' CNSHA resolves to the port of Shanghai.
func TestBookingAnAirConsolidationWithTheAirline(t *testing.T) {
	ctx := context.Background()
	platform := openPlatform(t)
	orgModule, err := platform.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := platform.RATE(orgModule, allowAll{}, marginFloor(2000))
	if err != nil {
		t.Fatal(err)
	}
	comModule, err := platform.COM(orgModule, rateModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	_, phyModule, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	bookModule, err := platform.BOOK(orgModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-book-")
	seedORG(t, platform, tenant, "airline-mu", "coloader-x")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	etd := at.Add(80 * time.Hour)
	iata := func(code string) kernel.LocationCode {
		return kernel.LocationCode{Scheme: kernel.IATAAirport, Code: code}
	}
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	evidence := func(h int) bookapi.Evidence {
		return bookapi.Evidence{SHA256: strings.Repeat("1e", 32), StorageID: "mail/confirmation.eml", MediaType: "message/rfc822", CapturedAt: at.Add(time.Duration(h) * time.Hour)}
	}

	// ORG: the airline's codes, then its CARRIER role; a code issued by IATA is refused with a state as issuer.
	identifier := func(command string, revision int64, kind, country, value string) error {
		_, err := orgModule.RecordPartyIdentifier(ctx, orgapi.RecordPartyIdentifier{CommandID: command, TenantID: tenant, ActorID: "md-1",
			PartyID: "airline-mu", ExpectedRevision: revision, Type: kind, Country: country, Value: value, EvidenceRef: "IATA airline coding directory"})
		return err
	}
	if err = identifier("mu-cn", 1, orgapi.IdentifierIATAAirline, "CN", "MU"); !errors.Is(err, orgapi.ErrRejected) {
		t.Fatalf("an IATA designator issued by a state: %v", err)
	}
	if err = errors.Join(identifier("mu-designator", 1, orgapi.IdentifierIATAAirline, orgapi.InternationalIssuer, "MU"),
		identifier("mu-prefix", 2, orgapi.IdentifierIATAAWBPrefix, orgapi.InternationalIssuer, "781")); err != nil {
		t.Fatal(err)
	}
	for party, role := range map[string]string{"airline-mu": orgapi.RoleCarrier, "coloader-x": orgapi.RoleCoLoader} {
		revision := int64(1)
		if party == "airline-mu" {
			revision = 3
		}
		if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-" + party, TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
			PartyID: party, ExpectedRevision: revision, RoleCode: role, ValidFrom: at.Add(-time.Hour), EvidenceRef: "carrier contract", OccurredAt: at}); err != nil {
			t.Fatal(err)
		}
	}

	// PHY: the air consolidation the booking is for.
	if _, err = phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		Mode: phyapi.ModeAir, Kind: phyapi.KindAirConsol, OperatingLegalEntityID: "LE", Origin: iata("PVG"), Destination: iata("JFK"),
		PlannedDeparture: etd, HouseCutOff: etd.Add(-12 * time.Hour), OccurredAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	kg := kernel.Quantity{Unit: "KGM", Amount: 1500}
	if _, err = bookModule.DraftCarrierBooking(ctx, bookapi.DraftCarrierBooking{CommandID: "draft", TenantID: tenant, ActorID: "ops-1", BookingID: "BK-1",
		Kind: bookapi.KindAirConsol, LegalEntityID: "LE", CarrierPartyID: "airline-mu", ConsolidationID: "C-1",
		Request: bookapi.Request{Legs: []bookapi.RequestedLeg{{Loading: iata("PVG"), Discharge: unlocode("USJFK"), Flight: "MU587",
			DepartureDate: etd.Format(time.DateOnly)}}, Capacity: bookapi.Capacity{Pieces: 30, GrossWeight: kg}, HandlingCodes: []string{"GEN"}},
		OccurredAt: at.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = bookModule.RecordBookingRequestSent(ctx, bookapi.RecordBookingRequestSent{CommandID: "sent", TenantID: tenant, ActorID: "ops-1",
		BookingID: "BK-1", ExpectedRevision: 1, Channel: bookapi.ChannelCarrierPortal, Evidence: evidence(3), OccurredAt: at.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	line := []bookapi.CapacityLine{{Capacity: bookapi.Capacity{Pieces: 30, GrossWeight: kg}, Legs: []bookapi.ConfirmedLeg{{Loading: iata("PVG"),
		Discharge: iata("JFK"), Flight: "MU587", ETD: etd, ETA: etd.Add(14 * time.Hour), CutOffs: []bookapi.CutOff{
			{Kind: bookapi.CutOffCargoAcceptance, At: etd.Add(-5 * time.Hour), TimeZone: "Asia/Shanghai"},
			{Kind: bookapi.CutOffSecurity, At: etd.Add(-6 * time.Hour), TimeZone: "Asia/Shanghai"}}}}}}
	confirm := func(command, mawb string) error {
		_, err := bookModule.RecordCarrierConfirmation(ctx, bookapi.RecordCarrierConfirmation{CommandID: command, TenantID: tenant, ActorID: "ops-1",
			BookingID: "BK-1", ExpectedRevision: 2, Answer: bookapi.Answer{Channel: bookapi.ChannelEmail, Evidence: evidence(4), CarrierTime: at.Add(4 * time.Hour),
				References: []bookapi.CarrierReference{{Type: bookapi.ReferenceMAWBNumber, Value: mawb}, {Type: bookapi.ReferenceBookingNumber, Value: "mu-bk-0815"}}},
			Lines: line, OccurredAt: at.Add(4 * time.Hour)})
		return err
	}
	if err = confirm("confirm-ca", "999-12345675"); !errors.Is(err, bookapi.ErrRejected) {
		t.Fatalf("a MAWB of another airline's prefix: %v", err)
	}
	if err = confirm("confirm", "781-12345675"); err != nil {
		t.Fatal(err)
	}
	b, err := bookModule.FindBookingByCarrierReference(ctx, bookapi.FindBookingByCarrierReference{TenantID: tenant, CarrierPartyID: "airline-mu",
		Type: bookapi.ReferenceMAWBNumber, Value: "78112345675"})
	if err != nil || b.BookingID != "BK-1" || b.Status != bookapi.StatusConfirmed || b.MAWBNumber != "781-12345675" || b.CarrierIATADesignator != "MU" ||
		b.CarrierPartyRevision != 4 || b.Request.Legs[0].Loading.Code != unlocode("CNPVG") || b.Capacity.Lines[0].Legs[0].Discharge.IATA != "JFK" ||
		b.Request.LocationEdition != "UNLOCODE-2026-11-01-S1" || len(b.References) != 2 {
		t.Fatalf("the booking by its MAWB: %+v %v", b, err)
	}
	held, err := bookModule.ListCapacityForConsumer(ctx, bookapi.ListCapacityForConsumer{TenantID: tenant, ConsolidationID: "C-1"})
	if err != nil || len(held) != 1 || held[0].Capacity.Lines[0].Legs[0].CutOffs[1].Kind != bookapi.CutOffSecurity {
		t.Fatalf("the consolidation's capacity: %+v %v", held, err)
	}

	// A co-load: SEA space from another consolidator; CNSHA is the carriers' code for the port of Shanghai.
	if _, err = bookModule.DraftCarrierBooking(ctx, bookapi.DraftCarrierBooking{CommandID: "coload", TenantID: tenant, ActorID: "ops-1", BookingID: "BK-2",
		Kind: bookapi.KindSeaLCLCoLoad, LegalEntityID: "LE", CarrierPartyID: "coloader-x", ConsolidationID: "C-SEA",
		Request: bookapi.Request{Legs: []bookapi.RequestedLeg{{Loading: unlocode("CNSHA"), Discharge: unlocode("DEHAM"), DepartureDate: etd.Format(time.DateOnly)}},
			Capacity: bookapi.Capacity{GrossWeight: kg, Volume: kernel.Quantity{Unit: "DMQ", Amount: 6000}}}, OccurredAt: at.Add(5 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	coload, err := bookModule.GetCarrierBooking(ctx, bookapi.GetCarrierBooking{TenantID: tenant, BookingID: "BK-2"})
	if err != nil || coload.Request.Legs[0].Loading.Code != unlocode("CNSHG") || coload.CarrierRole != orgapi.RoleCoLoader {
		t.Fatalf("co-load: %+v %v", coload, err)
	}
	if _, err = bookModule.DraftCarrierBooking(ctx, bookapi.DraftCarrierBooking{CommandID: "coload-airline", TenantID: tenant, ActorID: "ops-1", BookingID: "BK-3",
		Kind: bookapi.KindSeaLCLCoLoad, LegalEntityID: "LE", CarrierPartyID: "airline-mu", ConsolidationID: "C-SEA",
		Request: bookapi.Request{Legs: []bookapi.RequestedLeg{{Loading: unlocode("CNSHA"), Discharge: unlocode("DEHAM"), DepartureDate: etd.Format(time.DateOnly)}},
			Capacity: bookapi.Capacity{GrossWeight: kg, Volume: kernel.Quantity{Unit: "DMQ", Amount: 6000}}}, OccurredAt: at.Add(5 * time.Hour)}); !errors.Is(err, bookapi.ErrRejected) {
		t.Fatalf("a co-load from a party that is no CO_LOADER: %v", err)
	}
}
