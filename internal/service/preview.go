package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/google/uuid"
)

var previewAdjectives = []string{"amber", "brisk", "calm", "clear", "coral", "eager", "frost", "gentle", "lunar", "quiet", "rapid", "solar"}
var previewNouns = []string{"badger", "cedar", "falcon", "harbor", "heron", "maple", "otter", "panda", "river", "spruce", "tiger", "willow"}

type PreviewSlugGenerator interface{ Generate() (string, error) }

type randomPreviewSlugGenerator struct{ source io.Reader }

func (g randomPreviewSlugGenerator) Generate() (string, error) {
	var data [8]byte
	if _, err := io.ReadFull(g.source, data[:]); err != nil {
		return "", err
	}
	adjective := previewAdjectives[int(data[0])%len(previewAdjectives)]
	noun := previewNouns[int(data[1])%len(previewNouns)]
	value := uint64(data[2])<<40 | uint64(data[3])<<32 | uint64(data[4])<<24 | uint64(data[5])<<16 | uint64(data[6])<<8 | uint64(data[7])
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	suffix := []byte{'0', '0', '0', '0', '0', '0'}
	for index := len(suffix) - 1; index >= 0; index-- {
		suffix[index] = alphabet[value%36]
		value /= 36
	}
	return adjective + "-" + noun + "-" + string(suffix), nil
}

func (s *Service) SetPreviewSlugGenerator(generator PreviewSlugGenerator) {
	if generator != nil {
		s.previewSlugs = generator
	}
}

type PreviewPushInput struct{ AppID, Branch, CommitSHA string }

func (s *Service) AcceptPreviewPush(ctx context.Context, actor ActorContext, key string, input PreviewPushInput) (store.PreviewPushResult, error) {
	if err := validateActor(actor); err != nil {
		return store.PreviewPushResult{}, err
	}
	if strings.TrimSpace(key) == "" || strings.TrimSpace(input.AppID) == "" || strings.TrimSpace(input.Branch) == "" || !commitPattern.MatchString(input.CommitSHA) {
		return store.PreviewPushResult{}, ErrInvalidArgument
	}
	identities := make([]store.PreviewIdentity, 0, store.PreviewIdentityAttempts)
	for range store.PreviewIdentityAttempts {
		slug, err := s.previewSlugs.Generate()
		if err != nil || !slugPattern.MatchString(slug) || len("preview-"+slug) > 63 {
			return store.PreviewPushResult{}, fmt.Errorf("generate preview identity: %w", err)
		}
		identities = append(identities, store.PreviewIdentity{EnvironmentID: newID(), Slug: slug, Namespace: "preview-" + slug})
	}
	jobID, err := uuid.NewV7()
	if err != nil {
		return store.PreviewPushResult{}, err
	}
	result, err := s.store.AcceptPreviewPush(ctx, store.PreviewPushParams{OrgID: actor.OrgID, AppID: input.AppID,
		Branch: input.Branch, CommitSHA: input.CommitSHA, IdempotencyKey: key,
		OrchestrationJobID: jobID.String(), Identities: identities, Audit: audit(actor)})
	if errors.Is(err, store.ErrPreviewCapReached) {
		s.log().Warn("preview.cap_reached", "event", "preview.cap_reached",
			"org_id", actor.OrgID, "app_id", input.AppID)
	}
	return result, mapError(err)
}

func (s *Service) RequestPreviewBranchDelete(ctx context.Context, actor ActorContext, key, appID, branch string) (store.Environment, error) {
	return s.requestPreviewTeardown(ctx, actor, key, store.PreviewTeardownParams{OrgID: actor.OrgID,
		AppID: appID, Branch: branch, IdempotencyKey: key, EventKind: "delete", Reason: "branch_deleted", Audit: audit(actor)})
}

func (s *Service) RequestEnvironmentTeardown(ctx context.Context, actor ActorContext, key, environmentID, reason string) (store.Environment, error) {
	if err := validateActor(actor); err != nil {
		return store.Environment{}, err
	}
	environment, err := s.store.GetEnvironment(ctx, actor.OrgID, environmentID)
	if err != nil {
		return store.Environment{}, mapError(err)
	}
	return s.requestPreviewTeardown(ctx, actor, key, store.PreviewTeardownParams{OrgID: actor.OrgID,
		AppID: environment.AppID, Branch: environment.Branch, EnvironmentID: environmentID,
		IdempotencyKey: key, EventKind: "teardown", Reason: reason, Audit: audit(actor)})
}

func (s *Service) requestPreviewTeardown(ctx context.Context, actor ActorContext, key string, params store.PreviewTeardownParams) (store.Environment, error) {
	if err := validateActor(actor); err != nil {
		return store.Environment{}, err
	}
	if strings.TrimSpace(key) == "" || strings.TrimSpace(params.AppID) == "" || strings.TrimSpace(params.Branch) == "" || strings.TrimSpace(params.Reason) == "" {
		return store.Environment{}, ErrInvalidArgument
	}
	value, err := s.store.RequestPreviewTeardown(ctx, params)
	return value, mapError(err)
}
