## What this changes

<!-- And why the alternative was worse. -->

## Checklist

- [ ] `just check` passes (lint, race tests, codegen drift, config drift)
- [ ] Read [`CLAUDE.md`](https://github.com/mjrossi/difm-spotify-sync/blob/main/CLAUDE.md) if this touches the sync
      ordering, the matching thresholds, the DI.fm client, or the status
      endpoints — each has invariants that are easy to break invisibly
- [ ] No new dependency, or an issue was opened to discuss one first
- [ ] Docs updated if a flag, env var or default changed (`just verify-config`
      catches the README table, not the prose)
