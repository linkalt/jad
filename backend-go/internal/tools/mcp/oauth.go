package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/auth/extauth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"jad/backend-go/internal/fserr"
	"jad/backend-go/internal/sessionlock"
)

// errNeedLogin is a server whose stored OAuth tokens are missing, were issued for another
// address, or can no longer be refreshed. Only "jad mcp login" fixes it: JAD asks nobody
// during a run.
var errNeedLogin = errors.New("MCP OAuth sign-in required")

const tokenFileVersion = 1

// maxTokenFileBytes bounds the token file read; a real one is a few kilobytes.
const maxTokenFileBytes = 1 << 20

// tokenFile is the OAuth state of one MCP server: the client JAD signed in as, where its
// tokens are refreshed, and the tokens. Login writes it; a run refreshes it in place.
type tokenFile struct {
	SchemaVersion int    `json:"schema_version"`
	ServerURL     string `json:"server_url"`
	ClientID      string `json:"client_id"`
	// ClientSecret is a secret the server issued at dynamic registration. One set by
	// client_secret_from is never stored.
	ClientSecret string     `json:"client_secret,omitempty"`
	TokenURL     string     `json:"token_url"`
	AuthStyle    int        `json:"auth_style,omitempty"`
	Scopes       []string   `json:"scopes,omitempty"`
	AccessToken  string     `json:"access_token"`
	TokenType    string     `json:"token_type,omitempty"`
	RefreshToken string     `json:"refresh_token,omitempty"`
	Expiry       *time.Time `json:"expiry,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func (tokenFile) String() string   { return "mcp.tokenFile{redacted}" }
func (tokenFile) GoString() string { return "mcp.tokenFile{redacted}" }

func (f *tokenFile) token() *oauth2.Token {
	token := &oauth2.Token{AccessToken: f.AccessToken, TokenType: f.TokenType, RefreshToken: f.RefreshToken}
	if f.Expiry != nil {
		token.Expiry = *f.Expiry
	}
	return token
}

// setToken stores a token the server issued. A refresh that returns no new refresh token
// keeps the one it was made with.
func (f *tokenFile) setToken(token *oauth2.Token, now time.Time) {
	f.AccessToken = token.AccessToken
	f.TokenType = token.TokenType
	if token.RefreshToken != "" {
		f.RefreshToken = token.RefreshToken
	}
	f.Expiry = nil
	if !token.Expiry.IsZero() {
		expiry := token.Expiry.UTC()
		f.Expiry = &expiry
	}
	f.UpdatedAt = now.UTC()
}

func readTokenFile(path string) (*tokenFile, error) {
	handle, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errNeedLogin
		}
		return nil, fmt.Errorf("read the MCP token file: %w", fserr.UnwrapPathError(err))
	}
	defer handle.Close()
	data, err := io.ReadAll(io.LimitReader(handle, maxTokenFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the MCP token file: %w", fserr.UnwrapPathError(err))
	}
	if len(data) > maxTokenFileBytes {
		return nil, errors.New("the MCP token file is larger than 1 MiB")
	}
	var file tokenFile
	if err := json.Unmarshal(data, &file); err != nil || file.SchemaVersion != tokenFileVersion || file.AccessToken == "" || file.ClientID == "" || file.TokenURL == "" {
		return nil, errors.New("the MCP token file is not valid; sign in again")
	}
	return &file, nil
}

// updateTokenFile changes the token file under an exclusive lock, so two runs, or a run and
// a sign-in, cannot spend one rotating refresh token twice. change gets the current file,
// or nil when there is none.
func updateTokenFile(ctx context.Context, path string, change func(*tokenFile) (*tokenFile, error)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create the MCP token folder: %w", fserr.UnwrapPathError(err))
	}
	lockPath, err := sessionlock.PathFor(path)
	if err != nil {
		return err
	}
	lock, err := sessionlock.Acquire(ctx, lockPath)
	if err != nil {
		return fmt.Errorf("lock the MCP token file: %w", err)
	}
	defer lock.Close()
	current, err := readTokenFile(path)
	if err != nil && !errors.Is(err, errNeedLogin) {
		current = nil
	}
	next, err := change(current)
	if err != nil || next == nil {
		return err
	}
	next.SchemaVersion = tokenFileVersion
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the MCP token file: %w", err)
	}
	return writePrivateFile(path, append(data, '\n'))
}

// writePrivateFile replaces path with data through a temporary file of mode 0600, so a
// reader sees the old file or the new one and no one else can read either.
func writePrivateFile(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write the MCP token file: %w", fserr.UnwrapPathError(err))
	}
	name := temp.Name()
	_, writeErr := temp.Write(data)
	if writeErr == nil {
		writeErr = temp.Sync()
	}
	if closeErr := temp.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Rename(name, path)
	}
	if writeErr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write the MCP token file: %w", fserr.UnwrapPathError(writeErr))
	}
	return nil
}

// storedOAuth gives a connection the access token "jad mcp login" stored, refreshing it
// when it is about to expire or the server refuses it. It never starts a sign-in.
type storedOAuth struct {
	path      string
	serverURL string
	config    *oauthConfig
	client    *http.Client
	timeout   time.Duration
	now       func() time.Time

	mu    sync.Mutex
	token *oauth2.Token
	// issued is when token was stored, which with its expiry gives its lifetime.
	issued time.Time
	// needLogin is set when the failure is one only a new sign-in fixes, so the error the
	// run reports can say so.
	needLogin atomic.Bool
}

var _ auth.OAuthHandler = (*storedOAuth)(nil)

func newStoredOAuth(path, serverURL string, config *oauthConfig, timeout time.Duration) *storedOAuth {
	return &storedOAuth{path: path, serverURL: serverURL, config: config, client: &http.Client{Timeout: timeout}, timeout: timeout, now: time.Now}
}

func (s *storedOAuth) TokenSource(context.Context) (oauth2.TokenSource, error) { return s, nil }

// Token returns an access token that stays valid for at least refresh_margin_ms.
func (s *storedOAuth) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != nil && s.fresh(s.token, s.issued) {
		return s.token, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	return s.load(ctx, nil)
}

// Authorize is called when the server answered 401 or 403. A 401 refreshes the token once,
// even if it has not expired, since the server may have revoked it; the transport then
// retries. A 403 for a missing scope needs a new sign-in; any other 403 is the server's
// answer and is left to the call.
func (s *storedOAuth) Authorize(ctx context.Context, _ *http.Request, response *http.Response) error {
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxTokenFileBytes))
	if response.StatusCode == http.StatusForbidden {
		challenges, _ := oauthex.ParseWWWAuthenticate(response.Header.Values("WWW-Authenticate"))
		for _, challenge := range challenges {
			if challenge.Params["error"] == "insufficient_scope" {
				s.needLogin.Store(true)
				return errNeedLogin
			}
		}
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	refused := s.token
	if refused == nil {
		refused = &oauth2.Token{}
	}
	if _, err := s.load(ctx, refused); err != nil {
		return err
	}
	return nil
}

// fresh reports whether token stays valid for the refresh margin. The margin is at most
// half the token's lifetime, so a server that issues tokens for less than the margin does
// not get a refresh before every request.
func (s *storedOAuth) fresh(token *oauth2.Token, issued time.Time) bool {
	if token.AccessToken == "" {
		return false
	}
	if token.Expiry.IsZero() {
		return true
	}
	margin := s.config.refreshMargin
	if lifetime := token.Expiry.Sub(issued); lifetime > 0 && margin > lifetime/2 {
		margin = lifetime / 2
	}
	return token.Expiry.After(s.now().Add(margin))
}

// load reads the token file under its lock and refreshes the token when it is about to
// expire, or when it is the token the server just refused. Another process may have
// refreshed the file already; its token is then used as it is. s.mu is held.
func (s *storedOAuth) load(ctx context.Context, refused *oauth2.Token) (*oauth2.Token, error) {
	var result *oauth2.Token
	var issued time.Time
	err := updateTokenFile(ctx, s.path, func(file *tokenFile) (*tokenFile, error) {
		if file == nil || file.ServerURL != s.serverURL {
			return nil, errNeedLogin
		}
		stored := file.token()
		if s.fresh(stored, file.UpdatedAt) && (refused == nil || stored.AccessToken != refused.AccessToken) {
			result, issued = stored, file.UpdatedAt
			return nil, nil
		}
		if file.RefreshToken == "" {
			return nil, errNeedLogin
		}
		secret := file.ClientSecret
		if s.config.clientSecret != "" {
			secret = s.config.clientSecret
		}
		config := oauth2.Config{
			ClientID:     file.ClientID,
			ClientSecret: secret,
			Endpoint:     oauth2.Endpoint{TokenURL: file.TokenURL, AuthStyle: oauth2.AuthStyle(file.AuthStyle)},
			Scopes:       file.Scopes,
		}
		expired := &oauth2.Token{RefreshToken: file.RefreshToken, Expiry: time.Unix(1, 0)}
		next, err := config.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, s.client), expired).Token()
		if err != nil {
			var retrieve *oauth2.RetrieveError
			if errors.As(err, &retrieve) {
				if retrieve.ErrorCode == "invalid_grant" {
					return nil, errNeedLogin
				}
				// The response body can carry anything the server chose; only its status
				// and error code are reported.
				return nil, fmt.Errorf("refresh the MCP OAuth token: HTTP %d %s", retrieve.Response.StatusCode, retrieve.ErrorCode)
			}
			return nil, errors.New("refresh the MCP OAuth token: the token endpoint did not answer")
		}
		file.setToken(next, s.now())
		result, issued = file.token(), file.UpdatedAt
		return file, nil
	})
	if err != nil {
		if errors.Is(err, errNeedLogin) {
			s.needLogin.Store(true)
		}
		return nil, err
	}
	s.token, s.issued = result, issued
	return result, nil
}

// oauthHandler is the transport's OAuth handler for a server file, or nil when the file
// has no transport.oauth. login is the server's "<category>/<name>" for the message that
// asks for a sign-in.
func oauthHandler(config *oauthConfig, endpoint, tokenPath, configPath string, timeout time.Duration) (auth.OAuthHandler, *storedOAuth, error) {
	if config == nil {
		return nil, nil, nil
	}
	if config.grant == oauthGrantClientCredentials {
		handler, err := extauth.NewClientCredentialsHandler(&extauth.ClientCredentialsHandlerConfig{
			Credentials: &oauthex.ClientCredentials{ClientID: config.clientID, ClientSecretAuth: &oauthex.ClientSecretAuth{ClientSecret: config.clientSecret}},
			HTTPClient:  &http.Client{Timeout: timeout},
		})
		if err != nil {
			return nil, nil, configError(configPath, "transport.oauth: %v", err)
		}
		return handler, nil, nil
	}
	if tokenPath == "" {
		return nil, nil, configError(configPath, "transport.oauth needs the catalog layout, where jad mcp login keeps the tokens")
	}
	stored := newStoredOAuth(tokenPath, endpoint, config, timeout)
	return stored, stored, nil
}

// loginName is how "jad mcp login" names the server of a catalog file: <category>/<name>.
func loginName(configPath string) string {
	return filepath.Base(filepath.Dir(configPath)) + "/" + trimJSONExtension(filepath.Base(configPath))
}

func trimJSONExtension(name string) string {
	if extension := filepath.Ext(name); extension == ".json" {
		return name[:len(name)-len(extension)]
	}
	return name
}
