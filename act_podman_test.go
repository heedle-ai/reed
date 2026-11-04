package reed_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heedle-ai/reed"
)

var (
	actOnce  sync.Once
	actImage string
	actErr   error
)

// testActBrowser is the browser-agent image: REED_ACT_IMAGE, or (with
// REED_REQUIRE_PODMAN) one built from this machine's Chromium with
// BUILD_IMAGE_NODE and BUILD_IMAGE_MCP, which CI sets.
func testActBrowser(t *testing.T) string {
	t.Helper()
	needPodman(t)
	if os.Getenv("REED_ACT_IMAGE") == "" && os.Getenv("REED_REQUIRE_PODMAN") == "" {
		t.Skip("no browser-agent image: set REED_ACT_IMAGE, or REED_REQUIRE_PODMAN=1 with BUILD_IMAGE_NODE and BUILD_IMAGE_MCP to build one")
	}
	actOnce.Do(func() {
		if img := os.Getenv("REED_ACT_IMAGE"); img != "" {
			actImage = img
			return
		}
		var stderr strings.Builder
		cmd := exec.Command("bash", "scripts/build-image.sh")
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		actImage, actErr = strings.TrimSpace(string(out)), err
		if err != nil {
			actErr = fmt.Errorf("%v: %s", err, stderr.String())
		}
	})
	if actErr != nil {
		t.Fatalf("build-image.sh: %v", actErr)
	}
	return actImage
}

// mcpSession starts Playwright MCP in s and speaks MCP to it.
func mcpSession(t *testing.T, s *reed.Session) *mcp.ClientSession {
	t.Helper()
	cmd := s.Command(ctx)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	c := mcp.NewClient(&mcp.Implementation{Name: "reed-test", Version: "1"}, nil)
	cs, err := c.Connect(ctx, &mcp.IOTransport{Reader: out, Writer: in}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// call calls a tool and returns its text, and whether it was an error.
func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if args == nil {
		args = map[string]any{} // never null
	}
	r, err := cs.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), r.IsError
}

// ref is a snapshot's reference for the element described by label.
func ref(t *testing.T, snapshot, label string) string {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(label) + `[^\n]*\[ref=([a-z0-9]+)\]`).FindStringSubmatch(snapshot)
	if m == nil {
		t.Fatalf("no %s in:\n%s", label, snapshot)
	}
	return m[1]
}

// TestContainer_ActOnlyAllowedHostsAreReached: in an act session, the
// page that tries every way out (by name, by IP address, by WebSocket,
// with an image and an iframe) and Playwright navigating to the other
// host by name and by address get nowhere: the other host receives no
// request at all, while the allowed page loads and works.
func TestContainer_ActOnlyAllowedHostsAreReached(t *testing.T) {
	img := testActBrowser(t)
	network, ip, hits := sites(t)
	s, err := reed.Browser{Image: img, Network: network}.Start(ctx, reed.Visit{URL: "http://allowed-srv:8080/"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cs := mcpSession(t, s)
	call(t, cs, "browser_navigate", map[string]any{"url": "http://allowed-srv:8080/?ip=" + ip})
	if text, isErr := call(t, cs, "browser_wait_for", map[string]any{"text": "done", "time": 20}); isErr {
		t.Fatalf("the allowed page didn't finish: %s", text)
	}
	snap, _ := call(t, cs, "browser_snapshot", nil)
	if !strings.Contains(snap, "same-origin-ok") {
		t.Errorf("same-origin requests should work:\n%s", snap)
	}
	for _, u := range []string{"http://blocked-srv:8080/nav-by-name", "http://" + ip + ":8080/nav-by-ip"} {
		if text, isErr := call(t, cs, "browser_navigate", map[string]any{"url": u}); !isErr {
			t.Errorf("navigating to %s worked:\n%s", u, text)
		}
	}
	time.Sleep(time.Second)
	if n := hits(); n != 0 {
		t.Fatalf("the blocked host received %d requests", n)
	}
}

// TestContainer_ActFillsAForm: Playwright MCP, driven by hand as Claude
// would drive it, fills in a two-page form and submits it; the form's
// server gets exactly what was typed and chosen, and the blocked host,
// which the form's page asks for too, gets nothing.
func TestContainer_ActFillsAForm(t *testing.T) {
	img := testActBrowser(t)
	network, _, hits := sites(t)
	s, err := reed.Browser{Image: img, Network: network}.Start(ctx, reed.Visit{URL: "http://allowed-srv:8080/volunteer"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cs := mcpSession(t, s)
	call(t, cs, "browser_navigate", map[string]any{"url": "http://allowed-srv:8080/volunteer"})
	snap, _ := call(t, cs, "browser_snapshot", nil)
	if text, isErr := call(t, cs, "browser_fill_form", map[string]any{"fields": []map[string]any{
		{"name": "Name", "type": "textbox", "target": ref(t, snap, `textbox "Name"`), "value": "Ada Lovelace"},
		{"name": "Email", "type": "textbox", "target": ref(t, snap, `textbox "Email"`), "value": "ada@example.org"},
	}}); isErr {
		t.Fatal(text)
	}
	call(t, cs, "browser_click", map[string]any{"element": "Next", "target": ref(t, snap, `button "Next"`)})
	snap, _ = call(t, cs, "browser_snapshot", nil)
	call(t, cs, "browser_select_option", map[string]any{"element": "Shift", "target": ref(t, snap, `combobox "Shift"`), "values": []string{"Saturday"}})
	call(t, cs, "browser_click", map[string]any{"element": "Submit", "target": ref(t, snap, `button "Submit"`)})
	if snap, _ = call(t, cs, "browser_snapshot", nil); !strings.Contains(snap, "signed up") {
		t.Fatalf("not submitted:\n%s", snap)
	}
	got := allowedGot(t, network)
	if got != "email=ada%40example.org&name=Ada+Lovelace\nshift=Saturday" {
		t.Errorf("the form's server got %q", got)
	}
	if n := hits(); n != 0 {
		t.Fatalf("the blocked host received %d requests", n)
	}
}

// allowedGot is what the allowed server's form received, read from inside
// its network.
func allowedGot(t *testing.T, network string) string {
	t.Helper()
	ip := podman(t, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", network+"-allowed")
	// The test can't reach the network directly: ask from a container on it.
	id := podman(t, "run", "-d", "--rm", "--network", network, "--entrypoint", reed.ActNode, testActBrowser(t), "-e", "setInterval(() => {}, 1e9)")
	defer exec.Command("podman", "rm", "-f", "-t", "0", id).Run()
	return podman(t, "exec", id, reed.ActNode, "-e", `fetch("http://`+ip+`:8080/got").then((r) => r.text()).then((t) => process.stdout.write(t))`)
}

// TestContainer_ActServerIsPinned: the session runs the pinned Playwright
// MCP exactly as MCPArgv says, with every tool act steps use (ActTools,
// by name), downloads refused in its configuration, and the image's
// browser started with act's launch switches and no DevTools port; it
// offers scripts and uploads too, which is why pirn offers Claude only
// ActTools. With REED_EXPECT_DEBIAN set, the browser is Debian's
// Chromium.
func TestContainer_ActServerIsPinned(t *testing.T) {
	img := testActBrowser(t)
	network, _, _ := sites(t)
	s, err := reed.Browser{Image: img, Network: network}.Start(ctx, reed.Visit{URL: "http://allowed-srv:8080/"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cs := mcpSession(t, s)
	names := map[string]bool{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		names[tool.Name] = true
	}
	for _, n := range reed.ActTools {
		if !names[n] {
			t.Errorf("the pinned Playwright MCP has no %s", n)
		}
	}
	if !names["browser_evaluate"] || !names["browser_file_upload"] {
		t.Errorf("expected the core tools pirn must keep from Claude; got %v", names)
	}
	if text, isErr := call(t, cs, "browser_navigate", map[string]any{"url": "http://allowed-srv:8080/same-origin"}); isErr {
		t.Fatalf("navigate: %s", text)
	}
	name := strings.Split(s.MCPArgv()[2], " ")[0]
	procs := podman(t, "exec", name, reed.ActNode, "-e", `const fs = require("fs");
for (const p of fs.readdirSync("/proc").filter((d) => /^\d+$/.test(d))) {
  try { console.log(JSON.stringify(fs.readFileSync("/proc/" + p + "/cmdline", "utf8").split("\0").filter(Boolean))); } catch {}
}`)
	var server, browser []string
	for _, line := range strings.Split(procs, "\n") {
		var argv []string
		json.Unmarshal([]byte(line), &argv)
		switch {
		case len(argv) > 1 && argv[1] == reed.ActMCP:
			server = argv
		case len(argv) > 0 && argv[0] == reed.ActBrowser && !strings.Contains(line, "--type="):
			browser = argv
		}
	}
	if browser == nil {
		t.Fatalf("no browser among:\n%s", procs)
	}
	if want := s.MCPArgv()[3:]; strings.Join(server, " ") != strings.Join(want, " ") {
		t.Errorf("Playwright MCP runs as\n %q\nwant\n %q", server, want)
	}
	joined := strings.Join(browser, " ")
	for _, a := range reed.LaunchArgs(s.Allowed(), nil) {
		if !strings.Contains(joined, a) {
			t.Errorf("the browser lacks %q:\n%s", a, joined)
		}
	}
	if strings.Contains(joined, "--remote-debugging-port") || !strings.Contains(joined, "--headless") {
		t.Errorf("the browser has a DevTools port, or isn't headless:\n%s", joined)
	}
	cfg := podman(t, "exec", name, reed.ActNode, "-e", `process.stdout.write(require("fs").readFileSync("`+reed.ActConfig+`", "utf8"))`)
	if !strings.Contains(cfg, `"acceptDownloads":false`) {
		t.Errorf("config: %s", cfg)
	}
	if os.Getenv("REED_EXPECT_DEBIAN") != "" {
		v := podman(t, "exec", name, reed.ActBrowser, "--version")
		if !strings.HasPrefix(v, "Chromium ") {
			t.Errorf("the browser is %q, not Debian's Chromium", v)
		}
	}
}

// TestContainer_ActSessionsAreTheirOwn: Close removes the container, and
// browse's sweep never removes a session that is still working.
func TestContainer_ActSessionsAreTheirOwn(t *testing.T) {
	img := testActBrowser(t)
	s, err := reed.Browser{Image: img}.Start(ctx, reed.Visit{URL: "http://example.org/"})
	if err != nil {
		t.Fatal(err)
	}
	name := strings.Split(s.MCPArgv()[2], " ")[0]
	if err := reed.Sweep(ctx, "podman", 0); err != nil {
		t.Fatal(err)
	}
	if podman(t, "ps", "-q", "--filter", "name="+name) == "" {
		t.Fatal("browse's sweep removed a working act session")
	}
	s.Close()
	if out := podman(t, "ps", "-a", "-q", "--filter", "name="+name); out != "" {
		t.Fatalf("the session's container is still there: %s", out)
	}
}
