package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	gh "github.com/google/go-github/v88/github"
)

func TestSelectProductionBranchPriorityExplicitAndMissing(t *testing.T) {
	branches := map[string]string{"main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "master": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "release": "cccccccccccccccccccccccccccccccccccccccc"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var branch string
		_, _ = fmt.Sscanf(r.URL.Path, "/repos/acme/api/branches/%s", &branch)
		sha, ok := branches[branch]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"name":%q,"commit":{"sha":%q}}`, branch, sha)
	}))
	defer server.Close()
	base := server.URL + "/"
	client, err := gh.NewClient(gh.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, explicit, defaultBranch, want string }{
		{"main", "", "release", "main"}, {"master", "", "release", "master"}, {"default", "", "release", "release"}, {"explicit", "refs/heads/release", "main", "release"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current := mapsClone(branches)
			if test.name == "master" {
				delete(branches, "main")
			}
			if test.name == "default" {
				delete(branches, "main")
				delete(branches, "master")
			}
			t.Cleanup(func() { branches = current })
			branch, sha, err := selectProductionBranch(context.Background(), client, "acme", "api", test.defaultBranch, test.explicit)
			if err != nil || branch != test.want || sha != branches[test.want] {
				t.Fatalf("selection = %q %q %v", branch, sha, err)
			}
		})
	}
	delete(branches, "main")
	delete(branches, "master")
	delete(branches, "release")
	if _, _, err := selectProductionBranch(context.Background(), client, "acme", "api", "release", "missing"); !errors.Is(err, ErrGitHubRepositoryRemoved) {
		t.Fatalf("missing error = %v", err)
	}
}

func mapsClone(values map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range values {
		out[key] = value
	}
	return out
}
