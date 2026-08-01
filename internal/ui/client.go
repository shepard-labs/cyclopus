package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a thin HTTP wrapper around the Cyclopus grpc-gateway REST API.
// Every method maps 1:1 to one REST endpoint defined in cyclopus.proto.
type Client struct {
	base string
	http *http.Client
}

// NewClient constructs a Client that calls the REST API at listenAddr.
// listenAddr is the same value as cfg.HTTP.Address (e.g. ":8080" or "0.0.0.0:8080").
// It is normalized to a loopback URL so the UI can always reach the API in-process.
func NewClient(listenAddr string) *Client {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "8080"
	}
	return &Client{
		base: "http://" + host + ":" + port,
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// ── Auth ────────────────────────────────────────────────────────────────────

func (c *Client) Login(ctx context.Context, email, password string) (*Session, error) {
	var sess Session
	err := c.post(ctx, "", "/v1/auth/login", map[string]string{
		"email":    email,
		"password": password,
	}, &sess)
	return &sess, err
}

func (c *Client) Bootstrap(ctx context.Context, email, password, bootstrapToken string) (*Session, error) {
	var sess Session
	err := c.post(ctx, "", "/v1/auth/bootstrap", map[string]string{
		"email":          email,
		"password":       password,
		"bootstrapToken": bootstrapToken,
	}, &sess)
	return &sess, err
}

func (c *Client) Logout(ctx context.Context, token string) error {
	return c.post(ctx, token, "/v1/auth/logout", nil, nil)
}

func (c *Client) GetCurrentUser(ctx context.Context, token string) (*User, error) {
	var resp struct {
		User User `json:"user"`
	}
	err := c.get(ctx, token, "/v1/user", nil, &resp)
	return &resp.User, err
}

// ── Organization ────────────────────────────────────────────────────────────

func (c *Client) GetOrganization(ctx context.Context, token, orgID string) (*Organization, error) {
	var resp struct {
		Organization Organization `json:"organization"`
	}
	err := c.get(ctx, token, "/v1/organizations/"+orgID, nil, &resp)
	return &resp.Organization, err
}

// ── Projects ────────────────────────────────────────────────────────────────

func (c *Client) ListProjects(ctx context.Context, token, orgID, pageToken string) ([]Project, string, error) {
	var resp struct {
		Projects      []Project `json:"projects"`
		NextPageToken string    `json:"nextPageToken"`
	}
	params := url.Values{}
	if pageToken != "" {
		params.Set("page_token", pageToken)
	}
	params.Set("page_size", "50")
	err := c.get(ctx, token, "/v1/organizations/"+orgID+"/projects", params, &resp)
	return resp.Projects, resp.NextPageToken, err
}

func (c *Client) CreateProject(ctx context.Context, token, orgID, idemKey, name, slug string) (*Project, error) {
	var resp struct {
		Project Project `json:"project"`
	}
	body := map[string]any{
		"name":            name,
		"slug":            slug,
		"idempotency_key": idemKey,
	}
	err := c.postJSON(ctx, token, "/v1/organizations/"+orgID+"/projects", body, &resp)
	return &resp.Project, err
}

func (c *Client) GetProject(ctx context.Context, token, projectID string) (*Project, error) {
	var resp struct {
		Project Project `json:"project"`
	}
	err := c.get(ctx, token, "/v1/projects/"+projectID, nil, &resp)
	return &resp.Project, err
}

func (c *Client) UpdateProject(ctx context.Context, token, idemKey, projectID, name, slug string) (*Project, error) {
	var resp struct {
		Project Project `json:"project"`
	}
	body := map[string]any{
		"id":              projectID,
		"name":            name,
		"slug":            slug,
		"idempotency_key": idemKey,
		"update_mask":     map[string]any{"paths": []string{"name", "slug"}},
	}
	err := c.patchJSON(ctx, token, "/v1/projects/"+projectID, body, &resp)
	return &resp.Project, err
}

func (c *Client) DeleteProject(ctx context.Context, token, idemKey, projectID string) (*Operation, error) {
	var resp struct {
		Operation Operation `json:"operation"`
	}
	params := url.Values{"idempotency_key": {idemKey}}
	err := c.delete(ctx, token, "/v1/projects/"+projectID, params, &resp)
	return &resp.Operation, err
}

// ── Applications ────────────────────────────────────────────────────────────

func (c *Client) ListApplications(ctx context.Context, token, projectID, pageToken string) ([]Application, string, error) {
	var resp struct {
		Applications  []Application `json:"applications"`
		NextPageToken string        `json:"nextPageToken"`
	}
	params := url.Values{"page_size": {"50"}}
	if pageToken != "" {
		params.Set("page_token", pageToken)
	}
	err := c.get(ctx, token, "/v1/projects/"+projectID+"/applications", params, &resp)
	return resp.Applications, resp.NextPageToken, err
}

func (c *Client) CreateApplication(ctx context.Context, token, projectID, idemKey string, app Application) (*Application, error) {
	var resp struct {
		Application Application `json:"application"`
	}
	body := map[string]any{
		"name":              app.Name,
		"slug":              app.Slug,
		"hostname_label":    app.HostnameLabel,
		"container_port":    app.ContainerPort,
		"health_check_path": app.HealthCheckPath,
		"idempotency_key":   idemKey,
	}
	err := c.postJSON(ctx, token, "/v1/projects/"+projectID+"/applications", body, &resp)
	return &resp.Application, err
}

func (c *Client) GetApplication(ctx context.Context, token, appID string) (*Application, error) {
	var resp struct {
		Application Application `json:"application"`
	}
	err := c.get(ctx, token, "/v1/applications/"+appID, nil, &resp)
	return &resp.Application, err
}

func (c *Client) UpdateApplication(ctx context.Context, token, idemKey, appID string, fields map[string]any, paths []string) (*Application, error) {
	var resp struct {
		Application Application `json:"application"`
	}
	fields["id"] = appID
	fields["idempotency_key"] = idemKey
	fields["update_mask"] = map[string]any{"paths": paths}
	err := c.patchJSON(ctx, token, "/v1/applications/"+appID, fields, &resp)
	return &resp.Application, err
}

func (c *Client) DeleteApplication(ctx context.Context, token, idemKey, appID string) (*Operation, error) {
	var resp struct {
		Operation Operation `json:"operation"`
	}
	params := url.Values{"idempotency_key": {idemKey}}
	err := c.delete(ctx, token, "/v1/applications/"+appID, params, &resp)
	return &resp.Operation, err
}

// ── Environments ────────────────────────────────────────────────────────────

func (c *Client) ListEnvironments(ctx context.Context, token, appID string, includeTornDown bool) ([]Environment, error) {
	var resp struct {
		Environments []Environment `json:"environments"`
	}
	params := url.Values{"page_size": {"100"}}
	if includeTornDown {
		params.Set("include_torn_down", "true")
	}
	err := c.get(ctx, token, "/v1/applications/"+appID+"/environments", params, &resp)
	return resp.Environments, err
}

func (c *Client) GetEnvironment(ctx context.Context, token, envID string) (*Environment, error) {
	var resp struct {
		Environment Environment `json:"environment"`
	}
	err := c.get(ctx, token, "/v1/environments/"+envID, nil, &resp)
	return &resp.Environment, err
}

func (c *Client) TeardownEnvironment(ctx context.Context, token, idemKey, envID, reason string) (*Environment, error) {
	var resp struct {
		Environment Environment `json:"environment"`
	}
	err := c.post(ctx, token, "/v1/environments/"+envID+":teardown", map[string]string{
		"reason":          reason,
		"idempotency_key": idemKey,
	}, &resp)
	return &resp.Environment, err
}

// ── Releases ────────────────────────────────────────────────────────────────

func (c *Client) ListReleases(ctx context.Context, token, envID, pageToken string) ([]Release, string, error) {
	var resp struct {
		Releases      []Release `json:"releases"`
		NextPageToken string    `json:"nextPageToken"`
	}
	params := url.Values{"page_size": {"20"}}
	if pageToken != "" {
		params.Set("page_token", pageToken)
	}
	err := c.get(ctx, token, "/v1/environments/"+envID+"/releases", params, &resp)
	return resp.Releases, resp.NextPageToken, err
}

func (c *Client) GetRelease(ctx context.Context, token, releaseID string) (*Release, error) {
	var resp struct {
		Release Release `json:"release"`
	}
	err := c.get(ctx, token, "/v1/releases/"+releaseID, nil, &resp)
	return &resp.Release, err
}

func (c *Client) Rollback(ctx context.Context, token, idemKey, envID, releaseID string) (*Operation, error) {
	var resp struct {
		Operation Operation `json:"operation"`
	}
	err := c.post(ctx, token, "/v1/environments/"+envID+"/rollbacks", map[string]string{
		"environment_id":  envID,
		"release_id":      releaseID,
		"idempotency_key": idemKey,
	}, &resp)
	return &resp.Operation, err
}

// ── Environment variables ────────────────────────────────────────────────────

func (c *Client) GetEnvVars(ctx context.Context, token, envID string) (*EnvVarRevision, error) {
	var resp struct {
		Revision EnvVarRevision `json:"revision"`
	}
	err := c.get(ctx, token, "/v1/environments/"+envID+"/env-vars", nil, &resp)
	return &resp.Revision, err
}

// ── Aliases ──────────────────────────────────────────────────────────────────

func (c *Client) ListAliases(ctx context.Context, token, envID string) ([]Alias, error) {
	var resp struct {
		Aliases []Alias `json:"aliases"`
	}
	err := c.get(ctx, token, "/v1/environments/"+envID+"/aliases", nil, &resp)
	return resp.Aliases, err
}

// ── Certificates ─────────────────────────────────────────────────────────────

func (c *Client) ListCertificates(ctx context.Context, token, envID string) ([]Certificate, error) {
	var resp struct {
		Certificates []Certificate `json:"certificates"`
	}
	err := c.get(ctx, token, "/v1/environments/"+envID+"/certificates", nil, &resp)
	return resp.Certificates, err
}

// ── Domains ──────────────────────────────────────────────────────────────────

func (c *Client) ListDomains(ctx context.Context, token, appID string) ([]Domain, error) {
	var resp struct {
		Domains []Domain `json:"domains"`
	}
	err := c.get(ctx, token, "/v1/applications/"+appID+"/domains", nil, &resp)
	return resp.Domains, err
}

func (c *Client) CreateDomain(ctx context.Context, token, idemKey, appID, hostname, method string) (*Domain, error) {
	var resp struct {
		Domain Domain `json:"domain"`
	}
	err := c.postJSON(ctx, token, "/v1/applications/"+appID+"/domains", map[string]any{
		"hostname":            hostname,
		"verification_method": method,
		"idempotency_key":     idemKey,
	}, &resp)
	return &resp.Domain, err
}

func (c *Client) VerifyDomain(ctx context.Context, token, idemKey, domainID string) (*Domain, error) {
	var resp struct {
		Domain Domain `json:"domain"`
	}
	err := c.post(ctx, token, "/v1/domains/"+domainID+":verify", map[string]string{
		"idempotency_key": idemKey,
	}, &resp)
	return &resp.Domain, err
}

func (c *Client) DeleteDomain(ctx context.Context, token, idemKey, domainID string) error {
	params := url.Values{"idempotency_key": {idemKey}}
	return c.delete(ctx, token, "/v1/domains/"+domainID, params, nil)
}

// ── Git connections ───────────────────────────────────────────────────────────

func (c *Client) ListGitConnections(ctx context.Context, token, orgID string) ([]GitConnection, error) {
	var resp struct {
		GitConnections []GitConnection `json:"gitConnections"`
	}
	err := c.get(ctx, token, "/v1/organizations/"+orgID+"/git-connections", nil, &resp)
	return resp.GitConnections, err
}

func (c *Client) CreateGitConnection(ctx context.Context, token, idemKey, orgID string, gc GitConnection) (*GitConnection, error) {
	var resp struct {
		GitConnection GitConnection `json:"gitConnection"`
	}
	err := c.postJSON(ctx, token, "/v1/organizations/"+orgID+"/git-connections", map[string]any{
		"repo_full_name":     gc.RepoFullName,
		"default_branch":     gc.DefaultBranch,
		"installation_id":    gc.InstallationID,
		"webhook_secret_ref": gc.WebhookSecretRef,
		"idempotency_key":    idemKey,
	}, &resp)
	return &resp.GitConnection, err
}

func (c *Client) DeleteGitConnection(ctx context.Context, token, idemKey, gcID string) error {
	params := url.Values{"idempotency_key": {idemKey}}
	return c.delete(ctx, token, "/v1/git-connections/"+gcID, params, nil)
}

// ── Registries ───────────────────────────────────────────────────────────────

func (c *Client) ListRegistries(ctx context.Context, token, orgID string) ([]Registry, error) {
	var resp struct {
		Registries []Registry `json:"registries"`
	}
	err := c.get(ctx, token, "/v1/organizations/"+orgID+"/registries", nil, &resp)
	return resp.Registries, err
}

func (c *Client) CreateRegistry(ctx context.Context, token, idemKey, orgID string, reg Registry) (*Registry, error) {
	var resp struct {
		Registry Registry `json:"registry"`
	}
	err := c.postJSON(ctx, token, "/v1/organizations/"+orgID+"/registries", map[string]any{
		"provider":              reg.Provider,
		"registry_url":          reg.RegistryURL,
		"credential_secret_ref": reg.CredentialSecretRef,
		"idempotency_key":       idemKey,
	}, &resp)
	return &resp.Registry, err
}

func (c *Client) DeleteRegistry(ctx context.Context, token, idemKey, regID string) error {
	params := url.Values{"idempotency_key": {idemKey}}
	return c.delete(ctx, token, "/v1/registries/"+regID, params, nil)
}

// ── Webhooks ─────────────────────────────────────────────────────────────────

func (c *Client) ListWebhooks(ctx context.Context, token, orgID string) ([]Webhook, error) {
	var resp struct {
		Webhooks []Webhook `json:"webhooks"`
	}
	err := c.get(ctx, token, "/v1/organizations/"+orgID+"/webhooks", nil, &resp)
	return resp.Webhooks, err
}

func (c *Client) CreateWebhook(ctx context.Context, token, idemKey, orgID string, wh Webhook) (*Webhook, error) {
	var resp struct {
		Webhook Webhook `json:"webhook"`
	}
	err := c.postJSON(ctx, token, "/v1/organizations/"+orgID+"/webhooks", map[string]any{
		"target_url":      wh.TargetURL,
		"events":          wh.Events,
		"secret_ref":      wh.SecretRef,
		"idempotency_key": idemKey,
	}, &resp)
	return &resp.Webhook, err
}

func (c *Client) DeleteWebhook(ctx context.Context, token, idemKey, whID string) error {
	params := url.Values{"idempotency_key": {idemKey}}
	return c.delete(ctx, token, "/v1/webhooks/"+whID, params, nil)
}

// ── API tokens ───────────────────────────────────────────────────────────────

func (c *Client) ListAPITokens(ctx context.Context, token string) ([]APIToken, error) {
	var resp struct {
		APITokens []APIToken `json:"apiTokens"`
	}
	err := c.get(ctx, token, "/v1/user/api-tokens", nil, &resp)
	return resp.APITokens, err
}

func (c *Client) CreateAPIToken(ctx context.Context, token, idemKey, name string, scopes []string) (string, *APIToken, error) {
	var resp struct {
		APIToken APIToken `json:"apiToken"`
		Token    string   `json:"token"`
	}
	err := c.postJSON(ctx, token, "/v1/user/api-tokens", map[string]any{
		"name":            name,
		"scopes":          scopes,
		"idempotency_key": idemKey,
	}, &resp)
	return resp.Token, &resp.APIToken, err
}

func (c *Client) RevokeAPIToken(ctx context.Context, token, idemKey, tokenID string) error {
	params := url.Values{"idempotency_key": {idemKey}}
	return c.delete(ctx, token, "/v1/user/api-tokens/"+tokenID, params, nil)
}

// ── Audit events ─────────────────────────────────────────────────────────────

func (c *Client) ListAuditEvents(ctx context.Context, token, orgID, pageToken string) ([]AuditEvent, string, error) {
	var resp struct {
		AuditEvents   []AuditEvent `json:"auditEvents"`
		NextPageToken string       `json:"nextPageToken"`
	}
	params := url.Values{"page_size": {"50"}}
	if pageToken != "" {
		params.Set("page_token", pageToken)
	}
	err := c.get(ctx, token, "/v1/organizations/"+orgID+"/audit-events", params, &resp)
	return resp.AuditEvents, resp.NextPageToken, err
}

// ── Observability ─────────────────────────────────────────────────────────────

func (c *Client) QueryLogs(ctx context.Context, token, appID, envID, cursor, query string, limit int) ([]LogEntry, string, error) {
	var resp struct {
		Entries    []LogEntry `json:"entries"`
		NextCursor string     `json:"nextCursor"`
	}
	params := url.Values{"limit": {fmt.Sprintf("%d", limit)}}
	if envID != "" {
		params.Set("environment_id", envID)
	}
	if cursor != "" {
		params.Set("cursor", cursor)
	}
	if query != "" {
		params.Set("query", query)
	}
	err := c.get(ctx, token, "/v1/applications/"+appID+"/logs", params, &resp)
	return resp.Entries, resp.NextCursor, err
}

// ── Reconciliation ────────────────────────────────────────────────────────────

func (c *Client) ListReconciliations(ctx context.Context, token, orgID string) ([]ReconciliationRun, error) {
	var resp struct {
		Reconciliations []ReconciliationRun `json:"reconciliations"`
	}
	params := url.Values{"organization_id": {orgID}, "page_size": {"20"}}
	err := c.get(ctx, token, "/v1/reconciliations", params, &resp)
	return resp.Reconciliations, err
}

func (c *Client) StartReconciliation(ctx context.Context, token, idemKey, orgID string, dryRun bool) (*Operation, error) {
	var resp struct {
		Operation Operation `json:"operation"`
	}
	err := c.postJSON(ctx, token, "/v1/reconciliations", map[string]any{
		"scope": map[string]any{
			"kind":            "RECONCILE_SCOPE_KIND_ORGANIZATION",
			"organization_id": orgID,
		},
		"dry_run":         dryRun,
		"idempotency_key": idemKey,
	}, &resp)
	return &resp.Operation, err
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func (c *Client) get(ctx context.Context, token, path string, params url.Values, out any) error {
	u := c.base + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.do(req, out)
}

func (c *Client) post(ctx context.Context, token, path string, body any, out any) error {
	return c.sendJSON(ctx, token, http.MethodPost, path, body, out)
}

func (c *Client) postJSON(ctx context.Context, token, path string, body any, out any) error {
	return c.sendJSON(ctx, token, http.MethodPost, path, body, out)
}

func (c *Client) patchJSON(ctx context.Context, token, path string, body any, out any) error {
	return c.sendJSON(ctx, token, http.MethodPatch, path, body, out)
}

func (c *Client) delete(ctx context.Context, token, path string, params url.Values, out any) error {
	u := c.base + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.do(req, out)
}

func (c *Client) sendJSON(ctx context.Context, token, method, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

// APIError is returned when the API responds with a non-2xx status.
type APIError struct {
	Code    int
	Message string
	Body    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("API error %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("API error %d", e.Code)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Try to extract message from grpc-gateway error JSON.
		var errBody struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &errBody)
		msg := errBody.Message
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		return &APIError{Code: resp.StatusCode, Message: msg, Body: string(body)}
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// IsNotFound returns true if the error is a 404 API error.
func IsNotFound(err error) bool {
	var ae *APIError
	if ok := isAPIError(err, &ae); ok {
		return ae.Code == http.StatusNotFound
	}
	return false
}

// IsUnauthorized returns true if the error is a 401 API error.
func IsUnauthorized(err error) bool {
	var ae *APIError
	if ok := isAPIError(err, &ae); ok {
		return ae.Code == http.StatusUnauthorized
	}
	return false
}

func isAPIError(err error, out **APIError) bool {
	if err == nil {
		return false
	}
	if ae, ok := err.(*APIError); ok {
		*out = ae
		return true
	}
	return false
}
