// Package postgres persists AIR movements, HAWB series and register, command receipts and Outbox events in the
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
	"go.idp.xyz/idp-cargo/internal/contexts/air/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/air/internal/application"
	air "go.idp.xyz/idp-cargo/internal/contexts/air/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// MovementStore loads and writes air movements.
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
	segmentRow struct {
		LinkNo    int       `json:"link_no"`
		SegmentNo int       `json:"segment_no"`
		Flight    string    `json:"flight"`
		FromCode  string    `json:"from_code"`
		FromIATA  string    `json:"from_iata"`
		ToCode    string    `json:"to_code"`
		ToIATA    string    `json:"to_iata"`
		ETD       time.Time `json:"etd"`
		ETA       time.Time `json:"eta"`
	}
	entryRow struct {
		EntryNo         int       `json:"entry_no"`
		ShipmentID      string    `json:"shipment_id"`
		HAWB            string    `json:"hawb"`
		FULRevision     int64     `json:"ful_revision"`
		PlanVersion     int       `json:"plan_version"`
		WindowFrom      time.Time `json:"window_from"`
		WindowUntil     time.Time `json:"window_until"`
		Pieces          int64     `json:"pieces"`
		GrossWeightUnit string    `json:"gross_weight_unit"`
		GrossWeight     int64     `json:"gross_weight"`
		VolumeUnit      string    `json:"volume_unit"`
		Volume          int64     `json:"volume"`
		ChargeableGrams int64     `json:"chargeable_grams"`
		ChargeableRule  string    `json:"chargeable_rule"`
		Basis           string    `json:"basis"`
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
		PartNo            int    `json:"part_no"`
		ObligationID      string `json:"obligation_id"`
		ObligationVersion int64  `json:"obligation_version"`
		OrderID           string `json:"order_id"`
		Packs             int64  `json:"packs"`
		PackageType       string `json:"package_type"`
		GrossWeightUnit   string `json:"gross_weight_unit"`
		GrossWeight       int64  `json:"gross_weight"`
		VolumeUnit        string `json:"volume_unit"`
		Volume            int64  `json:"volume"`
	}
)

func quantity(unit string, amount int64) kernel.Quantity {
	return kernel.Quantity{Unit: kernel.UnitCode(unit), Amount: amount}
}

func airport(code, iata string) air.Airport {
	return air.Airport{Code: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}, IATA: iata}
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
	return `(SELECT coalesce(json_agg(x ORDER BY ` + order + `), '[]') FROM cargo_air.` + table + ` x WHERE x.tenant_id=m.tenant_id AND x.movement_id=m.movement_id)`
}

func (s MovementStore) Load(ctx context.Context, tenant air.TenantID, id air.MovementID) (*air.Movement, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_air.air_movement WHERE tenant_id=$1 AND movement_id=$2 FOR NO KEY UPDATE`, string(tenant), string(id)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: movement %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	return readMovement(ctx, q, tenant, id)
}

func (s MovementStore) Read(ctx context.Context, tenant air.TenantID, id air.MovementID) (*air.Movement, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readMovement(ctx, q, tenant, id)
}

func readMovement(ctx context.Context, q queryRower, tenant air.TenantID, id air.MovementID) (*air.Movement, int64, error) {
	st := air.MovementState{TenantID: tenant, MovementID: id}
	var state, mawbStatus, originCode, originIATA, destCode, destIATA string
	var cancelReason, cancelledBy *string
	var cancelledAt *time.Time
	var revision int64
	var links, segments, entries, parts []byte
	err := q.QueryRow(ctx, `SELECT m.consolidation_id, m.operating_legal_entity_id, m.airline_party_id, m.airline_party_revision, m.airline_designator,
        m.awb_prefix, m.origin_code, m.origin_iata, m.destination_code, m.destination_iata, m.location_edition, m.state, m.mawb_number, m.mawb_status,
        m.mawb_booking_id, m.mawb_capacity_version, m.cancel_reason, m.cancelled_by, m.cancelled_at, m.revision, `+
		child("booking_link", "x.link_no")+`, `+child("flight_segment", "x.link_no, x.segment_no")+`, `+child("house_waybill", "x.entry_no")+`, `+
		child("house_waybill_part", "x.entry_no, x.part_no")+`
      FROM cargo_air.air_movement m WHERE m.tenant_id=$1 AND m.movement_id=$2`, string(tenant), string(id)).Scan(
		&st.ConsolidationID, &st.LegalEntityID, &st.Airline.PartyID, &st.Airline.PartyRevision, &st.Airline.Designator, &st.Airline.AWBPrefix,
		&originCode, &originIATA, &destCode, &destIATA, &st.Edition, &state, &st.MAWB.Number, &mawbStatus, &st.MAWB.BookingID, &st.MAWB.CapacityVersion,
		&cancelReason, &cancelledBy, &cancelledAt, &revision, &links, &segments, &entries, &parts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: movement %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	var lr []linkRow
	var sr []segmentRow
	var er []entryRow
	var pr []partRow
	if err = errors.Join(json.Unmarshal(links, &lr), json.Unmarshal(segments, &sr), json.Unmarshal(entries, &er), json.Unmarshal(parts, &pr)); err != nil {
		return nil, 0, err
	}
	st.Origin, st.Destination = airport(originCode, originIATA), airport(destCode, destIATA)
	st.State, st.MAWB.Voided = air.State(state), mawbStatus == "VOIDED"
	if cancelledAt != nil {
		st.CancelReason, st.CancelledBy, st.CancelledAt = deref(cancelReason), deref(cancelledBy), cancelledAt.UTC()
	}
	for _, l := range lr {
		link := air.Link{No: l.LinkNo, BookingID: l.BookingID, BookingRevision: l.BookingRevision, CapacityVersion: l.CapacityVersion, By: l.LinkedBy,
			At: utc(l.LinkedAt), EnteredAt: utc(l.EnteredAt)}
		for _, g := range sr {
			if g.LinkNo == l.LinkNo {
				link.Segments = append(link.Segments, air.Segment{Flight: g.Flight, From: airport(g.FromCode, g.FromIATA), To: airport(g.ToCode, g.ToIATA),
					ETD: utc(g.ETD), ETA: utc(g.ETA)})
			}
		}
		st.Links = append(st.Links, link)
	}
	for _, e := range er {
		entry := air.Entry{No: e.EntryNo, ShipmentID: e.ShipmentID, HAWB: e.HAWB, FULRevision: e.FULRevision, PlanVersion: e.PlanVersion,
			Window: kernel.ValidityInterval{From: utc(e.WindowFrom), Until: utc(e.WindowUntil)},
			Facts: air.Facts{Pieces: e.Pieces, GrossWeight: quantity(e.GrossWeightUnit, e.GrossWeight), Volume: quantity(e.VolumeUnit, e.Volume),
				ChargeableGrams: e.ChargeableGrams, Rule: e.ChargeableRule, Basis: e.Basis},
			Start: e.StartKind, Predecessor: e.PredecessorNo, By: e.AssociatedBy, At: utc(e.AssociatedAt), EnteredAt: utc(e.EnteredAt), End: e.EndKind,
			EndedBy: e.EndedBy, EndedAt: utc(e.EndedAt), EndEnteredAt: utc(e.EndEnteredAt), Successor: e.SuccessorNo}
		for _, p := range pr {
			if p.EntryNo == e.EntryNo {
				entry.Facts.Parts = append(entry.Facts.Parts, air.Part{ObligationID: p.ObligationID, ObligationVersion: p.ObligationVersion, OrderID: p.OrderID,
					Packs: p.Packs, PackageType: p.PackageType, GrossWeight: quantity(p.GrossWeightUnit, p.GrossWeight), Volume: quantity(p.VolumeUnit, p.Volume)})
			}
		}
		st.Entries = append(st.Entries, entry)
	}
	m, err := air.RestoreMovement(st)
	return m, revision, err
}

// Insert writes a new movement at revision 1 with its first link.
func (s MovementStore) Insert(ctx context.Context, m *air.Movement) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := m.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_air.air_movement (tenant_id,movement_id,consolidation_id,operating_legal_entity_id,airline_party_id,
        airline_party_revision,airline_designator,awb_prefix,origin_code,origin_iata,destination_code,destination_iata,location_edition,state,mawb_number,
        mawb_status,mawb_booking_id,mawb_capacity_version) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'ASSIGNED',$16,$17) RETURNING revision`,
		string(st.TenantID), string(st.MovementID), st.ConsolidationID, st.LegalEntityID, st.Airline.PartyID, st.Airline.PartyRevision, st.Airline.Designator,
		st.Airline.AWBPrefix, st.Origin.Code.Code, st.Origin.IATA, st.Destination.Code.Code, st.Destination.IATA, st.Edition, string(st.State),
		st.MAWB.Number, st.MAWB.BookingID, st.MAWB.CapacityVersion).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "air_movement_pkey":
			return 0, fmt.Errorf("%w: movement %s", bentorepo.ErrAlreadyExists, st.MovementID)
		case "air_movement_one_open":
			return 0, fmt.Errorf("%w: consolidation %s has an open air movement", bentorepo.ErrConflict, st.ConsolidationID)
		case "air_movement_mawb_assigned":
			return 0, fmt.Errorf("%w: %s", app.ErrMAWBInUse, st.MAWB.Number)
		}
	}
	if err != nil {
		return 0, err
	}
	return revision, s.writeAdded(ctx, q, m)
}

// Save writes the state and what the movement added or ended since it was loaded, if the stored revision is
// still the expected one.
func (s MovementStore) Save(ctx context.Context, m *air.Movement, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := m.State()
	status := "ASSIGNED"
	if st.MAWB.Voided {
		status = "VOIDED"
	}
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_air.air_movement SET state=$3,mawb_status=$4,cancel_reason=$5,cancelled_by=$6,cancelled_at=$7,revision=revision+1,
        updated_at=now() WHERE tenant_id=$1 AND movement_id=$2 AND revision=$8 RETURNING revision`, string(st.TenantID), string(st.MovementID),
		string(st.State), status, orNull(st.CancelReason), orNull(st.CancelledBy), orNull(st.CancelledAt), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: movement %s", bentorepo.ErrConflict, st.MovementID)
	}
	if err != nil {
		return 0, err
	}
	return revision, s.writeAdded(ctx, q, m)
}

func (s MovementStore) writeAdded(ctx context.Context, q bentopg.Executor, m *air.Movement) error {
	st := m.State()
	exec := func(sql string, args ...any) error {
		_, err := q.Exec(ctx, sql, append([]any{string(st.TenantID), string(st.MovementID)}, args...)...)
		return err
	}
	links, entries, ended := m.Added()
	for no, e := range ended {
		tag, err := q.Exec(ctx, `UPDATE cargo_air.house_waybill SET end_kind=$4,ended_by=$5,ended_at=$6,end_entered_at=$7,successor_no=$8
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
		if err := exec(`INSERT INTO cargo_air.booking_link (tenant_id,movement_id,link_no,booking_id,booking_revision,capacity_version,linked_by,linked_at,
            entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, l.No, l.BookingID, l.BookingRevision, l.CapacityVersion, l.By, l.At, l.EnteredAt); err != nil {
			return err
		}
		for i, g := range l.Segments {
			if err := exec(`INSERT INTO cargo_air.flight_segment (tenant_id,movement_id,link_no,segment_no,flight,from_code,from_iata,to_code,to_iata,etd,eta)
                VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, l.No, i+1, g.Flight, g.From.Code.Code, g.From.IATA, g.To.Code.Code, g.To.IATA, g.ETD,
				orNull(g.ETA)); err != nil {
				return err
			}
		}
	}
	for _, e := range entries {
		f := e.Facts
		err := exec(`INSERT INTO cargo_air.house_waybill (tenant_id,movement_id,entry_no,shipment_id,hawb,ful_revision,plan_version,window_from,window_until,
            pieces,gross_weight_unit,gross_weight,volume_unit,volume,chargeable_grams,chargeable_rule,basis,start_kind,predecessor_no,associated_by,
            associated_at,entered_at,end_kind,ended_by,ended_at,end_entered_at,successor_no)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
			e.No, e.ShipmentID, e.HAWB, e.FULRevision, e.PlanVersion, e.Window.From, orNull(e.Window.Until), f.Pieces, string(f.GrossWeight.Unit),
			f.GrossWeight.Amount, orNull(string(f.Volume.Unit)), orNull(f.Volume.Amount), f.ChargeableGrams, f.Rule, f.Basis, e.Start, orNull(e.Predecessor),
			e.By, e.At, e.EnteredAt, orNull(e.End), orNull(e.EndedBy), orNull(e.EndedAt), orNull(e.EndEnteredAt), orNull(e.Successor))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && (pgErr.ConstraintName == "house_waybill_one_live" || pgErr.ConstraintName == "house_waybill_live_hawb") {
			return fmt.Errorf("%w: %s is associated with another movement", air.ErrAlreadyAssociated, e.ShipmentID)
		}
		if err != nil {
			return err
		}
		for i, p := range f.Parts {
			if err := exec(`INSERT INTO cargo_air.house_waybill_part (tenant_id,movement_id,entry_no,part_no,obligation_id,obligation_version,order_id,packs,
                package_type,gross_weight_unit,gross_weight,volume_unit,volume) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, e.No, i+1,
				p.ObligationID, p.ObligationVersion, p.OrderID, p.Packs, p.PackageType, string(p.GrossWeight.Unit), p.GrossWeight.Amount,
				orNull(string(p.Volume.Unit)), orNull(p.Volume.Amount)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ByMAWB finds the movement a MAWB number is assigned to.
func (s MovementStore) ByMAWB(ctx context.Context, tenant air.TenantID, mawb string) (air.MovementID, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return "", false, err
	}
	var id string
	err = q.QueryRow(ctx, `SELECT movement_id FROM cargo_air.air_movement WHERE tenant_id=$1 AND mawb_number=$2 AND mawb_status='ASSIGNED'`,
		string(tenant), mawb).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return air.MovementID(id), err == nil, err
}

// SeriesStore loads and writes HAWB series.
type SeriesStore struct{ DB *bentopg.DB }

var _ app.SeriesStore = SeriesStore{}

func (s SeriesStore) Load(ctx context.Context, tenant air.TenantID, le string) (*air.Series, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readSeries(ctx, q, tenant, le, " FOR UPDATE")
}

func (s SeriesStore) Read(ctx context.Context, tenant air.TenantID, le string) (*air.Series, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readSeries(ctx, q, tenant, le, "")
}

func readSeries(ctx context.Context, q queryRower, tenant air.TenantID, le, lock string) (*air.Series, int64, error) {
	st := air.SeriesState{TenantID: tenant, LegalEntityID: le}
	var revision int64
	err := q.QueryRow(ctx, `SELECT prefix,width,next_seq,registered_by,registered_at,entered_at,revision FROM cargo_air.hawb_series
      WHERE tenant_id=$1 AND legal_entity_id=$2`+lock, string(tenant), le).Scan(&st.Prefix, &st.Width, &st.Next, &st.By, &st.At, &st.EnteredAt, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: %s", app.ErrNoSeries, le)
	}
	if err != nil {
		return nil, 0, err
	}
	st.At, st.EnteredAt = st.At.UTC(), st.EnteredAt.UTC()
	series, err := air.RestoreSeries(st)
	return series, revision, err
}

func (s SeriesStore) Insert(ctx context.Context, series *air.Series) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := series.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_air.hawb_series (tenant_id,legal_entity_id,prefix,width,next_seq,registered_by,registered_at,entered_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING revision`, string(st.TenantID), st.LegalEntityID, st.Prefix, st.Width, st.Next, st.By, st.At,
		st.EnteredAt).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "hawb_series_pkey" {
		return 0, fmt.Errorf("%w: %s has a HAWB series", bentorepo.ErrAlreadyExists, st.LegalEntityID)
	}
	return revision, err
}

func (s SeriesStore) Save(ctx context.Context, series *air.Series, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := series.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_air.hawb_series SET next_seq=$3,revision=revision+1 WHERE tenant_id=$1 AND legal_entity_id=$2 AND revision=$4
      RETURNING revision`, string(st.TenantID), st.LegalEntityID, st.Next, expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: series of %s", bentorepo.ErrConflict, st.LegalEntityID)
	}
	return revision, err
}

// Register is the HAWB register.
type Register struct{ DB *bentopg.DB }

var _ app.HAWBRegister = Register{}

func (r Register) OfShipment(ctx context.Context, tenant air.TenantID, le, shipment string) (string, bool, error) {
	q, err := r.DB.ReadExecutor(ctx)
	if err != nil {
		return "", false, err
	}
	var hawb string
	err = q.QueryRow(ctx, `SELECT hawb FROM cargo_air.hawb_number WHERE tenant_id=$1 AND legal_entity_id=$2 AND shipment_id=$3`, string(tenant), le, shipment).Scan(&hawb)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return hawb, err == nil, err
}

func (r Register) Find(ctx context.Context, tenant air.TenantID, le, hawb string) (app.Registration, bool, error) {
	q, err := r.DB.ReadExecutor(ctx)
	if err != nil {
		return app.Registration{}, false, err
	}
	reg := app.Registration{HAWB: hawb}
	var movement *string
	var entry *int
	err = q.QueryRow(ctx, `SELECT n.shipment_id, e.movement_id, e.entry_no FROM cargo_air.hawb_number n
      LEFT JOIN cargo_air.house_waybill e ON e.tenant_id=n.tenant_id AND e.shipment_id=n.shipment_id AND e.hawb=n.hawb AND e.ended_at IS NULL
      WHERE n.tenant_id=$1 AND n.legal_entity_id=$2 AND n.hawb=$3`, string(tenant), le, hawb).Scan(&reg.ShipmentID, &movement, &entry)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Registration{}, false, nil
	}
	if err != nil {
		return app.Registration{}, false, err
	}
	reg.MovementID = air.MovementID(deref(movement))
	if entry != nil {
		reg.EntryNo = *entry
	}
	return reg, true, nil
}

func (r Register) Claim(ctx context.Context, tenant air.TenantID, le, hawb, shipment string, drawn bool, at time.Time) error {
	q, err := r.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_air.hawb_number (tenant_id,legal_entity_id,hawb,shipment_id,drawn,claimed_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		string(tenant), le, hawb, shipment, drawn, at)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "hawb_number_pkey" {
			return fmt.Errorf("%w: %s", app.ErrHAWBInUse, hawb)
		}
		return fmt.Errorf("%w: %s claimed a HAWB meanwhile", bentorepo.ErrConflict, shipment)
	}
	return err
}

// CommandReceipts stores the result of every committed AIR command for replays.
type CommandReceipts struct{ DB *bentopg.DB }

var _ app.Receipts = CommandReceipts{}

func (s CommandReceipts) Find(ctx context.Context, tenant air.TenantID, id string) (app.Receipt, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return app.Receipt{}, false, err
	}
	r := app.Receipt{CommandID: id}
	var entry *int
	var hawb *string
	err = q.QueryRow(ctx, `SELECT request_sha256,result_id,result_revision,result_entry_no,result_hawb FROM cargo_air.command_receipt
      WHERE tenant_id=$1 AND command_id=$2`, string(tenant), id).Scan(&r.RequestHash, &r.SubjectID, &r.NewRevision, &entry, &hawb)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	if entry != nil {
		r.EntryNo = *entry
	}
	r.HAWB = deref(hawb)
	return r, true, nil
}

func (s CommandReceipts) Save(ctx context.Context, tenant air.TenantID, r app.Receipt, kind string) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_air.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision,result_entry_no,
      result_hawb) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, string(tenant), r.CommandID, r.RequestHash, kind, r.SubjectID, r.NewRevision, orNull(r.EntryNo), orNull(r.HAWB))
	return err
}

// Outbox writes AIR events to the Bento Outbox in the command's transaction.
type Outbox struct {
	Writer     eventing.OutboxWriter
	CommandID  string
	RecordedAt time.Time
}

func (o Outbox) EnqueueAIREvent(ctx context.Context, e air.Event) error {
	envelope, err := AIREnvelope(o.CommandID, e, o.RecordedAt)
	if err != nil {
		return err
	}
	return o.Writer.Enqueue(ctx, envelope)
}

// AIREnvelope maps an AIR domain event to its Bento envelope; a retried command yields the same event ID.
func AIREnvelope(commandID string, e air.Event, recordedAt time.Time) (eventing.Envelope, error) {
	digest := sha256.Sum256([]byte(commandID + "\x00" + e.Type + "\x00" + string(e.TenantID) + "\x00" + e.SubjectID + "\x00" + e.ShipmentID + "\x00" +
		strconv.Itoa(e.EntryNo)))
	payload, err := json.Marshal(api.EventPayload{TenantID: string(e.TenantID), SubjectID: e.SubjectID, ConsolidationID: e.ConsolidationID,
		ShipmentID: e.ShipmentID, EntryNo: e.EntryNo, BookingID: e.BookingID, CapacityVersion: e.CapacityVersion, Reason: e.Reason})
	if err != nil {
		return eventing.Envelope{}, err
	}
	envelope := eventing.Envelope{SpecVersion: eventing.SpecVersionV1, ID: eventing.EventID(hex.EncodeToString(digest[:])), Source: api.Source,
		Type: eventing.EventType(e.Type), Version: 1, Scope: string(e.TenantID), Subject: e.SubjectID, PartitionKey: string(e.TenantID) + ":" + e.SubjectID,
		OccurredAt: e.At.UTC(), RecordedAt: recordedAt.UTC(), ContentType: eventing.JSONContentType, Payload: payload}
	return envelope, envelope.Validate()
}

// Conflict reports whether a commit failed on one of AIR's deferred checks.
func Conflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && (pgErr.ConstraintName == "house_waybill_consistent" || pgErr.ConstraintName == "air_movement_consistent")
}
