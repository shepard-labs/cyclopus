package kubernetes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/cyclopus-dev/cyclopus/internal/service"
	"github.com/cyclopus-dev/cyclopus/internal/store"
)

// ReconcileStore is the subset of the data store the reconciliation executor
// requires. It is distinct from RecoveryStore so tests can provide a targeted
// fake.
type ReconcileStore interface {
	ListEnvironmentsByOrg(context.Context, string) ([]store.Environment, error)
	ListEnvironments(context.Context, string, string, bool) ([]store.Environment, error)
	GetEnvironment(context.Context, string, string) (store.Environment, error)
	GetApp(context.Context, string, string) (store.App, error)
	GetRelease(context.Context, string, string) (store.Release, error)
	ListVerifiedDomainsByApp(context.Context, string, string) ([]store.Domain, error)
	StartReconciliationRun(context.Context, string, string, string, json.RawMessage, *store.AuditEvent) error
	AppendReconciliationEvidence(context.Context, string, string, string, store.ReconciliationEvidence) error
	CompleteReconciliationRun(context.Context, store.CompleteReconciliationRunParams) error
}

// ExecuteReconciliation runs a queued ReconciliationRun against the cluster.
func (a *RecoveryAdapter) ExecuteReconciliation(ctx context.Context, run *store.ReconciliationRun, owner string) error {
	rs, ok := a.store.(ReconcileStore)
	if !ok {
		return fmt.Errorf("reconciliation store interface unavailable")
	}
	if a.now == nil {
		a.now = time.Now
	}
	ex := &reconcileExec{adapter: a, rs: rs, run: run, owner: owner}
	return ex.execute(ctx)
}

type reconcileExec struct {
	adapter *RecoveryAdapter
	rs      ReconcileStore
	run     *store.ReconciliationRun
	owner   string
}

type objectAction struct {
	EnvironmentID string  `json:"environment_id"`
	Namespace     string  `json:"namespace"`
	ObjectKind    string  `json:"object_kind"`
	ObjectName    string  `json:"object_name"`
	Action        string  `json:"action"`
	DesiredHash   *string `json:"desired_hash,omitempty"`
	ObservedHash  *string `json:"observed_hash,omitempty"`
}

type driftEntry struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Expected  string `json:"expected"`
	Observed  string `json:"observed"`
}

type reconcilePlan struct {
	Operations []objectAction `json:"operations"`
	Orphans    []objectAction `json:"orphans"`
	Drift      []driftEntry   `json:"drift"`
	Errors     []string       `json:"errors,omitempty"`
}

// envPlan carries the per-environment result of the plan phase so the apply
// and prune phases can act on the exact desired objects computed for it.
type envPlan struct {
	env       store.Environment
	execution store.DeployExecution
	objects   DesiredObjects
	ops       []objectAction
	// customDomains holds the desired custom-domain Ingress requests derived from
	// verified domains (production environments only). They are keyed by the
	// Ingress object name so applyOperation can locate the request for a planned
	// custom-domain op.
	customDomains map[string]service.CustomDomainRequest
}

func (e *reconcileExec) execute(ctx context.Context) error {
	e.adapter.log().Info("reconcile.invoked", "event", "reconcile.invoked",
		"run_id", e.run.ID, "org_id", e.run.OrgID, "scope_kind", e.run.ScopeKind,
		"dry_run", e.run.DryRun, "prune", e.run.Prune)
	environments, err := e.environmentsInScope(ctx)
	if err != nil {
		return e.failRun(ctx, "scope_resolution_failed", err)
	}

	var plan reconcilePlan
	var envPlans []envPlan
	envErrors := 0
	for _, env := range environments {
		if env.CurrentReleaseID == nil || env.Namespace == "" {
			continue
		}
		ep, drift, errs, err := e.planEnvironment(ctx, env)
		if err != nil {
			envErrors++
			plan.Errors = append(plan.Errors, fmt.Sprintf("env %s: %s", env.ID, err.Error()))
			continue
		}
		plan.Operations = append(plan.Operations, ep.ops...)
		plan.Drift = append(plan.Drift, drift...)
		plan.Errors = append(plan.Errors, errs...)
		envPlans = append(envPlans, ep)
	}

	// Orphan detection runs per environment namespace (H2, §9.2).
	for _, ep := range envPlans {
		orphans, errs := e.detectOrphans(ctx, ep)
		plan.Orphans = append(plan.Orphans, orphans...)
		plan.Errors = append(plan.Errors, errs...)
	}

	planJSON, _ := json.Marshal(plan)
	auditID := reconcileAuditID()
	if err := e.rs.StartReconciliationRun(ctx, e.run.OrgID, e.run.ID, e.owner, planJSON,
		&store.AuditEvent{ID: auditID, ActorUserID: e.run.ActorUserID}); err != nil {
		return e.failRun(ctx, "start_failed", err)
	}

	applyErrors := 0

	// Apply phase (H1): only mutate when dry_run=false; record the applied
	// action + audit only after a successful apply.
	for i := range envPlans {
		ep := &envPlans[i]
		for _, op := range ep.ops {
			if op.Action != "create" && op.Action != "patch" {
				if err := e.recordPlanned(ctx, op); err != nil {
					if errors.Is(err, store.ErrLeaseNotOwned) {
						return e.abortLeaseLost(ctx, err)
					}
				}
				continue
			}
			if e.run.DryRun {
				if err := e.recordPlanned(ctx, op); err != nil {
					if errors.Is(err, store.ErrLeaseNotOwned) {
						return e.abortLeaseLost(ctx, err)
					}
				}
				continue
			}
			if err := e.applyOperation(ctx, ep, op); err != nil {
				applyErrors++
				plan.Errors = append(plan.Errors, fmt.Sprintf("apply %s %s/%s: %s", op.ObjectKind, op.Namespace, op.ObjectName, err.Error()))
				if err := e.recordFailure(ctx, op, err); err != nil {
					if errors.Is(err, store.ErrLeaseNotOwned) {
						return e.abortLeaseLost(ctx, err)
					}
				}
				continue
			}
			if err := e.recordApplied(ctx, op); err != nil {
				if errors.Is(err, store.ErrLeaseNotOwned) {
					return e.abortLeaseLost(ctx, err)
				}
			}
		}
	}

	// Prune phase (H2): delete orphans only when prune=true and not dry-run.
	for _, orphan := range plan.Orphans {
		if !e.run.Prune || e.run.DryRun {
			if err := e.recordPlanned(ctx, orphan); err != nil {
				if errors.Is(err, store.ErrLeaseNotOwned) {
					return e.abortLeaseLost(ctx, err)
				}
			}
			continue
		}
		ep := envPlanFor(envPlans, orphan.EnvironmentID)
		if ep == nil {
			continue
		}
		if err := e.deleteOrphan(ctx, ep, orphan); err != nil {
			applyErrors++
			plan.Errors = append(plan.Errors, fmt.Sprintf("prune %s %s/%s: %s", orphan.ObjectKind, orphan.Namespace, orphan.ObjectName, err.Error()))
			if err := e.recordFailure(ctx, orphan, err); err != nil {
				if errors.Is(err, store.ErrLeaseNotOwned) {
					return e.abortLeaseLost(ctx, err)
				}
			}
			continue
		}
		deleted := orphan
		deleted.Action = "delete"
		if err := e.recordApplied(ctx, deleted); err != nil {
			if errors.Is(err, store.ErrLeaseNotOwned) {
				return e.abortLeaseLost(ctx, err)
			}
		}
	}

	status := "completed"
	if envErrors > 0 || applyErrors > 0 {
		status = "failed"
	}

	report, _ := json.Marshal(plan)
	params := store.CompleteReconciliationRunParams{
		OrgID:  e.run.OrgID,
		RunID:  e.run.ID,
		Owner:  e.owner,
		Status: status,
		Report: report,
	}
	if status == "failed" {
		code := "reconcile_incomplete"
		msg := fmt.Sprintf("%d environment error(s), %d apply error(s)", envErrors, applyErrors)
		params.ErrorCode = &code
		params.ErrorMessage = &msg
	}
	return e.rs.CompleteReconciliationRun(ctx, params)
}

func envPlanFor(plans []envPlan, environmentID string) *envPlan {
	for i := range plans {
		if plans[i].env.ID == environmentID {
			return &plans[i]
		}
	}
	return nil
}

func (e *reconcileExec) environmentsInScope(ctx context.Context) ([]store.Environment, error) {
	switch e.run.ScopeKind {
	case "environment":
		env, err := e.rs.GetEnvironment(ctx, e.run.OrgID, e.run.ScopeID)
		if err != nil {
			return nil, err
		}
		return []store.Environment{env}, nil
	case "app":
		if e.run.ScopeAppID == nil {
			return nil, fmt.Errorf("app scope requires scope_app_id")
		}
		return e.rs.ListEnvironments(ctx, e.run.OrgID, *e.run.ScopeAppID, false)
	case "org":
		return e.rs.ListEnvironmentsByOrg(ctx, e.run.OrgID)
	default:
		return nil, fmt.Errorf("unknown reconciliation scope: %s", e.run.ScopeKind)
	}
}

// planEnvironment computes the desired objects and the create/patch/no_op plan
// for one environment. It returns the environment plan, its isolation drift,
// non-fatal error strings (e.g. GetApp failure), and a fatal error that skips
// the environment entirely.
func (e *reconcileExec) planEnvironment(ctx context.Context, env store.Environment) (envPlan, []driftEntry, []string, error) {
	release, err := e.rs.GetRelease(ctx, env.OrgID, *env.CurrentReleaseID)
	if err != nil {
		return envPlan{}, nil, nil, fmt.Errorf("get release: %w", err)
	}
	envelope, err := store.DecodeReleaseSnapshot(release.ConfigSnapshot)
	if err != nil {
		return envPlan{}, nil, nil, fmt.Errorf("decode snapshot: %w", err)
	}
	execution := store.DeployExecution{
		OrgID:         env.OrgID,
		EnvironmentID: env.ID,
		Namespace:     env.Namespace,
		ReleaseID:     release.ID,
		FailedRelease: release.ImmutableReleaseSnapshot,
		Fence:         e.adapter.now(),
		ExpiresAt:     e.reconcileDeadline(),
	}
	objects, err := BuildDesiredObjects(execution, envelope, nil)
	if err != nil {
		return envPlan{}, nil, nil, fmt.Errorf("build desired: %w", err)
	}

	ep := envPlan{env: env, execution: execution, objects: objects}
	var drift []driftEntry
	var errs []string

	if objects.Deployment != nil {
		ep.ops = append(ep.ops, e.compareObj(ctx, env, "Deployment", objects.Deployment.Name, env.Namespace, release.ConfigHash,
			func(name, ns string) (string, bool) {
				d, err := e.adapter.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return "", apierrors.IsNotFound(err)
				}
				return d.Annotations[ConfigHashAnnotation], false
			}))
	}
	if objects.Service != nil {
		ep.ops = append(ep.ops, e.compareObj(ctx, env, "Service", objects.Service.Name, env.Namespace, release.ConfigHash,
			func(name, ns string) (string, bool) {
				svc, err := e.adapter.client.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return "", apierrors.IsNotFound(err)
				}
				return svc.Annotations[ConfigHashAnnotation], false
			}))
	}
	if objects.HPA != nil {
		ep.ops = append(ep.ops, e.compareObj(ctx, env, "HorizontalPodAutoscaler", objects.HPA.Name, env.Namespace, release.ConfigHash,
			func(name, ns string) (string, bool) {
				h, err := e.adapter.client.AutoscalingV2().HorizontalPodAutoscalers(ns).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return "", apierrors.IsNotFound(err)
				}
				return h.Annotations[ConfigHashAnnotation], false
			}))
	}

	// RuntimeClass and NetworkPolicy drift.
	app, err := e.rs.GetApp(ctx, env.OrgID, env.AppID)
	if err != nil {
		// M3: surface the failure rather than silently skipping RuntimeClass drift.
		errs = append(errs, fmt.Sprintf("env %s: get app: %s", env.ID, err.Error()))
	} else {
		drift = append(drift, e.runtimeClassDrift(ctx, env.Namespace, app.RuntimeClass)...)
		// Custom-domain Ingresses are Postgres-derived state for the production
		// environment: each verified domain expects an Ingress. Plan them so
		// reconcile reports and (on apply) restores any that are missing (§9.1).
		cdErrs := e.planCustomDomains(ctx, &ep, app)
		errs = append(errs, cdErrs...)
	}
	drift = append(drift, e.networkPolicyDrift(ctx, env.Namespace)...)

	return ep, drift, errs, nil
}

// planCustomDomains adds create/patch/no_op operations for the custom-domain
// Ingresses expected for the environment. Custom domains route to the app's
// production environment only, so previews are skipped. It builds the same
// provisioning request the DomainService uses so an applied reconcile restores
// exactly the object VerifyDomain would have created.
func (e *reconcileExec) planCustomDomains(ctx context.Context, ep *envPlan, app store.App) []string {
	if ep.env.Kind != "production" || ep.env.Namespace == "" {
		return nil
	}
	domains, err := e.rs.ListVerifiedDomainsByApp(ctx, ep.env.OrgID, ep.env.AppID)
	if err != nil {
		return []string{fmt.Sprintf("env %s: list verified domains: %s", ep.env.ID, err.Error())}
	}
	if len(domains) == 0 {
		return nil
	}
	ep.customDomains = make(map[string]service.CustomDomainRequest, len(domains))
	for _, domain := range domains {
		req := service.CustomDomainRequest{
			Namespace:     ep.env.Namespace,
			EnvironmentID: ep.env.ID,
			DomainID:      domain.ID,
			Hostname:      domain.Hostname,
			ServiceName:   app.Slug,
			ServicePort:   80,
			TLSSecretName: service.CustomDomainTLSSecretName(domain.ID),
			ClusterIssuer: e.adapter.config.CertManagerClusterIssuer,
		}
		name := CustomDomainIngressName(domain.ID)
		ep.customDomains[name] = req
		ep.ops = append(ep.ops, e.compareObj(ctx, ep.env, "CustomDomainIngress", name, ep.env.Namespace, domain.Hostname,
			func(name, ns string) (string, bool) {
				ing, err := e.adapter.client.NetworkingV1().Ingresses(ns).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return "", apierrors.IsNotFound(err)
				}
				// Compare on the served host so a hostname change (should not
				// happen, hostname is immutable per-row) or a missing rule is
				// reported as drift and re-applied.
				if len(ing.Spec.Rules) > 0 {
					return ing.Spec.Rules[0].Host, false
				}
				return "", false
			}))
	}
	return nil
}

func (e *reconcileExec) compareObj(ctx context.Context, env store.Environment, kind, name, ns, desiredHash string,
	getObservedHash func(string, string) (string, bool)) objectAction {
	op := objectAction{
		EnvironmentID: env.ID,
		Namespace:     ns,
		ObjectKind:    kind,
		ObjectName:    name,
		DesiredHash:   strPtr(desiredHash),
	}
	observed, notFound := getObservedHash(name, ns)
	if notFound {
		op.Action = "create"
		return op
	}
	op.ObservedHash = strPtr(observed)
	if observed == desiredHash {
		op.Action = "no_op"
	} else {
		op.Action = "patch"
	}
	return op
}

// applyOperation applies the single desired object identified by op using the
// existing RecoveryAdapter apply helpers, running outside deploy-lease renewal.
func (e *reconcileExec) applyOperation(ctx context.Context, ep *envPlan, op objectAction) error {
	applyCtx := withReconcileApply(ctx)
	execution := ep.execution
	setDesiredIdentity(ep.objects, execution.ReleaseID, execution.FailedRelease.ConfigHash)
	switch op.ObjectKind {
	case "Deployment":
		return e.adapter.applyDeployment(applyCtx, &execution, ep.objects.Deployment)
	case "Service":
		return e.adapter.applyService(applyCtx, &execution, ep.objects.Service)
	case "HorizontalPodAutoscaler":
		return e.adapter.applyHPA(applyCtx, &execution, ep.objects.HPA)
	case "CustomDomainIngress":
		req, ok := ep.customDomains[op.ObjectName]
		if !ok {
			return fmt.Errorf("no custom-domain request for %s", op.ObjectName)
		}
		// Reuse the shared, idempotent provisioner so reconcile restores exactly
		// the object VerifyDomain creates (get-or-create with ownership check).
		return NewCustomDomainProvisioner(e.adapter.client).ProvisionCustomDomain(applyCtx, req)
	default:
		return fmt.Errorf("unsupported object kind for apply: %s", op.ObjectKind)
	}
}

func (e *reconcileExec) reconcileDeadline() time.Time {
	// The reconcile executor holds a reconciliation lease, not a deploy lease.
	// operationContext derives its per-call timeout from ExpiresAt, so give the
	// synthetic execution a far-future expiry.
	if e.run.LeaseExpiresAt != nil {
		return *e.run.LeaseExpiresAt
	}
	return e.adapter.now().Add(time.Hour)
}

func (e *reconcileExec) runtimeClassDrift(ctx context.Context, namespace, expected string) []driftEntry {
	var drift []driftEntry
	// M4: list ALL pods in the namespace (no ManagedByLabel selector) so rogue
	// or unlabeled pods with the wrong RuntimeClass are detected too.
	pods, err := e.adapter.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return drift
	}
	for _, pod := range pods.Items {
		observed := ""
		if pod.Spec.RuntimeClassName != nil {
			observed = *pod.Spec.RuntimeClassName
		}
		if observed != expected {
			drift = append(drift, driftEntry{Kind: "runtime_class_mismatch", Namespace: namespace, Name: pod.Name, Expected: expected, Observed: observed})
			e.adapter.log().Warn("runtime_class.drift_detected", "event", "runtime_class.drift_detected",
				"namespace", namespace, "pod", pod.Name, "expected", expected, "observed", observed)
		}
	}
	return drift
}

func (e *reconcileExec) networkPolicyDrift(ctx context.Context, namespace string) []driftEntry {
	// This check is existence-only: it reports missing ingress/egress policies
	// by PolicyType and does NOT reconstruct or compare the egress spec. The
	// Except CIDR list (defaults + operator-supplied node public IPs) is owned
	// entirely by NetworkPolicyProvisioner (see mergeEgressExceptCIDRs in
	// lifecycle.go); no CIDR source is threaded here, so there is no risk of
	// false-reporting drift on the Except contents.
	var drift []driftEntry
	policies, err := e.adapter.client.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return drift
	}
	hasIngress, hasEgress := false, false
	for _, pol := range policies.Items {
		if pol.Labels[ManagedByLabel] != ManagedByValue {
			continue
		}
		for _, pt := range pol.Spec.PolicyTypes {
			if string(pt) == "Ingress" {
				hasIngress = true
			}
			if string(pt) == "Egress" {
				hasEgress = true
			}
		}
	}
	if !hasIngress {
		drift = append(drift, driftEntry{Kind: "missing_network_policy", Namespace: namespace, Name: "ingress", Expected: "present", Observed: "absent"})
	}
	if !hasEgress {
		drift = append(drift, driftEntry{Kind: "missing_network_policy", Namespace: namespace, Name: "egress", Expected: "present", Observed: "absent"})
	}
	return drift
}

// detectOrphans lists Cyclopus-managed objects in the environment namespace and
// reports any whose name is not in the environment's desired set as an orphan
// (§9.2). Detection is report-only; deletion happens in the prune phase.
func (e *reconcileExec) detectOrphans(ctx context.Context, ep envPlan) ([]objectAction, []string) {
	var orphans []objectAction
	var errs []string
	ns := ep.env.Namespace
	selector := ManagedByLabel + "=" + ManagedByValue

	desiredDeployments := desiredNames(ep.objects.Deployment != nil, deployName(ep.objects))
	desiredServices := serviceDesiredNames(ep.objects)
	desiredHPAs := desiredNames(ep.objects.HPA != nil, hpaName(ep.objects))
	desiredIngresses := ingressDesiredNames(ep)

	deployments, err := e.adapter.client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		errs = append(errs, fmt.Sprintf("env %s: list deployments: %s", ep.env.ID, err.Error()))
	} else {
		for _, d := range deployments.Items {
			if d.Labels[EnvironmentIDLabel] != ep.env.ID || desiredDeployments[d.Name] {
				continue
			}
			orphans = append(orphans, orphanAction(ep.env.ID, ns, "Deployment", d.Name))
		}
	}

	services, err := e.adapter.client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		errs = append(errs, fmt.Sprintf("env %s: list services: %s", ep.env.ID, err.Error()))
	} else {
		for _, s := range services.Items {
			if s.Labels[EnvironmentIDLabel] != ep.env.ID || desiredServices[s.Name] {
				continue
			}
			orphans = append(orphans, orphanAction(ep.env.ID, ns, "Service", s.Name))
		}
	}

	ingresses, err := e.adapter.client.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		errs = append(errs, fmt.Sprintf("env %s: list ingresses: %s", ep.env.ID, err.Error()))
	} else {
		for _, ing := range ingresses.Items {
			if ing.Labels[EnvironmentIDLabel] != ep.env.ID || desiredIngresses[ing.Name] {
				continue
			}
			orphans = append(orphans, orphanAction(ep.env.ID, ns, "Ingress", ing.Name))
		}
	}

	hpas, err := e.adapter.client.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		errs = append(errs, fmt.Sprintf("env %s: list hpas: %s", ep.env.ID, err.Error()))
	} else {
		for _, h := range hpas.Items {
			if h.Labels[EnvironmentIDLabel] != ep.env.ID || desiredHPAs[h.Name] {
				continue
			}
			orphans = append(orphans, orphanAction(ep.env.ID, ns, "HorizontalPodAutoscaler", h.Name))
		}
	}

	// KEDA ScaledObject / HTTPScaledObject via the dynamic client, when wired.
	if e.adapter.dynamic != nil {
		for _, scaler := range []struct {
			gvr  schema.GroupVersionResource
			kind string
		}{
			{gvr: scaledObjectGVR, kind: "ScaledObject"},
			{gvr: httpScaledObjectGVR, kind: "HTTPScaledObject"},
		} {
			desired := scaledObjectDesiredNames(ep.objects, scaler.kind)
			list, err := e.adapter.dynamic.Resource(scaler.gvr).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				if isDynamicKindUnavailable(err) {
					continue
				}
				errs = append(errs, fmt.Sprintf("env %s: list %s: %s", ep.env.ID, scaler.kind, err.Error()))
				continue
			}
			for _, item := range list.Items {
				labels := item.GetLabels()
				if labels[EnvironmentIDLabel] != ep.env.ID || desired[item.GetName()] {
					continue
				}
				orphans = append(orphans, orphanAction(ep.env.ID, ns, scaler.kind, item.GetName()))
			}
		}
	}

	return orphans, errs
}

// deleteOrphan removes one orphaned object using the UID-preconditioned,
// ownership-verified delete helpers, running outside deploy-lease renewal.
func (e *reconcileExec) deleteOrphan(ctx context.Context, ep *envPlan, orphan objectAction) error {
	applyCtx := withReconcileApply(ctx)
	execution := ep.execution
	switch orphan.ObjectKind {
	case "Deployment":
		return e.adapter.deleteOwnedDeployment(applyCtx, &execution, orphan.ObjectName)
	case "Service":
		return e.adapter.deleteOwnedService(applyCtx, &execution, orphan.ObjectName)
	case "Ingress":
		return e.adapter.deleteOwnedIngress(applyCtx, &execution, orphan.ObjectName)
	case "HorizontalPodAutoscaler":
		return e.adapter.deleteOwnedHPA(applyCtx, &execution, orphan.ObjectName)
	case "ScaledObject":
		return e.adapter.deleteOwnedScaledObject(applyCtx, &execution, scaledObjectGVR, orphan.ObjectName)
	case "HTTPScaledObject":
		return e.adapter.deleteOwnedScaledObject(applyCtx, &execution, httpScaledObjectGVR, orphan.ObjectName)
	default:
		return fmt.Errorf("unsupported orphan kind for delete: %s", orphan.ObjectKind)
	}
}

func orphanAction(envID, ns, kind, name string) objectAction {
	return objectAction{EnvironmentID: envID, Namespace: ns, ObjectKind: kind, ObjectName: name, Action: "orphan"}
}

func desiredNames(present bool, name string) map[string]bool {
	set := map[string]bool{}
	if present && name != "" {
		set[name] = true
	}
	return set
}

func deployName(objects DesiredObjects) string {
	if objects.Deployment == nil {
		return ""
	}
	return objects.Deployment.Name
}

func hpaName(objects DesiredObjects) string {
	if objects.HPA == nil {
		return ""
	}
	return objects.HPA.Name
}

func serviceDesiredNames(objects DesiredObjects) map[string]bool {
	set := map[string]bool{}
	if objects.Service != nil {
		set[objects.Service.Name] = true
	}
	if objects.BridgeService != nil {
		set[objects.BridgeService.Name] = true
	}
	return set
}

// ingressDesiredNames is the set of Ingress names Cyclopus expects in the
// environment namespace: the environment's own Ingress plus one per verified
// custom domain (production only). Any other managed Ingress is an orphan — e.g.
// a custom-domain Ingress left behind when a domain was deleted while the
// provisioner was unavailable, or after a hostname/ID change.
func ingressDesiredNames(ep envPlan) map[string]bool {
	set := map[string]bool{}
	if ep.objects.Ingress != nil {
		set[ep.objects.Ingress.Name] = true
	}
	for name := range ep.customDomains {
		set[name] = true
	}
	return set
}

func scaledObjectDesiredNames(objects DesiredObjects, kind string) map[string]bool {
	set := map[string]bool{}
	switch kind {
	case "ScaledObject":
		if objects.ScaledObject != nil {
			set[objects.ScaledObject.GetName()] = true
		}
	case "HTTPScaledObject":
		if objects.HTTPScaledObject != nil {
			set[objects.HTTPScaledObject.GetName()] = true
		}
	}
	return set
}

// recordPlanned records evidence for a planned (unapplied) action with NO
// Action pointer, so no mutation audit row is emitted (H1).
func (e *reconcileExec) recordPlanned(ctx context.Context, op objectAction) error {
	return e.appendEvidence(ctx, op, nil, false)
}

// recordApplied records evidence for a successfully applied mutation, with the
// Action pointer set and a mutation audit event (H1).
func (e *reconcileExec) recordApplied(ctx context.Context, op objectAction) error {
	action := op.Action
	return e.appendEvidence(ctx, op, &action, true)
}

// recordFailure records evidence for an apply that failed. No mutation audit is
// emitted because nothing was mutated.
func (e *reconcileExec) recordFailure(ctx context.Context, op objectAction, cause error) error {
	kind := op.ObjectKind
	ns := op.Namespace
	name := op.ObjectName
	details, _ := json.Marshal(map[string]string{"planned_action": op.Action, "error": cause.Error()})
	evidence := store.ReconciliationEvidence{
		Kind:            "object_reconcile_failed",
		ObjectKind:      &kind,
		ObjectNamespace: &ns,
		ObjectName:      &name,
		DesiredHash:     op.DesiredHash,
		ObservedHash:    op.ObservedHash,
		Details:         details,
	}
	return e.rs.AppendReconciliationEvidence(ctx, e.run.OrgID, e.run.ID, e.owner, evidence)
}

func (e *reconcileExec) appendEvidence(ctx context.Context, op objectAction, actionPtr *string, audit bool) error {
	kind := op.ObjectKind
	ns := op.Namespace
	name := op.ObjectName
	details, _ := json.Marshal(map[string]string{"planned_action": op.Action})
	evidence := store.ReconciliationEvidence{
		Kind:            "object_reconciled",
		ObjectKind:      &kind,
		ObjectNamespace: &ns,
		ObjectName:      &name,
		Action:          actionPtr,
		DesiredHash:     op.DesiredHash,
		ObservedHash:    op.ObservedHash,
		Details:         details,
	}
	if audit {
		evidence.Audit = &store.AuditEvent{ID: reconcileAuditID(), ActorUserID: e.run.ActorUserID}
	}
	return e.rs.AppendReconciliationEvidence(ctx, e.run.OrgID, e.run.ID, e.owner, evidence)
}

// abortLeaseLost stops the run immediately when the reconciliation lease is no
// longer owned (M2). The store already changed ownership, so completing the run
// is not attempted here; the loss error is returned to the caller.
func (e *reconcileExec) abortLeaseLost(_ context.Context, cause error) error {
	return cause
}

func (e *reconcileExec) failRun(ctx context.Context, code string, cause error) error {
	msg := cause.Error()
	report, _ := json.Marshal(map[string]string{"error": msg})
	_ = e.rs.CompleteReconciliationRun(ctx, store.CompleteReconciliationRunParams{
		OrgID: e.run.OrgID, RunID: e.run.ID, Owner: e.owner,
		Status: "failed", Report: report, ErrorCode: &code, ErrorMessage: &msg,
	})
	return cause
}

func strPtr(s string) *string { return &s }

func reconcileAuditID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
