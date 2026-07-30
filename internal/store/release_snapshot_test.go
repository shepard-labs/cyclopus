package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestReleaseSnapshotEnvelopeRoundTripAndCanonicalHash(t *testing.T) {
	envelope := testSnapshotEnvelope()
	envelope.EnvVarRevision = &EnvVarRevision{Version: EnvVarRevisionVersion, ID: "revision", Revision: 2, Variables: []ReleaseEnvironment{
		{Key: "Z_TOKEN", SecretRef: "k8s-secret://tenant/runtime#Z_TOKEN", IsSecret: true},
		{Key: "DATABASE_URL", SecretRef: "k8s-secret://tenant/runtime#DATABASE_URL", IsSecret: true},
	}}
	envelope.EnvVarRevision.Hash = ComputeEnvVarRevisionHash(envelope.EnvVarRevision.Variables)
	destinationName := EnvDestinationSecretName("app", envelope.EnvVarRevision.ID)
	envelope.DesiredState.Secret = &SecretIntent{Version: 1, Identity: ObjectIdentity{Name: destinationName, Namespace: "tenant", Labels: envelope.DesiredState.Deployment.Identity.Labels}, Immutable: true, Entries: []SecretEntryIntent{
		{Key: "Z_TOKEN", SecretRef: "k8s-secret://tenant/runtime#Z_TOKEN"},
		{Key: "DATABASE_URL", SecretRef: "k8s-secret://tenant/runtime#DATABASE_URL"},
	}}
	envelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{destinationName}
	envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-revision-id"] = "revision"
	envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = envelope.EnvVarRevision.Hash
	encoded, hash, err := EncodeReleaseSnapshot(envelope)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeReleaseSnapshot(encoded)
	if err != nil || decoded.EnvVarRevision == nil || decoded.EnvVarRevision.Variables[0].Key != "DATABASE_URL" || len(hash) != 64 {
		t.Fatalf("decoded=%+v hash=%q err=%v", decoded, hash, err)
	}
	reencoded, rehash, err := EncodeReleaseSnapshot(decoded)
	if err != nil || string(reencoded) != string(encoded) || rehash != hash {
		t.Fatalf("canonical re-encode=%s/%s hash=%s/%s err=%v", reencoded, encoded, rehash, hash, err)
	}
}

func TestEnvSnapshotDestinationSecretIntentIsStrictAndDeterministic(t *testing.T) {
	envelope := testSnapshotEnvelope()
	variables := []ReleaseEnvironment{{Key: "TOKEN", SecretRef: "k8s-secret://tenant/source#TOKEN", IsSecret: true}}
	hash := ComputeEnvVarRevisionHash(variables)
	name := EnvDestinationSecretName("app", "revision")
	envelope.EnvVarRevision = &EnvVarRevision{Version: 1, ID: "revision", Revision: 1, Hash: hash, Variables: variables}
	envelope.DesiredState.Secret = &SecretIntent{Version: 1, Identity: ObjectIdentity{Name: name, Namespace: "tenant", Labels: envelope.DesiredState.Deployment.Identity.Labels}, Immutable: true, Entries: []SecretEntryIntent{{Key: "TOKEN", SecretRef: variables[0].SecretRef}}}
	envelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{name}
	envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-revision-id"] = "revision"
	envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = hash
	encoded, firstHash, err := EncodeReleaseSnapshot(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if name != EnvDestinationSecretName("app", "revision") {
		t.Fatalf("destination name = %q", name)
	}
	decoded, err := DecodeReleaseSnapshot(encoded)
	if err != nil || decoded.DesiredState.Secret == nil || decoded.DesiredState.Secret.Identity.Name != name {
		t.Fatalf("decoded = %+v, %v", decoded, err)
	}
	encodedAgain, secondHash, err := EncodeReleaseSnapshot(decoded)
	if err != nil || string(encodedAgain) != string(encoded) || secondHash != firstHash {
		t.Fatalf("canonical snapshot changed: %v", err)
	}
	decoded.DesiredState.Secret.Identity.Name = "mutable"
	if _, _, err := EncodeReleaseSnapshot(decoded); err == nil {
		t.Fatal("non-deterministic destination Secret name accepted")
	}
}

func TestReleaseSnapshotRejectsScalerIdentityDifferentFromDeployment(t *testing.T) {
	for _, kind := range []string{"hpa", "keda"} {
		t.Run(kind, func(t *testing.T) {
			envelope := testSnapshotEnvelope()
			envelope.DesiredState.Scaling.Name = "another-scaler"
			if kind == "keda" {
				envelope.DesiredState.Scaling.Kind = "keda"
				envelope.DesiredState.Scaling.HPA = nil
				envelope.DesiredState.Scaling.KEDA = &KEDAIntent{Mode: "http", MinReplicas: 1, MaxReplicas: 3, TargetCPUPercent: 70}
			}
			if _, _, err := EncodeReleaseSnapshot(envelope); err == nil {
				t.Fatal("EncodeReleaseSnapshot() accepted a scaler identity different from the Deployment")
			}
		})
	}
}

func TestReleaseSnapshotDecodesExistingNonEnvSnapshot(t *testing.T) {
	encoded, _, err := EncodeReleaseSnapshot(testSnapshotEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeReleaseSnapshot(encoded)
	if err != nil || decoded.EnvVarRevision != nil || decoded.DesiredState.Secret != nil {
		t.Fatalf("decoded = %+v, %v", decoded, err)
	}
}

func TestEnvSnapshotLegacyV2FixtureUpgradesInMemoryWithoutChangingStoredHash(t *testing.T) {
	envelope := testSnapshotEnvelope()
	variables := []ReleaseEnvironment{{Key: "DATABASE_URL", SecretRef: "k8s-secret://tenant/source#DATABASE_URL", IsSecret: true}}
	envelope.EnvVarRevision = &EnvVarRevision{Version: 1, ID: "018f4f5a-legacy", Revision: 7, Variables: variables}
	envelope.EnvVarRevision.Hash = ComputeEnvVarRevisionHash(variables)
	envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-revision-id"] = envelope.EnvVarRevision.ID
	envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = envelope.EnvVarRevision.Hash
	legacy, err := json.Marshal(canonicalReleaseSnapshot(envelope))
	if err != nil {
		t.Fatal(err)
	}
	originalHash := sha256.Sum256(legacy)
	decoded, err := DecodeReleaseSnapshot(legacy)
	if err != nil {
		t.Fatal(err)
	}
	wantName := EnvDestinationSecretName("app", envelope.EnvVarRevision.ID)
	if decoded.DesiredState.Secret == nil || decoded.DesiredState.Secret.Identity.Name != wantName || len(decoded.DesiredState.Deployment.Container.EnvFromSecrets) != 1 || decoded.DesiredState.Deployment.Container.EnvFromSecrets[0] != wantName {
		t.Fatalf("legacy upgrade = %+v", decoded.DesiredState)
	}
	if hex.EncodeToString(originalHash[:]) == "" || string(legacy) == "" {
		t.Fatal("legacy fixture hash was not preserved")
	}
}

func TestReleaseSnapshotEnvelopeRejectsLegacyUnknownAndInvalidDesiredState(t *testing.T) {
	for _, encoded := range []string{
		`{"version":1,"deployment":{}}`,
		`{"version":2,"desired_state":{},"future":true}`,
		`{"version":2,"desired_state":{}} trailing`,
	} {
		if _, err := DecodeReleaseSnapshot([]byte(encoded)); err == nil {
			t.Fatalf("DecodeReleaseSnapshot(%q) succeeded", encoded)
		}
	}
	invalid := testSnapshotEnvelope()
	invalid.DesiredState.Deployment.Strategy.MaxUnavailable = 1
	if _, _, err := EncodeReleaseSnapshot(invalid); err == nil {
		t.Fatal("snapshot with maxUnavailable=1 succeeded")
	}
	invalid = testSnapshotEnvelope()
	invalid.DesiredState.Scaling = ScalingIntent{Version: 1, Kind: "none"}
	invalid.DesiredState.Deployment.Replicas = 2
	if _, _, err := EncodeReleaseSnapshot(invalid); err == nil {
		t.Fatal("no-scaler snapshot with two replicas succeeded")
	}
}

func testSnapshotEnvelope() ReleaseSnapshotEnvelope {
	labels := map[string]string{"app.kubernetes.io/managed-by": "cyclopus", "app.kubernetes.io/name": "app", "cyclopus.dev/environment-id": "environment"}
	selector := map[string]string{"app.kubernetes.io/name": "app", "cyclopus.dev/environment-id": "environment"}
	identity := ObjectIdentity{Name: "app", Namespace: "tenant", Labels: labels}
	return ReleaseSnapshotEnvelope{Version: ReleaseSnapshotVersion, DesiredState: DesiredStateSnapshot{
		Version: DesiredStateSnapshotVersion,
		Deployment: DeploymentIntent{Version: 1, Identity: identity, Selector: selector, Replicas: 1,
			Strategy: RollingUpdateIntent{Type: "RollingUpdate", MaxUnavailable: 0, MaxSurge: 1}, PodLabels: labels,
			PodAnnotations:   map[string]string{"cyclopus.dev/release-id": ReleaseIDAnnotationTemplate, "cyclopus.dev/config-hash": ConfigHashAnnotationTemplate},
			RuntimeClassName: "gvisor", ProgressDeadlineSeconds: 600,
			Container: ContainerIntent{Name: "app", Image: "ghcr.io/acme/app@sha256:" + strings.Repeat("a", 64), Port: 8080,
				Resources: ResourceIntent{CPURequestMillicores: 100, CPULimitMillicores: 1000, MemoryRequestMiB: 128, MemoryLimitMiB: 512}, ReadinessProbe: ReadinessIntent{Path: "/healthz", Port: 8080}}},
		Service: ServiceIntent{Version: 1, Identity: identity, Selector: selector, Port: 80, TargetPort: 8080},
		Ingress: IngressIntent{Version: 1, Identity: identity, Hostname: "app.example.test", Path: "/", ServiceName: "app", ServicePort: 80, TLS: TLSIntent{Enabled: true, SecretName: "app-tls"}},
		Scaling: ScalingIntent{Version: 1, Kind: "hpa", Name: "app", HPA: &HPAIntent{MinReplicas: 1, MaxReplicas: 3, TargetCPUPercent: 70}},
	}}
}
