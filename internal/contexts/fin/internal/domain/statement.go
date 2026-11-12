package domain

import (
	"cmp"
	"fmt"
	"math/big"
	"slices"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// proportion is local × part / whole, rounded half up (away from zero): the local amount of the part of a note that
// is open. It is exact when the part is nothing or the whole.
func proportion(local, part, whole int64) int64 {
	if whole == 0 || part == 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(local), big.NewInt(part))
	n.Mul(n, big.NewInt(2))
	d := new(big.Int).Mul(big.NewInt(whole), big.NewInt(2))
	if (n.Sign() < 0) != (d.Sign() < 0) {
		n.Sub(n, new(big.Int).Abs(big.NewInt(whole)))
	} else {
		n.Add(n, new(big.Int).Abs(big.NewInt(whole)))
	}
	return n.Quo(n, d).Int64()
}

// Statement entry kinds.
const (
	EntryDebitNote        = "DEBIT_NOTE"
	EntryCreditNote       = "CREDIT_NOTE"
	EntryReceipt          = "RECEIPT"
	EntryReceiptCancelled = "RECEIPT_CANCELLED"
)

const maxStatementDays = 366

// StatementEntry is a line of a debtor statement: what a note or a receipt changes of what the debtor owes (a debit
// note adds; a credit note and a receipt take off; a cancelled receipt adds back), and the balance after it.
type StatementEntry struct {
	Date           time.Time
	Kind           string
	ID             string
	Number         string // a note's number; a receipt's evidence reference
	DocumentNumber string // a debit note's house document
	Amount         int64
	Balance        int64
}

// StatementItem is a debit note open at the end of a statement: its total, what credit notes and receipts took off by
// then, what is open and how many days past due.
type StatementItem struct {
	InvoiceID      string
	Number         string
	DocumentNumber string
	InvoiceDate    time.Time
	DueDate        time.Time
	Total          int64
	Credited       int64
	Settled        int64
	Open           int64
	DaysOverdue    int
}

// StatementReceipt is a receipt with money on account at the end of a statement.
type StatementReceipt struct {
	ReceiptID      string
	ReceivedOn     time.Time
	EvidenceSource string
	EvidenceRef    string
	Credited       int64
	Unallocated    int64
}

// Ageing sums open items by days past due at the end of a statement.
type Ageing struct {
	NotDue     int64
	Days1To30  int64
	Days31To60 int64
	Days61To90 int64
	Over90     int64
}

// CurrencyStatement is a debtor's statement in one currency: Closing = Opening + the entries, and also the open items
// less the money on account.
type CurrencyStatement struct {
	Currency  kernel.CurrencyCode
	Opening   int64
	Entries   []StatementEntry
	Closing   int64
	OpenItems []StatementItem
	OnAccount []StatementReceipt
	Ageing    Ageing
}

// Statement states a debtor's account with a legal entity from its notes and receipts, per currency and by business
// dates: notes count from their invoice date, receipts from the date received until the date cancelled, allocations
// from their date until the date reversed. from and to are civil dates, at most 366 days apart.
func Statement(notes []Note, receipts []ReceiptState, from, to time.Time) ([]CurrencyStatement, error) {
	if !date(from) || !date(to) || to.Before(from) || to.Sub(from) > maxStatementDays*24*time.Hour {
		return nil, fmt.Errorf("%w: a statement runs from a date to a date at most %d days later", ErrInvalid, maxStatementDays)
	}
	by := map[kernel.CurrencyCode]*CurrencyStatement{}
	of := func(c kernel.CurrencyCode) *CurrencyStatement {
		if by[c] == nil {
			by[c] = &CurrencyStatement{Currency: c}
		}
		return by[c]
	}
	var entries []struct {
		currency kernel.CurrencyCode
		StatementEntry
	}
	add := func(c kernel.CurrencyCode, e StatementEntry) {
		if !e.Date.After(to) {
			entries = append(entries, struct {
				currency kernel.CurrencyCode
				StatementEntry
			}{c, e})
		}
	}
	settled := map[string]int64{} // per debit note, live at the end
	for _, r := range receipts {
		ref := r.EvidenceSource + " " + r.EvidenceRef
		add(r.Currency, StatementEntry{Date: r.ReceivedOn, Kind: EntryReceipt, ID: r.ReceiptID, Number: ref, Amount: -r.Credited()})
		if r.State == ReceiptCancelled {
			add(r.Currency, StatementEntry{Date: r.CancelledOn, Kind: EntryReceiptCancelled, ID: r.ReceiptID, Number: ref, Amount: r.Credited()})
		}
		if r.ReceivedOn.After(to) || (r.State == ReceiptCancelled && !r.CancelledOn.After(to)) {
			continue
		}
		left := r.Credited()
		for _, a := range r.Allocations {
			if a.liveOn(to) {
				left -= a.Amount
				settled[a.InvoiceID] += a.Amount
			}
		}
		if left != 0 {
			s := of(r.Currency)
			s.OnAccount = append(s.OnAccount, StatementReceipt{ReceiptID: r.ReceiptID, ReceivedOn: r.ReceivedOn, EvidenceSource: r.EvidenceSource,
				EvidenceRef: r.EvidenceRef, Credited: r.Credited(), Unallocated: left})
		}
	}
	credited := map[string]int64{} // per debit note, by credit notes dated by the end
	for _, n := range notes {
		kind := EntryDebitNote
		if n.Kind == KindCreditNote {
			kind = EntryCreditNote
			if !n.InvoiceDate.After(to) {
				credited[n.CreditsID] -= n.Total
			}
		}
		add(n.Currency, StatementEntry{Date: n.InvoiceDate, Kind: kind, ID: n.ID, Number: n.Number, DocumentNumber: n.Document.Number, Amount: n.Total})
	}
	for _, n := range notes {
		if n.Kind != KindDebitNote || n.InvoiceDate.After(to) {
			continue
		}
		item := StatementItem{InvoiceID: n.ID, Number: n.Number, DocumentNumber: n.Document.Number, InvoiceDate: n.InvoiceDate, DueDate: n.DueDate,
			Total: n.Total, Credited: credited[n.ID], Settled: settled[n.ID]}
		if item.Open = item.Total - item.Credited - item.Settled; item.Open == 0 {
			continue
		}
		s := of(n.Currency)
		if to.After(n.DueDate) {
			item.DaysOverdue = int(to.Sub(n.DueDate) / (24 * time.Hour))
		}
		switch d := item.DaysOverdue; {
		case d == 0:
			s.Ageing.NotDue += item.Open
		case d <= 30:
			s.Ageing.Days1To30 += item.Open
		case d <= 60:
			s.Ageing.Days31To60 += item.Open
		case d <= 90:
			s.Ageing.Days61To90 += item.Open
		default:
			s.Ageing.Over90 += item.Open
		}
		s.OpenItems = append(s.OpenItems, item)
	}
	order := map[string]int{EntryDebitNote: 0, EntryCreditNote: 1, EntryReceipt: 2, EntryReceiptCancelled: 3}
	slices.SortStableFunc(entries, func(a, b struct {
		currency kernel.CurrencyCode
		StatementEntry
	}) int {
		return cmp.Or(a.Date.Compare(b.Date), cmp.Compare(order[a.Kind], order[b.Kind]), cmp.Compare(a.Number, b.Number), cmp.Compare(a.ID, b.ID))
	})
	for _, e := range entries {
		s := of(e.currency)
		if e.Date.Before(from) {
			s.Opening += e.Amount
			s.Closing = s.Opening
			continue
		}
		s.Closing += e.Amount
		e.Balance = s.Closing
		s.Entries = append(s.Entries, e.StatementEntry)
	}
	out := make([]CurrencyStatement, 0, len(by))
	for _, s := range by {
		slices.SortFunc(s.OpenItems, func(a, b StatementItem) int {
			return cmp.Or(a.DueDate.Compare(b.DueDate), cmp.Compare(a.Number, b.Number))
		})
		slices.SortFunc(s.OnAccount, func(a, b StatementReceipt) int {
			return cmp.Or(a.ReceivedOn.Compare(b.ReceivedOn), cmp.Compare(a.ReceiptID, b.ReceiptID))
		})
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b CurrencyStatement) int { return cmp.Compare(a.Currency, b.Currency) })
	return out, nil
}
