package server

import (
	"testing"
	"time"

	cyclopusv1 "github.com/shepard-labs/cyclopus/gen/go/cyclopus/v1"
)

func TestPageCursorRejectsFilterAndParentReuse(t *testing.T) {
	values := []cursorKey{
		{CreatedAt: time.Unix(1, 0), ID: "one"},
		{CreatedAt: time.Unix(2, 0), ID: "two"},
	}
	key := func(value cursorKey) cursorKey { return value }
	scope := pageScope("releases", "org-one", "environment-one", []string{"failed", "live"})
	page, token := pageValues(values, 0, 1, scope, key)
	if len(page) != 1 || token == "" {
		t.Fatalf("page/token = %+v/%q", page, token)
	}
	if _, _, err := pageWindow(1, token, pageScope("releases", "org-one", "environment-one", []string{"live"}), values, key); err == nil {
		t.Fatal("cursor was accepted with different filters")
	}
	if _, _, err := pageWindow(1, token, pageScope("releases", "org-one", "environment-two", []string{"failed", "live"}), values, key); err == nil {
		t.Fatal("cursor was accepted for a different parent")
	}
	if _, _, err := pageWindow(1, token, pageScope("releases", "org-two", "environment-one", []string{"failed", "live"}), values, key); err == nil {
		t.Fatal("cursor was accepted for a different organization")
	}
}

func TestReleaseCursorStatusScopeIsOrderIndependent(t *testing.T) {
	left, err := releaseStatusFilters([]cyclopusv1.ReleaseStatus{
		cyclopusv1.ReleaseStatus_RELEASE_STATUS_LIVE,
		cyclopusv1.ReleaseStatus_RELEASE_STATUS_FAILED,
	})
	if err != nil {
		t.Fatal(err)
	}
	right, err := releaseStatusFilters([]cyclopusv1.ReleaseStatus{
		cyclopusv1.ReleaseStatus_RELEASE_STATUS_FAILED,
		cyclopusv1.ReleaseStatus_RELEASE_STATUS_LIVE,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pageScope("releases", "org", "environment", left) != pageScope("releases", "org", "environment", right) {
		t.Fatal("equal normalized status filters produced different scopes")
	}
}
