package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
