package application

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
)

// RATE V0.5: the charge-code catalogue, rate cards and rate evaluation. As for quotes, ORG and the location
// reference data answer before RATE opens its transaction (T3); one transaction then writes the aggregate, its
// Outbox events and the command receipt (T2).

var (
	ErrPartyNotConfirmed = errors.New("rate application: RATE_PARTY_ROLE: ORG does not confirm the party in a role the card needs")
	ErrLocationRefused   = errors.New("rate application: RATE_LOCATION: the location reference data does not resolve the location for the mode")
	ErrValidFromTooEarly = errors.New("rate application: a version may not start before the record window")
)

// Supplier roles a SUPPLIER card's party is confirmed in, and the recipient roles a quote goes to (R7), in which a
// CUSTOMER card's party and an evaluation's customer are confirmed.
var (
	SupplierRoles = []string{"CARRIER", "CO_LOADER", "OVERSEAS_AGENT"}
	CustomerRoles = []string{"CUSTOMER", "OVERSEAS_AGENT", "SHIPPER", "CONSIGNEE"}
)

// Parties confirms with ORG that a party holds one of the roles for the legal entity at an instant, and answers
// its revision. It is called outside RATE's transactions.
type Parties interface {
	ConfirmRole(ctx context.Context, tenant rate.TenantID, party string, entity rate.LegalEntityID, roles []string, at time.Time) (int64, error)
}

// Locations resolves a UN/LOCODE, or an alias carriers use, for a mode against the current edition of the location
// reference data, and answers the UN/LOCODE and the edition. It is called outside RATE's transactions.
type Locations interface {
	Resolve(ctx context.Context, mode rate.Mode, code string) (string, string, error)
}

// ChargeCodeStore reads and writes the tenant's catalogue: Load answers the latest stored version or the starter
// entry; Insert writes the version added, refusing a second writer of the same version (rate.ErrConflict).
type ChargeCodeStore interface {
	Load(ctx context.Context, tenant rate.TenantID, code rate.ChargeCode) (*rate.ChargeCodeAggregate, error)
	Insert(ctx context.Context, c *rate.ChargeCodeAggregate) error
	List(ctx context.Context, tenant rate.TenantID) ([]rate.ChargeCodeEntry, error)
}

// CardFilter selects cards to list; empty fields select all. ValidAt keeps cards with a version published then.
type CardFilter struct {
	Side          rate.Side
	Scope         rate.CardScope
	LegalEntityID rate.LegalEntityID
	PartyID       string
	ValidAt       time.Time
}

// RateCardStore reads and writes cards. Load locks the card row and reads the entries of its DRAFT version.
type RateCardStore interface {
	Load(ctx context.Context, tenant rate.TenantID, id rate.RateCardID) (*rate.RateCard, int64, error)
	Read(ctx context.Context, tenant rate.TenantID, id rate.RateCardID) (*rate.RateCard, int64, error)
	Entries(ctx context.Context, tenant rate.TenantID, id rate.RateCardID, version int) ([]rate.RateEntry, error)
	Insert(ctx context.Context, c *rate.RateCard) (int64, error)
	Save(ctx context.Context, c *rate.RateCard, expected int64) (int64, error)
	List(ctx context.Context, tenant rate.TenantID, f CardFilter) ([]rate.RateCardState, []int64, error)
	Published(ctx context.Context, tenant rate.TenantID, entity rate.LegalEntityID, at time.Time, lane Lane) ([]rate.PublishedVersion, error)
}

// Lane selects the entries an evaluation can use: mode, container mode, origin and destination.
type Lane struct {
	Mode          rate.Mode
	ContainerMode rate.ContainerMode
	Origin        string
	Destination   string
}

// SnapshotStore writes and reads rate snapshots.
type SnapshotStore interface {
	Insert(ctx context.Context, s *rate.RateSnapshot) error
	Read(ctx context.Context, tenant rate.TenantID, id string) (*rate.RateSnapshot, error)
}

// BasisStore writes and reads issued pricing bases.
type BasisStore interface {
	Insert(ctx context.Context, b *rate.PricingBasis) error
	Read(ctx context.Context, tenant rate.TenantID, id string) (*rate.PricingBasis, error)
}

// CardDeps are the ports of the catalogue, card, rating and estimate commands.
type CardDeps struct {
	Deps
	Codes     ChargeCodeStore
	Cards     RateCardStore
	Snapshots SnapshotStore
	Bases     BasisStore
	Parties   Parties
	Locations Locations
}

type pendingAggregate interface {
	Pending() []rate.Event
	Acknowledge()
}

// begin checks the command, answers a stored receipt for a replay, and admits its business time (T1).
func (d CardDeps) begin(ctx context.Context, s Scope, commandID string, occurredAt time.Time, command any) (string, time.Time, Receipt, bool, error) {
	if err := d.check(ctx, s, commandID, occurredAt); err != nil {
		return "", time.Time{}, Receipt{}, false, err
	}
	hash, err := requestHash(command)
	if err != nil {
		return "", time.Time{}, Receipt{}, false, err
	}
	if r, ok, err := d.replay(ctx, s.TenantID, commandID, hash); err != nil || ok {
		return "", time.Time{}, r, ok, err
	}
	recorded, err := d.admit(occurredAt)
	return hash, recorded, Receipt{}, false, err
}

// commit writes what mutate changed, its Outbox events and the command receipt in one transaction; a receipt stored
// meanwhile by a concurrent replay wins.
func (d CardDeps) commit(ctx context.Context, s Scope, commandID, hash, kind string,
	mutate func(context.Context) (string, int64, pendingAggregate, error)) (Receipt, error) {
	var result Receipt
	var changed pendingAggregate
	err := d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		r, ok, err := d.Receipts.Find(txCtx, s.TenantID, commandID)
		if err != nil {
			return err
		}
		if ok {
			if r.RequestHash != hash {
				return ErrIdempotencyConflict
			}
			result = r
			return nil
		}
		subject, revision, agg, err := mutate(txCtx)
		if err != nil {
			return err
		}
		for _, e := range agg.Pending() {
			if err = d.Outbox.EnqueueRATEEvent(txCtx, e); err != nil {
				return err
			}
		}
		result = Receipt{CommandID: commandID, Kind: kind, SubjectID: subject, NewRevision: revision, RequestHash: hash}
		if err = d.Receipts.Save(txCtx, s.TenantID, result); err != nil {
			return err
		}
		changed = agg
		return nil
	})
	if err != nil {
		return Receipt{}, err
	}
	if changed != nil {
		changed.Acknowledge()
	}
	return result, nil
}

func (d CardDeps) allow(ctx context.Context, s Scope, action QuoteAction, subject string) error {
	if err := d.Auth.RequireQuoteAction(ctx, s, action, rate.QuoteID(subject)); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return nil
}

// ChargeCodeCommand defines, revises, deactivates or reactivates a code; Version is the version expected (0 for a
// starter code the tenant has not changed), ignored when defining.
type ChargeCodeCommand struct {
	CommandID  string
	Scope      Scope
	Action     string // DEFINE, REVISE, DEACTIVATE or REACTIVATE
	Code       rate.ChargeCode
	Version    int
	Input      rate.ChargeCodeInput
	OccurredAt time.Time
}

// ChangeChargeCode runs a catalogue command.
func (d CardDeps) ChangeChargeCode(ctx context.Context, c ChargeCodeCommand) (Receipt, error) {
	if d.Codes == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, recorded, stored, replayed, err := d.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.commit(ctx, c.Scope, c.CommandID, hash, "ChargeCode", func(txCtx context.Context) (string, int64, pendingAggregate, error) {
		if err := d.allow(txCtx, c.Scope, ActionMaintainChargeCodes, string(c.Code)); err != nil {
			return "", 0, nil, err
		}
		code, err := d.Codes.Load(txCtx, c.Scope.TenantID, c.Code)
		if err != nil {
			return "", 0, nil, err
		}
		switch c.Action {
		case "DEFINE":
			err = code.Define(c.Input, c.Scope.ActorID, recorded)
		case "REVISE":
			err = code.Revise(c.Version, c.Input, c.Scope.ActorID, recorded)
		case "DEACTIVATE":
			err = code.Deactivate(c.Version, c.Scope.ActorID, recorded)
		case "REACTIVATE":
			err = code.Reactivate(c.Version, c.Scope.ActorID, recorded)
		default:
			err = fmt.Errorf("%w: catalogue action %q", rate.ErrInvalidChargeCode, c.Action)
		}
		if err != nil {
			return "", 0, nil, err
		}
		if err = d.Codes.Insert(txCtx, code); err != nil {
			return "", 0, nil, err
		}
		return string(c.Code), int64(code.Version()), code, nil
	})
}

// CreateRateCardCommand opens a card. ORG confirms a CUSTOMER card's customer in the CUSTOMER role and a SUPPLIER
// card's supplier in a supplier role, for the legal entity at OccurredAt.
type CreateRateCardCommand struct {
	CommandID  string
	Scope      Scope
	Card       rate.RateCardDraft
	OccurredAt time.Time
}

func maintain(side rate.Side) QuoteAction {
	if side == rate.SideBuy {
		return ActionMaintainBuyRates
	}
	return ActionMaintainSellRates
}

func (d CardDeps) CreateRateCard(ctx context.Context, c CreateRateCardCommand) (Receipt, error) {
	if d.Cards == nil || d.Parties == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, recorded, stored, replayed, err := d.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	draft := c.Card
	draft.TenantID, draft.PartyRevision = c.Scope.TenantID, 0
	if err = d.allow(ctx, c.Scope, maintain(draft.Side), string(draft.RateCardID)); err != nil {
		return Receipt{}, err
	}
	switch draft.Scope {
	case rate.ScopeCustomer:
		draft.PartyRevision, err = d.Parties.ConfirmRole(ctx, c.Scope.TenantID, draft.PartyID, draft.LegalEntityID, CustomerRoles, c.OccurredAt)
	case rate.ScopeSupplier:
		draft.PartyRevision, err = d.Parties.ConfirmRole(ctx, c.Scope.TenantID, draft.PartyID, draft.LegalEntityID, SupplierRoles, c.OccurredAt)
	}
	if err != nil {
		return Receipt{}, err
	}
	return d.commit(ctx, c.Scope, c.CommandID, hash, "RateCard", func(txCtx context.Context) (string, int64, pendingAggregate, error) {
		if err := d.allow(txCtx, c.Scope, maintain(draft.Side), string(draft.RateCardID)); err != nil {
			return "", 0, nil, err
		}
		card, err := rate.NewRateCard(draft, c.Scope.ActorID, recorded)
		if err != nil {
			return "", 0, nil, err
		}
		revision, err := d.Cards.Insert(txCtx, card)
		return string(draft.RateCardID), revision, card, err
	})
}

// changeCard loads the card under its lock at the expected revision, checks the actor may maintain its side, applies
// the change and saves it.
func (d CardDeps) changeCard(ctx context.Context, s Scope, commandID string, occurredAt time.Time, command any, id rate.RateCardID, expected int64,
	prepare func(context.Context, rate.RateCardState) error, apply func(context.Context, *rate.RateCard, time.Time) error) (Receipt, error) {
	if d.Cards == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, recorded, stored, replayed, err := d.begin(ctx, s, commandID, occurredAt, command)
	if err != nil || replayed {
		return stored, err
	}
	card, _, err := d.readCard(ctx, s.TenantID, id)
	if err != nil {
		return Receipt{}, err
	}
	seen := card.State()
	if err = d.allow(ctx, s, maintain(seen.Side), string(id)); err != nil {
		return Receipt{}, err
	}
	if prepare != nil {
		if err = prepare(ctx, seen); err != nil {
			return Receipt{}, err
		}
	}
	return d.commit(ctx, s, commandID, hash, "RateCard", func(txCtx context.Context) (string, int64, pendingAggregate, error) {
		card, revision, err := d.Cards.Load(txCtx, s.TenantID, id)
		if err != nil {
			return "", 0, nil, err
		}
		if revision != expected {
			return "", 0, nil, rate.ErrConflict
		}
		st := card.State()
		if st.PartyID != seen.PartyID || st.PartyRevision != seen.PartyRevision {
			return "", 0, nil, rate.ErrConflict
		}
		if err = d.allow(txCtx, s, maintain(st.Side), string(id)); err != nil {
			return "", 0, nil, err
		}
		if err = apply(txCtx, card, recorded); err != nil {
			return "", 0, nil, err
		}
		revision, err = d.Cards.Save(txCtx, card, revision)
		return string(id), revision, card, err
	})
}

func (d CardDeps) readCard(ctx context.Context, tenant rate.TenantID, id rate.RateCardID) (*rate.RateCard, int64, error) {
	var card *rate.RateCard
	var revision int64
	err := d.Tx.WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		card, revision, err = d.Cards.Read(txCtx, tenant, id)
		return err
	})
	return card, revision, err
}

// DraftVersionCommand opens the next version of a card, copying the entries of version CopyFrom when it is not 0.
type DraftVersionCommand struct {
	CommandID        string
	Scope            Scope
	RateCardID       rate.RateCardID
	ExpectedRevision int64
	Version          rate.VersionInput
	CopyFrom         int
	OccurredAt       time.Time
}

func (d CardDeps) DraftRateCardVersion(ctx context.Context, c DraftVersionCommand) (Receipt, error) {
	return d.changeCard(ctx, c.Scope, c.CommandID, c.OccurredAt, c, c.RateCardID, c.ExpectedRevision, nil,
		func(txCtx context.Context, card *rate.RateCard, at time.Time) error {
			var copied []rate.RateEntry
			if c.CopyFrom != 0 {
				if !slices.ContainsFunc(card.State().Versions, func(v rate.RateCardVersion) bool { return v.No == c.CopyFrom }) {
					return fmt.Errorf("%w: card %s has no version %d to copy", rate.ErrInvalidRateCard, c.RateCardID, c.CopyFrom)
				}
				var err error
				if copied, err = d.Cards.Entries(txCtx, c.Scope.TenantID, c.RateCardID, c.CopyFrom); err != nil {
					return err
				}
			}
			_, err := card.DraftVersion(c.Version, copied, c.Scope.ActorID, at)
			return err
		})
}

// SetEntriesCommand replaces the entries of a DRAFT version. Locations are resolved for each entry's mode against
// the location reference data before the transaction; the entries keep the UN/LOCODEs and the edition.
type SetEntriesCommand struct {
	CommandID        string
	Scope            Scope
	RateCardID       rate.RateCardID
	ExpectedRevision int64
	Version          int
	Entries          []rate.RateEntry
	OccurredAt       time.Time
}

func (d CardDeps) SetRateCardEntries(ctx context.Context, c SetEntriesCommand) (Receipt, error) {
	if d.Locations == nil {
		return Receipt{}, ErrMissingDependency
	}
	entries := slices.Clone(c.Entries)
	resolve := func(ctx context.Context, _ rate.RateCardState) error {
		for i := range entries {
			e := &entries[i]
			edition := ""
			for _, code := range []*string{&e.Origin, &e.Destination, &e.Via} {
				if *code == "" {
					continue
				}
				resolved, ed, err := d.Locations.Resolve(ctx, e.Mode, *code)
				if err != nil {
					return fmt.Errorf("entry %d: %w", e.No, err)
				}
				if edition != "" && ed != edition {
					return fmt.Errorf("%w: entry %d resolved against two editions", ErrLocationRefused, e.No)
				}
				*code, edition = resolved, ed
			}
			e.Edition = edition
		}
		return nil
	}
	return d.changeCard(ctx, c.Scope, c.CommandID, c.OccurredAt, c, c.RateCardID, c.ExpectedRevision, resolve,
		func(_ context.Context, card *rate.RateCard, at time.Time) error {
			return card.SetEntries(c.Version, entries, c.Scope.ActorID, at)
		})
}

// PublishVersionCommand publishes a DRAFT version. A SELL tariff is published by the pricing manager
// (ActionApproveTariff), who approves it; one who prepared the version approves it only with ActionApproveOwnTariff,
// flagged (R3). ORG confirms a SUPPLIER card's party in a supplier role at OccurredAt.
type PublishVersionCommand struct {
	CommandID        string
	Scope            Scope
	RateCardID       rate.RateCardID
	ExpectedRevision int64
	Version          int
	OccurredAt       time.Time
}

func (d CardDeps) PublishRateCardVersion(ctx context.Context, c PublishVersionCommand) (Receipt, error) {
	if d.Codes == nil || d.Parties == nil {
		return Receipt{}, ErrMissingDependency
	}
	confirm := func(ctx context.Context, st rate.RateCardState) error {
		if st.Scope != rate.ScopeSupplier {
			return nil
		}
		_, err := d.Parties.ConfirmRole(ctx, st.TenantID, st.PartyID, st.LegalEntityID, SupplierRoles, c.OccurredAt)
		return err
	}
	return d.changeCard(ctx, c.Scope, c.CommandID, c.OccurredAt, c, c.RateCardID, c.ExpectedRevision, confirm,
		func(txCtx context.Context, card *rate.RateCard, at time.Time) error {
			st := card.State()
			p := rate.Publication{By: c.Scope.ActorID, At: at}
			for _, v := range st.Versions {
				if v.No != c.Version {
					continue
				}
				if earliest := at.Add(-d.Window.MaxBackdate); v.Validity.From.Before(earliest) {
					return fmt.Errorf("%w: version %d starts %s, before %s", ErrValidFromTooEarly, v.No, v.Validity.From.Format(time.RFC3339),
						earliest.Format(time.RFC3339))
				}
				if st.Scope == rate.ScopeTariff {
					if err := d.allow(txCtx, c.Scope, ActionApproveTariff, string(c.RateCardID)); err != nil {
						return err
					}
					p.ApprovedBy, p.SelfApprove = c.Scope.ActorID, slices.Contains(v.Preparers, c.Scope.ActorID)
					if p.SelfApprove {
						if err := d.allow(txCtx, c.Scope, ActionApproveOwnTariff, string(c.RateCardID)); err != nil {
							return err
						}
					}
				}
			}
			codes, err := d.Codes.List(txCtx, c.Scope.TenantID)
			if err != nil {
				return err
			}
			return card.Publish(c.Version, listed(codes), p)
		})
}

type listed []rate.ChargeCodeEntry

func (l listed) ChargeCode(code rate.ChargeCode) (rate.ChargeCodeVersion, bool) {
	for _, e := range l {
		if e.Code == code {
			return e.Current, true
		}
	}
	return rate.ChargeCodeVersion{}, false
}

func (listed) ServiceLevelListed(code string) bool {
	_, ok := rate.ListedServiceLevel(code)
	return ok
}

// WithdrawVersionCommand withdraws a PUBLISHED version from the time recorded.
type WithdrawVersionCommand struct {
	CommandID        string
	Scope            Scope
	RateCardID       rate.RateCardID
	ExpectedRevision int64
	Version          int
	Reason           string
	OccurredAt       time.Time
}

func (d CardDeps) WithdrawRateCardVersion(ctx context.Context, c WithdrawVersionCommand) (Receipt, error) {
	return d.changeCard(ctx, c.Scope, c.CommandID, c.OccurredAt, c, c.RateCardID, c.ExpectedRevision, nil,
		func(_ context.Context, card *rate.RateCard, at time.Time) error {
			return card.Withdraw(c.Version, c.Reason, c.Scope.ActorID, at)
		})
}

// EvaluateRatesCommand rates a lane and cargo at OccurredAt and records the snapshot. ORG confirms the customer in
// the CUSTOMER role when one is named; locations are resolved for the mode first.
type EvaluateRatesCommand struct {
	CommandID  string
	Scope      Scope
	SnapshotID string
	Input      rate.EvaluationInput
	OccurredAt time.Time
}

func (d CardDeps) EvaluateRates(ctx context.Context, c EvaluateRatesCommand) (Receipt, error) {
	return d.evaluate(ctx, c.Scope, c.CommandID, c.OccurredAt, c, c.SnapshotID, c.Input, "")
}

// evaluate rates the input at the command's business time and records the snapshot, and, when basisID is given, issues
// the pricing basis built from it in the same transaction. The Authorizer answers first; then the location reference
// data resolves the lane and ORG confirms a named customer (T3).
func (d CardDeps) evaluate(ctx context.Context, s Scope, commandID string, occurredAt time.Time, command any, snapshotID string,
	input rate.EvaluationInput, basisID string) (Receipt, error) {
	if d.Cards == nil || d.Snapshots == nil || d.Parties == nil || d.Locations == nil || (basisID != "" && d.Bases == nil) {
		return Receipt{}, ErrMissingDependency
	}
	hash, recorded, stored, replayed, err := d.begin(ctx, s, commandID, occurredAt, command)
	if err != nil || replayed {
		return stored, err
	}
	subject := snapshotID
	if basisID != "" {
		subject = basisID
	}
	if err = d.allow(ctx, s, ActionEvaluateRates, subject); err != nil {
		return Receipt{}, err
	}
	in := input
	in.At = occurredAt
	edition := ""
	for _, code := range []*string{&in.Origin, &in.Destination, &in.Via} {
		if *code == "" {
			continue
		}
		resolved, ed, err := d.Locations.Resolve(ctx, in.Mode, *code)
		if err != nil {
			return Receipt{}, err
		}
		*code, edition = resolved, ed
	}
	in.Edition = edition
	if in.Customer.PartyID != "" {
		if in.Customer.PartyRevision, err = d.Parties.ConfirmRole(ctx, s.TenantID, in.Customer.PartyID, in.LegalEntityID, CustomerRoles, in.At); err != nil {
			return Receipt{}, err
		}
	}
	kind := "RateSnapshot"
	if basisID != "" {
		kind = "PricingBasis"
	}
	return d.commit(ctx, s, commandID, hash, kind, func(txCtx context.Context) (string, int64, pendingAggregate, error) {
		if err := d.allow(txCtx, s, ActionEvaluateRates, subject); err != nil {
			return "", 0, nil, err
		}
		versions, err := d.Cards.Published(txCtx, s.TenantID, in.LegalEntityID, in.At,
			Lane{Mode: in.Mode, ContainerMode: in.ContainerMode, Origin: in.Origin, Destination: in.Destination})
		if err != nil {
			return "", 0, nil, err
		}
		snapshot, err := rate.RecordSnapshot(s.TenantID, snapshotID, in, versions, s.ActorID, recorded)
		if err != nil {
			return "", 0, nil, err
		}
		if err = d.Snapshots.Insert(txCtx, snapshot); err != nil {
			return "", 0, nil, err
		}
		if basisID == "" {
			return snapshotID, 1, snapshot, nil
		}
		basis, err := rate.IssueBasis(basisID, snapshot)
		if err != nil {
			return "", 0, nil, err
		}
		return basisID, 1, both{snapshot, basis}, d.Bases.Insert(txCtx, basis)
	})
}

// both is a snapshot and the basis issued from it, written in one transaction.
type both struct {
	snapshot *rate.RateSnapshot
	basis    *rate.PricingBasis
}

func (b both) Pending() []rate.Event { return append(b.snapshot.Pending(), b.basis.Pending()...) }
func (b both) Acknowledge()          { b.snapshot.Acknowledge(); b.basis.Acknowledge() }

// EstimateCommand asks RATE to rate a shipment (SHIPMENT_ESTIMATE: both sides, for the shipment's customer) or a
// consolidation (CONSOL_COST_ESTIMATE: the BUY side, with the consolidation's carrier when known) at OccurredAt and to
// issue PricingBasis v1 BasisID from the snapshot (FIN asks, RATE -> FIN). The snapshot takes the basis id.
type EstimateCommand struct {
	CommandID  string
	Scope      Scope
	BasisID    string
	Input      rate.EvaluationInput
	OccurredAt time.Time
}

func (d CardDeps) IssueEstimate(ctx context.Context, c EstimateCommand) (Receipt, error) {
	in := c.Input
	switch in.Purpose {
	case rate.PurposeShipmentEstimate:
		in.Sides, in.SubjectKind = []rate.Side{rate.SideSell, rate.SideBuy}, "SHIPMENT"
	case rate.PurposeConsolCostEstimate:
		in.Sides, in.SubjectKind, in.Customer = []rate.Side{rate.SideBuy}, "CONSOLIDATION", rate.CustomerRef{}
	default:
		return Receipt{}, fmt.Errorf("%w: an estimate is a SHIPMENT_ESTIMATE or a CONSOL_COST_ESTIMATE", rate.ErrInvalidBasis)
	}
	return d.evaluate(ctx, c.Scope, c.CommandID, c.OccurredAt, c, c.BasisID, in, c.BasisID)
}
