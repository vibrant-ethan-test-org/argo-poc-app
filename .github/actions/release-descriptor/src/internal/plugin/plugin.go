package plugin

import (
	"context"
	"encoding/json"
	"time"
)

// Validator checks plugin config when a pipeline is applied, so bad config fails at `project apply`,
// not at deploy time. Its error must not quote the config: the API returns it and stores it in the apply's audit
// row (principle 18, docs/design/14-config-versions.md §14.2).
type Validator interface {
	Validate(cfg json.RawMessage) error
}

// Deployer starts, observes and aborts deployments on a target.
type Deployer interface {
	Validator
	// Start begins deploying. It must be idempotent for the same req.DeploymentID.
	Start(ctx context.Context, req DeployRequest) (Progress, error)
	// Poll reports progress. It is called until it returns a terminal result.
	Poll(ctx context.Context, req DeployRequest, p Progress) (Status, Progress, error)
	// Abort stops an in-flight deployment and restores what was serving (D-020). Like Poll, it is called
	// repeatedly with its saved progress until Status.Done, and it must be idempotent. When done,
	// Status.Live says which release is verifiably serving.
	Abort(ctx context.Context, req DeployRequest, p Progress) (Status, Progress, error)
	// ExclusiveKey identifies the target (for argocd: "<appNamespace>/<app>"). Config validation rejects two
	// stages, in any project, with the same key, so one target can't be driven by two stages.
	ExclusiveKey(cfg json.RawMessage) string
	// Health reports the live state of the stage's target, for gate steps such as bake.
	Health(ctx context.Context, cfg json.RawMessage) (Health, error)
}

// Linker is an optional Deployer interface: human links for a stage's target, for the UI and Slack
// (docs/design/17-ui.md §17.6). URLs come only from the plugin's configured base URL plus the stage's validated
// config; an invalid config or an unset base URL returns none.
type Linker interface {
	Links(cfg json.RawMessage) []Link
}

// Link is one human link of a stage's target.
type Link struct{ Title, URL string }

// GateStep runs and observes one deployment gate step.
type GateStep interface {
	Validator
	Start(ctx context.Context, req StepRequest) (Progress, error)
	Poll(ctx context.Context, req StepRequest, p Progress) (Status, Progress, error)
	Cancel(ctx context.Context, req StepRequest, p Progress) error
}

// DeployRequest carries the immutable deployment payload and target configuration.
type DeployRequest struct {
	Project, Stage, DeploymentID string
	ExternalID                   string          // durable operation correlation id; empty uses DeploymentID for legacy callers
	Release                      Release         // version, commit, artifacts (image, tag, digest)
	PreviousRelease              *Release        // what served before this deployment; nil for a stage's first deployment
	Config                       json.RawMessage // stage.deploy.<plugin> from the snapshot
}

// StepRequest carries one gate step and its deployment context.
type StepRequest struct {
	Project, Stage, DeploymentID string
	Index                        int
	Release                      Release
	Config                       json.RawMessage
	StartedAt                    time.Time // durable
	Deployer                     Deployer  // the stage's deployer, for Health
	DeployCfg                    json.RawMessage
}

// Status reports the result or next poll of a plugin operation.
type Status struct {
	Done   bool
	OK     bool
	Reason string        // shown in history and the release view
	Next   time.Duration // when to poll again, if not done
	Live   Live          // Abort only: which release is verifiably serving when Done
}

// Live identifies which release serves after an abort completes.
type Live string

// Abort outcomes describe the verifiably serving release.
const (
	LivePrevious Live = "previous" // the abort worked: the previous release (or nothing, on a first deployment) serves
	LiveNew      Live = "new"      // too late: the new release already serves; the abort applies to the gate
	// There is no "unknown": while the live release can't be verified, Abort returns Done=false and keeps
	// retrying (no timeout, D-010; deploymentStuck still fires).
)

// Progress is opaque plugin state persisted between worker calls.
type Progress json.RawMessage // saved in action.progress between calls

// Release is the deployment payload described in docs/design/07-plugins.md §7.1.
type Release struct {
	Version, Commit string
	Artifacts       []Artifact
}

// Artifact identifies an immutable image build.
type Artifact struct{ Image, Tag, Digest string }

// Health reports target health and the live release, identified by the
// promotion.vibrantsci.com/release annotation on the active Service's pods.
type Health struct {
	Healthy         bool
	Degraded        bool // explicit failure; Progressing may be neither Healthy nor Degraded
	Release, Reason string
}
