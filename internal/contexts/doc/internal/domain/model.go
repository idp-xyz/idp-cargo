// Package domain is the model of DOC (Transport Documents) V0.1, the N1 air line: the house air waybill (HAWB)
// and the master air waybill (MAWB) of an air consolidation, built from AIR's finalized facts (AirDocFacts v1),
// the parties COM froze on the orders (HAWB) or the issuing legal entity and the destination agent ORG knows
// (MAWB). A document is a numbered series of immutable versions; each is drafted, approved and issued, and a
// correction after issue is the next version (amendment). It depends on the standard library and the kernel only.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

type TenantID string
type DocumentID string

var (
	ErrInvalidDocument   = errors.New("doc: invalid transport document")
	ErrInvalidTransition = errors.New("doc: invalid state transition")
	ErrInvalidChange     = errors.New("doc: invalid change")
	ErrConflict          = errors.New("doc: concurrent revision conflict")
	ErrSourceNotFinal    = errors.New("doc: DOC_SOURCE_NOT_FINAL: AIR's latest AirDocFacts version is not FINAL; AIR finalizes the movement's facts first")
	ErrSourceStale       = errors.New("doc: DOC_SOURCE_STALE: the version was built on facts or parties that changed since; refresh it")
	ErrMasterMismatch    = errors.New("doc: DOC_MASTER_MISMATCH: AIR's facts no longer show the shipment under the master")
	ErrPartiesDiverge    = errors.New("doc: DOC_PARTIES_DIVERGE: the orders of the house name different shippers, consignees or notify parties")
	ErrIncomplete        = errors.New("doc: DOC_INCOMPLETE: the document lacks what its kind needs")
	ErrNotLocked         = errors.New("doc: DOC_PARTIES_NOT_LOCKED: COM does not report the order's parties locked for the document (C12)")
	ErrCreditHold        = errors.New("doc: DOC_CREDIT_HOLD: FIN holds the ordering party's credit (C7)")
	ErrCreditUndecided   = errors.New("doc: DOC_CREDIT_UNDECIDED: FIN has no credit decision; issuing needs a credit override with a reason (C7, OD-3)")
	ErrIssuanceBlocked   = errors.New("doc: DOC_ISSUANCE_BLOCKED: the legal entity holds no issuance authority in force for the kind, place and airline (OD-10)")
	ErrIssuerNotVerified = errors.New("doc: the issuing legal entity is not VERIFIED in ORG")
	ErrInvalidAuthority  = errors.New("doc: invalid issuance authority")
	ErrNumberIssued      = errors.New("doc: DOC_NUMBER_ISSUED: another shipment or movement of the legal entity was issued the number; a number is issued for one of them only (G08-04)")
)

// Kind is a document kind; DOC V0.1 issues air waybills only.
type Kind string

const (
	KindHAWB Kind = "HAWB"
	KindMAWB Kind = "MAWB"
)

func (k Kind) valid() bool { return k == KindHAWB || k == KindMAWB }

// Capacity is the capacity the issuer signs in: consolidator for a HAWB, agent of the airline for a MAWB the
// forwarder executes.
func (k Kind) Capacity() string {
	if k == KindMAWB {
		return "AGENT_OF_CARRIER"
	}
	return "CONSOLIDATOR"
}

// Release type of air waybills: non-negotiable, released to the named consignee.
const ReleaseConsigneeNamed = "CONSIGNEE_NAMED"

// Payment indicators of the freight on the document.
const (
	PaymentPrepaid = "PREPAID"
	PaymentCollect = "COLLECT"
)

const maxText = 500

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func text(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= maxText }

func identifier(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= 128 }

// Digest is the SHA-256 of a version's content bytes, in hexadecimal.
func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}
