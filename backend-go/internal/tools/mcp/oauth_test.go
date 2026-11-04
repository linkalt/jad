package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// oauthFixture is an MCP server behind a bearer token, with its own authorization server:
// protected-resource and server metadata, dynamic registration, and a token endpoint that
// issues a new access and refresh token on every grant.
type oauthFixture struct {
	t      *testing.T
	server *httptest.Server

	mu         sync.Mutex
	valid      map[string]bool
	refresh    map[string]bool
	issued     int
	registered int
	grants     []string
	codes      map[string]bool
	expiresIn  int
}

func newOAuthFixture(t *testing.T) *oauthFixture {
	t.Helper()
	fixture := &oauthFixture{t: t, valid: map[string]bool{}, refresh: map[string]bool{}, codes: map[string]bool{}, expiresIn: 3600}
	mcpServer, _ := bodyLimitTestTool(t, "secret tool")
	inner := mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return mcpServer }, &mcpSDK.StreamableHTTPOptions{Stateless: true})
	mux := http.NewServeMux()
	fixture.server = httptest.NewServer(mux)
	base := fixture.server.URL
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		fixture.mu.Lock()
		ok := fixture.valid[token]
		fixture.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
	writeJSON := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token",
			"registration_endpoint": base + "/register", "response_types_supported": []string{"code"},
			"code_challenge_methods_supported": []string{"S256"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		fixture.mu.Lock()
		fixture.registered++
		fixture.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"client_id": "dynamic-client", "redirect_uris": []string{"http://127.0.0.1/callback"}, "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		grant := r.Form.Get("grant_type")
		fixture.grants = append(fixture.grants, grant)
		switch {
		case grant == "authorization_code" && fixture.codes[r.Form.Get("code")] && r.Form.Get("code_verifier") != "":
			delete(fixture.codes, r.Form.Get("code"))
		case grant == "refresh_token" && fixture.refresh[r.Form.Get("refresh_token")]:
			delete(fixture.refresh, r.Form.Get("refresh_token"))
		case grant == "client_credentials" && clientSecret(r) == "service-secret":
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"token secret-detail"}`)
			return
		}
		fixture.issued++
		access, refresh := fmt.Sprintf("access-%d", fixture.issued), fmt.Sprintf("refresh-%d", fixture.issued)
		fixture.valid[access] = true
		fixture.refresh[refresh] = true
		writeJSON(w, map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": fixture.expiresIn})
	})
	t.Cleanup(func() {
		fixture.server.CloseClientConnections()
		fixture.server.Close()
	})
	return fixture
}

// approve plays the browser: it signs the user in at the authorization address and
// returns the address the authorization server sends the browser back to.
func (f *oauthFixture) approve(authorizeURL string) string {
	parsed, err := url.Parse(authorizeURL)
	if err != nil {
		f.t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("code_challenge_method") != "S256" || query.Get("resource") != f.server.URL+"/mcp" {
		f.t.Errorf("authorization address lacks PKCE or the resource: %s", authorizeURL)
	}
	f.mu.Lock()
	code := fmt.Sprintf("code-%d", len(f.codes)+f.issued)
	f.codes[code] = true
	f.mu.Unlock()
	return query.Get("redirect_uri") + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(query.Get("state"))
}

func (f *oauthFixture) revokeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valid = map[string]bool{}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func writeOAuthServerFile(t *testing.T, dir, url string, oauth map[string]any) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"transport": map[string]any{"type": "streamable_http", "url": url, "oauth": oauth}, "timeout_ms": 10_000})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "remote", "docs.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOAuthLoginStoresTokensAndARunUsesAndRefreshesThem(t *testing.T) {
	fixture := newOAuthFixture(t)
	dir := t.TempDir()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", freeLoopbackPort(t))
	configPath := writeOAuthServerFile(t, dir, fixture.server.URL+"/mcp", map[string]any{"redirect_url": redirect})
	tokenPath := filepath.Join(dir, "state", ".mcp-oauth", "remote", "docs.json")
	reference := sessionfs.MCPServerReference{ID: "docs", ConfigPath: configPath, Enabled: true, TokenPath: tokenPath}

	// Before a sign-in, the run says which command signs in.
	manager := NewManager(nil)
	_, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, dir)
	manager.Close()
	if err == nil || !strings.Contains(err.Error(), "needs an OAuth sign-in: run jad mcp login remote/docs") {
		t.Fatalf("load before sign-in: %v", err)
	}

	// The browser reaches the loopback redirect address.
	var output strings.Builder
	err = Login(context.Background(), configPath, tokenPath, LoginOptions{Out: &output, OpenBrowser: func(address string) error {
		go func() {
			response, err := http.Get(fixture.approve(address))
			if err == nil {
				response.Body.Close()
			}
		}()
		return nil
	}})
	if err != nil {
		t.Fatalf("Login() = %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "Open this address") || !strings.Contains(output.String(), "Signed in") {
		t.Fatalf("login output = %q", output.String())
	}
	info, err := os.Stat(tokenPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v, mode %v; want 0600", err, info)
	}
	stored, err := readTokenFile(tokenPath)
	if err != nil || stored.ClientID != "dynamic-client" || stored.AccessToken != "access-1" || stored.RefreshToken != "refresh-1" || stored.ServerURL != fixture.server.URL+"/mcp" {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	if fmt.Sprintf("%v %+v %#v", *stored, *stored, *stored) != "mcp.tokenFile{redacted} mcp.tokenFile{redacted} mcp.tokenFile{redacted}" {
		t.Fatal("a token file prints its tokens")
	}

	// A run uses the stored token.
	manager = NewManager(nil)
	defer manager.Close()
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load after sign-in: %d tools, %v", len(loaded), err)
	}

	// A revoked token is refreshed once and the request retried; the rotated refresh
	// token is stored.
	fixture.revokeAll()
	if got := callEchoLike(t, loaded[0]); !strings.Contains(got, "secret tool") {
		t.Fatalf("call after revocation = %s", got)
	}
	stored, err = readTokenFile(tokenPath)
	if err != nil || stored.AccessToken != "access-2" || stored.RefreshToken != "refresh-2" {
		t.Fatalf("after refresh: %+v, %v", stored, err)
	}

	// A refresh token the server no longer accepts asks for a new sign-in.
	fixture.revokeAll()
	fixture.mu.Lock()
	fixture.refresh = map[string]bool{}
	fixture.mu.Unlock()
	other := NewManager(nil)
	defer other.Close()
	reference.DenyTools = []string{"x"}
	_, err = other.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, dir)
	if err == nil || !strings.Contains(err.Error(), "needs an OAuth sign-in") || strings.Contains(err.Error(), "secret-detail") {
		t.Fatalf("load with a dead refresh token: %v", err)
	}

	removed, err := Logout(context.Background(), tokenPath)
	if err != nil || !removed {
		t.Fatalf("Logout() = %v, %v", removed, err)
	}
	if removed, err := Logout(context.Background(), tokenPath); err != nil || removed {
		t.Fatalf("second Logout() = %v, %v", removed, err)
	}
}

// When the browser is on another machine, the address it was sent to is pasted. A token
// that expires within refresh_margin_ms is refreshed before it is sent; one whose whole
// lifetime is shorter than the margin is not refreshed again before every request.
func TestOAuthLoginTakesAPastedRedirectAndRefreshesBeforeExpiry(t *testing.T) {
	fixture := newOAuthFixture(t)
	fixture.expiresIn = 30
	dir := t.TempDir()
	configPath := writeOAuthServerFile(t, dir, fixture.server.URL+"/mcp", map[string]any{"redirect_url": "https://example.invalid/callback", "client_id": "registered-client"})
	tokenPath := filepath.Join(dir, "tokens", "docs.json")
	reader, writer := io.Pipe()
	defer writer.Close()
	var output strings.Builder
	err := Login(context.Background(), configPath, tokenPath, LoginOptions{In: reader, Out: &output, OpenBrowser: func(address string) error {
		go func() { _, _ = io.WriteString(writer, "not an address\n"+fixture.approve(address)+"\n") }()
		return nil
	}})
	if err != nil {
		t.Fatalf("Login() = %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "paste the address") {
		t.Fatalf("login output = %q", output.String())
	}
	if fixture.registered != 0 {
		t.Fatalf("a client_id was given, yet the client registered %d times", fixture.registered)
	}
	stored, err := readTokenFile(tokenPath)
	if err != nil || stored.ClientID != "registered-client" {
		t.Fatalf("stored = %+v, %v", stored, err)
	}

	// The stored token was issued an hour ago and has 10 seconds left: within the margin.
	if err := updateTokenFile(context.Background(), tokenPath, func(file *tokenFile) (*tokenFile, error) {
		expiry := time.Now().Add(10 * time.Second)
		file.Expiry, file.UpdatedAt = &expiry, time.Now().Add(-time.Hour)
		return file, nil
	}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(nil)
	defer manager.Close()
	reference := sessionfs.MCPServerReference{ID: "docs", ConfigPath: configPath, Enabled: true, TokenPath: tokenPath}
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, dir)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got := callEchoLike(t, loaded[0]); !strings.Contains(got, "secret tool") {
			t.Fatalf("call = %s", got)
		}
	}
	fixture.mu.Lock()
	grants := strings.Join(fixture.grants, ",")
	fixture.mu.Unlock()
	if grants != "authorization_code,refresh_token" {
		t.Fatalf("grants = %s, want one refresh: before the expiring token was sent, and none for the new 30-second token", grants)
	}
}

// Tokens are kept per address: after the file names another URL, the stored token is not
// sent to it.
func TestOAuthTokenIsNotSentToAnotherAddress(t *testing.T) {
	fixture := newOAuthFixture(t)
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "docs.json")
	expiry := time.Now().Add(time.Hour)
	if err := updateTokenFile(context.Background(), tokenPath, func(*tokenFile) (*tokenFile, error) {
		return &tokenFile{ServerURL: "https://elsewhere.example/mcp", ClientID: "c", TokenURL: "https://elsewhere.example/token", AccessToken: "access-0", Expiry: &expiry}, nil
	}); err != nil {
		t.Fatal(err)
	}
	fixture.valid["access-0"] = true
	configPath := writeOAuthServerFile(t, dir, fixture.server.URL+"/mcp", map[string]any{})
	manager := NewManager(nil)
	defer manager.Close()
	_, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{{ID: "docs", ConfigPath: configPath, Enabled: true, TokenPath: tokenPath}}, dir)
	if err == nil || !strings.Contains(err.Error(), "needs an OAuth sign-in") {
		t.Fatalf("load with a token for another address: %v", err)
	}
}

func callEchoLike(t *testing.T, tool tools.Tool) string {
	t.Helper()
	output, err := tool.ExecuteOutput(context.Background(), map[string]any{}, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

// clientSecret is the secret a token request authenticated with, in the form or as HTTP
// basic authentication.
func clientSecret(r *http.Request) string {
	if _, secret, ok := r.BasicAuth(); ok {
		return secret
	}
	return r.Form.Get("client_secret")
}

// The client_credentials grant needs no sign-in and no token file: the secret comes from
// the environment and the token from the token endpoint the server's metadata names.
func TestOAuthClientCredentialsNeedNoSignIn(t *testing.T) {
	fixture := newOAuthFixture(t)
	dir := t.TempDir()
	configPath := writeOAuthServerFile(t, dir, fixture.server.URL+"/mcp", map[string]any{"grant": "client_credentials", "client_id": "service", "client_secret_from": "SERVICE_SECRET"})
	manager := NewManager(map[string]string{"SERVICE_SECRET": "service-secret"})
	defer manager.Close()
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{{ID: "docs", ConfigPath: configPath, Enabled: true}}, dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load = %d tools, %v", len(loaded), err)
	}
	if got := callEchoLike(t, loaded[0]); !strings.Contains(got, "secret tool") {
		t.Fatalf("call = %s", got)
	}
	if err := Login(context.Background(), configPath, filepath.Join(dir, "token.json"), LoginOptions{Environment: map[string]string{"SERVICE_SECRET": "x"}}); err == nil || !strings.Contains(err.Error(), "needs no sign-in") {
		t.Fatalf("Login() for client_credentials = %v", err)
	}
	wrong := NewManager(map[string]string{"SERVICE_SECRET": "wrong"})
	defer wrong.Close()
	if _, err := wrong.LoadTools(context.Background(), []sessionfs.MCPServerReference{{ID: "docs", ConfigPath: configPath, Enabled: true}}, dir); err == nil {
		t.Fatal("a wrong client secret connected")
	}
}

// Without the catalog layout there is nowhere to keep tokens, so an authorization_code
// server is refused before it connects.
func TestOAuthAuthorizationCodeNeedsATokenPath(t *testing.T) {
	dir := t.TempDir()
	configPath := writeOAuthServerFile(t, dir, "https://example.invalid/mcp", map[string]any{})
	manager := NewManager(nil)
	defer manager.Close()
	_, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{{ID: "docs", ConfigPath: configPath, Enabled: true}}, dir)
	if err == nil || !strings.Contains(err.Error(), "needs the catalog layout") {
		t.Fatalf("load without a token path: %v", err)
	}
}
