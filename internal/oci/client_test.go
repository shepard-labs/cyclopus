package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func TestClientPublicResolve(t *testing.T) {
	configBody := []byte(`{"architecture":"amd64","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest := clientTestDigest(configBody)
	manifestBody := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":%q,"size":%d},"layers":[]}`,
		MediaTypeOCIImageManifest, MediaTypeOCIConfig, configDigest, len(configBody)))
	manifestDigest := clientTestDigest(manifestBody)

	var mu sync.Mutex
	var requests []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests = append(requests, request.Method+" "+request.URL.RequestURI())
		mu.Unlock()
		if request.Header.Get("Authorization") != "" || request.Header.Get("Accept-Encoding") != "identity" || request.Header.Get("User-Agent") != clientUserAgent {
			t.Errorf("unsafe request headers: %#v", request.Header)
		}
		if strings.Contains(request.URL.Path, "/manifests/") && request.Header.Get("Accept") != manifestAccept {
			t.Errorf("manifest Accept = %q", request.Header.Get("Accept"))
		}
		if strings.Contains(request.URL.Path, "/blobs/") && request.Header.Get("Accept") != MediaTypeOCIConfig {
			t.Errorf("config Accept = %q", request.Header.Get("Accept"))
		}
		switch {
		case request.Method == http.MethodHead:
			writer.Header().Set("Content-Type", MediaTypeOCIImageManifest)
			writer.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("0", 64))
			writer.Header().Set("Content-Length", fmt.Sprint(len(manifestBody)))
		case strings.Contains(request.URL.Path, "/manifests/"):
			writeVerified(writer, MediaTypeOCIImageManifest, manifestBody)
		case strings.Contains(request.URL.Path, "/blobs/"):
			writeVerified(writer, MediaTypeOCIConfig, configBody)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server)
	resolved, err := client.Resolve(context.Background(), ResolveRequest{Host: GHCRHost, Repository: "owner/repo", Tag: testSHA,
		ExpectedSource: RepositoryIdentity{Owner: "owner", Repository: "repo"}, ExpectedRevision: testSHA, AnnotationPolicy: AnnotationPolicyValidateIfPresent})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Digest != manifestDigest || resolved.ConfigDigest != configDigest || resolved.AnnotationStatus != AnnotationStatusAbsent ||
		resolved.ImmutableReference != "ghcr.io/owner/repo@"+manifestDigest {
		t.Fatalf("unexpected result: %#v", resolved)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"HEAD /v2/owner/repo/manifests/" + testSHA, "GET /v2/owner/repo/manifests/" + testSHA, "GET /v2/owner/repo/blobs/" + configDigest}
	if fmt.Sprint(requests) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", requests, want)
	}
}

func TestResolveRealisticScratchConfig(t *testing.T) {
	configBody := []byte(`{"created":"2026-08-19T00:00:00Z","architecture":"amd64","os":"linux","config":{"Env":["PATH=/usr/bin"],"Cmd":["/app"]},"rootfs":{"type":"layers","diff_ids":[]},"history":[{"created":"2026-08-19T00:00:00Z","created_by":"COPY app /app"}]}`)
	configDigest := clientTestDigest(configBody)
	manifestBody := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":%q,"size":%d},"layers":[]}`,
		MediaTypeOCIImageManifest, MediaTypeOCIConfig, configDigest, len(configBody)))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
		} else if strings.Contains(r.URL.Path, "/manifests/") {
			writeVerified(w, MediaTypeOCIImageManifest, manifestBody)
		} else {
			writeVerified(w, MediaTypeOCIConfig, configBody)
		}
	}))
	defer server.Close()
	if _, err := newTestClient(t, server).Resolve(context.Background(), testResolveRequest()); err != nil {
		t.Fatal(err)
	}
}

func TestResolveHEAD405StillRequiresGETAndIndexDoesNotFetchChildren(t *testing.T) {
	childDigest := "sha256:" + strings.Repeat("1", 64)
	body := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":%q,"size":1}]}`,
		MediaTypeOCIImageIndex, MediaTypeOCIImageManifest, childDigest))
	gets := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodHead {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		gets++
		writeVerified(writer, MediaTypeOCIImageIndex, body)
	}))
	defer server.Close()
	resolved, err := newTestClient(t, server).Resolve(context.Background(), ResolveRequest{Host: GHCRHost, Repository: "owner/repo", Tag: testSHA,
		ExpectedSource: RepositoryIdentity{Owner: "owner", Repository: "repo"}, ExpectedRevision: testSHA, AnnotationPolicy: AnnotationPolicyValidateIfPresent})
	if err != nil {
		t.Fatal(err)
	}
	if gets != 1 || resolved.ConfigDigest != "" || resolved.Digest != clientTestDigest(body) {
		t.Fatalf("gets=%d result=%#v", gets, resolved)
	}
}

func TestResolveHTTPFailures(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		retryAfter string
		code       Code
		retryable  bool
	}{
		{"not_found", http.StatusNotFound, "", CodeRegistryUnavailable, true},
		{"auth", http.StatusUnauthorized, "", CodeRegistryAuthRequired, false},
		{"rate", http.StatusTooManyRequests, "7", CodeRegistryRateLimited, true},
		{"server", http.StatusServiceUnavailable, "", CodeRegistryUnavailable, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if test.retryAfter != "" {
					writer.Header().Set("Retry-After", test.retryAfter)
				}
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte("secret upstream body"))
			}))
			defer server.Close()
			_, err := newTestClient(t, server).Resolve(context.Background(), testResolveRequest())
			if err == nil || strings.Contains(err.Error(), "secret upstream body") {
				t.Fatalf("error = %v", err)
			}
			disposition := Classify(err, time.Now())
			if disposition.Code != string(test.code) || disposition.Retryable != test.retryable {
				t.Fatalf("disposition = %#v", disposition)
			}
			if test.retryAfter != "" && disposition.RetryAfter != 7*time.Second {
				t.Fatalf("retry after = %v", disposition.RetryAfter)
			}
		})
	}
}

func TestResolveRejectsRedirectEncodingHeadersAndBounds(t *testing.T) {
	validBody := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":"sha256:%s","size":1}]}`,
		MediaTypeOCIImageIndex, MediaTypeOCIImageManifest, strings.Repeat("1", 64)))
	tests := []struct {
		name  string
		serve func(http.ResponseWriter, *http.Request)
	}{
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.com/stolen", http.StatusFound)
		}},
		{"gzip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			writeVerified(w, MediaTypeOCIImageIndex, validBody)
		}},
		{"duplicate_type", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Add("Content-Type", MediaTypeOCIImageIndex)
			w.Header().Add("Content-Type", MediaTypeOCIImageIndex)
			w.Header().Set("Docker-Content-Digest", clientTestDigest(validBody))
			_, _ = w.Write(validBody)
		}},
		{"missing_digest", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", MediaTypeOCIImageIndex)
			_, _ = w.Write(validBody)
		}},
		{"duplicate_digest", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", MediaTypeOCIImageIndex)
			w.Header().Add("Docker-Content-Digest", clientTestDigest(validBody))
			w.Header().Add("Docker-Content-Digest", clientTestDigest(validBody))
			_, _ = w.Write(validBody)
		}},
		{"missing_type", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Docker-Content-Digest", clientTestDigest(validBody))
			_, _ = w.Write(validBody)
		}},
		{"digest_mismatch", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", MediaTypeOCIImageIndex)
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("2", 64))
			_, _ = w.Write(validBody)
		}},
		{"document_media_mismatch", func(w http.ResponseWriter, _ *http.Request) {
			writeVerified(w, MediaTypeDockerList, validBody)
		}},
		{"oversized_length", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(MaxManifestBytes+1))
			w.WriteHeader(http.StatusOK)
		}},
		{"oversized_chunked", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", MediaTypeOCIImageIndex)
			w.Header().Set("Docker-Content-Digest", clientTestDigest(make([]byte, MaxManifestBytes+1)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(make([]byte, MaxManifestBytes+1))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				test.serve(w, r)
			}))
			defer server.Close()
			_, err := newTestClient(t, server).Resolve(context.Background(), testResolveRequest())
			if err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestResolveConfigDescriptorFailures(t *testing.T) {
	configBody := []byte(`{"architecture":"amd64","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":[]}}`)
	digest := clientTestDigest(configBody)
	tests := []struct {
		name            string
		descriptorSize  int
		descriptorMedia string
		blobMedia       string
		blobDigest      string
	}{
		{"size", len(configBody) + 1, MediaTypeOCIConfig, MediaTypeOCIConfig, digest},
		{"media", len(configBody), MediaTypeOCIConfig, MediaTypeDockerConfig, digest},
		{"digest", len(configBody), MediaTypeOCIConfig, MediaTypeOCIConfig, "sha256:" + strings.Repeat("3", 64)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":%q,"size":%d},"layers":[]}`,
				MediaTypeOCIImageManifest, test.descriptorMedia, digest, test.descriptorSize))
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusMethodNotAllowed)
				} else if strings.Contains(r.URL.Path, "/manifests/") {
					writeVerified(w, MediaTypeOCIImageManifest, manifest)
				} else {
					w.Header().Set("Content-Type", test.blobMedia)
					w.Header().Set("Docker-Content-Digest", test.blobDigest)
					_, _ = w.Write(configBody)
				}
			}))
			defer server.Close()
			if _, err := newTestClient(t, server).Resolve(context.Background(), testResolveRequest()); err == nil {
				t.Fatal("expected config rejection")
			}
		})
	}
}

func TestResolveConfigContentTypePolicy(t *testing.T) {
	configBody := []byte(`{"architecture":"amd64","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":[]}}`)
	digest := clientTestDigest(configBody)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":%q,"size":%d},"layers":[]}`,
		MediaTypeOCIImageManifest, MediaTypeOCIConfig, digest, len(configBody)))
	for _, test := range []struct {
		name, contentType string
		wantError         bool
	}{
		{"descriptor_type", MediaTypeOCIConfig, false},
		{"octet_stream", "application/octet-stream", false},
		{"wrong_type", "application/json", true},
		{"parameter", MediaTypeOCIConfig + "; charset=utf-8", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusMethodNotAllowed)
				} else if strings.Contains(r.URL.Path, "/manifests/") {
					writeVerified(w, MediaTypeOCIImageManifest, manifest)
				} else {
					writeVerified(w, test.contentType, configBody)
				}
			}))
			defer server.Close()
			_, err := newTestClient(t, server).Resolve(context.Background(), testResolveRequest())
			if (err != nil) != test.wantError {
				t.Fatalf("Resolve() error = %v, wantError %v", err, test.wantError)
			}
		})
	}
}

func TestResolveManifestContentTypePolicy(t *testing.T) {
	body := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":"sha256:%s","size":1}]}`,
		MediaTypeOCIImageIndex, MediaTypeOCIImageManifest, strings.Repeat("1", 64)))
	for _, test := range []struct {
		name, contentType string
		wantError         bool
	}{
		{"base", MediaTypeOCIImageIndex, false},
		{"case_and_charset", "Application/Vnd.Oci.Image.Index.V1+Json; Charset=UTF-8", false},
		{"unknown_parameter", MediaTypeOCIImageIndex + "; version=1", true},
		{"wrong_charset", MediaTypeOCIImageIndex + "; charset=iso-8859-1", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				writeVerified(w, test.contentType, body)
			}))
			defer server.Close()
			_, err := newTestClient(t, server).Resolve(context.Background(), testResolveRequest())
			if (err != nil) != test.wantError {
				t.Fatalf("Resolve() error = %v, wantError %v", err, test.wantError)
			}
		})
	}
}

func TestClientTimeoutCancellationAndValidatePull(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(100 * time.Millisecond) }))
		defer server.Close()
		client := newTestClientWithTimeout(t, server, 20*time.Millisecond)
		_, err := client.Resolve(context.Background(), testResolveRequest())
		if err == nil || !Classify(err, time.Now()).Retryable {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client, err := NewClient(ClientConfig{HTTPClient: http.DefaultClient, RequestTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Resolve(ctx, testResolveRequest()); err == nil {
			t.Fatal("expected cancellation")
		}
	})
	t.Run("validate", func(t *testing.T) {
		body := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":"sha256:%s","size":1}]}`,
			MediaTypeOCIImageIndex, MediaTypeOCIImageManifest, strings.Repeat("1", 64)))
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RequestURI() != "/v2/owner/repo/manifests/"+testSHA || r.Header.Get("Authorization") != "" {
				t.Errorf("request = %s headers=%v", r.URL.RequestURI(), r.Header)
			}
			writeVerified(w, MediaTypeOCIImageIndex, body)
		}))
		defer server.Close()
		if err := newTestClient(t, server).ValidatePull(context.Background(), PullValidationRequest{Host: GHCRHost, Repository: "owner/repo", Reference: testSHA}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestValidatePullReferenceAndStatus(t *testing.T) {
	body := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":"sha256:%s","size":1}]}`,
		MediaTypeOCIImageIndex, MediaTypeOCIImageManifest, strings.Repeat("1", 64)))
	digest := clientTestDigest(body)
	for _, test := range []struct {
		name, reference string
		status          int
		wantCode        Code
	}{
		{"digest", digest, http.StatusOK, ""},
		{"unauthorized", testSHA, http.StatusUnauthorized, CodeRegistryAuthRequired},
		{"not_found", testSHA, http.StatusNotFound, CodeRegistryUnavailable},
		{"invalid_reference", "latest", http.StatusOK, CodeRegistryResponseInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v2/owner/repo/manifests/"+url.PathEscape(test.reference) {
					t.Errorf("path = %q", r.URL.Path)
				}
				if test.status != http.StatusOK {
					w.WriteHeader(test.status)
					return
				}
				writeVerified(w, MediaTypeOCIImageIndex, body)
			}))
			defer server.Close()
			err := newTestClient(t, server).ValidatePull(context.Background(), PullValidationRequest{Host: GHCRHost, Repository: "owner/repo", Reference: test.reference})
			if test.wantCode == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantCode != "" && Classify(err, time.Now()).Code != string(test.wantCode) {
				t.Fatalf("error = %v, disposition = %#v", err, Classify(err, time.Now()))
			}
		})
	}
}

func TestClientConstructorAndPathPolicy(t *testing.T) {
	if _, err := NewClient(ClientConfig{}); err == nil {
		t.Fatal("expected invalid config")
	}
	for _, origin := range []string{"http://example.com", "https://user@example.com", "https://example.com/path", "https://example.com?q=1", " https://example.com", `https:\\example.com`} {
		if _, err := newClientForTest(ClientConfig{HTTPClient: http.DefaultClient, RequestTimeout: time.Second}, origin); err == nil {
			t.Fatalf("accepted origin %q", origin)
		}
	}
	client, err := NewClient(ClientConfig{HTTPClient: http.DefaultClient, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got := client.registryURL("owner/a.b_c-d", "manifests", testSHA).String(); got != "https://ghcr.io/v2/owner/a.b_c-d/manifests/"+testSHA {
		t.Fatalf("URL = %q", got)
	}
	redirecting := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	redirectClient, err := NewClient(ClientConfig{HTTPClient: redirecting, RequestTimeout: time.Second})
	if err != nil || redirectClient.httpClient == redirecting || redirectClient.httpClient.CheckRedirect == nil {
		t.Fatalf("redirect policy was not cloned and enforced: client=%#v err=%v", redirectClient, err)
	}
	if err := redirectClient.httpClient.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("redirect policy error = %v", err)
	}
	transport := redirectClient.httpClient.Transport.(*http.Transport)
	if transport.MaxResponseHeaderBytes != maxResponseHeaders || !transport.DisableCompression {
		t.Fatalf("transport bounds not enforced: %#v", transport)
	}
	if _, err := NewClient(ClientConfig{HTTPClient: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil })}, RequestTimeout: time.Second}); err == nil {
		t.Fatal("accepted unsupported custom transport")
	}
	if _, err := client.Resolve(context.Background(), ResolveRequest{Host: GHCRHost, Repository: "owner/../repo", Tag: testSHA}); err == nil {
		t.Fatal("accepted traversal")
	}
}

func TestClientDropsCallerCookieJar(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("https://ghcr.io")
	jar.SetCookies(origin, []*http.Cookie{{Name: "session", Value: "secret"}})
	caller := &http.Client{Jar: jar}
	client, err := NewClient(ClientConfig{HTTPClient: caller, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if client.httpClient.Jar != nil {
		t.Fatal("cloned OCI client retained caller cookie jar")
	}
	if len(jar.Cookies(origin)) != 1 {
		t.Fatal("constructing OCI client mutated caller cookie jar")
	}
}

func TestResolveOversizedHeadersFailAtTransport(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Oversized", strings.Repeat("x", maxResponseHeaders))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	_, err := newTestClient(t, server).Resolve(context.Background(), testResolveRequest())
	if err == nil || !Classify(err, time.Now()).Retryable {
		t.Fatalf("error = %v, disposition = %#v", err, Classify(err, time.Now()))
	}
}

func TestResolveRejectsCredentialUntilBearerSupport(t *testing.T) {
	client, err := NewClient(ClientConfig{HTTPClient: http.DefaultClient, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	request := testResolveRequest()
	request.Credential = &Credential{Username: []byte("user"), PAT: []byte("secret")}
	if _, err := client.Resolve(context.Background(), request); !IsCode(err, CodeRegistryAuthRequired) {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveHEAD200DigestIsPreliminary(t *testing.T) {
	body := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":"sha256:%s","size":1}]}`,
		MediaTypeOCIImageIndex, MediaTypeOCIImageManifest, strings.Repeat("1", 64)))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", MediaTypeOCIImageIndex)
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("0", 64))
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			return
		}
		writeVerified(w, MediaTypeOCIImageIndex, body)
	}))
	defer server.Close()
	resolved, err := newTestClient(t, server).Resolve(context.Background(), testResolveRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Digest != clientTestDigest(body) {
		t.Fatalf("digest = %q", resolved.Digest)
	}
}

func TestResolveEvidenceNormalization(t *testing.T) {
	body := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":"sha256:%s","size":1}],"annotations":{"%s":"https://github.com/OWNER/repo.git/","%s":%q}}`,
		MediaTypeOCIImageIndex, MediaTypeOCIImageManifest, strings.Repeat("1", 64), AnnotationSource, AnnotationRevision, testSHA))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeVerified(w, MediaTypeOCIImageIndex, body)
	}))
	defer server.Close()
	resolved, err := newTestClient(t, server).Resolve(context.Background(), testResolveRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Source != "https://github.com/owner/repo" || resolved.Revision != testSHA || resolved.AnnotationStatus != AnnotationStatusMatched {
		t.Fatalf("evidence = %#v", resolved)
	}
}

func newTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	return newTestClientWithTimeout(t, server, time.Second)
}

func newTestClientWithTimeout(t *testing.T, server *httptest.Server, timeout time.Duration) *Client {
	t.Helper()
	client, err := newClientForTest(ClientConfig{HTTPClient: server.Client(), RequestTimeout: timeout}, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func testResolveRequest() ResolveRequest {
	return ResolveRequest{Host: GHCRHost, Repository: "owner/repo", Tag: testSHA,
		ExpectedSource: RepositoryIdentity{Owner: "owner", Repository: "repo"}, ExpectedRevision: testSHA, AnnotationPolicy: AnnotationPolicyValidateIfPresent}
}

func clientTestDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func writeVerified(writer http.ResponseWriter, mediaType string, body []byte) {
	writer.Header().Set("Content-Type", mediaType)
	writer.Header().Set("Docker-Content-Digest", clientTestDigest(body))
	_, _ = writer.Write(body)
}
