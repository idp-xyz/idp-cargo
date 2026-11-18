package domain

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// carrierFacts are FINAL facts of a movement whose carrier issued a master bill: kind CARRIER_MBL, the carrier in role
// CARRIER.
func carrierFacts() OceanFacts {
	f := oceanFacts(2)
	f.CarrierPartyRevision, f.CarrierRole, f.MasterBillKind = 5, "CARRIER", "CARRIER_MBL"
	return f
}

func carrierEnteredMBL() CarrierEntered {
	return CarrierEntered{Release: ReleaseOriginals, Originals: 3, IssuePlace: "CNSHA", IssuedOn: "2026-11-11",
		Evidence: CarrierEvidence{SHA256: strings.Repeat("ef", 32), StorageID: "evidence/mbl-1.pdf", MediaType: "application/pdf", CapturedAt: t0.Add(2 * time.Hour)}}
}

var carrierAt = t0.Add(10 * time.Hour) // 18:00 UTC on 2026-11-11, 02:00 on the 12th in China

func record(t *testing.T, f OceanFacts, e CarrierEntered) *CarrierDocument {
	t.Helper()
	x, err := RecordCarrierDocument("T-1", "CD-1", f, e, "clerk-1", carrierAt)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestACarrierDocumentTakesItsKindAndCapacityFromTheFacts(t *testing.T) {
	for _, tc := range []struct {
		billKind, role string
		kind           Kind
		capacity       string
		entered        CarrierEntered
		release        string
	}{
		{"CARRIER_MBL", "CARRIER", KindMBL, "CARRIER", carrierEnteredMBL(), "ORIGINALS"},
		{"CARRIER_SEA_WAYBILL", "CARRIER", KindMasterSeaWaybill, "CARRIER", func() CarrierEntered { e := carrierEnteredMBL(); e.Release, e.Originals = "", 0; return e }(),
			"SEA_WAYBILL"},
		{"COLOAD_BILL", "CO_LOADER", KindMBL, "CO_LOADER", carrierEnteredMBL(), "ORIGINALS"},
	} {
		f := carrierFacts()
		f.MasterBillKind, f.CarrierRole = tc.billKind, tc.role
		st := record(t, f, tc.entered).State()
		if st.Kind != tc.kind || st.Doc.IssuerCapacity != tc.capacity || st.Doc.Kind != string(tc.kind) || st.Doc.ReleaseType != tc.release || st.Number != "LINU2611000457" ||
			st.CarrierRole != tc.role || st.State != CarrierIssued || st.Revision != 1 || st.SourceVersion != 2 || st.MovementID != "OM-1" {
			t.Errorf("%s with %s: %+v", tc.billKind, tc.role, st)
		}
	}
	// every other pair is refused, and so are kinds that are not a master bill's
	kinds := []string{"CARRIER_MBL", "CARRIER_SEA_WAYBILL", "COLOAD_BILL", "", "HOUSE", "carrier_mbl"}
	roles := []string{"CARRIER", "CO_LOADER", "", "SHIPPER"}
	good := map[[2]string]bool{{"CARRIER_MBL", "CARRIER"}: true, {"CARRIER_SEA_WAYBILL", "CARRIER"}: true, {"COLOAD_BILL", "CO_LOADER"}: true}
	for _, k := range kinds {
		for _, r := range roles {
			if good[[2]string{k, r}] {
				continue
			}
			f := carrierFacts()
			f.MasterBillKind, f.CarrierRole = k, r
			if _, err := RecordCarrierDocument("T-1", "CD-1", f, carrierEnteredMBL(), "clerk-1", carrierAt); !errors.Is(err, ErrInvalidDocument) {
				t.Errorf("%q with %q: %v", k, r, err)
			}
		}
	}
}

func TestTheOtherDocumentsStillKnowFourKindsOnly(t *testing.T) {
	for _, k := range []Kind{KindMBL, KindMasterSeaWaybill} {
		if k.valid() || k.House() || k.SeaHouse() {
			t.Errorf("%s is a kind DOC issues", k)
		}
	}
	for _, k := range []Kind{KindHAWB, KindMAWB, KindHBL, KindHouseSeaWaybill} {
		if !k.valid() {
			t.Errorf("%s is no longer valid", k)
		}
	}
}

func TestCarrierReleaseAndOriginals(t *testing.T) {
	with := func(release string, originals int) CarrierEntered {
		e := carrierEnteredMBL()
		e.Release, e.Originals = release, originals
		return e
	}
	waybill := func(f OceanFacts) OceanFacts { f.MasterBillKind = "CARRIER_SEA_WAYBILL"; return f }
	for name, tc := range map[string]struct {
		facts   OceanFacts
		entered CarrierEntered
		release string
		count   int
		ok      bool
	}{
		"three originals":              {carrierFacts(), with("ORIGINALS", 3), "ORIGINALS", 3, true},
		"one original":                 {carrierFacts(), with("ORIGINALS", 1), "ORIGINALS", 1, true},
		"nine originals":               {carrierFacts(), with("ORIGINALS", 9), "ORIGINALS", 9, true},
		"express":                      {carrierFacts(), with("EXPRESS", 0), "EXPRESS", 0, true},
		"no originals":                 {carrierFacts(), with("ORIGINALS", 0), "", 0, false},
		"ten originals":                {carrierFacts(), with("ORIGINALS", 10), "", 0, false},
		"negative originals":           {carrierFacts(), with("ORIGINALS", -1), "", 0, false},
		"express with originals":       {carrierFacts(), with("EXPRESS", 1), "", 0, false},
		"an MBL with an empty release": {carrierFacts(), with("", 0), "", 0, false},
		"an MBL as a sea waybill":      {carrierFacts(), with("SEA_WAYBILL", 0), "", 0, false},
		"an unknown release":           {carrierFacts(), with("CONSIGNEE_NAMED", 0), "", 0, false},
		"a sea waybill":                {waybill(carrierFacts()), with("SEA_WAYBILL", 0), "SEA_WAYBILL", 0, true},
		"a sea waybill, empty":         {waybill(carrierFacts()), with("", 0), "SEA_WAYBILL", 0, true},
		"a sea waybill with originals": {waybill(carrierFacts()), with("SEA_WAYBILL", 2), "", 0, false},
		"a sea waybill as originals":   {waybill(carrierFacts()), with("ORIGINALS", 3), "", 0, false},
		"a sea waybill as express":     {waybill(carrierFacts()), with("EXPRESS", 0), "", 0, false},
		"originals and no release":     {waybill(carrierFacts()), with("", 2), "", 0, false},
	} {
		x, err := RecordCarrierDocument("T-1", "CD-1", tc.facts, tc.entered, "clerk-1", carrierAt)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			} else if st := x.State(); st.Doc.ReleaseType != tc.release || st.Doc.Originals != tc.count {
				t.Errorf("%s: %s %d", name, st.Doc.ReleaseType, st.Doc.Originals)
			}
		} else if !errors.Is(err, ErrIncomplete) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestACarrierDocumentIsEnteredCorrectly(t *testing.T) {
	change := func(fn func(*CarrierEntered)) CarrierEntered {
		e := carrierEnteredMBL()
		fn(&e)
		return e
	}
	for name, tc := range map[string]struct {
		entered CarrierEntered
		ok      bool
	}{
		"as entered":                      {carrierEnteredMBL(), true},
		"a place with a digit":            {change(func(e *CarrierEntered) { e.IssuePlace = "CNSH2" }), true},
		"an empty place":                  {change(func(e *CarrierEntered) { e.IssuePlace = "" }), false},
		"a place of four":                 {change(func(e *CarrierEntered) { e.IssuePlace = "CNSH" }), false},
		"a place of six":                  {change(func(e *CarrierEntered) { e.IssuePlace = "CNSHAA" }), false},
		"a lower-case place":              {change(func(e *CarrierEntered) { e.IssuePlace = "cnsha" }), false},
		"a place with a 1":                {change(func(e *CarrierEntered) { e.IssuePlace = "CNSH1" }), false},
		"a place with a digit as country": {change(func(e *CarrierEntered) { e.IssuePlace = "2NSHA" }), false},
		"an empty date":                   {change(func(e *CarrierEntered) { e.IssuedOn = "" }), false},
		"a date written the other way":    {change(func(e *CarrierEntered) { e.IssuedOn = "11/11/2026" }), false},
		"a date with a time":              {change(func(e *CarrierEntered) { e.IssuedOn = "2026-11-11T08:00:00Z" }), false},
		"a date without zeros":            {change(func(e *CarrierEntered) { e.IssuedOn = "2026-1-5" }), false},
		"a month 13":                      {change(func(e *CarrierEntered) { e.IssuedOn = "2026-13-01" }), false},
		"the 30th of February":            {change(func(e *CarrierEntered) { e.IssuedOn = "2026-02-30" }), false},
		"a date in the past":              {change(func(e *CarrierEntered) { e.IssuedOn = "2020-01-31" }), true},
		"tomorrow in China":               {change(func(e *CarrierEntered) { e.IssuedOn = "2026-11-13" }), true},
		"the day after tomorrow":          {change(func(e *CarrierEntered) { e.IssuedOn = "2026-11-14" }), false},
		"a date a year ahead":             {change(func(e *CarrierEntered) { e.IssuedOn = "2027-11-11" }), false},
		"a digest of 63":                  {change(func(e *CarrierEntered) { e.Evidence.SHA256 = strings.Repeat("e", 63) }), false},
		"a digest of 65":                  {change(func(e *CarrierEntered) { e.Evidence.SHA256 = strings.Repeat("e", 65) }), false},
		"an upper-case digest":            {change(func(e *CarrierEntered) { e.Evidence.SHA256 = strings.Repeat("E", 64) }), false},
		"a digest that is not hex":        {change(func(e *CarrierEntered) { e.Evidence.SHA256 = strings.Repeat("g", 64) }), false},
		"no storage id":                   {change(func(e *CarrierEntered) { e.Evidence.StorageID = "" }), false},
		"a blank storage id":              {change(func(e *CarrierEntered) { e.Evidence.StorageID = "  " }), false},
		"a storage id of 200":             {change(func(e *CarrierEntered) { e.Evidence.StorageID = strings.Repeat("s", 200) }), true},
		"a storage id of 201":             {change(func(e *CarrierEntered) { e.Evidence.StorageID = strings.Repeat("s", 201) }), false},
		"no media type":                   {change(func(e *CarrierEntered) { e.Evidence.MediaType = "" }), false},
		"a blank media type":              {change(func(e *CarrierEntered) { e.Evidence.MediaType = " " }), false},
		"a media type of 100":             {change(func(e *CarrierEntered) { e.Evidence.MediaType = strings.Repeat("m", 100) }), true},
		"a media type of 101":             {change(func(e *CarrierEntered) { e.Evidence.MediaType = strings.Repeat("m", 101) }), false},
		"no capture time":                 {change(func(e *CarrierEntered) { e.Evidence.CapturedAt = time.Time{} }), false},
		"captured at the record":          {change(func(e *CarrierEntered) { e.Evidence.CapturedAt = carrierAt }), true},
		"captured after the record":       {change(func(e *CarrierEntered) { e.Evidence.CapturedAt = carrierAt.Add(time.Nanosecond) }), false},
	} {
		_, err := RecordCarrierDocument("T-1", "CD-1", carrierFacts(), tc.entered, "clerk-1", carrierAt)
		if tc.ok != (err == nil) || err != nil && !errors.Is(err, ErrInvalidChange) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// the issue date is judged by the day in China: at 17:00 UTC on the 11th it is already the 12th there
	late := t0.Add(9 * time.Hour)
	for day, ok := range map[string]bool{"2026-11-13": true, "2026-11-14": false} {
		e := carrierEnteredMBL()
		e.IssuedOn = day
		if _, err := RecordCarrierDocument("T-1", "CD-1", carrierFacts(), e, "clerk-1", late); ok != (err == nil) {
			t.Errorf("%s at %s: %v", day, late, err)
		}
	}
	early := time.Date(2026, 11, 11, 15, 59, 0, 0, time.UTC) // 23:59 on the 11th in China
	for day, ok := range map[string]bool{"2026-11-12": true, "2026-11-13": false} {
		e := carrierEnteredMBL()
		e.IssuedOn, e.Evidence.CapturedAt = day, t0
		if _, err := RecordCarrierDocument("T-1", "CD-1", carrierFacts(), e, "clerk-1", early); ok != (err == nil) {
			t.Errorf("%s at %s: %v", day, early, err)
		}
	}
}

func TestACarrierDocumentNeedsFinalCompleteFacts(t *testing.T) {
	if _, err := RecordCarrierDocument("T-1", "CD-1", func() OceanFacts { f := carrierFacts(); f.Final = false; return f }(), carrierEnteredMBL(), "clerk-1",
		carrierAt); !errors.Is(err, ErrSourceNotFinal) {
		t.Errorf("facts not FINAL: %v", err)
	}
	for name, fn := range map[string]func(*OceanFacts){
		"no movement":           func(f *OceanFacts) { f.MovementID = "" },
		"version 0":             func(f *OceanFacts) { f.Version = 0 },
		"a short digest":        func(f *OceanFacts) { f.SHA256 = strings.Repeat("a", 63) },
		"no digest":             func(f *OceanFacts) { f.SHA256 = "" },
		"no consolidation":      func(f *OceanFacts) { f.ConsolidationID = "" },
		"no legal entity":       func(f *OceanFacts) { f.LegalEntityID = "" },
		"no carrier":            func(f *OceanFacts) { f.CarrierPartyID = "" },
		"carrier revision 0":    func(f *OceanFacts) { f.CarrierPartyRevision = 0 },
		"no master bill number": func(f *OceanFacts) { f.MasterBill = "" },
		"a blank number":        func(f *OceanFacts) { f.MasterBill = "   " },
		"a number of 36":        func(f *OceanFacts) { f.MasterBill = strings.Repeat("N", 36) },
	} {
		f := carrierFacts()
		fn(&f)
		if _, err := RecordCarrierDocument("T-1", "CD-1", f, carrierEnteredMBL(), "clerk-1", carrierAt); !errors.Is(err, ErrInvalidDocument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	f := carrierFacts()
	f.MasterBill = strings.Repeat("N", 35)
	if x, err := RecordCarrierDocument("T-1", "CD-1", f, carrierEnteredMBL(), "clerk-1", carrierAt); err != nil || x.State().Number != f.MasterBill {
		t.Errorf("a number of 35: %v", err)
	}
	for name, args := range map[string][3]string{"no tenant": {"", "CD-1", "clerk"}, "no id": {"T-1", "", "clerk"}, "no person": {"T-1", "CD-1", " "}} {
		if _, err := RecordCarrierDocument(TenantID(args[0]), DocumentID(args[1]), carrierFacts(), carrierEnteredMBL(), args[2], carrierAt); !errors.Is(err, ErrInvalidDocument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := RecordCarrierDocument("T-1", "CD-1", carrierFacts(), carrierEnteredMBL(), "clerk-1", time.Time{}); !errors.Is(err, ErrInvalidDocument) {
		t.Errorf("no business time: %v", err)
	}
}

func TestACarrierDocumentsContent(t *testing.T) {
	x := record(t, carrierFacts(), carrierEnteredMBL())
	st := x.State()
	again := record(t, carrierFacts(), carrierEnteredMBL()).State()
	if string(st.Content) != string(again.Content) || st.ContentSHA256 != again.ContentSHA256 || st.ContentSHA256 != Digest(st.Content) {
		t.Fatalf("the same input gives other bytes:\n%s\n%s", st.Content, again.Content)
	}
	// the members in their order, and the values
	want := `{"kind":"MBL","document_id":"CD-1","number":"LINU2611000457","issuer_capacity":"CARRIER","carrier":{"party_id":"line-1","party_revision":5,` +
		`"role":"CARRIER"},"legal_entity_id":"LE","release_type":"ORIGINALS","originals":3,"issue_place":"CNSHA","issued_on":"2026-11-11","source":{"contract":` +
		`"OceanDocFacts v1","movement_id":"OM-1","version":2,"sha256":"` + strings.Repeat("cd", 32) + `"},"consolidation_id":"C-9","vessel":"EVER GIVEN",` +
		`"voyage":"0123W","place_of_receipt":"CNSHA","port_of_loading":"CNSHG","port_of_discharge":"DEHAM","on_board_at":"2026-11-11T08:00:00Z","evidence":` +
		`{"sha256":"` + strings.Repeat("ef", 32) + `","storage_id":"evidence/mbl-1.pdf","media_type":"application/pdf","captured_at":"2026-11-11T10:00:00Z"}}`
	if string(st.Content) != want {
		t.Fatalf("content\n%s\nwant\n%s", st.Content, want)
	}
	// originals, place_of_receipt and on_board_at are left out when empty
	f := carrierFacts()
	f.PlaceOfReceipt, f.OnBoardAt = "", time.Time{}
	e := carrierEnteredMBL()
	e.Release, e.Originals = ReleaseExpress, 0
	var members map[string]json.RawMessage
	if err := json.Unmarshal(record(t, f, e).State().Content, &members); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"originals", "place_of_receipt", "on_board_at"} {
		if _, ok := members[name]; ok {
			t.Errorf("%s is present when empty", name)
		}
	}
	for _, name := range []string{"kind", "document_id", "number", "issuer_capacity", "carrier", "legal_entity_id", "release_type", "issue_place", "issued_on", "source",
		"consolidation_id", "vessel", "voyage", "port_of_loading", "port_of_discharge", "evidence"} {
		if _, ok := members[name]; !ok {
			t.Errorf("%s is missing", name)
		}
	}
	// the on-board time is written in UTC, whatever zone SEA's facts carry it in
	f = carrierFacts()
	f.OnBoardAt = f.OnBoardAt.In(time.FixedZone("UTC+8", 8*3600))
	if got := record(t, f, carrierEnteredMBL()).State(); string(got.Content) != string(st.Content) {
		t.Errorf("an on-board time in another zone, other bytes:\n%s", got.Content)
	}
	// the evidence's capture time is written in UTC, whatever zone it was entered in
	e = carrierEnteredMBL()
	e.Evidence.CapturedAt = e.Evidence.CapturedAt.In(time.FixedZone("UTC+8", 8*3600))
	if got := record(t, carrierFacts(), e).State(); string(got.Content) != string(st.Content) {
		t.Errorf("another zone, other bytes:\n%s", got.Content)
	}
	// no name of a person or a party: every member is an identifier, a code, a number or a date
	var walk func(prefix string, raw json.RawMessage)
	walk = func(prefix string, raw json.RawMessage) {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			return
		}
		for name, member := range object {
			if strings.Contains(name, "name") && name != "storage_id" || strings.HasSuffix(name, "_by") || name == "by" {
				t.Errorf("the content has a member %s%s", prefix, name)
			}
			walk(prefix+name+".", member)
		}
	}
	walk("", st.Content)
	if strings.Contains(string(st.Content), "clerk") {
		t.Error("the content names the person who recorded it")
	}
	// the restored document is the one recorded
	back, err := RestoreCarrierDocument(st)
	if err != nil || back.State().Doc != st.Doc {
		t.Fatalf("restore: %v", err)
	}
	st.Content = append([]byte(nil), st.Content...)
	st.Content[3] = 'x'
	if _, err = RestoreCarrierDocument(st); !errors.Is(err, ErrInvalidDocument) {
		t.Errorf("content that is not the digest's: %v", err)
	}
}

func TestACarrierDocumentIsVerifiedBySecondPerson(t *testing.T) {
	x := record(t, carrierFacts(), carrierEnteredMBL())
	if x.Verified() {
		t.Fatal("verified at the record")
	}
	if err := x.Verify("clerk-1", carrierAt.Add(time.Hour)); !errors.Is(err, ErrSelfVerification) {
		t.Fatalf("the recorder verifies: %v", err)
	}
	if err := x.Verify("clerk-2", carrierAt.Add(-time.Nanosecond)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a verification before the record: %v", err)
	}
	if err := x.Verify(" ", carrierAt.Add(time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("nobody verifies: %v", err)
	}
	if x.Verified() || x.State().Revision != 1 || len(x.Pending()) != 1 {
		t.Fatalf("a refused verification changed the document: %+v", x.State())
	}
	if err := x.Verify("clerk-2", carrierAt); err != nil {
		t.Fatalf("at the time of the record: %v", err)
	}
	st := x.State()
	if !x.Verified() || st.VerifiedBy != "clerk-2" || !st.VerifiedAt.Equal(carrierAt) || st.Revision != 2 || st.State != CarrierIssued {
		t.Fatalf("verified: %+v", st)
	}
	for _, by := range []string{"clerk-2", "clerk-3", "clerk-1"} {
		if err := x.Verify(by, carrierAt.Add(time.Hour)); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("verified twice by %s: %v", by, err)
		}
	}
	if got := x.State(); got.Revision != 2 || got.VerifiedBy != "clerk-2" {
		t.Errorf("a second verification changed it: %+v", got)
	}
	var types []string
	for _, e := range x.Pending() {
		types = append(types, e.Type)
	}
	if !slices.Equal(types, []string{"CarrierDocumentRecorded.v1", "CarrierDocumentVerified.v1"}) {
		t.Errorf("events %v", types)
	}
	x.Stamp(carrierAt.Add(time.Minute))
	if got := x.State(); got.EnteredAt.IsZero() || got.VerifyEntered.IsZero() {
		t.Errorf("stamp: %+v", got)
	}
}

func TestACarrierDocumentIsCancelled(t *testing.T) {
	x := record(t, carrierFacts(), carrierEnteredMBL())
	if err := x.Cancel("", "clerk-1", carrierAt); !errors.Is(err, ErrInvalidChange) {
		t.Errorf("no reason: %v", err)
	}
	if err := x.Cancel(strings.Repeat("r", 501), "clerk-1", carrierAt); !errors.Is(err, ErrInvalidChange) {
		t.Errorf("a reason of 501: %v", err)
	}
	if err := x.Cancel("recorded against the wrong movement", "clerk-1", carrierAt.Add(-time.Nanosecond)); !errors.Is(err, ErrInvalidChange) {
		t.Errorf("before the record: %v", err)
	}
	if err := x.Cancel("recorded against the wrong movement", "", carrierAt); !errors.Is(err, ErrInvalidChange) {
		t.Errorf("nobody cancels: %v", err)
	}
	if st := x.State(); st.State != CarrierIssued || st.Revision != 1 {
		t.Fatalf("a refused cancellation changed the document: %+v", st)
	}
	if err := x.Cancel(strings.Repeat("r", 500), "clerk-1", carrierAt); err != nil {
		t.Fatalf("cancelled by its recorder, unverified: %v", err)
	}
	st := x.State()
	if st.State != CarrierCancelled || st.CancelledBy != "clerk-1" || st.Revision != 2 || st.Number != "LINU2611000457" {
		t.Fatalf("cancelled: %+v", st)
	}
	if err := x.Cancel("again", "clerk-2", carrierAt.Add(time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("cancelled twice: %v", err)
	}
	if err := x.Verify("clerk-2", carrierAt.Add(time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("a cancelled document is verified: %v", err)
	}
	if got := x.State(); got.Revision != 2 || got.VerifiedBy != "" {
		t.Errorf("a refused command changed a cancelled document: %+v", got)
	}
	// a verified document is cancelled too, not before its verification; each change adds one to the revision
	y := record(t, carrierFacts(), carrierEnteredMBL())
	if err := y.Verify("clerk-2", carrierAt.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := y.Cancel("superseded by a new bill", "clerk-3", carrierAt.Add(time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Errorf("cancelled before its verification: %v", err)
	}
	if err := y.Cancel("superseded by a new bill", "clerk-3", carrierAt.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := y.State(); st.Revision != 3 || st.VerifiedBy != "clerk-2" || st.State != CarrierCancelled {
		t.Errorf("verified then cancelled: %+v", st)
	}
	var types []string
	for _, e := range y.Pending() {
		types = append(types, e.Type)
		if e.DocumentID != "CD-1" || e.Kind != "MBL" || e.MovementID != "OM-1" || e.ConsolidationID != "C-9" || e.TenantID != "T-1" {
			t.Errorf("event %+v", e)
		}
	}
	if !slices.Equal(types, []string{"CarrierDocumentRecorded.v1", "CarrierDocumentVerified.v1", "CarrierDocumentCancelled.v1"}) {
		t.Errorf("events %v", types)
	}
	y.Acknowledge()
	if len(y.Pending()) != 0 {
		t.Error("events after the commit")
	}
}

// A person's id and a number carry no surrounding space: "clerk-1 " is not another person than "clerk-1".
func TestACarrierDocumentsIdsCarryNoSurroundingSpace(t *testing.T) {
	for _, by := range []string{" clerk-1", "clerk-1 ", "clerk-1\t"} {
		if _, err := RecordCarrierDocument("T-1", "CD-1", carrierFacts(), carrierEnteredMBL(), by, carrierAt); !errors.Is(err, ErrInvalidDocument) {
			t.Errorf("recorded by %q: %v", by, err)
		}
	}
	for _, number := range []string{" LINU2611000457", "LINU2611000457 ", "\tLINU2611000457"} {
		f := carrierFacts()
		f.MasterBill = number
		if _, err := RecordCarrierDocument("T-1", "CD-1", f, carrierEnteredMBL(), "clerk-1", carrierAt); !errors.Is(err, ErrInvalidDocument) {
			t.Errorf("number %q: %v", number, err)
		}
	}
	x := record(t, carrierFacts(), carrierEnteredMBL())
	for _, by := range []string{"clerk-1 ", " clerk-2", "clerk-2 "} {
		if err := x.Verify(by, carrierAt.Add(time.Hour)); !errors.Is(err, ErrInvalidChange) {
			t.Errorf("verified by %q: %v", by, err)
		}
		if err := x.Cancel("a reason", by, carrierAt.Add(time.Hour)); !errors.Is(err, ErrInvalidChange) {
			t.Errorf("cancelled by %q: %v", by, err)
		}
	}
	if x.Verified() || x.State().State != CarrierIssued || x.State().Revision != 1 {
		t.Fatalf("a refused command changed the document: %+v", x.State())
	}
}
