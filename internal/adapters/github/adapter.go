package github

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v88/github"
	"github.com/shepard-labs/cyclopus/internal/store"
)

const maxCorrelationPages = 10
const orchestrationCancellationCorrelationWindow = 5 * time.Minute
const workflowDispatchPinMargin = 5 * time.Second

type OrchestrationStore interface {
	BeginDispatchReconciliation(context.Context, string, string, string, string, string, string, time.Duration) error
	AuthorizeWorkflowDispatch(context.Context, string, string, string, time.Duration) error
	FinalizeWorkflowDispatch(context.Context, string, string, string) error
	RecordDispatchedRun(context.Context, string, string, string, string, int64, string, string, time.Duration) error
	RecordOrchestrationCancellation(context.Context, string, string, string, string, int64, string, string) error
	ContinueDispatchReconciliation(context.Context, string, string, string, time.Duration) error
	ContinueRunReconciliation(context.Context, string, string, string, string, *string, time.Duration) error
	ResolveOrchestration(context.Context, store.ResolveOrchestrationParams) (store.OrchestrationJob, error)
}

type Config struct {
	Owner                       string
	DispatchObservationInterval time.Duration
	RunPollInterval             time.Duration
	RequestTimeout              time.Duration
	Now                         func() time.Time
}

type Adapter struct {
	config  Config
	store   OrchestrationStore
	clients ClientFactory
}

func New(config Config, orchestrationStore OrchestrationStore, clients ClientFactory) (*Adapter, error) {
	if config.Owner == "" || config.DispatchObservationInterval <= 0 || config.RunPollInterval <= 0 || config.RequestTimeout <= 0 || orchestrationStore == nil || clients == nil {
		return nil, fmt.Errorf("invalid GitHub adapter configuration")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Adapter{config: config, store: orchestrationStore, clients: clients}, nil
}

func (a *Adapter) ErrorDisposition(err error) (bool, time.Duration, string, bool) {
	return ErrorDisposition(err)
}

func (a *Adapter) Dispatch(ctx context.Context, job *store.GitHubRecoveryJob) error {
	if err := validateJob(job, true); err != nil {
		return permanentJobError("github_dispatch_invalid_job", err)
	}
	client, err := a.clients.Client(job.InstallationID)
	if err != nil {
		return classifyError("create installation client", err)
	}
	owner, repository, err := splitRepository(job.RepositoryFullName)
	if err != nil {
		return permanentJobError("github_repository_invalid", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, a.config.RequestTimeout)
	ref, _, err := client.Git.GetRef(requestCtx, owner, repository, normalizeRefForAPI(job.GitRef))
	cancel()
	if err != nil {
		return classifyError("resolve dispatch ref", err)
	}
	requestCtx, cancel = context.WithTimeout(ctx, a.config.RequestTimeout)
	workflowFile, _, _, err := client.Repositories.GetContents(requestCtx, owner, repository, job.WorkflowID, &gh.RepositoryContentGetOptions{Ref: job.GitRef})
	cancel()
	if err != nil {
		return classifyError("resolve workflow revision", err)
	}
	if workflowFile == nil || workflowFile.GetPath() == "" || workflowFile.GetSHA() == "" {
		return permanentJobError("github_workflow_invalid", fmt.Errorf("GitHub workflow %q did not resolve to a file", job.WorkflowID))
	}
	if err := a.store.BeginDispatchReconciliation(ctx, job.OrgID, job.ID, a.config.Owner, ref.GetObject().GetSHA(), workflowFile.GetPath(), workflowFile.GetSHA(), a.config.DispatchObservationInterval); err != nil {
		return fmt.Errorf("begin GitHub dispatch reconciliation: %w", err)
	}
	request := gh.CreateWorkflowDispatchEventRequest{
		Ref: job.GitRef,
		Inputs: map[string]any{
			"orchestration_job_id": job.ID,
			"commit_sha":           job.CommitSHA,
		},
	}
	if err := a.store.AuthorizeWorkflowDispatch(ctx, job.OrgID, job.ID, a.config.Owner, a.config.RequestTimeout+workflowDispatchPinMargin); err != nil {
		return fmt.Errorf("authorize GitHub workflow dispatch: %w", err)
	}
	requestCtx, cancel = context.WithTimeout(ctx, a.config.RequestTimeout)
	_, _, err = client.Actions.CreateWorkflowDispatchEventByFileName(requestCtx, owner, repository, job.WorkflowID, request)
	cancel()
	finalizeCtx, finalizeCancel := context.WithTimeout(context.WithoutCancel(ctx), a.config.RequestTimeout)
	finalizeErr := a.store.FinalizeWorkflowDispatch(finalizeCtx, job.OrgID, job.ID, a.config.Owner)
	finalizeCancel()
	if finalizeErr != nil {
		return &DispatchStartedError{Err: &APIError{Operation: "finalize workflow dispatch authorization", Retryable: true, Err: finalizeErr}}
	}
	if err != nil {
		return &DispatchStartedError{Err: classifyError("dispatch workflow", err)}
	}
	return nil
}

func (a *Adapter) Cancel(ctx context.Context, job *store.GitHubRecoveryJob) error {
	if err := validateJob(job, false); err != nil {
		return permanentJobError("github_run_cancel_failed", err)
	}
	client, err := a.clients.Client(job.InstallationID)
	if err != nil {
		return classifyError("create installation client", err)
	}
	owner, repository, err := splitRepository(job.RepositoryFullName)
	if err != nil {
		return permanentJobError("github_run_cancel_failed", err)
	}
	runID, runStatus, runHeadSHA := int64(0), "", ""
	if job.GitHubRunID != nil {
		runID = *job.GitHubRunID
	} else {
		if job.DispatchRequestedAt == nil {
			return permanentJobError("github_run_cancel_failed", fmt.Errorf("cancelled dispatch %s is missing correlation evidence", job.ID))
		}
		requestCtx, cancel := context.WithTimeout(ctx, a.config.RequestTimeout)
		matches, complete, listErr := listMatchingRuns(requestCtx, client, owner, repository, job)
		cancel()
		if listErr != nil {
			return classifyError("list workflow runs for cancellation", listErr)
		}
		deadlineStartedAt := job.DispatchRequestedAt
		if job.CancellationRequestedAt != nil {
			deadlineStartedAt = job.CancellationRequestedAt
		}
		deadline := deadlineStartedAt.Add(store.OrchestrationCancellationDeadline)
		if !complete && a.config.Now().Before(deadline) {
			return &APIError{Operation: "list workflow runs for cancellation", Retryable: true, Err: errors.New("workflow run pagination exceeded correlation limit")}
		}
		if !complete {
			return permanentJobError("github_run_cancel_failed", errors.New("workflow run pagination remained incomplete until the cancellation deadline"))
		}
		if len(matches) == 0 && a.config.Now().Before(job.DispatchRequestedAt.Add(orchestrationCancellationCorrelationWindow)) {
			return &APIError{Operation: "correlate workflow run for cancellation", Retryable: true, RetryAfter: a.config.DispatchObservationInterval, Err: errors.New("workflow run is not observable yet")}
		}
		if len(matches) == 0 {
			return a.store.RecordOrchestrationCancellation(ctx, job.OrgID, job.ID, a.config.Owner, "no_run_observed", 0, "", "")
		}
		if len(matches) != 1 {
			return permanentJobError("github_run_cancel_failed", fmt.Errorf("cancellation correlation matched %d workflow runs", len(matches)))
		}
		runID, runStatus, runHeadSHA = matches[0].GetID(), normalizeNonterminalStatus(matches[0].GetStatus()), matches[0].GetHeadSHA()
	}
	requestCtx, cancel := context.WithTimeout(ctx, a.config.RequestTimeout)
	_, err = client.Actions.CancelWorkflowRunByID(requestCtx, owner, repository, runID)
	cancel()
	if err != nil {
		var response *gh.ErrorResponse
		if errors.As(err, &response) && statusCode(response.Response) == 409 {
			return a.store.RecordOrchestrationCancellation(ctx, job.OrgID, job.ID, a.config.Owner, "not_supported", runID, runStatus, runHeadSHA)
		}
		return classifyError("cancel workflow run", err)
	}
	return a.store.RecordOrchestrationCancellation(ctx, job.OrgID, job.ID, a.config.Owner, "accepted", runID, runStatus, runHeadSHA)
}

func (a *Adapter) ReconcileDispatch(ctx context.Context, job *store.GitHubRecoveryJob) error {
	if err := validateJob(job, false); err != nil {
		return permanentJobError("github_reconciliation_invalid_job", err)
	}
	client, err := a.clients.Client(job.InstallationID)
	if err != nil {
		return classifyError("create installation client", err)
	}
	owner, repository, err := splitRepository(job.RepositoryFullName)
	if err != nil {
		return permanentJobError("github_repository_invalid", err)
	}
	switch job.Status {
	case "dispatch_reconciling":
		return a.reconcileUncorrelated(ctx, client, owner, repository, job)
	case "dispatched":
		return a.reconcileRun(ctx, client, owner, repository, job)
	default:
		return permanentJobError("github_reconciliation_invalid_status", fmt.Errorf("unsupported GitHub recovery status %q", job.Status))
	}
}

func (a *Adapter) reconcileUncorrelated(ctx context.Context, client *gh.Client, owner, repository string, job *store.GitHubRecoveryJob) error {
	if job.DispatchRequestedAt == nil || job.DispatchDeadlineAt == nil {
		return permanentJobError("github_reconciliation_missing_deadline", fmt.Errorf("dispatch reconciliation job %s is missing deadlines", job.ID))
	}
	if !a.config.Now().Before(*job.DispatchDeadlineAt) {
		return a.resolveMissingRun(ctx, job)
	}
	requestCtx, cancel := context.WithTimeout(ctx, a.config.RequestTimeout)
	matches, complete, err := listMatchingRuns(requestCtx, client, owner, repository, job)
	cancel()
	if err != nil && !isNotFound(err) {
		if !a.config.Now().Before(*job.DispatchDeadlineAt) {
			return a.resolveMissingRun(ctx, job)
		}
		return classifyError("list workflow runs", err)
	}
	if !complete {
		if !a.config.Now().Before(*job.DispatchDeadlineAt) {
			return a.resolveMissingRun(ctx, job)
		}
		return &APIError{Operation: "list workflow runs", Retryable: true, Err: errors.New("workflow run pagination exceeded correlation limit")}
	}
	if len(matches) > 1 {
		if a.config.Now().Before(*job.DispatchDeadlineAt) {
			return a.store.ContinueDispatchReconciliation(ctx, job.OrgID, job.ID, a.config.Owner, a.config.DispatchObservationInterval)
		}
		code := "github_run_ambiguous"
		message := fmt.Sprintf("GitHub dispatch correlation matched %d workflow runs at the dispatch deadline", len(matches))
		_, err := a.store.ResolveOrchestration(ctx, store.ResolveOrchestrationParams{
			OrgID: job.OrgID, JobID: job.ID, Owner: a.config.Owner, RunStatus: "completed",
			TerminalCode: &code, TerminalMessage: &message,
		})
		return err
	}
	if len(matches) == 1 {
		run := matches[0]
		return a.store.RecordDispatchedRun(ctx, job.OrgID, job.ID, a.config.Owner,
			"cyclopus:"+job.ID, run.GetID(), normalizeNonterminalStatus(run.GetStatus()), run.GetHeadSHA(), a.config.RunPollInterval)
	}
	if !a.config.Now().Before(*job.DispatchDeadlineAt) {
		return a.resolveMissingRun(ctx, job)
	}
	return a.store.ContinueDispatchReconciliation(ctx, job.OrgID, job.ID, a.config.Owner, a.config.DispatchObservationInterval)
}

func (a *Adapter) reconcileRun(ctx context.Context, client *gh.Client, owner, repository string, job *store.GitHubRecoveryJob) error {
	if job.GitHubRunID == nil || job.RunDeadlineAt == nil {
		return permanentJobError("github_reconciliation_missing_run_identity", fmt.Errorf("dispatched job %s is missing run identity or deadline", job.ID))
	}
	if !a.config.Now().Before(*job.RunDeadlineAt) {
		return a.resolveRunTimeout(ctx, job)
	}
	requestCtx, cancel := context.WithTimeout(ctx, a.config.RequestTimeout)
	run, _, err := client.Actions.GetWorkflowRunByID(requestCtx, owner, repository, *job.GitHubRunID)
	cancel()
	if err != nil {
		if !a.config.Now().Before(*job.RunDeadlineAt) {
			return a.resolveRunTimeout(ctx, job)
		}
		if !isNotFound(err) {
			return classifyError("get workflow run", err)
		}
		if a.config.Now().Before(*job.RunDeadlineAt) {
			return a.store.ContinueRunReconciliation(ctx, job.OrgID, job.ID, a.config.Owner, "queued", nil, a.config.RunPollInterval)
		}
		return a.resolveRunTimeout(ctx, job)
	}

	status := run.GetStatus()
	if status != "completed" {
		if !a.config.Now().Before(*job.RunDeadlineAt) {
			return a.resolveRunTimeout(ctx, job)
		}
		status = normalizeNonterminalStatus(status)
		return a.store.ContinueRunReconciliation(ctx, job.OrgID, job.ID, a.config.Owner, status, nil, a.config.RunPollInterval)
	}

	conclusion := run.GetConclusion()
	if isSupportedConclusion(conclusion) {
		_, err := a.store.ResolveOrchestration(ctx, store.ResolveOrchestrationParams{
			OrgID: job.OrgID, JobID: job.ID, Owner: a.config.Owner, RunStatus: status, RunConclusion: conclusion,
		})
		return err
	}
	code := "github_run_conclusion_" + sanitizeCode(conclusion)
	message := fmt.Sprintf("GitHub workflow run completed with unsupported conclusion %q", conclusion)
	_, err = a.store.ResolveOrchestration(ctx, store.ResolveOrchestrationParams{
		OrgID: job.OrgID, JobID: job.ID, Owner: a.config.Owner, RunStatus: status, RunConclusion: "failure",
		TerminalCode: &code, TerminalMessage: &message,
	})
	return err
}

func (a *Adapter) resolveMissingRun(ctx context.Context, job *store.GitHubRecoveryJob) error {
	code := "github_run_missing"
	message := "no exactly correlated GitHub workflow run became observable before the dispatch deadline"
	_, err := a.store.ResolveOrchestration(ctx, store.ResolveOrchestrationParams{
		OrgID: job.OrgID, JobID: job.ID, Owner: a.config.Owner, RunStatus: "completed",
		TerminalCode: &code, TerminalMessage: &message,
	})
	return err
}

func (a *Adapter) resolveRunTimeout(ctx context.Context, job *store.GitHubRecoveryJob) error {
	code := "github_run_timed_out"
	message := "GitHub workflow run did not reach a terminal conclusion before the run deadline"
	_, err := a.store.ResolveOrchestration(ctx, store.ResolveOrchestrationParams{
		OrgID: job.OrgID, JobID: job.ID, Owner: a.config.Owner, RunStatus: "in_progress",
		TerminalCode: &code, TerminalMessage: &message,
	})
	return err
}

func listMatchingRuns(ctx context.Context, client *gh.Client, owner, repository string, job *store.GitHubRecoveryJob) ([]*gh.WorkflowRun, bool, error) {
	wantedTitle := "cyclopus:" + job.ID
	dispatchedAt := job.DispatchRequestedAt.UTC().Truncate(time.Second)
	created := ">=" + dispatchedAt.Format(time.RFC3339)
	var matches []*gh.WorkflowRun
	page := 1
	for page > 0 && page <= maxCorrelationPages {
		runs, response, err := client.Actions.ListWorkflowRunsByFileName(ctx, owner, repository, job.WorkflowID, &gh.ListWorkflowRunsOptions{
			Event: "workflow_dispatch", Created: created, ListOptions: gh.ListOptions{Page: page, PerPage: 100},
		})
		if err != nil {
			return nil, false, err
		}
		for _, run := range runs.WorkflowRuns {
			if run.GetEvent() == "workflow_dispatch" && run.GetDisplayTitle() == wantedTitle &&
				run.GetHeadSHA() == job.DispatchRefObservedSHA && run.GetHeadBranch() == normalizeGitRef(job.GitRef) &&
				run.GetHeadRepository().GetFullName() == job.RepositoryFullName &&
				run.CreatedAt != nil && !run.CreatedAt.Time.UTC().Truncate(time.Second).Before(dispatchedAt) {
				matches = append(matches, run)
			}
		}
		page = response.NextPage
	}
	return matches, page == 0, nil
}

func normalizeGitRef(ref string) string {
	return strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
}

func normalizeRefForAPI(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/")
	if strings.HasPrefix(ref, "heads/") || strings.HasPrefix(ref, "tags/") {
		return ref
	}
	return "heads/" + ref
}

func permanentJobError(code string, err error) error {
	return &PermanentJobError{Code: code, Err: err}
}

func validateJob(job *store.GitHubRecoveryJob, dispatch bool) error {
	if job == nil || job.ID == "" || job.OrgID == "" || job.InstallationID <= 0 || job.RepositoryFullName == "" || job.WorkflowID == "" {
		return fmt.Errorf("invalid GitHub orchestration job")
	}
	if job.CommitSHA == "" {
		return fmt.Errorf("GitHub orchestration job is missing commit SHA")
	}
	if dispatch && job.GitRef == "" {
		return fmt.Errorf("GitHub dispatch job is missing ref")
	}
	return nil
}

func splitRepository(fullName string) (string, string, error) {
	owner, repository, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || repository == "" || strings.Contains(repository, "/") {
		return "", "", fmt.Errorf("invalid GitHub repository full name %q", fullName)
	}
	return owner, repository, nil
}

func normalizeNonterminalStatus(status string) string {
	if status == "in_progress" {
		return status
	}
	return "queued"
}

func isSupportedConclusion(conclusion string) bool {
	switch conclusion {
	case "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale":
		return true
	default:
		return false
	}
}

func sanitizeCode(value string) string {
	if value == "" {
		return "unknown"
	}
	var result strings.Builder
	for _, char := range strings.ToLower(value) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' {
			result.WriteRune(char)
		} else {
			result.WriteByte('_')
		}
	}
	if result.Len() == 0 {
		return strconv.QuoteToASCII(value)
	}
	return result.String()
}
