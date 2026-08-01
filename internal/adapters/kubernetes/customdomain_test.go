package kubernetes

import (
	"context"
	"testing"

	"github.com/shepard-labs/cyclopus/internal/service"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func customDomainRequest() service.CustomDomainRequest {
	return service.CustomDomainRequest{
		Namespace:     "tenant",
		EnvironmentID: "environment",
		DomainID:      "domain-1",
		Hostname:      "app.customer.test",
		ServiceName:   "app",
		ServicePort:   80,
		TLSSecretName: "app-customer-test-tls",
		ClusterIssuer: "letsencrypt",
	}
}

func TestProvisionCustomDomainCreatesIngress(t *testing.T) {
	client := fake.NewSimpleClientset()
	p := NewCustomDomainProvisioner(client)
	req := customDomainRequest()
	if err := p.ProvisionCustomDomain(context.Background(), req); err != nil {
		t.Fatalf("ProvisionCustomDomain error = %v", err)
	}
	ing, err := client.NetworkingV1().Ingresses("tenant").Get(context.Background(), "custom-domain-domain-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created ingress = %v", err)
	}
	// Ownership labels.
	if ing.Labels[ManagedByLabel] != ManagedByValue || ing.Labels[EnvironmentIDLabel] != "environment" {
		t.Fatalf("ownership labels = %v", ing.Labels)
	}
	// cert-manager cluster-issuer annotation so a cert is issued.
	if ing.Annotations[CertManagerClusterIssuerAnnotation] != "letsencrypt" {
		t.Fatalf("cluster-issuer annotation = %q, want letsencrypt", ing.Annotations[CertManagerClusterIssuerAnnotation])
	}
	// Rule host -> service.
	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != "app.customer.test" {
		t.Fatalf("rules = %+v", ing.Spec.Rules)
	}
	backend := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	if backend.Name != "app" || backend.Port.Number != 80 {
		t.Fatalf("backend = %+v, want app:80", backend)
	}
	// TLS block references the derived secret and served host.
	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != "app-customer-test-tls" || ing.Spec.TLS[0].Hosts[0] != "app.customer.test" {
		t.Fatalf("tls = %+v", ing.Spec.TLS)
	}
}

func TestProvisionCustomDomainIsIdempotent(t *testing.T) {
	client := fake.NewSimpleClientset()
	p := NewCustomDomainProvisioner(client)
	req := customDomainRequest()
	if err := p.ProvisionCustomDomain(context.Background(), req); err != nil {
		t.Fatalf("first provision = %v", err)
	}
	// Change the host on the request; a re-provision must UPDATE the existing
	// object in place, not create a duplicate.
	req.Hostname = "renamed.customer.test"
	if err := p.ProvisionCustomDomain(context.Background(), req); err != nil {
		t.Fatalf("second provision = %v", err)
	}
	list, err := client.NetworkingV1().Ingresses("tenant").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("ingress count = %d, want 1 (no duplicate on re-provision)", len(list.Items))
	}
	if list.Items[0].Spec.Rules[0].Host != "renamed.customer.test" {
		t.Fatalf("host after re-provision = %q, want updated", list.Items[0].Spec.Rules[0].Host)
	}
}

func TestProvisionCustomDomainRejectsForeignObject(t *testing.T) {
	foreign := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "custom-domain-domain-1",
			Namespace: "tenant",
			Labels:    map[string]string{ManagedByLabel: "someone-else"},
		},
	}
	client := fake.NewSimpleClientset(foreign)
	p := NewCustomDomainProvisioner(client)
	if err := p.ProvisionCustomDomain(context.Background(), customDomainRequest()); err != ErrResourceOwnership {
		t.Fatalf("foreign object error = %v, want ErrResourceOwnership", err)
	}
}

func TestProvisionCustomDomainEmptyNamespaceFailsClosed(t *testing.T) {
	p := NewCustomDomainProvisioner(fake.NewSimpleClientset())
	req := customDomainRequest()
	req.Namespace = ""
	if err := p.ProvisionCustomDomain(context.Background(), req); err == nil {
		t.Fatal("empty namespace must fail closed")
	}
}

func TestProvisionCustomDomainNoIssuerOmitsAnnotation(t *testing.T) {
	client := fake.NewSimpleClientset()
	p := NewCustomDomainProvisioner(client)
	req := customDomainRequest()
	req.ClusterIssuer = ""
	if err := p.ProvisionCustomDomain(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	ing, err := client.NetworkingV1().Ingresses("tenant").Get(context.Background(), "custom-domain-domain-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ing.Annotations[CertManagerClusterIssuerAnnotation]; ok {
		t.Fatalf("cluster-issuer annotation must be absent when issuer empty, got %v", ing.Annotations)
	}
}

func TestDeprovisionCustomDomainDeletesIngress(t *testing.T) {
	client := fake.NewSimpleClientset()
	p := NewCustomDomainProvisioner(client)
	req := customDomainRequest()
	if err := p.ProvisionCustomDomain(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := p.DeprovisionCustomDomain(context.Background(), req); err != nil {
		t.Fatalf("Deprovision error = %v", err)
	}
	if _, err := client.NetworkingV1().Ingresses("tenant").Get(context.Background(), "custom-domain-domain-1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("ingress still present after deprovision: %v", err)
	}
}

func TestDeprovisionCustomDomainNotFoundIsNoOp(t *testing.T) {
	p := NewCustomDomainProvisioner(fake.NewSimpleClientset())
	// Nothing provisioned: deprovision must succeed (idempotent teardown).
	if err := p.DeprovisionCustomDomain(context.Background(), customDomainRequest()); err != nil {
		t.Fatalf("not-found deprovision = %v, want nil", err)
	}
}

func TestDeprovisionCustomDomainEmptyNamespaceNoOp(t *testing.T) {
	p := NewCustomDomainProvisioner(fake.NewSimpleClientset())
	req := customDomainRequest()
	req.Namespace = ""
	if err := p.DeprovisionCustomDomain(context.Background(), req); err != nil {
		t.Fatalf("empty-namespace deprovision = %v, want nil", err)
	}
}

// TestEnvironmentIngressClusterIssuerAnnotation asserts the LATENT-GAP fix: the
// environment Ingress carries the cert-manager cluster-issuer annotation when
// configured, and omits it when the issuer is empty.
func TestEnvironmentIngressClusterIssuerAnnotation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		issuer   string
		wantAnno bool
	}{
		{name: "issuer set", issuer: "letsencrypt", wantAnno: true},
		{name: "issuer empty", issuer: "", wantAnno: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := deployEnvelope(t, "none")
			execution := deployExecution(t, envelope)
			objects, err := BuildDesiredObjects(execution, envelope, nil)
			if err != nil {
				t.Fatal(err)
			}
			client := fake.NewSimpleClientset()
			adapter := newForwardAdapter(t, client, newFakeForwardStore(execution))
			adapter.config.CertManagerClusterIssuer = tc.issuer
			if err := adapter.applyDesired(context.Background(), &execution, objects); err != nil {
				t.Fatalf("applyDesired = %v", err)
			}
			ing, err := client.NetworkingV1().Ingresses("tenant").Get(context.Background(), "app", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get env ingress = %v", err)
			}
			got, ok := ing.Annotations[CertManagerClusterIssuerAnnotation]
			if tc.wantAnno && (!ok || got != tc.issuer) {
				t.Fatalf("env ingress issuer annotation = %q/%v, want %q", got, ok, tc.issuer)
			}
			if !tc.wantAnno && ok {
				t.Fatalf("env ingress issuer annotation present %q, want absent", got)
			}
		})
	}
}

// TestEnvironmentIngressClusterIssuerAnnotationRemovedOnConfigEmpty asserts the
// UPDATE path converges: an Ingress previously stamped with the cluster-issuer
// annotation has it REMOVED on the next apply once the configured issuer is
// cleared. This guards the mergeManagedMetadata stripping of the cert-manager
// key so a config -> empty change does not leave a stale annotation behind.
func TestEnvironmentIngressClusterIssuerAnnotationRemovedOnConfigEmpty(t *testing.T) {
	envelope := deployEnvelope(t, "none")
	execution := deployExecution(t, envelope)
	objects, err := BuildDesiredObjects(execution, envelope, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleClientset()
	adapter := newForwardAdapter(t, client, newFakeForwardStore(execution))

	// First apply with an issuer configured: annotation present.
	adapter.config.CertManagerClusterIssuer = "letsencrypt"
	if err := adapter.applyDesired(context.Background(), &execution, objects); err != nil {
		t.Fatalf("first applyDesired = %v", err)
	}
	ing, err := client.NetworkingV1().Ingresses("tenant").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get env ingress = %v", err)
	}
	if ing.Annotations[CertManagerClusterIssuerAnnotation] != "letsencrypt" {
		t.Fatalf("after first apply issuer annotation = %q, want letsencrypt", ing.Annotations[CertManagerClusterIssuerAnnotation])
	}

	// Re-apply with the issuer cleared: annotation must be gone.
	objects, err = BuildDesiredObjects(execution, envelope, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter.config.CertManagerClusterIssuer = ""
	if err := adapter.applyDesired(context.Background(), &execution, objects); err != nil {
		t.Fatalf("second applyDesired = %v", err)
	}
	ing, err = client.NetworkingV1().Ingresses("tenant").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get env ingress after clear = %v", err)
	}
	if got, ok := ing.Annotations[CertManagerClusterIssuerAnnotation]; ok {
		t.Fatalf("issuer annotation still present %q after config cleared, want removed", got)
	}
}
