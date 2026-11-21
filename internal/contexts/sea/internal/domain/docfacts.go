package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// ErrNotFinalizable means the movement's facts cannot be published for DOC yet.
var ErrNotFinalizable = errors.New("sea: the movement's facts are not ready for DOC")

// OceanDocFacts is one published OceanDocFacts v1 (relationship 12, SEA -> DOC). It carries a VGM assertion's
// identity, mass, method and responsible party, and never the signatory's name. Since V2.90 it also carries each
// container's own on-board date and ContainerDates, which says that it does: a compatible addition (a reader that
// does not know the two fields reads what it read, and the movement's OnBoardAt is as it was). A version published
// before V2.90 is stored as it was and reads with ContainerDates false and no container date.
type OceanDocFacts struct {
	Version                int            `json:"version"`
	MovementID             string         `json:"movement_id"`
	ConsolidationID        string         `json:"consolidation_id"`
	Kind                   string         `json:"kind"`
	OperatingLegalEntityID string         `json:"operating_legal_entity_id"`
	CarrierPartyID         string         `json:"carrier_party_id"`
	CarrierPartyRevision   int64          `json:"carrier_party_revision"`
	CarrierRole            string         `json:"carrier_role"`
	MasterBillNumber       string         `json:"master_bill_number"`
	MasterBillKind         string         `json:"master_bill_kind"`
	BookingID              string         `json:"booking_id"`
	CapacityVersion        int            `json:"capacity_version"`
	Vessel                 string         `json:"vessel"`
	VesselIMO              string         `json:"vessel_imo,omitempty"`
	Voyage                 string         `json:"voyage"`
	PlaceOfReceipt         string         `json:"place_of_receipt"`
	PortOfLoading          string         `json:"port_of_loading"`
	PortOfDischarge        string         `json:"port_of_discharge"`
	LocationEdition        string         `json:"location_edition"`
	ETD                    time.Time      `json:"etd"`
	OnBoardAt              time.Time      `json:"on_board_at,omitzero"` // omitempty does not omit a zero time
	Containers             []DocContainer `json:"containers"`
	Houses                 []DocHouse     `json:"houses"`

	// ContainerDates says that each container states its own on-board date, as every version from V2.90 does.
	ContainerDates bool `json:"container_dates,omitempty"`
}

// DocContainer is one live container of the movement as the facts name it. OnBoardAt is its own earliest loading: its
// own LOADED_ON_BOARD event, or the carrier's earliest LOADED_ON_BOARD report that names it or, with exactly one live
// container, one that names none; zero when it is not loaded.
type DocContainer struct {
	VisitID                  string `json:"visit_id"`
	Number                   string `json:"number"`
	SizeType                 string `json:"size_type"`
	SealNumber               string `json:"seal_number"`
	SealParty                string `json:"seal_party"`
	VGMAssertionID           string `json:"vgm_assertion_id"`
	VGMGrams                 int64  `json:"vgm_grams"`
	VGMMethod                string `json:"vgm_method"`
	ResponsiblePartyID       string `json:"responsible_party_id"`
	ResponsiblePartyRevision int64  `json:"responsible_party_revision"`
	Packs                    int64  `json:"packs"`
	GrossWeightGrams         int64  `json:"gross_weight_grams"`
	VolumeCubicCentimetres   int64  `json:"volume_cubic_centimetres"`

	OnBoardAt time.Time `json:"on_board_at,omitzero"` // this container's own loading; none when it is not loaded
}

// DocHouse is one live house.
type DocHouse struct {
	ShipmentID             string         `json:"shipment_id"`
	HBL                    string         `json:"hbl"`
	PlanVersion            int            `json:"plan_version"`
	Packs                  int64          `json:"packs"`
	PackageType            string         `json:"package_type,omitempty"`
	GrossWeightGrams       int64          `json:"gross_weight_grams"`
	VolumeCubicCentimetres int64          `json:"volume_cubic_centimetres"`
	OrderIDs               []string       `json:"order_ids"`
	GoodsDescription       string         `json:"goods_description,omitempty"`
	Containers             []DocBreakdown `json:"containers"`
}

// DocBreakdown is a house's packs in one container.
type DocBreakdown struct {
	VisitID                string `json:"visit_id"`
	ContainerNumber        string `json:"container_number"`
	Packs                  int64  `json:"packs"`
	GrossWeightGrams       int64  `json:"gross_weight_grams"`
	VolumeCubicCentimetres int64  `json:"volume_cubic_centimetres"`
}

// DocFactsVersion is one immutable published version. Content is the JSON as published.
type DocFactsVersion struct {
	No        int
	Content   []byte
	Facts     OceanDocFacts
	SHA256    string
	By        string
	At        time.Time
	EnteredAt time.Time
}

// Digest is the SHA-256 of a version's published bytes, in hexadecimal.
func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// Finalize publishes the next OceanDocFacts version of an OPEN movement and makes it FINAL. plans are FUL's plans
// of the live houses as read just before. visits are the movement's container visits. An own container needs every
// live visit sealed, with a VGM submitted by the cut-off or accepted by the carrier, and every live house's packs
// stuffed in those containers. A co-load needs its co-load bill. Either way a voyage leg and a current master bill
// are recorded, and FUL still reports each house as SEA froze it.
func (m *Movement) Finalize(plans []Plan, visits []VisitState, by string, at time.Time) error {
	if m.state.State != StateOpen {
		return ErrInvalidTransition
	}
	houses := m.Houses()
	if len(houses) == 0 || len(m.Current().Legs) == 0 {
		return fmt.Errorf("%w: a voyage and a house", ErrNotFinalizable)
	}
	bill, ok := m.CurrentBill()
	if !ok {
		return fmt.Errorf("%w: the master bill reference", ErrNotFinalizable)
	}
	live := liveVisits(visits)
	items := m.PendingItems(plans, live)
	blocking := 0
	var first *PendingItem
	for i, it := range items {
		if it.Blocking {
			blocking++
			if first == nil {
				first = &items[i]
			}
		}
	}
	refuse := func() error {
		more := ""
		if blocking > 1 {
			more = fmt.Sprintf(" (and %d more pending items; SEA's pending items of the movement list them)", blocking-1)
		}
		return fmt.Errorf("%w: %s%s", ErrNotFinalizable, first.Message, more)
	}
	if first != nil && houseStage(first.Kind) {
		return refuse()
	}
	if m.state.Kind == KindOwn {
		if bill.Kind != BillCarrierMBL && bill.Kind != BillCarrierSeaWaybill {
			return fmt.Errorf("%w: the carrier's master bill reference", ErrNotFinalizable)
		}
	} else if bill.Kind != BillCoLoad {
		return fmt.Errorf("%w: the co-load bill reference", ErrNotFinalizable)
	}
	if first != nil {
		return refuse()
	}
	if err := m.changeable(by, at); err != nil {
		return err
	}
	built, err := m.oceanDocFacts(len(m.state.DocFacts)+1, houses, live)
	if err != nil {
		return err
	}
	content, err := json.Marshal(built)
	if err != nil {
		return err
	}
	m.state.DocFacts = append(m.state.DocFacts, DocFactsVersion{No: built.Version, Content: content, Facts: built, SHA256: Digest(content), By: by, At: at.UTC()})
	m.state.State = StateFinal
	m.record(eventFinalized, at, "", 0, 0, "")
	m.pending[len(m.pending)-1].FactsVersion = built.Version
	return nil
}

func liveVisits(visits []VisitState) []VisitState {
	var out []VisitState
	for _, v := range visits {
		if v.Status != VisitReleased {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b VisitState) int { return a.LoadUnitNo - b.LoadUnitNo })
	return out
}

func vgmReady(v VisitState, cut time.Time, hasCut bool) bool {
	if len(v.Assertions) == 0 {
		return false
	}
	a := v.Assertions[len(v.Assertions)-1]
	switch a.Response {
	case VgmAccepted:
		return true
	case VgmRejected:
		return false
	}
	return !hasCut || !a.SubmittedAt.After(cut)
}

func (m *Movement) oceanDocFacts(version int, houses []Entry, visits []VisitState) (OceanDocFacts, error) {
	st, link, leg := m.state, m.Current(), m.Current().Legs[0]
	bill, _ := m.CurrentBill()
	f := OceanDocFacts{Version: version, MovementID: string(st.MovementID), ConsolidationID: st.ConsolidationID, Kind: string(st.Kind),
		OperatingLegalEntityID: st.LegalEntityID, CarrierPartyID: st.Carrier.PartyID, CarrierPartyRevision: st.Carrier.PartyRevision, CarrierRole: st.Carrier.Role,
		MasterBillNumber: bill.Number, MasterBillKind: bill.Kind, BookingID: link.BookingID, CapacityVersion: link.CapacityVersion,
		Vessel: leg.Vessel, VesselIMO: leg.VesselIMO, Voyage: leg.Voyage, PlaceOfReceipt: st.Origin.Code, PortOfLoading: leg.From.Code,
		PortOfDischarge: leg.To.Code, LocationEdition: st.Edition, ETD: leg.ETD.UTC(), ContainerDates: true, Containers: []DocContainer{}, Houses: []DocHouse{}}
	byUnit := map[int]VisitState{}
	for _, v := range visits {
		byUnit[v.LoadUnitNo] = v
		c := DocContainer{VisitID: string(v.VisitID), Number: v.Number, SizeType: v.SizeType}
		if n := len(v.Seals); n > 0 {
			c.SealNumber, c.SealParty = v.Seals[n-1].Number, v.Seals[n-1].Party
		}
		if n := len(v.Assertions); n > 0 {
			a := v.Assertions[n-1]
			c.VGMAssertionID = string(v.VisitID) + ":" + fmt.Sprint(a.Version)
			c.VGMGrams, c.VGMMethod, c.ResponsiblePartyID, c.ResponsiblePartyRevision = a.MassGrams, a.Method, a.PartyID, a.PartyRevision
		}
		for _, e := range v.Events {
			if e.Kind == EventLoaded && (f.OnBoardAt.IsZero() || e.OccurredAt.Before(f.OnBoardAt)) {
				f.OnBoardAt = e.OccurredAt.UTC()
			}
			if e.Kind == EventLoaded && (c.OnBoardAt.IsZero() || e.OccurredAt.Before(c.OnBoardAt)) {
				c.OnBoardAt = e.OccurredAt.UTC()
			}
		}
		// The carrier's report of a loading counts for the container it names and, with exactly one live container,
		// for a report that names none (as carrierLoadingFacts reads it).
		for _, r := range st.CarrierFacts {
			if r.Kind == CarrierLoaded && (r.ContainerNumber == v.Number || len(visits) == 1 && r.ContainerNumber == "") &&
				(c.OnBoardAt.IsZero() || r.OccurredAt.Before(c.OnBoardAt)) {
				c.OnBoardAt = r.OccurredAt.UTC()
			}
		}
		f.Containers = append(f.Containers, c)
	}
	// The carrier's own report of a loading counts as a container's does. A co-load has no container of its own, so
	// its on-board date comes only from here.
	for _, c := range st.CarrierFacts {
		if c.Kind == CarrierLoaded && (f.OnBoardAt.IsZero() || c.OccurredAt.Before(f.OnBoardAt)) {
			f.OnBoardAt = c.OccurredAt.UTC()
		}
	}
	slices.SortFunc(houses, func(a, b Entry) int { return strings.Compare(a.HBL, b.HBL) })
	for _, h := range houses {
		g, gg := measureGrams(h.Facts.GrossWeight)
		v, vv := measureCC(h.Facts.Volume)
		if gg != nil || vv != nil {
			return OceanDocFacts{}, fmt.Errorf("%w: %s %s weight or volume", ErrNotFinalizable, h.HBL, h.ShipmentID)
		}
		d := DocHouse{ShipmentID: h.ShipmentID, HBL: h.HBL, PlanVersion: h.PlanVersion, Packs: h.Facts.Packs, GrossWeightGrams: g,
			VolumeCubicCentimetres: v, OrderIDs: []string{}, Containers: []DocBreakdown{}}
		seen := map[string]bool{}
		var descriptions []string
		packageType, mixed := "", false
		for _, p := range h.Facts.Parts {
			if !seen[p.OrderID] {
				seen[p.OrderID] = true
				d.OrderIDs = append(d.OrderIDs, p.OrderID)
			}
			if p.GoodsDescription != "" && !slices.Contains(descriptions, p.GoodsDescription) {
				descriptions = append(descriptions, p.GoodsDescription)
			}
			if packageType == "" {
				packageType = p.PackageType
			} else if packageType != p.PackageType {
				mixed = true
			}
		}
		if !mixed {
			d.PackageType = packageType
		}
		if len(descriptions) == 1 {
			d.GoodsDescription = descriptions[0]
		}
		for _, s := range h.Facts.Stuffings {
			sg, sgErr := measureGrams(s.GrossWeight)
			sv, svErr := measureCC(s.Volume)
			if sgErr != nil || svErr != nil {
				return OceanDocFacts{}, fmt.Errorf("%w: %s stuffing weight or volume", ErrNotFinalizable, h.HBL)
			}
			visit := byUnit[s.LoadUnitNo]
			d.Containers = append(d.Containers, DocBreakdown{VisitID: string(visit.VisitID), ContainerNumber: visit.Number, Packs: s.Packs,
				GrossWeightGrams: sg, VolumeCubicCentimetres: sv})
			for i := range f.Containers {
				if f.Containers[i].VisitID == string(visit.VisitID) {
					f.Containers[i].Packs += s.Packs
					f.Containers[i].GrossWeightGrams += sg
					f.Containers[i].VolumeCubicCentimetres += sv
				}
			}
		}
		f.Houses = append(f.Houses, d)
	}
	return f, nil
}

// AddedDocFacts returns the versions published since the movement was loaded.
func (m *Movement) AddedDocFacts() []DocFactsVersion { return m.state.DocFacts[m.docs:] }

// DocFactsVersion returns a published version and whether it is the current one: the movement is FINAL and this is
// the latest version.
func (m *Movement) DocFactsVersion(version int) (DocFactsVersion, bool, bool) {
	if version == 0 {
		version = len(m.state.DocFacts)
	}
	if version < 1 || version > len(m.state.DocFacts) {
		return DocFactsVersion{}, false, false
	}
	v := m.state.DocFacts[version-1]
	return v, m.state.State == StateFinal && version == len(m.state.DocFacts), true
}

func measureGrams(q kernel.Quantity) (int64, error) {
	if q == (kernel.Quantity{}) {
		return 0, nil
	}
	n, ok := Grams(q)
	if !ok {
		return 0, fmt.Errorf("weight")
	}
	return n, nil
}

func measureCC(q kernel.Quantity) (int64, error) {
	if q == (kernel.Quantity{}) {
		return 0, nil
	}
	n, ok := CubicCentimetres(q)
	if !ok {
		return 0, fmt.Errorf("volume")
	}
	return n, nil
}

func (m *Movement) readDocFacts() error {
	for i := range m.state.DocFacts {
		v := &m.state.DocFacts[i]
		var f OceanDocFacts
		if v.No != i+1 || len(v.Content) == 0 || Digest(v.Content) != v.SHA256 || json.Unmarshal(v.Content, &f) != nil {
			return fmt.Errorf("doc facts version %d content", i+1)
		}
		v.Facts = f
		if v.Facts.Version != v.No || v.Facts.MovementID != string(m.state.MovementID) || blank(v.By) || v.At.IsZero() ||
			(i > 0 && v.At.Before(m.state.DocFacts[i-1].At)) {
			return fmt.Errorf("doc facts version %d", i+1)
		}
	}
	if m.state.State == StateFinal && len(m.state.DocFacts) == 0 {
		return fmt.Errorf("FINAL without doc facts")
	}
	return nil
}
