package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var repositoryPathPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+$`)

type Config struct {
	ListenAddress          string              `json:"listen_address"`
	DatabasePath           string              `json:"database_path"`
	LogLevel               string              `json:"log_level"`
	GitLab                 GitLab              `json:"gitlab"`
	Gemini                 Gemini              `json:"gemini"`
	PublicSources          PublicSources       `json:"public_sources"`
	AuthorizedRepositories []string            `json:"authorized_repositories"`
	RepositorySharing      map[string][]string `json:"repository_sharing"`
	ConfigFileBroadlyRead  bool                `json:"-"`
}

type GitLab struct {
	BaseURL             string `json:"base_url"`
	WebhookSecret       string `json:"webhook_secret"`
	PersonalAccessToken string `json:"personal_access_token"`
}

type Gemini struct {
	APIKey string `json:"api_key"`
	Model  string `json:"model"`
}

type PublicSources struct {
	AllowedDomains     []string `json:"allowed_domains"`
	GitHubRepositories []string `json:"github_repositories"`
}

func Load(path string) (Config, error) {
	if path == "" {
		return Config{}, errors.New("configuration path is required")
	}

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, errors.New("resolve configuration path")
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		return Config{}, fmt.Errorf("open configuration: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return Config{}, fmt.Errorf("inspect configuration: %w", err)
	}

	var cfg Config
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return Config{}, err
	}
	if err := validate(&cfg); err != nil {
		return Config{}, err
	}

	if !filepath.IsAbs(cfg.DatabasePath) {
		cfg.DatabasePath = filepath.Join(filepath.Dir(absolutePath), cfg.DatabasePath)
	}
	cfg.DatabasePath = filepath.Clean(cfg.DatabasePath)
	cfg.ConfigFileBroadlyRead = info.Mode().Perm()&0o044 != 0
	return cfg, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	return errors.New("decode configuration: multiple JSON values")
}

func validate(cfg *Config) error {
	if strings.TrimSpace(cfg.ListenAddress) == "" {
		return errors.New("listen_address is required")
	}
	if err := validateListenAddress(cfg.ListenAddress); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.DatabasePath) == "" {
		return errors.New("database_path is required")
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return errors.New("log_level must be debug, info, warn, or error")
	}
	if strings.TrimSpace(cfg.GitLab.BaseURL) == "" {
		return errors.New("gitlab.base_url is required")
	}
	canonicalBaseURL, err := canonicalGitLabURL(cfg.GitLab.BaseURL)
	if err != nil {
		return err
	}
	cfg.GitLab.BaseURL = canonicalBaseURL
	if cfg.GitLab.WebhookSecret == "" {
		return errors.New("gitlab.webhook_secret is required")
	}
	if cfg.GitLab.PersonalAccessToken == "" {
		return errors.New("gitlab.personal_access_token is required")
	}
	if cfg.Gemini.APIKey == "" {
		return errors.New("gemini.api_key is required")
	}
	if strings.TrimSpace(cfg.Gemini.Model) == "" {
		return errors.New("gemini.model is required")
	}
	if err := validatePublicSources(&cfg.PublicSources); err != nil {
		return err
	}
	if len(cfg.AuthorizedRepositories) == 0 {
		return errors.New("authorized_repositories is required")
	}

	authorized := make(map[string]struct{}, len(cfg.AuthorizedRepositories))
	for _, repository := range cfg.AuthorizedRepositories {
		if !validRepositoryPath(repository) {
			return fmt.Errorf("invalid authorized repository path %q", repository)
		}
		if _, exists := authorized[repository]; exists {
			return fmt.Errorf("duplicate authorized repository %q", repository)
		}
		authorized[repository] = struct{}{}
	}
	for target, relatedRepositories := range cfg.RepositorySharing {
		if _, allowed := authorized[target]; !allowed {
			return fmt.Errorf("repository sharing target %q is not authorized", target)
		}
		if len(relatedRepositories) == 0 {
			return fmt.Errorf("repository sharing target %q has no related repositories", target)
		}
		seen := make(map[string]struct{}, len(relatedRepositories))
		for _, related := range relatedRepositories {
			if _, allowed := authorized[related]; !allowed {
				return fmt.Errorf("shared repository %q is not authorized", related)
			}
			if related == target {
				return fmt.Errorf("repository sharing target %q includes itself", target)
			}
			if _, exists := seen[related]; exists {
				return fmt.Errorf("duplicate shared repository %q for target %q", related, target)
			}
			seen[related] = struct{}{}
		}
	}
	return nil
}

func validatePublicSources(sources *PublicSources) error {
	if len(sources.AllowedDomains) == 0 {
		return errors.New("public_sources.allowed_domains is required")
	}
	seenDomains := make(map[string]struct{}, len(sources.AllowedDomains))
	for index, domain := range sources.AllowedDomains {
		canonical := strings.ToLower(domain)
		if domain == "" || strings.TrimSpace(domain) != domain || !validDomain(canonical) {
			return fmt.Errorf("invalid public source domain %q", domain)
		}
		if _, exists := seenDomains[canonical]; exists {
			return fmt.Errorf("duplicate public source domain %q", domain)
		}
		seenDomains[canonical] = struct{}{}
		sources.AllowedDomains[index] = canonical
	}
	if _, exists := seenDomains["github.com"]; !exists {
		return errors.New("public_sources.allowed_domains must include github.com")
	}

	seenRepositories := make(map[string]struct{}, len(sources.GitHubRepositories))
	for _, repositorySlug := range sources.GitHubRepositories {
		if !validGitHubRepositorySlug(repositorySlug) {
			return fmt.Errorf("invalid public GitHub repository slug %q", repositorySlug)
		}
		key := strings.ToLower(repositorySlug)
		if _, exists := seenRepositories[key]; exists {
			return fmt.Errorf("duplicate public GitHub repository %q", repositorySlug)
		}
		seenRepositories[key] = struct{}{}
	}
	return nil
}

func validDomain(domain string) bool {
	if len(domain) == 0 || len(domain) > 253 || net.ParseIP(domain) != nil {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

func validGitHubRepositorySlug(slug string) bool {
	parts := strings.Split(slug, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > 100 {
			return false
		}
		for _, character := range part {
			if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') && character != '-' && character != '_' && character != '.' {
				return false
			}
		}
	}
	return true
}

func validateListenAddress(address string) error {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("listen_address must be a host:port address")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return errors.New("listen_address has an invalid port")
	}
	return nil
}

func canonicalGitLabURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("gitlab.base_url is invalid")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Hostname() == "" {
		return "", errors.New("gitlab.base_url must be an HTTP or HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("gitlab.base_url must not contain credentials, a query, or a fragment")
	}

	authority := parsed.Host
	if strings.HasPrefix(authority, "[") {
		address := parsed.Hostname()
		if !strings.HasSuffix(authority, "]") {
			host, authorityPort, err := net.SplitHostPort(authority)
			if err != nil || authorityPort == "" {
				return "", errors.New("gitlab.base_url has an invalid authority")
			}
			address = host
		}
		if index := strings.LastIndexByte(address, '%'); index >= 0 {
			address = address[:index]
		}
		if net.ParseIP(address) == nil {
			return "", errors.New("gitlab.base_url has an invalid authority")
		}
	} else if strings.Contains(authority, ":") {
		host, authorityPort, err := net.SplitHostPort(authority)
		if err != nil || host == "" || authorityPort == "" {
			return "", errors.New("gitlab.base_url has an invalid authority")
		}
	}

	port := parsed.Port()
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", errors.New("gitlab.base_url has an invalid port")
		}
		if (parsed.Scheme == "http" && portNumber == 80) || (parsed.Scheme == "https" && portNumber == 443) {
			port = ""
		} else {
			port = strconv.Itoa(portNumber)
		}
	}
	hostname := strings.ToLower(parsed.Hostname())
	if port != "" {
		parsed.Host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		parsed.Host = "[" + hostname + "]"
	} else {
		parsed.Host = hostname
	}

	if parsed.Path == "/" {
		parsed.Path = ""
		parsed.RawPath = ""
	} else if strings.HasSuffix(parsed.Path, "/") {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
		parsed.RawPath = strings.TrimSuffix(parsed.RawPath, "/")
	}
	return parsed.String(), nil
}

func validRepositoryPath(path string) bool {
	if !repositoryPathPattern.MatchString(path) {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
