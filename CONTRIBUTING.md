# Contributing

Issues and pull requests are welcome. This is a small, opinionated project —
reading [`CLAUDE.md`](CLAUDE.md) first will save you a round trip, since it is
the actual contract for how code here is written rather than a summary of it.

## Getting set up

Tooling is managed by [mise](https://mise.jdx.dev). One command provisions the
pinned Go toolchain, sqlc, goose, just, golangci-lint and sqlite:

```sh
mise install
```

Credentials are only needed for the recipes that talk to a real service. A
fresh clone runs the whole test suite without them.

```sh
cp .env.local.example .env.local   # then fill it in
chmod 600 .env.local
```

## The gate

```sh
just check     # lint + workflow lint + race tests + codegen/go.mod/config drift
```

CI runs exactly this, plus a container build and a smoke test that the image
actually starts. Run it before pushing; it takes under a minute.

`just` on its own lists every recipe.

## Conventions worth knowing up front

- **Standard library first.** The dependency set is deliberately five entries
  long. Please open an issue before adding a sixth — it is a discussion worth
  having and a cheap one to have early.
- **`internal/store/sqlite/gen/` is generated.** Edit the queries in
  `queries/`, then `just gen`. `just check` fails on drift. Schema changes are
  goose migrations in `migrations-sqlite/`.
- **`pkg/difm` and `pkg/match` must not import `internal/`.** They are useful
  independently of this binary, and that is the property keeping them honest.
- **`pkg/match` is where matching quality is proven.** Any weight or threshold
  change has to keep both directions passing: wrong edits stay below the auto
  bar, and genuine matches stay above it. A change that improves one number by
  breaking the other is not an improvement.
- **The DI.fm client tests run against recorded fixtures**
  (`pkg/difm/testdata/`), never the live API. Please keep it that way — the API
  is private and undocumented, and hammering it from CI is how access gets
  withdrawn for everyone.
- **Decode DI.fm responses defensively.** Unknown fields ignored, absent fields
  zero, a malformed record skipped rather than failing the batch — but the pass
  is marked unclean so the watermark holds.

## Things that look like bugs and are not

Before filing, these are all deliberate:

- Deleting a track from the Spotify playlist does not re-add it, and un-liking
  on DI.fm does not remove it. The sync is one-way and add-only.
- A pass that swallowed *any* error leaves the watermark where it was and exits
  non-zero. That is what makes a partial failure recoverable.
- The status endpoints never include the text of a recorded error, even though
  the CLI prints it.
- The consent server stays up after a *failed* consent. A denied grant has to
  be retryable by clicking the URL again, not by restarting the container.

## Commit messages

Say what changed and why the alternative was worse. Most of the comments in
this codebase exist because a subtlety cost someone an afternoon; commit
messages are held to the same standard.
