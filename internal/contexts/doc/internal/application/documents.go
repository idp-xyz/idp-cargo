package application

import (
	"context"
	"fmt"
	"slices"
	"time"

	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

// Source says what a version is built from: the kind, the movement whose AirDocFacts it freezes, the shipment of a
// HAWB or the destination agent of a MAWB, and what the person drafting entered.
type Source struct {
	Kind         doc.Kind
	MovementID   string
	ShipmentID   string
	AgentPartyID string
	Entered      doc.Entered
}

// build reads AIR's latest facts, the issuing legal entity and the parties (COM's for a HAWB, ORG's agent for a
// MAWB) before DOC's transaction (T3), and builds a version's content from them.
func (d Deps) build(ctx context.Context, s Scope, id doc.DocumentID, src Source, at time.Time) (doc.Content, error) {
	if d.Facts == nil || d.Parties == nil || d.Directory == nil {
		return doc.Content{}, ErrMissingDependency
	}
	facts, err := d.Facts.Latest(ctx, s.TenantID, src.MovementID)
	if err != nil {
		return doc.Content{}, err
	}
	issuer, err := d.Directory.Issuer(ctx, s.TenantID, facts.LegalEntityID)
	if err != nil {
		return doc.Content{}, err
	}
	switch src.Kind {
	case doc.KindHAWB:
		var orderIDs []string
		for _, h := range facts.Houses {
			if h.ShipmentID != src.ShipmentID {
				continue
			}
			for _, p := range h.Parts {
				if !slices.Contains(orderIDs, p.OrderID) {
					orderIDs = append(orderIDs, p.OrderID)
				}
			}
		}
		if len(orderIDs) == 0 {
			return doc.Content{}, fmt.Errorf("%w: %s", doc.ErrMasterMismatch, src.ShipmentID)
		}
		var orders []doc.OrderParties
		for _, o := range orderIDs {
			parties, err := d.Parties.Order(ctx, s.TenantID, s.ActorID, o)
			if err != nil {
				return doc.Content{}, err
			}
			orders = append(orders, parties)
		}
		return doc.HouseContent(id, src.ShipmentID, facts, issuer, orders, src.Entered)
	case doc.KindMAWB:
		agent, err := d.Directory.Agent(ctx, s.TenantID, src.AgentPartyID, facts.LegalEntityID, at)
		if err != nil {
			return doc.Content{}, err
		}
		return doc.MasterContent(id, facts, issuer, agent, src.Entered)
	}
	return doc.Content{}, fmt.Errorf("%w: kind %q", ErrInvalidCommand, src.Kind)
}

// Draft drafts a document.
func (d Deps) Draft(ctx context.Context, c Command, src Source) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	content, err := d.build(ctx, c.Scope, doc.DocumentID(c.Subject), src, c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	return d.write(ctx, c, hash, "TransportDocument", func(txCtx context.Context) (Receipt, aggregate, error) {
		m, err := doc.DraftDocument(c.Scope.TenantID, doc.DocumentID(c.Subject), content, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		st := m.State()
		subject, found, err := d.Documents.IssuedFor(txCtx, st.TenantID, st.LegalEntityID, st.Kind, st.Number)
		if err != nil {
			return Receipt{}, nil, err
		}
		if found && subject != st.Subject() {
			return Receipt{}, nil, fmt.Errorf("%w: %s %s was issued for %s", doc.ErrNumberIssued, st.Kind, st.Number, subject)
		}
		m.Stamp(entered)
		revision, err := d.Documents.Insert(txCtx, m)
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: 1}, m, err
	})
}

// change runs a command on a stored document at the expected revision: read asks other contexts first, outside
// DOC's transaction, with the document as it stood; apply changes it in the transaction.
func (d Deps) change(ctx context.Context, c Command, read func(context.Context, *doc.Document) error,
	apply func(context.Context, *doc.Document) error) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	if read != nil {
		var current *doc.Document
		if err = d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
			var revision int64
			current, revision, err = d.Documents.Read(txCtx, c.Scope.TenantID, doc.DocumentID(c.Subject))
			if err == nil && revision != c.ExpectedRevision {
				return doc.ErrConflict
			}
			return err
		}); err != nil {
			return Receipt{}, err
		}
		if err = read(ctx, current); err != nil {
			return Receipt{}, err
		}
	}
	return d.write(ctx, c, hash, "TransportDocument", func(txCtx context.Context) (Receipt, aggregate, error) {
		m, revision, err := d.Documents.Load(txCtx, c.Scope.TenantID, doc.DocumentID(c.Subject))
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, doc.ErrConflict
		}
		if err = apply(txCtx, m); err != nil {
			return Receipt{}, nil, err
		}
		m.Stamp(entered)
		if revision, err = d.Documents.Save(txCtx, m, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: m.Latest().No}, m, nil
	})
}

// sourceOf is what the latest version was built from, with the entered fields, agent and, for a HAWB whose
// shipment AIR now associates with another movement, the movement a command changes.
func sourceOf(m *doc.Document, payment, handling, agent, movement string) Source {
	st, latest := m.State(), m.Latest().Doc
	src := Source{Kind: st.Kind, MovementID: st.MovementID, ShipmentID: st.ShipmentID, AgentPartyID: latest.Consignee.PartyID,
		Entered: doc.Entered{PaymentIndicator: latest.PaymentIndicator, HandlingInformation: latest.HandlingInformation}}
	if payment != "" {
		src.Entered.PaymentIndicator = payment
	}
	if handling != "" {
		src.Entered.HandlingInformation = handling
	}
	if agent != "" && st.Kind == doc.KindMAWB {
		src.AgentPartyID = agent
	}
	if movement != "" {
		src.MovementID = movement
	}
	return src
}

// Revise is a refresh of a draft or the opening of an amendment: the next version from the current sources.
type Revise struct {
	Reason       string
	Payment      string
	Handling     string
	AgentPartyID string
	MovementID   string
	Amendment    bool
}

func (d Deps) Revise(ctx context.Context, c Command, r Revise) (Receipt, error) {
	var content doc.Content
	return d.change(ctx, c, func(ctx context.Context, m *doc.Document) (err error) {
		content, err = d.build(ctx, c.Scope, m.ID(), sourceOf(m, r.Payment, r.Handling, r.AgentPartyID, r.MovementID), c.OccurredAt)
		return err
	}, func(_ context.Context, m *doc.Document) error {
		if r.Amendment {
			return m.OpenAmendment(content, r.Reason, c.Scope.ActorID, c.OccurredAt)
		}
		return m.Refresh(content, r.Reason, c.Scope.ActorID, c.OccurredAt)
	})
}

// Approve approves the draft; an actor approving a draft they wrote must also be allowed ApproveOwnTransportDocument.
func (d Deps) Approve(ctx context.Context, c Command) (Receipt, error) {
	return d.change(ctx, c, func(ctx context.Context, m *doc.Document) error {
		if m.SelfApproval(c.Scope.ActorID) {
			return d.authorize(ctx, c.Scope, ActionApproveOwn, c.Subject)
		}
		return nil
	}, func(_ context.Context, m *doc.Document) error {
		return m.Approve(c.Scope.ActorID, c.OccurredAt)
	})
}

// orders reads COM's parties of every order of a version.
func (d Deps) orders(ctx context.Context, s Scope, v doc.Version) ([]doc.OrderParties, error) {
	var out []doc.OrderParties
	for _, ref := range v.Doc.Orders {
		o, err := d.Parties.Order(ctx, s.TenantID, s.ActorID, ref.OrderID)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// Issue issues the approved version, or the amendment's approved draft: it reads AIR's latest facts, ORG's issuing
// legal entity and, for a HAWB, COM's parties and FIN's credit decision first (T3); an override of a missing
// decision needs OverrideCreditCheck. The issuance authority in force is looked up in the transaction.
func (d Deps) Issue(ctx context.Context, c Command, overrideReason string, amendment bool) (Receipt, error) {
	if d.Facts == nil || d.Parties == nil || d.Credit == nil || d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	var gate doc.IssueGate
	return d.change(ctx, c, func(ctx context.Context, m *doc.Document) (err error) {
		latest := m.Latest()
		if gate.Facts, err = d.Facts.Latest(ctx, c.Scope.TenantID, latest.Doc.Source.MovementID); err != nil {
			return err
		}
		issuer, err := d.Directory.Issuer(ctx, c.Scope.TenantID, m.State().LegalEntityID)
		if err != nil {
			return err
		}
		gate.IssuerVerified = issuer.Verified
		if m.State().Kind != doc.KindHAWB {
			return nil
		}
		if gate.Orders, err = d.orders(ctx, c.Scope, latest); err != nil {
			return err
		}
		var debtors []string
		for _, o := range gate.Orders {
			if o.Customer != "" && !slices.Contains(debtors, o.Customer) {
				debtors = append(debtors, o.Customer)
			}
		}
		if gate.Credit, err = d.Credit.Decision(ctx, c.Scope.TenantID, m.State().LegalEntityID, debtors); err != nil {
			return err
		}
		if gate.Credit.Status != "OK" && overrideReason != "" {
			gate.OverrideReason = overrideReason
			return d.authorize(ctx, c.Scope, ActionOverrideCredit, c.Subject)
		}
		return nil
	}, func(txCtx context.Context, m *doc.Document) error {
		st, latest := m.State(), m.Latest().Doc
		airline := ""
		if st.Kind == doc.KindMAWB {
			airline = latest.Airline.PartyID
		}
		a, found, err := d.Authorities.InForce(txCtx, st.TenantID, st.LegalEntityID, st.Kind, latest.IssuePlace.Code, airline, c.OccurredAt)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: %s %s at %s", doc.ErrIssuanceBlocked, st.LegalEntityID, st.Kind, latest.IssuePlace.Code)
		}
		gate.AuthorityID, gate.AuthorityVersion = a.AuthorityID, a.Version
		if amendment {
			return m.IssueAmendment(gate, c.Scope.ActorID, c.OccurredAt)
		}
		return m.Issue(gate, c.Scope.ActorID, c.OccurredAt)
	})
}

// Abandon discards an amendment's draft; a HAWB's order parties are read first (T3) to check the lock is held again.
func (d Deps) Abandon(ctx context.Context, c Command, reason string) (Receipt, error) {
	var orders []doc.OrderParties
	return d.change(ctx, c, func(ctx context.Context, m *doc.Document) (err error) {
		if current, ok := m.Current(); ok && m.State().Kind == doc.KindHAWB {
			orders, err = d.orders(ctx, c.Scope, current)
		}
		return err
	}, func(_ context.Context, m *doc.Document) error {
		return m.AbandonAmendment(orders, reason, c.Scope.ActorID, c.OccurredAt)
	})
}

// Cancel cancels a document.
func (d Deps) Cancel(ctx context.Context, c Command, reason string) (Receipt, error) {
	return d.change(ctx, c, nil, func(_ context.Context, m *doc.Document) error {
		return m.Cancel(reason, c.Scope.ActorID, c.OccurredAt)
	})
}

// RecordAuthority records an issuance authority.
func (d Deps) RecordAuthority(ctx context.Context, c Command, a doc.IssuanceAuthority) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.write(ctx, c, hash, "IssuanceAuthority", func(txCtx context.Context) (Receipt, aggregate, error) {
		a.TenantID, a.AuthorityID = c.Scope.TenantID, c.Subject
		recorded, err := doc.RecordIssuanceAuthority(a, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		recorded.Stamp(entered)
		revision, err := d.Authorities.Insert(txCtx, recorded)
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: 1}, recorded, err
	})
}

// WithdrawAuthority withdraws an issuance authority.
func (d Deps) WithdrawAuthority(ctx context.Context, c Command, reason string) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.write(ctx, c, hash, "IssuanceAuthority", func(txCtx context.Context) (Receipt, aggregate, error) {
		a, revision, err := d.Authorities.Load(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, doc.ErrConflict
		}
		if err = a.Withdraw(reason, c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, nil, err
		}
		a.Stamp(entered)
		if revision, err = d.Authorities.Save(txCtx, a, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: int(a.Version)}, a, nil
	})
}

// Reads answers DOC's queries without locks, each in one snapshot.
type Reads struct {
	Tx          TxRunner
	Documents   DocumentStore
	Authorities AuthorityStore
}

func (r Reads) Document(ctx context.Context, tenant doc.TenantID, id doc.DocumentID) (*doc.Document, int64, error) {
	if tenant == "" || id == "" {
		return nil, 0, ErrInvalidQuery
	}
	var m *doc.Document
	var revision int64
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		m, revision, err = r.Documents.Read(txCtx, tenant, id)
		return err
	})
	return m, revision, err
}

func (r Reads) List(ctx context.Context, tenant doc.TenantID, shipment, consolidation string) ([]Summary, error) {
	if tenant == "" || (shipment == "") == (consolidation == "") {
		return nil, ErrInvalidQuery
	}
	var out []Summary
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		out, err = r.Documents.List(txCtx, tenant, shipment, consolidation)
		return err
	})
	return out, err
}

func (r Reads) Authority(ctx context.Context, tenant doc.TenantID, id string) (*doc.IssuanceAuthority, int64, error) {
	if tenant == "" || id == "" {
		return nil, 0, ErrInvalidQuery
	}
	var a *doc.IssuanceAuthority
	var revision int64
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		a, revision, err = r.Authorities.Read(txCtx, tenant, id)
		return err
	})
	return a, revision, err
}
