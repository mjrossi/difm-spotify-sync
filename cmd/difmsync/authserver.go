package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// consentServer is the daemon's answer to the one step that cannot run
// headless. Rather than exiting when there is no refresh token — which
// under `restart: unless-stopped` is a crash loop, not a prompt — the
// loop stands this up, logs a URL, and waits.
//
// It is deliberately NOT part of the status server, and CLAUDE.md's
// operator-surface rule is written to keep it that way. /healthz and
// /status.json are read-only and unauthenticated because nothing they
// serve can change anything; this listener writes a refresh token, so it
// gets its own port, its own guard, and a lifetime that ends the moment
// it has done its one job.
type consentServer struct {
	flow  *consentFlow
	log   *slog.Logger
	nonce string

	// startPath and callbackPath come from the redirect URL, so the path
	// registered with Spotify and the path served here cannot drift.
	startPath    string
	callbackPath string

	// done carries the first successful consent. Buffered, so a callback
	// handler never blocks on a receiver that has already gone away.
	done chan struct{}
}

// consentRoutes derives the served paths from the redirect URL.
//
// The callback path is whatever the redirect URL carries, because that is
// what Spotify will request. The start path is its sibling, so scoping
// the redirect URL to /difmsync/callback puts the start page at
// /difmsync/start rather than claiming /start at the root of a hostname
// that may front several services.
func consentRoutes(redirect string) (start, callback string, err error) {
	u, err := url.Parse(redirect)
	if err != nil {
		return "", "", fmt.Errorf("parse redirect url %q: %w", redirect, err)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("redirect url %q has no host", redirect)
	}
	callback = u.Path
	if callback == "" || callback == "/" {
		// A pathless redirect URL would otherwise register "/" as the
		// callback, which is a catch-all in ServeMux and would swallow
		// the start route as well.
		return "", "", fmt.Errorf("redirect url %q needs a path (e.g. %s/difmsync/callback) "+
			"so the consent routes do not sit at the root", redirect, strings.TrimSuffix(redirect, "/"))
	}
	return path.Join(path.Dir(callback), "start"), callback, nil
}

// startURL is the link an operator clicks. Built from the redirect URL's
// origin rather than from the listen address: the daemon listens on plain
// HTTP inside a container, while the browser reaches it through whatever
// terminates TLS in front (tailscale serve, a reverse proxy), and only
// the redirect URL knows that public origin.
func (s *consentServer) startURL(redirect string) string {
	u, err := url.Parse(redirect)
	if err != nil {
		// consentRoutes already parsed this; unreachable in practice.
		return s.startPath + "?t=" + s.nonce
	}
	u.Path = s.startPath
	u.RawQuery = url.Values{"t": {s.nonce}}.Encode()
	return u.String()
}

func (s *consentServer) handler() http.Handler {
	mux := http.NewServeMux()

	// Both routes are registered twice when the redirect URL is scoped to
	// a subpath: once at the full path and once at its last segment.
	//
	// This is not belt and braces for its own sake. `tailscale serve
	// --set-path /difmsync` forwards the full path to the backend in
	// current releases, but whether a path-mounted proxy strips its
	// prefix has changed across versions and is the subject of open
	// upstream issues. Guessing wrong produces a 404 on the callback,
	// which is indistinguishable from Spotify never calling back — the
	// exact silent failure callbackTarget's path derivation was written
	// to prevent. Answering on both costs nothing: the handlers validate
	// state and nonce regardless of which pattern matched.
	register := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, h)
		if base := "/" + path.Base(pattern); base != pattern {
			mux.HandleFunc(base, h)
		}
	}
	register(s.startPath, s.handleStart)
	register(s.callbackPath, s.handleCallback)

	// Anything else is logged rather than silently 404'd, so a proxy that
	// rewrote the path leaves evidence in the place an operator is
	// already looking.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.log.Warn("consent server: unrouted request",
			"path", r.URL.Path, "expect_start", s.startPath, "expect_callback", s.callbackPath)
		http.NotFound(w, r)
	})
	return mux
}

// handleStart begins the flow. The nonce is what keeps this from being an
// open redirect into a consent screen: without it, anyone who can reach
// the port during the unauthenticated window could complete the flow with
// their own Spotify account and bind the sync to a stranger's playlist.
func (s *consentServer) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("t") != s.nonce {
		// Deliberately a 404 rather than a 403: an unauthenticated caller
		// learns nothing about whether the path exists.
		s.log.Warn("consent server: start rejected, bad or missing nonce", "path", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, s.flow.AuthURL(), http.StatusFound)
}

// handleCallback completes the flow. It is reachable without the nonce by
// necessity — Spotify redirects the browser here and will not carry an
// extra parameter — so the OAuth state is what guards it, which is the
// standard protection and the same one the `auth` subcommand relies on.
func (s *consentServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	// WithoutCancel because a browser that closes the tab the instant the
	// callback lands would otherwise cancel the token exchange mid-flight,
	// losing a consent the operator has already given — and the daemon
	// would go back to waiting with no sign of why.
	err := s.flow.Complete(context.WithoutCancel(r.Context()), r.URL.Query())
	if err != nil {
		// Left running on failure: a mistyped consent, a denied grant or a
		// transient exchange error should be retryable by clicking the
		// start URL again, not require restarting the container.
		s.log.Error("consent server: flow failed", "err", err)
		http.Error(w, "Consent failed: "+err.Error()+
			"\n\nStart the flow again from the URL in the container logs.",
			http.StatusBadRequest)
		return
	}
	fmt.Fprintln(w, "Authorized. difmsync will begin syncing on its next tick; "+
		"you can close this tab.")
	select {
	case s.done <- struct{}{}:
	default:
	}
}

// awaitConsent serves the consent routes until a refresh token is stored
// or ctx is canceled, then shuts the listener down for the lifetime of
// the process.
//
// There is no timeout, unlike the `auth` subcommand's five minutes. That
// command holds a port on a workstation and should give it back; this one
// is a daemon whose entire job is blocked until consent happens, and an
// operator who deploys on Friday and clicks the link on Monday should
// find it still waiting rather than a container that gave up quietly.
func awaitConsent(ctx context.Context, addr, redirect string, flow *consentFlow, log *slog.Logger) error {
	start, callback, err := consentRoutes(redirect)
	if err != nil {
		return err
	}
	nonce, err := randomToken()
	if err != nil {
		return fmt.Errorf("generate consent nonce: %w", err)
	}
	s := &consentServer{
		flow: flow, log: log, nonce: nonce,
		startPath: start, callbackPath: callback,
		done: make(chan struct{}, 1),
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s for the consent endpoints: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("consent server shutdown", "err", err)
		}
	}()

	// One line, at Info, carrying the whole instruction. This is the only
	// place the nonce is ever emitted, and an operator reading it is by
	// definition looking at a daemon that cannot sync yet.
	log.Info("spotify consent required — open this URL to authorize",
		"url", s.startURL(redirect), "listening", ln.Addr().String())

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("consent server: %w", err)
		}
		return errors.New("consent server stopped before consent completed")
	case <-s.done:
		log.Info("spotify consent stored; starting sync")
		return nil
	}
}
