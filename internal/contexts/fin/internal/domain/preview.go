package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.91: every apportionment is previewed and then submitted, and a pool whose container is being reworked is
// frozen. A preview computes an apportionment as FIN always has and stores it with a fingerprint of what it was
// computed on; the pool's costs, jobs and apportionments stay as they were. A submission reads PHY and the pool again,
// refuses when the fingerprint is no longer the same, and otherwise writes the previewed apportionment, linked to its
// preview. While PHY reads any container of the consolidation in REWORK, no apportionment is previewed or submitted and
// the pool is not closed: the last submitted apportionment stands, in work. The apportionment submitted after a rework
// is submitted by a finance user other than the one who asked for the rework.

// ApportionmentPreview is one immutable preview of an apportionment: the figures FIN computed (Lines, as an
// Apportionment's), the basis revision and stage they were computed on, and the Fingerprint of what they were computed
// on with its SHA-256 in hexadecimal. A preview is submitted once, by the apportionment that names it.
type ApportionmentPreview struct {
	No                int
	BasisRevision     int64
	Stage             string
	By                string
	At                time.Time
	EnteredAt         time.Time
	Lines             []LineApportionment
	Fingerprint       string
	FingerprintSHA256 string
	// SettingsVersion is the version of FIN's settings the preview was made under (since V2.97; 0 when none was in
	// force, and for a preview made before V2.97, which the fingerprint then reads the same).
	SettingsVersion int
}

// SubmitInput is what a submission reads again before it writes: PHY's cost basis now, the open job of each live house
// (the application locks them) and, when the preview has a line by REVENUE, the houses' revenue by charge code.
type SubmitInput struct {
	Basis   CostBasis
	Jobs    map[string]string
	Revenue map[string]map[string]int64
	// Settings are the settings in force for the pool's legal entity now (since V2.97).
	Settings RuleSettings
}

func cloneLines(lines []LineApportionment) []LineApportionment {
	out := slices.Clone(lines)
	for i := range out {
		out[i] = cloneApportionedLine(out[i])
	}
	return out
}

// inWork refuses while PHY reads a container of the consolidation in REWORK, naming the containers. FIN reads PHY
// before its transaction and cannot lock PHY, so a rework opened after that read is not seen: the preview, the
// submission or the close takes effect as of its read of PHY, as if it had been made just before the rework (the pool
// is then in work with that submission as its last submitted apportionment, which the rework does not carry, and a
// close is reopened with ReopenConsolCostPool).
func inWork(basis CostBasis) error {
	units := basis.UnitsInRework()
	if len(units) == 0 {
		return nil
	}
	var names []string
	for _, no := range units {
		names = append(names, unitName(basis, no))
	}
	return fmt.Errorf("%w: %s in rework", ErrPoolInWork, strings.Join(names, ", "))
}

// unitName names a load unit of the basis for a message, with its stuffing when PHY gives it a packing list.
func unitName(basis CostBasis, no int) string {
	name := fmt.Sprintf("load unit %d", no)
	for _, l := range basis.PackingLists {
		if l.UnitNo == no && l.ExecutionID != "" {
			name += fmt.Sprintf(" (stuffing %s)", l.ExecutionID)
		}
	}
	return name
}

// Preview computes an apportionment of the named lines as Apportion did before V2.91, against PHY's cost basis and the
// pool as they are now, and stores it with the fingerprint of what it was computed on. It is refused while a container
// is in rework. It writes no share, changes no cost and no job, and supersedes nothing.
func (p *ConsolCostPool) Preview(in ApportionInput, basis CostBasis, by string, at time.Time) error {
	if err := p.open(by, at); err != nil {
		return err
	}
	if err := inWork(basis); err != nil {
		return err
	}
	a, err := p.apportionment(in, basis, by, at)
	if err != nil {
		return err
	}
	text := p.fingerprintUnder(in.Settings.Version, basis, in.Jobs, in.Revenue, a.Lines)
	sum := sha256.Sum256([]byte(text))
	p.state.Previews = append(p.state.Previews, ApportionmentPreview{No: len(p.state.Previews) + 1, BasisRevision: a.BasisRevision, Stage: a.Stage, By: by,
		At: at.UTC(), Lines: a.Lines, Fingerprint: text, FingerprintSHA256: hex.EncodeToString(sum[:]), SettingsVersion: in.Settings.Version})
	return nil
}

// Submit writes a preview's apportionment as Apportion wrote one, linked to the preview, once what the preview was
// computed on is still what PHY and the pool answer: the fingerprint is computed again and any difference is refused,
// naming what changed. It is refused too while a container is in rework, when the preview was submitted already, and
// when the submitter asked for a rework of the consolidation's containers that has ended and that no earlier submitted
// apportionment's fingerprint carried as ended. A preview that carries a new or changed attribution of a house that
// left is refused too to the person who asked for the rework that unloaded the house and to the person who ended its
// place in the consolidation, naming the house (since V2.93); an attribution carried unchanged needs no such check.
func (p *ConsolCostPool) Submit(previewNo int, in SubmitInput, by string, at time.Time) error {
	if err := p.open(by, at); err != nil {
		return err
	}
	if previewNo < 1 || previewNo > len(p.state.Previews) {
		return fmt.Errorf("%w: the pool has no preview %d", ErrInvalid, previewNo)
	}
	v := p.state.Previews[previewNo-1]
	for _, a := range p.state.Apportionments {
		if a.PreviewNo == previewNo {
			return fmt.Errorf("%w: preview %d was submitted as apportionment %d", ErrPreviewSubmitted, previewNo, a.No)
		}
	}
	if at.Before(v.At) {
		return fmt.Errorf("%w: business time %s before the preview's", ErrInvalid, at.UTC().Format(time.RFC3339))
	}
	if in.Basis.ConsolidationID != p.state.ConsolidationID {
		return fmt.Errorf("%w: PHY answered consolidation %s for %s", ErrInvalid, in.Basis.ConsolidationID, p.state.ConsolidationID)
	}
	if err := inWork(in.Basis); err != nil {
		return err
	}
	if now := p.fingerprintUnder(in.Settings.Version, in.Basis, in.Jobs, in.Revenue, v.Lines); now != v.Fingerprint {
		return fmt.Errorf("%w: preview %d: %s", ErrPreviewStale, previewNo, strings.Join(fingerprintChanges(v.Fingerprint, now), "; "))
	}
	for _, la := range v.Lines {
		for _, x := range la.Attributions {
			if x.Loss > 0 && in.Jobs[x.ShipmentID] != x.JobID {
				return fmt.Errorf("%w: house %s bears a loss on line %d and its job %s is not open; preview again", ErrPoolJobMissing, x.ShipmentID, la.LineNo, x.JobID)
			}
		}
	}
	for _, who := range in.Settings.confirmers(by, v.By) {
		if refused := p.attributionsConfirmedBy(in.Settings, in.Basis, who, v.Lines); len(refused) > 0 {
			return fmt.Errorf("%w: %s%s", ErrAttributionConfirmer, strings.Join(refused, "; "), who.hint)
		}
		if asked := p.reworksAskedByUnder(in.Settings, in.Basis, who.name, v.Lines); len(asked) > 0 {
			return fmt.Errorf("%w: %s asked for %s%s", ErrConfirmer, who.label, strings.Join(asked, ", "), who.hint)
		}
	}
	a := Apportionment{No: len(p.state.Apportionments) + 1, BasisRevision: v.BasisRevision, Stage: v.Stage, By: by, At: at.UTC(), Lines: cloneLines(v.Lines),
		PreviewNo: previewNo, SettingsVersion: in.Settings.Version}
	p.state.Apportionments = append(p.state.Apportionments, a)
	p.record(eventPoolApportioned, at, a.No)
	return nil
}

// reworksAskedBy names the reworks of the basis's containers that have ended and that the person asked for, each with
// the lines of the preview it is not carried for. A rework is carried for a line when an earlier submitted
// apportionment divided that line from a preview whose fingerprint read the rework as ended: another person's
// apportionment of one line does not confirm the others, and a line recorded after the rework ended is carried by
// nothing until someone else submits it.
func (p *ConsolCostPool) reworksAskedBy(basis CostBasis, by string, lines []LineApportionment) []string {
	return p.reworksAskedByUnder(RuleSettings{}, basis, by, lines)
}

// reworksAskedByUnder is reworksAskedBy under F1: with UNRESTRICTED no rework bars the person, and with
// NOT_ABOVE_THRESHOLD a rework bars the person only for the lines whose local cost is above the threshold.
func (p *ConsolCostPool) reworksAskedByUnder(rules RuleSettings, basis CostBasis, by string, lines []LineApportionment) []string {
	carried := map[string]map[int]bool{}
	for _, a := range p.state.Apportionments {
		if a.PreviewNo < 1 {
			continue
		}
		for key, value := range fingerprintFacts(p.state.Previews[a.PreviewNo-1].Fingerprint) {
			if !strings.HasPrefix(key, "rework.") || strings.HasPrefix(value, `end ""`) {
				continue
			}
			if carried[key] == nil {
				carried[key] = map[int]bool{}
			}
			for _, la := range a.Lines {
				carried[key][la.LineNo] = true
			}
		}
	}
	var out []string
	check := func(unit int, execution, kind string, reworks []ReworkFacts) {
		for _, r := range reworks {
			if r.End == "" || r.AskedBy != by {
				continue
			}
			var missing []string
			for _, la := range lines {
				if _, local := p.LineCost(la.LineNo); !carried[reworkKey(unit, execution, r)][la.LineNo] && rules.bars(local) {
					missing = append(missing, fmt.Sprint(la.LineNo)+rules.aboveNote(p.state.FunctionalCurrency, local))
				}
			}
			if len(missing) > 0 {
				out = append(out, fmt.Sprintf("rework %d of load unit %d (%s%s), which ended %s, for line %s", r.No, unit, kind, execution, r.End,
					strings.Join(missing, ", ")))
			}
		}
	}
	for _, l := range basis.PackingLists {
		check(l.UnitNo, l.ExecutionID, "stuffing ", l.Reworks)
	}
	for _, g := range basis.GivenUpStuffings {
		check(g.UnitNo, g.ExecutionID, "given-up stuffing ", g.Reworks)
	}
	return out
}

// reworkKey is the fingerprint's key of a rework: the same for a stuffing with a packing list and for one given up, so
// a rework that a preview read as ended is still read as that after its stuffing was given up.
func reworkKey(unit int, execution string, r ReworkFacts) string {
	return fmt.Sprintf("rework.%d.%q.%d", unit, execution, r.No)
}

func quantityText(q kernel.Quantity) string { return fmt.Sprintf("%d%s", q.Amount, q.Unit) }

// fingerprint is the text of what an apportionment of lines is computed on, as PHY's basis and the pool now answer it:
// one fact to a line, a key and a value separated by a tab, in key order. The facts are the basis revision; each load
// unit with its type, TEU, status and stuffing; each packing list with its stuffing, state and digest, and each rework
// of it with its end, and the reworks of each stuffing given up the same way; each house that left, with how, to where,
// when and in which rework; each live house with its job and its plan and loaded figures; each line of the preview with
// its version, its cost and the number of the apportionment in force for it (0 when none: another apportionment of the
// line submitted since makes the preview stale, a revision or a new line of another line does not); and, for a line by
// REVENUE, the revenue of each house that takes part. The people who asked for the reworks are not in it: the confirmer
// rule reads them from PHY's basis.
func (p *ConsolCostPool) fingerprint(basis CostBasis, jobs map[string]string, revenue map[string]map[string]int64, lines []LineApportionment) string {
	return p.fingerprintUnder(0, basis, jobs, revenue, lines)
}

// fingerprintUnder is fingerprint under a version of FIN's settings (since V2.97): the version is one more fact, so
// that a preview made under one version is stale under another. With no settings in force (version 0) there is no such
// fact, and the text is what V2.96 wrote, so that a preview made before V2.97 stays current while no setting is set.
func (p *ConsolCostPool) fingerprintUnder(settingsVersion int, basis CostBasis, jobs map[string]string, revenue map[string]map[string]int64,
	lines []LineApportionment) string {
	facts := map[string]string{"basis": fmt.Sprintf("revision %d", basis.Revision)}
	if settingsVersion > 0 {
		facts["settings"] = fmt.Sprintf("version %d", settingsVersion)
	}
	for _, u := range basis.LoadUnits {
		facts[fmt.Sprintf("unit.%d", u.UnitNo)] = fmt.Sprintf("type %q teu %d status %q stuffing %q state %q", u.TypeCode, u.TEUTenths, u.Status, u.ExecutionID,
			u.ExecutionState)
	}
	for _, l := range basis.PackingLists {
		facts[fmt.Sprintf("packing.%d", l.UnitNo)] = fmt.Sprintf("stuffing %q state %q digest %q", l.ExecutionID, l.ExecutionState, l.Digest)
		for _, r := range l.Reworks {
			facts[reworkKey(l.UnitNo, l.ExecutionID, r)] = fmt.Sprintf("end %q ended_at %s", r.End, r.EndedAt.UTC().Format(time.RFC3339Nano))
		}
	}
	for _, g := range basis.GivenUpStuffings {
		for _, r := range g.Reworks {
			facts[reworkKey(g.UnitNo, g.ExecutionID, r)] = fmt.Sprintf("end %q ended_at %s", r.End, r.EndedAt.UTC().Format(time.RFC3339Nano))
		}
	}
	for _, f := range basis.FormerHouses {
		facts[fmt.Sprintf("former.%q", f.ShipmentID)] = fmt.Sprintf("end %q to %q ended_at %s rework %q %d", f.End, f.TransferredTo,
			f.EndedAt.UTC().Format(time.RFC3339Nano), f.ReworkExecutionID, f.ReworkNo)
	}
	for _, h := range basis.Houses {
		var units []string
		for _, u := range h.Units {
			units = append(units, fmt.Sprintf("%d:%d:%s:%s", u.UnitNo, u.LoadedPacks, quantityText(u.LoadedGrossWeight), quantityText(u.LoadedVolume)))
		}
		facts[fmt.Sprintf("house.%q", h.ShipmentID)] = fmt.Sprintf("job %q packs %d weight %s volume %s loaded_packs %d loaded_weight %s loaded_volume %s units %s",
			jobs[h.ShipmentID], h.Packs, quantityText(h.GrossWeight), quantityText(h.Volume), h.LoadedPacks, quantityText(h.LoadedGrossWeight),
			quantityText(h.LoadedVolume), strings.Join(units, ","))
	}
	for _, la := range lines {
		l := p.state.Lines[la.LineNo-1]
		cost, local := p.LineCost(l.No)
		inForce := 0
		if a, _, ok := p.SharesOfLine(l.No); ok {
			inForce = a.No
		}
		facts[fmt.Sprintf("line.%d", l.No)] = fmt.Sprintf("version %d cost %d local %d apportioned %d", len(l.Versions), cost, local, inForce)
	}
	for _, la := range lines {
		if la.Method != MethodRevenue {
			continue
		}
		code := p.state.Lines[la.LineNo-1].ChargeCode
		for _, s := range la.Shares {
			if s.Used {
				facts[fmt.Sprintf("revenue.%d.%q", la.LineNo, s.ShipmentID)] = fmt.Sprintf("%d", revenue[s.ShipmentID][code])
			}
		}
	}
	keys := make([]string, 0, len(facts))
	for k := range facts {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "\t" + facts[k] + "\n")
	}
	return b.String()
}

// fingerprintFacts reads a fingerprint's facts by key.
func fingerprintFacts(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if key, value, ok := strings.Cut(line, "\t"); ok {
			out[key] = value
		}
	}
	return out
}

// fingerprintChanges names what differs between two fingerprints, fact by fact: what the preview read and what is there
// now.
func fingerprintChanges(was, now string) []string {
	a, b := fingerprintFacts(was), fingerprintFacts(now)
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	slices.Sort(ordered)
	var out []string
	for _, k := range ordered {
		before, had := a[k]
		after, has := b[k]
		if k == "settings" { // a version of FIN's settings; none in force is version 0; it is told first
			was, now := strings.TrimPrefix(before, "version "), strings.TrimPrefix(after, "version ")
			if !had {
				was = "0"
			}
			if !has {
				now = "0"
			}
			if was != now {
				out = append([]string{fmt.Sprintf("the settings changed from version %s to %s", was, now)}, out...)
			}
			continue
		}
		switch {
		case had && has && before != after:
			out = append(out, fmt.Sprintf("%s changed from [%s] to [%s]", k, before, after))
		case had && !has:
			out = append(out, fmt.Sprintf("%s [%s] is gone", k, before))
		case has && !had:
			out = append(out, fmt.Sprintf("%s [%s] is new", k, after))
		}
	}
	const most = 5
	if len(out) > most {
		out = append(out[:most], fmt.Sprintf("and %d more", len(out)-most))
	}
	return out
}
