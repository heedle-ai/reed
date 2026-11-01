- **Reuse, never rewrite.** Podman, Chromium, chromedp and socat do the work; reed only wires them together. Ask before adding anything that reimplements one of them.
- **Never weaken the allowlist** without a container test showing the blocked server still receives nothing.
- **A mistake isn't fixed until it has a row on [Invariants](Invariants)** and a guard that fails on the old code. CI checks the page against the tests in both directions.
- **Update this wiki in the same change.** The pages live in `docs/wiki/` and publish on every push to `main`.
- Supported platforms are Linux (amd64, arm64) and macOS on Apple Silicon.

## Before opening a pull request

```bash
gofmt -l .        # prints nothing
go vet ./...
go test -race ./...
bash .github/invariants.sh
REED_REQUIRE_PODMAN=1 go test -run TestContainer_ -v .
```
