package service

import (
	"strings"
	"testing"
	"time"
)

func TestGitHubCursorIsSignedAndScopeBound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := &Service{githubCursorKey: []byte(strings.Repeat("k", 32)), now: func() time.Time { return now }}
	token := s.newGitHubCursor("repositories", "org", "installation", "installation", "generation", 2, 50)
	if page, err := s.decodeGitHubCursor(token, "repositories", "org", "installation", "installation", "generation", 50); err != nil || page != 2 {
		t.Fatalf("decode = %d, %v", page, err)
	}
	tamperedByte := "A"
	if token[len(token)/2] == 'A' {
		tamperedByte = "B"
	}
	for name, value := range map[string]string{"tampered": token[:len(token)/2] + tamperedByte + token[len(token)/2+1:], "cross org": token, "cross scope": token} {
		t.Run(name, func(t *testing.T) {
			org, scope := "org", "installation"
			if name == "cross org" {
				org = "other"
			}
			if name == "cross scope" {
				scope = "other"
			}
			if _, err := s.decodeGitHubCursor(value, "repositories", org, scope, "installation", "generation", 50); err == nil {
				t.Fatal("cursor unexpectedly accepted")
			}
		})
	}
	for name, decode := range map[string]func() error{
		"page size": func() error {
			_, err := s.decodeGitHubCursor(token, "repositories", "org", "installation", "installation", "generation", 100)
			return err
		},
		"installation": func() error {
			_, err := s.decodeGitHubCursor(token, "repositories", "org", "installation", "other", "generation", 50)
			return err
		},
		"generation": func() error {
			_, err := s.decodeGitHubCursor(token, "repositories", "org", "installation", "installation", "new", 50)
			return err
		},
		"expired": func() error {
			s.now = func() time.Time { return now.Add(githubCursorTTL) }
			_, err := s.decodeGitHubCursor(token, "repositories", "org", "installation", "installation", "generation", 50)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			s.now = func() time.Time { return now }
			if err := decode(); err == nil {
				t.Fatal("cursor unexpectedly accepted")
			}
		})
	}
}
