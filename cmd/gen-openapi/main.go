// gen-openapi converts the buf-generated protoc-gen-openapiv2 (Swagger 2.0)
// document for cyclopus.proto into the canonical OpenAPI 3 document served
// publicly at /openapi.json and rendered at /docs.
//
// Usage:
//
//	go run ./cmd/gen-openapi gen/openapi/cyclopus/v1/cyclopus.swagger.json internal/apidocs/openapi.json
//
// The proto remains the single source of truth: this tool never edits paths or
// operations, it only up-converts the envelope and stamps presentation metadata.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
)

const (
	infoTitle       = "Cyclopus API"
	infoVersion     = "0.1.0"
	infoDescription = "Cyclopus is a self-hosted PaaS with hard workload isolation. This document describes the public REST API served at /v1/*; all operations except /v1/auth/bootstrap and /v1/auth/login require a Bearer credential."
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: gen-openapi <input.swagger.json> <output.openapi.json>")
		os.Exit(64)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fatal("read input: %v", err)
	}
	doc2 := &openapi2.T{}
	if err := doc2.UnmarshalJSON(raw); err != nil {
		fatal("parse swagger 2.0: %v", err)
	}
	doc3, err := openapi2conv.ToV3(doc2)
	if err != nil {
		fatal("convert to OpenAPI 3: %v", err)
	}

	// Presentation metadata. The proto carries no API-info annotation, so the
	// generator's placeholders are replaced wholesale.
	doc3.Info = &openapi3.Info{Title: infoTitle, Version: infoVersion, Description: infoDescription}
	doc3.Info.Extensions = map[string]any{}
	doc3.Servers = openapi3.Servers{{URL: "/"}}
	if doc3.Components == nil {
		doc3.Components = &openapi3.Components{}
	}
	if doc3.Components.SecuritySchemes == nil {
		doc3.Components.SecuritySchemes = openapi3.SecuritySchemes{}
	}
	bearer := openapi3.NewSecurityScheme()
	bearer.Type = "http"
	bearer.Scheme = "bearer"
	bearer.Description = "Session, API, or deployment token (cyp_v1_...)"
	doc3.Components.SecuritySchemes["bearerToken"] = &openapi3.SecuritySchemeRef{Value: bearer}

	out, err := json.MarshalIndent(doc3, "", "  ")
	if err != nil {
		fatal("marshal: %v", err)
	}
	out = append(out, '\n')
	if err := os.MkdirAll(dirOf(os.Args[2]), 0o755); err != nil {
		fatal("mkdir: %v", err)
	}
	if err := os.WriteFile(os.Args[2], out, 0o644); err != nil {
		fatal("write output: %v", err)
	}
	fmt.Printf("gen-openapi: wrote %s (%d bytes, %d paths)\n", os.Args[2], len(out), len(doc3.Paths.Map()))
}

func dirOf(path string) string {
	if i := len(path) - 1; i >= 0 {
		for j := i; j >= 0; j-- {
			if path[j] == '/' {
				return path[:j]
			}
		}
	}
	return "."
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gen-openapi: "+format+"\n", args...)
	os.Exit(1)
}
