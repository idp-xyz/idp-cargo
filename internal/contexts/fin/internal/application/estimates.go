package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
)

// Rated estimates (FB2 and FB3 with RATE V0.5 part 2): FIN asks RATE to rate a job's shipment or a pool's consolidation
// and applies the PricingBasis v1 RATE issues. FUL or PHY, RATE and ORG answer before FIN's transaction (T3).

// EstimateRequest is what FIN asks RATE to rate: the subject (a shipment and its plan version, or a consolidation and
// its revision), the legal entity, the customer of a shipment, the lane, the carrier when known and the cargo.
type EstimateRequest struct {
	CommandID       string
	ActorID         string
	BasisID         string
	LegalEntityID   string
	CustomerPartyID string
	SubjectID       string
	SubjectVersion  int64
	Mode            string
	Origin          string
	Destination     string
	Via             string
	ServiceLevel    string
	Carrier         string
	Pieces          int64
	GrossGrams      int64
	VolumeCM3       int64
	OccurredAt      time.Time
}

// RatedBasis is what FIN reads of a rated PricingBasis v1: its lines (creditors not yet confirmed by ORG). The charges
// RATE could not rate are listed by the basis in RATE, under the basis id the job and the pool show.
type RatedBasis struct {
	BasisID         string
	Purpose         string
	LegalEntityID   string
	CustomerPartyID string
	SubjectID       string
	SubjectVersion  int64
	Lines           []fin.EstimateLine
}

// Rating asks RATE to rate (RATE -> FIN), outside FIN's transactions. A retried FIN command asks again with the same
// command and basis ids, and RATE answers the basis it issued; when the facts FIN reads from FUL or PHY changed in
// between, RATE refuses the command id (an idempotency conflict) and the command is asked again with a new id.
type Rating interface {
	ShipmentEstimate(ctx context.Context, tenant fin.TenantID, r EstimateRequest) (RatedBasis, error)
	ConsolEstimate(ctx context.Context, tenant fin.TenantID, r EstimateRequest) (RatedBasis, error)
}

// EstimateBasisID is the basis id FIN asks RATE to issue for a command: the same for a retried command.
func EstimateBasisID(tenant fin.TenantID, commandID string) string {
	sum := sha256.Sum256([]byte(string(tenant) + "\x00" + commandID))
	return "FIN-" + hex.EncodeToString(sum[:12])
}

// RateJobInput is how a job's shipment is rated: the service level, and optionally the via and the carrier.
type RateJobInput struct {
	ServiceLevel string
	Via          string
	Carrier      string
}

// RateJob rates the job's shipment (SHIPMENT_ESTIMATE): FUL's CostDriver v1 gives the route, cargo, legal entity and
// customer of the shipment as planned; RATE rates both sides; ORG confirms the customer as a debtor and each supplier as
// a creditor. Every rated line becomes a RATED charge line; a job is priced by its quotes or by one rated estimate. A
// shipment carrying an order under an accepted ONE_OFF quote is priced by the quote, not here.
func (d Deps) RateJob(ctx context.Context, c Command, in RateJobInput) (Receipt, error) {
	if d.Shipments == nil || d.Rating == nil || d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	var basis RatedBasis
	var plan int
	var customerRevision int64
	return d.change(ctx, c, func(ctx context.Context, j *fin.FreightJob) error {
		st := j.State()
		if len(st.Bases) > 0 {
			return fmt.Errorf("%w: the job is priced by basis %d already", fin.ErrBasisApplied, len(st.Bases))
		}
		driver, err := d.Shipments.CostDriver(ctx, c.Scope.TenantID, st.ShipmentID)
		if err != nil {
			return err
		}
		switch {
		case driver.Status != "PLANNED" || driver.LegalEntityID != st.LegalEntityID || driver.CustomerPartyID != st.CustomerPartyID:
			return fmt.Errorf("%w: FUL reports shipment %s %s for %s of %s", fin.ErrNotPriceable, st.ShipmentID, driver.Status, driver.CustomerPartyID,
				driver.LegalEntityID)
		}
		// a shipment that carries an order under an accepted ONE_OFF quote is priced by that quote (ApplyPricingBasis,
		// re-evaluated on its cargo, OD-7 A), never by current cards; under an agreement its customer card is rated here
		asked := map[string]bool{}
		for _, o := range driver.Orders {
			if o.QuoteID == "" || asked[o.QuoteID] {
				continue
			}
			asked[o.QuoteID] = true
			if d.Pricing == nil {
				return ErrMissingDependency
			}
			_, err := d.Pricing.QuoteBasis(ctx, c.Scope.TenantID, o.QuoteID, nil)
			if err == nil {
				return fmt.Errorf("%w: shipment %s carries order %s under accepted quote %s; the job is priced by that quote", fin.ErrNotPriceable,
					st.ShipmentID, o.OrderID, o.QuoteID)
			}
			if !errors.Is(err, fin.ErrNotPriceable) {
				return err
			}
		}
		if driver.Origin.IsZero() || driver.Destination.IsZero() {
			return fmt.Errorf("%w: shipment %s has no planned route to rate", fin.ErrNotPriceable, st.ShipmentID)
		}
		gross, err := fin.Grams(driver.GrossWeight)
		if err != nil {
			return err
		}
		volume, err := fin.CubicCm(driver.Volume)
		if err != nil {
			return err
		}
		plan = driver.PlanVersion
		basis, err = d.Rating.ShipmentEstimate(ctx, c.Scope.TenantID, EstimateRequest{CommandID: c.CommandID, ActorID: c.Scope.ActorID,
			BasisID: EstimateBasisID(c.Scope.TenantID, c.CommandID), LegalEntityID: st.LegalEntityID, CustomerPartyID: st.CustomerPartyID, SubjectID: st.ShipmentID,
			SubjectVersion: int64(plan), Mode: driver.TransportMode, Origin: driver.Origin.Code, Destination: driver.Destination.Code, Via: in.Via,
			ServiceLevel: in.ServiceLevel, Carrier: in.Carrier, Pieces: driver.Packs, GrossGrams: gross, VolumeCM3: volume, OccurredAt: c.OccurredAt})
		if err != nil {
			return err
		}
		if basis.LegalEntityID != st.LegalEntityID || basis.CustomerPartyID != st.CustomerPartyID || basis.SubjectID != st.ShipmentID ||
			basis.SubjectVersion != int64(plan) {
			return fmt.Errorf("%w: RATE answered for another legal entity, customer, shipment or plan version", ErrUnknownSource)
		}
		if customerRevision, err = d.Directory.PartyInRole(ctx, c.Scope.TenantID, st.CustomerPartyID, st.LegalEntityID, DebtorRoles, c.OccurredAt); err != nil {
			return err
		}
		return d.confirmCreditors(ctx, c, st.LegalEntityID, basis.Lines)
	}, func(_ context.Context, j *fin.FreightJob, p *fin.FinancePolicy) (Receipt, error) {
		err := j.ApplyEstimate(fin.Basis{BasisID: basis.BasisID, PlanVersion: plan}, customerRevision, basis.Lines, p, c.Scope.ActorID, c.OccurredAt)
		return Receipt{Version: len(basis.Lines)}, err
	})
}

// confirmCreditors has ORG confirm each supplier of a rated cost in a creditor role and sets its revision.
func (d Deps) confirmCreditors(ctx context.Context, c Command, legalEntity string, lines []fin.EstimateLine) error {
	revisions := map[string]int64{}
	for i := range lines {
		party := lines[i].CreditorPartyID
		if !lines[i].HasCost || party == "" {
			continue
		}
		if _, ok := revisions[party]; !ok {
			revision, err := d.Directory.PartyInRole(ctx, c.Scope.TenantID, party, legalEntity, CreditorRoles, c.OccurredAt)
			if err != nil {
				return err
			}
			revisions[party] = revision
		}
		lines[i].CreditorRevision = revisions[party]
	}
	return nil
}

// RatePoolInput is how a pool's consolidation is rated: its carrier (when known), lane and service level, and the
// apportionment method of the lines.
type RatePoolInput struct {
	Carrier      string
	Origin       string
	Destination  string
	Via          string
	ServiceLevel string
	Method       string
}

// RatePool rates the pool's consolidation (CONSOL_COST_ESTIMATE): PHY's PhysicalCostBasis v1 gives the mode and the
// cargo of its live houses; RATE rates the BUY side with the carrier's cards (else the lowest total); ORG confirms each
// supplier as a creditor. Every rated cost becomes a RATED pool cost line; a pool takes one rated estimate.
func (d Deps) RatePool(ctx context.Context, c Command, in RatePoolInput) (Receipt, error) {
	if d.Physical == nil || d.Rating == nil || d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	var basis RatedBasis
	return d.changePool(ctx, c, func(ctx context.Context, pool *fin.ConsolCostPool) error {
		st := pool.State()
		if slices.ContainsFunc(st.Lines, func(l fin.PoolCostLine) bool { return l.Source == fin.SourceRated }) {
			return fmt.Errorf("%w: the pool has a rated estimate already", fin.ErrBasisApplied)
		}
		cb, err := d.Physical.CostBasis(ctx, c.Scope.TenantID, st.ConsolidationID)
		if err != nil {
			return err
		}
		mode := map[string]string{"AIR_CONSOL": "AIR", "SEA_LCL_COLOAD": "SEA"}[cb.Kind]
		if mode == "" || cb.OperatingLegalEntityID != st.LegalEntityID || len(cb.Houses) == 0 {
			return fmt.Errorf("%w: PHY reports consolidation %s of kind %s with %d houses", fin.ErrNotPriceable, st.ConsolidationID, cb.Kind, len(cb.Houses))
		}
		var pieces, gross, volume int64
		for _, h := range cb.Houses {
			g, err := fin.Grams(h.GrossWeight)
			if err != nil {
				return err
			}
			v, err := fin.CubicCm(h.Volume)
			if err != nil {
				return err
			}
			pieces, gross, volume = pieces+h.Packs, gross+g, volume+v
		}
		basis, err = d.Rating.ConsolEstimate(ctx, c.Scope.TenantID, EstimateRequest{CommandID: c.CommandID, ActorID: c.Scope.ActorID,
			BasisID: EstimateBasisID(c.Scope.TenantID, c.CommandID), LegalEntityID: st.LegalEntityID, SubjectID: st.ConsolidationID, SubjectVersion: cb.Revision,
			Mode: mode, Origin: in.Origin, Destination: in.Destination, Via: in.Via, ServiceLevel: in.ServiceLevel, Carrier: in.Carrier, Pieces: pieces,
			GrossGrams: gross, VolumeCM3: volume, OccurredAt: c.OccurredAt})
		if err != nil {
			return err
		}
		if basis.LegalEntityID != st.LegalEntityID || basis.SubjectID != st.ConsolidationID || basis.SubjectVersion != cb.Revision || basis.CustomerPartyID != "" {
			return fmt.Errorf("%w: RATE answered for another legal entity, consolidation or revision", ErrUnknownSource)
		}
		return d.confirmCreditors(ctx, c, st.LegalEntityID, basis.Lines)
	}, func(_ context.Context, pool *fin.ConsolCostPool, p *fin.FinancePolicy) (Receipt, error) {
		err := pool.RecordEstimate(basis.BasisID, basis.Lines, in.Method, p, c.Scope.ActorID, c.OccurredAt)
		return Receipt{Version: len(pool.State().Lines)}, err
	})
}
