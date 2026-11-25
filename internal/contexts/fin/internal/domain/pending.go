package domain

import (
	"fmt"
	"slices"
	"strings"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.94: what a cost pool still needs, as a list the operator reads (step 6c of "a house leaves a sealed
// container"). Each item has a kind, where it applies (a line, a house, a container), the amount where there is one,
// what to do next and who acts: FINANCE or OPERATIONS. The list is read on demand from the pool and PHY's basis and
// stores nothing.

// Who acts on a pending item.
const (
	ActorFinance    = "FINANCE"
	ActorOperations = "OPERATIONS"
)

// The kinds of pending items, in the order the list gives them.
const (
	pendingInWork             = "IN_WORK"
	pendingFormerShare        = "FORMER_SHARE_PENDING"
	pendingToBeAllocated      = "TO_BE_ALLOCATED"
	pendingExceptionCause     = "EXCEPTION_CAUSE_PENDING"
	pendingReopenToAttribute  = "REOPEN_TO_ATTRIBUTE"
	pendingLineNotApportioned = "LINE_NOT_APPORTIONED"
	pendingLineNotOnBasis     = "LINE_NOT_ON_CURRENT_BASIS"
	pendingCostNotFinal       = "COST_NOT_FINAL"
	pendingInvoiceHeld        = "INVOICE_HELD"
)

var pendingOrder = []string{pendingInWork, pendingFormerShare, pendingToBeAllocated, pendingExceptionCause, pendingReopenToAttribute,
	pendingLineNotApportioned, pendingLineNotOnBasis, pendingCostNotFinal, pendingInvoiceHeld}

// PendingItem is one pending item of a pool. LineNo, ShipmentID and UnitNo say where it applies (zero or empty when it
// does not apply to a line, a house or a container); Amount and Local are the amount it concerns in the line's currency
// and in the functional currency (zero with no Currency when it concerns none); Message says what to do and Actor who
// does it.
type PendingItem struct {
	Kind       string
	LineNo     int
	ShipmentID string
	UnitNo     int
	Currency   kernel.CurrencyCode
	Amount     int64
	Local      int64
	Message    string
	Actor      string
}

// PendingItems lists what the pool needs, from PHY's basis as it is now, in a fixed order: by kind (a container in
// rework, a former share pending, an amount to be allocated, an exception cost whose party is pending, a closed pool to
// reopen, a line not apportioned, a line not on the current basis, a cost not final, an invoice held), then by line,
// then by house. A pool with nothing pending lists nothing. A former share of an open pool is FORMER_SHARE_PENDING and
// that of a closed pool REOPEN_TO_ATTRIBUTE, which says the same and the reopening first: a house is never listed twice
// for a line. While a container is in rework, and in a closed pool, the lines are not listed as not apportioned or not
// on the basis: no apportionment is made before the container is sealed or the pool reopened. The lines are read
// through lineRefusal and accrualRefusal, the close's own checks, so that the list is empty exactly when the pool can
// be closed, apart from an exception cost whose party is pending, which does not hold the close back. When the pool has
// a line by REVENUE the caller sets HouseRevenue first, as the close does.
func (p *ConsolCostPool) PendingItems(basis CostBasis) []PendingItem {
	return p.PendingItemsUnder(RuleSettings{}, basis)
}

// PendingItemsUnder is PendingItems under the settings in force (since V2.97): under F7 BLOCKS an exception cost whose
// party is pending holds back the close, and its item says so; under DOES_NOT_BLOCK it says it does not. The list is
// still empty exactly when the pool can be closed, apart from an item of that kind under DOES_NOT_BLOCK.
func (p *ConsolCostPool) PendingItemsUnder(rules RuleSettings, basis CostBasis) []PendingItem {
	closed := p.state.State == PoolClosed
	working := basis.UnitsInRework()
	var out []PendingItem
	for _, no := range working {
		out = append(out, PendingItem{Kind: pendingInWork, UnitNo: no, Actor: ActorOperations, Message: fmt.Sprintf(
			"%s is in rework, so no apportionment is previewed, submitted or closed: ask operations to seal the container again or to give its stuffing "+
				"up in PHY (OPERATIONS), then preview and submit the apportionments again (FINANCE)", unitName(basis, no))})
	}
	after := ""
	if len(working) > 0 {
		after = "once the container is sealed again or given up, "
	}
	houses, gone := housesInOrder(basis)
	functional := p.state.FunctionalCurrency
	for _, l := range p.state.Lines {
		if kind, err := p.accrualRefusal(l); err != nil {
			way := "match the creditor invoice as final, or release the accrual"
			if kind == pendingInvoiceHeld {
				way = "settle the dispute or approve the variance"
			}
			out = append(out, PendingItem{Kind: kind, LineNo: l.No, Actor: ActorFinance, Message: fmt.Sprintf("%s: %s (FINANCE); the close refuses the line until then",
				strings.TrimPrefix(err.Error(), ErrPoolNotClosable.Error()+": "), way)})
		}
		kind, err := p.lineRefusal(l, basis, houses, gone)
		if err == nil {
			continue
		}
		_, la, apportioned := p.SharesOfLine(l.No)
		currency := l.Versions[len(l.Versions)-1].Amount.Currency
		item := PendingItem{LineNo: l.No, Currency: currency, Actor: ActorFinance}
		detail := strings.TrimPrefix(err.Error(), ErrPoolNotClosable.Error()+": ")
		switch kind {
		case pendingToBeAllocated:
			item.Kind, item.Amount, item.Local = kind, la.ToAllocate, la.ToAllocateLocal
			item.Message = fmt.Sprintf("line %d has %s to be allocated, as no house of the consolidation is live: %sadd cargo and apportion the line again, or "+
				"attribute it as a loss or an exception cost of a house that left, or cancel the consolidation and retain the line (FINANCE)", l.No,
				money(currency, la.ToAllocate), after)
			out = append(out, item)
		case pendingFormerShare:
			for _, s := range formerSharesOf(la, basis) {
				out = append(out, p.formerShareItem(item, s, basis, closed, after))
			}
		case pendingLineNotApportioned, pendingLineNotOnBasis:
			if closed || len(working) > 0 {
				continue
			}
			item.Kind, item.Currency = kind, ""
			if cost, local := p.LineCost(l.No); kind == pendingLineNotApportioned && apportioned && (la.Amount != cost || la.Local != local) {
				detail = fmt.Sprintf("cost line %d was apportioned at %s (%s), and its final cost is now %s (%s)", l.No, money(currency, la.Amount),
					money(functional, la.Local)+" local", money(currency, cost), money(functional, local)+" local")
			}
			item.Message = fmt.Sprintf("%s: preview the line again and submit it (FINANCE); the close refuses it until then", detail)
			out = append(out, item)
		}
	}
	for _, l := range p.state.Lines {
		currency := l.Versions[len(l.Versions)-1].Amount.Currency
		for _, x := range p.pendingCauses(l.No) {
			way := "preview the line again stating the party for house " + x.ShipmentID + " as a correction, with its reason and a voucher, and submit it"
			if closed {
				way = "reopen the pool with a reason (ReopenConsolCostPool), then " + way
			}
			way = after + way
			hold := "this does not hold back the close (F7 DOES_NOT_BLOCK; under F7 BLOCKS it would)"
			if rules.F7 == F7Blocks {
				hold = "this holds back the close (F7 BLOCKS) until the party is established"
			}
			out = append(out, PendingItem{Kind: pendingExceptionCause, LineNo: l.No, ShipmentID: x.ShipmentID, Currency: currency, Amount: x.Exception,
				Local: x.ExceptionLocal, Actor: ActorFinance, Message: fmt.Sprintf(
					"line %d holds an exception cost of %s of house %s whose party is not established (voucher %s): once it is, %s (FINANCE); %s",
					l.No, money(currency, x.Exception), x.ShipmentID, x.Voucher, way, hold)})
		}
	}
	rank := func(kind string) int { return slices.Index(pendingOrder, kind) }
	slices.SortStableFunc(out, func(a, b PendingItem) int {
		if c := rank(a.Kind) - rank(b.Kind); c != 0 {
			return c
		}
		if c := a.UnitNo - b.UnitNo; c != 0 {
			return c
		}
		if c := a.LineNo - b.LineNo; c != 0 {
			return c
		}
		return cmpString(a.ShipmentID, b.ShipmentID)
	})
	return out
}

// formerShareItem is the item of one former share not attributed: FORMER_SHARE_PENDING in an open pool, and in a closed
// one REOPEN_TO_ATTRIBUTE. The message names who cannot confirm the attribution: the person who asked for the rework
// that unloaded the house and the one who ended its place in the consolidation (as the submission refuses them).
func (p *ConsolCostPool) formerShareItem(base PendingItem, s Share, basis CostBasis, closed bool, after string) PendingItem {
	f, _ := basis.Former(s.ShipmentID)
	base.ShipmentID, base.Amount, base.Local = s.ShipmentID, s.Amount, s.Local
	who := "Whoever ended its place in the consolidation (" + f.EndedBy + ")"
	if f.ReworkAskedBy != "" {
		who = "Whoever asked for the rework that unloaded the house (" + f.ReworkAskedBy + ") and whoever ended its place in the consolidation (" + f.EndedBy + ")"
	}
	way := "preview the line again with the house's loss and exception cost, a voucher and a reason, and submit it"
	base.Kind = pendingFormerShare
	if closed {
		base.Kind = pendingReopenToAttribute
		way = "reopen the pool with a reason (ReopenConsolCostPool), then " + way
	}
	base.Message = fmt.Sprintf("house %s left the consolidation (%s) and holds a former share of %s on line %d, which is the loss it bears unless it is "+
		"attributed otherwise; to attribute it, %s%s (FINANCE). %s cannot submit it.", s.ShipmentID, f.End, money(base.Currency, s.Amount), base.LineNo, after, way, who)
	return base
}

// money writes an amount in minor units with its currency's decimals and thousands separators, as "CNY 1,500.00" (the
// minor units when the kernel does not list the currency).
func money(currency kernel.CurrencyCode, minor int64) string {
	digits, ok := kernel.MinorDigits(currency)
	if !ok {
		return fmt.Sprintf("%s %d minor units", currency, minor)
	}
	sign, abs := "", minor
	if minor < 0 {
		sign, abs = "-", -minor
	}
	scale := int64(1)
	for i := 0; i < digits; i++ {
		scale *= 10
	}
	whole := fmt.Sprint(abs / scale)
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	if digits == 0 {
		return fmt.Sprintf("%s %s%s", currency, sign, whole)
	}
	return fmt.Sprintf("%s %s%s.%0*d", currency, sign, whole, digits, abs%scale)
}
