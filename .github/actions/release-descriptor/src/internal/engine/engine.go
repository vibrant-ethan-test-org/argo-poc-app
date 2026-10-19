package engine

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Handle applies one input to a project's state and re-evaluates every stage (§3.3). It is pure: the same state,
// input and time always produce the same Result, and s is never modified (principle 8).
func Handle(s State, in Input, now time.Time) Result {
	r := &run{w: s.Clone(), in: in, now: now}
	if reason := r.validate(); reason != "" {
		return Result{Rejected: &Rejection{Reason: reason}, State: r.w}
	}
	r.record()
	r.expireHolds()
	r.checkStuck()
	for _, st := range slices.Clone(r.w.Stages) { // upstream first, so one input can move a release several stages
		r.reconcile(st)
	}
	res := Result{Transitions: r.ts, Actions: r.as, WakeAt: r.nextWake(), State: r.w}
	// D-056: a command that names its preview's digest is handled only if handling it now gives that preview. A
	// validation rejection above keeps its own, more specific reason.
	if expect := in.Meta().Expect; expect != "" && PreviewDigest(s, in, res) != expect {
		return Result{Rejected: &Rejection{Reason: StaleReason, Stale: true}, State: s.Clone()}
	}
	return res
}

// run is one evaluation. Every emit applies to w immediately, so later rules see earlier changes.
type run struct {
	w   State
	in  Input
	now time.Time
	ts  []Transition
	as  []Action
}

func (r *run) emit(t Transition) {
	r.w.apply(t)
	r.ts = append(r.ts, t)
}

func (r *run) act(a Action) { r.as = append(r.as, a) }

// validate returns why a command input is rejected (§3.9), or "".
func (r *run) validate() string {
	switch in := r.in.(type) {
	case NewRelease:
		if in.Release.Project != r.w.Project {
			return fmt.Sprintf("release %s belongs to project %q, not %q", in.Release.ID, in.Release.Project, r.w.Project) // invariant 7
		}
	case AddHold:
		if _, ok := r.w.Stage(in.Stage); !ok {
			return fmt.Sprintf("unknown stage %q", in.Stage)
		}
		if len(in.Dimensions) == 0 {
			return "no hold dimension given"
		}
		for _, d := range in.Dimensions {
			if d != DimDeploy && d != DimPromote {
				return fmt.Sprintf("unknown hold dimension %q", d)
			}
		}
		if !in.ExpiresAt.IsZero() && !in.ExpiresAt.After(r.now) {
			return fmt.Sprintf("hold expiry %s is in the past", in.ExpiresAt.Format(time.RFC3339))
		}
	case ClearHold:
		if in.Hold != "" {
			for _, h := range r.w.Holds {
				if h.ID == in.Hold {
					return ""
				}
			}
			return fmt.Sprintf("hold %q does not exist or is already cleared", in.Hold)
		}
		if _, ok := r.w.Stage(in.Stage); !ok {
			return fmt.Sprintf("unknown stage %q", in.Stage)
		}
		if in.Dimension != DimDeploy && in.Dimension != DimPromote {
			return fmt.Sprintf("unknown hold dimension %q", in.Dimension)
		}
	case ManualDeploy:
		// R27: holds and the upstream are not checked.
		if _, ok := r.w.Stage(in.Stage); !ok {
			return fmt.Sprintf("unknown stage %q", in.Stage)
		}
		if _, ok := r.w.Release(in.Release); !ok {
			return fmt.Sprintf("unknown release %q", in.Release)
		}
		if d, ok := r.w.Active(in.Stage); ok {
			switch d.Phase { // D-007: there is no queue
			case PhaseDeploying:
				return fmt.Sprintf("stage is deploying %s; abort it or try again when it finishes", r.w.version(d.Release))
			case PhaseAborting:
				return fmt.Sprintf("stage is aborting %s; try again when the abort finishes", r.w.version(d.Release))
			}
		}
	case Abort:
		if _, ok := r.w.Stage(in.Stage); !ok {
			return fmt.Sprintf("unknown stage %q", in.Stage)
		}
		// A stale screen can't abort the wrong deployment (§3.9).
		if d, ok := r.w.Active(in.Stage); !ok || d.ID != in.Deployment {
			return fmt.Sprintf("deployment %q is not the active deployment of stage %s", in.Deployment, in.Stage)
		}
	case Approve:
		return r.validateApproval(in.Stage, in.Deployment, in.Step)
	case Reject:
		if reason := r.validateApproval(in.Stage, in.Deployment, in.Step); reason != "" {
			return reason
		}
		if strings.TrimSpace(in.Reason) == "" {
			return "a reason is required to reject" // R33
		}
	}
	return ""
}

// validateApproval protects the deployment the caller actually inspected (D-014, §13.2).
func (r *run) validateApproval(stage, deployment string, step *int) string {
	if _, ok := r.w.Stage(stage); !ok {
		return fmt.Sprintf("unknown stage %q", stage)
	}
	d, ok := r.w.Active(stage)
	i := liveStep(d)
	waiting := ok && d.Phase == PhaseGating && i >= 0 && d.Steps[i].Type == StepApproval && d.Steps[i].State == StepWaiting
	if waiting && d.ID == deployment {
		// D-046: bind a decision to the inspected step, including consecutive approvals.
		if step != nil && *step != i {
			return fmt.Sprintf("deployment %q is waiting at step %d, not %d", deployment, i, *step)
		}
		return ""
	}
	reason := fmt.Sprintf("deployment %q is not waiting for approval in stage %s", deployment, stage)
	if waiting {
		reason += fmt.Sprintf("; %q is waiting", d.ID)
	}
	return reason
}

// record turns the input into facts (§3.4).
func (r *run) record() {
	switch in := r.in.(type) {
	case PipelineChanged:
		if in.Version < r.w.PipelineVersion {
			return // D-051: superseded; a newer apply's stages are already the state's
		}
		r.emit(StagesChanged{Version: in.Version, Stages: cloneStages(in.Stages)}) // running deployments keep their snapshot
	case NewRelease:
		if !r.isDuplicate(in.Release) {
			r.emit(ReleaseAdded{Release: cloneRelease(in.Release)})
		}
	case DeploymentFinished:
		r.deploymentFinished(in)
	case GateStepFinished:
		r.gateStepFinished(in)
	case Approve:
		r.decideApproval(in.Stage, in.Reason, true)
	case Reject:
		r.decideApproval(in.Stage, in.Reason, false)
	case AddHold:
		r.addHold(in)
	case ClearHold:
		r.clearHold(in)
	case ManualDeploy:
		r.manualDeploy(in)
	case Abort:
		r.abort(in)
	}
}

// isDuplicate reports whether the release is already known: same id, or same commit and digest set (R5, D-041).
func (r *run) isDuplicate(rel Release) bool {
	digests := digestSet(rel)
	for _, x := range r.w.Releases {
		if x.ID == rel.ID || (x.Commit == rel.Commit && slices.Equal(digestSet(x), digests)) {
			return true
		}
	}
	return false
}

func digestSet(r Release) []string {
	out := make([]string, len(r.Artifacts))
	for i, a := range r.Artifacts {
		out[i] = a.Image + "@" + a.Digest
	}
	slices.Sort(out)
	return out
}

func (r *run) deploymentFinished(in DeploymentFinished) {
	d, ok := r.w.Deployment(in.Deployment)
	if !ok {
		return
	}
	if _, ok := r.w.Stage(d.Stage); !ok {
		return // §3.10: a result for a deployment of a removed stage
	}
	switch {
	case d.Phase == PhaseDeploying && !in.Aborted:
		if in.OK {
			r.enterGate(d) // current(S) becomes d.Release
			return
		}
		r.deployFailed(d, in.Reason)
	case d.Phase == PhaseAborting && in.Aborted:
		r.abortFinished(d, in.Live)
	}
	// Otherwise a duplicate (§3.10), or a plain result for an aborting deployment, which only its verified abort
	// result ends (D-020).
}

// deployFailed ends a deployment whose release never took traffic; current(S) is unchanged (R14).
func (r *run) deployFailed(d Deployment, reason string) {
	r.emit(DeploymentPhaseChanged{Deployment: d.ID, Stage: d.Stage, Phase: PhaseDeployFailed, Reason: reason, At: r.now})
	r.mirror(d.ID, reason)
	r.notifyFailure(d, NotifyDeployFailed, "deploy failed", reason)
}

// abortFinished applies the deployer's verified abort result (§3.6, D-020). d.ResultReason holds the abort's reason.
func (r *run) abortFinished(d Deployment, live Live) {
	reason := d.ResultReason
	switch live {
	case LivePrevious:
		r.deployFailed(d, reason) // invariant 4 stops promotion from retrying it
	case LiveNew:
		// The abort came too late: the release is live, so the abort fails its gate.
		r.emit(DeploymentPhaseChanged{Deployment: d.ID, Stage: d.Stage, Phase: PhaseGating, At: r.now})
		r.mirror(d.ID, "")
		if len(d.Steps) == 0 {
			r.gateFailed(d, -1, reason)
			return
		}
		r.emit(GateStepChanged{Deployment: d.ID, Stage: d.Stage, Index: 0, State: StepFailed, Reason: reason, At: r.now})
		r.gateFailed(d, 0, reason)
	default:
		// Deployer.Abort never reports an unverified result (Done=false until verified, §7.1), so an empty or
		// unknown live is a bug signal. Nothing changes; the stage stays aborting and deploymentStuck fires.
	}
}

// enterGate starts step 0 of a deployment that took traffic, or passes it if the stage has no gate steps (R15).
func (r *run) enterGate(d Deployment) {
	if len(d.Steps) == 0 {
		r.pass(d)
		return
	}
	r.emit(DeploymentPhaseChanged{Deployment: d.ID, Stage: d.Stage, Phase: PhaseGating, At: r.now})
	r.mirror(d.ID, "")
	r.startStep(d, 0)
}

// startStep starts gate step i by itself (R15). An approval step waits for a person instead of a worker.
func (r *run) startStep(d Deployment, i int) {
	typ := d.Steps[i].Type
	if typ == StepApproval {
		r.emit(GateStepChanged{Deployment: d.ID, Stage: d.Stage, Index: i, State: StepWaiting, At: r.now})
		r.act(Notify{
			Actor: d.Actor, Reason: d.Reason, Index: i,
			ID: notifyStepID(d.ID, i, NotifyApprovalWaiting), Notification: NotifyApprovalWaiting, Urgency: UrgencyNormal,
			Stage: d.Stage, Release: d.Release, Deployment: d.ID,
			Message: fmt.Sprintf("%s: %s is waiting for approval", d.Stage, r.w.version(d.Release)),
		})
		return
	}
	r.emit(GateStepChanged{Deployment: d.ID, Stage: d.Stage, Index: i, State: StepRunning, At: r.now})
	r.act(StartGateStep{ID: gateActionID(d.ID, i), Deployment: d.ID, Stage: d.Stage, Index: i, Type: typ})
}

// pass marks a deployment passed; promotion follows in reconcile (R16).
func (r *run) pass(d Deployment) {
	r.emit(DeploymentPhaseChanged{Deployment: d.ID, Stage: d.Stage, Phase: PhasePassed, At: r.now})
	r.mirror(d.ID, "")
}

func (r *run) gateStepFinished(in GateStepFinished) {
	d, ok := r.w.Deployment(in.Deployment)
	if !ok || d.Phase != PhaseGating || in.Index < 0 || in.Index >= len(d.Steps) || d.Steps[in.Index].State != StepRunning {
		return // a duplicate (§3.10)
	}
	if _, ok := r.w.Stage(d.Stage); !ok {
		return // §3.10: a result for a deployment of a removed stage
	}
	if in.OK {
		r.emit(GateStepChanged{Deployment: d.ID, Stage: d.Stage, Index: in.Index, State: StepPassed, Reason: in.Reason, At: r.now})
		d, _ = r.w.Deployment(d.ID)
		if next := in.Index + 1; next < len(d.Steps) {
			r.startStep(d, next)
		} else {
			r.pass(d)
		}
		return
	}
	r.emit(GateStepChanged{Deployment: d.ID, Stage: d.Stage, Index: in.Index, State: StepFailed, Reason: in.Reason, At: r.now})
	r.gateFailed(d, in.Index, in.Reason)
}

// decideApproval records the trusted actor and continues the gate in the same evaluation (D-014, D-021).
func (r *run) decideApproval(stage, reason string, approved bool) {
	d, _ := r.w.Active(stage)
	i := liveStep(d)
	state := StepPassed
	if !approved {
		state = StepFailed
		reason = fmt.Sprintf("rejected by %s: %s", person(r.in.Meta().Actor), reason)
	}
	r.emit(GateStepChanged{Deployment: d.ID, Stage: stage, Index: i, State: state, Actor: r.in.Meta().Actor, Reason: reason, At: r.now})
	if !approved {
		r.gateFailed(d, i, reason)
		return
	}
	d, _ = r.w.Deployment(d.ID)
	if i+1 < len(d.Steps) {
		r.startStep(d, i+1)
	} else {
		r.pass(d)
	}
}

// gateFailed applies the gate-failure policy from the deployment's snapshot after step i failed (§3.7, R18). i is -1
// when an abort fails a deployment that has no gate steps.
func (r *run) gateFailed(d Deployment, i int, reason string) {
	step := "gate"
	if i >= 0 {
		step = d.Steps[i].Type
	}
	holdReason := fmt.Sprintf("rollback after failed %s of %s", step, r.w.version(d.Release))
	switch {
	case d.Config.OnGateFailure != PolicyRollback:
		// Policy keep: the release stays live but is never promotable. Holds don't change.
		r.endGate(d, PhaseGateFailed, reason)
	case d.Kind.IsRollback():
		// D-009: no second rollback; the holds stay, and notifyFailure sends rollbackFailed.
		r.endGate(d, PhaseGateFailed, reason)
	case d.PreviousRelease == "" || d.PreviousRelease == d.Release:
		// Nothing else to roll back to: the holds make a person look at it.
		r.endGate(d, PhaseGateFailed, reason)
		r.addRollbackHolds(d, holdReason)
	default:
		r.endGate(d, PhaseRolledBack, reason)
		r.addRollbackHolds(d, holdReason)
		st, _ := r.w.Stage(d.Stage)
		rb := r.startDeployment(st, d.PreviousRelease, KindGateRollback, holdReason) // the full deployment and gate
		r.act(Notify{
			Actor: rb.Actor, Reason: reason,
			ID: notifyDeploymentID(rb.ID, NotifyGateRollbackStarted), Notification: NotifyGateRollbackStarted, Urgency: UrgencyHigh,
			Stage: d.Stage, Release: rb.Release, Deployment: rb.ID,
			Message: fmt.Sprintf("%s: rolling back to %s after failed %s of %s: %s",
				d.Stage, r.w.version(rb.Release), step, r.w.version(d.Release), reason),
		})
		return
	}
	r.notifyFailure(d, NotifyGateFailed, step+" failed", reason)
}

// endGate moves a gating deployment to a terminal phase.
func (r *run) endGate(d Deployment, p Phase, reason string) {
	r.emit(DeploymentPhaseChanged{Deployment: d.ID, Stage: d.Stage, Phase: p, Reason: reason, At: r.now})
	r.mirror(d.ID, reason)
}

// addRollbackHolds puts both of the stage's switches on Hold after a gate failure under policy rollback (§3.7). They
// link to the failed deployment.
func (r *run) addRollbackHolds(d Deployment, reason string) {
	for _, dim := range []Dimension{DimDeploy, DimPromote} {
		r.addHoldRow(Hold{
			Stage: d.Stage, Dimension: dim, Source: SourceGateRollback, Reason: reason, Actor: r.in.Meta().Actor,
			Deployment: d.ID, CreatedAt: r.now,
		})
	}
}

// manualDeploy starts a deployment of the chosen release at once, past holds and the upstream (§3.6, R26–R30).
// validate rejected a stage that is deploying or aborting (D-007).
func (r *run) manualDeploy(in ManualDeploy) {
	st, _ := r.w.Stage(in.Stage)
	id := deploymentID(st.Name, r.w.Counters[st.Name].Deployments+1)
	who, rel := person(in.Actor), r.w.version(in.Release)
	for _, dim := range []Dimension{DimDeploy, DimPromote} { // R28
		r.addHoldRow(Hold{
			Stage: st.Name, Dimension: dim, Source: SourceManualDeploy, Actor: in.Actor, Deployment: id, CreatedAt: r.now,
			Reason: fmt.Sprintf("manual deployment of %s by %s: %s", rel, who, in.Reason),
		})
	}
	if d, ok := r.w.Active(st.Name); ok { // gating: cancel it (D-007)
		r.cancel(d, fmt.Sprintf("replaced by manual deployment of %s by %s", rel, who))
	}
	// §2.1: a rollback if the release is older than current, else an emergency (D-009).
	kind, n, what := KindEmergency, NotifyEmergencyDeployStarted, "emergency deployment of"
	if cur, ok := r.w.Current(st.Name); ok && r.w.older(in.Release, cur) {
		kind, n, what = KindRollback, NotifyRollbackStarted, "rollback to"
	}
	d := r.startDeployment(st, in.Release, kind, in.Reason) // R30: the full deployment and gate
	r.act(Notify{
		Actor: in.Actor, Reason: in.Reason,
		ID: notifyDeploymentID(d.ID, n), Notification: n, Urgency: UrgencyHigh, Stage: st.Name, Release: d.Release, Deployment: d.ID,
		Message: fmt.Sprintf("%s: %s %s by %s: %s", st.Name, what, rel, who, in.Reason),
	})
}

// cancel ends a gating deployment that a manual deployment replaces (§3.6). Its release stays live until the new
// one takes traffic, and it is never promotable.
func (r *run) cancel(d Deployment, reason string) {
	if i := liveStep(d); i >= 0 {
		r.emit(GateStepChanged{Deployment: d.ID, Stage: d.Stage, Index: i, State: StepCancelled, Reason: reason, At: r.now})
		r.cancelStep(d, i)
	}
	r.emit(DeploymentPhaseChanged{Deployment: d.ID, Stage: d.Stage, Phase: PhaseCancelled, Reason: reason, At: r.now})
	r.mirror(d.ID, reason)
}

// cancelStep stops step i's worker. An approval step has none.
func (r *run) cancelStep(d Deployment, i int) {
	if typ := d.Steps[i].Type; typ != StepApproval {
		r.act(CancelGateStep{ID: cancelActionID(d.ID, i), Deployment: d.ID, Stage: d.Stage, Index: i, Type: typ})
	}
}

// liveStep returns the index of the deployment's running or waiting step, or -1.
func liveStep(d Deployment) int {
	return slices.IndexFunc(d.Steps, func(s GateRun) bool { return s.State == StepRunning || s.State == StepWaiting })
}

// abort aborts the stage's active deployment, which validate checked is in.Deployment (§3.6, D-020). It adds no holds
// of its own; R14 and R18 decide what happens next.
func (r *run) abort(in Abort) {
	d, _ := r.w.Active(in.Stage)
	reason := fmt.Sprintf("aborted by %s: %s", person(in.Actor), in.Reason)
	switch d.Phase {
	case PhaseAborting:
		return // a no-op, because retries are expected
	case PhaseDeploying:
		// The stage stays busy until the deployer reports which release is verifiably live.
		r.emit(DeploymentPhaseChanged{Deployment: d.ID, Stage: d.Stage, Phase: PhaseAborting, Reason: reason, At: r.now})
		r.mirror(d.ID, "aborting: "+reason)
		r.act(AbortDeployment{ID: abortActionID(d.ID), Deployment: d.ID, Stage: d.Stage}) // cancels <d>/deploy (§6.1)
		r.notifyAborted(d, in.Actor, reason)
	case PhaseGating:
		// The running step fails as if it had failed by itself, and §3.7 applies.
		i := liveStep(d)
		if i >= 0 {
			r.emit(GateStepChanged{Deployment: d.ID, Stage: d.Stage, Index: i, State: StepFailed, Reason: reason, At: r.now})
			r.cancelStep(d, i)
		}
		r.notifyAborted(d, in.Actor, reason)
		r.gateFailed(d, i, reason)
	}
}

func (r *run) notifyAborted(d Deployment, actor, reason string) {
	r.act(Notify{
		Actor: actor, Reason: reason,
		ID: notifyDeploymentID(d.ID, NotifyDeploymentAborted), Notification: NotifyDeploymentAborted, Urgency: UrgencyHigh,
		Stage: d.Stage, Release: d.Release, Deployment: d.ID,
		Message: fmt.Sprintf("%s: %s %s", d.Stage, r.w.version(d.Release), reason),
	})
}

// person returns an actor as texts show it: a user without the "user:" prefix.
func person(actor string) string { return strings.TrimPrefix(actor, "user:") }

// notifyFailure sends kind, or rollbackFailed (urgent) if d was itself a rollback (§3.2, D-009).
func (r *run) notifyFailure(d Deployment, kind Notification, what, reason string) {
	urgency := UrgencyNormal
	if d.Kind.IsRollback() {
		kind, urgency, what = NotifyRollbackFailed, UrgencyUrgent, "rollback "+what
	}
	r.act(Notify{
		Actor: d.Actor, Reason: reason,
		ID: notifyDeploymentID(d.ID, kind), Notification: kind, Urgency: urgency,
		Stage: d.Stage, Release: d.Release, Deployment: d.ID,
		Message: fmt.Sprintf("%s: %s %s: %s", d.Stage, r.w.version(d.Release), what, reason),
	})
}

// addHold adds one hold per dimension (R22, R24).
func (r *run) addHold(in AddHold) {
	var seen []Dimension
	for _, dim := range in.Dimensions {
		if slices.Contains(seen, dim) {
			continue
		}
		seen = append(seen, dim)
		r.addHoldRow(Hold{
			Stage: in.Stage, Dimension: dim, Source: SourceManual, Reason: in.Reason, Actor: in.Actor, CreatedAt: r.now,
			ExpiresAt: in.ExpiresAt,
		})
	}
}

// addHoldRow gives h the stage's next hold id, adds it and notifies (§3.2).
func (r *run) addHoldRow(h Hold) {
	h.ID = holdID(h.Stage, r.w.Counters[h.Stage].Holds+1)
	r.emit(HoldAdded{Hold: h})
	r.act(Notify{
		Actor: h.Actor, Reason: h.Reason, Dimension: h.Dimension,
		ID: notifyHoldID(h.ID, NotifyHoldAdded), Notification: NotifyHoldAdded, Urgency: UrgencyNormal,
		Stage: h.Stage, Hold: h.ID,
		Message: fmt.Sprintf("%s: %s on Hold by %s: %s", h.Stage, dimTitle(h.Dimension), h.Actor, h.Reason),
	})
}

// clearHold clears the named hold, or every active hold of the dimension on the stage, of any source. Clearing one
// reason never clears the others (R24).
func (r *run) clearHold(in ClearHold) {
	var ids []string
	for _, h := range r.w.Holds {
		if (in.Hold != "" && h.ID == in.Hold) ||
			(in.Hold == "" && h.Stage == in.Stage && h.Dimension == in.Dimension && h.ActiveAt(r.now)) {
			ids = append(ids, h.ID)
		}
	}
	for _, id := range ids {
		r.clear(id, in.Actor, in.Reason)
	}
}

// expireHolds clears every hold whose expiry has passed (R25).
func (r *run) expireHolds() {
	var ids []string
	for _, h := range r.w.Holds {
		if !h.ExpiresAt.IsZero() && !r.now.Before(h.ExpiresAt) {
			ids = append(ids, h.ID)
		}
	}
	for _, id := range ids {
		r.clear(id, ActorExpiry, "expired")
	}
}

// checkStuck sends deploymentStuck for every deployment that has been deploying longer than stuckAfter (from
// startedAt) or aborting longer than stuckAfter (from abortRequestedAt), at most once per deployment per phase
// (§3.2, D-010). Nothing else changes: there is no timeout.
func (r *run) checkStuck() {
	var stuck []Deployment
	for _, d := range r.w.Deployments {
		if at, ok := r.w.stuckAt(d); ok && !r.now.Before(at) {
			stuck = append(stuck, d)
		}
	}
	for _, d := range stuck {
		since := d.StartedAt
		if d.Phase == PhaseAborting {
			since = d.AbortRequestedAt
		}
		r.emit(DeploymentStuck{Deployment: d.ID, Stage: d.Stage, Phase: d.Phase, At: r.now})
		r.act(Notify{
			Actor: d.Actor, Reason: d.Reason, Phase: d.Phase, Duration: r.now.Sub(since),
			ID: notifyStuckID(d.ID, d.Phase), Notification: NotifyDeploymentStuck, Urgency: UrgencyHigh,
			Stage: d.Stage, Release: d.Release, Deployment: d.ID,
			Message: fmt.Sprintf("%s: %s has been %s for %s", d.Stage, r.w.version(d.Release), d.Phase, r.now.Sub(since)),
		})
	}
}

func (r *run) clear(id, by, reason string) {
	i := slices.IndexFunc(r.w.Holds, func(h Hold) bool { return h.ID == id })
	h := r.w.Holds[i]
	r.emit(HoldCleared{Hold: h.ID, Stage: h.Stage, ClearedBy: by, Reason: reason, At: r.now})
	r.act(Notify{
		Actor: by, Reason: reason, Dimension: h.Dimension,
		ID: notifyHoldID(h.ID, NotifyHoldCleared), Notification: NotifyHoldCleared, Urgency: UrgencyNormal,
		Stage: h.Stage, Hold: h.ID,
		Message: fmt.Sprintf("%s: %s hold cleared by %s: %s", h.Stage, dimTitle(h.Dimension), by, h.Reason),
	})
}

func dimTitle(d Dimension) string {
	if d == DimPromote {
		return "Promote"
	}
	return "Deploy"
}

// reconcile moves one stage towards its candidate (§3.5).
func (r *run) reconcile(st Stage) {
	c, ok := r.w.candidate(st)
	if !ok || r.w.failedHere(c, st.Name) { // invariant 4
		r.clearPending(st.Name, "")
		return
	}
	if t, ok := r.w.target(st.Name); ok && t == c {
		r.clearPending(st.Name, c)
		return
	}
	blockers := r.blockersFor(st)
	if d, ok := r.w.Active(st.Name); ok { // R17: a busy stage keeps only the newest pending release
		blockers = append([]string{r.busy(d)}, blockers...)
	}
	if len(blockers) == 0 {
		reason := "promoted from " + st.Upstream
		if st.Upstream == "" {
			reason = "newest release" // R8
		}
		r.startDeployment(st, c, KindAuto, reason)
		return
	}
	r.updatePending(st.Name, c, strings.Join(blockers, "; "))
}

// blockersFor lists what stops a deployment into S at now (§3.8): its own Deploy holds, closed windows (R23) and
// the upstream's Promote holds (R10, R21).
func (r *run) blockersFor(st Stage) []string {
	var out []string
	for _, h := range r.w.ActiveHolds(st.Name, DimDeploy, r.now) {
		out = append(out, "Deploy on Hold: "+h.Reason)
	}
	for _, w := range r.w.WindowHolds(st.Name, r.now) { // R23, R24
		out = append(out, "Deploy on Hold: "+w)
	}
	if st.Upstream != "" {
		for _, h := range r.w.ActiveHolds(st.Upstream, DimPromote, r.now) {
			out = append(out, st.Upstream+" Promote on Hold: "+h.Reason)
		}
	}
	return out
}

func (r *run) busy(d Deployment) string {
	what := string(d.Phase)
	if d.Phase == PhaseGating {
		for _, s := range d.Steps {
			if s.State == StepRunning || s.State == StepWaiting {
				what = s.Type
				break
			}
		}
	}
	return fmt.Sprintf("busy: %s %s", r.w.version(d.Release), what)
}

// mirror sets the outcome of a deployment's release in its stage to the deployment's phase (D-027). It is the only
// way the engine changes an outcome, and it never touches the candidacy.
func (r *run) mirror(id, reason string) {
	d, _ := r.w.Deployment(id)
	r.emit(OutcomeChanged{Release: d.Release, Stage: d.Stage, Outcome: outcomeOf(d.Phase), Reason: reason, Deployment: d.ID, At: r.now})
}

// clearPending ends the stage's pending candidacy when nothing needs deploying. target is the candidate when it is
// already the stage's target: if it is the pending release (a failed deployment left it serving), its candidacy just
// ends. Any other pending release is no longer the candidate and is superseded. Outcomes are unchanged (D-027).
func (r *run) clearPending(stage, target string) {
	p, ok := r.w.Pending(stage)
	switch {
	case !ok:
	case p.Release == target:
		r.emit(CandidacyChanged{Release: p.Release, Stage: stage, Candidacy: CandidacyNone, At: r.now})
	default:
		r.emit(CandidacyChanged{Release: p.Release, Stage: stage, Candidacy: CandidacySuperseded, Reason: "no longer the candidate", At: r.now})
	}
}

// updatePending makes c the stage's only pending release, with its blockers as the reason (R10, R17). It changes
// only candidacies (D-027).
func (r *run) updatePending(stage, c, reason string) {
	p, ok := r.w.Pending(stage)
	if ok && p.Release == c && p.CandidacyReason == reason {
		return
	}
	r.supersedePending(stage, c)
	r.emit(CandidacyChanged{Release: c, Stage: stage, Candidacy: CandidacyPending, Reason: reason, At: r.now})
}

// supersedePending marks the stage's pending release superseded if it is not c.
func (r *run) supersedePending(stage, c string) {
	if p, ok := r.w.Pending(stage); ok && p.Release != c {
		r.emit(CandidacyChanged{
			Release: p.Release, Stage: stage, Candidacy: CandidacySuperseded, Reason: "superseded by " + r.w.version(c), At: r.now,
		})
	}
}

// startDeployment starts a deployment of release c into S on the stage's current config (R7, R9, R11, §3.5), and
// returns it. A deployment ends c's candidacy in S, if it had one.
func (r *run) startDeployment(st Stage, c string, kind DeploymentKind, reason string) Deployment {
	r.supersedePending(st.Name, c)
	prev, _ := r.w.Current(st.Name)
	d := Deployment{
		ID: deploymentID(st.Name, r.w.Counters[st.Name].Deployments+1), Stage: st.Name, Release: c, Kind: kind,
		PreviousRelease: prev, Actor: r.in.Meta().Actor, Reason: reason, Phase: PhaseDeploying,
		Config: StageConfig{
			PipelineVersion: r.w.PipelineVersion, Deploy: st.Deploy, Gate: st.Gate, OnGateFailure: st.OnGateFailure,
		},
		CreatedAt: r.now, StartedAt: r.now,
	}
	for i, g := range st.Gate {
		d.Steps = append(d.Steps, GateRun{Index: i, Type: g.Type, State: StepPending})
	}
	r.emit(DeploymentCreated{Deployment: cloneDeployment(d)})
	r.mirror(d.ID, "")
	if rs, ok := r.w.ReleaseStage(c, st.Name); ok && rs.Candidacy != CandidacyNone {
		r.emit(CandidacyChanged{Release: c, Stage: st.Name, Candidacy: CandidacyNone, At: r.now})
	}
	r.act(StartDeployment{ID: deployActionID(d.ID), Deployment: d.ID, Stage: st.Name, Release: c})
	return d
}

// nextWake returns the earliest time a re-evaluation could change something (§3.8): a hold expiry, the next change
// of a window that a stage with a candidate references, or when a deploying or aborting deployment becomes stuck.
func (r *run) nextWake() *time.Time {
	var at *time.Time
	earliest := func(t time.Time) {
		if at == nil || t.Before(*at) {
			at = &t
		}
	}
	for _, h := range r.w.Holds {
		if !h.ExpiresAt.IsZero() && h.ActiveAt(r.now) {
			earliest(h.ExpiresAt)
		}
	}
	for _, st := range r.w.Stages {
		if r.w.hasCandidate(st) {
			ws, _ := r.w.StageWindows(st.Name)
			if t, ok := WindowsNextChange(ws, r.now); ok { // a change that leaves the stage closed is harmless (principle 9)
				earliest(t)
			}
		}
	}
	for _, d := range r.w.Deployments {
		if t, ok := r.w.stuckAt(d); ok {
			earliest(t)
		}
	}
	return at
}
