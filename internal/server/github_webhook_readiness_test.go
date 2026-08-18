package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type readinessSecretResolver struct {
	mu      sync.Mutex
	secret  []byte
	err     error
	block   bool
	refs    []string
	calls   atomic.Int32
	lastOut []byte
}

func (r *readinessSecretResolver) Resolve(ctx context.Context, ref string) ([]byte, error) {
	r.calls.Add(1)
	r.mu.Lock()
	r.refs = append(r.refs, ref)
	block, err := r.block, r.err
	out := append([]byte(nil), r.secret...)
	r.lastOut = out
	r.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return out, err
}

func TestGitHubWebhookSecretCheckerActiveOnlyZeroesAndSanitizes(t *testing.T) {
	secret := "readiness-secret-must-not-leak"
	resolver := &readinessSecretResolver{secret: []byte(secret)}
	checker := NewGitHubWebhookSecretChecker(resolver, "active-ref", time.Minute, time.Second)
	if err := checker.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	resolver.mu.Lock()
	if len(resolver.refs) != 1 || resolver.refs[0] != "active-ref" {
		resolver.mu.Unlock()
		t.Fatalf("resolved refs = %v, want active only", resolver.refs)
	}
	for _, value := range resolver.lastOut {
		if value != 0 {
			resolver.mu.Unlock()
			t.Fatal("resolved secret bytes were not zeroed")
		}
	}
	resolver.err = errors.New("resolver exposed " + secret)
	resolver.mu.Unlock()
	checker.checked = time.Time{}
	err := checker.Check(t.Context())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("readiness error = %v", err)
	}
}

func TestGitHubWebhookSecretCheckerConcurrentCacheFailureAndRecovery(t *testing.T) {
	resolver := &readinessSecretResolver{secret: []byte("active")}
	checker := NewGitHubWebhookSecretChecker(resolver, "active-ref", time.Minute, time.Second)
	var probeCalls atomic.Int32
	checker.probe = func(secret []byte) error {
		probeCalls.Add(1)
		return probeGitHubWebhookHMAC(secret)
	}
	if err := checker.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := checker.Check(t.Context()); err != nil {
				t.Errorf("cached check: %v", err)
			}
		}()
	}
	wg.Wait()
	if resolver.calls.Load() != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls.Load())
	}
	if probeCalls.Load() != 1 {
		t.Fatalf("HMAC probe calls = %d, want 1", probeCalls.Load())
	}
	resolver.mu.Lock()
	resolver.err = errors.New("unavailable")
	resolver.mu.Unlock()
	checker.mu.Lock()
	checker.checked = time.Time{}
	checker.mu.Unlock()
	if err := checker.Check(t.Context()); err == nil {
		t.Fatal("expired successful cache remained ready after resolver failure")
	}
	resolver.mu.Lock()
	resolver.err = nil
	resolver.secret = []byte("restored")
	resolver.mu.Unlock()
	checker.mu.Lock()
	checker.checked = time.Time{}
	checker.mu.Unlock()
	if err := checker.Check(t.Context()); err != nil {
		t.Fatalf("restored check: %v", err)
	}
	if probeCalls.Load() != 2 {
		t.Fatalf("HMAC probe calls after recovery = %d, want 2", probeCalls.Load())
	}
}

func TestGitHubWebhookSecretCheckerProbeFailureIsCachedSanitizedAndRecovers(t *testing.T) {
	secret := "probe-secret-must-not-leak"
	resolver := &readinessSecretResolver{secret: []byte(secret)}
	checker := NewGitHubWebhookSecretChecker(resolver, "active-ref", time.Minute, time.Second)
	probeCalls := 0
	checker.probe = func([]byte) error {
		probeCalls++
		return errors.New("probe exposed " + secret)
	}
	if err := checker.Check(t.Context()); err == nil || err.Error() != errGitHubWebhookSecretUnavailable.Error() || strings.Contains(err.Error(), secret) {
		t.Fatalf("probe failure = %v", err)
	}
	if err := checker.Check(t.Context()); err == nil || probeCalls != 1 || resolver.calls.Load() != 1 {
		t.Fatalf("cached probe failure = %v, probes=%d resolves=%d", err, probeCalls, resolver.calls.Load())
	}
	checker.mu.Lock()
	checker.checked = time.Time{}
	checker.mu.Unlock()
	checker.probe = probeGitHubWebhookHMAC
	if err := checker.Check(t.Context()); err != nil {
		t.Fatalf("probe recovery: %v", err)
	}
}

func TestGitHubWebhookSecretCheckerRejectsEmptyAndTimesOut(t *testing.T) {
	resolver := &readinessSecretResolver{}
	checker := NewGitHubWebhookSecretChecker(resolver, "active-ref", time.Millisecond, 20*time.Millisecond)
	if err := checker.Check(t.Context()); err == nil {
		t.Fatal("empty active secret passed readiness")
	}
	resolver.mu.Lock()
	resolver.secret = []byte("active")
	resolver.block = true
	resolver.mu.Unlock()
	checker.mu.Lock()
	checker.checked = time.Time{}
	checker.mu.Unlock()
	started := time.Now()
	if err := checker.Check(t.Context()); err == nil {
		t.Fatal("blocked resolver passed readiness")
	}
	if elapsed := time.Since(started); elapsed < 15*time.Millisecond || elapsed > time.Second {
		t.Fatalf("probe timeout elapsed = %s", elapsed)
	}
}
