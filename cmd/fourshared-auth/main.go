// Command fourshared-auth performs the one-time OAuth 1.0a authorization dance
// for a single 4shared account and prints the access token / token secret to
// paste into .env. Run it once per 4shared account:
//
//	go run ./cmd/fourshared-auth -account 1
//
// It reads the app consumer key/secret and callback domain from .env
// (FOURSHARED_CONSUMER_KEY / FOURSHARED_CONSUMER_SECRET / FOURSHARED_CONSUMER_DOMAIN),
// starts a tiny local web server, and opens an authorize URL. After you approve
// access in the browser, 4shared redirects back to the callback, the local
// server captures the verifier, and it is exchanged for a long-lived access
// token automatically.
//
// Why a domain is needed: 4shared rejects "localhost" as an Application domain
// and its out-of-band ("PIN") page is broken. The reliable setup is to register
// a real domain you control (e.g. backmeup.syncsystem.net) as the app's
// Application domain, point it at 127.0.0.1 (A record or hosts file), and set
// FOURSHARED_CONSUMER_DOMAIN to it. The browser then resolves that domain to
// this machine and the callback reaches the local server. See the README.
//
// If the callback never reaches the local server (e.g. the domain points
// elsewhere), you can still copy the oauth_verifier value out of the browser's
// address bar and paste it into the prompt. Use -manual for the PIN flow.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/syncsystem-net/back-me-up/internal/provider/oauth1"
)

const (
	initiateURL  = "https://api.4shared.com/v1_2/oauth/initiate"
	authorizeURL = "https://api.4shared.com/v1_2/oauth/authorize"
	tokenURL     = "https://api.4shared.com/v1_2/oauth/token"
)

func main() {
	key := flag.String("key", "", "4shared consumer key (defaults to FOURSHARED_CONSUMER_KEY in .env)")
	secret := flag.String("secret", "", "4shared consumer secret (defaults to FOURSHARED_CONSUMER_SECRET in .env)")
	accountIndex := flag.Int("account", 1, "the FOURSHARED_ACCOUNT_<n> index these tokens are for (used in the printed .env keys)")
	port := flag.Int("port", 8723, "local port the callback listener binds on this machine")
	domainFlag := flag.String("domain", "", "callback domain matching the registered 4shared Application domain (defaults to FOURSHARED_CONSUMER_DOMAIN in .env)")
	manual := flag.Bool("manual", false, "use the out-of-band PIN flow instead of a callback")
	flag.Parse()

	_ = godotenv.Load(".env")
	// Prefer per-account credentials (FOURSHARED_ACCOUNT_<n>_CONSUMER_*), falling
	// back to the shared FOURSHARED_CONSUMER_* keys. Each 4shared account is
	// normally authorized through its own registered application.
	if *key == "" {
		*key = firstNonEmpty(
			os.Getenv(fmt.Sprintf("FOURSHARED_ACCOUNT_%d_CONSUMER_KEY", *accountIndex)),
			os.Getenv("FOURSHARED_CONSUMER_KEY"),
		)
	}
	if *secret == "" {
		*secret = firstNonEmpty(
			os.Getenv(fmt.Sprintf("FOURSHARED_ACCOUNT_%d_CONSUMER_SECRET", *accountIndex)),
			os.Getenv("FOURSHARED_CONSUMER_SECRET"),
		)
	}
	if *key == "" || *secret == "" {
		fmt.Fprintf(os.Stderr, "error: consumer key/secret required (set FOURSHARED_ACCOUNT_%d_CONSUMER_KEY/SECRET in .env or pass -key/-secret)\n", *accountIndex)
		os.Exit(1)
	}

	domain := *domainFlag
	if domain == "" {
		domain = firstNonEmpty(
			os.Getenv(fmt.Sprintf("FOURSHARED_ACCOUNT_%d_CONSUMER_DOMAIN", *accountIndex)),
			os.Getenv("FOURSHARED_CONSUMER_DOMAIN"),
		)
	}
	if domain == "" {
		domain = "localhost"
	}

	client := &http.Client{Timeout: 30 * time.Second}
	ctx := context.Background()

	if *manual {
		runManual(ctx, client, *key, *secret, *accountIndex)
		return
	}
	runCallback(ctx, client, *key, *secret, *accountIndex, domain, *port)
}

// callbackURL builds the OAuth callback URL the browser is redirected to. Its
// host must match the registered 4shared Application domain; its address must
// resolve to this machine so the local listener receives it.
func callbackURL(domain string, port int) string {
	if port == 80 {
		return fmt.Sprintf("http://%s/callback", domain)
	}
	return fmt.Sprintf("http://%s:%d/callback", domain, port)
}

// runCallback runs a local web server that captures the verifier from the
// redirect. As a fallback it also reads a verifier pasted on stdin, in case the
// browser redirect doesn't reach the local server.
func runCallback(ctx context.Context, client *http.Client, key, secret string, accountIndex int, domain string, port int) {
	callback := callbackURL(domain, port)

	// Bind the listener first so the port is ready before we authorize.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		fail("starting local callback server", fmt.Errorf("port %d: %w (try -port <other> or -manual)", port, err))
	}

	// Step 1: temporary credentials.
	signer := &oauth1.Signer{ConsumerKey: key, ConsumerSecret: secret}
	reqToken, reqSecret, err := postForm(ctx, client, signer, http.MethodPost, initiateURL, map[string]string{"oauth_callback": callback})
	if err != nil {
		fail("requesting temporary credentials", err)
	}

	// Step 2: authorize in the browser; the redirect lands on our server.
	// 4shared implements OAuth 1.0 (not 1.0a), so the callback carries only
	// oauth_token and NO oauth_verifier — its arrival is itself the completion
	// signal. We still capture oauth_verifier if a (1.0a) provider sends one.
	verifierCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query().Get("oauth_verifier")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<h2>BackMeUp: 4shared authorized.</h2><p>You can close this tab and return to the terminal.</p>")
		select {
		case verifierCh <- v:
		default:
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	authURL := fmt.Sprintf("%s?oauth_token=%s", authorizeURL, url.QueryEscape(reqToken))
	fmt.Printf("\nCallback configured as: %s\n", callback)
	fmt.Printf("\nOpening your browser to authorize. If it doesn't open, paste this URL:\n\n   %s\n\n", authURL)
	openBrowser(authURL)
	fmt.Println("Waiting for the browser callback...")
	fmt.Println("(If the browser instead lands on a page whose address bar contains")
	fmt.Println(" 'oauth_verifier=XXXX', copy that XXXX value, paste it here and press Enter.)")

	// Read a pasted verifier as a fallback, concurrently with the callback.
	stdinCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			stdinCh <- line
		}
	}()

	var verifier string
	select {
	case verifier = <-verifierCh:
		fmt.Println("\nCallback received.")
	case verifier = <-stdinCh:
	case <-time.After(5 * time.Minute):
		fail("waiting for authorization", fmt.Errorf("timed out after 5 minutes"))
	}

	// Step 3: exchange the authorized request token for the access token. Under
	// OAuth 1.0 there is no verifier; include it only if one was provided
	// (OAuth 1.0a), otherwise the access-token request is signed with the
	// request token alone.
	extra := map[string]string{}
	if verifier != "" {
		extra["oauth_verifier"] = verifier
	}
	signer = &oauth1.Signer{ConsumerKey: key, ConsumerSecret: secret, Token: reqToken, TokenSecret: reqSecret}
	accToken, accSecret, err := postForm(ctx, client, signer, http.MethodPost, tokenURL, extra)
	if err != nil {
		fail("exchanging request token for access token", err)
	}
	printResult(accountIndex, accToken, accSecret)
}

// runManual uses the out-of-band PIN flow (last-resort fallback).
func runManual(ctx context.Context, client *http.Client, key, secret string, accountIndex int) {
	signer := &oauth1.Signer{ConsumerKey: key, ConsumerSecret: secret}
	reqToken, reqSecret, err := postForm(ctx, client, signer, http.MethodPost, initiateURL, map[string]string{"oauth_callback": "oob"})
	if err != nil {
		fail("requesting temporary credentials", err)
	}

	fmt.Println("\n1. Open this URL, log in to the 4shared account you want to authorize, and approve access:")
	fmt.Printf("\n   %s?oauth_token=%s\n", authorizeURL, url.QueryEscape(reqToken))
	fmt.Print("\n2. Copy the verification code (PIN) shown, paste it here and press Enter:\n\n   PIN: ")
	pin, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	pin = strings.TrimSpace(pin)
	if pin == "" {
		fail("reading PIN", fmt.Errorf("no PIN entered"))
	}

	signer = &oauth1.Signer{ConsumerKey: key, ConsumerSecret: secret, Token: reqToken, TokenSecret: reqSecret}
	accToken, accSecret, err := postForm(ctx, client, signer, http.MethodPost, tokenURL, map[string]string{"oauth_verifier": pin})
	if err != nil {
		fail("exchanging verifier for access token", err)
	}
	printResult(accountIndex, accToken, accSecret)
}

func printResult(accountIndex int, token, secret string) {
	fmt.Printf("\nSuccess. Add these lines to your .env:\n\n")
	fmt.Printf("FOURSHARED_ACCOUNT_%d_OAUTH_TOKEN=%s\n", accountIndex, token)
	fmt.Printf("FOURSHARED_ACCOUNT_%d_OAUTH_TOKEN_SECRET=%s\n\n", accountIndex, secret)
}

// debug is enabled by FOURSHARED_DEBUG and logs the raw OAuth responses, which
// is essential for diagnosing token problems (e.g. what /token actually returns).
var debug = os.Getenv("FOURSHARED_DEBUG") != ""

// postForm signs and sends an OAuth request and parses the form-encoded
// oauth_token / oauth_token_secret response common to all three OAuth steps.
func postForm(ctx context.Context, c *http.Client, signer *oauth1.Signer, method, rawURL string, extra map[string]string) (token, secret string, err error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return "", "", err
	}
	signer.Debug = debug
	signer.Sign(req, extra)
	resp, err := c.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if debug {
		fmt.Printf("[debug] POST %s -> %d\n[debug] raw response: %s\n\n", rawURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("4shared returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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

// openBrowser best-effort opens a URL in the default browser.
func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func fail(stage string, err error) {
	fmt.Fprintf(os.Stderr, "error %s: %v\n", stage, err)
	os.Exit(1)
}
