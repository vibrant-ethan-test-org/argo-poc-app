package engine

import (
	"maps"
	"slices"
	"time"
)

// Apply folds transitions into a copy of s. The engine uses the same fold as it goes, so for any Result r of
// Handle(s, …), s.Apply(r.Transitions...) equals r.State; the store and tests check persisted state against it.
func (s State) Apply(ts ...Transition) State {
	w := s.Clone()
	for _, t := range ts {
		w.apply(t)
	}
	return w
}

// Clone returns a deep copy of s that shares no memory with it.
func (s State) Clone() State {
	c := State{
		Project:         s.Project,
		StuckAfter:      s.StuckAfter,
		PipelineVersion: s.PipelineVersion,
		Stages:          cloneStages(s.Stages),
		Releases:        cloneSlice(s.Releases, cloneRelease),
		Deployments:     cloneSlice(s.Deployments, cloneDeployment),
		Holds:           slices.Clone(s.Holds),
		ReleaseStages:   slices.Clone(s.ReleaseStages),
		Counters:        maps.Clone(s.Counters),
		Windows:         cloneSlice(s.Windows, cloneWindow),
	}
	return c
}

func (s *State) apply(t Transition) {
	switch t := t.(type) {
	case StagesChanged:
		s.PipelineVersion = t.Version
		s.Stages = cloneStages(t.Stages)
	case ReleaseAdded:
		r := cloneRelease(t.Release)
		i, _ := slices.BinarySearchFunc(s.Releases, r, compareReleases)
		s.Releases = slices.Insert(s.Releases, i, r)
	case DeploymentCreated:
		s.Deployments = append(s.Deployments, cloneDeployment(t.Deployment))
		c := s.Counters[t.Deployment.Stage]
		c.Deployments = max(c.Deployments, idSeq(t.Deployment.ID, t.Deployment.Stage+"-"))
		s.setCounters(t.Deployment.Stage, c)
	case DeploymentPhaseChanged:
		d := s.deployment(t.Deployment)
		if d == nil {
			return
		}
		d.Phase = t.Phase
		if t.Phase == PhaseAborting {
			d.AbortRequestedAt = t.At
			d.ResultReason = t.Reason // the abort's reason, which its verified result reports (§3.6)
		}
		if t.Phase.Terminal() {
			d.EndedAt = t.At
			d.ResultReason = t.Reason
		}
	case DeploymentStuck:
		if d := s.deployment(t.Deployment); d != nil && !slices.Contains(d.StuckNotified, t.Phase) {
			d.StuckNotified = append(d.StuckNotified, t.Phase)
		}
	case GateStepChanged:
		d := s.deployment(t.Deployment)
		if d == nil || t.Index < 0 || t.Index >= len(d.Steps) {
			return
		}
		st := &d.Steps[t.Index]
		st.State = t.State
		switch t.State {
		case StepWaiting, StepRunning:
			st.StartedAt = t.At
		case StepPassed, StepFailed, StepCancelled:
			st.EndedAt = t.At
		}
		if t.Actor != "" {
			st.Actor = t.Actor
		}
		st.Reason = t.Reason
	case HoldAdded:
		s.Holds = append(s.Holds, t.Hold)
		c := s.Counters[t.Hold.Stage]
		c.Holds = max(c.Holds, idSeq(t.Hold.ID, t.Hold.Stage+"-h"))
		s.setCounters(t.Hold.Stage, c)
	case HoldCleared:
		// The working set keeps only uncleared holds; the store keeps the cleared row.
		s.Holds = slices.DeleteFunc(s.Holds, func(h Hold) bool { return h.ID == t.Hold })
	case OutcomeChanged:
		rs := s.touchReleaseStage(t.Release, t.Stage, t.At)
		rs.Outcome, rs.Reason, rs.Deployment = t.Outcome, t.Reason, t.Deployment
	case CandidacyChanged:
		rs := s.touchReleaseStage(t.Release, t.Stage, t.At)
		rs.Candidacy, rs.CandidacyReason = t.Candidacy, t.Reason
	}
}

// touchReleaseStage returns the release's row in the stage, adding an empty one the first time, with UpdatedAt set.
func (s *State) touchReleaseStage(release, stage string, at time.Time) *ReleaseStage {
	rs := s.releaseStage(release, stage)
	if rs == nil {
		s.ReleaseStages = append(s.ReleaseStages, ReleaseStage{Release: release, Stage: stage})
		rs = &s.ReleaseStages[len(s.ReleaseStages)-1]
	}
	rs.UpdatedAt = at
	return rs
}

func (s *State) setCounters(stage string, c Counters) {
	if s.Counters == nil {
		s.Counters = map[string]Counters{}
	}
	s.Counters[stage] = c
}

func (s *State) deployment(id string) *Deployment {
	for i := range s.Deployments {
		if s.Deployments[i].ID == id {
			return &s.Deployments[i]
		}
	}
	return nil
}

func (s *State) releaseStage(release, stage string) *ReleaseStage {
	for i := range s.ReleaseStages {
		if rs := &s.ReleaseStages[i]; rs.Release == release && rs.Stage == stage {
			return rs
		}
	}
	return nil
}

func compareReleases(a, b Release) int {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	switch {
	case a.ID < b.ID:
		return -1
	case a.ID > b.ID:
		return 1
	}
	return 0
}

func cloneSlice[T any](in []T, f func(T) T) []T {
	if in == nil {
		return nil
	}
	out := make([]T, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

func cloneStages(in []Stage) []Stage {
	return cloneSlice(in, func(st Stage) Stage {
		st.Deploy.Config = slices.Clone(st.Deploy.Config)
		st.Gate = cloneGate(st.Gate)
		st.Windows = slices.Clone(st.Windows)
		return st
	})
}

func cloneGate(in []GateStepSpec) []GateStepSpec {
	return cloneSlice(in, func(g GateStepSpec) GateStepSpec {
		g.Config = slices.Clone(g.Config)
		return g
	})
}

func cloneWindow(w Window) Window {
	if w.Recurring != nil {
		r := *w.Recurring
		r.Days = slices.Clone(r.Days)
		w.Recurring = &r
	}
	if w.Once != nil {
		o := *w.Once
		w.Once = &o
	}
	return w
}

func cloneRelease(r Release) Release {
	r.Artifacts = slices.Clone(r.Artifacts)
	return r
}

func cloneDeployment(d Deployment) Deployment {
	d.Config.Deploy.Config = slices.Clone(d.Config.Deploy.Config)
	d.Config.Gate = cloneGate(d.Config.Gate)
	d.Steps = slices.Clone(d.Steps)
	d.StuckNotified = slices.Clone(d.StuckNotified)
	return d
}
