package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

// syncRunner is the daemon's outer loop: wait for consent if there is no
// refresh token, build an engine, run it, and — the reason this is a
// type rather than a closure — start over when Spotify revokes the
// grant. Before it existed the consent server ran only at boot, so a
// token revoked mid-life (a password change, a revoked app
// authorization) left the daemon failing identically every tick until a
// human ran `difmsync auth` and restarted the container.
//
// The two funcs are what the sync command supplies; a test supplies
// fakes. Both receive the account as read from the store immediately
// before the call, never a cached copy.
type syncRunner struct {
	store *sqlite.Store
	label string
	log   *slog.Logger

	// await blocks until a refresh token is stored — by its own consent
	// server, or by anything else that writes the row — and returns
	// spotify.ErrNoCredentials when there is no consent server to run.
	await func(ctx context.Context, account sqlite.Account) error
	// loop builds an engine for the account and runs it until ctx is
	// canceled or the grant is revoked.
	loop func(ctx context.Context, account sqlite.Account) error
}

func (r syncRunner) run(ctx context.Context) error {
	for {
		account, err := r.store.GetAccount(ctx, r.label)
		if err != nil {
			// A shutdown landing here — most likely right after the
			// clear below sends the loop back to the top — is a clean
			// stop, not a failure, same as a shutdown during await.
			// Without this the non-zero exit just moves one line down
			// from where it used to be.
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		if account.SpotifyRefreshToken == "" {
			if err := r.await(ctx, account); err != nil {
				// A shutdown while waiting is a clean stop, not a
				// failure — the same verdict Engine.Loop reaches on a
				// canceled context. Without this the process contract
				// disagrees with itself: an authorized daemon exits 0
				// on SIGTERM and one still waiting for consent exits 1,
				// which reads as a crash to anything watching exit
				// codes.
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}
			// Re-read rather than patching the local copy. The token
			// was written through the store, and everything below keys
			// off this struct.
			account, err = r.store.GetAccount(ctx, r.label)
			if err != nil {
				return err
			}
			if account.SpotifyRefreshToken == "" {
				// await's contract is "returns nil once a token is
				// stored". Looping on a violation would spin.
				return errors.New("consent reported complete but no refresh token is stored")
			}
		}

		err = r.loop(ctx, account)
		if !errors.Is(err, spotify.ErrGrantRevoked) {
			return err
		}
		// Clear before waiting, so the consent server's invariant —
		// it exists only while there is no refresh token — stays
		// literal, and so `difmsync status` reports "awaiting consent"
		// rather than a stale "authorized". awaitConsent polls the
		// store, so `auth --manual` in a sidecar remains a way out.
		r.log.Error("Spotify revoked the refresh token; consent is required again",
			"err", err)
		// Unconditional, not a compare-and-clear keyed on this account
		// copy. A token written by `auth --manual` in a sidecar between
		// Spotify revoking the grant and this goroutine noticing gets
		// cleared too, because the engine that hit ErrGrantRevoked was
		// still holding the old one — that copy has no way to tell a
		// rotation apart from the revocation it already observed. A CAS
		// would be wrong here: after a rotation the copy is stale, the
		// compare matches nothing, the dead-per-this-copy token survives
		// uncleared, and consent is never re-entered. One extra consent
		// round trip is cheaper than that.
		//
		// WithoutCancel, as FinishRun's close does: this is a tiny local
		// write that must land even when shutdown arrives mid-step,
		// otherwise the next boot starts with the same dead token and
		// does a jittered failing pass before ever reaching consent.
		if err := r.store.SetSpotifyRefreshToken(context.WithoutCancel(ctx), account.ID, ""); err != nil {
			return fmt.Errorf("clear revoked refresh token: %w", err)
		}
	}
}
