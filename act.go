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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	stdjpeg "image/jpeg"
	_ "image/png" // Playwright's screenshots may be PNG
	"os/exec"
	"strings"
	"sync"
	"time"

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
	// ActLabel marks act containers. They live as long as a step, much
	// longer than a visit, so browse's Sweep never touches them; Start
	// sweeps those older than StaleActAfter.
	ActLabel       = "reed.act"
	SessionTimeout = 20 * time.Minute // bounds one session, whatever the caller's deadline
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
}

// Start validates v (its URL and Allow hosts are the allowlist, as for a
// visit) and starts a fresh browser-agent container for it, idle until
// Playwright MCP runs in it (Command). b.Image is the browser-agent image.
func (b Browser) Start(ctx context.Context, v Visit) (*Session, error) {
	allowed, err := b.check(v, "no browser-agent image configured (give the worker -browser-agent-image)")
	if err != nil {
		return nil, err
	}
	podman := b.podman()
	// A session lasts minutes: sweeping each time costs one podman ps.
	_ = sweep(ctx, podman, ActLabel, StaleActAfter)
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	s := &Session{podman: podman, name: "reed-act-" + hex.EncodeToString(rnd[:]), allowed: allowed, resolve: b.Resolve}
	if out, err := podmanCmd(ctx, podman, b.ActArgv(s.name)...); err != nil {
		s.Close()
		return nil, startError(ctx, err, out)
	}
	cfg, _ := json.Marshal(PlaywrightConfig(allowed, b.Resolve))
	put := exec.CommandContext(ctx, podman, "exec", "-i", s.name, ActNode, "-e",
		`const fs = require("fs"); fs.mkdirSync(process.argv[1], { recursive: true }); fs.writeFileSync(process.argv[2], fs.readFileSync(0));`,
		ActDir, ActConfig)
	put.Env, put.Stdin = cleanEnv(), bytes.NewReader(cfg)
	if out, err := put.CombinedOutput(); err != nil {
		s.Close()
		return nil, startError(ctx, err, out)
	}
	return s, nil
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
	}, allowlistSwitches(allowed, resolve)...)
}

// PlaywrightConfig is Playwright MCP's configuration file for a session:
// Chromium's launch arguments, and downloads refused.
func PlaywrightConfig(allowed []string, resolve map[string]string) map[string]any {
	return map[string]any{"browser": map[string]any{
		"launchOptions":  map[string]any{"args": LaunchArgs(allowed, resolve)},
		"contextOptions": map[string]any{"acceptDownloads": false},
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
	cmd := exec.CommandContext(ctx, s.podman, s.MCPArgv()...)
	cmd.Env = cleanEnv()
	return cmd
}

// Allowed is the session's allowlist, sorted.
func (s *Session) Allowed() []string { return append([]string(nil), s.allowed...) }

// Close removes the container, whatever state it's in. It never leaves a
// browser running, even if the caller's context is done.
func (s *Session) Close() {
	s.once.Do(func() {
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
