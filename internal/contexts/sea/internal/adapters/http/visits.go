package http

import (
	nethttp "net/http"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

type seal struct {
	SealNo     int       `json:"seal_no"`
	Number     string    `json:"number"`
	Party      string    `json:"party"`
	AppliedAt  time.Time `json:"applied_at"`
	Source     string    `json:"source"`
	RecordedBy string    `json:"recorded_by"`
	RecordedAt time.Time `json:"recorded_at"`
}

// vgmAssertion is one version of a container's verified gross mass. It has no signatory: that name is read, and
// logged, at the assertion's signatory-reads.
type vgmAssertion struct {
	Version                  int       `json:"version"`
	MassGrams                int64     `json:"mass_grams"`
	Method                   string    `json:"method"`
	ResponsiblePartyID       string    `json:"responsible_party_id"`
	ResponsiblePartyRevision int64     `json:"responsible_party_revision"`
	DeterminedAt             time.Time `json:"determined_at"`
	SubmittedAt              time.Time `json:"submitted_at"`
	LateReason               string    `json:"late_reason,omitempty"`
	RecordedBy               string    `json:"recorded_by"`
	RecordedAt               time.Time `json:"recorded_at"`
	Response                 string    `json:"response,omitempty"`
	ResponseSource           string    `json:"response_source,omitempty"`
	RespondedAt              time.Time `json:"responded_at,omitzero"`
	ResponseBy               string    `json:"response_by,omitempty"`
}

type containerEvent struct {
	EventNo    int       `json:"event_no"`
	Kind       string    `json:"kind"`
	Location   string    `json:"location"`
	HappenedAt time.Time `json:"happened_at"`
	Source     string    `json:"source"`
	Reference  string    `json:"reference"`
	RecordedBy string    `json:"recorded_by"`
	RecordedAt time.Time `json:"recorded_at"`
}

// containerVisit is a container visit with its seals, VGM assertions and events, latest first.
type containerVisit struct {
	VisitID          string           `json:"visit_id"`
	Revision         int64            `json:"revision"`
	MovementID       string           `json:"movement_id"`
	ConsolidationID  string           `json:"consolidation_id"`
	LoadUnitNo       int              `json:"load_unit_no"`
	ContainerNumber  string           `json:"container_number"`
	SizeType         string           `json:"size_type"`
	TareKg           int64            `json:"tare_kg,omitempty"`
	MaxGrossKg       int64            `json:"max_gross_kg,omitempty"`
	PlateSource      string           `json:"plate_source,omitempty"`
	Status           string           `json:"status"`
	SealNumber       string           `json:"seal_number,omitempty"`
	SealParty        string           `json:"seal_party,omitempty"`
	Seals            []seal           `json:"seals"`
	VgmAssertions    []vgmAssertion   `json:"vgm_assertions"`
	Events           []containerEvent `json:"events"`
	ExecutionID      string           `json:"execution_id,omitempty"`
	DifferenceReason string           `json:"difference_reason,omitempty"`
	StuffedAt        time.Time        `json:"stuffed_at,omitzero"`
	ReleaseReason    string           `json:"release_reason,omitempty"`
	ReleasedAt       time.Time        `json:"released_at,omitzero"`
	AssignedBy       string           `json:"assigned_by"`
	AssignedAt       time.Time        `json:"assigned_at"`
}

func visitView(v api.ContainerVisit) containerVisit {
	out := containerVisit{VisitID: v.VisitID, Revision: v.Revision, MovementID: v.MovementID, ConsolidationID: v.ConsolidationID,
		LoadUnitNo: v.LoadUnitNo, ContainerNumber: v.ContainerNumber, SizeType: v.SizeType, TareKg: v.TareKg, MaxGrossKg: v.MaxGrossKg,
		PlateSource: v.PlateSource, Status: v.Status, SealNumber: v.SealNumber, SealParty: v.SealParty, Seals: []seal{},
		VgmAssertions: []vgmAssertion{}, Events: []containerEvent{}, ExecutionID: v.ExecutionID, DifferenceReason: v.DifferenceReason,
		StuffedAt: v.StuffedAt, ReleaseReason: v.ReleaseReason, ReleasedAt: v.ReleasedAt, AssignedBy: v.AssignedBy, AssignedAt: v.AssignedAt}
	for _, s := range v.Seals {
		out.Seals = append(out.Seals, seal{SealNo: s.SealNo, Number: s.Number, Party: s.Party, AppliedAt: s.AppliedAt, Source: s.Source,
			RecordedBy: s.RecordedBy, RecordedAt: s.RecordedAt})
	}
	for _, a := range v.Assertions {
		out.VgmAssertions = append(out.VgmAssertions, vgmAssertion{Version: a.Version, MassGrams: a.MassGrams, Method: a.Method,
			ResponsiblePartyID: a.ResponsiblePartyID, ResponsiblePartyRevision: a.ResponsiblePartyRevision, DeterminedAt: a.DeterminedAt,
			SubmittedAt: a.SubmittedAt, LateReason: a.LateReason, RecordedBy: a.RecordedBy, RecordedAt: a.RecordedAt, Response: a.Response,
			ResponseSource: a.ResponseSource, RespondedAt: a.RespondedAt, ResponseBy: a.ResponseBy})
	}
	for _, e := range v.Events {
		out.Events = append(out.Events, containerEvent{EventNo: e.EventNo, Kind: e.Kind, Location: e.Location.Code, HappenedAt: e.HappenedAt,
			Source: e.Source, Reference: e.Reference, RecordedBy: e.RecordedBy, RecordedAt: e.RecordedAt})
	}
	return out
}

// POST /v1/container-visits: AssignContainer; 201 with the visit's Location.
func (h handlers) assignContainer(w nethttp.ResponseWriter, r *nethttp.Request) {
	type body struct {
		VisitID         string `json:"visit_id"`
		MovementID      string `json:"movement_id"`
		LoadUnitNo      int    `json:"load_unit_no"`
		ContainerNumber string `json:"container_number"`
		SizeType        string `json:"size_type"`
		TareKg          int64  `json:"tare_kg"`
		MaxGrossKg      int64  `json:"max_gross_kg"`
		PlateSource     string `json:"plate_source"`
		occurred
	}
	create(w, r, func(rc api.Receipt) string { return visitPath(rc.VisitID) }, func(c command, b body) (api.Receipt, error) {
		return h.s.AssignContainer(r.Context(), api.AssignContainer{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID, VisitID: b.VisitID,
			MovementID: b.MovementID, LoadUnitNo: b.LoadUnitNo, ContainerNumber: b.ContainerNumber, SizeType: b.SizeType, TareKg: b.TareKg,
			MaxGrossKg: b.MaxGrossKg, PlateSource: b.PlateSource, OccurredAt: c.occurredAt})
	})
}

func (h handlers) writeVisit(w nethttp.ResponseWriter, r *nethttp.Request, v api.ContainerVisit, err error) {
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.ETag(w, v.Revision)
	httpapi.WriteJSON(w, nethttp.StatusOK, visitView(v))
}

// GET /v1/container-visits/{visit}; ETag is the visit's revision.
func (h handlers) getContainerVisit(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	v, err := h.s.GetContainerVisit(r.Context(), api.GetContainerVisit{TenantID: p.TenantID, VisitID: r.PathValue("visit")})
	h.writeVisit(w, r, v, err)
}

// GET /v1/container-visits?container_number=: the live visit that holds the container (404 when none does).
func (h handlers) findContainerVisit(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	number := q.Get("container_number")
	if number == "" || len(q) != 1 {
		httpapi.WriteProblem(w, r, nethttp.StatusBadRequest, "MALFORMED_REQUEST", "name container_number, and nothing else")
		return
	}
	v, err := h.s.FindLiveContainerVisit(r.Context(), api.FindLiveContainerVisit{TenantID: p.TenantID, ContainerNumber: number})
	h.writeVisit(w, r, v, err)
}

// GET /v1/ocean-movements/{movement}/container-visits: the movement's visits in the order they were assigned,
// released ones included. Each has its own revision.
func (h handlers) listContainerVisits(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := reader(w, r)
	if !ok {
		return
	}
	visits, err := h.s.ListContainerVisits(r.Context(), api.ListContainerVisits{TenantID: p.TenantID, MovementID: r.PathValue("movement")})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	out := make([]containerVisit, 0, len(visits))
	for _, v := range visits {
		out = append(out, visitView(v))
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, map[string]any{"container_visits": out})
}

// POST /v1/container-visits/{visit}/seals: RecordContainerSeal; the receipt names the seal (seal_no).
func (h handlers) recordSeal(w nethttp.ResponseWriter, r *nethttp.Request) {
	type body struct {
		Number    string    `json:"number"`
		Party     string    `json:"party"`
		AppliedAt time.Time `json:"applied_at"`
		Source    string    `json:"source"`
		occurred
	}
	change(w, r, func(c command, revision int64, b body) (api.Receipt, error) {
		return h.s.RecordContainerSeal(r.Context(), api.RecordContainerSeal{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			VisitID: r.PathValue("visit"), ExpectedRevision: revision, Number: b.Number, Party: b.Party, AppliedAt: b.AppliedAt, Source: b.Source,
			OccurredAt: c.occurredAt})
	})
}

// POST /v1/container-visits/{visit}/stuffing-done: RecordStuffingDone; difference_reason only when PHY's seal or
// size-type differs from the container's.
func (h handlers) recordStuffingDone(w nethttp.ResponseWriter, r *nethttp.Request) {
	type body struct {
		DifferenceReason string `json:"difference_reason"`
		occurred
	}
	change(w, r, func(c command, revision int64, b body) (api.Receipt, error) {
		return h.s.RecordStuffingDone(r.Context(), api.RecordStuffingDone{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			VisitID: r.PathValue("visit"), ExpectedRevision: revision, DifferenceReason: b.DifferenceReason, OccurredAt: c.occurredAt})
	})
}

// POST /v1/container-visits/{visit}/release: ReleaseContainer, with a reason.
func (h handlers) releaseContainer(w nethttp.ResponseWriter, r *nethttp.Request) {
	type body struct {
		Reason string `json:"reason"`
		occurred
	}
	change(w, r, func(c command, revision int64, b body) (api.Receipt, error) {
		return h.s.ReleaseContainer(r.Context(), api.ReleaseContainer{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			VisitID: r.PathValue("visit"), ExpectedRevision: revision, Reason: b.Reason, OccurredAt: c.occurredAt})
	})
}

// POST /v1/container-visits/{visit}/vgm-assertions: AssertVgm; the receipt names the assertion (assertion_no). The
// signatory's name is taken here and stored apart: no response and no event carries it.
func (h handlers) assertVgm(w nethttp.ResponseWriter, r *nethttp.Request) {
	type body struct {
		Mass                     quantity  `json:"mass"`
		Method                   string    `json:"method"`
		ResponsiblePartyID       string    `json:"responsible_party_id"`
		ResponsiblePartyRevision int64     `json:"responsible_party_revision"`
		DeterminedAt             time.Time `json:"determined_at"`
		LateReason               string    `json:"late_reason"`
		Signatory                string    `json:"signatory"`
		occurred
	}
	change(w, r, func(c command, revision int64, b body) (api.Receipt, error) {
		return h.s.AssertVgm(r.Context(), api.AssertVgm{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID, VisitID: r.PathValue("visit"),
			ExpectedRevision: revision, Mass: b.Mass.kernel(), Method: b.Method, ResponsiblePartyID: b.ResponsiblePartyID,
			ResponsiblePartyRevision: b.ResponsiblePartyRevision, DeterminedAt: b.DeterminedAt, LateReason: b.LateReason, Signatory: b.Signatory,
			OccurredAt: c.occurredAt})
	})
}

// POST /v1/container-visits/{visit}/vgm-response: RecordVgmResponse, the carrier's answer to the latest assertion;
// the receipt names that assertion.
func (h handlers) recordVgmResponse(w nethttp.ResponseWriter, r *nethttp.Request) {
	type body struct {
		Response string `json:"response"`
		Source   string `json:"source"`
		occurred
	}
	change(w, r, func(c command, revision int64, b body) (api.Receipt, error) {
		return h.s.RecordVgmResponse(r.Context(), api.RecordVgmResponse{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			VisitID: r.PathValue("visit"), ExpectedRevision: revision, Response: b.Response, Source: b.Source, OccurredAt: c.occurredAt})
	})
}

// POST /v1/container-visits/{visit}/events: RecordContainerEvent; location is a UN/LOCODE, happened_at when it
// happened. The receipt names the event (event_no): the one recorded, or the one the same source reference recorded
// before.
func (h handlers) recordContainerEvent(w nethttp.ResponseWriter, r *nethttp.Request) {
	type body struct {
		Kind       string    `json:"kind"`
		Location   string    `json:"location"`
		HappenedAt time.Time `json:"happened_at"`
		Source     string    `json:"source"`
		Reference  string    `json:"reference"`
		occurred
	}
	change(w, r, func(c command, revision int64, b body) (api.Receipt, error) {
		return h.s.RecordContainerEvent(r.Context(), api.RecordContainerEvent{CommandID: c.id, TenantID: c.who.TenantID, ActorID: c.who.ActorID,
			VisitID: r.PathValue("visit"), ExpectedRevision: revision, Kind: b.Kind, Location: port(b.Location), HappenedAt: b.HappenedAt,
			Source: b.Source, Reference: b.Reference, OccurredAt: c.occurredAt})
	})
}

// POST /v1/container-visits/{visit}/vgm-assertions/{version}/signatory-reads: ReadVgmSignatory. The body states the
// purpose: CUSTOMS_AUDIT, LEGAL_CLAIM or DATA_SUBJECT_REQUEST. The read needs sea:ReadVgmSignatory and is logged with
// the actor and the purpose; the response is the name.
func (h handlers) readSignatory(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Purpose string `json:"purpose"`
	}
	if !httpapi.Decode(w, r, &body) {
		return
	}
	p, ok := identity.PrincipalFrom(r.Context())
	if !ok {
		httpapi.WriteProblem(w, r, nethttp.StatusUnauthorized, "UNAUTHENTICATED", "")
		return
	}
	version, ok := positive(w, r, "version")
	if !ok {
		return
	}
	s, err := h.s.ReadVgmSignatory(r.Context(), api.ReadVgmSignatory{TenantID: p.TenantID, ActorID: p.ActorID, VisitID: r.PathValue("visit"),
		Version: version, Purpose: body.Purpose})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, map[string]string{"name": s.Name})
}

// GET /v1/container-visits/{visit}/vgm-assertions/{version}/signatory-reads: the logged reads of the assertion's
// signatory, newest first, without the name. It needs sea:ReadVgmSignatoryAccess.
func (h handlers) listSignatoryReads(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, ok := identity.PrincipalFrom(r.Context())
	if !ok {
		httpapi.WriteProblem(w, r, nethttp.StatusUnauthorized, "UNAUTHENTICATED", "")
		return
	}
	version, ok := positive(w, r, "version")
	if !ok {
		return
	}
	reads, err := h.s.ReadVgmSignatoryAccess(r.Context(), api.ReadVgmSignatoryAccess{TenantID: p.TenantID, ActorID: p.ActorID,
		VisitID: r.PathValue("visit"), Version: version})
	if err != nil {
		httpapi.Fail(w, r, kinds, err)
		return
	}
	type read struct {
		AccessID   string    `json:"access_id"`
		ActorID    string    `json:"actor_id"`
		Purpose    string    `json:"purpose"`
		RecordedAt time.Time `json:"recorded_at"`
	}
	out := make([]read, 0, len(reads))
	for _, a := range reads {
		out = append(out, read{AccessID: a.AccessID, ActorID: a.ActorID, Purpose: a.Purpose, RecordedAt: a.RecordedAt})
	}
	httpapi.WriteJSON(w, nethttp.StatusOK, map[string]any{"signatory_reads": out})
}
