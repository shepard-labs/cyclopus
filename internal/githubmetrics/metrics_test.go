package githubmetrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockingWriter struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
}

func (w *blockingWriter) Header() http.Header { return w.header }
func (*blockingWriter) WriteHeader(int)       {}
func (w *blockingWriter) Write(p []byte) (int, error) {
	select {
	case <-w.started:
	default:
		close(w.started)
	}
	<-w.release
	return len(p), nil
}

func TestMetricsRecordExactBoundedAndSensitiveFree(t *testing.T) {
	m := New()
	m.Ingress("push", "", "accepted")
	m.Delivery("push", "", "processed", 250*time.Millisecond, 2)
	m.Intent("push", "superseded", 500*time.Millisecond)
	m.Leases(1, 2, 3, 4)
	for i := range 10000 {
		value := fmt.Sprintf("secret-signature-sha-payload-id-%d", i)
		m.Ingress(value, value, value)
		m.Delivery(value, value, value, time.Second, i)
		m.Intent(value, value, time.Second)
	}
	recorder := httptest.NewRecorder()
	m.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	text := recorder.Body.String()
	for _, want := range []string{
		`cyclopus_github_webhook_ingress_total{event="push",action="",result="accepted"} 1`,
		`cyclopus_github_delivery_total{event="push",action="",result="processed"} 1`,
		`cyclopus_github_intent_total{event="push",result="superseded"} 1`,
		`cyclopus_github_leases{kind="intent",state="delayed"} 4`,
		`cyclopus_github_webhook_ingress_total{event="other",action="other",result="other"} 10000`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"secret", "signature", "sha", "payload", "id-9999"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("snapshot exposed %q", forbidden)
		}
	}
	if len(m.ingress) > 2 || len(m.delivery) > 2 || len(m.intent) > 2 {
		t.Fatalf("unbounded cardinality: %d/%d/%d", len(m.ingress), len(m.delivery), len(m.intent))
	}
}

func TestMetricsConcurrentRecording(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				m.Ingress("push", "", "accepted")
			}
		}()
	}
	wg.Wait()
	if got := m.ingress[key("push", "", "accepted")]; got != 10000 {
		t.Fatalf("count = %d", got)
	}
}

func TestMetricsBlockedWriterDoesNotBlockRecording(t *testing.T) {
	m := New()
	m.Ingress("push", "", "accepted")
	w := &blockingWriter{header: make(http.Header), started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { m.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil)); close(done) }()
	<-w.started
	recorded := make(chan struct{})
	go func() { m.Ingress("push", "", "accepted"); close(recorded) }()
	select {
	case <-recorded:
	case <-time.After(time.Second):
		t.Fatal("Record blocked behind metrics response writer")
	}
	close(w.release)
	<-done
}
