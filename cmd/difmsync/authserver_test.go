package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

// TestConsentRoutesDeriveFromTheRedirectURL pins the relationship the
// deployment depends on: the path registered with Spotify is the path
// served, and the start page is its sibling rather than a claim on the
// root of a hostname that may front other services.
func TestConsentRoutesDeriveFromTheRedirectURL(t *testing.T) {
	for _, tc := range []struct {
		name, redirect, start, callback string
		wantErr                         bool
	}{
		{
			name:     "scoped under a prefix",
			redirect: "https://nas.tail1234.ts.net/difmsync/callback",
			start:    "/difmsync/start",
			callback: "/difmsync/callback",
		},
		{
			name:     "at the root",
			redirect: "http://127.0.0.1:3437/callback",
			start:    "/start",
			callback: "/callback",
		},
		{
			// A pathless redirect URL would register "/" as the callback,
			// which is a ServeMux catch-all and would swallow the start
			// route with it. Refused outright rather than defaulted.
			name:     "pathless is refused",
			redirect: "https://nas.tail1234.ts.net",
			wantErr:  true,
		},
		{
			// A trailing slash is a subtree pattern in ServeMux, so the
			// callback handler would swallow everything beneath it —
			// including the unrouted-request logger that is the only
			// evidence a proxy rewrote the path.
			name:     "trailing slash is refused",
			redirect: "https://nas.tail1234.ts.net/difmsync/",
			wantErr:  true,
		},
		{
			// The start page is derived as the callback's sibling, so a
			// callback already named start makes the two paths identical
			// and ServeMux panics on the duplicate registration.
			name:     "callback named start is refused",
			redirect: "https://nas.tail1234.ts.net/difmsync/start",
			wantErr:  true,
		},
		{
			name:     "callback named start at the root is refused",
			redirect: "https://nas.tail1234.ts.net/start",
			wantErr:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, callback, err := consentRoutes(tc.redirect)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("consentRoutes(%q) = %q,%q,nil; want an error", tc.redirect, start, callback)
				}
				return
			}
			if err != nil {
				t.Fatalf("consentRoutes(%q): %v", tc.redirect, err)
			}
			if diff := cmp.Diff([]string{tc.start, tc.callback}, []string{start, callback}); diff != "" {
				t.Errorf("routes (-want +got):\n%s", diff)
			}
		})
	}
}

// newTestServer builds a consent server whose flow never reaches Spotify.
// Only the routing and the guards are under test here; the exchange
// itself belongs to pkg/spotify.
func newTestServer(t *testing.T, redirect string) *consentServer {
	t.Helper()
	start, callback, err := consentRoutes(redirect)
	if err != nil {
		t.Fatalf("consentRoutes: %v", err)
	}
	return &consentServer{
		// A real Authenticator, so AuthURL builds a genuine consent URL.
		// It never reaches the network: only the redirect to Spotify is
		// exercised here, not the exchange.
		flow: &consentFlow{
			auth:  spotify.NewAuthenticator("client-id", "client-secret", redirect),
			state: "test-state",
		},
		log:       discardLogger(),
		nonce:     "test-nonce",
		startPath: start, callbackPath: callback,
		done: make(chan struct{}, 1),
	}
}

// TestStartRequiresTheNonce is the guard that makes an unauthenticated
// listener defensible at all. Without it anyone who can reach the port
// during the consent window can complete the flow with their own Spotify
// account and bind the sync to a stranger's playlist.
func TestStartRequiresTheNonce(t *testing.T) {
	s := newTestServer(t, "https://nas.tail1234.ts.net/difmsync/callback")
	h := s.handler()

	for _, tc := range []struct {
		name, target string
		wantStatus   int
	}{
		{"no nonce", "/difmsync/start", http.StatusNotFound},
		{"wrong nonce", "/difmsync/start?t=guessed", http.StatusNotFound},
		{"correct nonce", "/difmsync/start?t=test-nonce", http.StatusFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))
			if rec.Code != tc.wantStatus {
				t.Errorf("GET %s = %d, want %d", tc.target, rec.Code, tc.wantStatus)
			}
			// A rejected start must not leak the consent URL in a Location
			// header, which would hand over exactly what the nonce withholds.
			if tc.wantStatus == http.StatusNotFound && rec.Header().Get("Location") != "" {
				t.Errorf("rejected start carried a Location header: %q", rec.Header().Get("Location"))
			}
		})
	}
}

// TestRoutesAnswerBothPrefixedAndBareForms covers the proxy-stripping
// ambiguity. `tailscale serve --set-path` forwards the full path in
// current releases, but that has changed across versions; guessing wrong
// 404s the callback, which is indistinguishable from Spotify never
// calling back. Both forms answer, and both are still guarded.
func TestRoutesAnswerBothPrefixedAndBareForms(t *testing.T) {
	s := newTestServer(t, "https://nas.tail1234.ts.net/difmsync/callback")
	h := s.handler()

	for _, target := range []string{"/difmsync/start?t=test-nonce", "/start?t=test-nonce"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusFound {
			t.Errorf("GET %s = %d, want %d", target, rec.Code, http.StatusFound)
		}
	}

	// The bare callback is reachable too, and still rejects a bad state.
	for _, target := range []string{"/difmsync/callback?state=wrong", "/callback?state=wrong"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want %d", target, rec.Code, http.StatusBadRequest)
		}
	}
}

// TestCallbackRejectsAStateMismatch pins the guard that protects the one
// route the nonce cannot cover — Spotify redirects the browser there and
// will not carry an extra parameter, so the OAuth state is the whole
// defense.
func TestCallbackRejectsAStateMismatch(t *testing.T) {
	s := newTestServer(t, "https://nas.tail1234.ts.net/difmsync/callback")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/difmsync/callback?state=forged&code=abc", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged state = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	// Nothing signaled: a rejected callback must leave the daemon waiting
	// rather than let it proceed as though consent had been given.
	select {
	case <-s.done:
		t.Error("a forged callback signaled consent complete")
	default:
	}
}

// TestStartURLUsesTheRedirectOrigin is what makes the logged link
// clickable. The daemon listens on plain HTTP inside a container while the
// browser reaches it through whatever terminates TLS in front, so a URL
// built from the listen address would be unreachable from anywhere the
// operator actually is.
func TestStartURLUsesTheRedirectOrigin(t *testing.T) {
	redirect := "https://nas.tail1234.ts.net/difmsync/callback"
	s := newTestServer(t, redirect)

	got := s.startURL(redirect)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Scheme != "https" || u.Host != "nas.tail1234.ts.net" {
		t.Errorf("start URL origin = %s://%s, want https://nas.tail1234.ts.net", u.Scheme, u.Host)
	}
	if u.Path != "/difmsync/start" {
		t.Errorf("start URL path = %q, want /difmsync/start", u.Path)
	}
	if u.Query().Get("t") != "test-nonce" {
		t.Errorf("start URL nonce = %q, want test-nonce", u.Query().Get("t"))
	}
}

// TestUnroutedRequestsAreLogged covers the diagnostic that keeps a proxy
// path rewrite from looking like silence from Spotify.
func TestUnroutedRequestsAreLogged(t *testing.T) {
	var sb strings.Builder
	s := newTestServer(t, "https://nas.tail1234.ts.net/difmsync/callback")
	s.log = slog.New(slog.NewTextHandler(&sb, nil))

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rewritten/callback", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("unrouted request = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if !strings.Contains(sb.String(), "/rewritten/callback") {
		t.Errorf("unrouted request was not logged with its path; got:\n%s", sb.String())
	}
}

// TestAwaitConsentStopsOnContextCancel pins that a shutdown while waiting
// for consent unwinds rather than hanging. There is deliberately no
// timeout on this listener, so cancellation is the only way out.
func TestAwaitConsentStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- awaitConsent(ctx, "127.0.0.1:0",
			"http://127.0.0.1:3437/callback",
			&consentFlow{
				auth:  spotify.NewAuthenticator("client-id", "client-secret", "http://127.0.0.1:3437/callback"),
				state: "s",
			}, discardLogger())
	}()
	cancel()
	if err := <-done; err == nil {
		t.Error("awaitConsent returned nil after cancellation, want the context error")
	}
}

// TestConsentPortMismatchWarnsOnlyWhenItCannotWork covers the check that
// catches a redirect URL and a listener that were configured
// independently — the state the shipped defaults were in, where the
// logged start URL named a port nothing served. It has to stay quiet for
// the proxy case, where differing ports are the normal arrangement.
func TestConsentPortMismatchWarnsOnlyWhenItCannotWork(t *testing.T) {
	for _, tc := range []struct {
		name, redirect, addr string
		want                 bool
	}{
		{
			name:     "loopback ports disagree",
			redirect: "http://127.0.0.1:8888/callback",
			addr:     "0.0.0.0:3437",
			want:     true,
		},
		{
			name:     "loopback ports agree",
			redirect: "http://127.0.0.1:3437/callback",
			addr:     "0.0.0.0:3437",
		},
		{
			// The deployed arrangement: a proxy terminates TLS on 443 and
			// forwards to the container's port. Differing is the point.
			name:     "proxied hostname says nothing",
			redirect: "https://nas.tail1234.ts.net/difmsync/callback",
			addr:     "0.0.0.0:3437",
		},
		{
			// A bare loopback redirect means port 80, which is not 3437.
			name:     "implicit port still counts",
			redirect: "http://127.0.0.1/callback",
			addr:     "0.0.0.0:3437",
			want:     true,
		},
		{
			name:     "any free port has nothing to disagree with",
			redirect: "http://127.0.0.1:8888/callback",
			addr:     "127.0.0.1:0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, got, mismatch := consentPortMismatch(tc.redirect, tc.addr)
			if mismatch != tc.want {
				t.Fatalf("consentPortMismatch(%q, %q) = %q,%q,%v; want mismatch=%v",
					tc.redirect, tc.addr, want, got, mismatch, tc.want)
			}
		})
	}
}

// listenAddrLogger captures the address awaitConsent reports itself
// listening on. That log line is the only place the bound port is
// published, and reading it there is what lets the test ask for port 0
// rather than gamble on a fixed one being free.
type listenAddrLogger struct {
	slog.Handler
	addrs chan string
}

// Enabled is overridden rather than inherited: the wrapped handler
// discards, and slog asks Enabled before it calls Handle at all.
func (h *listenAddrLogger) Enabled(context.Context, slog.Level) bool { return true }

func (h *listenAddrLogger) Handle(ctx context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if a.Key != "listening" {
			return true
		}
		select {
		case h.addrs <- a.Value.String():
		default:
		}
		return false
	})
	return h.Handler.Handle(ctx, r)
}

// TestAwaitConsentCompletesAndClosesTheListener is the property the
// daemon's whole exception rests on: the consent server exists only until
// consent is stored, and then not at all. Nothing covered the successful
// path — only cancellation, which is the exit nobody depends on.
func TestAwaitConsentCompletesAndClosesTheListener(t *testing.T) {
	const redirect = "http://127.0.0.1:3437/callback"
	flow, store := newConsentFixture(t)
	endpoint := &tokenEndpoint{
		body: `{"access_token":"at","token_type":"Bearer","refresh_token":"rt-2","expires_in":3600}`,
	}
	// Carried into the handler by the server's BaseContext, which is how
	// the exchange stays off the network without the callback path
	// growing a seam the production code does not have.
	ctx, cancel := context.WithCancel(exchangeContext(endpoint))
	defer cancel()

	addrs := make(chan string, 1)
	log := slog.New(&listenAddrLogger{Handler: slog.DiscardHandler, addrs: addrs})

	done := make(chan error, 1)
	go func() { done <- awaitConsent(ctx, "127.0.0.1:0", redirect, flow, log) }()

	addr := <-addrs
	resp, err := http.Get("http://" + addr + "/callback?state=the-state&code=good")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback = %d (%s), want %d", resp.StatusCode, body, http.StatusOK)
	}
	// Neither the code nor the state is echoed back to the browser, which
	// is a page an operator may well leave open.
	if strings.Contains(string(body), "the-state") || strings.Contains(string(body), "good") {
		t.Errorf("callback response echoed flow parameters: %s", body)
	}

	if err := <-done; err != nil {
		t.Fatalf("awaitConsent: %v", err)
	}
	if got := storedToken(t, store); got != "rt-2" {
		t.Errorf("stored refresh token = %q, want rt-2", got)
	}
	// The listener is gone for the life of the process, not merely
	// ignored: a port still accepting connections after consent is the
	// widening of the exception CLAUDE.md forbids.
	if conn, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		_ = conn.Close()
		t.Errorf("consent listener still accepting connections on %s after consent", addr)
	}
}

// freePort returns an address nothing is listening on, using the same
// bind-and-release trick as TestServeWhileServesUntilWorkStops: the test
// needs the address before the code under test creates its listener.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("probe close: %v", err)
	}
	return addr
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing came up on %s", addr)
}

// TestSyncExitsCleanWhenCanceledAwaitingConsent pins one half of the
// process contract against the other. Engine.Loop treats a canceled
// context as a clean stop and exits 0; a daemon still waiting for consent
// has to agree, or SIGTERM on a freshly deployed container reads as a
// crash to whatever watches exit codes — which under a restart policy is
// the difference between "stopped" and "failing".
func TestSyncExitsCleanWhenCanceledAwaitingConsent(t *testing.T) {
	clearEnv(t)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- newApp().Run(ctx, []string{
			"difmsync", "--db-path", filepath.Join(t.TempDir(), "consent.db"),
			"--log-level", "error",
			"sync", "--loop",
			"--auth-http-addr", addr,
			// Matched to the listener, which is also what keeps the
			// port-mismatch warning quiet: this is the shape the shipped
			// defaults now have.
			"--spotify-redirect-url", "http://" + addr + "/callback",
			"--api-key", "k", "--member-id", "1", "--playlist-id", "PL1",
			"--spotify-client-id", "id", "--spotify-client-secret", "secret",
		})
	}()

	// Waiting for the port rather than sleeping: the cancellation has to
	// land while the consent server is up, which is the state under test.
	waitForListener(t, addr)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("sync --loop returned %v after cancellation, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sync --loop did not return after the context was canceled")
	}
}

// TestAwaitConsentReturnsWhenATokenIsStoredElsewhere covers the reason
// the wait polls the store at all.
//
// The daemon's listener is not the only way consent can happen: `difmsync
// auth --manual` run in a sidecar writes to the same database, and so
// does restoring a backup. Before the poll existed, awaitConsent returned
// only on its own callback, so any of those stored a working refresh
// token and left the daemon waiting forever on a URL nobody was going to
// open — with the account row already authorized, which is what made it
// hard to see.
func TestAwaitConsentReturnsWhenATokenIsStoredElsewhere(t *testing.T) {
	const redirect = "http://127.0.0.1:3437/callback"
	flow, store := newConsentFixture(t)

	restore := consentPollInterval
	consentPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { consentPollInterval = restore })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addrs := make(chan string, 1)
	log := slog.New(&listenAddrLogger{Handler: slog.DiscardHandler, addrs: addrs})

	done := make(chan error, 1)
	go func() { done <- awaitConsent(ctx, "127.0.0.1:0", redirect, flow, log) }()

	addr := <-addrs
	waitForListener(t, addr)

	// The write another process would have made. Deliberately not through
	// flow.Complete: the point is that the wait ends on evidence it did
	// not produce itself.
	if err := store.SetSpotifyRefreshToken(context.Background(), flow.accountID, "rt-elsewhere"); err != nil {
		t.Fatalf("SetSpotifyRefreshToken: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("awaitConsent: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("awaitConsent never noticed a token stored outside its own flow")
	}

	// Same obligation as the callback path: the listener goes away for
	// the life of the process once there is a token, however it arrived.
	if conn, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		_ = conn.Close()
		t.Errorf("consent listener still accepting connections on %s after consent", addr)
	}
}
