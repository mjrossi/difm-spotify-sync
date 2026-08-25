package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

// Errors the callback handlers branch on. Both entry points — the `auth`
// subcommand's loopback listener and the daemon's consent server — report
// these to a browser as well as to the operator, so they are worded to be
// read by someone who is looking at a tab rather than a log.
var (
	errStateMismatch  = errors.New("oauth state mismatch — start the flow again")
	errNoRefreshToken = errors.New("spotify returned no refresh token; revoke the app's access and retry")
)

// consentFlow is the half of the Spotify Authorization Code exchange that
// does not depend on how the browser reached us.
//
// It exists because there are now two callers with the same security
// obligations and very different transports: `difmsync auth` binds a
// loopback listener derived from the redirect URL, while the daemon
// serves a path on a long-lived port behind a TLS-terminating proxy.
// Duplicating the exchange across them means duplicating the CSRF check
// and the empty-refresh-token guard, and a divergence in either is
// invisible until it matters — a state check that is skipped on one path
// still passes every test that exercises the other.
type consentFlow struct {
	auth      *spotify.Authenticator
	store     *sqlite.Store
	accountID int64

	// state is the CSRF token echoed by Spotify. Generated per flow, not
	// per process: restarting the daemon must invalidate a consent URL
	// that was captured but never used.
	state string
}

func newConsentFlow(auth *spotify.Authenticator, store *sqlite.Store, accountID int64) (*consentFlow, error) {
	state, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("generate oauth state: %w", err)
	}
	return &consentFlow{auth: auth, store: store, accountID: accountID, state: state}, nil
}

// AuthURL is the Spotify consent URL for this flow, carrying its state.
func (f *consentFlow) AuthURL() string { return f.auth.AuthURL(f.state) }

// Complete validates a callback's query string, exchanges the code, and
// persists the refresh token. It is the only place a refresh token is
// written during consent, so the ordering is fixed here rather than left
// to each caller: validate, exchange, check, store.
//
// A nil return means the token is durable — callers may shut their
// listener down on it and not before.
func (f *consentFlow) Complete(ctx context.Context, q url.Values) error {
	// State first, before anything in the query is used for anything.
	if q.Get("state") != f.state {
		return errStateMismatch
	}
	// Spotify reports a declined consent as a query parameter on an
	// otherwise ordinary callback, so this is not an error path the
	// transport can detect for us.
	if e := q.Get("error"); e != "" {
		return fmt.Errorf("spotify consent denied: %s", e)
	}
	tok, err := f.auth.Exchange(ctx, q.Get("code"))
	if err != nil {
		return err
	}
	// An access token without a refresh token leaves a daemon that works
	// until the first expiry and then cannot recover headlessly, which is
	// the one failure this whole step exists to prevent. Refuse it here
	// rather than storing an empty string that reads as "never authorized".
	if tok.RefreshToken == "" {
		return errNoRefreshToken
	}
	return f.store.SetSpotifyRefreshToken(ctx, f.accountID, tok.RefreshToken)
}

// tokenStored reports whether a refresh token has landed for this
// account, by any route.
//
// The daemon's consent wait polls this so consent given somewhere else —
// `difmsync auth --manual` in a sidecar, a restored database — ends the
// wait. It reads the store rather than tracking a flag on the flow
// precisely because the write it is looking for may not have come from
// this flow at all.
func (f *consentFlow) tokenStored(ctx context.Context) (bool, error) {
	return f.store.HasSpotifyRefreshToken(ctx, f.accountID)
}

// randomToken returns 16 bytes of hex, used for both the OAuth state and
// the consent server's start nonce.
func randomToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// completeConsentRequest runs the exchange for one callback request and
// writes the browser's side of it.
//
// Two transports land here — the loopback listener `difmsync auth` binds and
// the daemon's consent server — and both need the same three things: the
// exchange must survive the browser hanging up, a failure must reach the
// person looking at the tab, and success must say so. Only the follow-on
// differs, so only the follow-on stays at the call site: `auth` reports to a
// terminal, the daemon logs and signals its wait.
//
// ok is the success text. hint is appended to a failure, for the caller that
// can tell the operator how to retry.
//
// The context is WithoutCancel because a browser that closes the tab the
// instant the callback lands would otherwise cancel the token exchange
// mid-flight — losing a consent the operator has already given, with nothing
// to show for it. It keeps the request context's values either way.
func completeConsentRequest(w http.ResponseWriter, r *http.Request, flow *consentFlow, ok, hint string) error {
	if err := flow.Complete(context.WithoutCancel(r.Context()), r.URL.Query()); err != nil {
		http.Error(w, "Consent failed: "+err.Error()+hint, http.StatusBadRequest)
		return err
	}
	fmt.Fprintln(w, ok)
	return nil
}

// parseRedirect is the one parse of DIFMSYNC_SPOTIFY_REDIRECT_URL. It
// enforces only what every consumer needs — that it parses, and that it
// carries a host — because that is genuinely all they agree on.
//
// The path rules deliberately do NOT live here, and the two callers reach
// opposite verdicts on the same input:
//
//   - callbackTarget (auth.go) defaults a pathless URL to "/". Its listener
//     is single-purpose and shuts down after one callback, so a catch-all is
//     harmless — and Spotify really will call back to "/", which a hardcoded
//     "/callback" would 404.
//   - consentRoutes (authserver.go) refuses a pathless URL. Its mux carries
//     the start route and the unrouted-request logger too, and "/" in a
//     ServeMux swallows both.
//
// Both are right for their transport. Unifying them would mean picking one
// and breaking the other, so what is shared is the prefix and the error text
// — which had already been written out twice, identically.
func parseRedirect(redirect string) (*url.URL, error) {
	u, err := url.Parse(redirect)
	if err != nil {
		return nil, fmt.Errorf("parse redirect url %q: %w", redirect, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("redirect url %q has no host", redirect)
	}
	return u, nil
}
