// Package metrics implements the bounded, private Prometheus registry of design §18.
package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Vibrant-America/promotion-controller/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics belongs to one controller replica; no collectors use the global registry.
type Metrics struct {
	stateGauges
	stateMu        sync.RWMutex
	Registry       *prometheus.Registry
	Evaluations    *prometheus.CounterVec
	Evaluation     prometheus.Observer
	Action         *prometheus.HistogramVec
	Deployments    *prometheus.CounterVec
	Promotion      *prometheus.HistogramVec
	Ingest         *prometheus.HistogramVec
	Notifications  *prometheus.CounterVec // §18.2: notifier records sent/failed; log fallback records logged.
	GitHubRequests *prometheus.CounterVec // §18.2: the lineage client records actual HTTP request outcomes (F41).
}

// New registers the full §18.2 contract. commit identifies the binary, never a release.
func New(version, commit string, schemaVersion int64) *Metrics {
	r := prometheus.NewRegistry()
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "promotion_" + name, Help: help}, labels)
		r.MustRegister(c)
		return c
	}
	gauge := func(name, help string, labels ...string) *prometheus.GaugeVec {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "promotion_" + name, Help: help}, labels)
		return g
	}
	histogram := func(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
		h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "promotion_" + name, Help: help, Buckets: buckets}, labels)
		r.MustRegister(h)
		return h
	}
	// D-021: 120 seconds must be an explicit edge for both N2 histograms.
	buckets := []float64{1, 5, 15, 30, 60, 90, 120, 180, 300, 600}
	m := &Metrics{Registry: r, stateGauges: newStateGauges(),
		Evaluations:    counter("engine_evaluations_total", "Inputs processed by the engine.", "result"),
		Evaluation:     histogram("engine_evaluation_seconds", "Lock, load, handle and persist time per project batch.", nil).WithLabelValues(),
		Action:         histogram("action_latency_seconds", "Seconds from action creation to its first successful start.", nil, "type"),
		Deployments:    counter("deployments_total", "Deployments reaching a terminal phase.", "project", "stage", "result"),
		Promotion:      histogram("promotion_latency_seconds", "Seconds from upstream pass to successful immediate downstream auto start (N2).", buckets, "project", "stage"),
		Ingest:         histogram("ingest_latency_seconds", "Seconds from newest image build to release creation (N2).", buckets, "project"),
		Notifications:  counter("notifications_total", "Notifier outcomes.", "result"),
		GitHubRequests: counter("github_requests_total", "Branch lineage request outcomes.", "result"),
	}
	r.MustRegister(stateCollector{m})
	for _, s := range []string{"accepted", "rejected", "error"} {
		m.Evaluations.WithLabelValues(s)
	}
	for _, s := range []string{"StartDeployment", "StartGateStep", "CancelGateStep", "AbortDeployment", "Notify"} {
		m.Action.WithLabelValues(s)
	}
	for _, s := range []string{"sent", "failed", "logged"} {
		m.Notifications.WithLabelValues(s)
	}
	for _, s := range []string{"ok", "notModified", "rateLimited", "error"} {
		m.GitHubRequests.WithLabelValues(s)
	}
	build := gauge("build_info", "Controller binary build information.", "version", "commit", "schema_version")
	build.WithLabelValues(version, commit, strconv.FormatInt(schemaVersion, 10)).Set(1)
	r.MustRegister(build)
	return m
}

// Handler serves only this replica's registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// Batch records only committed engine results; failed transactions count processed inputs as errors.
func (m *Metrics) Batch(results []engine.Result, elapsed time.Duration, err error) {
	if m == nil || len(results) == 0 {
		return
	}
	m.Evaluation.Observe(elapsed.Seconds())
	for _, r := range results {
		result := "accepted"
		if r.Rejected != nil {
			result = "rejected"
		}
		if err != nil {
			result = "error"
		}
		m.Evaluations.WithLabelValues(result).Inc()
		if err != nil {
			continue
		}
		for _, tr := range r.Transitions {
			if t, ok := tr.(engine.DeploymentPhaseChanged); ok && t.Phase.Terminal() {
				m.Deployments.WithLabelValues(r.State.Project, t.Stage, string(t.Phase)).Inc()
			}
		}
	}
}
