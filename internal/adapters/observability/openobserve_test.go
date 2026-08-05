package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captureServer stands in for OpenObserve. It records the last search SQL and
// returns a fixed hit set so tests can assert what Cyclopus sent upstream.
type captureServer struct {
	server  *httptest.Server
	lastSQL string
	lastReq searchQuery
	hits    []map[string]any
}

func newCaptureServer(t *testing.T, hits []map[string]any) *captureServer {
	t.Helper()
	capture := &captureServer{hits: hits}
	capture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request searchRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode search request: %v", err)
		}
		capture.lastSQL = request.Query.SQL
		capture.lastReq = request.Query
		_ = json.NewEncoder(w).Encode(searchResponse{Hits: capture.hits})
	}))
	t.Cleanup(capture.server.Close)
	return capture
}

// newTestClient builds a client that trusts the plaintext httptest server by
// pointing the bounded HTTP client at it directly. The production constructor
// requires HTTPS; here we bypass just the URL scheme check for the test double.
func newTestClient(t *testing.T, capture *captureServer) *OpenObserveClient {
	t.Helper()
	return &OpenObserveClient{
		config: OpenObserveConfig{BaseURL: capture.server.URL, Organization: "operator", LogStream: "logs", TraceStream: "traces"},
		client: capture.server.Client(),
	}
}

func window() TimeRange {
	return TimeRange{Start: time.Unix(1000, 0), End: time.Unix(2000, 0)}
}

func TestOpenObserveQueryLogsAlwaysIncludesTenantFilter(t *testing.T) {
	capture := newCaptureServer(t, nil)
	client := newTestClient(t, capture)
	scope := TenantScope{OrgID: "org-1", AppID: "app-9", EnvironmentID: "env-3", ReleaseID: "rel-7"}
	if _, err := client.QueryLogs(context.Background(), scope, window(), LogFilter{}); err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	for _, want := range []string{
		"org_id = 'org-1'",
		"app_id = 'app-9'",
		"environment_id = 'env-3'",
		"release_id = 'rel-7'",
	} {
		if !strings.Contains(capture.lastSQL, want) {
			t.Fatalf("tenant predicate %q missing from SQL: %s", want, capture.lastSQL)
		}
	}
	if !strings.Contains(capture.lastSQL, "WHERE org_id = 'org-1' AND app_id = 'app-9'") {
		t.Fatalf("tenant filter is not the leading WHERE clause: %s", capture.lastSQL)
	}
}

func TestOpenObserveResponseBodyIsBounded(t *testing.T) {
	// A misbehaving backend streams far more than MaxResponseBytes of a single
	// unterminated JSON string; the LimitReader must cut it off (decode error)
	// rather than letting the decoder allocate unboundedly.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"hits":[{"body":"`))
		chunk := make([]byte, 64<<10)
		for i := range chunk {
			chunk[i] = 'a'
		}
		for written := 0; written < MaxResponseBytes+(1<<20); written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	client := &OpenObserveClient{
		config: OpenObserveConfig{BaseURL: server.URL, Organization: "operator", LogStream: "logs", TraceStream: "traces"},
		client: server.Client(),
	}
	scope := TenantScope{OrgID: "org-1", AppID: "app-9"}
	if _, err := client.QueryLogs(context.Background(), scope, window(), LogFilter{}); err == nil {
		t.Fatal("expected a decode error when the response exceeds MaxResponseBytes")
	}
}

func TestOpenObserveMaliciousQueryCannotEscapeTenantFilter(t *testing.T) {
	capture := newCaptureServer(t, nil)
	client := newTestClient(t, capture)
	scope := TenantScope{OrgID: "org-1", AppID: "app-9"}
	// A hostile free-text filter attempting to terminate the string and drop the
	// tenant scope. The adapter must escape it and keep the tenant WHERE intact.
	malicious := "' OR '1'='1' -- "
	if _, err := client.QueryLogs(context.Background(), scope, window(), LogFilter{Substring: malicious}); err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if !strings.Contains(capture.lastSQL, "org_id = 'org-1' AND app_id = 'app-9'") {
		t.Fatalf("tenant filter removed by malicious query: %s", capture.lastSQL)
	}
	// The single quotes in the needle must be doubled, never left to break out.
	if strings.Contains(capture.lastSQL, "OR '1'='1'") && !strings.Contains(capture.lastSQL, "''1''=''1''") {
		t.Fatalf("malicious quotes were not escaped: %s", capture.lastSQL)
	}
	// The LIKE predicate must be an additional AND, never replacing the tenant WHERE.
	if !strings.Contains(capture.lastSQL, "AND body LIKE") {
		t.Fatalf("substring filter not applied as an additional AND: %s", capture.lastSQL)
	}
}

func TestOpenObserveTenantIdentifierQuotesAreEscaped(t *testing.T) {
	capture := newCaptureServer(t, nil)
	client := newTestClient(t, capture)
	// Even if an identifier somehow contained a quote, it must be doubled.
	scope := TenantScope{OrgID: "o'r'g", AppID: "app"}
	if _, err := client.QueryLogs(context.Background(), scope, window(), LogFilter{}); err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if !strings.Contains(capture.lastSQL, "org_id = 'o''r''g'") {
		t.Fatalf("identifier quotes not escaped: %s", capture.lastSQL)
	}
}

func TestOpenObserveSeverityFloorApplied(t *testing.T) {
	capture := newCaptureServer(t, nil)
	client := newTestClient(t, capture)
	scope := TenantScope{OrgID: "o", AppID: "a"}
	if _, err := client.QueryLogs(context.Background(), scope, window(), LogFilter{MinSeverity: SeverityError}); err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if !strings.Contains(capture.lastSQL, "AND severity_number >= 17") {
		t.Fatalf("severity floor missing: %s", capture.lastSQL)
	}
}

func TestOpenObserveRejectsIncompleteScope(t *testing.T) {
	capture := newCaptureServer(t, nil)
	client := newTestClient(t, capture)
	if _, err := client.QueryLogs(context.Background(), TenantScope{OrgID: "o"}, window(), LogFilter{}); err != ErrScopeIncomplete {
		t.Fatalf("expected ErrScopeIncomplete, got %v", err)
	}
}

func TestOpenObserveCursorPagination(t *testing.T) {
	// Return a full page so a next cursor is produced.
	hits := make([]map[string]any, 2)
	for i := range hits {
		hits[i] = map[string]any{"_timestamp": float64(1_000_000), "severity_number": float64(9), "body": "line"}
	}
	capture := newCaptureServer(t, hits)
	client := newTestClient(t, capture)
	scope := TenantScope{OrgID: "o", AppID: "a"}
	page, err := client.QueryLogs(context.Background(), scope, window(), LogFilter{Limit: 2})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if page.NextCursor == "" {
		t.Fatal("expected a next cursor when a full page is returned")
	}
	if capture.lastReq.From != 0 || capture.lastReq.Size != 2 {
		t.Fatalf("unexpected paging window: from=%d size=%d", capture.lastReq.From, capture.lastReq.Size)
	}
	// Following the cursor advances the offset.
	if _, err := client.QueryLogs(context.Background(), scope, window(), LogFilter{Limit: 2, Cursor: page.NextCursor}); err != nil {
		t.Fatalf("QueryLogs page 2: %v", err)
	}
	if capture.lastReq.From != 2 {
		t.Fatalf("cursor did not advance offset: from=%d", capture.lastReq.From)
	}
}

func TestOpenObserveGetTraceScopesById(t *testing.T) {
	capture := newCaptureServer(t, []map[string]any{{"trace_id": "abc", "span_name": "GET /", "duration": float64(5_000), "start_time": float64(1_000_000), "span_status": "OK"}})
	client := newTestClient(t, capture)
	scope := TenantScope{OrgID: "o", AppID: "a"}
	summary, found, err := client.GetTrace(context.Background(), scope, window(), "abc")
	if err != nil || !found {
		t.Fatalf("GetTrace: found=%v err=%v", found, err)
	}
	if summary.DurationMs != 5 {
		t.Fatalf("duration not converted to ms: %d", summary.DurationMs)
	}
	if !strings.Contains(capture.lastSQL, "trace_id = 'abc'") || !strings.Contains(capture.lastSQL, "org_id = 'o' AND app_id = 'a'") {
		t.Fatalf("trace lookup missing tenant+id filter: %s", capture.lastSQL)
	}
}

func TestOpenObserveInvalidCursorRejected(t *testing.T) {
	capture := newCaptureServer(t, nil)
	client := newTestClient(t, capture)
	scope := TenantScope{OrgID: "o", AppID: "a"}
	if _, err := client.QueryLogs(context.Background(), scope, window(), LogFilter{Cursor: "!!!not-base64!!!"}); err == nil {
		t.Fatal("expected invalid cursor to be rejected")
	}
}

func TestOpenObserveConstructorRequiresHTTPS(t *testing.T) {
	if _, err := NewOpenObserveClient(OpenObserveConfig{BaseURL: "http://insecure.example", Organization: "o"}); err == nil {
		t.Fatal("expected non-HTTPS base URL to be rejected")
	}
	if _, err := NewOpenObserveClient(OpenObserveConfig{BaseURL: "https://oo.example:5080", Organization: "o"}); err != nil {
		t.Fatalf("valid HTTPS config rejected: %v", err)
	}
	// §8 keeps OpenObserve in-cluster only, so the installer points the API at
	// its ClusterIP Service; that address is plaintext by design.
	if _, err := NewOpenObserveClient(OpenObserveConfig{BaseURL: "http://openobserve.cyclopus.svc:5080", Organization: "o"}); err != nil {
		t.Fatalf("in-cluster .svc config rejected: %v", err)
	}
}

func TestOpenObserveClientRefusesRedirects(t *testing.T) {
	client := boundedHTTPClient()
	if client.Timeout != RequestTimeout {
		t.Fatalf("expected %s timeout, got %s", RequestTimeout, client.Timeout)
	}
	if err := client.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("expected redirects to be refused, got %v", err)
	}
}

func TestOpenObserveNon2xxIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	client := &OpenObserveClient{config: OpenObserveConfig{BaseURL: server.URL, Organization: "o", LogStream: "logs"}, client: server.Client()}
	if _, err := client.QueryLogs(context.Background(), TenantScope{OrgID: "o", AppID: "a"}, window(), LogFilter{}); err == nil {
		t.Fatal("expected an error on HTTP 500")
	}
}
