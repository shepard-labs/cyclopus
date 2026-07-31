package service

import (
	"context"
	"strings"

	"github.com/cyclopus-dev/cyclopus/internal/store"
)

// CreateProductionEnvironmentInput holds the caller-supplied parameters for
// provisioning a production environment. Hostname is optional: when empty the
// platform auto-generates it from the app's hostname_label and the configured
// preview_hostname_suffix. When provided it is used as-is and takes
// precedence, which is how operators later attach their own domain.
type CreateProductionEnvironmentInput struct {
	AppID    string
	Branch   string // optional; defaults to "main" when empty
	Hostname string // optional; auto-generated when empty
}

// CreateProductionEnvironment creates a production environment for the given
// app. The environment is inserted as lifecycle_status='ready' with its
// planned_hostname already set, bypassing the async lifecycle provisioner
// used for preview environments. This is the first-class API path for
// production environment creation (§4.1, §7.5).
func (s *Service) CreateProductionEnvironment(ctx context.Context, actor ActorContext, key string, input CreateProductionEnvironmentInput) (store.Environment, error) {
	if err := validateActor(actor); err != nil {
		return store.Environment{}, err
	}
	app, err := s.store.GetApp(ctx, actor.OrgID, input.AppID)
	if err != nil {
		return store.Environment{}, mapError(err)
	}

	branch := strings.TrimSpace(input.Branch)
	if branch == "" {
		branch = "main"
	}

	hostname := strings.TrimSpace(input.Hostname)
	if hostname == "" {
		hostname, err = productionHostname(app.HostnameLabel, s.hostnameProvider)
		if err != nil {
			return store.Environment{}, err
		}
	}

	namespace := app.HostnameLabel + "-production"

	request := struct {
		AppID  string
		Branch string
	}{input.AppID, branch}
	var replay store.Environment
	claimed, err := s.claim(ctx, actor, "production_environment.create", key, request, &replay)
	if err != nil || !claimed {
		return replay, err
	}

	value, err := s.store.CreateProductionEnvironment(ctx, store.CreateProductionEnvironmentParams{
		ID:        newID(),
		OrgID:     actor.OrgID,
		AppID:     input.AppID,
		Branch:    branch,
		Namespace: namespace,
		Hostname:  hostname,
		Audit:     audit(actor),
	})
	return value, mapError(err)
}
