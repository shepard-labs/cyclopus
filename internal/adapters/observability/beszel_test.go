package observability

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeJWT builds an unsigned-but-well-formed JWT carrying only an exp claim,
// sufficient for jwtExpiry to decode. Signature verification is intentionally
// not part of the client (see authenticate's doc comment), so tests don't need
// a real PocketBase signing key.
func fakeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return header + "." + payload + ".sig"
}

// unauthedTestClient builds a BeszelClient with a pre-seeded, far-future
// cached token so GetOperatorMetrics skips authentication entirely — for
// tests that only care about the systems-fetch behavior.
func unauthedTestClient(serverURL string, httpClient *http.Client) *BeszelClient {
	return &BeszelClient{
		config:           BeszelConfig{BaseURL: serverURL},
		client:           httpClient,
		cachedToken:      "test-token",
		cachedTokenUntil: time.Now().Add(time.Hour),
	}
}

func TestBeszelGetOperatorMetricsAllowlistOnly(t *testing.T) {
	var requestPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		_, _ = w.Write([]byte(`{"items":[{"name":"node-1","updated":"2026-01-02 03:04:05.000Z","info":{"cpu":42.5,"mp":80,"secret_internal":"leak","container_id":"abc"}}]}`))
	}))
	t.Cleanup(server.Close)
	client := unauthedTestClient(server.URL, server.Client())

	metrics, err := client.GetOperatorMetrics(context.Background())
	if err != nil {
		t.Fatalf("GetOperatorMetrics: %v", err)
	}
	if !strings.HasPrefix(requestPath, "/api/collections/systems/records") {
		t.Fatalf("unexpected Beszel path: %s", requestPath)
	}
	names := map[string]bool{}
	for _, metric := range metrics {
		names[metric.Name] = true
		if strings.Contains(metric.Name, "secret") || strings.Contains(metric.Name, "container") {
			t.Fatalf("non-allowlisted metric leaked: %s", metric.Name)
		}
	}
	if !names["node-1/cpu.usage"] || !names["node-1/memory.usage"] {
		t.Fatalf("expected allowlisted metrics, got %v", names)
	}
}

func TestBeszelNoTenantParameters(t *testing.T) {
	var rawQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(server.Close)
	client := unauthedTestClient(server.URL, server.Client())
	if _, err := client.GetOperatorMetrics(context.Background()); err != nil {
		t.Fatalf("GetOperatorMetrics: %v", err)
	}
	for _, forbidden := range []string{"org_id", "app_id", "environment_id", "tenant"} {
		if strings.Contains(rawQuery, forbidden) {
			t.Fatalf("Beszel request carried a tenant parameter %q: %s", forbidden, rawQuery)
		}
	}
}

func TestBeszelConstructorRequiresHTTPS(t *testing.T) {
	if _, err := NewBeszelClient(BeszelConfig{BaseURL: "http://insecure.example", Credential: "admin:password123"}); err == nil {
		t.Fatal("expected non-HTTPS Beszel URL to be rejected")
	}
	// §8 keeps Beszel in-cluster only; its ClusterIP Service is plaintext.
	if _, err := NewBeszelClient(BeszelConfig{BaseURL: "http://beszel.cyclopus.svc:8090", Credential: "admin:password123"}); err != nil {
		t.Fatalf("in-cluster .svc Beszel config rejected: %v", err)
	}
	if _, err := NewBeszelClient(BeszelConfig{BaseURL: "https://beszel.example", Credential: "admin:password123"}); err != nil {
		t.Fatalf("valid HTTPS Beszel config rejected: %v", err)
	}
}

func TestBeszelConstructorRequiresCredential(t *testing.T) {
	cases := []struct {
		name       string
		credential string
		wantErr    bool
	}{
		{"missing colon", "admin-no-password", true},
		{"empty identity", ":password123", true},
		{"empty password", "admin:", true},
		{"empty credential", "", true},
		{"valid", "admin@cyclopus.local:password123", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewBeszelClient(BeszelConfig{BaseURL: "https://beszel.example", Credential: tc.credential})
			if tc.wantErr && err == nil {
				t.Fatalf("credential %q: expected error, got none", tc.credential)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("credential %q: unexpected error: %v", tc.credential, err)
			}
		})
	}
}

func TestBeszelNon2xxIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	client := unauthedTestClient(server.URL, server.Client())
	if _, err := client.GetOperatorMetrics(context.Background()); err == nil {
		t.Fatal("expected an error on HTTP 502")
	}
}

// beszelAuthServer builds a test hub that handles both the superuser
// auth-with-password login and the systems listing, so re-authentication
// behavior can be exercised end to end.
type beszelAuthServer struct {
	authCalls   int
	fetchCalls  int
	tokenExpiry time.Time
	// wantToken, if set, is checked against every systems request; a mismatch
	// responds 401 so re-auth-on-401 behavior can be tested.
	validToken string
}

func (s *beszelAuthServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/collections/_superusers/auth-with-password":
			s.authCalls++
			var body struct{ Identity, Password string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode auth request: %v", err)
			}
			if body.Identity != "admin@cyclopus.local" || body.Password != "s3cret-pw" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			token := fakeJWT(t, s.tokenExpiry)
			s.validToken = token
			_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/collections/systems/records"):
			s.fetchCalls++
			if r.Header.Get("Authorization") != s.validToken || s.validToken == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// httptestBeszelClient builds a BeszelClient talking to an httptest server
// (plain HTTP) with the given credential, bypassing NewBeszelClient's HTTPS
// requirement — HTTPS enforcement is covered separately by
// TestBeszelConstructorRequiresHTTPS.
func httptestBeszelClient(serverURL string, httpClient *http.Client, identity, password string) *BeszelClient {
	return &BeszelClient{
		config:   BeszelConfig{BaseURL: serverURL, Credential: identity + ":" + password},
		identity: identity,
		password: password,
		client:   httpClient,
	}
}

func TestBeszelAuthenticatesAndCachesToken(t *testing.T) {
	auth := &beszelAuthServer{tokenExpiry: time.Now().Add(time.Hour)}
	server := httptest.NewServer(auth.handler(t))
	t.Cleanup(server.Close)

	client := httptestBeszelClient(server.URL, server.Client(), "admin@cyclopus.local", "s3cret-pw")

	for range 2 {
		if _, err := client.GetOperatorMetrics(context.Background()); err != nil {
			t.Fatalf("GetOperatorMetrics: %v", err)
		}
	}
	if auth.authCalls != 1 {
		t.Fatalf("expected 1 auth call (cached token reused), got %d", auth.authCalls)
	}
	if auth.fetchCalls != 2 {
		t.Fatalf("expected 2 systems fetches, got %d", auth.fetchCalls)
	}
}

func TestBeszelReauthenticatesBeforeExpiry(t *testing.T) {
	auth := &beszelAuthServer{tokenExpiry: time.Now().Add(30 * time.Second)} // inside tokenRefreshMargin
	server := httptest.NewServer(auth.handler(t))
	t.Cleanup(server.Close)

	client := httptestBeszelClient(server.URL, server.Client(), "admin@cyclopus.local", "s3cret-pw")

	for range 2 {
		if _, err := client.GetOperatorMetrics(context.Background()); err != nil {
			t.Fatalf("GetOperatorMetrics: %v", err)
		}
	}
	if auth.authCalls != 2 {
		t.Fatalf("expected 2 auth calls (near-expiry token not reused), got %d", auth.authCalls)
	}
}

func TestBeszelReauthenticatesOn401(t *testing.T) {
	auth := &beszelAuthServer{tokenExpiry: time.Now().Add(time.Hour)}
	server := httptest.NewServer(auth.handler(t))
	t.Cleanup(server.Close)

	client := httptestBeszelClient(server.URL, server.Client(), "admin@cyclopus.local", "s3cret-pw")

	// Prime a cached token, then invalidate it server-side (simulating a
	// revoked/stale session) so the next call must observe a 401 and recover.
	if _, err := client.GetOperatorMetrics(context.Background()); err != nil {
		t.Fatalf("GetOperatorMetrics (priming): %v", err)
	}
	auth.validToken = "revoked"

	if _, err := client.GetOperatorMetrics(context.Background()); err != nil {
		t.Fatalf("GetOperatorMetrics (after revocation): %v", err)
	}
	if auth.authCalls != 2 {
		t.Fatalf("expected re-auth after a 401, got %d auth calls", auth.authCalls)
	}
}

func TestBeszelAuthFailureIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	client := httptestBeszelClient(server.URL, server.Client(), "admin@cyclopus.local", "wrong-pw")

	if _, err := client.GetOperatorMetrics(context.Background()); err == nil {
		t.Fatal("expected an error when Beszel rejects the superuser credential")
	}
}

func TestBeszelGetNodeSystemDetailsHostIdentity(t *testing.T) {
	t.Run("from system_details collection", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/api/collections/systems/records"):
				_, _ = w.Write([]byte(`{"items":[{"id":"sys-1","name":"node-1","host":"192.168.1.10","port":"45876","status":"up","updated":"2026-01-02 03:04:05.000Z","info":{"u":604800,"cpu":12.5}}]}`))
			case strings.HasPrefix(r.URL.Path, "/api/collections/system_stats/records"):
				_, _ = w.Write([]byte(`{"items":[]}`))
			case strings.HasPrefix(r.URL.Path, "/api/collections/system_details/records"):
				_, _ = w.Write([]byte(`{"items":[{"id":"sys-1","system":"sys-1","os_name":"Debian GNU/Linux 13 (trixie)","cpu":"Intel Core Processor (Haswell, no TSX)","hostname":"vps-99e943cd","kernel":"6.12.41","arch":"x86_64","cores":4,"threads":8,"memory":12240656793}]}`))
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		assertHostIdentity(t, unauthedTestClient(server.URL, server.Client()))
	})

	t.Run("falls back to info o/m", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/api/collections/systems/records"):
				_, _ = w.Write([]byte(`{"items":[{"id":"sys-1","name":"node-1","host":"192.168.1.10","port":"45876","status":"up","updated":"2026-01-02 03:04:05.000Z","info":{"u":604800,"o":"Debian GNU/Linux 13 (trixie)","m":"Intel Core Processor (Haswell, no TSX)","cpu":12.5,"secret_internal":"leak"}}]}`))
			case strings.HasPrefix(r.URL.Path, "/api/collections/system_stats/records"):
				_, _ = w.Write([]byte(`{"items":[]}`))
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		assertHostIdentity(t, unauthedTestClient(server.URL, server.Client()))
	})

	t.Run("prefers system_details over info", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/api/collections/systems/records"):
				_, _ = w.Write([]byte(`{"items":[{"id":"sys-1","name":"node-1","host":"192.168.1.10","port":"45876","status":"up","updated":"2026-01-02 03:04:05.000Z","info":{"u":604800,"o":"old-os","m":"old-cpu"}}]}`))
			case strings.HasPrefix(r.URL.Path, "/api/collections/system_stats/records"):
				_, _ = w.Write([]byte(`{"items":[]}`))
			case strings.HasPrefix(r.URL.Path, "/api/collections/system_details/records"):
				_, _ = w.Write([]byte(`{"items":[{"id":"sys-1","system":"sys-1","os_name":"Debian GNU/Linux 13 (trixie)","cpu":"Intel Core Processor (Haswell, no TSX)"}]}`))
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		assertHostIdentity(t, unauthedTestClient(server.URL, server.Client()))
	})
}

func assertHostIdentity(t *testing.T, client *BeszelClient) {
	t.Helper()
	nodes, err := client.GetNodeSystemDetails(context.Background())
	if err != nil {
		t.Fatalf("GetNodeSystemDetails: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	n := nodes[0]
	if n.OS != "Debian GNU/Linux 13 (trixie)" {
		t.Errorf("OS = %q", n.OS)
	}
	if n.CPUModel != "Intel Core Processor (Haswell, no TSX)" {
		t.Errorf("CPUModel = %q", n.CPUModel)
	}
	if n.UptimeSec != 604800 {
		t.Errorf("UptimeSec = %v, want 604800", n.UptimeSec)
	}
}

func TestBeszelGetNodeSystemDetailsTimeWindow(t *testing.T) {
	t.Run("filter query param carries the window start per system", func(t *testing.T) {
		var statsFilters []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/api/collections/systems/records"):
				_, _ = w.Write([]byte(`{"items":[{"id":"sys-1","name":"node-1","status":"up","updated":"2026-01-02 03:04:05.000Z"},{"id":"sys-2","name":"node-2","status":"up","updated":"2026-01-02 03:04:05.000Z"}]}`))
			case strings.HasPrefix(r.URL.Path, "/api/collections/system_stats/records"):
				statsFilters = append(statsFilters, r.URL.Query().Get("filter"))
				if got := r.URL.Query().Get("perPage"); got != "500" {
					t.Errorf("perPage = %q, want 500", got)
				}
				_, _ = w.Write([]byte(`{"items":[]}`))
			case strings.HasPrefix(r.URL.Path, "/api/collections/system_details/records"):
				_, _ = w.Write([]byte(`{"items":[]}`))
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		client := unauthedTestClient(server.URL, server.Client())

		window, err := MetricsTimeRange(MetricsRange1w, time.Date(2026, 1, 8, 3, 4, 5, 0, time.UTC))
		if err != nil {
			t.Fatalf("MetricsTimeRange: %v", err)
		}
		if _, err := client.GetNodeSystemDetailsInWindow(context.Background(), window); err != nil {
			t.Fatalf("GetNodeSystemDetailsInWindow: %v", err)
		}
		// One stats request per system; each filter binds its own system id
		// plus the shared window start.
		if len(statsFilters) != 2 {
			t.Fatalf("got %d stats requests, want 2 (one per system)", len(statsFilters))
		}
		for i, filter := range statsFilters {
			wantSystem := fmt.Sprintf(`system="sys-%d"`, i+1)
			if !strings.Contains(filter, wantSystem) {
				t.Errorf("filter %d = %q, want it to contain %q", i, filter, wantSystem)
			}
			if !strings.Contains(filter, `created>="2026-01-01 03:04:05"`) {
				t.Errorf("filter %d = %q, want it to contain the window start in PocketBase datetime layout", i, filter)
			}
			if !strings.Contains(filter, `type="120m"`) {
				t.Errorf("filter %d = %q, want it to contain %q", i, filter, `type="120m"`)
			}
		}
	})

	t.Run("long windows downsample to MaxHistoryPoints", func(t *testing.T) {
		// 1200 records for one system; the windowed fetch must trim the
		// returned history to <= MaxHistoryPoints evenly-spaced points.
		// Newest first, matching the sort=-created order the client requests.
		var items strings.Builder
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for j := range 1200 {
			if j > 0 {
				items.WriteString(",")
			}
			i := 1200 - 1 - j
			created := base.Add(time.Duration(i) * time.Minute).UTC().Format("2006-01-02 15:04:05.000Z")
			fmt.Fprintf(&items, `{"created":"%s","system":"sys-1","type":"1m","stats":{"cpu":%d}}`, created, i)
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/api/collections/systems/records"):
				_, _ = w.Write([]byte(`{"items":[{"id":"sys-1","name":"node-1","status":"up","updated":"2026-01-02 03:04:05.000Z"}]}`))
			case strings.HasPrefix(r.URL.Path, "/api/collections/system_stats/records"):
				_, _ = fmt.Fprintf(w, `{"items":[%s]}`, items.String())
			case strings.HasPrefix(r.URL.Path, "/api/collections/system_details/records"):
				_, _ = w.Write([]byte(`{"items":[]}`))
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		client := unauthedTestClient(server.URL, server.Client())

		window, err := MetricsTimeRange(MetricsRange1w, time.Now())
		if err != nil {
			t.Fatalf("MetricsTimeRange: %v", err)
		}
		nodes, err := client.GetNodeSystemDetailsInWindow(context.Background(), window)
		if err != nil {
			t.Fatalf("GetNodeSystemDetailsInWindow: %v", err)
		}
		if len(nodes) != 1 {
			t.Fatalf("got %d nodes, want 1", len(nodes))
		}
		history := nodes[0].History
		if len(history) != MaxHistoryPoints {
			t.Fatalf("history points = %d, want %d", len(history), MaxHistoryPoints)
		}
		// Chronological order, endpoints preserved, roughly even spacing.
		if !history[0].Timestamp.Equal(base) {
			t.Fatalf("first point = %s, want oldest sample %s", history[0].Timestamp, base)
		}
		if !history[len(history)-1].Timestamp.Equal(base.Add(1199 * time.Minute)) {
			t.Fatalf("last point = %s, want newest sample", history[len(history)-1].Timestamp)
		}
		for i := 1; i < len(history); i++ {
			if !history[i].Timestamp.After(history[i-1].Timestamp) {
				t.Fatalf("history not chronological at %d: %s <= %s", i, history[i].Timestamp, history[i-1].Timestamp)
			}
		}
	})

	t.Run("filter carries the Beszel record type for the window tier", func(t *testing.T) {
		// Beszel pre-aggregates system_stats into retention tiers keyed by a
		// `type` field; the per-system stats request must pin the tier that
		// matches the requested range, mirroring Beszel's own chartTimeData.
		cases := []struct {
			id       MetricsTimeRangeID
			wantType string
		}{
			{MetricsRange24h, "20m"},
			{MetricsRange12h, "10m"},
			{MetricsRange1h, "1m"},
		}
		for _, tc := range cases {
			t.Run(string(tc.id), func(t *testing.T) {
				var statsFilters []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasPrefix(r.URL.Path, "/api/collections/systems/records"):
						_, _ = w.Write([]byte(`{"items":[{"id":"sys-1","name":"node-1","status":"up","updated":"2026-01-02 03:04:05.000Z"}]}`))
					case strings.HasPrefix(r.URL.Path, "/api/collections/system_stats/records"):
						statsFilters = append(statsFilters, r.URL.Query().Get("filter"))
						_, _ = fmt.Fprintf(w, `{"items":[{"created":"2026-01-08 03:04:05.000Z","system":"sys-1","type":"%s","stats":{"cpu":42}}]}`, tc.wantType)
					case strings.HasPrefix(r.URL.Path, "/api/collections/system_details/records"):
						_, _ = w.Write([]byte(`{"items":[]}`))
					default:
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(server.Close)
				client := unauthedTestClient(server.URL, server.Client())

				window, err := MetricsTimeRange(tc.id, time.Date(2026, 1, 8, 3, 4, 5, 0, time.UTC))
				if err != nil {
					t.Fatalf("MetricsTimeRange: %v", err)
				}
				if _, err := client.GetNodeSystemDetailsInWindow(context.Background(), window); err != nil {
					t.Fatalf("GetNodeSystemDetailsInWindow: %v", err)
				}
				if len(statsFilters) != 1 {
					t.Fatalf("got %d stats requests, want 1", len(statsFilters))
				}
				if !strings.Contains(statsFilters[0], `type="`+tc.wantType+`"`) {
					t.Errorf("filter = %q, want it to contain %q", statsFilters[0], `type="`+tc.wantType+`"`)
				}
				if !strings.Contains(statsFilters[0], `system="sys-1"`) {
					t.Errorf("filter = %q, want it to contain the system id", statsFilters[0])
				}
			})
		}
	})
}

func TestMetricsTimeRange(t *testing.T) {
	now := time.Date(2026, 1, 8, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		id   MetricsTimeRangeID
		want time.Duration
	}{
		{MetricsRange1h, time.Hour},
		{MetricsRange12h, 12 * time.Hour},
		{MetricsRange24h, 24 * time.Hour},
		{MetricsRange1w, 7 * 24 * time.Hour},
	}
	wantTypes := map[MetricsTimeRangeID]string{
		MetricsRange1h:  "1m",
		MetricsRange12h: "10m",
		MetricsRange24h: "20m",
		MetricsRange1w:  "120m",
	}
	for _, tc := range cases {
		window, err := MetricsTimeRange(tc.id, now)
		if err != nil {
			t.Fatalf("MetricsTimeRange(%s): %v", tc.id, err)
		}
		if got := window.End.Sub(window.Start); got != tc.want {
			t.Fatalf("MetricsTimeRange(%s) length = %s, want %s", tc.id, got, tc.want)
		}
		if window.ID != tc.id {
			t.Errorf("MetricsTimeRange(%s) window ID = %q, want %q", tc.id, window.ID, tc.id)
		}
		if got := window.ID.RecordType(); got != wantTypes[tc.id] {
			t.Errorf("MetricsTimeRangeID(%s).RecordType() = %q, want %q", tc.id, got, wantTypes[tc.id])
		}
	}
	if _, err := MetricsTimeRange("bogus", now); err == nil {
		t.Fatal("expected an error for an unknown range ID")
	}
	if got := MetricsTimeRangeID("bogus").RecordType(); got != "20m" {
		t.Errorf("unknown range RecordType() = %q, want fallback %q", got, "20m")
	}
}

func TestBeszelGetNodeSystemDetailsHardware(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/collections/systems/records"):
			_, _ = w.Write([]byte(`{"items":[{"id":"sys-1","name":"vps-99e943cd","host":"10.0.0.5","port":"45876","status":"up","updated":"2026-01-02 03:04:05.000Z","info":{"u":604800,"cpu":12.5}}]}`))
		case strings.HasPrefix(r.URL.Path, "/api/collections/system_stats/records"):
			_, _ = w.Write([]byte(`{"items":[]}`))
		case strings.HasPrefix(r.URL.Path, "/api/collections/system_details/records"):
			_, _ = w.Write([]byte(`{"items":[{"id":"sys-1","system":"sys-1","os_name":"Debian GNU/Linux 13 (trixie)","cpu":"Intel Core Processor (Haswell, no TSX)","hostname":"vps-99e943cd","kernel":"6.12.41+deb13-amd64","arch":"x86_64","cores":4,"threads":8,"memory":12240656793}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	nodes, err := unauthedTestClient(server.URL, server.Client()).GetNodeSystemDetails(context.Background())
	if err != nil {
		t.Fatalf("GetNodeSystemDetails: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	n := nodes[0]
	if n.Hostname != "vps-99e943cd" {
		t.Errorf("Hostname = %q", n.Hostname)
	}
	if n.Kernel != "6.12.41+deb13-amd64" {
		t.Errorf("Kernel = %q", n.Kernel)
	}
	if n.Arch != "x86_64" {
		t.Errorf("Arch = %q", n.Arch)
	}
	if n.Cores != 4 {
		t.Errorf("Cores = %d, want 4", n.Cores)
	}
	if n.Threads != 8 {
		t.Errorf("Threads = %d, want 8", n.Threads)
	}
	if n.MemoryBytes != 12240656793 {
		t.Errorf("MemoryBytes = %d, want 12240656793", n.MemoryBytes)
	}
}

func TestBeszelGetNodeSystemDetailsSystemdServices(t *testing.T) {
	updatedMs := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/collections/systems/records"):
			_, _ = w.Write([]byte(`{"items":[{"id":"sys-1","name":"node-1","status":"up","updated":"2026-01-02 03:04:05.000Z"},{"id":"sys-2","name":"node-2","status":"up","updated":"2026-01-02 03:04:05.000Z"}]}`))
		case strings.HasPrefix(r.URL.Path, "/api/collections/system_stats/records"):
			_, _ = w.Write([]byte(`{"items":[]}`))
		case strings.HasPrefix(r.URL.Path, "/api/collections/system_details/records"):
			_, _ = w.Write([]byte(`{"items":[]}`))
		case strings.HasPrefix(r.URL.Path, "/api/collections/systemd_services/records"):
			// The filter must scope services to the system being fetched.
			filter := r.URL.Query().Get("filter")
			if strings.Contains(filter, `system="sys-1"`) {
				_, _ = fmt.Fprintf(w, `{"items":[
					{"system":"sys-1","name":"beszel-agent","state":0,"sub":1,"cpu":0.03,"cpuPeak":0.31,"memory":12582912,"memPeak":14784921,"updated":%d},
					{"system":"sys-1","name":"apparmor","state":0,"sub":2,"cpu":0,"cpuPeak":0,"memory":0,"memPeak":23592960,"updated":"%d"},
					{"system":"sys-1","name":"cloud-init-main","state":1,"sub":0,"cpu":0,"cpuPeak":0,"memory":0,"memPeak":60922266,"updated":%d},
					{"system":"sys-1","name":"broken-svc","state":2,"sub":3,"cpu":0,"cpuPeak":0,"memory":0,"memPeak":0,"updated":%d},
					{"system":"sys-1","name":"future-svc","state":9,"sub":-1,"cpu":0,"cpuPeak":0,"memory":0,"memPeak":0,"updated":0}
				]}`, updatedMs, updatedMs, updatedMs, updatedMs)
				return
			}
			// sys-2's agent reports no services (or predates the collection).
			w.WriteHeader(http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	nodes, err := unauthedTestClient(server.URL, server.Client()).GetNodeSystemDetails(context.Background())
	if err != nil {
		t.Fatalf("GetNodeSystemDetails: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(nodes))
	}

	byID := map[string]NodeSystemDetail{}
	for _, n := range nodes {
		byID[n.ID] = n
	}

	services := byID["sys-1"].Services
	if len(services) != 5 {
		t.Fatalf("sys-1 services = %d, want 5", len(services))
	}
	// Sorted by name.
	wantOrder := []string{"apparmor", "beszel-agent", "broken-svc", "cloud-init-main", "future-svc"}
	for i, want := range wantOrder {
		if services[i].Name != want {
			t.Errorf("services[%d].Name = %q, want %q", i, services[i].Name, want)
		}
	}

	running := services[1] // beszel-agent
	if running.State != "Active" || running.SubState != "Running" {
		t.Errorf("beszel-agent state/sub = %q/%q, want Active/Running", running.State, running.SubState)
	}
	if running.CPU != 0.03 || running.CPUPeak != 0.31 {
		t.Errorf("beszel-agent cpu/peak = %v/%v, want 0.03/0.31", running.CPU, running.CPUPeak)
	}
	if running.MemoryBytes != 12582912 || running.MemoryPeakBytes != 14784921 {
		t.Errorf("beszel-agent mem/peak = %d/%d", running.MemoryBytes, running.MemoryPeakBytes)
	}
	if got := running.Updated; got.UnixMilli() != updatedMs {
		t.Errorf("beszel-agent Updated = %v, want UnixMilli %d", got, updatedMs)
	}

	exited := services[0] // apparmor
	if exited.State != "Active" || exited.SubState != "Exited" {
		t.Errorf("apparmor state/sub = %q/%q, want Active/Exited", exited.State, exited.SubState)
	}
	// updated may also arrive as a quoted string; it must still parse.
	if got := exited.Updated; got.UnixMilli() != updatedMs {
		t.Errorf("apparmor Updated = %v, want UnixMilli %d (string form)", got, updatedMs)
	}

	dead := services[3] // cloud-init-main
	if dead.State != "Inactive" || dead.SubState != "Dead" {
		t.Errorf("cloud-init-main state/sub = %q/%q, want Inactive/Dead", dead.State, dead.SubState)
	}

	failed := services[2] // broken-svc
	if failed.State != "Failed" || failed.SubState != "Failed" {
		t.Errorf("broken-svc state/sub = %q/%q, want Failed/Failed", failed.State, failed.SubState)
	}

	unknown := services[4] // future-svc: out-of-range enums, zero timestamp
	if unknown.State != "Unknown" || unknown.SubState != "Unknown" {
		t.Errorf("future-svc state/sub = %q/%q, want Unknown/Unknown", unknown.State, unknown.SubState)
	}
	if !unknown.Updated.IsZero() {
		t.Errorf("future-svc Updated = %v, want zero", unknown.Updated)
	}

	// A node whose systemd_services request fails renders no services (and
	// does not fail the page fetch).
	if got := byID["sys-2"].Services; len(got) != 0 {
		t.Errorf("sys-2 services = %d, want 0 on collection error", len(got))
	}
}

func TestBeszelStatParsingDiskAndBandwidthSplit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/collections/systems/records"):
			_, _ = w.Write([]byte(`{"items":[{"id":"sys-1","name":"node-1","status":"up","updated":"2026-01-02 03:04:05.000Z","info":{"cpu":12.5,"dp":20.1,"du":30.5,"d":100.0}}]}`))
		case strings.HasPrefix(r.URL.Path, "/api/collections/system_stats/records"):
			_, _ = w.Write([]byte(`{"items":[{"created":"2026-01-02 03:04:05.000Z","system":"sys-1","type":"20m","stats":{"cpu":10,"dp":20,"du":30,"d":100,"b":[1048576,2097152]}}]}`))
		case strings.HasPrefix(r.URL.Path, "/api/collections/system_details/records"):
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	nodes, err := unauthedTestClient(server.URL, server.Client()).GetNodeSystemDetails(context.Background())
	if err != nil {
		t.Fatalf("GetNodeSystemDetails: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	n := nodes[0]
	// info carries the live disk used/total (GB).
	if n.DiskUsed != 30.5 || n.DiskTotal != 100.0 {
		t.Errorf("node DiskUsed/DiskTotal = %v/%v, want 30.5/100", n.DiskUsed, n.DiskTotal)
	}
	if len(n.History) != 1 {
		t.Fatalf("got %d history points, want 1", len(n.History))
	}
	pt := n.History[0]
	if pt.DiskUsed != 30 || pt.DiskTotal != 100 {
		t.Errorf("stat DiskUsed/DiskTotal = %v/%v, want 30/100", pt.DiskUsed, pt.DiskTotal)
	}
	// b = [sent bytes, recv bytes] -> MB/s split plus the summed Bandwidth.
	if pt.NetSent != 1.0 {
		t.Errorf("stat NetSent = %v, want 1.0 MB/s", pt.NetSent)
	}
	if pt.NetRecv != 2.0 {
		t.Errorf("stat NetRecv = %v, want 2.0 MB/s", pt.NetRecv)
	}
	if pt.Bandwidth != 3.0 {
		t.Errorf("stat Bandwidth = %v, want 3.0 MB/s", pt.Bandwidth)
	}
}
