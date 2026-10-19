package engine

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Derived stage values (§2.4). They are computed from the working set, never stored.

// Stage returns the definition of the named stage.
func (s State) Stage(name string) (Stage, bool) {
	for _, st := range s.Stages {
		if st.Name == name {
			return st, true
		}
	}
	return Stage{}, false
}

// Release returns the release with this id.
func (s State) Release(id string) (Release, bool) {
	for _, r := range s.Releases {
		if r.ID == id {
			return r, true
		}
	}
	return Release{}, false
}

// Newest returns the project's newest release, by (CreatedAt, ID).
func (s State) Newest() (Release, bool) {
	if len(s.Releases) == 0 {
		return Release{}, false
	}
	return s.Releases[len(s.Releases)-1], true
}

// Deployment returns the deployment with this id.
func (s State) Deployment(id string) (Deployment, bool) {
	if d := s.deployment(id); d != nil {
		return *d, true
	}
	return Deployment{}, false
}

// History returns the deployments of a stage in the working set, newest first (R20).
func (s State) History(stage string) []Deployment {
	var out []Deployment
	for i := len(s.Deployments) - 1; i >= 0; i-- {
		if s.Deployments[i].Stage == stage {
			out = append(out, s.Deployments[i])
		}
	}
	return out
}

// Active returns active(S): the deployment in deploying, aborting or gating, if any.
func (s State) Active(stage string) (Deployment, bool) {
	for _, d := range s.History(stage) {
		if d.Phase.Busy() {
			return d, true
		}
	}
	return Deployment{}, false
}

// Current returns current(S), the release that serves now: the release of the latest deployment that took traffic.
// A rolledBack deployment counts only until its rollback deployment takes traffic, which a newer deployment that
// took traffic shows.
func (s State) Current(stage string) (string, bool) {
	for _, d := range s.History(stage) {
		if d.Phase.TookTraffic() {
			return d.Release, true
		}
	}
	return "", false
}

// Settled returns settled(S): the latest deployment that took traffic and has a terminal phase.
func (s State) Settled(stage string) (Deployment, bool) {
	for _, d := range s.History(stage) {
		if d.Phase.TookTraffic() && d.Phase.Terminal() {
			return d, true
		}
	}
	return Deployment{}, false
}

// Promotable returns promotable(S): settled(S).release if it passed (D-006).
func (s State) Promotable(stage string) (string, bool) {
	if d, ok := s.Settled(stage); ok && d.Phase == PhasePassed {
		return d.Release, true
	}
	return "", false
}

// ReleaseStage returns a release's state in a stage, if it has one.
func (s State) ReleaseStage(release, stage string) (ReleaseStage, bool) {
	for _, rs := range s.ReleaseStages {
		if rs.Release == release && rs.Stage == stage {
			return rs, true
		}
	}
	return ReleaseStage{}, false
}

// Pending returns the row of the stage's pending release, if any. There is at most one (invariant 2).
func (s State) Pending(stage string) (ReleaseStage, bool) {
	for _, rs := range s.ReleaseStages {
		if rs.Stage == stage && rs.Candidacy == CandidacyPending {
			return rs, true
		}
	}
	return ReleaseStage{}, false
}

// ActiveHolds returns the stage's holds of one dimension that are active at now, oldest first.
func (s State) ActiveHolds(stage string, dim Dimension, now time.Time) []Hold {
	var out []Hold
	for _, h := range s.Holds {
		if h.Stage == stage && h.Dimension == dim && h.ActiveAt(now) {
			out = append(out, h)
		}
	}
	return out
}

// DeployOpen returns deployOpen(S, now): no active Deploy hold, and the stage's windows allow now (§2.4, §12.3).
func (s State) DeployOpen(stage string, now time.Time) bool {
	return len(s.ActiveHolds(stage, DimDeploy, now)) == 0 && len(s.WindowHolds(stage, now)) == 0
}

// Window returns the state's window with this name.
func (s State) Window(name string) (Window, bool) {
	for _, w := range s.Windows {
		if w.Name == name {
			return w, true
		}
	}
	return Window{}, false
}

// StageWindows returns the windows the stage references, in its order, and the names the state has no window for.
func (s State) StageWindows(stage string) (ws []Window, missing []string) {
	st, _ := s.Stage(stage)
	for _, name := range st.Windows {
		if w, ok := s.Window(name); ok {
			ws = append(ws, w)
		} else {
			missing = append(missing, name)
		}
	}
	return ws, missing
}

// WindowHolds returns why the stage's windows hold its Deploy at now (§12.3), as the blocker text after "Deploy on
// Hold: ". It is empty when they don't. A referenced window the state lacks fails closed: it signals a bug, which
// CheckInvariants reports.
func (s State) WindowHolds(stage string, now time.Time) []string {
	ws, missing := s.StageWindows(stage)
	var out []string
	for _, name := range missing {
		out = append(out, "window "+name+" is not defined")
	}
	closers := WindowClosers(ws, now)
	if len(closers) == 0 {
		return out
	}
	names := make([]string, len(closers))
	for i, w := range closers {
		names[i] = w.Name + " closed"
		if w.Mode == WindowDeny {
			names[i] = w.Name + " (deny)"
		}
	}
	opens := fmt.Sprintf("opens: not within %d days", WindowHorizon/(24*time.Hour))
	if at, ok := WindowsNextOpen(ws, now); ok {
		loc := time.UTC
		if i := slices.IndexFunc(ws, func(w Window) bool { return w.Recurring != nil }); i >= 0 {
			loc = ws[i].Recurring.Loc
		}
		opens = "opens " + at.In(loc).Format("Mon 2006-01-02 15:04 MST")
	}
	return append(out, "window "+strings.Join(names, ", ")+"; "+opens)
}

// PromoteOpen returns promoteOpen(S, now).
func (s State) PromoteOpen(stage string, now time.Time) bool {
	return len(s.ActiveHolds(stage, DimPromote, now)) == 0
}

// latestFor returns the release's latest deployment in a stage.
func (s State) latestFor(release, stage string) (Deployment, bool) {
	for _, d := range s.History(stage) {
		if d.Release == release {
			return d, true
		}
	}
	return Deployment{}, false
}

// failedHere reports whether c's latest deployment in S failed, so promotion never retries it (invariant 4).
func (s State) failedHere(release, stage string) bool {
	d, ok := s.latestFor(release, stage)
	return ok && d.Phase.Failed()
}

// candidate returns candidate(S) (§3.5): the newest release for the first stage, else promotable(upstream(S)).
// Holds are not considered; they are blockers.
func (s State) candidate(st Stage) (string, bool) {
	if st.Upstream == "" { // R8
		r, ok := s.Newest()
		return r.ID, ok
	}
	return s.Promotable(st.Upstream) // R9, D-006
}

// hasCandidate reports whether S has a candidate that isn't its target yet, which reconcile deploys or leaves pending:
// only then can a window's change make the next evaluation do something (§3.8).
func (s State) hasCandidate(st Stage) bool {
	c, ok := s.candidate(st)
	if !ok || s.failedHere(c, st.Name) {
		return false
	}
	t, ok := s.target(st.Name)
	return !ok || t != c
}

// target returns active(S).release if any, else current(S).
func (s State) target(stage string) (string, bool) {
	if d, ok := s.Active(stage); ok {
		return d.Release, true
	}
	return s.Current(stage)
}

// older reports whether release a is older than release b, by (createdAt, id) (§2.1).
func (s State) older(a, b string) bool {
	ra, aok := s.Release(a)
	rb, bok := s.Release(b)
	return aok && bok && compareReleases(ra, rb) < 0
}

// stuckAfter returns the engine's stuckAfter setting (§3.2).
func (s State) stuckAfter() time.Duration {
	if s.StuckAfter > 0 {
		return s.StuckAfter
	}
	return DefaultStuckAfter
}

// stuckAt returns when d becomes stuck in its phase: startedAt + stuckAfter while deploying, abortRequestedAt +
// stuckAfter while aborting. It returns false in any other phase, once deploymentStuck was sent for the phase, or
// for a removed stage, whose results are ignored too (§3.10).
func (s State) stuckAt(d Deployment) (time.Time, bool) {
	if _, ok := s.Stage(d.Stage); !ok {
		return time.Time{}, false
	}
	var since time.Time
	switch d.Phase {
	case PhaseDeploying:
		since = d.StartedAt
	case PhaseAborting:
		since = d.AbortRequestedAt
	default:
		return time.Time{}, false
	}
	if slices.Contains(d.StuckNotified, d.Phase) {
		return time.Time{}, false
	}
	return since.Add(s.stuckAfter()), true
}

// version returns a release's display version, falling back to its id.
func (s State) version(id string) string {
	if r, ok := s.Release(id); ok && r.Version != "" {
		return r.Version
	}
	return id
}
