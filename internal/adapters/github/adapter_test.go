package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyclopus-dev/cyclopus/internal/store"
	gh "github.com/google/go-github/v88/github"
)

func TestDispatchPersistsReconciliationBeforeRequest(t *testing.T) {
	storeFake := &fakeStore{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if storeFake.beginCalls != 1 {
			t.Fatal("workflow dispatched before reconciliation was persisted")
		}
		if request.Method != http.MethodPost || request.URL.Path != "/repos/acme/widget/actions/workflows/build.yml/dispatches" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("X-GitHub-Api-Version") != "2026-03-10" || request.Header.Get("Accept") != "application/vnd.github+json" {
			t.Fatalf("headers = %#v", request.Header)
		}
		body := readBody(t, request)
		if !strings.Contains(body, `"ref":"main"`) || !strings.Contains(body, `"orchestration_job_id":"job-one"`) ||
			!strings.Contains(body, `"commit_sha":"`+strings.Repeat("a", 40)+`"`) {
			t.Fatalf("body = %s", body)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	adapter := newTestAdapter(t, storeFake, server.URL, time.Now)
	job := testJob()
	job.Status = "leased"
	job.GitRef = "main"
	if err := adapter.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if storeFake.beginCalls != 1 {
		t.Fatalf("begin calls = %d", storeFake.beginCalls)
	}
}

func TestReconcileDispatchCorrelatesExactRunAcrossPages(t *testing.T) {
	now := time.Now().UTC()
	storeFake := &fakeStore{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			t.Fatal("recovery must never redispatch")
		}
		if request.URL.Query().Get("event") != "workflow_dispatch" || request.URL.Query().Get("per_page") != "100" {
			t.Fatalf("query = %s", request.URL.RawQuery)
		}
		if request.URL.Query().Get("page") == "1" {
			writer.Header().Set("Link", `<`+serverURL(request)+`?event=workflow_dispatch&page=2&per_page=100>; rel="next"`)
			writeJSON(t, writer, `{"total_count":2,"workflow_runs":[{"id":1,"event":"workflow_dispatch","display_title":"cyclopus:job-one-extra","status":"queued","head_sha":"`+strings.Repeat("a", 40)+`","head_branch":"main","head_repository":{"full_name":"acme/widget"},"created_at":"`+now.Format(time.RFC3339)+`"}]}`)
			return
		}
		writeJSON(t, writer, `{"total_count":2,"workflow_runs":[{"id":99,"event":"workflow_dispatch","display_title":"cyclopus:job-one","status":"in_progress","head_sha":"`+strings.Repeat("a", 40)+`","head_branch":"main","head_repository":{"full_name":"acme/widget"},"created_at":"`+now.Format(time.RFC3339)+`"}]}`)
	}))
	defer server.Close()

	adapter := newTestAdapter(t, storeFake, server.URL, func() time.Time { return now })
	job := testJob()
	job.DispatchRequestedAt = ptrTime(now.Add(-time.Minute))
	job.DispatchDeadlineAt = ptrTime(now.Add(time.Minute))
	if err := adapter.ReconcileDispatch(context.Background(), job); err != nil {
		t.Fatalf("ReconcileDispatch() error = %v", err)
	}
	if storeFake.recordRunID != 99 || storeFake.recordStatus != "in_progress" {
		t.Fatalf("recorded run = %d/%s", storeFake.recordRunID, storeFake.recordStatus)
	}
}

func TestReconcileDispatchNoRunBeforeAndAfterDeadline(t *testing.T) {
	now := time.Now().UTC()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		writeJSON(t, writer, `{"total_count":0,"workflow_runs":[]}`)
	}))
	defer server.Close()

	t.Run("continue", func(t *testing.T) {
		storeFake := &fakeStore{}
		adapter := newTestAdapter(t, storeFake, server.URL, func() time.Time { return now })
		job := testJob()
		job.DispatchRequestedAt = ptrTime(now.Add(-time.Minute))
		job.DispatchDeadlineAt = ptrTime(now.Add(time.Minute))
		if err := adapter.ReconcileDispatch(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		if storeFake.continueDispatchCalls != 1 || storeFake.resolveCalls != 0 {
			t.Fatalf("calls = continue:%d resolve:%d", storeFake.continueDispatchCalls, storeFake.resolveCalls)
		}
	})

	t.Run("missing", func(t *testing.T) {
		requestsBefore := requests
		storeFake := &fakeStore{}
		adapter := newTestAdapter(t, storeFake, server.URL, func() time.Time { return now })
		job := testJob()
		job.DispatchRequestedAt = ptrTime(now.Add(-time.Minute))
		job.DispatchDeadlineAt = ptrTime(now)
		if err := adapter.ReconcileDispatch(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		if storeFake.resolve.TerminalCode == nil || *storeFake.resolve.TerminalCode != "github_run_missing" {
			t.Fatalf("resolution = %+v", storeFake.resolve)
		}
		if requests != requestsBefore {
			t.Fatalf("expired reconciliation made %d API requests, want 0", requests-requestsBefore)
		}
	})
}

func TestReconcileDispatchRetryableListFailureAtDeadlineResolvesTerminal(t *testing.T) {
	now := time.Now().UTC()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		http.Error(writer, `{"message":"unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	storeFake := &fakeStore{}
	nowCalls := 0
	adapter := newTestAdapter(t, storeFake, server.URL, func() time.Time {
		nowCalls++
		if nowCalls == 1 {
			return now
		}
		return now.Add(2 * time.Second)
	})
	job := testJob()
	job.DispatchRequestedAt = ptrTime(now.Add(-time.Minute))
	job.DispatchDeadlineAt = ptrTime(now.Add(time.Second))
	if err := adapter.ReconcileDispatch(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || storeFake.continueDispatchCalls != 0 || storeFake.resolve.TerminalCode == nil ||
		*storeFake.resolve.TerminalCode != "github_run_missing" {
		t.Fatalf("requests/store = %d/%+v", requests, storeFake)
	}
}

func TestReconcileDispatchReschedulesDuplicateExactMatches(t *testing.T) {
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, `{"total_count":2,"workflow_runs":[`+
			`{"id":1,"event":"workflow_dispatch","display_title":"cyclopus:job-one","status":"queued","head_sha":"`+strings.Repeat("a", 40)+`","head_branch":"main","head_repository":{"full_name":"acme/widget"},"created_at":"`+now.Format(time.RFC3339)+`"},`+
			`{"id":2,"event":"workflow_dispatch","display_title":"cyclopus:job-one","status":"queued","head_sha":"`+strings.Repeat("a", 40)+`","head_branch":"main","head_repository":{"full_name":"acme/widget"},"created_at":"`+now.Format(time.RFC3339)+`"}]}`)
	}))
	defer server.Close()

	storeFake := &fakeStore{}
	adapter := newTestAdapter(t, storeFake, server.URL, func() time.Time { return now })
	job := testJob()
	job.DispatchRequestedAt = ptrTime(now.Add(-time.Minute))
	job.DispatchDeadlineAt = ptrTime(now.Add(time.Minute))
	if err := adapter.ReconcileDispatch(context.Background(), job); err != nil {
		t.Fatalf("ReconcileDispatch() error = %v", err)
	}
	if storeFake.recordRunID != 0 || storeFake.continueDispatchCalls != 1 || storeFake.resolveCalls != 0 {
		t.Fatalf("store mutated after ambiguous correlation: %+v", storeFake)
	}
}

func TestReconcileRunStatesAndConclusions(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name       string
		status     string
		conclusion string
		wantPoll   string
		wantResult string
	}{
		{name: "queued", status: "queued", wantPoll: "queued"},
		{name: "waiting normalizes queued", status: "waiting", wantPoll: "queued"},
		{name: "in progress", status: "in_progress", wantPoll: "in_progress"},
		{name: "success", status: "completed", conclusion: "success", wantResult: "success"},
		{name: "failure", status: "completed", conclusion: "failure", wantResult: "failure"},
		{name: "neutral", status: "completed", conclusion: "neutral", wantResult: "neutral"},
		{name: "cancelled", status: "completed", conclusion: "cancelled", wantResult: "cancelled"},
		{name: "skipped", status: "completed", conclusion: "skipped", wantResult: "skipped"},
		{name: "timed out", status: "completed", conclusion: "timed_out", wantResult: "timed_out"},
		{name: "action required", status: "completed", conclusion: "action_required", wantResult: "action_required"},
		{name: "stale", status: "completed", conclusion: "stale", wantResult: "stale"},
		{name: "startup failure", status: "completed", conclusion: "startup_failure", wantResult: "startup_failure"},
		{name: "unsupported", status: "completed", conclusion: "new_conclusion", wantResult: "failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storeFake := &fakeStore{}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writeJSON(t, writer, `{"id":99,"status":"`+test.status+`","conclusion":"`+test.conclusion+`"}`)
			}))
			defer server.Close()
			adapter := newTestAdapter(t, storeFake, server.URL, func() time.Time { return now })
			job := testJob()
			job.Status = "dispatched"
			job.GitHubRunID = ptrInt64(99)
			job.RunDeadlineAt = ptrTime(now.Add(time.Minute))
			if err := adapter.ReconcileDispatch(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			if test.wantPoll != "" && storeFake.continueRunStatus != test.wantPoll {
				t.Fatalf("poll status = %q", storeFake.continueRunStatus)
			}
			if test.wantResult != "" && storeFake.resolve.RunConclusion != test.wantResult {
				t.Fatalf("resolution = %+v", storeFake.resolve)
			}
		})
	}
}

func TestReconcileRunTransient404AndTimeout(t *testing.T) {
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	for _, test := range []struct {
		name     string
		deadline time.Time
		wantCode string
	}{
		{name: "transient", deadline: now.Add(time.Minute)},
		{name: "timed out", deadline: now, wantCode: "github_run_timed_out"},
	} {
		t.Run(test.name, func(t *testing.T) {
			storeFake := &fakeStore{}
			adapter := newTestAdapter(t, storeFake, server.URL, func() time.Time { return now })
			job := testJob()
			job.Status = "dispatched"
			job.GitHubRunID = ptrInt64(99)
			job.RunDeadlineAt = &test.deadline
			if err := adapter.ReconcileDispatch(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			if test.wantCode == "" && storeFake.continueRunStatus != "queued" {
				t.Fatalf("continue status = %q", storeFake.continueRunStatus)
			}
			if test.wantCode != "" && (storeFake.resolve.TerminalCode == nil || *storeFake.resolve.TerminalCode != test.wantCode) {
				t.Fatalf("resolution = %+v", storeFake.resolve)
			}
		})
	}
}

func TestReconcileRunRetryableGetFailureAtDeadlineResolvesTerminal(t *testing.T) {
	now := time.Now().UTC()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		http.Error(writer, `{"message":"unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	storeFake := &fakeStore{}
	nowCalls := 0
	adapter := newTestAdapter(t, storeFake, server.URL, func() time.Time {
		nowCalls++
		if nowCalls == 1 {
			return now
		}
		return now.Add(2 * time.Second)
	})
	job := testJob()
	job.Status = "dispatched"
	job.GitHubRunID = ptrInt64(99)
	job.RunDeadlineAt = ptrTime(now.Add(time.Second))
	if err := adapter.ReconcileDispatch(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || storeFake.continueRunStatus != "" || storeFake.resolve.TerminalCode == nil ||
		*storeFake.resolve.TerminalCode != "github_run_timed_out" {
		t.Fatalf("requests/store = %d/%+v", requests, storeFake)
	}
}

func TestReconcileClassifiesGitHubErrors(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		retryable bool
	}{
		{name: "auth", status: http.StatusUnauthorized},
		{name: "rate", status: http.StatusTooManyRequests, retryable: true},
		{name: "server", status: http.StatusServiceUnavailable, retryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if test.status == http.StatusTooManyRequests {
					writer.Header().Set("Retry-After", "10")
				}
				http.Error(writer, `{"message":"failed"}`, test.status)
			}))
			defer server.Close()
			adapter := newTestAdapter(t, &fakeStore{}, server.URL, time.Now)
			job := testJob()
			job.DispatchRequestedAt = ptrTime(time.Now().Add(-time.Minute))
			job.DispatchDeadlineAt = ptrTime(time.Now().Add(time.Minute))
			err := adapter.ReconcileDispatch(context.Background(), job)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != test.status || apiErr.Retryable != test.retryable {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestDispatchHangingServerTimesOutAfterReconciliationBegins(t *testing.T) {
	storeFake := &fakeStore{}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	adapter := newTestAdapterWithTimeout(t, storeFake, server.URL, time.Now, 20*time.Millisecond)
	job := testJob()
	job.Status = "leased"
	err := adapter.Dispatch(context.Background(), job)
	var started *DispatchStartedError
	var apiErr *APIError
	if !errors.As(err, &started) || !errors.As(err, &apiErr) || !apiErr.Retryable {
		t.Fatalf("Dispatch() error = %#v", err)
	}
	if storeFake.beginCalls != 1 {
		t.Fatalf("begin calls = %d, want 1", storeFake.beginCalls)
	}
}

func newTestAdapter(t *testing.T, orchestrationStore OrchestrationStore, baseURL string, now func() time.Time) *Adapter {
	return newTestAdapterWithTimeout(t, orchestrationStore, baseURL, now, time.Second)
}

func newTestAdapterWithTimeout(t *testing.T, orchestrationStore OrchestrationStore, baseURL string, now func() time.Time, requestTimeout time.Duration) *Adapter {
	t.Helper()
	apiURL := baseURL + "/"
	client, err := gh.NewClient(
		gh.WithURLs(&apiURL, &apiURL),
		gh.WithTransport(apiVersionTransport{version: "2026-03-10", next: http.DefaultTransport}),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{Owner: "reconciler", DispatchObservationInterval: time.Minute, RunPollInterval: 30 * time.Second, RequestTimeout: requestTimeout, Now: now}, orchestrationStore, staticFactory{client: client})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

type staticFactory struct{ client *gh.Client }

func (f staticFactory) Client(int64) (*gh.Client, error) { return f.client, nil }

type fakeStore struct {
	beginCalls            int
	continueDispatchCalls int
	continueRunStatus     string
	recordRunID           int64
	recordStatus          string
	resolveCalls          int
	resolve               store.ResolveOrchestrationParams
}

func (f *fakeStore) BeginDispatchReconciliation(context.Context, string, string, string, time.Duration) error {
	f.beginCalls++
	return nil
}

func (f *fakeStore) RecordDispatchedRun(_ context.Context, _, _, _, _ string, runID int64, status string, _ time.Duration) error {
	f.recordRunID = runID
	f.recordStatus = status
	return nil
}

func (f *fakeStore) ContinueDispatchReconciliation(context.Context, string, string, string, time.Duration) error {
	f.continueDispatchCalls++
	return nil
}

func (f *fakeStore) ContinueRunReconciliation(_ context.Context, _, _, _, status string, _ *string, _ time.Duration) error {
	f.continueRunStatus = status
	return nil
}

func (f *fakeStore) ResolveOrchestration(_ context.Context, params store.ResolveOrchestrationParams) (store.OrchestrationJob, error) {
	f.resolveCalls++
	f.resolve = params
	return store.OrchestrationJob{}, nil
}

func testJob() *store.GitHubRecoveryJob {
	return &store.GitHubRecoveryJob{
		ID: "job-one", OrgID: "org-one", Status: "dispatch_reconciling", WorkflowID: "build.yml",
		InstallationID: 42, RepositoryFullName: "acme/widget", GitRef: "refs/heads/main",
		CommitSHA: strings.Repeat("a", 40),
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
func ptrInt64(value int64) *int64        { return &value }

func readBody(t *testing.T, request *http.Request) string {
	t.Helper()
	contents, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func writeJSON(t *testing.T, writer http.ResponseWriter, body string) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
}

func serverURL(request *http.Request) string {
	return "http://" + request.Host + request.URL.Path
}
