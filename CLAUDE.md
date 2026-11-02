# reed

Gives a browsing agent step its own browser: a fresh Chromium in a fresh Podman container. Part of heedle-ai:
weft (algebra) → draft (language) → shuttle (execution) → bobbin (runtime) →
warp (object store) → drawdown (UI); reed gives browsing
agents a browser; pirn gives them Claude; loom runs them together.

## The first rule: reuse, never rewrite

Build on battle-tested tools (Git, Temporal, the Go standard library and
other long-proven software) through their public interfaces. Add on top of
them; never fork, patch, mutate or reimplement them. Prefer the real tool to
a reimplementation of it (for example the `git` CLI over a Go port of Git).
If a task seems to need a rewrite, stop and ask the owner first, explaining
why the existing tool can't do it.

## Rules for every change

1. **Keep the wiki current.** Any change that affects how a developer
   installs, builds, tests, uses or understands this repo updates
   `docs/wiki/` in the same commit or PR. That folder is the source of truth;
   `.github/workflows/wiki.yml` mirrors it to
   https://github.com/heedle-ai/reed/wiki on every push to `main`. Never edit
   the wiki on GitHub directly: the next sync overwrites it. Code, README and
   wiki must never disagree.
2. **Wiki quality bar.** Every command is copy-pasteable and has been run.
   Every code snippet compiles and is checked by a test; prefer quoting a
   Go `Example` test with an `// Output:` comment. State required versions.
   No placeholders, TODOs or "coming soon" on published pages. Mark design
   that isn't built yet as **Planned**, plainly.
3. **Platforms: Linux (amd64, arm64) and macOS on Apple Silicon only.** Don't
   write Windows instructions, scripts, CI jobs or path handling.
4. **Bare minimum.** Add only what the task needs. No speculative packages,
   dependencies or abstractions.
5. **Tests pass before commit:** `go vet ./...` and `go test -race ./...`.
6. **Codify every mistake.** A mistake isn't fixed until it has a row in
   `docs/wiki/Invariants.md` (ID, invariant, guard, where it came from) and
   a guard that fails on the old code: a test, a fuzz seed, or a CI step
   (`ci:NAME`). `bash .github/invariants.sh` checks the page against the
   code in both directions and runs in CI. Name guard tests `TestLaw_*` or
   `TestInv_*`; every `Fuzz*` test is a guard. Keep inputs that ever broke a
   fuzz test in `testdata/fuzz/`.
7. **Show, don't tell.** Explain structure with string diagrams: SVGs in
   `docs/wiki/images/` that work in light and dark mode, using `;` for
   "then" and `⊗` for "side by side". Program diagrams are never drawn by
   hand: put a generated Mermaid block straight after the program, and the
   `TestInv_WikiDiagramsMatchPlans` test keeps them identical.
8. **Get an independent review before significant pushes.** A fresh
   reviewer that hasn't seen the work tries to break it; every finding
   becomes an invariant row.

## Repo-specific

- reed reimplements nothing. Podman runs and isolates the browser, Chromium
  does all networking and rendering, chromedp speaks the DevTools protocol,
  socat inside the container carries it over `podman exec`. The browser image is assembled from a Chromium already on
  the machine (`scripts/build-image.sh`), never a fork of one.
- Every visit is a new container (fresh browser profile), removed
  afterwards even on error or cancellation, and bounded by `VisitTimeout`.
  Never reuse a browser between visits.
- **The allowlist is the security boundary.** It comes from the program
  (the URL's host plus extra hosts), so it is compiled into the plan and
  recorded with every run. Chromium's own switches enforce it: everything
  goes to a proxy that doesn't exist unless its host is on the bypass list,
  and only allowed names resolve. Request interception is a second check
  and the record of what was blocked; on its own it misses WebSockets.
  Never weaken a switch without a container test showing the blocked host
  still receives nothing.
- No volumes, mounts or published ports. DevTools is reached only through
  `podman exec`, so nothing else on the machine can drive the browser.
  `TestInv_NoMounts` fails if a new flag or field appears.
- Every field of a page record is bounded (`Max*`), so a hostile page can't
  make an output too large for Temporal or Git.
- Error classes are for Temporal: invalid visits, setup failures and a
  page leaving the allowlist are non-retryable; a page that fails to load,
  or a browser slow to start, is retryable, in a fresh browser. A cancelled
  visit reports the cancellation, never a setup error.
- Tests that need podman and a browser image are named `TestContainer_*`
  and skip without them; CI's `containers` step builds the image from the
  runner's Chrome and runs them with `REED_REQUIRE_PODMAN=1`, where
  skipping fails. Everything else (`TestInv_*`) runs everywhere.
- `go.mod` replaces the heedle-ai modules with `../` checkouts until they
  are published; clone all repos as siblings.
