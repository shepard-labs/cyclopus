package github

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	ghinstallation "github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v88/github"
)

type APIError struct {
	Operation  string
	StatusCode int
	Retryable  bool
	RetryAfter time.Duration
	Err        error
}

type PermanentJobError struct {
	Code string
	Err  error
}

type DispatchStartedError struct{ Err error }

func (e *DispatchStartedError) Error() string         { return e.Err.Error() }
func (e *DispatchStartedError) Unwrap() error         { return e.Err }
func (e *DispatchStartedError) DispatchStarted() bool { return true }

func (e *PermanentJobError) Error() string { return e.Err.Error() }
func (e *PermanentJobError) Unwrap() error { return e.Err }

func ErrorDisposition(err error) (retryable bool, retryAfter time.Duration, code string, ok bool) {
	var api *APIError
	if errors.As(err, &api) {
		code = "github_api_rejected"
		if api.StatusCode != 0 {
			code = fmt.Sprintf("github_api_status_%d", api.StatusCode)
		}
		return api.Retryable, api.RetryAfter, code, true
	}
	var permanent *PermanentJobError
	if errors.As(err, &permanent) {
		return false, 0, permanent.Code, true
	}
	return false, 0, "", false
}

func (e *APIError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("GitHub %s failed with status %d: %v", e.Operation, e.StatusCode, e.Err)
	}
	return fmt.Sprintf("GitHub %s failed: %v", e.Operation, e.Err)
}

func (e *APIError) Unwrap() error { return e.Err }

func classifyError(operation string, err error) error {
	if err == nil {
		return nil
	}
	var rate *gh.RateLimitError
	if errors.As(err, &rate) {
		return &APIError{Operation: operation, StatusCode: http.StatusForbidden, Retryable: true, RetryAfter: time.Until(rate.Rate.Reset.Time), Err: err}
	}
	var abuse *gh.AbuseRateLimitError
	if errors.As(err, &abuse) {
		return &APIError{Operation: operation, StatusCode: statusCode(abuse.Response), Retryable: true, RetryAfter: abuse.GetRetryAfter(), Err: err}
	}
	var response *gh.ErrorResponse
	if errors.As(err, &response) {
		code := statusCode(response.Response)
		retryAfter := time.Duration(0)
		if response.Response != nil {
			if seconds, parseErr := time.ParseDuration(response.Response.Header.Get("Retry-After") + "s"); parseErr == nil {
				retryAfter = seconds
			}
		}
		return &APIError{Operation: operation, StatusCode: code, Retryable: code == http.StatusTooManyRequests || code >= 500, RetryAfter: retryAfter, Err: err}
	}
	var installation *ghinstallation.HTTPError
	if errors.As(err, &installation) {
		code := statusCode(installation.Response)
		return &APIError{Operation: operation, StatusCode: code, Retryable: code == 0 || code == http.StatusTooManyRequests || code >= 500, Err: err}
	}
	return &APIError{Operation: operation, Retryable: true, Err: err}
}

func statusCode(response *http.Response) int {
	if response == nil {
		return 0
	}
	return response.StatusCode
}

func isNotFound(err error) bool {
	var response *gh.ErrorResponse
	return errors.As(err, &response) && statusCode(response.Response) == http.StatusNotFound
}
