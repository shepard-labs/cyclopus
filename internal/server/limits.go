package server

import (
	"container/list"
	"context"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	cyclopusv1 "github.com/shepard-labs/cyclopus/gen/go/cyclopus/v1"
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
	keys := make([]string, 0, 2)
	if account != "" {
		keys = append(keys, "account:"+account)
	}
	if client != "" {
		keys = append(keys, "client:"+client)
	}
	if len(keys) == 0 {
		return true, 0
	}
	missing := 0
	protected := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		protected[key] = struct{}{}
		if _, ok := l.perKey[key]; !ok {
			missing++
		}
	}
	if retryAfter := l.reserveCapacity(now, missing, protected); retryAfter > 0 {
		return false, retryAfter
	}
	buckets := make([]rateBucket, 0, len(keys))
	var retryAfter time.Duration
	for _, key := range keys {
		bucket := l.loadBucket(key, now)
		buckets = append(buckets, bucket)
		if bucket.tokens < 1 {
			retry := time.Duration(math.Ceil((1-bucket.tokens)/l.rate*1000)) * time.Millisecond
			if retry > retryAfter {
				retryAfter = retry
			}
		}
	}
	if retryAfter > 0 {
		for _, bucket := range buckets {
			if _, ok := l.perKey[bucket.key]; ok {
				l.storeBucket(bucket)
			}
		}
		return false, retryAfter
	}
	for _, bucket := range buckets {
		bucket.tokens--
		l.storeBucket(bucket)
	}
	return true, 0
}

func (l *authRateLimiter) reserveCapacity(now time.Time, missing int, protected map[string]struct{}) time.Duration {
	needed := len(l.perKey) + missing - l.maxEntries
	if needed <= 0 {
		return 0
	}
	candidates := make([]*list.Element, 0, needed)
	waits := make([]time.Duration, 0, needed)
	available := 0
	for element := l.recency.Back(); element != nil; element = element.Prev() {
		bucket := element.Value.(rateBucket)
		if _, keep := protected[bucket.key]; keep {
			continue
		}
		available++
		tokens := l.tokensAt(bucket, now)
		if tokens >= 1 {
			candidates = append(candidates, element)
			continue
		}
		waits = append(waits, time.Duration(math.Ceil((1-tokens)/l.rate*1000))*time.Millisecond)
	}
	if len(candidates) >= needed {
		for _, element := range candidates[:needed] {
			bucket := element.Value.(rateBucket)
			delete(l.perKey, bucket.key)
			l.recency.Remove(element)
		}
		return 0
	}
	if available < needed {
		return time.Minute
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	return waits[needed-len(candidates)-1]
}

func (l *authRateLimiter) tokensAt(bucket rateBucket, now time.Time) float64 {
	if bucket.updated.IsZero() {
		return l.burst
	}
	return math.Min(l.burst, bucket.tokens+now.Sub(bucket.updated).Seconds()*l.rate)
}

func (l *authRateLimiter) loadBucket(key string, now time.Time) rateBucket {
	var bucket rateBucket
	if element, ok := l.perKey[key]; ok {
		bucket = element.Value.(rateBucket)
		l.recency.MoveToFront(element)
	} else {
		bucket.key = key
	}
	if bucket.updated.IsZero() {
		bucket.tokens = l.burst
	} else {
		bucket.tokens = l.tokensAt(bucket, now)
	}
	bucket.updated = now
	return bucket
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
