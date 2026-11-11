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
	PartPacks         int64     `json:"part_packs"`
	PartGrossWeight   int64     `json:"part_gross_weight"`
	PartVolume        int64     `json:"part_volume"`
	AddedBy           string    `json:"added_by"`
	AddedAt           time.Time `json:"added_at"`
	ReleasedBy        string    `json:"released_by"`
	ReleasedAt        time.Time `json:"released_at"`
	GoodsDescription  string    `json:"goods_description"`
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
	OriginIATA        string    `json:"origin_iata"`
	DestinationIATA   string    `json:"destination_iata"`
	LocationEdition   string    `json:"location_edition"`
	EnteredAt         time.Time `json:"entered_at"`
}

type attachmentRow struct {
	AttachmentNo       int       `json:"attachment_no"`
	Stage              string    `json:"stage"`
	ConsolidationID    string    `json:"consolidation_id"`
	MembershipEntryNo  int       `json:"membership_entry_no"`
	MembershipRevision int64     `json:"membership_revision"`
	AttachedFrom       time.Time `json:"attached_from"`
	RecordedBy         string    `json:"recorded_by"`
	RecordedAt         time.Time `json:"recorded_at"`
	EnteredAt          time.Time `json:"entered_at"`
	EndedBy            string    `json:"ended_by"`
	EndedAt            time.Time `json:"ended_at"`
	EndReason          string    `json:"end_reason"`
	EndEnteredAt       time.Time `json:"end_entered_at"`
}

type evidenceRow struct {
	EvidenceNo         int    `json:"evidence_no"`
	Digest             string `json:"digest"`
	Lots               int    `json:"lots"`
	ReceiptComplete    bool   `json:"receipt_complete"`
	ReceivedPacks      int64  `json:"received_packs"`
	ReceivedWeightUnit string `json:"received_weight_unit"`
	ReceivedWeight     int64  `json:"received_weight"`
	ReceivedVolumeUnit string `json:"received_volume_unit"`
	ReceivedVolume     int64  `json:"received_volume"`
	ConsolidationID    string `json:"consolidation_id"`
	TenderedPacks      int64  `json:"tendered_packs"`
	TenderedWeightUnit string `json:"tendered_weight_unit"`
	TenderedWeight     int64  `json:"tendered_weight"`
	TenderedVolumeUnit string `json:"tendered_volume_unit"`
	TenderedVolume     int64  `json:"tendered_volume"`
	Departed           int64  `json:"departed"`
	Arrived            int64  `json:"arrived"`
	Offloaded          int64  `json:"offloaded"`
	Security           string `json:"security"`
	OpenDiscrepancies  int    `json:"open_discrepancies"`
	Screenings         []struct {
		LotID      string    `json:"lot_id"`
		Outcome    string    `json:"outcome"`
		Screener   string    `json:"screener"`
		Method     string    `json:"method"`
		Reference  string    `json:"reference"`
		ScreenedAt time.Time `json:"screened_at"`
	} `json:"screenings"`
	ExecutionStatus string    `json:"execution_status"`
	DepartedFinal   int64     `json:"departed_final"`
	RecordedBy      string    `json:"recorded_by"`
	RecordedAt      time.Time `json:"recorded_at"`
	EnteredAt       time.Time `json:"entered_at"`
}

func (e evidenceRow) version() ful.EvidenceVersion {
	var screenings []ful.Screening
	for _, s := range e.Screenings {
		screenings = append(screenings, ful.Screening{LotID: s.LotID, Outcome: s.Outcome, Screener: s.Screener, Method: s.Method, Reference: s.Reference,
			ScreenedAt: utc(s.ScreenedAt)})
	}
	return ful.EvidenceVersion{No: e.EvidenceNo, Digest: e.Digest, Execution: ful.ExecutionStatus(e.ExecutionStatus), Final: e.DepartedFinal, By: e.RecordedBy,
		At: utc(e.RecordedAt), EnteredAt: utc(e.EnteredAt), Evidence: ful.LoadEvidence{Lots: e.Lots, ReceiptComplete: e.ReceiptComplete,
			Received: ful.CargoScope{Packs: e.ReceivedPacks, GrossWeight: quantity(e.ReceivedWeightUnit, e.ReceivedWeight),
				Volume: quantity(e.ReceivedVolumeUnit, e.ReceivedVolume)},
			ConsolidationID: e.ConsolidationID,
			Tendered: ful.CargoScope{Packs: e.TenderedPacks, GrossWeight: quantity(e.TenderedWeightUnit, e.TenderedWeight),
				Volume: quantity(e.TenderedVolumeUnit, e.TenderedVolume)},
			Departed: e.Departed, Arrived: e.Arrived, Offloaded: e.Offloaded, Security: ful.Security(e.Security),
			OpenDiscrepancies: e.OpenDiscrepancies, Screenings: screenings}}
}

type confirmationRow struct {
	PlanNo      int       `json:"plan_no"`
	ConfirmedBy string    `json:"confirmed_by"`
	ConfirmedAt time.Time `json:"confirmed_at"`
	EnteredAt   time.Time `json:"entered_at"`
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
	var obligationJSON, planJSON, confirmationJSON, attachmentJSON, evidenceJSON []byte
	err := q.QueryRow(ctx, `SELECT s.customer_party_id, s.legal_entity_id, s.status, s.cancelled_by, s.cancelled_at, s.cancel_reason, s.revision,
        (SELECT coalesce(json_agg(o ORDER BY o.entry_no), '[]') FROM cargo_ful.shipment_obligation o
          WHERE o.tenant_id=s.tenant_id AND o.shipment_id=s.shipment_id),
        (SELECT coalesce(json_agg(p ORDER BY p.plan_no), '[]') FROM cargo_ful.shipment_plan_version p
          WHERE p.tenant_id=s.tenant_id AND p.shipment_id=s.shipment_id),
        (SELECT coalesce(json_agg(c ORDER BY c.plan_no), '[]') FROM cargo_ful.shipment_plan_confirmation c
          WHERE c.tenant_id=s.tenant_id AND c.shipment_id=s.shipment_id),
        (SELECT coalesce(json_agg(a ORDER BY a.attachment_no), '[]') FROM cargo_ful.shipment_consol_attachment a
          WHERE a.tenant_id=s.tenant_id AND a.shipment_id=s.shipment_id),
        (SELECT coalesce(json_agg(to_jsonb(e) || jsonb_build_object('screenings',
            (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY x.screening_no), '[]') FROM cargo_ful.shipment_load_evidence_screening x
              WHERE x.tenant_id=e.tenant_id AND x.shipment_id=e.shipment_id AND x.evidence_no=e.evidence_no)) ORDER BY e.evidence_no), '[]')
          FROM cargo_ful.shipment_load_evidence e WHERE e.tenant_id=s.tenant_id AND e.shipment_id=s.shipment_id)
      FROM cargo_ful.shipment s WHERE s.tenant_id=$1 AND s.shipment_id=$2`, string(tenant), string(id)).Scan(
		&st.CustomerPartyID, &st.LegalEntityID, &status, &cancelledBy, &cancelledAt, &cancelReason, &revision,
		&obligationJSON, &planJSON, &confirmationJSON, &attachmentJSON, &evidenceJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	var obligations []obligationRow
	var plans []planRow
	var confirmations []confirmationRow
	var attachments []attachmentRow
	var evidence []evidenceRow
	if err = errors.Join(json.Unmarshal(obligationJSON, &obligations), json.Unmarshal(planJSON, &plans),
		json.Unmarshal(confirmationJSON, &confirmations), json.Unmarshal(attachmentJSON, &attachments),
		json.Unmarshal(evidenceJSON, &evidence)); err != nil {
		return nil, 0, err
	}
	for _, e := range evidence {
		st.Evidence = append(st.Evidence, e.version())
	}
	st.Status = ful.ShipmentStatus(status)
	if cancelledAt != nil && cancelledBy != nil && cancelReason != nil {
		st.CancelledBy, st.CancelledAt, st.CancelReason = *cancelledBy, cancelledAt.UTC(), *cancelReason
	}
	for _, o := range obligations {
		st.Obligations = append(st.Obligations, ful.PlannedObligation{
			ObligationRef: ful.ObligationRef{ObligationID: ful.ObligationID(o.ObligationID), ObligationVersion: o.ObligationVersion,
				OrderID: o.OrderID, CustomerPartyID: o.CustomerPartyID, LegalEntityID: o.LegalEntityID, Packs: o.Packs,
				PackageType: o.PackageType, GrossWeight: quantity(o.GrossWeightUnit, o.GrossWeight), Volume: quantity(o.VolumeUnit, o.Volume),
				Description: o.GoodsDescription},
			Part:    ful.CargoScope{Packs: o.PartPacks, GrossWeight: quantity(o.GrossWeightUnit, o.PartGrossWeight), Volume: quantity(o.VolumeUnit, o.PartVolume)},
			AddedBy: o.AddedBy, AddedAt: utc(o.AddedAt), ReleasedBy: o.ReleasedBy, ReleasedAt: utc(o.ReleasedAt)})
	}
	for _, p := range plans {
		st.Plans = append(st.Plans, ful.PlanVersion{No: p.PlanNo, Change: ful.PlanChange(p.Change),
			Route: ful.Route{Mode: ful.TransportMode(p.TransportMode),
				Origin:      kernel.LocationCode{Scheme: kernel.LocationScheme(p.OriginScheme), Code: p.OriginCode},
				Destination: kernel.LocationCode{Scheme: kernel.LocationScheme(p.DestinationScheme), Code: p.DestinationCode},
				Departure:   kernel.ValidityInterval{From: utc(p.DepartureFrom), Until: utc(p.DepartureUntil)},
				OriginIATA:  p.OriginIATA, DestinationIATA: p.DestinationIATA, LocationEdition: p.LocationEdition},
			Scope:  ful.CargoScope{Packs: p.ScopePacks, GrossWeight: quantity(p.ScopeWeightUnit, p.ScopeWeight), Volume: quantity(p.ScopeVolumeUnit, p.ScopeVolume)},
			Reason: p.Reason, By: p.WrittenBy, At: utc(p.WrittenAt), EnteredAt: utc(p.EnteredAt)})
	}
	for _, c := range confirmations {
		st.Confirmations = append(st.Confirmations, ful.Confirmation{PlanNo: c.PlanNo, By: c.ConfirmedBy, At: utc(c.ConfirmedAt), EnteredAt: utc(c.EnteredAt)})
	}
	for i, a := range attachments {
		if a.AttachmentNo != i+1 {
			return nil, 0, fmt.Errorf("%w: consol attachments of shipment %s are not numbered from 1", ful.ErrInvalidShipment, id)
		}
		st.Attachments = append(st.Attachments, ful.ConsolAttachment{Stage: a.Stage, ConsolidationID: a.ConsolidationID,
			MembershipEntryNo: a.MembershipEntryNo, MembershipRevision: a.MembershipRevision, AttachedFrom: utc(a.AttachedFrom),
			RecordedBy: a.RecordedBy, RecordedAt: utc(a.RecordedAt), EnteredAt: utc(a.EnteredAt), EndedBy: a.EndedBy,
			EndedAt: utc(a.EndedAt), EndReason: ful.AttachmentEnd(a.EndReason), EndEnteredAt: utc(a.EndEnteredAt)})
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
	for no, a := range shipment.EndedAttachments() {
		tag, err := q.Exec(ctx, `UPDATE cargo_ful.shipment_consol_attachment SET ended_by=$4,ended_at=$5,end_reason=$6,end_entered_at=$7
          WHERE tenant_id=$1 AND shipment_id=$2 AND attachment_no=$3 AND ended_at IS NULL`,
			string(st.TenantID), string(st.ShipmentID), no, a.EndedBy, a.EndedAt, string(a.EndReason), a.EndEnteredAt)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() != 1 {
			return 0, fmt.Errorf("%w: consol attachment %d of shipment %s", bentorepo.ErrConflict, no, st.ShipmentID)
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
            part_packs,part_gross_weight,part_volume,added_by,added_at,released_by,released_at,goods_description)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
			string(st.TenantID), string(st.ShipmentID), first+i+1, string(p.ObligationID), p.ObligationVersion,
			p.OrderID, p.CustomerPartyID, p.LegalEntityID, p.Packs, p.PackageType, string(p.GrossWeight.Unit), p.GrossWeight.Amount,
			orNull(string(p.Volume.Unit)), orNull(p.Volume.Amount), p.Part.Packs, p.Part.GrossWeight.Amount, orNull(p.Part.Volume.Amount),
			p.AddedBy, p.AddedAt, orNull(p.ReleasedBy), orNull(p.ReleasedAt), orNull(p.Description))
		var pgErr *pgconn.PgError
		switch {
		case errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "shipment_obligation_one_live_part":
			return fmt.Errorf("%w: %s", ful.ErrAlreadyPlanned, p.ObligationID)
		case errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "shipment_obligation_within_obligation":
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
            scope_weight,scope_volume_unit,scope_volume,reason,written_by,written_at,origin_iata,destination_iata,location_edition,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
			string(st.TenantID), string(st.ShipmentID), p.No, string(p.Change), orNull(string(r.Mode)),
			originScheme, originCode, destinationScheme, destinationCode, orNull(r.Departure.From), orNull(r.Departure.Until),
			p.Scope.Packs, string(p.Scope.GrossWeight.Unit), p.Scope.GrossWeight.Amount, orNull(string(p.Scope.Volume.Unit)),
			orNull(p.Scope.Volume.Amount), orNull(p.Reason), p.By, p.At, orNull(r.OriginIATA), orNull(r.DestinationIATA),
			orNull(r.LocationEdition), orNull(p.EnteredAt)); err != nil {
			return err
		}
	}
	for _, c := range shipment.AddedConfirmations() {
		if _, err := q.Exec(ctx, `INSERT INTO cargo_ful.shipment_plan_confirmation (tenant_id,shipment_id,plan_no,confirmed_by,confirmed_at,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6)`, string(st.TenantID), string(st.ShipmentID), c.PlanNo, c.By, c.At, orNull(c.EnteredAt)); err != nil {
			return err
		}
	}
	firstAttachment := len(st.Attachments) - len(shipment.AddedAttachments())
	for i, a := range shipment.AddedAttachments() {
		_, err := q.Exec(ctx, `INSERT INTO cargo_ful.shipment_consol_attachment (tenant_id,shipment_id,attachment_no,stage,consolidation_id,
            membership_entry_no,membership_revision,attached_from,recorded_by,recorded_at,entered_at,ended_by,ended_at,end_reason,end_entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			string(st.TenantID), string(st.ShipmentID), firstAttachment+i+1, a.Stage, a.ConsolidationID, a.MembershipEntryNo,
			a.MembershipRevision, a.AttachedFrom, a.RecordedBy, a.RecordedAt, a.EnteredAt, orNull(a.EndedBy), orNull(a.EndedAt),
			orNull(string(a.EndReason)), orNull(a.EndEnteredAt))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "shipment_consol_attachment_one_current" {
			return fmt.Errorf("%w: shipment %s has a current consol attachment on stage %s", bentorepo.ErrConflict, st.ShipmentID, a.Stage)
		}
		if err != nil {
			return err
		}
	}
	for _, v := range shipment.AddedEvidence() {
		e := v.Evidence
		if _, err := q.Exec(ctx, `INSERT INTO cargo_ful.shipment_load_evidence (tenant_id,shipment_id,evidence_no,digest,lots,receipt_complete,
            received_packs,received_weight_unit,received_weight,received_volume_unit,received_volume,consolidation_id,tendered_packs,
            tendered_weight_unit,tendered_weight,tendered_volume_unit,tendered_volume,departed,arrived,offloaded,security,open_discrepancies,
            execution_status,recorded_by,recorded_at,entered_at,departed_final)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
			string(st.TenantID), string(st.ShipmentID), v.No, v.Digest, e.Lots, e.ReceiptComplete, e.Received.Packs,
			orNull(string(e.Received.GrossWeight.Unit)), e.Received.GrossWeight.Amount, orNull(string(e.Received.Volume.Unit)), e.Received.Volume.Amount,
			orNull(e.ConsolidationID), e.Tendered.Packs, orNull(string(e.Tendered.GrossWeight.Unit)), e.Tendered.GrossWeight.Amount,
			orNull(string(e.Tendered.Volume.Unit)), e.Tendered.Volume.Amount, e.Departed, e.Arrived, e.Offloaded, orNull(string(e.Security)),
			e.OpenDiscrepancies, string(v.Execution), v.By, v.At, v.EnteredAt, v.Final); err != nil {
			return err
		}
		for i, sc := range e.Screenings {
			if _, err := q.Exec(ctx, `INSERT INTO cargo_ful.shipment_load_evidence_screening (tenant_id,shipment_id,evidence_no,screening_no,lot_id,outcome,
                screener,method,reference,screened_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
				string(st.TenantID), string(st.ShipmentID), v.No, i+1, sc.LotID, sc.Outcome, sc.Screener, sc.Method, sc.Reference, sc.ScreenedAt); err != nil {
				return err
			}
		}
	}
	return nil
}

// PlannedElsewhere returns the sum of the live parts other shipments plan of an obligation, in their units.
// It first takes the obligation's transaction lock, the one the shipment_obligation trigger takes, so that
// commands planning parts of one obligation run one after another and each sees what the others committed
// (read committed). A live part of another version or COM commitment is reported as planned elsewhere.
func (s ShipmentStore) PlannedElsewhere(ctx context.Context, tenant ful.TenantID, id ful.ObligationID, version int64, except ful.ShipmentID) (ful.CargoScope, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return ful.CargoScope{}, err
	}
	if _, err = q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cargo_ful.obligation:' || $1 || '/' || $2, 0))`,
		string(tenant), string(id)); err != nil {
		return ful.CargoScope{}, err
	}
	var versions, packs, weight, volume int64
	var weightUnit, volumeUnit *string
	err = q.QueryRow(ctx, `SELECT count(DISTINCT obligation_version) FILTER (WHERE obligation_version <> $3),
        coalesce(sum(part_packs), 0), coalesce(sum(part_gross_weight), 0), coalesce(sum(part_volume), 0),
        min(gross_weight_unit), min(volume_unit)
      FROM cargo_ful.shipment_obligation
      WHERE tenant_id=$1 AND obligation_id=$2 AND released_at IS NULL AND shipment_id <> $4`,
		string(tenant), string(id), version, string(except)).Scan(&versions, &packs, &weight, &volume, &weightUnit, &volumeUnit)
	if err != nil {
		return ful.CargoScope{}, err
	}
	if versions > 0 {
		return ful.CargoScope{}, fmt.Errorf("%w: another shipment plans another version of %s", app.ErrPlannedElsewhere, id)
	}
	if packs == 0 {
		return ful.CargoScope{}, nil
	}
	var elsewhere ful.CargoScope
	elsewhere.Packs = packs
	elsewhere.GrossWeight = quantity(deref(weightUnit), weight)
	elsewhere.Volume = quantity(deref(volumeUnit), volume)
	return elsewhere, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
