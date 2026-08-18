// Package api serves the JSON API, the Prometheus endpoint and the embedded UI.
package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/OdedNeuhaus/peevee/internal/cluster"
	"github.com/OdedNeuhaus/peevee/internal/config"
	"github.com/OdedNeuhaus/peevee/internal/metrics"
	"github.com/OdedNeuhaus/peevee/internal/remotewrite"
	"github.com/OdedNeuhaus/peevee/internal/store"
	"github.com/OdedNeuhaus/peevee/internal/version"
)

// Refresher triggers an out-of-band collection cycle.
type Refresher interface {
	Trigger()
	LastError() string
}

type Server struct {
	store     *store.Store
	pool      *cluster.Pool
	cfg       config.Config
	rw        *remotewrite.Client
	refresher Refresher
	web       fs.FS
	log       *slog.Logger
	started   time.Time
}

func NewServer(st *store.Store, pool *cluster.Pool, cfg config.Config, rw *remotewrite.Client, refresher Refresher, web fs.FS, log *slog.Logger) *Server {
	return &Server{store: st, pool: pool, cfg: cfg, rw: rw, refresher: refresher, web: web, log: log, started: time.Now()}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/volumes", s.handleVolumes)
	mux.HandleFunc("GET /api/v1/clusters", s.handleClusters)
	mux.HandleFunc("GET /api/v1/config", s.handleConfig)
	mux.HandleFunc("GET /api/v1/config.yaml", s.handleConfigYAML)
	mux.HandleFunc("GET /api/v1/history", s.handleHistory)
	mux.HandleFunc("GET /api/v1/export.csv", s.handleExportCSV)
	mux.HandleFunc("GET /api/v1/stream", s.handleStream)
	mux.HandleFunc("POST /api/v1/refresh", s.handleRefresh)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	mux.Handle("GET /", http.FileServer(http.FS(s.web)))

	return s.withLogging(mux)
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// The SSE stream is long-lived; logging it at info would emit one line
		// per connected browser tab per reconnect.
		level := slog.LevelDebug
		if rec.status >= 500 {
			level = slog.LevelError
		}
		s.log.Log(r.Context(), level, "http",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so SSE streaming keeps working
// through this wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) queryFromRequest(r *http.Request) store.Query {
	q := r.URL.Query()
	out := store.Query{
		Clusters:      multi(q, "cluster"),
		Namespaces:    multi(q, "namespace"),
		StorageClass:  multi(q, "storageClass"),
		Provisioner:   multi(q, "provisioner"),
		Status:        multi(q, "status"),
		Search:        strings.TrimSpace(q.Get("search")),
		Sort:          q.Get("sort"),
		Desc:          q.Get("desc") == "true",
		OnlyAtRisk:    q.Get("atRisk") == "true",
		WarnThreshold: s.cfg.UI.Thresholds.Warning,
		CritThreshold: s.cfg.UI.Thresholds.Critical,
	}
	if v, err := strconv.ParseFloat(q.Get("minUsage"), 64); err == nil {
		out.MinUsage = v
	}
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		out.Limit = v
	}
	if v, err := strconv.Atoi(q.Get("offset")); err == nil && v > 0 {
		out.Offset = v
	}
	return out
}

// multi accepts both repeated params and a comma separated list, so the UI can
// use whichever is more convenient.
func multi(q map[string][]string, key string) []string {
	var out []string
	for _, raw := range q[key] {
		for _, part := range strings.Split(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func (s *Server) handleVolumes(w http.ResponseWriter, r *http.Request) {
	res := s.store.Query(s.queryFromRequest(r))
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleClusters(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"clusters":   s.pool.HealthList(),
		"loadErrors": s.pool.LoadErrors(),
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key := q.Get("cluster") + "/" + q.Get("namespace") + "/" + q.Get("name")
	writeJSON(w, http.StatusOK, map[string]any{
		"key":    key,
		"points": s.store.History(key),
	})
}

// handleConfig exposes the effective configuration read-only. Helm and the
// mounted kubeconfig directory remain the single source of truth, so this is a
// window onto what the process actually loaded, not an editor.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"config":      redact(s.cfg),
		"source":      s.cfg.SourceName(),
		"readOnly":    true,
		"remoteWrite": s.rw.Status(),
		"version": map[string]string{
			"version": version.Version,
			"commit":  version.Commit,
			"date":    version.Date,
		},
		"uptimeSeconds": int64(time.Since(s.started).Seconds()),
		"lastError":     s.refresher.LastError(),
	})
}

// redact strips anything secret-shaped before the config reaches a browser.
func redact(c config.Config) config.Config {
	out := c
	if c.RemoteWrite.BasicAuth != nil {
		ba := *c.RemoteWrite.BasicAuth
		ba.Password = ""
		out.RemoteWrite.BasicAuth = &ba
	}
	if len(c.RemoteWrite.Headers) > 0 {
		headers := make(map[string]string, len(c.RemoteWrite.Headers))
		for k, v := range c.RemoteWrite.Headers {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "auth") || strings.Contains(lk, "token") ||
				strings.Contains(lk, "key") || strings.Contains(lk, "secret") {
				headers[k] = "***redacted***"
				continue
			}
			headers[k] = v
		}
		out.RemoteWrite.Headers = headers
	}
	return out
}

// handleConfigYAML renders the same redacted configuration as YAML. It is the
// effective config the process actually loaded, shaped exactly like the `config:`
// block in values.yaml, so it can be diffed against or pasted straight back into
// the chart.
func (s *Server) handleConfigYAML(w http.ResponseWriter, r *http.Request) {
	// Redact first, always. This endpoint serves a browser, and the config
	// struct's YAML tags include the remote write password.
	out, err := yaml.Marshal(redact(s.cfg))
	if err != nil {
		http.Error(w, "could not render configuration as YAML", http.StatusInternalServerError)
		s.log.Error("marshal config to yaml", "error", err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "# Effective configuration loaded from %s\n", s.cfg.SourceName())
	fmt.Fprintf(w, "# Read-only. Change it through the Helm values and run `helm upgrade`.\n")
	fmt.Fprintf(w, "# Secrets are redacted.\n\n")
	w.Write(out)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	s.refresher.Trigger()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "collection triggered"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	res := s.store.Query(store.Query{
		WarnThreshold: s.cfg.UI.Thresholds.Warning,
		CritThreshold: s.cfg.UI.Thresholds.Critical,
	})
	samples := metrics.Build(res, s.pool.HealthList(), time.Now())

	var sb strings.Builder
	metrics.WriteText(&sb, samples)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, sb.String())
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports ready only once a first snapshot exists, so a rolling
// update does not send traffic to a pod that would render an empty table.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	snap := s.store.Snapshot()
	if snap.GeneratedAt.IsZero() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "waiting for first collection",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"generatedAt": snap.GeneratedAt,
		"volumes":     len(snap.Volumes),
	})
}

// handleStream pushes a server-sent event whenever a new snapshot lands, so the
// UI updates on collection rather than on a polling timer of its own.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Without this, an nginx ingress buffers the stream and the UI never
	// receives an event until the connection closes.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	updates, unsubscribe := s.store.Subscribe()
	defer unsubscribe()

	// A periodic comment keeps proxies from reaping an idle connection.
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-updates:
			snap := s.store.Snapshot()
			payload, err := json.Marshal(map[string]any{
				"generatedAt": snap.GeneratedAt,
				"volumes":     len(snap.Volumes),
				"durationMs":  snap.DurationMS,
			})
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	q := s.queryFromRequest(r)
	q.Limit = 0
	q.Offset = 0
	res := s.store.Query(q)

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=\"peevee-%s.csv\"", time.Now().UTC().Format("20060102-150405")))

	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{
		"cluster", "namespace", "pvc", "status", "storage_class", "provisioner",
		"used_bytes", "capacity_bytes", "requested_bytes", "usage_percent",
		"inodes_percent", "shared_filesystem", "node", "workload",
		"growth_bytes_per_day", "days_until_full", "sampled",
	})
	for _, v := range res.Volumes {
		growth, days := "", ""
		if v.GrowthBytesPerDay != nil {
			growth = strconv.FormatFloat(*v.GrowthBytesPerDay, 'f', 0, 64)
		}
		if v.DaysUntilFull != nil {
			days = strconv.FormatFloat(*v.DaysUntilFull, 'f', 1, 64)
		}
		sampled := ""
		if !v.Sampled.IsZero() {
			sampled = v.Sampled.UTC().Format(time.RFC3339)
		}
		_ = cw.Write([]string{
			v.Cluster, v.Namespace, v.Name, string(v.Status), v.StorageClass, v.Provisioner,
			strconv.FormatInt(v.UsedBytes, 10),
			strconv.FormatInt(v.CapacityBytes, 10),
			strconv.FormatInt(v.RequestedBytes, 10),
			strconv.FormatFloat(v.UsagePercent, 'f', 2, 64),
			strconv.FormatFloat(v.InodesPercent, 'f', 2, 64),
			strconv.FormatBool(v.SharedFilesystem),
			v.Node, v.Workload, growth, days, sampled,
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
