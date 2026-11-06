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

**heedle's labels** (roadmap 9.2) come last among the flags. Every browser container carries `heedle.kind`: `browse`, or `act` for an act session. One started for a step of a run also carries `heedle.workflow`, `heedle.run` and `heedle.step`: the workflow that scheduled the step and its run, as Temporal reports them, and the step's node ID. These are bobbin's labels, with bobbin's rules, and they're how heedle finds its own containers to list, stop and clean up, never anyone else's ([bobbin → heedle's labels](https://github.com/heedle-ai/bobbin/wiki/How-It-Works#heedles-labels-and-tidying-up)). A visit sets the run labels in `Visit.Labels`, and reed sets the kind itself. The browse command fills them from Temporal's activity info (`StepLabels`), and pirn fills an act session's. A browser started by a worker that knows its home also carries `heedle.home` (roadmap 9.2.1), bobbin's 12 hex digits for the worker's store, so two heedle homes running at once each tidy only their own: the worker puts it on the context all its activities start from (`WithHome`), `StepLabels` reads it, and `Open` and `Start` add it to a visit that names none. A home that isn't 12 lowercase hex digits is refused as `ErrInvalid`. A label other than these four, or a value that isn't plain printable ASCII (no spaces, quotes, commas or `=`, starting with a letter or digit, at most 255 bytes), is refused as `ErrInvalid` before Podman runs. The `reed.visit` and `reed.act` labels stay, and `Sweep` still uses them. `Sweep` doesn't look at homes: it only ever removes a browser past its own deadline (a visit's `VisitTimeout`, 90 s, is well under `StaleAfter`, 10 minutes; an act session's `SessionTimeout` under `StaleActAfter`), so one home's sweep never cuts another home's live browser.

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

A browse step's view is for watching. An act step's can be driven, when the step asks for a person ([Act sessions](#act-sessions), roadmap 9.10).

## Act sessions

[[/images/act.svg|alt=An act step, from the program to the record]]

An `act` step (roadmap 9.5) gives Claude a browser to fill in forms with. reed's part is the browser; [pirn](https://github.com/heedle-ai/pirn/wiki) runs Claude's loop and records it. `Browser.Start` takes a visit like `Open` (its URL and `Allow` hosts are the allowlist, checked the same way) and starts a fresh container from the **browser-agent image**, under its own label (`reed.act`), with browse's options: nothing published or mounted, no capabilities, an unprivileged user. Its Node waits idle; no browser runs yet. On Podman's default network, `Start` also resolves each allowed name and pins the browser to that address, refusing a name that resolves to this machine, a private network or link-local (a cloud's metadata service), and an allowed address that isn't public: an act step can send what it reads, so an allowed name's DNS mustn't point it inward, then or later. On a network named for internal hosts (`-browser-network`), names are left to that network, as browse leaves them.

`Session.Command` is Microsoft's Playwright MCP, run in that container through `podman exec` with MCP on its standard input and output (`Session.MCPArgv`). It launches the image's Chromium itself, headless, with a fresh in-memory profile, and with **the same allowlist switches as browse** as its launch arguments (`LaunchArgs`, written to Playwright's config file in the container). Playwright's own `--allowed-origins` is set too, each host on any port, but its README says it isn't a security boundary: Chromium's switches are. Page-registered tools (WebMCP) are off and downloads are refused.

Playwright MCP always offers some tools no flag removes: running scripts (`browser_evaluate`, `browser_run_code_unsafe`) and uploading files (`browser_file_upload`). `ActTools` lists the ones an act step may use (reading pages and working forms), and whoever speaks to the server offers Claude only those; `TestContainer_ActServerIsPinned` checks the pinned server has each by name, and still has the others.

**Nothing is sent until a person confirms** (roadmap 9.7). Every page Playwright MCP opens runs reed's `HoldJS` first (its config's `initPage`), which routes the whole browser context with Playwright's own route: GET, HEAD and OPTIONS pass, and every other request (a form's POST, a script's fetch or beacon, from any page, frame, popup or worker) is aborted and counted as held. Service workers, which a route can't see, are blocked; a WebSocket opened while the hold is on never reaches its server, and what the page sends on it is counted. A form that sends with GET is a request like any page load, and isn't held. When a person confirms a send, pirn runs `ReleaseJS` through Playwright MCP (a tool no model is given) just before the one action that sends, and `CloseJS` just after it; the main frame loading a page, or 30 seconds, closes the way too, so one confirmation sends one form and doesn't carry over to another page. The hold's module and the opt-out sit in their own read-only folder (`ActHoldDir`), apart from where Playwright MCP writes. Each form that posts waits for its own confirmation: a page that posts its first step and one that posts its last look the same to the browser. `HeldJS` answers what was held. A program that lets a step send without asking has pirn call `Session.SendWithoutAsking` before Playwright MCP starts, which lifts the hold for that session (`TestContainer_ActHoldsFormsUntilConfirmed`).

**Your details stay out of sight** (roadmap 9.8). A step that may use some of the person's details gets them with `Session.UseProfile` before Playwright MCP starts: each value under its placeholder, such as `⟨email⟩`, as one of Playwright MCP's own `secrets`. Where the model types the placeholder as a field's whole value (`browser_type`, or a text field in `browser_fill_form`), Playwright MCP types the real value, and wherever the value would appear in what it answers (snapshots, logs, results) it shows the placeholder instead. The values reach the container on standard input, never a command line, and also go read-only beside the hold (`ActProfile`), for frames: `ShotJSFor(n)`, run through Playwright MCP, is a screenshot with every password field, every field a person typed into while driving, every field holding one of the values, and every piece of text showing one, masked with Playwright's own screenshot mask, in every frame of the page, open shadow roots included. A value is matched by its letters and digits, so one the page uppercases, reformats or spells out is masked too, and each field is marked for the mask in the same pass that matched it; it fails, rather than give an unmasked frame, unless the browser holds exactly the `n` values it should. While the hold is on, a request carrying one of the values, in its URL or body, plainly, URL-encoded or as a phone number's digits, is held like a send whatever its method, so a page can't read a filled field and pass it on with a GET (`TestContainer_ActUsesProfileWithoutShowingIt`). A form that sends with GET and no detail is still a request like a page load.

With `--site-per-process` a frame from another site runs in a process of its own, and the hold covers it as it covers the page: its fetches, forms and WebSocket messages are held (`TestContainer_ActMasksAcrossSites`). **A person can take the wheel** (roadmap 9.10). When an act step asks for a person ([pirn → Take the wheel](https://github.com/heedle-ai/pirn/wiki/How-It-Works#take-the-wheel)), pirn runs `PersonJS`. From then on each of the person's inputs that may send (a click; Enter, except in a textarea or contenteditable, where it makes a new line; Space, except in a field that takes text, where it types a space) opens the hold for `PersonWindow` (3 seconds) from that input, never the whole pause, and a character typed opens nothing: what the browser sends in that window goes through, counted as the person's, and what the page sends on its own later, a fetch or a message on a WebSocket, is held as it would be with Claude driving. `PersonDoneJS` closes the way at once and answers what they sent; if it can't run (a dialog left open), the way has closed anyway once `PersonWindow` has passed. A WebSocket opened while the way is open connects, but each message the page sends on it goes through only while the way is still open, so one opened by a confirmed send or while a person drove doesn't stay open after. Their clicks and keys reach the page as DevTools input events, through Playwright's own mouse and keyboard, run by Playwright MCP: `ClickJS(x, y)` clicks a point on the page (refusing one off it), and `KeyJS(key)` presses one named key (Enter, Tab, Shift+Tab, Backspace, Delete, Escape, the arrows, Home, End, PageUp, PageDown) or types one printable character, which reaches the page's script only as a string literal (`IsKey`; anything else, a shortcut such as Control+a, is refused). Each key that edits (a character, Backspace, Delete) marks the field it goes to (`data-heedle-typed`, in whichever frame or open shadow root has the focus, inside an iframe too, before and after the key, so a code box that moves on by itself stays marked); other keys mark nothing, so tabbing through fields the agent filled hides none of them. Each key answers the focused field's value (a contenteditable's text) if it was typed into, at most `MaxTypedValue` (4096) characters, a longer value hidden by its start; at hand-back `TypedJS` answers the values of those fields and of every password field, in every frame, so pirn hides what was typed however it was typed, a typo fixed with an arrow key included. What a field held when the person left it, and those values (not one in a password field when read, whose field is masked as a field; a field shown as text by a show-password toggle is read as text), are masked in every later frame as text too, wherever the page echoes them ("Signed in as …"), if at least four characters with at least three letters or digits: a shorter one is masked only in its field. A dialog the page opens (`alert`, `confirm`) can't be answered through the wheel, as Playwright MCP runs no script while one is open: hand back, and Claude answers it, and every frame (`ShotJSFor`) masks password fields and those fields, whatever the step uses, so what a person typed never shows in a frame (`TestContainer_ActPersonDrives`). Masking matches the values, the person's details and what they typed, in a DevTools isolated world of each frame (`worldsJS`; a frame from another site, in a process of its own under `--site-per-process`, through its own DevTools session), never in the page's own script, so a page that replaces its `String`, `RegExp` or `Array` methods, or a field's `value`, sees and changes none of them; and it writes nothing to the page: it answers the boxes of what matched, read before and after the screenshot, and `ShotOf` paints them over the image in Go, so no attribute tells a page which of its elements hold a value (review 52). A box is a field holding a value, or the smallest element whose text shows one together with each of its text nodes, so text spilling out of a narrow element is covered too; a frame from another site is placed, offset and scale, by DevTools from the `<iframe>` holding it. Every frame of the page must be reached, or there is no screenshot; a DevTools session whose frame moved to another process is made again. If any frame's document changed between the two readings, or any box moved, or a frame can't be read, there is no screenshot at all, never an unmasked one: a page can deny frames so (a document reloading without end, or more than 20 million characters of text to match), but never get an unmasked one. The person's field values are read in that world too (`KeyJS`, `TypedJS`); a key needs only the frame with the focus to be read, so frames coming and going never lose it; a key that doesn't edit, and a click, never wait on that read, while a key that edits, Space and Enter do (Space with nothing focused too: whether it types or presses depends on the field).

`FrameOf` turns a screenshot into a frame like browse's live frames (half width, JPEG quality 40), refusing anything that isn't a PNG or JPEG or is too large to decode cheaply. `Session.Close` removes the container whatever state it's in, and stops Playwright MCP, as `SessionTimeout` (5 hours, longer than an act step with every send it may wait 30 minutes on and every hand-over to a person it may wait 30 minutes on) does whatever the caller's deadline; act containers older than `StaleActAfter` are swept by their own label, since an act step outlives browse's sweep.

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

heedle runs the whole course with a stand-in Claude in CI, twice, so the second time each form that went through replays without Claude ([pirn → Replay without Claude](https://github.com/heedle-ai/pirn/wiki/How-It-Works#replay-without-claude), roadmap 9.9), and scores real Claude on it when asked ([heedle → Browser agent scores](https://github.com/heedle-ai/heedle/wiki/Browser-agent-scores)). A replay is only data driving Playwright MCP's own tools, so the browser's allowlist and hold apply to it unchanged.

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
