// Package reauth runs the OAuth re-authorization dance from inside the running
// server and writes the resulting token straight to the encrypted store.
//
// It exists because OAuth 1.0 has no refresh-token concept: when 4shared answers
// 401.0301 the only remedy is to authorize again. Before credentials lived in
// the database that meant stopping the server, running cmd/fourshared-auth,
// pasting two hex strings into .env and starting up again. Now it is a button.
//
// The flow spans a browser round trip, so it is start/poll rather than one long
// request — the same shape as the Auto-Sync crawl, and for the same reason: the
// page keeps refreshing while the user is off approving access.
package reauth

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/credentials"
	"github.com/syncsystem-net/back-me-up/internal/provider/oauth1"
)

// Supported reports whether a provider authenticates in a way that can be
// re-authorized at all.
//
// It matters beyond the button: MEGA answers a wrong password with the same
// ErrAuthExpired a rejected 4shared token produces (see the phase-2 note on
// MEGA's generic ENOENT), so without this check a mistyped MEGA password would
// flag the account as "needs re-authorization" — a state MEGA has no way to
// leave, since there is no OAuth token to replace.
func Supported(providerName string) bool {
	return providerName == string(accounts.ProviderFourShared)
}

// Phase is where a re-authorization run has got to.
type Phase string

const (
	PhaseIdle       Phase = "idle"
	PhaseStarting   Phase = "starting"   // asking the provider for temporary credentials
	PhaseAwaiting   Phase = "awaiting"   // waiting for the user to approve in the browser
	PhaseExchanging Phase = "exchanging" // trading the request token for an access token
	PhaseDone       Phase = "done"
	PhaseError      Phase = "error"
	PhaseCancelled  Phase = "cancelled"
)

// Status is the snapshot the UI polls.
type Status struct {
	Phase    Phase  `json:"phase"`
	Provider string `json:"provider,omitempty"`
	Email    string `json:"email,omitempty"`
	IsMain   bool   `json:"is_main,omitempty"`
	// AuthorizeURL is shown as a clickable link for the whole run. The server also
	// tries to open the default browser, but a failed auto-open (headless machine,
	// no default browser) must never be a dead end.
	AuthorizeURL string `json:"authorize_url,omitempty"`
	Message      string `json:"message,omitempty"`
	Error        string `json:"error,omitempty"`
}

// Config is the flow's environment.
type Config struct {
	CallbackPort int
	Timeout      time.Duration
	// OpenBrowser is best-effort browser launching, a field so tests do not spawn
	// a browser on the machine running them.
	OpenBrowser func(url string)
	// Endpoints defaults to 4shared's. It is a field for the same reason the
	// connect seams elsewhere are: this package is defined by what it does across
	// a provider round trip, which is unreachable without one to talk to.
	Endpoints oauth1.Endpoints
	Client    *http.Client
}

// Manager runs at most one re-authorization at a time. One at a time is a real
// constraint, not simplification: the callback listener binds a fixed port that
// the provider's registered application knows about, and two runs would fight
// over it.
type Manager struct {
	creds *credentials.Manager
	cfg   Config

	mu     sync.Mutex
	status Status
	cancel context.CancelFunc
	// running guards the port: a second Start while one is in flight is refused
	// rather than failing later with "address already in use".
	running bool
}

func New(creds *credentials.Manager, cfg Config) *Manager {
	if cfg.CallbackPort <= 0 {
		cfg.CallbackPort = 8723
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Minute
	}
	if cfg.OpenBrowser == nil {
		cfg.OpenBrowser = openBrowser
	}
	if cfg.Endpoints == (oauth1.Endpoints{}) {
		cfg.Endpoints = oauth1.FourSharedEndpoints
	}
	return &Manager{creds: creds, cfg: cfg, status: Status{Phase: PhaseIdle}}
}

// Status returns the current snapshot.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Cancel stops an in-flight run and releases the port.
func (m *Manager) Cancel() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Start begins re-authorizing one account. It returns as soon as the authorize
// URL is known, so the caller can hand the user a link; the rest runs in the
// background and is observed through Status.
func (m *Manager) Start(providerName, email string, isMain bool) (Status, error) {
	if m.creds.Locked() {
		return Status{}, fmt.Errorf("%w: %s", accounts.ErrLocked, m.creds.LockReason())
	}
	if !Supported(providerName) {
		// MEGA is password login; there is nothing to re-authorize. Saying so beats
		// starting a flow that cannot end.
		return Status{}, fmt.Errorf("%s accounts do not use OAuth and cannot be re-authorized", providerName)
	}

	key, domain, err := m.consumerFor(providerName, email, isMain)
	if err != nil {
		return Status{}, err
	}

	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return Status{}, fmt.Errorf("a re-authorization is already in progress for %s — finish or cancel it first", m.status.Email)
	}
	m.running = true
	m.status = Status{Phase: PhaseStarting, Provider: providerName, Email: email, IsMain: isMain,
		Message: "Asking 4shared for a temporary token…"}
	m.mu.Unlock()

	// The listener is bound before anything is requested, so a port already in use
	// is reported immediately rather than after the user has approved access and
	// the callback has nowhere to land.
	addr := fmt.Sprintf("127.0.0.1:%d", m.cfg.CallbackPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		m.fail(fmt.Errorf("could not bind the callback listener on %s: %w (change reauth.callback_port in config.yml, or stop whatever is using it)", addr, err))
		return m.Status(), err
	}

	callback := oauth1.CallbackURL(domain, m.cfg.CallbackPort)
	flow := oauth1.Flow{
		Endpoints:      m.cfg.Endpoints,
		ConsumerKey:    key.ConsumerKey,
		ConsumerSecret: key.ConsumerSecret,
		Client:         m.cfg.Client,
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.Timeout)
	// Published before the first request, so Cancel works during Initiate too —
	// that call reaches the provider over the network and can hang.
	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()

	reqToken, reqSecret, authURL, err := flow.Initiate(ctx, callback)
	if err != nil {
		cancel()
		ln.Close()
		m.fail(err)
		return m.Status(), err
	}

	m.mu.Lock()
	m.status.Phase = PhaseAwaiting
	m.status.AuthorizeURL = authURL
	m.status.Message = "Approve access in the browser. If it did not open, use the link below."
	started := m.status
	m.mu.Unlock()

	go m.await(ctx, cancel, ln, flow, reqToken, reqSecret, providerName, email, isMain)
	m.cfg.OpenBrowser(authURL)
	return started, nil
}

// await serves the callback, exchanges the token and stores it.
//
// The listener is closed before the status settles, not after. The status is
// what the UI watches to decide the run is over, so settling first would let a
// user press Re-authorize again in the moment before the port is released and
// hit "address already in use".
func (m *Manager) await(ctx context.Context, cancel context.CancelFunc, ln net.Listener, flow oauth1.Flow,
	reqToken, reqSecret, providerName, email string, isMain bool) {
	defer cancel()

	phase, message, err := m.run(ctx, ln, flow, reqToken, reqSecret, providerName, email, isMain)
	if err != nil {
		m.fail(err)
		return
	}
	m.settle(phase, message)
}

// run performs the flow and releases the listener before returning, so the
// caller can publish an outcome only once the port is genuinely free.
func (m *Manager) run(ctx context.Context, ln net.Listener, flow oauth1.Flow,
	reqToken, reqSecret, providerName, email string, isMain bool) (Phase, string, error) {

	verifierCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<h2>BackMeUp: 4shared authorized.</h2><p>You can close this tab and return to BackMeUp.</p>")
		select {
		case verifierCh <- r.URL.Query().Get("oauth_verifier"):
		default:
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	var verifier string
	select {
	case verifier = <-verifierCh:
	case <-ctx.Done():
		// A cancelled run and an expired one are different things to the user: one
		// they did, the other happened to them.
		if ctx.Err() == context.DeadlineExceeded {
			return "", "", fmt.Errorf("timed out waiting for approval in the browser")
		}
		return PhaseCancelled, "Re-authorization cancelled.", nil
	}

	m.mu.Lock()
	m.status.Phase = PhaseExchanging
	m.status.Message = "Exchanging the approval for a token…"
	m.mu.Unlock()

	// The exchange must not inherit the wait's deadline pressure; it is a single
	// request and the user has already done their part.
	exchangeCtx, exchangeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer exchangeCancel()
	token, secret, err := flow.Exchange(exchangeCtx, reqToken, reqSecret, verifier)
	if err != nil {
		return "", "", err
	}

	if err := m.creds.SaveOAuthToken(providerName, email, token, secret, isMain); err != nil {
		return "", "", fmt.Errorf("storing the new token: %w", err)
	}
	slog.Info("account re-authorized; new token stored", "provider", providerName, "email", email, "main", isMain)
	return PhaseDone, "Re-authorized. The new token is stored and in use.", nil
}

// consumerFor resolves the registered application's consumer credentials and
// callback domain for one account. They are part of the stored credentials, so a
// re-authorization needs no .env lookup — which is the point: the button has to
// work without the user editing files.
func (m *Manager) consumerFor(providerName, email string, isMain bool) (oauth1.Signer, string, error) {
	store := m.creds.Store()
	var key, secret, domain string
	if isMain {
		mn, ok := store.MainFor(accounts.ProviderType(providerName))
		if !ok {
			return oauth1.Signer{}, "", fmt.Errorf("no main account configured for %s", providerName)
		}
		key, secret, domain = mn.ConsumerKey, mn.ConsumerSecret, mn.ConsumerDomain
	} else {
		a, ok := store.Find(providerName, email)
		if !ok {
			return oauth1.Signer{}, "", fmt.Errorf("no stored account for %s/%s", providerName, email)
		}
		key, secret, domain = a.ConsumerKey, a.ConsumerSecret, a.ConsumerDomain
	}

	if key == "" || secret == "" {
		return oauth1.Signer{}, "", fmt.Errorf("this account has no registered application credentials; set its CONSUMER_KEY and CONSUMER_SECRET in .env and restart")
	}
	if domain == "" {
		// 4shared rejects localhost as an application domain, so there is no usable
		// default to fall back to.
		return oauth1.Signer{}, "", fmt.Errorf("this account has no callback domain; set its CONSUMER_DOMAIN in .env (it must be the domain registered with the 4shared application, pointed at 127.0.0.1) and restart")
	}
	return oauth1.Signer{ConsumerKey: key, ConsumerSecret: secret}, domain, nil
}

func (m *Manager) fail(err error) {
	slog.Warn("re-authorization failed", "error", err)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.Phase = PhaseError
	m.status.Error = err.Error()
	m.status.Message = ""
	m.running = false
	m.cancel = nil
}

func (m *Manager) settle(phase Phase, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.Phase = phase
	m.status.Message = message
	m.status.Error = ""
	m.running = false
	m.cancel = nil
}

// openBrowser best-effort opens a URL in the default browser on the machine
// running the server. Failure is not reported: the authorize URL is always shown
// as a link, which is the reliable path.
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
