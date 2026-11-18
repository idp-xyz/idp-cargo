package doc

import (
	"context"

	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

var (
	_ api.CarrierCommands = (*Module)(nil)
	_ api.CarrierQueries  = (*Module)(nil)
)

// carrierRequest is what a carrier command's hash covers: its name too, as the commands that change a document at its
// revision have the same fields.
type carrierRequest struct {
	Command string
	Request any
}

func (m *Module) RecordCarrierDocument(ctx context.Context, cmd api.RecordCarrierDocument) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordCarrier(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordCarrierDocument,
		cmd.DocumentID, 0, cmd.OccurredAt, carrierRequest{api.ActionRecordCarrierDocument, cmd}), application.CarrierInput{MovementID: cmd.MovementID,
		Entered: domain.CarrierEntered{Release: cmd.Release, Originals: cmd.Originals, IssuePlace: cmd.IssuePlace, IssuedOn: cmd.IssuedOn,
			Evidence: domain.CarrierEvidence{SHA256: cmd.Evidence.SHA256, StorageID: cmd.Evidence.StorageID, MediaType: cmd.Evidence.MediaType,
				CapturedAt: stored(cmd.Evidence.CapturedAt)}}})
	return receipt(r, err)
}

func (m *Module) VerifyCarrierDocument(ctx context.Context, cmd api.VerifyCarrierDocument) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).VerifyCarrier(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionVerifyCarrierDocument,
		cmd.DocumentID, cmd.ExpectedRevision, cmd.OccurredAt, carrierRequest{api.ActionVerifyCarrierDocument, cmd}))
	return receipt(r, err)
}

func (m *Module) CancelCarrierDocument(ctx context.Context, cmd api.CancelCarrierDocument) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).CancelCarrier(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionCancelCarrierDocument,
		cmd.DocumentID, cmd.ExpectedRevision, cmd.OccurredAt, carrierRequest{api.ActionCancelCarrierDocument, cmd}), cmd.Reason)
	return receipt(r, err)
}

func (m *Module) GetCarrierDocument(ctx context.Context, q api.GetCarrierDocument) (api.CarrierDocument, error) {
	x, revision, err := m.reads(q.TenantID).CarrierDocument(ctx, postgres.CarrierStore{DB: m.db}, domain.TenantID(q.TenantID), q.DocumentID)
	if err != nil {
		return api.CarrierDocument{}, classify(err)
	}
	return carrierView(x, revision), nil
}

func (m *Module) FindCarrierDocumentByMovement(ctx context.Context, q api.FindCarrierDocumentByMovement) (api.CarrierDocument, error) {
	x, revision, err := m.reads(q.TenantID).CarrierDocumentOf(ctx, postgres.CarrierStore{DB: m.db}, domain.TenantID(q.TenantID), q.MovementID)
	if err != nil {
		return api.CarrierDocument{}, classify(err)
	}
	return carrierView(x, revision), nil
}

func carrierView(x *domain.CarrierDocument, revision int64) api.CarrierDocument {
	st := x.State()
	c := st.Doc
	return api.CarrierDocument{DocumentID: string(st.DocumentID), Kind: string(st.Kind), Number: st.Number, MovementID: st.MovementID,
		ConsolidationID: st.ConsolidationID, LegalEntityID: st.LegalEntityID, CarrierPartyID: st.CarrierPartyID, CarrierPartyRevision: c.Carrier.PartyRevision,
		CarrierRole: st.CarrierRole, IssuerCapacity: c.IssuerCapacity, Release: c.ReleaseType, Originals: c.Originals, IssuePlace: c.IssuePlace, IssuedOn: c.IssuedOn,
		Evidence:      api.CarrierEvidence{SHA256: c.Evidence.SHA256, StorageID: c.Evidence.StorageID, MediaType: c.Evidence.MediaType, CapturedAt: c.Evidence.CapturedAt},
		SourceVersion: st.SourceVersion, SourceSHA256: c.Source.SHA256, Content: st.Content, ContentSHA256: st.ContentSHA256, State: string(st.State),
		RecordedBy: st.RecordedBy, RecordedAt: st.RecordedAt, Verified: x.Verified(), VerifiedBy: st.VerifiedBy, VerifiedAt: st.VerifiedAt,
		CancelReason: st.CancelReason, CancelledBy: st.CancelledBy, CancelledAt: st.CancelledAt, Revision: revision}
}
