// Package doc assembles the DOC (Transport Documents) bounded context, V0.2: the HAWB and MAWB of the N1 air line,
// rendered to PDF and dispatched with the P5 records. Code outside DOC uses it through package api; everything under
// internal/ is private to DOC.
package doc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	airapi "go.idp.xyz/idp-cargo/internal/contexts/air/api"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	airacl "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/adapters/air"
	comacl "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/adapters/com"
	finacl "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/adapters/fin"
	orgacl "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/adapters/org"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/adapters/pdf"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Module wires DOC's use cases to PostgreSQL through IDP-Bento-Go, to AIR's AirDocFacts v1, COM's OrderParties v1
// and ORG's legal entities, parties and contact transfer register, to FIN's CreditStatus v1 (C7; a debtor without a
// credit account still needs an override, OD-3 B), and renders with the IDP-AWB-V1 template.
type Module struct {
	db        *bentopg.DB
	outbox    *bentooutbox.Store
	facts     application.Facts
	parties   application.Parties
	directory application.Directory
	credit    application.Credit
	renderer  application.Renderer
	auth      api.Authorizer
	clock     bento.SystemClock
	window    kernel.RecordWindow
}

// Option configures a Module.
type Option func(*Module)

// WithRecordWindow sets how far a command's business time may lie before the time DOC records it (T1).
func WithRecordWindow(w kernel.RecordWindow) Option { return func(m *Module) { m.window = w } }

var (
	_ api.Commands                         = (*Module)(nil)
	_ api.Queries                          = (*Module)(nil)
	_ api.TransportDocumentSnapshotQueries = (*Module)(nil)
)

// New assembles DOC. air answers AirDocFacts v1 (relationship 11), com OrderParties v1 and org LegalEntityRef v1,
// PartyRef v1 and ContactTransferRegister v1; DOC writes to none of them.
func New(db *bentopg.DB, air airapi.AirDocFactsQueries, com comapi.OrderPartyQueries, org orgacl.ORG, fin finapi.CreditStatusQueries, auth api.Authorizer,
	opts ...Option) (*Module, error) {
	if db == nil || air == nil || com == nil || org == nil || fin == nil || auth == nil {
		return nil, errors.New("doc: module needs a database, AIR's facts, COM's parties, ORG, FIN's credit status and an authorizer")
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	m := &Module{db: db, outbox: outbox, facts: airacl.Facts{AIR: air}, parties: comacl.Parties{COM: com}, directory: orgacl.Directory{ORG: org},
		credit: finacl.Credit{FIN: fin}, renderer: pdf.Renderer{}, auth: auth, window: kernel.DefaultRecordWindow}
	for _, opt := range opts {
		opt(m)
	}
	if !m.window.Valid() {
		return nil, errors.New("doc: the record window must not be negative")
	}
	return m, nil
}

func stored(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Microsecond)
}

func (m *Module) command(id, tenant, actor, action, subject string, expected int64, at time.Time, payload any) application.Command {
	return application.Command{CommandID: id, Scope: application.Scope{TenantID: domain.TenantID(tenant), ActorID: actor}, Action: application.Action(action),
		Subject: subject, ExpectedRevision: expected, OccurredAt: stored(at), Payload: payload}
}

func (m *Module) DraftTransportDocument(ctx context.Context, cmd api.DraftTransportDocument) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Draft(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionDraftTransportDocument,
		cmd.DocumentID, 0, cmd.OccurredAt, cmd), application.Source{Kind: domain.Kind(cmd.Kind), MovementID: cmd.MovementID, ShipmentID: cmd.ShipmentID,
		AgentPartyID: cmd.AgentPartyID, Entered: domain.Entered{PaymentIndicator: cmd.PaymentIndicator, HandlingInformation: cmd.HandlingInformation}})
	return receipt(r, err)
}

func (m *Module) RefreshDocumentDraft(ctx context.Context, cmd api.RefreshDocumentDraft) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Revise(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRefreshDocumentDraft,
		cmd.DocumentID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), application.Revise{Reason: cmd.Reason, Payment: cmd.PaymentIndicator,
		Handling: cmd.HandlingInformation, AgentPartyID: cmd.AgentPartyID, MovementID: cmd.MovementID})
	return receipt(r, err)
}

func (m *Module) ApproveTransportDocument(ctx context.Context, cmd api.ApproveTransportDocument) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Approve(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionApproveTransportDocument,
		cmd.DocumentID, cmd.ExpectedRevision, cmd.OccurredAt, cmd))
	return receipt(r, err)
}

func (m *Module) IssueTransportDocument(ctx context.Context, cmd api.IssueTransportDocument) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Issue(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionIssueTransportDocument,
		cmd.DocumentID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.CreditOverrideReason, false)
	return receipt(r, err)
}

func (m *Module) OpenDocumentAmendment(ctx context.Context, cmd api.OpenDocumentAmendment) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Revise(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionOpenDocumentAmendment,
		cmd.DocumentID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), application.Revise{Reason: cmd.Reason, Payment: cmd.PaymentIndicator,
		Handling: cmd.HandlingInformation, AgentPartyID: cmd.AgentPartyID, MovementID: cmd.MovementID, Amendment: true})
	return receipt(r, err)
}

func (m *Module) IssueDocumentAmendment(ctx context.Context, cmd api.IssueDocumentAmendment) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Issue(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionIssueDocumentAmendment,
		cmd.DocumentID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.CreditOverrideReason, true)
	return receipt(r, err)
}

func (m *Module) AbandonDocumentAmendment(ctx context.Context, cmd api.AbandonDocumentAmendment) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Abandon(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionAbandonDocumentAmendment,
		cmd.DocumentID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Reason)
	return receipt(r, err)
}

func (m *Module) CancelTransportDocument(ctx context.Context, cmd api.CancelTransportDocument) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).Cancel(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionCancelTransportDocument,
		cmd.DocumentID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Reason)
	return receipt(r, err)
}

func (m *Module) RecordIssuanceAuthority(ctx context.Context, cmd api.RecordIssuanceAuthority) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordAuthority(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordIssuanceAuthority,
		cmd.AuthorityID, 0, cmd.OccurredAt, cmd), domain.IssuanceAuthority{LegalEntityID: cmd.LegalEntityID, Kind: domain.Kind(cmd.Kind),
		IssuePlace: cmd.IssuePlace, AirlinePartyID: cmd.AirlinePartyID, Qualification: cmd.Qualification, Validity: cmd.Validity, EvidenceRef: cmd.EvidenceRef})
	return receipt(r, err)
}

func (m *Module) WithdrawIssuanceAuthority(ctx context.Context, cmd api.WithdrawIssuanceAuthority) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).WithdrawAuthority(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionWithdrawIssuanceAuthority, cmd.AuthorityID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Reason)
	return receipt(r, err)
}

func (m *Module) GetTransportDocument(ctx context.Context, q api.GetTransportDocument) (api.TransportDocument, error) {
	d, revision, err := m.reads(q.TenantID).Document(ctx, domain.TenantID(q.TenantID), domain.DocumentID(q.DocumentID))
	if err != nil {
		return api.TransportDocument{}, classify(err)
	}
	st := d.State()
	out := api.TransportDocument{TenantID: q.TenantID, DocumentID: q.DocumentID, Revision: revision, Kind: string(st.Kind), State: string(st.State),
		Number: st.Number, MovementID: st.MovementID, ConsolidationID: st.ConsolidationID, ShipmentID: st.ShipmentID, LegalEntityID: st.LegalEntityID,
		LatestVersion: d.Latest().No, CancelReason: st.CancelReason, CancelledAt: st.CancelledAt}
	if v, ok := d.Current(); ok && st.State != domain.StateCancelled {
		out.CurrentVersion = v.No
	}
	for _, v := range st.Versions {
		s := api.VersionSummary{Version: v.No, Status: d.Status(v.No), SHA256: v.SHA256, SourceFactsVersion: v.Doc.Source.Version, Reason: v.Reason,
			DraftedBy: v.DraftedBy, DraftedAt: v.DraftedAt}
		if a := v.Approval; a != nil {
			s.ApprovedBy, s.ApprovedAt, s.SelfApproved = a.By, a.At, a.SelfApproved
		}
		if i := v.Issue; i != nil {
			s.IssuedBy, s.IssuedAt, s.AuthorityID, s.CreditBasis, s.CreditOverrideReason = i.By, i.At, i.AuthorityID, i.Credit.Basis, i.Credit.Reason
		}
		if x := v.Discard; x != nil {
			s.DiscardReason = x.Reason
		}
		out.Versions = append(out.Versions, s)
	}
	return out, nil
}

func (m *Module) GetDocumentVersion(ctx context.Context, q api.GetDocumentVersion) (api.DocumentVersion, error) {
	if q.Version < 0 {
		return api.DocumentVersion{}, classify(application.ErrInvalidQuery)
	}
	d, _, err := m.reads(q.TenantID).Document(ctx, domain.TenantID(q.TenantID), domain.DocumentID(q.DocumentID))
	if err != nil {
		return api.DocumentVersion{}, classify(err)
	}
	v, ok := d.Current()
	if !ok {
		v = d.Latest()
	}
	if q.Version > 0 {
		versions := d.State().Versions
		if q.Version > len(versions) {
			return api.DocumentVersion{}, fmt.Errorf("%w: document %s has no version %d", api.ErrNotFound, q.DocumentID, q.Version)
		}
		v = versions[q.Version-1]
	}
	out := api.DocumentVersion{DocumentID: q.DocumentID, Version: v.No, Status: d.Status(v.No), Content: append([]byte(nil), v.Content...), SHA256: v.SHA256}
	if err = json.Unmarshal(v.Content, &out.Doc); err != nil {
		return api.DocumentVersion{}, err
	}
	return out, nil
}

func (m *Module) ListDocumentsForShipment(ctx context.Context, q api.ListDocumentsForShipment) ([]api.DocumentSummary, error) {
	return m.list(ctx, q.TenantID, q.ShipmentID, "")
}

func (m *Module) ListDocumentsForConsolidation(ctx context.Context, q api.ListDocumentsForConsolidation) ([]api.DocumentSummary, error) {
	return m.list(ctx, q.TenantID, "", q.ConsolidationID)
}

func (m *Module) list(ctx context.Context, tenant, shipment, consolidation string) ([]api.DocumentSummary, error) {
	found, err := m.reads(tenant).List(ctx, domain.TenantID(tenant), shipment, consolidation)
	if err != nil {
		return nil, classify(err)
	}
	out := []api.DocumentSummary{}
	for _, s := range found {
		out = append(out, api.DocumentSummary{DocumentID: string(s.DocumentID), Kind: string(s.Kind), State: string(s.State), Number: s.Number,
			Revision: s.Revision, CurrentVersion: s.CurrentVersion, LatestVersion: s.LatestVersion})
	}
	return out, nil
}

// GetHouseDocumentSnapshot answers TransportDocumentSnapshot v1 for FIN: the valid issued version of the shipment's
// house document (a HAWB in N1), issued or under amendment; anything else is not found.
func (m *Module) GetHouseDocumentSnapshot(ctx context.Context, q api.GetHouseDocumentSnapshot) (api.TransportDocumentSnapshot, error) {
	if q.TenantID == "" || q.ShipmentID == "" {
		return api.TransportDocumentSnapshot{}, classify(application.ErrInvalidQuery)
	}
	r := m.reads(q.TenantID)
	found, err := r.List(ctx, domain.TenantID(q.TenantID), q.ShipmentID, "")
	if err != nil {
		return api.TransportDocumentSnapshot{}, classify(err)
	}
	var house []application.Summary
	for _, s := range found {
		if s.Kind == domain.KindHAWB && (s.State == domain.StateIssued || s.State == domain.StateAmending) {
			house = append(house, s)
		}
	}
	switch len(house) {
	case 0:
		return api.TransportDocumentSnapshot{}, fmt.Errorf("%w: shipment %s has no issued house document", api.ErrNotFound, q.ShipmentID)
	case 1:
	default:
		return api.TransportDocumentSnapshot{}, fmt.Errorf("doc: shipment %s has %d valid house documents", q.ShipmentID, len(house))
	}
	d, _, err := r.Document(ctx, domain.TenantID(q.TenantID), house[0].DocumentID)
	if err != nil {
		return api.TransportDocumentSnapshot{}, classify(err)
	}
	v, ok := d.Current()
	st := d.State()
	if !ok || v.Issue == nil || d.Status(v.No) != domain.VersionIssued || st.ShipmentID != q.ShipmentID {
		return api.TransportDocumentSnapshot{}, fmt.Errorf("%w: document %s has no valid issued version", api.ErrNotFound, house[0].DocumentID)
	}
	c := v.Doc
	return api.TransportDocumentSnapshot{DocumentID: string(st.DocumentID), Kind: string(st.Kind), Version: v.No, VersionStatus: api.VersionIssued,
		SHA256: v.SHA256, Number: c.Number, MasterNumber: c.MAWB, ShipmentID: st.ShipmentID, ConsolidationID: st.ConsolidationID,
		LegalEntityID: st.LegalEntityID, IssuedAt: v.Issue.At, Origin: c.Origin.Code, Destination: c.Destination.Code, Pieces: c.Cargo.Pieces,
		GrossGrams: c.Cargo.GrossGrams, ChargeableGrams: c.Cargo.ChargeableGrams}, nil
}

func (m *Module) GetIssuanceAuthority(ctx context.Context, q api.GetIssuanceAuthority) (api.IssuanceAuthority, error) {
	a, revision, err := m.reads(q.TenantID).Authority(ctx, domain.TenantID(q.TenantID), q.AuthorityID)
	if err != nil {
		return api.IssuanceAuthority{}, classify(err)
	}
	return api.IssuanceAuthority{AuthorityID: a.AuthorityID, Revision: revision, LegalEntityID: a.LegalEntityID, Kind: string(a.Kind), IssuePlace: a.IssuePlace,
		AirlinePartyID: a.AirlinePartyID, Qualification: a.Qualification, Validity: a.Validity, EvidenceRef: a.EvidenceRef, RecordedBy: a.RecordedBy,
		RecordedAt: a.RecordedAt, WithdrawnBy: a.WithdrawnBy, WithdrawnAt: a.WithdrawnAt, WithdrawReason: a.WithdrawReason}, nil
}

func (m *Module) reads(tenant string) application.Reads {
	return application.Reads{Tx: bento.TenantTransactor{DB: m.db, Tenant: tenant}, Documents: postgres.DocumentStore{DB: m.db},
		Authorities: postgres.AuthorityStore{DB: m.db}}
}

func (m *Module) deps(tenant, commandID string) application.Deps {
	return application.Deps{
		Tx:          bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Documents:   postgres.DocumentStore{DB: m.db},
		Authorities: postgres.AuthorityStore{DB: m.db},
		Receipts:    postgres.CommandReceipts{DB: m.db},
		Outbox:      postgres.Outbox{Writer: m.outbox, CommandID: commandID, RecordedAt: m.clock.Now()},
		Auth:        authorizer{m.auth},
		Clock:       m.clock,
		Window:      m.window,
		Facts:       m.facts,
		Parties:     m.parties,
		Directory:   m.directory,
		Credit:      m.credit,
		Dispatches:  postgres.DispatchStore{DB: m.db},
		Renderer:    m.renderer,
	}
}

func receipt(r application.Receipt, err error) (api.Receipt, error) {
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	return api.Receipt{CommandID: r.CommandID, DocumentID: r.SubjectID, NewRevision: r.NewRevision, Version: r.Version}, nil
}

type authorizer struct{ api.Authorizer }

func (a authorizer) RequireDocAction(ctx context.Context, s application.Scope, action application.Action, subject string) error {
	return a.AuthorizeDocAction(ctx, string(s.TenantID), s.ActorID, string(action), subject)
}

// classify wraps known DOC, kernel and Bento causes in their api error kind and passes anything else through.
func classify(err error) error {
	var kind error
	switch {
	case errors.Is(err, application.ErrUnauthenticated):
		kind = api.ErrUnauthenticated
	case errors.Is(err, application.ErrUnauthorized), errors.Is(err, bento.ErrTenantScopeConflict):
		kind = api.ErrUnauthorized
	case errors.Is(err, application.ErrIdempotencyConflict):
		kind = api.ErrIdempotencyConflict
	case errors.Is(err, domain.ErrConflict), errors.Is(err, bentorepo.ErrConflict), errors.Is(err, bentorepo.ErrAlreadyExists), postgres.Conflict(err):
		kind = api.ErrConflict
	case errors.Is(err, bentorepo.ErrNotFound):
		kind = api.ErrNotFound
	case errors.Is(err, domain.ErrInvalidDocument), errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrInvalidChange),
		errors.Is(err, domain.ErrSourceNotFinal), errors.Is(err, domain.ErrSourceStale), errors.Is(err, domain.ErrMasterMismatch),
		errors.Is(err, domain.ErrPartiesDiverge), errors.Is(err, domain.ErrIncomplete), errors.Is(err, domain.ErrNotLocked),
		errors.Is(err, domain.ErrCreditHold), errors.Is(err, domain.ErrCreditUndecided), errors.Is(err, domain.ErrIssuanceBlocked),
		errors.Is(err, domain.ErrIssuerNotVerified), errors.Is(err, domain.ErrInvalidAuthority), errors.Is(err, domain.ErrNumberIssued),
		errors.Is(err, domain.ErrNotDispatchable), errors.Is(err, domain.ErrTransferBlocked), errors.Is(err, domain.ErrTransferNotReserved),
		errors.Is(err, domain.ErrInvalidDispatch), errors.Is(err, domain.ErrCopyChanged),
		errors.Is(err, application.ErrInvalidQuery), errors.Is(err, application.ErrInvalidCommand), errors.Is(err, application.ErrUnknownSource),
		errors.Is(err, kernel.ErrOutsideRecordWindow):
		kind = api.ErrRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
