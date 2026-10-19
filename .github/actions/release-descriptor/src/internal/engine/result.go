package engine

import "time"

// Result is what Handle decided (§3).
type Result struct {
	Transitions []Transition // state changes, in order; also the history and audit record
	Actions     []Action     // work for plugin workers
	WakeAt      *time.Time   // earliest time a re-evaluation could change something
	Rejected    *Rejection   // set when a command input is invalid; then Transitions and Actions are empty
	State       State        // the working state after the input; equals s.Apply(Transitions...)
}

// Rejection is why a command changed nothing (§3.9).
type Rejection struct {
	Reason string
	Stale  bool // the command's preview digest no longer matched (D-056)
}

// Transition is one state change (§3.2). Apply folds transitions into a State.
type Transition interface {
	isTransition()
}

// StagesChanged replaces the stage definitions with those of pipeline version Version.
type StagesChanged struct {
	Version int
	Stages  []Stage
}

// ReleaseAdded adds a new release.
type ReleaseAdded struct {
	Release Release
}

// DeploymentCreated adds a deployment in phase deploying and advances its stage's deployment counter.
type DeploymentCreated struct {
	Deployment Deployment
}

// DeploymentPhaseChanged moves a deployment to a new phase. Reason becomes the result reason on a terminal phase.
type DeploymentPhaseChanged struct {
	Deployment string
	Stage      string
	Phase      Phase
	Reason     string
	At         time.Time
}

// DeploymentStuck records that deploymentStuck was sent for the deployment's current phase, so it is sent at most
// once per deployment per phase (§3.2, D-010).
type DeploymentStuck struct {
	Deployment string
	Stage      string
	Phase      Phase
	At         time.Time
}

// GateStepChanged changes the state of one gate step.
type GateStepChanged struct {
	Deployment string
	Stage      string
	Index      int
	State      StepState
	Actor      string
	Reason     string
	At         time.Time
}

// HoldAdded adds a hold and advances its stage's hold counter.
type HoldAdded struct {
	Hold Hold
}

// HoldCleared clears a hold. ClearedBy is a user or ActorExpiry.
type HoldCleared struct {
	Hold      string
	Stage     string
	ClearedBy string
	Reason    string
	At        time.Time
}

// OutcomeChanged sets a release's outcome in a stage to its latest deployment's phase (D-027). It never changes the
// candidacy.
type OutcomeChanged struct {
	Release    string
	Stage      string
	Outcome    Outcome
	Reason     string
	Deployment string
	At         time.Time
}

// CandidacyChanged sets a release's candidacy in a stage (D-027). It never changes the outcome.
type CandidacyChanged struct {
	Release   string
	Stage     string
	Candidacy Candidacy
	Reason    string
	At        time.Time
}

func (StagesChanged) isTransition()          {}
func (ReleaseAdded) isTransition()           {}
func (DeploymentCreated) isTransition()      {}
func (DeploymentPhaseChanged) isTransition() {}
func (DeploymentStuck) isTransition()        {}
func (GateStepChanged) isTransition()        {}
func (HoldAdded) isTransition()              {}
func (HoldCleared) isTransition()            {}
func (OutcomeChanged) isTransition()         {}
func (CandidacyChanged) isTransition()       {}

// Action is work for a plugin worker. Its id is deterministic, so the outbox ignores duplicates (§3.2).
type Action interface {
	ActionID() string
}

// StartDeployment asks the deployer named in the deployment's snapshot to deploy. Id "<deployment>/deploy".
type StartDeployment struct {
	ID         string
	Deployment string
	Stage      string
	Release    string
}

// StartGateStep asks a gate step plugin to run step Index. Id "<deployment>/gate/<i>". Never sent for approval.
type StartGateStep struct {
	ID         string
	Deployment string
	Stage      string
	Index      int
	Type       string
}

// CancelGateStep stops a running step. Id "<deployment>/gate/<i>/cancel".
type CancelGateStep struct {
	ID         string
	Deployment string
	Stage      string
	Index      int
	Type       string
}

// AbortDeployment asks the deployer to stop a running sync and restore what was serving (D-020). Id
// "<deployment>/abort". Inserting it cancels the deployment's "<deployment>/deploy" action in the same transaction
// (docs/design/06-runtime.md §6.1).
type AbortDeployment struct {
	ID         string
	Deployment string
	Stage      string
}

// Notification is the kind of a Notify action (§3.2).
type Notification string

// Notification kinds identify the event to deliver.
const (
	NotifyEmergencyDeployStarted Notification = "emergencyDeployStarted"
	NotifyRollbackStarted        Notification = "rollbackStarted"
	NotifyGateRollbackStarted    Notification = "gateRollbackStarted"
	NotifyRollbackFailed         Notification = "rollbackFailed"
	NotifyDeployFailed           Notification = "deployFailed"
	NotifyGateFailed             Notification = "gateFailed"
	NotifyHoldAdded              Notification = "holdAdded"
	NotifyHoldCleared            Notification = "holdCleared"
	NotifyApprovalWaiting        Notification = "approvalWaiting"
	NotifyDeploymentAborted      Notification = "deploymentAborted"
	NotifyDeploymentStuck        Notification = "deploymentStuck"
)

// Urgency of a notification.
type Urgency string

// Notification urgencies select the delivery priority.
const (
	UrgencyNormal Urgency = "normal"
	UrgencyHigh   Urgency = "high"
	UrgencyUrgent Urgency = "urgent"
)

// Notify is a notification, sent in the same transaction as the transition that caused it (principle 7, R34).
type Notify struct {
	// Metadata is snapshotted when the engine emits the notification (§15.2).
	Actor, Reason string
	Phase         Phase
	Duration      time.Duration
	Index         int
	Dimension     Dimension
	ID            string
	Notification  Notification
	Urgency       Urgency
	Stage         string
	Release       string
	Deployment    string
	Hold          string
	Message       string
}

// ActionID returns the stable idempotency key for this action.
func (a StartDeployment) ActionID() string { return a.ID }

// ActionID returns the stable idempotency key for this action.
func (a StartGateStep) ActionID() string { return a.ID }

// ActionID returns the stable idempotency key for this action.
func (a CancelGateStep) ActionID() string { return a.ID }

// ActionID returns the stable idempotency key for this action.
func (a AbortDeployment) ActionID() string { return a.ID }

// ActionID returns the stable idempotency key for this action.
func (a Notify) ActionID() string { return a.ID }
