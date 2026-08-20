//go:build k3s_integration

package kubernetes

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shepard-labs/cyclopus/internal/store"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func TestForwardDeployK3sPostgresIntegration(t *testing.T) {
	client := k3sIntegrationClient(t)
	dataStore, pool := newK3sPostgresStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	namespace := fmt.Sprintf("cyclopus-forward-%d", time.Now().UnixNano())
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = client.CoreV1().Namespaces().Delete(cleanupCtx, namespace, metav1.DeleteOptions{})
	})

	const (
		orgID         = "30000000-0000-0000-0000-000000000001"
		projectID     = "30000000-0000-0000-0000-000000000002"
		appID         = "30000000-0000-0000-0000-000000000003"
		environmentID = "30000000-0000-0000-0000-000000000004"
		releaseID     = "30000000-0000-0000-0000-000000000005"
		repositoryID  = "30000000-0000-0000-0000-000000000006"
		revisionID    = "30000000-0000-0000-0000-000000000007"
	)
	seedK3sForwardScope(t, pool, orgID, projectID, appID, environmentID, repositoryID, namespace)
	immutable := true
	sourceData := map[string][]byte{"GREETING": []byte("cyclopus-forward")}
	if _, err := client.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-source", Namespace: namespace}, Immutable: &immutable, Data: sourceData}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	image, imageRepository, digest := deployTestImage(t)
	if _, err := pool.Exec(ctx, `UPDATE apps SET image_repository=$1 WHERE id=$2`, imageRepository, appID); err != nil {
		t.Fatal(err)
	}
	envelope := k3sForwardEnvelope(namespace, environmentID, releaseID, revisionID, image)
	snapshot, configHash, err := store.EncodeReleaseSnapshot(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO env_var_revisions (id,org_id,environment_id,revision) VALUES ($1,$2,$3,1)`, revisionID, orgID, environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO env_var_revision_entries (org_id,environment_id,env_var_revision_id,key,secret_ref) VALUES ($1,$2,$3,'GREETING',$4)`, orgID, environmentID, revisionID, "k8s-secret://"+namespace+"/runtime-source#GREETING"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO releases (id,org_id,environment_id,image_ref,git_commit_sha,kind,env_var_revision_id,config_snapshot,config_hash,status) VALUES ($1,$2,$3,$4,$5,'env_change',$6,$7,$8,'pending')`, releaseID, orgID, environmentID, image, strings.Repeat("1", 40), revisionID, snapshot, configHash); err != nil {
		t.Fatal(err)
	}
	seedK3sCompletedForwardRoute(t, pool, orgID, repositoryID, environmentID,
		"30000000-0000-0000-0000-000000000008", strings.Repeat("1", 40), "forward-success-route", digest)
	if _, err := dataStore.AcquireDeployLease(ctx, orgID, environmentID, releaseID); err != nil {
		t.Fatal(err)
	}
	execution, err := dataStore.ClaimPendingDeploy(ctx, orgID, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewAdapter(Config{PollInterval: 500 * time.Millisecond, ConflictRetries: 5}, client, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.ExecuteDeploy(ctx, execution); err != nil {
		t.Fatalf("execute forward deploy: %v", err)
	}
	assertK3sForwardLive(t, ctx, client, pool, namespace, environmentID, releaseID, revisionID, configHash, sourceData)
}

func TestForwardDeployStartupResumeK3sPostgresIntegration(t *testing.T) {
	client := k3sIntegrationClient(t)
	dataStore, pool := newK3sPostgresStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	namespace := fmt.Sprintf("cyclopus-forward-resume-%d", time.Now().UnixNano())
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = client.CoreV1().Namespaces().Delete(cleanupCtx, namespace, metav1.DeleteOptions{})
	})
	const orgID = "31000000-0000-0000-0000-000000000001"
	const projectID = "31000000-0000-0000-0000-000000000002"
	const appID = "31000000-0000-0000-0000-000000000003"
	const environmentID = "31000000-0000-0000-0000-000000000004"
	const releaseID = "31000000-0000-0000-0000-000000000005"
	const repositoryID = "31000000-0000-0000-0000-000000000006"
	seedK3sForwardScope(t, pool, orgID, projectID, appID, environmentID, repositoryID, namespace)
	image, imageRepository, digest := deployTestImage(t)
	if _, err := pool.Exec(ctx, `UPDATE apps SET image_repository=$1 WHERE id=$2`, imageRepository, appID); err != nil {
		t.Fatal(err)
	}
	envelope := k3sForwardEnvelope(namespace, environmentID, releaseID, "", image)
	envelope.EnvVarRevision = nil
	envelope.DesiredState.Secret = nil
	envelope.DesiredState.Deployment.Container.EnvFromSecrets = nil
	delete(envelope.DesiredState.Deployment.PodAnnotations, EnvRevisionAnnotation)
	delete(envelope.DesiredState.Deployment.PodAnnotations, "cyclopus.dev/env-hash")
	snapshot, configHash, err := store.EncodeReleaseSnapshot(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO releases (id,org_id,environment_id,image_ref,git_commit_sha,kind,config_snapshot,config_hash,status) VALUES ($1,$2,$3,$4,$5,'deploy',$6,$7,'deploying')`, releaseID, orgID, environmentID, image, strings.Repeat("2", 40), snapshot, configHash); err != nil {
		t.Fatal(err)
	}
	seedK3sCompletedForwardRoute(t, pool, orgID, repositoryID, environmentID,
		"31000000-0000-0000-0000-000000000007", strings.Repeat("2", 40), "forward-resume-route", digest)
	lease, err := dataStore.AcquireDeployLease(ctx, orgID, environmentID, releaseID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := dataStore.ClaimUnexpiredDeploying(ctx, orgID, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewAdapter(Config{PollInterval: 500 * time.Millisecond, ConflictRetries: 5}, client, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := store.DecodeReleaseSnapshot(first.FailedRelease.ConfigSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := BuildDesiredObjects(first, decoded, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.applyDesired(ctx, &first, objects); err != nil {
		t.Fatal(err)
	}
	resumed, err := dataStore.ClaimUnexpiredDeploying(ctx, orgID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Fence.After(lease.Fence) {
		t.Fatalf("resume fence=%v initial=%v", resumed.Fence, lease.Fence)
	}
	if _, err := adapter.ExecuteDeploy(ctx, resumed); err != nil {
		t.Fatal(err)
	}
	assertK3sForwardLive(t, ctx, client, pool, namespace, environmentID, releaseID, "", configHash, nil)
}

func k3sIntegrationClient(t *testing.T) kubernetes.Interface {
	t.Helper()
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Fatal("KUBECONFIG is required")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

var deployTestImagePattern = regexp.MustCompile(`^(ghcr\.io/[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)*)@(sha256:[0-9a-f]{64})$`)

func deployTestImage(t *testing.T) (string, string, string) {
	t.Helper()
	image := strings.TrimSpace(os.Getenv("CYCLOPUS_DEPLOY_TEST_IMAGE"))
	match := deployTestImagePattern.FindStringSubmatch(image)
	if match == nil {
		t.Fatal("CYCLOPUS_DEPLOY_TEST_IMAGE must match ghcr.io/<repository>@sha256:<64 lowercase hex>")
	}
	return image, strings.TrimPrefix(match[1], "ghcr.io/"), match[2]
}

func k3sForwardEnvelope(namespace, environmentID, releaseID, revisionID, image string) store.ReleaseSnapshotEnvelope {
	labels := map[string]string{ManagedByLabel: ManagedByValue, EnvironmentIDLabel: environmentID, "app.kubernetes.io/name": "app"}
	selector := map[string]string{EnvironmentIDLabel: environmentID, "app.kubernetes.io/name": "app"}
	identity := store.ObjectIdentity{Name: "app", Namespace: namespace, Labels: labels}
	envelope := store.ReleaseSnapshotEnvelope{Version: 2, DesiredState: store.DesiredStateSnapshot{Version: 1, Deployment: store.DeploymentIntent{Version: 1, Identity: identity, Selector: selector, Replicas: 1, Strategy: store.RollingUpdateIntent{Type: "RollingUpdate", MaxUnavailable: 0, MaxSurge: 1}, PodLabels: labels, PodAnnotations: map[string]string{ReleaseAnnotation: store.ReleaseIDAnnotationTemplate, ConfigHashAnnotation: store.ConfigHashAnnotationTemplate}, RuntimeClassName: "runc", ProgressDeadlineSeconds: 120, Container: store.ContainerIntent{Name: "app", Image: image, Port: 80, Resources: store.ResourceIntent{CPURequestMillicores: 10, CPULimitMillicores: 100, MemoryRequestMiB: 16, MemoryLimitMiB: 64}, ReadinessProbe: store.ReadinessIntent{Path: "/", Port: 80}}}, Service: store.ServiceIntent{Version: 1, Identity: identity, Selector: selector, Port: 80, TargetPort: 80}, Ingress: store.IngressIntent{Version: 1, Identity: identity, Hostname: "forward.example.test", Path: "/", ServiceName: "app", ServicePort: 80, TLS: store.TLSIntent{Enabled: true, SecretName: "forward-tls"}}, Scaling: store.ScalingIntent{Version: 1, Kind: "hpa", Name: "app", HPA: &store.HPAIntent{MinReplicas: 1, MaxReplicas: 2, TargetCPUPercent: 70}}}}
	if revisionID != "" {
		variables := []store.ReleaseEnvironment{{Key: "GREETING", SecretRef: "k8s-secret://" + namespace + "/runtime-source#GREETING", IsSecret: true}}
		hash := store.ComputeEnvVarRevisionHash(variables)
		name := store.EnvDestinationSecretName("app", revisionID)
		envelope.EnvVarRevision = &store.EnvVarRevision{Version: 1, ID: revisionID, Revision: 1, Hash: hash, Variables: variables}
		envelope.DesiredState.Secret = &store.SecretIntent{Version: 1, Identity: store.ObjectIdentity{Name: name, Namespace: namespace, Labels: labels}, Immutable: true, Entries: []store.SecretEntryIntent{{Key: "GREETING", SecretRef: variables[0].SecretRef}}}
		envelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{name}
		envelope.DesiredState.Deployment.PodAnnotations[EnvRevisionAnnotation] = revisionID
		envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = hash
	}
	return envelope
}

func assertK3sForwardLive(t *testing.T, ctx context.Context, client kubernetes.Interface, pool *pgxpool.Pool, namespace, environmentID, releaseID, revisionID, configHash string, secretData map[string][]byte) {
	t.Helper()
	deployment := waitForK3sForwardReady(t, ctx, client, namespace, "app")
	if deployment.Annotations[ReleaseAnnotation] != releaseID || deployment.Annotations[ConfigHashAnnotation] != configHash || deployment.Annotations[DeployFenceAnnotation] == "" {
		t.Fatalf("deployment annotations=%v", deployment.Annotations)
	}
	if _, err := client.CoreV1().Services(namespace).Get(ctx, "app", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.NetworkingV1().Ingresses(namespace).Get(ctx, "app", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(ctx, "app", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if revisionID != "" {
		secret, err := client.CoreV1().Secrets(namespace).Get(ctx, store.EnvDestinationSecretName("app", revisionID), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if secret.Immutable == nil || !*secret.Immutable || secret.Annotations[SecretContentHashAnnotation] != ComputeSecretDataHash(secretData) || secret.Annotations[EnvRevisionAnnotation] != revisionID || secret.Labels[EnvironmentIDLabel] != environmentID {
			t.Fatalf("destination Secret metadata=%v/%v", secret.Labels, secret.Annotations)
		}
	}
	var status, aliasRelease string
	var inProgress *string
	if err := pool.QueryRow(ctx, `SELECT release.status,alias.release_id::text,environment.in_progress_release_id::text FROM releases release JOIN environments environment ON environment.id=release.environment_id JOIN aliases alias ON alias.environment_id=environment.id WHERE release.id=$1`, releaseID).Scan(&status, &aliasRelease, &inProgress); err != nil {
		t.Fatal(err)
	}
	if status != "live" || aliasRelease != releaseID || inProgress != nil {
		t.Fatalf("persisted forward=%s/%s/%v", status, aliasRelease, inProgress)
	}
}

func waitForK3sForwardReady(t *testing.T, ctx context.Context, client kubernetes.Interface, namespace, name string) *appsv1.Deployment {
	t.Helper()
	var ready *appsv1.Deployment
	err := wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if deployment.Generation > 0 && deployment.Status.ObservedGeneration >= deployment.Generation && deployment.Status.UpdatedReplicas == 1 && deployment.Status.ReadyReplicas == 1 && deployment.Status.AvailableReplicas == 1 && deployment.Status.UnavailableReplicas == 0 {
			ready = deployment
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ready
}

func seedK3sForwardScope(t *testing.T, pool *pgxpool.Pool, orgID, projectID, appID, environmentID, repositoryID, namespace string) {
	t.Helper()
	ctx := context.Background()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO orgs (id,name,slug) VALUES ($1,'Forward Org',$2)`, []any{orgID, "forward-" + environmentID[len(environmentID)-4:]}},
		{`INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(3001,$1,3002,'cyclopus','Organization','selected','active',1,1)`, []any{orgID}},
		{`INSERT INTO github_repositories(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(3003,$1,3001,$2,'cyclopus','forward','cyclopus/forward','public','main','active',1,1)`, []any{orgID, "forward-node-" + environmentID}},
		{`INSERT INTO git_connections (id,org_id,github_repository_id) VALUES ($1,$2,3003)`, []any{repositoryID, orgID}},
		{`INSERT INTO projects (id,org_id,name,slug,git_connection_id) VALUES ($1,$2,'Forward','forward',$3)`, []any{projectID, orgID, repositoryID}},
		{`INSERT INTO apps (id,org_id,project_id,name,slug,hostname_label,image_repository,github_workflow_path,production_branch,deploy_timeout_seconds,runtime_class) VALUES ($1,$2,$3,'Forward','forward',$4,'cyclopus/forward','.github/workflows/deploy.yml','main',180,'runc')`, []any{appID, orgID, projectID, "forward-" + environmentID[len(environmentID)-4:]}},
		{`INSERT INTO environments (id,org_id,app_id,branch,kind,namespace,planned_hostname) VALUES ($1,$2,$3,'main','production',$4,$5)`, []any{environmentID, orgID, appID, namespace, "forward-" + environmentID[len(environmentID)-4:] + ".example.test"}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func seedK3sCompletedForwardRoute(t *testing.T, pool *pgxpool.Pool, orgID, repositoryID,
	environmentID, id, commitSHA, key, digest string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO orchestration_jobs
		(id,org_id,app_id,environment_id,github_repository_id,github_workflow_path,workflow_revision,dispatch_ref,
		dispatch_ref_observed_sha,desired_checkout_sha,desired_generation,idempotency_key,status,dispatch_requested_at,
		dispatch_ambiguity_state,github_dispatch_identity,github_run_id,github_run_head_sha,github_run_status,
		github_run_conclusion,github_status_observed_at,oci_resolution_state,image_tag_ref,resolved_image_digest,
		immutable_image_ref,oci_media_type,oci_resolved_at,deployment_state,release_id)
		SELECT $1,$2,app.id,$4,connection.github_repository_id,app.github_workflow_path,$5,'refs/heads/main',$5,$5,
			environment.desired_generation,$6,'deploying',now(),'matched','cyclopus:'||$1,3004,$5,'completed','success',now(),
			'resolved','ghcr.io/'||app.image_repository||':'||$5,$7,'ghcr.io/'||app.image_repository||'@'||$7,
			'application/vnd.oci.image.manifest.v1+json',now(),'applying',release.id
		FROM environments environment JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
		JOIN projects project ON project.org_id=app.org_id AND project.id=app.project_id
		JOIN git_connections connection ON connection.org_id=project.org_id AND connection.id=project.git_connection_id
		JOIN releases release ON release.org_id=environment.org_id AND release.environment_id=environment.id
		WHERE environment.org_id=$2 AND environment.id=$4`, id, orgID, repositoryID, environmentID, commitSHA, key, digest); err != nil {
		t.Fatal(err)
	}
}
