package server

import (
	"context"
	"errors"
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

	mu      sync.Mutex
	checked time.Time
	err     error
}

func NewGitHubWebhookSecretChecker(resolver GitHubWebhookSecretResolver, activeRef string, interval, timeout time.Duration) *GitHubWebhookSecretChecker {
	return &GitHubWebhookSecretChecker{resolver: resolver, ref: activeRef, interval: interval, timeout: timeout, now: time.Now}
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
	clear(secret)
	c.checked = now
	if !valid {
		c.err = errGitHubWebhookSecretUnavailable
		return c.err
	}
	c.err = nil
	return nil
}
