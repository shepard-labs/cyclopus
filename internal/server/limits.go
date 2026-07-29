package server

import (
	"container/list"
	"context"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	cyclopusv1 "github.com/cyclopus-dev/cyclopus/gen/go/cyclopus/v1"
	"google.golang.org/grpc/peer"
)

const authRateLimiterMaxEntries = 4096

type rateBucket struct {
	tokens  float64
	updated time.Time
	key     string
}

type authRateLimiter struct {
	mu         sync.Mutex
	rate       float64
	burst      float64
	maxEntries int
	perKey     map[string]*list.Element
	recency    *list.List
	now        func() time.Time
}

func newAuthRateLimiter(limit int) *authRateLimiter {
	return &authRateLimiter{rate: float64(limit) / time.Minute.Seconds(), burst: float64(limit),
		maxEntries: authRateLimiterMaxEntries, perKey: make(map[string]*list.Element), recency: list.New(), now: time.Now}
}

func NewAuthRateLimiter(limit int) *authRateLimiter {
	return newAuthRateLimiter(limit)
}

func (l *authRateLimiter) allow(client, account string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	account = strings.ToLower(strings.TrimSpace(account))
	client = strings.TrimSpace(client)
	if client == "unknown" {
		client = ""
	}
	key := "account:" + account
	if client != "" {
		key += "\x00client:" + client
	}
	var bucket rateBucket
	if element, ok := l.perKey[key]; ok {
		bucket = element.Value.(rateBucket)
		l.recency.MoveToFront(element)
	} else {
		if len(l.perKey) >= l.maxEntries {
			oldest := l.recency.Back()
			delete(l.perKey, oldest.Value.(rateBucket).key)
			l.recency.Remove(oldest)
		}
		bucket.key = key
	}
	if bucket.updated.IsZero() {
		bucket.tokens = l.burst
	} else {
		bucket.tokens = math.Min(l.burst, bucket.tokens+now.Sub(bucket.updated).Seconds()*l.rate)
	}
	bucket.updated = now
	if bucket.tokens < 1 {
		l.storeBucket(bucket)
		return false, time.Duration(math.Ceil((1-bucket.tokens)/l.rate*1000)) * time.Millisecond
	}
	bucket.tokens--
	l.storeBucket(bucket)
	return true, 0
}

func (l *authRateLimiter) storeBucket(bucket rateBucket) {
	if element, ok := l.perKey[bucket.key]; ok {
		element.Value = bucket
		return
	}
	l.perKey[bucket.key] = l.recency.PushFront(bucket)
}

func authAccount(req any) string {
	switch value := req.(type) {
	case *cyclopusv1.BootstrapRequest:
		return value.GetEmail()
	case *cyclopusv1.LoginRequest:
		return value.GetEmail()
	default:
		return ""
	}
}

type clientContextKey struct{}

func contextWithClient(ctx context.Context, client string) context.Context {
	return context.WithValue(ctx, clientContextKey{}, client)
}

func clientFromRESTContext(ctx context.Context) string {
	if client, ok := ctx.Value(clientContextKey{}).(string); ok {
		return client
	}
	return "unknown"
}

func clientFromGRPCPeer(ctx context.Context) string {
	if remote, ok := peer.FromContext(ctx); ok && remote.Addr != nil {
		return clientFromRemoteAddr(remote.Addr.String())
	}
	return "unknown"
}

func clientFromRemoteAddr(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
