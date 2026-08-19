package oci

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Disposition struct {
	Code           string
	Retryable      bool
	RetryAfter     time.Duration
	ExhaustionCode string
}

func Classify(err error, _ time.Time) Disposition {
	if err == nil {
		return Disposition{Code: string(CodeImageResolved)}
	}
	var typed *Error
	if errors.As(err, &typed) {
		return Disposition{Code: string(typed.code), Retryable: typed.retryable,
			RetryAfter: time.Duration(typed.retryAfter), ExhaustionCode: string(typed.exhaustionCode)}
	}
	var status *HTTPStatusError
	if errors.As(err, &status) {
		return classifyHTTPStatus(status)
	}
	var networkError net.Error
	if errors.As(err, &networkError) || errors.Is(err, context.DeadlineExceeded) {
		return Disposition{Code: string(CodeRegistryUnavailable), Retryable: true, ExhaustionCode: string(CodeRegistryUnavailable)}
	}
	return Disposition{Code: string(CodeRegistryResponseInvalid)}
}

func classifyHTTPStatus(status *HTTPStatusError) Disposition {
	retry := func(code Code) Disposition {
		return Disposition{Code: string(code), Retryable: true, RetryAfter: status.RetryAfter, ExhaustionCode: string(CodeRegistryUnavailable)}
	}
	switch status.StatusCode {
	case http.StatusUnauthorized:
		return Disposition{Code: string(CodeRegistryAuthFailed)}
	case http.StatusForbidden:
		if status.HasRetryAfter {
			return retry(CodeRegistryRateLimited)
		}
		return Disposition{Code: string(CodeRegistryForbidden)}
	case http.StatusNotFound:
		if status.Kind == ResponseManifest && !status.DeadlineExpired {
			return retry(CodeRegistryUnavailable)
		}
		if status.Kind == ResponseManifest {
			return Disposition{Code: string(CodeImageNotPublished)}
		}
		return Disposition{Code: string(CodeRegistryResponseInvalid)}
	case http.StatusConflict, http.StatusUnprocessableEntity:
		return Disposition{Code: string(CodeRegistryResponseInvalid)}
	case http.StatusTooManyRequests:
		return retry(CodeRegistryRateLimited)
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return retry(CodeRegistryUnavailable)
	default:
		if status.StatusCode >= 400 && status.StatusCode < 500 {
			return Disposition{Code: string(CodeRegistryResponseInvalid)}
		}
		return Disposition{Code: string(CodeRegistryUnavailable), Retryable: true, ExhaustionCode: string(CodeRegistryUnavailable)}
	}
}

func ParseRetryAfter(values []string, now time.Time) (time.Duration, bool) {
	if len(values) != 1 || strings.TrimSpace(values[0]) != values[0] || values[0] == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseUint(values[0], 10, 31); err == nil {
		return min(time.Duration(seconds)*time.Second, 5*time.Minute), true
	}
	parsed, err := http.ParseTime(values[0])
	if err != nil {
		return 0, false
	}
	delay := parsed.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return min(delay, 5*time.Minute), true
}

func RetryDelay(attempt int, retryAfter, remaining time.Duration, random io.Reader) time.Duration {
	if remaining <= 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	base := 2 * time.Second
	for i := 1; i < attempt && base < 30*time.Second; i++ {
		base *= 2
	}
	base = min(base, 30*time.Second)
	half := base / 2
	if random == nil {
		random = rand.Reader
	}
	var buffer [8]byte
	delay := base
	if _, err := io.ReadFull(random, buffer[:]); err == nil {
		delay = half + time.Duration(binary.BigEndian.Uint64(buffer[:])%uint64(base-half+1))
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	return min(delay, remaining)
}

func ExhaustionDisposition(kind string, publicationDeadlineExpired bool) Disposition {
	switch kind {
	case "image":
		if publicationDeadlineExpired {
			return Disposition{Code: string(CodeImageNotPublished)}
		}
		return Disposition{Code: string(CodeRegistryUnavailable)}
	case "credential":
		return Disposition{Code: string(CodeCredentialOperationExhausted)}
	case "namespace":
		return Disposition{Code: string(CodeImagePullSecretReconcileFailed)}
	default:
		return Disposition{Code: string(CodeRegistryResponseInvalid)}
	}
}
