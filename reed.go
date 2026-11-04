// Package reed gives a browsing agent step its own browser: a fresh
// Chromium in a fresh Podman container, driven over Chrome's DevTools
// protocol (with chromedp), allowed to reach only the hosts its program
// names.
//
// reed reimplements nothing. Podman runs and isolates the container,
// Chromium does all networking and rendering, and the allowlist is enforced
// by Chromium's own switches (a proxy bypass list and host resolver rules).
// The DevTools protocol is reached through `podman exec` and socat inside
// the container: no port is published, so nothing else can drive the
// browser.
package reed

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/gobwas/ws"
)

// Errors. ErrInvalid can never succeed; ErrSetup means the browser can't
// be started here (no image configured or present, no podman); ErrBlocked
// means the page itself left the allowlist (a redirect to another host).
// Anything else, such as a page that failed to load or a browser that was
// slow to start, may succeed on a retry in a fresh container.
var (
	ErrInvalid = errors.New("reed: invalid")
	ErrSetup   = errors.New("reed: browser setup failed")
	ErrBlocked = errors.New("reed: page left the allowlist")
)

// Limits. Every field of a Page is bounded, so a hostile page can't make
// an output too large for Temporal or the record.
const (
	MaxText       = 64 << 10 // bytes of page text kept
	MaxTitle      = 1 << 10
	MaxURL        = 4 << 10
	MaxBlocked    = 100 // blocked requests listed; BlockedTotal counts all
	MaxBlockedURL = 512
	Width, Height = 1280, 800
	MemoryLimit   = "1g"
	PidsLimit     = 512
	// VisitTimeout bounds a whole visit, whatever the caller's deadline.
	VisitTimeout = 90 * time.Second
	// StaleAfter: a browser container older than this is left over from a
	// worker that died, and the next visit removes it.
	StaleAfter = 10 * time.Minute
	// Label marks reed's containers.
	Label = "reed.visit"
)

// Browser starts browsers in containers with the podman command line.
type Browser struct {
	// Podman is the podman executable; empty means "podman" on PATH.
	Podman string
	// Image is the browser image, pinned by content: a full 64-hex image
	// ID or name@sha256:<digest>. scripts/build-image.sh makes one.
	Image string
	// Network is the Podman network the container joins; empty means
	// Podman's default.
	Network string
	// Resolve pins allowed host names to addresses inside the browser
	// (Chromium's host resolver rules), for hosts DNS can't find, such as
	// internal services.
	Resolve map[string]string
	// FrameEvery is how often Visit sends a live frame to OnFrame; zero
	// means every 2 seconds.
	FrameEvery time.Duration
}

// Visit is one page load.
type Visit struct {
	URL string
	// Allow lists further hosts the page may load from, besides URL's own
	// host: exact names, no wildcards.
	Allow []string
	// OnFrame, if set, receives a small JPEG of the page while it loads
	// and is read, every FrameEvery.
	OnFrame func(Frame)
}

// Frame is one live view of the browser.
type Frame struct {
	URL  string `json:"url"`
	JPEG []byte `json:"jpeg"`
}

// Page is what a visit saw: the record of a page that can't be replayed.
type Page struct {
	Reed       int      `json:"reed"`       // format version, 1
	URL        string   `json:"url"`        // after redirects, at most MaxURL bytes
	Title      string   `json:"title"`      // at most MaxTitle bytes
	Text       string   `json:"text"`       // document.body.innerText, at most MaxText bytes
	Screenshot string   `json:"screenshot"` // data:image/jpeg;base64,…
	Allowed    []string `json:"allowed"`    // the allowlist, sorted
	// Blocked lists requests from the page and its frames that were
	// refused because their host isn't allowed (at most MaxBlocked, each
	// cut to MaxBlockedURL bytes); BlockedTotal counts them all. Requests
	// Chromium refuses before they reach the page's network layer (popups,
	// workers, WebSockets, prefetches) are blocked too, but not listed.
	Blocked      []string `json:"blocked"`
	BlockedTotal int      `json:"blockedTotal"`
}

var (
	pinned   = regexp.MustCompile(`^(sha256:)?[0-9a-f]{64}$|^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)
	hostName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
)

// Allowlist validates a visit and returns the hosts it may reach, sorted:
// the URL's host and every Allow entry. IP addresses are allowed, except
// loopback, link-local (such as cloud metadata at 169.254.169.254) and
// unspecified ones; so is localhost.
func (v Visit) Allowlist() ([]string, error) {
	u, err := url.Parse(v.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return nil, fmt.Errorf("%w: URL %q must be http(s)://host/…, without credentials", ErrInvalid, v.URL)
	}
	set := map[string]bool{}
	for _, h := range append([]string{u.Hostname()}, v.Allow...) {
		h = strings.ToLower(h)
		if ip := net.ParseIP(h); ip != nil {
			if !publicEnough(ip) {
				return nil, fmt.Errorf("%w: host %q: loopback, link-local and unspecified addresses can't be allowed", ErrInvalid, h)
			}
		} else if !hostName.MatchString(h) || h == "localhost" || strings.HasSuffix(h, ".localhost") {
			return nil, fmt.Errorf("%w: host %q: give an exact host name or IP address", ErrInvalid, h)
		}
		set[h] = true
	}
	hosts := make([]string, 0, len(set))
	for h := range set {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts, nil
}

func publicEnough(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}

// ChromiumFlags are the switches the browser starts with for an
// allowlist. Every request goes to a proxy that doesn't exist (so it
// fails) unless its host is on the bypass list, which is the allowlist;
// and only allowed host names resolve at all (a pinned name resolves to
// its pinned address). WebRTC may not send UDP outside the (dead) proxy.
func ChromiumFlags(allowed []string, resolve map[string]string) []string {
	flags := []string{
		"--headless", "--no-sandbox", // the container is the sandbox
		"--disable-dev-shm-usage", "--no-first-run", "--no-default-browser-check",
		"--user-data-dir=/tmp/profile", "--crash-dumps-dir=/tmp",
		"--disable-background-networking", "--disable-component-update", "--disable-sync",
		"--remote-debugging-port=9222", // on the container's own loopback only
	}
	flags = append(flags, allowlistSwitches(allowed, resolve)...)
	return append(flags, "--window-size="+strconv.Itoa(Width)+","+strconv.Itoa(Height), "about:blank")
}

// allowlistSwitches are the switches that confine a Chromium to the
// allowed hosts: browse's and act's browsers both start with them.
func allowlistSwitches(allowed []string, resolve map[string]string) []string {
	var rules []string
	for _, h := range allowed {
		if ip, ok := resolve[h]; ok {
			rules = append(rules, "MAP "+h+" "+ip)
		}
	}
	rules = append(rules, "MAP * ~NOTFOUND")
	for _, h := range allowed {
		// EXCLUDE also cancels a MAP of the same name, so pinned names
		// aren't excluded: their MAP comes first and wins.
		if _, ok := resolve[h]; !ok {
			rules = append(rules, "EXCLUDE "+h)
		}
	}
	return []string{
		"--proxy-server=http://127.0.0.1:9",
		"--proxy-bypass-list=<-loopback>;" + strings.Join(allowed, ";"),
		"--host-resolver-rules=" + strings.Join(rules, ", "),
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
	}
}

// Argv is the exact podman command line for a browser container, without
// the executable. Nothing is published or mounted; the image comes after
// every flag.
func (b Browser) Argv(name string, allowed []string) []string {
	argv := append(b.runOptions(name, Label), b.Image)
	return append(argv, ChromiumFlags(allowed, b.Resolve)...)
}

// runOptions are podman run's options for a browser container labelled
// label, before the image: browse's and act's containers share them.
func (b Browser) runOptions(name, label string) []string {
	argv := []string{
		"run", "-d", "--rm", "--name", name, "--label", label,
		"--pull=never", "--cap-drop=all", "--security-opt=no-new-privileges",
		"--pids-limit=" + strconv.Itoa(PidsLimit), "--memory=" + MemoryLimit,
		"--http-proxy=false", "--log-driver=none",
	}
	if b.Network != "" {
		argv = append(argv, "--network="+b.Network)
	}
	return argv
}

// RelayArgv is how reed reaches Chromium's DevTools port: socat inside the
// browser container, its standard input and output carrying the
// connection, through `podman exec`. Only a process allowed to run podman
// can do this.
func RelayArgv(name string) []string {
	return []string{"exec", "-i", name, "/usr/bin/socat", "STDIO", "TCP:127.0.0.1:9222"}
}

// Open validates v, starts a fresh browser container and loads the page,
// returning what it saw. The container is removed afterwards, whatever
// happens.
func (b Browser) Open(ctx context.Context, v Visit) (Page, error) {
	allowed, err := b.check(v, "no browser image configured (build one with scripts/build-image.sh and give the worker its ID)")
	if err != nil {
		return Page{}, err
	}
	podman := b.podman()
	ctx, cancel := context.WithTimeout(ctx, VisitTimeout)
	defer cancel()
	sweepOnce.Do(func() { _ = Sweep(ctx, podman, StaleAfter) })

	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	name := "reed-" + hex.EncodeToString(rnd[:])
	defer func() {
		// Remove it even if ctx is done; never leave a browser running.
		rm, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = podmanCmd(rm, podman, "rm", "-f", "-t", "0", name)
	}()
	if out, err := podmanCmd(ctx, podman, b.Argv(name, allowed)...); err != nil {
		return Page{}, startError(ctx, err, out)
	}

	key := register(podman, name)
	defer unregister(key)
	wsURL, err := devtoolsURL(ctx, key)
	if err != nil {
		return Page{}, err
	}
	return drive(ctx, wsURL, v, allowed, b.frameEvery())
}

// check validates a visit and the browser's settings, returning the
// allowlist; noImage says what to do when no image is configured.
func (b Browser) check(v Visit, noImage string) ([]string, error) {
	allowed, err := v.Allowlist()
	if err != nil {
		return nil, err
	}
	if b.Image == "" {
		return nil, fmt.Errorf("%w: %s", ErrSetup, noImage)
	}
	if !pinned.MatchString(b.Image) {
		return nil, fmt.Errorf("%w: image %q must be pinned: a full 64-hex image ID or name@sha256:<digest>", ErrInvalid, b.Image)
	}
	for h, ip := range b.Resolve {
		if a := net.ParseIP(ip); a == nil || !hostName.MatchString(h) || !publicEnough(a) {
			return nil, fmt.Errorf("%w: resolve %q to %q", ErrInvalid, h, ip)
		}
	}
	return allowed, nil
}

func (b Browser) podman() string {
	if b.Podman == "" {
		return "podman"
	}
	return b.Podman
}

// startError classifies a failed podman run: the caller's deadline or
// cancellation as is; a missing image or podman as ErrSetup; anything else
// as retryable.
func startError(ctx context.Context, err error, out []byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	msg := strings.TrimSpace(string(out))
	var ee *exec.ExitError
	if !errors.As(err, &ee) || strings.Contains(msg, "image not known") || strings.Contains(msg, "no such image") {
		return fmt.Errorf("%w: podman run: %v: %s", ErrSetup, err, msg)
	}
	return fmt.Errorf("reed: podman run: %v: %s", err, msg)
}

func (b Browser) frameEvery() time.Duration {
	if b.FrameEvery > 0 {
		return b.FrameEvery
	}
	return 2 * time.Second
}

// podmanCmd runs podman with a cleaned environment, like bobbin: nothing
// that could point it at another machine or configuration.
func podmanCmd(ctx context.Context, podman string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, podman, args...)
	cmd.Env = cleanEnv()
	return cmd.CombinedOutput()
}

func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "CONTAINER") || strings.HasPrefix(k, "PODMAN") || strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "LANG") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "LC_ALL=C")
}

var sweepOnce sync.Once

// Sweep removes reed browser containers (labelled Label) created more
// than olderThan ago: left over from a worker that died mid-visit. Visits
// are bounded by VisitTimeout, so a container that old isn't in use. The
// first visit in each process sweeps with StaleAfter.
func Sweep(ctx context.Context, podman string, olderThan time.Duration) error {
	return sweep(ctx, podman, Label, olderThan)
}

// sweep removes the containers labelled label created more than olderThan
// ago.
func sweep(ctx context.Context, podman, label string, olderThan time.Duration) error {
	out, err := podmanCmd(ctx, podman, "ps", "-a", "--filter", "label="+label, "--format", "json")
	if err != nil {
		return err
	}
	var cs []struct {
		ID      string `json:"Id"`
		Created int64  `json:"Created"`
	}
	if err := json.Unmarshal(out, &cs); err != nil {
		return err
	}
	var stale []string
	for _, c := range cs {
		if time.Since(time.Unix(c.Created, 0)) > olderThan {
			stale = append(stale, c.ID)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	_, err = podmanCmd(ctx, podman, append([]string{"rm", "-f", "-t", "0"}, stale...)...)
	return err
}

// === reaching DevTools through podman exec ================================

// Each visit registers its container under a key that looks like a
// loopback address, 127.0.0.1:<n>, because Chromium only accepts DevTools
// requests whose Host is an IP address or localhost. Dialing a registered
// key runs `podman exec … socat` instead of opening a socket; chromedp's
// WebSocket library (gobwas/ws) dials through it. Nothing listens on the
// host.
var (
	visitsMu sync.Mutex
	visits   = map[string][2]string{} // key -> podman, container
	nextKey  = 20000
	dialOnce sync.Once
)

func register(podman, name string) string {
	dialOnce.Do(func() {
		base := &net.Dialer{}
		ws.DefaultDialer.NetDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if c, ok, err := dialVisit(addr); ok {
				return c, err
			}
			return base.DialContext(ctx, network, addr)
		}
	})
	visitsMu.Lock()
	defer visitsMu.Unlock()
	nextKey++
	if nextKey > 60000 {
		nextKey = 20001
	}
	key := "127.0.0.1:" + strconv.Itoa(nextKey)
	visits[key] = [2]string{podman, name}
	return key
}

func unregister(key string) {
	visitsMu.Lock()
	delete(visits, key)
	visitsMu.Unlock()
}

// dialVisit connects to a registered visit's DevTools port; ok is false
// for any other address.
func dialVisit(addr string) (net.Conn, bool, error) {
	visitsMu.Lock()
	v, ok := visits[addr]
	visitsMu.Unlock()
	if !ok {
		return nil, false, nil
	}
	cmd := exec.Command(v[0], RelayArgv(v[1])...)
	cmd.Env = cleanEnv()
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, true, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, true, err
	}
	if err := cmd.Start(); err != nil {
		return nil, true, fmt.Errorf("%w: podman exec: %v", ErrSetup, err)
	}
	return &execConn{in: in, out: out, cmd: cmd}, true, nil
}

// execConn is a net.Conn over a process's standard input and output.
type execConn struct {
	in   io.WriteCloser
	out  io.ReadCloser
	cmd  *exec.Cmd
	once sync.Once
}

func (c *execConn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *execConn) Write(p []byte) (int, error) { return c.in.Write(p) }
func (c *execConn) Close() error {
	c.once.Do(func() {
		c.in.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		_ = c.cmd.Wait()
	})
	return nil
}
func (c *execConn) LocalAddr() net.Addr              { return pipeAddr{} }
func (c *execConn) RemoteAddr() net.Addr             { return pipeAddr{} }
func (c *execConn) SetDeadline(time.Time) error      { return nil }
func (c *execConn) SetReadDeadline(time.Time) error  { return nil }
func (c *execConn) SetWriteDeadline(time.Time) error { return nil }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "podman-exec" }
func (pipeAddr) String() string  { return "podman-exec" }

// devtoolsURL waits for the browser's DevTools endpoint and returns its
// WebSocket URL, addressed to the visit's key.
func devtoolsURL(ctx context.Context, key string) (string, error) {
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			c, ok, err := dialVisit(addr)
			if !ok {
				return nil, errors.New("reed: not a visit")
			}
			return c, err
		},
		DisableKeepAlives: true,
	}}
	deadline := time.Now().Add(30 * time.Second)
	var last error
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+key+"/json/version", nil)
		res, err := client.Do(req)
		if err == nil {
			var v struct {
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			err = json.NewDecoder(res.Body).Decode(&v)
			res.Body.Close()
			if err == nil && v.WebSocketDebuggerURL != "" {
				u, err := url.Parse(v.WebSocketDebuggerURL)
				if err != nil {
					return "", err
				}
				u.Host = key
				return u.String(), nil
			}
		}
		if errors.Is(err, ErrSetup) {
			return "", err
		}
		last = err
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("reed: browser didn't start in 30s: %v", last) // retryable
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// === driving the page =====================================================

// drive loads the page over the DevTools protocol and reads it.
func drive(ctx context.Context, wsURL string, v Visit, allowed []string, frameEvery time.Duration) (Page, error) {
	actx, cancelA := chromedp.NewRemoteAllocator(ctx, wsURL, chromedp.NoModifyURL)
	defer cancelA()
	bctx, cancelB := chromedp.NewContext(actx)
	defer cancelB()

	ok := map[string]bool{}
	for _, h := range allowed {
		ok[h] = true
	}
	var mu sync.Mutex
	blocked := map[string]bool{}
	total := 0
	limit := make(chan struct{}, 16) // at most 16 decisions in flight
	// Second check, inside the page: requests from the page and its frames
	// pause here; only allowed hosts continue. It also records what was
	// refused. The switches are the boundary; this doesn't see everything.
	chromedp.ListenTarget(bctx, func(ev any) {
		e, isPause := ev.(*fetch.EventRequestPaused)
		if !isPause {
			return
		}
		go func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			c := cdp.WithExecutor(bctx, chromedp.FromContext(bctx).Target)
			u, err := url.Parse(e.Request.URL)
			if err == nil && (u.Scheme == "data" || u.Scheme == "blob" || ok[strings.ToLower(u.Hostname())]) {
				_ = fetch.ContinueRequest(e.RequestID).Do(c)
				return
			}
			mu.Lock()
			total++
			if len(blocked) < MaxBlocked {
				blocked[cut(e.Request.URL, MaxBlockedURL)] = true
			}
			mu.Unlock()
			_ = fetch.FailRequest(e.RequestID, network.ErrorReasonBlockedByClient).Do(c)
		}()
	})
	if err := chromedp.Run(bctx,
		chromedp.EmulateViewport(Width, Height),
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*"}}),
	); err != nil {
		if ctx.Err() != nil {
			return Page{}, ctx.Err()
		}
		return Page{}, fmt.Errorf("reed: DevTools: %w", err)
	}

	stop := make(chan struct{})
	var frames sync.WaitGroup
	if v.OnFrame != nil {
		frames.Add(1)
		go func() {
			defer frames.Done()
			t := time.NewTicker(frameEvery)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					var shot []byte
					var loc string
					if chromedp.Run(bctx, chromedp.Evaluate(fmt.Sprintf(`location.href.slice(0, %d)`, MaxURL), &loc), jpeg(&shot, 40, 0.5)) == nil {
						v.OnFrame(Frame{URL: cut(loc, MaxURL), JPEG: shot})
					}
				}
			}
		}()
	}

	var read struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	var shot []byte
	err := chromedp.Run(bctx,
		chromedp.Navigate(v.URL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Evaluate(fmt.Sprintf(`({url: location.href.slice(0, %d), title: document.title.slice(0, %d), text: (document.body ? document.body.innerText : "").slice(0, %d)})`,
			MaxURL, MaxTitle, MaxText), &read),
		jpeg(&shot, 70, 1),
	)
	close(stop)
	frames.Wait()
	if err != nil {
		if ctx.Err() != nil {
			return Page{}, ctx.Err()
		}
		if strings.Contains(err.Error(), "ERR_BLOCKED_BY_CLIENT") {
			return Page{}, fmt.Errorf("%w: loading %s was refused: it, or a redirect from it, goes to a host that isn't allowed", ErrBlocked, v.URL)
		}
		return Page{}, fmt.Errorf("reed: loading %s: %w", v.URL, err)
	}
	p := Page{Reed: 1, URL: cut(read.URL, MaxURL), Title: cut(read.Title, MaxTitle), Text: cut(read.Text, MaxText),
		Screenshot: "data:image/jpeg;base64," + base64(shot), Allowed: allowed, Blocked: []string{}}
	mu.Lock()
	for u := range blocked {
		p.Blocked = append(p.Blocked, u)
	}
	p.BlockedTotal = total
	mu.Unlock()
	sort.Strings(p.Blocked)
	if v.OnFrame != nil {
		v.OnFrame(Frame{URL: p.URL, JPEG: shot})
	}
	return p, nil
}

// cut shortens s to at most n bytes, at a UTF-8 boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// jpeg captures the viewport as a JPEG at quality q, scaled by scale.
func jpeg(out *[]byte, q int64, scale float64) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		*out, err = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatJpeg).WithQuality(q).
			WithClip(&page.Viewport{X: 0, Y: 0, Width: Width, Height: Height, Scale: scale}).Do(ctx)
		return err
	})
}
