package githubmetrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Recorder interface {
	Ingress(event, action, result string)
	Delivery(event, action, result string, latency time.Duration, fanout int)
	Intent(event, result string, latency time.Duration)
	Leases(activeDeliveries, delayedDeliveries, activeIntents, delayedIntents int64)
}

type noop struct{}

func (noop) Ingress(string, string, string)                      {}
func (noop) Delivery(string, string, string, time.Duration, int) {}
func (noop) Intent(string, string, time.Duration)                {}
func (noop) Leases(int64, int64, int64, int64)                   {}

func Noop() Recorder { return noop{} }

var (
	events          = set("ping", "installation", "installation_repositories", "push")
	actions         = set("", "added", "created", "deleted", "new_permissions_accepted", "removed", "suspend", "unsuspend")
	ingressResults  = set("accepted", "replayed", "conflict", "auth_invalid", "overload", "rate_limited")
	deliveryResults = set("processed", "retry", "failed", "lease_lost")
	intentResults   = set("processed", "retry", "failed", "superseded", "lease_lost")
	latencyBuckets  = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	fanoutBuckets   = []float64{0, 1, 2, 5, 10, 25, 100, 1000}
)

type histogram struct {
	count   uint64
	sum     float64
	buckets []uint64
}

type Metrics struct {
	mu              sync.RWMutex
	ingress         map[string]uint64
	delivery        map[string]uint64
	intent          map[string]uint64
	deliveryLatency map[string]*histogram
	intentLatency   map[string]*histogram
	fanout          map[string]*histogram
	leases          [4]int64
}

func New() *Metrics {
	return &Metrics{ingress: map[string]uint64{}, delivery: map[string]uint64{}, intent: map[string]uint64{},
		deliveryLatency: map[string]*histogram{}, intentLatency: map[string]*histogram{}, fanout: map[string]*histogram{}}
}

func (m *Metrics) Ingress(event, action, result string) {
	event, action, result = normalize(event, events), normalize(action, actions), normalize(result, ingressResults)
	m.mu.Lock()
	m.ingress[key(event, action, result)]++
	m.mu.Unlock()
}

func (m *Metrics) Delivery(event, action, result string, latency time.Duration, fanout int) {
	event, action, result = normalize(event, events), normalize(action, actions), normalize(result, deliveryResults)
	k := key(event, action, result)
	m.mu.Lock()
	m.delivery[k]++
	observe(m.deliveryLatency, k, latency.Seconds(), latencyBuckets)
	observe(m.fanout, key(event, action), float64(max(fanout, 0)), fanoutBuckets)
	m.mu.Unlock()
}

func (m *Metrics) Intent(event, result string, latency time.Duration) {
	event, result = normalize(event, events), normalize(result, intentResults)
	k := key(event, result)
	m.mu.Lock()
	m.intent[k]++
	observe(m.intentLatency, k, latency.Seconds(), latencyBuckets)
	m.mu.Unlock()
}

func (m *Metrics) Leases(activeDeliveries, delayedDeliveries, activeIntents, delayedIntents int64) {
	m.mu.Lock()
	m.leases = [4]int64{activeDeliveries, delayedDeliveries, activeIntents, delayedIntents}
	m.mu.Unlock()
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	m.mu.RLock()
	ingress, delivery, intent := cloneMap(m.ingress), cloneMap(m.delivery), cloneMap(m.intent)
	deliveryLatency, intentLatency, fanout := cloneHistograms(m.deliveryLatency), cloneHistograms(m.intentLatency), cloneHistograms(m.fanout)
	leases := m.leases
	m.mu.RUnlock()
	writeCounters(w, "cyclopus_github_webhook_ingress_total", []string{"event", "action", "result"}, ingress)
	writeCounters(w, "cyclopus_github_delivery_total", []string{"event", "action", "result"}, delivery)
	writeCounters(w, "cyclopus_github_intent_total", []string{"event", "result"}, intent)
	writeHistogram(w, "cyclopus_github_delivery_processing_seconds", []string{"event", "action", "result"}, deliveryLatency, latencyBuckets)
	writeHistogram(w, "cyclopus_github_intent_processing_seconds", []string{"event", "result"}, intentLatency, latencyBuckets)
	writeHistogram(w, "cyclopus_github_delivery_fanout", []string{"event", "action"}, fanout, fanoutBuckets)
	for i, labels := range []string{`kind="delivery",state="active"`, `kind="delivery",state="delayed"`, `kind="intent",state="active"`, `kind="intent",state="delayed"`} {
		fmt.Fprintf(w, "cyclopus_github_leases{%s} %d\n", labels, leases[i])
	}
}

func cloneMap(values map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneHistograms(values map[string]*histogram) map[string]*histogram {
	out := make(map[string]*histogram, len(values))
	for key, value := range values {
		out[key] = &histogram{count: value.count, sum: value.sum, buckets: append([]uint64(nil), value.buckets...)}
	}
	return out
}

func observe(values map[string]*histogram, k string, value float64, bounds []float64) {
	h := values[k]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(bounds))}
		values[k] = h
	}
	h.count++
	h.sum += value
	for i, bound := range bounds {
		if value <= bound {
			h.buckets[i]++
		}
	}
}

func writeCounters(w http.ResponseWriter, name string, labels []string, values map[string]uint64) {
	keys := sortedKeys(values)
	for _, k := range keys {
		fmt.Fprintf(w, "%s{%s} %d\n", name, labelText(labels, k), values[k])
	}
}

func writeHistogram(w http.ResponseWriter, name string, labels []string, values map[string]*histogram, bounds []float64) {
	for _, k := range sortedKeys(values) {
		h := values[k]
		base := labelText(labels, k)
		for i, bound := range bounds {
			fmt.Fprintf(w, "%s_bucket{%s,le=\"%g\"} %d\n", name, base, bound, h.buckets[i])
		}
		fmt.Fprintf(w, "%s_bucket{%s,le=\"+Inf\"} %d\n%s_sum{%s} %g\n%s_count{%s} %d\n", name, base, h.count, name, base, h.sum, name, base, h.count)
	}
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func labelText(names []string, encoded string) string {
	values := strings.Split(encoded, "\x00")
	parts := make([]string, len(names))
	for i := range names {
		parts[i] = names[i] + `="` + values[i] + `"`
	}
	return strings.Join(parts, ",")
}

func key(values ...string) string { return strings.Join(values, "\x00") }
func set(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}
func normalize(value string, allowed map[string]struct{}) string {
	if _, ok := allowed[value]; ok {
		return value
	}
	return "other"
}
