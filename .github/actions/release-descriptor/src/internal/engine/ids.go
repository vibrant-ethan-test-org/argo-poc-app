package engine

import (
	"fmt"
	"strconv"
	"strings"
)

// Deterministic ids (§3.2): a retried input produces the same ids, so the outbox ignores duplicates.

func deploymentID(stage string, n int) string { return fmt.Sprintf("%s-%d", stage, n) }
func holdID(stage string, n int) string       { return fmt.Sprintf("%s-h%d", stage, n) }

func deployActionID(d string) string        { return d + "/deploy" }
func abortActionID(d string) string         { return d + "/abort" }
func gateActionID(d string, i int) string   { return fmt.Sprintf("%s/gate/%d", d, i) }
func cancelActionID(d string, i int) string { return gateActionID(d, i) + "/cancel" }

// PhaseActionID returns the id of the action that drives deployment d while it is in phase p: its StartDeployment
// while deploying, its AbortDeployment while aborting, and "" in any other phase. The worker reads that action's last
// retry error for deploymentStuck (15 §15.2).
func PhaseActionID(d string, p Phase) string {
	switch p {
	case PhaseDeploying:
		return deployActionID(d)
	case PhaseAborting:
		return abortActionID(d)
	}
	return ""
}

func notifyDeploymentID(d string, n Notification) string { return d + "/notify/" + string(n) }
func notifyHoldID(h string, n Notification) string       { return h + "/notify/" + string(n) }
func notifyStuckID(d string, p Phase) string {
	return notifyDeploymentID(d, NotifyDeploymentStuck) + "/" + string(p)
}
func notifyStepID(d string, i int, n Notification) string {
	return gateActionID(d, i) + "/notify/" + string(n)
}

// idSeq returns n from an id "<prefix><n>", or 0 if id has another shape.
func idSeq(id, prefix string) int {
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 || strconv.Itoa(n) != rest {
		return 0
	}
	return n
}
