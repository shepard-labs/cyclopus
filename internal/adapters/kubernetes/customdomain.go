package kubernetes

import (
	"context"
	"fmt"
	"strings"

	"github.com/shepard-labs/cyclopus/internal/service"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// customDomainIngressPrefix names the per-domain Ingress the provisioner
// creates. The domain ID is appended so the name is deterministic and unique per
// domain (independent of the hostname, which may be long or change casing).
const customDomainIngressPrefix = "custom-domain-"

// CustomDomainProvisioner provisions and tears down the live routing objects for
// a verified custom domain (§7.5): an Ingress in the production environment's
// namespace that routes the custom hostname to the environment's Service, with
// TLS and the cert-manager cluster-issuer annotation so cert-manager issues a
// certificate for the custom hostname. It uses only the typed networkingv1
// client — no cert-manager Go module.
type CustomDomainProvisioner struct {
	client kubernetes.Interface
}

// NewCustomDomainProvisioner builds a provisioner backed by the typed client.
func NewCustomDomainProvisioner(client kubernetes.Interface) *CustomDomainProvisioner {
	return &CustomDomainProvisioner{client: client}
}

// CustomDomainIngressName derives the deterministic Ingress name for a domain.
func CustomDomainIngressName(domainID string) string {
	return customDomainIngressPrefix + domainID
}

// ProvisionCustomDomain creates (or idempotently updates) the custom-domain
// Ingress. It fails closed when the namespace is empty. The apply is
// get-or-create: on update it verifies Cyclopus ownership before mutating so it
// never adopts a foreign object.
func (p *CustomDomainProvisioner) ProvisionCustomDomain(ctx context.Context, req service.CustomDomainRequest) error {
	if p.client == nil {
		return fmt.Errorf("custom-domain provisioner has no Kubernetes client")
	}
	if strings.TrimSpace(req.Namespace) == "" {
		return fmt.Errorf("custom-domain provisioning requires a production environment namespace")
	}
	if strings.TrimSpace(req.Hostname) == "" || strings.TrimSpace(req.ServiceName) == "" || strings.TrimSpace(req.TLSSecretName) == "" {
		return fmt.Errorf("custom-domain provisioning requires hostname, service, and TLS secret name")
	}
	desired := p.buildIngress(req)
	ingresses := p.client.NetworkingV1().Ingresses(req.Namespace)
	current, err := ingresses.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = ingresses.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	// Ownership check: only mutate an object that carries our managed labels for
	// this environment, so a name collision with a foreign object fails closed.
	if current.Labels[ManagedByLabel] != ManagedByValue || current.Labels[EnvironmentIDLabel] != req.EnvironmentID {
		return ErrResourceOwnership
	}
	candidate := current.DeepCopy()
	candidate.Labels = desired.Labels
	candidate.Annotations = desired.Annotations
	candidate.Spec = desired.Spec
	_, err = ingresses.Update(ctx, candidate, metav1.UpdateOptions{})
	return err
}

// DeprovisionCustomDomain deletes the custom-domain Ingress. A not-found delete
// is a success so deleting a never-verified/never-provisioned domain does not
// error. Deleting the Ingress is sufficient: cert-manager garbage-collects the
// Certificate and TLS Secret it created from the Ingress annotation.
func (p *CustomDomainProvisioner) DeprovisionCustomDomain(ctx context.Context, req service.CustomDomainRequest) error {
	if p.client == nil {
		return fmt.Errorf("custom-domain provisioner has no Kubernetes client")
	}
	if strings.TrimSpace(req.Namespace) == "" {
		// No namespace means nothing was ever provisioned (e.g. the app has no
		// production environment). Treat as a no-op so delete is idempotent.
		return nil
	}
	name := CustomDomainIngressName(req.DomainID)
	ingresses := p.client.NetworkingV1().Ingresses(req.Namespace)
	current, err := ingresses.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.Labels[ManagedByLabel] != ManagedByValue || current.Labels[EnvironmentIDLabel] != req.EnvironmentID {
		return ErrResourceOwnership
	}
	err = ingresses.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &current.UID}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (p *CustomDomainProvisioner) buildIngress(req service.CustomDomainRequest) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	meta := metav1.ObjectMeta{
		Name:      CustomDomainIngressName(req.DomainID),
		Namespace: req.Namespace,
		Labels:    managedLabels(nil, req.EnvironmentID),
	}
	setClusterIssuerAnnotation(&meta, req.ClusterIssuer)
	return &networkingv1.Ingress{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: meta,
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: req.Hostname,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
					Path: "/", PathType: &pathType,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: req.ServiceName, Port: networkingv1.ServiceBackendPort{Number: req.ServicePort}}},
				}}}},
			}},
			TLS: []networkingv1.IngressTLS{{Hosts: []string{req.Hostname}, SecretName: req.TLSSecretName}},
		},
	}
}

// compile-time assertion that the adapter satisfies the service seam.
var _ service.CustomDomainProvisioner = (*CustomDomainProvisioner)(nil)
