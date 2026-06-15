// Package oauth1 implements the subset of OAuth 1.0a (RFC 5849) needed to sign
// HTTP requests with HMAC-SHA1. It is provider-agnostic: 4shared uses it today,
// and any future provider that speaks OAuth 1.0a can reuse it by constructing a
// Signer with the relevant consumer and token credentials.
package oauth1

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Signer holds the credentials used to sign requests. Token/TokenSecret may be
// empty during the temporary-credential ("request token") step of the flow.
type Signer struct {
	ConsumerKey    string
	ConsumerSecret string
	Token          string
	TokenSecret    string

	// Debug, when true, logs the signature base string and Authorization header
	// for each signed request. Enable via the provider's debug env switch.
	Debug bool
}

// Sign computes the OAuth signature for the request and sets its Authorization
// header. extra carries protocol parameters that are not yet stored on the
// Signer for this particular call — e.g. oauth_callback on the request-token
// step or oauth_verifier on the access-token step. Only form bodies
// (application/x-www-form-urlencoded) contribute their parameters to the
// signature base string, per RFC 5849 §3.4.1.3; binary upload bodies do not.
func (s *Signer) Sign(req *http.Request, extra map[string]string) {
	oauth := map[string]string{
		"oauth_consumer_key":     s.ConsumerKey,
		"oauth_nonce":            nonce(),
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        strconv.FormatInt(time.Now().Unix(), 10),
		"oauth_version":          "1.0",
	}
	if s.Token != "" {
		oauth["oauth_token"] = s.Token
	}
	for k, v := range extra {
		oauth[k] = v
	}

	// Parameters that contribute to the signature: oauth_* plus query-string
	// params plus form-body params.
	params := map[string]string{}
	for k, v := range oauth {
		params[k] = v
	}
	for k, vs := range req.URL.Query() {
		if len(vs) > 0 {
			params[k] = vs[0]
		}
	}
	if req.Header.Get("Content-Type") == "application/x-www-form-urlencoded" && req.Form != nil {
		for k, vs := range req.Form {
			if len(vs) > 0 {
				params[k] = vs[0]
			}
		}
	}

	oauth["oauth_signature"] = s.signature(req.Method, baseURL(req.URL), params)

	// Build the Authorization header from the oauth_* params only.
	var parts []string
	keys := sortedKeys(oauth)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", percentEncode(k), percentEncode(oauth[k])))
	}
	header := "OAuth " + strings.Join(parts, ", ")
	req.Header.Set("Authorization", header)

	if s.Debug {
		slog.Info("oauth1 signed request",
			"method", req.Method,
			"url", baseURL(req.URL),
			"token", s.Token,
			"authorization", header,
		)
	}
}

func (s *Signer) signature(method, baseURL string, params map[string]string) string {
	var pairs []string
	for _, k := range sortedKeys(params) {
		pairs = append(pairs, percentEncode(k)+"="+percentEncode(params[k]))
	}
	base := strings.Join([]string{
		strings.ToUpper(method),
		percentEncode(baseURL),
		percentEncode(strings.Join(pairs, "&")),
	}, "&")

	if s.Debug {
		slog.Info("oauth1 signature base string", "base", base)
	}

	key := percentEncode(s.ConsumerSecret) + "&" + percentEncode(s.TokenSecret)
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(base))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// baseURL returns the scheme://host/path form used in the signature base
// string, with the query and any default port removed (RFC 5849 §3.4.1.2).
func baseURL(u *url.URL) string {
	host := strings.ToLower(u.Host)
	if (u.Scheme == "https" && strings.HasSuffix(host, ":443")) ||
		(u.Scheme == "http" && strings.HasSuffix(host, ":80")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	return strings.ToLower(u.Scheme) + "://" + host + u.Path
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// percentEncode applies RFC 3986 unreserved-character encoding, as OAuth
// requires (net/url's encoders differ on spaces and a few other characters).
func percentEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}

func nonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
