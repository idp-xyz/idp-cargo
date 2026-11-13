package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// RATE V0.5 part 2: PricingBasis v1 of purposes SHIPMENT_ESTIMATE and CONSOL_COST_ESTIMATE (RATE -> FIN). FIN asks
// RATE to rate a shipment or a consolidation; RATE evaluates, records the snapshot and issues a basis built from it.
// A basis is an estimate, never revenue or a payable; it never changes, and a new estimate is a new basis.

// BasisLine is a rated charge of a basis: per charge code the sell estimate (for the customer) and the cost estimate
// (with the supplier whose card priced it), each with the snapshot line it comes from. A side the evaluation did not
// rate is absent (HasSell or HasCost false), never zero.
type BasisLine struct {
	LineNo          int
	ChargeCode      ChargeCode
	HasSell         bool
	Sell            kernel.Money
	SellLineNo      int
	HasCost         bool
	Cost            kernel.Money
	CostLineNo      int
	CreditorPartyID string
}

// BasisGap is a charge the evaluation could not rate: FIN leaves it to a person.
type BasisGap struct {
	Side       Side
	ChargeCode ChargeCode
	Gap        Gap
}

// PricingBasisState is an issued basis.
type PricingBasisState struct {
	TenantID       TenantID
	BasisID        string
	Purpose        SnapshotPurpose
	LegalEntityID  LegalEntityID
	Customer       CustomerRef
	SubjectKind    string // SHIPMENT or CONSOLIDATION
	SubjectID      string
	SubjectVersion int64
	SnapshotID     string
	Lines          []BasisLine
	Gaps           []BasisGap
	IssuedBy       string
	IssuedAt       time.Time
}

// PricingBasis is the aggregate of an issued basis; it never changes.
type PricingBasis struct {
	state   PricingBasisState
	pending []Event
}

// EventPricingBasisIssued is published when a basis is issued.
const EventPricingBasisIssued = "PricingBasisIssued.v1"

var ErrInvalidBasis = errors.New("rate: invalid pricing basis")

// IssueBasis builds a basis from a recorded snapshot of purpose SHIPMENT_ESTIMATE (SELL and BUY, for a customer of a
// shipment) or CONSOL_COST_ESTIMATE (BUY only, for a consolidation): one line per charge code rated on either side,
// and the gaps.
func IssueBasis(id string, s *RateSnapshot) (*PricingBasis, error) {
	st := s.State()
	in := st.Input
	switch {
	case !identifier(id):
		return nil, fmt.Errorf("%w: a basis id", ErrInvalidBasis)
	case in.Purpose == PurposeShipmentEstimate && (in.SubjectKind != "SHIPMENT" || in.Customer.PartyID == "" || !slices.Contains(in.Sides, SideSell) ||
		!slices.Contains(in.Sides, SideBuy)):
		return nil, fmt.Errorf("%w: a shipment estimate rates both sides of a shipment for its customer", ErrInvalidBasis)
	case in.Purpose == PurposeConsolCostEstimate && (in.SubjectKind != "CONSOLIDATION" || in.Customer.PartyID != "" || len(in.Sides) != 1 ||
		in.Sides[0] != SideBuy):
		return nil, fmt.Errorf("%w: a consolidation cost estimate rates the BUY side of a consolidation", ErrInvalidBasis)
	case in.Purpose != PurposeShipmentEstimate && in.Purpose != PurposeConsolCostEstimate:
		return nil, fmt.Errorf("%w: purpose %s", ErrInvalidBasis, in.Purpose)
	}
	b := &PricingBasis{state: PricingBasisState{TenantID: st.TenantID, BasisID: id, Purpose: in.Purpose, LegalEntityID: in.LegalEntityID,
		Customer: in.Customer, SubjectKind: in.SubjectKind, SubjectID: in.SubjectID, SubjectVersion: in.SubjectVersion, SnapshotID: st.SnapshotID,
		IssuedBy: st.EvaluatedBy, IssuedAt: st.RecordedAt}}
	at := map[ChargeCode]int{}
	for _, l := range st.Lines {
		if l.Gap != GapNone {
			b.state.Gaps = append(b.state.Gaps, BasisGap{Side: l.Side, ChargeCode: l.ChargeCode, Gap: l.Gap})
			continue
		}
		i, ok := at[l.ChargeCode]
		if !ok {
			b.state.Lines = append(b.state.Lines, BasisLine{ChargeCode: l.ChargeCode})
			i = len(b.state.Lines) - 1
			at[l.ChargeCode] = i
		}
		bl := &b.state.Lines[i]
		if l.Side == SideSell {
			bl.HasSell, bl.Sell, bl.SellLineNo = true, l.Amount, l.LineNo
		} else {
			bl.HasCost, bl.Cost, bl.CostLineNo, bl.CreditorPartyID = true, l.Amount, l.LineNo, l.PartyID
		}
	}
	slices.SortFunc(b.state.Lines, func(x, y BasisLine) int { return strings.Compare(string(x.ChargeCode), string(y.ChargeCode)) })
	for i := range b.state.Lines {
		b.state.Lines[i].LineNo = i + 1
	}
	b.pending = append(b.pending, Event{Type: EventPricingBasisIssued, At: b.state.IssuedAt, TenantID: b.state.TenantID, SubjectID: id})
	return b, nil
}

// RestoreBasis rebuilds a stored basis.
func RestoreBasis(st PricingBasisState) *PricingBasis { return &PricingBasis{state: st} }

func (b *PricingBasis) State() PricingBasisState {
	st := b.state
	st.Lines, st.Gaps = slices.Clone(b.state.Lines), slices.Clone(b.state.Gaps)
	return st
}
func (b *PricingBasis) Pending() []Event { return append([]Event(nil), b.pending...) }
func (b *PricingBasis) Acknowledge()     { b.pending = nil }
