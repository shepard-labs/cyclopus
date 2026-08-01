package kubernetes

import (
	"context"
	"testing"

	"github.com/shepard-labs/cyclopus/internal/service"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func certificateObject(namespace, name string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetAPIVersion("cert-manager.io/v1")
	object.SetKind("Certificate")
	object.SetNamespace(namespace)
	object.SetName(name)
	object.Object["spec"] = map[string]any{
		"secretName": "app-tls",
		"commonName": "app.example.test",
		"dnsNames":   []any{"app.example.test", "www.app.example.test"},
		"issuerRef":  map[string]any{"name": "letsencrypt-prod", "kind": "ClusterIssuer"},
	}
	object.Object["status"] = map[string]any{
		"notAfter": "2035-01-02T15:04:05Z",
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "True", "reason": "Ready", "message": "Certificate is up to date and has not expired"},
		},
	}
	return object
}

func newCertDynamicClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	listKinds := map[schema.GroupVersionResource]string{certificateGVR: "CertificateList"}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
}

func TestCertificateReaderParsesReadyCertificate(t *testing.T) {
	client := newCertDynamicClient(certificateObject("tel-app-production", "app"))
	reader := NewCertificateReader(client)
	statuses, err := reader.ListCertificates(context.Background(), service.CertScope{Namespace: "tel-app-production", EnvironmentID: "env"})
	if err != nil {
		t.Fatalf("ListCertificates error = %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %d, want 1", len(statuses))
	}
	status := statuses[0]
	if status.SecretName != "app-tls" {
		t.Fatalf("secretName = %q", status.SecretName)
	}
	if status.Hostname != "app.example.test" {
		t.Fatalf("hostname = %q, want commonName", status.Hostname)
	}
	if !status.Ready {
		t.Fatalf("ready = false, want true for Ready=True condition")
	}
	if status.Reason != "Certificate is up to date and has not expired" {
		t.Fatalf("reason = %q, want the condition message", status.Reason)
	}
	if status.Issuer != "letsencrypt-prod" {
		t.Fatalf("issuer = %q", status.Issuer)
	}
	if len(status.DNSNames) != 2 || status.DNSNames[0] != "app.example.test" {
		t.Fatalf("dnsNames = %v", status.DNSNames)
	}
	if status.NotAfter == nil || status.NotAfter.Year() != 2035 {
		t.Fatalf("notAfter = %v, want 2035 expiry", status.NotAfter)
	}
}

func TestCertificateReaderNotReadyCondition(t *testing.T) {
	object := certificateObject("tel-app-production", "app")
	object.Object["status"] = map[string]any{
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "Issuing", "message": "Issuing certificate"},
		},
	}
	reader := NewCertificateReader(newCertDynamicClient(object))
	statuses, err := reader.ListCertificates(context.Background(), service.CertScope{Namespace: "tel-app-production"})
	if err != nil {
		t.Fatalf("ListCertificates error = %v", err)
	}
	if len(statuses) != 1 || statuses[0].Ready {
		t.Fatalf("statuses = %+v, want a single not-ready certificate", statuses)
	}
	if statuses[0].NotAfter != nil {
		t.Fatalf("notAfter = %v, want nil when status has no notAfter", statuses[0].NotAfter)
	}
}

func TestCertificateReaderIsNamespaceScoped(t *testing.T) {
	// Two certs in different namespaces; a read scoped to one namespace must not
	// return the other tenant's certificate.
	client := newCertDynamicClient(
		certificateObject("tel-app-production", "app"),
		certificateObject("other-app-production", "other"),
	)
	reader := NewCertificateReader(client)
	statuses, err := reader.ListCertificates(context.Background(), service.CertScope{Namespace: "tel-app-production"})
	if err != nil {
		t.Fatalf("ListCertificates error = %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %d, want only the in-namespace certificate", len(statuses))
	}
}

func TestCertificateReaderMissingCRDReturnsEmpty(t *testing.T) {
	// When cert-manager's Certificate CRD is not installed, the API server
	// returns a NoMatch/NotFound-style error for the list. isDynamicKindUnavailable
	// classifies it, so the reader degrades to empty rather than surfacing a 500.
	client := newCertDynamicClient()
	client.PrependReactor("list", "certificates", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, &meta.NoResourceMatchError{PartialResource: certificateGVR}
	})
	reader := NewCertificateReader(client)
	statuses, err := reader.ListCertificates(context.Background(), service.CertScope{Namespace: "tel-app-production"})
	if err != nil {
		t.Fatalf("ListCertificates on absent CRD error = %v, want nil", err)
	}
	if len(statuses) != 0 {
		t.Fatalf("statuses = %+v, want empty on absent CRD", statuses)
	}
}

func TestCertificateReaderNilClientReturnsEmpty(t *testing.T) {
	reader := NewCertificateReader(nil)
	statuses, err := reader.ListCertificates(context.Background(), service.CertScope{Namespace: "tel-app-production"})
	if err != nil || statuses != nil {
		t.Fatalf("nil client read = %v, %v", statuses, err)
	}
}
