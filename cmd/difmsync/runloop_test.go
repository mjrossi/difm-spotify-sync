package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

// runnerFixture is a syncRunner over a real store whose await and loop
// are recorded fakes. The store is real for the same reason the consent
// tests use one: "the token is cleared before consent is re-entered" is
// a durable-state claim.
type runnerFixture struct {
	runner     syncRunner
	store      *sqlite.Store
	awaitCalls int
	loopCalls  int
	// tokenSeenByAwait records what await found stored when it ran.
	tokenSeenByAwait []string
	// tokenSeenByLoop records account.SpotifyRefreshToken as loop
	// received it each call — the "never a cached copy" claim.
	tokenSeenByLoop []string
}

func newRunnerFixture(t *testing.T, loopResults []error) *runnerFixture {
	t.Helper()
	_, store := newConsentFixture(t)
	f := &runnerFixture{store: store}
	f.runner = syncRunner{
		store: store,
		label: "default",
		log:   slog.New(slog.DiscardHandler),
		await: func(ctx context.Context, account sqlite.Account) error {
			f.awaitCalls++
			f.tokenSeenByAwait = append(f.tokenSeenByAwait, storedToken(t, store))
			// Consent "happens": the store gets a fresh token, the way
			// consentFlow.Complete writes one.
			return store.SetSpotifyRefreshToken(ctx, account.ID, fmt.Sprintf("token-%d", f.awaitCalls))
		},
		loop: func(_ context.Context, account sqlite.Account) error {
			f.loopCalls++
			f.tokenSeenByLoop = append(f.tokenSeenByLoop, account.SpotifyRefreshToken)
			if f.loopCalls > len(loopResults) {
				t.Fatalf("loop called %d times, only %d results scripted", f.loopCalls, len(loopResults))
			}
			return loopResults[f.loopCalls-1]
		},
	}
	return f
}

var errRevoked = fmt.Errorf("spotify search: %w",
	fmt.Errorf("%w (invalid_grant): %w", spotify.ErrGrantRevoked, spotify.ErrUnauthorized))

// The whole point: a revoked grant clears the stored token and goes
// back through the consent wait, then runs the loop again with the new
// token. No restart, no human-issued `difmsync auth`.
func TestSyncRunnerReentersConsentWhenTheGrantIsRevoked(t *testing.T) {
	ctx := context.Background()
	f := newRunnerFixture(t, []error{errRevoked, nil})
	if err := f.store.SetSpotifyRefreshToken(ctx, 1, "original"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	if err := f.runner.run(ctx); err != nil {
		t.Fatalf("run returned %v, want nil once the loop stops cleanly", err)
	}
	if f.loopCalls != 2 {
		t.Errorf("loop ran %d times, want 2 (once revoked, once after re-consent)", f.loopCalls)
	}
	if f.awaitCalls != 1 {
		t.Fatalf("await ran %d times, want 1", f.awaitCalls)
	}
	if f.tokenSeenByAwait[0] != "" {
		t.Errorf("await found token %q stored, want it cleared before consent is re-entered", f.tokenSeenByAwait[0])
	}
	if got := storedToken(t, f.store); got != "token-1" {
		t.Errorf("stored token after re-consent = %q, want token-1", got)
	}
	// Each loop call must key off a fresh read of the store, never a
	// cached copy from before the clear-and-reconsent.
	want := []string{"original", "token-1"}
	if diff := cmp.Diff(want, f.tokenSeenByLoop); diff != "" {
		t.Errorf("tokens seen by loop (-want +got):\n%s", diff)
	}
}

// A shutdown while waiting for consent is a clean stop, matching what
// Engine.Loop and the existing consent path return on cancellation.
func TestSyncRunnerReturnsNilWhenCanceledWhileAwaitingConsent(t *testing.T) {
	f := newRunnerFixture(t, nil)
	f.runner.await = func(ctx context.Context, _ sqlite.Account) error {
		f.awaitCalls++
		return context.Canceled
	}
	if err := f.runner.run(context.Background()); err != nil {
		t.Errorf("run returned %v, want nil", err)
	}
	if f.loopCalls != 0 {
		t.Errorf("loop ran %d times with no token, want 0", f.loopCalls)
	}
}

// Without a consent server there is nothing to wait on. The error must
// come back unchanged so the operator sees "run difmsync auth" rather
// than a wrapped mystery.
func TestSyncRunnerSurfacesMissingCredentials(t *testing.T) {
	f := newRunnerFixture(t, nil)
	f.runner.await = func(context.Context, sqlite.Account) error {
		return spotify.ErrNoCredentials
	}
	err := f.runner.run(context.Background())
	if !errors.Is(err, spotify.ErrNoCredentials) {
		t.Errorf("run returned %v, want ErrNoCredentials", err)
	}
}

var errLoop = errors.New("status server died")

// Any other loop error is returned as-is: the runner only knows how to
// recover from a revoked grant.
func TestSyncRunnerReturnsOtherLoopErrors(t *testing.T) {
	ctx := context.Background()
	f := newRunnerFixture(t, []error{errLoop})
	if err := f.store.SetSpotifyRefreshToken(ctx, 1, "original"); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	if err := f.runner.run(ctx); !errors.Is(err, errLoop) {
		t.Errorf("run returned %v, want the loop error unchanged", err)
	}
	if got := storedToken(t, f.store); got != "original" {
		t.Errorf("token = %q after an unrelated error, want it untouched", got)
	}
}

// await's contract is "returns nil once a token is stored". A fake that
// violates it (returns nil without writing one) must not send the
// runner into loop with an empty token — that would hand the engine a
// credential it can't use and blame Spotify for it.
func TestSyncRunnerErrorsWhenAwaitReturnsWithoutAToken(t *testing.T) {
	f := newRunnerFixture(t, nil)
	f.runner.await = func(context.Context, sqlite.Account) error {
		f.awaitCalls++
		return nil
	}
	err := f.runner.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no refresh token is stored") {
		t.Errorf(`run returned %v, want an error containing "no refresh token is stored"`, err)
	}
	if f.loopCalls != 0 {
		t.Errorf("loop ran %d times, want 0 — await's violation must not reach it", f.loopCalls)
	}
}

// A SIGTERM landing between the loop returning ErrGrantRevoked and the
// clear completing must not leave the dead token in the store, and must
// not turn a clean shutdown into a non-zero exit.
func TestSyncRunnerClearsTheTokenEvenWhenCanceledMidStep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := newRunnerFixture(t, nil)
	if err := f.store.SetSpotifyRefreshToken(context.Background(), 1, "original"); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	f.runner.loop = func(context.Context, sqlite.Account) error {
		f.loopCalls++
		cancel()
		return errRevoked
	}

	if err := f.runner.run(ctx); err != nil {
		t.Fatalf("run returned %v, want nil on a clean shutdown mid-clear", err)
	}
	if got := storedToken(t, f.store); got != "" {
		t.Errorf("stored token = %q, want cleared even though ctx was canceled first", got)
	}
}
