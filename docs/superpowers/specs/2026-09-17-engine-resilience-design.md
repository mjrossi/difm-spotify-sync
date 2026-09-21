# Engine resilience: self-healing consent, Retry-After, typed failure kinds

Date: 2026-09-17. Status: approved design, awaiting implementation plan.

## Context

The daemon has run cleanly for weeks. A survey of the code against that
log found that the failures a real deployment will eventually meet are
handled *correctly* — the watermark holds, nothing is lost — but
*expensively* or *silently*:

- A refresh token revoked mid-life (`invalid_grant` from the token
  endpoint) makes every tick fail identically forever. The consent server
  starts only at boot, and only when the token is empty
  (`cmd/difmsync/main.go`, the `loop` closure), so the fix today is a human
  noticing a 503, running `difmsync auth`, and restarting the container.
- `Retry-After` is parsed by both clients and read by nothing. A 429
  aborts the pass and `Engine.Loop` resets to the full interval. At a 2h
  interval one rate limit costs two hours.
- A rejected DI.fm API key is indistinguishable from a network blip. The
  engine has no branch on `difm.ErrUnauthorized`, and `/healthz` can only
  say "newest run errored" because recorded error *text* must never reach
  the endpoints.
- `Engine.Loop` has one test, and it cancels the context before the first
  tick.

This is the first of four slices the maintainer chose. The others —
operator diagnostics, data hygiene, container hardening with built-in
backups — each get their own spec.

## Decisions

Made in discussion; restated here so the plan does not reopen them.

1. Re-consent triggers **only** on a token-endpoint rejection, never on an
   API 401/403. A scope or Development-Mode 403 that consent cannot fix
   would otherwise become a consent loop instead of a red healthcheck
   with a readable error.
2. `Retry-After` is honoured by **rescheduling the next tick**, not by
   sleeping inside the pass. The pass abort is already cheap: tracks added
   to Spotify before the 429 are picked up next pass by the live-playlist
   reconcile.
3. The failure category is a **typed `error_kind` column** on
   `sync_runs`, set by the engine from sentinels. Status names the kind
   because it is an enum the code chose, not text an API returned.

## Section 1: Self-healing consent on a revoked grant

**Typed distinction.** `pkg/spotify` gains `ErrGrantRevoked`.
`classifyTokenError` returns it in place of the bare `ErrUnauthorized`
wrap for **`invalid_grant` only** from the *token endpoint*.
`invalid_client` (a wrong client secret, which does not invalidate the
grant) and a bodiless 400/401/403 (an upstream proxy, not a verdict on
the token) stay on the plain `ErrUnauthorized` wrap: they still abort the
pass and still need a human, but they must not authorize deleting a
stored credential. It is defined so that `errors.Is(err, ErrUnauthorized)`
remains true — a double wrap, `fmt.Errorf("%w: %w", ErrGrantRevoked,
ErrUnauthorized)`, is enough — so every existing branch and test on
`ErrUnauthorized` keeps working and the new branch is strictly narrower.
The API 401/403 path in `do()` is unchanged.

**`Loop` returns on it.** `Engine.Loop` today returns only on context
cancellation. When a pass error `Is` `ErrGrantRevoked`, it returns that
error instead of resetting the timer. The engine has no concept of
consent; it hands the decision up. The `sync_runs` row for that pass is
closed as normal by the deferred `FinishRun`, carrying the kind from
Section 3.

**`main.go` re-enters the consent wait.** The body of the `loop` closure
is extracted into a function that can be tested with fakes:

```go
// runUntilStopped waits for consent if there is no token, builds the
// engine, and runs Loop. On ErrGrantRevoked it clears the stored token
// and goes round again, which lands in the same consent wait.
func runUntilStopped(ctx, store, account, awaitConsentFn, newEngineFn, log) error
```

On `ErrGrantRevoked` it logs one Error line for the operator — "Spotify
revoked the refresh token; consent is required again" — clears the stored
token under `context.WithoutCancel` (a shutdown landing between the
revoked pass and the clear must not leave the dead token in place or turn
a clean stop into a non-zero exit; a canceled account read at the top of
the loop is likewise a clean stop), and loops. The clear is unconditional
and documented as such: a token written by `auth --manual` in the window
between Spotify revoking the grant and the daemon noticing is cleared
too, because a compare-and-clear keyed on the runner's copy is wrong
after a rotation and would leave the dead token in place forever.

`newEngine`'s boot-time `PlaylistName` probe returns `ErrGrantRevoked`
rather than warning, so a stored-but-dead token reaches consent
immediately instead of after the first jittered pass. That reaches the existing `awaitConsent` with a
fresh server and a fresh nonce. No new consent code is written: the fourth
caller of `consentFlow.Complete` is the first caller run twice. Without
`--auth-http-addr` (the workstation case) the round trip hits
`ErrNoCredentials`, wrapped to say that `--auth-http-addr` lets the daemon
serve consent itself, and the process exits — loud, rather than ticking
forever.

Clearing the token before waiting keeps the CLAUDE.md invariant literal:
the consent server exists only while there is no refresh token.
`awaitConsent` already polls the store, so `auth --manual` in a sidecar
remains a way out. `/healthz` goes red immediately through the existing
`authorized=false` override rather than after `--max-age`.

One doc consequence: "one nonce per process" becomes "one nonce per
consent wait", which in the common case is the same thing.

## Section 2: Honour Retry-After in the tick

`internal/syncer` gains a pure function:

```go
func nextDelay(err error, interval time.Duration) time.Duration
```

If `errors.As` finds a `*spotify.RateLimitError` or `*difm.RateLimitError`
with a non-zero `RetryAfter`, the result is that value clamped to
`[minInterval, 24h]`; otherwise `interval`. `Loop` calls it in place of
the bare `timer.Reset(interval)` and, when the result differs from
`interval`, logs at Warn: `"rate limited; delaying next pass"` with the
delay. An absent header yields `interval`, which is today's behaviour.

`RunOnce` does not change. The pass still aborts on a 429 and the
watermark still holds. Because the scheduling rule is a pure function it
is tested with a table and needs no clock.

## Section 3: Typed failure kind on sync_runs

- **Migration** `migrations-sqlite/0002_sync_runs_error_kind.sql`:
  `ALTER TABLE sync_runs ADD COLUMN error_kind TEXT NOT NULL DEFAULT ''`.
  Existing rows read as `''`, which status treats as unclassified — the
  current behaviour. Down uses `DROP COLUMN`, which modernc.org/sqlite
  supports. No quote characters in `.sql` comments (sqlc lexing bug, see
  CLAUDE.md).
- **Queries.** `sync_runs.sql` adds `error_kind` to the `FinishSyncRun`
  SET list and to both SELECT lists; `just gen` regenerates.
- **Classification lives in the engine, not the store.** `sqlite.RunStats`
  gains `Kind RunErrorKind`, a string type with constants:
  `KindDiFMUnauthorized = "difm_unauthorized"`,
  `KindSpotifyGrantRevoked = "spotify_grant_revoked"`,
  `KindRateLimited = "rate_limited"`, `KindIncomplete = "incomplete"`,
  `KindError = "error"`. `FinishRun` writes it. The store does not import
  `pkg/difm` or `pkg/spotify`; `syncer` sets the kind with a `classify(err)`
  helper that tests the sentinels with `errors.Is`, checking
  `ErrGrantRevoked` before `ErrUnauthorized` and mapping
  `ErrPassIncomplete` to `KindIncomplete`. It runs once, at the point where
  the final error of a pass is known, so a kind is never set in one branch
  and forgotten in another.
- **New engine branch.** `RunOnce` gets `case errors.Is(err,
  difm.ErrUnauthorized)` ahead of the generic `err != nil` after
  `ListLikedTracks`. It logs an operator-facing Error line — "DI.fm
  rejected the API key; set a fresh DIFMSYNC_API_KEY — the README
  Credentials section says where to find it" (a repo path is useless to
  someone reading a container log) — and returns with the kind set. The tick continues at
  `interval`: hammering a dead key every interval is harmless and the
  alternative is a crash loop.
- **Status surfaces the kind, never the text.** `status.Run` gains
  `ErrorKind string` with `json:"error_kind"` — visible, because it is an
  enum. `newRun` copies it field by field like everything else.
  `describe()` switches on the kind first. Each reason is a past-tense
  fact leading with the same subject as the generic ones, and claims
  nothing about daemon state that status cannot know:
  - `spotify_grant_revoked` → "newest run found the Spotify grant
    revoked; if consent has not been re-given, open the consent URL from
    the daemon log or run difmsync auth". In the daemon this is usually
    shadowed by `Build`'s "awaiting Spotify consent" override once the
    token is cleared; it survives for a one-shot `sync` and for the
    window after a sidecar re-consent before the next pass.
  - `difm_unauthorized` → "newest run had its DI.fm API key rejected —
    set a fresh DIFMSYNC_API_KEY; the README Credentials section says
    where to find it"
  - `rate_limited` → "newest run was rate limited by an API; the daemon
    backs off before retrying"
  - anything else → the existing generic text.

  The exclusion is structural on both endpoints: `newRun` publishes a
  kind only if the store's `Known()` predicate accepts it, so a restored
  or hand-edited row cannot put arbitrary text on `/status.json` either.

  `TestEndpointsCarryNoSecretsFromAFailedRun` must pass unchanged, because
  nothing interpolates `Error`. The CLI `status` table gains a kind column.

## Section 4: Tests

- **`nextDelay` table test:** header present and absent, below the floor,
  above the ceiling, a non-rate-limit error, DI.fm versus Spotify type, and
  a rate-limit error wrapped in `ErrPassIncomplete`.
- **`Loop` under a fake clock.** `Engine` gets an unexported
  `after func(time.Duration) <-chan time.Time`, nil meaning `time.After`,
  replacing the `time.NewTimer` in `Loop`. The test injects a recorder that
  fires on demand. Three cases: a normal pass reschedules at `interval`; a
  429 with `Retry-After: 300` reschedules at 5m; `ErrGrantRevoked` makes
  `Loop` return that error rather than reschedule. This is the Loop test
  the file's own comment says never existed.
- **`runUntilStopped`:** a fake loop returns `ErrGrantRevoked` once and
  then nil; assert the token was cleared and the consent wait was entered
  exactly once more. A second case cancels the context mid-wait and asserts
  a clean nil return, following
  `TestSyncExitsCleanWhenCanceledAwaitingConsent`.
- **`classifyTokenError`:** `TestRevokedGrantIsTypedUnauthorized` extended
  to assert both `Is(ErrGrantRevoked)` and `Is(ErrUnauthorized)`; a
  companion asserts an API 403 from `do()` is *not* `ErrGrantRevoked`.
- **Status:** a fixture with `error_kind=spotify_grant_revoked` yields the
  specific reason; the same fixture is added to the no-secrets test.
- **Store:** open a database migrated only to 0001, migrate, and assert
  old rows read `error_kind = ''`.
- **Engine:** `TestRunOnce_DiFMUnauthorizedIsTypedAndLogged` — kind
  recorded, watermark held, and the log line asserted (the harness
  captures logs; without that, deleting the branch passes). Also a dry
  run that swallowed a failure records `incomplete`.
- Cancellation mid-pass is recorded as `KindError` by design; the
  constant's comment says so.

## Documentation

- `CLAUDE.md`, Operator surface: nonce wording; add the rule that a reason
  string may name a *kind* — an enum the code chose — but never recorded
  text. Sync semantics: `Loop` returns on a revoked grant and `Retry-After`
  reschedules the tick; `error_kind` is set by the engine, not the store.
- `docs/deploy.md`: a runbook entry, "Spotify revoked the grant" — what the
  log says, that the consent URL is re-emitted with a new nonce, and that
  `auth --manual` still works.
- `CHANGELOG.md` under `[Unreleased]`.

## Out of scope

Retry on 5xx, a per-pass deadline, panic recovery, deriving `--max-age`
from the interval, the shape of the idle-pass log line, extra
`/status.json` fields, `sync_runs` pruning, `quick_check` on open, fuzzing
the matcher, container hardening, built-in backups. All belong to later
slices.

## Verification

1. `just check` passes: lint, actionlint, race tests, sqlc drift, config
   drift.
2. `just gen` produces no diff once the query change is committed.
3. Manual, against a stub Spotify: run `sync --loop`, then corrupt the
   stored token (`UPDATE accounts SET spotify_refresh_token='garbage'`).
   The next tick must log the revoked line, clear the token, and re-emit a
   consent URL with a new nonce. `curl /healthz` returns 503 with the
   "consent is required" reason. Completing consent resumes the loop
   without a restart.
4. Manual: a harness variant of `TestRunOnce_RateLimitAbortsPass` with
   `Retry-After: 120` shows "delaying next pass" with a 2m delay.
5. `difmsync status` and `/status.json` show `error_kind` on a failed run;
   the JSON contains neither the member id nor the refresh token.
