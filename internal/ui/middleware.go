package ui

import (
	"context"
	"net/http"
)

type contextKey string

const tokenContextKey contextKey = "ui_token"

// AuthMiddleware checks for a session cookie. Missing → redirect to /login.
// Present → injects the token into the request context.
func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := GetSession(r)
		if !ok || token == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), tokenContextKey, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// tokenFromContext extracts the Bearer token injected by authMiddleware.
func TokenFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tokenContextKey).(string)
	return v
}
