package domain

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.100: what a change of the settings would change for a pool, answered by asking the pool's own rules under the
// settings in force and under the settings the change would make, and comparing the answers. Nothing here changes the
// pool: every rule it asks of is one that a command also asks, through the same function.

// CloseRefusal is the close's refusal of an open pool under the settings, from PHY's basis as it is now (the caller
// sets
// HouseRevenue first when the pool has a line by REVENUE); empty when the pool could be closed.
func (p *ConsolCostPool) CloseRefusal(rules RuleSettings, basis CostBasis) string {
	if err := p.closeRefusal(rules, basis); err != nil {
		return err.Error()
	}
	return ""
}

// OneStepRefusal is the refusal of an apportionment of every line in one step under the settings, from PHY's basis and
// the open jobs (and their revenue) as they are now, whoever asks (who asks is checked after it, and is not guessed);
// empty when it could be made.
func (p *ConsolCostPool) OneStepRefusal(rules RuleSettings, basis CostBasis, jobs map[string]string, revenue map[string]map[string]int64) string {
	if _, err := p.oneStepRefusal(ApportionInput{Settings: rules, Jobs: jobs, Revenue: revenue}, basis, "", time.Time{}); err != nil {
		return err.Error()
	}
	return ""
}

// CarriedHouse is an attribution's amounts in the line's currency.
type CarriedHouse struct {
	ShipmentID string
	Loss       int64
	Exception  int64
	ByRatio    bool
}

// LineOutcome is what the next preview of a line would do, with nobody stating an attribution, under settings: which
// houses that hold a former share it would attribute by default (F8), whether it would be refused for want of one, and
// the attributions in force it would carry, with their amounts (F9), or be refused for them to be stated again. Former
// are the houses that hold a former share on the line, and InForce the attributions in force.
type LineOutcome struct {
	LineNo    int
	Currency  kernel.CurrencyCode
	Former    []string
	InForce   []CarriedHouse
	Defaulted []string
	Missing   bool
	Restate   bool
	Carried   []CarriedHouse
}

// LineOutcomes tells, for each cost line of the pool apportioned at least once, the outcome of its next preview under
// the settings: through the same function as a preview (attribute).
func (p *ConsolCostPool) LineOutcomes(rules RuleSettings, basis CostBasis, jobs map[string]string) []LineOutcome {
	var out []LineOutcome
	for _, l := range p.state.Lines {
		_, la, ok := p.SharesOfLine(l.No)
		if !ok {
			continue
		}
		o := LineOutcome{LineNo: l.No, Currency: p.lineCurrency(l)}
		for _, s := range formerSharesOf(la, basis) {
			o.Former = append(o.Former, s.ShipmentID)
		}
		for _, a := range la.Attributions {
			o.InForce = append(o.InForce, CarriedHouse{ShipmentID: a.ShipmentID, Loss: a.Loss, Exception: a.Exception})
		}
		rows, err := p.attribute(ApportionInput{Settings: rules, Jobs: jobs}, basis, []int{l.No})
		switch {
		case errors.Is(err, ErrAttributionMissing):
			o.Missing = true
		case errors.Is(err, ErrAttributionReconfirm):
			o.Restate = true
		case err != nil:
			continue // the line cannot be previewed for another reason, whatever the settings say
		}
		for _, r := range rows[l.No].rows {
			switch {
			case r.ByDefault:
				o.Defaulted = append(o.Defaulted, r.ShipmentID)
			case r.Carried:
				o.Carried = append(o.Carried, CarriedHouse{ShipmentID: r.ShipmentID, Loss: r.Loss, Exception: r.Exception, ByRatio: r.ByRatio})
			}
		}
		out = append(out, o)
	}
	return out
}

// UnsubmittedPreviews are the previews that no apportionment names.
func (p *ConsolCostPool) UnsubmittedPreviews() []ApportionmentPreview {
	var out []ApportionmentPreview
	for _, v := range p.state.Previews {
		if !slices.ContainsFunc(p.state.Apportionments, func(a Apportionment) bool { return a.PreviewNo == v.No }) {
			out = append(out, v)
		}
	}
	return out
}

// PreviewCheck says that the submission of a preview would check a rule differently: Rule is F1, F2 or F3; Now and
// After
// say how it is checked now and after the change.
type PreviewCheck struct {
	Rule       string
	LineNo     int
	ShipmentID string
	Now        string
	After      string
	Note       string
}

// PreviewChecks lists what the submission of an unsubmitted preview would check differently under the settings after
// the change than under those in force, without saying who submits: F1 for each new or changed attribution (whether the
// people who asked for the rework that unloaded the house, or who ended its place, are barred from confirming it) and
// for each line of the preview that a rework which ended and is not carried for it would bar its asker from;
// F2 for the preview as a whole, when the previewer's being checked or not matters (some attribution or rework check
// bars someone under either settings); F3 for each new or changed attribution whose voucher is needed under one and not
// the other. An attribution carried unchanged needs no check, as in a submission.
func (p *ConsolCostPool) PreviewChecks(now, after RuleSettings, basis CostBasis, v ApportionmentPreview) []PreviewCheck {
	var out []PreviewCheck
	checked := false
	checking := func(r RuleSettings, share int64, found bool) bool {
		return r.F1 != F1Unrestricted && (!found || r.bars(share))
	}
	way := func(b bool) string {
		if b {
			return "checked"
		}
		return "not checked"
	}
	for _, la := range v.Lines {
		for _, a := range la.Attributions {
			if _, former := basis.Former(a.ShipmentID); a.Carried || !former {
				continue
			}
			share, found := p.formerShareLocal(la.LineNo, a)
			was, will := checking(now, share, found), checking(after, share, found)
			checked = checked || was || will
			if was != will {
				out = append(out, PreviewCheck{Rule: "F1", LineNo: la.LineNo, ShipmentID: a.ShipmentID, Now: way(was), After: way(will),
					Note: "whether the person who asked for the rework that unloaded the house, or ended its place, may confirm the attribution"})
			}
			input := AttributionInput{Loss: a.Loss, Exception: a.Exception}
			if needed, willNeed := voucherNeeded(now, input), voucherNeeded(after, input); needed != willNeed {
				out = append(out, PreviewCheck{Rule: "F3", LineNo: la.LineNo, ShipmentID: a.ShipmentID, Now: map[bool]string{true: "voucher needed", false: "no voucher needed"}[needed],
					After: map[bool]string{true: "voucher needed", false: "no voucher needed"}[willNeed], Note: "the attribution moves no amount"})
			}
		}
	}
	carried := p.carriedReworks()
	endedReworks(basis, func(unit int, execution, kind string, r ReworkFacts) {
		for _, la := range v.Lines {
			if carried[reworkKey(unit, execution, r)][la.LineNo] {
				continue
			}
			_, local := p.LineCost(la.LineNo)
			was, will := now.bars(local), after.bars(local)
			checked = checked || was || will
			if was != will {
				out = append(out, PreviewCheck{Rule: "F1", LineNo: la.LineNo, Now: way(was), After: way(will),
					Note: fmt.Sprintf("whether the person who asked for rework %d of load unit %d may submit the first apportionment of the line after it", r.No, unit)})
			}
		}
	})
	if now.F2 != after.F2 && checked {
		describe := func(r RuleSettings) string {
			if r.F2 == F2SubmitterAndPreviewer {
				return "the submitter and the previewer " + v.By
			}
			return "the submitter only"
		}
		out = append(out, PreviewCheck{Rule: "F2", Now: describe(now), After: describe(after), Note: "who is checked as the confirmer"})
	}
	return out
}
