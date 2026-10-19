package engine

import (
	"errors"
	"fmt"
	"reflect"
	"time"
)

// CheckInvariants checks the invariants of §2.5 that a single state shows (1, 2 and 7), the two facts of every
// release-in-stage row (§2.3, D-027), and the structure the engine relies on. CheckStep checks the ones about transitions (3–6).
func CheckInvariants(s State) error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	stages := map[string]bool{}
	roots := 0
	for _, st := range s.Stages {
		if st.Name == "" || stages[st.Name] {
			fail("stage name %q is empty or not unique", st.Name)
		}
		if st.Upstream == "" {
			roots++
		} else if !stages[st.Upstream] {
			fail("stage %s: upstream %q is not an earlier stage (topological order, D-008)", st.Name, st.Upstream)
		}
		stages[st.Name] = true
	}
	if len(s.Stages) > 0 && roots != 1 {
		fail("%d stages have no upstream; want exactly one first stage", roots)
	}

	// §12.3: windows are sorted by name and valid, and every window a stage references is in the state.
	for i, w := range s.Windows {
		if err := w.Validate(); err != nil {
			fail("window %q: %v", w.Name, err)
		}
		if i > 0 && s.Windows[i-1].Name >= w.Name {
			fail("windows are not sorted by unique name at %q", w.Name)
		}
	}
	for _, st := range s.Stages {
		if _, missing := s.StageWindows(st.Name); len(missing) > 0 {
			fail("stage %s: windows %q are not defined", st.Name, missing)
		}
	}

	releases := map[string]bool{}
	for i, r := range s.Releases {
		if r.Project != s.Project {
			fail("invariant 7: release %s belongs to project %q", r.ID, r.Project)
		}
		if releases[r.ID] {
			fail("release %s is listed twice", r.ID)
		}
		releases[r.ID] = true
		if i > 0 && compareReleases(s.Releases[i-1], r) >= 0 {
			fail("releases are not ordered by (createdAt, id) at %s", r.ID)
		}
	}

	deployments := map[string]Deployment{}
	busy := map[string]string{}
	for _, d := range s.Deployments {
		if _, dup := deployments[d.ID]; dup {
			fail("deployment %s is listed twice", d.ID)
		}
		deployments[d.ID] = d
		if n := idSeq(d.ID, d.Stage+"-"); n == 0 || n > s.Counters[d.Stage].Deployments {
			fail("deployment %s: id is not <stage>-<n> within the stage's counter", d.ID)
		}
		if !releases[d.Release] || d.PreviousRelease != "" && !releases[d.PreviousRelease] {
			fail("invariant 7: deployment %s refers to a release outside the working set", d.ID)
		}
		if d.Phase.Busy() {
			if other, ok := busy[d.Stage]; ok {
				fail("invariant 1: stage %s has two active deployments, %s and %s", d.Stage, other, d.ID)
			}
			busy[d.Stage] = d.ID
		}
		if len(d.Steps) != len(d.Config.Gate) {
			fail("deployment %s has %d steps for %d gate steps", d.ID, len(d.Steps), len(d.Config.Gate))
			continue
		}
		live := 0
		for i, st := range d.Steps {
			if st.Index != i || st.Type != d.Config.Gate[i].Type {
				fail("deployment %s: step %d does not match its gate snapshot", d.ID, i)
			}
			if st.State == StepRunning || st.State == StepWaiting {
				live++
			}
		}
		if live > 1 || live == 1 && d.Phase != PhaseGating {
			fail("deployment %s in %s has %d running or waiting steps", d.ID, d.Phase, live)
		}
		if d.Phase == PhaseAborting && d.AbortRequestedAt.IsZero() {
			fail("deployment %s is aborting without abortRequestedAt", d.ID)
		}
		for _, p := range d.StuckNotified {
			if p != PhaseDeploying && p != PhaseAborting {
				fail("deployment %s: deploymentStuck recorded for phase %s", d.ID, p)
			}
		}
	}

	holds := map[string]bool{}
	for _, h := range s.Holds {
		if holds[h.ID] {
			fail("hold %s is listed twice", h.ID)
		}
		holds[h.ID] = true
		if n := idSeq(h.ID, h.Stage+"-h"); n == 0 || n > s.Counters[h.Stage].Holds {
			fail("hold %s: id is not <stage>-h<n> within the stage's counter", h.ID)
		}
		if !h.ClearedAt.IsZero() {
			fail("hold %s is cleared but still in the working set", h.ID)
		}
		if h.Dimension != DimDeploy && h.Dimension != DimPromote {
			fail("hold %s has dimension %q", h.ID, h.Dimension)
		}
	}

	type key struct{ release, stage string }
	seen := map[key]bool{}
	pending := map[string]string{}
	for _, rs := range s.ReleaseStages {
		k := key{rs.Release, rs.Stage}
		if seen[k] {
			fail("release %s has two rows in stage %s", rs.Release, rs.Stage)
		}
		seen[k] = true
		if !releases[rs.Release] {
			fail("invariant 7: release-in-stage row for unknown release %s", rs.Release)
		}
		if rs.Outcome == OutcomeNone && rs.Candidacy == CandidacyNone {
			fail("release %s in %s: row has neither an outcome nor a candidacy", rs.Release, rs.Stage)
		}
		// D-027: the outcome mirrors the release's latest deployment in the stage, and only that.
		latest, deployed := s.latestFor(rs.Release, rs.Stage)
		switch {
		case !deployed && (rs.Outcome != OutcomeNone || rs.Deployment != ""):
			fail("release %s in %s: outcome %q (%q) without a deployment", rs.Release, rs.Stage, rs.Outcome, rs.Deployment)
		case deployed && rs.Deployment != latest.ID:
			fail("release %s in %s: outcome links %q, not its latest deployment %s", rs.Release, rs.Stage, rs.Deployment, latest.ID)
		case deployed && rs.Outcome != outcomeOf(latest.Phase):
			fail("release %s in %s: outcome %q does not mirror %s's phase %s", rs.Release, rs.Stage, rs.Outcome, latest.ID, latest.Phase)
		}
		switch rs.Candidacy {
		case CandidacyPending:
			if other, ok := pending[rs.Stage]; ok {
				fail("invariant 2: stage %s has two pending releases, %s and %s", rs.Stage, other, rs.Release)
			}
			pending[rs.Stage] = rs.Release
			if t, ok := s.target(rs.Stage); ok && t == rs.Release {
				fail("stage %s: pending release %s is already its target", rs.Stage, rs.Release)
			}
			if s.failedHere(rs.Release, rs.Stage) {
				fail("invariant 4: stage %s: pending release %s failed there", rs.Stage, rs.Release)
			}
		case CandidacySuperseded:
		case CandidacyNone:
			if rs.CandidacyReason != "" {
				fail("release %s in %s: candidacy reason %q without a candidacy", rs.Release, rs.Stage, rs.CandidacyReason)
			}
		default:
			fail("release %s in %s: unknown candidacy %q", rs.Release, rs.Stage, rs.Candidacy)
		}
	}
	for _, d := range s.Deployments {
		if !seen[key{d.Release, d.Stage}] {
			fail("deployment %s has no release-in-stage row", d.ID)
		}
	}
	// The release that serves a stage shows an outcome that took traffic. The one exception is a redeploy of the
	// serving release itself that hasn't taken traffic (yet): its latest deployment is the redeploy (§3.10).
	for _, st := range s.Stages {
		c, ok := s.Current(st.Name)
		if !ok {
			continue
		}
		rs, _ := s.ReleaseStage(c, st.Name)
		latest, _ := s.latestFor(c, st.Name)
		if !Phase(rs.Outcome).TookTraffic() && (rs.Outcome == OutcomeNone || latest.PreviousRelease != c) {
			fail("stage %s: current release %s shows outcome %q, which never took traffic", st.Name, c, rs.Outcome)
		}
	}
	return errors.Join(errs...)
}

// CheckStep checks the invariants of §2.5 that are about what one Handle call did (3–6), by folding r's
// transitions into before one at a time. It assumes r = Handle(before, in, now).
func CheckStep(before State, in Input, now time.Time, r Result) error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if r.Rejected != nil {
		if len(r.Transitions) > 0 || len(r.Actions) > 0 {
			fail("a rejected input produced transitions or actions")
		}
		return errors.Join(errs...)
	}

	cur := before.Clone()
	for i, t := range r.Transitions {
		switch t := t.(type) {
		case ReleaseAdded:
			if t.Release.Project != cur.Project {
				fail("transition %d: invariant 7: release %s of project %q", i, t.Release.ID, t.Release.Project)
			}
		case DeploymentCreated:
			d := t.Deployment
			st, ok := cur.Stage(d.Stage)
			if _, rok := cur.Release(d.Release); !ok || !rok {
				fail("transition %d: invariant 7: deployment %s of unknown stage or release", i, d.ID)
				break
			}
			if d.ID != deploymentID(d.Stage, cur.Counters[d.Stage].Deployments+1) {
				fail("transition %d: deployment id %s is not the stage's next id", i, d.ID)
			}
			if d.Phase != PhaseDeploying {
				fail("transition %d: deployment %s created in phase %s", i, d.ID, d.Phase)
			}
			if d.Kind != KindAuto {
				break
			}
			if !cur.DeployOpen(d.Stage, now) { // R24: holds and windows stop automatic deployments, not manual ones (R27)
				fail("transition %d: %s deploys automatically into %s while its Deploy is on Hold", i, d.ID, d.Stage)
			}
			if st.Upstream == "" {
				if n, _ := cur.Newest(); n.ID != d.Release {
					fail("transition %d: invariant 3: %s deploys %s into the first stage, but the newest release is %s", i, d.ID, d.Release, n.ID)
				}
			} else if p, _ := cur.Promotable(st.Upstream); p != d.Release {
				fail("transition %d: invariant 3: %s deploys %s, but promotable(%s) is %q", i, d.ID, d.Release, st.Upstream, p)
			}
			if cur.failedHere(d.Release, d.Stage) {
				fail("transition %d: invariant 4: %s automatically redeploys %s, which failed in %s", i, d.ID, d.Release, d.Stage)
			}
		case DeploymentPhaseChanged:
			d, ok := cur.Deployment(t.Deployment)
			switch {
			case !ok || d.Phase.Terminal():
				fail("transition %d: invariant 6: phase change of unknown or terminal deployment %s", i, t.Deployment)
			case t.Stage != d.Stage:
				fail("transition %d: phase change of %s names stage %s, not %s", i, d.ID, t.Stage, d.Stage)
			}
		case DeploymentStuck:
			d, ok := cur.Deployment(t.Deployment)
			at, due := cur.stuckAt(d)
			switch {
			case !ok || d.Phase.Terminal():
				fail("transition %d: invariant 6: deploymentStuck of unknown or terminal deployment %s", i, t.Deployment)
			case t.Stage != d.Stage:
				fail("transition %d: deploymentStuck of %s names stage %s, not %s", i, d.ID, t.Stage, d.Stage)
			case t.Phase != d.Phase || !due || now.Before(at):
				fail("transition %d: deploymentStuck of %s in %s is not due, or was already sent", i, d.ID, t.Phase)
			}
		case GateStepChanged:
			d, ok := cur.Deployment(t.Deployment)
			switch {
			case !ok || d.Phase.Terminal():
				fail("transition %d: invariant 6: step change of unknown or terminal deployment %s", i, t.Deployment)
			case t.Stage != d.Stage:
				fail("transition %d: step change of %s names stage %s, not %s", i, d.ID, t.Stage, d.Stage)
			}
		case HoldAdded:
			if _, ok := cur.Stage(t.Hold.Stage); !ok {
				fail("transition %d: invariant 7: hold on unknown stage %s", i, t.Hold.Stage)
			}
		case HoldCleared:
			var h *Hold
			for j := range cur.Holds {
				if cur.Holds[j].ID == t.Hold {
					h = &cur.Holds[j]
				}
			}
			_, byCommand := in.(ClearHold)
			expired := h != nil && !h.ExpiresAt.IsZero() && !now.Before(h.ExpiresAt)
			switch {
			case h == nil:
				fail("transition %d: clears unknown hold %s", i, t.Hold)
			case t.Stage != h.Stage:
				fail("transition %d: clearing hold %s names stage %s, not %s", i, t.Hold, t.Stage, h.Stage)
			case t.ClearedBy == ActorExpiry && !expired, t.ClearedBy != ActorExpiry && (!byCommand || t.ClearedBy != in.Meta().Actor):
				fail("transition %d: invariant 5: hold %s cleared by %s, not by a clear event or expiry", i, t.Hold, t.ClearedBy)
			}
		case OutcomeChanged:
			_, sok := cur.Stage(t.Stage)
			if _, rok := cur.Release(t.Release); !sok || !rok {
				fail("transition %d: invariant 7: outcome change for unknown release %s or stage %s", i, t.Release, t.Stage)
			}
			// D-027: only deployments change the outcome, so it names the release's latest deployment and its phase.
			latest, ok := cur.latestFor(t.Release, t.Stage)
			if !ok || latest.ID != t.Deployment || outcomeOf(latest.Phase) != t.Outcome {
				fail("transition %d: outcome %s of %s in %s does not mirror its latest deployment %q", i, t.Outcome, t.Release, t.Stage, t.Deployment)
			}
		case CandidacyChanged:
			_, sok := cur.Stage(t.Stage)
			if _, rok := cur.Release(t.Release); !sok || !rok {
				fail("transition %d: invariant 7: candidacy change for unknown release %s or stage %s", i, t.Release, t.Stage)
			}
		}
		rowBefore, _ := cur.ReleaseStage(releaseStageOf(t))
		cur.apply(t)
		rowAfter, _ := cur.ReleaseStage(releaseStageOf(t))
		switch t.(type) {
		case CandidacyChanged: // D-027: promotion never changes an outcome
			if rowAfter.Outcome != rowBefore.Outcome || rowAfter.Reason != rowBefore.Reason || rowAfter.Deployment != rowBefore.Deployment {
				fail("transition %d: candidacy change of %s in %s altered its outcome from %+v to %+v", i, rowAfter.Release, rowAfter.Stage, rowBefore, rowAfter)
			}
		case OutcomeChanged: // and a deployment never changes a candidacy
			if rowAfter.Candidacy != rowBefore.Candidacy || rowAfter.CandidacyReason != rowBefore.CandidacyReason {
				fail("transition %d: outcome change of %s in %s altered its candidacy from %+v to %+v", i, rowAfter.Release, rowAfter.Stage, rowBefore, rowAfter)
			}
		}
	}

	for _, d := range before.Deployments {
		if after, ok := r.State.Deployment(d.ID); d.Phase.Terminal() && (!ok || !reflect.DeepEqual(after, d)) {
			fail("invariant 6: terminal deployment %s changed", d.ID)
		}
	}
	return errors.Join(errs...)
}

// releaseStageOf returns the release and stage whose row a transition changes, or empty strings.
func releaseStageOf(t Transition) (string, string) {
	switch t := t.(type) {
	case OutcomeChanged:
		return t.Release, t.Stage
	case CandidacyChanged:
		return t.Release, t.Stage
	}
	return "", ""
}
