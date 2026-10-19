package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// Refresh reads replica-independent state gauges from Postgres (§18.1). A failed read retains the last snapshot.
func (m *Metrics) Refresh(ctx context.Context, db *pgxpool.Pool) error {
	type stage struct {
		project, name, phase string
		pending              bool
	}
	var stages []stage
	var unprocessed int64
	type blocked struct {
		project, reason string
		count           int64
	}
	var lineage []blocked
	err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select p.name, s.name,
   exists(select 1 from release_stage r where r.project_id=s.project_id and r.stage=s.name and r.candidacy='pending'),
   coalesce((select d.phase from deployment d where d.project_id=s.project_id and d.stage=s.name and d.phase in ('deploying','aborting','gating')), '')
   from stage s join project p on p.id=s.project_id where s.removed_at is null`)
		if err != nil {
			return err
		}
		stages, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (stage, error) {
			var s stage
			err := r.Scan(&s.project, &s.name, &s.pending, &s.phase)
			return s, err
		})
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `select p.name, reasons.reason, count(c.commit)
          from project p cross join (values ('notOnBranch'), ('error')) reasons(reason)
          left join release_candidate c on c.project_id=p.id and c.state='open'
            and cardinality(c.missing)=0 and c.lineage->>'state'=reasons.reason
          group by p.name, reasons.reason`)
		if err != nil {
			return err
		}
		lineage, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (blocked, error) {
			var b blocked
			err := r.Scan(&b.project, &b.reason, &b.count)
			return b, err
		})
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `select count(*) from event where processed_at is null`).Scan(&unprocessed)
	})
	if err != nil {
		return fmt.Errorf("metrics: refresh state: %w", err)
	}
	next := newStateGauges()
	for _, s := range stages {
		pending := 0.
		if s.pending {
			pending = 1
		}
		next.Pending.WithLabelValues(s.project, s.name).Set(pending)
		for _, phase := range []string{"deploying", "aborting", "gating"} {
			active := 0.
			if s.phase == phase {
				active = 1
			}
			next.Active.WithLabelValues(s.project, s.name, phase).Set(active)
		}
	}
	for _, b := range lineage {
		next.LineageBlocked.WithLabelValues(b.project, b.reason).Set(float64(b.count))
	}

	next.Unprocessed.Set(float64(unprocessed))
	// Publish only fully populated vectors. Old vectors remain immutable during scrapes.
	m.stateMu.Lock()
	m.stateGauges = next
	m.stateMu.Unlock()
	return nil
}

// Run refreshes once immediately and every 15 seconds, retaining the last good snapshot on transient failure.
func (m *Metrics) Run(ctx context.Context, db *pgxpool.Pool, report func(error)) error {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if err := m.Refresh(ctx, db); err != nil && ctx.Err() == nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// stateGauges is built off to the side and published as one snapshot (§18.1).
type stateGauges struct {
	Pending        *prometheus.GaugeVec
	Active         *prometheus.GaugeVec
	LineageBlocked *prometheus.GaugeVec
	Unprocessed    prometheus.Gauge
}

func newStateGauges() stateGauges {
	gauge := func(name, help string, labels ...string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "promotion_" + name, Help: help}, labels)
	}
	return stateGauges{
		Pending:        gauge("pending_releases", "Whether a stage has a pending release.", "project", "stage"),
		Active:         gauge("deployments_active", "Active deployments by phase.", "project", "stage", "phase"),
		LineageBlocked: gauge("lineage_blocked_candidates", "Complete candidates waiting for lineage verification.", "project", "reason"),
		Unprocessed:    gauge("unprocessed_events", "Unprocessed event rows.").WithLabelValues(),
	}
}

// stateCollector captures one complete snapshot per scrape; collecting it needs no database I/O.
type stateCollector struct{ m *Metrics }

func (s stateCollector) collectors() []prometheus.Collector {
	s.m.stateMu.RLock()
	defer s.m.stateMu.RUnlock()
	return []prometheus.Collector{s.m.Pending, s.m.Active, s.m.LineageBlocked, s.m.Unprocessed}
}

func (s stateCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, c := range s.collectors() {
		c.Describe(ch)
	}
}

func (s stateCollector) Collect(ch chan<- prometheus.Metric) {
	for _, c := range s.collectors() {
		c.Collect(ch)
	}
}
