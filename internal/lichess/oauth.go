package lichess

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
)

// Auth is the sign-in half of the Lichess integration (spec §3.1): the
// two steps of an authorization-code flow. It is an interface, like
// API, so the web handlers can be tested against Fake with no HTTP.
type Auth interface {
	// AuthCodeURL is where to send the browser. state comes back
	// verbatim on the redirect (the caller checks it against its
	// cookie); verifier is the PKCE secret whose S256 challenge is
	// embedded in the URL and must be presented again at Exchange.
	AuthCodeURL(state, verifier string, scopes []string) string

	// Exchange trades the code from the redirect for an access token.
	Exchange(ctx context.Context, code, verifier string) (Token, error)
}

// Token is what an exchange yields. Lichess issues no refresh token
// (verified against the API: "Refresh tokens are not supported"), so
// the remedy for an expired token is simply signing in again.
type Token struct {
	AccessToken tokencrypt.Secret
	// ExpiresAt is zero when Lichess reported no expiry.
	ExpiresAt time.Time
}

// OAuth is the real Auth, backed by golang.org/x/oauth2. Lichess is a
// public PKCE client: there is no client secret and nothing to
// register, so the client id is an arbitrary string of our choosing.
type OAuth struct {
	cfg        oauth2.Config
	httpClient *http.Client
}

// OAuthOption configures NewOAuth.
type OAuthOption func(*OAuth)

// WithOAuthBaseURL points both endpoints at another host — an
// httptest.Server in tests.
func WithOAuthBaseURL(u string) OAuthOption {
	return func(o *OAuth) { o.cfg.Endpoint = endpointAt(u) }
}

// WithOAuthHTTPClient overrides the *http.Client used for the token
// exchange.
func WithOAuthHTTPClient(hc *http.Client) OAuthOption { return func(o *OAuth) { o.httpClient = hc } }

func endpointAt(base string) oauth2.Endpoint {
	return oauth2.Endpoint{
		AuthURL:  base + "/oauth",
		TokenURL: base + "/api/token",
		// Lichess wants client_id in the form body; there is no secret
		// to put in a Basic-auth header anyway.
		AuthStyle: oauth2.AuthStyleInParams,
	}
}

// NewOAuth builds the real Auth. redirectURI must be byte-for-byte the
// URL Lichess will be told at both steps.
func NewOAuth(clientID, redirectURI string, opts ...OAuthOption) *OAuth {
	o := &OAuth{
		cfg: oauth2.Config{
			ClientID:    clientID,
			RedirectURL: redirectURI,
			Endpoint:    endpointAt(defaultBaseURL),
		},
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

var _ Auth = (*OAuth)(nil)

// GenerateVerifier returns a fresh PKCE code verifier. It lives here so
// that nothing outside this package needs to import x/oauth2.
func GenerateVerifier() string { return oauth2.GenerateVerifier() }

// AuthCodeURL implements Auth.
func (o *OAuth) AuthCodeURL(state, verifier string, scopes []string) string {
	cfg := o.cfg
	cfg.Scopes = scopes
	return cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

// Exchange implements Auth. A rejection by Lichess (HTTP 400 with
// {error, error_description}) is returned as an *ExchangeError naming
// both, so the log line says why rather than just that it failed.
func (o *OAuth) Exchange(ctx context.Context, code, verifier string) (Token, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, o.httpClient)
	tok, err := o.cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode != "" {
			return Token{}, &ExchangeError{Code: re.ErrorCode, Description: re.ErrorDescription}
		}
		return Token{}, fmt.Errorf("lichess: token exchange: %w", err)
	}
	return Token{AccessToken: tokencrypt.Secret(tok.AccessToken), ExpiresAt: tok.Expiry}, nil
}

// ExchangeError is Lichess's own reason for refusing a code exchange
// (verified OAuthError schema).
type ExchangeError struct {
	Code        string
	Description string
}

func (e *ExchangeError) Error() string {
	if e.Description == "" {
		return "lichess: token exchange refused: " + e.Code
	}
	return fmt.Sprintf("lichess: token exchange refused: %s: %s", e.Code, e.Description)
}
