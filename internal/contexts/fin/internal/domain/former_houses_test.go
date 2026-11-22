package domain

import (
	"strings"
	"testing"
)

// The houses that left and the stuffings given up (V2.92) are read by the fingerprint and, for the reworks of a
// stuffing given up, by the confirmer rule. Nothing else reads the former houses yet.

// departed is a sealed basis that tells of a house that left the consolidation: SH-OUT, transferred to C-2 in rework 1
// of the stuffing X-1 on unit 1.
func departed() CostBasis {
	b := sealedBasis()
	b.FormerHouses = []FormerHouseFacts{{ShipmentID: "SH-OUT", End: "TRANSFERRED", TransferredTo: "C-2", Reason: "WRONG_CONSOL", EndedBy: "ops-1", EndedAt: hour(7),
		EndedEnteredAt: hour(8), ReworkFrom: "ENTRY", ReworkExecutionID: "X-1", ReworkNo: 1, ReworkUnitNo: 1, ReworkAskedBy: "ops-asker"}}
	return b
}

// givenUpAfter is the basis of the owner's F3 scenario once the stuffing is given up: rework 1, asked by ask-1, ended
// sealed, and rework 2, asked by ask-2, ended by giving the stuffing up. The unit is planned again and has no packing
// list.
func givenUpAfter() CostBasis {
	b := sealedBasis()
	b.LoadUnits[0] = LoadUnitFacts{UnitNo: 1, TypeCode: "22G1", TEUTenths: 10, Status: "PLANNED"}
	b.PackingLists = nil
	b.Houses[0].Units, b.Houses[1].Units = nil, nil
	b.GivenUpStuffings = []GivenUpStuffingFacts{{UnitNo: 1, ExecutionID: "X-1", Reworks: []ReworkFacts{
		{No: 1, AskedBy: "ask-1", End: ReworkSealed, EndedAt: hour(6)}, {No: 2, AskedBy: "ask-2", End: ReworkGivenUp, EndedAt: hour(9)}}}}
	return b
}

func TestTheFormerHousesAndTheGivenUpStuffingsArePartOfTheFingerprint(t *testing.T) {
	p := previewed(t, sealedBasis(), MethodChargeable)
	base := p.fingerprint(departed(), jobs(), nil, nil)
	if p.fingerprint(departed(), jobs(), nil, nil) != base || p.fingerprint(sealedBasis(), jobs(), nil, nil) == base {
		t.Fatal("a house that left is not part of the fingerprint")
	}
	for name, change := range map[string]func(b *CostBasis){
		"a second house that left": func(b *CostBasis) {
			b.FormerHouses = append(b.FormerHouses, FormerHouseFacts{ShipmentID: "SH-OUT-2", End: "DETACHED", EndedAt: hour(7)})
		},
		"the house":                  func(b *CostBasis) { b.FormerHouses[0].ShipmentID = "SH-OTHER" },
		"how it ended":               func(b *CostBasis) { b.FormerHouses[0].End = "DETACHED" },
		"where it went":              func(b *CostBasis) { b.FormerHouses[0].TransferredTo = "C-3" },
		"when it ended":              func(b *CostBasis) { b.FormerHouses[0].EndedAt = hour(9) },
		"the stuffing of its rework": func(b *CostBasis) { b.FormerHouses[0].ReworkExecutionID = "X-2" },
		"the number of its rework":   func(b *CostBasis) { b.FormerHouses[0].ReworkNo = 2 },
		"no rework":                  func(b *CostBasis) { b.FormerHouses[0].ReworkExecutionID, b.FormerHouses[0].ReworkNo = "", 0 },
	} {
		b := departed()
		change(&b)
		if p.fingerprint(b, jobs(), nil, nil) == base {
			t.Errorf("%s: the fingerprint did not change", name)
		}
	}
	// what is carried for the next step to read, and nothing reads yet, is not in it
	for name, change := range map[string]func(b *CostBasis){
		"the reason":                 func(b *CostBasis) { b.FormerHouses[0].Reason = "ROLLOVER" },
		"where PHY found the rework": func(b *CostBasis) { b.FormerHouses[0].ReworkFrom = "REVERSAL" },
		"who ended it":               func(b *CostBasis) { b.FormerHouses[0].EndedBy = "ops-9" },
		"when PHY recorded it":       func(b *CostBasis) { b.FormerHouses[0].EndedEnteredAt = hour(10) },
		"the unit of its rework":     func(b *CostBasis) { b.FormerHouses[0].ReworkUnitNo = 2 },
		"who asked for its rework":   func(b *CostBasis) { b.FormerHouses[0].ReworkAskedBy = "someone-else" },
	} {
		b := departed()
		change(&b)
		if p.fingerprint(b, jobs(), nil, nil) != base {
			t.Errorf("%s: the fingerprint changed", name)
		}
	}

	given := givenUpAfter()
	gbase := p.fingerprint(given, jobs(), nil, nil)
	for name, change := range map[string]func(b *CostBasis){
		"a rework of a stuffing given up": func(b *CostBasis) { b.GivenUpStuffings[0].Reworks = b.GivenUpStuffings[0].Reworks[:1] },
		"how it ended":                    func(b *CostBasis) { b.GivenUpStuffings[0].Reworks[0].End = ReworkGivenUp },
		"when it ended":                   func(b *CostBasis) { b.GivenUpStuffings[0].Reworks[1].EndedAt = hour(10) },
		"the number":                      func(b *CostBasis) { b.GivenUpStuffings[0].Reworks[1].No = 3 },
		"the stuffing":                    func(b *CostBasis) { b.GivenUpStuffings[0].ExecutionID = "X-2" },
		"the unit":                        func(b *CostBasis) { b.GivenUpStuffings[0].UnitNo = 2 },
		"a second stuffing given up": func(b *CostBasis) {
			b.GivenUpStuffings = append(b.GivenUpStuffings, GivenUpStuffingFacts{UnitNo: 2, ExecutionID: "X-3", Reworks: []ReworkFacts{{No: 1, AskedBy: "ask-3", End: ReworkGivenUp, EndedAt: hour(9)}}})
		},
	} {
		b := givenUpAfter()
		change(&b)
		if p.fingerprint(b, jobs(), nil, nil) == gbase {
			t.Errorf("%s: the fingerprint did not change", name)
		}
	}
	b := givenUpAfter()
	b.GivenUpStuffings[0].Reworks[0].AskedBy = "someone-else"
	if p.fingerprint(b, jobs(), nil, nil) != gbase {
		t.Error("the person who asked for a rework of a stuffing given up is part of the fingerprint")
	}
}

// A preview made before a house left, or before a stuffing was given up, is stale, and the refusal names the fact.
func TestAPreviewIsStaleOnceAHouseLeftOrAStuffingWasGivenUp(t *testing.T) {
	p := previewed(t, sealedBasis(), MethodChargeable)
	no := preview(t, p, sealedBasis(), "ops-1", 4)
	// only the former house differs from what the preview read: the live houses and the units are the same
	refused(t, "a house left", submit(p, no, departed(), "fin-2", 5), ErrPreviewStale, "former.\"SH-OUT\"")
	if len(p.State().Apportionments) != 0 {
		t.Fatal("a stale submission was written")
	}
	only := sealedBasis()
	only.GivenUpStuffings = givenUpAfter().GivenUpStuffings
	refused(t, "a stuffing given up", submit(p, no, only, "fin-2", 5), ErrPreviewStale, "rework.1.\"X-1\".1", "rework.1.\"X-1\".2")
	if err := submit(p, no, sealedBasis(), "fin-2", 5); err != nil {
		t.Fatalf("what the preview read: %v", err)
	}
}

// The review's F3 scenario: rework 1 asked by ask-1 ended sealed, rework 2 asked by ask-2 gave the stuffing up. A
// preview of the basis that tells of the given-up stuffing is submitted by neither of them, but by a third person.
func TestTheConfirmerOfARealGivenUpStuffingIsNeitherAskerButAThirdPerson(t *testing.T) {
	given := givenUpAfter()
	p := previewed(t, sealedBasis(), MethodChargeable)
	no := preview(t, p, given, "ops-1", 10)
	refused(t, "the asker of the sealed rework", submit(p, no, given, "ask-1", 10), ErrConfirmer,
		"ask-1 asked for rework 1 of load unit 1 (given-up stuffing X-1), which ended SEALED, for line 1", "another finance user submits")
	refused(t, "the asker of the rework given up", submit(p, no, given, "ask-2", 10), ErrConfirmer,
		"ask-2 asked for rework 2 of load unit 1 (given-up stuffing X-1), which ended GIVEN_UP, for line 1")
	if len(p.State().Apportionments) != 0 {
		t.Fatal("a refused submission was written")
	}
	if err := submit(p, no, given, "fin-3", 10); err != nil {
		t.Fatalf("a third person: %v", err)
	}
	// carried: the next apportionment of the line may be submitted by either of them
	next := preview(t, p, given, "ops-1", 11)
	if err := submit(p, next, given, "ask-1", 11); err != nil {
		t.Fatalf("carried by the submission of a third person: %v", err)
	}
}

// A rework that a submitted preview read as ended in a packing list is still carried once its stuffing is given up (the
// key is the same), and the rework that ended the stuffing is not: its asker is refused for that one only.
func TestAReworkCarriedWhileItsStuffingHadAPackingListIsCarriedAfterItIsGivenUp(t *testing.T) {
	sealed := sealedBasis()
	sealed.PackingLists[0].Reworks = []ReworkFacts{{No: 1, AskedBy: "ask-1", End: ReworkSealed, EndedAt: hour(6)}}
	p := previewed(t, sealedBasis(), MethodChargeable)
	if err := submit(p, preview(t, p, sealed, "ops-1", 8), sealed, "fin-2", 8); err != nil {
		t.Fatal(err)
	}
	given := givenUpAfter()
	no := preview(t, p, given, "ops-1", 10)
	err := submit(p, no, given, "ask-2", 10)
	refused(t, "rework 2 ended after the carrying", err, ErrConfirmer, "ask-2 asked for rework 2 of load unit 1 (given-up stuffing X-1)")
	if strings.Contains(err.Error(), "rework 1 of") {
		t.Fatalf("rework 1 was carried: %v", err)
	}
	if err = submit(p, no, given, "ask-1", 10); err != nil {
		t.Fatalf("rework 1 was carried by the submission of fin-2, and ask-1 did not ask for rework 2: %v", err)
	}
}

// A rework of a stuffing given up that is still open in the list is not an ended one: nobody is named for it. PHY ends
// a rework when it gives the stuffing up, so this is not a state PHY answers; the rule reads End as it does elsewhere.
func TestAnOpenReworkOfAStuffingGivenUpNamesNobody(t *testing.T) {
	b := givenUpAfter()
	b.GivenUpStuffings[0].Reworks = b.GivenUpStuffings[0].Reworks[1:]
	b.GivenUpStuffings[0].Reworks[0].End = ""
	if asked := previewed(t, sealedBasis(), MethodChargeable).reworksAskedBy(b, "ask-2", []LineApportionment{{LineNo: 1}}); asked != nil {
		t.Fatalf("an open rework was named: %v", asked)
	}
}
