package engine

import (
	"encoding/json"
	"time"
)

// Zero time.Time values mean "unset" throughout this package; the store maps them to SQL NULL.

// Stage is one stage definition from pipeline.yaml (docs/design/02-domain-model.md §2.1).
type Stage struct {
	Name          string
	Upstream      string // empty for the first stage; exactly one upstream otherwise (D-008)
	Deploy        DeploySpec
	Gate          []GateStepSpec
	OnGateFailure GatePolicy
	Windows       []string
}

// DeploySpec names the deployer plugin; its config is opaque to the engine.
type DeploySpec struct {
	Plugin string
	Config json.RawMessage
}

// GateStepSpec is one gate step; its config is opaque to the engine.
type GateStepSpec struct {
	Type   string
	Config json.RawMessage
}

// Gate step types (D-004). The engine only treats approval specially: it has no worker.
const (
	StepBake     = "bake"
	StepApproval = "approval"
)

// GatePolicy is a stage's gate-failure policy (R18).
type GatePolicy string

// Gate policies select how a stage handles a failed gate.
const (
	PolicyKeep     GatePolicy = "keep"
	PolicyRollback GatePolicy = "rollback"
)

// StageConfig is the snapshot of a stage's config that a deployment runs on, so a re-apply doesn't change it.
type StageConfig struct {
	PipelineVersion int // the pipeline version the config is from (§14.1)
	Deploy          DeploySpec
	Gate            []GateStepSpec
	OnGateFailure   GatePolicy
}

// Release is immutable. Its identity is commit plus artifact digest set (R5, D-041); releases are ordered by
// (CreatedAt, ID).
type Release struct {
	SourceRepo string // source repository captured when the immutable release is inserted (R32)
	ID         string
	Project    string
	Commit     string
	Version    string // display: the first image's tag
	CreatedAt  time.Time
	Artifacts  []Artifact
}

// Artifact is one image of a release (D-041).
type Artifact struct {
	Image, Tag, Digest string
}

// DeploymentKind says why a deployment exists.
type DeploymentKind string

// Deployment kinds identify the rule that started an attempt.
const (
	KindAuto         DeploymentKind = "auto"
	KindEmergency    DeploymentKind = "emergency"
	KindRollback     DeploymentKind = "rollback"
	KindGateRollback DeploymentKind = "gateRollback"
)

// IsRollback reports whether a gate failure of this deployment must not roll back again (D-009).
func (k DeploymentKind) IsRollback() bool { return k == KindRollback || k == KindGateRollback }

// Phase is a deployment's phase (§2.2). Its terminal phase is its result.
type Phase string

// Deployment phases include active work and terminal results.
const (
	PhaseDeploying    Phase = "deploying"
	PhaseAborting     Phase = "aborting"
	PhaseGating       Phase = "gating"
	PhasePassed       Phase = "passed"
	PhaseDeployFailed Phase = "deployFailed"
	PhaseGateFailed   Phase = "gateFailed"
	PhaseRolledBack   Phase = "rolledBack"
	PhaseCancelled    Phase = "cancelled"
)

// Busy reports whether a deployment in this phase makes its stage busy (§2.2).
func (p Phase) Busy() bool { return p == PhaseDeploying || p == PhaseAborting || p == PhaseGating }

// Terminal reports whether the phase is a result. A terminal deployment never changes (invariant 6).
func (p Phase) Terminal() bool {
	switch p {
	case PhasePassed, PhaseDeployFailed, PhaseGateFailed, PhaseRolledBack, PhaseCancelled:
		return true
	}
	return false
}

// TookTraffic reports whether a deployment in this phase made its release live (§2.4).
func (p Phase) TookTraffic() bool {
	switch p {
	case PhaseGating, PhasePassed, PhaseGateFailed, PhaseRolledBack, PhaseCancelled:
		return true
	}
	return false
}

// Failed reports whether promotion must never redeploy this deployment's release automatically (invariant 4).
func (p Phase) Failed() bool {
	switch p {
	case PhaseDeployFailed, PhaseGateFailed, PhaseRolledBack, PhaseCancelled:
		return true
	}
	return false
}

// Deployment is one attempt to put one release into one stage, including its gate (§2.1).
type Deployment struct {
	ID               string // "<stage>-<n>" (§3.2)
	Stage            string
	Release          string
	Kind             DeploymentKind
	PreviousRelease  string // current(stage) when it was created; empty on a stage's first deployment
	Actor            string
	Reason           string
	Config           StageConfig
	Phase            Phase
	ResultReason     string // the result's reason; while aborting, the abort's reason (§3.6)
	Steps            []GateRun
	CreatedAt        time.Time
	StartedAt        time.Time
	EndedAt          time.Time
	AbortRequestedAt time.Time
	StuckNotified    []Phase // phases for which deploymentStuck was sent
}

// StepState is the state of one gate step.
type StepState string

// Gate step states track work from scheduling through completion.
const (
	StepPending   StepState = "pending"
	StepWaiting   StepState = "waiting" // approval
	StepRunning   StepState = "running"
	StepPassed    StepState = "passed"
	StepFailed    StepState = "failed"
	StepCancelled StepState = "cancelled"
)

// GateRun is one step of a deployment's gate.
type GateRun struct {
	Index     int
	Type      string
	State     StepState
	StartedAt time.Time
	EndedAt   time.Time
	Actor     string // approval
	Reason    string
}

// Dimension is one of a stage's two switches (R21).
type Dimension string

// Hold dimensions control deployment and promotion independently.
const (
	DimDeploy  Dimension = "deploy"
	DimPromote Dimension = "promote"
)

// HoldSource says what added a hold (R24).
type HoldSource string

// Hold sources identify the rule or user action that added a hold.
const (
	SourceManual       HoldSource = "manual"
	SourceManualDeploy HoldSource = "manualDeploy"
	SourceGateRollback HoldSource = "gateRollback"
)

// Hold is one reason a stage's switch is on Hold. Holds stack (R24).
type Hold struct {
	ID          string // "<stage>-h<n>" (§3.2)
	Stage       string
	Dimension   Dimension
	Source      HoldSource
	Reason      string
	Actor       string
	Deployment  string // for manualDeploy and gateRollback holds
	CreatedAt   time.Time
	ExpiresAt   time.Time
	ClearedAt   time.Time
	ClearedBy   string
	ClearReason string
}

// ActiveAt reports whether the hold holds at now (§2.4).
func (h Hold) ActiveAt(now time.Time) bool {
	return h.ClearedAt.IsZero() && (h.ExpiresAt.IsZero() || now.Before(h.ExpiresAt))
}

// Outcome is the phase of a release's latest deployment in a stage, as the release view shows it (§2.3, D-027).
// There is no aborting outcome: an aborting deployment's release shows as deploying, with the abort in its reason.
type Outcome string

// Release outcomes reflect the latest deployment result in a stage.
const (
	OutcomeNone         Outcome = "" // the release was never deployed in the stage
	OutcomeDeploying    Outcome = "deploying"
	OutcomeGating       Outcome = "gating"
	OutcomePassed       Outcome = "passed"
	OutcomeDeployFailed Outcome = "deployFailed"
	OutcomeGateFailed   Outcome = "gateFailed"
	OutcomeRolledBack   Outcome = "rolledBack"
	OutcomeCancelled    Outcome = "cancelled"
)

// outcomeOf is the outcome that mirrors a deployment phase.
func outcomeOf(p Phase) Outcome {
	if p == PhaseAborting {
		return OutcomeDeploying
	}
	return Outcome(p)
}

// Candidacy says whether a release is the stage's candidate for another deployment (§2.3, D-027).
type Candidacy string

// Candidacy states track whether a release is waiting for another deployment.
const (
	CandidacyNone       Candidacy = ""
	CandidacyPending    Candidacy = "pending"    // the candidate, but blocked; at most one per stage
	CandidacySuperseded Candidacy = "superseded" // was pending, and stopped being the candidate before it deployed
)

// ReleaseStage is a release's state in a stage, for the release view (R6). It keeps two independent facts (D-027):
// the outcome, which only deployments change, and the candidacy, which promotion changes and a deployment of the
// release ends.
type ReleaseStage struct {
	Release         string
	Stage           string
	Outcome         Outcome
	Reason          string // the outcome's reason
	Deployment      string // the deployment the outcome mirrors
	Candidacy       Candidacy
	CandidacyReason string
	UpdatedAt       time.Time
}

// Counters hold a stage's deterministic id sequences (§3.2). They outlive stage definitions.
type Counters struct {
	Deployments int
	Holds       int
}

// DefaultStuckAfter is when deploymentStuck fires if State.StuckAfter is unset (§3.2, D-010).
const DefaultStuckAfter = 30 * time.Minute

// State is a project's working set (§3, docs/design/05-storage.md §5.3). It has value semantics: Handle and Apply
// never modify the State they are given.
type State struct {
	Project         string
	StuckAfter      time.Duration       // engine setting (--stuck-after), not persisted; zero means DefaultStuckAfter
	PipelineVersion int                 // the pipeline version Stages are from (§14.1); 0 before the first
	Stages          []Stage             // topological order
	Releases        []Release           // ordered by (CreatedAt, ID)
	Deployments     []Deployment        // creation order
	Holds           []Hold              // uncleared holds, creation order
	ReleaseStages   []ReleaseStage      // one per (release, stage), first-touch order
	Counters        map[string]Counters // by stage name
	Windows         []Window            // the windows the stages reference, sorted by name; no transition changes them (§12.3)
}
