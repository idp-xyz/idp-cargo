package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/application"
	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// RATE V0.5 stores: the charge-code catalogue, rate cards and rate snapshots.

// ChargeCodeStore reads a tenant's code versions over the starter list and writes new versions.
type ChargeCodeStore struct{ DB *bentopg.DB }

var _ app.ChargeCodeStore = ChargeCodeStore{}

func scanCodeVersion(row pgx.Row) (rate.ChargeCode, rate.ChargeCodeVersion, error) {
	var code, group, calculator string
	var unit *string
	var modes []string
	var v rate.ChargeCodeVersion
	if err := row.Scan(&code, &v.Version, &v.NameZH, &v.NameEN, &group, &calculator, &unit, &modes, &v.Active, &v.SetBy, &v.SetAt); err != nil {
		return "", v, err
	}
	v.Group, v.Calculator, v.Unit, v.SetAt = rate.ChargeGroup(group), rate.Calculator(calculator), rate.RateUnit(deref(unit)), v.SetAt.UTC()
	for _, m := range modes {
		v.Modes = append(v.Modes, rate.Mode(m))
	}
	return rate.ChargeCode(code), v, nil
}

const latestCodes = `SELECT DISTINCT ON (code) code,version,name_zh,name_en,charge_group,calculator,rate_unit,modes,active,set_by,set_at
  FROM cargo_rate.charge_code_version WHERE tenant_id=$1`

func (s ChargeCodeStore) Load(ctx context.Context, tenant rate.TenantID, code rate.ChargeCode) (*rate.ChargeCodeAggregate, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	_, v, err := scanCodeVersion(q.QueryRow(ctx, latestCodes+` AND code=$2 ORDER BY code, version DESC`, string(tenant), string(code)))
	if errors.Is(err, pgx.ErrNoRows) {
		return rate.RestoreChargeCode(tenant, code, rate.ChargeCodeVersion{}, false), nil
	}
	if err != nil {
		return nil, err
	}
	return rate.RestoreChargeCode(tenant, code, v, true), nil
}

// Insert writes the version added; a concurrent writer of the same version loses (rate.ErrConflict).
func (s ChargeCodeStore) Insert(ctx context.Context, c *rate.ChargeCodeAggregate) error {
	v := c.Added()
	if v == nil {
		return nil
	}
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	modes := make([]string, len(v.Modes))
	for i, m := range v.Modes {
		modes[i] = string(m)
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_rate.charge_code_version (tenant_id,code,version,name_zh,name_en,charge_group,calculator,rate_unit,modes,active,
        set_by,set_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, string(c.Tenant()), string(c.Code()), v.Version, v.NameZH, v.NameEN,
		string(v.Group), string(v.Calculator), orNull(string(v.Unit)), modes, v.Active, v.SetBy, v.SetAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: charge code %s version %d was written meanwhile", rate.ErrConflict, c.Code(), v.Version)
	}
	return err
}

// List answers the tenant's catalogue: every stored code at its latest version and the starter codes it never
// changed, in the order of their codes.
func (s ChargeCodeStore) List(ctx context.Context, tenant rate.TenantID) ([]rate.ChargeCodeEntry, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, latestCodes+` ORDER BY code, version DESC`, string(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[rate.ChargeCode]bool{}
	var out []rate.ChargeCodeEntry
	for rows.Next() {
		code, v, err := scanCodeVersion(rows)
		if err != nil {
			return nil, err
		}
		seen[code] = true
		out = append(out, rate.ChargeCodeEntry{Code: code, Current: v})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for code, v := range rate.StarterChargeCodes {
		if !seen[code] {
			out = append(out, rate.ChargeCodeEntry{Code: code, Current: v})
		}
	}
	slices.SortFunc(out, func(a, b rate.ChargeCodeEntry) int { return compareStrings(string(a.Code), string(b.Code)) })
	return out, nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// RateCardStore reads and writes rate cards with their versions, entries, lines and breaks.
type RateCardStore struct{ DB *bentopg.DB }

var _ app.RateCardStore = RateCardStore{}

const cardColumns = `c.rate_card_id,c.side,c.scope,c.legal_entity_id,c.party_id,c.party_revision,c.name,c.created_by,c.created_at,c.revision`

func scanCard(row pgx.Row, tenant rate.TenantID) (rate.RateCardState, int64, error) {
	st := rate.RateCardState{TenantID: tenant}
	var id, side, scope, entity string
	var party *string
	var partyRevision *int64
	var revision int64
	if err := row.Scan(&id, &side, &scope, &entity, &party, &partyRevision, &st.Name, &st.CreatedBy, &st.CreatedAt, &revision); err != nil {
		return st, 0, err
	}
	st.RateCardID, st.Side, st.Scope, st.LegalEntityID, st.PartyID = rate.RateCardID(id), rate.Side(side), rate.CardScope(scope), rate.LegalEntityID(entity), deref(party)
	st.CreatedAt = st.CreatedAt.UTC()
	if partyRevision != nil {
		st.PartyRevision = *partyRevision
	}
	return st, revision, nil
}

func (s RateCardStore) Load(ctx context.Context, tenant rate.TenantID, id rate.RateCardID) (*rate.RateCard, int64, error) {
	return s.read(ctx, tenant, id, " FOR NO KEY UPDATE")
}

func (s RateCardStore) Read(ctx context.Context, tenant rate.TenantID, id rate.RateCardID) (*rate.RateCard, int64, error) {
	return s.read(ctx, tenant, id, "")
}

func (s RateCardStore) read(ctx context.Context, tenant rate.TenantID, id rate.RateCardID, lock string) (*rate.RateCard, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	st, revision, err := scanCard(q.QueryRow(ctx, `SELECT `+cardColumns+` FROM cargo_rate.rate_card c WHERE c.tenant_id=$1 AND c.rate_card_id=$2`+lock,
		string(tenant), string(id)), tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	if st.Versions, err = versions(ctx, q, tenant, id); err != nil {
		return nil, 0, err
	}
	loaded := map[int]bool{}
	for i, v := range st.Versions {
		if v.Status == rate.VersionDraft {
			if st.Versions[i].Entries, err = entries(ctx, q, tenant, id, v.No); err != nil {
				return nil, 0, err
			}
			loaded[v.No] = true
		}
	}
	return rate.RestoreRateCard(st, loaded), revision, nil
}

func versions(ctx context.Context, q bentopg.Querier, tenant rate.TenantID, id rate.RateCardID) ([]rate.RateCardVersion, error) {
	rows, err := q.Query(ctx, `SELECT version_no,status,valid_from,valid_until,currency,source_ref,filing_ref,preparers,prepared_at,published_by,
        published_at,approved_by,self_approved,withdrawn_by,withdrawn_at,withdraw_reason
      FROM cargo_rate.rate_card_version WHERE tenant_id=$1 AND rate_card_id=$2 ORDER BY version_no`, string(tenant), string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rate.RateCardVersion
	for rows.Next() {
		var v rate.RateCardVersion
		var status, currency string
		var until, publishedAt, withdrawnAt *time.Time
		var source, filing, publishedBy, approvedBy, withdrawnBy, reason *string
		var self *bool
		if err = rows.Scan(&v.No, &status, &v.Validity.From, &until, &currency, &source, &filing, &v.Preparers, &v.PreparedAt, &publishedBy,
			&publishedAt, &approvedBy, &self, &withdrawnBy, &withdrawnAt, &reason); err != nil {
			return nil, err
		}
		v.Status, v.Currency, v.SourceRef, v.FilingRef = rate.VersionStatus(status), kernel.CurrencyCode(currency), deref(source), deref(filing)
		v.Validity.From, v.Validity.Until, v.PreparedAt = v.Validity.From.UTC(), utc(until), v.PreparedAt.UTC()
		v.PublishedBy, v.PublishedAt, v.ApprovedBy = deref(publishedBy), utc(publishedAt), deref(approvedBy)
		v.WithdrawnBy, v.WithdrawnAt, v.WithdrawReason = deref(withdrawnBy), utc(withdrawnAt), deref(reason)
		if self != nil {
			v.SelfApproved = *self
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func utc(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

func timeOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// entries reads a version's entries with their lines and breaks.
func entries(ctx context.Context, q bentopg.Querier, tenant rate.TenantID, id rate.RateCardID, no int) ([]rate.RateEntry, error) {
	byVersion, err := content(ctx, q, tenant, `e.rate_card_id=$2 AND e.version_no=$3`, string(id), no)
	if err != nil {
		return nil, err
	}
	return byVersion[versionKey{id, no}], nil
}

type versionKey struct {
	id rate.RateCardID
	no int
}

// content reads the entries of the versions where selects, by version.
func content(ctx context.Context, q bentopg.Querier, tenant rate.TenantID, where string, args ...any) (map[versionKey][]rate.RateEntry, error) {
	rows, err := q.Query(ctx, `SELECT e.rate_card_id,e.version_no,e.entry_no,e.mode,e.container_mode,e.origin,e.destination,e.via,e.edition,e.carrier_party_id,
        e.service_level,e.commodity,e.transit_days,e.frequency,e.valid_from,e.valid_until,
        coalesce((SELECT jsonb_agg(jsonb_build_object('no',l.line_no,'code',l.charge_code,'currency',l.currency,'calc',l.calculator,'unit',l.rate_unit,
          'rate',l.rate_minor,'scale',l.rate_scale,'min',l.minimum_minor,'max',l.maximum_minor,'step',l.quantity_step,'minq',l.min_quantity,
          'factor',l.volume_factor,'bp',l.percent_bp,'base',l.base_code,'cfp',l.call_for_pricing,
          'breaks',coalesce((SELECT jsonb_agg(jsonb_build_array(b.from_quantity,b.rate_minor) ORDER BY b.from_quantity) FROM cargo_rate.rate_break b
            WHERE b.tenant_id=l.tenant_id AND b.rate_card_id=l.rate_card_id AND b.version_no=l.version_no AND b.entry_no=l.entry_no
              AND b.line_no=l.line_no),'[]'::jsonb)) ORDER BY l.line_no)
          FROM cargo_rate.rate_line l WHERE l.tenant_id=e.tenant_id AND l.rate_card_id=e.rate_card_id AND l.version_no=e.version_no
            AND l.entry_no=e.entry_no),'[]'::jsonb)
      FROM cargo_rate.rate_entry e WHERE e.tenant_id=$1 AND `+where+` ORDER BY e.rate_card_id, e.version_no, e.entry_no`,
		append([]any{string(tenant)}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[versionKey][]rate.RateEntry{}
	for rows.Next() {
		var e rate.RateEntry
		var id, mode, container string
		var no int
		var via, carrier *string
		var from, until *time.Time
		var lines []byte
		if err = rows.Scan(&id, &no, &e.No, &mode, &container, &e.Origin, &e.Destination, &via, &e.Edition, &carrier, &e.ServiceLevel, &e.Commodity,
			&e.TransitDays, &e.Frequency, &from, &until, &lines); err != nil {
			return nil, err
		}
		e.Mode, e.ContainerMode, e.Via, e.Carrier = rate.Mode(mode), rate.ContainerMode(container), deref(via), deref(carrier)
		e.Validity = kernel.ValidityInterval{From: utc(from), Until: utc(until)}
		var ls []struct {
			No       int
			Code     string
			Currency string
			Calc     string
			Unit     *string
			Rate     int64
			Scale    int
			Min      int64
			Max      *int64
			Step     *int64
			MinQ     *int64 `json:"minq"`
			Factor   *int64
			BP       *int64 `json:"bp"`
			Base     *string
			CFP      bool `json:"cfp"`
			Breaks   [][2]int64
		}
		if err = json.Unmarshal(lines, &ls); err != nil {
			return nil, err
		}
		for _, l := range ls {
			line := rate.RateLine{No: l.No, ChargeCode: rate.ChargeCode(l.Code), Currency: kernel.CurrencyCode(l.Currency), Calculator: rate.Calculator(l.Calc),
				Unit: rate.RateUnit(deref(l.Unit)), Rate: rate.UnitRate{Minor: l.Rate, Scale: l.Scale}, Minimum: l.Min, Maximum: zero(l.Max),
				Step: zero(l.Step), MinQuantity: zero(l.MinQ), VolumeFactor: zero(l.Factor), PercentBP: zero(l.BP), BaseCode: rate.ChargeCode(deref(l.Base)),
				CallForPricing: l.CFP}
			for _, b := range l.Breaks {
				line.Breaks = append(line.Breaks, rate.RateBreak{From: b[0], Rate: b[1]})
			}
			e.Lines = append(e.Lines, line)
		}
		k := versionKey{rate.RateCardID(id), no}
		out[k] = append(out[k], e)
	}
	return out, rows.Err()
}

func zero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func positiveOrNull(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func (s RateCardStore) Entries(ctx context.Context, tenant rate.TenantID, id rate.RateCardID, no int) ([]rate.RateEntry, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	return entries(ctx, q, tenant, id, no)
}

// Insert writes a new card at revision 1.
func (s RateCardStore) Insert(ctx context.Context, c *rate.RateCard) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := c.State()
	var party, partyRevision any
	if st.PartyID != "" {
		party, partyRevision = st.PartyID, st.PartyRevision
	}
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_rate.rate_card (tenant_id,rate_card_id,side,scope,legal_entity_id,party_id,party_revision,name,created_by,
        created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING revision`, string(st.TenantID), string(st.RateCardID), string(st.Side),
		string(st.Scope), string(st.LegalEntityID), party, partyRevision, st.Name, st.CreatedBy, st.CreatedAt).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "rate_card_pkey" {
		return 0, bentorepo.ErrAlreadyExists
	}
	if err != nil {
		return 0, err
	}
	return revision, s.writeVersions(ctx, q, c, st)
}

// Save writes the versions drafted or changed since the card was loaded and raises its revision, if the stored
// revision is still the one expected.
func (s RateCardStore) Save(ctx context.Context, c *rate.RateCard, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := c.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_rate.rate_card SET revision=revision+1,updated_at=now() WHERE tenant_id=$1 AND rate_card_id=$2 AND revision=$3
      RETURNING revision`, string(st.TenantID), string(st.RateCardID), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, bentorepo.ErrConflict
	}
	if err != nil {
		return 0, err
	}
	return revision, s.writeVersions(ctx, q, c, st)
}

func (s RateCardStore) writeVersions(ctx context.Context, q bentopg.Executor, c *rate.RateCard, st rate.RateCardState) error {
	t, id := string(st.TenantID), string(st.RateCardID)
	for i, v := range st.Versions {
		if !c.HeaderChanged(v.No) {
			continue
		}
		var self any
		if v.ApprovedBy != "" {
			self = v.SelfApproved
		}
		if i >= c.Stored() {
			if _, err := q.Exec(ctx, `INSERT INTO cargo_rate.rate_card_version (tenant_id,rate_card_id,version_no,status,valid_from,valid_until,currency,
                source_ref,filing_ref,preparers,prepared_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, t, id, v.No, string(v.Status),
				v.Validity.From, timeOrNull(v.Validity.Until), string(v.Currency), orNull(v.SourceRef), orNull(v.FilingRef), v.Preparers, v.PreparedAt); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" {
					return fmt.Errorf("%w: card %s has a draft already", rate.ErrConflict, id)
				}
				return err
			}
		}
		if c.ContentChanged(v.No) {
			for _, stmt := range []string{`DELETE FROM cargo_rate.rate_break WHERE tenant_id=$1 AND rate_card_id=$2 AND version_no=$3`,
				`DELETE FROM cargo_rate.rate_line WHERE tenant_id=$1 AND rate_card_id=$2 AND version_no=$3`,
				`DELETE FROM cargo_rate.rate_entry WHERE tenant_id=$1 AND rate_card_id=$2 AND version_no=$3`} {
				if _, err := q.Exec(ctx, stmt, t, id, v.No); err != nil {
					return err
				}
			}
			if err := insertEntries(ctx, q, t, id, v.No, v.Entries); err != nil {
				return err
			}
		}
		if _, err := q.Exec(ctx, `UPDATE cargo_rate.rate_card_version SET status=$4,preparers=$5,prepared_at=$6,published_by=$7,published_at=$8,
            approved_by=$9,self_approved=$10,withdrawn_by=$11,withdrawn_at=$12,withdraw_reason=$13
          WHERE tenant_id=$1 AND rate_card_id=$2 AND version_no=$3`, t, id, v.No, string(v.Status), v.Preparers, v.PreparedAt, orNull(v.PublishedBy),
			timeOrNull(v.PublishedAt), orNull(v.ApprovedBy), self, orNull(v.WithdrawnBy), timeOrNull(v.WithdrawnAt), orNull(v.WithdrawReason)); err != nil {
			return err
		}
	}
	return nil
}

func insertEntries(ctx context.Context, q bentopg.Executor, t, id string, no int, es []rate.RateEntry) error {
	for _, e := range es {
		from, until := timeOrNull(e.Validity.From), timeOrNull(e.Validity.Until)
		if _, err := q.Exec(ctx, `INSERT INTO cargo_rate.rate_entry (tenant_id,rate_card_id,version_no,entry_no,mode,container_mode,origin,destination,via,
            edition,carrier_party_id,service_level,commodity,transit_days,frequency,valid_from,valid_until)
          VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, t, id, no, e.No, string(e.Mode), string(e.ContainerMode), e.Origin,
			e.Destination, orNull(e.Via), e.Edition, orNull(e.Carrier), e.ServiceLevel, e.Commodity, e.TransitDays, e.Frequency, from, until); err != nil {
			return err
		}
		for _, l := range e.Lines {
			var step, minq any
			if l.Calculator == rate.CalcPerUnit {
				step, minq = l.Step, l.MinQuantity
			}
			if _, err := q.Exec(ctx, `INSERT INTO cargo_rate.rate_line (tenant_id,rate_card_id,version_no,entry_no,line_no,charge_code,currency,calculator,
                rate_unit,rate_minor,rate_scale,minimum_minor,maximum_minor,quantity_step,min_quantity,volume_factor,percent_bp,base_code,call_for_pricing)
              VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`, t, id, no, e.No, l.No, string(l.ChargeCode), string(l.Currency),
				string(l.Calculator), orNull(string(l.Unit)), l.Rate.Minor, l.Rate.Scale, l.Minimum, positiveOrNull(l.Maximum), step, minq,
				positiveOrNull(l.VolumeFactor), positiveOrNull(l.PercentBP), orNull(string(l.BaseCode)), l.CallForPricing); err != nil {
				return err
			}
			for _, b := range l.Breaks {
				if _, err := q.Exec(ctx, `INSERT INTO cargo_rate.rate_break (tenant_id,rate_card_id,version_no,entry_no,line_no,from_quantity,rate_minor)
                  VALUES ($1,$2,$3,$4,$5,$6,$7)`, t, id, no, e.No, l.No, b.From, b.Rate); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// List answers the cards the filter selects, with their version headers, in the order of their ids.
func (s RateCardStore) List(ctx context.Context, tenant rate.TenantID, f app.CardFilter) ([]rate.RateCardState, []int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, nil, err
	}
	var at any
	if !f.ValidAt.IsZero() {
		at = f.ValidAt
	}
	rows, err := q.Query(ctx, `SELECT `+cardColumns+` FROM cargo_rate.rate_card c WHERE c.tenant_id=$1 AND ($2='' OR c.side=$2) AND ($3='' OR c.scope=$3)
        AND ($4='' OR c.legal_entity_id=$4) AND ($5='' OR c.party_id=$5)
        AND ($6::timestamptz IS NULL OR EXISTS (SELECT 1 FROM cargo_rate.rate_card_version v WHERE v.tenant_id=c.tenant_id AND v.rate_card_id=c.rate_card_id
          AND v.status IN ('PUBLISHED','WITHDRAWN') AND `+effective+` @> $6::timestamptz))
      ORDER BY c.rate_card_id`, string(tenant), string(f.Side), string(f.Scope), string(f.LegalEntityID), f.PartyID, at)
	if err != nil {
		return nil, nil, err
	}
	var out []rate.RateCardState
	var revisions []int64
	for rows.Next() {
		st, revision, err := scanCard(rows, tenant)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		out, revisions = append(out, st), append(revisions, revision)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	for i := range out {
		if out[i].Versions, err = versions(ctx, q, tenant, out[i].RateCardID); err != nil {
			return nil, nil, err
		}
	}
	return out, revisions, nil
}

// effective is the SQL range a version rates in: a PUBLISHED one its validity, a WITHDRAWN one its validity up to the
// withdrawal (rate.RateCardVersion.Effective).
const effective = `tstzrange(v.valid_from, CASE WHEN v.status = 'WITHDRAWN' THEN greatest(v.valid_from, least(v.valid_until, v.withdrawn_at))
  ELSE v.valid_until END)`

// Published answers the versions of a legal entity's cards that rate at the instant (PUBLISHED, or WITHDRAWN after it),
// with their validity as they rate and the entries of the lane.
func (s RateCardStore) Published(ctx context.Context, tenant rate.TenantID, entity rate.LegalEntityID, at time.Time, lane app.Lane) ([]rate.PublishedVersion, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT c.rate_card_id,c.side,c.scope,c.party_id,v.version_no,lower(`+effective+`),upper(`+effective+`)
      FROM cargo_rate.rate_card c JOIN cargo_rate.rate_card_version v ON v.tenant_id=c.tenant_id AND v.rate_card_id=c.rate_card_id
      WHERE c.tenant_id=$1 AND c.legal_entity_id=$2 AND v.status IN ('PUBLISHED','WITHDRAWN') AND `+effective+` @> $3::timestamptz
      ORDER BY c.rate_card_id, v.version_no`, string(tenant), string(entity), at)
	if err != nil {
		return nil, err
	}
	var out []rate.PublishedVersion
	for rows.Next() {
		var p rate.PublishedVersion
		var id, side, scope string
		var party *string
		var until *time.Time
		if err = rows.Scan(&id, &side, &scope, &party, &p.No, &p.Validity.From, &until); err != nil {
			rows.Close()
			return nil, err
		}
		p.RateCardID, p.Side, p.Scope, p.PartyID, p.LegalEntityID = rate.RateCardID(id), rate.Side(side), rate.CardScope(scope), deref(party), entity
		p.Validity.From, p.Validity.Until = p.Validity.From.UTC(), utc(until)
		out = append(out, p)
	}
	rows.Close()
	if err = rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	ids, nos := make([]string, len(out)), make([]int32, len(out))
	for i, p := range out {
		ids[i], nos[i] = string(p.RateCardID), int32(p.No)
	}
	byVersion, err := content(ctx, q, tenant, `(e.rate_card_id, e.version_no) IN (SELECT * FROM unnest($2::text[], $3::int[]))
        AND e.mode=$4 AND e.container_mode=$5 AND e.origin=$6 AND e.destination=$7`, ids, nos, string(lane.Mode), string(lane.ContainerMode), lane.Origin,
		lane.Destination)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Entries = byVersion[versionKey{out[i].RateCardID, out[i].No}]
	}
	return out, nil
}

// SnapshotStore writes and reads rate snapshots.
type SnapshotStore struct{ DB *bentopg.DB }

var _ app.SnapshotStore = SnapshotStore{}

func (s SnapshotStore) Insert(ctx context.Context, snap *rate.RateSnapshot) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	st := snap.State()
	in := st.Input
	sides := make([]string, len(in.Sides))
	for i, side := range in.Sides {
		sides[i] = string(side)
	}
	var customerRevision any
	if in.Customer.PartyID != "" {
		customerRevision = in.Customer.PartyRevision
	}
	t := string(st.TenantID)
	_, err = q.Exec(ctx, `INSERT INTO cargo_rate.rate_snapshot (tenant_id,snapshot_id,purpose,legal_entity_id,sides,customer_party_id,customer_revision,
        carrier_party_id,mode,container_mode,origin,destination,via,edition,service_level,commodity,business_at,pieces,gross_grams,volume_cm3,documents,
        subject_kind,subject_id,subject_version,engine_version,evaluated_by,recorded_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
		t, st.SnapshotID, string(in.Purpose), string(in.LegalEntityID), sides, orNull(in.Customer.PartyID), customerRevision, orNull(in.Carrier),
		string(in.Mode), string(in.ContainerMode), in.Origin, in.Destination, orNull(in.Via), in.Edition, in.ServiceLevel, in.Commodity, in.At,
		in.Cargo.Pieces, in.Cargo.GrossGrams, in.Cargo.VolumeCM3, in.Cargo.Documents, orNull(in.SubjectKind), orNull(in.SubjectID), in.SubjectVersion,
		st.EngineVersion, st.EvaluatedBy, st.RecordedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "rate_snapshot_pkey" {
		return bentorepo.ErrAlreadyExists
	}
	if err != nil {
		return err
	}
	for i, c := range st.Candidates {
		totals, err := json.Marshal(c.Total)
		if err != nil {
			return err
		}
		if _, err = q.Exec(ctx, `INSERT INTO cargo_rate.rate_snapshot_candidate (tenant_id,snapshot_id,candidate_no,side,rate_card_id,version_no,entry_no,taken,
            reason,totals) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, t, st.SnapshotID, i+1, string(c.Side), string(c.RateCardID), c.Version, c.EntryNo,
			c.Taken, c.Reason, totals); err != nil {
			return err
		}
	}
	for _, l := range st.Lines {
		var code, card, version, entry, lineNo, calculator, currency any
		party := orNull(l.PartyID)
		if l.ChargeCode != "" {
			code, card, version, entry, lineNo, calculator, currency = string(l.ChargeCode), string(l.RateCardID), l.Version, l.EntryNo, l.RateLineNo,
				string(l.Calculator), string(l.Amount.Currency)
		}
		if _, err = q.Exec(ctx, `INSERT INTO cargo_rate.rate_snapshot_line (tenant_id,snapshot_id,line_no,side,charge_code,rate_card_id,version_no,entry_no,
            rate_line_no,calculator,rate_unit,quantity,charged,band_from,rate_minor,rate_scale,percent_bp,base_amount,minimum_applied,maximum_applied,currency,
            amount_minor,gap,party_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`, t, st.SnapshotID, l.LineNo,
			string(l.Side), code, card, version, entry, lineNo, calculator, orNull(string(l.Unit)), l.Quantity, l.Charged, l.BandFrom, l.Rate.Minor,
			l.Rate.Scale, l.PercentBP, l.BaseAmount, l.MinimumApplied, l.MaximumApplied, currency, l.Amount.Minor, orNull(string(l.Gap)), party); err != nil {
			return err
		}
	}
	return nil
}

func (s SnapshotStore) Read(ctx context.Context, tenant rate.TenantID, id string) (*rate.RateSnapshot, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	st := rate.RateSnapshotState{TenantID: tenant, SnapshotID: id}
	in := &st.Input
	var purpose, entity, mode, container string
	var sides []string
	var customer, carrier, via, subjectKind, subjectID *string
	var customerRevision *int64
	err = q.QueryRow(ctx, `SELECT purpose,legal_entity_id,sides,customer_party_id,customer_revision,carrier_party_id,mode,container_mode,origin,destination,
        via,edition,service_level,commodity,business_at,pieces,gross_grams,volume_cm3,documents,subject_kind,subject_id,subject_version,engine_version,
        evaluated_by,recorded_at FROM cargo_rate.rate_snapshot WHERE tenant_id=$1 AND snapshot_id=$2`, string(tenant), id).Scan(&purpose, &entity, &sides,
		&customer, &customerRevision, &carrier, &mode, &container, &in.Origin, &in.Destination, &via, &in.Edition, &in.ServiceLevel, &in.Commodity, &in.At,
		&in.Cargo.Pieces, &in.Cargo.GrossGrams, &in.Cargo.VolumeCM3, &in.Cargo.Documents, &subjectKind, &subjectID, &in.SubjectVersion, &st.EngineVersion,
		&st.EvaluatedBy, &st.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	in.Purpose, in.LegalEntityID, in.Mode, in.ContainerMode = rate.SnapshotPurpose(purpose), rate.LegalEntityID(entity), rate.Mode(mode), rate.ContainerMode(container)
	for _, side := range sides {
		in.Sides = append(in.Sides, rate.Side(side))
	}
	in.Customer.PartyID, in.Carrier, in.Via, in.SubjectKind, in.SubjectID = deref(customer), deref(carrier), deref(via), deref(subjectKind), deref(subjectID)
	if customerRevision != nil {
		in.Customer.PartyRevision = *customerRevision
	}
	in.At, st.RecordedAt = in.At.UTC(), st.RecordedAt.UTC()
	rows, err := q.Query(ctx, `SELECT side,rate_card_id,version_no,entry_no,taken,reason,totals FROM cargo_rate.rate_snapshot_candidate
      WHERE tenant_id=$1 AND snapshot_id=$2 ORDER BY candidate_no`, string(tenant), id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c rate.Candidate
		var side, card string
		var totals []byte
		if err = rows.Scan(&side, &card, &c.Version, &c.EntryNo, &c.Taken, &c.Reason, &totals); err != nil {
			rows.Close()
			return nil, err
		}
		c.Side, c.RateCardID = rate.Side(side), rate.RateCardID(card)
		if err = json.Unmarshal(totals, &c.Total); err != nil {
			rows.Close()
			return nil, err
		}
		if len(c.Total) == 0 {
			c.Total = nil
		}
		st.Candidates = append(st.Candidates, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT line_no,side,charge_code,rate_card_id,version_no,entry_no,rate_line_no,calculator,rate_unit,quantity,charged,band_from,
        rate_minor,rate_scale,percent_bp,base_amount,minimum_applied,maximum_applied,currency,amount_minor,gap,party_id
      FROM cargo_rate.rate_snapshot_line WHERE tenant_id=$1 AND snapshot_id=$2 ORDER BY line_no`, string(tenant), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l rate.SnapshotLine
		var side string
		var code, card, calculator, unit, currency, gap, party *string
		var version, entry, lineNo *int
		if err = rows.Scan(&l.LineNo, &side, &code, &card, &version, &entry, &lineNo, &calculator, &unit, &l.Quantity, &l.Charged, &l.BandFrom, &l.Rate.Minor,
			&l.Rate.Scale, &l.PercentBP, &l.BaseAmount, &l.MinimumApplied, &l.MaximumApplied, &currency, &l.Amount.Minor, &gap, &party); err != nil {
			return nil, err
		}
		l.Side, l.ChargeCode, l.RateCardID, l.Calculator, l.Unit = rate.Side(side), rate.ChargeCode(deref(code)), rate.RateCardID(deref(card)),
			rate.Calculator(deref(calculator)), rate.RateUnit(deref(unit))
		l.Amount.Currency, l.Gap, l.PartyID = kernel.CurrencyCode(deref(currency)), rate.Gap(deref(gap)), deref(party)
		if version != nil {
			l.Version, l.EntryNo, l.RateLineNo = *version, *entry, *lineNo
		}
		st.Lines = append(st.Lines, l)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return rate.RestoreSnapshot(st), nil
}

// Conflict reports whether err is one of RATE's commit checks refusing what another transaction made inconsistent
// meanwhile, such as content of a version deleted past RATE while it was published: the command may be retried.
func Conflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		return false
	}
	switch pgErr.ConstraintName {
	case "rate_version_overlap", "rate_version_publishable", "rate_snapshot_written_once", "charge_code_version_sequence", "rate_card_version_sequence":
		return true
	}
	return false
}
