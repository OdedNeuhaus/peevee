// Package app owns the collection loop that drives everything else.
package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/OdedNeuhaus/peevee/internal/cluster"
	"github.com/OdedNeuhaus/peevee/internal/collector"
	"github.com/OdedNeuhaus/peevee/internal/config"
	"github.com/OdedNeuhaus/peevee/internal/metrics"
	"github.com/OdedNeuhaus/peevee/internal/remotewrite"
	"github.com/OdedNeuhaus/peevee/internal/store"
)

type Runner struct {
	col  *collector.Collector
	st   *store.Store
	pool *cluster.Pool
	rw   *remotewrite.Client
	cfg  config.Config
	log  *slog.Logger

	// trigger carries manual refresh requests from the API. It is buffered by
	// one so a burst of clicks collapses into a single extra cycle.
	trigger chan struct{}

	mu        sync.RWMutex
	lastError string
}

func NewRunner(col *collector.Collector, st *store.Store, pool *cluster.Pool, rw *remotewrite.Client, cfg config.Config, log *slog.Logger) *Runner {
	return &Runner{
		col: col, st: st, pool: pool, rw: rw, cfg: cfg, log: log,
		trigger: make(chan struct{}, 1),
	}
}

func (r *Runner) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

func (r *Runner) LastError() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastError
}

func (r *Runner) setLastError(msg string) {
	r.mu.Lock()
	r.lastError = msg
	r.mu.Unlock()
}

// Run collects immediately, then on every interval tick or manual trigger,
// until the context is cancelled.
func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Collector.Interval.D())
	defer ticker.Stop()

	r.cycle(ctx)

	for {
		select {
		case <-ctx.Done():
			r.log.Info("collection loop stopped")
			return
		case <-ticker.C:
			r.cycle(ctx)
		case <-r.trigger:
			r.log.Info("manual refresh requested")
			r.cycle(ctx)
		}
	}
}

func (r *Runner) cycle(ctx context.Context) {
	start := time.Now()
	snap := r.col.Collect(ctx)
	r.st.Put(snap)

	if len(snap.Errors) > 0 {
		msg := ""
		for name, e := range snap.Errors {
			msg += name + ": " + e + "; "
		}
		r.setLastError(msg)
	} else {
		r.setLastError("")
	}

	r.log.Info("collection complete",
		"volumes", len(snap.Volumes),
		"clusters_failed", len(snap.Errors),
		"duration_ms", snap.DurationMS)

	r.push(ctx, start)
}

// push ships the freshly collected snapshot to Mimir. It runs inside the same
// cycle so a slow remote write shows up as backpressure rather than as an
// unbounded queue.
func (r *Runner) push(ctx context.Context, ts time.Time) {
	if !r.cfg.RemoteWrite.Enabled {
		return
	}
	res := r.st.Query(store.Query{
		WarnThreshold: r.cfg.UI.Thresholds.Warning,
		CritThreshold: r.cfg.UI.Thresholds.Critical,
	})
	samples := metrics.Build(res, r.pool.HealthList(), ts)
	series := metrics.ToSeries(samples, ts)

	pushCtx, cancel := context.WithTimeout(ctx, r.cfg.RemoteWrite.Timeout.D())
	defer cancel()

	if err := r.rw.Push(pushCtx, series); err != nil {
		r.log.Error("remote write failed", "error", err, "series", len(series))
		return
	}
	r.log.Info("remote write ok", "series", len(series), "url", r.cfg.RemoteWrite.URL)
}
