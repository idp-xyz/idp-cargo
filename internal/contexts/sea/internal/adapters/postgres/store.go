// Package postgres persists SEA movements, HBL series and register, command receipts and Outbox events in the
// tenant's RLS scope.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/application"
	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// MovementStore loads and writes ocean movements.
type MovementStore struct{ DB *bentopg.DB }

var _ app.MovementStore = MovementStore{}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type (
	linkRow struct {
		LinkNo          int       `json:"link_no"`
		BookingID       string    `json:"booking_id"`
		BookingRevision int64     `json:"booking_revision"`
		CapacityVersion int       `json:"capacity_version"`
		LinkedBy        string    `json:"linked_by"`
		LinkedAt        time.Time `json:"linked_at"`
		EnteredAt       time.Time `json:"entered_at"`
	}
	legRow struct {
		LinkNo    int       `json:"link_no"`
		LegNo     int       `json:"leg_no"`
		Vessel    string    `json:"vessel"`
		VesselIMO string    `json:"vessel_imo"`
		Voyage    string    `json:"voyage"`
		FromCode  string    `json:"from_code"`
		ToCode    string    `json:"to_code"`
		ETD       time.Time `json:"etd"`
		ETA       time.Time `json:"eta"`
	}
	cutOffRow struct {
		LinkNo       int       `json:"link_no"`
		LegNo        int       `json:"leg_no"`
		Kind         string    `json:"kind"`
		CutOffAt     time.Time `json:"cut_off_at"`
		TimeZone     string    `json:"time_zone"`
		LocationCode string    `json:"location_code"`
	}
	billRow struct {
		BillNo            int       `json:"bill_no"`
		BillNumber        string    `json:"bill_number"`
		Kind              string    `json:"kind"`
		EvidenceSHA256    string    `json:"evidence_sha256"`
		EvidenceStorageID string    `json:"evidence_storage_id"`
		EvidenceMediaType string    `json:"evidence_media_type"`
		EvidenceCaptured  time.Time `json:"evidence_captured_at"`
		Status            string    `json:"status"`
		RecordedBy        string    `json:"recorded_by"`
		RecordedAt        time.Time `json:"recorded_at"`
		EnteredAt         time.Time `json:"entered_at"`
	}
	entryRow struct {
		EntryNo         int       `json:"entry_no"`
		ShipmentID      string    `json:"shipment_id"`
		HBL             string    `json:"hbl"`
		FULRevision     int64     `json:"ful_revision"`
		PlanVersion     int       `json:"plan_version"`
		ContainerMode   string    `json:"container_mode"`
		WindowFrom      time.Time `json:"window_from"`
		WindowUntil     time.Time `json:"window_until"`
		Packs           int64     `json:"packs"`
		GrossWeightUnit string    `json:"gross_weight_unit"`
		GrossWeight     int64     `json:"gross_weight"`
		VolumeUnit      string    `json:"volume_unit"`
		Volume          int64     `json:"volume"`
		Basis           string    `json:"basis"`
		EvidenceVersion int       `json:"evidence_version"`
		StartKind       string    `json:"start_kind"`
		PredecessorNo   int       `json:"predecessor_no"`
		AssociatedBy    string    `json:"associated_by"`
		AssociatedAt    time.Time `json:"associated_at"`
		EnteredAt       time.Time `json:"entered_at"`
		EndKind         string    `json:"end_kind"`
		EndedBy         string    `json:"ended_by"`
		EndedAt         time.Time `json:"ended_at"`
		EndEnteredAt    time.Time `json:"end_entered_at"`
		SuccessorNo     int       `json:"successor_no"`
	}
	partRow struct {
		EntryNo           int    `json:"entry_no"`
		ObligationID      string `json:"obligation_id"`
		ObligationVersion int64  `json:"obligation_version"`
		OrderID           string `json:"order_id"`
		Packs             int64  `json:"packs"`
		PackageType       string `json:"package_type"`
		GrossWeightUnit   string `json:"gross_weight_unit"`
		GrossWeight       int64  `json:"gross_weight"`
		VolumeUnit        string `json:"volume_unit"`
		Volume            int64  `json:"volume"`
		GoodsDescription  string `json:"goods_description"`
	}
	stuffingRow struct {
		EntryNo         int       `json:"entry_no"`
		ExecutionID     string    `json:"execution_id"`
		LoadUnitNo      int       `json:"load_unit_no"`
		TypeCode        string    `json:"type_code"`
		Packs           int64     `json:"packs"`
		GrossWeightUnit string    `json:"gross_weight_unit"`
		GrossWeight     int64     `json:"gross_weight"`
		VolumeUnit      string    `json:"volume_unit"`
		Volume          int64     `json:"volume"`
		LineNos         []int     `json:"line_nos"`
		Seal            string    `json:"seal"`
		SealSHA256      string    `json:"seal_sha256"`
		SealedAt        time.Time `json:"sealed_at"`
	}
)

func quantity(unit string, amount int64) kernel.Quantity {
	return kernel.Quantity{Unit: kernel.UnitCode(unit), Amount: amount}
}

func unlocode(code string) kernel.LocationCode {
	if code == "" {
		return kernel.LocationCode{}
	}
	return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}
}

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

func orNull[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func child(table, order string) string {
	return `(SELECT coalesce(json_agg(x ORDER BY ` + order + `), '[]') FROM cargo_sea.` + table + ` x WHERE x.tenant_id=m.tenant_id AND x.movement_id=m.movement_id)`
}

func (s MovementStore) Load(ctx context.Context, tenant sea.TenantID, id sea.MovementID) (*sea.Movement, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_sea.ocean_movement WHERE tenant_id=$1 AND movement_id=$2 FOR NO KEY UPDATE`, string(tenant), string(id)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: movement %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	return readMovement(ctx, q, tenant, id)
}

func (s MovementStore) Read(ctx context.Context, tenant sea.TenantID, id sea.MovementID) (*sea.Movement, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readMovement(ctx, q, tenant, id)
}

type docFactsRow struct {
	VersionNo   int       `json:"version_no"`
	Content     string    `json:"content"`
	SHA256      string    `json:"content_sha256"`
	FinalizedBy string    `json:"finalized_by"`
	FinalizedAt time.Time `json:"finalized_at"`
	EnteredAt   time.Time `json:"entered_at"`
}

func readMovement(ctx context.Context, q queryRower, tenant sea.TenantID, id sea.MovementID) (*sea.Movement, int64, error) {
	st := sea.MovementState{TenantID: tenant, MovementID: id}
	var kind, state, originCode, destCode, booking string
	var cancelReason, cancelledBy *string
	var cancelledAt *time.Time
	var revision int64
	var links, legs, cutOffs, bills, entries, parts, stuffings, facts []byte
	err := q.QueryRow(ctx, `SELECT m.consolidation_id, m.kind, m.operating_legal_entity_id, m.carrier_party_id, m.carrier_party_revision, m.carrier_role,
        m.origin_code, m.destination_code, m.location_edition, m.state, m.booking_id, m.cancel_reason, m.cancelled_by, m.cancelled_at, m.revision, `+
		child("booking_link", "x.link_no")+`, `+child("voyage_leg", "x.link_no, x.leg_no")+`, `+child("voyage_cut_off", "x.link_no, x.leg_no, x.cut_off_no")+`, `+
		child("master_bill", "x.bill_no")+`, `+child("house_bill", "x.entry_no")+`, `+child("house_bill_part", "x.entry_no, x.part_no")+`, `+
		child("house_bill_stuffing", "x.entry_no, x.stuffing_no")+`, `+child("ocean_doc_facts_version", "x.version_no")+`
      FROM cargo_sea.ocean_movement m WHERE m.tenant_id=$1 AND m.movement_id=$2`, string(tenant), string(id)).Scan(
		&st.ConsolidationID, &kind, &st.LegalEntityID, &st.Carrier.PartyID, &st.Carrier.PartyRevision, &st.Carrier.Role, &originCode, &destCode,
		&st.Edition, &state, &booking, &cancelReason, &cancelledBy, &cancelledAt, &revision, &links, &legs, &cutOffs, &bills, &entries, &parts, &stuffings, &facts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: movement %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	var lr []linkRow
	var gr []legRow
	var cr []cutOffRow
	var br []billRow
	var er []entryRow
	var pr []partRow
	var sr []stuffingRow
	var fr []docFactsRow
	if err = errors.Join(json.Unmarshal(links, &lr), json.Unmarshal(legs, &gr), json.Unmarshal(cutOffs, &cr), json.Unmarshal(bills, &br),
		json.Unmarshal(entries, &er), json.Unmarshal(parts, &pr), json.Unmarshal(stuffings, &sr), json.Unmarshal(facts, &fr)); err != nil {
		return nil, 0, err
	}
	st.Kind, st.State, st.Origin, st.Destination = sea.Kind(kind), sea.State(state), unlocode(originCode), unlocode(destCode)
	if cancelledAt != nil {
		st.CancelReason, st.CancelledBy, st.CancelledAt = deref(cancelReason), deref(cancelledBy), cancelledAt.UTC()
	}
	for _, l := range lr {
		link := sea.Link{No: l.LinkNo, BookingID: l.BookingID, BookingRevision: l.BookingRevision, CapacityVersion: l.CapacityVersion, By: l.LinkedBy,
			At: utc(l.LinkedAt), EnteredAt: utc(l.EnteredAt)}
		for _, g := range gr {
			if g.LinkNo != l.LinkNo {
				continue
			}
			leg := sea.Leg{Vessel: g.Vessel, VesselIMO: g.VesselIMO, Voyage: g.Voyage, From: unlocode(g.FromCode), To: unlocode(g.ToCode), ETD: utc(g.ETD),
				ETA: utc(g.ETA)}
			for _, c := range cr {
				if c.LinkNo == g.LinkNo && c.LegNo == g.LegNo {
					leg.CutOffs = append(leg.CutOffs, sea.CutOff{Kind: c.Kind, At: utc(c.CutOffAt), TimeZone: c.TimeZone, Location: unlocode(c.LocationCode)})
				}
			}
			link.Legs = append(link.Legs, leg)
		}
		st.Links = append(st.Links, link)
	}
	if len(st.Links) > 0 && st.Links[0].BookingID != booking {
		return nil, 0, fmt.Errorf("%w: movement %s is of booking %s, its first link of %s", sea.ErrInvalidMovement, id, booking, st.Links[0].BookingID)
	}
	for _, b := range br {
		st.Bills = append(st.Bills, sea.MasterBill{No: b.BillNo, Number: b.BillNumber, Kind: b.Kind, Status: b.Status, By: b.RecordedBy, At: utc(b.RecordedAt),
			EnteredAt: utc(b.EnteredAt), Evidence: sea.Evidence{SHA256: b.EvidenceSHA256, StorageID: b.EvidenceStorageID, MediaType: b.EvidenceMediaType,
				CapturedAt: utc(b.EvidenceCaptured)}})
	}
	for _, e := range er {
		entry := sea.Entry{No: e.EntryNo, ShipmentID: e.ShipmentID, HBL: e.HBL, FULRevision: e.FULRevision, PlanVersion: e.PlanVersion,
			ContainerMode: e.ContainerMode, Window: kernel.ValidityInterval{From: utc(e.WindowFrom), Until: utc(e.WindowUntil)},
			Facts: sea.Facts{Packs: e.Packs, GrossWeight: quantity(e.GrossWeightUnit, e.GrossWeight), Volume: quantity(e.VolumeUnit, e.Volume),
				Basis: e.Basis, Evidence: e.EvidenceVersion},
			Start: e.StartKind, Predecessor: e.PredecessorNo, By: e.AssociatedBy, At: utc(e.AssociatedAt), EnteredAt: utc(e.EnteredAt), End: e.EndKind,
			EndedBy: e.EndedBy, EndedAt: utc(e.EndedAt), EndEnteredAt: utc(e.EndEnteredAt), Successor: e.SuccessorNo}
		for _, p := range pr {
			if p.EntryNo == e.EntryNo {
				entry.Facts.Parts = append(entry.Facts.Parts, sea.Part{ObligationID: p.ObligationID, ObligationVersion: p.ObligationVersion, OrderID: p.OrderID,
					Packs: p.Packs, PackageType: p.PackageType, GrossWeight: quantity(p.GrossWeightUnit, p.GrossWeight), Volume: quantity(p.VolumeUnit, p.Volume),
					GoodsDescription: p.GoodsDescription})
			}
		}
		for _, x := range sr {
			if x.EntryNo == e.EntryNo {
				entry.Facts.Stuffings = append(entry.Facts.Stuffings, sea.Stuffing{ExecutionID: x.ExecutionID, LoadUnitNo: x.LoadUnitNo, TypeCode: x.TypeCode,
					Packs: x.Packs, GrossWeight: quantity(x.GrossWeightUnit, x.GrossWeight), Volume: quantity(x.VolumeUnit, x.Volume), LineNos: x.LineNos,
					Seal: x.Seal, SealSHA256: x.SealSHA256, SealedAt: utc(x.SealedAt)})
			}
		}
		st.Entries = append(st.Entries, entry)
	}
	for _, f := range fr {
		st.DocFacts = append(st.DocFacts, sea.DocFactsVersion{No: f.VersionNo, Content: []byte(f.Content), SHA256: f.SHA256, By: f.FinalizedBy,
			At: utc(f.FinalizedAt), EnteredAt: utc(f.EnteredAt)})
	}
	m, err := sea.RestoreMovement(st)
	return m, revision, err
}

// Insert writes a new movement at revision 1 with its first link.
func (s MovementStore) Insert(ctx context.Context, m *sea.Movement) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := m.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_sea.ocean_movement (tenant_id,movement_id,consolidation_id,kind,operating_legal_entity_id,carrier_party_id,
        carrier_party_revision,carrier_role,origin_code,destination_code,location_edition,state,booking_id)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING revision`,
		string(st.TenantID), string(st.MovementID), st.ConsolidationID, string(st.Kind), st.LegalEntityID, st.Carrier.PartyID, st.Carrier.PartyRevision,
		st.Carrier.Role, st.Origin.Code, st.Destination.Code, st.Edition, string(st.State), m.Current().BookingID).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "ocean_movement_pkey":
			return 0, fmt.Errorf("%w: movement %s", bentorepo.ErrAlreadyExists, st.MovementID)
		case "ocean_movement_one_open":
			return 0, fmt.Errorf("%w: consolidation %s has an open ocean movement", bentorepo.ErrConflict, st.ConsolidationID)
		}
	}
	if err != nil {
		return 0, err
	}
	return revision, s.writeAdded(ctx, q, m, revision)
}

// Save writes the state and what the movement added or changed since it was loaded, if the stored revision is
// still the expected one.
func (s MovementStore) Save(ctx context.Context, m *sea.Movement, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := m.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_sea.ocean_movement SET state=$3,cancel_reason=$4,cancelled_by=$5,cancelled_at=$6,revision=revision+1,
        updated_at=now() WHERE tenant_id=$1 AND movement_id=$2 AND revision=$7 RETURNING revision`, string(st.TenantID), string(st.MovementID),
		string(st.State), orNull(st.CancelReason), orNull(st.CancelledBy), orNull(st.CancelledAt), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: movement %s", bentorepo.ErrConflict, st.MovementID)
	}
	if err != nil {
		return 0, err
	}
	return revision, s.writeAdded(ctx, q, m, revision)
}

// writeAdded writes what the movement added, the master bills that left CURRENT and the entries that ended. A
// superseded master bill is written before its successor, since one reference is current at a time.
func (s MovementStore) writeAdded(ctx context.Context, q bentopg.Executor, m *sea.Movement, revision int64) error {
	st := m.State()
	exec := func(sql string, args ...any) error {
		_, err := q.Exec(ctx, sql, append([]any{string(st.TenantID), string(st.MovementID)}, args...)...)
		return err
	}
	links, bills, entries := m.Added()
	retired, ended := m.Changed()
	for no, b := range retired {
		tag, err := q.Exec(ctx, `UPDATE cargo_sea.master_bill SET status=$4 WHERE tenant_id=$1 AND movement_id=$2 AND bill_no=$3 AND status='CURRENT'`,
			string(st.TenantID), string(st.MovementID), no, b.Status)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: master bill %d of %s", bentorepo.ErrConflict, no, st.MovementID)
		}
	}
	for no, e := range ended {
		tag, err := q.Exec(ctx, `UPDATE cargo_sea.house_bill SET end_kind=$4,ended_by=$5,ended_at=$6,end_entered_at=$7,successor_no=$8
          WHERE tenant_id=$1 AND movement_id=$2 AND entry_no=$3 AND ended_at IS NULL`, string(st.TenantID), string(st.MovementID), no, e.End, e.EndedBy,
			e.EndedAt, e.EndEnteredAt, orNull(e.Successor))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: entry %d of %s", bentorepo.ErrConflict, no, st.MovementID)
		}
	}
	for _, l := range links {
		if err := exec(`INSERT INTO cargo_sea.booking_link (tenant_id,movement_id,link_no,booking_id,booking_revision,capacity_version,linked_by,linked_at,
            entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, l.No, l.BookingID, l.BookingRevision, l.CapacityVersion, l.By, l.At, l.EnteredAt); err != nil {
			return err
		}
		for i, g := range l.Legs {
			if err := exec(`INSERT INTO cargo_sea.voyage_leg (tenant_id,movement_id,link_no,leg_no,vessel,vessel_imo,voyage,from_code,to_code,etd,eta)
                VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, l.No, i+1, g.Vessel, orNull(g.VesselIMO), g.Voyage, g.From.Code, g.To.Code, g.ETD,
				orNull(g.ETA)); err != nil {
				return err
			}
			for j, c := range g.CutOffs {
				if err := exec(`INSERT INTO cargo_sea.voyage_cut_off (tenant_id,movement_id,link_no,leg_no,cut_off_no,kind,cut_off_at,time_zone,location_code)
                    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, l.No, i+1, j+1, c.Kind, c.At, c.TimeZone, orNull(c.Location.Code)); err != nil {
					return err
				}
			}
		}
	}
	for _, b := range bills {
		err := exec(`INSERT INTO cargo_sea.master_bill (tenant_id,movement_id,bill_no,bill_number,kind,issuer_party_id,evidence_sha256,evidence_storage_id,
            evidence_media_type,evidence_captured_at,status,recorded_by,recorded_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			b.No, b.Number, b.Kind, st.Carrier.PartyID, b.Evidence.SHA256, b.Evidence.StorageID, b.Evidence.MediaType, b.Evidence.CapturedAt, b.Status,
			b.By, b.At, b.EnteredAt)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "master_bill_current_number" {
			return fmt.Errorf("%w: %s", app.ErrMasterBillInUse, b.Number)
		}
		if err != nil {
			return err
		}
	}
	for _, e := range entries {
		f := e.Facts
		err := exec(`INSERT INTO cargo_sea.house_bill (tenant_id,movement_id,entry_no,shipment_id,hbl,ful_revision,plan_version,container_mode,window_from,
            window_until,packs,gross_weight_unit,gross_weight,volume_unit,volume,basis,evidence_version,start_kind,predecessor_no,associated_by,
            associated_at,entered_at,end_kind,ended_by,ended_at,end_entered_at,successor_no)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
			e.No, e.ShipmentID, e.HBL, e.FULRevision, e.PlanVersion, orNull(e.ContainerMode), e.Window.From, orNull(e.Window.Until), f.Packs,
			string(f.GrossWeight.Unit), f.GrossWeight.Amount, orNull(string(f.Volume.Unit)), orNull(f.Volume.Amount), f.Basis, f.Evidence, e.Start,
			orNull(e.Predecessor), e.By, e.At, e.EnteredAt, orNull(e.End), orNull(e.EndedBy), orNull(e.EndedAt), orNull(e.EndEnteredAt), orNull(e.Successor))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && (pgErr.ConstraintName == "house_bill_one_live" || pgErr.ConstraintName == "house_bill_live_hbl") {
			return fmt.Errorf("%w: %s is associated with another movement", sea.ErrAlreadyAssociated, e.ShipmentID)
		}
		if err != nil {
			return err
		}
		for i, p := range f.Parts {
			if err := exec(`INSERT INTO cargo_sea.house_bill_part (tenant_id,movement_id,entry_no,part_no,obligation_id,obligation_version,order_id,packs,
                package_type,gross_weight_unit,gross_weight,volume_unit,volume,goods_description) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
				e.No, i+1, p.ObligationID, p.ObligationVersion, p.OrderID, p.Packs, p.PackageType, string(p.GrossWeight.Unit), p.GrossWeight.Amount,
				orNull(string(p.Volume.Unit)), orNull(p.Volume.Amount), orNull(p.GoodsDescription)); err != nil {
				return err
			}
		}
		for i, x := range f.Stuffings {
			if err := exec(`INSERT INTO cargo_sea.house_bill_stuffing (tenant_id,movement_id,entry_no,stuffing_no,execution_id,load_unit_no,type_code,packs,
                gross_weight_unit,gross_weight,volume_unit,volume,line_nos,seal,seal_sha256,sealed_at)
                VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
				e.No, i+1, x.ExecutionID, x.LoadUnitNo, x.TypeCode, x.Packs, orNull(string(x.GrossWeight.Unit)), orNull(x.GrossWeight.Amount),
				orNull(string(x.Volume.Unit)), orNull(x.Volume.Amount), x.LineNos, x.Seal, orNull(x.SealSHA256), x.SealedAt); err != nil {
				return err
			}
		}
	}
	for _, v := range m.AddedDocFacts() {
		if err := exec(`INSERT INTO cargo_sea.ocean_doc_facts_version (tenant_id,movement_id,version_no,content,content_sha256,movement_revision,
            finalized_by,finalized_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			v.No, string(v.Content), v.SHA256, revision, v.By, v.At, v.EnteredAt); err != nil {
			return err
		}
	}
	return nil
}

// ByMasterBill finds the movement a number is the current master bill reference of, for an issuer.
func (s MovementStore) ByMasterBill(ctx context.Context, tenant sea.TenantID, issuer, number string) (sea.MovementID, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return "", false, err
	}
	var id string
	err = q.QueryRow(ctx, `SELECT movement_id FROM cargo_sea.master_bill WHERE tenant_id=$1 AND issuer_party_id=$2 AND bill_number=$3 AND status='CURRENT'`,
		string(tenant), issuer, number).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return sea.MovementID(id), err == nil, err
}

// SeriesStore reads and writes HBL series.
type SeriesStore struct{ DB *bentopg.DB }

var _ app.SeriesStore = SeriesStore{}

func (s SeriesStore) Read(ctx context.Context, tenant sea.TenantID, le string) (*sea.Series, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	st := sea.SeriesState{TenantID: tenant, LegalEntityID: le}
	var revision int64
	err = q.QueryRow(ctx, `SELECT template,first_number,seed_period,registered_by,registered_at,entered_at,revision FROM cargo_sea.hbl_series
      WHERE tenant_id=$1 AND legal_entity_id=$2`, string(tenant), le).Scan(&st.Template, &st.FirstNumber, &st.SeedPeriod, &st.By, &st.At, &st.EnteredAt, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: %s", app.ErrNoSeries, le)
	}
	if err != nil {
		return nil, 0, err
	}
	st.At, st.EnteredAt = st.At.UTC(), st.EnteredAt.UTC()
	series, err := sea.RestoreSeries(st)
	return series, revision, err
}

func (s SeriesStore) Insert(ctx context.Context, series *sea.Series) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := series.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_sea.hbl_series (tenant_id,legal_entity_id,template,first_number,seed_period,registered_by,registered_at,
      entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING revision`, string(st.TenantID), st.LegalEntityID, st.Template, st.FirstNumber, st.SeedPeriod,
		st.By, st.At, st.EnteredAt).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "hbl_series_pkey" {
		return 0, fmt.Errorf("%w: %s has an HBL series", bentorepo.ErrAlreadyExists, st.LegalEntityID)
	}
	return revision, err
}

// Register is the HBL register.
type Register struct{ DB *bentopg.DB }

var _ app.HBLRegister = Register{}

func (r Register) OfShipment(ctx context.Context, tenant sea.TenantID, le, shipment string) (string, bool, error) {
	q, err := r.DB.ReadExecutor(ctx)
	if err != nil {
		return "", false, err
	}
	var hbl string
	err = q.QueryRow(ctx, `SELECT hbl FROM cargo_sea.hbl_number WHERE tenant_id=$1 AND legal_entity_id=$2 AND shipment_id=$3`, string(tenant), le, shipment).Scan(&hbl)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return hbl, err == nil, err
}

func (r Register) Find(ctx context.Context, tenant sea.TenantID, le, hbl string) (app.Registration, bool, error) {
	q, err := r.DB.ReadExecutor(ctx)
	if err != nil {
		return app.Registration{}, false, err
	}
	reg := app.Registration{HBL: hbl}
	var movement *string
	var entry *int
	err = q.QueryRow(ctx, `SELECT n.shipment_id, e.movement_id, e.entry_no FROM cargo_sea.hbl_number n
      LEFT JOIN cargo_sea.house_bill e ON e.tenant_id=n.tenant_id AND e.shipment_id=n.shipment_id AND e.hbl=n.hbl AND e.ended_at IS NULL
      WHERE n.tenant_id=$1 AND n.legal_entity_id=$2 AND n.hbl=$3`, string(tenant), le, hbl).Scan(&reg.ShipmentID, &movement, &entry)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Registration{}, false, nil
	}
	if err != nil {
		return app.Registration{}, false, err
	}
	reg.MovementID = sea.MovementID(deref(movement))
	if entry != nil {
		reg.EntryNo = *entry
	}
	return reg, true, nil
}

func (r Register) Claim(ctx context.Context, tenant sea.TenantID, le, hbl, shipment string, drawn bool, at time.Time) error {
	q, err := r.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_sea.hbl_number (tenant_id,legal_entity_id,hbl,shipment_id,drawn,claimed_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		string(tenant), le, hbl, shipment, drawn, at)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "hbl_number_pkey" {
			return fmt.Errorf("%w: %s", app.ErrHBLInUse, hbl)
		}
		return fmt.Errorf("%w: %s claimed an HBL meanwhile", bentorepo.ErrConflict, shipment)
	}
	return err
}

// CommandReceipts stores the result of every committed SEA command for replays.
type CommandReceipts struct{ DB *bentopg.DB }

var _ app.Receipts = CommandReceipts{}

func (s CommandReceipts) Find(ctx context.Context, tenant sea.TenantID, id string) (app.Receipt, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return app.Receipt{}, false, err
	}
	r := app.Receipt{CommandID: id}
	var entry, bill *int
	var hbl *string
	var seal, assertion, eventNo, facts *int
	err = q.QueryRow(ctx, `SELECT request_sha256,result_type,result_id,result_revision,result_entry_no,result_hbl,result_bill_no,result_seal_no,result_assertion_no,result_event_no,result_facts_version
      FROM cargo_sea.command_receipt WHERE tenant_id=$1 AND command_id=$2`, string(tenant), id).Scan(
		&r.RequestHash, &r.Kind, &r.SubjectID, &r.NewRevision, &entry, &hbl, &bill, &seal, &assertion, &eventNo, &facts)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	if entry != nil {
		r.EntryNo = *entry
	}
	if bill != nil {
		r.BillNo = *bill
	}
	if seal != nil {
		r.SealNo = *seal
	}
	if assertion != nil {
		r.AssertionNo = *assertion
	}
	if eventNo != nil {
		r.EventNo = *eventNo
	}
	if facts != nil {
		r.FactsVersion = *facts
	}
	r.HBL = deref(hbl)
	return r, true, nil
}

func (s CommandReceipts) Save(ctx context.Context, tenant sea.TenantID, r app.Receipt, kind string) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_sea.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision,result_entry_no,
      result_hbl,result_bill_no,result_seal_no,result_assertion_no,result_event_no,result_facts_version) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		string(tenant), r.CommandID, r.RequestHash, kind, r.SubjectID, r.NewRevision, orNull(r.EntryNo), orNull(r.HBL), orNull(r.BillNo), orNull(r.SealNo),
		orNull(r.AssertionNo), orNull(r.EventNo), orNull(r.FactsVersion))
	return err
}

// Outbox writes SEA events to the Bento Outbox in the command's transaction.
type Outbox struct {
	Writer     eventing.OutboxWriter
	CommandID  string
	RecordedAt time.Time
}

func (o Outbox) EnqueueSEAEvent(ctx context.Context, e sea.Event) error {
	envelope, err := SEAEnvelope(o.CommandID, e, o.RecordedAt)
	if err != nil {
		return err
	}
	return o.Writer.Enqueue(ctx, envelope)
}

// SEAEnvelope maps a SEA domain event to its Bento envelope; a retried command yields the same event ID.
func SEAEnvelope(commandID string, e sea.Event, recordedAt time.Time) (eventing.Envelope, error) {
	id := commandID + "\x00" + e.Type + "\x00" + string(e.TenantID) + "\x00" + e.SubjectID + "\x00" + e.ShipmentID + "\x00" +
		strconv.Itoa(e.EntryNo) + "\x00" + strconv.Itoa(e.BillNo)
	// Movement and seal join the id only when a visit sets them, so a command recorded before keeps its event id.
	if e.MovementID != "" || e.SealNo != 0 || e.AssertionNo != 0 || e.EventNo != 0 {
		id += "\x00" + e.MovementID + "\x00" + strconv.Itoa(e.SealNo)
		if e.AssertionNo != 0 {
			id += "\x00" + strconv.Itoa(e.AssertionNo)
		}
		if e.EventNo != 0 {
			id += "\x00" + strconv.Itoa(e.EventNo)
		}
	}
	digest := sha256.Sum256([]byte(id))
	payload, err := json.Marshal(api.EventPayload{TenantID: string(e.TenantID), SubjectID: e.SubjectID, ConsolidationID: e.ConsolidationID,
		MovementID: e.MovementID, ShipmentID: e.ShipmentID, EntryNo: e.EntryNo, BillNo: e.BillNo, SealNo: e.SealNo, AssertionNo: e.AssertionNo, EventNo: e.EventNo,
		FactsVersion:    e.FactsVersion,
		BookingID:       e.BookingID,
		CapacityVersion: e.CapacityVersion, Reason: e.Reason})
	if err != nil {
		return eventing.Envelope{}, err
	}
	envelope := eventing.Envelope{SpecVersion: eventing.SpecVersionV1, ID: eventing.EventID(hex.EncodeToString(digest[:])), Source: api.Source,
		Type: eventing.EventType(e.Type), Version: 1, Scope: string(e.TenantID), Subject: e.SubjectID, PartitionKey: string(e.TenantID) + ":" + e.SubjectID,
		OccurredAt: e.At.UTC(), RecordedAt: recordedAt.UTC(), ContentType: eventing.JSONContentType, Payload: payload}
	return envelope, envelope.Validate()
}

// Conflict reports whether a commit failed on one of SEA's deferred checks.
func Conflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && (pgErr.ConstraintName == "house_bill_consistent" ||
		pgErr.ConstraintName == "ocean_movement_consistent" || pgErr.ConstraintName == "container_visit_consistent" ||
		pgErr.ConstraintName == "container_seal_consistent" || pgErr.ConstraintName == "ocean_movement_holds_no_container" ||
		pgErr.ConstraintName == "vgm_assertion_consistent" || pgErr.ConstraintName == "container_event_consistent" ||
		pgErr.ConstraintName == "ocean_doc_facts_consistent")
}
