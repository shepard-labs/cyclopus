// Package ui provides the server-rendered admin dashboard for Cyclopus,
// served at / alongside the grpc-gateway REST API at /v1/*.
//
// Page handlers read and mutate through the in-process Backend (backend.go),
// which calls the service and auth layers directly — no loopback HTTP.
// Templates consume plain store/service/observability types; only the two
// genuinely UI-local shapes below live in this package.
package ui

import (
	"time"

	"github.com/shepard-labs/cyclopus/internal/store"
)

// Session is the result of Login or Bootstrap: the session-token cookie payload
// plus the freshly issued token's expiry.
type Session struct {
	AccessToken string
	ExpiresAt   time.Time
	User        store.User
}

// FlashMessage is a transient UI notification passed through a one-time cookie.
type FlashMessage struct {
	Kind    string // "success" | "error" | "info"
	Message string
}

type ImportProgress struct {
	Operation   store.ImportOperation
	Deployment  *store.ImportDeploymentStatus
	Project     *store.Project
	App         *store.App
	Environment *store.Environment
}
