// Package remotewrite ships PVC usage series to Grafana Mimir, or anything else
// that speaks the Prometheus remote write protocol.
package remotewrite

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang/snappy"

	"github.com/OdedNeuhaus/peevee/internal/config"
)

// Status is the last outcome of a push, surfaced in the UI so an operator can
// tell whether Mimir is actually receiving anything.
type Status struct {
	Enabled       bool      `json:"enabled"`
	URL           string    `json:"url,omitempty"`
	TenantID      string    `json:"tenantId,omitempty"`
	LastAttempt   time.Time `json:"lastAttempt,omitempty"`
	LastSuccess   time.Time `json:"lastSuccess,omitempty"`
	LastError     string    `json:"lastError,omitempty"`
	SamplesSent   int64     `json:"samplesSent"`
	SamplesFailed int64     `json:"samplesFailed"`
	Batches       int64     `json:"batches"`
}

type Client struct {
	cfg    config.RemoteWriteConfig
	http   *http.Client
	log    *slog.Logger
	mu     sync.RWMutex
	status Status
}

func New(cfg config.RemoteWriteConfig, log *slog.Logger) (*Client, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.TLS.InsecureSkipVerify,
		ServerName:         cfg.TLS.ServerName,
	}
	if cfg.TLS.CAFile != "" {
		pem, err := os.ReadFile(cfg.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read remoteWrite CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("remoteWrite CA file %s contains no usable certificate", cfg.TLS.CAFile)
		}
		tlsCfg.RootCAs = pool
	}

	return &Client{
		cfg: cfg,
		log: log,
		http: &http.Client{
			Timeout: cfg.Timeout.D(),
			Transport: &http.Transport{
				TLSClientConfig:     tlsCfg,
				MaxIdleConnsPerHost: 4,
			},
		},
		status: Status{Enabled: cfg.Enabled, URL: cfg.URL, TenantID: cfg.TenantID},
	}, nil
}

func (c *Client) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

// Push sends every series, split into batches of at most MaxSamplesPerSend and
// shipped by at most MaxShards goroutines.
func (c *Client) Push(ctx context.Context, series []Series) error {
	if !c.cfg.Enabled || len(series) == 0 {
		return nil
	}

	// External labels are the standard way to say which peevee produced a
	// sample when several write into one Mimir tenant.
	if len(c.cfg.ExternalLabels) > 0 {
		for i := range series {
			for k, v := range c.cfg.ExternalLabels {
				series[i].Labels = append(series[i].Labels, Label{Name: k, Value: v})
			}
		}
	}

	batchSize := c.cfg.MaxSamplesPerSend
	if batchSize < 1 {
		batchSize = 500
	}
	var batches [][]Series
	for i := 0; i < len(series); i += batchSize {
		end := i + batchSize
		if end > len(series) {
			end = len(series)
		}
		batches = append(batches, series[i:end])
	}

	shards := c.cfg.MaxShards
	if shards < 1 {
		shards = 1
	}
	sem := make(chan struct{}, shards)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sent     int64
		failed   int64
	)

	for _, batch := range batches {
		wg.Add(1)
		go func(batch []Series) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			err := c.sendWithRetry(ctx, batch)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed += int64(len(batch))
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			sent += int64(len(batch))
		}(batch)
	}
	wg.Wait()

	c.mu.Lock()
	c.status.LastAttempt = time.Now()
	c.status.SamplesSent += sent
	c.status.SamplesFailed += failed
	c.status.Batches += int64(len(batches))
	if firstErr != nil {
		c.status.LastError = firstErr.Error()
	} else {
		c.status.LastError = ""
		c.status.LastSuccess = time.Now()
	}
	c.mu.Unlock()

	if firstErr != nil {
		return firstErr
	}
	c.log.Debug("remote write complete", "series", len(series), "batches", len(batches))
	return nil
}

func (c *Client) sendWithRetry(ctx context.Context, batch []Series) error {
	retries := c.cfg.MaxRetries
	if retries < 0 {
		retries = 0
	}
	var lastErr error
	backoff := 500 * time.Millisecond

	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		err := c.send(ctx, batch)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryable(err) {
			return err
		}
	}
	return lastErr
}

// retryableError marks failures worth another attempt: 5xx, 429 and transport
// errors. A 400 means the payload itself is wrong and will never succeed.
type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

func isRetryable(err error) bool {
	var re retryableError
	return errorsAs(err, &re)
}

func errorsAs(err error, target *retryableError) bool {
	for err != nil {
		if re, ok := err.(retryableError); ok {
			*target = re
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func (c *Client) send(ctx context.Context, batch []Series) error {
	payload := EncodeWriteRequest(batch)
	compressed := snappy.Encode(nil, payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(compressed))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	req.Header.Set("User-Agent", "peevee")
	// Mimir routes by tenant; without this header a multi-tenant cluster
	// rejects the write with "no org id".
	if c.cfg.TenantID != "" {
		req.Header.Set("X-Scope-OrgID", c.cfg.TenantID)
	}
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}
	// An empty basicAuth block renders as `{}` from Helm, which unmarshals to a
	// non-nil struct with no username. Sending that would put an empty
	// Authorization header on the wire, which Mimir rejects.
	if c.cfg.BasicAuth != nil && c.cfg.BasicAuth.Username != "" {
		password := c.cfg.BasicAuth.Password
		if password == "" && c.cfg.BasicAuth.PasswordFile != "" {
			b, err := os.ReadFile(c.cfg.BasicAuth.PasswordFile)
			if err != nil {
				return fmt.Errorf("read remoteWrite password file: %w", err)
			}
			password = strings.TrimSpace(string(b))
		}
		req.SetBasicAuth(c.cfg.BasicAuth.Username, password)
	}
	if c.cfg.BearerTokenFile != "" {
		b, err := os.ReadFile(c.cfg.BearerTokenFile)
		if err != nil {
			return fmt.Errorf("read remoteWrite bearer token file: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return retryableError{err}
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 == 2 {
		io.Copy(io.Discard, resp.Body)
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	err = fmt.Errorf("remote write %s: %s", resp.Status, strings.TrimSpace(string(body)))
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5 {
		return retryableError{err}
	}
	return err
}
