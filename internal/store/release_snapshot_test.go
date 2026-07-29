package store

import (
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
