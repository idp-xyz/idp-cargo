package domain

import (
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
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
	// ErrAttributionReconfirm is a carried attribution that cannot be carried as it is (since V2.98): F9 RECONFIRM asks for
	// it to be stated again because its line's cost changed, or F9 KEEP_RATIO cannot scale it because the line's cost
	// then or now is not above 0, or the amounts carried no longer fit the cost.
	ErrAttributionReconfirm = fmt.Errorf("fin: FIN_ATTRIBUTION_RECONFIRM: an attribution carried from an earlier apportionment of the line cannot be carried as it is, because the line's cost has changed; state it again in the preview of the line")
)

// voucherNeeded tells whether an attribution needs a voucher: every one does, except under F3 ONLY_WHEN_AMOUNT_MOVES
// one that puts no amount on the house's job or on the consolidation (loss 0 and exception cost 0), which gives the
// whole former share to the cargo that remains. A voucher given is always checked. A correction, one that replaces a
// different attribution in force, always needs its voucher, whatever F3 says; attribute checks that where it knows the
// attribution in force.
func voucherNeeded(rules RuleSettings, a AttributionInput) bool {
	return rules.F3 != F3OnlyWhenAmountMoves || a.Loss > 0 || a.Exception > 0
}

// voucherStated tells whether the voucher of an attribution is as the rules ask: a text, or empty when none is needed.
func voucherStated(rules RuleSettings, a AttributionInput) bool {
	return text(a.Voucher) || (a.Voucher == "" && !voucherNeeded(rules, a))
}

// voucherWayOut is the way out of a refused voucher: state one; and what F3 would not ask for.
func voucherWayOut(rules RuleSettings, a AttributionInput) string {
	if voucherNeeded(rules, a) {
		return "; state a voucher of at most 500 characters"
	}
	return "; a voucher given is at most 500 characters (an attribution that moves no amount needs none under F3 ONLY_WHEN_AMOUNT_MOVES)"
}

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
// stated that this one carries forward. ByDefault is true for one nobody stated, which F8 DEFAULT_TO_REMAINING made for
// a house that held a former share (loss 0, exception cost 0, no party, no voucher; since V2.98). ByRatio is true for a
// carried one whose loss and exception cost F9 KEEP_RATIO scaled to the line's cost (since V2.98), never for one stated.
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
	ByDefault      bool
	ByRatio        bool
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

// alsoReconfirm is an error that is an ErrAttributionReconfirm too and keeps its own text: carried amounts that no
// longer fit the cost of their line are an invalid attribution, and one that has to be stated again.
type alsoReconfirm struct{ error }

func (e alsoReconfirm) Unwrap() error { return e.error }

func (alsoReconfirm) Is(target error) bool { return target == ErrAttributionReconfirm }

// stating is where an attribution in force was stated: the line's cost then, in its currency and in the functional
// currency, and the attribution as it was stated.
type stating struct {
	amount, local int64
	row           Attribution
}

// statingOf finds where the attribution in force of a house on a line was stated: the latest apportionment of the line
// that holds the house's attribution not carried, reached through the apportionments that carried it. If a link of that
// chain is missing (no command writes one; a raw write could), the attribution in force is read as stated by the
// apportionment in force.
func (p *ConsolCostPool) statingOf(lineNo int, shipment string, inForce LineApportionment, held Attribution) stating {
	for i := len(p.state.Apportionments) - 1; i >= 0; i-- {
		for _, la := range p.state.Apportionments[i].Lines {
			if la.LineNo != lineNo {
				continue
			}
			k := slices.IndexFunc(la.Attributions, func(x Attribution) bool { return x.ShipmentID == shipment })
			switch {
			case k < 0:
				return stating{inForce.Amount, inForce.Local, held}
			case !la.Attributions[k].Carried:
				return stating{la.Amount, la.Local, la.Attributions[k]}
			}
		}
	}
	return stating{inForce.Amount, inForce.Local, held}
}

// carry is what an apportionment of a line takes of an attribution in force that nobody states again (F9, since
// V2.98). Whatever the setting, an attribution is carried while the line's cost, in its currency and in the functional
// currency, is what it was when the attribution was stated. When the cost changed, KEEP_AMOUNT carries its amounts as
// they are; KEEP_RATIO takes the loss and the exception cost each as the amount stated times the line's cost now over
// its cost then, to the nearest minor unit (a half away from zero), always from the apportionment that stated it and
// never from an earlier rescale, so that a line back at its cost then gives the amounts stated; RECONFIRM refuses. The
// local amounts are divided by the caller as for any attribution.
func (p *ConsolCostPool) carry(rules RuleSettings, lineNo int, currency kernel.CurrencyCode, held Attribution, then stating, changed bool, cost, local int64) (Attribution, error) {
	// the mark of a rescale belongs to the apportionment that rescales: a row carried by amount is not marked, whatever it was
	held.Carried, held.ByRatio = true, false
	functional := p.state.FunctionalCurrency
	switch {
	case rules.F9 == F9Reconfirm && changed:
		return Attribution{}, fmt.Errorf("%w: the cost of line %d was %s (%s local) when the attribution of house %s was stated and is %s (%s local) now, and F9 RECONFIRM asks for "+
			"it to be confirmed again: state the attribution of house %s on line %d again in the preview of the line (a restatement equal to the one in force needs no reason, "+
			"and is a confirmation like any other)", ErrAttributionReconfirm, lineNo, money(currency, then.amount), money(functional, then.local), held.ShipmentID,
			money(currency, cost), money(functional, local), held.ShipmentID, lineNo)
	case rules.F9 == F9KeepRatio && changed && (then.amount <= 0 || cost <= 0):
		return Attribution{}, fmt.Errorf("%w: the cost of line %d was %s when the attribution of house %s was stated and is %s now, and F9 KEEP_RATIO needs a cost above 0 on both: "+
			"state the attribution of house %s on line %d again in the preview of the line", ErrAttributionReconfirm, lineNo, money(currency, then.amount), held.ShipmentID,
			money(currency, cost), held.ShipmentID, lineNo)
	case rules.F9 == F9KeepRatio:
		held.Loss, held.Exception, held.ByRatio = then.row.Loss, then.row.Exception, changed
		if changed {
			held.Loss, held.Exception = scaled(then.row.Loss, then.amount, cost), scaled(then.row.Exception, then.amount, cost)
		}
	}
	return held, nil
}

// scaled is amount times now over then, to the nearest whole unit, a half away from zero (amount is not below 0, then
// and now are above it).
func scaled(amount, then, now int64) int64 {
	q, r := new(big.Int).QuoRem(new(big.Int).Mul(big.NewInt(amount), big.NewInt(now)), big.NewInt(then), new(big.Int))
	if r.Lsh(r, 1).Cmp(big.NewInt(then)) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
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
// the one in force is carried forward (since V2.98 as F9 says when the line's cost changed since it was stated), and
// with none in force either the preview is refused, or under F8 DEFAULT_TO_REMAINING the house is attributed nothing by
// default. A stated attribution equal to the one in force is the one in force (carried), except under F9 RECONFIRM on a
// line whose cost changed, where it is recorded as stated. An attribution in force of a house PHY does not list as a
// former house any more (it is attached again) is not carried: the house takes its share of the line as the others do.
// What remains of the line's cost after the losses and the exception costs is the remainder; the local amounts divide
// the line's local cost over the remainder and each loss and exception cost by largest remainder.
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
		case !voucherStated(in.Settings, a) || (a.PartyReference != "" && !text(a.PartyReference)) || (a.Reason != "" && !text(a.Reason)):
			return nil, fmt.Errorf("%w: the attribution of house %s on line %d names its voucher%s", ErrInvalid, a.ShipmentID, a.LineNo, voucherWayOut(in.Settings, a))
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
			if !owes && !has {
				continue
			}
			var then stating
			var changed bool
			if has {
				then = p.statingOf(no, f.ShipmentID, la, held)
				changed = cost != then.amount || local != then.local
			}
			equal := isStated && has && a.Loss == held.Loss && a.Exception == held.Exception && a.Party == held.Party &&
				a.PartyReference == held.PartyReference && a.Voucher == held.Voucher
			// a statement equal to the attribution in force is the one in force, carried; only under F9 RECONFIRM, once the
			// line's cost has changed, it is recorded as stated, a confirmation that is not a correction
			restated := equal && changed && in.Settings.F9 == F9Reconfirm
			if equal && !restated {
				isStated = false
			}
			switch {
			case !isStated && has:
				row, err := p.carry(in.Settings, no, currency, held, then, changed, cost, local)
				if err != nil {
					return nil, err
				}
				carried = true
				rows = append(rows, row)
			case !isStated && in.Settings.F8 == F8DefaultToRemaining && !voucherNeeded(in.Settings, AttributionInput{}):
				// nobody stated it and none is in force: the house bears nothing and its whole former share goes to the cargo
				// that remains (or is to be allocated when no house is live)
				row := Attribution{ShipmentID: f.ShipmentID, JobID: share.JobID, DepartedAt: f.EndedAt.UTC(), ByDefault: true}
				if job := in.Jobs[f.ShipmentID]; job != "" {
					row.JobID = job
				}
				rows = append(rows, row)
			case !isStated:
				return nil, fmt.Errorf("%w: line %d holds a former share of house %s of %d %s (the loss it would bear by default, for reference); state who bears it: the house's loss and the consolidation's exception cost, with a voucher, in the preview of the line",
					ErrAttributionMissing, no, f.ShipmentID, share.Amount, currency)
			default:
				row := Attribution{ShipmentID: f.ShipmentID, JobID: share.JobID, Loss: a.Loss, Exception: a.Exception, Party: a.Party,
					PartyReference: a.PartyReference, Voucher: a.Voucher, Reason: a.Reason, DepartedAt: f.EndedAt.UTC()}
				if has {
					row.JobID = held.JobID
					if !restated {
						if !text(a.Reason) {
							return nil, fmt.Errorf("%w: the attribution of house %s on line %d replaces a different one in force; a correction states its reason", ErrInvalid,
								f.ShipmentID, no)
						}
						if !text(a.Voucher) { // a correction changes who bears an amount that was recorded with a voucher
							return nil, fmt.Errorf("%w: the attribution of house %s on line %d replaces a different one in force; a correction states its reason and its voucher "+
								"(F3 does not excuse it)", ErrInvalid, f.ShipmentID, no)
						}
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
				return nil, alsoReconfirm{fmt.Errorf("%w: the attributions of line %d (losses and exception costs of %d) exceed its cost of %d %s now; state them again", ErrInvalid, no,
					taken, cost, currency)}
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
// who ended the house's place in the consolidation. An attribution carried unchanged needs no check. Since V2.97 F1
// decides: under UNRESTRICTED nobody is barred, and under NOT_ABOVE_THRESHOLD the person is barred only on an
// attribution whose former share, in the functional currency, is above the threshold, each attribution on its own.
func (p *ConsolCostPool) attributionsConfirmedBy(rules RuleSettings, basis CostBasis, who confirmer, lines []LineApportionment) []string {
	var out []string
	seen := map[string]bool{}
	for _, la := range lines {
		for _, a := range la.Attributions {
			f, former := basis.Former(a.ShipmentID)
			key := a.ShipmentID // under a threshold each attribution is read on its own, so a house may be named on more than one line
			if rules.F1 == F1AboveThreshold {
				key = fmt.Sprintf("%s/%d", a.ShipmentID, la.LineNo)
			}
			// a carried attribution needs no confirmer, a rescale by F9 KEEP_RATIO included: it is the configured arithmetic applied to
			// a confirmed attribution, nobody chooses its amount (as with an amount carried under KEEP_AMOUNT), and the lever on it is
			// the revision of the line's cost, a command of its own
			if a.Carried || !former || seen[key] {
				continue
			}
			share, found := p.formerShareLocal(la.LineNo, a)
			if rules.F1 == F1Unrestricted || (found && !rules.bars(share)) { // a share not found bars the person: the amount at risk is not known
				continue
			}
			var why []string
			if f.ReworkAskedBy != "" && f.ReworkAskedBy == who.name {
				why = append(why, fmt.Sprintf("asked for rework %d of load unit %d (stuffing %s), which unloaded house %s", f.ReworkNo, f.ReworkUnitNo,
					f.ReworkExecutionID, a.ShipmentID))
			}
			if f.EndedBy != "" && f.EndedBy == who.name {
				why = append(why, fmt.Sprintf("ended the place of house %s in the consolidation (%s)", a.ShipmentID, f.End))
			}
			if len(why) > 0 {
				seen[key] = true
				out = append(out, fmt.Sprintf("%s %s; the attribution of house %s on line %d is new or changed%s", who.label, strings.Join(why, " and "),
					a.ShipmentID, la.LineNo, noteOf(rules, p.state.FunctionalCurrency, share, found)))
			}
		}
	}
	return out
}

// formerShareLocal is the former share of a house on a line that an attribution answers, in the functional currency:
// the share the line's latest apportionment holds for the house, and for an attribution that corrects one in force the
// share the house held when it left, which is the one the latest apportionment that gave the house a used share
// recorded. found is false when no apportionment of the line gave the house a used share; then the rules bar the person
// whatever the threshold, because the amount at risk is not known and a person who may be barred must not pass on a
// guess. No command reaches that (reasoned, not run: an attribution answers a former share or one in force, and either
// comes from a used share of an apportionment), so it is the safe answer for a state that should not be.
func (p *ConsolCostPool) formerShareLocal(lineNo int, a Attribution) (share int64, found bool) {
	for i := len(p.state.Apportionments) - 1; i >= 0; i-- {
		for _, la := range p.state.Apportionments[i].Lines {
			if la.LineNo != lineNo {
				continue
			}
			for _, s := range la.Shares {
				if s.ShipmentID == a.ShipmentID && s.Used && s.Amount > 0 {
					return s.Local, true
				}
			}
		}
	}
	return 0, false
}

// noteOf is the note of a refusal that says why a bar applies, when the amount at risk is known.
func noteOf(rules RuleSettings, currency kernel.CurrencyCode, share int64, found bool) string {
	if !found {
		return ""
	}
	return rules.aboveNote(currency, share)
}

func cloneApportionedLine(la LineApportionment) LineApportionment {
	la.Shares = slices.Clone(la.Shares)
	la.Attributions = slices.Clone(la.Attributions)
	return la
}
