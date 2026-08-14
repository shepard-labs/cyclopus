package uihandler_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shepard-labs/cyclopus/internal/uihandler"
)

func TestPreviewRoutesReturn404(t *testing.T) {
	t.Setenv("CYCLOPUS_UI_PREVIEW", "1")
	handler := uihandler.New(nil)

	for _, path := range []string{"/__theme-preview", "/__theme-preview/flash"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusNotFound {
				t.Fatalf("GET %s status = %d, want %d", path, response.Code, http.StatusNotFound)
			}
		})
	}
}
