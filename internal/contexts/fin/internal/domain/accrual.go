package domain

import (
	"fmt"
	"math/big"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V0.5 (FB4): accruals and what creditor invoices consume of them. A job's cost side and a pool's cost line are
// accruals: estimates of what a creditor will invoice. A creditor invoice's matches consume them; the accrual's own
// versions record only estimates and their release, while what is matched is read from the live matches of creditor
// invoices not cancelled (one aggregate per transaction; the database checks both sides at commit).

// Accrual statuses beyond ACCRUED: PARTLY_MATCHED while live non-final matches consumed part of it and the rest stays
// accrued (carried forward), MATCHED once a live final match closed it, RELEASED once a person released what was left
// with a reason (a version of the accrual with amount zero).
const (
	StatusPartlyMatched = "PARTLY_MATCHED"
	StatusMatched       = "MATCHED"
	StatusReleased      = "RELEASED"
)

// Accrual kinds a creditor invoice matches: a job's cost side or a consol cost pool's cost line.
const (
	TargetJobCost  = "JOB_COST"
	TargetPoolCost = "POOL_COST"
)

var (
	ErrMatchCurrency    = fmt.Errorf("fin: FIN_MATCH_CURRENCY: the accrual is in another currency than the creditor invoice; revise the accrual into the invoice's currency first")
	ErrMatchCreditor    = fmt.Errorf("fin: FIN_MATCH_CREDITOR: the accrual names another creditor than the invoice's")
	ErrMatchTarget      = fmt.Errorf("fin: FIN_MATCH_TARGET: not an accrual of an open job or pool of the invoice's legal entity")
	ErrAccrualClosed    = fmt.Errorf("fin: FIN_ACCRUAL_CLOSED: the accrual is matched with a final match or released; it takes no further match or revision")
	ErrMatchExceeds     = fmt.Errorf("fin: FIN_MATCH_EXCEEDS: a match that is not final consumes no more than the accrual has open; mark it final or revise the accrual")
	ErrMatchIncomplete  = fmt.Errorf("fin: FIN_MATCH_INCOMPLETE: every line of the creditor invoice is matched in full, each accrual once")
	ErrMatchChargeCode  = fmt.Errorf("fin: FIN_MATCH_CHARGE_CODE: a match to an accrual of another charge code needs a reason")
	ErrInvoiceNumber    = fmt.Errorf("fin: FIN_SUPPLIER_INVOICE_USED: a creditor invoice of the legal entity and creditor not cancelled has this supplier invoice number")
	ErrVarianceApproval = fmt.Errorf("fin: FIN_VARIANCE_APPROVAL: the variance needs approval by another person than the one who matched, or by one allowed to approve their own")
	ErrAccrualMatched   = fmt.Errorf("fin: FIN_ACCRUAL_MATCHED: creditor invoices consume more of the accrual than the change would leave; cancel or dispute them first")
	ErrPoolNotClosable  = fmt.Errorf("fin: FIN_POOL_NOT_CLOSABLE: every cost line is matched with a final match or released, by invoices not disputed and approved where needed, and apportioned on the current loaded basis at its final cost")
	ErrTargetClosed     = fmt.Errorf("fin: FIN_TARGET_CLOSED: an accrual the creditor invoice matched belongs to a pool or job that is not open; reopen it first")
	ErrFinalBehind      = fmt.Errorf("fin: FIN_FINAL_MATCH_BEHIND: another creditor invoice closed an accrual this one matched in part, and its variance counted this one; dispute and cancel that invoice first, then match it again")
)

// Consumption is what the live matches of creditor invoices not cancelled consumed of one accrual: the amount in the
// accrual's currency, the local amount at their ACCOUNTING rates, whether one of them is final, whether one comes from
// an invoice disputed or with a variance not yet approved (Held), and the invoices' creditors.
type Consumption struct {
	Amount    int64
	Local     int64
	Final     bool
	Held      bool
	Creditors []string
}

// names reports whether an accrual consumed so may name a creditor: none, or the creditor of every matching invoice.
func (c Consumption) names(party string) bool {
	for _, x := range c.Creditors {
		if party != "" && x != party {
			return false
		}
	}
	return true
}

// AccrualStatus is an accrual's status from its latest version's status (ACCRUED or RELEASED) and what is matched.
func AccrualStatus(versionStatus string, c Consumption) string {
	switch {
	case versionStatus == StatusReleased:
		return StatusReleased
	case c.Final:
		return StatusMatched
	case c.Amount > 0:
		return StatusPartlyMatched
	}
	return StatusAccrued
}

// Open is what the accrual still expects to be invoiced, in its currency: nothing once matched final or released.
func Open(amount int64, versionStatus string, c Consumption) int64 {
	if versionStatus == StatusReleased || c.Final || c.Amount >= amount {
		return 0
	}
	return amount - c.Amount
}

// CurrentCost is an accrual's cost now, in its currency and in the functional currency: what live matches consumed,
// at their ACCOUNTING rates, plus what is still open of the latest version, at that version's conversion (half up per
// accrual, as a note's open part). Once matched final or released it is what was matched.
func CurrentCost(amount, local int64, versionStatus string, c Consumption) (int64, int64) {
	if versionStatus == StatusReleased || c.Final {
		return c.Amount, c.Local
	}
	open := Open(amount, versionStatus, c)
	return c.Amount + open, c.Local + proportion(local, open, amount)
}

// AccrualTarget is what a match needs to know of an accrual, read under the lock of the job or pool it belongs to:
// the aggregate's legal entity and whether it is open, the accrual's charge code, its latest version (creditor,
// amount, local amount and status) and what live matches consumed of it.
type AccrualTarget struct {
	Kind            string
	ID              string
	LineNo          int
	LegalEntityID   string
	Open            bool
	ChargeCode      string
	SupplierRef     string
	Reference       string // the job's shipment or the pool's consolidation
	CreditorPartyID string
	Version         int
	Amount          kernel.Money
	Local           int64
	Status          string
	Consumed        Consumption
}

// Key identifies an accrual.
func (t AccrualTarget) Key() TargetKey { return TargetKey{Kind: t.Kind, ID: t.ID, LineNo: t.LineNo} }

// TargetKey names an accrual: a job's cost side or a pool's cost line.
type TargetKey struct {
	Kind   string
	ID     string
	LineNo int
}

func (k TargetKey) String() string { return fmt.Sprintf("%s %s line %d", k.Kind, k.ID, k.LineNo) }

// VarianceThreshold is a version of a legal entity's cost variance threshold (OD-9 B): a final match whose variance
// against the accrual, in the functional currency, is beyond the greater of Amount and RateBP basis points of the
// accrual's local amount needs approval. Without a threshold every variance does.
type VarianceThreshold struct {
	No        int
	Amount    int64
	RateBP    int
	By        string
	At        time.Time
	EnteredAt time.Time
}

// Exceeded reports whether a variance needs approval under the threshold (nil: none set, so any variance does).
func Exceeded(t *VarianceThreshold, variance, accrualLocal int64) bool {
	if variance == 0 {
		return false
	}
	if t == nil {
		return true
	}
	v := new(big.Int).Abs(big.NewInt(variance))
	limit := new(big.Int).Mul(new(big.Int).Abs(big.NewInt(accrualLocal)), big.NewInt(int64(t.RateBP)))
	limit.Quo(limit, big.NewInt(10_000))
	if amount := big.NewInt(t.Amount); amount.Cmp(limit) > 0 {
		limit = amount
	}
	return v.Cmp(limit) > 0
}
