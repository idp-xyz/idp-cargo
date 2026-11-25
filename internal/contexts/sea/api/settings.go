package api

import (
	"context"
	"time"
)

// SEA V2.100: SEA's settings (the configuration of the rules, design sections 3 and 6) and the setting S1, "a carrier
// report that contradicts SEA's records" (sea.S1). The commands carry no business time: SEA records its own clock, and
// a change set takes effect at an effective time that is not before the proposal and not more than 366 days after it.

// SeaSettingEntry is one value proposed for one item in one scope: ScopeKind TENANT with no ScopeID, or LEGAL_ENTITY
// with the legal entity's ID. SEA's items take no parameter.
type SeaSettingEntry struct {
	ScopeKind string
	ScopeID   string
	Key       string
	Value     string
}

// ProposeSeaSettingsChange proposes the entries together, with a reason, as the change set ChangeID. The proposal is
// recorded at SEA's clock, never at a business time; EffectiveAt is when it is to take effect, not before now (zero: at
// once) and not more than 366 days after. A change set that only tightens or keeps is approved on the spot
// (Receipt.SettingsVersion is the version it made); one that loosens a value, or sets a legal entity's value looser
// than the company's, waits for ApproveSeaSettingsChange by another actor. The configuration it would make is checked
// as a whole, each legal entity named must be one SEA knows (it has an HBL series, RegisterHblSeries, or a movement),
// and the actor needs the permission ConfigureSettings, granted by name only. Receipt.NewRevision is the records of the
// change set: 1, or 2 when it was approved on the spot. Receipt.Notice names the legal entities that stay looser than
// the company's new value.
type ProposeSeaSettingsChange struct {
	CommandID   string
	TenantID    string
	ActorID     string
	ChangeID    string
	Entries     []SeaSettingEntry
	Reason      string
	EffectiveAt time.Time
}

// ApproveSeaSettingsChange approves a change set that waits for a second person. The approver is not the proposer
// (SETTINGS_SAME_ACTOR: have another approver approve it) and needs the permission ApproveSettings, granted by name
// only. The change set takes effect at its effective time, or at the approval when that is later.
// Receipt.SettingsVersion is the version it made.
type ApproveSeaSettingsChange struct {
	CommandID string
	TenantID  string
	ActorID   string
	ChangeID  string
}

// RefuseSeaSettingsChange is an approver's refusal of a change set that waits for approval, with a reason: it is
// recorded as a cancellation of kind refused, status REFUSED, and cannot be approved. A change set approved already is
// cancelled instead (CancelSeaSettingsChange) while it is not in force. It needs the permission ApproveSettings,
// granted by name only.
type RefuseSeaSettingsChange struct {
	CommandID string
	TenantID  string
	ActorID   string
	ChangeID  string
	Reason    string
}

// CancelSeaSettingsChange cancels a change set that is not in force yet, proposed or approved with a time to come, with
// a reason. One in force cannot be cancelled: a rollback is a new change set. It needs the permission ConfigureSettings
// or the permission ApproveSettings (so that an approver can clear a far-off change set that stands in the way); the
// role it was cancelled in is recorded.
type CancelSeaSettingsChange struct {
	CommandID string
	TenantID  string
	ActorID   string
	ChangeID  string
	Reason    string
}

// SettingsCommands is SEA's command entry point for the settings.
type SettingsCommands interface {
	ProposeSeaSettingsChange(ctx context.Context, cmd ProposeSeaSettingsChange) (Receipt, error)
	ApproveSeaSettingsChange(ctx context.Context, cmd ApproveSeaSettingsChange) (Receipt, error)
	CancelSeaSettingsChange(ctx context.Context, cmd CancelSeaSettingsChange) (Receipt, error)
	RefuseSeaSettingsChange(ctx context.Context, cmd RefuseSeaSettingsChange) (Receipt, error)
}

// Actions of the settings, and the permission the release of a carrier conflict needs under S1 SUPERVISOR_ONLY. The
// three are granted by name only: "sea:*" and "*" grant none of them, so that a general grant never lets a person
// change how SEA's controls work, approve the change or release a carrier's contradiction as a supervisor. That the
// approver is not the proposer is SEA's own rule, not a permission. The subject of ActionSuperviseCarrierConflicts is
// the visit.
const (
	ActionConfigureSettings         = "ConfigureSettings"
	ActionApproveSettings           = "ApproveSettings"
	ActionSuperviseCarrierConflicts = "SuperviseCarrierConflicts"
)

// Settings events have the change set as subject; an approval carries the settings version it made as SettingsVersion.
const (
	SeaSettingsChangeProposed  = "SeaSettingsChangeProposed.v1"
	SeaSettingsChangeApproved  = "SeaSettingsChangeApproved.v1"
	SeaSettingsChangeCancelled = "SeaSettingsChangeCancelled.v1"
	SeaSettingsChangeRefused   = "SeaSettingsChangeRefused.v1"
)

// The item of SEA's catalogue and its values, from strict to loose. S1 is set at the company and at a legal entity (the
// legal entity of the movement the container belongs to). SUPERVISOR_ONLY: the release of a conflict with a carrier's
// loading report also needs the permission SuperviseCarrierConflicts. WITH_REASON, the standard profile's value:
// whoever may release a conflict does so with a reason, as before V2.100.
const (
	SettingS1 = "sea.S1"

	S1SupervisorOnly = "SUPERVISOR_ONLY"
	S1WithReason     = "WITH_REASON"
)

// GetSeaSettings answers SEA's settings in force for a scope (ScopeKind TENANT with no ScopeID, or LEGAL_ENTITY with
// its ID) at AsOf (zero: now), with the version in force. A legal entity's value is its own, else the company's, else
// the default; before any change set is in force the answer is the default, version 0.
type GetSeaSettings struct {
	TenantID  string
	ScopeKind string
	ScopeID   string
	AsOf      time.Time
}

// SeaSetting is the value in force for one item: the scope it is set at (the company's when the default applies),
// whether it is the default, the change set that set it and, for a legal entity, whether it is looser than the
// company's (an approved exception that stays when the company tightens).
type SeaSetting struct {
	Key           string
	Value         string
	FromScopeKind string
	FromScopeID   string
	Default       bool
	ChangeID      string
	Exception     bool
}

// SeaSettings is the settings in force for a scope at a time, with the version in force (0 before any change set).
type SeaSettings struct {
	TenantID  string
	ScopeKind string
	ScopeID   string
	AsOf      time.Time
	Version   int
	Items     []SeaSetting
}

// ListSeaSettingsChanges lists the tenant's change sets in the order proposed.
type ListSeaSettingsChanges struct{ TenantID string }

// SeaSettingEntryView is an entry of a change set as proposed.
type SeaSettingEntryView struct {
	ScopeKind string
	ScopeID   string
	Key       string
	Value     string
}

// Statuses of a change set.
const (
	SettingsProposed  = "PROPOSED"
	SettingsApproved  = "APPROVED"
	SettingsInForce   = "IN_FORCE"
	SettingsCancelled = "CANCELLED"
	SettingsRefused   = "REFUSED"
)

// SeaSettingsChange is a change set with its history: who proposed it, why and when, whether it loosens, who approved
// it, the version it made, when it is or was in force, and who cancelled it, why and in which role (CONFIGURER or
// APPROVER).
type SeaSettingsChange struct {
	ChangeID      string
	Status        string
	Reason        string
	Loosening     bool
	ProposedBy    string
	ProposedAt    time.Time
	EffectiveAt   time.Time
	Entries       []SeaSettingEntryView
	ApprovedBy    string
	ApprovedAt    time.Time
	Version       int
	InForceAt     time.Time
	AutoApproved  bool
	CancelledBy   string
	CancelledAt   time.Time
	CancelReason  string
	CancelledRole string
}

// GetSeaSettingsCatalogue answers the catalogue of SEA's settings.
type GetSeaSettingsCatalogue struct{}

// SeaSettingValue is a value of an item.
type SeaSettingValue struct {
	ID      string
	TitleZH string
	TitleEN string
}

// SeaSettingItem is an item of the catalogue, its values from strict to loose, and the kinds of scope it is set at.
type SeaSettingItem struct {
	Key             string
	TitleZH         string
	TitleEN         string
	Values          []SeaSettingValue
	Default         string
	ScopeKinds      []string
	InternalControl bool
	Explanation     string
}

// SeaSettingsCatalogue is the catalogue, and the same as a Markdown table (for the manual).
type SeaSettingsCatalogue struct {
	Items []SeaSettingItem
	Table string
}

// GetSeaSettingsChangeImpact answers what a change of SEA's settings would change before it takes effect, and changes
// nothing. It names a change set not yet in force (ChangeID: proposed and waiting, or approved with a time to come) or
// the entries someone thinks of proposing (Entries; they are checked as a proposal is, and nothing is written); one of
// the two. It reads the settings in force now and the settings as they would be when the change set takes effect.
type GetSeaSettingsChangeImpact struct {
	TenantID string
	ChangeID string
	Entries  []SeaSettingEntry
}

// Who may release a conflict with a carrier's loading report under a value of S1.
const (
	ReleaseByAnyOperator = "ANY_OPERATOR" // S1 WITH_REASON: whoever holds AcceptCarrierLoadingConflict
	ReleaseBySupervisor  = "SUPERVISOR"   // S1 SUPERVISOR_ONLY: also a holder of SuperviseCarrierConflicts
)

// SeaImpactConflict is an unreleased conflict with a carrier's loading report on a container of an open movement of the
// legal entity, and who could release it before the change and after it.
type SeaImpactConflict struct {
	MovementID      string
	VisitID         string
	ContainerNumber string
	FactNo          int
	ReleaseBefore   string
	ReleaseAfter    string
}

// SeaImpactEntity is a legal entity whose S1 would differ, from what to what, with its open conflicts. The legal
// entities whose S1 would not differ are left out.
type SeaImpactEntity struct {
	LegalEntityID string
	From          string
	To            string
	Conflicts     []SeaImpactConflict
}

// SeaSettingsChangeImpact is the answer: the time the change set would take effect (its effective time, or when it can
// be approved at the earliest, now, when that is later) and the legal entities affected, in the order of their IDs.
type SeaSettingsChangeImpact struct {
	TenantID    string
	ChangeID    string
	TakesEffect time.Time
	Entities    []SeaImpactEntity
}

// SettingsQueries is SEA's read side for the settings.
type SettingsQueries interface {
	GetSeaSettings(ctx context.Context, q GetSeaSettings) (SeaSettings, error)
	ListSeaSettingsChanges(ctx context.Context, q ListSeaSettingsChanges) ([]SeaSettingsChange, error)
	GetSeaSettingsCatalogue(ctx context.Context, q GetSeaSettingsCatalogue) (SeaSettingsCatalogue, error)
	GetSeaSettingsChangeImpact(ctx context.Context, q GetSeaSettingsChangeImpact) (SeaSettingsChangeImpact, error)
}
