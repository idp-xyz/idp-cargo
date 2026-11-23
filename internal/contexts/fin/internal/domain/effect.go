package domain

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.94: what submitting a stored preview would change, against the apportionment in force now (step 6c of "a
// house leaves a sealed container"; the owner's rule of 2026-11-18, item 6: before finance confirms, show each house's
// old amount, new amount, the change in its profit and why, and check that the line's cost is unchanged). It is read on
// demand from the pool, the preview and PHY's basis, and changes nothing.

// What a house bears of a line.
const (
	BearsShare       = "SHARE"                // a share of the line's cost
	BearsFormerShare = "FORMER_SHARE_PENDING" // a former share of a house that left, nobody's cost until attributed
	BearsLoss        = "LOSS"                 // the re-routing loss of a house that left, borne by its own job
	BearsNothing     = "NONE"
)

// LineEffect is one line of the preview: its cost in force now and in the preview, what the preview divides it into and
// whether the parts sum to it. Conserved is a figure shown, not a refusal: the shares, losses, exception costs, the
// amount to be allocated and, for a line retained on the consolidation, what is retained (the cost less the losses and
// the exception costs, so that a retained line is only checked for the attributions) are the cost in the preview, in
// the line's currency and in the functional currency. CostChanged says that the line's cost now is not the one the
// preview divided.
type LineEffect struct {
	LineNo       int
	Method       string
	Reason       string // the MANUAL reason
	Currency     kernel.CurrencyCode
	CostNow      int64
	CostNowLocal int64
	Cost         int64 // in the preview
	CostLocal    int64
	CostChanged  bool
	Shares       int64
	Losses       int64
	Exceptions   int64
	ToAllocate   int64
	// ToAllocateNow is what the line in force now has to be allocated (zero when it is not apportioned).
	ToAllocateNow      int64
	ToAllocateNowLocal int64
	Retained           int64
	SharesLocal        int64
	LossesLocal        int64
	ExceptLocal        int64
	ToAllocLocal       int64
	RetainedLoc        int64
	Conserved          bool
}

// HouseEffect is one house's amount on one line of the preview: in force now (Now, of the kind NowKind) and in the
// preview (Preview of PreviewKind), the difference, and the change to the house's job profit in the functional currency
// if the preview is submitted: a share or a loss lowers the profit by its amount, a former share pending is not in the
// profit, so attributing it a loss lowers the profit by the loss and attributing it nothing leaves it.
type HouseEffect struct {
	LineNo          int
	ShipmentID      string
	JobID           string
	Live            bool // PHY lists the house as live now
	Former          bool // PHY lists the house as having left
	NowKind         string
	Now             int64
	NowLocal        int64
	PreviewKind     string
	Preview         int64
	PreviewLocal    int64
	Difference      int64
	DifferenceLocal int64
	ProfitChange    int64
}

// ExceptionEffect is the exception cost of one party on one line, before (in force now) and after (in the preview), in
// the line's currency and in the functional currency.
type ExceptionEffect struct {
	LineNo      int
	Party       string
	Currency    kernel.CurrencyCode
	Before      int64
	After       int64
	BeforeLocal int64
	AfterLocal  int64
}

// ReasonEffect is why a house's figure changes: how and when its place in the consolidation ended and the operator's
// reason (PHY's FormerHouse), and what the preview's attribution says: the party and its reference, the voucher, the
// reason, and whether it is only carried forward.
type ReasonEffect struct {
	LineNo          int
	ShipmentID      string
	Former          bool
	End             string
	EndReason       string
	EndedBy         string
	EndedAt         time.Time
	TransferredTo   string
	Attributed      bool // the preview states an attribution of the house on the line
	Party           string
	PartyReference  string
	Voucher         string
	AttributionNote string
	Carried         bool
}

// HouseTotal is the change to one house's job profit over every line of the preview, in the functional currency.
type HouseTotal struct {
	ShipmentID   string
	JobID        string
	ProfitChange int64
}

// PreviewEffect is what submitting a stored preview would change. Current is whether the preview is still what a
// submission would accept (the fingerprint it was computed on is the one PHY and the pool answer now), and Changes says
// what differs when it is not, as the submission's refusal would (up to five facts, then "and N more"). A preview
// already submitted is not Current and has no Changes: SubmittedAs is the apportionment that took it. InWork says that
// PHY reads a container in rework, which a submission refuses whatever the fingerprint says. The other refusals of a
// submission (the confirmer, a loss's job no longer open) are not told here.
type PreviewEffect struct {
	PreviewNo   int
	Stage       string
	PreviewedBy string
	PreviewedAt time.Time
	SubmittedAs int
	Current     bool
	Changes     []string
	InWork      bool
	// PoolClosed says that the pool is CLOSED, which a submission refuses whatever the fingerprint says: the preview is
	// not Current then, and WayOut says what to do first.
	PoolClosed bool
	WayOut     string
	Lines      []LineEffect
	Houses     []HouseEffect
	Totals     []HouseTotal
	Exceptions []ExceptionEffect
	Reasons    []ReasonEffect
}

// PreviewEffect tells what submitting a preview would change against the apportionment in force of each of its lines.
// in carries what the submission reads again: PHY's basis now, the open jobs of the houses and, for a line by REVENUE,
// their revenue. An unknown preview is refused.
func (p *ConsolCostPool) PreviewEffect(previewNo int, in SubmitInput) (PreviewEffect, error) {
	if previewNo < 1 || previewNo > len(p.state.Previews) {
		return PreviewEffect{}, fmt.Errorf("%w: the pool has no preview %d", ErrInvalid, previewNo)
	}
	if in.Basis.ConsolidationID != p.state.ConsolidationID {
		return PreviewEffect{}, fmt.Errorf("%w: PHY answered consolidation %s for %s", ErrInvalid, in.Basis.ConsolidationID, p.state.ConsolidationID)
	}
	v := p.state.Previews[previewNo-1]
	out := PreviewEffect{PreviewNo: previewNo, Stage: v.Stage, PreviewedBy: v.By, PreviewedAt: v.At, InWork: len(in.Basis.UnitsInRework()) > 0}
	for _, a := range p.state.Apportionments {
		if a.PreviewNo == previewNo {
			out.SubmittedAs = a.No
		}
	}
	if out.SubmittedAs == 0 {
		changes := fingerprintChanges(v.Fingerprint, p.fingerprint(in.Basis, in.Jobs, in.Revenue, v.Lines))
		out.Current, out.Changes = len(changes) == 0, changes
	}
	if p.state.State == PoolClosed {
		out.PoolClosed, out.Current = true, false
		out.WayOut = "the pool is closed: reopen it with a reason (ReopenConsolCostPool), then submit the preview if it is still current"
	}
	totals := map[string]*HouseTotal{}
	var shipments []string
	for _, nl := range v.Lines {
		line := p.state.Lines[nl.LineNo-1]
		currency := line.Versions[len(line.Versions)-1].Amount.Currency
		cost, costLocal := p.LineCost(nl.LineNo)
		_, now, _ := p.SharesOfLine(nl.LineNo)
		le := lineEffect(nl, currency, cost, costLocal)
		le.ToAllocateNow, le.ToAllocateNowLocal = now.ToAllocate, now.ToAllocateLocal
		out.Lines = append(out.Lines, le)
		out.Houses, out.Reasons = p.houseEffects(out.Houses, out.Reasons, nl, now, in.Basis)
		out.Exceptions = append(out.Exceptions, exceptionEffects(nl, now, currency)...)
	}
	for _, h := range out.Houses {
		if totals[h.ShipmentID] == nil {
			totals[h.ShipmentID] = &HouseTotal{ShipmentID: h.ShipmentID, JobID: h.JobID}
			shipments = append(shipments, h.ShipmentID)
		}
		totals[h.ShipmentID].ProfitChange += h.ProfitChange
	}
	slices.Sort(shipments)
	for _, s := range shipments {
		out.Totals = append(out.Totals, *totals[s])
	}
	return out, nil
}

func lineEffect(nl LineApportionment, currency kernel.CurrencyCode, cost, costLocal int64) LineEffect {
	e := LineEffect{LineNo: nl.LineNo, Method: nl.Method, Reason: nl.Reason, Currency: currency, CostNow: cost, CostNowLocal: costLocal, Cost: nl.Amount,
		CostLocal: nl.Local, CostChanged: nl.Amount != cost || nl.Local != costLocal, ToAllocate: nl.ToAllocate, ToAllocLocal: nl.ToAllocateLocal}
	for _, s := range nl.Shares {
		if s.Used {
			e.Shares, e.SharesLocal = e.Shares+s.Amount, e.SharesLocal+s.Local
		}
	}
	for _, x := range nl.Attributions {
		e.Losses, e.LossesLocal = e.Losses+x.Loss, e.LossesLocal+x.LossLocal
		e.Exceptions, e.ExceptLocal = e.Exceptions+x.Exception, e.ExceptLocal+x.ExceptionLocal
	}
	if nl.Method == MethodRetained {
		e.Retained, e.RetainedLoc = nl.Amount-e.Losses-e.Exceptions, nl.Local-e.LossesLocal-e.ExceptLocal
	}
	e.Conserved = e.Shares+e.Losses+e.Exceptions+e.ToAllocate+e.Retained == nl.Amount &&
		e.SharesLocal+e.LossesLocal+e.ExceptLocal+e.ToAllocLocal+e.RetainedLoc == nl.Local
	return e
}

// bearing is what a house bears of a line under an apportionment, as the profit of its job counts it: the amount of its
// kind, in the line's currency and in the functional currency, and how much of it is in the job's profit (a share that
// is not a former share, and a loss).
type bearing struct {
	kind                  string
	amount, local         int64
	inProfit, inProfitLoc int64
	jobID                 string
	used                  bool
}

func bearingOf(la LineApportionment, ok bool, shipment string, basis CostBasis) bearing {
	var b bearing
	b.kind = BearsNothing
	if !ok {
		return b
	}
	_, former := basis.Former(shipment)
	for _, s := range la.Shares {
		if s.ShipmentID != shipment || !s.Used {
			continue
		}
		b.jobID, b.used = s.JobID, true
		if s.Amount > 0 && former {
			b.kind, b.amount, b.local = BearsFormerShare, s.Amount, s.Local
		} else if s.Amount > 0 {
			b.kind, b.amount, b.local, b.inProfit, b.inProfitLoc = BearsShare, s.Amount, s.Local, s.Amount, s.Local
		}
	}
	for _, x := range la.Attributions {
		if x.ShipmentID != shipment {
			continue
		}
		b.jobID, b.used = x.JobID, true
		if x.Loss > 0 {
			b.kind, b.amount, b.local, b.inProfit, b.inProfitLoc = BearsLoss, b.amount+x.Loss, b.local+x.LossLocal, b.inProfit+x.Loss, b.inProfitLoc+x.LossLocal
		}
	}
	return b
}

// houseEffects adds a row for each house that takes part in the line in force now or in the preview, and the reasons
// of those that left or that the preview attributes.
func (p *ConsolCostPool) houseEffects(houses []HouseEffect, reasons []ReasonEffect, nl, now LineApportionment, basis CostBasis) ([]HouseEffect, []ReasonEffect) {
	_, _, ok := p.SharesOfLine(nl.LineNo)
	named := map[string]bool{}
	for _, la := range []LineApportionment{nl, now} {
		for _, s := range la.Shares {
			named[s.ShipmentID] = s.Used || named[s.ShipmentID]
		}
		for _, x := range la.Attributions {
			named[x.ShipmentID] = true
		}
	}
	for _, shipment := range slices.Sorted(maps.Keys(named)) {
		if !named[shipment] {
			continue
		}
		was, will := bearingOf(now, ok, shipment, basis), bearingOf(nl, true, shipment, basis)
		h := HouseEffect{LineNo: nl.LineNo, ShipmentID: shipment, JobID: will.jobID, NowKind: was.kind, Now: was.amount, NowLocal: was.local,
			PreviewKind: will.kind, Preview: will.amount, PreviewLocal: will.local, Difference: will.amount - was.amount, DifferenceLocal: will.local - was.local,
			ProfitChange: -(will.inProfitLoc - was.inProfitLoc)}
		if h.JobID == "" {
			h.JobID = was.jobID
		}
		for _, live := range basis.Houses {
			h.Live = h.Live || live.ShipmentID == shipment
		}
		f, former := basis.Former(shipment)
		h.Former = former
		houses = append(houses, h)
		r := ReasonEffect{LineNo: nl.LineNo, ShipmentID: shipment, Former: former, End: f.End, EndReason: f.Reason, EndedBy: f.EndedBy, EndedAt: f.EndedAt,
			TransferredTo: f.TransferredTo}
		for _, x := range nl.Attributions {
			if x.ShipmentID == shipment {
				r.Attributed, r.Party, r.PartyReference, r.Voucher, r.AttributionNote, r.Carried = true, x.Party, x.PartyReference, x.Voucher, x.Reason, x.Carried
			}
		}
		if former || r.Attributed {
			reasons = append(reasons, r)
		}
	}
	return houses, reasons
}

// exceptionEffects are the exception costs of each party on a line before and after.
func exceptionEffects(nl, now LineApportionment, currency kernel.CurrencyCode) []ExceptionEffect {
	by := map[string]*ExceptionEffect{}
	add := func(la LineApportionment, after bool) {
		for _, x := range la.Attributions {
			if x.Exception == 0 {
				continue
			}
			e := by[x.Party]
			if e == nil {
				e = &ExceptionEffect{LineNo: nl.LineNo, Party: x.Party, Currency: currency}
				by[x.Party] = e
			}
			if after {
				e.After, e.AfterLocal = e.After+x.Exception, e.AfterLocal+x.ExceptionLocal
			} else {
				e.Before, e.BeforeLocal = e.Before+x.Exception, e.BeforeLocal+x.ExceptionLocal
			}
		}
	}
	add(now, false)
	add(nl, true)
	var out []ExceptionEffect
	for _, party := range slices.Sorted(maps.Keys(by)) {
		out = append(out, *by[party])
	}
	return out
}
