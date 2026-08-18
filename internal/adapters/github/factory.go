package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	ghinstallation "github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v88/github"
)

const githubResponseLimit = 1 << 20

type ClientFactory interface {
	Client(installationID int64) (*gh.Client, error)
}

// Refresh evicts a cached installation client so the next request obtains a
// fresh installation token from the current app credential.
func (f *InstallationClientFactory) Refresh(installationID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.clients {
		if key.installationID == installationID {
			delete(f.clients, key)
		}
	}
	clear(f.apps)
}

type AppCredential struct {
	AppID           int64
	PrivateKey      []byte
	ResourceVersion string
}

type AppCredentialProvider interface {
	AppCredential(context.Context) (AppCredential, error)
}

type InstallationClientFactory struct {
	credentials AppCredentialProvider
	base        http.RoundTripper
	apiBaseURL  string
	uploadURL   string
	apiVersion  string

	mu      sync.Mutex
	clients map[clientKey]*gh.Client
	apps    map[appClientKey]*gh.Client
}

type clientKey struct {
	appID, installationID int64
	resourceVersion       string
}

type appClientKey struct {
	appID           int64
	resourceVersion string
}

func NewInstallationClientFactory(credentials AppCredentialProvider, apiBaseURL, uploadURL, apiVersion string, base http.RoundTripper) (*InstallationClientFactory, error) {
	if credentials == nil {
		return nil, fmt.Errorf("GitHub App credential provider is required")
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &InstallationClientFactory{credentials: credentials, base: base, apiBaseURL: ensureTrailingSlash(apiBaseURL),
		uploadURL: ensureTrailingSlash(uploadURL), apiVersion: apiVersion, clients: make(map[clientKey]*gh.Client), apps: make(map[appClientKey]*gh.Client)}, nil
}

func (f *InstallationClientFactory) AppClient(ctx context.Context) (*gh.Client, error) {
	credential, err := f.credentials.AppCredential(ctx)
	if err != nil {
		return nil, fmt.Errorf("load GitHub App credential: %w", err)
	}
	key := appClientKey{appID: credential.AppID, resourceVersion: credential.ResourceVersion}
	f.mu.Lock()
	defer f.mu.Unlock()
	if client := f.apps[key]; client != nil {
		return client, nil
	}
	client, err := NewAppClient(credential, f.apiBaseURL, f.uploadURL, f.apiVersion, f.base)
	if err != nil {
		return nil, err
	}
	f.apps = map[appClientKey]*gh.Client{key: client}
	return client, nil
}

func (f *InstallationClientFactory) Client(installationID int64) (*gh.Client, error) {
	if installationID <= 0 {
		return nil, fmt.Errorf("installation ID must be greater than zero")
	}
	credential, err := f.credentials.AppCredential(context.Background())
	if err != nil {
		return nil, fmt.Errorf("load GitHub App credential: %w", err)
	}
	key := clientKey{appID: credential.AppID, installationID: installationID, resourceVersion: credential.ResourceVersion}
	f.mu.Lock()
	defer f.mu.Unlock()
	if client := f.clients[key]; client != nil {
		return client, nil
	}
	versionedBase := apiVersionTransport{version: f.apiVersion, next: f.base}
	installation, err := ghinstallation.New(versionedBase, credential.AppID, installationID, credential.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("create GitHub installation transport: %w", err)
	}
	installation.BaseURL = strings.TrimRight(f.apiBaseURL, "/")
	client, err := newClient(newBoundedTransport(apiVersionTransport{version: f.apiVersion, next: installation}, f.apiBaseURL, f.uploadURL), f.apiBaseURL, f.uploadURL)
	if err != nil {
		return nil, err
	}
	f.clients = map[clientKey]*gh.Client{key: client}
	return client, nil
}

func NewAppClient(credential AppCredential, apiBaseURL, uploadURL, apiVersion string, base http.RoundTripper) (*gh.Client, error) {
	if base == nil {
		base = http.DefaultTransport
	}
	transport, err := ghinstallation.NewAppsTransport(apiVersionTransport{version: apiVersion, next: base}, credential.AppID, credential.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("create GitHub App transport: %w", err)
	}
	transport.BaseURL = strings.TrimRight(apiBaseURL, "/")
	return newClient(newBoundedTransport(apiVersionTransport{version: apiVersion, next: transport}, apiBaseURL, uploadURL), ensureTrailingSlash(apiBaseURL), ensureTrailingSlash(uploadURL))
}

func newClient(transport http.RoundTripper, apiBaseURL, uploadURL string) (*gh.Client, error) {
	client, err := gh.NewClient(gh.WithTransport(transport), gh.WithURLs(&apiBaseURL, &uploadURL), gh.WithUserAgent("cyclopus"))
	if err != nil {
		return nil, fmt.Errorf("create GitHub client: %w", err)
	}
	return client, nil
}

type apiVersionTransport struct {
	version string
	next    http.RoundTripper
}

type boundedTransport struct {
	next         http.RoundTripper
	allowedHosts map[string]struct{}
}

func newBoundedTransport(next http.RoundTripper, baseURLs ...string) boundedTransport {
	allowed := make(map[string]struct{}, len(baseURLs))
	for _, raw := range baseURLs {
		if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
			allowed[strings.ToLower(parsed.Host)] = struct{}{}
		}
	}
	return boundedTransport{next: next, allowedHosts: allowed}
}

func (t boundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if _, ok := t.allowedHosts[strings.ToLower(request.URL.Host)]; len(t.allowedHosts) > 0 && !ok {
		return nil, fmt.Errorf("GitHub request host is not trusted")
	}
	response, err := t.next.RoundTrip(request)
	if err != nil || response == nil || response.Body == nil {
		return response, err
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		_ = response.Body.Close()
		return nil, fmt.Errorf("GitHub redirects are not allowed")
	}
	response.Body = &boundedResponseBody{ReadCloser: response.Body, reader: io.LimitReader(response.Body, githubResponseLimit+1)}
	return response, nil
}

type boundedResponseBody struct {
	io.ReadCloser
	reader io.Reader
	read   int64
}

func (b *boundedResponseBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += int64(n)
	if b.read > githubResponseLimit {
		return n, fmt.Errorf("GitHub response exceeded 1 MiB limit")
	}
	return n, err
}

func (t apiVersionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header.Set("X-GitHub-Api-Version", t.version)
	cloned.Header.Set("Accept", "application/vnd.github+json")
	return t.next.RoundTrip(cloned)
}

func ensureTrailingSlash(value string) string { return strings.TrimRight(value, "/") + "/" }
