package main

import (
	"strings"
	"testing"
)

// TestManualCallbackQueryAcceptsWhatPeopleActuallyPaste covers the two
// halves of the parser's contract: it is lenient about the shape of a
// paste and strict about its content.
func TestManualCallbackQueryAcceptsWhatPeopleActuallyPaste(t *testing.T) {
	for _, tc := range []struct {
		name, pasted, code, state string
		wantErr                   bool
	}{
		{
			name:   "the whole url",
			pasted: "http://127.0.0.1:3437/callback?code=AQD123&state=abc",
			code:   "AQD123", state: "abc",
		},
		{
			name:   "an https url from behind a proxy",
			pasted: "https://nas.tail1234.ts.net/difmsync/callback?code=AQD123&state=abc",
			code:   "AQD123", state: "abc",
		},
		{
			// Some people copy from the query string onward.
			name:   "the query string alone",
			pasted: "?code=AQD123&state=abc",
			code:   "AQD123", state: "abc",
		},
		{
			name:   "surrounding whitespace and quotes",
			pasted: "  \"http://127.0.0.1:3437/callback?code=AQD123&state=abc\"  ",
			code:   "AQD123", state: "abc",
		},
		{
			name:   "a trailing fragment",
			pasted: "http://127.0.0.1:3437/callback?code=AQD123&state=abc#_=_",
			code:   "AQD123", state: "abc",
		},
		{
			// A declined grant is still a callback worth carrying back:
			// Complete turns it into a legible error rather than a state
			// mismatch.
			name:   "a denied consent",
			pasted: "http://127.0.0.1:3437/callback?error=access_denied&state=abc",
			state:  "abc",
		},
		{
			// The mistake the error message exists for. Accepting it would
			// mean inventing a state, which deletes the CSRF guard on this
			// path only.
			name:    "a bare authorization code is refused",
			pasted:  "AQD123",
			wantErr: true,
		},
		{
			name:    "nothing pasted is refused",
			pasted:  "   ",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := manualCallbackQuery(tc.pasted)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("manualCallbackQuery(%q) = %v, nil; want an error", tc.pasted, q)
				}
				return
			}
			if err != nil {
				t.Fatalf("manualCallbackQuery(%q): %v", tc.pasted, err)
			}
			if got := q.Get("code"); got != tc.code {
				t.Errorf("code = %q, want %q", got, tc.code)
			}
			if got := q.Get("state"); got != tc.state {
				t.Errorf("state = %q, want %q", got, tc.state)
			}
		})
	}
}

// TestBareCodeErrorExplainsWhy pins the wording, not just the failure. A
// bare code otherwise reaches Complete and comes back as "oauth state
// mismatch — start the flow again", which sends the operator to redo a
// flow that worked, rather than to paste more of what is already on their
// screen.
func TestBareCodeErrorExplainsWhy(t *testing.T) {
	_, err := manualCallbackQuery("AQD123")
	if err == nil {
		t.Fatal("a bare code was accepted")
	}
	for _, want := range []string{"whole URL", "state"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestParseErrorsDoNotEchoThePaste keeps credential material out of the
// error text on the one path where it arrives as an argument. What gets
// pasted here is an authorization code — by definition in the bare-code
// case — and an error an operator cannot fix is an error they paste into
// an issue. Single-use and useless without the client secret, so this is
// hygiene rather than a hole; it is also the only place in the codebase
// where a credential could reach a string, which is why it is pinned.
func TestParseErrorsDoNotEchoThePaste(t *testing.T) {
	for _, tc := range []struct{ name, pasted string }{
		{"bare code", "AQD-secret-code-123"},
		{"unparseable", "%zz-secret-code-123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := manualCallbackQuery(tc.pasted)
			if err == nil {
				t.Fatalf("manualCallbackQuery(%q) succeeded", tc.pasted)
			}
			if strings.Contains(err.Error(), "secret-code-123") {
				t.Errorf("error echoes the pasted credential: %q", err)
			}
		})
	}
}

// TestRunManualConsentStoresTheToken exercises the flow end to end
// against the real store and a stubbed token endpoint, which is what
// proves the manual path writes a durable token rather than merely
// parsing a URL.
func TestRunManualConsentStoresTheToken(t *testing.T) {
	flow, store := newConsentFixture(t)
	endpoint := &tokenEndpoint{
		body: `{"access_token":"at","token_type":"Bearer","refresh_token":"rt-manual","expires_in":3600}`,
	}
	var out strings.Builder
	in := strings.NewReader("http://127.0.0.1:3437/callback?code=good&state=the-state\n")

	if err := runManualConsent(exchangeContext(endpoint), flow,
		"http://127.0.0.1:3437/callback", in, &out); err != nil {
		t.Fatalf("runManualConsent: %v", err)
	}
	if got := storedToken(t, store); got != "rt-manual" {
		t.Errorf("stored refresh token = %q, want rt-manual", got)
	}
	// The consent URL has to be in the output — it is the only thing the
	// operator can act on, and this path has no log line carrying it.
	if !strings.Contains(out.String(), "accounts.spotify.com") {
		t.Errorf("output did not contain the consent URL:\n%s", out.String())
	}
}

// TestRunManualConsentRejectsAStateMismatch is the check that must not be
// skippable on this transport. consentFlow exists so that all three entry
// points share it; a test per path is what keeps that true.
func TestRunManualConsentRejectsAStateMismatch(t *testing.T) {
	flow, store := newConsentFixture(t)
	endpoint := &tokenEndpoint{
		body: `{"access_token":"at","token_type":"Bearer","refresh_token":"rt","expires_in":3600}`,
	}
	var out strings.Builder
	in := strings.NewReader("http://127.0.0.1:3437/callback?code=good&state=not-the-state\n")

	err := runManualConsent(exchangeContext(endpoint), flow,
		"http://127.0.0.1:3437/callback", in, &out)
	if err == nil {
		t.Fatal("a state mismatch was accepted")
	}
	if endpoint.calls != 0 {
		t.Errorf("token endpoint was called %d time(s) despite the state mismatch", endpoint.calls)
	}
	if got := storedToken(t, store); got != "" {
		t.Errorf("a token was stored despite the state mismatch: %q", got)
	}
}

// TestManualConsentNeedsNoReachableRedirect pins the property that makes
// this flow worth having: an https redirect URL is rejected outright by
// the listening path, because that listener cannot terminate TLS. The
// manual path binds nothing, so it must accept one.
func TestManualConsentNeedsNoReachableRedirect(t *testing.T) {
	if _, err := callbackTarget("https://nas.tail1234.ts.net/difmsync/callback", ""); err == nil {
		t.Fatal("callbackTarget accepted an https redirect; this test is no longer meaningful")
	}

	flow, store := newConsentFixture(t)
	endpoint := &tokenEndpoint{
		body: `{"access_token":"at","token_type":"Bearer","refresh_token":"rt-https","expires_in":3600}`,
	}
	var out strings.Builder
	in := strings.NewReader("https://nas.tail1234.ts.net/difmsync/callback?code=good&state=the-state\n")

	if err := runManualConsent(exchangeContext(endpoint), flow,
		"https://nas.tail1234.ts.net/difmsync/callback", in, &out); err != nil {
		t.Fatalf("runManualConsent with an https redirect: %v", err)
	}
	if got := storedToken(t, store); got != "rt-https" {
		t.Errorf("stored refresh token = %q, want rt-https", got)
	}
}
