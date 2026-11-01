package reed_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heedle-ai/reed"
)

var ctx = context.Background()

// needPodman skips without a working podman, unless REED_REQUIRE_PODMAN
// is set (CI's containers step), where it fails instead.
func needPodman(t *testing.T) {
	t.Helper()
	if err := exec.Command("podman", "info").Run(); err != nil {
		if os.Getenv("REED_REQUIRE_PODMAN") != "" {
			t.Fatalf("podman isn't usable: %v", err)
		}
		t.Skip("podman isn't usable here; set REED_REQUIRE_PODMAN=1 to make this fail")
	}
}

var (
	browserOnce  sync.Once
	browserImage string
	browserErr   error
)

// testBrowser builds the browser image from this machine's Chromium once
// (or uses REED_IMAGE).
func testBrowser(t *testing.T) string {
	t.Helper()
	needPodman(t)
	if os.Getenv("REED_IMAGE") == "" && os.Getenv("REED_REQUIRE_PODMAN") == "" {
		t.Skip("no browser image: set REED_IMAGE to an image ID, or REED_REQUIRE_PODMAN=1 to build one with scripts/build-image.sh")
	}
	browserOnce.Do(func() {
		if img := os.Getenv("REED_IMAGE"); img != "" {
			browserImage = img
			return
		}
		out, err := exec.Command("bash", "scripts/build-image.sh").Output()
		browserImage, browserErr = strings.TrimSpace(string(out)), err
	})
	if browserErr != nil {
		t.Fatalf("build-image.sh: %v", browserErr)
	}
	return browserImage
}

func podman(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("podman", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("podman %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// sites starts the allowed and blocked test servers on a fresh network
// and returns the network, the blocked server's address on it, and a
// function reporting how many requests the blocked server received.
func sites(t *testing.T) (network, blockedIP string, hits func() int) {
	t.Helper()
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "server"), "./testdata/server")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM scratch\nCOPY server /server\nENTRYPOINT [\"/server\"]\n"), 0o644)
	img := podman(t, "build", "--quiet", dir)
	network = fmt.Sprintf("reed-test-%d", time.Now().UnixNano())
	podman(t, "network", "create", network)
	t.Cleanup(func() {
		exec.Command("podman", "rm", "-f", "-t", "0", network+"-allowed", network+"-blocked").Run()
		exec.Command("podman", "network", "rm", "-f", network).Run()
	})
	podman(t, "run", "-d", "--rm", "--name", network+"-allowed", "--network", network, "--network-alias", "allowed-srv", img, "allowed")
	podman(t, "run", "-d", "--rm", "--name", network+"-blocked", "--network", network, "--network-alias", "blocked-srv", "--publish", "127.0.0.1::8080", img, "blocked")
	blockedIP = podman(t, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", network+"-blocked")
	addr := strings.Split(podman(t, "port", network+"-blocked", "8080/tcp"), "\n")[0]
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	hits = func() int {
		for i := 0; i < 50; i++ {
			res, err := client.Get("http://" + addr + "/hits")
			if err == nil {
				b, _ := io.ReadAll(res.Body)
				res.Body.Close()
				var n int
				fmt.Sscan(string(b), &n)
				return n
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("blocked server unreachable for its hit count")
		return -1
	}
	hits() // wait until it's up
	return network, blockedIP, hits
}

// TestContainer_OnlyAllowedHostsAreReached: a page on the allowed host
// that tries to reach another host by name, by IP address, by WebSocket,
// with an image and with an iframe gets nowhere: the other host receives
// no request at all. The page itself loads and reads, same-origin
// requests work, and the blocked requests are recorded.
func TestContainer_OnlyAllowedHostsAreReached(t *testing.T) {
	img := testBrowser(t)
	network, ip, hits := sites(t)
	var frames int
	p, err := reed.Browser{Image: img, Network: network, FrameEvery: 300 * time.Millisecond}.Open(ctx, reed.Visit{
		URL:     "http://allowed-srv:8080/?ip=" + ip,
		OnFrame: func(f reed.Frame) { frames++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // let any stray request arrive
	if n := hits(); n != 0 {
		t.Fatalf("the blocked host received %d requests", n)
	}
	if p.Title != "Allowed page" || !strings.Contains(p.Text, "Allowed page") || p.Reed != 1 {
		t.Errorf("page = %q %q", p.Title, p.Text)
	}
	if !strings.HasPrefix(p.Screenshot, "data:image/jpeg;base64,") || len(p.Screenshot) < 1000 {
		t.Errorf("screenshot = %.60q…", p.Screenshot)
	}
	if fmt.Sprint(p.Allowed) != "[allowed-srv]" || len(p.Blocked) == 0 || frames == 0 {
		t.Errorf("allowed %v blocked %v frames %d", p.Allowed, p.Blocked, frames)
	}
	t.Logf("page text: %q\nblocked: %v", p.Text, p.Blocked)
}

// TestContainer_EveryVisitStartsClean: cookies and storage one visit
// leaves behind are gone on the next: each visit is a new container with
// a new browser profile, and it is removed afterwards.
func TestContainer_EveryVisitStartsClean(t *testing.T) {
	img := testBrowser(t)
	network, _, _ := sites(t)
	b := reed.Browser{Image: img, Network: network}
	for i := 0; i < 2; i++ {
		p, err := b.Open(ctx, reed.Visit{URL: "http://allowed-srv:8080/state"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.Text, "cookie=none") || !strings.Contains(p.Text, "storage=none") {
			t.Fatalf("visit %d saw an earlier visit's state: %q", i+1, p.Text)
		}
	}
	if left := podman(t, "ps", "-a", "-q", "--filter", "label=reed.visit"); left != "" {
		t.Fatalf("containers left behind: %s", left)
	}
}

// TestContainer_CancelRemovesThePod: cancelling a visit midway still
// removes its container.
func TestContainer_CancelRemovesThePod(t *testing.T) {
	img := testBrowser(t)
	network, _, _ := sites(t)
	c, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(1500 * time.Millisecond); cancel() }()
	_, err := reed.Browser{Image: img, Network: network}.Open(c, reed.Visit{URL: "http://allowed-srv:8080/never-ends"})
	_ = err
	if left := podman(t, "ps", "-a", "-q", "--filter", "label=reed.visit"); left != "" {
		t.Fatalf("containers left behind: %s", left)
	}
}

// TestContainer_MissingImageIsSetup: an image that isn't there is a setup
// error, which a program doesn't retry.
func TestContainer_MissingImageIsSetup(t *testing.T) {
	needPodman(t)
	_, err := reed.Browser{Image: strings.Repeat("0", 64)}.Open(ctx, reed.Visit{URL: "http://example.invalid/"})
	if !errors.Is(err, reed.ErrSetup) {
		t.Fatalf("err = %v, want ErrSetup", err)
	}
}

// TestContainer_InAProgram: (browse …) as a program step with the real
// browser: the page comes back as the step's output, and live frames are
// heartbeated to Temporal while it runs.
func TestContainer_InAProgram(t *testing.T) {
	img := testBrowser(t)
	network, ip, hits := sites(t)
	res, err, frames := runBrowse(t, reed.Browser{Image: img, Network: network, FrameEvery: 300 * time.Millisecond},
		`(browse "http://allowed-srv:8080/?ip=`+ip+`")`)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := reed.ParsePage(res.Outputs["n0"])
	if !ok || p.Title != "Allowed page" || len(frames) == 0 || len(frames[len(frames)-1].JPEG) < 100 {
		t.Fatalf("page %v %q, %d frames", ok, p.Title, len(frames))
	}
	if n := hits(); n != 0 {
		t.Fatalf("the blocked host received %d requests", n)
	}
}

// TestContainer_DevToolsIsNotExposed: while a visit runs, the browser's
// container publishes no port, and its DevTools port isn't reachable on
// the container's address: only podman exec reaches it. (Review 5: a
// published relay let any local user or container drive the browser.)
func TestContainer_DevToolsIsNotExposed(t *testing.T) {
	img := testBrowser(t)
	network, _, _ := sites(t)
	checked := false
	_, err := reed.Browser{Image: img, Network: network, FrameEvery: 300 * time.Millisecond}.Open(ctx, reed.Visit{
		URL: "http://allowed-srv:8080/",
		OnFrame: func(reed.Frame) {
			if checked {
				return
			}
			checked = true
			ids := strings.Fields(podman(t, "ps", "-q", "--filter", "label=reed.visit"))
			if len(ids) == 0 {
				t.Error("no running browser to check")
				return
			}
			for _, id := range ids {
				if ports := podman(t, "inspect", "--format", "{{json .NetworkSettings.Ports}}", id); ports != "{}" && ports != "null" && ports != "map[]" {
					t.Errorf("container %s publishes %s", id, ports)
				}
				ip := podman(t, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", id)
				for _, port := range []string{"9222", "9223"} {
					if c, err := net.DialTimeout("tcp", ip+":"+port, time.Second); err == nil {
						c.Close()
						t.Errorf("DevTools reachable at %s:%s", ip, port)
					}
				}
			}
		},
	})
	if err != nil || !checked {
		t.Fatalf("visit: %v (checked %v)", err, checked)
	}
}

// TestContainer_ResolvePinsHosts: an allowed name pinned with Resolve
// resolves to its address. (Review 5: EXCLUDE cancelled the pin.)
func TestContainer_ResolvePinsHosts(t *testing.T) {
	img := testBrowser(t)
	network, _, _ := sites(t)
	ip := podman(t, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", network+"-allowed")
	p, err := reed.Browser{Image: img, Network: network, Resolve: map[string]string{"pinned.example": ip}}.Open(ctx, reed.Visit{URL: "http://pinned.example:8080/"})
	if err != nil || p.Title != "Allowed page" {
		t.Fatalf("visit: %v, title %q", err, p.Title)
	}
}

// TestContainer_OutputsAreBounded: a 5 MB title, a 1.5 MB URL and 3000
// blocked requests each still give a small page record; a page doing all
// three at once is stopped at VisitTimeout rather than hanging, and its
// browser is removed. (Review 5: the output was 6.5 MB and such a page
// kept a visit running for more than 8 minutes.)
func TestContainer_OutputsAreBounded(t *testing.T) {
	img := testBrowser(t)
	network, _, hits := sites(t)
	b := reed.Browser{Image: img, Network: network}
	for _, path := range []string{"big-title", "big-url", "many"} {
		p, err := b.Open(ctx, reed.Visit{URL: "http://allowed-srv:8080/" + path})
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out, _ := json.Marshal(p)
		if len(out) > 1<<20 || len(p.Title) > reed.MaxTitle || len(p.URL) > reed.MaxURL || len(p.Blocked) > reed.MaxBlocked {
			t.Fatalf("%s: output %d bytes, title %d, url %d, blocked %d", path, len(out), len(p.Title), len(p.URL), len(p.Blocked))
		}
		for _, u := range p.Blocked {
			if len(u) > reed.MaxBlockedURL {
				t.Fatalf("%s: blocked entry of %d bytes", path, len(u))
			}
		}
		if path == "many" && (p.BlockedTotal < len(p.Blocked) || len(p.Blocked) != reed.MaxBlocked) {
			t.Fatalf("many: blocked %d of %d", len(p.Blocked), p.BlockedTotal)
		}
	}
	start := time.Now()
	_, err := b.Open(ctx, reed.Visit{URL: "http://allowed-srv:8080/big"})
	if took := time.Since(start); took > reed.VisitTimeout+20*time.Second {
		t.Fatalf("the hostile page took %v (err %v)", took, err)
	}
	if left := podman(t, "ps", "-a", "-q", "--filter", "label=reed.visit"); left != "" {
		t.Fatalf("containers left behind: %s", left)
	}
	if n := hits(); n != 0 {
		t.Fatalf("the blocked host received %d requests", n)
	}
}

// TestContainer_RedirectOffTheListIsBlocked: a page that redirects to a
// host that isn't allowed fails with ErrBlocked, which isn't retried,
// and the other host gets nothing. (Review 5: it was retryable.)
func TestContainer_RedirectOffTheListIsBlocked(t *testing.T) {
	img := testBrowser(t)
	network, _, hits := sites(t)
	_, err := reed.Browser{Image: img, Network: network}.Open(ctx, reed.Visit{URL: "http://allowed-srv:8080/redir"})
	if !errors.Is(err, reed.ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if n := hits(); n != 0 {
		t.Fatalf("the blocked host received %d requests", n)
	}
}

// TestContainer_CancelledSetupIsNotASetupError: a visit cancelled while the
// browser starts reports the cancellation, not ErrSetup (which would stop
// retries). (Review 5.)
func TestContainer_CancelledSetupIsNotASetupError(t *testing.T) {
	img := testBrowser(t)
	for _, d := range []time.Duration{50 * time.Millisecond, 400 * time.Millisecond, 1500 * time.Millisecond} {
		c, cancel := context.WithTimeout(ctx, d)
		_, err := reed.Browser{Image: img}.Open(c, reed.Visit{URL: "http://allowed-srv.invalid/"})
		cancel()
		if err == nil || errors.Is(err, reed.ErrSetup) {
			t.Errorf("after %v: err = %v, want the context's error", d, err)
		}
	}
	if left := podman(t, "ps", "-a", "-q", "--filter", "label=reed.visit"); left != "" {
		t.Fatalf("containers left behind: %s", left)
	}
}
