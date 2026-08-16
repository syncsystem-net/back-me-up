package oauth1

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file holds the three-legged authorization dance, separated from request
// signing because two callers need it: cmd/fourshared-auth (bootstrapping an
// account from a terminal) and the in-app Re-authorize button. They must produce
// identical results — a token obtained one way has to work the other way — so
// they share one implementation rather than two that drift.

// Endpoints are a provider's three OAuth URLs.
type Endpoints struct {
	Initiate  string
	Authorize string
	Token     string
}

// FourSharedEndpoints are 4shared's. Kept here beside the flow because the
// quirks documented on Flow are all 4shared's, and a second OAuth provider would
// declare its own set next to this one.
var FourSharedEndpoints = Endpoints{
	Initiate:  "https://api.4shared.com/v1_2/oauth/initiate",
	Authorize: "https://api.4shared.com/v1_2/oauth/authorize",
	Token:     "https://api.4shared.com/v1_2/oauth/token",
}

// Flow runs the authorization dance for one account.
//
// 4shared implements OAuth 1.0, not 1.0a: the authorize callback carries only
// oauth_token and no oauth_verifier, so the callback's arrival is itself the
// completion signal and the access-token request must be signed with the request
// token alone. Verifier is therefore optional throughout — a 1.0a provider that
// does send one still works.
type Flow struct {
	Endpoints      Endpoints
	ConsumerKey    string
	ConsumerSecret string
	Client         *http.Client
	Debug          bool
}

// Initiate obtains temporary credentials and returns them with the URL the user
// must open to approve access. callback is the URL the provider redirects to;
// its host must match the registered application domain.
func (f Flow) Initiate(ctx context.Context, callback string) (reqToken, reqSecret, authorizeURL string, err error) {
	signer := &Signer{ConsumerKey: f.ConsumerKey, ConsumerSecret: f.ConsumerSecret, Debug: f.Debug}
	reqToken, reqSecret, err = f.postForm(ctx, signer, f.Endpoints.Initiate, map[string]string{"oauth_callback": callback})
	if err != nil {
		return "", "", "", fmt.Errorf("requesting temporary credentials: %w", err)
	}
	return reqToken, reqSecret, fmt.Sprintf("%s?oauth_token=%s", f.Endpoints.Authorize, url.QueryEscape(reqToken)), nil
}

// Exchange trades an authorized request token for the long-lived access token.
// verifier may be empty (OAuth 1.0); when present it is included (OAuth 1.0a).
func (f Flow) Exchange(ctx context.Context, reqToken, reqSecret, verifier string) (token, secret string, err error) {
	extra := map[string]string{}
	if verifier != "" {
		extra["oauth_verifier"] = verifier
	}
	signer := &Signer{
		ConsumerKey:    f.ConsumerKey,
		ConsumerSecret: f.ConsumerSecret,
		Token:          reqToken,
		TokenSecret:    reqSecret,
		Debug:          f.Debug,
	}
	token, secret, err = f.postForm(ctx, signer, f.Endpoints.Token, extra)
	if err != nil {
		return "", "", fmt.Errorf("exchanging request token for access token: %w", err)
	}
	return token, secret, nil
}

// postForm signs and sends an OAuth request and parses the form-encoded
// oauth_token / oauth_token_secret response common to all three steps.
func (f Flow) postForm(ctx context.Context, signer *Signer, rawURL string, extra map[string]string) (token, secret string, err error) {
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, nil)
	if err != nil {
		return "", "", err
	}
	signer.Sign(req, extra)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("provider returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		return "", "", fmt.Errorf("parsing response %q: %w", string(body), err)
	}
	token = vals.Get("oauth_token")
	secret = vals.Get("oauth_token_secret")
	if token == "" || secret == "" {
		return "", "", fmt.Errorf("response missing oauth_token/secret: %s", strings.TrimSpace(string(body)))
	}
	return token, secret, nil
}

// CallbackURL builds the redirect URL for a domain and port. The provider must
// accept the domain (4shared rejects localhost), and it must resolve to this
// machine so the local listener receives the redirect.
func CallbackURL(domain string, port int) string {
	if port == 80 {
		return fmt.Sprintf("http://%s/callback", domain)
	}
	return fmt.Sprintf("http://%s:%d/callback", domain, port)
}
