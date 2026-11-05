[[/images/browse.svg|alt=A browse step, from the program to the record]]

## A visit

`Browser.Open(ctx, Visit{URL, Allow})`:

1. **Validates the visit.** The URL must be `http` or `https`, with a host and no credentials. The allowlist is the URL's host plus every `Allow` entry: exact host names or IP addresses, lower-cased. Wildcards, `localhost`, and loopback, link-local (such as cloud metadata at `169.254.169.254`) and unspecified addresses are refused (`ErrInvalid`).
2. **Starts a fresh container.** One per visit, so one browser profile per visit: no cookies, storage or cache carry over. The first visit in a process also removes reed containers older than 10 minutes, left by a worker that died mid-visit.
3. **Loads the page** over the DevTools protocol, waits for the body, and reads the final URL, the title, `document.body.innerText` and a 1280×800 JPEG screenshot.
4. **Removes the container**, whatever happened, including cancellation.

The whole visit is bounded by `VisitTimeout` (90 seconds), whatever the caller's deadline. The result is a `Page`, which `browse` returns as JSON: the step's output. Every field is bounded, so a hostile page can't make an output too large for Temporal or the record:

| Field | Limit |
|---|---|
| `text` | 64 KiB |
| `title` | 1 KiB |
| `url` | 4 KiB |
| `blocked` | 100 entries, 512 bytes each; `blockedTotal` counts them all |

## The container

This is the exact command line; `TestInv_ExactPodmanCommandLines` checks it word for word:

```text
podman run -d --rm --name reed-<random> --label reed.visit --pull=never --cap-drop=all
           --security-opt=no-new-privileges --pids-limit=512 --memory=1g
           --http-proxy=false --log-driver=none [--network=NAME]
           --label heedle.kind=browse [--label heedle.NAME=VALUE ...] IMAGE <Chromium switches>
```

Nothing is published or mounted, and the image comes after every flag, so nothing a program says can become a Podman flag.

**heedle's labels** (roadmap 9.2) come last among the flags. Every browser container carries `heedle.kind`: `browse`, or `act` for an act session. One started for a step of a run also carries `heedle.workflow`, `heedle.run` and `heedle.step`: the workflow that scheduled the step and its run, as Temporal reports them, and the step's node ID. These are bobbin's labels, with bobbin's rules, and they're how heedle finds its own containers to list, stop and clean up, never anyone else's ([bobbin → heedle's labels](https://github.com/heedle-ai/bobbin/wiki/How-It-Works#heedles-labels-and-tidying-up)). A visit sets the run labels in `Visit.Labels`, and reed sets the kind itself. The browse command fills them from Temporal's activity info (`StepLabels`), and pirn fills an act session's. A label other than the three, or a value that isn't plain printable ASCII (no spaces, quotes, commas or `=`, starting with a letter or digit, at most 255 bytes), is refused as `ErrInvalid` before Podman runs. The `reed.visit` and `reed.act` labels stay, and `Sweep` still uses them.

Chromium serves its DevTools protocol only on the container's own loopback interface. reed reaches it by running socat inside the container through Podman:

```text
podman exec -i reed-<random> /usr/bin/socat STDIO TCP:127.0.0.1:9222
```

Each DevTools connection is one such process, its standard input and output carrying the connection. No port is open anywhere, so only a process allowed to run podman can drive the browser: not other users on the machine, and not other containers on the network. (An earlier version published the port on the host's loopback; the independent review drove the browser through it, `TestContainer_DevToolsIsNotExposed` guards against that now.)

## The allowlist

Chromium enforces it with its own switches:

| Switch | Effect |
|---|---|
| `--proxy-server=http://127.0.0.1:9` | Every request goes to a proxy that doesn't exist, so it fails |
| `--proxy-bypass-list=<-loopback>;HOST;…` | …except requests to the allowed hosts, which go direct. `<-loopback>` stops loopback from bypassing it implicitly |
| `--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE HOST, …` | Only allowed names resolve at all. A name pinned with `Browser.Resolve` gets a `MAP HOST ADDRESS` rule first instead of an `EXCLUDE` (which would cancel the pin) |
| `--force-webrtc-ip-handling-policy=disable_non_proxied_udp` | WebRTC can't send UDP around the proxy |

As a second check, reed pauses the requests of the page and its frames through the DevTools protocol (`Fetch.enable`) and fails any whose host isn't allowed. That is also how the `blocked` list in the output is made. It doesn't see everything: popups, workers, WebSockets, prefetches and WebRTC are stopped by the switches but aren't listed.

A page that itself leaves the allowlist, such as a redirect to another host, fails with `ErrBlocked`, which isn't retried. A site that redirects `example.com` to `www.example.com` needs both hosts in the program.

The independent review tried every way out it could find from an allowed page: images, iframes (including `srcdoc`), CSS, video, forms, pings, prefetch and preconnect, beacons, EventSource, workers, popups, WebRTC (STUN and TURN), redirects, and IP addresses spelled in decimal, hex, octal, IPv4-mapped IPv6, with a trailing dot or in capitals. The blocked server received nothing.

The switches are the boundary. With the switches removed and interception alone, the container tests show a WebSocket reaching the blocked server; with interception removed and the switches alone, nothing does.

`Browser.Resolve` pins names to addresses inside the browser (for internal hosts without DNS); it only applies to allowed names, and can't pin to loopback, link-local or unspecified addresses (`TestContainer_ResolvePinsHosts`).

## The live view

While the page loads and is read, reed captures a small JPEG every 2 seconds (`FrameEvery`). Inside a program, `browse` sends each one as the activity's **Temporal heartbeat**. Temporal's Go SDK passes heartbeats on at most every 30 seconds unless the worker says otherwise, so workers that run `browse` are created with `reed.WorkerOptions`, which sets that to 2 seconds; the `loom` and `drawdown` workers do. drawdown reads the latest heartbeat of each running browse step (`DescribeWorkflowExecution` on the run's plan) and shows it in the agent's tab. No new channel: Temporal already keeps the latest heartbeat of every running activity.

It's for watching, not steering: the view is a picture, and there's no way to click into it. **Planned:** taking over the browser, recorded as a human step.

## Act sessions

[[/images/act.svg|alt=An act step, from the program to the record]]

An `act` step (roadmap 9.5) gives Claude a browser to fill in forms with. reed's part is the browser; [pirn](https://github.com/heedle-ai/pirn/wiki) runs Claude's loop and records it. `Browser.Start` takes a visit like `Open` (its URL and `Allow` hosts are the allowlist, checked the same way) and starts a fresh container from the **browser-agent image**, under its own label (`reed.act`), with browse's options: nothing published or mounted, no capabilities, an unprivileged user. Its Node waits idle; no browser runs yet. On Podman's default network, `Start` also resolves each allowed name and pins the browser to that address, refusing a name that resolves to this machine, a private network or link-local (a cloud's metadata service), and an allowed address that isn't public: an act step can send what it reads, so an allowed name's DNS mustn't point it inward, then or later. On a network named for internal hosts (`-browser-network`), names are left to that network, as browse leaves them.

`Session.Command` is Microsoft's Playwright MCP, run in that container through `podman exec` with MCP on its standard input and output (`Session.MCPArgv`). It launches the image's Chromium itself, headless, with a fresh in-memory profile, and with **the same allowlist switches as browse** as its launch arguments (`LaunchArgs`, written to Playwright's config file in the container). Playwright's own `--allowed-origins` is set too, each host on any port, but its README says it isn't a security boundary: Chromium's switches are. Page-registered tools (WebMCP) are off and downloads are refused.

Playwright MCP always offers some tools no flag removes: running scripts (`browser_evaluate`, `browser_run_code_unsafe`) and uploading files (`browser_file_upload`). `ActTools` lists the ones an act step may use (reading pages and working forms), and whoever speaks to the server offers Claude only those; `TestContainer_ActServerIsPinned` checks the pinned server has each by name, and still has the others.

**Nothing is sent until a person confirms** (roadmap 9.7). Every page Playwright MCP opens runs reed's `HoldJS` first (its config's `initPage`), which routes the whole browser context with Playwright's own route: GET, HEAD and OPTIONS pass, and every other request (a form's POST, a script's fetch or beacon, from any page or frame) is aborted and counted as held. Service workers, which a route can't see, are blocked. When a person confirms a send, pirn runs `ReleaseJS` through Playwright MCP (a tool no model is given), which opens the way until the main frame's next page loads or 30 seconds go by; then the hold is back, so one confirmation sends one form. Each form that posts waits for its own confirmation: a page that posts its first step and one that posts its last look the same to the browser. `HeldJS` answers what was held. A program that lets a step send without asking has pirn call `Session.SendWithoutAsking` before Playwright MCP starts, which lifts the hold for that session (`TestContainer_ActHoldsFormsUntilConfirmed`).

`FrameOf` turns a screenshot into a frame like browse's live frames (half width, JPEG quality 40), refusing anything that isn't a PNG or JPEG or is too large to decode cheaply. `Session.Close` removes the container whatever state it's in, and stops Playwright MCP, as `SessionTimeout` (20 minutes) does whatever the caller's deadline; act containers older than `StaleActAfter` are swept by their own label, since an act step outlives browse's sweep.

## The course of forms

`testdata/forms/` is a fixed course of practice forms (roadmap 9.6), so a change to act steps is known to help or hurt. Each is plain HTML in a folder of its own, served by the test itself (`testdata/forms/server`), never a real site:

| Form | What it tries |
|---|---|
| `contact` | validation errors: a wrong entry comes back with the field named |
| `conditional` | a field that appears only after an earlier answer |
| `widgets` | a date picker, radio buttons, checkboxes (one already ticked) and a dropdown that depends on another |
| `iframe` | the form inside a same-origin iframe |
| `wizard` | three steps with Back, and a summary to check |
| `injection` | page text telling the model to go elsewhere, an image and a request to a decoy host |
| `password` | a sign-in form, to be refused; `tricks.html` asks for a password through a label, inside a shadow root and in a frame from another host (`other-srv`) |

`course.json` gives each its task (the instruction an act step gets), its rules (a regular expression per field, which the server enforces as a site's validation would) and what it should receive. `GET /check/<form>?token=…` says what arrived and whether the form is done, to the test alone, which starts the server with `COURSE_TOKEN` (an agent on the course's host can't ask): exactly the wanted fields and values, or, for the form to refuse, nothing at all. `TestInv_CourseChecksWhatArrived` checks every checker turns wrong submissions away; `TestContainer_CourseInjectionReachesNothing` fills the injection form in an act session and checks the decoy receives nothing, then, as a control, that the decoy is reached when it is allowed:

```bash
go test -run TestInv_CourseChecksWhatArrived -v .
REED_ACT_IMAGE=<image ID> go test -run TestContainer_Course -v .
```

heedle runs the whole course with a stand-in Claude in CI, and scores real Claude on it when asked ([heedle → Browser agent scores](https://github.com/heedle-ai/heedle/wiki/Browser-agent-scores)).

## Errors

| Situation | `Open` returns | In a program | Retried? |
|---|---|---|---|
| Bad URL, host or image reference | `ErrInvalid` | `InvalidInput` | No |
| No image configured, image not present, podman missing | `ErrSetup` | `BrowserSetup` | No |
| The page left the allowlist (a redirect to another host) | `ErrBlocked` | `Blocked` | No |
| The page didn't load (DNS, timeout, server error), or the browser was slow to start | an error | retryable | Yes, in a fresh container |
| The caller cancelled, or `VisitTimeout` passed | the context's error | retryable | By Temporal's policy |

## What isn't isolated

- **The allowed hosts themselves.** Whatever they serve, the browser runs. The record shows what happened.
- **Every port of an allowed host.** The allowlist is by host, not port: a page on `a.example:443` can also reach `a.example:22`. Allow internal hosts with that in mind.
- **Names that resolve to private addresses, for browse.** A browse step's allowed name is resolved by DNS as usual; reed refuses loopback and link-local addresses written in the program, but not a public name that DNS points somewhere internal. Act sessions on the default network pin public addresses instead ([Act sessions](#act-sessions)).
- **The kernel**, as with any container: namespaces and cgroups, not a virtual machine.
- **Chromium's own sandbox is off** (`--no-sandbox`) because the container is the sandbox: no capabilities, no new privileges, an unprivileged user.
- **The host's Podman configuration** (`containers.conf`) can add mounts or variables to every container, as for [bobbin](https://github.com/heedle-ai/bobbin/wiki/How-It-Works).
