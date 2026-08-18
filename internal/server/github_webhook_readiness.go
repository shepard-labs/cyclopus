package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"
)

var errGitHubWebhookSecretUnavailable = errors.New("GitHub webhook active secret is unavailable")

// GitHubWebhookSecretChecker bounds resolver traffic while ensuring a cached
// success is never used after its probe interval has elapsed.
type GitHubWebhookSecretChecker struct {
	resolver GitHubWebhookSecretResolver
	ref      string
	interval time.Duration
	timeout  time.Duration
	now      func() time.Time
	probe    func([]byte) error

	mu      sync.Mutex
	checked time.Time
	err     error
}

func NewGitHubWebhookSecretChecker(resolver GitHubWebhookSecretResolver, activeRef string, interval, timeout time.Duration) *GitHubWebhookSecretChecker {
	return &GitHubWebhookSecretChecker{resolver: resolver, ref: activeRef, interval: interval, timeout: timeout, now: time.Now, probe: probeGitHubWebhookHMAC}
}

func probeGitHubWebhookHMAC(secret []byte) error {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("cyclopus:github-webhook-readiness:v1"))
	sum := mac.Sum(nil)
	defer clear(sum)
	if len(sum) != sha256.Size {
		return fmt.Errorf("unexpected HMAC-SHA-256 length %d", len(sum))
	}
	return nil
}

func (c *GitHubWebhookSecretChecker) Check(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if !c.checked.IsZero() && now.Sub(c.checked) < c.interval {
		return c.err
	}
	probeCtx, cancel := context.WithTimeout(ctx, c.timeout)
	secret, err := c.resolver.Resolve(probeCtx, c.ref)
	cancel()
	valid := err == nil && len(secret) > 0
	if valid {
		err = c.probe(secret)
		valid = err == nil
	}
	clear(secret)
	c.checked = now
	if !valid {
		c.err = errGitHubWebhookSecretUnavailable
		return c.err
	}
	c.err = nil
	return nil
}
