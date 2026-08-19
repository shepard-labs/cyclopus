package observability

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// BeszelConfig is the resolved configuration for the Beszel PocketBase backend.
type BeszelConfig struct {
	// BaseURL is the Beszel HTTPS endpoint.
	BaseURL string
	// Credential is the Beszel superuser "identity:password" pair, resolved
	// from a secret reference by the caller. Unlike a pre-minted PocketBase
	// session token (capped at PocketBase's ~2 year maximum), a password
	// credential lets the client re-authenticate indefinitely, so it never
	// silently expires the way a static token would.
	Credential string
}

// BeszelClient is a bounded, non-redirecting HTTPS client for Beszel's
// PocketBase REST API. It intentionally exposes no tenant parameters: Beszel
// host metrics describe the operator's own machines and cannot be attributed to
// a tenant app (Phase 4 design note). GetOperatorMetrics returns a fixed
// allowlist of system-level metrics only.
//
// Authentication is credential-based rather than a static token: the client
// authenticates against PocketBase's superuser auth endpoint on first use,
// caches the resulting session token in memory, and transparently
// re-authenticates before the cached token's expiry (or immediately, on a 401
// from the metrics endpoint). This mirrors OpenObserveClient's use of a
// permanent Basic Auth credential rather than an expiring session — the only
// difference is PocketBase requires a login call to mint a session token
// instead of accepting the credential directly on every request.
type BeszelClient struct {
	config           BeszelConfig
	identity         string
	password         string
	client           *http.Client
	mu               sync.Mutex
	cachedToken      string
	cachedTokenUntil time.Time
}

// tokenRefreshMargin is subtracted from a cached token's expiry so a refresh
// happens comfortably before PocketBase would reject the token, rather than
// racing it.
const tokenRefreshMargin = 60 * time.Second

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
// OpenObserve client. Credential must be an "identity:password" pair for a
// Beszel superuser account.
func NewBeszelClient(config BeszelConfig) (*BeszelClient, error) {
	if err := ValidateBackendBaseURL(config.BaseURL); err != nil {
		return nil, fmt.Errorf("observability: invalid Beszel base URL: %w", err)
	}
	identity, password, ok := strings.Cut(config.Credential, ":")
	if !ok || identity == "" || password == "" {
		return nil, fmt.Errorf("observability: invalid Beszel credential")
	}
	return &BeszelClient{config: config, identity: identity, password: password, client: boundedHTTPClient()}, nil
}

type beszelListResponse struct {
	Items []beszelSystem `json:"items"`
}

type beszelSystem struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Host    string          `json:"host"`
	Port    string          `json:"port"`
	Status  string          `json:"status"`
	Updated string          `json:"updated"`
	Info    json.RawMessage `json:"info"`
}

type beszelDetailsListResponse struct {
	Items []beszelSystemDetails `json:"items"`
}

type beszelSystemDetails struct {
	ID       string `json:"id"`
	System   string `json:"system"`
	CPU      string `json:"cpu"`
	OSName   string `json:"os_name"`
	Hostname string `json:"hostname"`
	Kernel   string `json:"kernel"`
	Arch     string `json:"arch"`
	Cores    int    `json:"cores"`
	Threads  int    `json:"threads"`
	Memory   int64  `json:"memory"`
}

type beszelStatsListResponse struct {
	Items []beszelStatRecord `json:"items"`
}

type beszelStatRecord struct {
	Created string          `json:"created"`
	System  string          `json:"system"`
	Type    string          `json:"type"`
	Stats   json.RawMessage `json:"stats"`
}

type beszelAuthResponse struct {
	Token string `json:"token"`
}

// GetNodeSystemDetails fetches all systems and their recent time-series samples
// from Beszel to power the full cluster metrics dashboard. It uses the default
// chart window; see GetNodeSystemDetailsInWindow.
func (c *BeszelClient) GetNodeSystemDetails(ctx context.Context) ([]NodeSystemDetail, error) {
	window, err := MetricsTimeRange(DefaultMetricsTimeRange, time.Now())
	if err != nil {
		return nil, err
	}
	return c.GetNodeSystemDetailsInWindow(ctx, window)
}

// GetNodeSystemDetailsInWindow is GetNodeSystemDetails with an explicit time
// window: the system_stats query carries a (created>=window.Start) filter and
// the record type matching window.ID's retention tier (see
// MetricsTimeRangeID.RecordType), and per-node histories are downsampled to at
// most MaxHistoryPoints evenly-spaced samples so long windows keep the
// rendered SVG paths small.
func (c *BeszelClient) GetNodeSystemDetailsInWindow(ctx context.Context, window TimeRange) ([]NodeSystemDetail, error) {
	token, err := c.token(ctx, false)
	if err != nil {
		return nil, err
	}
	details, unauthorized, err := c.fetchNodeSystemDetails(ctx, token, window)
	if err != nil && unauthorized {
		token, err = c.token(ctx, true)
		if err != nil {
			return nil, err
		}
		details, _, err = c.fetchNodeSystemDetails(ctx, token, window)
	}
	return details, err
}

func (c *BeszelClient) fetchNodeSystemDetails(ctx context.Context, token string, window TimeRange) ([]NodeSystemDetail, bool, error) {
	systemsURL := strings.TrimRight(c.config.BaseURL, "/") + "/api/collections/systems/records?perPage=200"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, systemsURL, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", token)
	response, err := c.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("observability: Beszel request failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return nil, true, fmt.Errorf("observability: Beszel returned HTTP 401")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, false, fmt.Errorf("observability: Beszel returned HTTP %d", response.StatusCode)
	}
	var decoded beszelListResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, MaxResponseBytes)).Decode(&decoded); err != nil {
		return nil, false, fmt.Errorf("observability: decode Beszel systems response")
	}

	// Stats are fetched per system so each node gets up to 500 of ITS OWN
	// points: one global query would let dense recent writes crowd out each
	// node's window on multi-node hubs. The perPage cap stays in place; long
	// windows are trimmed back to it by the even downsampling applied to each
	// node's history below.
	//
	// The filter pins Beszel's pre-aggregated record type for the window's
	// retention tier (see MetricsTimeRangeID.RecordType). Without it PocketBase
	// returns raw 1m rows, which are only retained densely for the recent past
	// and collapse long windows to the last ~40 minutes of data.
	recordType := window.ID.RecordType()
	statsBySystem := make(map[string][]beszelStatRecord, len(decoded.Items))
	for _, sys := range decoded.Items {
		statsValues := url.Values{"perPage": {"500"}, "sort": {"-created"}}
		filter := fmt.Sprintf(`(system="%s"&&type="%s")`, sys.ID, recordType)
		if !window.Start.IsZero() {
			// PocketBase datetime comparisons require its native storage
			// layout ("YYYY-MM-DD HH:MM:SS"); an RFC3339 literal ("...T...Z")
			// compares incorrectly and silently collapses the window to the
			// most recent handful of rows (verified against a live hub:
			// 2 of 53 matching records returned).
			filter = fmt.Sprintf(`(system="%s"&&created>="%s"&&type="%s")`, sys.ID, window.Start.UTC().Format("2006-01-02 15:04:05"), recordType)
		}
		statsValues.Set("filter", filter)
		statsURL := strings.TrimRight(c.config.BaseURL, "/") + "/api/collections/system_stats/records?" + statsValues.Encode()
		reqStats, err := http.NewRequestWithContext(ctx, http.MethodGet, statsURL, nil)
		if err != nil {
			return nil, false, err
		}
		reqStats.Header.Set("Accept", "application/json")
		reqStats.Header.Set("Authorization", token)
		respStats, err := c.client.Do(reqStats)
		if err != nil {
			return nil, false, fmt.Errorf("observability: Beszel stats request failed")
		}
		if respStats.StatusCode == http.StatusUnauthorized {
			respStats.Body.Close()
			return nil, true, fmt.Errorf("observability: Beszel stats returned HTTP 401")
		}
		var decodedStats beszelStatsListResponse
		if respStats.StatusCode >= 200 && respStats.StatusCode < 300 {
			_ = json.NewDecoder(io.LimitReader(respStats.Body, MaxResponseBytes)).Decode(&decodedStats)
		}
		respStats.Body.Close()
		statsBySystem[sys.ID] = decodedStats.Items
	}

	detailsURL := strings.TrimRight(c.config.BaseURL, "/") + "/api/collections/system_details/records?perPage=200"
	reqDetails, err := http.NewRequestWithContext(ctx, http.MethodGet, detailsURL, nil)
	if err != nil {
		return nil, false, err
	}
	reqDetails.Header.Set("Accept", "application/json")
	reqDetails.Header.Set("Authorization", token)
	respDetails, err := c.client.Do(reqDetails)
	if err != nil {
		return nil, false, fmt.Errorf("observability: Beszel details request failed")
	}
	defer respDetails.Body.Close()
	if respDetails.StatusCode == http.StatusUnauthorized {
		return nil, true, fmt.Errorf("observability: Beszel details returned HTTP 401")
	}
	var decodedDetails beszelDetailsListResponse
	if respDetails.StatusCode >= 200 && respDetails.StatusCode < 300 {
		_ = json.NewDecoder(io.LimitReader(respDetails.Body, MaxResponseBytes)).Decode(&decodedDetails)
	}
	detailsBySystem := make(map[string]beszelSystemDetails, len(decodedDetails.Items))
	for _, item := range decodedDetails.Items {
		key := item.System
		if key == "" {
			key = item.ID
		}
		detailsBySystem[key] = item
	}

	systemStatsMap := make(map[string][]SystemStatPoint, len(statsBySystem))
	for systemID, items := range statsBySystem {
		for _, item := range items {
			ts := parseBeszelTime(item.Created)
			raw := map[string]any{}
			if len(item.Stats) > 0 {
				_ = json.Unmarshal(item.Stats, &raw)
			}
			var pt SystemStatPoint
			pt.Timestamp = ts
			pt.CPU = floatField(raw["cpu"])
			if cpub, ok := raw["cpub"].([]any); ok && len(cpub) >= 3 {
				pt.CPUUser = floatField(cpub[0])
				pt.CPUSystem = floatField(cpub[1])
				pt.CPUIOWait = floatField(cpub[2])
			}
			pt.Memory = floatField(raw["mp"])
			// Beszel reports memory in GB: "m" is total, "mu" is used.
			// Store absolute used memory in MB.
			pt.MemoryMB = floatField(raw["mu"]) * 1024.0
			pt.MemoryUsed = floatField(raw["mu"])
			pt.Disk = floatField(raw["dp"])
			// Beszel reports disk in GB: "du" is used, "d" is total.
			pt.DiskUsed = floatField(raw["du"])
			pt.DiskTotal = floatField(raw["d"])
			pt.DiskRead = floatField(raw["dr"])
			pt.DiskWrite = floatField(raw["dw"])
			// "b" is [sent bytes, recv bytes]; keep the summed throughput for
			// Bandwidth and split the two directions out separately (MB/s).
			if b, ok := raw["b"].([]any); ok && len(b) >= 2 {
				pt.NetSent = floatField(b[0]) / 1024.0 / 1024.0
				pt.NetRecv = floatField(b[1]) / 1024.0 / 1024.0
				pt.Bandwidth = pt.NetSent + pt.NetRecv
			} else if bVal, ok := raw["b"].(float64); ok {
				pt.Bandwidth = bVal
			}
			if la, ok := raw["la"].([]any); ok && len(la) >= 3 {
				pt.Load1 = floatField(la[0])
				pt.Load5 = floatField(la[1])
				pt.Load15 = floatField(la[2])
			}
			pt.Temperature = floatField(raw["t"])

			systemStatsMap[systemID] = append(systemStatsMap[systemID], pt)
		}
	}

	nodes := make([]NodeSystemDetail, 0, len(decoded.Items))
	for _, sys := range decoded.Items {
		info := map[string]any{}
		if len(sys.Info) > 0 {
			_ = json.Unmarshal(sys.Info, &info)
		}
		hostDetails := detailsBySystem[sys.ID]
		detail := NodeSystemDetail{
			ID:          sys.ID,
			Name:        sys.Name,
			Host:        sys.Host,
			Port:        sys.Port,
			Status:      sys.Status,
			Updated:     parseBeszelTime(sys.Updated),
			OS:          firstNonEmpty(hostDetails.OSName, jsonString(info["o"])),
			CPUModel:    firstNonEmpty(hostDetails.CPU, jsonString(info["m"])),
			Hostname:    hostDetails.Hostname,
			Kernel:      hostDetails.Kernel,
			Arch:        hostDetails.Arch,
			Cores:       hostDetails.Cores,
			Threads:     hostDetails.Threads,
			MemoryBytes: hostDetails.Memory,
			UptimeSec:   floatField(info["u"]),
			CPUPercent:  floatField(info["cpu"]),
			MemoryUsed:  floatField(info["mp"]),
			DiskPercent: floatField(info["dp"]),
			DiskUsed:    floatField(info["du"]),
			DiskTotal:   floatField(info["d"]),
			Temperature: floatField(info["t"]),
		}
		if b, ok := info["b"].(float64); ok {
			detail.Bandwidth = b
		}
		if la, ok := info["la"].([]any); ok && len(la) >= 3 {
			detail.Load1 = floatField(la[0])
			detail.Load5 = floatField(la[1])
			detail.Load15 = floatField(la[2])
		}
		hist := systemStatsMap[sys.ID]
		// Reverse to chronological order (oldest -> newest) for plotting
		for i, j := 0, len(hist)-1; i < j; i, j = i+1, j-1 {
			hist[i], hist[j] = hist[j], hist[i]
		}
		detail.History = downsampleHistory(hist, MaxHistoryPoints)
		nodes = append(nodes, detail)
	}

	return nodes, false, nil
}

// GetOperatorMetrics fetches the operator's host systems and projects only the
// allowlisted system-level metrics from each. No tenant parameters are accepted
// or forwarded. It authenticates lazily (caching the session token) and
// transparently re-authenticates once if the cached token is rejected.
func (c *BeszelClient) GetOperatorMetrics(ctx context.Context) ([]OperatorMetric, error) {
	token, err := c.token(ctx, false)
	if err != nil {
		return nil, err
	}
	metrics, unauthorized, err := c.fetchOperatorMetrics(ctx, token)
	if err != nil && unauthorized {
		token, err = c.token(ctx, true)
		if err != nil {
			return nil, err
		}
		metrics, _, err = c.fetchOperatorMetrics(ctx, token)
	}
	return metrics, err
}

// fetchOperatorMetrics performs one authenticated request. The bool result
// reports whether the failure was a 401 (and therefore worth one re-auth retry
// by the caller) as opposed to any other error.
func (c *BeszelClient) fetchOperatorMetrics(ctx context.Context, token string) ([]OperatorMetric, bool, error) {
	endpoint := strings.TrimRight(c.config.BaseURL, "/") + "/api/collections/systems/records?perPage=200"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", token)
	response, err := c.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("observability: Beszel request failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return nil, true, fmt.Errorf("observability: Beszel returned HTTP 401")
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, false, errors.New("observability: Beszel returned a redirect")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, false, fmt.Errorf("observability: Beszel returned HTTP %d", response.StatusCode)
	}
	var decoded beszelListResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, MaxResponseBytes)).Decode(&decoded); err != nil {
		return nil, false, fmt.Errorf("observability: decode Beszel response")
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
	return metrics, false, nil
}

// token returns a cached, still-valid session token, or authenticates for a
// new one if forceRefresh is set or the cached token is missing/near expiry.
func (c *BeszelClient) token(ctx context.Context, forceRefresh bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !forceRefresh && c.cachedToken != "" && time.Now().Before(c.cachedTokenUntil.Add(-tokenRefreshMargin)) {
		return c.cachedToken, nil
	}
	token, expiry, err := c.authenticate(ctx)
	if err != nil {
		return "", err
	}
	c.cachedToken = token
	c.cachedTokenUntil = expiry
	return token, nil
}

// authenticate logs in as the configured Beszel superuser and returns the
// session token together with its expiry, decoded from the JWT's `exp` claim.
// The signature is not verified client-side: the token is only ever used over
// the same HTTPS channel it was issued on, so there is nothing to gain from
// verifying it locally (the same trust posture OpenObserveClient applies to
// its Basic Auth credential).
func (c *BeszelClient) authenticate(ctx context.Context) (string, time.Time, error) {
	body, err := json.Marshal(map[string]string{"identity": c.identity, "password": c.password})
	if err != nil {
		return "", time.Time{}, err
	}
	endpoint := strings.TrimRight(c.config.BaseURL, "/") + "/api/collections/_superusers/auth-with-password"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := c.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("observability: Beszel auth request failed")
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return "", time.Time{}, errors.New("observability: Beszel auth returned a redirect")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", time.Time{}, fmt.Errorf("observability: Beszel auth returned HTTP %d", response.StatusCode)
	}
	var decoded beszelAuthResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, MaxResponseBytes)).Decode(&decoded); err != nil {
		return "", time.Time{}, fmt.Errorf("observability: decode Beszel auth response")
	}
	if decoded.Token == "" {
		return "", time.Time{}, errors.New("observability: Beszel auth response missing token")
	}
	expiry, err := jwtExpiry(decoded.Token)
	if err != nil {
		return "", time.Time{}, err
	}
	return decoded.Token, expiry, nil
}

// jwtExpiry decodes the `exp` claim from a JWT without verifying its
// signature (see authenticate's doc comment for why that's an acceptable
// trust posture here).
func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("observability: malformed Beszel session token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("observability: decode Beszel session token: %w", err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("observability: decode Beszel session token claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, errors.New("observability: Beszel session token has no expiry claim")
	}
	return time.Unix(claims.Exp, 0), nil
}

func parseBeszelTime(value string) time.Time {
	for _, layout := range []string{"2006-01-02 15:04:05.999Z", time.RFC3339, "2006-01-02 15:04:05.999999Z07:00"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func jsonString(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
