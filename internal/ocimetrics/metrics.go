package ocimetrics

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Recorder records OCI worker outcomes without accepting resource identifiers.
type Recorder interface {
	ImageResolution(result string, latency time.Duration)
	CredentialSaga(result string, latency time.Duration)
	NamespaceReconciliation(result string, latency time.Duration)
	Work(kind string, leases, backlog int64)
}

type noop struct{}

func (noop) ImageResolution(string, time.Duration)         {}
func (noop) CredentialSaga(string, time.Duration)          {}
func (noop) NamespaceReconciliation(string, time.Duration) {}
func (noop) Work(string, int64, int64)                     {}

func Noop() Recorder { return noop{} }

var (
	imageResults      = set("complete", "retry", "failed", "superseded", "lease_lost")
	credentialResults = set("validated", "retry", "failed", "lease_lost")
	namespaceResults  = set("reconciled", "retry", "failed", "superseded", "lease_lost")
	workKinds         = set("image_resolution", "credential", "namespace")
	latencyBuckets    = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
)

type histogram struct {
	count   uint64
	sum     float64
	buckets []uint64
}

type gauges struct{ leases, backlog int64 }

type Metrics struct {
	mu                sync.RWMutex
	image             map[string]uint64
	credential        map[string]uint64
	namespace         map[string]uint64
	imageLatency      map[string]*histogram
	credentialLatency map[string]*histogram
	namespaceLatency  map[string]*histogram
	work              map[string]gauges
}

func New() *Metrics {
	return &Metrics{
		image: map[string]uint64{}, credential: map[string]uint64{}, namespace: map[string]uint64{},
		imageLatency: map[string]*histogram{}, credentialLatency: map[string]*histogram{}, namespaceLatency: map[string]*histogram{},
		work: map[string]gauges{},
	}
}

func (m *Metrics) ImageResolution(result string, latency time.Duration) {
	m.record(m.image, m.imageLatency, normalize(result, imageResults), latency)
}

func (m *Metrics) CredentialSaga(result string, latency time.Duration) {
	m.record(m.credential, m.credentialLatency, normalize(result, credentialResults), latency)
}

func (m *Metrics) NamespaceReconciliation(result string, latency time.Duration) {
	m.record(m.namespace, m.namespaceLatency, normalize(result, namespaceResults), latency)
}

func (m *Metrics) record(counters map[string]uint64, histograms map[string]*histogram, result string, latency time.Duration) {
	m.mu.Lock()
	counters[result]++
	observe(histograms, result, max(latency.Seconds(), 0), latencyBuckets)
	m.mu.Unlock()
}

func (m *Metrics) Work(kind string, leases, backlog int64) {
	kind = normalize(kind, workKinds)
	m.mu.Lock()
	m.work[kind] = gauges{leases: max(leases, 0), backlog: max(backlog, 0)}
	m.mu.Unlock()
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	m.mu.RLock()
	image, credential, namespace := cloneMap(m.image), cloneMap(m.credential), cloneMap(m.namespace)
	imageLatency, credentialLatency, namespaceLatency := cloneHistograms(m.imageLatency), cloneHistograms(m.credentialLatency), cloneHistograms(m.namespaceLatency)
	work := make(map[string]gauges, len(m.work))
	for kind, value := range m.work {
		work[kind] = value
	}
	m.mu.RUnlock()
	writeCounters(w, "cyclopus_oci_image_resolution_total", image)
	writeCounters(w, "cyclopus_oci_credential_saga_total", credential)
	writeCounters(w, "cyclopus_oci_namespace_reconciliation_total", namespace)
	writeHistogram(w, "cyclopus_oci_image_resolution_seconds", imageLatency)
	writeHistogram(w, "cyclopus_oci_credential_saga_seconds", credentialLatency)
	writeHistogram(w, "cyclopus_oci_namespace_reconciliation_seconds", namespaceLatency)
	for _, kind := range sortedKeys(work) {
		fmt.Fprintf(w, "cyclopus_oci_worker_leases{kind=\"%s\"} %d\n", kind, work[kind].leases)
		fmt.Fprintf(w, "cyclopus_oci_worker_backlog{kind=\"%s\"} %d\n", kind, work[kind].backlog)
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

func observe(values map[string]*histogram, key string, value float64, bounds []float64) {
	h := values[key]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(bounds))}
		values[key] = h
	}
	h.count++
	h.sum += value
	for i, bound := range bounds {
		if value <= bound {
			h.buckets[i]++
		}
	}
}

func writeCounters(w http.ResponseWriter, name string, values map[string]uint64) {
	for _, result := range sortedKeys(values) {
		fmt.Fprintf(w, "%s{result=\"%s\"} %d\n", name, result, values[result])
	}
}

func writeHistogram(w http.ResponseWriter, name string, values map[string]*histogram) {
	for _, result := range sortedKeys(values) {
		h := values[result]
		for i, bound := range latencyBuckets {
			fmt.Fprintf(w, "%s_bucket{result=\"%s\",le=\"%g\"} %d\n", name, result, bound, h.buckets[i])
		}
		fmt.Fprintf(w, "%s_bucket{result=\"%s\",le=\"+Inf\"} %d\n%s_sum{result=\"%s\"} %g\n%s_count{result=\"%s\"} %d\n", name, result, h.count, name, result, h.sum, name, result, h.count)
	}
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

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
