// Package postgres persists COM orders, command receipts and Outbox events in the tenant's RLS scope.
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
	app "go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// OrderStore loads and writes orders for commands. Load locks the order row before reading, so commands
// on one order run one after another and see everything committed before them.
type OrderStore struct{ DB *bentopg.DB }

func (s OrderStore) Load(ctx context.Context, tenant com.TenantID, id com.OrderID) (*com.CustomerOrder, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_com.customer_order WHERE tenant_id=$1 AND order_id=$2 FOR NO KEY UPDATE`,
		string(tenant), string(id)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	return readOrder(ctx, q, tenant, id)
}

// OrderOf finds the order that holds an obligation. Obligation rows never change, so the answer holds for
// good.
func (s OrderStore) OrderOf(ctx context.Context, tenant com.TenantID, id com.ObligationID) (com.OrderID, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return "", err
	}
	var order string
	err = q.QueryRow(ctx, `SELECT order_id FROM cargo_com.fulfillment_obligation WHERE tenant_id=$1 AND obligation_id=$2 LIMIT 1`,
		string(tenant), string(id)).Scan(&order)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: obligation %s", bentorepo.ErrNotFound, id)
	}
	return com.OrderID(order), err
}

// OrderReader reads orders for queries without locking.
type OrderReader struct{ DB *bentopg.DB }

// ReadObligation reads the order holding one version of an obligation. Obligation rows never change, so
// the order found by the first statement is the one that holds it.
func (r OrderReader) ReadObligation(ctx context.Context, tenant com.TenantID, id com.ObligationID, version int64) (*com.CustomerOrder, error) {
	q, err := r.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	var order string
	err = q.QueryRow(ctx, `SELECT order_id FROM cargo_com.fulfillment_obligation
      WHERE tenant_id=$1 AND obligation_id=$2 AND obligation_version=$3`, string(tenant), string(id), version).Scan(&order)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	o, _, err := readOrder(ctx, q, tenant, com.OrderID(order))
	return o, err
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Rows of the child tables as json_agg renders them; NULL columns read as zero values.
type lineRow struct {
	LineNo          int    `json:"line_no"`
	Description     string `json:"description"`
	PackageType     string `json:"package_type"`
	Packs           int64  `json:"packs"`
	GrossWeightUnit string `json:"gross_weight_unit"`
	GrossWeight     int64  `json:"gross_weight"`
	VolumeUnit      string `json:"volume_unit"`
	Volume          int64  `json:"volume"`
}

type obligationRow struct {
	ObligationID      string    `json:"obligation_id"`
	ObligationVersion int64     `json:"obligation_version"`
	LineNo            int       `json:"line_no"`
	Packs             int64     `json:"packs"`
	GrossWeightUnit   string    `json:"gross_weight_unit"`
	GrossWeight       int64     `json:"gross_weight"`
	VolumeUnit        string    `json:"volume_unit"`
	Volume            int64     `json:"volume"`
	Status            string    `json:"status"`
	AllocatedBy       string    `json:"allocated_by"`
	AllocatedAt       time.Time `json:"allocated_at"`
	RevokedBy         string    `json:"revoked_by"`
	RevokedAt         time.Time `json:"revoked_at"`
}

type reservationRow struct {
	ReservationNo     int       `json:"reservation_no"`
	ObligationID      string    `json:"obligation_id"`
	ObligationVersion int64     `json:"obligation_version"`
	ShipmentID        string    `json:"shipment_id"`
	ReservedBy        string    `json:"reserved_by"`
	ReservedAt        time.Time `json:"reserved_at"`
	ReleasedBy        string    `json:"released_by"`
	ReleasedAt        time.Time `json:"released_at"`
}

func measure(packs int64, weightUnit string, weight int64, volumeUnit string, volume int64) com.Measure {
	return com.Measure{Packs: packs, GrossWeight: kernel.Quantity{Unit: kernel.UnitCode(weightUnit), Amount: weight},
		Volume: kernel.Quantity{Unit: kernel.UnitCode(volumeUnit), Amount: volume}}
}

// volume returns the volume columns of a measure, NULL when it declares none.
func volume(m com.Measure) (unit, amount any) {
	if !m.DeclaresVolume() {
		return nil, nil
	}
	return string(m.Volume.Unit), m.Volume.Amount
}

// utc keeps zero times zero.
func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

// readOrder reads an order with its lines, obligation versions and reservations in one statement, so all
// come from one snapshot even without a lock: under READ COMMITTED separate statements could see a change
// that committed in between.
func readOrder(ctx context.Context, q queryRower, tenant com.TenantID, id com.OrderID) (*com.CustomerOrder, int64, error) {
	s := com.OrderState{OrderDraft: com.OrderDraft{TenantID: tenant, OrderID: id}}
	var entity, currency, status string
	var roleUntil, cancelledAt *time.Time
	var cancelledBy *string
	var revision int64
	var lineJSON, obligationJSON, reservationJSON []byte
	err := q.QueryRow(ctx, `SELECT o.legal_entity_id,o.customer_party_id,o.customer_party_revision,o.customer_legal_name,
        o.customer_country_code,o.customer_role_valid_from,o.customer_role_valid_until,o.customer_role_evidence_ref,
        o.quote_id,o.quote_revision,o.quote_customer_revision,o.quote_currency,o.quote_total_minor,o.quote_valid_from,
        o.quote_valid_until,o.quote_accepted_at,o.quote_acceptance_evidence_ref,o.accepted_terms_sha256,
        o.recorded_by,o.recorded_at,o.status,o.revision,o.cancelled_by,o.cancelled_at,
        (SELECT coalesce(json_agg(l ORDER BY l.line_no), '[]') FROM cargo_com.order_line l
          WHERE l.tenant_id=o.tenant_id AND l.order_id=o.order_id),
        (SELECT coalesce(json_agg(f ORDER BY f.allocated_at, f.obligation_id, f.obligation_version), '[]')
          FROM cargo_com.fulfillment_obligation f WHERE f.tenant_id=o.tenant_id AND f.order_id=o.order_id),
        (SELECT coalesce(json_agg(r ORDER BY r.reservation_no), '[]') FROM cargo_com.obligation_reservation r
          WHERE r.tenant_id=o.tenant_id AND r.order_id=o.order_id)
      FROM cargo_com.customer_order o WHERE o.tenant_id=$1 AND o.order_id=$2`, string(tenant), string(id)).Scan(
		&entity, &s.Customer.PartyID, &s.Customer.PartyRevision, &s.Customer.LegalName,
		&s.Customer.CountryCode, &s.Customer.RoleValidity.From, &roleUntil, &s.Customer.RoleEvidenceRef,
		&s.Quote.QuoteID, &s.Quote.QuoteRevision, &s.Quote.CustomerRevision, &currency, &s.Quote.Total.Minor, &s.Quote.Validity.From,
		&s.Quote.Validity.Until, &s.Quote.AcceptedAt, &s.Quote.EvidenceRef, &s.Quote.TermsHash,
		&s.RecordedBy, &s.RecordedAt, &status, &revision, &cancelledBy, &cancelledAt,
		&lineJSON, &obligationJSON, &reservationJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	var lines []lineRow
	var obligations []obligationRow
	var reservations []reservationRow
	if err = errors.Join(json.Unmarshal(lineJSON, &lines), json.Unmarshal(obligationJSON, &obligations),
		json.Unmarshal(reservationJSON, &reservations)); err != nil {
		return nil, 0, err
	}
	s.Quote.LegalEntityID, s.Quote.CustomerPartyID, s.Status = com.LegalEntityID(entity), s.Customer.PartyID, com.OrderStatus(status)
	s.Quote.Total.Currency = kernel.CurrencyCode(currency)
	if roleUntil != nil {
		s.Customer.RoleValidity.Until = *roleUntil
	}
	if cancelledAt != nil && cancelledBy != nil {
		s.CancelledBy, s.CancelledAt = *cancelledBy, cancelledAt.UTC()
	}
	for _, l := range lines {
		s.Lines = append(s.Lines, com.OrderLine{LineNo: l.LineNo, Description: l.Description, PackageType: com.PackageType(l.PackageType),
			Measure: measure(l.Packs, l.GrossWeightUnit, l.GrossWeight, l.VolumeUnit, l.Volume)})
	}
	for _, f := range obligations {
		s.Obligations = append(s.Obligations, com.Obligation{ObligationID: com.ObligationID(f.ObligationID), Version: f.ObligationVersion,
			LineNo: f.LineNo, Measure: measure(f.Packs, f.GrossWeightUnit, f.GrossWeight, f.VolumeUnit, f.Volume),
			Status: com.ObligationStatus(f.Status), AllocatedBy: f.AllocatedBy, AllocatedAt: utc(f.AllocatedAt),
			RevokedBy: f.RevokedBy, RevokedAt: utc(f.RevokedAt)})
	}
	for _, r := range reservations {
		s.Reservations = append(s.Reservations, com.Reservation{No: r.ReservationNo, ObligationID: com.ObligationID(r.ObligationID),
			ObligationVersion: r.ObligationVersion, ShipmentID: r.ShipmentID, ReservedBy: r.ReservedBy, ReservedAt: utc(r.ReservedAt),
			ReleasedBy: r.ReleasedBy, ReleasedAt: utc(r.ReleasedAt)})
	}
	order, err := com.RestoreOrder(s)
	return order, revision, err
}

// Insert writes a new order at revision 1 with its lines.
func (s OrderStore) Insert(ctx context.Context, order *com.CustomerOrder) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := order.State()
	c, qt := st.Customer, st.Quote
	var roleUntil any
	if !c.RoleValidity.Until.IsZero() {
		roleUntil = c.RoleValidity.Until
	}
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_com.customer_order (tenant_id,order_id,legal_entity_id,customer_party_id,
        customer_party_revision,customer_legal_name,customer_country_code,customer_role_valid_from,customer_role_valid_until,
        customer_role_evidence_ref,quote_id,quote_revision,quote_customer_revision,quote_currency,quote_total_minor,
        quote_valid_from,quote_valid_until,quote_accepted_at,quote_acceptance_evidence_ref,accepted_terms_sha256,
        recorded_by,recorded_at,status)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23) RETURNING revision`,
		string(st.TenantID), string(st.OrderID), string(qt.LegalEntityID), c.PartyID,
		c.PartyRevision, c.LegalName, c.CountryCode, c.RoleValidity.From, roleUntil,
		c.RoleEvidenceRef, qt.QuoteID, qt.QuoteRevision, qt.CustomerRevision, string(qt.Total.Currency), qt.Total.Minor,
		qt.Validity.From, qt.Validity.Until, qt.AcceptedAt, qt.EvidenceRef, qt.TermsHash,
		st.RecordedBy, st.RecordedAt, string(st.Status)).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "customer_order_pkey":
			return 0, bentorepo.ErrAlreadyExists
		case "customer_order_one_per_quote":
			return 0, fmt.Errorf("%w: quote %s", app.ErrQuoteAlreadyOrdered, qt.QuoteID)
		}
	}
	if err != nil {
		return 0, err
	}
	for _, l := range st.Lines {
		volumeUnit, volumeAmount := volume(l.Measure)
		if _, err = q.Exec(ctx, `INSERT INTO cargo_com.order_line (tenant_id,order_id,line_no,description,package_type,packs,
            gross_weight_unit,gross_weight,volume_unit,volume) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			string(st.TenantID), string(st.OrderID), l.LineNo, l.Description, string(l.PackageType), l.Measure.Packs,
			string(l.Measure.GrossWeight.Unit), l.Measure.GrossWeight.Amount, volumeUnit, volumeAmount); err != nil {
			return 0, err
		}
	}
	return revision, nil
}

// Save writes the order's status and cancellation, the obligation versions and reservations it added and
// the reservations it released, if the stored revision is still the expected one.
func (s OrderStore) Save(ctx context.Context, order *com.CustomerOrder, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := order.State()
	var cancelledBy, cancelledAt any
	if !st.CancelledAt.IsZero() {
		cancelledBy, cancelledAt = st.CancelledBy, st.CancelledAt
	}
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_com.customer_order SET status=$3,cancelled_by=$5,cancelled_at=$6,revision=revision+1,updated_at=now()
      WHERE tenant_id=$1 AND order_id=$2 AND revision=$4 RETURNING revision`,
		string(st.TenantID), string(st.OrderID), string(st.Status), expected, cancelledBy, cancelledAt).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, bentorepo.ErrConflict
	}
	if err != nil {
		return 0, err
	}
	for _, ob := range order.Added() {
		volumeUnit, volumeAmount := volume(ob.Measure)
		var revokedBy, revokedAt any
		if ob.Status == com.ObligationRevoked {
			revokedBy, revokedAt = ob.RevokedBy, ob.RevokedAt
		}
		_, err = q.Exec(ctx, `INSERT INTO cargo_com.fulfillment_obligation (tenant_id,obligation_id,obligation_version,order_id,line_no,
            packs,gross_weight_unit,gross_weight,volume_unit,volume,status,allocated_by,allocated_at,revoked_by,revoked_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			string(st.TenantID), string(ob.ObligationID), ob.Version, string(st.OrderID), ob.LineNo,
			ob.Measure.Packs, string(ob.Measure.GrossWeight.Unit), ob.Measure.GrossWeight.Amount, volumeUnit, volumeAmount,
			string(ob.Status), ob.AllocatedBy, ob.AllocatedAt, revokedBy, revokedAt)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "fulfillment_obligation_pkey" {
			return 0, fmt.Errorf("%w: obligation %s", bentorepo.ErrAlreadyExists, ob.ObligationID)
		}
		if err != nil {
			return 0, err
		}
	}
	for _, r := range order.AddedReservations() {
		var releasedBy, releasedAt any
		if !r.Held() {
			releasedBy, releasedAt = r.ReleasedBy, r.ReleasedAt
		}
		if _, err = q.Exec(ctx, `INSERT INTO cargo_com.obligation_reservation (tenant_id,order_id,reservation_no,obligation_id,
            obligation_version,shipment_id,reserved_by,reserved_at,released_by,released_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			string(st.TenantID), string(st.OrderID), r.No, string(r.ObligationID), r.ObligationVersion, r.ShipmentID,
			r.ReservedBy, r.ReservedAt, releasedBy, releasedAt); err != nil {
			return 0, err
		}
	}
	for _, r := range order.ReleasedReservations() {
		tag, err := q.Exec(ctx, `UPDATE cargo_com.obligation_reservation SET released_by=$4,released_at=$5
          WHERE tenant_id=$1 AND order_id=$2 AND reservation_no=$3 AND released_at IS NULL`,
			string(st.TenantID), string(st.OrderID), r.No, r.ReleasedBy, r.ReleasedAt)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() != 1 {
			return 0, fmt.Errorf("%w: reservation %d of order %s", bentorepo.ErrConflict, r.No, st.OrderID)
		}
	}
	return revision, nil
}

// CommandReceipts stores the result of every committed COM command for replays.
type CommandReceipts struct{ DB *bentopg.DB }

func (s CommandReceipts) Find(ctx context.Context, tenant com.TenantID, id string) (app.Receipt, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return app.Receipt{}, false, err
	}
	r := app.Receipt{CommandID: id}
	var order string
	err = q.QueryRow(ctx, `SELECT request_sha256,result_id,result_revision FROM cargo_com.command_receipt
      WHERE tenant_id=$1 AND command_id=$2`, string(tenant), id).Scan(&r.RequestHash, &order, &r.NewRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	r.OrderID = com.OrderID(order)
	return r, true, nil
}

func (s CommandReceipts) Save(ctx context.Context, tenant com.TenantID, r app.Receipt) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_com.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision)
      VALUES ($1,$2,$3,'CustomerOrder',$4,$5)`, string(tenant), r.CommandID, r.RequestHash, string(r.OrderID), r.NewRevision)
	return err
}
