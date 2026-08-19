package spotify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// stubSource hands out a scripted sequence of tokens.
type stubSource struct {
	tokens []*oauth2.Token
	calls  int
}

func (s *stubSource) Token() (*oauth2.Token, error) {
	if s.calls >= len(s.tokens) {
		return s.tokens[len(s.tokens)-1], nil
	}
	tok := s.tokens[s.calls]
	s.calls++
	return tok, nil
}

func tok(refresh string) *oauth2.Token {
	return &oauth2.Token{
		AccessToken:  "access",
		RefreshToken: refresh,
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}
}

// Spotify can hand back a new refresh token on renewal. The oauth2
// transport keeps it in memory only, so without persistence the daemon
// works until it restarts and then presents a dead token — recoverable
// only by re-running the interactive consent step.
func TestRotatingTokenSourcePersistsANewRefreshToken(t *testing.T) {
	var persisted []string
	src := &rotatingTokenSource{
		src:       &stubSource{tokens: []*oauth2.Token{tok("old"), tok("new")}},
		last:      "old",
		onRefresh: func(s string) error { persisted = append(persisted, s); return nil },
	}

	if _, err := src.Token(); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	if len(persisted) != 0 {
		t.Errorf("persisted %v on an unchanged token; expected no write", persisted)
	}

	if _, err := src.Token(); err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if len(persisted) != 1 || persisted[0] != "new" {
		t.Fatalf("persisted = %v, want [new]", persisted)
	}

	// A repeat of the same value must not write again.
	if _, err := src.Token(); err != nil {
		t.Fatalf("third Token: %v", err)
	}
	if len(persisted) != 1 {
		t.Errorf("persisted = %v, want the rotation written exactly once", persisted)
	}
}

// If the new token cannot be stored, the old one may already be dead.
// Continuing would hide that until the next restart.
func TestRotatingTokenSourceSurfacesAPersistFailure(t *testing.T) {
	sentinel := errors.New("disk full")
	src := &rotatingTokenSource{
		src:       &stubSource{tokens: []*oauth2.Token{tok("new")}},
		last:      "old",
		onRefresh: func(string) error { return sentinel },
	}

	if _, err := src.Token(); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the persist failure surfaced", err)
	}
}

// A token carrying no refresh token at all must pass straight through.
func TestRotatingTokenSourceIgnoresAnEmptyRefreshToken(t *testing.T) {
	var called bool
	src := &rotatingTokenSource{
		src:       &stubSource{tokens: []*oauth2.Token{tok("")}},
		last:      "old",
		onRefresh: func(string) error { called = true; return nil },
	}

	if _, err := src.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if called {
		t.Error("onRefresh fired for a response with no refresh token")
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"30", 30 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"", 0},
		{"not-a-number", 0},
	}
	for _, tc := range tests {
		if got := parseRetryAfter(tc.in); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}

	// The HTTP-date form is the other documented spelling. http.TimeFormat
	// is the GMT spelling RFC 7231 requires and real servers send.
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got < 30*time.Second || got > 50*time.Second {
		t.Errorf("parseRetryAfter(%q) = %s, want ~45s", future, got)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Errorf("parseRetryAfter(past) = %s, want 0", got)
	}
}

// TestRevokedGrantIsTypedUnauthorized: token renewal happens inside the
// oauth2 transport, so a dead refresh token surfaces through a different
// path than every other failure this package types. Left untyped, the
// engine misses its abort branch and retries the whole batch against a
// credential that cannot work — logging one error per like while the
// real problem, "a human must re-consent", is never reported.
func TestRevokedGrantIsTypedUnauthorized(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Refresh token revoked"}`))
	}))
	defer tokenSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("API was reached despite a dead grant")
		w.WriteHeader(http.StatusOK)
	}))
	defer apiSrv.Close()

	a := &Authenticator{cfg: &oauth2.Config{
		ClientID:     "id",
		ClientSecret: "secret",
		Endpoint:     oauth2.Endpoint{TokenURL: tokenSrv.URL},
	}}

	c, err := a.Client(context.Background(), "dead-refresh-token", nil)
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	c.base = apiSrv.URL

	_, err = c.Search(context.Background(), "artist", "title", 5, "")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Search err = %v, want ErrUnauthorized", err)
	}
}

// TestTokenEndpointRateLimitIsTyped: a 429 from the token endpoint is a
// throttle, not a dead grant. Collapsing the two would send an operator
// to re-run interactive consent for a problem that resolves by waiting.
func TestTokenEndpointRateLimitIsTyped(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
	}))
	defer tokenSrv.Close()

	a := &Authenticator{cfg: &oauth2.Config{
		ClientID: "id", ClientSecret: "secret",
		Endpoint: oauth2.Endpoint{TokenURL: tokenSrv.URL},
	}}
	c, err := a.Client(context.Background(), "some-token", nil)
	if err != nil {
		t.Fatalf("Client: %v", err)
	}

	_, err = c.Search(context.Background(), "artist", "title", 5, "")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Search err = %v, want ErrRateLimited", err)
	}
	var rle *RateLimitError
	if errors.As(err, &rle) && rle.RetryAfter != 42*time.Second {
		t.Errorf("RetryAfter = %s, want 42s", rle.RetryAfter)
	}
}
