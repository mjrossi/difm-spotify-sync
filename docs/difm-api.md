# DI.fm / AudioAddict private API — captured reference

DI.fm has no public API. Everything here was reverse-engineered on 2026-08-19 by
capturing requests from the logged-in web app at `www.di.fm`. Nothing here is
contractual — expect it to break without notice.

**No secrets in this file.** Credentials come from the environment; see the bottom.

## Hosts

| Host | Cloudflare? | Notes |
|---|---|---|
| `www.di.fm` | **Yes** — plain requests get `403` | The SPA. Do not automate against it. |
| `api.audioaddict.com` | **No** — plain `curl` gets `200` | The actual API. Use this exclusively. |

The Cloudflare JS challenge that broke older tools (e.g. `di-tui` login) only guards the
web app. The API host is unprotected, so **no headless browser is needed at runtime**.

## Base URL

```
https://api.audioaddict.com/v1/{network}
```

`network` is `di` for DI.fm. The same backend serves `rockradio`, `radiotunes`,
`jazzradio`, `classicalradio`, `zenradio` — so this client generalizes for free.

## Authentication

Two mechanisms work, both **stateless and cookie-free**:

```
X-API-Key: <api_key>          # preferred — header, not logged in URLs
?api_key=<api_key>            # equivalent, but leaks the key into access logs
```

These do **not** work: `X-Session-Key` (returns `Invalid Session`), `listen_key`
as a query param, `Authorization: Bearer`, HTTP Basic `streams:diradio`
(all return `403 Member Authentication required`).

The **`listen_key` is a streaming token, not an API credential** — it authorizes
stream URLs only. Do not use it for API calls.

### Obtaining the api_key

Either is fine; the first avoids handling a password at all.

1. **From the browser (recommended).** Log in to `www.di.fm`, open DevTools console:
   ```js
   document.documentElement.innerHTML.match(/api_key["':\s]+([a-f0-9]{32})/)[1]
   ```
   The key is long-lived; capture it once.
2. **Via login.** `POST /v1/di/member_sessions`, HTTP Basic `streams:diradio`, body
   `member_session[username]` + `member_session[password]`. The response member object
   carries `api_key`, `listen_key` and `id`. Only needed if the key must be re-minted.

### member_id

Required in the favorites path and **not discoverable from the key** — every
`members/current`, `members/me`, `session` variant returns `404`. Capture it once from
the likes request URL in DevTools, or from the `member_sessions` login response.

## The endpoint that matters: liked tracks

DI.fm's "Likes" / "Your Favorites" playlist is **not** a favorites endpoint. It is the
*voting* system filtered to upvotes. This is why every `favorites/tracks`,
`favorite_tracks`, `track_favorites` path guess returns `404`.

```
GET /v1/di/members/{member_id}/track_votes?vote_type=up&page=1&per_page=100
X-API-Key: <api_key>
```

Verified: the UI at `www.di.fm/my/likes` renders exactly the rows this returns.

### Response

Top level is a **bare JSON array** (no envelope, no metadata object).

| Field | Type | Use |
|---|---|---|
| `id` | int | Vote ID. Monotonic — usable as a cursor. |
| `created_at` | RFC3339 w/ offset | **When the track was liked.** Incremental-sync watermark. |
| `updated_at` | RFC3339 w/ offset | |
| `track_id` | int | Stable DI.fm track ID — the idempotency key. |
| `up` / `down` | bool | Vote direction. Redundant when filtering `vote_type=up`. |
| `channel_id` | int | Channel the like happened on. |
| `playlist`, `position`, `episode` | usually null | `episode` non-null ⇒ mix-show content, not a track. |
| `track` | object | See below. |

Nested `track` object — the fields worth consuming:

| Field | Type | Use |
|---|---|---|
| `id` | int | Same as `track_id`. |
| `display_artist` | string | **Artist.** e.g. `"Funk D'Void & Berny"` |
| `display_title` / `title` | string | **Title**, version descriptor included: `"Junkies (Joe Silva Remix)"` |
| `track` | string | Pre-joined `"Artist - Title"`. |
| `length` | int | **Duration in seconds.** Verified against the UI (442 ⇒ 7:22). Highest-value Spotify matching signal. |
| `version` | string\|null | Version descriptor when broken out. Often null — parse the title instead. |
| `isrc` | string\|null | Often null, but when present it is an **exact** Spotify match key. Always check first. |
| `mix` | bool | True ⇒ DJ mix, not a single track. |
| `details_url` | string | Human-readable page, useful in the review queue. |

Also present and not needed: `waveform_url`, `images`, `votes`, `content`, `retail`,
`artists[]`, `artist{}`, `release`, `*_accessibility`.

### Pagination

`page` is honoured. **`per_page` is ignored** — the server returns its own page size
regardless of the value sent. Do not rely on `per_page` to size batches; page until a
short/empty array comes back.

There are **no pagination headers** — no `Link`, no total count. Termination is by
short page only.

### Ordering

Votes arrive **newest first**, ordered by `created_at` descending. This is observed
behaviour, not a documented guarantee — nothing in the response states it — but the
client now depends on it: once a whole page of new records falls at or below the
caller's watermark, pagination stops, on the reasoning that everything after it is
older still. Without that short-circuit every tick walks the member's entire like
history.

If the server ever changes this ordering, the failure is **silent truncation**, not an
error: the walk would stop early and report a clean, complete read. If likes start
going missing with no error in `sync_runs`, check this first.

Termination itself is keyed on the **raw record count**, never on how many usable
tracks a page yielded. A page can decode to zero tracks and still be followed by more
— every record on it might be a downvote, or malformed — and treating that as the end
of the list silently truncates the history.

### Rate limiting

A 429 has been observed under sustained paging, carrying `Retry-After` in both
documented spellings (delay-seconds and an HTTP date). It is typed as
`difm.ErrRateLimited` rather than read as an empty result: a throttled request that
looks like "no likes" makes the caller record a wrong verdict for every track it was
throttled on.

## Filtering out non-tracks

Not everything liked is a Spotify-shaped song. Skip an item when any holds:

- `episode != null` — mix-show episode
- `track.mix == true` — DJ mix
- `track.length` greater than ~15 min — almost certainly a set, not a track

Without this the sync will search Spotify for hour-long DJ sets and either fail or,
worse, match some unrelated track of the same name.

## Other endpoints confirmed live

| Endpoint | Auth | Notes |
|---|---|---|
| `GET /v1/di/channels` | none | Full channel list. |
| `GET /v1/di/track_history/channel/{id}` | none | Recent tracks on a channel. |
| `GET /v1/di/currently_playing` | none | Site-wide now-playing. |
| `GET /v1/di/tracks/{id}` | none | Single track. |
| `GET /v1/di/playlists` | none | DI.fm's *curated* playlists — unrelated to user likes. |
| `GET /v1/di/members/{id}/favorites/channels` | key | Favorite **channels**, not tracks. |
| `GET /v1/di/listen_history` | key | Not explored. |

Route probing trick: unauthenticated `403` means the route exists but needs auth;
`404` means no such route. This cleanly distinguishes real endpoints from guesses.

## Configuration

```
DIFMSYNC_API_KEY=<32 hex chars>
DIFMSYNC_MEMBER_ID=<integer>
DIFMSYNC_NETWORK=di
```

These are the names the binary reads (every flag has a `DIFMSYNC_*`
fallback). Exporting the unprefixed `DIFM_*` forms does nothing, and the
resulting failure — `missing required configuration` — does not point at
the cause.

No password is needed at runtime. Never commit these; `.env` is gitignored.
