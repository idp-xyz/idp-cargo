package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// V2.90: each container of the published facts states its own on-board date, and a loading that changes what the
// latest facts state for that container returns a FINAL movement to OPEN, so that DOC can date a house's bill by the
// loading of its own containers. CV-1 is CSQU3054383 and CV-2 is TEMU1234565 (see newTwoDay).

const (
	box1 = "CSQU3054383"
	box2 = "TEMU1234565"
)

// published is the containers of the latest published version, by number, and the version.
func published(t *testing.T, m *Movement) (map[string]DocContainer, DocFactsVersion) {
	t.Helper()
	v, _, ok := m.DocFactsVersion(0)
	if !ok {
		t.Fatal("nothing published")
	}
	out := map[string]DocContainer{}
	for _, c := range v.Facts.Containers {
		out[c.Number] = c
	}
	return out, v
}

// report records a carrier's loading report that names the container, or none, as an operator does at hour recorded.
func report(t *testing.T, m *Movement, live []string, container string, occurred, recorded int) {
	t.Helper()
	bill := "MAEU123456789"
	if container != "" {
		bill = ""
	}
	f := CarrierFact{Kind: CarrierLoaded, ContainerNumber: container, BillNumber: bill, Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line",
		Reference: "LD-" + container + "-" + strings.Repeat("x", occurred%7) + string(rune('A'+occurred)), SHA256: strings.Repeat("ab", 32), OccurredAt: hr(occurred)}
	if _, _, err := m.RecordCarrierFact(f, live, "ops", hr(recorded)); err != nil {
		t.Fatal(err)
	}
}

func loadedAt(t *testing.T, d *twoDay, v *Visit, hour int) {
	t.Helper()
	d.event(v, EventGateIn, "GIN-"+v.State().Number, hour-1, hour-1)
	d.event(v, EventLoaded, "LD-"+v.State().Number, hour, hour)
}

func TestEachContainerStatesItsOwnLoading(t *testing.T) {
	// its own event: CV-1 loaded at hour 24, CV-2 not loaded
	d := newTwoDay(t)
	loadedAt(t, d, d.v1, 24)
	if err := d.finalize(30); err != nil {
		t.Fatal(err)
	}
	got, v := published(t, d.m)
	if !v.Facts.ContainerDates || !got[box1].OnBoardAt.Equal(hr(24)) || !got[box2].OnBoardAt.IsZero() || !v.Facts.OnBoardAt.Equal(hr(24)) {
		t.Fatalf("own event: %+v movement %s", got, v.Facts.OnBoardAt)
	}
	var raw struct {
		ContainerDates bool `json:"container_dates"`
		Containers     []map[string]any
	}
	if err := json.Unmarshal(v.Content, &raw); err != nil || !raw.ContainerDates || len(raw.Containers) != 2 ||
		raw.Containers[0]["on_board_at"] == nil || raw.Containers[1]["on_board_at"] != nil {
		t.Fatalf("published content: %s", v.Content)
	}

	// the carrier's report that names CV-2
	d = newTwoDay(t)
	report(t, d.m, []string{box1, box2}, box2, 26, 26)
	if err := d.finalize(30); err != nil {
		t.Fatal(err)
	}
	if got, v = published(t, d.m); !got[box2].OnBoardAt.Equal(hr(26)) || !got[box1].OnBoardAt.IsZero() || !v.Facts.OnBoardAt.Equal(hr(26)) {
		t.Fatalf("named report: %+v movement %s", got, v.Facts.OnBoardAt)
	}

	// a report that names no container, with two live containers: it is the movement's date and no container's
	d = newTwoDay(t)
	report(t, d.m, []string{box1, box2}, "", 26, 26)
	if err := d.finalize(30); err != nil {
		t.Fatal(err)
	}
	if got, v = published(t, d.m); !got[box1].OnBoardAt.IsZero() || !got[box2].OnBoardAt.IsZero() || !v.Facts.OnBoardAt.Equal(hr(26)) || !v.Facts.ContainerDates {
		t.Fatalf("nameless report, two containers: %+v movement %s", got, v.Facts.OnBoardAt)
	}

	// the same report with exactly one live container is about it
	one := newReDay(t)
	report(t, one.m, []string{box1}, "", 26, 26)
	if err := one.finalize(30); err != nil {
		t.Fatal(err)
	}
	if got, v = published(t, one.m); !got[box1].OnBoardAt.Equal(hr(26)) || !v.Facts.OnBoardAt.Equal(hr(26)) {
		t.Fatalf("nameless report, one container: %+v movement %s", got, v.Facts.OnBoardAt)
	}

	// the earliest of its own event and the reports that count for it
	d = newTwoDay(t)
	loadedAt(t, d, d.v1, 24)
	report(t, d.m, []string{box1, box2}, box1, 23, 26)
	report(t, d.m, []string{box1, box2}, box2, 22, 26)
	report(t, d.m, []string{box1, box2}, box1, 25, 26)
	if err := d.finalize(30); err != nil {
		t.Fatal(err)
	}
	if got, v = published(t, d.m); !got[box1].OnBoardAt.Equal(hr(23)) || !got[box2].OnBoardAt.Equal(hr(22)) || !v.Facts.OnBoardAt.Equal(hr(22)) {
		t.Fatalf("earliest: %+v movement %s", got, v.Facts.OnBoardAt)
	}

	// nothing loaded: no date anywhere, and the version still says that it states container dates
	d = newTwoDay(t)
	if err := d.finalize(30); err != nil {
		t.Fatal(err)
	}
	if got, v = published(t, d.m); !v.Facts.ContainerDates || !got[box1].OnBoardAt.IsZero() || !got[box2].OnBoardAt.IsZero() || !v.Facts.OnBoardAt.IsZero() ||
		strings.Contains(string(v.Content), `"on_board_at"`) {
		t.Fatalf("not loaded: %s", v.Content)
	}
}

// A loading that the movement's own date does not change still returns the movement to OPEN when the latest facts state
// no date, or a later one, for that container.
func TestALoadingOfAnotherContainerReopensTheMovement(t *testing.T) {
	d := newTwoDay(t)
	loadedAt(t, d, d.v1, 24)
	if err := d.finalize(30); err != nil {
		t.Fatal(err)
	}
	// CV-2 loads at hour 27, after CV-1: the movement's date (hour 24) is not changed, CV-2's is (none to hour 27)
	if d.m.changesOnBoard(hr(27)) {
		t.Fatal("the movement's date does not change")
	}
	if err := d.m.LoadedOnBoard(hr(27), "ops", hr(31)); err != nil || d.m.State().State != StateFinal {
		t.Fatalf("the movement's own reading leaves it FINAL: %v %s", err, d.m.State().State)
	}
	if err := d.m.ContainerLoadedOnBoard(box2, hr(27), "ops", hr(31)); err != nil || d.m.State().State != StateOpen {
		t.Fatalf("CV-2's first loading: %v %s", err, d.m.State().State)
	}
	if e := d.m.Pending()[len(d.m.Pending())-1]; e.Type != eventSuperseded || e.FactsVersion != 1 {
		t.Fatalf("event: %+v", e)
	}
	if err := d.finalize(32); err != nil {
		t.Fatal(err)
	}
	got, _ := published(t, d.m)
	if !got[box2].OnBoardAt.IsZero() {
		t.Fatalf("version 2 was published before CV-2's loading was recorded in the visit: %+v", got[box2])
	}
	// a later loading of CV-1 and an unknown container leave it FINAL; an earlier loading of CV-1 changes it
	if err := d.m.ContainerLoadedOnBoard(box1, hr(25), "ops", hr(33)); err != nil || d.m.State().State != StateFinal {
		t.Fatalf("a later loading of CV-1: %v %s", err, d.m.State().State)
	}
	if err := d.m.ContainerLoadedOnBoard("NO_SUCH_BOX", hr(26), "ops", hr(33)); err != nil || d.m.State().State != StateFinal {
		t.Fatalf("an unknown container: %v %s", err, d.m.State().State)
	}
	if err := d.m.ContainerLoadedOnBoard(box1, hr(23), "ops", hr(33)); err != nil || d.m.State().State != StateOpen {
		t.Fatalf("an earlier loading of CV-1: %v %s", err, d.m.State().State)
	}
}

// The container's own date counts, not the movement's: CV-2's published date is later than a new loading of CV-2 that
// is itself later than the movement's date (CV-1's).
func TestALoadingEarlierThanTheContainersPublishedDateReopensTheMovement(t *testing.T) {
	d := newTwoDay(t)
	loadedAt(t, d, d.v1, 24)
	loadedAt(t, d, d.v2, 27)
	if err := d.finalize(30); err != nil {
		t.Fatal(err)
	}
	if err := d.m.ContainerLoadedOnBoard(box2, hr(28), "ops", hr(31)); err != nil || d.m.State().State != StateFinal {
		t.Fatalf("a loading of CV-2 after its published date: %v %s", err, d.m.State().State)
	}
	if d.m.changesOnBoard(hr(25)) {
		t.Fatal("the movement's date, hour 24, is earlier than the new loading")
	}
	if err := d.m.ContainerLoadedOnBoard(box2, hr(25), "ops", hr(31)); err != nil || d.m.State().State != StateOpen {
		t.Fatalf("a loading of CV-2 before its published date: %v %s", err, d.m.State().State)
	}
}

// The carrier's reports act the same: one that names a container, and one that names none with exactly one live
// container, change that container's date; one that names none with two live containers changes only the movement's.
func TestACarrierLoadingReopensByTheContainerItIsAbout(t *testing.T) {
	both := []string{box1, box2}
	fresh := func() *twoDay {
		d := newTwoDay(t)
		loadedAt(t, d, d.v1, 24)
		if err := d.finalize(30); err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := fresh()
	report(t, d.m, both, "", 27, 40)
	if d.m.State().State != StateFinal {
		t.Fatal("a report naming no container, with two live containers, is not about CV-2: the movement's date stands")
	}
	d = fresh()
	report(t, d.m, both, box2, 27, 40)
	if d.m.State().State != StateOpen {
		t.Fatal("a report naming CV-2 gives it a date the facts do not state")
	}
	d = fresh()
	report(t, d.m, both, box1, 27, 40)
	if d.m.State().State != StateFinal {
		t.Fatal("a later report naming CV-1 changes nothing")
	}
	one := newReDay(t)
	loadedAt(t, &twoDay{t: t, m: one.m, v1: one.v}, one.v, 24)
	if err := one.finalize(30); err != nil {
		t.Fatal(err)
	}
	report(t, one.m, []string{box1}, "", 25, 40)
	if one.m.State().State != StateFinal {
		t.Fatal("with one live container a later nameless report is about it and changes nothing")
	}
	// with one live container, and facts stating no date for it, a nameless report is about it
	one = newReDay(t)
	if err := one.finalize(30); err != nil {
		t.Fatal(err)
	}
	report(t, one.m, []string{box1}, "", 31, 40)
	if one.m.State().State != StateOpen {
		t.Fatal("with one live container a nameless report gives it a date")
	}
}

// Facts published before V2.90 state no date for any container and say nothing of container dates: a loading is read
// by the movement's date only, as it was.
func TestFactsOfEarlierVersionsAreReadByTheMovementsDate(t *testing.T) {
	d := newTwoDay(t)
	loadedAt(t, d, d.v1, 24)
	if err := d.finalize(30); err != nil {
		t.Fatal(err)
	}
	st := d.m.State()
	var generic map[string]any
	if err := json.Unmarshal(st.DocFacts[0].Content, &generic); err != nil {
		t.Fatal(err)
	}
	delete(generic, "container_dates")
	for _, c := range generic["containers"].([]any) {
		delete(c.(map[string]any), "on_board_at")
	}
	old, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	st.DocFacts[0] = DocFactsVersion{No: 1, Content: old, SHA256: Digest(old), By: st.DocFacts[0].By, At: st.DocFacts[0].At}
	m, err := RestoreMovement(st)
	if err != nil {
		t.Fatal(err)
	}
	v, _, _ := m.DocFactsVersion(0)
	if v.Facts.ContainerDates || !v.Facts.Containers[0].OnBoardAt.IsZero() || !v.Facts.OnBoardAt.Equal(hr(24)) {
		t.Fatalf("an old version: %+v", v.Facts)
	}
	if err = m.ContainerLoadedOnBoard(box2, hr(27), "ops", hr(31)); err != nil || m.State().State != StateFinal {
		t.Fatalf("a loading after the movement's date, old facts: %v %s", err, m.State().State)
	}
	if err = m.ContainerLoadedOnBoard(box2, hr(20), "ops", hr(31)); err != nil || m.State().State != StateOpen {
		t.Fatalf("a loading before the movement's date, old facts: %v %s", err, m.State().State)
	}
}

// A container takes the earliest of its own loading events, and a second loading event is accepted.
func TestAContainersDateIsTheEarliestOfItsOwnLoadings(t *testing.T) {
	d := newTwoDay(t)
	d.event(d.v1, EventGateIn, "GIN-1", 22, 22)
	d.event(d.v1, EventLoaded, "LD-1", 26, 26)
	d.event(d.v1, EventLoaded, "LD-1B", 24, 27)
	d.event(d.v1, EventLoaded, "LD-1C", 28, 28)
	if err := d.finalize(30); err != nil {
		t.Fatal(err)
	}
	if got, v := published(t, d.m); !got[box1].OnBoardAt.Equal(hr(24)) || !v.Facts.OnBoardAt.Equal(hr(24)) {
		t.Fatalf("the earliest of three loadings: %+v", got[box1])
	}
}

// A loading of a container that no house of the published facts has packs in changes no house's date: the movement
// stays FINAL, whether it is the container's own event or the carrier's report. A container a house names still reopens
// it.
func TestALoadingOfAContainerNoHouseNamesLeavesTheMovementFinal(t *testing.T) {
	d := newTwoDay(t)
	loadedAt(t, d, d.v1, 24)
	if err := d.finalize(30); err != nil {
		t.Fatal(err)
	}
	st := d.m.State()
	var generic map[string]any
	if err := json.Unmarshal(st.DocFacts[0].Content, &generic); err != nil {
		t.Fatal(err)
	}
	var visit2 string
	for _, c := range generic["containers"].([]any) {
		if c.(map[string]any)["number"] == box2 {
			visit2 = c.(map[string]any)["visit_id"].(string)
		}
	}
	for _, h := range generic["houses"].([]any) {
		var kept []any
		for _, b := range h.(map[string]any)["containers"].([]any) {
			if b.(map[string]any)["visit_id"] != visit2 {
				kept = append(kept, b)
			}
		}
		h.(map[string]any)["containers"] = kept
	}
	content, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	st.DocFacts[0] = DocFactsVersion{No: 1, Content: content, SHA256: Digest(content), By: st.DocFacts[0].By, At: st.DocFacts[0].At}
	m, err := RestoreMovement(st)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.ContainerLoadedOnBoard(box2, hr(27), "ops", hr(31)); err != nil || m.State().State != StateFinal {
		t.Fatalf("the loading of a container no house names: %v %s", err, m.State().State)
	}
	report(t, m, []string{box1, box2}, box2, 27, 32)
	if m.State().State != StateFinal {
		t.Fatal("the carrier's report of a container no house names")
	}
	if err = m.ContainerLoadedOnBoard(box1, hr(23), "ops", hr(33)); err != nil || m.State().State != StateOpen {
		t.Fatalf("the loading of a container a house names: %v %s", err, m.State().State)
	}
}
