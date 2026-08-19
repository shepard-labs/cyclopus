package oci

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	productionGHCROrigin = "https://ghcr.io"
	clientUserAgent      = "cyclopus-oci/1"
	maxResponseHeaders   = 32 << 10
	maxErrorDrainBytes   = 4 << 10
)

var manifestAccept = strings.Join([]string{
	MediaTypeOCIImageIndex,
	MediaTypeOCIImageManifest,
	MediaTypeDockerList,
	MediaTypeDockerManifest,
}, ", ")

type ResolveRequest struct {
	Host, Repository, Tag string
	Credential            *Credential
	ExpectedSource        RepositoryIdentity
	ExpectedRevision      string
	AnnotationPolicy      AnnotationPolicy
}

type ResolvedImage struct {
	Digest, ImmutableReference, ManifestMediaType string
	ConfigDigest, ConfigMediaType                 string
	Source, Revision                              string
	AnnotationStatus                              AnnotationStatus
}

type PullValidationRequest struct {
	Host, Repository, Reference string
	Credential                  *Credential
}

type Resolver interface {
	Resolve(context.Context, ResolveRequest) (ResolvedImage, error)
	ValidatePull(context.Context, PullValidationRequest) error
}

type ClientConfig struct {
	HTTPClient     *http.Client
	RequestTimeout time.Duration
	Now            func() time.Time
}

type Client struct {
	httpClient *http.Client
	baseOrigin *url.URL
	now        func() time.Time
}

func NewClient(config ClientConfig) (*Client, error) {
	baseOrigin, _ := url.Parse(productionGHCROrigin)
	return newClient(config, baseOrigin)
}

func newClientForTest(config ClientConfig, rawOrigin string) (*Client, error) {
	if rawOrigin == "" || strings.TrimSpace(rawOrigin) != rawOrigin || strings.Contains(rawOrigin, "\\") {
		return nil, errors.New("oci: invalid test base origin")
	}
	baseOrigin, err := url.Parse(rawOrigin)
	if err != nil || baseOrigin.Scheme != "https" || baseOrigin.Host == "" || baseOrigin.User != nil ||
		baseOrigin.Path != "" || baseOrigin.RawQuery != "" || baseOrigin.Fragment != "" || baseOrigin.Opaque != "" {
		return nil, errors.New("oci: invalid test base origin")
	}
	return newClient(config, baseOrigin)
}

func newClient(config ClientConfig, baseOrigin *url.URL) (*Client, error) {
	if config.HTTPClient == nil || config.RequestTimeout <= 0 {
		return nil, errors.New("oci: HTTP client and positive request timeout are required")
	}
	transport := config.HTTPClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		return nil, errors.New("oci: HTTP client transport must be *http.Transport")
	}
	clonedTransport := httpTransport.Clone()
	clonedTransport.MaxResponseHeaderBytes = maxResponseHeaders
	clonedTransport.DisableCompression = true
	client := *config.HTTPClient
	client.Jar = nil
	client.Transport = clonedTransport
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Timeout = config.RequestTimeout
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Client{httpClient: &client, baseOrigin: baseOrigin, now: now}, nil
}

func (c *Client) Resolve(ctx context.Context, request ResolveRequest) (ResolvedImage, error) {
	if request.Credential != nil {
		return ResolvedImage{}, NewError(CodeRegistryAuthRequired)
	}
	reference, err := ParseReference(request.Host, request.Repository, request.Tag)
	if err != nil {
		return ResolvedImage{}, err
	}
	manifestURL := c.registryURL(reference.Repository, "manifests", reference.Tag)
	if err := c.manifestHEAD(ctx, manifestURL); err != nil {
		return ResolvedImage{}, err
	}
	manifest, mediaType, digest, err := c.resolveManifest(ctx, manifestURL, "")
	if err != nil {
		return ResolvedImage{}, err
	}

	values := EvidenceValues{ManifestAnnotations: manifest.Annotations}
	resolved := ResolvedImage{Digest: digest, ManifestMediaType: mediaType}
	if manifest.Config != nil {
		configURL := c.registryURL(reference.Repository, "blobs", manifest.Config.Digest)
		configBody, configMedia, configDigest, getErr := c.getVerified(ctx, configURL, manifest.Config.MediaType, MaxConfigBytes, ResponseConfigBlob, manifest.Config.Digest)
		if getErr != nil {
			return ResolvedImage{}, getErr
		}
		if int64(len(configBody)) != manifest.Config.Size || configMedia != manifest.Config.MediaType || configDigest != manifest.Config.Digest {
			return ResolvedImage{}, NewError(CodeRegistryConfigInvalid)
		}
		labels, parseErr := ParseConfigLabels(configBody)
		if parseErr != nil {
			return ResolvedImage{}, parseErr
		}
		values.ConfigLabels = labels
		resolved.ConfigDigest, resolved.ConfigMediaType = configDigest, configMedia
	}
	evidence, err := VerifySourceEvidence(request.ExpectedSource, request.ExpectedRevision, request.AnnotationPolicy, values)
	if err != nil {
		return ResolvedImage{}, err
	}
	resolved.ImmutableReference, err = ImmutableReference(reference.Repository, digest)
	if err != nil {
		return ResolvedImage{}, err
	}
	resolved.Source, resolved.Revision, resolved.AnnotationStatus = evidence.Source, evidence.Revision, evidence.Status
	return resolved, nil
}

func (c *Client) ValidatePull(ctx context.Context, request PullValidationRequest) error {
	if request.Credential != nil {
		return NewError(CodeRegistryAuthRequired)
	}
	if request.Host != GHCRHost || !ValidRepository(request.Repository, 1) {
		return NewError(CodeRegistryResponseInvalid)
	}
	expectedDigest := ""
	if lowerCommitSHA.MatchString(request.Reference) {
		// A full commit SHA is an immutable tag in the Cyclopus publication contract.
	} else if _, canonical, err := ParseDigest(request.Reference); err == nil {
		expectedDigest = canonical
	} else {
		return NewError(CodeRegistryResponseInvalid)
	}
	_, _, _, err := c.resolveManifest(ctx, c.registryURL(request.Repository, "manifests", request.Reference), expectedDigest)
	return err
}

func (c *Client) resolveManifest(ctx context.Context, target *url.URL, expectedDigest string) (ManifestEvidence, string, string, error) {
	body, mediaType, digest, err := c.getVerified(ctx, target, manifestAccept, MaxManifestBytes, ResponseManifest, expectedDigest)
	if err != nil {
		return ManifestEvidence{}, "", "", err
	}
	manifest, err := ParseManifest(mediaType, body)
	if err != nil {
		return ManifestEvidence{}, "", "", err
	}
	return manifest, mediaType, digest, nil
}

func (c *Client) manifestHEAD(ctx context.Context, target *url.URL) error {
	response, err := c.do(ctx, http.MethodHead, target, manifestAccept)
	if err != nil {
		return err
	}
	defer closeResponse(response)
	switch response.StatusCode {
	case http.StatusOK:
		if err := validateIdentityResponse(response); err != nil || response.ContentLength < 1 || response.ContentLength > MaxManifestBytes {
			return NewError(CodeRegistryResponseInvalid)
		}
		mediaType, mediaErr := responseMediaType(response.Header, true)
		digest, digestErr := exactHeader(response.Header, "Docker-Content-Digest")
		if mediaErr != nil || !IsSupportedManifestMediaType(mediaType) || digestErr != nil {
			return NewError(CodeRegistryResponseInvalid)
		}
		_, _, err = ParseDigest(digest)
		return err
	case http.StatusMethodNotAllowed:
		return nil
	case http.StatusUnauthorized:
		return NewError(CodeRegistryAuthRequired)
	default:
		return c.statusError(response, ResponseManifest)
	}
}

func (c *Client) getVerified(ctx context.Context, target *url.URL, accept string, limit int64, kind ResponseKind, expectedDigest string) ([]byte, string, string, error) {
	response, err := c.do(ctx, http.MethodGet, target, accept)
	if err != nil {
		return nil, "", "", err
	}
	defer closeResponse(response)
	if response.StatusCode == http.StatusUnauthorized {
		return nil, "", "", NewError(CodeRegistryAuthRequired)
	}
	if response.StatusCode != http.StatusOK {
		return nil, "", "", c.statusError(response, kind)
	}
	if err := validateIdentityResponse(response); err != nil || response.ContentLength > limit {
		return nil, "", "", NewError(CodeRegistryResponseInvalid)
	}
	mediaType, err := responseMediaType(response.Header, kind == ResponseManifest)
	if err != nil {
		return nil, "", "", err
	}
	if kind == ResponseManifest && !IsSupportedManifestMediaType(mediaType) || kind == ResponseConfigBlob && mediaType != accept && mediaType != "application/octet-stream" {
		return nil, "", "", NewError(CodeRegistryMediaTypeUnsupported)
	}
	digest, err := exactHeader(response.Header, "Docker-Content-Digest")
	if err != nil {
		return nil, "", "", err
	}
	if expectedDigest != "" && digest != expectedDigest {
		return nil, "", "", NewError(CodeRegistryDigestInvalid)
	}
	body, err := readBounded(response.Body, limit)
	if err != nil {
		return nil, "", "", err
	}
	canonical, err := VerifyBodyDigest(body, digest)
	if err != nil {
		return nil, "", "", err
	}
	if kind == ResponseConfigBlob {
		mediaType = accept
	}
	return body, mediaType, canonical, nil
}

func responseMediaType(header http.Header, allowUTF8Charset bool) (string, error) {
	raw, err := exactHeader(header, "Content-Type")
	if err != nil {
		return "", err
	}
	mediaType, parameters, err := mime.ParseMediaType(raw)
	if err != nil {
		return "", NewError(CodeRegistryResponseInvalid)
	}
	if len(parameters) == 0 {
		return mediaType, nil
	}
	charset, ok := parameters["charset"]
	if !allowUTF8Charset || len(parameters) != 1 || !ok || !strings.EqualFold(charset, "utf-8") {
		return "", NewError(CodeRegistryMediaTypeUnsupported)
	}
	return mediaType, nil
}

func (c *Client) do(ctx context.Context, method string, target *url.URL, accept string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return nil, NewError(CodeRegistryResponseInvalid)
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", clientUserAgent)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	if response == nil || response.Body == nil {
		return nil, NewError(CodeRegistryResponseInvalid)
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		closeResponse(response)
		return nil, NewError(CodeRegistryResponseInvalid)
	}
	return response, nil
}

func (c *Client) statusError(response *http.Response, kind ResponseKind) error {
	retryAfter, ok := ParseRetryAfter(response.Header.Values("Retry-After"), c.now())
	return &HTTPStatusError{StatusCode: response.StatusCode, Kind: kind, RetryAfter: retryAfter, HasRetryAfter: ok}
}

func (c *Client) registryURL(repository, operation, value string) *url.URL {
	segments := strings.Split(repository, "/")
	for index := range segments {
		segments[index] = url.PathEscape(segments[index])
	}
	result := *c.baseOrigin
	result.Path = "/v2/" + strings.Join(segments, "/") + "/" + url.PathEscape(operation) + "/" + url.PathEscape(value)
	result.RawPath = result.Path
	return &result
}

func validateIdentityResponse(response *http.Response) error {
	if response.Uncompressed || len(response.Header.Values("Content-Encoding")) != 0 || headerBytes(response.Header) > maxResponseHeaders {
		return NewError(CodeRegistryResponseInvalid)
	}
	return nil
}

func exactHeader(header http.Header, name string) (string, error) {
	values := header.Values(name)
	if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] || strings.Contains(values[0], ",") {
		return "", NewError(CodeRegistryResponseInvalid)
	}
	return values[0], nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || len(body) == 0 || int64(len(body)) > limit {
		return nil, NewError(CodeRegistryResponseInvalid)
	}
	return body, nil
}

func headerBytes(header http.Header) int {
	total := 2
	for name, values := range header {
		for _, value := range values {
			total += len(name) + len(value) + 4
		}
	}
	return total
}

func closeResponse(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorDrainBytes))
	_ = response.Body.Close()
}

var _ Resolver = (*Client)(nil)
