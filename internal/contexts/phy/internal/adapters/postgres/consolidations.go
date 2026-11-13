// Package postgres persists PHY consolidations, membership ledgers, command receipts and Outbox events in
// the tenant's RLS scope.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/application"
	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// ConsolidationStore loads and writes consolidations.
type ConsolidationStore struct{ DB *bentopg.DB }

var _ app.ConsolidationStore = ConsolidationStore{}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type versionRow struct {
	VersionNo        int       `json:"version_no"`
	Change           string    `json:"change"`
	State            string    `json:"state"`
	OriginCode       string    `json:"origin_code"`
	DestinationCode  string    `json:"destination_code"`
	OriginIATA       string    `json:"origin_iata"`
	DestinationIATA  string    `json:"destination_iata"`
	LocationEdition  string    `json:"location_edition"`
	PlannedDeparture time.Time `json:"planned_departure"`
	HouseCutOff      time.Time `json:"house_cut_off"`
	Reason           string    `json:"reason"`
	WrittenBy        string    `json:"written_by"`
	WrittenAt        time.Time `json:"written_at"`
	EnteredAt        time.Time `json:"entered_at"`
}

func (v versionRow) version() phy.Version {
	return phy.Version{No: v.VersionNo, Change: phy.Change(v.Change), State: phy.State(v.State),
		Route: phy.Route{Origin: unlocode(v.OriginCode), Destination: unlocode(v.DestinationCode), OriginIATA: v.OriginIATA,
			DestinationIATA: v.DestinationIATA, LocationEdition: v.LocationEdition},
		PlannedDeparture: v.PlannedDeparture.UTC(), HouseCutOff: v.HouseCutOff.UTC(), Reason: v.Reason, By: v.WrittenBy,
		At: v.WrittenAt.UTC(), EnteredAt: v.EnteredAt.UTC()}
}

type unitRow struct {
	UnitNo           int       `json:"unit_no"`
	Kind             string    `json:"kind"`
	TypeCode         string    `json:"type_code"`
	Edition          string    `json:"equipment_edition"`
	TEUTenths        int       `json:"teu_tenths"`
	MaxGrossKg       int64     `json:"max_gross_kg"`
	TareKg           int64     `json:"tare_kg"`
	CapacityCM3      int64     `json:"capacity_cm3"`
	Status           string    `json:"status"`
	PlannedBy        string    `json:"planned_by"`
	PlannedAt        time.Time `json:"planned_at"`
	PlannedEnteredAt time.Time `json:"planned_entered_at"`
	RemoveReason     string    `json:"remove_reason"`
	RemovedBy        string    `json:"removed_by"`
	RemovedAt        time.Time `json:"removed_at"`
	RemoveEnteredAt  time.Time `json:"remove_entered_at"`
}

func (u unitRow) unit() phy.PlannedLoadUnit {
	return phy.PlannedLoadUnit{No: u.UnitNo, Kind: phy.LoadUnitKind(u.Kind), Status: phy.UnitStatus(u.Status),
		Limits: phy.UnitLimits{TypeCode: u.TypeCode, TEUTenths: u.TEUTenths, MaxGrossKg: u.MaxGrossKg, TareKg: u.TareKg,
			CapacityCM3: u.CapacityCM3, Edition: u.Edition},
		PlannedBy: u.PlannedBy, PlannedAt: u.PlannedAt.UTC(), PlannedEnteredAt: u.PlannedEnteredAt.UTC(), RemoveReason: u.RemoveReason,
		RemovedBy: u.RemovedBy, RemovedAt: u.RemovedAt.UTC(), RemoveEnteredAt: u.RemoveEnteredAt.UTC()}
}

func unlocode(code string) kernel.LocationCode {
	return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}
}

func (s ConsolidationStore) Load(ctx context.Context, tenant phy.TenantID, id phy.ConsolidationID) (*phy.Consolidation, int64, error) {
	return s.locked(ctx, tenant, id, "FOR UPDATE")
}

func (s ConsolidationStore) LoadShared(ctx context.Context, tenant phy.TenantID, id phy.ConsolidationID) (*phy.Consolidation, int64, error) {
	return s.locked(ctx, tenant, id, "FOR SHARE")
}

func (s ConsolidationStore) locked(ctx context.Context, tenant phy.TenantID, id phy.ConsolidationID, lock string) (*phy.Consolidation, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_phy.consolidation WHERE tenant_id=$1 AND consolidation_id=$2 `+lock,
		string(tenant), string(id)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: consolidation %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	return readConsolidation(ctx, q, tenant, id)
}

func (s ConsolidationStore) Read(ctx context.Context, tenant phy.TenantID, id phy.ConsolidationID) (*phy.Consolidation, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readConsolidation(ctx, q, tenant, id)
}

// readConsolidation reads a consolidation with its versions and planned load units in one statement.
func readConsolidation(ctx context.Context, q queryRower, tenant phy.TenantID, id phy.ConsolidationID) (*phy.Consolidation, int64, error) {
	st := phy.ConsolidationState{TenantID: tenant, ConsolidationID: id}
	var mode, kind, state string
	var revision int64
	var versionJSON, unitJSON []byte
	err := q.QueryRow(ctx, `SELECT c.transport_mode, c.kind, c.operating_legal_entity_id, c.state, c.revision,
        (SELECT coalesce(json_agg(v ORDER BY v.version_no), '[]') FROM cargo_phy.consolidation_version v
          WHERE v.tenant_id=c.tenant_id AND v.consolidation_id=c.consolidation_id),
        (SELECT coalesce(json_agg(u ORDER BY u.unit_no), '[]') FROM cargo_phy.planned_load_unit u
          WHERE u.tenant_id=c.tenant_id AND u.consolidation_id=c.consolidation_id)
      FROM cargo_phy.consolidation c WHERE c.tenant_id=$1 AND c.consolidation_id=$2`, string(tenant), string(id)).Scan(
		&mode, &kind, &st.OperatingLegalEntityID, &state, &revision, &versionJSON, &unitJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: consolidation %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	var versions []versionRow
	var units []unitRow
	if err = errors.Join(json.Unmarshal(versionJSON, &versions), json.Unmarshal(unitJSON, &units)); err != nil {
		return nil, 0, err
	}
	st.Mode, st.Kind, st.State = phy.Mode(mode), phy.Kind(kind), phy.State(state)
	for _, v := range versions {
		st.Versions = append(st.Versions, v.version())
	}
	for _, u := range units {
		st.LoadUnits = append(st.LoadUnits, u.unit())
	}
	c, err := phy.RestoreConsolidation(st)
	return c, revision, err
}

// Insert writes a new consolidation at revision 1 with its first version.
func (s ConsolidationStore) Insert(ctx context.Context, c *phy.Consolidation) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := c.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_phy.consolidation (tenant_id,consolidation_id,transport_mode,kind,operating_legal_entity_id,state,current_version)
      VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING revision`, string(st.TenantID), string(st.ConsolidationID), string(st.Mode), string(st.Kind),
		st.OperatingLegalEntityID, string(st.State), len(st.Versions)).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "consolidation_pkey" {
		return 0, fmt.Errorf("%w: consolidation %s", bentorepo.ErrAlreadyExists, st.ConsolidationID)
	}
	if err != nil {
		return 0, err
	}
	if err = insertVersions(ctx, q, c); err != nil {
		return 0, err
	}
	return revision, writeUnits(ctx, q, c)
}

// Save writes the state and the versions, load units and removals added since the consolidation was loaded, if the
// stored revision is still the expected one.
func (s ConsolidationStore) Save(ctx context.Context, c *phy.Consolidation, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := c.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_phy.consolidation SET state=$3,current_version=$4,revision=revision+1,updated_at=now()
      WHERE tenant_id=$1 AND consolidation_id=$2 AND revision=$5 RETURNING revision`,
		string(st.TenantID), string(st.ConsolidationID), string(st.State), len(st.Versions), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: consolidation %s", bentorepo.ErrConflict, st.ConsolidationID)
	}
	if err != nil {
		return 0, err
	}
	if err = insertVersions(ctx, q, c); err != nil {
		return 0, err
	}
	return revision, writeUnits(ctx, q, c)
}

// writeUnits inserts the load units planned since the consolidation was loaded and writes the removal of earlier ones.
func writeUnits(ctx context.Context, q bentopg.Executor, c *phy.Consolidation) error {
	st := c.State()
	for _, u := range c.AddedLoadUnits() {
		l := u.Limits
		if _, err := q.Exec(ctx, `INSERT INTO cargo_phy.planned_load_unit (tenant_id,consolidation_id,unit_no,kind,type_code,equipment_edition,
            teu_tenths,max_gross_kg,tare_kg,capacity_cm3,status,planned_by,planned_at,planned_entered_at,remove_reason,removed_by,removed_at,
            remove_entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
			string(st.TenantID), string(st.ConsolidationID), u.No, string(u.Kind), l.TypeCode, l.Edition, l.TEUTenths, l.MaxGrossKg, l.TareKg,
			l.CapacityCM3, string(u.Status), u.PlannedBy, u.PlannedAt, u.PlannedEnteredAt, orNull(u.RemoveReason), orNull(u.RemovedBy),
			orNull(u.RemovedAt), orNull(u.RemoveEnteredAt)); err != nil {
			return err
		}
	}
	for _, u := range c.RemovedLoadUnits() {
		tag, err := q.Exec(ctx, `UPDATE cargo_phy.planned_load_unit SET status=$4,remove_reason=$5,removed_by=$6,removed_at=$7,remove_entered_at=$8
          WHERE tenant_id=$1 AND consolidation_id=$2 AND unit_no=$3 AND status='PLANNED'`,
			string(st.TenantID), string(st.ConsolidationID), u.No, string(u.Status), u.RemoveReason, u.RemovedBy, u.RemovedAt, u.RemoveEnteredAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: load unit %d of consolidation %s", bentorepo.ErrConflict, u.No, st.ConsolidationID)
		}
	}
	return nil
}

func insertVersions(ctx context.Context, q bentopg.Executor, c *phy.Consolidation) error {
	st := c.State()
	for _, v := range c.AddedVersions() {
		r := v.Route
		if _, err := q.Exec(ctx, `INSERT INTO cargo_phy.consolidation_version (tenant_id,consolidation_id,version_no,change,state,
            origin_code,destination_code,origin_iata,destination_iata,location_edition,planned_departure,house_cut_off,reason,
            written_by,written_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			string(st.TenantID), string(st.ConsolidationID), v.No, string(v.Change), string(v.State), r.Origin.Code, r.Destination.Code,
			orNull(r.OriginIATA), orNull(r.DestinationIATA), r.LocationEdition, v.PlannedDeparture, v.HouseCutOff, orNull(v.Reason),
			v.By, v.At, v.EnteredAt); err != nil {
			return err
		}
	}
	return nil
}

// LiveHouses returns the live houses of a consolidation, by shipment.
func (s ConsolidationStore) LiveHouses(ctx context.Context, tenant phy.TenantID, id phy.ConsolidationID) ([]app.House, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT m.revision, row_to_json(e),
        (SELECT coalesce(json_agg(p ORDER BY p.part_no), '[]') FROM cargo_phy.membership_entry_part p
          WHERE p.tenant_id=e.tenant_id AND p.shipment_id=e.shipment_id AND p.stage=e.stage AND p.entry_no=e.entry_no)
      FROM cargo_phy.membership_entry e JOIN cargo_phy.consol_membership m
        ON m.tenant_id=e.tenant_id AND m.shipment_id=e.shipment_id AND m.stage=e.stage
      WHERE e.tenant_id=$1 AND e.consolidation_id=$2 AND e.ended_at IS NULL ORDER BY e.shipment_id`, string(tenant), string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var houses []app.House
	for rows.Next() {
		var revision int64
		var entryJSON, partJSON []byte
		if err = rows.Scan(&revision, &entryJSON, &partJSON); err != nil {
			return nil, err
		}
		var e entryRow
		var parts []partRow
		if err = errors.Join(json.Unmarshal(entryJSON, &e), json.Unmarshal(partJSON, &parts)); err != nil {
			return nil, err
		}
		houses = append(houses, app.House{ShipmentID: phy.ShipmentID(e.ShipmentID), MembershipRevision: revision, Entry: e.entry(parts)})
	}
	return houses, rows.Err()
}

// ListOpen lists OPEN consolidations by planned departure, with their number of live houses.
func (s ConsolidationStore) ListOpen(ctx context.Context, tenant phy.TenantID, f app.ListFilter) ([]app.Summary, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT c.consolidation_id, c.transport_mode, c.kind, c.operating_legal_entity_id, c.state, c.revision, row_to_json(v),
        (SELECT count(*) FROM cargo_phy.membership_entry e WHERE e.tenant_id=c.tenant_id AND e.consolidation_id=c.consolidation_id AND e.ended_at IS NULL)
      FROM cargo_phy.consolidation c JOIN cargo_phy.consolidation_version v
        ON v.tenant_id=c.tenant_id AND v.consolidation_id=c.consolidation_id AND v.version_no=c.current_version
      WHERE c.tenant_id=$1 AND c.state='OPEN' AND ($2='' OR c.transport_mode=$2) AND ($3='' OR v.origin_code=$3)
        AND ($4='' OR v.destination_code=$4) AND ($5='' OR c.operating_legal_entity_id=$5)
        AND ($6::timestamptz IS NULL OR v.planned_departure >= $6) AND ($7::timestamptz IS NULL OR v.planned_departure < $7)
      ORDER BY v.planned_departure, c.consolidation_id LIMIT $8`,
		string(tenant), string(f.Mode), f.Origin.Code, f.Destination.Code, f.Operator, orNull(f.DepartureFrom), orNull(f.DepartureUntil), f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.Summary
	for rows.Next() {
		var id, mode, kind, operator, state string
		var revision int64
		var versionJSON []byte
		var houses int
		if err = rows.Scan(&id, &mode, &kind, &operator, &state, &revision, &versionJSON, &houses); err != nil {
			return nil, err
		}
		var v versionRow
		if err = json.Unmarshal(versionJSON, &v); err != nil {
			return nil, err
		}
		out = append(out, app.Summary{Revision: revision, LiveHouses: houses, State: phy.ConsolidationState{TenantID: tenant,
			ConsolidationID: phy.ConsolidationID(id), Mode: phy.Mode(mode), Kind: phy.Kind(kind), OperatingLegalEntityID: operator,
			State: phy.State(state), Versions: []phy.Version{v.version()}}})
	}
	return out, rows.Err()
}

// orNull returns nil for a zero value, so that the column stores NULL.
func orNull[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}
