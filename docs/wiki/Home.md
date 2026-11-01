**reed** gives a browsing agent step its own browser. Each `(browse …)` step runs a brand-new Chromium in a brand-new [Podman](https://podman.io) container, reaches only the hosts its program names, and records what it saw: the page's text, a screenshot, the final URL and every request it refused. While it runs, its live view appears in the agent's own tab in [drawdown](https://github.com/heedle-ai/drawdown/wiki).

```lisp
(pipe (browse "https://shop.example/pricing" "cdn.shop.example")
      (assess "the price went up"))
```

The first argument is the page; the rest are further hosts it may load from. That allowlist is part of the program, so it's compiled into the plan and recorded with every run.

[[/images/browse.svg|alt=A browse step: the visit and its allowlist start a fresh container with Chromium, reached through podman exec and socat; Chromium enforces the allowlist; frames go to Temporal heartbeats and the agent's tab; the page becomes the step's output and is recorded in Git; the Git store is outside every container]]

## Why

A web page can't be replayed: it changes, and it's different for everyone. So an agent that reads the web needs two things the rest of heedle-ai doesn't:

- **A boundary.** The agent should reach only the sites it was asked to, and nothing it loads (scripts, images, frames, WebSockets) should reach anywhere else.
- **A record of what it saw.** The output of a browse step is the evidence: text, screenshot, URL, and the requests it blocked, stored in [warp](https://github.com/heedle-ai/warp/wiki) like every other output.

reed reimplements nothing. Podman isolates the browser, Chromium does the networking and rendering and enforces the allowlist with its own switches, [chromedp](https://github.com/chromedp/chromedp) speaks Chrome's DevTools protocol, and socat carries it out of the container through `podman exec`.

## Where reed fits

| Repo | Role |
|---|---|
| [weft](https://github.com/heedle-ai/weft/wiki) | Composition algebra |
| [draft](https://github.com/heedle-ai/draft/wiki) | Language: s-expressions that compile to shuttle plans |
| [shuttle](https://github.com/heedle-ai/shuttle/wiki) | Durable execution on Temporal |
| [bobbin](https://github.com/heedle-ai/bobbin/wiki) | A fresh Podman container per agent step |
| **reed** | A fresh browser per browsing step (this repo) |
| [warp](https://github.com/heedle-ai/warp/wiki) | Content-addressed store |
| [drawdown](https://github.com/heedle-ai/drawdown/wiki) | Web UI: one tab per agent, with its live browser |
| [loom](https://github.com/heedle-ai/loom/wiki) | Runs them together |

## Pages

- [Getting Started](Getting-Started): build a browser image, run the tests, use `browse`
- [How It Works](How-It-Works): the container, the allowlist, the live view, errors
- [Invariants](Invariants): every guarantee, the test that guards it, and where it came from
- [Contributing](Contributing)

## Requirements

- Go **1.24** or later
- **Podman 4.9** or later, and **socat**, on Linux (amd64 or arm64)
- A Chromium on that machine to build the image from: Google Chrome, Chromium, or Chrome's headless shell
- macOS on Apple Silicon: Podman runs containers in its Linux VM, so the image must be built inside Linux. **Planned:** a tested recipe for building the image on a Mac.
