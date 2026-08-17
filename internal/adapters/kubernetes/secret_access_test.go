package kubernetes

import (
	"context"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestWebhookSecretAuthorizerAllowsOnlyConfiguredOwnedSecret(t *testing.T) {
	immutable := true
	client := fake.NewSimpleClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "cyclopus", Name: "cyclopus-webhook-primary", UID: "uid-1",
		Labels: map[string]string{"cyclopus.app/credential-purpose": "webhook-signing", "app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "release"},
	}, Immutable: &immutable, Data: map[string][]byte{"secret": []byte("value")}})
	reference := "k8s-secret://cyclopus/cyclopus-webhook-primary#secret"
	authorizer, err := NewWebhookSecretAuthorizer(client, "cyclopus", "release", []string{reference})
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := authorizer.AuthorizeReference(context.Background(), reference)
	if err != nil {
		t.Fatalf("AuthorizeReference() error = %v", err)
	}
	defer authorized.Cleanup()
	resolved := authorized.Resolve()
	resolved[0] = 'X'
	live, err := client.CoreV1().Secrets("cyclopus").Get(context.Background(), "cyclopus-webhook-primary", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(live.Data["secret"]) != "value" {
		t.Fatal("AuthorizeReference returned Secret data without a defensive copy")
	}
	if err := authorizer.Validate(context.Background(), reference); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if _, err := authorizer.AuthorizeReference(context.Background(), "k8s-secret://cyclopus/cyclopus-webhook-other#secret"); err == nil {
		t.Fatal("AuthorizeReference() accepted an unconfigured Secret")
	}
}

func TestWebhookSecretAuthorizationCleanupLifecycle(t *testing.T) {
	immutable := true
	client := fake.NewSimpleClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "cyclopus", Name: "cyclopus-webhook-primary", UID: "uid-1",
		Labels: map[string]string{"cyclopus.app/credential-purpose": "webhook-signing", "app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "release"},
	}, Immutable: &immutable, Data: map[string][]byte{"secret": []byte("super-secret-value")}})
	reference := "k8s-secret://cyclopus/cyclopus-webhook-primary#secret"
	authorizer, err := NewWebhookSecretAuthorizer(client, "cyclopus", "release", []string{reference})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot []byte
	authorizer.onSnapshot = func(value []byte) { snapshot = value }

	authorized, err := authorizer.AuthorizeReference(context.Background(), reference)
	if err != nil {
		t.Fatal(err)
	}
	resolved := authorized.Resolve()
	if string(resolved) != "super-secret-value" || !allZeroBytes(snapshot) {
		t.Fatalf("Resolve cleanup = value:%q internal cleared:%t", resolved, allZeroBytes(snapshot))
	}
	clear(resolved)
	authorized.Cleanup()
	if !allZeroBytes(resolved) {
		t.Fatal("caller-owned resolved bytes were not cleared")
	}

	authorized, err = authorizer.AuthorizeReference(context.Background(), reference)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			authorized.Cleanup()
		}()
	}
	wg.Wait()
	if !allZeroBytes(snapshot) || len(authorized.Resolve()) != 0 {
		t.Fatal("concurrent cleanup did not discard the snapshot exactly once")
	}
}

func TestWebhookSecretValidateCleansAuthorizedSnapshot(t *testing.T) {
	immutable := true
	client := fake.NewSimpleClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "cyclopus", Name: "cyclopus-webhook-primary", UID: "uid-1",
		Labels: map[string]string{"cyclopus.app/credential-purpose": "webhook-signing", "app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "release"},
	}, Immutable: &immutable, Data: map[string][]byte{"secret": []byte("validation-secret")}})
	reference := "k8s-secret://cyclopus/cyclopus-webhook-primary#secret"
	authorizer, err := NewWebhookSecretAuthorizer(client, "cyclopus", "release", []string{reference})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot []byte
	authorizer.onSnapshot = func(value []byte) { snapshot = value }
	if err := authorizer.Validate(context.Background(), reference); err != nil {
		t.Fatal(err)
	}
	if !allZeroBytes(snapshot) {
		t.Fatal("Validate left the authorized snapshot uncleared")
	}
}

func allZeroBytes(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}

func TestWebhookSecretAuthorizerRevalidatesLiveSecretAndPinsIdentity(t *testing.T) {
	immutable := true
	reference := "k8s-secret://cyclopus/cyclopus-webhook-primary#secret"
	valid := func() *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cyclopus", Name: "cyclopus-webhook-primary", UID: "uid-1", ResourceVersion: "1",
			Labels: map[string]string{"cyclopus.app/credential-purpose": "webhook-signing", "app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "release"}},
			Immutable: &immutable, Data: map[string][]byte{"secret": []byte("super-secret-value")}}
	}
	for _, mutate := range []struct {
		name    string
		fn      func(*corev1.Secret)
		revoked bool
	}{
		{"mutable", func(secret *corev1.Secret) { value := false; secret.Immutable = &value }, true},
		{"purpose removed", func(secret *corev1.Secret) { delete(secret.Labels, "cyclopus.app/credential-purpose") }, true},
		{"manager removed", func(secret *corev1.Secret) { delete(secret.Labels, "app.kubernetes.io/managed-by") }, true},
		{"ownership removed", func(secret *corev1.Secret) { delete(secret.Labels, "app.kubernetes.io/instance") }, true},
		{"uid removed", func(secret *corev1.Secret) { secret.UID = "" }, true},
		{"key removed", func(secret *corev1.Secret) { delete(secret.Data, "secret") }, true},
		{"key empty", func(secret *corev1.Secret) { secret.Data["secret"] = nil }, true},
		{"key changed", func(secret *corev1.Secret) { secret.Data["secret"] = []byte("different-secret-value") }, true},
		{"uid changed", func(secret *corev1.Secret) { secret.UID = "uid-2" }, true},
		{"resource version changed", func(secret *corev1.Secret) { secret.ResourceVersion = "2" }, false},
		{"irrelevant metadata changed", func(secret *corev1.Secret) {
			secret.ResourceVersion = "2"
			secret.Annotations = map[string]string{"example.com/note": "updated"}
		}, false},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(valid())
			authorizer, err := NewWebhookSecretAuthorizer(client, "cyclopus", "release", []string{reference})
			if err != nil {
				t.Fatal(err)
			}
			authorized, err := authorizer.AuthorizeReference(context.Background(), reference)
			if err != nil {
				t.Fatal(err)
			}
			value := authorized.Resolve()
			authorized.Cleanup()
			clear(value)
			value[0] = 'X'
			live, err := client.CoreV1().Secrets("cyclopus").Get(context.Background(), "cyclopus-webhook-primary", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(string(live.Data["secret"]), "X") {
				t.Fatal("AuthorizeReference returned Secret data without a defensive copy")
			}
			mutate.fn(live)
			if _, err := client.CoreV1().Secrets("cyclopus").Update(context.Background(), live, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			authorized, err = authorizer.AuthorizeReference(context.Background(), reference)
			if mutate.revoked {
				if err == nil {
					t.Fatal("AuthorizeReference accepted revoked live Secret")
				}
				if strings.Contains(err.Error(), "super-secret-value") || strings.Contains(err.Error(), "different-secret-value") {
					t.Fatalf("AuthorizeReference error exposed Secret value: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("AuthorizeReference rejected metadata-only update: %v", err)
			}
			value = authorized.Resolve()
			authorized.Cleanup()
			if string(value) != "super-secret-value" {
				t.Fatal("AuthorizeReference returned unexpected value after metadata-only update")
			}
			clear(value)
		})
	}
}

func TestWebhookSecretAuthorizerRejectsDeletionAndIsConcurrentSafe(t *testing.T) {
	immutable := true
	reference := "k8s-secret://cyclopus/cyclopus-webhook-primary#secret"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cyclopus", Name: "cyclopus-webhook-primary", UID: "uid-1", ResourceVersion: "1",
		Labels: map[string]string{"cyclopus.app/credential-purpose": "webhook-signing", "app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "release"}},
		Immutable: &immutable, Data: map[string][]byte{"secret": []byte("super-secret-value")}}
	client := fake.NewSimpleClientset(secret)
	authorizer, err := NewWebhookSecretAuthorizer(client, "cyclopus", "release", []string{reference})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			authorized, err := authorizer.AuthorizeReference(context.Background(), reference)
			if err != nil {
				t.Errorf("AuthorizeReference() error = %v", err)
				return
			}
			value := authorized.Resolve()
			authorized.Cleanup()
			if string(value) != "super-secret-value" {
				t.Error("AuthorizeReference() returned unexpected value")
			}
			clear(value)
		}()
	}
	wg.Wait()
	if err := client.CoreV1().Secrets("cyclopus").Delete(context.Background(), "cyclopus-webhook-primary", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := authorizer.AuthorizeReference(context.Background(), reference); err == nil {
		t.Fatal("AuthorizeReference accepted a deleted Secret")
	} else if strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("AuthorizeReference error exposed Secret value: %v", err)
	}
	recreated := secret.DeepCopy()
	recreated.UID = "uid-2"
	recreated.ResourceVersion = "2"
	if _, err := client.CoreV1().Secrets("cyclopus").Create(context.Background(), recreated, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := authorizer.AuthorizeReference(context.Background(), reference); err == nil {
		t.Fatal("AuthorizeReference accepted a recreated Secret")
	}
}

func TestWebhookSecretAuthorizerPerformsLiveGetEveryAttempt(t *testing.T) {
	immutable := true
	reference := "k8s-secret://cyclopus/cyclopus-webhook-primary#secret"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cyclopus", Name: "cyclopus-webhook-primary", UID: "uid-1", ResourceVersion: "1",
		Labels: map[string]string{"cyclopus.app/credential-purpose": "webhook-signing", "app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "release"}},
		Immutable: &immutable, Data: map[string][]byte{"secret": []byte("super-secret-value")}}
	client := fake.NewSimpleClientset(secret)
	gets := 0
	client.PrependReactor("get", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		gets++
		return false, nil, nil
	})
	authorizer, err := NewWebhookSecretAuthorizer(client, "cyclopus", "release", []string{reference})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := authorizer.AuthorizeReference(context.Background(), reference); err != nil {
			t.Fatal(err)
		}
	}
	if gets != 2 {
		t.Fatalf("live Secret GETs = %d, want 2", gets)
	}
}

func TestWebhookSecretAuthorizerAllowlistRemovalRevokes(t *testing.T) {
	immutable := true
	reference := "k8s-secret://cyclopus/cyclopus-webhook-primary#secret"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cyclopus", Name: "cyclopus-webhook-primary", UID: "uid-1", ResourceVersion: "1",
		Labels: map[string]string{"cyclopus.app/credential-purpose": "webhook-signing", "app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "release"}},
		Immutable: &immutable, Data: map[string][]byte{"secret": []byte("super-secret-value")}}
	authorizer, err := NewWebhookSecretAuthorizer(fake.NewSimpleClientset(secret), "cyclopus", "release", []string{reference})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authorizer.AuthorizeReference(context.Background(), reference); err != nil {
		t.Fatal(err)
	}
	delete(authorizer.allowed, reference)
	if _, err := authorizer.AuthorizeReference(context.Background(), reference); err == nil {
		t.Fatal("AuthorizeReference accepted a removed allowlist reference")
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
