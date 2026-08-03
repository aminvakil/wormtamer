package publicsource

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
)

type staticResolver map[string][]string

func (r staticResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	values, ok := r[host]
	if !ok {
		return nil, errors.New("unknown host")
	}
	result := make([]net.IPAddr, 0, len(values))
	for _, value := range values {
		result = append(result, net.IPAddr{IP: net.ParseIP(value)})
	}
	return result, nil
}

func TestFetchAllowsConfiguredSubdomainAndAttributesText(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/docs" || request.Header.Get("User-Agent") != "wormtamer" {
			t.Fatalf("request = %+v", request)
		}
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = response.Write([]byte("public documentation"))
	}))
	defer server.Close()
	client := testClient(t, []string{"example.com"}, nil, staticResolver{"docs.example.com": {"93.184.216.34"}}, server)
	client.now = func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) }

	result, err := client.Fetch(context.Background(), "https://docs.example.com/docs#section")
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceURL != "https://docs.example.com/docs" || result.Content != "public documentation" || result.ContentType != "text/plain" || !result.RetrievedAt.Equal(client.now()) {
		t.Fatalf("Fetch() = %+v", result)
	}
}

func TestFetchRejectsUnapprovedAndUnsafeURLsBeforeDial(t *testing.T) {
	client, err := newClient([]string{"example.com"}, nil, nil,
		staticResolver{"example.com": {"93.184.216.34"}}, func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("unsafe URL reached dial")
			return nil, nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rawURL := range []string{
		"http://example.com/docs", "https://user@example.com/docs", "https://example.com:8443/docs",
		"https://example.com/docs?q=private", "https://fake-example.com/docs", "https://127.0.0.1/docs",
	} {
		t.Run(rawURL, func(t *testing.T) {
			_, err := client.Fetch(context.Background(), rawURL)
			var failureError *failure.Error
			if !errors.As(err, &failureError) || failureError.Retryable {
				t.Fatalf("Fetch(%q) error = %v", rawURL, err)
			}
		})
	}
}

func TestFetchRejectsEncodedConfiguredSecretBeforeDial(t *testing.T) {
	client, err := newClient([]string{"example.com"}, nil, []string{"deployment-secret"},
		staticResolver{"example.com": {"93.184.216.34"}}, func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("sensitive URL reached dial")
			return nil, nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Fetch(context.Background(), "https://example.com/deployment%2Dsecret")
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "sensitive_public_source_request" {
		t.Fatalf("Fetch() error = %v", err)
	}
}

func TestFetchRejectsPrivateAndMixedDNSAnswers(t *testing.T) {
	for name, addresses := range map[string][]string{
		"private":       {"10.0.0.1"},
		"metadata":      {"169.254.169.254"},
		"mixed":         {"93.184.216.34", "127.0.0.1"},
		"documentation": {"192.0.2.1"},
		"this-network":  {"0.1.2.3"},
		"reserved-ipv4": {"240.0.0.1"},
		"nat64":         {"64:ff9b::a00:1"},
		"six-to-four":   {"2002:0a00:0001::"},
		"reserved-ipv6": {"3fff::1"},
	} {
		t.Run(name, func(t *testing.T) {
			client, err := newClient([]string{"example.com"}, nil, nil,
				staticResolver{"example.com": addresses}, func(context.Context, string, string) (net.Conn, error) {
					t.Fatal("blocked destination reached dial")
					return nil, nil
				}, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Fetch(context.Background(), "https://example.com/")
			var failureError *failure.Error
			if !errors.As(err, &failureError) || failureError.Category != "public_source_destination_blocked" {
				t.Fatalf("Fetch() error = %v", err)
			}
		})
	}
}

func TestFetchRevalidatesRedirectDestination(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, "https://internal.example.net/private", http.StatusFound)
	}))
	defer server.Close()
	client := testClient(t, []string{"example.com"}, nil, staticResolver{"example.com": {"93.184.216.34"}}, server)
	_, err := client.Fetch(context.Background(), "https://example.com/")
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "public_source_destination_blocked" {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestFetchRejectsRedirectResolvedToPrivateAddress(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, "https://internal.example.net/private", http.StatusFound)
	}))
	defer server.Close()
	client := testClient(t, []string{"example.com", "example.net"}, nil, staticResolver{
		"example.com":          {"93.184.216.34"},
		"internal.example.net": {"10.0.0.1"},
	}, server)
	_, err := client.Fetch(context.Background(), "https://example.com/")
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "public_source_destination_blocked" {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestFetchRejectsSensitiveAndUnsupportedContent(t *testing.T) {
	for name, test := range map[string][3]string{
		"secret": {"text/plain", "contains deployment-secret", "sensitive_public_source_content"},
		"binary": {"application/octet-stream", "binary", "public_source_response_type_unsupported"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", test[0])
				_, _ = response.Write([]byte(test[1]))
			}))
			defer server.Close()
			client := testClient(t, []string{"example.com"}, nil, staticResolver{"example.com": {"93.184.216.34"}}, server)
			client.forbidden = []string{"deployment-secret"}
			_, err := client.Fetch(context.Background(), "https://example.com/")
			var failureError *failure.Error
			if !errors.As(err, &failureError) || failureError.Category != test[2] {
				t.Fatalf("Fetch() error = %v", err)
			}
		})
	}
}

func TestLoadGitHubRepositoryPinsDefaultBranchHead(t *testing.T) {
	revision := strings.Repeat("a", 40)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Host + request.URL.Path {
		case "api.github.com/repos/nginx/nginx":
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"default_branch":"master","extra":true}`))
		case "api.github.com/repos/nginx/nginx/commits/master":
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"sha":"` + revision + `","extra":true}`))
		case "codeload.github.com/nginx/nginx/tar.gz/" + revision:
			response.Header().Set("Content-Type", "application/gzip")
			_, _ = response.Write([]byte("archive"))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	resolver := staticResolver{
		"api.github.com":      {"140.82.112.5"},
		"codeload.github.com": {"140.82.112.9"},
	}
	client := testClient(t, []string{"github.com"}, []string{"https://github.com/nginx/nginx"}, resolver, server)
	client.now = func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) }

	snapshot, err := client.LoadGitHubRepository(context.Background(), "https://github.com/nginx/nginx")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Repository != "https://github.com/nginx/nginx" || snapshot.Revision != revision || string(snapshot.Archive) != "archive" || !snapshot.RetrievedAt.Equal(client.now()) {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestGitHubRepositoryMustBeConfigured(t *testing.T) {
	client, err := New([]string{"github.com"}, []string{"https://github.com/nginx/nginx"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.LoadGitHubRepository(context.Background(), "https://github.com/other/project")
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "public_repository_unavailable" {
		t.Fatalf("LoadGitHubRepository() error = %v", err)
	}
}

func TestValidateToolResultBoundsEncodedOutput(t *testing.T) {
	if err := ValidateToolResult(map[string]any{"content": strings.Repeat("x", MaxToolResponse)}); err == nil {
		t.Fatal("ValidateToolResult() accepted oversized output")
	}
}

func testClient(t *testing.T, domains, repositories []string, resolver Resolver, server *httptest.Server) *Client {
	t.Helper()
	serverAddress := server.Listener.Addr().String()
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, serverAddress)
	}
	configuration := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	configuration.ServerName = "example.com"
	client, err := newClient(domains, repositories, nil, resolver, dial, configuration)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
