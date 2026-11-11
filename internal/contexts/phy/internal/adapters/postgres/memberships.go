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

// MembershipStore loads and writes membership ledgers. Load locks the ledger row FOR UPDATE, so house
// commands on one shipment run one after another; two first attaches of one shipment race on the ledger's
// primary key, and one of them is a conflict.
type MembershipStore struct{ DB *bentopg.DB }

var _ app.MembershipStore = MembershipStore{}

// Rows as row_to_json and json_agg render them; NULL columns read as zero values.
type entryRow struct {
	ShipmentID      string    `json:"shipment_id"`
	EntryNo         int       `json:"entry_no"`
	ConsolidationID string    `json:"consolidation_id"`
	StartKind       string    `json:"start_kind"`
	PredecessorNo   int       `json:"predecessor_no"`
	FULRevision     int64     `json:"ful_revision"`
	PlanVersion     int       `json:"plan_version"`
	Packs           int64     `json:"packs"`
	GrossWeightUnit string    `json:"gross_weight_unit"`
	GrossWeight     int64     `json:"gross_weight"`
	VolumeUnit      string    `json:"volume_unit"`
	Volume          int64     `json:"volume"`
	AttachedBy      string    `json:"attached_by"`
	AttachedAt      time.Time `json:"attached_at"`
	EnteredAt       time.Time `json:"entered_at"`
	EndKind         string    `json:"end_kind"`
	EndReason       string    `json:"end_reason"`
	EndText         string    `json:"end_text"`
	EndedBy         string    `json:"ended_by"`
	EndedAt         time.Time `json:"ended_at"`
	EndEnteredAt    time.Time `json:"end_entered_at"`
	SuccessorNo     int       `json:"successor_no"`
}

type partRow struct {
	EntryNo           int    `json:"entry_no"`
	PartNo            int    `json:"part_no"`
	ObligationID      string `json:"obligation_id"`
	ObligationVersion int64  `json:"obligation_version"`
	OrderID           string `json:"order_id"`
	Packs             int64  `json:"packs"`
	PackageType       string `json:"package_type"`
	GrossWeight       int64  `json:"gross_weight"`
	Volume            int64  `json:"volume"`
}

func quantity(unit string, amount int64) kernel.Quantity {
	return kernel.Quantity{Unit: kernel.UnitCode(unit), Amount: amount}
}

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

func (e entryRow) entry(parts []partRow) phy.Entry {
	out := phy.Entry{No: e.EntryNo, ConsolidationID: phy.ConsolidationID(e.ConsolidationID), Start: phy.StartKind(e.StartKind),
		Predecessor: e.PredecessorNo, FULRevision: e.FULRevision, PlanVersion: e.PlanVersion,
		Scope: phy.CargoScope{Packs: e.Packs, GrossWeight: quantity(e.GrossWeightUnit, e.GrossWeight), Volume: quantity(e.VolumeUnit, e.Volume)},
		By:    e.AttachedBy, At: utc(e.AttachedAt), EnteredAt: utc(e.EnteredAt), End: phy.EndKind(e.EndKind), Reason: phy.Reason(e.EndReason),
		ReasonText: e.EndText, EndedBy: e.EndedBy, EndedAt: utc(e.EndedAt), EndEnteredAt: utc(e.EndEnteredAt), Successor: e.SuccessorNo}
	for _, p := range parts {
		out.Parts = append(out.Parts, phy.Part{ObligationID: p.ObligationID, ObligationVersion: p.ObligationVersion, OrderID: p.OrderID,
			Packs: p.Packs, PackageType: p.PackageType, GrossWeight: quantity(e.GrossWeightUnit, p.GrossWeight), Volume: quantity(e.VolumeUnit, p.Volume)})
	}
	return out
}

func (s MembershipStore) Load(ctx context.Context, tenant phy.TenantID, shipment phy.ShipmentID) (*phy.Membership, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_phy.consol_membership WHERE tenant_id=$1 AND shipment_id=$2 AND stage=$3 FOR UPDATE`,
		string(tenant), string(shipment), phy.StageMain).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		m, err := phy.NewMembership(tenant, shipment)
		return m, 0, err
	}
	if err != nil {
		return nil, 0, err
	}
	return readMembership(ctx, q, tenant, shipment)
}

// LoadShared locks the ledger FOR SHARE, for loading the house: a detach or transfer waits for it.
func (s MembershipStore) LoadShared(ctx context.Context, tenant phy.TenantID, shipment phy.ShipmentID) (*phy.Membership, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_phy.consol_membership WHERE tenant_id=$1 AND shipment_id=$2 AND stage=$3 FOR SHARE`,
		string(tenant), string(shipment), phy.StageMain).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: %s was never attached", phy.ErrNotAttached, shipment)
	}
	if err != nil {
		return nil, 0, err
	}
	return readMembership(ctx, q, tenant, shipment)
}

func (s MembershipStore) Read(ctx context.Context, tenant phy.TenantID, shipment phy.ShipmentID) (*phy.Membership, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readMembership(ctx, q, tenant, shipment)
}

// readMembership reads a ledger with its entries and their parts in one statement.
func readMembership(ctx context.Context, q queryRower, tenant phy.TenantID, shipment phy.ShipmentID) (*phy.Membership, int64, error) {
	var revision int64
	var entryJSON, partJSON []byte
	err := q.QueryRow(ctx, `SELECT m.revision,
        (SELECT coalesce(json_agg(e ORDER BY e.entry_no), '[]') FROM cargo_phy.membership_entry e
          WHERE e.tenant_id=m.tenant_id AND e.shipment_id=m.shipment_id AND e.stage=m.stage),
        (SELECT coalesce(json_agg(p ORDER BY p.entry_no, p.part_no), '[]') FROM cargo_phy.membership_entry_part p
          WHERE p.tenant_id=m.tenant_id AND p.shipment_id=m.shipment_id AND p.stage=m.stage)
      FROM cargo_phy.consol_membership m WHERE m.tenant_id=$1 AND m.shipment_id=$2 AND m.stage=$3`,
		string(tenant), string(shipment), phy.StageMain).Scan(&revision, &entryJSON, &partJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: membership of %s", bentorepo.ErrNotFound, shipment)
	}
	if err != nil {
		return nil, 0, err
	}
	var entries []entryRow
	var parts []partRow
	if err = errors.Join(json.Unmarshal(entryJSON, &entries), json.Unmarshal(partJSON, &parts)); err != nil {
		return nil, 0, err
	}
	byEntry := map[int][]partRow{}
	for _, p := range parts {
		byEntry[p.EntryNo] = append(byEntry[p.EntryNo], p)
	}
	st := phy.MembershipState{TenantID: tenant, ShipmentID: shipment, Stage: phy.StageMain}
	for _, e := range entries {
		st.Entries = append(st.Entries, e.entry(byEntry[e.EntryNo]))
	}
	m, err := phy.RestoreMembership(st)
	return m, revision, err
}

// Save inserts the ledger at revision 0, or bumps its revision if the stored one is still the expected one;
// then it ends the stored entries the command ended and inserts the new ones, in that order, so that the
// one-live-entry index sees the end first.
func (s MembershipStore) Save(ctx context.Context, m *phy.Membership, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := m.State()
	var revision int64
	if expected == 0 {
		err = q.QueryRow(ctx, `INSERT INTO cargo_phy.consol_membership (tenant_id,shipment_id,stage) VALUES ($1,$2,$3) RETURNING revision`,
			string(st.TenantID), string(st.ShipmentID), st.Stage).Scan(&revision)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "consol_membership_pkey" {
			return 0, fmt.Errorf("%w: another command attached %s first", bentorepo.ErrConflict, st.ShipmentID)
		}
	} else {
		err = q.QueryRow(ctx, `UPDATE cargo_phy.consol_membership SET revision=revision+1,updated_at=now()
          WHERE tenant_id=$1 AND shipment_id=$2 AND stage=$3 AND revision=$4 RETURNING revision`,
			string(st.TenantID), string(st.ShipmentID), st.Stage, expected).Scan(&revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("%w: membership of %s", bentorepo.ErrConflict, st.ShipmentID)
		}
	}
	if err != nil {
		return 0, err
	}
	for no, e := range m.EndedEntries() {
		tag, err := q.Exec(ctx, `UPDATE cargo_phy.membership_entry SET end_kind=$5,end_reason=$6,end_text=$7,ended_by=$8,ended_at=$9,
            end_entered_at=$10,successor_no=$11 WHERE tenant_id=$1 AND shipment_id=$2 AND stage=$3 AND entry_no=$4 AND ended_at IS NULL`,
			string(st.TenantID), string(st.ShipmentID), st.Stage, no, string(e.End), orNull(string(e.Reason)), orNull(e.ReasonText),
			e.EndedBy, e.EndedAt, e.EndEnteredAt, orNull(e.Successor))
		if err != nil {
			return 0, entryError(err)
		}
		if tag.RowsAffected() != 1 {
			return 0, fmt.Errorf("%w: entry %d of %s", bentorepo.ErrConflict, no, st.ShipmentID)
		}
	}
	for _, e := range m.AddedEntries() {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_phy.membership_entry (tenant_id,shipment_id,stage,entry_no,consolidation_id,start_kind,
            predecessor_no,ful_revision,plan_version,packs,gross_weight_unit,gross_weight,volume_unit,volume,attached_by,attached_at,entered_at,
            end_kind,end_reason,end_text,ended_by,ended_at,end_entered_at,successor_no)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`,
			string(st.TenantID), string(st.ShipmentID), st.Stage, e.No, string(e.ConsolidationID), string(e.Start), orNull(e.Predecessor),
			e.FULRevision, e.PlanVersion, e.Scope.Packs, string(e.Scope.GrossWeight.Unit), e.Scope.GrossWeight.Amount,
			orNull(string(e.Scope.Volume.Unit)), orNull(e.Scope.Volume.Amount), e.By, e.At, e.EnteredAt, orNull(string(e.End)),
			orNull(string(e.Reason)), orNull(e.ReasonText), orNull(e.EndedBy), orNull(e.EndedAt), orNull(e.EndEnteredAt), orNull(e.Successor)); err != nil {
			return 0, entryError(err)
		}
		for i, p := range e.Parts {
			if _, err = q.Exec(ctx, `INSERT INTO cargo_phy.membership_entry_part (tenant_id,shipment_id,stage,entry_no,part_no,obligation_id,
                obligation_version,order_id,packs,package_type,gross_weight,volume) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
				string(st.TenantID), string(st.ShipmentID), st.Stage, e.No, i+1, p.ObligationID, p.ObligationVersion, p.OrderID, p.Packs,
				p.PackageType, p.GrossWeight.Amount, orNull(p.Volume.Amount)); err != nil {
				return 0, err
			}
		}
	}
	return revision, nil
}

// entryError maps the one-live-entry index and the frozen-houses trigger to a conflict: another command
// changed the house or its consolidation first.
func entryError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && ((pgErr.Code == "23505" && pgErr.ConstraintName == "membership_entry_one_live") ||
		(pgErr.Code == "23514" && pgErr.ConstraintName == "membership_entry_frozen")) {
		return fmt.Errorf("%w: %s", bentorepo.ErrConflict, pgErr.Message)
	}
	return err
}

// Conflict reports whether a commit failed on one of PHY's deferred checks, which only a concurrent command
// can trip: a house joining a consolidation that closed or was cancelled meanwhile, loads of one house from two
// lots together exceeding its plan, a house leaving or replanned while it was being loaded, a consolidation
// reopened, cancelled or reconciled while one of its tenders changed.
func Conflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		return false
	}
	switch pgErr.ConstraintName {
	case "membership_entry_consolidation_open", "consolidation_cancelled_without_houses", "load_line_conservation", "membership_entry_loaded",
		"consolidation_executions_closed", "consolidation_reopen_before_tender", "load_execution_consolidation", "cargo_lot_complete":
		return true
	}
	return false
}
