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
  "gitlab": {
    "base_url": "http://gitlab.internal",
    "webhook_secret": "secret"
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
	if cfg.ConfigFileBroadlyRead {
		t.Fatal("ConfigFileBroadlyRead = true for a 0600 file")
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

func TestLoadDetectsBroadReadPermission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(validConfiguration), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.ConfigFileBroadlyRead {
		t.Fatal("ConfigFileBroadlyRead = false for a 0644 file")
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
		{name: "invalid URL scheme", replace: `"http://gitlab.internal"`, with: `"ftp://gitlab.internal"`, want: "HTTP or HTTPS"},
		{name: "URL credentials", replace: `"http://gitlab.internal"`, with: `"http://user:pass@gitlab.internal"`, want: "must not contain credentials"},
		{name: "empty URL query", replace: `"http://gitlab.internal"`, with: `"http://gitlab.internal?"`, want: "must not contain credentials"},
		{name: "multi-colon URL authority", replace: `"http://gitlab.internal"`, with: `"http://gitlab.internal:80:80"`, want: "gitlab.base_url is invalid"},
		{name: "empty URL port", replace: `"http://gitlab.internal"`, with: `"http://gitlab.internal:"`, want: "invalid authority"},
		{name: "invalid IP literal", replace: `"http://gitlab.internal"`, with: `"http://[not-an-ip]"`, want: "gitlab.base_url is invalid"},
		{name: "empty secret", replace: `"secret"`, with: `""`, want: "webhook_secret is required"},
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
