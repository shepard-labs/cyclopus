package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BeszelConfig is the resolved configuration for the Beszel PocketBase backend.
type BeszelConfig struct {
	// BaseURL is the Beszel HTTPS endpoint.
	BaseURL string
	// Token is a PocketBase auth token placed in the Authorization header. It is
	// resolved from a secret reference by the caller.
	Token string
}

// BeszelClient is a bounded, non-redirecting HTTPS client for Beszel's
// PocketBase REST API. It intentionally exposes no tenant parameters: Beszel
// host metrics describe the operator's own machines and cannot be attributed to
// a tenant app (Phase 4 design note). GetOperatorMetrics returns a fixed
// allowlist of system-level metrics only.
type BeszelClient struct {
	config BeszelConfig
	client *http.Client
}

// metricAllowlist is the fixed set of system-level metric names Cyclopus will
// surface from a Beszel system record's `info` object. Anything outside this
// set is dropped, so a backend that starts returning richer or per-container
// data cannot leak it through this endpoint.
var metricAllowlist = []struct {
	// field is the key inside the Beszel system `info` object.
	field string
	name  string
	unit  string
}{
	{"cpu", "cpu.usage", "percent"},
	{"mp", "memory.usage", "percent"},
	{"dp", "disk.usage", "percent"},
	{"b", "bandwidth", "MB/s"},
	{"t", "temperature", "celsius"},
}

// NewBeszelClient builds a bounded client with the same posture as the
// OpenObserve client.
func NewBeszelClient(config BeszelConfig) (*BeszelClient, error) {
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("observability: invalid Beszel base URL")
	}
	return &BeszelClient{config: config, client: boundedHTTPClient()}, nil
}

type beszelListResponse struct {
	Items []beszelSystem `json:"items"`
}

type beszelSystem struct {
	Name    string          `json:"name"`
	Updated string          `json:"updated"`
	Info    json.RawMessage `json:"info"`
}

// GetOperatorMetrics fetches the operator's host systems and projects only the
// allowlisted system-level metrics from each. No tenant parameters are accepted
// or forwarded.
func (c *BeszelClient) GetOperatorMetrics(ctx context.Context) ([]OperatorMetric, error) {
	endpoint := strings.TrimRight(c.config.BaseURL, "/") + "/api/collections/systems/records?perPage=200"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.config.Token != "" {
		req.Header.Set("Authorization", c.config.Token)
	}
	response, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("observability: Beszel request failed")
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, errors.New("observability: Beszel returned a redirect")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("observability: Beszel returned HTTP %d", response.StatusCode)
	}
	var decoded beszelListResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, MaxResponseBytes)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("observability: decode Beszel response")
	}
	metrics := make([]OperatorMetric, 0, len(decoded.Items)*len(metricAllowlist))
	for _, system := range decoded.Items {
		observedAt := parseBeszelTime(system.Updated)
		info := map[string]any{}
		if len(system.Info) > 0 {
			_ = json.Unmarshal(system.Info, &info)
		}
		host := system.Name
		if host == "" {
			host = "host"
		}
		for _, allowed := range metricAllowlist {
			raw, ok := info[allowed.field]
			if !ok {
				continue
			}
			metrics = append(metrics, OperatorMetric{
				Name:       host + "/" + allowed.name,
				Value:      floatField(raw),
				Unit:       allowed.unit,
				ObservedAt: observedAt,
			})
		}
	}
	return metrics, nil
}

func parseBeszelTime(value string) time.Time {
	for _, layout := range []string{"2006-01-02 15:04:05.999Z", time.RFC3339, "2006-01-02 15:04:05.999999Z07:00"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}
