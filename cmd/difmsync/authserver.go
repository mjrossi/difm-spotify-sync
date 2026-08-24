package main

import (
	"context"
	"crypto/subtle"
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
	u, err := parseRedirect(redirect)
	if err != nil {
		return "", "", err
	}
	callback = u.Path
	if callback == "" || callback == "/" {
		// A pathless redirect URL would otherwise register "/" as the
		// callback, which is a catch-all in ServeMux and would swallow
		// the start route as well.
		return "", "", fmt.Errorf("redirect url %q needs a path (e.g. %s/difmsync/callback) "+
			"so the consent routes do not sit at the root", redirect, strings.TrimSuffix(redirect, "/"))
	}
	if strings.HasSuffix(callback, "/") {
		// Same failure as the pathless case, one level down: a trailing
		// slash is a subtree pattern in ServeMux, so this handler would
		// swallow every path beneath it — including the unrouted-request
		// logger, which is the only evidence an operator gets that a
		// proxy rewrote the path.
		return "", "", fmt.Errorf("redirect url %q must not end in a slash: a trailing "+
			"slash registers a subtree pattern that swallows the other consent routes "+
			"(use %scallback)", redirect, redirect)
	}
	start = path.Join(path.Dir(callback), "start")
	if start == callback {
		// A callback already named "start" collides with the sibling
		// derived from it, and ServeMux panics on a duplicate pattern.
		// That panic would fire in the daemon's work goroutine after the
		// process is up — the crash loop this whole server exists to
		// eliminate, re-armed by a config value rather than by a missing
		// token. Refused here, where it is one legible startup error.
		return "", "", fmt.Errorf("redirect url %q must not end in /start: the consent "+
			"start page is derived as the callback's sibling and would collide with it "+
			"(use a path ending in %s)", redirect, path.Join(path.Dir(callback), "callback"))
	}
	return start, callback, nil
}

// consentPortMismatch reports a redirect URL that cannot reach the
// listener. A loopback redirect means the browser connects to the port
// directly — nothing terminates TLS in front of 127.0.0.1 — so the two
// ports have to agree, and when they do not the operator gets a start URL
// that refuses the connection and a callback Spotify cannot deliver.
//
// It is a warning rather than a refusal because a container can publish
// its port under a different number (127.0.0.1:8888:3437 is a valid
// mapping for exactly this pair), and the process cannot see the mapping
// from inside. A non-loopback host means a proxy is involved and the
// ports are expected to differ, so that case says nothing.
func consentPortMismatch(redirect, addr string) (redirectPort, listenPort string, mismatch bool) {
	u, err := url.Parse(redirect)
	if err != nil || u.Host == "" {
		// consentRoutes has already rejected these; nothing to add.
		return "", "", false
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return "", "", false
	}
	redirectPort = u.Port()
	if redirectPort == "" {
		redirectPort = "80"
		if u.Scheme == "https" {
			redirectPort = "443"
		}
	}
	_, listenPort, err = net.SplitHostPort(addr)
	// Port 0 is "any free port", which only a test asks for; it has no
	// fixed number to disagree with.
	if err != nil || listenPort == "" || listenPort == "0" || listenPort == redirectPort {
		return "", "", false
	}
	return redirectPort, listenPort, true
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
	// Constant-time, though the realistic attack is guessing rather than
	// timing 128 bits of hex over HTTP. It costs one stdlib call, and the
	// alternative is a guard that is correct only by argument.
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(s.nonce)) != 1 {
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
	err := completeConsentRequest(w, r, s.flow,
		"Authorized. difmsync will begin syncing on its next tick; you can close this tab.",
		"\n\nStart the flow again from the URL in the container logs.")
	if err != nil {
		// Left running on failure: a mistyped consent, a denied grant or a
		// transient exchange error should be retryable by clicking the
		// start URL again, not require restarting the container. The
		// listener therefore narrows only on success, below — the same
		// condition as `done`, not a second one.
		s.log.Error("consent server: flow failed", "err", err)
		return
	}
	select {
	case s.done <- struct{}{}:
	default:
	}
}

// consentPollInterval is how often the consent wait re-checks the store
// for a token it did not write itself.
//
// Ten seconds because the thing being waited on is a human with a
// browser: the cost of a miss is that much extra staring at a terminal,
// and the cost of the poll is one indexed row read against a database
// nothing else is touching yet.
//
// A var rather than a const only so the test that proves the poll works
// does not have to take ten seconds to do it. It is read once, when the
// ticker is created.
var consentPollInterval = 10 * time.Second

// awaitConsent serves the consent routes until a refresh token is stored
// or ctx is canceled, then shuts the listener down for the lifetime of
// the process.
//
// There is no timeout, unlike the `auth` subcommand's five minutes. That
// command holds a port on a workstation and should give it back; this one
// is a daemon whose entire job is blocked until consent happens, and an
// operator who deploys on Friday and clicks the link on Monday should
// find it still waiting rather than a container that gave up quietly.
//
// It also returns when a token appears in the store by some other route,
// which is what makes every out-of-band path work — `difmsync auth
// --manual` run in a sidecar, or a database restored from backup. The
// listener is not the only way consent can happen, and a daemon that
// only ever noticed its own callback would sit waiting on a URL nobody
// was going to open.
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

	srv, err := serveHTTP(ctx, addr, "the consent endpoints", s.handler())
	if err != nil {
		return err
	}
	// Deferred at this scope deliberately: the listener must die when
	// awaitConsent returns, which is what makes the consent server exist
	// only while there is no refresh token. Moving this anywhere wider
	// leaves an unauthenticated token-writing endpoint up for the life of
	// the process. TestAwaitConsentCompletesAndClosesTheListener pins it.
	defer srv.stop(log)

	// A loopback redirect whose port is not this listener's cannot
	// complete: the browser connects straight to the redirect's port, so
	// the start URL below refuses the connection and Spotify's callback
	// lands nowhere — which is indistinguishable from Spotify never
	// calling back. Said here, immediately above the URL it invalidates,
	// because that log line is where the operator is already looking.
	if want, got, mismatch := consentPortMismatch(redirect, addr); mismatch {
		log.Warn("consent redirect port does not match the consent listener; "+
			"the flow can only complete if something forwards one port to the other "+
			"(a published container port does; nothing else here will)",
			"redirect_port", want, "listen_port", got,
			"redirect", redirect, "auth_http_addr", addr)
	}

	// One line, at Info, carrying the whole instruction. This is the only
	// place the nonce is ever emitted, and an operator reading it is by
	// definition looking at a daemon that cannot sync yet.
	log.Info("spotify consent required — open this URL to authorize",
		"url", s.startURL(redirect), "listening", srv.Addr.String())

	poll := time.NewTicker(consentPollInterval)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-srv.Err:
			if err != nil {
				return fmt.Errorf("consent server: %w", err)
			}
			return errors.New("consent server stopped before consent completed")
		case <-s.done:
			log.Info("spotify consent stored; starting sync")
			return nil
		case <-poll.C:
			// A read failure is logged and the wait continues. The
			// database is the same one the daemon is about to sync
			// against, so a persistent failure will resurface loudly
			// there; killing the consent wait over one transient error
			// would discard a listener an operator may be seconds away
			// from using.
			stored, err := flow.tokenStored(ctx)
			if err != nil {
				log.Warn("consent server: could not check for a stored token", "err", err)
				continue
			}
			if stored {
				log.Info("spotify consent was stored elsewhere; starting sync")
				return nil
			}
		}
	}
}
