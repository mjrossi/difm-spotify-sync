## What this changes

<!-- And why the alternative was worse. -->

## Checklist

- [ ] `just check` passes (lint, workflow lint, race tests, codegen/go.mod/config drift)
- [ ] Read [`CLAUDE.md`](https://github.com/mjrossi/difm-spotify-sync/blob/main/CLAUDE.md) if this touches the sync
      ordering, the matching thresholds, the DI.fm client, or the status
      endpoints — each has invariants that are easy to break invisibly
- [ ] No new dependency, or an issue was opened to discuss one first
- [ ] Docs updated if a flag, env var or default changed — `just check`
      compares the README table against the real flags, including the
      default values, but says nothing about the prose around it
