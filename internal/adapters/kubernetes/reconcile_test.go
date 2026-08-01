package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/shepard-labs/cyclopus/internal/store"
)

type fakeReconcileStore struct {
	RecoveryStore // embedded (nil) to satisfy the RecoveryStore-typed adapter field
	envByOrg      []store.Environment
	envByApp      []store.Environment
	env           store.Environment
	app           store.App
	release       store.Release
	getEnvErr     error
	started       bool
	startPlan     json.RawMessage
	evidence      []store.ReconciliationEvidence
	completed     bool
	completeParam store.CompleteReconciliationRunParams
	listByOrgHit  bool
	listByAppHit  bool
	verifiedDoms  []store.Domain
	appendErr     func(store.ReconciliationEvidence) error
}

func (f *fakeReconcileStore) ListEnvironmentsByOrg(context.Context, string) ([]store.Environment, error) {
	f.listByOrgHit = true
	return f.envByOrg, nil
}
func (f *fakeReconcileStore) ListEnvironments(context.Context, string, string, bool) ([]store.Environment, error) {
	f.listByAppHit = true
	return f.envByApp, nil
}
func (f *fakeReconcileStore) GetEnvironment(context.Context, string, string) (store.Environment, error) {
	return f.env, f.getEnvErr
}
func (f *fakeReconcileStore) GetApp(context.Context, string, string) (store.App, error) {
	return f.app, nil
}
func (f *fakeReconcileStore) GetRelease(context.Context, string, string) (store.Release, error) {
	return f.release, nil
}
func (f *fakeReconcileStore) ListVerifiedDomainsByApp(context.Context, string, string) ([]store.Domain, error) {
	return f.verifiedDoms, nil
}
func (f *fakeReconcileStore) StartReconciliationRun(_ context.Context, _, _, _ string, plan json.RawMessage, _ *store.AuditEvent) error {
	f.started = true
	f.startPlan = plan
	return nil
}
func (f *fakeReconcileStore) AppendReconciliationEvidence(_ context.Context, _, _, _ string, e store.ReconciliationEvidence) error {
	if f.appendErr != nil {
		if err := f.appendErr(e); err != nil {
			return err
		}
	}
	f.evidence = append(f.evidence, e)
	return nil
}
func (f *fakeReconcileStore) CompleteReconciliationRun(_ context.Context, p store.CompleteReconciliationRunParams) error {
	f.completed = true
	f.completeParam = p
	return nil
}

func reconcileTestRelease(t *testing.T) store.Release {
	t.Helper()
	envelope := deployEnvelope(t, "hpa")
	encoded, hash, err := store.EncodeReleaseSnapshot(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return store.Release{ImmutableReleaseSnapshot: store.ImmutableReleaseSnapshot{
		ID: "release", OrgID: "org", EnvironmentID: "environment", ConfigSnapshot: encoded, ConfigHash: hash,
	}}
}

func reconcileEnv() store.Environment {
	releaseID := "release"
	return store.Environment{ID: "environment", OrgID: "org", AppID: "app", Namespace: "tenant", CurrentReleaseID: &releaseID}
}

func runReconcile(t *testing.T, rs *fakeReconcileStore, client *fake.Clientset, run *store.ReconciliationRun) error {
	t.Helper()
	adapter := &RecoveryAdapter{
		config: Config{PollInterval: time.Nanosecond, ConflictRetries: 3, APITimeout: time.Second},
		client: client,
		store:  rs,
	}
	return adapter.ExecuteReconciliation(context.Background(), run, "worker-one")
}

func planFrom(t *testing.T, rs *fakeReconcileStore) reconcilePlan {
	t.Helper()
	var plan reconcilePlan
	if err := json.Unmarshal(rs.startPlan, &plan); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	return plan
}

func TestReconcileEnvironmentScopePlanReturnsNoOpWhenHashMatches(t *testing.T) {
	release := reconcileTestRelease(t)
	env := reconcileEnv()
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "tenant",
		Annotations: map[string]string{ConfigHashAnnotation: release.ConfigHash}}}
	client := fake.NewSimpleClientset(deployment)
	rs := &fakeReconcileStore{env: env, release: release, app: store.App{RuntimeClass: "gvisor"}}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: true}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	plan := planFrom(t, rs)
	var deployOp *objectAction
	for i := range plan.Operations {
		if plan.Operations[i].ObjectKind == "Deployment" {
			deployOp = &plan.Operations[i]
		}
	}
	if deployOp == nil || deployOp.Action != "no_op" {
		t.Fatalf("deployment action = %+v, want no_op", deployOp)
	}
	if !rs.completed || rs.completeParam.Status != "completed" {
		t.Fatalf("run not completed: %+v", rs.completeParam)
	}
}

func TestReconcileEnvironmentScopeDetectsPatch(t *testing.T) {
	release := reconcileTestRelease(t)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "tenant",
		Annotations: map[string]string{ConfigHashAnnotation: "stale-hash"}}}
	client := fake.NewSimpleClientset(deployment)
	rs := &fakeReconcileStore{env: reconcileEnv(), release: release, app: store.App{RuntimeClass: "gvisor"}}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: true}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	plan := planFrom(t, rs)
	found := false
	for _, op := range plan.Operations {
		if op.ObjectKind == "Deployment" && op.Action == "patch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected patch action, plan = %+v", plan.Operations)
	}
}

func TestReconcileEnvironmentScopeDetectsCreate(t *testing.T) {
	release := reconcileTestRelease(t)
	client := fake.NewSimpleClientset() // no deployment exists
	rs := &fakeReconcileStore{env: reconcileEnv(), release: release, app: store.App{RuntimeClass: "gvisor"}}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: true}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	plan := planFrom(t, rs)
	found := false
	for _, op := range plan.Operations {
		if op.ObjectKind == "Deployment" && op.Action == "create" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected create action, plan = %+v", plan.Operations)
	}
}

func TestReconcileRuntimeClassDriftDetected(t *testing.T) {
	release := reconcileTestRelease(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app-pod", Namespace: "tenant",
		Labels: map[string]string{ManagedByLabel: ManagedByValue}}, Spec: corev1.PodSpec{}}
	client := fake.NewSimpleClientset(pod)
	rs := &fakeReconcileStore{env: reconcileEnv(), release: release, app: store.App{RuntimeClass: "gvisor"}}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: true}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	plan := planFrom(t, rs)
	found := false
	for _, d := range plan.Drift {
		if d.Kind == "runtime_class_mismatch" && d.Expected == "gvisor" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected runtime_class_mismatch drift, got %+v", plan.Drift)
	}
}

func TestReconcileNetworkPolicyDriftDetected(t *testing.T) {
	release := reconcileTestRelease(t)
	// Only an ingress policy present → egress drift reported.
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "ingress", Namespace: "tenant",
		Labels: map[string]string{ManagedByLabel: ManagedByValue}}, Spec: networkingv1.NetworkPolicySpec{
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}}
	client := fake.NewSimpleClientset(policy)
	rs := &fakeReconcileStore{env: reconcileEnv(), release: release, app: store.App{RuntimeClass: "gvisor"}}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: true}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	plan := planFrom(t, rs)
	egress := false
	for _, d := range plan.Drift {
		if d.Kind == "missing_network_policy" && d.Name == "egress" {
			egress = true
		}
	}
	if !egress {
		t.Fatalf("expected egress network policy drift, got %+v", plan.Drift)
	}
}

func TestReconcileOrgScopeCallsListEnvironmentsByOrg(t *testing.T) {
	release := reconcileTestRelease(t)
	client := fake.NewSimpleClientset()
	rs := &fakeReconcileStore{envByOrg: []store.Environment{reconcileEnv()}, release: release, app: store.App{RuntimeClass: "gvisor"}}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "org", ScopeID: "org", DryRun: true}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	if !rs.listByOrgHit {
		t.Fatal("org scope did not call ListEnvironmentsByOrg")
	}
}

func TestReconcileAppScopeCallsListEnvironments(t *testing.T) {
	release := reconcileTestRelease(t)
	client := fake.NewSimpleClientset()
	appID := "app"
	rs := &fakeReconcileStore{envByApp: []store.Environment{reconcileEnv()}, release: release, app: store.App{RuntimeClass: "gvisor"}}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "app", ScopeID: "app", ScopeAppID: &appID, DryRun: true}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	if !rs.listByAppHit {
		t.Fatal("app scope did not call ListEnvironments")
	}
}

func TestReconcileFailsRunOnScopeResolutionError(t *testing.T) {
	client := fake.NewSimpleClientset()
	rs := &fakeReconcileStore{getEnvErr: errors.New("boom")}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: true}
	if err := runReconcile(t, rs, client, run); err == nil {
		t.Fatal("expected scope resolution error")
	}
	if !rs.completed || rs.completeParam.Status != "failed" {
		t.Fatalf("run not failed: %+v", rs.completeParam)
	}
}

func managedDeployment(name string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "tenant",
		Labels: map[string]string{ManagedByLabel: ManagedByValue, EnvironmentIDLabel: "environment"},
	}}
}

func hasActionOn(actions []k8stesting.Action, verb, resource, name string) bool {
	for _, action := range actions {
		if action.GetVerb() != verb || action.GetResource().Resource != resource {
			continue
		}
		switch typed := action.(type) {
		case k8stesting.CreateAction:
			obj, ok := typed.GetObject().(metav1.Object)
			if ok && obj.GetName() == name {
				return true
			}
		case k8stesting.DeleteAction:
			if typed.GetName() == name {
				return true
			}
		}
	}
	return false
}

func mutationAuditCount(evidence []store.ReconciliationEvidence) int {
	count := 0
	for _, e := range evidence {
		if e.Action != nil && e.Audit != nil {
			count++
		}
	}
	return count
}

func TestReconcileAppliesOnNonDryRun(t *testing.T) {
	release := reconcileTestRelease(t)
	client := fake.NewSimpleClientset() // deployment absent
	rs := &fakeReconcileStore{env: reconcileEnv(), release: release, app: store.App{RuntimeClass: "gvisor"}}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: false}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	if !hasActionOn(client.Actions(), "create", "deployments", "app") {
		t.Fatalf("expected Deployment create, actions = %+v", client.Actions())
	}
	found := false
	for _, e := range rs.evidence {
		if e.ObjectKind != nil && *e.ObjectKind == "Deployment" && e.Action != nil && *e.Action == "create" && e.Audit != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected create evidence with audit, got %+v", rs.evidence)
	}
	if rs.completeParam.Status != "completed" {
		t.Fatalf("run status = %q, want completed", rs.completeParam.Status)
	}
}

func productionReconcileEnv() store.Environment {
	env := reconcileEnv()
	env.Kind = "production"
	return env
}

func TestReconcileRestoresMissingCustomDomainIngress(t *testing.T) {
	release := reconcileTestRelease(t)
	client := fake.NewSimpleClientset() // no custom-domain Ingress exists
	rs := &fakeReconcileStore{
		env:          productionReconcileEnv(),
		release:      release,
		app:          store.App{RuntimeClass: "gvisor", Slug: "app"},
		verifiedDoms: []store.Domain{{ID: "domain-1", OrgID: "org", AppID: "app", Hostname: "app.customer.test"}},
	}
	adapter := &RecoveryAdapter{
		config: Config{PollInterval: time.Nanosecond, ConflictRetries: 3, APITimeout: time.Second, CertManagerClusterIssuer: "letsencrypt"},
		client: client,
		store:  rs,
	}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: false}
	if err := adapter.ExecuteReconciliation(context.Background(), run, "worker-one"); err != nil {
		t.Fatal(err)
	}
	ing, err := client.NetworkingV1().Ingresses("tenant").Get(context.Background(), "custom-domain-domain-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("custom-domain Ingress not restored: %v", err)
	}
	if ing.Spec.Rules[0].Host != "app.customer.test" || ing.Annotations[CertManagerClusterIssuerAnnotation] != "letsencrypt" {
		t.Fatalf("restored ingress = host %q issuer %q", ing.Spec.Rules[0].Host, ing.Annotations[CertManagerClusterIssuerAnnotation])
	}
	if ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name != "app" {
		t.Fatalf("restored ingress backend = %+v, want app", ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service)
	}
}

func TestReconcileReportsMissingCustomDomainIngressOnDryRun(t *testing.T) {
	release := reconcileTestRelease(t)
	client := fake.NewSimpleClientset()
	rs := &fakeReconcileStore{
		env:          productionReconcileEnv(),
		release:      release,
		app:          store.App{RuntimeClass: "gvisor", Slug: "app"},
		verifiedDoms: []store.Domain{{ID: "domain-1", OrgID: "org", AppID: "app", Hostname: "app.customer.test"}},
	}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: true}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	plan := planFrom(t, rs)
	found := false
	for _, op := range plan.Operations {
		if op.ObjectKind == "CustomDomainIngress" && op.ObjectName == "custom-domain-domain-1" && op.Action == "create" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected CustomDomainIngress create op, plan = %+v", plan.Operations)
	}
	// Dry-run must not mutate.
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" || action.GetVerb() == "update" {
			t.Fatalf("dry-run mutated: %+v", action)
		}
	}
}

func TestReconcileDryRunDoesNotMutate(t *testing.T) {
	release := reconcileTestRelease(t)
	client := fake.NewSimpleClientset() // deployment absent
	rs := &fakeReconcileStore{env: reconcileEnv(), release: release, app: store.App{RuntimeClass: "gvisor"}}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: true}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" || action.GetVerb() == "update" || action.GetVerb() == "delete" {
			t.Fatalf("dry-run mutated cluster: %+v", action)
		}
	}
	if n := mutationAuditCount(rs.evidence); n != 0 {
		t.Fatalf("dry-run emitted %d mutation audit rows, want 0", n)
	}
	for _, e := range rs.evidence {
		if e.Action != nil {
			t.Fatalf("dry-run evidence has non-nil action: %+v", e)
		}
	}
}

func TestReconcilePruneReportsOrphan(t *testing.T) {
	release := reconcileTestRelease(t)
	orphan := managedDeployment("orphan-app") // managed, no desired match
	client := fake.NewSimpleClientset(orphan)
	rs := &fakeReconcileStore{env: reconcileEnv(), release: release, app: store.App{RuntimeClass: "gvisor"}}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: true, Prune: false}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	plan := planFrom(t, rs)
	found := false
	for _, o := range plan.Orphans {
		if o.ObjectKind == "Deployment" && o.ObjectName == "orphan-app" && o.Action == "orphan" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected orphan report, got %+v", plan.Orphans)
	}
	if hasActionOn(client.Actions(), "delete", "deployments", "orphan-app") {
		t.Fatal("orphan deleted despite prune=false")
	}
}

func TestReconcilePruneDeletesOrphanWhenEnabled(t *testing.T) {
	release := reconcileTestRelease(t)
	orphan := managedDeployment("orphan-app")
	client := fake.NewSimpleClientset(orphan)
	rs := &fakeReconcileStore{env: reconcileEnv(), release: release, app: store.App{RuntimeClass: "gvisor"}}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: false, Prune: true}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	if !hasActionOn(client.Actions(), "delete", "deployments", "orphan-app") {
		t.Fatalf("expected orphan delete, actions = %+v", client.Actions())
	}
	found := false
	for _, e := range rs.evidence {
		if e.ObjectName != nil && *e.ObjectName == "orphan-app" && e.Action != nil && *e.Action == "delete" && e.Audit != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected delete evidence with audit, got %+v", rs.evidence)
	}
}

// managedIngress builds a Cyclopus-managed Ingress owned by the "environment"
// env, mirroring what the custom-domain provisioner creates (managed labels, no
// deploy-fence annotation).
func managedIngress(name string) *networkingv1.Ingress {
	return &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "tenant",
		Labels: map[string]string{ManagedByLabel: ManagedByValue, EnvironmentIDLabel: "environment"},
	}}
}

// A managed custom-domain Ingress with no matching verified domain is a stale
// orphan (e.g. the domain was deleted while the provisioner was unavailable). It
// must be reported on a no-prune run and NOT deleted.
func TestReconcilePruneReportsOrphanCustomDomainIngress(t *testing.T) {
	release := reconcileTestRelease(t)
	stale := managedIngress("custom-domain-deleted-1")
	client := fake.NewSimpleClientset(stale)
	rs := &fakeReconcileStore{
		env:     productionReconcileEnv(),
		release: release,
		app:     store.App{RuntimeClass: "gvisor", Slug: "app"},
		// no verified domains -> the stale custom-domain Ingress has no desired match
	}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: true, Prune: false}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	plan := planFrom(t, rs)
	found := false
	for _, o := range plan.Orphans {
		if o.ObjectKind == "Ingress" && o.ObjectName == "custom-domain-deleted-1" && o.Action == "orphan" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected stale custom-domain Ingress orphan report, got %+v", plan.Orphans)
	}
	if hasActionOn(client.Actions(), "delete", "ingresses", "custom-domain-deleted-1") {
		t.Fatal("orphan Ingress deleted despite prune=false")
	}
}

func TestReconcilePruneDeletesOrphanCustomDomainIngress(t *testing.T) {
	release := reconcileTestRelease(t)
	stale := managedIngress("custom-domain-deleted-1")
	client := fake.NewSimpleClientset(stale)
	rs := &fakeReconcileStore{
		env:     productionReconcileEnv(),
		release: release,
		app:     store.App{RuntimeClass: "gvisor", Slug: "app"},
	}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: false, Prune: true}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	if !hasActionOn(client.Actions(), "delete", "ingresses", "custom-domain-deleted-1") {
		t.Fatalf("expected orphan Ingress delete, actions = %+v", client.Actions())
	}
	found := false
	for _, e := range rs.evidence {
		if e.ObjectName != nil && *e.ObjectName == "custom-domain-deleted-1" && e.Action != nil && *e.Action == "delete" && e.Audit != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected delete evidence with audit for orphan Ingress, got %+v", rs.evidence)
	}
}

// A verified domain's own custom-domain Ingress is in the desired set and must
// NEVER be reported as an orphan, even under prune.
func TestReconcileDoesNotOrphanVerifiedCustomDomainIngress(t *testing.T) {
	release := reconcileTestRelease(t)
	wanted := managedIngress("custom-domain-domain-1")
	client := fake.NewSimpleClientset(wanted)
	rs := &fakeReconcileStore{
		env:          productionReconcileEnv(),
		release:      release,
		app:          store.App{RuntimeClass: "gvisor", Slug: "app"},
		verifiedDoms: []store.Domain{{ID: "domain-1", OrgID: "org", AppID: "app", Hostname: "app.customer.test"}},
	}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: false, Prune: true}
	if err := runReconcile(t, rs, client, run); err != nil {
		t.Fatal(err)
	}
	plan := planFrom(t, rs)
	for _, o := range plan.Orphans {
		if o.ObjectName == "custom-domain-domain-1" {
			t.Fatalf("verified custom-domain Ingress wrongly reported as orphan: %+v", o)
		}
	}
	if hasActionOn(client.Actions(), "delete", "ingresses", "custom-domain-domain-1") {
		t.Fatal("verified custom-domain Ingress wrongly deleted")
	}
}

func TestReconcileAbortsOnLeaseLoss(t *testing.T) {
	release := reconcileTestRelease(t)
	client := fake.NewSimpleClientset() // deployment absent → create planned
	rs := &fakeReconcileStore{env: reconcileEnv(), release: release, app: store.App{RuntimeClass: "gvisor"}}
	rs.appendErr = func(store.ReconciliationEvidence) error { return store.ErrLeaseNotOwned }
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: true}
	err := runReconcile(t, rs, client, run)
	if !errors.Is(err, store.ErrLeaseNotOwned) {
		t.Fatalf("expected ErrLeaseNotOwned, got %v", err)
	}
	if rs.completed {
		t.Fatal("run completed despite lease loss abort")
	}
}
