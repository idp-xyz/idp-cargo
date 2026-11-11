package application

import (
	"context"
	"fmt"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
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

func (d Deps) policy(ctx context.Context, tenant fin.TenantID, legalEntity string) (*fin.FinancePolicy, error) {
	p, _, err := d.Policies.Read(ctx, tenant, legalEntity)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrNoPolicy, legalEntity, err)
	}
	return p, nil
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
// under its legal entity's policy.
func (d Deps) change(ctx context.Context, c Command, ask func(context.Context, *fin.FreightJob) error,
	apply func(*fin.FreightJob, *fin.FinancePolicy) (int, error)) (Receipt, error) {
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
		version, err := apply(j, p)
		if err != nil {
			return Receipt{}, nil, err
		}
		j.Stamp(entered)
		if revision, err = d.Jobs.Save(txCtx, j, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: version}, j, nil
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
	}, func(j *fin.FreightJob, p *fin.FinancePolicy) (int, error) {
		b := fin.Basis{BasisID: basis.BasisID, QuoteID: basis.QuoteID, QuoteRevision: basis.QuoteRevision, OrderID: order}
		if err := j.ApplyBasis(b, basis.CustomerPartyID, basis.CustomerRevision, basis.Lines, p, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, err
		}
		return len(j.State().Bases), nil
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
	}, func(j *fin.FreightJob, p *fin.FinancePolicy) (int, error) {
		if err := j.RecordLine(in.ChargeCode, in.Description, in.Sell, in.Cost, in.Reason, p, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, err
		}
		return len(j.State().Lines), nil
	})
}

// ReviseInput is a side revision as asked; an empty sell party keeps the debtor.
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
		if in.Side == fin.SideSell && in.Value.PartyID == "" && in.LineNo >= 1 && in.LineNo <= len(st.Lines) {
			if sell := st.Lines[in.LineNo-1].Sell; len(sell) > 0 {
				in.Value.PartyID, in.Value.PartyRevision = sell[len(sell)-1].PartyID, sell[len(sell)-1].PartyRevision
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
	}, func(j *fin.FreightJob, p *fin.FinancePolicy) (int, error) {
		if err := j.ReviseSide(in.LineNo, in.Side, in.Expected, in.Value, in.Reason, p, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, err
		}
		return in.Expected + 1, nil
	})
}

// CancelJob cancels an open job with a reason.
func (d Deps) CancelJob(ctx context.Context, c Command, reason string) (Receipt, error) {
	return d.change(ctx, c, nil, func(j *fin.FreightJob, _ *fin.FinancePolicy) (int, error) {
		return 0, j.Cancel(reason, c.Scope.ActorID, c.OccurredAt)
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

// Exposure is a debtor's estimated sell on the open jobs of a legal entity.
func (d Deps) Exposure(ctx context.Context, tenant fin.TenantID, legalEntity, debtor string) (int64, error) {
	jobs, err := d.Jobs.OpenOf(ctx, tenant, legalEntity, "")
	if err != nil {
		return 0, err
	}
	var sum int64
	for _, j := range jobs {
		sum += j.SellEstimate(debtor)
	}
	return sum, nil
}

// Reader runs reads in one snapshot each.
func (d Deps) Reader(ctx context.Context, fn func(context.Context) error) error {
	if d.Tx == nil {
		return ErrMissingDependency
	}
	return d.read(ctx, fn)
}
