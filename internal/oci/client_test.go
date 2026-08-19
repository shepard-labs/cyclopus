package oci

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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

func TestResolvePrivateBearerAuthentication(t *testing.T) {
	username, pat, token := "private-user", "private-pat", "private-token"
	configBody := []byte(`{"architecture":"amd64","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest := clientTestDigest(configBody)
	manifestBody := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":%q,"size":%d},"layers":[]}`,
		MediaTypeOCIImageManifest, MediaTypeOCIConfig, configDigest, len(configBody)))
	var mu sync.Mutex
	var registryAuth []string
	tokenCalls := 0
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			mu.Lock()
			tokenCalls++
			mu.Unlock()
			if got, want := r.Header.Get("Authorization"), "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+pat)); got != want {
				t.Errorf("token Authorization = %q, want %q", got, want)
			}
			if r.URL.Query().Get("service") != GHCRHost || r.URL.Query().Get("scope") != "repository:owner/repo:pull" {
				t.Errorf("token query = %q", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			_, _ = w.Write([]byte(`{"token":"` + token + `","expires_in":300}`))
			return
		}
		mu.Lock()
		registryAuth = append(registryAuth, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token",service="ghcr.io",scope="repository:owner/repo:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", MediaTypeOCIImageManifest)
			w.Header().Set("Docker-Content-Digest", clientTestDigest(manifestBody))
			w.Header().Set("Content-Length", fmt.Sprint(len(manifestBody)))
			return
		}
		if strings.Contains(r.URL.Path, "/manifests/") {
			writeVerified(w, MediaTypeOCIImageManifest, manifestBody)
			return
		}
		writeVerified(w, MediaTypeOCIConfig, configBody)
	}))
	defer server.Close()

	credential := &Credential{Username: []byte(username), PAT: []byte(pat), OrgID: "org", RegistryID: "registry", Generation: 3}
	request := testResolveRequest()
	request.Credential = credential
	resolved, err := newTestClient(t, server).Resolve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Digest != clientTestDigest(manifestBody) || resolved.ConfigDigest != configDigest {
		t.Fatalf("resolved = %#v", resolved)
	}
	mu.Lock()
	defer mu.Unlock()
	if tokenCalls != 1 || fmt.Sprint(registryAuth) != fmt.Sprint([]string{
		"HEAD /v2/owner/repo/manifests/" + testSHA + " ",
		"HEAD /v2/owner/repo/manifests/" + testSHA + " Bearer " + token,
		"GET /v2/owner/repo/manifests/" + testSHA + " Bearer " + token,
		"GET /v2/owner/repo/blobs/" + configDigest + " Bearer " + token,
	}) {
		t.Fatalf("token calls=%d registry requests=%v", tokenCalls, registryAuth)
	}
	if string(credential.Username) != username || string(credential.PAT) != pat {
		t.Fatalf("Resolve mutated caller credential: %#v", credential)
	}
}

func TestPrivateTokenTransportRejectsUnsafeResponses(t *testing.T) {
	for _, test := range []struct {
		name  string
		serve func(http.ResponseWriter, *http.Request)
	}{
		{"unknown_charset", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=iso-8859-1")
			_, _ = w.Write([]byte(`{"token":"secret"}`))
		}},
		{"gzip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write([]byte(`{"token":"secret"}`))
		}},
		{"duplicate_type", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Add("Content-Type", "application/json")
			w.Header().Add("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"secret"}`))
		}},
		{"oversized_length", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", fmt.Sprint(maxTokenResponseBytes+1))
		}},
		{"oversized_body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.Repeat("x", maxTokenResponseBytes+1)))
		}},
		{"redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/stolen", http.StatusFound) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					test.serve(w, r)
					return
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token",service="ghcr.io",scope="repository:owner/repo:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer server.Close()
			request := testResolveRequest()
			request.Credential = &Credential{Username: []byte("user"), PAT: []byte("pat"), OrgID: "org", RegistryID: "registry", Generation: 1}
			_, err := newTestClient(t, server).Resolve(t.Context(), request)
			if !IsCode(err, CodeRegistryResponseInvalid) {
				t.Fatalf("Resolve() error = %v", err)
			}
		})
	}
}

func TestPrivateCredentialMetadataRequired(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid credential made a request") }))
	defer server.Close()
	for _, credential := range []*Credential{
		{Username: []byte("user"), PAT: []byte("pat"), RegistryID: "registry", Generation: 1},
		{Username: []byte("user"), PAT: []byte("pat"), OrgID: "org", Generation: 1},
		{Username: []byte("user"), PAT: []byte("pat"), OrgID: "org", RegistryID: "registry"},
	} {
		client := newTestClient(t, server)
		request := testResolveRequest()
		request.Credential = credential
		if _, err := client.Resolve(t.Context(), request); !IsCode(err, CodeCredentialValidationFailed) {
			t.Fatalf("credential %#v error = %v", credential, err)
		}
	}
}

func TestPrivateAuthenticationRefreshAndNoLeaks(t *testing.T) {
	username, pat, oldToken, freshToken, sentinel := "user-secret", "pat-secret", "old-secret", "fresh-secret", "upstream-secret"
	var tokenCalls, registryCalls int
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"` + freshToken + `"}`))
			return
		}
		registryCalls++
		if r.Header.Get("Authorization") != "Bearer "+freshToken {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token",service="ghcr.io",scope="repository:owner/repo:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(sentinel))
	}))
	defer server.Close()
	client := newTestClient(t, server)
	credential := &Credential{Username: []byte(username), PAT: []byte(pat), OrgID: "org", RegistryID: "registry", Generation: 1}
	key := credentialCacheKey(client.baseOrigin.Host, "owner/repo", credential)
	if _, err := client.tokens.load(t.Context(), key, client.now(), func() ([]byte, time.Time, error) { return []byte(oldToken), time.Now().Add(time.Minute), nil }); err != nil {
		t.Fatal(err)
	}
	request := testResolveRequest()
	request.Credential = credential
	_, err := client.Resolve(t.Context(), request)
	if !IsCode(err, CodeRegistryAuthFailed) || tokenCalls != 1 || registryCalls != 2 {
		t.Fatalf("Resolve() err=%v token/registry calls=%d/%d", err, tokenCalls, registryCalls)
	}
	for _, secret := range []string{username, pat, oldToken, freshToken, base64.StdEncoding.EncodeToString([]byte(username + ":" + pat)), sentinel} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %v", secret, err)
		}
	}
}

func TestClientClosePreventsFutureOperations(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("closed client made a request") }))
	defer server.Close()
	client := newTestClient(t, server)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resolve(t.Context(), testResolveRequest()); !IsCode(err, CodeClientClosed) {
		t.Fatalf("Resolve() after Close error = %v", err)
	}
	if err := client.ValidatePull(t.Context(), PullValidationRequest{Host: GHCRHost, Repository: "owner/repo", Reference: testSHA}); !IsCode(err, CodeClientClosed) {
		t.Fatalf("ValidatePull() after Close error = %v", err)
	}
}

func TestValidatePullPrivateTagAndDigest(t *testing.T) {
	body := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":"sha256:%s","size":1}]}`,
		MediaTypeOCIImageIndex, MediaTypeOCIImageManifest, strings.Repeat("1", 64)))
	for _, reference := range []string{testSHA, clientTestDigest(body)} {
		t.Run(reference[:6], func(t *testing.T) {
			var tokenCalls, registryCalls int
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokenCalls++
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"access_token":"token"}`))
					return
				}
				registryCalls++
				if r.Header.Get("Authorization") != "Bearer token" {
					w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token",service="ghcr.io",scope="repository:owner/repo:pull"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if strings.HasPrefix(reference, "sha256:") {
					writeVerified(w, MediaTypeOCIImageIndex, body)
					return
				}
				writeVerified(w, MediaTypeOCIImageIndex, body)
			}))
			defer server.Close()
			err := newTestClient(t, server).ValidatePull(t.Context(), PullValidationRequest{Host: GHCRHost, Repository: "owner/repo", Reference: reference, Credential: &Credential{Username: []byte("u"), PAT: []byte("p"), OrgID: "org", RegistryID: "registry", Generation: 1}})
			if err != nil || tokenCalls != 1 || registryCalls != 2 {
				t.Fatalf("ValidatePull() err=%v token/registry calls=%d/%d", err, tokenCalls, registryCalls)
			}
		})
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
