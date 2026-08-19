package oci

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseBearerChallenge(t *testing.T) {
	realm := "https://registry.example/token"
	valid := `Bearer realm="` + realm + `",service="ghcr.io",scope="repository:owner/repo:pull"`
	for _, header := range []http.Header{
		{"Www-Authenticate": {valid}},
		{"Www-Authenticate": {`Basic realm="other", ` + valid}},
		{"Www-Authenticate": {valid + `, Unknown abc123`}},
		{"Www-Authenticate": {valid + `,extra="x"`}},
		{"Www-Authenticate": {"\tBasic\trealm=\"other\"", "\tBearer\trealm=\"" + realm + "\",service=\"ghcr.io\",scope=\"repository:owner/repo:pull\""}},
	} {
		challenge, err := parseBearerChallenge(header, realm, "owner/repo")
		if err != nil || challenge.realm.String() != realm || challenge.service != GHCRHost || challenge.scope != "repository:owner/repo:pull" {
			t.Fatalf("parseBearerChallenge(%q) = %#v, %v", header.Values("WWW-Authenticate"), challenge, err)
		}
	}
	for _, value := range []http.Header{
		{"Www-Authenticate": {valid, valid}},
		{"Www-Authenticate": {`Basic realm="other"`}},
		{"Www-Authenticate": {`Unknown !unsafe, ` + valid}},
		{"Www-Authenticate": {`Bearer realm="` + realm + `",service="ghcr.io"`}},
		{"Www-Authenticate": {`Bearer realm="` + realm + `",realm="` + realm + `",service="ghcr.io",scope="repository:owner/repo:pull"`}},
		{"Www-Authenticate": {`Bearer realm="https://registry.example/token\",service="ghcr.io",scope="repository:owner/repo:pull"`}},
		{"Www-Authenticate": {`Bearer realm="http://registry.example/token",service="ghcr.io",scope="repository:owner/repo:pull"`}},
		{"Www-Authenticate": {`Bearer realm="https://user@registry.example/token",service="ghcr.io",scope="repository:owner/repo:pull"`}},
		{"Www-Authenticate": {`Bearer realm="https://registry.example/token?x=1",service="ghcr.io",scope="repository:owner/repo:pull"`}},
		{"Www-Authenticate": {`Bearer realm="https://registry.example/token",service="other",scope="repository:owner/repo:pull"`}},
		{"Www-Authenticate": {`Bearer realm="` + realm + `",service="ghcr.io",scope="repository:owner/repo:pull,push"`}},
		{"Www-Authenticate": {`Bearer realm="` + realm + `",service="ghcr.io",scope="repository:owner/repo:pull%20push"`}},
		{"Www-Authenticate": {`Bearer realm="` + realm + `#fragment",service="ghcr.io",scope="repository:owner/repo:pull"`}},
		{"Www-Authenticate": {`Bearer realm="https://other.example/token",service="ghcr.io",scope="repository:owner/repo:pull"`}},
		{"Www-Authenticate": {`Bearer realm="` + realm + `/other",service="ghcr.io",scope="repository:owner/repo:pull"`}},
		{"Www-Authenticate": {`Bearer realm="` + realm + `",service="*.io",scope="repository:owner/repo:pull"`}},
		{"Www-Authenticate": {`bearer realm="` + realm + `",service="GHCR.IO",scope="repository:owner/repo:pull"`}},
		{"Www-Authenticate": {`Bearer realm="` + realm + `",service="ghcr.io",scope="repository:owner%2Frepo:pull"`}},
		{"Www-Authenticate": {`Bearer realm="` + realm + `",service="ghcr.io",scope="repository:owner/repo%2Fchild:pull"`}},
	} {
		if _, err := parseBearerChallenge(value, realm, "owner/repo"); !IsCode(err, CodeRegistryAuthChallengeInvalid) {
			t.Fatalf("header %q error = %v", value.Values("WWW-Authenticate"), err)
		}
	}
}

func TestTokenCacheIsolationExpiryEvictionAndClose(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	cache := newTokenCache()
	credential := &Credential{Username: []byte("user"), PAT: []byte("pat"), OrgID: "org", RegistryID: "registry", Generation: 1}
	key := credentialCacheKey(GHCRHost, "owner/repo", credential)
	loads := 0
	load := func(value string, expiry time.Time) ([]byte, error) {
		return cache.load(context.Background(), key, now, func() ([]byte, time.Time, error) {
			loads++
			return []byte(value), expiry, nil
		})
	}
	if token, err := load("one", now.Add(time.Minute)); err != nil || string(token) != "one" {
		t.Fatalf("initial load = %q, %v", token, err)
	}
	if token, err := load("two", now.Add(time.Minute)); err != nil || string(token) != "one" || loads != 1 {
		t.Fatalf("cached load = %q, %v; loads=%d", token, err, loads)
	}
	for _, other := range []tokenCacheKey{
		credentialCacheKey(GHCRHost, "owner/other", credential),
		credentialCacheKey(GHCRHost, "owner/repo", &Credential{Username: []byte("other"), PAT: []byte("pat"), OrgID: "org", RegistryID: "registry", Generation: 1}),
		credentialCacheKey(GHCRHost, "owner/repo", &Credential{Username: []byte("user"), PAT: []byte("pat"), OrgID: "other", RegistryID: "registry", Generation: 1}),
		credentialCacheKey(GHCRHost, "owner/repo", &Credential{Username: []byte("user"), PAT: []byte("pat"), OrgID: "org", RegistryID: "other", Generation: 1}),
		credentialCacheKey(GHCRHost, "owner/repo", &Credential{Username: []byte("user"), PAT: []byte("other"), OrgID: "org", RegistryID: "registry", Generation: 1}),
		credentialCacheKey(GHCRHost, "owner/repo", &Credential{Username: []byte("user"), PAT: []byte("pat"), OrgID: "org", RegistryID: "registry", Generation: 2}),
	} {
		if _, err := cache.load(context.Background(), other, now, func() ([]byte, time.Time, error) { return []byte("isolated"), now.Add(time.Minute), nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) != 7 {
		t.Fatalf("cache entries = %d", len(cache.entries))
	}
	if _, err := cache.load(context.Background(), key, now.Add(31*time.Second), func() ([]byte, time.Time, error) { loads++; return []byte("renewed"), now.Add(time.Minute), nil }); err != nil || loads != 2 {
		t.Fatalf("skew expiry err=%v loads=%d", err, loads)
	}

	evicted := make([]byte, 0)
	var observedCached []byte
	var evictedCached []byte
	for index := 0; index <= maxTokenCacheEntries; index++ {
		entryKey := credentialCacheKey(GHCRHost, fmt.Sprintf("owner/repo-%d", index), &Credential{Username: []byte(fmt.Sprintf("user-%d", index)), PAT: []byte("pat"), OrgID: "org", RegistryID: "registry", Generation: 1})
		value := []byte(fmt.Sprintf("token-%d", index))
		if index == 0 {
			evicted = value
		}
		if _, err := cache.load(context.Background(), entryKey, now, func() ([]byte, time.Time, error) { return value, now.Add(time.Minute), nil }); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			evictedCached = cache.entries[entryKey].Value.(*tokenCacheEntry).token
		}
	}
	if len(cache.entries) != maxTokenCacheEntries {
		t.Fatalf("bounded cache entries = %d", len(cache.entries))
	}
	if element := cache.entries[credentialCacheKey(GHCRHost, "owner/repo-1", &Credential{Username: []byte("user-1"), PAT: []byte("pat"), OrgID: "org", RegistryID: "registry", Generation: 1})]; element != nil {
		observedCached = element.Value.(*tokenCacheEntry).token
	}
	if got := string(evicted); got != "token-0" { // The cache clones tokens; caller buffers are never zeroed.
		t.Fatalf("caller token mutated to %q", got)
	}
	for _, value := range evictedCached {
		if value != 0 {
			t.Fatalf("LRU eviction did not zero cached token %q", evictedCached)
		}
	}
	for element := cache.lru.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*tokenCacheEntry)
		if len(entry.token) == 0 {
			t.Fatal("empty cached token")
		}
	}
	cache.close()
	cache.close()
	if len(cache.entries) != 0 || cache.lru.Len() != 0 {
		t.Fatalf("closed cache retained entries=%d lru=%d", len(cache.entries), cache.lru.Len())
	}
	if len(observedCached) == 0 {
		t.Fatal("expected observed cached token")
	}
	for _, value := range observedCached {
		if value != 0 {
			t.Fatalf("close did not zero cached token %q", observedCached)
		}
	}
	if _, err := cache.load(context.Background(), key, now, func() ([]byte, time.Time, error) { return []byte("no"), now, nil }); !IsCode(err, CodeClientClosed) {
		t.Fatalf("load after close error = %v", err)
	}
}

func TestTokenCacheSingleFlight(t *testing.T) {
	cache := newTokenCache()
	key := credentialCacheKey(GHCRHost, "owner/repo", &Credential{Username: []byte("user"), PAT: []byte("pat"), OrgID: "org", RegistryID: "registry", Generation: 1})
	now := time.Now()
	var calls atomic.Int32
	start := make(chan struct{})
	var group sync.WaitGroup
	errors := make(chan error, 50)
	for range 50 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := cache.load(context.Background(), key, now, func() ([]byte, time.Time, error) {
				calls.Add(1)
				<-start
				return []byte("token"), now.Add(time.Minute), nil
			})
			errors <- err
		}()
	}
	time.Sleep(10 * time.Millisecond)
	close(start)
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("token fetches = %d", calls.Load())
	}
}

func TestTokenCacheExpirySkewAndCanceledWaiters(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	cache := newTokenCache()
	key := credentialCacheKey(GHCRHost, "owner/repo", &Credential{Username: []byte("user"), PAT: []byte("pat"), OrgID: "org", RegistryID: "registry", Generation: 1})
	loads := 0
	if token, err := cache.load(context.Background(), key, now, func() ([]byte, time.Time, error) { loads++; return []byte("token"), now.Add(tokenExpirySkew), nil }); err != nil || string(token) != "token" {
		t.Fatalf("current token = %q, %v", token, err)
	}
	if _, err := cache.load(context.Background(), key, now, func() ([]byte, time.Time, error) { loads++; return []byte("renewed"), now.Add(time.Minute), nil }); err != nil || loads != 2 {
		t.Fatalf("skew token cached: err=%v loads=%d", err, loads)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	leaderContext, cancelLeader := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() {
		_, err := cache.load(leaderContext, key, now.Add(31*time.Second), func() ([]byte, time.Time, error) {
			close(started)
			<-release
			return []byte("shared"), now.Add(time.Minute), nil
		})
		leader <- err
	}()
	<-started
	waiter := make(chan struct {
		token []byte
		err   error
	}, 1)
	var duplicateFetch atomic.Bool
	go func() {
		token, err := cache.load(context.Background(), key, now.Add(31*time.Second), func() ([]byte, time.Time, error) {
			duplicateFetch.Store(true)
			return []byte("duplicate"), now.Add(time.Minute), nil
		})
		waiter <- struct {
			token []byte
			err   error
		}{token, err}
	}()
	for {
		cache.mu.Lock()
		joined := cache.inflight[key] != nil && cache.inflight[key].waiters == 2
		cache.mu.Unlock()
		if joined {
			break
		}
		runtime.Gosched()
	}
	cancelLeader()
	if err := <-leader; err != context.Canceled {
		t.Fatalf("canceled leader error = %v", err)
	}
	close(release)
	result := <-waiter
	if result.err != nil || string(result.token) != "shared" || loads != 2 || duplicateFetch.Load() {
		t.Fatalf("waiter = %q, %v; loads=%d", result.token, result.err, loads)
	}
}

func TestParseTokenResponse(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, body string
		want       string
		ok         bool
	}{
		{"token", `{"token":"one","expires_in":60}`, "one", true},
		{"alternate", `{"access_token":"two"}`, "two", true},
		{"both_equal", `{"token":"one","access_token":"one"}`, "one", true},
		{"conflict", `{"token":"one","access_token":"two"}`, "", false},
		{"duplicate", `{"token":"one","token":"two"}`, "", false},
		{"nested", `{"token":{"nested":"no"}}`, "", false},
		{"trailing", `{"token":"one"} x`, "", false},
		{"unknown", `{"token":"one","extra":true}`, "", false},
		{"empty", `{"token":""}`, "", false},
		{"expired", `{"token":"one","expires_in":0}`, "", false},
		{"negative", `{"token":"one","expires_in":-1}`, "", false},
		{"capped", `{"token":"one","expires_in":3601}`, "one", true},
		{"safe_integer_capped", `{"token":"one","expires_in":9007199254740991}`, "one", true},
		{"beyond_safe_integer", `{"token":"one","expires_in":9007199254740992}`, "", false},
		{"nonintegral", `{"token":"one","expires_in":1.5}`, "", false},
		{"future_issue", `{"token":"one","issued_at":"2026-08-19T12:00:31Z"}`, "", false},
		{"invalid_issue", `{"token":"one","issued_at":"no"}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, expiry, err := parseTokenResponse([]byte(tc.body), now)
			if (err == nil) != tc.ok || tc.ok && string(token) != tc.want {
				t.Fatalf("parseTokenResponse(%s) = %q, %v", tc.body, token, err)
			}
			if tc.name == "capped" || tc.name == "safe_integer_capped" {
				if !expiry.Equal(now.Add(time.Hour)) {
					t.Fatalf("expiry = %v, want %v", expiry, now.Add(time.Hour))
				}
			}
			if tc.name == "alternate" && !expiry.Equal(now.Add(time.Minute)) {
				t.Fatalf("omitted expiry = %v, want %v", expiry, now.Add(time.Minute))
			}
			if tc.ok {
				token[0] = 'X'
				if token[0] != 'X' {
					t.Fatal("token is not mutable")
				}
			}
		})
	}
	if _, _, err := parseTokenResponse([]byte(`{"token":"`+strings.Repeat("x", maxBearerTokenBytes+1)+`"}`), now); err == nil {
		t.Fatal("oversized token accepted")
	}
}
