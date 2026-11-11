package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	app "go.idp.xyz/idp-cargo/internal/contexts/book/internal/application"
	book "go.idp.xyz/idp-cargo/internal/contexts/book/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// ForConsumer lists the bookings with an ACTIVE allocation to a consumer.
func (s BookingStore) ForConsumer(ctx context.Context, tenant book.TenantID, kind, id string) ([]book.BookingID, error) {
	return s.ids(ctx, `SELECT a.booking_id FROM cargo_book.capacity_allocation a
      WHERE a.tenant_id=$1 AND a.consumer_kind=$2 AND a.consumer_id=$3 AND a.status='ACTIVE'
        AND a.version_no = (SELECT max(version_no) FROM cargo_book.capacity_allocation x
                            WHERE x.tenant_id=a.tenant_id AND x.booking_id=a.booking_id AND x.allocation_no=a.allocation_no)
      ORDER BY a.booking_id`, string(tenant), kind, id)
}

// ByReference finds the live booking of a carrier holding a reference.
func (s BookingStore) ByReference(ctx context.Context, tenant book.TenantID, carrier string, ref book.Reference) (book.BookingID, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return "", false, err
	}
	var id string
	err = q.QueryRow(ctx, `SELECT booking_id FROM cargo_book.carrier_reference
      WHERE tenant_id=$1 AND carrier_party_id=$2 AND ref_type=$3 AND value=$4 AND live`, string(tenant), carrier, string(ref.Type), ref.Value).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return book.BookingID(id), err == nil, err
}

// Awaiting lists bookings the carrier has yet to answer whose last submission was sent before an instant.
func (s BookingStore) Awaiting(ctx context.Context, tenant book.TenantID, sentBefore time.Time, limit int) ([]book.BookingID, error) {
	return s.ids(ctx, `SELECT b.booking_id FROM cargo_book.carrier_booking b
      JOIN LATERAL (SELECT max(sent_at) AS sent_at FROM cargo_book.booking_submission s WHERE s.tenant_id=b.tenant_id AND s.booking_id=b.booking_id) s ON true
      WHERE b.tenant_id=$1 AND b.status IN ('REQUESTED','ACKNOWLEDGED','CHANGE_REQUESTED','CANCELLATION_REQUESTED') AND s.sent_at < $2
      ORDER BY s.sent_at, b.booking_id LIMIT $3`, string(tenant), sentBefore, limit)
}

func (s BookingStore) ids(ctx context.Context, sql string, args ...any) ([]book.BookingID, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []book.BookingID
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, book.BookingID(id))
	}
	return out, rows.Err()
}

// CutOffsDue lists cut-offs in [from, until) of the current capacity version of bookings in force.
func (s BookingStore) CutOffsDue(ctx context.Context, tenant book.TenantID, from, until time.Time, limit int) ([]app.CutOffDue, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT b.booking_id, b.consumer_id, c.version_no, c.line_no, c.leg_no, coalesce(l.flight,''), coalesce(l.voyage,''),
        c.kind, c.cut_off_at, c.time_zone, c.location_code
      FROM cargo_book.capacity_cut_off c
      JOIN cargo_book.carrier_booking b ON b.tenant_id=c.tenant_id AND b.booking_id=c.booking_id AND b.current_capacity_version=c.version_no
      JOIN cargo_book.capacity_leg l ON l.tenant_id=c.tenant_id AND l.booking_id=c.booking_id AND l.version_no=c.version_no
        AND l.line_no=c.line_no AND l.leg_no=c.leg_no
      WHERE c.tenant_id=$1 AND c.cut_off_at >= $2 AND c.cut_off_at < $3 AND b.status IN ('CONFIRMED','CHANGE_REQUESTED','CANCELLATION_REQUESTED')
      ORDER BY c.cut_off_at, b.booking_id, c.line_no, c.leg_no, c.kind LIMIT $4`, string(tenant), from, until, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.CutOffDue
	for rows.Next() {
		var d app.CutOffDue
		var id, kind, loc string
		if err = rows.Scan(&id, &d.ConsumerID, &d.CapacityVersion, &d.Line, &d.Leg, &d.Flight, &d.Voyage, &kind, &d.CutOff.At, &d.CutOff.Zone, &loc); err != nil {
			return nil, err
		}
		d.BookingID, d.CutOff.Kind, d.CutOff.At = book.BookingID(id), book.CutOffKind(kind), d.CutOff.At.UTC()
		d.CutOff.Location = kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: loc}
		out = append(out, d)
	}
	return out, rows.Err()
}
