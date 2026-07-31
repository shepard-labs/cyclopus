package kubernetes

import (
	"context"
	"time"

	"github.com/cyclopus-dev/cyclopus/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// certificateGVR is the cert-manager Certificate CRD. It is read through the
// dynamic client (like KEDA ScaledObjects) so Cyclopus takes no cert-manager Go
// module dependency.
var certificateGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}

// CertificateReader reads live cert-manager Certificate status from the cluster.
// It implements service.CertReader. Reads are bounded to the single namespace
// the service resolves from an already-authorized environment, so this adapter
// never widens beyond a caller's own Cyclopus-managed namespace.
type CertificateReader struct {
	dynamic dynamic.Interface
}

// NewCertificateReader builds a reader backed by the dynamic client. A nil
// client yields a reader that reports no certificates (the service treats a nil
// reader as unavailable; a nil client here is defensive).
func NewCertificateReader(client dynamic.Interface) *CertificateReader {
	return &CertificateReader{dynamic: client}
}

// ListCertificates returns the Certificate statuses in scope.Namespace. It is
// strictly namespace-scoped: only Certificates in the resolved managed
// namespace are listed, so no cross-tenant certificate is ever returned. When
// the cert-manager CRD is not installed (or the namespace has none), it returns
// an empty slice rather than an error, so the RPC degrades gracefully instead of
// surfacing a 500.
func (r *CertificateReader) ListCertificates(ctx context.Context, scope service.CertScope) ([]service.CertificateStatus, error) {
	if r.dynamic == nil || scope.Namespace == "" {
		return nil, nil
	}
	list, err := r.dynamic.Resource(certificateGVR).Namespace(scope.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if isDynamicKindUnavailable(err) {
			// cert-manager CRD absent or namespace missing: no certs, not a failure.
			return nil, nil
		}
		return nil, err
	}
	statuses := make([]service.CertificateStatus, 0, len(list.Items))
	for index := range list.Items {
		statuses = append(statuses, parseCertificate(&list.Items[index]))
	}
	return statuses, nil
}

// parseCertificate projects an unstructured cert-manager Certificate into the
// service-owned CertificateStatus. It reads spec.secretName, spec.dnsNames,
// spec.commonName, spec.issuerRef.name, status.notAfter, and the Ready condition
// under status.conditions. Missing fields degrade to zero values.
func parseCertificate(object *unstructured.Unstructured) service.CertificateStatus {
	status := service.CertificateStatus{}
	status.SecretName, _, _ = unstructured.NestedString(object.Object, "spec", "secretName")
	status.Issuer, _, _ = unstructured.NestedString(object.Object, "spec", "issuerRef", "name")
	if dnsNames, found, _ := unstructured.NestedStringSlice(object.Object, "spec", "dnsNames"); found {
		status.DNSNames = dnsNames
	}
	// hostname is the primary served name: commonName when set, else the first
	// dnsName. This is the customer-facing identity for the certificate.
	if commonName, found, _ := unstructured.NestedString(object.Object, "spec", "commonName"); found && commonName != "" {
		status.Hostname = commonName
	} else if len(status.DNSNames) > 0 {
		status.Hostname = status.DNSNames[0]
	}
	if notAfter, found, _ := unstructured.NestedString(object.Object, "status", "notAfter"); found && notAfter != "" {
		if parsed, err := time.Parse(time.RFC3339, notAfter); err == nil {
			utc := parsed.UTC()
			status.NotAfter = &utc
		}
	}
	status.Ready, status.Reason = readyCondition(object)
	return status
}

// readyCondition extracts the Ready condition from status.conditions. ready is
// true only when a condition of type "Ready" has status "True"; the condition's
// message (falling back to its reason) is returned as the human-readable reason.
func readyCondition(object *unstructured.Unstructured) (bool, string) {
	conditions, found, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if !found || err != nil {
		return false, ""
	}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		conditionType, _ := condition["type"].(string)
		if conditionType != "Ready" {
			continue
		}
		conditionStatus, _ := condition["status"].(string)
		reason := ""
		if message, ok := condition["message"].(string); ok && message != "" {
			reason = message
		} else if value, ok := condition["reason"].(string); ok {
			reason = value
		}
		return conditionStatus == "True", reason
	}
	return false, ""
}

// compile-time assertion that the adapter satisfies the service seam.
var _ service.CertReader = (*CertificateReader)(nil)
