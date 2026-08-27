package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfiguration = `{
  "listen_address": ":8080",
  "database_path": "data/wormtamer.db",
  "review_workspace_path": "../wormtamer-reviews",
  "log_level": "info",
  "gitlab": {
    "base_url": "http://gitlab.internal",
    "webhook_secret": "secret",
    "personal_access_token": "gitlab-token"
  },
  "gemini": {
    "api_key": "gemini-key",
    "model": "gemini-test"
  },
  "authorized_repositories": ["group/project", "parent/team/project"]
}`

func TestLoad(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	if err := os.WriteFile(path, []byte(validConfiguration), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	wantDatabasePath := filepath.Join(directory, "data", "wormtamer.db")
	if cfg.DatabasePath != wantDatabasePath {
		t.Fatalf("DatabasePath = %q, want %q", cfg.DatabasePath, wantDatabasePath)
	}
	wantWorkspacePath := filepath.Join(filepath.Dir(directory), "wormtamer-reviews")
	if cfg.ReviewWorkspacePath != wantWorkspacePath {
		t.Fatalf("ReviewWorkspacePath = %q, want %q", cfg.ReviewWorkspacePath, wantWorkspacePath)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.Gemini.BaseURL != "" {
		t.Fatalf("Gemini.BaseURL = %q, want empty", cfg.Gemini.BaseURL)
	}
	if cfg.Gemini.ThinkingLevel != "default" {
		t.Fatalf("Gemini.ThinkingLevel = %q, want default", cfg.Gemini.ThinkingLevel)
	}
	if cfg.ShareAllAuthorizedRepositories {
		t.Fatal("ShareAllAuthorizedRepositories = true when omitted")
	}
}

func TestLoadCanonicalizesGeminiBaseURL(t *testing.T) {
	contents := strings.Replace(validConfiguration, `"model": "gemini-test"`, `"base_url": "https://GATEWAY.EXAMPLE:443/", "model": "gemini-test"`, 1)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Gemini.BaseURL != "https://gateway.example" {
		t.Fatalf("Gemini.BaseURL = %q, want https://gateway.example", cfg.Gemini.BaseURL)
	}
}

func TestLoadPassesThroughGeminiThinkingLevel(t *testing.T) {
	contents := strings.Replace(validConfiguration, `"model": "gemini-test"`, `"model": "gemini-test", "thinking_level": " max "`, 1)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Gemini.ThinkingLevel != "max" {
		t.Fatalf("Gemini.ThinkingLevel = %q, want max", cfg.Gemini.ThinkingLevel)
	}
}

func TestLoadShareAllAuthorizedRepositories(t *testing.T) {
	contents := strings.Replace(validConfiguration,
		`"authorized_repositories": ["group/project", "parent/team/project"]`,
		`"authorized_repositories": ["group/project", "parent/team/project", "group/third"],
  "share_all_authorized_repositories": true`, 1)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.ShareAllAuthorizedRepositories {
		t.Fatal("ShareAllAuthorizedRepositories = false")
	}
}

func TestCanonicalGitLabURL(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "http://gitlab.internal", want: "http://gitlab.internal"},
		{raw: "http://gitlab.internal/", want: "http://gitlab.internal"},
		{raw: "http://GITLAB.internal:80", want: "http://gitlab.internal"},
		{raw: "http://GITLAB.internal:080", want: "http://gitlab.internal"},
		{raw: "HTTPS://GITLAB.internal:443/gitlab/", want: "https://gitlab.internal/gitlab"},
		{raw: "https://GITLAB.internal:8443/gitlab", want: "https://gitlab.internal:8443/gitlab"},
		{raw: "http://[2001:db8::1]", want: "http://[2001:db8::1]"},
		{raw: "http://[2001:db8::1]:80/", want: "http://[2001:db8::1]"},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			got, err := canonicalGitLabURL(test.raw)
			if err != nil {
				t.Fatalf("canonicalGitLabURL() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("canonicalGitLabURL() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLoadDefaultsLogLevelToInfo(t *testing.T) {
	contents := strings.Replace(validConfiguration, "  \"log_level\": \"info\",\n", "", 1)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("LogLevel = %q, want info", cfg.LogLevel)
	}
}

func TestLoadUsesCanonicalGitLabURL(t *testing.T) {
	contents := strings.Replace(validConfiguration, "http://gitlab.internal", "http://GITLAB.internal:80/", 1)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.GitLab.BaseURL != "http://gitlab.internal" {
		t.Fatalf("GitLab.BaseURL = %q", cfg.GitLab.BaseURL)
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		replace string
		with    string
		want    string
	}{
		{name: "unknown field", replace: `"listen_address": ":8080",`, with: `"unknown": true, "listen_address": ":8080",`, want: "unknown field"},
		{name: "empty address", replace: `":8080"`, with: `""`, want: "listen_address is required"},
		{name: "invalid address", replace: `":8080"`, with: `"8080"`, want: "host:port"},
		{name: "empty database", replace: `"data/wormtamer.db"`, with: `""`, want: "database_path is required"},
		{name: "empty review workspace", replace: `"../wormtamer-reviews"`, with: `""`, want: "review_workspace_path is required"},
		{name: "workspace inside private database directory", replace: `"../wormtamer-reviews"`, with: `"data/reviews"`, want: "must be outside service-private"},
		{name: "workspace contains private directories", replace: `"../wormtamer-reviews"`, with: `".."`, want: "must be outside service-private"},
		{name: "invalid log level", replace: `"log_level": "info"`, with: `"log_level": "trace"`, want: "log_level must be"},
		{name: "invalid URL scheme", replace: `"http://gitlab.internal"`, with: `"ftp://gitlab.internal"`, want: "HTTP or HTTPS"},
		{name: "URL credentials", replace: `"http://gitlab.internal"`, with: `"http://user:pass@gitlab.internal"`, want: "must not contain credentials"},
		{name: "empty URL query", replace: `"http://gitlab.internal"`, with: `"http://gitlab.internal?"`, want: "must not contain credentials"},
		{name: "multi-colon URL authority", replace: `"http://gitlab.internal"`, with: `"http://gitlab.internal:80:80"`, want: "gitlab.base_url is invalid"},
		{name: "empty URL port", replace: `"http://gitlab.internal"`, with: `"http://gitlab.internal:"`, want: "invalid authority"},
		{name: "invalid IP literal", replace: `"http://gitlab.internal"`, with: `"http://[not-an-ip]"`, want: "gitlab.base_url is invalid"},
		{name: "empty webhook secret", replace: `"secret"`, with: `""`, want: "webhook_secret is required"},
		{name: "empty personal access token", replace: `"gitlab-token"`, with: `""`, want: "personal_access_token is required"},
		{name: "empty Gemini API key", replace: `"gemini-key"`, with: `""`, want: "gemini.api_key is required"},
		{name: "invalid Gemini base URL scheme", replace: `"api_key": "gemini-key",`, with: `"api_key": "gemini-key", "base_url": "ftp://gateway.example",`, want: "gemini.base_url must be an HTTP or HTTPS URL"},
		{name: "Gemini base URL credentials", replace: `"api_key": "gemini-key",`, with: `"api_key": "gemini-key", "base_url": "https://user:pass@gateway.example",`, want: "gemini.base_url must not contain credentials"},
		{name: "Gemini base URL query", replace: `"api_key": "gemini-key",`, with: `"api_key": "gemini-key", "base_url": "https://gateway.example?key=value",`, want: "gemini.base_url must not contain credentials"},
		{name: "empty Gemini model", replace: `"gemini-test"`, with: `""`, want: "gemini.model is required"},
		{name: "blank Gemini model", replace: `"gemini-test"`, with: `"  "`, want: "gemini.model is required"},
		{name: "long Gemini model", replace: `"gemini-test"`, with: `"` + strings.Repeat("m", 257) + `"`, want: "must not exceed 256 bytes"},
		{name: "Gemini model control character", replace: `"gemini-test"`, with: `"gemini\ntest"`, want: "must not contain control characters"},
		{name: "obsolete public sources", replace: `"authorized_repositories":`, with: `"public_sources": {}, "authorized_repositories":`, want: "unknown field"},
		{name: "removed repository sharing", replace: `"authorized_repositories":`, with: `"repository_sharing": {}, "authorized_repositories":`, want: `unknown field "repository_sharing"`},
		{name: "malformed repository", replace: `"group/project"`, with: `"group//project"`, want: "invalid authorized repository"},
		{name: "duplicate repository", replace: `"parent/team/project"`, with: `"group/project"`, want: "duplicate authorized repository"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contents := strings.Replace(validConfiguration, test.replace, test.with, 1)
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestLoadDoesNotExposeSecretInDecodeError(t *testing.T) {
	secret := "do-not-log-this-secret"
	contents := strings.Replace(validConfiguration, `"secret"`, `{"bad":"`+secret+`"}`, 1)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Load() error exposed secret: %v", err)
	}
}
