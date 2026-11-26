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
//
// OnboardingID (V2.102, batch F) holds the change set for that onboarding: it is not approved when it is proposed, not even when it
// only tightens, its approver is never its proposer, and until it is approved or cancelled no other change set of the tenant is
// proposed or approved (SETTINGS_ONBOARDING_WAITING: confirm the onboarding, or withdraw it). It is the platform's onboarding that
// holds one, in the consultant's own name: the action is ActionProposeHeldSettings, granted only with the platform's mark besides
// ConfigureSettings by name, so that no one else can freeze a tenant's settings with a made-up onboarding.
type ProposeSeaSettingsChange struct {
	CommandID    string
	TenantID     string
	ActorID      string
	ChangeID     string
	Entries      []SeaSettingEntry
	Reason       string
	EffectiveAt  time.Time
	OnboardingID string `json:",omitempty"`
}

// ApproveSeaSettingsChange approves a change set that waits for a second person. The approver is not the proposer
// (SETTINGS_SAME_ACTOR: have another approver approve it) and needs the permission ApproveSettings, granted by name
// only. The change set takes effect at its effective time, or at the approval when that is later.
// Receipt.SettingsVersion is the version it made.
//
// A tenant whose onboarding declared a single administrator (DeclareSeaSingleAdministrator) may have a loosening
// approved by its own proposer, but only with a written risk acknowledgement of 1 to 2000 characters in
// RiskAcknowledgement (SETTINGS_ACKNOWLEDGEMENT: give it); the acknowledgement is stored with the approval and listed in
// the exception report. Another approver gives none, and without the declaration the proposer is refused as before.
type ApproveSeaSettingsChange struct {
	CommandID           string
	TenantID            string
	ActorID             string
	ChangeID            string
	RiskAcknowledgement string `json:",omitempty"`
}

// DeclareSeaSingleAdministrator records that the tenant declared a single administrator in the onboarding OnboardingID,
// proposed by ProposedBy and confirmed by ActorID, the customer's owner (two different people), who named AdministratorID
// as the one administrator (not the consultant ProposedBy). Only that administrator may approve their own loosening. It is the platform's
// onboarding that calls it, once the owner has confirmed; it needs the permission DeclareSingleAdministrator, granted by
// name only and not by ConfigureSettings, so that a configurer alone cannot make it. It is recorded once for each
// onboarding, never withdrawn, and idempotent by command id. Receipt.ChangeID is the onboarding.
type DeclareSeaSingleAdministrator struct {
	CommandID       string
	TenantID        string
	ActorID         string
	OnboardingID    string
	ProposedBy      string
	AdministratorID string
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

// ReviewSeaSelfApprovedSettingsChange records the review of a change set that its own proposer approved as the tenant's single
// administrator (V2.102, batch F), one by one: Conclusion is ACCEPTED or NOT_ACCEPTED, and a NOT_ACCEPTED has a Note (1 to 2000
// characters; given for ACCEPTED too, it has the same length). The reviewer is never the person who approved it, needs the
// permission ReviewSettingsExceptions, granted by name only, and reviews a change set once: a review is immutable and replayed by its
// command id. A NOT_ACCEPTED changes no setting: undoing the change is a normal change set (ProposeSeaSettingsChange). Receipt.SubjectID
// is the change set.
type ReviewSeaSelfApprovedSettingsChange struct {
	CommandID  string
	TenantID   string
	ActorID    string
	ChangeID   string
	Conclusion string
	Note       string `json:",omitempty"`
}

// SettingsCommands is SEA's command entry point for the settings.
type SettingsCommands interface {
	ProposeSeaSettingsChange(ctx context.Context, cmd ProposeSeaSettingsChange) (Receipt, error)
	ApproveSeaSettingsChange(ctx context.Context, cmd ApproveSeaSettingsChange) (Receipt, error)
	CancelSeaSettingsChange(ctx context.Context, cmd CancelSeaSettingsChange) (Receipt, error)
	RefuseSeaSettingsChange(ctx context.Context, cmd RefuseSeaSettingsChange) (Receipt, error)
	DeclareSeaSingleAdministrator(ctx context.Context, cmd DeclareSeaSingleAdministrator) (Receipt, error)
	ReviewSeaSelfApprovedSettingsChange(ctx context.Context, cmd ReviewSeaSelfApprovedSettingsChange) (Receipt, error)
}

// Actions of the settings, and the permission the release of a carrier conflict needs under S1 SUPERVISOR_ONLY. The
// three are granted by name only: "sea:*" and "*" grant none of them, so that a general grant never lets a person
// change how SEA's controls work, approve the change or release a carrier's contradiction as a supervisor. That the
// approver is not the proposer is SEA's own rule, not a permission. The subject of ActionSuperviseCarrierConflicts
// is the visit.
const (
	ActionConfigureSettings         = "ConfigureSettings"
	ActionApproveSettings           = "ApproveSettings"
	ActionSuperviseCarrierConflicts = "SuperviseCarrierConflicts"
	// ActionDeclareSingleAdministrator (V2.102) is granted by name only too; the platform's onboarding holds it.
	ActionDeclareSingleAdministrator = "DeclareSingleAdministrator"
	// ActionProposeHeldSettings and ActionApproveHeldSettings (V2.102, batch F) are the platform's onboarding's too: it proposes a change set held
	// for an onboarding in the consultant's own name, and approves it in the owner's own name. Each needs the platform's mark, besides
	// ConfigureSettings (proposing) or ApproveSettings (approving) by name, which no token carries.
	ActionProposeHeldSettings = "ProposeHeldSettings"
	ActionApproveHeldSettings = "ApproveHeldSettings"
	// ActionReviewSettingsExceptions (V2.102, batch F) reviews a single administrator's self-approval, and reads the reviews and what still
	// waits for one. It is granted by name only: "sea:*" and "*" do not include it.
	ActionReviewSettingsExceptions = "ReviewSettingsExceptions"
)

// Settings events have the change set as subject; an approval carries the settings version it made as SettingsVersion.
const (
	SeaSettingsChangeProposed  = "SeaSettingsChangeProposed.v1"
	SeaSettingsChangeApproved  = "SeaSettingsChangeApproved.v1"
	SeaSettingsChangeCancelled = "SeaSettingsChangeCancelled.v1"
	SeaSettingsChangeRefused   = "SeaSettingsChangeRefused.v1"
	// SeaSingleAdministratorDeclared has the onboarding as subject.
	SeaSingleAdministratorDeclared = "SeaSingleAdministratorDeclared.v1"
	// SeaSelfApprovalReviewed has the change set whose self-approval was reviewed as subject.
	SeaSelfApprovalReviewed = "SeaSelfApprovalReviewed.v1"
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

// SeaSettingsChange is a change set with its history: who proposed it, why and when, whether it loosens, who
// approved it, the version it made, when it is or was in force, and who cancelled it, why and in which role
// (CONFIGURER or APPROVER).
type SeaSettingsChange struct {
	ChangeID     string
	Status       string
	Reason       string
	Loosening    bool
	ProposedBy   string
	ProposedAt   time.Time
	EffectiveAt  time.Time
	Entries      []SeaSettingEntryView
	ApprovedBy   string
	ApprovedAt   time.Time
	Version      int
	InForceAt    time.Time
	AutoApproved bool
	// RiskAcknowledgement is the written acknowledgement of a single administrator who approved their own loosening, and
	// BasisOnboardingID the onboarding of the declaration of the single administrator that approval relies on (V2.102, batch F).
	RiskAcknowledgement string
	BasisOnboardingID   string
	// HeldForOnboarding is the onboarding the change set is held for, empty for an ordinary change set (V2.102, batch F).
	HeldForOnboarding string
	CancelledBy       string
	CancelledAt       time.Time
	CancelReason      string
	CancelledRole     string
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
	// Version names the catalogue's content (its items, their values in order and defaults, scopes and controls).
	Version string
	Items   []SeaSettingItem
	Table   string
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

// GetSeaExceptionReport is SEA's part of the exception report of a tenant for the period From (included) to To
// (excluded): the approved legal-entity exceptions looser than the company and the items in force now looser than the
// standard profile, with who proposed and who approved each, the loosenings that a single administrator approved
// themselves with their acknowledgements and the declarations, and the actors who proposed or approved a change set in
// the period. Read-only; the actor needs the permission ApproveSettings, granted by name only.
type GetSeaExceptionReport struct {
	TenantID string
	ActorID  string
	From     time.Time
	To       time.Time
}

// SeaExceptionLine is an entry of a scope looser than the company's value (Exception) or than the standard profile's.
type SeaExceptionLine struct {
	ScopeKind  string
	ScopeID    string
	Key        string
	Value      string
	ThanValue  string // the company's value for an exception, the standard profile's for a looser item
	ChangeID   string
	ProposedBy string
	ApprovedBy string
}

// SeaSelfApproval is a loosening that its own proposer approved as the single administrator, with the declaration it relies on (its
// onboarding, the administrator it names and when it was declared: V2.102, batch F).
type SeaSelfApproval struct {
	ChangeID           string
	ApprovedBy         string
	ApprovedAt         time.Time
	Acknowledgement    string
	BasisOnboardingID  string
	BasisAdministrator string
	BasisDeclaredAt    time.Time
	Entries            []SeaSettingEntryView
}

// SeaSettingsChanger is an actor who proposed or approved change sets in the period.
type SeaSettingsChanger struct {
	ActorID  string
	Proposed int
	Approved int
}

// SeaSingleAdministratorDeclaration is a declaration of a single administrator.
type SeaSingleAdministratorDeclaration struct {
	OnboardingID    string
	ProposedBy      string
	ConfirmedBy     string
	AdministratorID string
	DeclaredAt      time.Time
}

// SeaExceptionReport is SEA's part of the exception report.
type SeaExceptionReport struct {
	TenantID           string
	From               time.Time
	To                 time.Time
	AsOf               time.Time
	Exceptions         []SeaExceptionLine
	LooserThanStandard []SeaExceptionLine
	SelfApproved       []SeaSelfApproval
	Declarations       []SeaSingleAdministratorDeclaration
	Changers           []SeaSettingsChanger
}

// SettingsQueries is SEA's read side for the settings.
type SettingsQueries interface {
	GetSeaExceptionReport(ctx context.Context, q GetSeaExceptionReport) (SeaExceptionReport, error)
	GetSeaSettings(ctx context.Context, q GetSeaSettings) (SeaSettings, error)
	ListSeaSettingsChanges(ctx context.Context, q ListSeaSettingsChanges) ([]SeaSettingsChange, error)
	GetSeaSettingsCatalogue(ctx context.Context, q GetSeaSettingsCatalogue) (SeaSettingsCatalogue, error)
	GetSeaSettingsChangeImpact(ctx context.Context, q GetSeaSettingsChangeImpact) (SeaSettingsChangeImpact, error)
	ListSeaSelfApprovalReviews(ctx context.Context, q ListSeaSelfApprovalReviews) (SeaSelfApprovalReviews, error)
}

// ListSeaSelfApprovalReviews answers, for a period of approvals (From included, To excluded), the self-approved change sets that no
// one has reviewed and the reviewed ones with their reviewer, conclusion and note (SEA V2.102, batch F). It is read-only. The actor
// needs ReviewSettingsExceptions, granted by name only.
type ListSeaSelfApprovalReviews struct {
	TenantID string
	ActorID  string
	From     time.Time
	To       time.Time
}

// SeaSelfApprovalReview is a self-approved change set with its review.
type SeaSelfApprovalReview struct {
	SeaSelfApproval
	ReviewedBy string
	ReviewedAt time.Time
	Conclusion string
	Note       string
}

// SeaSelfApprovalReviews is the self-approved change sets of a period: Pending have no review.
type SeaSelfApprovalReviews struct {
	TenantID string
	From     time.Time
	To       time.Time
	AsOf     time.Time
	Pending  []SeaSelfApproval
	Reviewed []SeaSelfApprovalReview
}
