// Package postgres persists FUL shipments, command receipts and Outbox events in the tenant's RLS scope.
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
	app "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/application"
	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// ShipmentStore loads and writes shipments. Load locks the shipment row before reading, so commands on
// one shipment run one after another; Read takes no lock.
type ShipmentStore struct{ DB *bentopg.DB }

func (s ShipmentStore) Load(ctx context.Context, tenant ful.TenantID, id ful.ShipmentID) (*ful.Shipment, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_ful.shipment WHERE tenant_id=$1 AND shipment_id=$2 FOR NO KEY UPDATE`,
		string(tenant), string(id)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	return readShipment(ctx, q, tenant, id)
}

func (s ShipmentStore) Read(ctx context.Context, tenant ful.TenantID, id ful.ShipmentID) (*ful.Shipment, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readShipment(ctx, q, tenant, id)
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Rows of the child tables as json_agg renders them; NULL columns read as zero values.
type obligationRow struct {
	EntryNo           int       `json:"entry_no"`
	ObligationID      string    `json:"obligation_id"`
	ObligationVersion int64     `json:"obligation_version"`
	OrderID           string    `json:"order_id"`
	CustomerPartyID   string    `json:"customer_party_id"`
	LegalEntityID     string    `json:"legal_entity_id"`
	Packs             int64     `json:"packs"`
	PackageType       string    `json:"package_type"`
	GrossWeightUnit   string    `json:"gross_weight_unit"`
	GrossWeight       int64     `json:"gross_weight"`
	VolumeUnit        string    `json:"volume_unit"`
	Volume            int64     `json:"volume"`
	AddedBy           string    `json:"added_by"`
	AddedAt           time.Time `json:"added_at"`
	ReleasedBy        string    `json:"released_by"`
	ReleasedAt        time.Time `json:"released_at"`
}

type planRow struct {
	PlanNo            int       `json:"plan_no"`
	Change            string    `json:"change"`
	TransportMode     string    `json:"transport_mode"`
	OriginScheme      string    `json:"origin_scheme"`
	OriginCode        string    `json:"origin_code"`
	DestinationScheme string    `json:"destination_scheme"`
	DestinationCode   string    `json:"destination_code"`
	DepartureFrom     time.Time `json:"departure_from"`
	DepartureUntil    time.Time `json:"departure_until"`
	ScopePacks        int64     `json:"scope_packs"`
	ScopeWeightUnit   string    `json:"scope_weight_unit"`
	ScopeWeight       int64     `json:"scope_weight"`
	ScopeVolumeUnit   string    `json:"scope_volume_unit"`
	ScopeVolume       int64     `json:"scope_volume"`
	Reason            string    `json:"reason"`
	WrittenBy         string    `json:"written_by"`
	WrittenAt         time.Time `json:"written_at"`
}

type confirmationRow struct {
	PlanNo      int       `json:"plan_no"`
	ConfirmedBy string    `json:"confirmed_by"`
	ConfirmedAt time.Time `json:"confirmed_at"`
}

func quantity(unit string, amount int64) kernel.Quantity {
	return kernel.Quantity{Unit: kernel.UnitCode(unit), Amount: amount}
}

// orNull returns nil for a zero value, so that the column stores NULL.
func orNull[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

// readShipment reads a shipment with its obligations, plan versions and confirmations in one statement,
// so all come from one snapshot even without a lock.
func readShipment(ctx context.Context, q queryRower, tenant ful.TenantID, id ful.ShipmentID) (*ful.Shipment, int64, error) {
	st := ful.ShipmentState{TenantID: tenant, ShipmentID: id}
	var status string
	var cancelledBy, cancelReason *string
	var cancelledAt *time.Time
	var revision int64
	var obligationJSON, planJSON, confirmationJSON []byte
	err := q.QueryRow(ctx, `SELECT s.customer_party_id, s.legal_entity_id, s.status, s.cancelled_by, s.cancelled_at, s.cancel_reason, s.revision,
        (SELECT coalesce(json_agg(o ORDER BY o.entry_no), '[]') FROM cargo_ful.shipment_obligation o
          WHERE o.tenant_id=s.tenant_id AND o.shipment_id=s.shipment_id),
        (SELECT coalesce(json_agg(p ORDER BY p.plan_no), '[]') FROM cargo_ful.shipment_plan_version p
          WHERE p.tenant_id=s.tenant_id AND p.shipment_id=s.shipment_id),
        (SELECT coalesce(json_agg(c ORDER BY c.plan_no), '[]') FROM cargo_ful.shipment_plan_confirmation c
          WHERE c.tenant_id=s.tenant_id AND c.shipment_id=s.shipment_id)
      FROM cargo_ful.shipment s WHERE s.tenant_id=$1 AND s.shipment_id=$2`, string(tenant), string(id)).Scan(
		&st.CustomerPartyID, &st.LegalEntityID, &status, &cancelledBy, &cancelledAt, &cancelReason, &revision,
		&obligationJSON, &planJSON, &confirmationJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	var obligations []obligationRow
	var plans []planRow
	var confirmations []confirmationRow
	if err = errors.Join(json.Unmarshal(obligationJSON, &obligations), json.Unmarshal(planJSON, &plans),
		json.Unmarshal(confirmationJSON, &confirmations)); err != nil {
		return nil, 0, err
	}
	st.Status = ful.ShipmentStatus(status)
	if cancelledAt != nil && cancelledBy != nil && cancelReason != nil {
		st.CancelledBy, st.CancelledAt, st.CancelReason = *cancelledBy, cancelledAt.UTC(), *cancelReason
	}
	for _, o := range obligations {
		st.Obligations = append(st.Obligations, ful.PlannedObligation{
			ObligationRef: ful.ObligationRef{ObligationID: ful.ObligationID(o.ObligationID), ObligationVersion: o.ObligationVersion,
				OrderID: o.OrderID, CustomerPartyID: o.CustomerPartyID, LegalEntityID: o.LegalEntityID, Packs: o.Packs,
				PackageType: o.PackageType, GrossWeight: quantity(o.GrossWeightUnit, o.GrossWeight), Volume: quantity(o.VolumeUnit, o.Volume)},
			AddedBy: o.AddedBy, AddedAt: utc(o.AddedAt), ReleasedBy: o.ReleasedBy, ReleasedAt: utc(o.ReleasedAt)})
	}
	for _, p := range plans {
		st.Plans = append(st.Plans, ful.PlanVersion{No: p.PlanNo, Change: ful.PlanChange(p.Change),
			Route: ful.Route{Mode: ful.TransportMode(p.TransportMode),
				Origin:      kernel.LocationCode{Scheme: kernel.LocationScheme(p.OriginScheme), Code: p.OriginCode},
				Destination: kernel.LocationCode{Scheme: kernel.LocationScheme(p.DestinationScheme), Code: p.DestinationCode},
				Departure:   kernel.ValidityInterval{From: utc(p.DepartureFrom), Until: utc(p.DepartureUntil)}},
			Scope:  ful.CargoScope{Packs: p.ScopePacks, GrossWeight: quantity(p.ScopeWeightUnit, p.ScopeWeight), Volume: quantity(p.ScopeVolumeUnit, p.ScopeVolume)},
			Reason: p.Reason, By: p.WrittenBy, At: utc(p.WrittenAt)})
	}
	for _, c := range confirmations {
		st.Confirmations = append(st.Confirmations, ful.Confirmation{PlanNo: c.PlanNo, By: c.ConfirmedBy, At: utc(c.ConfirmedAt)})
	}
	shipment, err := ful.RestoreShipment(st)
	return shipment, revision, err
}

// Insert writes a new shipment at revision 1 with its first obligation and plan version.
func (s ShipmentStore) Insert(ctx context.Context, shipment *ful.Shipment) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := shipment.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_ful.shipment (tenant_id,shipment_id,customer_party_id,legal_entity_id,status)
      VALUES ($1,$2,$3,$4,$5) RETURNING revision`,
		string(st.TenantID), string(st.ShipmentID), st.CustomerPartyID, st.LegalEntityID, string(st.Status)).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "shipment_pkey" {
		return 0, bentorepo.ErrAlreadyExists
	}
	if err != nil {
		return 0, err
	}
	return revision, s.writeAdded(ctx, q, shipment)
}

// Save writes the shipment's status and cancellation and what it added or released since it was loaded,
// if the stored revision is still the expected one.
func (s ShipmentStore) Save(ctx context.Context, shipment *ful.Shipment, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := shipment.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_ful.shipment SET status=$3,cancelled_by=$5,cancelled_at=$6,cancel_reason=$7,revision=revision+1,updated_at=now()
      WHERE tenant_id=$1 AND shipment_id=$2 AND revision=$4 RETURNING revision`,
		string(st.TenantID), string(st.ShipmentID), string(st.Status), expected,
		orNull(st.CancelledBy), orNull(st.CancelledAt), orNull(st.CancelReason)).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, bentorepo.ErrConflict
	}
	if err != nil {
		return 0, err
	}
	for no, p := range shipment.ReleasedObligations() {
		tag, err := q.Exec(ctx, `UPDATE cargo_ful.shipment_obligation SET released_by=$4,released_at=$5
          WHERE tenant_id=$1 AND shipment_id=$2 AND entry_no=$3 AND released_at IS NULL`,
			string(st.TenantID), string(st.ShipmentID), no, p.ReleasedBy, p.ReleasedAt)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() != 1 {
			return 0, fmt.Errorf("%w: obligation entry %d of shipment %s", bentorepo.ErrConflict, no, st.ShipmentID)
		}
	}
	return revision, s.writeAdded(ctx, q, shipment)
}

// writeAdded inserts the obligations, plan versions and confirmations added since the shipment was loaded.
func (s ShipmentStore) writeAdded(ctx context.Context, q bentopg.Executor, shipment *ful.Shipment) error {
	st := shipment.State()
	first := len(st.Obligations) - len(shipment.AddedObligations())
	for i, p := range shipment.AddedObligations() {
		_, err := q.Exec(ctx, `INSERT INTO cargo_ful.shipment_obligation (tenant_id,shipment_id,entry_no,obligation_id,obligation_version,
            order_id,customer_party_id,legal_entity_id,packs,package_type,gross_weight_unit,gross_weight,volume_unit,volume,
            added_by,added_at,released_by,released_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
			string(st.TenantID), string(st.ShipmentID), first+i+1, string(p.ObligationID), p.ObligationVersion,
			p.OrderID, p.CustomerPartyID, p.LegalEntityID, p.Packs, p.PackageType, string(p.GrossWeight.Unit), p.GrossWeight.Amount,
			orNull(string(p.Volume.Unit)), orNull(p.Volume.Amount), p.AddedBy, p.AddedAt, orNull(p.ReleasedBy), orNull(p.ReleasedAt))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "shipment_obligation_one_live_shipment" {
			return fmt.Errorf("%w: %s", app.ErrPlannedElsewhere, p.ObligationID)
		}
		if err != nil {
			return err
		}
	}
	for _, p := range shipment.AddedPlans() {
		r := p.Route
		var originScheme, originCode, destinationScheme, destinationCode any
		if !r.Origin.IsZero() {
			originScheme, originCode = string(r.Origin.Scheme), r.Origin.Code
		}
		if !r.Destination.IsZero() {
			destinationScheme, destinationCode = string(r.Destination.Scheme), r.Destination.Code
		}
		if _, err := q.Exec(ctx, `INSERT INTO cargo_ful.shipment_plan_version (tenant_id,shipment_id,plan_no,change,transport_mode,
            origin_scheme,origin_code,destination_scheme,destination_code,departure_from,departure_until,scope_packs,scope_weight_unit,
            scope_weight,scope_volume_unit,scope_volume,reason,written_by,written_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
			string(st.TenantID), string(st.ShipmentID), p.No, string(p.Change), orNull(string(r.Mode)),
			originScheme, originCode, destinationScheme, destinationCode, orNull(r.Departure.From), orNull(r.Departure.Until),
			p.Scope.Packs, string(p.Scope.GrossWeight.Unit), p.Scope.GrossWeight.Amount, orNull(string(p.Scope.Volume.Unit)),
			orNull(p.Scope.Volume.Amount), orNull(p.Reason), p.By, p.At); err != nil {
			return err
		}
	}
	for _, c := range shipment.AddedConfirmations() {
		if _, err := q.Exec(ctx, `INSERT INTO cargo_ful.shipment_plan_confirmation (tenant_id,shipment_id,plan_no,confirmed_by,confirmed_at)
            VALUES ($1,$2,$3,$4,$5)`, string(st.TenantID), string(st.ShipmentID), c.PlanNo, c.By, c.At); err != nil {
			return err
		}
	}
	return nil
}

// CommandReceipts stores the result of every committed FUL command for replays.
type CommandReceipts struct{ DB *bentopg.DB }

func (s CommandReceipts) Find(ctx context.Context, tenant ful.TenantID, id string) (app.Receipt, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return app.Receipt{}, false, err
	}
	r := app.Receipt{CommandID: id}
	var shipment string
	err = q.QueryRow(ctx, `SELECT request_sha256,result_id,result_revision FROM cargo_ful.command_receipt
      WHERE tenant_id=$1 AND command_id=$2`, string(tenant), id).Scan(&r.RequestHash, &shipment, &r.NewRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	r.ShipmentID = ful.ShipmentID(shipment)
	return r, true, nil
}

func (s CommandReceipts) Save(ctx context.Context, tenant ful.TenantID, r app.Receipt) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_ful.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision)
      VALUES ($1,$2,$3,'Shipment',$4,$5)`, string(tenant), r.CommandID, r.RequestHash, string(r.ShipmentID), r.NewRevision)
	return err
}
