// Package spotify wraps the Spotify Web API for the three things the
// connector needs: finding candidate recordings for a DI.fm like,
// appending them to a playlist, and naming that playlist.
//
// This is a hand-rolled client rather than a third-party wrapper. The
// established Go library (zmb3/spotify) last shipped in November 2024 and
// hardcodes POST /playlists/{id}/tracks, which Spotify's February 2026 API
// migration deprecated — it now returns a bare 403 for Development Mode
// apps, with no error body to diagnose from. The replacement endpoint is
// /playlists/{id}/items. Since the surface we need is three endpoints, an
// unmaintained dependency that cannot perform the central write is a worse
// trade than owning the requests.
//
// Search is deliberately not "take the first result". Spotify's relevance
// ranking is tuned for humans typing queries, not for reconciling radio
// metadata, and for electronic music the top hit is frequently the wrong
// edit of the right track. Candidates are scored by pkg/match instead.
package spotify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/mjrossi/difm-spotify-sync/pkg/match"
)

const (
	apiBaseURL = "https://api.spotify.com/v1"
	authURL    = "https://accounts.spotify.com/authorize"
	tokenURL   = "https://accounts.spotify.com/api/token" //nolint:gosec // endpoint URL, not a credential

	// maxSearchResults bounds how many candidates are scored per track.
	// Spotify's relevance falls off sharply; beyond this it is noise.
	maxSearchResults = 10

	// addBatchSize is the API's documented per-request cap for playlist adds.
	addBatchSize = 100
)

// Scopes are the minimum needed to read and append to the target playlist.
// Notably absent: anything touching listening history, profile, or library.
var Scopes = []string{
	"playlist-modify-private",
	"playlist-modify-public",
	"playlist-read-private",
}

// ErrNoCredentials indicates the one-time consent step has not been run.
var ErrNoCredentials = errors.New("spotify: no refresh token; run `difmsync auth` first")

// ErrRateLimited reports a 429. Callers must be able to tell this apart
// from "Spotify has no such track": a sync pass that reads a throttled
// search as an empty result records a permanent wrong verdict for every
// track it was throttled on.
var ErrRateLimited = errors.New("spotify: rate limited")

// ErrUnauthorized reports a 401/403 — an expired grant, a revoked app,
// or a missing scope. Unlike a rate limit this needs a human, so callers
// should stop rather than back off and retry.
var ErrUnauthorized = errors.New("spotify: unauthorized")

// RateLimitError carries Spotify's backoff hint alongside ErrRateLimited.
// Spotify's Retry-After is authoritative and can be minutes long.
type RateLimitError struct {
	RetryAfter time.Duration
	StatusCode int
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("spotify: rate limited (status %d), retry after %s", e.StatusCode, e.RetryAfter)
	}
	return fmt.Sprintf("spotify: rate limited (status %d)", e.StatusCode)
}

// Unwrap lets callers branch with errors.Is(err, spotify.ErrRateLimited).
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// Authenticator builds OAuth flows and authenticated clients.
type Authenticator struct {
	cfg *oauth2.Config
}

// NewAuthenticator configures the Authorization Code flow.
func NewAuthenticator(clientID, clientSecret, redirectURL string) *Authenticator {
	return &Authenticator{
		cfg: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Scopes:       Scopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:  authURL,
				TokenURL: tokenURL,
			},
		},
	}
}

// AuthURL returns the consent URL for the one-time browser step.
func (a *Authenticator) AuthURL(state string) string {
	return a.cfg.AuthCodeURL(state)
}

// Exchange trades an authorization code for a token carrying a refresh token.
func (a *Authenticator) Exchange(ctx context.Context, code string) (*oauth2.Token, error) {
	tok, err := a.cfg.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("spotify: exchange code: %w", err)
	}
	return tok, nil
}

// Client builds an API client from a stored refresh token. The oauth2
// transport renews the access token on demand, so a long-running daemon
// never needs a restart to stay authenticated.
//
// onRefresh, when non-nil, is called with a *new* refresh token whenever
// Spotify rotates one during a renewal. It must be wired to durable
// storage: the oauth2 transport keeps the rotated token in memory only,
// so without this the daemon works until it restarts and then presents a
// stale token — recoverable solely by re-running the interactive consent
// step, which is the one step that cannot run headless.
//
// A persistence failure is returned to the caller rather than logged and
// dropped, because at that point the stored token may already be dead
// and continuing would hide that.
func (a *Authenticator) Client(ctx context.Context, refreshToken string, onRefresh func(string) error) (*Client, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ErrNoCredentials
	}
	tok := &oauth2.Token{RefreshToken: refreshToken, TokenType: "Bearer"}
	src := &rotatingTokenSource{
		src:       a.cfg.TokenSource(ctx, tok),
		last:      refreshToken,
		onRefresh: onRefresh,
	}
	// oauth2.NewClient returns a client with no Timeout, and NewClient's
	// own fallback only applies to a nil client. The daemon's context is
	// the root signal context and is never bounded per pass, so without
	// this one hung connection stalls the whole sync ticker indefinitely.
	hc := oauth2.NewClient(ctx, src)
	hc.Timeout = 30 * time.Second
	return NewClient(hc, apiBaseURL), nil
}

// rotatingTokenSource persists a rotated refresh token as it is issued.
type rotatingTokenSource struct {
	src       oauth2.TokenSource
	onRefresh func(string) error

	mu   sync.Mutex
	last string
}

func (r *rotatingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := r.src.Token()
	if err != nil {
		return nil, classifyTokenError(err)
	}
	if tok.RefreshToken == "" || r.onRefresh == nil {
		return tok, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if tok.RefreshToken == r.last {
		return tok, nil
	}
	if err := r.onRefresh(tok.RefreshToken); err != nil {
		return nil, fmt.Errorf("spotify: persist rotated refresh token: %w", err)
	}
	r.last = tok.RefreshToken
	return tok, nil
}

// classifyTokenError maps an oauth2 token-endpoint failure onto this
// package's sentinels.
//
// Token renewal happens inside the oauth2 transport, so its failures
// surface as *oauth2.RetrieveError rather than through the API response
// path that types everything else. A revoked or expired grant is the
// single most likely "needs a human" cause there is — it is what a
// password change or a revoked app authorization produces — and left
// untyped the engine misses its abort branch and instead retries every
// remaining like against a credential that cannot work.
func classifyTokenError(err error) error {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return err
	}
	switch {
	case re.Response != nil && re.Response.StatusCode == http.StatusTooManyRequests:
		var retryAfter time.Duration
		if re.Response.Header != nil {
			retryAfter = parseRetryAfter(re.Response.Header.Get("Retry-After"))
		}
		return fmt.Errorf("spotify: token endpoint: %w", &RateLimitError{
			RetryAfter: retryAfter,
			StatusCode: re.Response.StatusCode,
		})
	case re.ErrorCode == "invalid_grant" || re.ErrorCode == "invalid_client",
		re.Response != nil && (re.Response.StatusCode == http.StatusUnauthorized ||
			re.Response.StatusCode == http.StatusBadRequest ||
			re.Response.StatusCode == http.StatusForbidden):
		return fmt.Errorf("spotify: refresh token rejected (%s): %w", re.ErrorCode, ErrUnauthorized)
	}
	return err
}

// Client is an authenticated Spotify API client.
type Client struct {
	http *http.Client
	base string
}

// NewClient builds a Client over a caller-supplied HTTP client and API base
// URL. The HTTP client is expected to carry authentication — normally the
// oauth2 transport from Authenticator.Client, which is the usual way to get
// one. This lower-level constructor exists for callers that manage tokens
// themselves, and for pointing the client at a stub server in tests.
func NewClient(httpc *http.Client, baseURL string) *Client {
	if baseURL == "" {
		baseURL = apiBaseURL
	}
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{http: httpc, base: strings.TrimRight(baseURL, "/")}
}

// do issues a request and decodes a JSON response into out (which may be
// nil when the body is not needed). Non-2xx responses become errors
// carrying the body, because Spotify's failures are often bare status
// codes and the body is the only diagnostic available.
func (c *Client) do(ctx context.Context, method, endpoint string, body, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return &RateLimitError{
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			StatusCode: resp.StatusCode,
		}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%w: status %d: %s", ErrUnauthorized, resp.StatusCode, strings.TrimSpace(string(detail)))
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// searchResponse mirrors only the fields consumed.
type searchResponse struct {
	Tracks struct {
		Items []trackObject `json:"items"`
	} `json:"tracks"`
}

type trackObject struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	DurationMs int    `json:"duration_ms"`
	Artists    []struct {
		Name string `json:"name"`
	} `json:"artists"`
	ExternalIDs struct {
		ISRC string `json:"isrc"`
	} `json:"external_ids"`
}

// Search returns scored candidates for a track, best first.
//
// The field-scoped query (`artist:"X" track:"Y"`) is tried first because it
// is precise; it is brittle against metadata noise, so a freeform query
// backs it up when the scoped form finds nothing.
func (c *Client) Search(ctx context.Context, artist, title string, durationSec int, isrc string) ([]match.Scored, error) {
	want := match.Parse(artist, title)

	// ISRC identifies a specific recording, so when DI.fm supplies one it
	// settles the question outright — no fuzzy scoring, and no risk of
	// picking the wrong edit. Tried first and short-circuited on a hit.
	if exact, err := c.searchByISRC(ctx, isrc); err != nil {
		return nil, err
	} else if exact != nil {
		return []match.Scored{*exact}, nil
	}

	queries := []string{
		fmt.Sprintf("artist:%q track:%q", stripQuotes(artist), stripQuotes(bareTitle(title))),
		strings.TrimSpace(artist + " " + title),
	}

	var (
		found   []trackObject
		lastErr error
	)
	for _, q := range queries {
		res, err := c.searchQuery(ctx, q)
		if err != nil {
			// A rate limit or a dead grant will hit the fallback query
			// just the same, so retrying it only burns quota.
			if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUnauthorized) {
				return nil, fmt.Errorf("spotify: search %q: %w", q, err)
			}
			// Anything else: a failing scoped query must not skip the
			// freeform fallback the two-query design exists for. Only if
			// every query fails is this a search failure.
			lastErr = fmt.Errorf("spotify: search %q: %w", q, err)
			continue
		}
		if len(res.Tracks.Items) > 0 {
			found = res.Tracks.Items
			break
		}
	}
	if len(found) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, nil
	}

	scored := make([]match.Scored, 0, len(found))
	for _, t := range found {
		scored = append(scored, match.ScoreCandidate(want, durationSec, t.toCandidate()))
	}

	// Stable sort keeps Spotify's own relevance order as the tie-break,
	// which is a reasonable secondary signal between equal scores.
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	return scored, nil
}

// searchQuery issues one search request.
//
// No `market` parameter is sent, deliberately. Scoping to the user's
// market would drop recordings that are unplayable there, but a playlist
// append still succeeds for those and the entry stays valid if licensing
// later changes — whereas a filtered-out track becomes a permanent
// no-match. Relinking on playback is Spotify's job, not ours.
func (c *Client) searchQuery(ctx context.Context, q string) (searchResponse, error) {
	params := url.Values{}
	params.Set("q", q)
	params.Set("type", "track")
	params.Set("limit", strconv.Itoa(maxSearchResults))

	var res searchResponse
	err := c.do(ctx, http.MethodGet, c.base+"/search?"+params.Encode(), nil, &res)
	return res, err
}

// searchByISRC resolves a recording code to its Spotify track. It returns
// (nil, nil) when no ISRC was supplied or nothing matched, so the caller
// falls through to the fuzzy path.
func (c *Client) searchByISRC(ctx context.Context, isrc string) (*match.Scored, error) {
	isrc = strings.TrimSpace(isrc)
	if isrc == "" {
		return nil, nil
	}

	res, err := c.searchQuery(ctx, "isrc:"+isrc)
	if err != nil {
		// A rate limit or an expired grant must surface; only a genuine
		// miss may fall through to the fuzzy path.
		if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUnauthorized) {
			return nil, fmt.Errorf("spotify: isrc lookup %q: %w", isrc, err)
		}
		return nil, nil
	}

	for _, t := range res.Tracks.Items {
		// Spotify's isrc: filter is not always exact; confirm the code it
		// returned is the one we asked for before trusting it.
		if !strings.EqualFold(strings.TrimSpace(t.ExternalIDs.ISRC), isrc) {
			continue
		}
		return &match.Scored{
			Candidate: t.toCandidate(),
			Score:     1.0,
			Why:       "isrc exact match",
		}, nil
	}
	return nil, nil
}

// parseRetryAfter reads the header in either documented form: a delay in
// seconds, or an HTTP date. An unparseable value yields 0, meaning the
// caller should fall back to its own backoff.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// AddToPlaylist appends tracks, batching to the API's per-request cap.
//
// Note the endpoint: /items, not the /tracks path deprecated in Spotify's
// February 2026 migration. The deprecated path returns 403 with an empty
// body, so if this ever regresses, check the path before suspecting
// scopes or playlist ownership.
func (c *Client) AddToPlaylist(ctx context.Context, playlistID string, trackIDs []string) error {
	if len(trackIDs) == 0 {
		return nil
	}
	for start := 0; start < len(trackIDs); start += addBatchSize {
		end := min(start+addBatchSize, len(trackIDs))

		uris := make([]string, 0, end-start)
		for _, id := range trackIDs[start:end] {
			uris = append(uris, "spotify:track:"+id)
		}

		endpoint := fmt.Sprintf("%s/playlists/%s/items", c.base, url.PathEscape(playlistID))
		if err := c.do(ctx, http.MethodPost, endpoint, map[string]any{"uris": uris}, nil); err != nil {
			return fmt.Errorf("spotify: add %d track(s) to playlist %s: %w", len(uris), playlistID, err)
		}
	}
	return nil
}

// PlaylistTrackIDs returns the set of track IDs currently in a playlist.
//
// The sync pass uses this to reconcile against reality rather than
// trusting the ledger alone. The ledger can legitimately disagree with
// Spotify — a restored database, a cleared ledger via `resync`, or a
// track added by hand — and in every one of those cases re-adding would
// silently duplicate. Checking costs one paginated read per pass.
func (c *Client) PlaylistTrackIDs(ctx context.Context, playlistID string) (map[string]bool, error) {
	ids := make(map[string]bool)

	const pageSize = 100
	// maxOffset bounds the walk at 100k tracks — an order of magnitude
	// past Spotify's own 10k playlist cap. Termination normally comes
	// from total or a short page; this is the backstop for a server that
	// keeps returning full pages, which would otherwise spin forever
	// issuing requests as fast as the network allows.
	const maxOffset = 100_000

	for offset := 0; offset <= maxOffset; offset += pageSize {
		params := url.Values{}
		// Ask only for what is needed; the full item objects are large.
		params.Set("fields", "total,items(item(id))")
		params.Set("limit", strconv.Itoa(pageSize))
		params.Set("offset", strconv.Itoa(offset))

		endpoint := fmt.Sprintf("%s/playlists/%s/items?%s",
			c.base, url.PathEscape(playlistID), params.Encode())

		// Note `item`, not `track` — the February 2026 migration renamed
		// the field in playlist item responses.
		var page struct {
			Total int `json:"total"`
			Items []struct {
				Item struct {
					ID string `json:"id"`
				} `json:"item"`
			} `json:"items"`
		}
		if err := c.do(ctx, http.MethodGet, endpoint, nil, &page); err != nil {
			return nil, fmt.Errorf("spotify: list playlist %s items: %w", playlistID, err)
		}
		for _, it := range page.Items {
			if it.Item.ID != "" {
				ids[it.Item.ID] = true
			}
		}
		// Three independent stopping conditions, because none alone is
		// trustworthy: a short page, an empty page, and the server's own
		// total. `total` comes back through a `fields` filter, so it is
		// used as a bound rather than believed outright.
		if len(page.Items) == 0 || len(page.Items) < pageSize {
			return ids, nil
		}
		if page.Total > 0 && offset+len(page.Items) >= page.Total {
			return ids, nil
		}
	}
	return nil, fmt.Errorf("spotify: list playlist %s items: no end after %d items", playlistID, maxOffset)
}

// PlaylistName resolves a playlist ID to its name, so the CLI can report
// which playlist it is about to write to before writing to it.
func (c *Client) PlaylistName(ctx context.Context, playlistID string) (string, error) {
	endpoint := fmt.Sprintf("%s/playlists/%s?fields=name", c.base, url.PathEscape(playlistID))
	var out struct {
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return "", fmt.Errorf("spotify: get playlist %s: %w", playlistID, err)
	}
	return out.Name, nil
}

func (t trackObject) toCandidate() match.Candidate {
	names := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		names = append(names, a.Name)
	}
	return match.Candidate{
		ID:          t.ID,
		Artist:      strings.Join(names, " & "),
		Title:       t.Name,
		DurationSec: t.DurationMs / 1000,
		ISRC:        t.ExternalIDs.ISRC,
	}
}

// bareTitle drops parenthesised descriptors for the field-scoped query.
// Spotify's `track:` filter matches poorly when the version descriptor is
// included; version agreement is checked during scoring instead.
func bareTitle(title string) string {
	if i := strings.IndexAny(title, "(["); i > 0 {
		return strings.TrimSpace(title[:i])
	}
	return title
}

func stripQuotes(s string) string { return strings.ReplaceAll(s, `"`, "") }
