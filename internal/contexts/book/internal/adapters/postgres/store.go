// Package postgres persists BOOK bookings, command receipts and Outbox events in the tenant's RLS scope.
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
	app "go.idp.xyz/idp-cargo/internal/contexts/book/internal/application"
	book "go.idp.xyz/idp-cargo/internal/contexts/book/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// BookingStore loads and writes bookings. Load locks the booking row, so commands on one booking run one
// after another; Read takes no lock.
type BookingStore struct{ DB *bentopg.DB }

var (
	_ app.BookingStore   = BookingStore{}
	_ app.BookingQueries = BookingStore{}
)

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s BookingStore) Load(ctx context.Context, tenant book.TenantID, id book.BookingID) (*book.Booking, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_book.carrier_booking WHERE tenant_id=$1 AND booking_id=$2 FOR NO KEY UPDATE`,
		string(tenant), string(id)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: booking %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	return readBooking(ctx, q, tenant, id)
}

func (s BookingStore) Read(ctx context.Context, tenant book.TenantID, id book.BookingID) (*book.Booking, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readBooking(ctx, q, tenant, id)
}

// Rows as json_agg renders them; NULL columns read as zero values.
type (
	requestRow struct {
		VersionNo          int       `json:"version_no"`
		ForwarderReference string    `json:"forwarder_reference"`
		Pieces             int64     `json:"pieces"`
		GrossWeightUnit    string    `json:"gross_weight_unit"`
		GrossWeight        int64     `json:"gross_weight"`
		VolumeUnit         string    `json:"volume_unit"`
		Volume             int64     `json:"volume"`
		HandlingCodes      []string  `json:"handling_codes"`
		LocationEdition    string    `json:"location_edition"`
		Reason             string    `json:"reason"`
		WrittenBy          string    `json:"written_by"`
		WrittenAt          time.Time `json:"written_at"`
		EnteredAt          time.Time `json:"entered_at"`
	}
	requestLegRow struct {
		VersionNo     int    `json:"version_no"`
		LegNo         int    `json:"leg_no"`
		LoadingCode   string `json:"loading_code"`
		LoadingIATA   string `json:"loading_iata"`
		DischargeCode string `json:"discharge_code"`
		DischargeIATA string `json:"discharge_iata"`
		Flight        string `json:"flight"`
		Vessel        string `json:"vessel"`
		Voyage        string `json:"voyage"`
		DepartureDate string `json:"departure_date"`
	}
	equipmentRow struct {
		VersionNo int    `json:"version_no"`
		LineNo    int    `json:"line_no"`
		SizeType  string `json:"size_type"`
		Count     int64  `json:"count"`
	}
	stepRow struct {
		StepNo         int       `json:"step_no"`
		Kind           string    `json:"kind"`
		RequestVersion int       `json:"request_version"`
		Answers        int       `json:"answers_request_version"`
		Reason         string    `json:"reason"`
		ReasonCode     string    `json:"reason_code"`
		ReasonText     string    `json:"reason_text"`
		Channel        string    `json:"channel"`
		SHA256         string    `json:"evidence_sha256"`
		StorageID      string    `json:"evidence_storage_id"`
		MediaType      string    `json:"evidence_media_type"`
		CapturedAt     time.Time `json:"evidence_captured_at"`
		CarrierTime    time.Time `json:"carrier_time"`
		SentBy         string    `json:"sent_by"`
		SentAt         time.Time `json:"sent_at"`
		RecordedBy     string    `json:"recorded_by"`
		RecordedAt     time.Time `json:"recorded_at"`
		EnteredAt      time.Time `json:"entered_at"`
		Capacity       int       `json:"capacity_version"`
	}
	referenceRow struct {
		Type      string `json:"ref_type"`
		Value     string `json:"value"`
		FirstStep int    `json:"first_step"`
	}
	capacityRow struct {
		VersionNo       int    `json:"version_no"`
		Reason          string `json:"reason"`
		ResponseStep    int    `json:"response_step"`
		LocationEdition string `json:"location_edition"`
	}
	lineRow struct {
		VersionNo        int    `json:"version_no"`
		LineNo           int    `json:"line_no"`
		Pieces           int64  `json:"pieces"`
		GrossWeightUnit  string `json:"gross_weight_unit"`
		GrossWeight      int64  `json:"gross_weight"`
		VolumeUnit       string `json:"volume_unit"`
		Volume           int64  `json:"volume"`
		CarrierReference string `json:"carrier_reference"`
	}
	legRow struct {
		VersionNo     int       `json:"version_no"`
		LineNo        int       `json:"line_no"`
		LegNo         int       `json:"leg_no"`
		LoadingCode   string    `json:"loading_code"`
		LoadingIATA   string    `json:"loading_iata"`
		DischargeCode string    `json:"discharge_code"`
		DischargeIATA string    `json:"discharge_iata"`
		Flight        string    `json:"flight"`
		Vessel        string    `json:"vessel"`
		VesselIMO     string    `json:"vessel_imo"`
		Voyage        string    `json:"voyage"`
		ETD           time.Time `json:"etd"`
		ETA           time.Time `json:"eta"`
	}
	cutOffRow struct {
		VersionNo    int       `json:"version_no"`
		LineNo       int       `json:"line_no"`
		LegNo        int       `json:"leg_no"`
		Kind         string    `json:"kind"`
		At           time.Time `json:"cut_off_at"`
		Zone         string    `json:"time_zone"`
		LocationCode string    `json:"location_code"`
	}
	allocationRow struct {
		AllocationNo int       `json:"allocation_no"`
		VersionNo    int       `json:"version_no"`
		Capacity     int       `json:"capacity_version"`
		ConsumerKind string    `json:"consumer_kind"`
		ConsumerID   string    `json:"consumer_id"`
		Status       string    `json:"status"`
		Reason       string    `json:"reason"`
		WrittenBy    string    `json:"written_by"`
		WrittenAt    time.Time `json:"written_at"`
		EnteredAt    time.Time `json:"entered_at"`
	}
)

func quantity(unit string, amount int64) kernel.Quantity {
	return kernel.Quantity{Unit: kernel.UnitCode(unit), Amount: amount}
}

func location(code, iata string) book.Location {
	return book.Location{Code: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}, IATA: iata}
}

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

func child(table, order string) string {
	return `(SELECT coalesce(json_agg(x ORDER BY ` + order + `), '[]') FROM cargo_book.` + table + ` x WHERE x.tenant_id=b.tenant_id AND x.booking_id=b.booking_id)`
}

// readBooking reads a booking with all its rows in one statement, so all come from one snapshot.
func readBooking(ctx context.Context, q queryRower, tenant book.TenantID, id book.BookingID) (*book.Booking, int64, error) {
	st := book.BookingState{TenantID: tenant, BookingID: id}
	var kind, status, carrierRole string
	var validUntil, withdrawnAt *time.Time
	var designator, prefix, rebooks, withdrawReason, withdrawnBy *string
	var revision int64
	var blobs [12][]byte
	err := q.QueryRow(ctx, `SELECT b.kind, b.status, b.legal_entity_id, b.carrier_party_id, b.carrier_party_revision, b.carrier_role,
        b.carrier_role_valid_from, b.carrier_role_valid_until, b.carrier_role_evidence, b.carrier_iata_designator, b.carrier_awb_prefix,
        b.consumer_kind, b.consumer_id, b.rebooks_booking_id, b.withdraw_reason, b.withdrawn_by, b.withdrawn_at, b.revision, `+
		child("booking_request_version", "x.version_no")+`, `+child("booking_request_leg", "x.version_no, x.leg_no")+`, `+
		child("booking_request_equipment", "x.version_no, x.size_type")+`, `+child("booking_submission", "x.step_no")+`, `+
		child("carrier_response", "x.step_no")+`, `+child("carrier_reference", "x.first_step, x.ref_type, x.value")+`, `+
		child("capacity_version", "x.version_no")+`, `+child("capacity_line", "x.version_no, x.line_no")+`, `+
		child("capacity_equipment", "x.version_no, x.line_no, x.size_type")+`, `+child("capacity_leg", "x.version_no, x.line_no, x.leg_no")+`, `+
		child("capacity_cut_off", "x.version_no, x.line_no, x.leg_no, x.kind")+`, `+child("capacity_allocation", "x.allocation_no, x.version_no")+`
      FROM cargo_book.carrier_booking b WHERE b.tenant_id=$1 AND b.booking_id=$2`, string(tenant), string(id)).Scan(
		&kind, &status, &st.LegalEntityID, &st.Carrier.PartyID, &st.Carrier.PartyRevision, &carrierRole, &st.Carrier.ValidFrom, &validUntil,
		&st.Carrier.RoleEvidence, &designator, &prefix, &st.ConsumerKind, &st.ConsumerID, &rebooks, &withdrawReason, &withdrawnBy, &withdrawnAt,
		&revision, &blobs[0], &blobs[1], &blobs[2], &blobs[3], &blobs[4], &blobs[5], &blobs[6], &blobs[7], &blobs[8], &blobs[9], &blobs[10], &blobs[11])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: booking %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	var (
		requests   []requestRow
		reqLegs    []requestLegRow
		reqEquip   []equipmentRow
		submits    []stepRow
		responses  []stepRow
		references []referenceRow
		capacities []capacityRow
		lines      []lineRow
		lineEquip  []equipmentRow
		legs       []legRow
		cutOffs    []cutOffRow
		allocs     []allocationRow
	)
	targets := []any{&requests, &reqLegs, &reqEquip, &submits, &responses, &references, &capacities, &lines, &lineEquip, &legs, &cutOffs, &allocs}
	for i, target := range targets {
		if err = json.Unmarshal(blobs[i], target); err != nil {
			return nil, 0, err
		}
	}
	st.Kind, st.Status, st.Carrier.Role = book.Kind(kind), book.Status(status), carrierRole
	st.Carrier.ValidFrom = st.Carrier.ValidFrom.UTC()
	if validUntil != nil {
		st.Carrier.ValidUntil = validUntil.UTC()
	}
	st.Carrier.IATADesignator, st.Carrier.AWBPrefix, st.Rebooks = deref(designator), deref(prefix), book.BookingID(deref(rebooks))
	if withdrawnAt != nil {
		st.WithdrawReason, st.WithdrawnBy, st.WithdrawnAt = deref(withdrawReason), deref(withdrawnBy), withdrawnAt.UTC()
	}
	for _, r := range requests {
		req := book.Request{No: r.VersionNo, ForwarderReference: r.ForwarderReference, HandlingCodes: r.HandlingCodes, LocationEdition: r.LocationEdition,
			Reason: r.Reason, By: r.WrittenBy, At: utc(r.WrittenAt), EnteredAt: utc(r.EnteredAt),
			Capacity: book.Capacity{Pieces: r.Pieces, GrossWeight: quantity(r.GrossWeightUnit, r.GrossWeight), Volume: quantity(r.VolumeUnit, r.Volume)}}
		if len(req.HandlingCodes) == 0 {
			req.HandlingCodes = nil
		}
		for _, l := range reqLegs {
			if l.VersionNo == r.VersionNo {
				req.Legs = append(req.Legs, book.RequestedLeg{Loading: location(l.LoadingCode, l.LoadingIATA), Discharge: location(l.DischargeCode, l.DischargeIATA),
					Flight: l.Flight, Vessel: l.Vessel, Voyage: l.Voyage, DepartureDate: l.DepartureDate})
			}
		}
		for _, e := range reqEquip {
			if e.VersionNo == r.VersionNo {
				req.Capacity.Equipment = append(req.Capacity.Equipment, book.Equipment{SizeType: e.SizeType, Count: e.Count})
			}
		}
		st.Requests = append(st.Requests, req)
	}
	for _, s := range submits {
		st.Submissions = append(st.Submissions, book.Submission{Step: s.StepNo, Kind: book.SubmissionKind(s.Kind), RequestVersion: s.RequestVersion,
			Reason: s.Reason, Channel: book.Channel(s.Channel), Evidence: evidence(s), By: s.SentBy, At: utc(s.SentAt), EnteredAt: utc(s.EnteredAt)})
	}
	for _, r := range responses {
		resp := book.Response{Step: r.StepNo, Kind: book.ResponseKind(r.Kind), Answers: r.Answers, ReasonCode: r.ReasonCode, ReasonText: r.ReasonText,
			Channel: book.Channel(r.Channel), Evidence: evidence(r), CarrierTime: utc(r.CarrierTime), By: r.RecordedBy, At: utc(r.RecordedAt),
			EnteredAt: utc(r.EnteredAt), CapacityVersion: r.Capacity}
		for _, ref := range references {
			if ref.FirstStep == r.StepNo {
				resp.References = append(resp.References, book.Reference{Type: book.ReferenceType(ref.Type), Value: ref.Value})
			}
		}
		st.Responses = append(st.Responses, resp)
	}
	for _, c := range capacities {
		cv := book.CapacityVersion{No: c.VersionNo, Reason: book.CapacityReason(c.Reason), Response: c.ResponseStep, LocationEdition: c.LocationEdition}
		for _, l := range lines {
			if l.VersionNo != c.VersionNo {
				continue
			}
			line := book.Line{No: l.LineNo, CarrierReference: l.CarrierReference,
				Capacity: book.Capacity{Pieces: l.Pieces, GrossWeight: quantity(l.GrossWeightUnit, l.GrossWeight), Volume: quantity(l.VolumeUnit, l.Volume)}}
			for _, e := range lineEquip {
				if e.VersionNo == c.VersionNo && e.LineNo == l.LineNo {
					line.Capacity.Equipment = append(line.Capacity.Equipment, book.Equipment{SizeType: e.SizeType, Count: e.Count})
				}
			}
			for _, g := range legs {
				if g.VersionNo != c.VersionNo || g.LineNo != l.LineNo {
					continue
				}
				leg := book.Leg{Loading: location(g.LoadingCode, g.LoadingIATA), Discharge: location(g.DischargeCode, g.DischargeIATA), Flight: g.Flight,
					Vessel: g.Vessel, VesselIMO: g.VesselIMO, Voyage: g.Voyage, ETD: utc(g.ETD), ETA: utc(g.ETA)}
				for _, k := range cutOffs {
					if k.VersionNo == c.VersionNo && k.LineNo == l.LineNo && k.LegNo == g.LegNo {
						leg.CutOffs = append(leg.CutOffs, book.CutOff{Kind: book.CutOffKind(k.Kind), At: utc(k.At), Zone: k.Zone,
							Location: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: k.LocationCode}})
					}
				}
				line.Legs = append(line.Legs, leg)
			}
			cv.Lines = append(cv.Lines, line)
		}
		st.Capacities = append(st.Capacities, cv)
	}
	for _, a := range allocs {
		st.Allocations = append(st.Allocations, book.Allocation{No: a.AllocationNo, Version: a.VersionNo, CapacityVersion: a.Capacity,
			ConsumerKind: a.ConsumerKind, ConsumerID: a.ConsumerID, Status: book.AllocationStatus(a.Status), Reason: a.Reason, By: a.WrittenBy,
			At: utc(a.WrittenAt), EnteredAt: utc(a.EnteredAt)})
	}
	b, err := book.RestoreBooking(st)
	return b, revision, err
}

func evidence(r stepRow) book.Evidence {
	return book.Evidence{SHA256: r.SHA256, StorageID: r.StorageID, MediaType: r.MediaType, CapturedAt: utc(r.CapturedAt)}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// orNull returns nil for a zero value, so that the column stores NULL.
func orNull[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}

// Insert writes a new booking at revision 1 with its first request version.
func (s BookingStore) Insert(ctx context.Context, b *book.Booking) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := b.State()
	c := st.Carrier
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_book.carrier_booking (tenant_id,booking_id,kind,status,legal_entity_id,carrier_party_id,
        carrier_party_revision,carrier_role,carrier_role_valid_from,carrier_role_valid_until,carrier_role_evidence,carrier_iata_designator,
        carrier_awb_prefix,consumer_kind,consumer_id,rebooks_booking_id,current_request_version)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING revision`,
		string(st.TenantID), string(st.BookingID), string(st.Kind), string(st.Status), st.LegalEntityID, c.PartyID, c.PartyRevision, c.Role,
		c.ValidFrom, orNull(c.ValidUntil), c.RoleEvidence, orNull(c.IATADesignator), orNull(c.AWBPrefix), st.ConsumerKind, st.ConsumerID,
		orNull(string(st.Rebooks)), len(st.Requests)).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "carrier_booking_pkey" {
		return 0, fmt.Errorf("%w: booking %s", bentorepo.ErrAlreadyExists, st.BookingID)
	}
	if err != nil {
		return 0, err
	}
	return revision, s.writeAdded(ctx, q, b)
}

// Save writes the booking's status and pointers and what it added since it was loaded, if the stored revision
// is still the expected one. An ended booking's references stop being live.
func (s BookingStore) Save(ctx context.Context, b *book.Booking, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := b.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_book.carrier_booking SET status=$3,current_request_version=$4,current_capacity_version=$5,consumer_id=$6,
        withdraw_reason=$7,withdrawn_by=$8,withdrawn_at=$9,revision=revision+1,updated_at=now()
      WHERE tenant_id=$1 AND booking_id=$2 AND revision=$10 RETURNING revision`,
		string(st.TenantID), string(st.BookingID), string(st.Status), len(st.Requests), orNull(len(st.Capacities)), st.ConsumerID,
		orNull(st.WithdrawReason), orNull(st.WithdrawnBy), orNull(st.WithdrawnAt), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: booking %s", bentorepo.ErrConflict, st.BookingID)
	}
	if err != nil {
		return 0, err
	}
	if st.Status.Terminal() {
		if _, err = q.Exec(ctx, `UPDATE cargo_book.carrier_reference SET live=false WHERE tenant_id=$1 AND booking_id=$2 AND live`,
			string(st.TenantID), string(st.BookingID)); err != nil {
			return 0, err
		}
	}
	return revision, s.writeAdded(ctx, q, b)
}

// writeAdded inserts what the booking added, parents before children.
func (s BookingStore) writeAdded(ctx context.Context, q bentopg.Executor, b *book.Booking) error {
	st := b.State()
	tenant, id := string(st.TenantID), string(st.BookingID)
	requests, submissions, responses, capacities, allocations := b.Added()
	exec := func(sql string, args ...any) error {
		_, err := q.Exec(ctx, sql, append([]any{tenant, id}, args...)...)
		return err
	}
	for _, r := range requests {
		c := r.Capacity
		if err := exec(`INSERT INTO cargo_book.booking_request_version (tenant_id,booking_id,version_no,forwarder_reference,pieces,gross_weight_unit,
            gross_weight,volume_unit,volume,handling_codes,location_edition,reason,written_by,written_at,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, r.No, orNull(r.ForwarderReference), orNull(c.Pieces),
			orNull(string(c.GrossWeight.Unit)), orNull(c.GrossWeight.Amount), orNull(string(c.Volume.Unit)), orNull(c.Volume.Amount),
			append([]string{}, r.HandlingCodes...), r.LocationEdition, orNull(r.Reason), r.By, r.At, r.EnteredAt); err != nil {
			return err
		}
		for i, l := range r.Legs {
			if err := exec(`INSERT INTO cargo_book.booking_request_leg (tenant_id,booking_id,version_no,leg_no,loading_code,loading_iata,discharge_code,
                discharge_iata,flight,vessel,voyage,departure_date) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, r.No, i+1, l.Loading.Code.Code,
				orNull(l.Loading.IATA), l.Discharge.Code.Code, orNull(l.Discharge.IATA), orNull(l.Flight), orNull(l.Vessel), orNull(l.Voyage),
				orNull(l.DepartureDate)); err != nil {
				return err
			}
		}
		for _, e := range c.Equipment {
			if err := exec(`INSERT INTO cargo_book.booking_request_equipment (tenant_id,booking_id,version_no,size_type,count) VALUES ($1,$2,$3,$4,$5)`,
				r.No, e.SizeType, e.Count); err != nil {
				return err
			}
		}
	}
	for _, sub := range submissions {
		e := sub.Evidence
		if err := exec(`INSERT INTO cargo_book.booking_submission (tenant_id,booking_id,step_no,kind,request_version,reason,channel,evidence_sha256,
            evidence_storage_id,evidence_media_type,evidence_captured_at,sent_by,sent_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			sub.Step, string(sub.Kind), sub.RequestVersion, orNull(sub.Reason), string(sub.Channel), e.SHA256, e.StorageID, e.MediaType, e.CapturedAt,
			sub.By, sub.At, sub.EnteredAt); err != nil {
			return err
		}
	}
	for _, r := range responses {
		e := r.Evidence
		if err := exec(`INSERT INTO cargo_book.carrier_response (tenant_id,booking_id,step_no,kind,answers_request_version,reason_code,reason_text,channel,
            evidence_sha256,evidence_storage_id,evidence_media_type,evidence_captured_at,verification_level,carrier_time,recorded_by,recorded_at,entered_at,
            capacity_version) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
			r.Step, string(r.Kind), r.Answers, orNull(r.ReasonCode), orNull(r.ReasonText), string(r.Channel), e.SHA256, e.StorageID, e.MediaType,
			e.CapturedAt, book.VerificationOperatorEntered, orNull(r.CarrierTime), r.By, r.At, r.EnteredAt, orNull(r.CapacityVersion)); err != nil {
			return err
		}
	}
	firstNew := 0
	if len(responses) > 0 {
		firstNew = responses[0].Step
	}
	for _, ref := range b.References() {
		if firstNew == 0 || ref.Step < firstNew {
			continue
		}
		err := exec(`INSERT INTO cargo_book.carrier_reference (tenant_id,booking_id,ref_type,value,carrier_party_id,first_step,live)
            VALUES ($1,$2,$3,$4,$5,$6,$7)`, string(ref.Type), ref.Value, st.Carrier.PartyID, ref.Step, !st.Status.Terminal())
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "carrier_reference_live" {
			return fmt.Errorf("%w: %s %s", app.ErrReferenceInUse, ref.Type, ref.Value)
		}
		if err != nil {
			return err
		}
	}
	for _, c := range capacities {
		if err := exec(`INSERT INTO cargo_book.capacity_version (tenant_id,booking_id,version_no,reason,response_step,location_edition)
            VALUES ($1,$2,$3,$4,$5,$6)`, c.No, string(c.Reason), c.Response, c.LocationEdition); err != nil {
			return err
		}
		for _, l := range c.Lines {
			cap := l.Capacity
			if err := exec(`INSERT INTO cargo_book.capacity_line (tenant_id,booking_id,version_no,line_no,pieces,gross_weight_unit,gross_weight,
                volume_unit,volume,carrier_reference) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, c.No, l.No, orNull(cap.Pieces),
				orNull(string(cap.GrossWeight.Unit)), orNull(cap.GrossWeight.Amount), orNull(string(cap.Volume.Unit)), orNull(cap.Volume.Amount),
				orNull(l.CarrierReference)); err != nil {
				return err
			}
			for _, e := range cap.Equipment {
				if err := exec(`INSERT INTO cargo_book.capacity_equipment (tenant_id,booking_id,version_no,line_no,size_type,count)
                    VALUES ($1,$2,$3,$4,$5,$6)`, c.No, l.No, e.SizeType, e.Count); err != nil {
					return err
				}
			}
			for i, g := range l.Legs {
				if err := exec(`INSERT INTO cargo_book.capacity_leg (tenant_id,booking_id,version_no,line_no,leg_no,loading_code,loading_iata,
                    discharge_code,discharge_iata,flight,vessel,vessel_imo,voyage,etd,eta) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
					c.No, l.No, i+1, g.Loading.Code.Code, orNull(g.Loading.IATA), g.Discharge.Code.Code, orNull(g.Discharge.IATA), orNull(g.Flight),
					orNull(g.Vessel), orNull(g.VesselIMO), orNull(g.Voyage), g.ETD, orNull(g.ETA)); err != nil {
					return err
				}
				for _, k := range g.CutOffs {
					if err := exec(`INSERT INTO cargo_book.capacity_cut_off (tenant_id,booking_id,version_no,line_no,leg_no,kind,cut_off_at,time_zone,
                        location_code) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, c.No, l.No, i+1, string(k.Kind), k.At, k.Zone, k.Location.Code); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, a := range allocations {
		if err := exec(`INSERT INTO cargo_book.capacity_allocation (tenant_id,booking_id,allocation_no,version_no,capacity_version,consumer_kind,
            consumer_id,status,reason,written_by,written_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			a.No, a.Version, a.CapacityVersion, a.ConsumerKind, a.ConsumerID, string(a.Status), a.Reason, a.By, a.At, a.EnteredAt); err != nil {
			return err
		}
	}
	return nil
}

// Conflict reports whether a commit failed on BOOK's deferred check, which only a concurrent write or a write
// past BOOK can trip.
func Conflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "carrier_booking_consistent"
}
