package auth

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

const deployAppScopePrefix = "deploy:app:"

var ErrInvalidScope = errors.New("auth: invalid scope")

func DeployAppScope(appID string) (string, error) {
	id, err := uuid.Parse(appID)
	if err != nil || id.String() != appID {
		return "", ErrInvalidScope
	}
	return deployAppScopePrefix + appID, nil
}

func ParseScope(scope string) (string, error) {
	if !strings.HasPrefix(scope, deployAppScopePrefix) {
		return "", ErrInvalidScope
	}
	appID := strings.TrimPrefix(scope, deployAppScopePrefix)
	canonical, err := DeployAppScope(appID)
	if err != nil || canonical != scope {
		return "", ErrInvalidScope
	}
	return appID, nil
}

func ValidateScopes(scopes []string) error {
	if len(scopes) == 0 || len(scopes) > 64 {
		return ErrInvalidScope
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if _, err := ParseScope(scope); err != nil {
			return err
		}
		if _, exists := seen[scope]; exists {
			return ErrInvalidScope
		}
		seen[scope] = struct{}{}
	}
	return nil
}
