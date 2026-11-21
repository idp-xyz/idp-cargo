package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// V2.90: a house's bill is dated by the loading of its own containers and issued only when every one of them is loaded.

func day(n int) time.Time { return t0.Add(time.Duration(n) * 24 * time.Hour) }

// twoBoxes is SEA's facts of a movement with two containers: V1 (CSQU3054383, loaded on day 1) and V2 (TEMU1234565,
// loaded on the day given, or not loaded when it is zero). SH-A is in V1, SH-B in V1 and V2, SH-L in V2.
func twoBoxes(version int, second time.Time) OceanFacts {
	f := oceanFacts(version)
	f.ContainerDates, f.OnBoardAt = true, day(1)
	f.Containers = []OceanContainer{{VisitID: "V1", Number: "CSQU3054383", SizeType: "22G1", SealNumber: "S1", OnBoardAt: day(1)},
		{VisitID: "V2", Number: "TEMU1234565", SizeType: "22G1", SealNumber: "S2", OnBoardAt: second}}
	in := func(visit, number string) OceanHouseContainer {
		return OceanHouseContainer{VisitID: visit, Number: number, Packs: 3, GrossGrams: 150_000, VolumeCm3: 500_000}
	}
	house := func(shipment, hbl string, boxes ...OceanHouseContainer) OceanHouse {
		h := f.Houses[0]
		h.ShipmentID, h.HBL, h.Containers = shipment, hbl, boxes
		return h
	}
	f.Houses = []OceanHouse{house("SH-A", "SHAL0000002", in("V1", "CSQU3054383")), house("SH-B", "SHAL0000003", in("V1", "CSQU3054383"), in("V2", "TEMU1234565")),
		house("SH-L", "SHAL0000001", in("V2", "TEMU1234565"))}
	return f
}

func TestAHousesOnBoardDateIsTheLatestOfItsContainers(t *testing.T) {
	later := twoBoxes(1, day(3))
	notYet := twoBoxes(1, time.Time{})
	old := twoBoxes(1, day(3))
	old.ContainerDates = false
	for i := range old.Containers {
		old.Containers[i].OnBoardAt = time.Time{}
	}
	coload := oceanFacts(1) // houses name no container, as a co-load's do
	coload.ContainerDates = true
	anonymous := twoBoxes(1, time.Time{}) // the carrier reported a loading naming no container: the movement's date, no container's
	anonymous.Containers[0].OnBoardAt = time.Time{}
	for name, tc := range map[string]struct {
		facts    OceanFacts
		shipment string
		at       time.Time
		missing  string
	}{
		"one container":                      {later, "SH-A", day(1), ""},
		"two containers loaded on two days":  {later, "SH-B", day(3), ""},
		"the other container alone":          {later, "SH-L", day(3), ""},
		"two containers loaded on one day":   {twoBoxes(1, day(1)), "SH-B", day(1), ""},
		"a container not loaded":             {notYet, "SH-L", time.Time{}, "TEMU1234565"},
		"one of two containers not loaded":   {notYet, "SH-B", time.Time{}, "TEMU1234565"},
		"the loaded container's house":       {notYet, "SH-A", day(1), ""},
		"old facts are the movement's date":  {old, "SH-L", day(1), ""},
		"a co-load is the movement's date":   {coload, "SH-L", t0, ""},
		"a report naming no container":       {anonymous, "SH-A", time.Time{}, "CSQU3054383"},
		"a report naming no container, both": {anonymous, "SH-B", time.Time{}, "CSQU3054383, TEMU1234565"},
	} {
		h, _ := tc.facts.house(tc.shipment)
		at, missing := tc.facts.HouseOnBoard(h)
		if !at.Equal(tc.at) || strings.Join(missing, ", ") != tc.missing {
			t.Errorf("%s: %s, not loaded %v; want %s %q", name, at, missing, tc.at, tc.missing)
		}
	}
	c, err := OceanHouseContent(KindHBL, "DOC-B", "SH-B", later, issuer, seaOrders(party("consignee", "DE", 1), nil), entered)
	if err != nil || !c.OnBoardAt.Equal(day(3)) || len(c.Containers) != 2 {
		t.Fatalf("the bill of SH-B is dated by its later container: %v %s", err, c.OnBoardAt)
	}
	if c, err = OceanHouseContent(KindHBL, "DOC-A", "SH-A", later, issuer, seaOrders(party("consignee", "DE", 1), nil), entered); err != nil || !c.OnBoardAt.Equal(day(1)) {
		t.Fatalf("the bill of SH-A: %v %s", err, c.OnBoardAt)
	}
	// a draft is allowed without the date
	if c, err = OceanHouseContent(KindHBL, "DOC-L", "SH-L", notYet, issuer, seaOrders(party("consignee", "DE", 1), nil), entered); err != nil || !c.OnBoardAt.IsZero() {
		t.Fatalf("a draft of a house in a container not loaded: %v %s", err, c.OnBoardAt)
	}
	// every house of a movement whose containers all load together is dated as the movement is
	together := twoBoxes(1, day(1))
	for _, h := range together.Houses {
		if at, _ := together.HouseOnBoard(h); !at.Equal(together.OnBoardAt) {
			t.Errorf("%s: %s, the movement's is %s", h.ShipmentID, at, together.OnBoardAt)
		}
	}
}

// draftedFrom drafts and approves the bill of SH-L from facts, orders locked for it.
func draftedFrom(t *testing.T, f OceanFacts) *Document {
	t.Helper()
	c, err := OceanHouseContent(KindHBL, "DOC-L", "SH-L", f, issuer, seaOrders(party("consignee", "DE", 1), nil, "DOC-L"), entered)
	if err != nil {
		t.Fatal(err)
	}
	d, err := DraftDocument("T", "DOC-L", c, "doc-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Approve("doc-2", t0); err != nil {
		t.Fatal(err)
	}
	return d
}

func gateOf(f OceanFacts) IssueGate {
	return IssueGate{Ocean: f, Orders: seaOrders(party("consignee", "DE", 1), nil, "DOC-L"), OverrideReason: "the account is opened by hand", IssuerVerified: true,
		AuthorityID: "A", AuthorityVersion: 1}
}

func TestAHouseBillIsIssuedOnlyWhenEveryContainerIsLoaded(t *testing.T) {
	notYet := twoBoxes(1, time.Time{})
	d := draftedFrom(t, notYet)
	err := d.Issue(gateOf(notYet), "doc-1", t0)
	if !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "TEMU1234565") || !strings.Contains(err.Error(), "wait for its loading") ||
		!strings.Contains(err.Error(), "refresh") || d.State().State != StateApproved {
		t.Fatalf("issued with a container not loaded: %v %s", err, d.State().State)
	}
	// SEA finalizes again with the container loaded on day 3: refresh, approve, issue
	loaded := twoBoxes(2, day(3))
	c, err := OceanHouseContent(KindHBL, "DOC-L", "SH-L", loaded, issuer, seaOrders(party("consignee", "DE", 1), nil, "DOC-L"), entered)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(d.Refresh(c, "the container is loaded", "doc-1", t0), d.Approve("doc-2", t0), d.Issue(gateOf(loaded), "doc-1", t0)); err != nil {
		t.Fatal(err)
	}
	if cur, _ := d.Current(); d.State().State != StateIssued || !cur.Doc.OnBoardAt.Equal(day(3)) {
		t.Fatalf("issued: %s %s", d.State().State, cur.Doc.OnBoardAt)
	}
	// an amendment is gated the same way: built while another container is not loaded, it is not issued
	again := twoBoxes(3, time.Time{})
	c, err = OceanHouseContent(KindHBL, "DOC-L", "SH-L", again, issuer, seaOrders(party("consignee", "DE", 1), nil, "DOC-L"), entered)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(d.OpenAmendment(c, "SEA changed the container", "doc-1", t0.Add(time.Hour)), d.Approve("doc-2", t0.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err = d.IssueAmendment(gateOf(again), "doc-1", t0.Add(time.Hour)); !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "TEMU1234565") || d.State().State != StateAmending {
		t.Fatalf("an amendment with a container not loaded: %v %s", err, d.State().State)
	}
	// facts that were not final or are other than the draft's are refused for that first
	if err = d.IssueAmendment(gateOf(loaded), "doc-1", t0.Add(time.Hour)); !errors.Is(err, ErrSourceStale) {
		t.Fatalf("other facts: %v", err)
	}
	// facts published before V2.90 date by the movement: the same bill is issued, as it was
	old := twoBoxes(1, time.Time{})
	old.ContainerDates = false
	d = draftedFrom(t, old)
	if err = d.Issue(gateOf(old), "doc-1", t0); err != nil {
		t.Fatalf("old facts: %v", err)
	}
	if cur, _ := d.Current(); !cur.Doc.OnBoardAt.Equal(day(1)) {
		t.Fatalf("old facts date the bill by the movement: %s", cur.Doc.OnBoardAt)
	}
	// and with no date at all, as before
	none := oceanFacts(1)
	none.OnBoardAt = time.Time{}
	d = draftedFrom(t, none)
	if err = d.Issue(gateOf(none), "doc-1", t0); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("no date: %v", err)
	}
}

func pending(t *testing.T, d *Document, f OceanFacts) map[string]PendingItem {
	t.Helper()
	out := map[string]PendingItem{}
	for _, it := range d.PendingItems(f) {
		if _, dup := out[it.Kind]; dup || it.DocumentID != "DOC-L" || it.ShipmentID != "SH-L" || it.Number != "SHAL0000001" || it.Message == "" {
			t.Fatalf("a pending item: %+v", it)
		}
		out[it.Kind] = it
	}
	return out
}

func wantKinds(t *testing.T, label string, got map[string]PendingItem, blocking map[string]bool) {
	t.Helper()
	if len(got) != len(blocking) {
		t.Fatalf("%s: %+v, want %v", label, got, blocking)
	}
	for kind, block := range blocking {
		if it, ok := got[kind]; !ok || it.Blocking != block {
			t.Fatalf("%s: %s: %+v, want blocking %v", label, kind, it, block)
		}
	}
}

func TestPendingItemsOfAHouseBill(t *testing.T) {
	notYet := twoBoxes(1, time.Time{})
	d := draftedFrom(t, notYet)
	// built on version 1 with a container not loaded
	it := pending(t, d, notYet)
	wantKinds(t, "not all loaded", it, map[string]bool{PendingHouseNotAllLoaded: true})
	if m := it[PendingHouseNotAllLoaded].Message; !strings.Contains(m, "TEMU1234565") || !strings.Contains(m, "records its loading") || !strings.Contains(m, "refreshed") {
		t.Fatalf("the message says who does what: %s", m)
	}
	// SEA's latest final version is 2, the container loaded: the draft was built on version 1
	loaded := twoBoxes(2, day(3))
	wantKinds(t, "superseded", pending(t, d, loaded), map[string]bool{PendingSourceSuperseded: true})
	// version 2 again, the container not loaded, a different version: both
	other := twoBoxes(2, time.Time{})
	wantKinds(t, "superseded and not loaded", pending(t, d, other), map[string]bool{PendingSourceSuperseded: true, PendingHouseNotAllLoaded: true})
	// the same version number with other content is other facts too
	sameVersion := notYet
	sameVersion.SHA256 = strings.Repeat("ef", 32)
	wantKinds(t, "superseded by content", pending(t, d, sameVersion), map[string]bool{PendingSourceSuperseded: true, PendingHouseNotAllLoaded: true})
	// the movement is open: wait
	open := notYet
	open.Final = false
	wantKinds(t, "not final", pending(t, d, open), map[string]bool{PendingSourceNotFinal: true})
	// the house is not in SEA's latest facts
	gone := loaded
	gone.Houses = gone.Houses[:2]
	wantKinds(t, "left", pending(t, d, gone), map[string]bool{PendingHouseLeft: true})
	gone.Final = false
	wantKinds(t, "left, not final", pending(t, d, gone), map[string]bool{PendingHouseLeft: true})
	// built on the facts SEA has, with the container loaded: nothing
	d = draftedFrom(t, loaded)
	wantKinds(t, "nothing", pending(t, d, loaded), map[string]bool{})

	// an issued bill has the same items, none of them blocking, and is read by its valid version
	if err := d.Issue(gateOf(loaded), "doc-1", t0); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "issued, nothing", pending(t, d, loaded), map[string]bool{})
	// SEA finalizing again is no reason to touch an issued bill whose house did not change
	wantKinds(t, "issued, finalized again, unchanged", pending(t, d, twoBoxes(3, day(3))), map[string]bool{})
	// what changed for the house is named
	changed := func(label, want string, edit func(*OceanFacts)) {
		t.Helper()
		f := twoBoxes(3, day(3))
		edit(&f)
		got := pending(t, d, f)
		wantKinds(t, label, got, map[string]bool{PendingIssuedChanged: false})
		if m := got[PendingIssuedChanged].Message; !strings.Contains(m, want) || !strings.Contains(m, "amendment") || !strings.Contains(m, "version 3") {
			t.Fatalf("%s: %s", label, m)
		}
	}
	changed("a later date", "the on-board date", func(f *OceanFacts) { f.Containers[1].OnBoardAt = day(5) })
	changed("an earlier date", "the on-board date", func(f *OceanFacts) { f.Containers[1].OnBoardAt = day(2) })
	changed("a seal", "the seal", func(f *OceanFacts) { f.Containers[1].SealNumber = "S9" })
	changed("the packs", "the packs, weights or volume", func(f *OceanFacts) { f.Houses[2].Containers[0].Packs = 4 })
	changed("the house's weight", "the packs, weights or volume", func(f *OceanFacts) { f.Houses[2].GrossGrams++ })
	changed("another container", "the containers", func(f *OceanFacts) { f.Houses[2].Containers[0].Number = "MSKU1234565" })
	changed("the voyage", "the voyage, ports or master bill", func(f *OceanFacts) { f.Voyage = "0999E" })
	changed("a size type", "the containers", func(f *OceanFacts) { f.Containers[1].SizeType = "42G1" })
	changed("a container more", "the containers", func(f *OceanFacts) {
		f.Houses[2].Containers = append(f.Houses[2].Containers, f.Houses[2].Containers[0])
	})
	changed("a container's volume", "the packs, weights or volume", func(f *OceanFacts) { f.Houses[2].Containers[0].VolumeCm3++ })
	changed("the vessel", "the voyage, ports or master bill", func(f *OceanFacts) { f.Vessel = "OTHER VESSEL" })
	changed("the master bill", "the voyage, ports or master bill", func(f *OceanFacts) { f.MasterBill = "LINU2611009999" })
	changed("the port of discharge", "the voyage, ports or master bill", func(f *OceanFacts) { f.PortOfDischarge = "NLRTM" })
	changed("the place of receipt", "the voyage, ports or master bill", func(f *OceanFacts) { f.PlaceOfReceipt = "CNNGB" })
	changed("the location edition", "the voyage, ports or master bill", func(f *OceanFacts) { f.Edition = "E2" })
	changed("the goods description", "the goods description", func(f *OceanFacts) { f.Houses[2].Goods = "Cartons of machinery" })
	changed("the package type", "the package type", func(f *OceanFacts) { f.Houses[2].Package = "PK" })
	changed("the consolidation", "the movement or consolidation", func(f *OceanFacts) { f.ConsolidationID = "C-77" })
	changed("the movement", "the movement or consolidation", func(f *OceanFacts) { f.MovementID = "OM-77" })
	changed("a container not loaded any more", "the on-board date", func(f *OceanFacts) { f.Containers[1].OnBoardAt = time.Time{} })
	gone.Final = true
	left := pending(t, d, gone)
	wantKinds(t, "issued, left", left, map[string]bool{PendingHouseLeft: false})
	if m := left[PendingHouseLeft].Message; !strings.Contains(m, "stays valid") || !strings.Contains(m, "amendment") || strings.Contains(m, "refreshes") {
		t.Fatalf("an issued bill is not refreshed: %s", m)
	}
	wantKinds(t, "issued, not final", pending(t, d, open), map[string]bool{PendingSourceNotFinal: false})
	// an amendment in progress is read by its draft, and blocks again
	c, err := OceanHouseContent(KindHBL, "DOC-L", "SH-L", twoBoxes(3, day(3)), issuer, seaOrders(party("consignee", "DE", 1), nil, "DOC-L"), entered)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.OpenAmendment(c, "SEA changed", "doc-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "amending, superseded", pending(t, d, twoBoxes(4, day(3))), map[string]bool{PendingSourceSuperseded: true})
	wantKinds(t, "amending, current", pending(t, d, twoBoxes(3, day(3))), map[string]bool{})
	// a cancelled document, a surrendered one and an air waybill have none
	if err = d.Cancel("withdrawn", "doc-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := d.PendingItems(open); len(got) != 0 {
		t.Fatalf("cancelled: %+v", got)
	}
	if _, ok := d.PendingMovement(); ok {
		t.Fatal("a cancelled document names no movement to read")
	}
	air, err := DraftDocument("T", "DOC-H1", hawb(t, 1, orders()), "doc-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := air.PendingMovement(); ok || len(air.PendingItems(open)) != 0 {
		t.Fatal("an air waybill has no sea pending items")
	}
}

// A bill moved to another movement by an amendment, then abandoned, is on the valid version's movement again.
func TestAbandoningAnAmendmentRestoresTheMovement(t *testing.T) {
	loaded := twoBoxes(1, day(3))
	d := draftedFrom(t, loaded)
	if err := d.Issue(gateOf(loaded), "doc-1", t0); err != nil {
		t.Fatal(err)
	}
	moved := twoBoxes(1, day(3))
	moved.MovementID, moved.ConsolidationID = "OM-2", "C-2"
	c, err := OceanHouseContent(KindHBL, "DOC-L", "SH-L", moved, issuer, seaOrders(party("consignee", "DE", 1), nil, "DOC-L"), entered)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.OpenAmendment(c, "the house moved", "doc-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := d.State(); st.MovementID != "OM-2" || st.ConsolidationID != "C-2" {
		t.Fatalf("the draft moved the row: %+v", st)
	}
	if err = d.AbandonAmendment(seaOrders(party("consignee", "DE", 1), nil, "DOC-L"), "the move was a mistake", "doc-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := d.State(); st.MovementID != "OM-1" || st.ConsolidationID != "C-9" || st.State != StateIssued {
		t.Fatalf("abandoned: %+v", st)
	}
	if movement, _ := d.PendingMovement(); movement != "OM-1" {
		t.Fatalf("read by the valid version: %s", movement)
	}
	// a row written before V2.90 names the discarded draft's movement and still restores; one that names neither does
	// not
	st := d.State()
	if _, err = RestoreDocument(st); err != nil {
		t.Fatalf("restored: %v", err)
	}
	st.MovementID, st.ConsolidationID = "OM-2", "C-2"
	if _, err = RestoreDocument(st); err != nil {
		t.Fatalf("a row written before the fix: %v", err)
	}
	st.MovementID = "NO_SUCH_MOVEMENT"
	if _, err = RestoreDocument(st); err == nil {
		t.Fatal("a row that names no version's movement restored")
	}
}

// When the facts state no on-board date at all (a co-load with no carrier loading report, or facts of an earlier
// version whose date is none) there is no container to name: issue is refused and the pending list says so, both with
// the way out.
func TestFactsThatStateNoLoadingAtAllAreRefusedWithTheWayOut(t *testing.T) {
	coload := oceanFacts(1)
	coload.ContainerDates, coload.OnBoardAt = true, time.Time{}
	old := twoBoxes(1, time.Time{})
	old.ContainerDates, old.OnBoardAt = false, time.Time{}
	for name, f := range map[string]OceanFacts{"a co-load with no report": coload, "old facts with no date": old} {
		d := draftedFrom(t, f)
		err := d.Issue(gateOf(f), "doc-1", t0)
		for _, want := range []string{"state no loading", "record the loading in SEA", "carrier's report", "container's event", "finalize"} {
			if !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: issue: %v, want it to say %q", name, err, want)
			}
		}
		got := pending(t, d, f)
		wantKinds(t, name, got, map[string]bool{PendingHouseNotAllLoaded: true})
		for _, want := range []string{"no loading", "carrier's report", "container's event", "finalizes"} {
			if m := got[PendingHouseNotAllLoaded].Message; !strings.Contains(m, want) {
				t.Fatalf("%s: %s, want it to say %q", name, m, want)
			}
		}
		// once the facts state the date the item is gone
		f.OnBoardAt = day(1)
		wantKinds(t, name+", dated", pending(t, d, f), map[string]bool{})
	}
}

// A container the facts do not list, a container named twice and a breakdown without a number (the survivors of the
// review).
func TestAHouseNotLoadedNamesItsContainersOnce(t *testing.T) {
	f := twoBoxes(1, day(3))
	h, _ := f.house("SH-B")
	h.Containers = []OceanHouseContainer{{VisitID: "V9", Number: "MSKU1234565", Packs: 1}, {VisitID: "V9", Number: "MSKU1234565", Packs: 1},
		{VisitID: "V3", Packs: 1}}
	f.Containers = append(f.Containers, OceanContainer{VisitID: "V3", Number: "HLCU2345674"}) // listed, with no date
	at, missing := f.HouseOnBoard(h)
	if !at.IsZero() || strings.Join(missing, ",") != "MSKU1234565,HLCU2345674" {
		t.Fatalf("an unlisted container, once, and a number taken from the facts: %s %v", at, missing)
	}
	f.Containers[1].OnBoardAt = time.Time{}
	h, _ = f.house("SH-L")
	if _, missing = f.HouseOnBoard(h); strings.Join(missing, ",") != "TEMU1234565" {
		t.Fatalf("the number is the breakdown's: %v", missing)
	}
}

// The restore check accepts the row naming the latest version's movement or the latest valid one's, and no other
// version's: a movement of an earlier discarded draft is refused.
func TestARowNamingAnEarlierDiscardedDraftsMovementDoesNotRestore(t *testing.T) {
	loaded := twoBoxes(1, day(3))
	d := draftedFrom(t, loaded)
	if err := d.Issue(gateOf(loaded), "doc-1", t0); err != nil {
		t.Fatal(err)
	}
	orders := seaOrders(party("consignee", "DE", 1), nil, "DOC-L")
	for i, movement := range []string{"OM-2", "OM-3"} {
		moved := twoBoxes(1, day(3))
		moved.MovementID, moved.ConsolidationID = movement, "C-"+movement[3:]
		c, err := OceanHouseContent(KindHBL, "DOC-L", "SH-L", moved, issuer, orders, entered)
		if err != nil {
			t.Fatal(err)
		}
		at := t0.Add(time.Duration(2*i+1) * time.Hour)
		if err = errors.Join(d.OpenAmendment(c, "moved", "doc-1", at), d.AbandonAmendment(orders, "no", "doc-1", at.Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	st := d.State()
	if _, err := RestoreDocument(st); err != nil {
		t.Fatalf("restored: %v", err)
	}
	st.MovementID, st.ConsolidationID = "OM-3", "C-3" // the latest version, a discarded draft: written before the fix
	if _, err := RestoreDocument(st); err != nil {
		t.Fatalf("the latest version's movement: %v", err)
	}
	st.MovementID, st.ConsolidationID = "OM-2", "C-2" // an earlier discarded draft's
	if _, err := RestoreDocument(st); err == nil {
		t.Fatal("a row naming an earlier discarded draft's movement restored")
	}
}
