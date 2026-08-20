package ocimetrics

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMetricsBoundLabelsAndSnapshot(t *testing.T) {
	m := New()
	m.ImageResolution("complete", 250*time.Millisecond)
	m.CredentialSaga("validated", 500*time.Millisecond)
	m.NamespaceReconciliation("reconciled", time.Second)
	m.Work("image_resolution", 2, 7)
	for i := range 10000 {
		value := fmt.Sprintf("org/repo/image/secret-%d", i)
		m.ImageResolution(value, -time.Second)
		m.CredentialSaga(value, time.Second)
		m.NamespaceReconciliation(value, time.Second)
		m.Work(value, -1, -1)
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	text := w.Body.String()
	for _, want := range []string{
		`cyclopus_oci_image_resolution_total{result="complete"} 1`,
		`cyclopus_oci_credential_saga_total{result="validated"} 1`,
		`cyclopus_oci_namespace_reconciliation_total{result="reconciled"} 1`,
		`cyclopus_oci_image_resolution_seconds_count{result="complete"} 1`,
		`cyclopus_oci_worker_leases{kind="image_resolution"} 2`,
		`cyclopus_oci_worker_backlog{kind="image_resolution"} 7`,
		`cyclopus_oci_worker_backlog{kind="other"} 0`,
		`cyclopus_oci_image_resolution_total{result="other"} 10000`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"org/", "repo/", "image/", "secret-9999"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("snapshot exposed %q", forbidden)
		}
	}
	if len(m.image) != 2 || len(m.credential) != 2 || len(m.namespace) != 2 || len(m.work) != 2 {
		t.Fatalf("unbounded cardinality: %d/%d/%d/%d", len(m.image), len(m.credential), len(m.namespace), len(m.work))
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
				m.ImageResolution("complete", time.Millisecond)
			}
		}()
	}
	wg.Wait()
	if got := m.image["complete"]; got != 10000 {
		t.Fatalf("count = %d", got)
	}
}

func TestNoopImplementsRecorder(t *testing.T) {
	var recorder Recorder = Noop()
	recorder.ImageResolution("complete", time.Second)
	recorder.CredentialSaga("validated", time.Second)
	recorder.NamespaceReconciliation("reconciled", time.Second)
	recorder.Work("image_resolution", 1, 1)
}
