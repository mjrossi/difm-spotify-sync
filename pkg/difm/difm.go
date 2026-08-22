// Package difm is a client for the private AudioAddict API that backs
// DI.fm and its sibling networks (RadioTunes, JazzRadio, RockRadio,
// ClassicalRadio, ZenRadio).
//
// The API is undocumented, unversioned, and has had endpoints restricted
// before, so every response is decoded defensively: unknown fields are
// ignored, absent fields are zero, and a malformed record is skipped
// rather than failing the whole page.
//
// See docs/difm-api.md for the captured request/response reference.
package difm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the API host. Note this is NOT www.di.fm, which sits
// behind Cloudflare and rejects non-browser clients; the API host is
// unprotected and needs no browser emulation.
const DefaultBaseURL = "https://api.audioaddict.com/v1"

// DefaultNetwork is the AudioAddict network slug for DI.fm.
const DefaultNetwork = "di"

// maxTrackLength bounds what counts as a "track". Anything longer is a
// DJ set or mix-show episode, which has no meaningful Spotify analog —
// searching for it yields either nothing or a wrong same-titled song.
const maxTrackLength = 15 * 60

// ErrUnauthorized is returned when the API key is missing, wrong, or revoked.
var ErrUnauthorized = errors.New("difm: unauthorized")

// ErrRateLimited reports a 429. Callers must be able to distinguish this
// from "no results": treating a throttled request as an empty answer
// would let a sync pass record a wrong verdict and advance past the
// likes it never actually read.
var ErrRateLimited = errors.New("difm: rate limited")

// ErrTruncated reports that pagination hit maxPages before the server
// ran out of records, so the result is an incomplete prefix of the
// member's likes. It is deliberately an error rather than a short
// return: a caller that mistook truncation for completion would advance
// its watermark past likes it never saw, putting them permanently out
// of reach.
var ErrTruncated = errors.New("difm: result truncated at page limit")

// ErrDropped reports that at least one record could not be understood
// and was skipped. It is returned alongside every like that *was* read,
// so a caller can process the prefix — but it must not treat the pass as
// clean, because a dropped record is a like that reached no durable
// state anywhere.
//
// This is the seam between two rules that pull in opposite directions: a
// malformed record must not fail the whole batch, and the watermark must
// not advance past a like that was never recorded. Skipping the record
// satisfies the first; returning this error satisfies the second. The
// cost is that a *persistently* unreadable record stalls the watermark
// and re-reads it every pass — loudly, in sync_runs.error, which is the
// intended trade against losing it silently and unrecoverably.
var ErrDropped = errors.New("difm: records dropped")

// RateLimitError carries the server's backoff hint alongside ErrRateLimited.
type RateLimitError struct {
	RetryAfter time.Duration
	StatusCode int
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("difm: rate limited (status %d), retry after %s", e.StatusCode, e.RetryAfter)
	}
	return fmt.Sprintf("difm: rate limited (status %d)", e.StatusCode)
}

// Unwrap lets callers branch with errors.Is(err, difm.ErrRateLimited).
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// maxPages bounds pagination so a server that ignores `page` cannot spin
// forever. Hitting it yields ErrTruncated, never a silent short result.
const maxPages = 200

// Client talks to the AudioAddict API on behalf of one member.
type Client struct {
	BaseURL  string
	Network  string
	APIKey   string
	MemberID string
	HTTP     *http.Client

	// Logf, when set, receives non-fatal diagnostics — currently the
	// count of records skipped as malformed. Left nil, they are dropped;
	// pkg/difm deliberately takes no logging dependency.
	Logf func(format string, args ...any)
}

// New returns a Client with sane transport defaults.
func New(apiKey, memberID string) *Client {
	return &Client{
		BaseURL:  DefaultBaseURL,
		Network:  DefaultNetwork,
		APIKey:   apiKey,
		MemberID: memberID,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}
}

// Track is a normalized liked track, flattened from the API's nested shape.
type Track struct {
	VoteID      int64
	TrackID     int64
	Artist      string
	Title       string
	DurationSec int
	ISRC        string
	DetailsURL  string
	ChannelID   int64
	LikedAt     time.Time

	// Skip reports that this record is not a Spotify-shaped song (a DJ
	// mix or a mix-show episode). Callers should record it rather than
	// search for it.
	Skip       bool
	SkipReason string
}

// wire mirrors only the fields consumed; everything else in the payload
// (waveform_url, images, votes, retail, ...) is deliberately ignored.
type wire struct {
	ID        int64  `json:"id"`
	TrackID   int64  `json:"track_id"`
	ChannelID int64  `json:"channel_id"`
	CreatedAt string `json:"created_at"`
	Up        bool   `json:"up"`
	Episode   *struct {
		ID int64 `json:"id"`
	} `json:"episode"`
	Track *struct {
		ID            int64  `json:"id"`
		Title         string `json:"title"`
		DisplayTitle  string `json:"display_title"`
		DisplayArtist string `json:"display_artist"`
		Length        int    `json:"length"`
		Mix           bool   `json:"mix"`
		ISRC          string `json:"isrc"`
		DetailsURL    string `json:"details_url"`
	} `json:"track"`
}

// ListLikedTracks returns every upvoted track newer than since, oldest
// first. A zero since fetches everything.
//
// DI.fm's "Likes" / "Your Favorites" playlist is the vote system filtered
// to upvotes — there is no separate favorites endpoint.
func (c *Client) ListLikedTracks(ctx context.Context, since time.Time) ([]Track, error) {
	var out []Track
	seen := make(map[int64]bool)
	complete := false

	// per_page is accepted but ignored by the server, so pages are sized
	// server-side and termination is by short/empty page only. There are
	// no pagination headers and no total count to check against.
	var dropped int
	for page := 1; page <= maxPages; page++ {
		p, err := c.fetchPage(ctx, page)
		if err != nil {
			return nil, err
		}
		dropped += p.dropped

		// Termination keys on the raw row count, never on len(p.tracks).
		// A page whose records were all downvotes, or all malformed,
		// yields no tracks but is emphatically not the end of the list —
		// reading it as one silently truncated the history and reported
		// a clean, complete pass, so every like beyond it was lost.
		if p.rows == 0 {
			complete = true
			break
		}

		// Progress is judged on every decoded record, including the ones
		// filtered out, for the same reason.
		newOnPage := make(map[int64]bool, len(p.voteIDs))
		for _, id := range p.voteIDs {
			if seen[id] {
				continue // server ignored `page`; stop rather than loop forever
			}
			seen[id] = true
			newOnPage[id] = true
		}
		progressed := len(newOnPage) > 0

		considered, kept := 0, 0
		for _, t := range p.tracks {
			if !newOnPage[t.VoteID] {
				continue
			}
			considered++
			if !since.IsZero() && !t.LikedAt.After(since) {
				continue
			}
			kept++
			out = append(out, t)
		}
		if !progressed {
			complete = true
			break
		}
		// Votes arrive newest-first (see docs/difm-api.md), so once a
		// whole page of new-to-us records falls at or below the
		// watermark, everything after it does too. Without this the
		// client walks the member's entire like history on every tick —
		// a lot of traffic against a private API, for zero new rows.
		//
		// considered > 0 guards the short-circuit: a page that offered no
		// candidates at all (all downvotes, say) says nothing about where
		// the watermark falls, and stopping on it would truncate.
		if !since.IsZero() && considered > 0 && kept == 0 {
			complete = true
			break
		}
	}

	// Oldest first, so a partial failure still advances the watermark
	// monotonically over the prefix that succeeded.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}

	if !complete {
		// Return what was read *and* the error: the caller may want to
		// report the prefix, but it must not treat this as a clean pass.
		return out, fmt.Errorf("difm: stopped after %d pages: %w", maxPages, ErrTruncated)
	}
	if dropped > 0 {
		// Same contract as ErrTruncated: the prefix is real, the pass is
		// not clean. See ErrDropped.
		return out, fmt.Errorf("difm: dropped %d unreadable record(s): %w", dropped, ErrDropped)
	}
	return out, nil
}

// httpClient falls back to a sane default so a Client built as a struct
// literal (every field is exported) does not nil-panic on first use.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// pageResult is one page of votes, kept separate from the tracks it
// yielded so the caller can tell "the server sent nothing" from "we
// filtered everything away". Conflating those truncates the history.
type pageResult struct {
	tracks []Track
	// voteIDs of every record that decoded, including filtered ones.
	// Pagination progress is judged on these: a page of nothing but
	// downvotes still means the cursor moved.
	voteIDs []int64
	// rows the server returned, before any filtering. Termination keys
	// on this and nothing else.
	rows int
	// dropped counts records we should have been able to use and could
	// not — a shape drift, not a downvote. See ErrDropped.
	dropped int
}

// scrubMemberID rewrites a transport error so the member id cannot travel
// with it.
//
// net/http returns *url.Error from both NewRequest and Do, and *url.Error
// embeds the full request URL in its Error() text. This client puts the
// member id in the path, so an unscrubbed transport failure — a DNS blip,
// a timeout, a refused connection — carries the id into the error the
// engine records in sync_runs.error, and from there onto the status
// endpoints, which are served to the LAN without authentication.
//
// The id is not a credential on its own, but it is half of the DI.fm
// capture and a status page has no reason to publish it. The host and
// path are kept, because "which request failed" is the useful part.
//
// Call this with the error exactly as net/http returned it, before any
// wrapping of our own is added. errors.As matches a *url.Error at any
// depth, and what comes back is the scrubbed *url.Error alone — so
// handing this an already-wrapped error would drop the outer context.
// Both call sites below add their context afterwards, to the return
// value, which is why there is none to lose.
func (c *Client) scrubMemberID(err error) error {
	if c.MemberID == "" {
		return err
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	// Clone rather than mutate: the *url.Error belongs to the caller's
	// error chain. Copying keeps ue.Err intact, so errors.Is against
	// context.Canceled and friends still works at the call site.
	clone := *ue
	clone.URL = strings.ReplaceAll(clone.URL, url.PathEscape(c.MemberID), "<member-id>")
	return &clone
}

func (c *Client) fetchPage(ctx context.Context, page int) (pageResult, error) {
	q := url.Values{}
	q.Set("vote_type", "up")
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", "100")

	endpoint := fmt.Sprintf("%s/%s/members/%s/track_votes?%s",
		strings.TrimRight(c.BaseURL, "/"), c.Network, url.PathEscape(c.MemberID), q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return pageResult{}, fmt.Errorf("difm: build request: %w", c.scrubMemberID(err))
	}
	// Header rather than ?api_key= so the credential stays out of access logs.
	req.Header.Set("X-API-Key", c.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return pageResult{}, fmt.Errorf("difm: get track_votes page %d: %w", page, c.scrubMemberID(err))
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return pageResult{}, fmt.Errorf("difm: page %d: %w", page, ErrUnauthorized)
	case resp.StatusCode == http.StatusTooManyRequests:
		return pageResult{}, fmt.Errorf("difm: page %d: %w", page, &RateLimitError{
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			StatusCode: resp.StatusCode,
		})
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return pageResult{}, fmt.Errorf("difm: page %d: unexpected status %d: %s",
			page, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Decode row-at-a-time rather than into []wire directly. A single
	// type drift on one record — this API is unversioned and has changed
	// shape before — would otherwise abort the whole page and, with it,
	// the whole sync. Skipping the bad row loses one like; failing the
	// page loses every like on it.
	var rows []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return pageResult{}, fmt.Errorf("difm: decode page %d: %w", page, err)
	}

	out := pageResult{rows: len(rows), tracks: make([]Track, 0, len(rows))}
	for _, row := range rows {
		var w wire
		if err := json.Unmarshal(row, &w); err != nil {
			out.dropped++
			// Recover the vote id on its own. A record we cannot decode
			// is still a record, and pagination judges progress on ids —
			// without this, a page of nothing but drifted rows looks
			// like the server repeating itself and ends the walk.
			var ident struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal(row, &ident) == nil && ident.ID != 0 {
				out.voteIDs = append(out.voteIDs, ident.ID)
			}
			continue
		}
		if w.ID != 0 {
			out.voteIDs = append(out.voteIDs, w.ID)
		}
		switch t, verdict := w.normalize(); verdict {
		case rowOK:
			out.tracks = append(out.tracks, t)
		case rowDropped:
			out.dropped++
		}
	}
	if out.dropped > 0 {
		// A rising count is the first sign the upstream shape has drifted.
		// The caller turns this into ErrDropped; logging it here names the
		// page, which is what makes the drift diagnosable.
		c.logf("difm: dropped %d unreadable record(s) on page %d", out.dropped, page)
	}
	return out, nil
}

// parseRetryAfter reads the header in either documented form: delay in
// seconds, or an HTTP date. An unparseable value yields 0, meaning
// "no hint", which callers should treat as "use your own backoff".
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

func (c *Client) logf(format string, args ...any) {
	if c.Logf == nil {
		return
	}
	c.Logf(format, args...)
}

// rowVerdict separates "this record is legitimately not a liked track"
// from "this record should have been usable and was not". Only the
// second is a signal that anything is wrong, and only the second may
// hold the caller's watermark — a downvote is not a data-loss event, and
// counting one as such would stall the sync permanently.
type rowVerdict int

const (
	rowOK rowVerdict = iota
	rowFiltered
	rowDropped
)

// normalize flattens a wire record, reporting whether it is usable,
// legitimately filtered out, or unreadable.
func (w wire) normalize() (Track, rowVerdict) {
	// A downvote or a vote with no track payload is not a like. Expected
	// and uninteresting: the endpoint is the whole vote system.
	if w.Track == nil || !w.Up {
		return Track{}, rowFiltered
	}

	title := w.Track.DisplayTitle
	if strings.TrimSpace(title) == "" {
		title = w.Track.Title
	}

	t := Track{
		VoteID:      w.ID,
		TrackID:     firstNonZero(w.TrackID, w.Track.ID),
		Artist:      strings.TrimSpace(w.Track.DisplayArtist),
		Title:       strings.TrimSpace(title),
		DurationSec: w.Track.Length,
		ISRC:        strings.TrimSpace(w.Track.ISRC),
		DetailsURL:  w.Track.DetailsURL,
		ChannelID:   w.ChannelID,
	}
	// A vote carrying a track object with no id or no title is a record
	// we ought to be able to read and cannot — shape drift, not a
	// filter. Dropping it silently loses the like for good.
	if t.TrackID == 0 || t.Title == "" {
		return Track{}, rowDropped
	}

	// created_at is RFC3339 with a numeric offset. Without it there is no
	// way to place the record against the watermark — it would be
	// filtered out on every pass and never sync — so an unparseable
	// timestamp is a drop rather than a zero value quietly carried
	// forward.
	ts, err := time.Parse(time.RFC3339, w.CreatedAt)
	if err != nil {
		return Track{}, rowDropped
	}
	t.LikedAt = ts.UTC()

	switch {
	case w.Episode != nil:
		t.Skip, t.SkipReason = true, "mix-show episode"
	case w.Track.Mix:
		t.Skip, t.SkipReason = true, "DJ mix"
	case t.DurationSec > maxTrackLength:
		t.Skip, t.SkipReason = true, fmt.Sprintf("runtime %ds exceeds track threshold", t.DurationSec)
	}
	return t, rowOK
}

func firstNonZero(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}
