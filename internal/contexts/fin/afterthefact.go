package fin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.102 (batch B): the after-the-fact review list and its mark. The command carries no business time: FIN records
// its own clock.

// MarkAfterTheFactReviewed marks a legal entity's week reviewed.
func (m *Module) MarkAfterTheFactReviewed(ctx context.Context, cmd api.MarkAfterTheFactReviewed) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).MarkAfterTheFactReviewed(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionReviewAfterTheFact,
		domain.ReviewSubject(cmd.LegalEntityID, cmd.Week), 0, m.clock.Now(), cmd), cmd.LegalEntityID, cmd.Week, cmd.Note)
	return receipt(r, err)
}

// afterTheFactCursor is where a page follows: the key of the last line of the page before, as an opaque string.
func afterTheFactCursor(k domain.ReviewKey) string {
	blob, _ := json.Marshal([]any{k.PoolID, k.ApportionmentNo, k.LineNo, k.ShipmentID})
	return base64.RawURLEncoding.EncodeToString(blob)
}

func parseAfterTheFactCursor(s string) (domain.ReviewKey, error) {
	var k domain.ReviewKey
	if s == "" {
		return k, nil
	}
	blob, err := base64.RawURLEncoding.DecodeString(s)
	var parts []json.RawMessage
	if err == nil {
		err = json.Unmarshal(blob, &parts)
	}
	if err == nil && len(parts) != 4 {
		err = fmt.Errorf("%d parts", len(parts))
	}
	for i, dst := range []any{&k.PoolID, &k.ApportionmentNo, &k.LineNo, &k.ShipmentID} {
		if err == nil {
			err = json.Unmarshal(parts[i], dst)
		}
	}
	if err != nil {
		return domain.ReviewKey{}, fmt.Errorf("%w: after is the NextAfter of the page before", application.ErrInvalidQuery)
	}
	return k, nil
}

// GetAfterTheFactReview lists a week's attributions that have no second person, no voucher or were made by default, and
// says whether the week is reviewed. FIN's rows are read in one snapshot.
func (m *Module) GetAfterTheFactReview(ctx context.Context, q api.GetAfterTheFactReview) (api.AfterTheFactPage, error) {
	if err := m.authorizeRead(ctx, q.TenantID, q.ActorID, domain.ReviewSubject(q.LegalEntityID, q.Week)); err != nil {
		return api.AfterTheFactPage{}, err
	}
	if q.LegalEntityID == "" || q.Limit < 1 || q.Limit > maxPage {
		return api.AfterTheFactPage{}, classify(fmt.Errorf("%w: a legal entity and a page of 1 to %d", application.ErrInvalidQuery, maxPage))
	}
	after, err := parseAfterTheFactCursor(q.After)
	if err != nil {
		return api.AfterTheFactPage{}, classify(err)
	}
	start, end, err := domain.ParseWeek(q.Week)
	if err != nil {
		return api.AfterTheFactPage{}, classify(err)
	}
	got, err := m.deps(q.TenantID, "").AfterTheFactPage(ctx, domain.TenantID(q.TenantID), q.LegalEntityID, q.Week, after, q.Limit)
	if err != nil {
		return api.AfterTheFactPage{}, classify(err)
	}
	page := api.AfterTheFactPage{LegalEntityID: q.LegalEntityID, Week: q.Week, WeekStart: start, WeekEnd: end, Lines: []api.AfterTheFactLine{}}
	for _, l := range got.Lines {
		money := func(c kernel.CurrencyCode, n int64) kernel.Money { return kernel.Money{Currency: c, Minor: n} }
		page.Lines = append(page.Lines, api.AfterTheFactLine{PoolID: l.PoolID, ConsolidationID: l.ConsolidationID, LineNo: l.LineNo, ShipmentID: l.ShipmentID, JobID: l.JobID,
			ApportionmentNo: l.ApportionmentNo, PreviewNo: l.PreviewNo, EnteredAt: l.EnteredAt, Loss: money(l.Currency, l.Loss), LossLocal: money(l.FunctionalCurrency, l.LossLocal),
			Exception: money(l.Currency, l.Exception), ExceptionLocal: money(l.FunctionalCurrency, l.ExceptionLocal), Party: l.Party, Voucher: l.Voucher,
			SubmittedBy: l.SubmittedBy, PreviewedBy: l.PreviewedBy, Reasons: l.Reasons()})
	}
	if got.More {
		page.NextAfter = afterTheFactCursor(got.Lines[len(got.Lines)-1].ReviewKey)
	}
	if r := got.Review; r != nil {
		page.Review = api.AfterTheFactReviewMark{Reviewed: true, ReviewNo: r.ReviewNo, ReviewedBy: r.ReviewedBy, ReviewedAt: r.ReviewedAt, Note: r.Note, ListSize: r.ListSize, ListDigest: r.ListDigest,
			CurrentSize: got.Size, CurrentDigest: got.Digest, Matches: got.Matches}
	}
	return page, nil
}
