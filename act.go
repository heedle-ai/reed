package reed

// Act sessions (roadmap 9.5): a browser that Claude works through
// Microsoft's Playwright MCP, for a step that fills in forms. reed starts
// the container and confines its browser; pirn speaks MCP to the server
// over the session's standard input and output.
//
// The browser-agent image is the browser image plus Node and a pinned
// @playwright/mcp (scripts/build-image.sh with BUILD_IMAGE_NODE and
// BUILD_IMAGE_MCP). Its container starts idle; Playwright MCP runs in it
// through `podman exec` and launches the image's Chromium itself
// (--executable-path), with the same allowlist switches as browse's
// browser (allowlistSwitches) as its launch arguments. Playwright's own
// origin allowlist is a second check: its README says it isn't a security
// boundary, and Chromium's switches are.

import (
	"bytes"
	"context"
	"crypto/rand"
	stdbase64 "encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	stdjpeg "image/jpeg"
	_ "image/png" // Playwright's screenshots may be PNG
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/draw"
)

// Where the browser-agent image keeps things, and where a session's
// Playwright MCP writes.
const (
	ActNode    = "/node/bin/node"
	ActMCP     = "/mcp/node_modules/@playwright/mcp/cli.js"
	ActBrowser = "/browser/chromium"
	ActDir     = "/tmp/act" // Playwright MCP's configuration and output, inside the container
	ActConfig  = ActDir + "/config.json"
	// ActHoldDir holds the module Playwright MCP runs on every page it
	// opens (its initPage), holding form submissions (HoldJS), and
	// ActSendFreely, a file that lifts the hold for a whole session
	// (SendWithoutAsking). It is apart from ActDir, where Playwright MCP
	// writes, and read-only once written (review 41).
	ActHoldDir    = "/tmp/act-hold"
	ActHold       = ActHoldDir + "/hold.cjs"
	ActSendFreely = ActHoldDir + "/send-without-asking"
	// ActProfile lists the person's details a step may use (roadmap 9.8),
	// for HoldJS to mask wherever a frame shows them (ShotJS).
	ActProfile = ActHoldDir + "/profile.json"
	// ActLabel marks act containers. They live as long as a step, much
	// longer than a visit, so browse's Sweep never touches them; Start
	// sweeps those older than StaleActAfter.
	ActLabel = "reed.act"
	// SessionTimeout bounds one session, whatever the caller's deadline.
	// It outlasts an act step's own bound, which includes waiting for a
	// person to confirm each send (review 41: the browser stopped at 20
	// minutes while a send could wait 30) and to hand the browser back
	// (review 52).
	SessionTimeout = 5 * time.Hour
	StaleActAfter  = SessionTimeout + 10*time.Minute
	// FrameWidth and FrameQuality are a frame's size and JPEG quality:
	// browse's live frames' (a half-size viewport at quality 40).
	FrameWidth, FrameQuality = Width / 2, 40
	// MaxScreenshot bounds a screenshot FrameOf will decode, in bytes and
	// in pixels, so a hostile page can't make a frame costly.
	MaxScreenshot       = 8 << 20
	MaxScreenshotPixels = 4096 * 4096
)

// ActTools are the tools of the pinned Playwright MCP an act step may
// use: reading pages and working forms. Every other tool it has stays
// off, among them browser_evaluate and browser_run_code_unsafe (scripts),
// browser_file_upload, network and console inspection, and its PDF and
// coordinate tools: they're core or opt-in, and no flag removes the core
// ones, so whoever speaks to the server (pirn) offers Claude only these.
// TestContainer_ActServerIsPinned checks each exists, as named.
var ActTools = []string{
	"browser_navigate", "browser_navigate_back", "browser_snapshot",
	"browser_click", "browser_type", "browser_fill_form", "browser_select_option",
	"browser_press_key", "browser_hover", "browser_wait_for", "browser_handle_dialog",
}

// Session is one act step's browser: a fresh container from the
// browser-agent image. Close removes it.
type Session struct {
	podman  string
	name    string
	allowed []string
	resolve map[string]string
	once    sync.Once
	// stop ends Playwright MCP when SessionTimeout passes or the session
	// is closed, whatever the caller's deadline (review 39: the bound
	// was declared but never applied).
	stop context.CancelFunc
	ctx  context.Context
}

// Start validates v (its URL and Allow hosts are the allowlist, as for a
// visit) and starts a fresh browser-agent container for it, idle until
// Playwright MCP runs in it (Command). b.Image is the browser-agent image.
func (b Browser) Start(ctx context.Context, v Visit) (*Session, error) {
	v.Labels = withHome(ctx, v.Labels) // the worker's home (roadmap 9.2.1)
	allowed, err := b.check(v, "no browser-agent image configured (give the worker -browser-agent-image)")
	if err != nil {
		return nil, err
	}
	resolve, err := b.pinPublic(ctx, allowed)
	if err != nil {
		return nil, err
	}
	podman := b.podman()
	b.run = v.Labels
	// A session lasts minutes: sweeping each time costs one podman ps.
	_ = sweep(ctx, podman, ActLabel, StaleActAfter)
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	sctx, stop := context.WithTimeout(context.Background(), SessionTimeout)
	s := &Session{podman: podman, name: "reed-act-" + hex.EncodeToString(rnd[:]), allowed: allowed, resolve: resolve, ctx: sctx, stop: stop}
	if out, err := podmanCmd(ctx, podman, b.ActArgv(s.name)...); err != nil {
		s.Close()
		return nil, startError(ctx, err, out)
	}
	cfg, _ := json.Marshal(PlaywrightConfig(allowed, resolve))
	files, _ := json.Marshal(map[string]string{ActConfig: string(cfg), ActHold: HoldJS})
	put := exec.CommandContext(ctx, podman, "exec", "-i", s.name, ActNode, "-e",
		`const fs = require("fs"); for (const d of process.argv.slice(1)) fs.mkdirSync(d, { recursive: true }); `+
			`for (const [p, b] of Object.entries(JSON.parse(fs.readFileSync(0)))) fs.writeFileSync(p, b, { mode: 0o444 }); fs.chmodSync(process.argv[2], 0o555);`,
		ActDir, ActHoldDir)
	put.Env, put.Stdin = cleanEnv(), bytes.NewReader(files)
	if out, err := put.CombinedOutput(); err != nil {
		s.Close()
		return nil, startError(ctx, err, out)
	}
	return s, nil
}

// lookupIP resolves the names an act session pins (tests replace it).
var lookupIP = net.DefaultResolver.LookupIPAddr

// pinPublic is b.Resolve plus, on Podman's default network, every other
// allowed name resolved now and pinned to its first address, each of
// which must be public: so an allowed name's DNS can't point the browser
// at this machine, its network or a cloud's metadata service, then or
// later (review 39: browse only reads such a page; act could send what it
// read). An allowed address must be public too. A named network is for
// internal hosts by choice, as Resolve is, and is left as browse has it.
func (b Browser) pinPublic(ctx context.Context, allowed []string) (map[string]string, error) {
	if b.Network != "" {
		return b.Resolve, nil
	}
	out := map[string]string{}
	for h, ip := range b.Resolve {
		out[h] = ip
	}
	public := func(ip net.IP) bool { return publicEnough(ip) && !ip.IsPrivate() && !ip.IsMulticast() }
	for _, h := range allowed {
		if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
			if !public(ip) {
				return nil, fmt.Errorf("%w: %s isn't a public address; act reaches internal hosts only on a network named for them (-browser-network)", ErrInvalid, h)
			}
			continue
		}
		if _, ok := out[h]; ok {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		addrs, err := lookupIP(lctx, h)
		cancel()
		if err != nil || len(addrs) == 0 {
			return nil, fmt.Errorf("reed: can't resolve %s: %v", h, err)
		}
		for _, a := range addrs {
			if !public(a.IP) {
				return nil, fmt.Errorf("%w: %s resolves to %s, which isn't public; act reaches internal hosts only on a network named for them (-browser-network)", ErrInvalid, h, a.IP)
			}
		}
		out[h] = addrs[0].IP.String()
	}
	return out, nil
}

// ActArgv is the exact podman command line for an act container, without
// the executable: browse's options (nothing published or mounted), the
// act label, and the image's Node kept idle (no browser runs until
// Playwright MCP starts one).
func (b Browser) ActArgv(name string) []string {
	argv := append(b.runOptions(name, ActLabel), b.Image)
	return append(argv, "-e", "setInterval(() => {}, 1 << 30)")
}

// LaunchArgs are the switches act's Chromium starts with, as Playwright's
// launch arguments: browse's allowlist switches and its hardening, but no
// DevTools port or profile (Playwright speaks over a pipe and keeps the
// profile in memory) and no window flags (it sets the viewport).
func LaunchArgs(allowed []string, resolve map[string]string) []string {
	return append([]string{
		"--no-sandbox", // the container is the sandbox
		"--disable-dev-shm-usage", "--no-first-run", "--no-default-browser-check", "--crash-dumps-dir=/tmp",
		"--disable-background-networking", "--disable-component-update", "--disable-sync",
		// Every site's frames in a process of their own, as a full Chrome
		// keeps them (review 52: the headless shell didn't, so masking
		// across sites was never exercised).
		"--site-per-process",
	}, allowlistSwitches(allowed, resolve)...)
}

// PlaywrightConfig is Playwright MCP's configuration file for a session:
// Chromium's launch arguments, and downloads refused.
func PlaywrightConfig(allowed []string, resolve map[string]string) map[string]any {
	return map[string]any{"browser": map[string]any{
		"launchOptions":  map[string]any{"args": LaunchArgs(allowed, resolve)},
		"contextOptions": map[string]any{"acceptDownloads": false, "serviceWorkers": "block"},
		"initPage":       []string{ActHold},
	}}
}

// Origins is Playwright MCP's --allowed-origins for an allowlist: each
// host, by name (any scheme, default port) and on any port, over http and
// https. A bare host matches only its default ports.
func Origins(allowed []string) string {
	var o []string
	for _, h := range allowed {
		o = append(o, h, "http://"+h+":*", "https://"+h+":*")
	}
	return strings.Join(o, ";")
}

// MCPArgv is the exact command line that runs Playwright MCP in the
// session over standard input and output, without the executable: a
// headless browser, a fresh in-memory profile, the image's Chromium, the
// allowlist as a second check, no tools a page registers (WebMCP), images
// only for screenshots, and its files in ActDir.
func (s *Session) MCPArgv() []string {
	return []string{"exec", "-i", s.name, ActNode, ActMCP,
		"--headless", "--isolated", "--executable-path", ActBrowser,
		"--allowed-origins", Origins(s.allowed), "--no-webmcp",
		"--config", ActConfig, "--output-dir", ActDir,
		"--viewport-size", fmt.Sprintf("%dx%d", Width, Height)}
}

// Command is Playwright MCP running in the session (MCPArgv), not yet
// started: its standard input and output carry MCP. Ending it or closing
// the session stops it.
func (s *Session) Command(ctx context.Context) *exec.Cmd {
	ctx, stop := context.WithCancel(ctx)
	go func() {
		select {
		case <-s.ctx.Done():
		case <-ctx.Done():
		}
		stop()
	}()
	cmd := exec.CommandContext(ctx, s.podman, s.MCPArgv()...)
	cmd.Env = cleanEnv()
	return cmd
}

// HoldJS is the module Playwright MCP runs on each page it opens
// (roadmap 9.7): until a person confirms, the browser sends no form. It
// routes every request of the browser context (Playwright's own route,
// which sees what pages, frames and their scripts send; service workers,
// which a route can't see, are blocked) and lets through only GET, HEAD
// and OPTIONS. Any other request, to any host, is aborted and counted,
// unless a confirmation opened the way (ReleaseJS) for the one action
// that sends: pirn runs it just before that action and CloseJS just
// after, and the main frame loading a page, or 30 seconds, closes it too
// (review 41: one confirmation could send other forms). WebSockets, which
// a route can't hold, reach their server only if opened while the way
// is open; one opened while the hold is on never connects, and what the
// page sends on it is counted. Forms that send with GET are
// requests like any page load and aren't held. ActSendFreely, written
// before the browser starts, lifts the hold for the session. The state
// lives on the browser context, which only Playwright MCP's own process
// can reach.
const HoldJS = `exports.default = async ({ page }) => {
  const fs = require("fs");
  const context = page.context();
  let hold = context.__heedleHold;
  if (!hold) {
    hold = context.__heedleHold = { open: 0, person: 0, held: [], heldTotal: 0, byYou: [], byYouTotal: 0, profile: [], typed: [] };
    try { hold.profile = JSON.parse(fs.readFileSync(` + "`" + ActProfile + "`" + `, "utf8")); } catch {}
    const freely = fs.existsSync(` + "`" + ActSendFreely + "`" + `);
    // A person's input (ClickJS, KeyJS) opens the way for PersonWindow
    // from that input, never the whole pause (review 52).
    const yours = () => Date.now() < hold.person;
    const passes = () => freely || yours() || Date.now() < hold.open;
    const held = (what) => { hold.heldTotal++; if (hold.held.length < 100) hold.held.push(what.slice(0, 520)); };
    // What a person sends while they drive (PersonJS) is theirs, and is
    // counted as sent by them (roadmap 9.10).
    const byYou = (what) => { hold.byYouTotal++; if (hold.byYou.length < 100) hold.byYou.push(what.slice(0, 520)); };
    // A request carrying one of the person's details, in the URL or the
    // body, plainly, encoded or as a phone number's digits, is held like
    // a send, whatever its method (review 42: a page could read a filled
    // field and send it with a GET).
    const digits = (s) => s.replace(/\D/g, "");
    const carries = (r) => {
      if (!hold.profile.length) return false;
      let body = "";
      try { body = r.postData() || ""; } catch {}
      const low = (r.url() + " " + body).toLowerCase();
      let dec = low;
      try { dec = decodeURIComponent(low.replace(/\+/g, " ")); } catch {}
      return hold.profile.some((p) => {
        const v = p.toLowerCase(), d = digits(p);
        return low.includes(v) || dec.includes(v) || low.includes(encodeURIComponent(v)) || (d.length >= 7 && digits(low).includes(d));
      });
    };
    await context.route("**/*", (route) => {
      const r = route.request(), m = r.method();
      const sends = !(m === "GET" || m === "HEAD" || m === "OPTIONS") || carries(r);
      if (passes()) { if (yours() && sends) byYou(m + " " + r.url()); return route.fallback(); }
      if (!sends) return route.fallback();
      held(m + " " + r.url());
      return route.abort("blockedbyclient");
    });
    // A WebSocket opened while the way is open connects, but each message
    // the page sends on it is let through only while the way is open still
    // (review 52: one opened by a confirmed action, or while a person
    // drove, stayed open after).
    await context.routeWebSocket("**/*", (ws) => {
      if (!passes()) { ws.onMessage(() => held("WS " + ws.url())); return; }
      const server = ws.connectToServer();
      if (freely) return;
      ws.onMessage((m) => {
        if (!passes()) return held("WS " + ws.url());
        if (yours()) byYou("WS " + ws.url());
        server.send(m);
      });
    });
  }
  page.on("framenavigated", (frame) => { if (frame === page.mainFrame()) hold.open = 0; });
};
`

// ReleaseJS, run by Playwright MCP's browser_run_code_unsafe (a tool no
// model is given), opens the hold for the one action that sends, after a
// person confirmed it; CloseJS closes it again once that action is done.
// HeldJS answers the requests held so far.
const (
	ReleaseJS = `async (page) => { const h = page.context().__heedleHold; if (!h) throw new Error("no hold"); h.open = Date.now() + 30000; return "released"; }`
	CloseJS   = `async (page) => { const h = page.context().__heedleHold; if (h) h.open = 0; return "closed"; }`
	HeldJS    = `async (page) => { const h = page.context().__heedleHold; return h ? { held: h.held, total: h.heldTotal } : null; }`
)

// PersonJS, run by Playwright MCP's browser_run_code_unsafe (a tool no
// model is given), hands the browser to a person (roadmap 9.10): from then
// on each of their inputs (ClickJS, KeyJS) lets what is sent within
// PersonWindow of it through, counted as sent by them. PersonDoneJS hands
// it back, closing the way at once, and answers what they sent. The way
// is never open longer than PersonWindow after their last input, so a
// PersonDoneJS that fails (a dialog left open) leaves the hold on anyway
// (review 52).
const (
	PersonJS     = `async (page) => { const h = page.context().__heedleHold; if (!h) throw new Error("no hold"); h.person = 0; h.byYou = []; h.byYouTotal = 0; return "yours"; }`
	PersonDoneJS = `async (page) => { const h = page.context().__heedleHold; if (!h) return null; h.person = 0; return { sent: h.byYou, total: h.byYouTotal }; }`
)

// PersonWindow is how long after a person's input what the page sends is
// theirs.
const PersonWindow = 3 * time.Second

// personInput opens the way for a person's input (ClickJS, KeyJS).
var personInput = fmt.Sprintf(`const h = page.context().__heedleHold; if (!h) throw new Error("no hold"); h.person = Date.now() + %d;`, PersonWindow.Milliseconds())

// TypedJS, run by Playwright MCP's browser_run_code_unsafe at hand-back,
// answers the values of every field a person typed into (KeyJS marks them)
// and every password field, in every frame and open shadow root, read in
// an isolated world (worldsJS), so pirn can hide them, in memory only,
// however they were typed (review 52: a typo fixed with an arrow key split
// what was typed). Those not in a password field now are kept to mask as
// text in later frames.
var TypedJS = "async (page) => { " + worldsJS + strings.Replace(typedJS, "MAX", strconv.Itoa(MaxTypedValue), 1) + " }"

const typedJS = `
  const read = ((max) => {
    const out = [], roots = [document];
    while (roots.length) {
      const root = roots.pop();
      for (const e of root.querySelectorAll("*")) {
        if (e.shadowRoot) roots.push(e.shadowRoot);
        const pw = e.tagName === "INPUT" && (e.getAttribute("type") || "").toLowerCase() === "password";
        if (!pw && !e.hasAttribute("data-heedle-typed")) continue;
        const v = e.isContentEditable ? e.textContent : e.value;
        if (typeof v === "string" && v) out.push({ v: v.slice(0, max), pw });
      }
    }
    return out;
  }).toString();
  const values = [], kept = [];
  for (const vs of (await inWorlds(page, read, MAX, false)).values) for (const x of vs || []) if (values.length < 200) { values.push(x.v); if (!x.pw) kept.push(x.v); }
  const h = page.context().__heedleHold;
  if (h) for (const v of kept) if (v.length >= 4 && !h.typed.includes(v) && h.typed.length < 200) h.typed.push(v);
  return { values };`

// worldsJS defines, in a script run by Playwright MCP, inWorlds(page, fn,
// arg, rects): fn run with arg in a DevTools isolated world of every frame
// of the page, never the page's own world, so a page that replaces its
// String, RegExp or Array methods, or a field's value getter, sees and
// changes nothing (review 52). Frames in a process of their own (another
// site's) are reached through their own DevTools session. It answers each
// frame's value (by value), or, with rects, the boxes, in the page's
// viewport, of the elements fn answers, and each frame's document
// (frame and loader IDs), so a page that loads another document in
// between can be told. Sessions and worlds are kept on the browser
// context, which only Playwright's own process reaches, and a world is
// made again when its document changes.
const worldsJS = `const inWorlds = async (page, fn, arg, rects) => {
  const ctx = page.context();
  const cache = ctx.__heedleWorlds || (ctx.__heedleWorlds = { sessions: new WeakMap(), worlds: new Map() });
  const session = async (target) => { let s = cache.sessions.get(target); if (!s) { s = await ctx.newCDPSession(target); cache.sessions.set(target, s); } return s; };
  const targets = [{ s: await session(page), off: [0, 0] }];
  for (const f of page.frames()) {
    if (f === page.mainFrame()) continue;
    let s;
    try { s = await session(f); } catch { continue; }
    let off = null;
    const fe = await f.frameElement().catch(() => null);
    const box = fe && await fe.boundingBox();
    if (box) { const pad = await fe.evaluate((e) => { const c = getComputedStyle(e); return [e.clientLeft + parseFloat(c.paddingLeft), e.clientTop + parseFloat(c.paddingTop)]; }); off = [box.x + pad[0], box.y + pad[1]]; }
    targets.push({ s, off, own: true });
  }
  for (const t of targets) t.tree = (await t.s.send("Page.getFrameTree")).frameTree;
  const roots = new Set(targets.filter((t) => t.own).map((t) => t.tree.frame.id));
  const out = { values: [], rects: [], docs: [] };
  for (const t of targets) {
    const frames = [], todo = [t.tree];
    while (todo.length) { const n = todo.pop(); if (n !== t.tree && roots.has(n.frame.id)) continue; frames.push(n.frame); todo.push(...(n.childFrames || [])); }
    for (const fr of frames) {
      const key = fr.id + "/" + fr.loaderId;
      out.docs.push(key);
      let r = null;
      for (let attempt = 0; !r; attempt++) {
        let id = cache.worlds.get(key);
        if (!id) { id = (await t.s.send("Page.createIsolatedWorld", { frameId: fr.id, worldName: "heedle" })).executionContextId; cache.worlds.set(key, id); }
        try { r = await t.s.send("Runtime.callFunctionOn", { functionDeclaration: fn, executionContextId: id, arguments: [{ value: arg }], returnByValue: !rects, objectGroup: "heedle" }); }
        catch (e) { cache.worlds.delete(key); if (attempt) throw e; }
      }
      if (r.exceptionDetails) throw new Error("a frame couldn't be read");
      if (!rects) { out.values.push(r.result.value); continue; }
      if (t.off) {
        const { result } = await t.s.send("Runtime.getProperties", { objectId: r.result.objectId, ownProperties: true });
        for (const p of result) {
          if (!p.value || p.value.subtype !== "node") continue;
          let q;
          try { q = await t.s.send("DOM.getContentQuads", { objectId: p.value.objectId }); } catch { continue; } // not rendered
          for (const d of q.quads) {
            const xs = [d[0], d[2], d[4], d[6]], ys = [d[1], d[3], d[5], d[7]];
            out.rects.push([Math.min(...xs) + t.off[0], Math.min(...ys) + t.off[1], Math.max(...xs) - Math.min(...xs), Math.max(...ys) - Math.min(...ys)]);
          }
        }
      }
      await t.s.send("Runtime.releaseObjectGroup", { objectGroup: "heedle" }).catch(() => {});
    }
  }
  if (cache.worlds.size > 500) cache.worlds.clear();
  return out;
};`

// keys are the named keys a person may press while driving; any other
// key is one printable character, typed.
var keys = map[string]bool{"Enter": true, "Tab": true, "Shift+Tab": true, "Backspace": true, "Delete": true, "Escape": true,
	"ArrowUp": true, "ArrowDown": true, "ArrowLeft": true, "ArrowRight": true, "Home": true, "End": true, "PageUp": true, "PageDown": true}

// IsKey says whether key is one a person may press while driving: a named
// key, or one printable character.
func IsKey(key string) bool {
	if keys[key] {
		return true
	}
	r, n := utf8.DecodeRuneInString(key)
	return n == len(key) && n > 0 && r != utf8.RuneError && unicode.IsPrint(r)
}

// personField defines, in a person's input's script, field(mark): the
// focused field, in whichever frame or open shadow root has the focus
// (never a frame's <iframe> element: the field inside it), read in an
// isolated world (worldsJS), marked as typed into if mark, as {v, pw,
// text, area}: its value (a contenteditable's text, at most MaxTypedValue
// characters) if it is marked, else null; whether it is a password field
// now; whether it takes text; and whether Enter makes a new line in it (a
// textarea, a contenteditable). It is null with no field focused. keep(f)
// adds f's value, if at least four characters and not in a password field
// now, to what every frame masks as text too (hold.typed; review 52: a
// page echoing what was typed showed it in frames). Nothing typed is ever
// passed into the page's own script.
var personField = worldsJS + fmt.Sprintf(` const probe = ((mark) => { if (!document.hasFocus()) return null; let a = document.activeElement; while (a && a.shadowRoot && a.shadowRoot.activeElement) a = a.shadowRoot.activeElement; if (!a || a === document.body || a === document.documentElement || a.tagName === "IFRAME" || a.tagName === "FRAME") return null; if (mark) a.setAttribute("data-heedle-typed", ""); const t = a.tagName === "INPUT" ? (a.getAttribute("type") || "text").toLowerCase() : ""; const area = a.tagName === "TEXTAREA" || a.isContentEditable; const text = area || (a.tagName === "INPUT" && !["button", "submit", "reset", "checkbox", "radio", "file", "image", "color", "range", "hidden"].includes(t)); const raw = a.isContentEditable ? a.textContent : a.value; return { v: a.hasAttribute("data-heedle-typed") && typeof raw === "string" ? raw.slice(0, %[1]d) : null, pw: t === "password", text, area }; }).toString(); const field = async (mark) => (await inWorlds(page, probe, mark, false)).values.find((v) => v) || null; const keep = (f) => { const h = page.context().__heedleHold; if (h && f && !f.pw && typeof f.v === "string" && f.v.length >= 4 && !h.typed.includes(f.v) && h.typed.length < 200) h.typed.push(f.v); };`, MaxTypedValue)

// MaxTypedValue is the most of a field's value read as typed; a longer
// value is hidden by its first MaxTypedValue characters.
const MaxTypedValue = 4096

// ClickJS is a person's click at (x, y) on the page, in CSS pixels of the
// viewport, as DevTools input events (Playwright's mouse) (roadmap 9.10).
// A click may send, so it opens the way for PersonWindow; the field it
// leaves is kept to mask.
func ClickJS(x, y int) (string, error) {
	if x < 0 || y < 0 || x >= Width || y >= Height {
		return "", fmt.Errorf("%w: a click at %d,%d is off the page", ErrInvalid, x, y)
	}
	return fmt.Sprintf("async (page) => { %s %s keep(await field(false)); await page.mouse.click(%d, %d); return \"clicked\"; }", personInput, personField, x, y), nil
}

// KeyJS is a person's key press (IsKey) as DevTools input events
// (Playwright's keyboard): a named key is pressed, a character typed. A
// key that edits (a character, Backspace, Delete) marks the field it goes
// to, before and after the key (a code box that moves on by itself stays
// marked), so every later frame masks it; another key marks nothing, so
// tabbing through fields the agent filled hides none of them. It answers
// {before, after}: the focused field's value before and after the key if
// it is one typed into, so pirn hides what a field holds however it was
// typed (review 52: a typo fixed with an arrow key split what was typed).
// Only a key that may send opens the way for PersonWindow (review 52:
// every keystroke opened it, so a page sending on each key passed as
// you): Enter, except where it makes a new line (a textarea, a
// contenteditable), and Space where it doesn't type (a button, a
// checkbox), as the focused element decides.
func KeyJS(key string) (string, error) {
	if !IsKey(key) {
		return "", fmt.Errorf("%w: not a key a person may press here", ErrInvalid)
	}
	k := jsonText(key) // a string literal: nothing in key can be code
	do := "await page.keyboard.type(" + k + ");"
	if keys[key] {
		do = "await page.keyboard.press(" + k + ");"
	}
	open := ""
	switch key {
	case "Enter":
		open = "if (!(before && before.area)) { " + personInput + " }"
	case " ":
		open = "if (!(before && before.text)) { " + personInput + " }"
	}
	// A key that edits marks the field; Space only one that takes text (on
	// a button it presses it; review 52).
	mark, leave := "false", ""
	switch {
	case key == " ":
		mark = "!!(before && before.text)"
	case !keys[key] || key == "Backspace" || key == "Delete":
		mark = "true"
	}
	if key == "Tab" || key == "Shift+Tab" || key == "Enter" || key == "Escape" {
		leave = "keep(before);"
	}
	return "async (page) => { if (!page.context().__heedleHold) throw new Error(\"no hold\"); " + personField + " let before = await field(false); const mark = " + mark + "; if (mark) before = await field(true); " + open + " " + leave + " " + do +
		" const after = await field(mark); return { before: before && before.v, after: after && after.v }; }", nil
}

// ShotJSFor's script, run by Playwright MCP's browser_run_code_unsafe,
// answers {image, rects}: a JPEG screenshot of the page (as base64), with
// password fields and every field a person typed into while driving (KeyJS
// marks them) masked by Playwright's own screenshot mask, and the boxes of
// every field holding one of the person's details the step may use or
// what they typed (hold.typed), and of every smallest element whose text
// shows one, in every frame, for ShotOf to paint over (roadmap 9.8,
// 9.10). A value is matched by its letters and digits, so one the page
// reformats, spaces out or uppercases is masked too. The matching runs in
// an isolated world of each frame (worldsJS), never the page's, and writes
// nothing to the page: no attribute tells the page which of its elements
// hold a value (review 52: a page could plant candidates and read which
// were marked). The boxes are read before and after the screenshot and
// both are painted; if any frame's document changed in between, there is
// no frame at all, never an unmasked one. A page can deny frames so (a
// document reloading without end, or text too large to match), never get
// an unmasked one. pirn takes every act frame with it.
const shotJS = `async (page) => {
  ` + worldsJS + `
  const hold = page.context().__heedleHold || {};
  const details = hold.profile || [];
  if (details.length !== WANT) throw new Error("the details to mask aren't loaded");
  const norm = (s) => (s || "").toLowerCase().replace(/[^\p{L}\p{N}]/gu, "");
  const ns = details.concat((hold.typed || []).filter((v) => v.length >= 4)).map(norm).filter((v) => v.length >= 3);
  const mask = [];
  for (const frame of page.frames()) mask.push(frame.locator('input[type="password" i], [data-heedle-typed]'));
  // The fields holding a value, and each smallest element whose text
  // shows one, in a frame's document and its open shadow roots; at most
  // 20 million characters of text looked at.
  const marks = ((ns) => {
    const norm = (s) => (s || "").toLowerCase().replace(/[^\p{L}\p{N}]/gu, "");
    let budget = 20e6;
    const has = (s) => { s = s || ""; budget -= s.length; if (budget < 0) throw new Error("too much text to mask"); const v = norm(s); return v !== "" && ns.some((n) => v.includes(n)); };
    const out = [], roots = [document];
    while (roots.length) {
      const root = roots.pop();
      const els = [...root.querySelectorAll("*")];
      const shows = new Set(els.filter((el) => !["SCRIPT", "STYLE", "TEMPLATE"].includes(el.tagName) && has(el.textContent)));
      for (const el of els) {
        if (el.shadowRoot) roots.push(el.shadowRoot);
        const field = el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.tagName === "SELECT" || el.isContentEditable;
        if (field && has("value" in el ? el.value : el.textContent)) out.push(el);
        else if (shows.has(el) && ![...el.children].some((c) => shows.has(c))) out.push(el);
      }
    }
    return out;
  }).toString();
  const before = ns.length ? await inWorlds(page, marks, ns, true) : null;
  const b = await page.screenshot({ type: "jpeg", quality: 90, scale: "css", mask, maskColor: "#5b5f6b", animations: "disabled", timeout: 10000 });
  const after = ns.length ? await inWorlds(page, marks, ns, true) : null;
  if (before && before.docs.slice().sort().join(" ") !== after.docs.slice().sort().join(" ")) throw new Error("the page changed while its frame was taken");
  return { image: b.toString("base64"), rects: before ? before.rects.concat(after.rects) : [] };
}`

// ShotJSFor is ShotJS for a session given n details: it fails, rather
// than taking an unmasked frame, unless the browser has exactly n to
// mask (review 42).
func ShotJSFor(n int) string { return strings.Replace(shotJS, "WANT", strconv.Itoa(n), 1) }

// UseProfile gives the session the person's details a step may use
// (roadmap 9.8), each under its placeholder (such as "⟨email⟩"), before
// Playwright MCP starts: Playwright MCP's own secrets, which type the
// value where the model types the placeholder and show the placeholder
// wherever the value would appear in what it answers; and the values,
// for ShotJS to mask. They go to the container on standard input, never
// on a command line, and stay in it.
func (s *Session) UseProfile(ctx context.Context, secrets map[string]string) error {
	cfg := PlaywrightConfig(s.allowed, s.resolve)
	cfg["secrets"] = secrets
	vals := make([]string, 0, len(secrets))
	for _, v := range secrets {
		vals = append(vals, v)
	}
	sort.Strings(vals)
	c, p := jsonText(cfg), jsonText(vals) // values exactly as given: no \u0026 for "&" (review 42)
	files, _ := json.Marshal(map[string]string{ActConfig: c, ActProfile: p})
	put := exec.CommandContext(ctx, s.podman, "exec", "-i", s.name, ActNode, "-e",
		`const fs = require("fs"), d = process.argv[1]; fs.chmodSync(d, 0o755); `+
			`for (const [f, b] of Object.entries(JSON.parse(fs.readFileSync(0)))) { try { fs.chmodSync(f, 0o644); } catch {} fs.writeFileSync(f, b, { mode: 0o444 }); fs.chmodSync(f, 0o444); } fs.chmodSync(d, 0o555);`,
		ActHoldDir)
	put.Env, put.Stdin = cleanEnv(), bytes.NewReader(files)
	if out, err := put.CombinedOutput(); err != nil {
		return fmt.Errorf("reed: %v: %s", err, bytes.TrimSpace(out)) // the values went on stdin: the output never has them
	}
	return nil
}

// SendWithoutAsking lifts the hold for the whole session, for a step its
// program let send without asking: call it before Playwright MCP starts.
func (s *Session) SendWithoutAsking(ctx context.Context) error {
	if out, err := podmanCmd(ctx, s.podman, "exec", s.name, ActNode, "-e",
		`const fs = require("fs"), d = require("path").dirname(process.argv[1]); fs.chmodSync(d, 0o755); fs.writeFileSync(process.argv[1], "", { mode: 0o444 }); fs.chmodSync(d, 0o555);`,
		ActSendFreely); err != nil {
		return fmt.Errorf("reed: %v: %s", err, out)
	}
	return nil
}

// Allowed is the session's allowlist, sorted.
func (s *Session) Allowed() []string { return append([]string(nil), s.allowed...) }

// Close removes the container, whatever state it's in. It never leaves a
// browser running, even if the caller's context is done.
func (s *Session) Close() {
	s.once.Do(func() {
		s.stop()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = podmanCmd(ctx, s.podman, "rm", "-f", "-t", "0", s.name)
	})
}

// FrameOf makes a frame of a screenshot (PNG or JPEG) taken at url: at
// most FrameWidth wide, as a JPEG at FrameQuality, like browse's live
// frames. It refuses anything that isn't such an image, or is larger
// than MaxScreenshot or MaxScreenshotPixels.
func FrameOf(url string, shot []byte) (Frame, error) {
	if len(shot) > MaxScreenshot {
		return Frame{}, fmt.Errorf("%w: a screenshot of %d bytes", ErrInvalid, len(shot))
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(shot))
	if err != nil {
		return Frame{}, fmt.Errorf("%w: a screenshot that isn't a PNG or JPEG: %v", ErrInvalid, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > MaxScreenshotPixels {
		return Frame{}, fmt.Errorf("%w: a screenshot of %dx%d", ErrInvalid, cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(shot))
	if err != nil {
		return Frame{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	w, h := cfg.Width, cfg.Height
	if w > FrameWidth {
		w, h = FrameWidth, max(1, h*FrameWidth/cfg.Width)
	}
	small := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(small, small.Bounds(), img, img.Bounds(), draw.Src, nil)
	var out bytes.Buffer
	if err := stdjpeg.Encode(&out, small, &stdjpeg.Options{Quality: FrameQuality}); err != nil {
		return Frame{}, err
	}
	return Frame{URL: cut(url, MaxURL), JPEG: out.Bytes()}, nil
}

// MaxMaskRects bounds the boxes ShotOf paints over.
const MaxMaskRects = 20000

// maskColor is the colour masks are painted, Playwright's mask's.
var maskColor = image.NewUniform(color.RGBA{0x5b, 0x5f, 0x6b, 0xff})

// ShotOf is the screenshot a ShotJSFor call answered, as Playwright MCP's
// browser_run_code_unsafe gives it (its "### Result" section), with every
// box it names painted over, a little larger than the box; an answer that
// isn't one, or names too many boxes, is an error, never an unmasked
// image.
func ShotOf(answer string) ([]byte, error) {
	i := strings.Index(answer, "### Result\n")
	var a struct {
		Image string       `json:"image"`
		Rects [][4]float64 `json:"rects"`
	}
	if i < 0 || json.NewDecoder(strings.NewReader(answer[i+len("### Result\n"):])).Decode(&a) != nil || a.Image == "" || a.Rects == nil && !strings.Contains(answer[i:], `"rects"`) {
		return nil, fmt.Errorf("%w: not a screenshot", ErrInvalid)
	}
	if len(a.Rects) > MaxMaskRects {
		return nil, fmt.Errorf("%w: %d boxes to mask", ErrInvalid, len(a.Rects))
	}
	shot, err := stdbase64.StdEncoding.DecodeString(a.Image)
	if err != nil || len(shot) > MaxScreenshot {
		return nil, fmt.Errorf("%w: not a screenshot", ErrInvalid)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(shot))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > MaxScreenshotPixels {
		return nil, fmt.Errorf("%w: not a screenshot", ErrInvalid)
	}
	src, _, err := image.Decode(bytes.NewReader(shot))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	img := image.NewRGBA(src.Bounds())
	draw.Draw(img, img.Bounds(), src, src.Bounds().Min, draw.Src)
	for _, r := range a.Rects {
		box := image.Rect(int(r[0])-3, int(r[1])-3, int(r[0]+r[2])+4, int(r[1]+r[3])+4).Intersect(img.Bounds())
		draw.Draw(img, box, maskColor, image.Point{}, draw.Src)
	}
	var out bytes.Buffer
	if err := stdjpeg.Encode(&out, img, &stdjpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// jsonText is v as JSON, without HTML escaping.
func jsonText(v any) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}
