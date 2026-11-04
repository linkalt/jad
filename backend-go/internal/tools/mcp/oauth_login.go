package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"jad/backend-go/internal/fserr"
	"jad/backend-go/internal/sessionfs"
)

// LoginOptions are the streams and environment of one "jad mcp login".
type LoginOptions struct {
	// In is where an address the browser was sent to can be pasted, when the browser cannot
	// reach the redirect address. Nil reads nothing.
	In io.Reader
	// Out receives the address to open and progress lines.
	Out io.Writer
	// Environment resolves client_secret_from and headers_from.
	Environment map[string]string
	// OpenBrowser opens the sign-in address; nil only prints it.
	OpenBrowser func(string) error
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

// ErrNoSignIn is a server that answered without asking for authorization, so there was
// nothing to sign in to and nothing was stored.
var ErrNoSignIn = errors.New("the MCP server did not ask for authorization; no token was stored")

// Login signs in to the MCP server of the file at configPath with the authorization-code
// flow and stores the tokens at tokenPath, replacing any there. It connects as a run
// would; the server's 401 answer starts the flow, as the MCP authorization spec says.
func Login(ctx context.Context, configPath, tokenPath string, options LoginOptions) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", configPath, fserr.UnwrapPathError(err))
	}
	reference := sessionfs.MCPServerReference{ConfigPath: configPath}
	checked, err := normalizeConfigFor(raw, reference, "", nil, false)
	if err != nil {
		return err
	}
	if checked.transport.http == nil || checked.transport.http.oauth == nil {
		return fmt.Errorf("%s: sign-in needs a streamable_http transport with transport.oauth", configPath)
	}
	if checked.transport.http.oauth.grant != oauthGrantAuthorizationCode {
		return fmt.Errorf("%s: grant %s needs no sign-in", configPath, checked.transport.http.oauth.grant)
	}
	config, err := normalizeConfigFor(raw, reference, "", options.Environment, true)
	if err != nil {
		return err
	}
	httpConfig := config.transport.http
	oauth := httpConfig.oauth
	out := options.Out
	if out == nil {
		out = io.Discard
	}
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	client := &http.Client{Timeout: config.timeout}
	stored := false
	handlerConfig := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              oauth.redirectURL,
		AuthorizationCodeFetcher: codeFetcher(oauth.redirectURL, options.In, out, options.OpenBrowser),
		RequestRefreshToken:      true,
		Client:                   client,
		NewTokenSource: func(tokenCtx context.Context, issued *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			file := &tokenFile{
				ServerURL: httpConfig.endpoint,
				ClientID:  issued.ClientID,
				TokenURL:  issued.Endpoint.TokenURL,
				AuthStyle: int(issued.Endpoint.AuthStyle),
				Scopes:    issued.Scopes,
			}
			if oauth.clientSecret == "" {
				file.ClientSecret = issued.ClientSecret
			}
			file.setToken(token, now())
			if err := updateTokenFile(ctx, tokenPath, func(*tokenFile) (*tokenFile, error) { return file, nil }); err != nil {
				return nil, err
			}
			stored = true
			return issued.TokenSource(tokenCtx, token), nil
		},
	}
	if len(oauth.scopes) > 0 {
		scopes := append([]string(nil), oauth.scopes...)
		handlerConfig.ScopeFilter = func([]string) []string { return append([]string(nil), scopes...) }
	}
	if oauth.clientID != "" {
		credentials := &oauthex.ClientCredentials{ClientID: oauth.clientID}
		if oauth.clientSecret != "" {
			credentials.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: oauth.clientSecret}
		}
		handlerConfig.PreregisteredClient = credentials
	} else {
		handlerConfig.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName:              "JAD",
			RedirectURIs:            []string{oauth.redirectURL},
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
		}}
	}
	handler, err := auth.NewAuthorizationCodeHandler(handlerConfig)
	if err != nil {
		return fmt.Errorf("%s: transport.oauth: %w", configPath, err)
	}
	transport := &mcpSDK.StreamableClientTransport{
		Endpoint:     httpConfig.endpoint,
		OAuthHandler: handler,
		HTTPClient:   newHeaderHTTPClient(httpConfig.endpoint, httpConfig.headers, config.maxResultBytes),
		MaxEventSize: config.maxResultBytes,
	}
	session, err := mcpSDK.NewClient(&mcpSDK.Implementation{Name: "jad-login", Version: "1.0.0"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("sign in to the MCP server: %w", err)
	}
	_ = session.Close()
	if !stored {
		return ErrNoSignIn
	}
	_, _ = fmt.Fprintf(out, "Signed in; the tokens are in %s\n", tokenPath)
	return nil
}

// Logout removes the stored tokens. It reports whether there were any.
func Logout(ctx context.Context, tokenPath string) (bool, error) {
	removed := false
	err := updateTokenFile(ctx, tokenPath, func(*tokenFile) (*tokenFile, error) {
		err := os.Remove(tokenPath)
		if err == nil {
			removed = true
			return nil, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("remove the MCP token file: %w", fserr.UnwrapPathError(err))
	})
	return removed, err
}

type codeResult struct {
	result *auth.AuthorizationResult
	err    error
}

// codeFetcher shows the sign-in address and waits for the authorization code: from the
// browser's redirect when the redirect address is a loopback port this process can listen
// on, or from the address the browser was sent to, pasted into in.
func codeFetcher(redirect string, in io.Reader, out io.Writer, open func(string) error) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		_, _ = fmt.Fprintf(out, "Open this address in a browser to sign in:\n\n%s\n\n", args.URL)
		if open != nil {
			_ = open(args.URL)
		}
		results := make(chan codeResult, 2)
		listen, _ := loginRedirect(redirect)
		if listen {
			stop, err := serveRedirect(redirect, results)
			if err != nil {
				return nil, err
			}
			defer stop()
			_, _ = fmt.Fprintf(out, "Waiting for the browser at %s. If it cannot reach this machine, paste the address it was sent to here.\n", redirect)
		} else {
			_, _ = fmt.Fprintln(out, "After signing in, paste the address the browser was sent to here.")
		}
		if in != nil {
			go readPastedRedirect(in, results)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case got := <-results:
			return got.result, got.err
		}
	}
}

// serveRedirect listens on the redirect address and answers its path once.
func serveRedirect(redirect string, results chan<- codeResult) (func(), error) {
	parsed, _ := url.Parse(redirect)
	listener, err := net.Listen("tcp", parsed.Host)
	if err != nil {
		return nil, fmt.Errorf("listen on %s for the sign-in redirect: %w", parsed.Host, err)
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, request *http.Request) {
		result, err := redirectResult(request.URL)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, "<p>Sign-in failed: %s</p>", html.EscapeString(err.Error()))
		} else {
			_, _ = io.WriteString(w, "<p>Signed in. You can close this tab.</p>")
		}
		select {
		case results <- codeResult{result: result, err: err}:
		default:
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}, nil
}

// readPastedRedirect reads lines until one is an address with a code or an error.
func readPastedRedirect(in io.Reader, results chan<- codeResult) {
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parsed, err := url.Parse(line)
		if err != nil || (parsed.Query().Get("code") == "" && parsed.Query().Get("error") == "") {
			continue
		}
		result, err := redirectResult(parsed)
		select {
		case results <- codeResult{result: result, err: err}:
		default:
		}
		return
	}
}

// redirectResult reads the authorization response from the redirect address's query.
func redirectResult(address *url.URL) (*auth.AuthorizationResult, error) {
	query := address.Query()
	if code := query.Get("error"); code != "" {
		description := query.Get("error_description")
		if len(description) > 200 {
			description = description[:200]
		}
		return nil, fmt.Errorf("the authorization server refused: %s %s", code, description)
	}
	if query.Get("code") == "" {
		return nil, errors.New("the redirect carried no authorization code")
	}
	return &auth.AuthorizationResult{Code: query.Get("code"), State: query.Get("state"), Iss: query.Get("iss")}, nil
}
