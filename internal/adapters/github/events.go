package github

import (
	"context"
	"net/http"
	"regexp"

	gh "github.com/google/go-github/v88/github"
)

var branchRefSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

type EventInstallation struct {
	ID                  int64
	AccountID           int64
	AccountLogin        string
	AccountType         string
	RepositorySelection string
	Suspended           bool
}

type EventRepository struct {
	ID, InstallationID            int64
	NodeID, Owner, Name, FullName string
	Visibility, DefaultBranch     string
	Archived                      bool
}

type EventClient struct {
	clients eventClientFactory
}

type eventClientFactory interface {
	AppClient(context.Context) (*gh.Client, error)
	Client(int64) (*gh.Client, error)
	Refresh(int64)
}

func NewEventClient(clients *InstallationClientFactory) *EventClient {
	return &EventClient{clients: clients}
}

func (c *EventClient) GetInstallation(ctx context.Context, installationID int64) (EventInstallation, error) {
	client, err := c.clients.AppClient(ctx)
	if err != nil {
		return EventInstallation{}, classifyError("get installation", err)
	}
	value, _, err := client.Apps.GetInstallation(ctx, installationID)
	if err != nil {
		return EventInstallation{}, classifyError("get installation", err)
	}
	account := value.GetAccount()
	return EventInstallation{ID: value.GetID(), AccountID: account.GetID(), AccountLogin: account.GetLogin(),
		AccountType: account.GetType(), RepositorySelection: value.GetRepositorySelection(), Suspended: value.SuspendedAt != nil}, nil
}

func (c *EventClient) ListInstallationRepositories(ctx context.Context, installationID int64) ([]EventRepository, error) {
	client, err := c.clients.Client(installationID)
	if err != nil {
		return nil, classifyError("list installation repositories", err)
	}
	var out []EventRepository
	for page := 1; ; page++ {
		values, response, err := client.Apps.ListRepos(ctx, &gh.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, classifyError("list installation repositories", err)
		}
		for _, repository := range values.Repositories {
			out = append(out, eventRepository(installationID, repository))
		}
		if response == nil || response.NextPage == 0 {
			return out, nil
		}
		page = response.NextPage - 1
	}
}

func (c *EventClient) GetRepository(ctx context.Context, installationID, repositoryID int64) (EventRepository, error) {
	client, err := c.clients.Client(installationID)
	if err != nil {
		return EventRepository{}, classifyError("get repository", err)
	}
	value, _, err := client.Repositories.GetByID(ctx, repositoryID)
	if err != nil {
		return EventRepository{}, classifyError("get repository", err)
	}
	return eventRepository(installationID, value), nil
}

func (c *EventClient) GetBranchRef(ctx context.Context, installationID, repositoryID int64, branch string) (string, bool, error) {
	client, err := c.clients.Client(installationID)
	if err != nil {
		return "", false, withAccessScope(classifyError("get branch ref", err), "installation")
	}
	repository, _, err := client.Repositories.GetByID(ctx, repositoryID)
	if err != nil {
		return "", false, withAccessScope(classifyError("resolve repository", err), "repository")
	}
	owner, name := repository.GetOwner().GetLogin(), repository.GetName()
	if owner == "" || name == "" {
		return "", false, &PermanentJobError{Code: "malformed_response", Err: errMalformedResponse}
	}
	ref, response, err := client.Git.GetRef(ctx, owner, name, "heads/"+branch)
	if err != nil {
		if response != nil && response.StatusCode == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, withAccessScope(classifyError("get branch ref", err), "repository")
	}
	if !branchRefSHA.MatchString(ref.GetObject().GetSHA()) {
		return "", false, &PermanentJobError{Code: "malformed_response", Err: errMalformedResponse}
	}
	return ref.GetObject().GetSHA(), true, nil
}

func withAccessScope(err error, scope string) error {
	if api, ok := err.(*APIError); ok {
		api.AccessScope = scope
	}
	return err
}

func (c *EventClient) RefreshInstallationCredentials(_ context.Context, installationID int64) error {
	c.clients.Refresh(installationID)
	return nil
}

func eventRepository(installationID int64, value *gh.Repository) EventRepository {
	visibility := value.GetVisibility()
	if visibility == "" {
		if value.GetPrivate() {
			visibility = "private"
		} else {
			visibility = "public"
		}
	}
	return EventRepository{ID: value.GetID(), InstallationID: installationID, NodeID: value.GetNodeID(),
		Owner: value.GetOwner().GetLogin(), Name: value.GetName(), FullName: value.GetFullName(), Visibility: visibility,
		DefaultBranch: value.GetDefaultBranch(), Archived: value.GetArchived()}
}
