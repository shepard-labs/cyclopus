// Package apidocs publishes Cyclopus's canonical API documentation
//
//	GET /openapi.json  — the OpenAPI 3 document (generated from cyclopus.proto)
//	GET /docs          — a Scalar API-reference UI rendering that document
//
// The document is generated (buf: protoc-gen-openapiv2 → Swagger 2.0, then
// cmd/gen-openapi → OpenAPI 3), so the proto remains the single source of
// truth. Both routes are public: they describe the API, they cannot call it —
// every operation except auth bootstrap/login still requires a Bearer token.
package apidocs

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var specJSON []byte

// SpecHandler serves the canonical OpenAPI 3 document for the REST API.
func SpecHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(specJSON)
	})
}

// docsPage renders the spec with the vendored Scalar API-reference asset,
// consistent with the rest of the UI's no-CDN statics (htmx, app.css).
const docsPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1.0"/>
  <title>Cyclopus API — Reference</title>
  <style>body{margin:0}</style>
</head>
<body>
  <script id="api-reference" data-url="/openapi.json"></script>
  <script src="/static/scalar-api-reference.js"></script>
</body>
</html>
`

// DocsHandler serves the browsable API reference for /openapi.json.
func DocsHandler() http.Handler {
	page := []byte(docsPage)
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
}
