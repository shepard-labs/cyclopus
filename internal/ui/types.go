// Package ui provides the server-rendered admin dashboard for Cyclopus,
// served at / alongside the grpc-gateway REST API at /v1/*.
//
// Every page handler makes HTTP calls to the loopback REST API so the UI is
// fully rooted in the defined API contract. No direct service or store calls.
package ui

import "time"

// Session is the decoded result of POST /v1/auth/login or /v1/auth/bootstrap.
type Session struct {
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
	User        User      `json:"user"`
}

// User mirrors the User proto message.
type User struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organizationId"`
	Email          string    `json:"email"`
	CreatedAt      time.Time `json:"createdAt"`
}

// Organization mirrors the Organization proto message.
type Organization struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"createdAt"`
}

// Project mirrors the Project proto message.
type Project struct {
	ID              string    `json:"id"`
	OrganizationID  string    `json:"organizationId"`
	Name            string    `json:"name"`
	Slug            string    `json:"slug"`
	GitConnectionID string    `json:"gitConnectionId"`
	CreatedAt       time.Time `json:"createdAt"`
}

// Application mirrors the Application proto message.
type Application struct {
	ID                     string    `json:"id"`
	OrganizationID         string    `json:"organizationId"`
	ProjectID              string    `json:"projectId"`
	Name                   string    `json:"name"`
	Slug                   string    `json:"slug"`
	HostnameLabel          string    `json:"hostnameLabel"`
	RegistryID             string    `json:"registryId"`
	ImageRepository        string    `json:"imageRepository"`
	ContainerPort          int       `json:"containerPort"`
	HealthCheckPath        string    `json:"healthCheckPath"`
	ScalingMode            string    `json:"scalingMode"`
	MinReplicas            int       `json:"minReplicas"`
	MaxReplicas            int       `json:"maxReplicas"`
	HPATargetCPUPercent    int       `json:"hpaTargetCpuPercent"`
	CPURequestMillicores   int       `json:"cpuRequestMillicores"`
	CPULimitMillicores     int       `json:"cpuLimitMillicores"`
	MemoryRequestMiB       int       `json:"memoryRequestMib"`
	MemoryLimitMiB         int       `json:"memoryLimitMib"`
	PreviewsEnabled        bool      `json:"previewsEnabled"`
	MaxPreviewEnvironments int       `json:"maxPreviewEnvironments"`
	PreviewIdleTTLHours    int       `json:"previewIdleTtlHours"`
	DeployTimeoutSeconds   int       `json:"deployTimeoutSeconds"`
	ReleaseRetentionCount  int       `json:"releaseRetentionCount"`
	RuntimeClass           string    `json:"runtimeClass"`
	CIWorkflowID           string    `json:"ciWorkflowId"`
	CreatedAt              time.Time `json:"createdAt"`
}

// Environment mirrors the Environment proto message.
type Environment struct {
	ID                    string     `json:"id"`
	OrganizationID        string     `json:"organizationId"`
	ApplicationID         string     `json:"applicationId"`
	Branch                string     `json:"branch"`
	Kind                  string     `json:"kind"` // ENVIRONMENT_KIND_PRODUCTION / _PREVIEW
	PreviewSlug           string     `json:"previewSlug"`
	Namespace             string     `json:"namespace"`
	PlannedHostname       string     `json:"plannedHostname"`
	CurrentReleaseID      string     `json:"currentReleaseId"`
	InProgressReleaseID   string     `json:"inProgressReleaseId"`
	LifecycleStatus       string     `json:"lifecycleStatus"`
	LifecycleReason       string     `json:"lifecycleReason"`
	LifecycleErrorCode    string     `json:"lifecycleErrorCode"`
	LifecycleErrorMessage string     `json:"lifecycleErrorMessage"`
	CreatedAt             time.Time  `json:"createdAt"`
	TornDownAt            *time.Time `json:"tornDownAt"`
}

// Release mirrors the Release proto message.
type Release struct {
	ID                string    `json:"id"`
	OrganizationID    string    `json:"organizationId"`
	EnvironmentID     string    `json:"environmentId"`
	ImageRef          string    `json:"imageRef"`
	GitCommitSHA      string    `json:"gitCommitSha"`
	TriggeredByUserID string    `json:"triggeredByUserId"`
	Kind              string    `json:"kind"`   // RELEASE_KIND_DEPLOY / _ROLLBACK / _ENV_CHANGE
	Status            string    `json:"status"` // RELEASE_STATUS_LIVE etc.
	FailureReason     string    `json:"failureReason"`
	CreatedAt         time.Time `json:"createdAt"`
}

// EnvVar is a single environment variable entry.
type EnvVar struct {
	Key    string `json:"key"`
	Secret bool   `json:"secret"`
}

// EnvVarRevision mirrors the EnvironmentVariableRevision proto message.
type EnvVarRevision struct {
	ID              string    `json:"id"`
	EnvironmentID   string    `json:"environmentId"`
	Revision        int64     `json:"revision"`
	CreatedByUserID string    `json:"createdByUserId"`
	Variables       []EnvVar  `json:"variables"`
	CreatedAt       time.Time `json:"createdAt"`
}

// Alias mirrors the Alias proto message.
type Alias struct {
	ID            string    `json:"id"`
	EnvironmentID string    `json:"environmentId"`
	Hostname      string    `json:"hostname"`
	ReleaseID     string    `json:"releaseId"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Certificate mirrors the CertificateStatus proto message.
type Certificate struct {
	Hostname   string     `json:"hostname"`
	SecretName string     `json:"secretName"`
	Ready      bool       `json:"ready"`
	Reason     string     `json:"reason"`
	NotAfter   *time.Time `json:"notAfter"`
	Issuer     string     `json:"issuer"`
	DNSNames   []string   `json:"dnsNames"`
}

// Domain mirrors the Domain proto message.
type Domain struct {
	ID                 string             `json:"id"`
	ApplicationID      string             `json:"applicationId"`
	Hostname           string             `json:"hostname"`
	VerificationMethod string             `json:"verificationMethod"`
	VerificationToken  string             `json:"verificationToken"`
	VerifiedAt         *time.Time         `json:"verifiedAt"`
	CreatedAt          time.Time          `json:"createdAt"`
	Instructions       DomainInstructions `json:"instructions"`
}

// DomainInstructions mirrors DomainDnsInstructions.
type DomainInstructions struct {
	RecordType        string `json:"recordType"`
	RecordName        string `json:"recordName"`
	RecordValue       string `json:"recordValue"`
	ApexARecordTarget string `json:"apexARecordTarget"`
}

// GitConnection mirrors the GitConnection proto message.
type GitConnection struct {
	ID               string    `json:"id"`
	OrganizationID   string    `json:"organizationId"`
	Provider         string    `json:"provider"`
	InstallationID   int64     `json:"installationId"`
	RepoFullName     string    `json:"repoFullName"`
	DefaultBranch    string    `json:"defaultBranch"`
	WebhookSecretRef string    `json:"webhookSecretRef"`
	CreatedAt        time.Time `json:"createdAt"`
}

// Registry mirrors the Registry proto message.
type Registry struct {
	ID                  string    `json:"id"`
	OrganizationID      string    `json:"organizationId"`
	Provider            string    `json:"provider"`
	RegistryURL         string    `json:"registryUrl"`
	CredentialSecretRef string    `json:"credentialSecretRef"`
	CreatedAt           time.Time `json:"createdAt"`
}

// Webhook mirrors the WebhookEndpoint proto message.
type Webhook struct {
	ID                  string     `json:"id"`
	OrganizationID      string     `json:"organizationId"`
	TargetURL           string     `json:"targetUrl"`
	Events              []string   `json:"events"`
	SecretRef           string     `json:"secretRef"`
	Enabled             bool       `json:"enabled"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
	DisabledAt          *time.Time `json:"disabledAt"`
	CreatedAt           time.Time  `json:"createdAt"`
}

// APIToken mirrors the ApiToken proto message.
type APIToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"userId"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expiresAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

// AuditEvent mirrors the AuditEvent proto message.
type AuditEvent struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organizationId"`
	ActorUserID    string    `json:"actorUserId"`
	Action         string    `json:"action"`
	TargetType     string    `json:"targetType"`
	TargetID       string    `json:"targetId"`
	CreatedAt      time.Time `json:"createdAt"`
}

// ReconcileScope mirrors the ReconcileScope proto message.
type ReconcileScope struct {
	Kind           string `json:"kind"`
	OrganizationID string `json:"organizationId"`
	ApplicationID  string `json:"applicationId"`
	EnvironmentID  string `json:"environmentId"`
}

// ReconcileFinding mirrors the ReconcileFinding proto message.
type ReconcileFinding struct {
	Category     string    `json:"category"`
	Status       string    `json:"status"`
	ResourceName string    `json:"resourceName"`
	Summary      string    `json:"summary"`
	ObservedAt   time.Time `json:"observedAt"`
}

// ReconciliationRun mirrors the ReconciliationRun proto message.
type ReconciliationRun struct {
	ID           string             `json:"id"`
	Scope        ReconcileScope     `json:"scope"`
	Status       string             `json:"status"`
	CreatedAt    time.Time          `json:"createdAt"`
	CompletedAt  *time.Time         `json:"completedAt"`
	DryRun       bool               `json:"dryRun"`
	Prune        bool               `json:"prune"`
	Findings     []ReconcileFinding `json:"findings"`
	ErrorCode    string             `json:"errorCode"`
	ErrorMessage string             `json:"errorMessage"`
}

// Operation mirrors the Operation proto message.
type Operation struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	ResourceName string `json:"resourceName"`
}

// LogEntry mirrors the LogEntry proto message.
type LogEntry struct {
	Timestamp  time.Time         `json:"timestamp"`
	Severity   string            `json:"severity"`
	Body       string            `json:"body"`
	Attributes map[string]string `json:"attributes"`
}

// PageInfo carries pagination state for list views.
type PageInfo struct {
	NextPageToken string
	HasMore       bool
}

// FlashMessage is a transient UI notification passed via query param or context.
type FlashMessage struct {
	Kind    string // "success" | "error" | "info"
	Message string
}

// navItem groups navigation link metadata for the sidebar.
type navItem struct {
	Label  string
	Href   string
	Active bool
	Icon   string // heroicon name
}
