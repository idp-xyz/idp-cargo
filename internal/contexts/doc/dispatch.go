package doc

import (
	"context"

	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

var (
	_ api.DispatchCommands = (*Module)(nil)
	_ api.DispatchQueries  = (*Module)(nil)
)

func (m *Module) PrepareDocumentDispatch(ctx context.Context, cmd api.PrepareDocumentDispatch) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).PrepareDispatch(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionPrepareDocumentDispatch,
		cmd.DispatchID, 0, cmd.OccurredAt, cmd), application.DispatchInput{DocumentID: domain.DocumentID(cmd.DocumentID), Version: cmd.Version,
		Recipient: cmd.RecipientParty, Role: cmd.RecipientRole, Channel: cmd.Channel})
	return receipt(r, err)
}

func (m *Module) RecordDispatchOutcome(ctx context.Context, cmd api.RecordDispatchOutcome) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordOutcome(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordDispatchOutcome,
		cmd.DispatchID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Outcome, cmd.Reference)
	return receipt(r, err)
}

func (m *Module) RenderDocumentVersion(ctx context.Context, q api.RenderDocumentVersion) (api.RenderedDocument, error) {
	pdf, sum, in, err := m.reads(q.TenantID).Render(ctx, m.renderer, domain.TenantID(q.TenantID), domain.DocumentID(q.DocumentID), q.Version)
	if err != nil {
		return api.RenderedDocument{}, classify(err)
	}
	return api.RenderedDocument{DocumentID: q.DocumentID, Version: in.Version, Status: in.Status, Template: m.renderer.Template(), PDF: pdf, SHA256: sum}, nil
}

func (m *Module) GetDispatchCopy(ctx context.Context, q api.GetDispatchCopy) (api.DispatchCopy, error) {
	pdf, x, err := m.deps(q.TenantID, "").DispatchCopy(ctx, application.Scope{TenantID: domain.TenantID(q.TenantID), ActorID: q.ActorID}, q.DispatchID)
	if err != nil {
		return api.DispatchCopy{}, classify(err)
	}
	return api.DispatchCopy{Dispatch: dispatchView(x, 0), PDF: pdf}, nil
}

func (m *Module) GetDispatch(ctx context.Context, q api.GetDispatch) (api.DocumentDispatch, error) {
	x, revision, err := m.reads(q.TenantID).Dispatch(ctx, postgres.DispatchStore{DB: m.db}, domain.TenantID(q.TenantID), q.DispatchID)
	if err != nil {
		return api.DocumentDispatch{}, classify(err)
	}
	return dispatchView(x, revision), nil
}

func (m *Module) ListDocumentDispatches(ctx context.Context, q api.ListDocumentDispatches) ([]api.DocumentDispatch, error) {
	found, err := m.reads(q.TenantID).Dispatches(ctx, postgres.DispatchStore{DB: m.db}, domain.TenantID(q.TenantID), domain.DocumentID(q.DocumentID))
	if err != nil {
		return nil, classify(err)
	}
	return dispatchViews(found), nil
}

func (m *Module) ListContactProvisions(ctx context.Context, q api.ListContactProvisions) ([]api.DocumentDispatch, error) {
	found, err := m.deps(q.TenantID, "").Provisions(ctx, application.Scope{TenantID: domain.TenantID(q.TenantID), ActorID: q.ActorID}, stored(q.From),
		stored(q.Until))
	if err != nil {
		return nil, classify(err)
	}
	return dispatchViews(found), nil
}

func dispatchViews(found []*domain.Dispatch) []api.DocumentDispatch {
	out := []api.DocumentDispatch{}
	for _, x := range found {
		out = append(out, dispatchView(x, 0))
	}
	return out
}

// dispatchView maps a dispatch; revision 0 leaves the revision out (lists, copies).
func dispatchView(x *domain.Dispatch, revision int64) api.DocumentDispatch {
	st := x.State()
	out := api.DocumentDispatch{TenantID: string(st.TenantID), DispatchID: st.DispatchID, Revision: revision, DocumentID: string(st.DocumentID),
		Version: st.Version, Kind: string(st.Kind), VersionStatus: st.VersionStatus, Recipient: api.Recipient(st.Recipient), OrderingParty: st.OrderingParty,
		Channel: st.Channel, State: x.Current(), CopySHA256: st.CopySHA256, Template: st.Template, RegisterYear: st.RegisterYear,
		RegisterState: st.RegisterState, PreparedBy: st.PreparedBy, PreparedAt: st.PreparedAt, CrossBorderKeys: x.CrossBorderKeys()}
	for _, c := range st.Contacts {
		v := api.DispatchContact{No: c.No, Key: c.Key, PartyRole: c.PartyRole, OrderID: c.OrderID, Classification: c.Classification, LegalPath: c.LegalPath,
			Status: c.Status, OmitReason: c.OmitReason}
		if k := c.Consent; k != nil {
			consent := api.ContactConsent(*k)
			v.Consent = &consent
		}
		out.Contacts = append(out.Contacts, v)
	}
	for _, o := range st.Outcomes {
		out.Outcomes = append(out.Outcomes, api.DispatchOutcome{No: o.No, Outcome: o.Outcome, Reference: o.Reference, RecordedBy: o.By, RecordedAt: o.At})
	}
	return out
}
