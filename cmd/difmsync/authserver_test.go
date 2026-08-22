package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
