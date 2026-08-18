package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole platform configuration. It is loaded from a YAML file
// that Helm renders into a ConfigMap, plus a directory of kubeconfigs mounted
// from a Secret. The UI renders this read-only; the file on disk is the single
// source of truth, so `helm upgrade` is never fighting the running process.
type Config struct {
	Server      ServerConfig      `yaml:"server" json:"server"`
	Discovery   DiscoveryConfig   `yaml:"discovery" json:"discovery"`
	Collector   CollectorConfig   `yaml:"collector" json:"collector"`
	RemoteWrite RemoteWriteConfig `yaml:"remoteWrite" json:"remoteWrite"`
	UI          UIConfig          `yaml:"ui" json:"ui"`

	// Clusters holds optional per-cluster overrides, keyed by the cluster name
	// derived during discovery. Anything not listed here still gets collected
	// with defaults; this exists to relabel, disable, or annotate a cluster.
	Clusters []ClusterOverride `yaml:"clusters" json:"clusters"`

	// path is where this config was read from, for the UI to display.
	path string `yaml:"-" json:"-"`
}

type ServerConfig struct {
	ListenAddress string `yaml:"listenAddress" json:"listenAddress"`
	Port          int    `yaml:"port" json:"port"`
}

// DiscoveryConfig controls how clusters are found. Every file in Dir is treated
// as a kubeconfig; a file with several contexts can be fanned out into several
// logical clusters via AllContexts.
type DiscoveryConfig struct {
	Dir            string   `yaml:"dir" json:"dir"`
	AllContexts    bool     `yaml:"allContexts" json:"allContexts"`
	FileExtensions []string `yaml:"fileExtensions" json:"fileExtensions"`
	// ReloadInterval controls how often the kubeconfig directory is re-scanned,
	// so adding a cluster is a Secret update rather than a restart.
	ReloadInterval Duration `yaml:"reloadInterval" json:"reloadInterval"`
	// InClusterFallback adds the cluster peevee itself runs in, useful for a
	// single-cluster install with no kubeconfigs mounted at all.
	InClusterFallback bool `yaml:"inClusterFallback" json:"inClusterFallback"`
}

type CollectorConfig struct {
	Interval Duration `yaml:"interval" json:"interval"`
	Timeout  Duration `yaml:"timeout" json:"timeout"`
	// ClusterConcurrency is how many clusters are scraped at once; NodeConcurrency
	// is how many kubelets are queried at once within a single cluster.
	ClusterConcurrency int `yaml:"clusterConcurrency" json:"clusterConcurrency"`
	NodeConcurrency    int `yaml:"nodeConcurrency" json:"nodeConcurrency"`

	IncludeNamespaces []string `yaml:"includeNamespaces" json:"includeNamespaces"`
	ExcludeNamespaces []string `yaml:"excludeNamespaces" json:"excludeNamespaces"`

	// IncludeUnmounted keeps PVCs with no live filesystem stats in the results,
	// reported with status "unmounted" rather than being silently dropped.
	IncludeUnmounted bool `yaml:"includeUnmounted" json:"includeUnmounted"`
	// StaleAfter marks a volume's sample stale if kubelet's timestamp is older
	// than this. Kubelet caches volume stats, typically on a ~1m cycle.
	StaleAfter Duration `yaml:"staleAfter" json:"staleAfter"`
}

// RemoteWriteConfig pushes collected series to Grafana Mimir (or any endpoint
// speaking the Prometheus remote write protocol).
type RemoteWriteConfig struct {
	Enabled  bool              `yaml:"enabled" json:"enabled"`
	URL      string            `yaml:"url" json:"url"`
	TenantID string            `yaml:"tenantId" json:"tenantId"`
	Timeout  Duration          `yaml:"timeout" json:"timeout"`
	Headers  map[string]string `yaml:"headers" json:"headers"`

	BasicAuth       *BasicAuth `yaml:"basicAuth,omitempty" json:"basicAuth,omitempty"`
	BearerTokenFile string     `yaml:"bearerTokenFile" json:"bearerTokenFile"`
	TLS             TLSConfig  `yaml:"tlsConfig" json:"tlsConfig"`

	// ExternalLabels are attached to every series, the usual way to tag which
	// peevee instance or region a sample came from.
	ExternalLabels map[string]string `yaml:"externalLabels" json:"externalLabels"`

	MaxSamplesPerSend int `yaml:"maxSamplesPerSend" json:"maxSamplesPerSend"`
	MaxRetries        int `yaml:"maxRetries" json:"maxRetries"`
	// MaxShards caps how many batches are pushed in parallel.
	MaxShards int `yaml:"maxShards" json:"maxShards"`
}

type BasicAuth struct {
	Username     string `yaml:"username" json:"username"`
	Password     string `yaml:"password,omitempty" json:"-"`
	PasswordFile string `yaml:"passwordFile" json:"passwordFile"`
}

type TLSConfig struct {
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify" json:"insecureSkipVerify"`
	CAFile             string `yaml:"caFile" json:"caFile"`
	ServerName         string `yaml:"serverName" json:"serverName"`
}

type UIConfig struct {
	Title           string     `yaml:"title" json:"title"`
	RefreshInterval Duration   `yaml:"refreshInterval" json:"refreshInterval"`
	Thresholds      Thresholds `yaml:"thresholds" json:"thresholds"`
}

// Thresholds are usage percentages that colour a volume in the UI and drive the
// "at risk" counters.
type Thresholds struct {
	Warning  float64 `yaml:"warning" json:"warning"`
	Critical float64 `yaml:"critical" json:"critical"`
}

type ClusterOverride struct {
	// Name matches the discovered cluster name.
	Name        string            `yaml:"name" json:"name"`
	DisplayName string            `yaml:"displayName" json:"displayName"`
	Disabled    bool              `yaml:"disabled" json:"disabled"`
	Labels      map[string]string `yaml:"labels" json:"labels"`
	// InsecureSkipTLSVerify overrides the kubeconfig's TLS setting, for clusters
	// whose kubeconfig ships without a CA bundle.
	InsecureSkipTLSVerify bool `yaml:"insecureSkipTlsVerify" json:"insecureSkipTlsVerify"`
}

func Default() Config {
	return Config{
		Server: ServerConfig{ListenAddress: "0.0.0.0", Port: 8080},
		Discovery: DiscoveryConfig{
			Dir:               "/etc/peevee/kubeconfigs",
			AllContexts:       false,
			FileExtensions:    []string{"", ".yaml", ".yml", ".conf", ".kubeconfig"},
			ReloadInterval:    Duration(2 * time.Minute),
			InClusterFallback: false,
		},
		Collector: CollectorConfig{
			Interval:           Duration(60 * time.Second),
			Timeout:            Duration(30 * time.Second),
			ClusterConcurrency: 4,
			NodeConcurrency:    8,
			ExcludeNamespaces:  []string{},
			IncludeUnmounted:   true,
			StaleAfter:         Duration(10 * time.Minute),
		},
		RemoteWrite: RemoteWriteConfig{
			Enabled:           false,
			TenantID:          "",
			Timeout:           Duration(30 * time.Second),
			Headers:           map[string]string{},
			ExternalLabels:    map[string]string{},
			MaxSamplesPerSend: 500,
			MaxRetries:        3,
			MaxShards:         2,
		},
		UI: UIConfig{
			Title:           "Peevee",
			RefreshInterval: Duration(30 * time.Second),
			Thresholds:      Thresholds{Warning: 75, Critical: 90},
		},
	}
}

// Load reads a YAML config from path, layering it over the defaults. A missing
// file is not an error: it means "run with defaults", which keeps the chart's
// configmap optional.
func Load(path string) (Config, error) {
	cfg := Default()
	cfg.path = path

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg.applyEnvOverrides()
			return cfg, cfg.Validate()
		}
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}

	cfg.applyEnvOverrides()
	return cfg, cfg.Validate()
}

// applyEnvOverrides lets secrets stay out of the ConfigMap. Only credentials are
// overridable this way; structural config belongs in the file.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("PEEVEE_REMOTE_WRITE_URL"); v != "" {
		c.RemoteWrite.URL = v
	}
	if v := os.Getenv("PEEVEE_REMOTE_WRITE_TENANT_ID"); v != "" {
		c.RemoteWrite.TenantID = v
	}
	if v := os.Getenv("PEEVEE_REMOTE_WRITE_USERNAME"); v != "" {
		if c.RemoteWrite.BasicAuth == nil {
			c.RemoteWrite.BasicAuth = &BasicAuth{}
		}
		c.RemoteWrite.BasicAuth.Username = v
	}
	if v := os.Getenv("PEEVEE_REMOTE_WRITE_PASSWORD"); v != "" {
		if c.RemoteWrite.BasicAuth == nil {
			c.RemoteWrite.BasicAuth = &BasicAuth{}
		}
		c.RemoteWrite.BasicAuth.Password = v
	}
}

func (c *Config) Validate() error {
	var errs []string

	if c.Server.Port < 1 || c.Server.Port > 65535 {
		errs = append(errs, fmt.Sprintf("server.port %d is out of range", c.Server.Port))
	}
	if c.Collector.Interval.D() < 5*time.Second {
		errs = append(errs, "collector.interval must be at least 5s")
	}
	if c.Collector.Timeout.D() <= 0 {
		errs = append(errs, "collector.timeout must be positive")
	}
	if c.Collector.Timeout.D() > c.Collector.Interval.D() {
		errs = append(errs, fmt.Sprintf(
			"collector.timeout (%s) must not exceed collector.interval (%s), or scrapes will overlap",
			c.Collector.Timeout, c.Collector.Interval))
	}
	if c.Collector.ClusterConcurrency < 1 {
		errs = append(errs, "collector.clusterConcurrency must be at least 1")
	}
	if c.Collector.NodeConcurrency < 1 {
		errs = append(errs, "collector.nodeConcurrency must be at least 1")
	}
	if len(c.Collector.IncludeNamespaces) > 0 && len(c.Collector.ExcludeNamespaces) > 0 {
		errs = append(errs, "collector.includeNamespaces and collector.excludeNamespaces are mutually exclusive")
	}
	if c.RemoteWrite.Enabled {
		if c.RemoteWrite.URL == "" {
			errs = append(errs, "remoteWrite.url is required when remoteWrite.enabled is true")
		} else if !strings.HasPrefix(c.RemoteWrite.URL, "http://") && !strings.HasPrefix(c.RemoteWrite.URL, "https://") {
			errs = append(errs, "remoteWrite.url must be an http:// or https:// URL")
		}
		if c.RemoteWrite.MaxSamplesPerSend < 1 {
			errs = append(errs, "remoteWrite.maxSamplesPerSend must be at least 1")
		}
		if c.RemoteWrite.MaxShards < 1 {
			errs = append(errs, "remoteWrite.maxShards must be at least 1")
		}
	}
	t := c.UI.Thresholds
	if t.Warning <= 0 || t.Warning >= 100 {
		errs = append(errs, "ui.thresholds.warning must be between 0 and 100")
	}
	if t.Critical <= 0 || t.Critical > 100 {
		errs = append(errs, "ui.thresholds.critical must be between 0 and 100")
	}
	if t.Warning >= t.Critical {
		errs = append(errs, "ui.thresholds.warning must be lower than ui.thresholds.critical")
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func (c Config) Path() string { return c.path }

// SourceName is what the UI shows as the origin of the config.
func (c Config) SourceName() string {
	if c.path == "" {
		return "defaults (no file)"
	}
	return filepath.Clean(c.path)
}
