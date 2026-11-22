package domain

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// FIN V2.93: finance attributes a departed house's former share of the original consolidation's costs (step 6b of "a
// house leaves a sealed container"). A house PHY lists among the former houses of a consolidation that holds a used
// share on a line's apportionment in force has a former share on that line: it is pending finance's confirmation,
// outside the house's profit, until an apportionment of the line attributes it. An attribution is stated inside the
// preview of the line, for each former house: the house's re-routing loss (a cost its own job bears), the
// consolidation's exception cost (with who is responsible, or PENDING while that is not established) and a voucher.
// What remains of the line's cost is divided over the live houses by the line's method; with no live house it is to be
// allocated on the line.

// Who is responsible for a consolidation's exception cost. PENDING is a cost whose cause is not established yet: it is
// settled on the consolidation and does not stop the close.
const (
	PartyCarrier       = "CARRIER"
	PartyWarehouse     = "WAREHOUSE"
	PartyOwnOperations = "OWN_OPERATIONS"
	PartyPending       = "PENDING"
)

func validParty(party string) bool {
	switch party {
	case PartyCarrier, PartyWarehouse, PartyOwnOperations, PartyPending:
		return true
	}
	return false
}

var (
	ErrJobBearsPoolCost     = fmt.Errorf("fin: FIN_JOB_BEARS_POOL_COST: a freight job that bears a share or a re-routing loss of a consol cost pool is not cancelled")
	ErrAttributionMissing   = fmt.Errorf("fin: FIN_ATTRIBUTION_MISSING: a line holds a former share of a house that left and nobody has stated who bears it")
	ErrAttributionConfirmer = fmt.Errorf("fin: FIN_ATTRIBUTION_CONFIRMER: the person who asked for the rework that unloaded a house, or who ended its place in the consolidation, does not confirm who bears its former share; another finance user submits")
)

// AttributionInput is what finance states for one house that left a consolidation on one cost line, in the line's
// currency: the house's re-routing loss, the consolidation's exception cost with the party responsible for it (empty
// when the exception cost is zero) and a free reference, the voucher (required) and a reason (required when it replaces
// a different attribution in force: a correction).
type AttributionInput struct {
	LineNo         int
	ShipmentID     string
	Loss           int64
	Exception      int64
	Party          string
	PartyReference string
	Voucher        string
	Reason         string
}

// Attribution is the attribution of one house on one line under an apportionment: the loss and the exception cost in
// the line's currency and in the functional currency (the line's local cost divided by the same largest remainder as
// the shares, in proportion to the amounts in the line's currency), the voucher and the reason, the job that bears the
// loss and the house's departure as its business date. Carried is true for one an earlier apportionment of the line
// stated that this one carries forward unchanged.
type Attribution struct {
	ShipmentID     string
	JobID          string
	Loss           int64
	LossLocal      int64
	Exception      int64
	ExceptionLocal int64
	Party          string
	PartyReference string
	Voucher        string
	Reason         string
	DepartedAt     time.Time
	Carried        bool
}

// FormerShare is a former house's used share on a line's apportionment in force: pending finance's confirmation.
type FormerShare struct {
	LineNo int
	Share  Share
	House  FormerHouseFacts
}

// Former is the house PHY lists as having left the consolidation, if it does.
func (b CostBasis) Former(shipment string) (FormerHouseFacts, bool) {
	for _, f := range b.FormerHouses {
		if f.ShipmentID == shipment {
			return f, true
		}
	}
	return FormerHouseFacts{}, false
}

// formerSharesOf are the used shares of an apportioned line that belong to houses the basis lists as former houses and
// bear something: those pending finance's confirmation, in share order.
func formerSharesOf(la LineApportionment, basis CostBasis) []Share {
	var out []Share
	for _, s := range la.Shares {
		if _, former := basis.Former(s.ShipmentID); former && s.Used && s.Amount > 0 {
			out = append(out, s)
		}
	}
	return out
}

// FormerShares are the former shares pending on the pool's lines: those of the latest apportionment of each line.
func (p *ConsolCostPool) FormerShares(basis CostBasis) []FormerShare {
	var out []FormerShare
	for _, l := range p.state.Lines {
		if _, la, ok := p.SharesOfLine(l.No); ok {
			for _, s := range formerSharesOf(la, basis) {
				f, _ := basis.Former(s.ShipmentID)
				out = append(out, FormerShare{LineNo: l.No, Share: s, House: f})
			}
		}
	}
	return out
}

// HousesToAttribute are the houses PHY lists as former houses that hold a former share, or an attribution in force, on
// one of the pool's lines: those whose job may have to bear a loss, in shipment order.
func (p *ConsolCostPool) HousesToAttribute(basis CostBasis) []string {
	held := map[string]bool{}
	for _, l := range p.state.Lines {
		if _, la, ok := p.SharesOfLine(l.No); ok {
			for _, s := range formerSharesOf(la, basis) {
				held[s.ShipmentID] = true
			}
			for _, a := range la.Attributions {
				if _, former := basis.Former(a.ShipmentID); former {
					held[a.ShipmentID] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(held))
}

// lineAttributions is what a preview gives a line: the attributions it carries and the cost left after them, in the
// line's currency and in the functional currency.
type lineAttributions struct {
	rows           []Attribution
	remainder      int64
	remainderLocal int64
}

// attribute reads the attributions of the lines of a preview. For each house that left the consolidation and holds a
// former share on the line, or an attribution in force on it, the attribution stated for it is taken; with none stated
// the one in force is carried forward unchanged, and with none in force either the preview is refused. A stated
// attribution equal to the one in force is the one in force (carried). An attribution in force of a house PHY does not
// list as a former house any more (it is attached again) is not carried: the house takes its share of the line as the
// others do. What remains of the line's cost after the losses and the exception costs is the remainder; the local
// amounts divide the line's local cost over the remainder and each loss and exception cost by largest remainder.
func (p *ConsolCostPool) attribute(in ApportionInput, basis CostBasis, lineNos []int) (map[int]lineAttributions, error) {
	stated := map[int]map[string]AttributionInput{}
	for _, a := range in.Attributions {
		switch {
		case !slices.Contains(lineNos, a.LineNo):
			return nil, fmt.Errorf("%w: an attribution names line %d, which the preview does not divide", ErrInvalid, a.LineNo)
		case stated[a.LineNo][a.ShipmentID].ShipmentID != "":
			return nil, fmt.Errorf("%w: house %s is attributed twice on line %d", ErrInvalid, a.ShipmentID, a.LineNo)
		case a.Loss < 0 || a.Exception < 0 || a.Loss > MaxAmount || a.Exception > MaxAmount:
			return nil, fmt.Errorf("%w: the loss and the exception cost of house %s on line %d are not below 0 and at most %d minor units", ErrInvalid,
				a.ShipmentID, a.LineNo, MaxAmount)
		case (a.Exception == 0) != (a.Party == "") || (a.Party != "" && !validParty(a.Party)):
			return nil, fmt.Errorf("%w: the exception cost of house %s on line %d names who is responsible (CARRIER, WAREHOUSE, OWN_OPERATIONS or PENDING), and only when it is not zero",
				ErrInvalid, a.ShipmentID, a.LineNo)
		case !text(a.Voucher) || (a.PartyReference != "" && !text(a.PartyReference)) || (a.Reason != "" && !text(a.Reason)):
			return nil, fmt.Errorf("%w: the attribution of house %s on line %d names its voucher", ErrInvalid, a.ShipmentID, a.LineNo)
		}
		if _, former := basis.Former(a.ShipmentID); !former {
			return nil, fmt.Errorf("%w: house %s is not a house PHY reports as having left the consolidation", ErrInvalid, a.ShipmentID)
		}
		if stated[a.LineNo] == nil {
			stated[a.LineNo] = map[string]AttributionInput{}
		}
		stated[a.LineNo][a.ShipmentID] = a
	}
	out := map[int]lineAttributions{}
	for _, no := range lineNos {
		cost, local := p.LineCost(no)
		versions := p.state.Lines[no-1].Versions
		currency := versions[len(versions)-1].Amount.Currency
		_, la, apportioned := p.SharesOfLine(no)
		pending := map[string]Share{}
		if apportioned {
			for _, s := range formerSharesOf(la, basis) {
				pending[s.ShipmentID] = s
			}
		}
		inForce := map[string]Attribution{}
		for _, a := range la.Attributions {
			inForce[a.ShipmentID] = a
		}
		for _, ship := range slices.Sorted(maps.Keys(stated[no])) {
			if _, hold := pending[ship]; !hold {
				if _, hold = inForce[ship]; !hold {
					return nil, fmt.Errorf("%w: house %s holds no former share on line %d and no attribution in force on it", ErrInvalid, ship, no)
				}
			}
		}
		var rows []Attribution
		var carried bool
		for _, f := range basis.FormerHouses {
			share, owes := pending[f.ShipmentID]
			held, has := inForce[f.ShipmentID]
			a, isStated := stated[no][f.ShipmentID]
			switch {
			case !owes && !has:
				continue
			case !isStated && has:
				held.Carried, carried = true, true
				rows = append(rows, held)
			case !isStated:
				return nil, fmt.Errorf("%w: line %d holds a former share of house %s of %d %s (the loss it would bear by default, for reference); state who bears it: the house's loss and the consolidation's exception cost, with a voucher, in the preview of the line",
					ErrAttributionMissing, no, f.ShipmentID, share.Amount, currency)
			default:
				row := Attribution{ShipmentID: f.ShipmentID, JobID: share.JobID, Loss: a.Loss, Exception: a.Exception, Party: a.Party,
					PartyReference: a.PartyReference, Voucher: a.Voucher, Reason: a.Reason, DepartedAt: f.EndedAt.UTC()}
				if has {
					row.JobID = held.JobID
					if a.Loss == held.Loss && a.Exception == held.Exception && a.Party == held.Party && a.PartyReference == held.PartyReference &&
						a.Voucher == held.Voucher {
						held.Carried, carried = true, true
						rows = append(rows, held)
						continue
					}
					if !text(a.Reason) {
						return nil, fmt.Errorf("%w: the attribution of house %s on line %d replaces a different one in force; a correction states its reason", ErrInvalid,
							f.ShipmentID, no)
					}
				}
				if job := in.Jobs[f.ShipmentID]; job != "" {
					row.JobID = job
				} else if a.Loss > 0 {
					return nil, fmt.Errorf("%w: house %s bears a loss on line %d and has no open job of the pool's legal entity", ErrPoolJobMissing, f.ShipmentID, no)
				}
				rows = append(rows, row)
			}
		}
		var taken int64
		for _, r := range rows {
			taken += r.Loss + r.Exception
		}
		if taken > cost {
			if carried {
				return nil, fmt.Errorf("%w: the attributions of line %d (losses and exception costs of %d) exceed its cost of %d %s now; state them again", ErrInvalid, no,
					taken, cost, currency)
			}
			return nil, fmt.Errorf("%w: the losses and exception costs of line %d (%d) exceed its cost of %d %s", ErrInvalid, no, taken, cost, currency)
		}
		parts := []int64{cost - taken}
		for _, r := range rows {
			parts = append(parts, r.Loss, r.Exception)
		}
		locals := make([]int64, len(parts))
		if cost > 0 && len(rows) > 0 {
			var err error
			if locals, err = split(local, parts); err != nil {
				return nil, err
			}
		}
		if len(rows) == 0 {
			locals[0] = local
		}
		for i := range rows {
			rows[i].LossLocal, rows[i].ExceptionLocal = locals[1+2*i], locals[2+2*i]
		}
		out[no] = lineAttributions{rows: rows, remainder: cost - taken, remainderLocal: locals[0]}
	}
	return out, nil
}

// attributionsConfirmedBy names the houses whose new or changed attribution in the lines the person may not confirm:
// the person who asked for the rework that unloaded the house (whatever the rework's end and where PHY found it), or
// who ended the house's place in the consolidation. An attribution carried unchanged needs no check.
func attributionsConfirmedBy(basis CostBasis, by string, lines []LineApportionment) []string {
	var out []string
	seen := map[string]bool{}
	for _, la := range lines {
		for _, a := range la.Attributions {
			f, former := basis.Former(a.ShipmentID)
			if a.Carried || !former || seen[a.ShipmentID] {
				continue
			}
			var why []string
			if f.ReworkAskedBy != "" && f.ReworkAskedBy == by {
				why = append(why, fmt.Sprintf("asked for rework %d of load unit %d (stuffing %s), which unloaded house %s", f.ReworkNo, f.ReworkUnitNo,
					f.ReworkExecutionID, a.ShipmentID))
			}
			if f.EndedBy != "" && f.EndedBy == by {
				why = append(why, fmt.Sprintf("ended the place of house %s in the consolidation (%s)", a.ShipmentID, f.End))
			}
			if len(why) > 0 {
				seen[a.ShipmentID] = true
				out = append(out, fmt.Sprintf("%s %s; the attribution of house %s on line %d is new or changed", by, strings.Join(why, " and "), a.ShipmentID,
					la.LineNo))
			}
		}
	}
	return out
}

func cloneApportionedLine(la LineApportionment) LineApportionment {
	la.Shares = slices.Clone(la.Shares)
	la.Attributions = slices.Clone(la.Attributions)
	return la
}
