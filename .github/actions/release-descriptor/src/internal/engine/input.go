package engine

import "time"

// Input is one event for Handle (§3.1).
type Input interface {
	Meta() InputMeta
}

// InputMeta is common to every input.
type InputMeta struct {
	ID    string // idempotency key
	Actor string // "user:<name>", "system:ingest", "system:worker" or "system:scheduler"
	// Expect is a command's preview digest (D-056): set, the command is handled only if PreviewDigest still gives it.
	Expect string
}

// Meta returns the input's id and actor.
func (m InputMeta) Meta() InputMeta { return m }

// Actors that are not people.
const (
	ActorIngest    = "system:ingest"
	ActorWorker    = "system:worker"
	ActorScheduler = "system:scheduler"
	ActorExpiry    = "system:expiry" // HoldCleared by expiry
)

// PipelineChanged carries new stage definitions from `project apply`: those of pipeline version Version
// (docs/design/14-config-versions.md §14.1).
type PipelineChanged struct {
	InputMeta
	Version int
	Stages  []Stage
}

// NewRelease is a release that ingest completed.
type NewRelease struct {
	InputMeta
	Release Release
}

// Live says which release serves after an abort.
type Live string

// Abort results identify the release that is verifiably serving.
const (
	LivePrevious Live = "previous"
	LiveNew      Live = "new"
)

// DeploymentFinished is the deployer's result. Aborted and Live are set only when it reports an abort (D-020).
type DeploymentFinished struct {
	InputMeta
	Deployment string
	OK         bool
	Reason     string
	Aborted    bool
	Live       Live
}

// GateStepFinished is a gate step plugin's result.
type GateStepFinished struct {
	InputMeta
	Deployment string
	Index      int
	OK         bool
	Reason     string
}

// Approve passes the approval step that Deployment is waiting at.
type Approve struct {
	InputMeta
	Stage      string
	Deployment string // the one the person looked at
	Step       *int   // optional index the person inspected (D-046)
	Reason     string
}

// Reject fails the approval step that Deployment is waiting at.
type Reject struct {
	InputMeta
	Stage      string
	Deployment string // the one the person looked at
	Step       *int   // optional index the person inspected (D-046)
	Reason     string
}

// AddHold puts one or both of a stage's switches on Hold (R22).
type AddHold struct {
	InputMeta
	Stage      string
	Dimensions []Dimension
	Reason     string
	ExpiresAt  time.Time // optional
}

// ClearHold clears one hold by id, or every active hold of Dimension on Stage, of any source.
type ClearHold struct {
	InputMeta
	Hold      string
	Stage     string
	Dimension Dimension
	Reason    string
}

// ManualDeploy deploys a chosen release to a stage (R26).
type ManualDeploy struct {
	InputMeta
	Stage   string
	Release string
	Reason  string
}

// Abort aborts the stage's active deployment (D-020).
type Abort struct {
	InputMeta
	Stage      string
	Deployment string // the one the person looked at
	Reason     string
}

// Tick triggers a re-evaluation for expiries and windows.
type Tick struct {
	InputMeta
}
