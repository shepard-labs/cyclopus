package github

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	ghinstallation "github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v88/github"
)

type ClientFactory interface {
	Client(installationID int64) (*gh.Client, error)
}

type InstallationClientFactory struct {
	appID      int64
	privateKey []byte
	base       http.RoundTripper
	apiBaseURL string
	uploadURL  string
	apiVersion string

	mu      sync.Mutex
	clients map[int64]*gh.Client
}

func NewInstallationClientFactory(appID int64, privateKeyFile, apiBaseURL, uploadURL, apiVersion string, base http.RoundTripper) (*InstallationClientFactory, error) {
	privateKey, err := os.ReadFile(privateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read GitHub App private key: %w", err)
	}
	if base == nil {
		base = http.DefaultTransport
	}
	if _, err := ghinstallation.NewAppsTransport(base, appID, privateKey); err != nil {
		return nil, fmt.Errorf("create GitHub App transport: %w", err)
	}
	return &InstallationClientFactory{
		appID:      appID,
		privateKey: append([]byte(nil), privateKey...),
		base:       base,
		apiBaseURL: ensureTrailingSlash(apiBaseURL),
		uploadURL:  ensureTrailingSlash(uploadURL),
		apiVersion: apiVersion,
		clients:    make(map[int64]*gh.Client),
	}, nil
}

func (f *InstallationClientFactory) Client(installationID int64) (*gh.Client, error) {
	if installationID <= 0 {
		return nil, fmt.Errorf("installation ID must be greater than zero")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if client := f.clients[installationID]; client != nil {
		return client, nil
	}

	versionedBase := apiVersionTransport{version: f.apiVersion, next: f.base}
	installation, err := ghinstallation.New(versionedBase, f.appID, installationID, f.privateKey)
	if err != nil {
		return nil, fmt.Errorf("create GitHub installation transport: %w", err)
	}
	installation.BaseURL = strings.TrimRight(f.apiBaseURL, "/")
	transport := apiVersionTransport{version: f.apiVersion, next: installation}
	client, err := gh.NewClient(
		gh.WithTransport(transport),
		gh.WithURLs(&f.apiBaseURL, &f.uploadURL),
		gh.WithUserAgent("cyclopus"),
	)
	if err != nil {
		return nil, fmt.Errorf("create GitHub installation client: %w", err)
	}
	f.clients[installationID] = client
	return client, nil
}

type apiVersionTransport struct {
	version string
	next    http.RoundTripper
}

func (t apiVersionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header.Set("X-GitHub-Api-Version", t.version)
	cloned.Header.Set("Accept", "application/vnd.github+json")
	return t.next.RoundTrip(cloned)
}

func ensureTrailingSlash(value string) string {
	return strings.TrimRight(value, "/") + "/"
}
