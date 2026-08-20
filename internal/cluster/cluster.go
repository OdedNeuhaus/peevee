// Package cluster discovers clusters from a directory of kubeconfig files and
// maintains a pool of Kubernetes clients, one per logical cluster.
package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/OdedNeuhaus/peevee/internal/config"
)

// Cluster is one logical target: a kubeconfig context plus a ready client.
type Cluster struct {
	// Name is the stable identifier used in the UI and as the `cluster` metric label.
	Name        string
	DisplayName string
	// Source is the kubeconfig file this cluster came from.
	Source  string
	Context string
	// Endpoint is the API server URL, shown in the UI for operator sanity checks.
	Endpoint string
	Labels   map[string]string

	Client kubernetes.Interface
	// fingerprint detects a changed kubeconfig so the client is rebuilt.
	fingerprint string
}

// Health is the last known reachability of a cluster.
type Health struct {
	Name         string            `json:"name"`
	DisplayName  string            `json:"displayName"`
	Source       string            `json:"source"`
	Context      string            `json:"context"`
	Endpoint     string            `json:"endpoint"`
	Labels       map[string]string `json:"labels,omitempty"`
	Reachable    bool              `json:"reachable"`
	Version      string            `json:"version,omitempty"`
	Error        string            `json:"error,omitempty"`
	LastCheck    time.Time         `json:"lastCheck"`
	NodesTotal   int               `json:"nodesTotal"`
	NodesScraped int               `json:"nodesScraped"`
	// NodeErrors maps node name to why its kubelet could not be scraped, so a
	// denied nodes/proxy call is visible instead of showing up as claims that
	// look unmounted.
	NodeErrors   map[string]string `json:"nodeErrors,omitempty"`
	PVCsTotal    int               `json:"pvcsTotal"`
	ScrapeMillis int64             `json:"scrapeMillis"`
}

// Pool holds the live set of clusters and refreshes it from disk.
type Pool struct {
	mu       sync.RWMutex
	clusters map[string]*Cluster
	health   map[string]*Health
	// loadErrors records kubeconfig files that could not be parsed, so the UI
	// can show a bad file rather than silently ignoring it.
	loadErrors map[string]string

	cfg config.Config
	log *slog.Logger
}

func NewPool(cfg config.Config, log *slog.Logger) *Pool {
	return &Pool{
		clusters:   map[string]*Cluster{},
		health:     map[string]*Health{},
		loadErrors: map[string]string{},
		cfg:        cfg,
		log:        log,
	}
}

// Clusters returns the current enabled clusters, sorted by name for stable output.
func (p *Pool) Clusters() []*Cluster {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Cluster, 0, len(p.clusters))
	for _, c := range p.clusters {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (p *Pool) LoadErrors() map[string]string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]string, len(p.loadErrors))
	for k, v := range p.loadErrors {
		out[k] = v
	}
	return out
}

// Reload rescans the kubeconfig directory. Clients for unchanged kubeconfigs are
// preserved so their connection pools and caches survive.
func (p *Pool) Reload() error {
	discovered, loadErrs, err := p.discover()
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	next := make(map[string]*Cluster, len(discovered))
	for name, cand := range discovered {
		if existing, ok := p.clusters[name]; ok && existing.fingerprint == cand.cluster.fingerprint {
			// Same kubeconfig content: keep the live client, refresh metadata.
			existing.DisplayName = cand.cluster.DisplayName
			existing.Labels = cand.cluster.Labels
			next[name] = existing
			continue
		}
		client, err := kubernetes.NewForConfig(cand.restConfig)
		if err != nil {
			loadErrs[cand.cluster.Source] = fmt.Sprintf("build client for context %q: %v", cand.cluster.Context, err)
			continue
		}
		cand.cluster.Client = client
		next[name] = cand.cluster
		p.log.Info("cluster registered", "cluster", name, "source", cand.cluster.Source, "endpoint", cand.cluster.Endpoint)
	}

	for name := range p.clusters {
		if _, ok := next[name]; !ok {
			p.log.Info("cluster removed", "cluster", name)
			delete(p.health, name)
		}
	}

	p.clusters = next
	p.loadErrors = loadErrs
	return nil
}

type candidate struct {
	cluster    *Cluster
	restConfig *rest.Config
}

// discover walks the kubeconfig directory and turns every context into a candidate.
func (p *Pool) discover() (map[string]*candidate, map[string]string, error) {
	out := map[string]*candidate{}
	loadErrs := map[string]string{}

	overrides := map[string]config.ClusterOverride{}
	for _, o := range p.cfg.Clusters {
		overrides[o.Name] = o
	}

	dir := p.cfg.Discovery.Dir
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("read kubeconfig dir %s: %w", dir, err)
		}
		p.log.Warn("kubeconfig directory does not exist", "dir", dir)
		entries = nil
	}

	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			// Skip ..data and ..2026_* symlink dirs that Kubernetes secret
			// projection creates alongside the real files.
			continue
		}
		if !p.extensionAllowed(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			loadErrs[path] = err.Error()
			continue
		}
		apiCfg, err := clientcmd.Load(raw)
		if err != nil {
			loadErrs[path] = fmt.Sprintf("not a valid kubeconfig: %v", err)
			continue
		}
		fingerprint := sha256sum(raw)

		contexts := p.contextsFor(apiCfg)
		if len(contexts) == 0 {
			loadErrs[path] = "kubeconfig contains no usable context"
			continue
		}
		for _, ctxName := range contexts {
			name := p.clusterName(e.Name(), ctxName, len(contexts) > 1)
			ov := overrides[name]
			if ov.Disabled {
				p.log.Info("cluster disabled by config", "cluster", name)
				continue
			}

			restCfg, err := restConfigFor(apiCfg, ctxName, ov, p.cfg.Collector.Timeout.D())
			if err != nil {
				loadErrs[path] = fmt.Sprintf("context %q: %v", ctxName, err)
				continue
			}

			display := ov.DisplayName
			if display == "" {
				display = name
			}
			c := &Cluster{
				Name:        name,
				DisplayName: display,
				Source:      filepath.Base(path),
				Context:     ctxName,
				Endpoint:    restCfg.Host,
				Labels:      ov.Labels,
				fingerprint: fingerprint,
			}
			if _, clash := out[name]; clash {
				loadErrs[path] = fmt.Sprintf("duplicate cluster name %q; rename the kubeconfig file or set a clusters[].name override", name)
				continue
			}
			out[name] = &candidate{cluster: c, restConfig: restCfg}
		}
	}

	if p.cfg.Discovery.InClusterFallback {
		if restCfg, err := rest.InClusterConfig(); err == nil {
			name := "in-cluster"
			if _, clash := out[name]; !clash {
				restCfg.Timeout = p.cfg.Collector.Timeout.D()
				out[name] = &candidate{
					cluster: &Cluster{
						Name:        name,
						DisplayName: name,
						Source:      "serviceaccount",
						Context:     "in-cluster",
						Endpoint:    restCfg.Host,
						fingerprint: "in-cluster",
					},
					restConfig: restCfg,
				}
			}
		} else {
			p.log.Debug("in-cluster fallback unavailable", "error", err)
		}
	}

	return out, loadErrs, nil
}

func (p *Pool) extensionAllowed(name string) bool {
	allowed := p.cfg.Discovery.FileExtensions
	if len(allowed) == 0 {
		return true
	}
	ext := filepath.Ext(name)
	for _, a := range allowed {
		if strings.EqualFold(ext, a) {
			return true
		}
	}
	return false
}

func (p *Pool) contextsFor(apiCfg *clientcmdapi.Config) []string {
	if p.cfg.Discovery.AllContexts {
		names := make([]string, 0, len(apiCfg.Contexts))
		for name := range apiCfg.Contexts {
			names = append(names, name)
		}
		sort.Strings(names)
		return names
	}
	if apiCfg.CurrentContext != "" {
		if _, ok := apiCfg.Contexts[apiCfg.CurrentContext]; ok {
			return []string{apiCfg.CurrentContext}
		}
	}
	// No current-context set: fall back to the only context, if unambiguous.
	if len(apiCfg.Contexts) == 1 {
		for name := range apiCfg.Contexts {
			return []string{name}
		}
	}
	return nil
}

// clusterName derives the metric label from the filename, which is what an
// operator controls when they drop a kubeconfig into the folder. The context is
// appended only when one file yields several clusters.
func (p *Pool) clusterName(fileName, ctxName string, multi bool) string {
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	base = sanitize(base)
	if !multi {
		return base
	}
	return base + "-" + sanitize(ctxName)
}

func restConfigFor(apiCfg *clientcmdapi.Config, ctxName string, ov config.ClusterOverride, timeout time.Duration) (*rest.Config, error) {
	clientCfg := clientcmd.NewNonInteractiveClientConfig(*apiCfg, ctxName, &clientcmd.ConfigOverrides{}, nil)
	restCfg, err := clientCfg.ClientConfig()
	if err != nil {
		return nil, err
	}
	restCfg.Timeout = timeout
	// Raise the client-side rate limits: a scrape lists PVCs, PVs, pods and
	// nodes then fans out to every kubelet, which trips the 5 QPS default.
	restCfg.QPS = 50
	restCfg.Burst = 100
	restCfg.UserAgent = "peevee"
	if ov.InsecureSkipTLSVerify {
		restCfg.TLSClientConfig.Insecure = true
		restCfg.TLSClientConfig.CAData = nil
		restCfg.TLSClientConfig.CAFile = ""
	}
	return restCfg, nil
}

// sanitize makes a string safe as a Prometheus label value and a URL fragment.
func sanitize(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func sha256sum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SetHealth records the outcome of the most recent scrape of a cluster.
func (p *Pool) SetHealth(h *Health) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.health[h.Name] = h
}

func (p *Pool) HealthList() []Health {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Health, 0, len(p.health))
	for _, h := range p.health {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Watch re-scans the kubeconfig directory on an interval so clusters can be
// added or removed by updating the Secret, with no restart.
func (p *Pool) Watch(ctx context.Context) {
	interval := p.cfg.Discovery.ReloadInterval.D()
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.Reload(); err != nil {
				p.log.Error("kubeconfig reload failed", "error", err)
			}
		}
	}
}
