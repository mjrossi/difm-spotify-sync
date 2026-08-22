package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

// tokenEndpoint stands in for accounts.spotify.com, so the exchange is
// exercised for real without a network. It counts its calls, which is
// what pins the ordering inside Complete: a callback that fails the state
// check must not reach the token endpoint with the code it was handed.
type tokenEndpoint struct {
	calls  int
	status int
	body   string
}

func (e *tokenEndpoint) RoundTrip(r *http.Request) (*http.Response, error) {
	e.calls++
	status := e.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(e.body)),
		Request:    r,
	}, nil
}

// exchangeContext points the oauth2 exchange at the stub rather than at
// Spotify. Complete takes its context from the transport that called it,
// so this is the same seam both entry points already use.
func exchangeContext(e *tokenEndpoint) context.Context {
	return context.WithValue(context.Background(), oauth2.HTTPClient,
		&http.Client{Transport: e})
}

// newConsentFixture builds a flow over a real database. The store is real
// on purpose: "the token is durable when Complete returns nil" is the
// property the callers depend on when they shut their listener down, and
// a stub store cannot prove it.
func newConsentFixture(t *testing.T) (*consentFlow, *sqlite.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "consent.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	account, err := store.EnsureAccount(ctx, "default", "1", "PL1")
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}
	return &consentFlow{
		auth: spotify.NewAuthenticator("client-id", "client-secret",
			"http://127.0.0.1:3437/callback"),
		store:     store,
		accountID: account.ID,
		state:     "the-state",
	}, store
}

func storedToken(t *testing.T, store *sqlite.Store) string {
	t.Helper()
	account, err := store.GetAccount(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	return account.SpotifyRefreshToken
}

// TestConsentFlowComplete is the test CLAUDE.md's justification for
// consentFlow asks for. The type exists so that two entry points cannot
// drift on the state check and the empty-token guard; nothing enforced
// that until these ran against Complete itself rather than through one
// transport.
func TestConsentFlowComplete(t *testing.T) {
	for _, tc := range []struct {
		name     string
		query    url.Values
		endpoint tokenEndpoint
		wantErr  error
		// wantExchange is whether the token endpoint should be reached at
		// all, not how often: oauth2 probes a second client-auth style
		// when the first is rejected, so the count is its business.
		wantExchange bool
		wantToken    string
	}{
		{
			// The CSRF guard, checked before anything else in the query
			// is used for anything — including before the exchange, which
			// is why wantCalls is zero rather than unasserted.
			name:    "forged state never reaches the token endpoint",
			query:   url.Values{"state": {"forged"}, "code": {"stolen"}},
			wantErr: errStateMismatch,
		},
		{
			// Spotify reports a declined consent as a parameter on an
			// otherwise ordinary callback, so nothing below the transport
			// can detect it for us.
			name:  "a denied grant is not an exchange",
			query: url.Values{"state": {"the-state"}, "error": {"access_denied"}},
		},
		{
			// An access token with no refresh token leaves a daemon that
			// works until the first expiry and then cannot recover
			// headlessly — the one failure this step exists to prevent.
			name:         "an access token without a refresh token is refused",
			query:        url.Values{"state": {"the-state"}, "code": {"good"}},
			endpoint:     tokenEndpoint{body: `{"access_token":"at","token_type":"Bearer"}`},
			wantErr:      errNoRefreshToken,
			wantExchange: true,
		},
		{
			name:         "a rejected exchange stores nothing",
			query:        url.Values{"state": {"the-state"}, "code": {"expired"}},
			endpoint:     tokenEndpoint{status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`},
			wantExchange: true,
		},
		{
			name:  "consent completes and the token is durable",
			query: url.Values{"state": {"the-state"}, "code": {"good"}},
			endpoint: tokenEndpoint{
				body: `{"access_token":"at","token_type":"Bearer","refresh_token":"rt-1","expires_in":3600}`,
			},
			wantExchange: true,
			wantToken:    "rt-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flow, store := newConsentFixture(t)
			endpoint := tc.endpoint

			err := flow.Complete(exchangeContext(&endpoint), tc.query)

			switch {
			case tc.wantToken != "":
				if err != nil {
					t.Fatalf("Complete: %v", err)
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Complete = %v, want %v", err, tc.wantErr)
				}
			default:
				if err == nil {
					t.Fatal("Complete = nil, want an error")
				}
			}
			if exchanged := endpoint.calls > 0; exchanged != tc.wantExchange {
				t.Errorf("reached the token endpoint = %v (%d calls), want %v",
					exchanged, endpoint.calls, tc.wantExchange)
			}
			// Every failing case must leave the account unauthorized: a
			// stored empty string and a never-stored token are the same
			// state, and anything else would be a token written on a path
			// that reported failure.
			if got := storedToken(t, store); got != tc.wantToken {
				t.Errorf("stored refresh token = %q, want %q", got, tc.wantToken)
			}
		})
	}
}
