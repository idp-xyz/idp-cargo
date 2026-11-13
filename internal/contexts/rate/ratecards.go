package rate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	orgacl "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/adapters/org"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/adapters/postgres"
	refacl "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/adapters/reference"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	refapi "go.idp.xyz/idp-cargo/internal/reference/api"
)

// RATE V0.5: the charge-code catalogue, rate cards and rate evaluation.

var (
	_ api.RateCardCommands = (*Module)(nil)
	_ api.RateCardQueries  = (*Module)(nil)
)

// WithLocations sets the location reference data rate entries and evaluations resolve their locations against.
// Without it, setting entries and evaluating rates are refused.
func WithLocations(l refapi.Locations) Option { return func(m *Module) { m.locations = l } }

func (m *Module) cardDeps(tenant, commandID string) application.CardDeps {
	d := application.CardDeps{Deps: m.deps(tenant, commandID), Codes: postgres.ChargeCodeStore{DB: m.db}, Cards: postgres.RateCardStore{DB: m.db},
		Agreements: postgres.QuoteStore{DB: m.db},
		Snapshots:  postgres.SnapshotStore{DB: m.db}, Bases: postgres.BasisStore{DB: m.db}, Parties: orgacl.Parties{ORG: m.org}}
	if m.locations != nil {
		d.Locations = refacl.Locations{Ref: m.locations}
	}
	return d
}

func cardReceipt(r application.Receipt, err error) (api.Receipt, error) {
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	return api.Receipt{CommandID: r.CommandID, QuoteID: string(r.QuoteID), SubjectID: r.SubjectID, NewRevision: r.NewRevision}, nil
}

func codeInput(zh, en, group, calculator, unit string, modes []string) domain.ChargeCodeInput {
	in := domain.ChargeCodeInput{NameZH: zh, NameEN: en, Group: domain.ChargeGroup(group), Calculator: domain.Calculator(calculator), Unit: domain.RateUnit(unit)}
	for _, mode := range modes {
		in.Modes = append(in.Modes, domain.Mode(mode))
	}
	return in
}

func (m *Module) changeCode(ctx context.Context, tenant, actor, commandID, action, code string, version int, in domain.ChargeCodeInput, at time.Time) (api.Receipt, error) {
	return cardReceipt(m.cardDeps(tenant, commandID).ChangeChargeCode(ctx, application.ChargeCodeCommand{CommandID: commandID, Scope: scope(tenant, actor),
		Action: action, Code: domain.ChargeCode(code), Version: version, Input: in, OccurredAt: stored(at)}))
}

func (m *Module) DefineChargeCode(ctx context.Context, c api.DefineChargeCode) (api.Receipt, error) {
	return m.changeCode(ctx, c.TenantID, c.ActorID, c.CommandID, "DEFINE", c.Code, 0, codeInput(c.NameZH, c.NameEN, c.Group, c.Calculator, c.Unit, c.Modes), c.OccurredAt)
}

func (m *Module) ReviseChargeCode(ctx context.Context, c api.ReviseChargeCode) (api.Receipt, error) {
	return m.changeCode(ctx, c.TenantID, c.ActorID, c.CommandID, "REVISE", c.Code, c.ExpectedVersion,
		codeInput(c.NameZH, c.NameEN, c.Group, c.Calculator, c.Unit, c.Modes), c.OccurredAt)
}

func (m *Module) DeactivateChargeCode(ctx context.Context, c api.DeactivateChargeCode) (api.Receipt, error) {
	return m.changeCode(ctx, c.TenantID, c.ActorID, c.CommandID, "DEACTIVATE", c.Code, c.ExpectedVersion, domain.ChargeCodeInput{}, c.OccurredAt)
}

func (m *Module) ReactivateChargeCode(ctx context.Context, c api.ReactivateChargeCode) (api.Receipt, error) {
	return m.changeCode(ctx, c.TenantID, c.ActorID, c.CommandID, "REACTIVATE", c.Code, c.ExpectedVersion, domain.ChargeCodeInput{}, c.OccurredAt)
}

func (m *Module) CreateRateCard(ctx context.Context, c api.CreateRateCard) (api.Receipt, error) {
	return cardReceipt(m.cardDeps(c.TenantID, c.CommandID).CreateRateCard(ctx, application.CreateRateCardCommand{CommandID: c.CommandID,
		Scope: scope(c.TenantID, c.ActorID), OccurredAt: stored(c.OccurredAt), Card: domain.RateCardDraft{RateCardID: domain.RateCardID(c.RateCardID),
			Side: domain.Side(c.Side), Scope: domain.CardScope(c.Scope), LegalEntityID: domain.LegalEntityID(c.LegalEntityID), PartyID: c.PartyID,
			Name: c.Name}}))
}

func (m *Module) DraftRateCardVersion(ctx context.Context, c api.DraftRateCardVersion) (api.Receipt, error) {
	return cardReceipt(m.cardDeps(c.TenantID, c.CommandID).DraftRateCardVersion(ctx, application.DraftVersionCommand{CommandID: c.CommandID,
		Scope: scope(c.TenantID, c.ActorID), RateCardID: domain.RateCardID(c.RateCardID), ExpectedRevision: c.ExpectedRevision, CopyFrom: c.CopyFrom,
		Version: domain.VersionInput{Validity: domain.VersionValidity{From: stored(c.ValidFrom), Until: stored(c.ValidUntil)}, Currency: c.Currency,
			SourceRef: c.SourceRef, FilingRef: c.FilingRef}, OccurredAt: stored(c.OccurredAt)}))
}

// Defaults of OD-8 for a PER_UNIT line that leaves them zero, by the entry's mode: AIR chargeable weight at 6000 cm3
// per kg in steps of 0.5 kg; SEA chargeable weight at 1 t = 1 m3 (1000 cm3 per kg) and W/M at 1 t = 1 m3, at least
// 1 W/M.
func lineDefaults(l domain.RateLine, mode domain.Mode) domain.RateLine {
	if l.Calculator != domain.CalcPerUnit {
		return l
	}
	switch l.Unit {
	case domain.UnitKgChargeable:
		if mode == domain.ModeSea {
			l.Step, l.VolumeFactor = cmpOr(l.Step, 1), cmpOr(l.VolumeFactor, 1000)
			break
		}
		l.Step, l.VolumeFactor = cmpOr(l.Step, 500), cmpOr(l.VolumeFactor, 6000)
	case domain.UnitFreightTonne:
		l.Step, l.VolumeFactor, l.MinQuantity = cmpOr(l.Step, 1), cmpOr(l.VolumeFactor, 1_000_000), cmpOr(l.MinQuantity, 1000)
	default:
		l.Step = cmpOr(l.Step, 1)
	}
	return l
}

func cmpOr(v, d int64) int64 {
	if v == 0 {
		return d
	}
	return v
}

func toEntries(in []api.RateEntry) []domain.RateEntry {
	out := make([]domain.RateEntry, len(in))
	for i, e := range in {
		out[i] = domain.RateEntry{No: i + 1, Mode: domain.Mode(e.Mode), ContainerMode: domain.ContainerMode(e.ContainerMode), Origin: e.Origin,
			Destination: e.Destination, Via: e.Via, Carrier: e.Carrier, ServiceLevel: e.ServiceLevel, Commodity: e.Commodity, TransitDays: e.TransitDays,
			Frequency: e.Frequency}
		out[i].Validity.From, out[i].Validity.Until = stored(e.ValidFrom), stored(e.ValidUntil)
		for j, l := range e.Lines {
			line := domain.RateLine{No: j + 1, ChargeCode: domain.ChargeCode(l.ChargeCode), Currency: l.Currency, Calculator: domain.Calculator(l.Calculator),
				Unit: domain.RateUnit(l.Unit), Rate: domain.UnitRate{Minor: l.Rate, Scale: l.RateScale}, Minimum: l.Minimum, Maximum: l.Maximum, Step: l.Step,
				MinQuantity: l.MinQuantity, VolumeFactor: l.VolumeFactor, PercentBP: l.PercentBP, BaseCode: domain.ChargeCode(l.BaseCode),
				CallForPricing: l.CallForPricing}
			for _, b := range l.Breaks {
				line.Breaks = append(line.Breaks, domain.RateBreak{From: b.From, Rate: b.Rate})
			}
			out[i].Lines = append(out[i].Lines, lineDefaults(line, out[i].Mode))
		}
	}
	return out
}

func (m *Module) SetRateCardEntries(ctx context.Context, c api.SetRateCardEntries) (api.Receipt, error) {
	return cardReceipt(m.cardDeps(c.TenantID, c.CommandID).SetRateCardEntries(ctx, application.SetEntriesCommand{CommandID: c.CommandID,
		Scope: scope(c.TenantID, c.ActorID), RateCardID: domain.RateCardID(c.RateCardID), ExpectedRevision: c.ExpectedRevision, Version: c.Version,
		Entries: toEntries(c.Entries), OccurredAt: stored(c.OccurredAt)}))
}

func (m *Module) PublishRateCardVersion(ctx context.Context, c api.PublishRateCardVersion) (api.Receipt, error) {
	return cardReceipt(m.cardDeps(c.TenantID, c.CommandID).PublishRateCardVersion(ctx, application.PublishVersionCommand{CommandID: c.CommandID,
		Scope: scope(c.TenantID, c.ActorID), RateCardID: domain.RateCardID(c.RateCardID), ExpectedRevision: c.ExpectedRevision, Version: c.Version,
		OccurredAt: stored(c.OccurredAt)}))
}

func (m *Module) WithdrawRateCardVersion(ctx context.Context, c api.WithdrawRateCardVersion) (api.Receipt, error) {
	return cardReceipt(m.cardDeps(c.TenantID, c.CommandID).WithdrawRateCardVersion(ctx, application.WithdrawVersionCommand{CommandID: c.CommandID,
		Scope: scope(c.TenantID, c.ActorID), RateCardID: domain.RateCardID(c.RateCardID), ExpectedRevision: c.ExpectedRevision, Version: c.Version,
		Reason: c.Reason, OccurredAt: stored(c.OccurredAt)}))
}

func (m *Module) EvaluateRates(ctx context.Context, c api.EvaluateRates) (api.Receipt, error) {
	if c.Purpose != api.PurposeQuoteLine {
		return api.Receipt{}, fmt.Errorf("%w: %w: EvaluateRates rates quote lines; the estimates for FIN come with PricingBasis v1", api.ErrRejected,
			domain.ErrInvalidEvaluation)
	}
	in := domain.EvaluationInput{Purpose: domain.SnapshotPurpose(c.Purpose), LegalEntityID: domain.LegalEntityID(c.LegalEntityID),
		Customer: domain.CustomerRef{PartyID: c.CustomerPartyID}, Carrier: c.Carrier, Mode: domain.Mode(c.Mode), ContainerMode: domain.ContainerMode(c.ContainerMode),
		Origin: c.Origin, Destination: c.Destination, Via: c.Via, ServiceLevel: c.ServiceLevel, Commodity: c.Commodity,
		Cargo:       domain.Cargo{Pieces: c.Cargo.Pieces, GrossGrams: c.Cargo.GrossGrams, VolumeCM3: c.Cargo.VolumeCM3, Documents: c.Cargo.Documents},
		SubjectKind: c.SubjectKind, SubjectID: c.SubjectID, SubjectVersion: c.SubjectVersion}
	for _, side := range c.Sides {
		in.Sides = append(in.Sides, domain.Side(side))
	}
	return cardReceipt(m.cardDeps(c.TenantID, c.CommandID).EvaluateRates(ctx, application.EvaluateRatesCommand{CommandID: c.CommandID,
		Scope: scope(c.TenantID, c.ActorID), SnapshotID: c.SnapshotID, Input: in, OccurredAt: stored(c.OccurredAt)}))
}

// reader runs read in one read-only snapshot of the tenant.
func (m *Module) reader(ctx context.Context, tenant string, read func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("%w: no tenant", api.ErrUnauthenticated)
	}
	if err := (bento.TenantTransactor{DB: m.db, Tenant: tenant, Snapshot: true}).WithinTransaction(ctx, read); err != nil {
		return classify(err)
	}
	return nil
}

func codeView(e domain.ChargeCodeEntry) api.ChargeCode {
	v := e.Current
	out := api.ChargeCode{Code: string(e.Code), NameZH: v.NameZH, NameEN: v.NameEN, Group: string(v.Group), Calculator: string(v.Calculator),
		Unit: string(v.Unit), Active: v.Active, Version: v.Version, SetBy: v.SetBy, SetAt: v.SetAt}
	for _, mode := range v.Modes {
		out.Modes = append(out.Modes, string(mode))
	}
	return out
}

// GetChargeCode answers a code of the tenant's catalogue; a code not in it is ErrNotFound.
func (m *Module) GetChargeCode(ctx context.Context, q api.GetChargeCode) (api.ChargeCode, error) {
	var out api.ChargeCode
	err := m.reader(ctx, q.TenantID, func(txCtx context.Context) error {
		c, err := postgres.ChargeCodeStore{DB: m.db}.Load(txCtx, domain.TenantID(q.TenantID), domain.ChargeCode(q.Code))
		if err != nil {
			return err
		}
		if !c.Defined() {
			return fmt.Errorf("%w: charge code %q is not in the catalogue", api.ErrNotFound, q.Code)
		}
		out = codeView(domain.ChargeCodeEntry{Code: c.Code(), Current: c.Current()})
		return nil
	})
	return out, err
}

func (m *Module) ListChargeCodes(ctx context.Context, q api.ListChargeCodes) ([]api.ChargeCode, error) {
	out := []api.ChargeCode{}
	err := m.reader(ctx, q.TenantID, func(txCtx context.Context) error {
		codes, err := postgres.ChargeCodeStore{DB: m.db}.List(txCtx, domain.TenantID(q.TenantID))
		for _, c := range codes {
			if c.Current.Active || q.IncludeInactive {
				out = append(out, codeView(c))
			}
		}
		return err
	})
	return out, err
}

func headerView(v domain.RateCardVersion) api.RateCardVersionHeader {
	return api.RateCardVersionHeader{Version: v.No, Status: string(v.Status), ValidFrom: v.Validity.From, ValidUntil: v.Validity.Until, Currency: v.Currency,
		SourceRef: v.SourceRef, FilingRef: v.FilingRef, Preparers: v.Preparers, PreparedAt: v.PreparedAt, PublishedBy: v.PublishedBy,
		PublishedAt: v.PublishedAt, ApprovedBy: v.ApprovedBy, SelfApproved: v.SelfApproved, WithdrawnBy: v.WithdrawnBy, WithdrawnAt: v.WithdrawnAt,
		WithdrawReason: v.WithdrawReason}
}

func cardView(st domain.RateCardState, revision int64) api.RateCard {
	out := api.RateCard{RateCardID: string(st.RateCardID), Side: string(st.Side), Scope: string(st.Scope), LegalEntityID: string(st.LegalEntityID),
		PartyID: st.PartyID, PartyRevision: st.PartyRevision, Name: st.Name, CreatedBy: st.CreatedBy, CreatedAt: st.CreatedAt, Revision: revision,
		Versions: []api.RateCardVersionHeader{}}
	for _, v := range st.Versions {
		out.Versions = append(out.Versions, headerView(v))
	}
	return out
}

func (m *Module) GetRateCard(ctx context.Context, q api.GetRateCard) (api.RateCard, error) {
	var out api.RateCard
	err := m.reader(ctx, q.TenantID, func(txCtx context.Context) error {
		c, revision, err := postgres.RateCardStore{DB: m.db}.Read(txCtx, domain.TenantID(q.TenantID), domain.RateCardID(q.RateCardID))
		if err == nil {
			out = cardView(c.State(), revision)
		}
		return err
	})
	return out, err
}

func (m *Module) GetRateCardVersion(ctx context.Context, q api.GetRateCardVersion) (api.RateCardVersion, error) {
	var out api.RateCardVersion
	err := m.reader(ctx, q.TenantID, func(txCtx context.Context) error {
		store := postgres.RateCardStore{DB: m.db}
		c, revision, err := store.Read(txCtx, domain.TenantID(q.TenantID), domain.RateCardID(q.RateCardID))
		if err != nil {
			return err
		}
		i := slices.IndexFunc(c.State().Versions, func(v domain.RateCardVersion) bool { return v.No == q.Version })
		if i < 0 {
			return fmt.Errorf("%w: card %s has no version %d", api.ErrNotFound, q.RateCardID, q.Version)
		}
		entries, err := store.Entries(txCtx, domain.TenantID(q.TenantID), domain.RateCardID(q.RateCardID), q.Version)
		if err != nil {
			return err
		}
		out = api.RateCardVersion{RateCardID: q.RateCardID, CardRevision: revision, RateCardVersionHeader: headerView(c.State().Versions[i]),
			Entries: []api.RateEntry{}}
		for _, e := range entries {
			out.Entries = append(out.Entries, entryView(e))
		}
		return nil
	})
	return out, err
}

func entryView(e domain.RateEntry) api.RateEntry {
	out := api.RateEntry{No: e.No, Mode: string(e.Mode), ContainerMode: string(e.ContainerMode), Origin: e.Origin, Destination: e.Destination, Via: e.Via,
		Edition: e.Edition, Carrier: e.Carrier, ServiceLevel: e.ServiceLevel, Commodity: e.Commodity, TransitDays: e.TransitDays, Frequency: e.Frequency,
		ValidFrom: e.Validity.From, ValidUntil: e.Validity.Until}
	for _, l := range e.Lines {
		line := api.RateLine{No: l.No, ChargeCode: string(l.ChargeCode), Currency: l.Currency, Calculator: string(l.Calculator), Unit: string(l.Unit),
			Rate: l.Rate.Minor, RateScale: l.Rate.Scale, Minimum: l.Minimum, Maximum: l.Maximum, Step: l.Step, MinQuantity: l.MinQuantity,
			VolumeFactor: l.VolumeFactor, PercentBP: l.PercentBP, BaseCode: string(l.BaseCode), CallForPricing: l.CallForPricing}
		for _, b := range l.Breaks {
			line.Breaks = append(line.Breaks, api.RateBreak{From: b.From, Rate: b.Rate})
		}
		out.Lines = append(out.Lines, line)
	}
	return out
}

func (m *Module) ListRateCards(ctx context.Context, q api.ListRateCards) ([]api.RateCard, error) {
	out := []api.RateCard{}
	err := m.reader(ctx, q.TenantID, func(txCtx context.Context) error {
		cards, revisions, err := postgres.RateCardStore{DB: m.db}.List(txCtx, domain.TenantID(q.TenantID), application.CardFilter{Side: domain.Side(q.Side),
			Scope: domain.CardScope(q.Scope), LegalEntityID: domain.LegalEntityID(q.LegalEntityID), PartyID: q.PartyID, ValidAt: stored(q.ValidAt)})
		for i, c := range cards {
			out = append(out, cardView(c, revisions[i]))
		}
		return err
	})
	return out, err
}

func (m *Module) GetRateSnapshot(ctx context.Context, q api.GetRateSnapshot) (api.RateSnapshot, error) {
	var out api.RateSnapshot
	err := m.reader(ctx, q.TenantID, func(txCtx context.Context) error {
		s, err := postgres.SnapshotStore{DB: m.db}.Read(txCtx, domain.TenantID(q.TenantID), q.SnapshotID)
		if err != nil {
			return err
		}
		st := s.State()
		in := st.Input
		out = api.RateSnapshot{SnapshotID: st.SnapshotID, Purpose: string(in.Purpose), LegalEntityID: string(in.LegalEntityID),
			Customer: api.CustomerRef{PartyID: in.Customer.PartyID, PartyRevision: in.Customer.PartyRevision}, Carrier: in.Carrier, Mode: string(in.Mode),
			ContainerMode: string(in.ContainerMode), Origin: in.Origin, Destination: in.Destination, Via: in.Via, Edition: in.Edition,
			ServiceLevel: in.ServiceLevel, Commodity: in.Commodity, At: in.At, Cargo: api.Cargo{Pieces: in.Cargo.Pieces, GrossGrams: in.Cargo.GrossGrams,
				VolumeCM3: in.Cargo.VolumeCM3, Documents: in.Cargo.Documents}, SubjectKind: in.SubjectKind, SubjectID: in.SubjectID,
			SubjectVersion: in.SubjectVersion, EngineVersion: st.EngineVersion, EvaluatedBy: st.EvaluatedBy, RecordedAt: st.RecordedAt,
			Candidates: []api.RateCandidate{}, Lines: []api.RateSnapshotLine{}}
		for _, side := range in.Sides {
			out.Sides = append(out.Sides, string(side))
		}
		for _, c := range st.Candidates {
			out.Candidates = append(out.Candidates, api.RateCandidate{Side: string(c.Side), RateCardID: string(c.RateCardID), Version: c.Version,
				EntryNo: c.EntryNo, Taken: c.Taken, Reason: c.Reason, Total: c.Total})
		}
		for _, l := range st.Lines {
			out.Lines = append(out.Lines, api.RateSnapshotLine{LineNo: l.LineNo, Side: string(l.Side), ChargeCode: string(l.ChargeCode),
				RateCardID: string(l.RateCardID), Version: l.Version, EntryNo: l.EntryNo, RateLineNo: l.RateLineNo, Calculator: string(l.Calculator),
				Unit: string(l.Unit), Quantity: l.Quantity, Charged: l.Charged, BandFrom: l.BandFrom, Rate: l.Rate.Minor, RateScale: l.Rate.Scale,
				PercentBP: l.PercentBP, BaseAmount: l.BaseAmount, MinimumApplied: l.MinimumApplied, MaximumApplied: l.MaximumApplied, Amount: l.Amount,
				Gap: string(l.Gap)})
		}
		return nil
	})
	return out, err
}
