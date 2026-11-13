package fin

import (
	"context"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V0.7: fapiao records (FB6).

func (m *Module) RequestFapiao(ctx context.Context, cmd api.RequestFapiao) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RequestFapiao(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRequestFapiao,
		cmd.FapiaoID, 0, cmd.OccurredAt, cmd), domain.FapiaoInput{ID: cmd.FapiaoID, LegalEntityID: cmd.LegalEntityID, DebtorPartyID: cmd.DebtorPartyID,
		Type: cmd.Type}, cmd.InvoiceIDs)
	return receipt(r, err)
}

func platformEvidence(e api.FapiaoEvidence, confirmation string) (domain.PlatformEvidence, error) {
	on, err := domain.ParseDate(e.Date)
	return domain.PlatformEvidence{Number: e.Number, On: on, EvidenceRef: e.EvidenceRef, ConfirmationRef: confirmation}, err
}

func (m *Module) RecordFapiaoIssued(ctx context.Context, cmd api.RecordFapiaoIssued) (api.Receipt, error) {
	e, err := platformEvidence(cmd.Fapiao, "")
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordFapiaoIssued(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordFapiaoIssued,
		cmd.FapiaoID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), e)
	return receipt(r, err)
}

func (m *Module) RecordFapiaoFailed(ctx context.Context, cmd api.RecordFapiaoFailed) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordFapiaoFailed(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordFapiaoFailed,
		cmd.FapiaoID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Reason)
	return receipt(r, err)
}

func (m *Module) RecordRedLetter(ctx context.Context, cmd api.RecordRedLetter) (api.Receipt, error) {
	e, err := platformEvidence(cmd.RedLetter, cmd.ConfirmationRef)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordRedLetter(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordRedLetter,
		cmd.FapiaoID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), e, cmd.Reason)
	return receipt(r, err)
}

func fapiaoView(r *domain.FapiaoRecord, revision int64) api.FapiaoRecord {
	st := r.State()
	money := func(v int64) kernel.Money { return kernel.Money{Currency: st.Currency, Minor: v} }
	gross, vat := st.Total()
	b := st.Buyer
	out := api.FapiaoRecord{TenantID: string(st.TenantID), FapiaoID: st.FapiaoID, Revision: revision, LegalEntityID: st.LegalEntityID,
		DebtorPartyID: st.DebtorPartyID, DebtorRevision: st.DebtorRevision, Type: st.Type, State: st.State, ProfileVersion: b.ProfileVersion,
		BuyerTitle: b.Title, TaxpayerNumber: b.TaxpayerNumber, BuyerAddress: b.Address, BuyerPhone: b.Phone, BankName: b.BankName,
		BankAccount: b.BankAccount, Gross: money(gross), VAT: money(vat), Lines: []api.FapiaoLine{}, Excluded: []api.FapiaoAmount{},
		Notes: []api.FapiaoCoveredNote{}, RequestedBy: st.RequestedBy, RequestedAt: st.RequestedAt, FailReason: st.FailReason, FailedBy: st.FailedBy,
		FailedAt: st.FailedAt, RedReason: st.RedReason}
	lines, excluded := st.Lines()
	for _, l := range lines {
		out.Lines = append(out.Lines, api.FapiaoLine{Treatment: l.Treatment, RateBP: l.RateBP, Gross: money(l.Gross), Net: money(l.Net), VAT: money(l.VAT)})
	}
	for _, a := range excluded {
		out.Excluded = append(out.Excluded, api.FapiaoAmount{Treatment: a.Treatment, RateBP: a.RateBP, Amount: money(a.Local)})
	}
	for _, n := range st.Notes {
		v := api.FapiaoCoveredNote{InvoiceID: n.InvoiceID, Number: n.Number, InvoiceDate: dateView(n.InvoiceDate), Amounts: []api.FapiaoAmount{},
			Released: n.Released}
		for _, a := range n.Amounts {
			v.Amounts = append(v.Amounts, api.FapiaoAmount{Treatment: a.Treatment, RateBP: a.RateBP, Amount: money(a.Local)})
		}
		out.Notes = append(out.Notes, v)
	}
	evidence := func(e *domain.PlatformEvidence) *api.FapiaoRecordEvidence {
		if e == nil {
			return nil
		}
		return &api.FapiaoRecordEvidence{Number: e.Number, Date: dateView(e.On), EvidenceRef: e.EvidenceRef, ConfirmationRef: e.ConfirmationRef,
			RecordedBy: e.By, RecordedAt: e.At}
	}
	out.Issued, out.RedLetter = evidence(st.Issued), evidence(st.RedLetter)
	return out
}

func (m *Module) GetFapiaoRecord(ctx context.Context, q api.GetFapiaoRecord) (api.FapiaoRecord, error) {
	d := m.deps(q.TenantID, "")
	var out api.FapiaoRecord
	err := d.Reader(ctx, func(txCtx context.Context) error {
		r, revision, err := d.Fapiao.Read(txCtx, domain.TenantID(q.TenantID), q.FapiaoID)
		if err == nil {
			out = fapiaoView(r, revision)
		}
		return err
	})
	if err != nil {
		return api.FapiaoRecord{}, classify(err)
	}
	return out, nil
}

func (m *Module) ListFapiaoRecords(ctx context.Context, q api.ListFapiaoRecords) ([]api.FapiaoRecord, error) {
	d := m.deps(q.TenantID, "")
	out := []api.FapiaoRecord{}
	err := d.Reader(ctx, func(txCtx context.Context) error {
		records, revisions, err := d.Fapiao.OfDebtor(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.DebtorPartyID)
		for i, r := range records {
			out = append(out, fapiaoView(r, revisions[i]))
		}
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}
