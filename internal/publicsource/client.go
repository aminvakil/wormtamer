package publicsource

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aminvakil/wormtamer/internal/failure"
)

const (
	ToolFetchURL      = "fetch_public_url"
	ToolListFiles     = "list_public_repository_files"
	ToolReadFile      = "read_public_repository_file"
	MaxToolCalls      = 8
	MaxToolResponse   = 64 << 10
	requestTimeout    = 10 * time.Second
	responseBodyLimit = 48 << 10
	metadataBodyLimit = 256 << 10
	archiveBodyLimit  = 32 << 20
	maxRedirects      = 5
	maxURLBytes       = 2048
)

var (
	githubRevisionPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	globalIPv6Prefix      = netip.MustParsePrefix("2000::/3")
	blockedPublicPrefixes = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"),
		netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
		netip.MustParsePrefix("3fff::/20"),
	}
)

type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type DialContext func(context.Context, string, string) (net.Conn, error)

type Broker interface {
	Fetch(context.Context, string) (WebResult, error)
	LoadGitHubRepository(context.Context, string) (RepositorySnapshot, error)
}

type Client struct {
	allowedDomains map[string]struct{}
	repositories   map[string]githubRepository
	forbidden      []string
	resolver       Resolver
	dial           DialContext
	tlsConfig      *tls.Config
	now            func() time.Time
}

type githubRepository struct {
	owner string
	name  string
}

type WebResult struct {
	SourceURL   string
	ContentType string
	Content     string
	RetrievedAt time.Time
}

type RepositorySnapshot struct {
	Repository  string
	Revision    string
	Archive     []byte
	RetrievedAt time.Time
}

func New(allowedDomains, githubRepositories, forbidden []string) (*Client, error) {
	dialer := &net.Dialer{Timeout: requestTimeout}
	return newClient(allowedDomains, githubRepositories, forbidden, net.DefaultResolver, dialer.DialContext, nil)
}

func newClient(allowedDomains, githubRepositories, forbidden []string, resolver Resolver, dial DialContext, tlsConfig *tls.Config) (*Client, error) {
	if len(allowedDomains) == 0 || resolver == nil || dial == nil {
		return nil, errors.New("invalid public source client configuration")
	}
	domains := make(map[string]struct{}, len(allowedDomains))
	for _, domain := range allowedDomains {
		domain = strings.ToLower(domain)
		if domain == "" {
			return nil, errors.New("invalid public source domain")
		}
		domains[domain] = struct{}{}
	}
	repositories := make(map[string]githubRepository, len(githubRepositories))
	for _, repositoryURL := range githubRepositories {
		repository, err := parseGitHubRepository(repositoryURL)
		if err != nil {
			return nil, err
		}
		repositories[repositoryURL] = repository
	}
	return &Client{
		allowedDomains: domains, repositories: repositories,
		forbidden: append([]string(nil), forbidden...), resolver: resolver,
		dial: dial, tlsConfig: tlsConfig, now: time.Now,
	}, nil
}

func (c *Client) Fetch(ctx context.Context, rawURL string) (WebResult, error) {
	parsed, err := c.validateURL(rawURL)
	if err != nil {
		return WebResult{}, err
	}
	response, err := c.do(ctx, parsed.String(), "text/html, text/plain, text/markdown, application/json, application/xml, application/xhtml+xml", responseBodyLimit)
	if err != nil {
		return WebResult{}, err
	}
	defer response.Body.Close()
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !supportedTextType(mediaType) {
		return WebResult{}, failure.Failed("public_source_response_type_unsupported")
	}
	contents, err := readBounded(response.Body, responseBodyLimit)
	if err != nil {
		return WebResult{}, err
	}
	if !utf8.Valid(contents) || strings.IndexByte(string(contents), 0) >= 0 {
		return WebResult{}, failure.Failed("public_source_response_type_unsupported")
	}
	if containsForbidden(string(contents), c.forbidden) {
		return WebResult{}, failure.Failed("sensitive_public_source_content")
	}
	return WebResult{
		SourceURL: response.Request.URL.String(), ContentType: mediaType,
		Content: string(contents), RetrievedAt: c.now().UTC(),
	}, nil
}

func (c *Client) LoadGitHubRepository(ctx context.Context, repositoryURL string) (RepositorySnapshot, error) {
	repository, allowed := c.repositories[repositoryURL]
	if !allowed {
		return RepositorySnapshot{}, failure.Failed("public_repository_unavailable")
	}
	base := "https://api.github.com/repos/" + repository.owner + "/" + repository.name
	var metadata struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.getJSON(ctx, base, &metadata); err != nil {
		return RepositorySnapshot{}, err
	}
	if metadata.DefaultBranch == "" || len(metadata.DefaultBranch) > 255 || strings.ContainsAny(metadata.DefaultBranch, "\x00\r\n") {
		return RepositorySnapshot{}, failure.Failed("malformed_public_source_response")
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	commitURL := base + "/commits/" + url.PathEscape(metadata.DefaultBranch)
	if err := c.getJSON(ctx, commitURL, &commit); err != nil {
		return RepositorySnapshot{}, err
	}
	if !githubRevisionPattern.MatchString(commit.SHA) {
		return RepositorySnapshot{}, failure.Failed("malformed_public_source_response")
	}
	revision := strings.ToLower(commit.SHA)
	archiveURL := "https://codeload.github.com/" + repository.owner + "/" + repository.name + "/tar.gz/" + revision
	response, err := c.do(ctx, archiveURL, "application/gzip, application/x-gzip, application/octet-stream", archiveBodyLimit)
	if err != nil {
		return RepositorySnapshot{}, err
	}
	defer response.Body.Close()
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/gzip" && mediaType != "application/x-gzip" && mediaType != "application/octet-stream") {
		return RepositorySnapshot{}, failure.Failed("public_source_response_type_unsupported")
	}
	archive, err := readBounded(response.Body, archiveBodyLimit)
	if err != nil {
		return RepositorySnapshot{}, err
	}
	return RepositorySnapshot{
		Repository: repositoryURL, Revision: revision, Archive: archive,
		RetrievedAt: c.now().UTC(),
	}, nil
}

func (c *Client) getJSON(ctx context.Context, rawURL string, target any) error {
	response, err := c.do(ctx, rawURL, "application/vnd.github+json", metadataBodyLimit)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && mediaType != "application/vnd.github+json") {
		return failure.Failed("public_source_response_type_unsupported")
	}
	contents, err := readBounded(response.Body, metadataBodyLimit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(contents, target); err != nil {
		return failure.Failed("malformed_public_source_response")
	}
	return nil
}

func (c *Client) do(ctx context.Context, rawURL, accept string, limit int64) (*http.Response, error) {
	parsed, err := c.validateURL(rawURL)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		cancel()
		return nil, failure.Failed("public_source_request_invalid")
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("User-Agent", "wormtamer")
	client := &http.Client{
		Timeout:   requestTimeout,
		Transport: &validatedTransport{client: c},
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return failure.Failed("public_source_redirect_limit_exceeded")
			}
			_, err := c.validateURL(request.URL.String())
			return err
		},
	}
	response, err := client.Do(request)
	if err != nil {
		cancel()
		var failureError *failure.Error
		if errors.As(err, &failureError) {
			return nil, failureError
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure.Retry("public_source_network_failure", 0)
	}
	response.Body = &cancelReadCloser{ReadCloser: response.Body, cancel: cancel}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		switch {
		case response.StatusCode == http.StatusRequestTimeout:
			return nil, failure.Retry("public_source_timeout", 0)
		case response.StatusCode == http.StatusTooManyRequests ||
			(response.StatusCode == http.StatusForbidden && response.Header.Get("X-RateLimit-Remaining") == "0"):
			return nil, failure.Retry("public_source_rate_limited", 0)
		case response.StatusCode >= 500:
			return nil, failure.Retry("public_source_unavailable", 0)
		default:
			return nil, failure.Failed("public_source_request_rejected")
		}
	}
	if response.ContentLength > limit {
		response.Body.Close()
		return nil, failure.Failed("public_source_response_limit_exceeded")
	}
	return response, nil
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

type validatedTransport struct {
	client *Client
}

func (t *validatedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	parsed, err := t.client.validateURL(request.URL.String())
	if err != nil {
		return nil, err
	}
	addresses, err := t.client.resolver.LookupIPAddr(request.Context(), parsed.Hostname())
	if err != nil || len(addresses) == 0 {
		return nil, failure.Retry("public_source_dns_failure", 0)
	}
	validated := make([]string, 0, len(addresses))
	for _, address := range addresses {
		parsedAddress, ok := netip.AddrFromSlice(address.IP)
		if !ok || !publicAddress(parsedAddress.Unmap()) {
			return nil, failure.Failed("public_source_destination_blocked")
		}
		validated = append(validated, parsedAddress.String())
	}
	transport := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: requestTimeout,
		ResponseHeaderTimeout: requestTimeout, MaxResponseHeaderBytes: 64 << 10,
		TLSClientConfig: cloneTLSConfig(t.client.tlsConfig),
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || !strings.EqualFold(strings.TrimSuffix(host, "."), parsed.Hostname()) || port != "443" {
				return nil, failure.Failed("public_source_destination_blocked")
			}
			var lastErr error
			for _, ip := range validated {
				connection, err := t.client.dial(ctx, network, net.JoinHostPort(ip, port))
				if err == nil {
					return connection, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
	return transport.RoundTrip(request)
}

func (c *Client) validateURL(rawURL string) (*url.URL, error) {
	if rawURL == "" || len(rawURL) > maxURLBytes || !utf8.ValidString(rawURL) {
		return nil, failure.Failed("public_source_request_invalid")
	}
	if containsForbidden(rawURL, c.forbidden) {
		return nil, failure.Failed("sensitive_public_source_request")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery {
		return nil, failure.Failed("public_source_request_invalid")
	}
	if containsForbidden(parsed.Path, c.forbidden) {
		return nil, failure.Failed("sensitive_public_source_request")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return nil, failure.Failed("public_source_request_invalid")
	}
	hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if hostname == "" || net.ParseIP(hostname) != nil || !asciiHostname(hostname) || !c.domainAllowed(hostname) {
		return nil, failure.Failed("public_source_destination_blocked")
	}
	parsed.Scheme = "https"
	parsed.Host = hostname
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed, nil
}

func (c *Client) domainAllowed(hostname string) bool {
	for domain := range c.allowedDomains {
		if hostname == domain || strings.HasSuffix(hostname, "."+domain) {
			return true
		}
	}
	return false
}

func parseGitHubRepository(raw string) (githubRepository, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return githubRepository{}, errors.New("invalid GitHub repository URL")
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parsed.Path != "/"+parts[0]+"/"+parts[1] {
		return githubRepository{}, errors.New("invalid GitHub repository URL")
	}
	return githubRepository{owner: parts[0], name: parts[1]}, nil
}

func publicAddress(address netip.Addr) bool {
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	if address.Is6() && !globalIPv6Prefix.Contains(address) {
		return false
	}
	for _, prefix := range blockedPublicPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func supportedTextType(mediaType string) bool {
	switch strings.ToLower(mediaType) {
	case "text/plain", "text/html", "text/markdown", "text/x-markdown", "application/json", "application/xml", "application/xhtml+xml":
		return true
	default:
		return false
	}
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, failure.Retry("public_source_response_read_failed", 0)
	}
	if int64(len(contents)) > limit {
		return nil, failure.Failed("public_source_response_limit_exceeded")
	}
	return contents, nil
}

func cloneTLSConfig(configuration *tls.Config) *tls.Config {
	if configuration == nil {
		return nil
	}
	return configuration.Clone()
}

func asciiHostname(hostname string) bool {
	if len(hostname) > 253 {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
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

func containsForbidden(value string, forbidden []string) bool {
	for _, secret := range forbidden {
		if secret != "" && strings.Contains(value, secret) {
			return true
		}
	}
	return false
}

func (r WebResult) ToolResult() map[string]any {
	return map[string]any{
		"authority": "untrusted_public", "source_url": r.SourceURL,
		"content_type": r.ContentType, "retrieved_at": r.RetrievedAt.Format(time.RFC3339Nano),
		"content": r.Content,
	}
}

func ValidateToolResult(result map[string]any) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return failure.Failed("public_source_tool_output_invalid")
	}
	if len(encoded) > MaxToolResponse {
		return failure.Failed("public_source_tool_output_limit_exceeded")
	}
	return nil
}
