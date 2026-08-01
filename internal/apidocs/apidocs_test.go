package apidocs

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSpecHandlerServesEmbeddedOpenAPI3(t *testing.T) {
	recorder := httptest.NewRecorder()
	SpecHandler().ServeHTTP(recorder, httptest.NewRequest("GET", "/openapi.json", nil))
	if recorder.Code != 200 {
		t.Fatalf("status = %d", recorder.Code)
	}
	if ct := recorder.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title string `json:"title"`
		} `json:"info"`
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") || doc.Info.Title != "Cyclopus API" || len(doc.Paths) == 0 {
		t.Fatalf("unexpected document: openapi=%q title=%q paths=%d", doc.OpenAPI, doc.Info.Title, len(doc.Paths))
	}
}

func TestDocsHandlerReferencesSpec(t *testing.T) {
	recorder := httptest.NewRecorder()
	DocsHandler().ServeHTTP(recorder, httptest.NewRequest("GET", "/docs", nil))
	body := recorder.Body.String()
	if recorder.Code != 200 || !strings.Contains(body, `data-url="/openapi.json"`) || !strings.Contains(body, "scalar-api-reference") {
		t.Fatalf("docs page = %d %q...", recorder.Code, body[:120])
	}
}
