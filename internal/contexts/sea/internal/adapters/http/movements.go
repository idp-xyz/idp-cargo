package http

import (
	"encoding/json"
	"errors"
	nethttp "net/http"
	"net/url"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
)

type evidence struct {
	SHA256     string    `json:"sha256"`
	StorageID  string    `json:"storage_id"`
	MediaType  string    `json:"media_type"`
	CapturedAt time.Time `json:"captured_at"`
}

type cutOff struct {
	Kind     string    `json:"kind"`
	At       time.Time `json:"at"`
	TimeZone string    `json:"time_zone"`
	Location string    `json:"location,omitempty"`
}

type voyageLeg struct {
	Leg             int       `json:"leg"`
	Vessel          string    `json:"vessel"`
	VesselIMO       string    `json:"vessel_imo,omitempty"`
	Voyage          string    `json:"voyage"`
	From            string    `json:"from"`
	To              string    `json:"to"`
	ETD             time.Time `json:"etd"`
	ETA             time.Time `json:"eta,omitzero"`
	CutOffs         []cutOff  `json:"cut_offs"`
	BookingID       string    `json:"booking_id"`
	CapacityVersion int       `json:"capacity_version"`
	Current         bool      `json:"current"`
}

type masterBill struct {
	BillNo     int       `json:"bill_no"`
	Number     string    `json:"number"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	Evidence   evidence  `json:"evidence"`
	RecordedBy string    `json:"recorded_by"`
	RecordedAt time.Time `json:"recorded_at"`
}

type housePart struct {
	ObligationID      string    `json:"obligation_id"`
	ObligationVersion int64     `json:"obligation_version"`
	OrderID           string    `json:"order_id"`
	Packs             int64     `json:"packs"`
	PackageType       string    `json:"package_type"`
	GrossWeight       *quantity `json:"gross_weight,omitempty"`
	Volume            *quantity `json:"volume,omitempty"`
	GoodsDescription  string    `json:"goods_description,omitempty"`
}

// houseContainer is a sealed container holding the house's packs, as PHY verified it. Its container number is not
// here: it is the container visit's, which names the same execution_id and load_unit_no.
type houseContainer struct {
	ExecutionID string    `json:"execution_id"`
	LoadUnitNo  int       `json:"load_unit_no"`
	TypeCode    string    `json:"type_code"`
	Packs       int64     `json:"packs"`
	GrossWeight *quantity `json:"gross_weight,omitempty"`
	Volume      *quantity `json:"volume,omitempty"`
	LineNos     []int     `json:"line_nos"`
	Seal        string    `json:"seal"`
	SealSHA256  string    `json:"seal_sha256,omitempty"`
	SealedAt    time.Time `json:"sealed_at"`
}

type house struct {
	EntryNo         int              `json:"entry_no"`
	ShipmentID      string           `json:"shipment_id"`
	HBL             string           `json:"hbl"`
	FULRevision     int64            `json:"ful_revision"`
	PlanVersion     int              `json:"plan_version"`
	ContainerMode   string           `json:"container_mode,omitempty"`
	Packs           int64            `json:"packs"`
	GrossWeight     *quantity        `json:"gross_weight,omitempty"`
	Volume          *quantity        `json:"volume,omitempty"`
	Basis           string           `json:"basis"`
	EvidenceVersion int              `json:"evidence_version,omitempty"`
	Parts           []housePart      `json:"parts"`
	Containers      []houseContainer `json:"containers"`
	Start           string           `json:"start"`
	AssociatedAt    time.Time        `json:"associated_at"`
	WindowDrift     bool             `json:"window_drift"`
}

// movement is an ocean movement as its revision covers it: the voyage, the master bill references, the live houses
// and the totals. Its container visits and carrier facts are read at their own paths.
type movement struct {
	MovementID             string       `json:"movement_id"`
	Revision               int64        `json:"revision"`
	State                  string       `json:"state"`
	DocFactsVersion        int          `json:"doc_facts_version"`
	Kind                   string       `json:"kind"`
	ConsolidationID        string       `json:"consolidation_id"`
	OperatingLegalEntityID string       `json:"operating_legal_entity_id"`
	CarrierPartyID         string       `json:"carrier_party_id"`
	CarrierPartyRevision   int64        `json:"carrier_party_revision"`
	CarrierRole            string       `json:"carrier_role"`
	Origin                 string       `json:"origin"`
	Destination            string       `json:"destination"`
	LocationEdition        string       `json:"location_edition"`
	BookingID              string       `json:"booking_id"`
	CapacityVersion        int          `json:"capacity_version"`
	Legs                   []voyageLeg  `json:"legs"`
	MasterBillNumber       string       `json:"master_bill_number,omitempty"`
	MasterBillKind         string       `json:"master_bill_kind,omitempty"`
	MasterBills            []masterBill `json:"master_bills"`
	Houses                 []house      `json:"houses"`
	Packs                  int64        `json:"packs"`
	GrossWeight            *quantity    `json:"gross_weight,omitempty"`
	Volume                 *quantity    `json:"volume,omitempty"`
}

func movementView(m api.OceanMovement) movement {
	out := movement{MovementID: m.MovementID, Revision: m.Revision, State: m.State, DocFactsVersion: m.DocFactsVersion, Kind: m.Kind,
		ConsolidationID: m.ConsolidationID, OperatingLegalEntityID: m.OperatingLegalEntityID, CarrierPartyID: m.CarrierPartyID,
		CarrierPartyRevision: m.CarrierPartyRevision, CarrierRole: m.CarrierRole, Origin: m.Origin.Code, Destination: m.Destination.Code,
		LocationEdition: m.LocationEdition, BookingID: m.BookingID, CapacityVersion: m.CapacityVersion, Legs: []voyageLeg{},
		MasterBillNumber: m.MasterBillNumber, MasterBillKind: m.MasterBillKind, MasterBills: []masterBill{}, Houses: []house{}, Packs: m.Packs,
		GrossWeight: quantityOf(m.GrossWeight), Volume: quantityOf(m.Volume)}
	for _, l := range m.Legs {
		leg := voyageLeg{Leg: l.Leg, Vessel: l.Vessel, VesselIMO: l.VesselIMO, Voyage: l.Voyage, From: l.From.Code, To: l.To.Code, ETD: l.ETD, ETA: l.ETA,
			CutOffs: []cutOff{}, BookingID: l.BookingID, CapacityVersion: l.CapacityVersion, Current: l.Current}
		for _, c := range l.CutOffs {
			leg.CutOffs = append(leg.CutOffs, cutOff{Kind: c.Kind, At: c.At, TimeZone: c.TimeZone, Location: c.Location.Code})
		}
		out.Legs = append(out.Legs, leg)
	}
	for _, b := range m.MasterBills {
		out.MasterBills = append(out.MasterBills, masterBill{BillNo: b.BillNo, Number: b.Number, Kind: b.Kind, Status: b.Status,
			Evidence:   evidence{SHA256: b.Evidence.SHA256, StorageID: b.Evidence.StorageID, MediaType: b.Evidence.MediaType, CapturedAt: b.Evidence.CapturedAt},
			RecordedBy: b.RecordedBy, RecordedAt: b.RecordedAt})
	}
	for _, e := range m.Houses {
		h := house{EntryNo: e.EntryNo, ShipmentID: e.ShipmentID, HBL: e.HBL, FULRevision: e.FULRevision, PlanVersion: e.PlanVersion,
			ContainerMode: e.ContainerMode, Packs: e.Packs, GrossWeight: quantityOf(e.GrossWeight), Volume: quantityOf(e.Volume), Basis: e.Basis,
			EvidenceVersion: e.EvidenceVersion, Parts: []housePart{}, Containers: []houseContainer{}, Start: e.Start, AssociatedAt: e.AssociatedAt,
			WindowDrift: e.WindowDrift}
		for _, p := range e.Parts {
			h.Parts = append(h.Parts, housePart{ObligationID: p.ObligationID, ObligationVersion: p.ObligationVersion, OrderID: p.OrderID, Packs: p.Packs,
				PackageType: p.PackageType, GrossWeight: quantityOf(p.GrossWeight), Volume: quantityOf(p.Volume), GoodsDescription: p.GoodsDescription})
		}
		for _, c := range e.Containers {
			lines := c.LineNos
			if lines == nil {
				lines = []int{}
			}
			h.Containers = append(h.Containers, houseContainer{ExecutionID: c.ExecutionID, LoadUnitNo: c.LoadUnitNo, TypeCode: c.TypeCode, Packs: c.Packs,
				GrossWeight: quantityOf(c.GrossWeight), Volume: quantityOf(c.Volume), LineNos: lines, Seal: c.Seal, SealSHA256: c.SealSHA256, SealedAt: c.SealedAt})
		}
		out.Houses = append(out.Houses, h)
	}
	return out
}

// POST /v1/ocean-movements: OpenOceanMovement; 201 with the movement's Location.
func (h handlers) openMovement(w nethttp.ResponseWriter, r *nethttp.Request) {
	type body struct {
		MovementID      string `json:"movement_id"`
		ConsolidationID string `json:"consolidation_id"`
		BookingID       string `json:"booking_id"`
		occurred
	}
	create(w, r, func(rc api.Receipt) string { return movementPath(rc.MovementID) }, func(c command, b body) (api.Receipt, error) {
		return h.s.OpenOceanMovement(r.Context(), api.OpenOceanMovement{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			MovementID: b.MovementID, ConsolidationID: b.ConsolidationID, BookingID: b.BookingID, OccurredAt: c.occurredAt})
	})
}

func (h handlers) writeMovement(w nethttp.ResponseWriter, r *nethttp.Request, m api.OceanMovement, err error) {
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.ETag(w, m.Revision)
	httpapi.WriteJSON(w, nethttp.StatusOK, movementView(m))
}

// GET /v1/ocean-movements/{movement}; ETag is the movement's revision.
func (h handlers) getMovement(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	m, err := h.s.GetOceanMovement(r.Context(), api.GetOceanMovement{TenantID: p.TenantID, MovementID: r.PathValue("movement")})
	h.writeMovement(w, r, m, err)
}

// GET /v1/ocean-movements?issuer_party_id=&master_bill_number=: the movement that holds the number as its current
// master bill reference of the issuer (404 when none does).
func (h handlers) findMovement(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	issuer, number := q.Get("issuer_party_id"), q.Get("master_bill_number")
	if issuer == "" || number == "" || len(q) != 2 {
		httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", "name issuer_party_id and master_bill_number, and nothing else")
		return
	}
	m, err := h.s.FindOceanMovementByMasterBill(r.Context(), api.FindOceanMovementByMasterBill{TenantID: p.TenantID, IssuerPartyID: issuer, Number: number})
	h.writeMovement(w, r, m, err)
}

// POST /v1/ocean-movements/{movement}/link-carrier-booking: LinkCarrierBooking at If-Match's revision.
func (h handlers) linkCarrierBooking(w nethttp.ResponseWriter, r *nethttp.Request) {
	change(w, r, func(c command, revision int64, _ occurred) (api.Receipt, error) {
		return h.s.LinkCarrierBooking(r.Context(), api.LinkCarrierBooking{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			MovementID: r.PathValue("movement"), ExpectedRevision: revision, OccurredAt: c.occurredAt})
	})
}

// POST /v1/ocean-movements/{movement}/master-bills: RecordMasterBill; the receipt names the reference (bill_no).
func (h handlers) recordMasterBill(w nethttp.ResponseWriter, r *nethttp.Request) {
	type body struct {
		Number   string   `json:"number"`
		Kind     string   `json:"kind"`
		Evidence evidence `json:"evidence"`
		occurred
	}
	change(w, r, func(c command, revision int64, b body) (api.Receipt, error) {
		return h.s.RecordMasterBill(r.Context(), api.RecordMasterBill{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			MovementID: r.PathValue("movement"), ExpectedRevision: revision, Number: b.Number, Kind: b.Kind,
			Evidence:   api.Evidence{SHA256: b.Evidence.SHA256, StorageID: b.Evidence.StorageID, MediaType: b.Evidence.MediaType, CapturedAt: b.Evidence.CapturedAt},
			OccurredAt: c.occurredAt})
	})
}

// POST /v1/ocean-movements/{movement}/house-bills: AssociateHouseBill; hbl is the number given, or absent to draw the
// next of the legal entity's series. The receipt names the entry and the HBL.
func (h handlers) associateHouseBill(w nethttp.ResponseWriter, r *nethttp.Request) {
	type body struct {
		ShipmentID string `json:"shipment_id"`
		HBL        string `json:"hbl"`
		occurred
	}
	change(w, r, func(c command, revision int64, b body) (api.Receipt, error) {
		return h.s.AssociateHouseBill(r.Context(), api.AssociateHouseBill{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			MovementID: r.PathValue("movement"), ExpectedRevision: revision, ShipmentID: b.ShipmentID, HBL: b.HBL, OccurredAt: c.occurredAt})
	})
}

// POST /v1/ocean-movements/{movement}/house-bills/{shipment}/refresh: RefreshHouseBillFacts.
func (h handlers) refreshHouseBill(w nethttp.ResponseWriter, r *nethttp.Request) {
	change(w, r, func(c command, revision int64, _ occurred) (api.Receipt, error) {
		return h.s.RefreshHouseBillFacts(r.Context(), api.RefreshHouseBillFacts{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			MovementID: r.PathValue("movement"), ExpectedRevision: revision, ShipmentID: r.PathValue("shipment"), OccurredAt: c.occurredAt})
	})
}

// POST /v1/ocean-movements/{movement}/house-bills/{shipment}/end: EndHouseBillAssociation.
func (h handlers) endHouseBill(w nethttp.ResponseWriter, r *nethttp.Request) {
	change(w, r, func(c command, revision int64, _ occurred) (api.Receipt, error) {
		return h.s.EndHouseBillAssociation(r.Context(), api.EndHouseBillAssociation{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			MovementID: r.PathValue("movement"), ExpectedRevision: revision, ShipmentID: r.PathValue("shipment"), OccurredAt: c.occurredAt})
	})
}

// POST /v1/ocean-movements/{movement}/finalize-doc-facts: FinalizeOceanDocFacts; the receipt names the version
// published (facts_version).
func (h handlers) finalizeDocFacts(w nethttp.ResponseWriter, r *nethttp.Request) {
	change(w, r, func(c command, revision int64, _ occurred) (api.Receipt, error) {
		return h.s.FinalizeOceanDocFacts(r.Context(), api.FinalizeOceanDocFacts{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			MovementID: r.PathValue("movement"), ExpectedRevision: revision, OccurredAt: c.occurredAt})
	})
}

// docFacts is what the response says about a published OceanDocFacts v1 version, before its facts. status is FINAL
// while the version is the latest and the movement is FINAL, and SUPERSEDED otherwise.
type docFacts struct {
	Version     int       `json:"version"`
	Status      string    `json:"status"`
	SHA256      string    `json:"sha256"`
	FinalizedBy string    `json:"finalized_by"`
	FinalizedAt time.Time `json:"finalized_at"`
}

// GET /v1/ocean-movements/{movement}/doc-facts[/{version}]: the latest published version, or the one named. The
// member facts is the published JSON byte for byte, and sha256 is the digest of those bytes: they are written as
// they are stored, not encoded again, which could escape or order them differently.
func (h handlers) getDocFacts(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	version := 0
	if r.PathValue("version") != "" {
		if version, ok = positive(w, r, "version"); !ok {
			return
		}
	}
	f, err := h.s.GetOceanDocFacts(r.Context(), api.GetOceanDocFacts{TenantID: p.TenantID, MovementID: r.PathValue("movement"), Version: version})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	head, err := json.Marshal(docFacts{Version: f.Version, Status: f.Status, SHA256: f.SHA256, FinalizedBy: f.FinalizedBy, FinalizedAt: f.FinalizedAt})
	if err == nil && !json.Valid(f.Content) {
		err = errors.New("sea: a published version that is not JSON")
	}
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	body := make([]byte, 0, len(head)+len(f.Content)+12)
	body = append(body, head[:len(head)-1]...) // without its closing brace
	body = append(body, `,"facts":`...)
	body = append(body, f.Content...)
	body = append(body, '}', '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(nethttp.StatusOK)
	_, _ = w.Write(body)
}

// carrierFact is a carrier's report. happened_at is when the carrier says it happened.
type carrierFact struct {
	FactNo          int       `json:"fact_no"`
	Kind            string    `json:"kind"`
	ContainerNumber string    `json:"container_number,omitempty"`
	BillNumber      string    `json:"bill_number,omitempty"`
	Vessel          string    `json:"vessel"`
	Voyage          string    `json:"voyage"`
	Source          string    `json:"source"`
	Reference       string    `json:"reference"`
	SHA256          string    `json:"sha256"`
	HappenedAt      time.Time `json:"happened_at"`
	RecordedBy      string    `json:"recorded_by"`
	RecordedAt      time.Time `json:"recorded_at"`
}

// POST /v1/ocean-movements/{movement}/carrier-facts: RecordOceanCarrierFact; the receipt names the fact (fact_no).
// The movement's revision moves only when a loading returns a FINAL movement to OPEN.
func (h handlers) recordCarrierFact(w nethttp.ResponseWriter, r *nethttp.Request) {
	type body struct {
		Kind            string    `json:"kind"`
		ContainerNumber string    `json:"container_number"`
		BillNumber      string    `json:"bill_number"`
		Vessel          string    `json:"vessel"`
		Voyage          string    `json:"voyage"`
		Source          string    `json:"source"`
		Reference       string    `json:"reference"`
		SHA256          string    `json:"sha256"`
		HappenedAt      time.Time `json:"happened_at"`
		occurred
	}
	change(w, r, func(c command, revision int64, b body) (api.Receipt, error) {
		return h.s.RecordOceanCarrierFact(r.Context(), api.RecordOceanCarrierFact{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			MovementID: r.PathValue("movement"), ExpectedRevision: revision, Kind: b.Kind, ContainerNumber: b.ContainerNumber, BillNumber: b.BillNumber,
			Vessel: b.Vessel, Voyage: b.Voyage, Source: b.Source, Reference: b.Reference, SHA256: b.SHA256, HappenedAt: b.HappenedAt,
			OccurredAt: c.occurredAt})
	})
}

// GET /v1/ocean-movements/{movement}/carrier-facts: the carrier's reports, in the order they were recorded.
func (h handlers) listCarrierFacts(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	m, err := h.s.GetOceanMovement(r.Context(), api.GetOceanMovement{TenantID: p.TenantID, MovementID: r.PathValue("movement")})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	out := make([]carrierFact, 0, len(m.CarrierFacts))
	for _, f := range m.CarrierFacts {
		out = append(out, carrierFact{FactNo: f.FactNo, Kind: f.Kind, ContainerNumber: f.ContainerNumber, BillNumber: f.BillNumber, Vessel: f.Vessel,
			Voyage: f.Voyage, Source: f.Source, Reference: f.Reference, SHA256: f.SHA256, HappenedAt: f.OccurredAt, RecordedBy: f.RecordedBy,
			RecordedAt: f.RecordedAt})
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, map[string]any{"carrier_facts": out})
}

// POST /v1/ocean-movements/{movement}/cancel: CancelOceanMovement, with a reason.
func (h handlers) cancelMovement(w nethttp.ResponseWriter, r *nethttp.Request) {
	type body struct {
		Reason string `json:"reason"`
		occurred
	}
	change(w, r, func(c command, revision int64, b body) (api.Receipt, error) {
		return h.s.CancelOceanMovement(r.Context(), api.CancelOceanMovement{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			MovementID: r.PathValue("movement"), ExpectedRevision: revision, Reason: b.Reason, OccurredAt: c.occurredAt})
	})
}

// POST /v1/hbl-series: RegisterHblSeries; 201 with the series' Location.
func (h handlers) registerHblSeries(w nethttp.ResponseWriter, r *nethttp.Request) {
	type body struct {
		LegalEntityID string `json:"legal_entity_id"`
		Template      string `json:"template"`
		FirstNumber   int64  `json:"first_number"`
		occurred
	}
	create(w, r, func(rc api.Receipt) string { return "/v1/hbl-series/" + url.PathEscape(rc.MovementID) }, func(c command, b body) (api.Receipt, error) {
		return h.s.RegisterHblSeries(r.Context(), api.RegisterHblSeries{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			LegalEntityID: b.LegalEntityID, Template: b.Template, FirstNumber: b.FirstNumber, OccurredAt: c.occurredAt})
	})
}

// GET /v1/hbl-series/{legal_entity}: the legal entity's HBL numbering and where each period's sequence stands. It
// has no ETag: a series is registered once and no command changes it, and the sequence moves with every number drawn.
func (h handlers) getHblSeries(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	s, err := h.s.GetHblSeries(r.Context(), api.GetHblSeries{TenantID: p.TenantID, LegalEntityID: r.PathValue("legal_entity")})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	type period struct {
		Period string `json:"period"`
		Last   int64  `json:"last"`
	}
	last := make([]period, 0, len(s.Last))
	for _, l := range s.Last {
		last = append(last, period{Period: l.Period, Last: l.Last})
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, map[string]any{"legal_entity_id": s.LegalEntityID, "template": s.Template, "first_number": s.FirstNumber,
		"last": last})
}

// GET /v1/house-bills/{legal_entity}/{hbl}: where an HBL number belongs: its shipment and, while an association is
// live, the movement.
func (h handlers) findHouseBill(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	ref, err := h.s.FindHouseBill(r.Context(), api.FindHouseBill{TenantID: p.TenantID, LegalEntityID: r.PathValue("legal_entity"), HBL: r.PathValue("hbl")})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	out := map[string]any{"legal_entity_id": ref.LegalEntityID, "hbl": ref.HBL, "shipment_id": ref.ShipmentID}
	if ref.MovementID != "" {
		out["movement_id"], out["entry_no"] = ref.MovementID, ref.EntryNo
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, out)
}
