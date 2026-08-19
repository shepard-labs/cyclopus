package oci

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDispositionRetryAfterAndJitter(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
		valid bool
	}{
		{"0", 0, true}, {"30", 30 * time.Second, true}, {"9999", 5 * time.Minute, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true}, {now.Add(10 * time.Minute).Format(http.TimeFormat), 5 * time.Minute, true},
		{" 1", 0, false}, {"1, 2", 0, false}, {"nope", 0, false},
	} {
		got, valid := ParseRetryAfter([]string{tc.value}, now)
		if got != tc.want || valid != tc.valid {
			t.Fatalf("ParseRetryAfter(%q) = %v, %v", tc.value, got, valid)
		}
	}
	if _, valid := ParseRetryAfter([]string{"1", "2"}, now); valid {
		t.Fatal("multiple Retry-After values accepted")
	}
	for attempt := 1; attempt <= 8; attempt++ {
		base := min(2*time.Second*time.Duration(1<<min(attempt-1, 4)), 30*time.Second)
		delay := RetryDelay(attempt, 0, time.Minute, bytes.NewReader(make([]byte, 8)))
		if delay < base/2 || delay > base {
			t.Fatalf("attempt %d delay %v outside [%v,%v]", attempt, delay, base/2, base)
		}
	}
	if got := RetryDelay(1, 20*time.Second, 5*time.Second, nil); got != 5*time.Second {
		t.Fatalf("deadline clamp = %v", got)
	}

	err := NewRetryableError(CodeRegistryRateLimited, CodeRegistryUnavailable, 3*time.Second)
	disposition := Classify(err, now)
	if disposition.Code != string(CodeRegistryRateLimited) || !disposition.Retryable || disposition.RetryAfter != 3*time.Second || disposition.ExhaustionCode != string(CodeRegistryUnavailable) {
		t.Fatalf("Classify() = %+v", disposition)
	}
	secret := "PAT-SENTINEL"
	if strings.Contains(NewError(CodeRegistryAuthFailed).Error(), secret) || strings.Contains(Classify(errors.New(secret), now).Code, secret) {
		t.Fatal("error leaked raw value")
	}
	for _, tc := range []struct {
		err       *HTTPStatusError
		code      Code
		retryable bool
	}{
		{&HTTPStatusError{StatusCode: 401}, CodeRegistryAuthFailed, false},
		{&HTTPStatusError{StatusCode: 403}, CodeRegistryForbidden, false},
		{&HTTPStatusError{StatusCode: 403, HasRetryAfter: true}, CodeRegistryRateLimited, true},
		{&HTTPStatusError{StatusCode: 404, Kind: ResponseManifest}, CodeRegistryUnavailable, true},
		{&HTTPStatusError{StatusCode: 404, Kind: ResponseManifest, DeadlineExpired: true}, CodeImageNotPublished, false},
		{&HTTPStatusError{StatusCode: 404, Kind: ResponseConfigBlob}, CodeRegistryResponseInvalid, false},
		{&HTTPStatusError{StatusCode: 429}, CodeRegistryRateLimited, true},
		{&HTTPStatusError{StatusCode: 503}, CodeRegistryUnavailable, true},
		{&HTTPStatusError{StatusCode: 422}, CodeRegistryResponseInvalid, false},
	} {
		got := Classify(tc.err, now)
		if got.Code != string(tc.code) || got.Retryable != tc.retryable {
			t.Fatalf("Classify(%+v) = %+v", tc.err, got)
		}
	}
	for kind, want := range map[string]string{"image": string(CodeRegistryUnavailable), "credential": string(CodeCredentialOperationExhausted), "namespace": string(CodeImagePullSecretReconcileFailed)} {
		if got := ExhaustionDisposition(kind, false).Code; got != want {
			t.Fatalf("ExhaustionDisposition(%q) = %q", kind, got)
		}
	}
	if got := ExhaustionDisposition("image", true).Code; got != string(CodeImageNotPublished) {
		t.Fatalf("publication exhaustion = %q", got)
	}
}
