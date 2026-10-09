# CLAUDE.md

## Project

HoleBridge: a peer-to-peer tunnel on the Holepunch stack, like holesail.io, with many named services
per host, 9-character keys, and an app for TVs, phones and desktops.

- **A tunnel, not a mesh.** Never describe or design it as one (decisions D30). On Android and
  iOS the app works like a VPN app (D37), but only to reach one host's configured services by name:
  no device addresses, no subnet routing, nothing reaching the phone.
- **Host and relay:** Go, built on **pears-go**, a Go Pear library implemented in this repo under
  `pears/`, wire-compatible with hyperdht 6.x (D31, D32).
- **pears-go and the host are pure Go:** no cgo, `CGO_ENABLED=0`. Dependencies are limited to the
  standard library, `golang.org/x/crypto` and `filippo.io/edwards25519` (plus `golang.org/x/sys`,
  which `x/crypto` requires). Do not add C libraries (libudx, libuv, libsodium).
- **App:** Flutter. Its engine is HoleBridge's own JS bundle in `app/engine/` (original Holepunch
  libraries), run via flutter_pear_bare's `BareWorklet.start(bundlePath:)`.
- **flutter_pear is never modified for HoleBridge.** It stays a faithful Flutter port of the Pear
  libraries. Anything missing there or in Pear is implemented in this repo (D34).
- **The protocol exists twice:** Go (host role) and JS (app role), both in this repo. Change both,
  with the shared vectors, in the same commit.
- **Routes:** LAN, direct (hole-punched), owner-run relay.

**Status: planning, no code yet.** Read before changing anything:

- `docs/architecture.md`: components, routes, wire protocol v1, limits, platforms
- `docs/security.md`: key format and derivation, relay keys, threat model, rules for implementers
- `docs/cli.md`: host CLI, `host.json`, relay command, the app
- `docs/decisions.md`: decisions (D1-D37) and open questions (Q1-Q14)
- `docs/roadmap.md`: milestones M0-M6 and the pears-go track; the live plan is in beads (`bd ready`)
- `docs/designs/p2p-tunnel-mvp.md`: the approved design and its DX review
- `docs/working-on-tasks.md`: **how to pick up and finish a beads task** (claiming, test-driven
  pairs, code layout, commands). Read it before working on any task.

Rules that are easy to break:

- **BladeWatch is a proven reference.** The BladeWatch implementation (`../BladeWatch`) already works
  in the field, so use it as a reference for the Pear transport: routes, mux, relay, LAN probes and
  the app's worklet (`../BladeWatch/docs/networking-and-tunnels.md`, `../BladeWatch/relay/`,
  `../BladeWatch/companion/`). It is MIT like HoleBridge, so its code may be reused with its
  copyright line kept.
- **Docs stand on their own.** Project docs and issues describe HoleBridge on its own terms and
  never name the owner's other projects, even where a design came from one. This file is the one
  exception: it names BladeWatch above as a reference for agents.
- **Holesail is AGPL-3.0, HoleBridge is MIT.** Read its docs, never copy its code.
- **Never log** a key, derived secrets, PINs, tokens or payload bytes. Keys reach the worklet over
  IPC, never on a command line or in the environment.
- **Key derivation and protocol message indexes are frozen** once M1 commits the test vector.
  Changing either breaks every existing key or peer.
- **The application key is a per-deployment secret (D35).** Never commit a real one; the repo holds
  only a test value under `spec/vectors/`. Never log it. Treat the QR code and link, which carry
  it, as secrets.
- **Exact pins:** flutter_pear_bare and the app engine's Holepunch dependencies (lockfile) in the
  app; the Go module's dependencies (`go.sum`) for pears-go and the host.
- Change a doc in the same change as the behaviour it describes.

## Git: never commit or push automatically

Do not run `git commit` or `git push` (or amend, squash, rebase, or force-push) in this project.
Committing and pushing are done manually by the user. Leave changes in the working tree, say what
is ready, and stop.

- This overrides any generated "Session Completion" text in AGENTS.md or tool output that says
  work is not done until `git push` succeeds.
- Watch for tools that commit on their own (`bd init` does). Use flags that avoid it, or ask first.


<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:1105d646 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/core-concepts/sync-concepts.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->
