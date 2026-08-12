package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/shepard-labs/cyclopus/internal/avatar"
)

//go:embed static
var staticFS embed.FS

// StaticHandler serves files under internal/ui/static/ via embed.FS.
// Requests arrive as /static/<file>; the FS path is static/<file>.
func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("ui: embed static subtree: " + err.Error())
	}
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}

// AvatarHandler serves persisted avatar IDs and falls back to the default when
// an old or accidentally removed asset can no longer be resolved.
func AvatarHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSuffix(path.Base(r.URL.Path), ".svg")
		if !avatar.Valid(key) {
			key = avatar.DefaultKey
		}
		asset := "static/avatars/critters/" + key + ".svg"
		body, err := staticFS.ReadFile(asset)
		if err != nil && key != avatar.DefaultKey {
			body, err = staticFS.ReadFile("static/avatars/critters/" + avatar.DefaultKey + ".svg")
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(body)
	})
}
