package kubernetes

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestWebhookSecretAuthorizerAllowsOnlyConfiguredOwnedSecret(t *testing.T) {
	immutable := true
	client := fake.NewSimpleClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "cyclopus", Name: "cyclopus-webhook-primary",
		Labels: map[string]string{"cyclopus.app/credential-purpose": "webhook-signing", "app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "release"},
	}, Immutable: &immutable, Data: map[string][]byte{"secret": []byte("value")}})
	reference := "k8s-secret://cyclopus/cyclopus-webhook-primary#secret"
	authorizer, err := NewWebhookSecretAuthorizer(client, "cyclopus", "release", []string{reference})
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizer.AuthorizeReference(context.Background(), reference); err != nil {
		t.Fatalf("AuthorizeReference() error = %v", err)
	}
	if err := authorizer.Validate(context.Background(), reference); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if err := authorizer.AuthorizeReference(context.Background(), "k8s-secret://cyclopus/cyclopus-webhook-other#secret"); err == nil {
		t.Fatal("AuthorizeReference() accepted an unconfigured Secret")
	}
}

func TestWebhookSecretAuthorizerRejectsMalformedCrossNamespaceAndInvalidMetadata(t *testing.T) {
	for _, reference := range []string{"bad", "k8s-secret://tenant/cyclopus-webhook-primary#secret", "k8s-secret://cyclopus/arbitrary#secret"} {
		if _, err := NewWebhookSecretAuthorizer(fake.NewSimpleClientset(), "cyclopus", "release", []string{reference}); err == nil {
			t.Fatalf("NewWebhookSecretAuthorizer() accepted %q", reference)
		}
	}
	client := fake.NewSimpleClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cyclopus", Name: "cyclopus-webhook-primary"}, Data: map[string][]byte{"secret": []byte("value")}})
	reference := "k8s-secret://cyclopus/cyclopus-webhook-primary#secret"
	authorizer, err := NewWebhookSecretAuthorizer(client, "cyclopus", "release", []string{reference})
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizer.Validate(context.Background(), reference); err == nil {
		t.Fatal("Validate() accepted missing immutable ownership metadata")
	}
}
