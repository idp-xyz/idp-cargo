package domain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var plate = Plate{TareKg: 2200, MaxGrossKg: 30480, Source: PlateCSC}

func TestContainerNumbers(t *testing.T) {
	// MSKU0000080 has the remainder 10, MSKU0000140 the remainder 0: both end in 0.
	for _, number := range []string{"CSQU3054383", "TEMU1234565", "HLXU1234561", "MSKU0000080", "MSKU0000140"} {
		if !ValidContainerNumber(number) {
			t.Errorf("%s is a container number", number)
		}
	}
	for _, number := range []string{"CSQU3054384", "MSKU0000081", "CSQU305438", "CSQU30543830", "csqu3054383", "CSQ13054383", "CSQA3054383", "CSQU305438X",
		" CSQU3054383", ""} {
		if ValidContainerNumber(number) {
			t.Errorf("%q is taken as a container number", number)
		}
	}
}

// stuffedMovement is an open own-container movement whose house SH-1 carries PHY's sealed stuffing X-1 of unit 1,
// a 22G1 sealed 20 hours after the movement was opened.
func stuffedMovement(t *testing.T, seal string) *Movement {
	t.Helper()
	m := open(t, booking(1))
	if err := m.Associate(stuffed(plan("SH-1", "C-1", 1), 1, seal), "H1", "ops", at); err != nil {
		t.Fatal(err)
	}
	return m
}

func assigned(t *testing.T, m *Movement) *Visit {
	t.Helper()
	v, err := AssignContainer("CV-1", m, 1, "CSQU3054383", "22G1", plate, "ops", at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func sealed(t *testing.T, v *Visit, number string, hours int) {
	t.Helper()
	when := at.Add(time.Duration(hours) * time.Hour)
	if err := v.RecordSeal(number, SealShipper, when, "tally sheet 7", "ops", when); err != nil {
		t.Fatal(err)
	}
}

func TestAssignContainer(t *testing.T) {
	m := stuffedMovement(t, "CN-SEAL-1")
	// the movement's master bill is recorded two days in: a container may still be assigned before that
	if err := m.RecordMasterBill("MAEU1", BillCarrierMBL, sha, "ops", at.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	v := assigned(t, m)
	st := v.State()
	if st.Status != VisitAssigned || st.MovementID != "OM-1" || st.ConsolidationID != "C-1" || st.LoadUnitNo != 1 || st.Number != "CSQU3054383" ||
		st.SizeType != "22G1" || st.Plate != plate || !st.AssignedAt.Equal(at.Add(time.Hour)) || !v.Live() || v.Seal().No != 0 {
		t.Fatalf("assigned: %+v", st)
	}
	if e := v.Pending(); len(e) != 1 || e[0].Type != eventContainerAssigned || e[0].SubjectID != "CV-1" || e[0].MovementID != "OM-1" ||
		e[0].ConsolidationID != "C-1" || e[0].TenantID != "T" {
		t.Fatalf("event: %+v", e)
	}
	if _, err := AssignContainer("CV-2", m, 2, "TEMU1234565", "42G1", Plate{}, "ops", at); err != nil {
		t.Fatalf("without plate data, at the time the movement was opened: %v", err)
	}
	cancelled := open(t, booking(1))
	if err := cancelled.Cancel("no cargo", "ops", at); err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Hour)
	for name, tc := range map[string]struct {
		assign func() (*Visit, error)
		want   error
	}{
		"a cancelled movement": {func() (*Visit, error) {
			return AssignContainer("CV-9", cancelled, 1, "CSQU3054383", "22G1", plate, "ops", later)
		}, ErrInvalidTransition},
		"a co-load": {func() (*Visit, error) {
			return AssignContainer("CV-9", open(t, coload()), 1, "CSQU3054383", "22G1", plate, "ops", later)
		}, ErrInvalidVisit},
		"no movement": {func() (*Visit, error) {
			return AssignContainer("CV-9", nil, 1, "CSQU3054383", "22G1", plate, "ops", later)
		}, ErrInvalidVisit},
		"a wrong check digit": {func() (*Visit, error) {
			return AssignContainer("CV-9", m, 1, "CSQU3054384", "22G1", plate, "ops", later)
		}, ErrInvalidContainer},
		"unit 0": {func() (*Visit, error) {
			return AssignContainer("CV-9", m, 0, "CSQU3054383", "22G1", plate, "ops", later)
		}, ErrInvalidVisit},
		"a size-type in small letters": {func() (*Visit, error) {
			return AssignContainer("CV-9", m, 1, "CSQU3054383", "22g1", plate, "ops", later)
		},
			ErrInvalidVisit},
		"no size-type": {func() (*Visit, error) { return AssignContainer("CV-9", m, 1, "CSQU3054383", "", plate, "ops", later) }, ErrInvalidVisit},
		"a tare without a maximum gross": {func() (*Visit, error) {
			return AssignContainer("CV-9", m, 1, "CSQU3054383", "22G1", Plate{TareKg: 2200, Source: PlateCSC}, "ops", later)
		}, ErrInvalidVisit},
		"a maximum gross below the tare": {func() (*Visit, error) {
			return AssignContainer("CV-9", m, 1, "CSQU3054383", "22G1", Plate{TareKg: 2200, MaxGrossKg: 2200, Source: PlateCSC}, "ops", later)
		}, ErrInvalidVisit},
		"plate data without a source": {func() (*Visit, error) {
			return AssignContainer("CV-9", m, 1, "CSQU3054383", "22G1", Plate{TareKg: 2200, MaxGrossKg: 30480}, "ops", later)
		}, ErrInvalidVisit},
		"plate data from elsewhere": {func() (*Visit, error) {
			return AssignContainer("CV-9", m, 1, "CSQU3054383", "22G1", Plate{TareKg: 2200, MaxGrossKg: 30480, Source: "DEPOT"}, "ops", later)
		}, ErrInvalidVisit},
		"no visit id": {func() (*Visit, error) { return AssignContainer(" ", m, 1, "CSQU3054383", "22G1", plate, "ops", later) }, ErrInvalidVisit},
		"no actor":    {func() (*Visit, error) { return AssignContainer("CV-9", m, 1, "CSQU3054383", "22G1", plate, " ", later) }, ErrInvalidVisit},
		"no time": {func() (*Visit, error) {
			return AssignContainer("CV-9", m, 1, "CSQU3054383", "22G1", plate, "ops", time.Time{})
		}, ErrInvalidVisit},
		"before the movement was opened": {func() (*Visit, error) {
			return AssignContainer("CV-9", m, 1, "CSQU3054383", "22G1", plate, "ops", at.Add(-time.Second))
		}, ErrInvalidChange},
	} {
		if v, err := tc.assign(); !errors.Is(err, tc.want) || v != nil {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}

func TestContainerSeals(t *testing.T) {
	v := assigned(t, stuffedMovement(t, "CN-SEAL-1"))
	when := at.Add(2 * time.Hour)
	for name, tc := range map[string]struct {
		number, party, source, by string
		applied, at               time.Time
		want                      error
	}{
		"no number":                 {"", SealShipper, "tally", "ops", when, when, ErrInvalidSeal},
		"small letters":             {"seal1", SealShipper, "tally", "ops", when, when, ErrInvalidSeal},
		"two hyphens":               {"SEAL--1", SealShipper, "tally", "ops", when, when, ErrInvalidSeal},
		"a space before":            {" SEAL1", SealShipper, "tally", "ops", when, when, ErrInvalidSeal},
		"other signs":               {"SEAL#1", SealShipper, "tally", "ops", when, when, ErrInvalidSeal},
		"36 characters":             {strings.Repeat("A", 36), SealShipper, "tally", "ops", when, when, ErrInvalidSeal},
		"the forwarder as party":    {"SEAL1", "FORWARDER", "tally", "ops", when, when, ErrInvalidSeal},
		"no party":                  {"SEAL1", "", "tally", "ops", when, when, ErrInvalidSeal},
		"applied after it is dated": {"SEAL1", SealShipper, "tally", "ops", when.Add(time.Second), when, ErrInvalidSeal},
		"not applied":               {"SEAL1", SealShipper, "tally", "ops", time.Time{}, when, ErrInvalidSeal},
		"no source":                 {"SEAL1", SealShipper, " ", "ops", when, when, ErrInvalidSeal},
		"no actor":                  {"SEAL1", SealShipper, "tally", "", when, when, ErrInvalidSeal},
		"before the assignment":     {"SEAL1", SealShipper, "tally", "ops", at, at, ErrInvalidChange},
	} {
		if err := v.RecordSeal(tc.number, tc.party, tc.applied, tc.source, tc.by, tc.at); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if len(v.State().Seals) != 0 {
		t.Fatalf("a refused seal is kept: %+v", v.State().Seals)
	}
	// 35 characters with a space, a hyphen and a slash inside; applied an hour before it is recorded
	long := "AB 12-CD/" + strings.Repeat("9", 26)
	if err := v.RecordSeal(long, SealCarrier, when.Add(-time.Hour), "release order", "ops", when); err != nil {
		t.Fatal(err)
	}
	if err := v.RecordSeal(long, SealShipper, when, "tally", "ops", when); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("the current seal again: %v", err)
	}
	if err := v.RecordSeal("SEAL2", SealShipper, when.Add(-time.Hour), "tally", "ops", when.Add(-time.Minute)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a seal dated before the one it replaces: %v", err)
	}
	if err := v.RecordSeal("SEAL2", SealCustoms, when, "customs notice 4", "officer", when); err != nil {
		t.Fatal(err)
	}
	// the first number may come back after another
	if err := v.RecordSeal(long, SealShipper, when, "tally", "ops", when); err != nil {
		t.Fatalf("an earlier number after another: %v", err)
	}
	st := v.State()
	if len(st.Seals) != 3 || v.Seal().No != 3 || v.Seal().Number != long || st.Seals[1].Party != SealCustoms || st.Seals[1].By != "officer" ||
		!st.Seals[0].AppliedAt.Equal(when.Add(-time.Hour)) || len(v.AddedSeals()) != 3 || st.Status != VisitAssigned {
		t.Fatalf("seals: %+v", st.Seals)
	}
	if e := v.Pending(); len(e) != 4 || e[1].Type != eventSealRecorded || e[1].SealNo != 1 || e[3].SealNo != 3 || e[3].MovementID != "OM-1" || e[3].Reason != "" {
		t.Fatalf("events: %+v", e)
	}
	v.Stamp(when.Add(time.Minute))
	v.Acknowledge()
	if len(v.AddedSeals()) != 0 || v.Pending() != nil || !v.State().Seals[2].EnteredAt.Equal(when.Add(time.Minute)) || !v.State().EnteredAt.Equal(when.Add(time.Minute)) {
		t.Fatalf("acknowledged: %+v", v.State())
	}
	if err := v.Release("the carrier took it back", "ops", when); err != nil {
		t.Fatal(err)
	}
	if err := v.RecordSeal("SEAL4", SealShipper, when, "tally", "ops", when); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a seal on a released container: %v", err)
	}
}

func TestStuffingDone(t *testing.T) {
	sealedAt := at.Add(20 * time.Hour) // when PHY sealed X-1
	done := at.Add(21 * time.Hour)
	m := stuffedMovement(t, "CN-SEAL-1")

	v := assigned(t, m)
	if err := v.StuffingDone(m, "", "ops", done); !errors.Is(err, ErrNotStuffed) || !strings.Contains(err.Error(), "no seal") {
		t.Fatalf("without a seal: %v", err)
	}
	sealed(t, v, "CN SEAL/1", 2) // PHY's seal keyed with other separators: the same seal
	other := open(t, booking(1))
	other.state.MovementID = "OM-2"
	for name, tc := range map[string]struct {
		m      *Movement
		reason string
		at     time.Time
		want   error
	}{
		"another movement":                {other, "", done, ErrInvalidVisit},
		"no movement":                     {nil, "", done, ErrInvalidVisit},
		"a reason without a difference":   {m, "to be safe", done, ErrInvalidChange},
		"before PHY sealed the container": {m, "", sealedAt.Add(-time.Microsecond), ErrInvalidChange},
		"before the visit's last change":  {m, "", at.Add(time.Hour), ErrInvalidChange},
	} {
		if err := v.StuffingDone(tc.m, tc.reason, "ops", tc.at); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if err := v.StuffingDone(m, "", " ", done); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("no actor: %v", err)
	}
	if err := v.StuffingDone(m, "", "cfs", sealedAt); err != nil {
		t.Fatalf("at the time PHY sealed: %v", err)
	}
	st := v.State()
	if st.Status != VisitStuffed || st.ExecutionID != "X-1" || st.DifferenceReason != "" || st.StuffedBy != "cfs" || !st.StuffedAt.Equal(sealedAt) || !v.Live() {
		t.Fatalf("stuffed: %+v", st)
	}
	if e := v.Pending(); len(e) != 3 || e[2].Type != eventContainerEvent || e[2].Reason != ContainerStuffedEvent || e[2].MovementID != "OM-1" || e[2].SealNo != 0 {
		t.Fatalf("events: %+v", e)
	}
	if err := v.StuffingDone(m, "", "ops", done); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("twice: %v", err)
	}
	if err := v.Release("sent back", "ops", done); !errors.Is(err, ErrCargoInContainer) {
		t.Fatalf("a stuffed container released: %v", err)
	}
	// customs open the container and seal it again
	if err := v.RecordSeal("CUS-778", SealCustoms, done, "customs notice 12", "ops", done); err != nil || v.Seal().Number != "CUS-778" || v.State().Status != VisitStuffed {
		t.Fatalf("a customs seal on the stuffed container: %v", err)
	}

	// The differences PHY's record may have, each taken only with a reason.
	for name, tc := range map[string]struct {
		phySeal, sizeType string
		seals             []string
		differs           string // part of the refusal; empty when the container is taken without a reason
	}{
		"the same seal":                         {"CN-SEAL-1", "22G1", []string{"CN-SEAL-1"}, ""},
		"PHY's seal in small letters":           {"cn-seal-1", "22G1", []string{"CN-SEAL-1"}, ""},
		"PHY's seal replaced by customs since":  {"CN-SEAL-1", "22G1", []string{"CN-SEAL-1", "CUS-778"}, ""},
		"another seal":                          {"CN-SEAL-2", "22G1", []string{"CN-SEAL-1"}, `seal "CN-SEAL-2"`},
		"PHY's seal as a sentence":              {"sealed by Zhang, gate 3", "22G1", []string{"CN-SEAL-1"}, `seal "sealed by Zhang, gate 3"`},
		"PHY's seal without a letter or digit":  {"--", "22G1", []string{"CN-SEAL-1"}, `seal "--"`},
		"another size-type":                     {"CN-SEAL-1", "22G0", []string{"CN-SEAL-1"}, "unit 1 is a 22G1 in PHY, the container a 22G0"},
		"another seal and another size-type":    {"CN-SEAL-2", "42G1", []string{"CN-SEAL-1"}, "; unit 1 is a 22G1 in PHY"},
		"a seal that only begins as PHY's does": {"CN-SEAL-1", "22G1", []string{"CN-SEAL-12"}, `seal "CN-SEAL-1"`},
	} {
		m := stuffedMovement(t, tc.phySeal)
		v, err := AssignContainer("CV-1", m, 1, "CSQU3054383", tc.sizeType, Plate{}, "ops", at.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, number := range tc.seals {
			sealed(t, v, number, 2)
		}
		err = v.StuffingDone(m, "", "ops", done)
		if tc.differs == "" {
			if err != nil || v.State().DifferenceReason != "" {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, ErrStuffingDiffers) || !strings.Contains(err.Error(), tc.differs) || v.State().Status != VisitAssigned {
			t.Errorf("%s: %v, want %q", name, err, tc.differs)
			continue
		}
		if err = v.StuffingDone(m, " ", "ops", done); !errors.Is(err, ErrStuffingDiffers) {
			t.Errorf("%s with a blank reason: %v", name, err)
		}
		if err = v.StuffingDone(m, "the tally sheet names the seal applied", "ops", done); err != nil ||
			v.State().DifferenceReason != "the tally sheet names the seal applied" || v.State().ExecutionID != "X-1" {
			t.Errorf("%s with its reason: %v %+v", name, err, v.State())
		}
	}

	// What the houses must carry.
	bare := open(t, booking(1))
	if err := bare.Associate(plan("SH-1", "C-1", 1), "H1", "ops", at); err != nil {
		t.Fatal(err)
	}
	v = assigned(t, bare)
	sealed(t, v, "CN-SEAL-1", 2)
	if err := v.StuffingDone(bare, "", "ops", done); !errors.Is(err, ErrNotStuffed) || !strings.Contains(err.Error(), "unit 1") {
		t.Fatalf("no house stuffed: %v", err)
	}
	unit2, err := AssignContainer("CV-2", m, 2, "TEMU1234565", "22G1", Plate{}, "ops", at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sealed(t, unit2, "CN-SEAL-1", 2)
	if err = unit2.StuffingDone(m, "", "ops", done); !errors.Is(err, ErrNotStuffed) || !strings.Contains(err.Error(), "unit 2") {
		t.Fatalf("a container on another unit than the stuffing's: %v", err)
	}
	if err = unit2.StuffingDone(m, "whatever", "ops", done); !errors.Is(err, ErrNotStuffed) {
		t.Fatalf("a reason does not stand in for the stuffing: %v", err)
	}
	// two houses in the unit agree on its stuffing; a house naming another stuffing of the unit is refused
	second := stuffed(plan("SH-2", "C-1", 1), 1, "CN-SEAL-1")
	if err = m.Associate(second, "H2", "ops", at); err != nil {
		t.Fatal(err)
	}
	v = assigned(t, m)
	sealed(t, v, "CN-SEAL-1", 2)
	if err = v.StuffingDone(m, "", "ops", done); err != nil {
		t.Fatalf("two houses in one container: %v", err)
	}
	third := stuffed(plan("SH-3", "C-1", 1), 1, "CN-SEAL-1")
	third.Stuffings[0].ExecutionID = "X-9"
	if err = m.Associate(third, "H3", "ops", at); err != nil {
		t.Fatal(err)
	}
	v = assigned(t, m)
	sealed(t, v, "CN-SEAL-1", 2)
	if err = v.StuffingDone(m, "", "ops", done); !errors.Is(err, ErrNotStuffed) || !strings.Contains(err.Error(), "two stuffings of unit 1") {
		t.Fatalf("two stuffings of one unit: %v", err)
	}
	// an ended house no longer counts
	gone := second
	gone.ConsolidationID = "C-2"
	if err = m.EndAssociation(gone, "ops", at); err != nil {
		t.Fatal(err)
	}
	away := third
	away.ConsolidationID = "C-2"
	if err = m.EndAssociation(away, "ops", at); err != nil {
		t.Fatal(err)
	}
	if err = v.StuffingDone(m, "", "ops", done); err != nil {
		t.Fatalf("after the house naming another stuffing was transferred: %v", err)
	}
}

func TestReleaseContainer(t *testing.T) {
	v := assigned(t, stuffedMovement(t, "CN-SEAL-1"))
	when := at.Add(3 * time.Hour)
	for name, tc := range map[string]struct {
		reason, by string
		at         time.Time
	}{
		"no reason":             {" ", "ops", when},
		"no actor":              {"not used", "", when},
		"no time":               {"not used", "ops", time.Time{}},
		"before the assignment": {"not used", "ops", at},
	} {
		if err := v.Release(tc.reason, tc.by, tc.at); !errors.Is(err, ErrInvalidChange) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := v.Release("the booking was cut to one container", "ops", when); err != nil {
		t.Fatal(err)
	}
	st := v.State()
	if st.Status != VisitReleased || st.ReleaseReason != "the booking was cut to one container" || st.ReleasedBy != "ops" || !st.ReleasedAt.Equal(when) || v.Live() {
		t.Fatalf("released: %+v", st)
	}
	if e := v.Pending(); len(e) != 2 || e[1].Type != eventContainerReleased || e[1].Reason != "" || e[1].MovementID != "OM-1" {
		t.Fatalf("events carry no free text: %+v", e)
	}
	if err := v.Release("again", "ops", when); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("released twice: %v", err)
	}
	if err := v.StuffingDone(stuffedMovement(t, "CN-SEAL-1"), "", "ops", at.Add(21*time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a released container stuffed: %v", err)
	}
}

func TestRestoreRefusesInvalidVisits(t *testing.T) {
	m := stuffedMovement(t, "CN-SEAL-1")
	v := assigned(t, m)
	sealed(t, v, "CN-SEAL-1", 2)
	sealed(t, v, "CUS-778", 3)
	if err := v.StuffingDone(m, "", "cfs", at.Add(21*time.Hour)); err != nil {
		t.Fatal(err)
	}
	good := v.State()
	if again, err := RestoreVisit(good); err != nil || again.Seal().Number != "CUS-778" || again.Pending() != nil || len(again.AddedSeals()) != 0 ||
		!again.LastChange().Equal(at.Add(21*time.Hour)) {
		t.Fatalf("a stored visit restored: %+v %v", again, err)
	}
	free := assigned(t, m)
	if err := free.Release("not used", "ops", at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	released := free.State()
	for name, tc := range map[string]struct {
		from   VisitState
		change func(*VisitState)
		reason string
	}{
		"no movement":                       {good, func(s *VisitState) { s.MovementID = "" }, "identity"},
		"no consolidation":                  {good, func(s *VisitState) { s.ConsolidationID = " " }, "identity"},
		"unit 0":                            {good, func(s *VisitState) { s.LoadUnitNo = 0 }, "identity"},
		"not assigned by anyone":            {good, func(s *VisitState) { s.AssignedBy = "" }, "identity"},
		"a wrong check digit":               {good, func(s *VisitState) { s.Number = "CSQU3054384" }, "container"},
		"a size-type of three":              {good, func(s *VisitState) { s.SizeType = "22G" }, "container"},
		"a tare over the maximum gross":     {good, func(s *VisitState) { s.Plate.TareKg = 40000 }, "container"},
		"submitted with no assertion":       {good, func(s *VisitState) { s.Status = VisitVGM }, "vgm"},
		"stuffed without its stuffing":      {good, func(s *VisitState) { s.ExecutionID = "" }, "stuffing"},
		"stuffed by no one":                 {good, func(s *VisitState) { s.StuffedBy = "" }, "stuffing"},
		"stuffed without a seal":            {good, func(s *VisitState) { s.Seals = nil }, "stuffing"},
		"stuffed before it was assigned":    {good, func(s *VisitState) { s.StuffedAt = at }, "stuffing"},
		"assigned with a stuffing":          {good, func(s *VisitState) { s.Status = VisitAssigned }, "stuffing"},
		"a difference on an unused one":     {released, func(s *VisitState) { s.DifferenceReason = "x" }, "difference reason"},
		"released without a reason":         {released, func(s *VisitState) { s.ReleaseReason = "" }, "release"},
		"released before it was assigned":   {released, func(s *VisitState) { s.ReleasedAt = at }, "release"},
		"assigned with a release":           {released, func(s *VisitState) { s.Status = VisitAssigned }, "release"},
		"seals out of number":               {good, func(s *VisitState) { s.Seals[1].No = 3 }, "seal 2"},
		"the same number twice running":     {good, func(s *VisitState) { s.Seals[1].Number = "CN-SEAL-1" }, "seal 2"},
		"a seal dated before the first":     {good, func(s *VisitState) { s.Seals[1].At = at.Add(90 * time.Minute); s.Seals[1].AppliedAt = at }, "seal 2"},
		"a seal dated before the assigning": {good, func(s *VisitState) { s.Seals[0].At = at; s.Seals[0].AppliedAt = at }, "seal 1"},
		"a seal applied after it is dated":  {good, func(s *VisitState) { s.Seals[0].AppliedAt = at.Add(5 * time.Hour) }, "seal 1"},
		"a seal of no party":                {good, func(s *VisitState) { s.Seals[0].Party = "" }, "seal 1"},
	} {
		st := tc.from
		st.Seals = append([]VisitSeal(nil), tc.from.Seals...)
		tc.change(&st)
		if _, err := RestoreVisit(st); !errors.Is(err, ErrInvalidVisit) || !strings.HasSuffix(err.Error(), ": "+tc.reason) {
			t.Errorf("%s: %v, want %q", name, err, tc.reason)
		}
	}
}

func TestVGM(t *testing.T) {
	m := stuffedMovement(t, "CN-SEAL-1")
	v := assigned(t, m)
	if err := v.AssertVGM(m, kernel.Quantity{Unit: "KGM", Amount: 4000}, VgmMethod1, "LE", 1, at, "", "Ada Lovelace", "ops", at); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("before the container is stuffed: %v", err)
	}
	sealed(t, v, "CN-SEAL-1", 2)
	done := at.Add(21 * time.Hour)
	if err := v.StuffingDone(m, "", "cfs", done); err != nil {
		t.Fatal(err)
	}
	cut, ok := m.VGMCutOff()
	if !ok || !cut.Equal(at.Add(48*time.Hour)) {
		t.Fatalf("cut-off: %s %v", cut, ok)
	}
	submit := done.Add(time.Hour) // still before the cut-off
	equal := kernel.Quantity{Unit: "KGM", Amount: plate.TareKg}
	elsewhere, err := OpenMovement("T", "OM-9", "C-1", booking(1), "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"below the tare": func() error {
			return v.AssertVGM(m, kernel.Quantity{Unit: "KGM", Amount: plate.TareKg - 1}, VgmMethod1, "LE", 1, submit, "", "Ada Lovelace", "ops", submit)
		},
		"no weight": func() error {
			return v.AssertVGM(m, kernel.Quantity{}, VgmMethod1, "LE", 1, submit, "", "Ada Lovelace", "ops", submit)
		},
		"a method SEA does not know": func() error {
			return v.AssertVGM(m, equal, "weighed", "LE", 1, submit, "", "Ada Lovelace", "ops", submit)
		},
		"determined after submission": func() error {
			return v.AssertVGM(m, equal, VgmMethod2, "LE", 1, submit.Add(time.Minute), "", "Ada Lovelace", "ops", submit)
		},
		"a reason while still in time": func() error {
			return v.AssertVGM(m, equal, VgmMethod1, "LE", 1, submit, "the gate was slow", "Ada Lovelace", "ops", submit)
		},
		"late with no reason": func() error {
			return v.AssertVGM(m, equal, VgmMethod1, "LE", 1, cut.Add(time.Minute), "", "Ada Lovelace", "ops", cut.Add(time.Minute))
		},
		"a name with a space before it": func() error { return v.AssertVGM(m, equal, VgmMethod1, "LE", 1, submit, "", " Ada", "ops", submit) },
		"no name":                       func() error { return v.AssertVGM(m, equal, VgmMethod1, "LE", 1, submit, "", "", "ops", submit) },
		"another movement": func() error {
			return v.AssertVGM(elsewhere, equal, VgmMethod1, "LE", 1, submit, "", "Ada Lovelace", "ops", submit)
		},
	} {
		if err := call(); !errors.Is(err, ErrInvalidVGM) || len(v.State().Assertions) != 0 {
			t.Errorf("%s: %v, assertions %d", name, err, len(v.State().Assertions))
		}
	}
	// 2.2 t is the tare: equal is allowed. Method 2 sums cargo, packing and tare.
	if err := v.AssertVGM(m, kernel.Quantity{Unit: "TNE", Amount: 3}, VgmMethod2, "LE", 4, submit.Add(-time.Hour), "", "Ada Lovelace", "ops", submit); err != nil {
		t.Fatal(err)
	}
	st := v.State()
	if st.Status != VisitVGM || len(st.Assertions) != 1 || st.Assertions[0].MassGrams != 3_000_000 || st.Assertions[0].Signatory != "Ada Lovelace" ||
		st.Assertions[0].Version != 1 || v.Pending()[len(v.Pending())-1].AssertionNo != 1 {
		t.Fatalf("asserted: %+v", st.Assertions)
	}
	if again, err := RestoreVisit(st); err != nil || again.State().Assertions[0].Signatory != "" || len(again.AddedAssertions()) != 0 {
		t.Fatalf("the name is not kept on a visit read back: %+v %v", again, err)
	}
	if err := v.RecordVgmResponse(VgmAccepted, "carrier portal", "line", submit.Add(-time.Minute)); !errors.Is(err, ErrInvalidVGM) {
		t.Fatalf("an answer before the submission: %v", err)
	}
	if err := v.RecordVgmResponse("NOTED", "carrier portal", "line", submit); !errors.Is(err, ErrInvalidVGM) {
		t.Fatalf("an answer that is neither: %v", err)
	}
	if err := v.RecordVgmResponse(VgmRejected, "carrier portal", "line", submit); err != nil {
		t.Fatal(err)
	}
	v.Acknowledge()
	if err := v.RecordVgmResponse(VgmAccepted, "carrier portal", "line", submit); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("an answer twice: %v", err)
	}
	late := cut.Add(time.Hour)
	if err := v.AssertVGM(m, equal, VgmMethod1, "LE", 4, late, "the weighbridge queued the container", "Lin Wei", "ops", late); err != nil {
		t.Fatalf("a late assertion: %v", err)
	}
	if a := v.State().Assertions; len(a) != 2 || a[0].Response != VgmRejected || a[1].Version != 2 || a[1].LateReason == "" || a[1].Response != "" || v.RespondedAssertion() != 0 {
		t.Fatalf("a second version leaves the first answer: %+v", a)
	}
	bare := booking(1)
	bare.Legs[0].CutOffs = nil
	none := open(t, bare)
	if err := none.Associate(stuffed(plan("SH-1", "C-1", 1), 1, "CN-SEAL-1"), "H1", "ops", at); err != nil {
		t.Fatal(err)
	}
	other, err := AssignContainer("CV-2", none, 1, "TEMU1234565", "22G1", plate, "ops", at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sealed(t, other, "CN-SEAL-1", 2)
	if err = other.StuffingDone(none, "", "cfs", done); err != nil {
		t.Fatal(err)
	}
	if err = other.AssertVGM(none, equal, VgmMethod1, "LE", 1, submit, "no cut-off to be late for", "Ada Lovelace", "ops", submit); !errors.Is(err, ErrInvalidVGM) {
		t.Fatalf("a reason where the voyage has no VGM cut-off: %v", err)
	}
}

func TestContainerEvents(t *testing.T) {
	m := stuffedMovement(t, "CN-SEAL-1")
	v := assigned(t, m)
	where := loc("CNSHG")
	recorded := at.Add(2 * time.Hour)
	if _, _, err := v.RecordContainerEvent(EventGateIn, where, recorded, "terminal", "GIN-1", "ops", recorded); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("gated in before the VGM: %v", err)
	}
	if _, _, err := v.RecordContainerEvent(ContainerStuffedEvent, where, recorded, "cfs", "STF-1", "ops", recorded); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("stuffing by this command: %v", err)
	}
	if _, _, err := v.RecordContainerEvent(EventEmptyPickedUp, where, recorded.Add(time.Minute), "depot", "EMP-1", "ops", recorded); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("a fact dated after it is recorded: %v", err)
	}
	no, added, err := v.RecordContainerEvent(EventEmptyPickedUp, where, at.Add(90*time.Minute), "depot", "EMP-1", "ops", recorded)
	if err != nil || !added || no != 1 || v.State().Status != VisitAssigned || v.Pending()[len(v.Pending())-1].EventNo != 1 || v.Pending()[len(v.Pending())-1].Reason != EventEmptyPickedUp {
		t.Fatalf("picked up empty: %d %v %v", no, added, err)
	}
	again, added, err := v.RecordContainerEvent(EventEmptyPickedUp, where, at.Add(90*time.Minute), "depot", "EMP-1", "ops", recorded)
	if err != nil || added || again != 1 || v.RepeatedEvent() != 1 || len(v.State().Events) != 1 {
		t.Fatalf("the same message again: %d %v %v", again, added, err)
	}
	if _, _, err = v.RecordContainerEvent(EventArrivedAtCFS, where, recorded, "depot", "EMP-1", "ops", recorded); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("the same reference with other content: %v", err)
	}
	sealed(t, v, "CN-SEAL-1", 3)
	done := at.Add(21 * time.Hour)
	if err = v.StuffingDone(m, "", "cfs", done); err != nil {
		t.Fatal(err)
	}
	submit := done.Add(time.Hour)
	if err = v.AssertVGM(m, kernel.Quantity{Unit: "KGM", Amount: plate.TareKg}, VgmMethod1, "LE", 1, submit, "", "Ada Lovelace", "ops", submit); err != nil {
		t.Fatal(err)
	}
	if _, _, err = v.RecordContainerEvent(EventLoaded, where, submit, "vessel", "LOA-1", "ops", submit); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("loaded before the gate: %v", err)
	}
	if no, added, err = v.RecordContainerEvent(EventGateIn, where, submit, "terminal", "GIN-1", "ops", submit); err != nil || !added || no != 2 || v.State().Status != VisitGatedIn {
		t.Fatalf("gated in: %d %v %v", no, added, err)
	}
	if _, _, err = v.RecordContainerEvent(EventGateIn, where, submit, "terminal", "GIN-2", "ops", submit); err != nil || v.State().Status != VisitGatedIn {
		t.Fatalf("a second gate report: %v", err)
	}
	loadedAt := submit.Add(time.Hour)
	if no, added, err = v.RecordContainerEvent(EventLoaded, where, loadedAt, "vessel", "LOA-1", "ops", loadedAt); err != nil || !added || v.State().Status != VisitLoaded {
		t.Fatalf("loaded: %d %v %v", no, added, err)
	}
	if _, _, err = v.RecordContainerEvent(EventRolled, loc("SGSIN"), loadedAt, "line", "ROL-1", "ops", loadedAt); err != nil || v.State().Status != VisitLoaded {
		t.Fatalf("rolled, and the house is not moved by it: %v", err)
	}
	if again, err := RestoreVisit(v.State()); err != nil || again.State().Status != VisitLoaded || len(again.AddedEvents()) != 0 || again.State().Events[0].Kind != EventEmptyPickedUp {
		t.Fatalf("restored: %+v %v", again, err)
	}
	free := assigned(t, m)
	if err = free.Release("not used", "ops", at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = free.RecordContainerEvent(EventEmptyReleased, where, at.Add(2*time.Hour), "depot", "REL-1", "ops", at.Add(3*time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("an event on a released container: %v", err)
	}
}
