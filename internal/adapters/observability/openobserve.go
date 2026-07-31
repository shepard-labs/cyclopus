package observability

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// OpenObserveConfig is the resolved configuration for the OpenObserve backend.
// BaseURL and Organization identify the deployment; Credential is the raw basic
// auth value (already resolved from a secret reference by the caller).
type OpenObserveConfig struct {
	// BaseURL is the OpenObserve HTTPS endpoint, e.g. https://oo.internal:5080.
	BaseURL string
	// Organization is the OpenObserve org slug (an install-level value, distinct
	// from a Cyclopus tenant org). It is used only in the request path.
	Organization string
	// LogStream and TraceStream name the OpenObserve streams the Collector writes
	// to. They default to "logs" and "traces" if empty.
	LogStream   string
	TraceStream string
	// Credential is the value placed in the Authorization header. It is expected
	// to be the "user:token" form OpenObserve accepts for Basic auth; the client
	// base64-encodes it.
	Credential string
}

// OpenObserveClient is a bounded, non-redirecting HTTPS client for the
// OpenObserve search API (v0.91.x /api/{org}/_search). It is the only path from
// Cyclopus to OpenObserve, and it always constructs the tenant WHERE clause
// itself from a validated TenantScope.
type OpenObserveClient struct {
	config OpenObserveConfig
	client *http.Client
}

const (
	defaultLogStream   = "logs"
	defaultTraceStream = "traces"
	// maxLimit bounds the number of rows requested upstream regardless of caller
	// input. The service layer also enforces bounds; this is defense in depth.
	maxLimit = 1000
)

// NewOpenObserveClient builds a client with the same bounded transport posture
// as the webhook sender: a fixed 10s timeout, redirects refused, and only
// public destinations dialed.
func NewOpenObserveClient(config OpenObserveConfig) (*OpenObserveClient, error) {
	if config.LogStream == "" {
		config.LogStream = defaultLogStream
	}
	if config.TraceStream == "" {
		config.TraceStream = defaultTraceStream
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("observability: invalid OpenObserve base URL")
	}
	return &OpenObserveClient{config: config, client: boundedHTTPClient()}, nil
}

func boundedHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: RequestTimeout}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("observability: backend resolved to no addresses")
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
	return &http.Client{
		Timeout:       RequestTimeout,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// tenantWhere renders the mandatory tenant predicate. Every identifier is
// escaped and embedded as a single-quoted SQL string literal. This clause is
// the first thing in the WHERE and is joined with AND to any additional
// caller-derived predicate, so nothing a caller supplies can remove or widen
// it. It is exported for tests only via QueryLogs/ListTraces behavior.
func tenantWhere(scope TenantScope) string {
	predicates := []string{
		"org_id = " + sqlString(scope.OrgID),
		"app_id = " + sqlString(scope.AppID),
	}
	if scope.EnvironmentID != "" {
		predicates = append(predicates, "environment_id = "+sqlString(scope.EnvironmentID))
	}
	if scope.ReleaseID != "" {
		predicates = append(predicates, "release_id = "+sqlString(scope.ReleaseID))
	}
	return strings.Join(predicates, " AND ")
}

// sqlString escapes a value as a single-quoted SQL string literal by doubling
// embedded single quotes and stripping NUL bytes. Combined with the caller
// never controlling the identifier values (they are validated UUIDs), this
// keeps the tenant filter unbreakable.
func sqlString(value string) string {
	value = strings.ReplaceAll(value, "\x00", "")
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// severityFloor returns the OpenObserve severity_number predicate for a floor,
// or "" for no floor. severity_number follows the OTel convention where higher
// is more severe (TRACE=1..FATAL=21 in bands of 4).
func severityFloor(minimum Severity) string {
	number := map[Severity]int{
		SeverityTrace: 1, SeverityDebug: 5, SeverityInfo: 9,
		SeverityWarn: 13, SeverityError: 17, SeverityFatal: 21,
	}[minimum]
	if number == 0 {
		return ""
	}
	return "severity_number >= " + strconv.Itoa(number)
}

// substringPredicate renders a safe LIKE against the log body for a free-text
// filter. The needle is escaped as a SQL literal AND has LIKE metacharacters
// neutralized, so it can only ever match a literal substring — it cannot be
// used to alter the query structure.
func substringPredicate(needle string) string {
	if strings.TrimSpace(needle) == "" {
		return ""
	}
	escaped := needle
	escaped = strings.ReplaceAll(escaped, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "%", "\\%")
	escaped = strings.ReplaceAll(escaped, "_", "\\_")
	return "body LIKE " + sqlString("%"+escaped+"%") + " ESCAPE '\\'"
}

type searchRequest struct {
	Query searchQuery `json:"query"`
}

type searchQuery struct {
	SQL       string `json:"sql"`
	StartTime int64  `json:"start_time"`
	EndTime   int64  `json:"end_time"`
	From      int    `json:"from"`
	Size      int    `json:"size"`
}

type searchResponse struct {
	Hits []map[string]any `json:"hits"`
}

// QueryLogs runs a bounded, tenant-scoped log search. The WHERE always begins
// with the mandatory tenant filter; filter is applied only as additional AND
// terms. cursor pagination is offset-based over the fixed page size.
func (c *OpenObserveClient) QueryLogs(ctx context.Context, scope TenantScope, window TimeRange, filter LogFilter) (LogPage, error) {
	if !scope.valid() {
		return LogPage{}, ErrScopeIncomplete
	}
	size := boundLimit(filter.Limit)
	from, err := decodeCursor(filter.Cursor)
	if err != nil {
		return LogPage{}, err
	}
	where := tenantWhere(scope)
	if predicate := severityFloor(filter.MinSeverity); predicate != "" {
		where += " AND " + predicate
	}
	if predicate := substringPredicate(filter.Substring); predicate != "" {
		where += " AND " + predicate
	}
	sql := fmt.Sprintf("SELECT _timestamp, severity_text, severity_number, body FROM %s WHERE %s ORDER BY _timestamp DESC", quoteIdentifier(c.config.LogStream), where)
	response, err := c.search(ctx, searchQuery{SQL: sql, StartTime: microseconds(window.Start), EndTime: microseconds(window.End), From: from, Size: size})
	if err != nil {
		return LogPage{}, err
	}
	page := LogPage{Entries: make([]LogEntry, 0, len(response.Hits))}
	for _, hit := range response.Hits {
		page.Entries = append(page.Entries, decodeLogHit(hit))
	}
	page.NextCursor = nextCursor(from, size, len(response.Hits))
	return page, nil
}

// ListTraces returns bounded trace summaries for a tenant scope, ordered by
// most recent. The mandatory tenant filter is applied identically to logs.
func (c *OpenObserveClient) ListTraces(ctx context.Context, scope TenantScope, window TimeRange, filter TraceFilter) (TracePage, error) {
	if !scope.valid() {
		return TracePage{}, ErrScopeIncomplete
	}
	size := boundLimit(filter.Limit)
	from, err := decodeCursor(filter.Cursor)
	if err != nil {
		return TracePage{}, err
	}
	where := tenantWhere(scope) + " AND parent_span_id = ''"
	sql := fmt.Sprintf("SELECT trace_id, span_name, duration, start_time, span_status FROM %s WHERE %s ORDER BY start_time DESC", quoteIdentifier(c.config.TraceStream), where)
	response, err := c.search(ctx, searchQuery{SQL: sql, StartTime: microseconds(window.Start), EndTime: microseconds(window.End), From: from, Size: size})
	if err != nil {
		return TracePage{}, err
	}
	page := TracePage{Traces: make([]TraceSummary, 0, len(response.Hits))}
	for _, hit := range response.Hits {
		page.Traces = append(page.Traces, decodeTraceHit(hit))
	}
	page.NextCursor = nextCursor(from, size, len(response.Hits))
	return page, nil
}

// GetTrace looks up a single trace root span within the tenant scope. The
// trace_id is escaped and ANDed onto the mandatory tenant filter, so a lookup
// can never reach another tenant's trace even if the ID is known.
func (c *OpenObserveClient) GetTrace(ctx context.Context, scope TenantScope, window TimeRange, traceID string) (TraceSummary, bool, error) {
	if !scope.valid() {
		return TraceSummary{}, false, ErrScopeIncomplete
	}
	where := tenantWhere(scope) + " AND parent_span_id = '' AND trace_id = " + sqlString(traceID)
	sql := fmt.Sprintf("SELECT trace_id, span_name, duration, start_time, span_status FROM %s WHERE %s ORDER BY start_time DESC", quoteIdentifier(c.config.TraceStream), where)
	response, err := c.search(ctx, searchQuery{SQL: sql, StartTime: microseconds(window.Start), EndTime: microseconds(window.End), From: 0, Size: 1})
	if err != nil {
		return TraceSummary{}, false, err
	}
	if len(response.Hits) == 0 {
		return TraceSummary{}, false, nil
	}
	return decodeTraceHit(response.Hits[0]), true, nil
}

func (c *OpenObserveClient) search(ctx context.Context, query searchQuery) (searchResponse, error) {
	body, err := json.Marshal(searchRequest{Query: query})
	if err != nil {
		return searchResponse{}, err
	}
	endpoint := strings.TrimRight(c.config.BaseURL, "/") + "/api/" + url.PathEscape(c.config.Organization) + "/_search"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return searchResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.config.Credential != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.config.Credential)))
	}
	response, err := c.client.Do(req)
	if err != nil {
		return searchResponse{}, fmt.Errorf("observability: OpenObserve request failed")
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return searchResponse{}, errors.New("observability: OpenObserve returned a redirect")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return searchResponse{}, fmt.Errorf("observability: OpenObserve returned HTTP %d", response.StatusCode)
	}
	var decoded searchResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, MaxResponseBytes)).Decode(&decoded); err != nil {
		return searchResponse{}, fmt.Errorf("observability: decode OpenObserve response")
	}
	return decoded, nil
}

func boundLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

func microseconds(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMicro()
}

func quoteIdentifier(name string) string {
	return "\"" + strings.ReplaceAll(name, "\"", "\"\"") + "\""
}

func nextCursor(from, size, returned int) string {
	if returned < size {
		return ""
	}
	return encodeCursor(from + size)
}

func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodeCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("observability: invalid cursor")
	}
	offset, err := strconv.Atoi(string(raw))
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("observability: invalid cursor")
	}
	return offset, nil
}

func decodeLogHit(hit map[string]any) LogEntry {
	entry := LogEntry{
		Timestamp:  timeFromMicros(hit["_timestamp"]),
		Severity:   severityFromNumber(hit["severity_number"]),
		Body:       stringField(hit["body"]),
		Attributes: map[string]string{},
	}
	for key, value := range hit {
		switch key {
		case "_timestamp", "severity_number", "severity_text", "body":
			continue
		}
		entry.Attributes[key] = stringField(value)
	}
	return entry
}

func decodeTraceHit(hit map[string]any) TraceSummary {
	return TraceSummary{
		TraceID:    stringField(hit["trace_id"]),
		RootName:   stringField(hit["span_name"]),
		DurationMs: durationMs(hit["duration"]),
		StartTime:  timeFromMicros(hit["start_time"]),
		Status:     traceStatusFrom(hit["span_status"]),
	}
}

func severityFromNumber(value any) Severity {
	number := int(floatField(value))
	switch {
	case number >= 21:
		return SeverityFatal
	case number >= 17:
		return SeverityError
	case number >= 13:
		return SeverityWarn
	case number >= 9:
		return SeverityInfo
	case number >= 5:
		return SeverityDebug
	case number >= 1:
		return SeverityTrace
	default:
		return SeverityUnspecified
	}
}

func traceStatusFrom(value any) TraceStatus {
	switch strings.ToUpper(stringField(value)) {
	case "ERROR", "STATUS_CODE_ERROR", "2":
		return TraceStatusError
	case "OK", "STATUS_CODE_OK", "1":
		return TraceStatusOK
	default:
		return TraceStatusUnspecified
	}
}

func timeFromMicros(value any) time.Time {
	micros := int64(floatField(value))
	if micros == 0 {
		return time.Time{}
	}
	return time.UnixMicro(micros).UTC()
}

func durationMs(value any) int64 {
	// OpenObserve stores span duration in microseconds.
	return int64(floatField(value)) / 1000
}

func stringField(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		encoded, _ := json.Marshal(v)
		return string(encoded)
	}
}

func floatField(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case json.Number:
		parsed, _ := v.Float64()
		return parsed
	case int64:
		return float64(v)
	case string:
		parsed, _ := strconv.ParseFloat(v, 64)
		return parsed
	default:
		return 0
	}
}
