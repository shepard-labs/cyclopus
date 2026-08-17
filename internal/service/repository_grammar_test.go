package service

import "strings"

type repositoryGrammarCase struct {
	name             string
	publicRepository string
	registryPath     string
	publicAccepted   bool
	registryAccepted bool
}

var repositoryGrammarCases = []repositoryGrammarCase{
	{name: "simple", publicRepository: "ghcr.io/acme/api", registryPath: "acme/api", publicAccepted: true, registryAccepted: true},
	{name: "one component", publicRepository: "ghcr.io/acme", registryPath: "acme", registryAccepted: true},
	{name: "dot separator", publicRepository: "ghcr.io/acme/api.v2", registryPath: "acme/api.v2", publicAccepted: true, registryAccepted: true},
	{name: "single underscore", publicRepository: "ghcr.io/acme/api_v2", registryPath: "acme/api_v2", publicAccepted: true, registryAccepted: true},
	{name: "double underscore", publicRepository: "ghcr.io/acme/api__v2", registryPath: "acme/api__v2", publicAccepted: true, registryAccepted: true},
	{name: "repeated hyphens", publicRepository: "ghcr.io/acme/api---v2", registryPath: "acme/api---v2", publicAccepted: true, registryAccepted: true},
	{name: "leading dot", publicRepository: "ghcr.io/acme/.api", registryPath: "acme/.api"},
	{name: "trailing dot", publicRepository: "ghcr.io/acme/api.", registryPath: "acme/api."},
	{name: "leading underscore", publicRepository: "ghcr.io/acme/_api", registryPath: "acme/_api"},
	{name: "trailing underscore", publicRepository: "ghcr.io/acme/api_", registryPath: "acme/api_"},
	{name: "leading hyphen", publicRepository: "ghcr.io/acme/-api", registryPath: "acme/-api"},
	{name: "trailing hyphen", publicRepository: "ghcr.io/acme/api-", registryPath: "acme/api-"},
	{name: "triple underscore", publicRepository: "ghcr.io/acme/api___v2", registryPath: "acme/api___v2"},
	{name: "empty path", publicRepository: "ghcr.io/", registryPath: ""},
	{name: "repeated slash", publicRepository: "ghcr.io/acme//api", registryPath: "acme//api"},
	{name: "uppercase", publicRepository: "ghcr.io/Acme/api", registryPath: "Acme/api"},
	{name: "unicode", publicRepository: "ghcr.io/acme/apé", registryPath: "acme/apé"},
	{name: "tag", publicRepository: "ghcr.io/acme/api:v2", registryPath: "acme/api:v2"},
	{name: "digest", publicRepository: "ghcr.io/acme/api@sha256:abc", registryPath: "acme/api@sha256:abc"},
	{name: "scheme", publicRepository: "https://ghcr.io/acme/api", registryPath: "https://ghcr.io/acme/api"},
	{name: "query", publicRepository: "ghcr.io/acme/api?q=1", registryPath: "acme/api?q=1"},
	{name: "fragment", publicRepository: "ghcr.io/acme/api#x", registryPath: "acme/api#x"},
	{name: "dot segment", publicRepository: "ghcr.io/acme/.", registryPath: "acme/."},
	{name: "dot dot segment", publicRepository: "ghcr.io/acme/..", registryPath: "acme/.."},
	{name: "host-prefixed registry path", publicRepository: "ghcr.io/acme/api", registryPath: "ghcr.io/acme/api", publicAccepted: true},
	{name: "public length 255", publicRepository: "ghcr.io/a/" + strings.Repeat("b", 245), registryPath: "a/" + strings.Repeat("b", 253), publicAccepted: true, registryAccepted: true},
	{name: "public length 256", publicRepository: "ghcr.io/a/" + strings.Repeat("b", 246), registryPath: "a/" + strings.Repeat("b", 254)},
}
