package application

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/numbering"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Party roles FIN asks ORG about: a debtor bills as customer or overseas agent, a creditor supplies as carrier,
// co-loader or overseas agent.
var (
	DebtorRoles   = []string{"CUSTOMER", "OVERSEAS_AGENT"}
	CreditorRoles = []string{"CARRIER", "CO_LOADER", "OVERSEAS_AGENT"}
)

// OpenPolicy opens a legal entity's finance policy; ORG must report the legal entity VERIFIED (T3).
func (d Deps) OpenPolicy(ctx context.Context, c Command, currency kernel.CurrencyCode, book string) (Receipt, error) {
	if d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = d.verified(ctx, c.Scope.TenantID, c.Subject); err != nil {
		return Receipt{}, err
	}
	return d.write(ctx, c, hash, "FinancePolicy", func(txCtx context.Context) (Receipt, aggregate, error) {
		p, err := fin.OpenPolicy(c.Scope.TenantID, c.Subject, currency, book, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		p.Stamp(entered)
		revision, err := d.Policies.Insert(txCtx, p)
		return Receipt{SubjectID: c.Subject, NewRevision: revision}, p, err
	})
}

func (d Deps) verified(ctx context.Context, tenant fin.TenantID, legalEntity string) error {
	ok, err := d.Directory.LegalEntityVerified(ctx, tenant, legalEntity)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotVerified
	}
	return nil
}

// RecordRate adds an exchange rate entry to a legal entity's policy, under the policy's lock.
func (d Deps) RecordRate(ctx context.Context, c Command, e fin.RateEntry) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.write(ctx, c, hash, "FinancePolicy", func(txCtx context.Context) (Receipt, aggregate, error) {
		p, revision, err := d.Policies.Load(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, fin.ErrConflict
		}
		if err = p.RecordRate(e, c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, nil, err
		}
		p.Stamp(entered)
		if revision, err = d.Policies.Save(txCtx, p, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: len(p.State().Rates)}, p, nil
	})
}

// changePolicy applies a change to the locked policy of the legal entity named by the command; after, when given, runs
// in the same transaction once the policy is saved.
func (d Deps) changePolicy(ctx context.Context, c Command, apply func(context.Context, *fin.FinancePolicy) (int, error),
	after ...func(context.Context, *fin.FinancePolicy) error) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.write(ctx, c, hash, "FinancePolicy", func(txCtx context.Context) (Receipt, aggregate, error) {
		p, revision, err := d.Policies.Load(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, fin.ErrConflict
		}
		version, err := apply(txCtx, p)
		if err != nil {
			return Receipt{}, nil, err
		}
		p.Stamp(entered)
		if revision, err = d.Policies.Save(txCtx, p, revision); err != nil {
			return Receipt{}, nil, err
		}
		for _, fn := range after {
			if err = fn(txCtx, p); err != nil {
				return Receipt{}, nil, err
			}
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: version}, p, nil
	})
}

// SetTax records the next tax treatment version of a charge code in the legal entity's policy.
func (d Deps) SetTax(ctx context.Context, c Command, t fin.TaxTreatment) (Receipt, error) {
	return d.changePolicy(ctx, c, func(_ context.Context, p *fin.FinancePolicy) (int, error) {
		if err := p.SetTaxTreatment(t, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, err
		}
		return len(p.State().Taxes), nil
	})
}

// SeriesAsk is a numbering series as asked: the note kind, the template, the first number and, for a template that
// resets, a date of the period the first number belongs to (needed when the first number is above 1).
type SeriesAsk struct {
	Kind            string
	Template        string
	FirstNumber     int64
	FirstPeriodDate time.Time
}

// notePattern is what a note number may hold (debtor_invoice.number: five to twenty characters).
var notePattern = regexp.MustCompile(`^[0-9A-Z-]{5,20}$`)

// RegisterSeries fixes the legal entity's numbering of a note kind: a template of the platform's language in Cargo's
// subset whose numbers are capital letters, digits and hyphens, five to twenty characters, and the first number, which
// seeds the platform's sequence in the same transaction: of the template's only sequence, or, for a template that
// resets, of the period of the date given (notes take the period of their invoice dates, which FIN cannot derive from
// a registration's instant without the legal entity's time zone).
func (d Deps) RegisterSeries(ctx context.Context, c Command, in SeriesAsk) (Receipt, error) {
	if d.Numbers == nil {
		return Receipt{}, ErrMissingDependency
	}
	var seed string
	return d.changePolicy(ctx, c, func(_ context.Context, p *fin.FinancePolicy) (int, error) {
		tmpl, err := numbering.Parse(in.Template)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", fin.ErrInvalid, err)
		}
		longest, err := tmpl.Longest()
		if err != nil || !notePattern.MatchString(longest) {
			return 0, fmt.Errorf("%w: %s renders %q; a note number is 5 to 20 capital letters, digits and hyphens", fin.ErrInvalid, in.Template, longest)
		}
		period := ""
		switch {
		case !tmpl.Resets() && !in.FirstPeriodDate.IsZero():
			return 0, fmt.Errorf("%w: %s never resets; a first period date is for a template that does", fin.ErrInvalid, in.Template)
		case !tmpl.Resets():
		case !in.FirstPeriodDate.IsZero():
			period = tmpl.Period(in.FirstPeriodDate)
		case in.FirstNumber > 1:
			return 0, fmt.Errorf("%w: %s restarts by date; a first number above 1 names the date of its period", fin.ErrInvalid, in.Template)
		}
		if _, err = tmpl.Render(in.FirstNumber, c.OccurredAt); err != nil {
			return 0, fmt.Errorf("%w: first number %d of %s: %v", fin.ErrInvalid, in.FirstNumber, in.Template, err)
		}
		s := fin.Series{Kind: in.Kind, Prefix: tmpl.Prefix(), Template: in.Template, FirstNumber: in.FirstNumber, SeedPeriod: period}
		if err := p.RegisterSeries(s, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, err
		}
		seed = period
		return len(p.State().Series), nil
	}, func(txCtx context.Context, p *fin.FinancePolicy) error {
		// the sequence's seed needs its series saved (the sequence table admits rows of registered series only)
		return d.Numbers.Seed(txCtx, string(c.Scope.TenantID), numbering.DocType(in.Kind, p.State().LegalEntityID), seed, in.FirstNumber-1)
	})
}

// SetProfile records the next version of a debtor's billing profile from ORG's identity of the debtor, read first
// (T3); ExpectedRevision is the version the caller read, 0 for a new profile.
func (d Deps) SetProfile(ctx context.Context, c Command, in fin.ProfileInput) (Receipt, error) {
	if d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	id, err := d.Directory.PartyIdentity(ctx, c.Scope.TenantID, c.Subject)
	if err != nil {
		return Receipt{}, err
	}
	return d.write(ctx, c, hash, "BillingProfile", func(txCtx context.Context) (Receipt, aggregate, error) {
		b, found, err := d.Profiles.Load(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		if !found {
			if b, err = fin.NewProfile(c.Scope.TenantID, c.Subject); err != nil {
				return Receipt{}, nil, err
			}
		}
		if int64(len(b.State().Versions)) != c.ExpectedRevision {
			return Receipt{}, nil, fin.ErrConflict
		}
		if err = b.Set(in, id, c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, nil, err
		}
		b.Stamp(entered)
		if err = d.Profiles.Save(txCtx, b); err != nil {
			return Receipt{}, nil, err
		}
		n := len(b.State().Versions)
		return Receipt{SubjectID: c.Subject, NewRevision: int64(n), Version: n}, b, nil
	})
}

// draw takes numbers from the platform's sequence of the legal entity's series in the command's transaction, in the
// period of the note's date: a failed command takes none.
func (d Deps) draw(ctx context.Context, tenant fin.TenantID, p *fin.FinancePolicy) fin.Draw {
	return func(kind string, date time.Time) (string, int64, string, error) {
		s, ok := p.SeriesOf(kind)
		if !ok {
			return "", 0, "", fmt.Errorf("%w: %s of %s", fin.ErrNoSeries, kind, p.State().LegalEntityID)
		}
		tmpl, err := numbering.Parse(s.Template)
		if err != nil {
			return "", 0, "", fmt.Errorf("%w: stored template %s: %v", fin.ErrInvalid, s.Template, err)
		}
		period := tmpl.Period(date)
		seq, err := d.Numbers.Next(ctx, string(tenant), numbering.DocType(kind, p.State().LegalEntityID), period)
		if err != nil {
			return "", 0, "", err
		}
		number, err := tmpl.Render(seq, date)
		if errors.Is(err, numbering.ErrExhausted) {
			return "", 0, "", fmt.Errorf("%w: %v", fin.ErrSeriesExhausted, err)
		}
		return number, seq, period, err
	}
}

// DebitNoteAsk is a debit note as asked.
type DebitNoteAsk struct {
	ID          string
	Debtor      string
	LineNos     []int
	Currency    kernel.CurrencyCode
	InvoiceDate time.Time
}

// IssueDebitNote bills sell sides of the job to a debtor. Before FIN's transaction (T3) DOC reports the shipment's
// issued house document (TransportDocumentSnapshot v1) and ORG confirms the debtor in a debtor role for the legal
// entity and answers its identity; in the transaction FIN takes the debtor's billing profile, which must still name it
// as ORG does, the terms of its credit account and the next number.
func (d Deps) IssueDebitNote(ctx context.Context, c Command, in DebitNoteAsk) (Receipt, error) {
	if d.Directory == nil || d.Documents == nil {
		return Receipt{}, ErrMissingDependency
	}
	var document HouseDocument
	var revision int64
	var identity fin.PartyIdentity
	return d.change(ctx, c, func(ctx context.Context, j *fin.FreightJob) (err error) {
		st := j.State()
		if document, err = d.Documents.HouseDocument(ctx, c.Scope.TenantID, st.ShipmentID, c.OccurredAt); err != nil {
			return err
		}
		switch {
		case document.ShipmentID != st.ShipmentID || document.LegalEntityID != st.LegalEntityID:
			return fmt.Errorf("%w: DOC reports house document %s of %s for %s", fin.ErrNoIssuedDocument, document.Number, document.LegalEntityID,
				document.ShipmentID)
		case c.OccurredAt.Before(document.IssuedAt):
			return fmt.Errorf("%w: house document %s was issued at %s, after the note's business time", fin.ErrNoIssuedDocument, document.Number,
				document.IssuedAt.Format(time.RFC3339))
		}
		if revision, err = d.Directory.PartyInRole(ctx, c.Scope.TenantID, in.Debtor, st.LegalEntityID, DebtorRoles, c.OccurredAt); err != nil {
			return err
		}
		identity, err = d.Directory.PartyIdentity(ctx, c.Scope.TenantID, in.Debtor)
		return err
	}, func(txCtx context.Context, j *fin.FreightJob, p *fin.FinancePolicy) (Receipt, error) {
		st := j.State()
		profile, found, err := d.Profiles.Load(txCtx, c.Scope.TenantID, in.Debtor)
		if err != nil {
			return Receipt{}, err
		}
		if !found {
			return Receipt{}, fmt.Errorf("%w: %s", fin.ErrNoBillingProfile, in.Debtor)
		}
		version, _ := profile.Current()
		if !version.Matches(identity) {
			return Receipt{}, fmt.Errorf("%w: %s, version %d", fin.ErrProfileStale, in.Debtor, version.No)
		}
		var terms *fin.TermsVersion
		account, _, err := d.Credit.Read(txCtx, c.Scope.TenantID, st.LegalEntityID, in.Debtor)
		switch {
		case err == nil:
			if t, ok := account.CurrentTerms(); ok {
				terms = &t
			}
		case !errors.Is(err, bentorepo.ErrNotFound):
			return Receipt{}, err
		}
		n := fin.DebitNoteInput{ID: in.ID, DebtorPartyID: in.Debtor, DebtorRevision: revision, LineNos: in.LineNos, Currency: in.Currency,
			InvoiceDate: in.InvoiceDate, Profile: version, Terms: terms, Document: document.DocumentRef}
		if err = j.IssueDebitNote(n, p, d.draw(txCtx, c.Scope.TenantID, p), c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, err
		}
		notes := j.State().Notes
		return Receipt{Number: notes[len(notes)-1].Number}, nil
	})
}

// IssueCreditNote reverses lines of a debit note of the job.
func (d Deps) IssueCreditNote(ctx context.Context, c Command, in fin.CreditNoteInput) (Receipt, error) {
	return d.change(ctx, c, nil, func(txCtx context.Context, j *fin.FreightJob, p *fin.FinancePolicy) (Receipt, error) {
		if err := j.IssueCreditNote(in, p, d.draw(txCtx, c.Scope.TenantID, p), c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, err
		}
		notes := j.State().Notes
		return Receipt{Number: notes[len(notes)-1].Number}, nil
	})
}

// policy reads the legal entity's policy in a write transaction, holding it FOR SHARE.
func (d Deps) policy(ctx context.Context, tenant fin.TenantID, legalEntity string) (*fin.FinancePolicy, error) {
	p, found, err := d.Policies.Shared(ctx, tenant, legalEntity)
	if err == nil && !found {
		return nil, fmt.Errorf("%w: %s", ErrNoPolicy, legalEntity)
	}
	return p, err
}

// EndRate ends a rate entry's period earlier, once, under the policy's lock.
func (d Deps) EndRate(ctx context.Context, c Command, no int, until time.Time, reason string) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.write(ctx, c, hash, "FinancePolicy", func(txCtx context.Context) (Receipt, aggregate, error) {
		p, revision, err := d.Policies.Load(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, fin.ErrConflict
		}
		if err = p.EndRate(no, until, reason, c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, nil, err
		}
		p.Stamp(entered)
		if revision, err = d.Policies.Save(txCtx, p, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: no}, p, nil
	})
}

// OpenJob opens the job of a shipment: FIN reads FUL's CostDriver v1 and ORG's legal entity first (T3).
func (d Deps) OpenJob(ctx context.Context, c Command, shipment string) (Receipt, error) {
	if d.Shipments == nil || d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	driver, err := d.Shipments.CostDriver(ctx, c.Scope.TenantID, shipment)
	if err != nil {
		return Receipt{}, err
	}
	kind := ""
	switch driver.TransportMode {
	case "AIR":
		kind = fin.KindAirExportHouse
	case "SEA":
		kind = fin.KindSeaExportLCLHouse
	}
	if (driver.Status != "DRAFT" && driver.Status != "PLANNED") || kind == "" || driver.ShipmentID != shipment {
		return Receipt{}, fmt.Errorf("%w: FUL reports shipment %s %s by %q", fin.ErrNotOpenable, shipment, driver.Status, driver.TransportMode)
	}
	if err = d.verified(ctx, c.Scope.TenantID, driver.LegalEntityID); err != nil {
		return Receipt{}, err
	}
	return d.write(ctx, c, hash, "FreightJob", func(txCtx context.Context) (Receipt, aggregate, error) {
		p, err := d.policy(txCtx, c.Scope.TenantID, driver.LegalEntityID)
		if err != nil {
			return Receipt{}, nil, err
		}
		j, err := fin.OpenJob(fin.JobState{TenantID: c.Scope.TenantID, JobID: c.Subject, LegalEntityID: driver.LegalEntityID, ShipmentID: shipment,
			Kind: kind, CustomerPartyID: driver.CustomerPartyID, PlanVersion: driver.PlanVersion}, p, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		j.Stamp(entered)
		revision, err := d.Jobs.Insert(txCtx, j)
		return Receipt{SubjectID: c.Subject, NewRevision: revision}, j, err
	})
}

// change reads the job before FIN's transaction for the T3 questions, then applies the change to the locked job
// under its legal entity's policy (held FOR SHARE); apply answers the version and the number of a note it issued.
func (d Deps) change(ctx context.Context, c Command, ask func(context.Context, *fin.FreightJob) error,
	apply func(context.Context, *fin.FreightJob, *fin.FinancePolicy) (Receipt, error)) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	if ask != nil {
		var current *fin.FreightJob
		if err = d.read(ctx, func(txCtx context.Context) error {
			var revision int64
			current, revision, err = d.Jobs.Read(txCtx, c.Scope.TenantID, c.Subject)
			if err == nil && revision != c.ExpectedRevision {
				return fin.ErrConflict
			}
			return err
		}); err != nil {
			return Receipt{}, err
		}
		if err = ask(ctx, current); err != nil {
			return Receipt{}, err
		}
	}
	return d.write(ctx, c, hash, "FreightJob", func(txCtx context.Context) (Receipt, aggregate, error) {
		j, revision, err := d.Jobs.Load(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, fin.ErrConflict
		}
		p, err := d.policy(txCtx, c.Scope.TenantID, j.State().LegalEntityID)
		if err != nil {
			return Receipt{}, nil, err
		}
		r, err := apply(txCtx, j, p)
		if err != nil {
			return Receipt{}, nil, err
		}
		j.Stamp(entered)
		if revision, err = d.Jobs.Save(txCtx, j, revision); err != nil {
			return Receipt{}, nil, err
		}
		r.SubjectID, r.NewRevision = c.Subject, revision
		return r, j, nil
	})
}

// ApplyBasis applies an accepted quote to the job for one of the shipment's orders: FIN reads FUL's CostDriver v1
// and RATE's PricingBasis v1 first (T3).
func (d Deps) ApplyBasis(ctx context.Context, c Command, quote, order string) (Receipt, error) {
	if d.Shipments == nil || d.Pricing == nil {
		return Receipt{}, ErrMissingDependency
	}
	var basis PricingBasis
	return d.change(ctx, c, func(ctx context.Context, j *fin.FreightJob) error {
		st := j.State()
		driver, err := d.Shipments.CostDriver(ctx, c.Scope.TenantID, st.ShipmentID)
		if err != nil {
			return err
		}
		var held *CostOrder
		for i, o := range driver.Orders {
			if o.OrderID == order && o.QuoteID == quote {
				held = &driver.Orders[i]
			}
		}
		if held == nil || driver.LegalEntityID != st.LegalEntityID || driver.Status == "CANCELLED" {
			return fmt.Errorf("%w: FUL reports no part of order %s under quote %s in shipment %s", fin.ErrNotPriceable, order, quote, st.ShipmentID)
		}
		if basis, err = d.Pricing.QuoteBasis(ctx, c.Scope.TenantID, quote); err != nil {
			return err
		}
		switch {
		case basis.QuoteID != quote || basis.LegalEntityID != st.LegalEntityID:
			return fmt.Errorf("%w: the quote is %s's", fin.ErrNotPriceable, basis.LegalEntityID)
		case basis.QuoteRevision != held.QuoteRevision:
			return fmt.Errorf("%w: FUL planned the order on quote revision %d, RATE answers revision %d", fin.ErrNotPriceable, held.QuoteRevision,
				basis.QuoteRevision)
		}
		return nil
	}, func(_ context.Context, j *fin.FreightJob, p *fin.FinancePolicy) (Receipt, error) {
		b := fin.Basis{BasisID: basis.BasisID, QuoteID: basis.QuoteID, QuoteRevision: basis.QuoteRevision, OrderID: order}
		err := j.ApplyBasis(b, basis.CustomerPartyID, basis.CustomerRevision, basis.Lines, p, c.Scope.ActorID, c.OccurredAt)
		return Receipt{Version: len(j.State().Bases)}, err
	})
}

// LineInput is a manual line as asked.
type LineInput struct {
	ChargeCode  string
	Description string
	Sell        *fin.SideInput
	Cost        *fin.SideInput
	Reason      string
}

// RecordLine adds a manual charge line: ORG confirms the debtor and creditor in their roles first (T3).
func (d Deps) RecordLine(ctx context.Context, c Command, in LineInput) (Receipt, error) {
	if d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	return d.change(ctx, c, func(ctx context.Context, j *fin.FreightJob) (err error) {
		le := j.State().LegalEntityID
		if in.Sell != nil {
			if in.Sell.PartyRevision, err = d.Directory.PartyInRole(ctx, c.Scope.TenantID, in.Sell.PartyID, le, DebtorRoles, c.OccurredAt); err != nil {
				return err
			}
		}
		if in.Cost != nil && in.Cost.PartyID != "" {
			if in.Cost.PartyRevision, err = d.Directory.PartyInRole(ctx, c.Scope.TenantID, in.Cost.PartyID, le, CreditorRoles, c.OccurredAt); err != nil {
				return err
			}
		}
		return nil
	}, func(_ context.Context, j *fin.FreightJob, p *fin.FinancePolicy) (Receipt, error) {
		err := j.RecordLine(in.ChargeCode, in.Description, in.Sell, in.Cost, in.Reason, p, c.Scope.ActorID, c.OccurredAt)
		return Receipt{Version: len(j.State().Lines)}, err
	})
}

// ReviseInput is a side revision as asked; an empty party keeps the side's debtor or creditor.
type ReviseInput struct {
	LineNo   int
	Side     string
	Expected int
	Value    fin.SideInput
	Reason   string
}

// Revise writes the next version of a side: ORG confirms a party it names (T3).
func (d Deps) Revise(ctx context.Context, c Command, in ReviseInput) (Receipt, error) {
	if d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	return d.change(ctx, c, func(ctx context.Context, j *fin.FreightJob) (err error) {
		st := j.State()
		if in.Value.PartyID == "" && in.LineNo >= 1 && in.LineNo <= len(st.Lines) {
			versions := st.Lines[in.LineNo-1].Sell
			if in.Side == fin.SideCost {
				versions = st.Lines[in.LineNo-1].Cost
			}
			if n := len(versions); n > 0 {
				in.Value.PartyID, in.Value.PartyRevision = versions[n-1].PartyID, versions[n-1].PartyRevision
				return nil
			}
		}
		if in.Value.PartyID == "" {
			return nil
		}
		roles := DebtorRoles
		if in.Side == fin.SideCost {
			roles = CreditorRoles
		}
		in.Value.PartyRevision, err = d.Directory.PartyInRole(ctx, c.Scope.TenantID, in.Value.PartyID, st.LegalEntityID, roles, c.OccurredAt)
		return err
	}, func(_ context.Context, j *fin.FreightJob, p *fin.FinancePolicy) (Receipt, error) {
		err := j.ReviseSide(in.LineNo, in.Side, in.Expected, in.Value, in.Reason, p, c.Scope.ActorID, c.OccurredAt)
		return Receipt{Version: in.Expected + 1}, err
	})
}

// CancelJob cancels an open job with a reason.
func (d Deps) CancelJob(ctx context.Context, c Command, reason string) (Receipt, error) {
	return d.change(ctx, c, nil, func(_ context.Context, j *fin.FreightJob, _ *fin.FinancePolicy) (Receipt, error) {
		return Receipt{}, j.Cancel(reason, c.Scope.ActorID, c.OccurredAt)
	})
}

// TermsInput is a terms change as asked.
type TermsInput struct {
	LegalEntityID string
	Debtor        string
	Limit         kernel.Money
	Days          int
	Basis         string
}

// SetTerms opens or changes a debtor's account: ORG confirms the debtor in a debtor role first (T3); the account's
// currency is the legal entity's functional currency.
func (d Deps) SetTerms(ctx context.Context, c Command, in TermsInput) (Receipt, error) {
	if d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	if _, err = d.Directory.PartyInRole(ctx, c.Scope.TenantID, in.Debtor, in.LegalEntityID, DebtorRoles, c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	return d.write(ctx, c, hash, "CreditAccount", func(txCtx context.Context) (Receipt, aggregate, error) {
		a, revision, found, err := d.Credit.Load(txCtx, c.Scope.TenantID, in.LegalEntityID, in.Debtor)
		isNew := err == nil && !found
		switch {
		case err != nil:
			return Receipt{}, nil, err
		case revision != c.ExpectedRevision:
			return Receipt{}, nil, fin.ErrConflict
		case isNew:
			p, err := d.policy(txCtx, c.Scope.TenantID, in.LegalEntityID)
			if err != nil {
				return Receipt{}, nil, err
			}
			if a, err = fin.OpenAccount(c.Scope.TenantID, in.LegalEntityID, in.Debtor, p.State().FunctionalCurrency); err != nil {
				return Receipt{}, nil, err
			}
		}
		if err = a.SetTerms(in.Limit, in.Days, in.Basis, c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, nil, err
		}
		a.Stamp(entered)
		if isNew {
			revision, err = d.Credit.Insert(txCtx, a)
		} else {
			revision, err = d.Credit.Save(txCtx, a, revision)
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: len(a.State().Terms)}, a, err
	})
}

// Hold places or releases a debtor's hold.
func (d Deps) Hold(ctx context.Context, c Command, legalEntity, debtor string, place bool, code, reason string) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.write(ctx, c, hash, "CreditAccount", func(txCtx context.Context) (Receipt, aggregate, error) {
		a, revision, found, err := d.Credit.Load(txCtx, c.Scope.TenantID, legalEntity, debtor)
		if err != nil {
			return Receipt{}, nil, err
		}
		if !found {
			return Receipt{}, nil, fmt.Errorf("%w: %s has no credit account with %s", fin.ErrInvalidTransition, debtor, legalEntity)
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, fin.ErrConflict
		}
		if place {
			err = a.PlaceHold(code, reason, c.Scope.ActorID, c.OccurredAt)
		} else {
			err = a.ReleaseHold(reason, c.Scope.ActorID, c.OccurredAt)
		}
		if err != nil {
			return Receipt{}, nil, err
		}
		a.Stamp(entered)
		if revision, err = d.Credit.Save(txCtx, a, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: len(a.State().Holds)}, a, nil
	})
}

// Exposure is a debtor's sell on the open jobs of a legal entity: not yet billed, and billed on debit notes not
// reversed.
func (d Deps) Exposure(ctx context.Context, tenant fin.TenantID, legalEntity, debtor string) (unbilled, billed int64, err error) {
	jobs, err := d.Jobs.OpenOf(ctx, tenant, legalEntity, "")
	if err != nil {
		return 0, 0, err
	}
	for _, j := range jobs {
		u, b := j.Exposure(debtor)
		unbilled, billed = unbilled+u, billed+b
	}
	return unbilled, billed, nil
}

// Reader runs reads in one snapshot each.
func (d Deps) Reader(ctx context.Context, fn func(context.Context) error) error {
	if d.Tx == nil {
		return ErrMissingDependency
	}
	return d.read(ctx, fn)
}
