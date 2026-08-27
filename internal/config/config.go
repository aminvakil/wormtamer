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
	ListenAddress                  string   `json:"listen_address"`
	DatabasePath                   string   `json:"database_path"`
	ReviewWorkspacePath            string   `json:"review_workspace_path"`
	LogLevel                       string   `json:"log_level"`
	GitLab                         GitLab   `json:"gitlab"`
	Gemini                         Gemini   `json:"gemini"`
	AuthorizedRepositories         []string `json:"authorized_repositories"`
	ShareAllAuthorizedRepositories bool     `json:"share_all_authorized_repositories"`
	ConfigPath                     string   `json:"-"`
}

type GitLab struct {
	BaseURL             string `json:"base_url"`
	WebhookSecret       string `json:"webhook_secret"`
	PersonalAccessToken string `json:"personal_access_token"`
}

type Gemini struct {
	APIKey        string `json:"api_key"`
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	ThinkingLevel string `json:"thinking_level"`
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
	if !filepath.IsAbs(cfg.ReviewWorkspacePath) {
		cfg.ReviewWorkspacePath = filepath.Join(filepath.Dir(absolutePath), cfg.ReviewWorkspacePath)
	}
	cfg.ReviewWorkspacePath = filepath.Clean(cfg.ReviewWorkspacePath)
	for _, privateDirectory := range []string{filepath.Dir(absolutePath), filepath.Dir(cfg.DatabasePath)} {
		if pathWithin(privateDirectory, cfg.ReviewWorkspacePath) || pathWithin(cfg.ReviewWorkspacePath, privateDirectory) {
			return Config{}, errors.New("review_workspace_path must be outside service-private configuration and database directories and must not contain them")
		}
	}
	cfg.ConfigPath = absolutePath
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
	if strings.TrimSpace(cfg.ReviewWorkspacePath) == "" {
		return errors.New("review_workspace_path is required")
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
	if cfg.Gemini.BaseURL != "" {
		canonicalBaseURL, err := canonicalHTTPBaseURL(cfg.Gemini.BaseURL, "gemini.base_url")
		if err != nil {
			return err
		}
		cfg.Gemini.BaseURL = canonicalBaseURL
	}
	if strings.TrimSpace(cfg.Gemini.Model) == "" {
		return errors.New("gemini.model is required")
	}
	if len(cfg.Gemini.Model) > 256 {
		return errors.New("gemini.model must not exceed 256 bytes")
	}
	for _, character := range cfg.Gemini.Model {
		if character < 0x20 || character == 0x7f {
			return errors.New("gemini.model must not contain control characters")
		}
	}
	cfg.Gemini.ThinkingLevel = strings.TrimSpace(cfg.Gemini.ThinkingLevel)
	if cfg.Gemini.ThinkingLevel == "" {
		cfg.Gemini.ThinkingLevel = "default"
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
	return nil
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
	return canonicalHTTPBaseURL(raw, "gitlab.base_url")
}

func canonicalHTTPBaseURL(raw, field string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s is invalid", field)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("%s must be an HTTP or HTTPS URL", field)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("%s must not contain credentials, a query, or a fragment", field)
	}

	authority := parsed.Host
	if strings.HasPrefix(authority, "[") {
		address := parsed.Hostname()
		if !strings.HasSuffix(authority, "]") {
			host, authorityPort, err := net.SplitHostPort(authority)
			if err != nil || authorityPort == "" {
				return "", fmt.Errorf("%s has an invalid authority", field)
			}
			address = host
		}
		if index := strings.LastIndexByte(address, '%'); index >= 0 {
			address = address[:index]
		}
		if net.ParseIP(address) == nil {
			return "", fmt.Errorf("%s has an invalid authority", field)
		}
	} else if strings.Contains(authority, ":") {
		host, authorityPort, err := net.SplitHostPort(authority)
		if err != nil || host == "" || authorityPort == "" {
			return "", fmt.Errorf("%s has an invalid authority", field)
		}
	}

	port := parsed.Port()
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", fmt.Errorf("%s has an invalid port", field)
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

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
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
