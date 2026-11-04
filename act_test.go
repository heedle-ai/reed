package reed_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/heedle-ai/reed"
)

// TestInv_ActCommandLines: an act session's container has browse's
// options and label of its own, its Node idle; Playwright MCP runs with
// exactly the flags the design names (headless, a fresh in-memory
// profile, the image's Chromium, the allowlist as a second check, no
// page-registered tools, its files in /tmp); its Chromium starts with
// browse's very allowlist switches and no DevTools port or profile; and
// downloads are refused. (Roadmap 9.5.)
func TestInv_ActCommandLines(t *testing.T) {
	b := reed.Browser{Image: img, Network: "net1"}
	if got, want := strings.Join(b.ActArgv("reed-act-x"), " "), "run -d --rm --name reed-act-x --label reed.act --pull=never --cap-drop=all --security-opt=no-new-privileges --pids-limit=512 --memory=1g --http-proxy=false --log-driver=none --network=net1 "+img+" -e setInterval(() => {}, 1 << 30)"; got != want {
		t.Errorf("container:\n got  %s\n want %s", got, want)
	}
	allowed := []string{"cdn.example.org", "forms.example.org"}
	if got := reed.Origins(allowed); got != "cdn.example.org;http://cdn.example.org:*;https://cdn.example.org:*;forms.example.org;http://forms.example.org:*;https://forms.example.org:*" {
		t.Errorf("origins: %s", got)
	}
	launch := reed.LaunchArgs(allowed, map[string]string{"cdn.example.org": "203.0.113.7"})
	browse := reed.ChromiumFlags(allowed, map[string]string{"cdn.example.org": "203.0.113.7"})
	for _, f := range browse {
		shared := strings.HasPrefix(f, "--proxy") || strings.HasPrefix(f, "--host-resolver-rules=") || strings.HasPrefix(f, "--force-webrtc")
		if in := contains(launch, f); shared && !in {
			t.Errorf("act's browser lacks browse's %s", f)
		}
	}
	for _, f := range launch {
		if strings.HasPrefix(f, "--remote-debugging") || strings.HasPrefix(f, "--user-data-dir") || f == "--headless" || !strings.HasPrefix(f, "--") {
			t.Errorf("act's launch arguments carry %s", f)
		}
	}
	cfg, _ := json.Marshal(reed.PlaywrightConfig(allowed, nil))
	var c struct {
		Browser struct {
			LaunchOptions  struct{ Args []string }
			ContextOptions map[string]any
		}
	}
	json.Unmarshal(cfg, &c)
	if c.Browser.ContextOptions["acceptDownloads"] != false || strings.Join(c.Browser.LaunchOptions.Args, " ") != strings.Join(reed.LaunchArgs(allowed, nil), " ") {
		t.Errorf("config: %s", cfg)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// TestInv_ActMCPCommandLine: Playwright MCP runs in the session with
// exactly these arguments, which TestContainer_ActServerIsPinned also
// reads back from the running process. (Roadmap 9.5.)
func TestInv_ActMCPCommandLine(t *testing.T) {
	dir := t.TempDir()
	podman := fakePodman(t, dir, "")
	s, err := reed.Browser{Podman: podman, Image: img}.Start(context.Background(), reed.Visit{URL: "https://forms.example.org/volunteer", Allow: []string{"cdn.example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	argv := s.MCPArgv()
	if len(argv) < 3 || argv[0] != "exec" || argv[1] != "-i" || !strings.HasPrefix(argv[2], "reed-act-") {
		t.Fatalf("argv: %q", argv)
	}
	want := "/node/bin/node /mcp/node_modules/@playwright/mcp/cli.js --headless --isolated --executable-path /browser/chromium --allowed-origins " +
		reed.Origins([]string{"cdn.example.org", "forms.example.org"}) + " --no-webmcp --config /tmp/act/config.json --output-dir /tmp/act --viewport-size 1280x800"
	if got := strings.Join(argv[3:], " "); got != want {
		t.Errorf("Playwright MCP:\n got  %s\n want %s", got, want)
	}
	if cmd := s.Command(context.Background()); cmd.Path != podman || strings.Join(cmd.Args[1:], " ") != strings.Join(argv, " ") {
		t.Errorf("command: %v", cmd.Args)
	}
}

// fakePodman writes a podman that logs its arguments and answers ps with
// psJSON, and returns its path.
func fakePodman(t *testing.T, dir, psJSON string) string {
	t.Helper()
	p := filepath.Join(dir, "podman")
	script := "#!/bin/sh\necho \"$@\" >> " + filepath.Join(dir, "log") + "\ncat > /dev/null\ncase \"$1\" in ps) printf '%s' '" + psJSON + "' ;; esac\n"
	if psJSON == "" {
		script = strings.Replace(script, "printf '%s' ''", "printf '[]'", 1)
	}
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestInv_ActSessionsAreSweptOnTheirOwn: Start removes act containers
// left over from a worker that died (labelled reed.act, older than
// StaleActAfter) and only those; browse's Sweep looks only at visits, so
// a session working for longer than a visit may last is never removed.
// Close removes the container. (Roadmap 9.5.)
func TestInv_ActSessionsAreSweptOnTheirOwn(t *testing.T) {
	dir := t.TempDir()
	old := `[{"Id":"stale1","Created":1}]`
	podman := fakePodman(t, dir, old)
	s, err := reed.Browser{Podman: podman, Image: img}.Start(context.Background(), reed.Visit{URL: "https://forms.example.org/"})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s.Close() // once
	log, _ := os.ReadFile(filepath.Join(dir, "log"))
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 5 || lines[0] != "ps -a --filter label=reed.act --format json" || lines[1] != "rm -f -t 0 stale1" ||
		!strings.HasPrefix(lines[2], "run -d --rm --name reed-act-") || !strings.HasPrefix(lines[3], "exec -i reed-act-") ||
		!strings.HasPrefix(lines[4], "rm -f -t 0 reed-act-") {
		t.Errorf("podman calls:\n%s", log)
	}
	if !strings.Contains(lines[3], "/node/bin/node -e ") || !strings.HasSuffix(lines[3], "/tmp/act /tmp/act/config.json") {
		t.Errorf("the configuration is written as: %s", lines[3])
	}
}

// TestInv_StartChecksAsOpenDoes: a session's visit, image and resolve
// map are checked as a visit's are, before anything runs. (Roadmap 9.5.)
func TestInv_StartChecksAsOpenDoes(t *testing.T) {
	dir := t.TempDir()
	podman := fakePodman(t, dir, "")
	for _, c := range []struct {
		b    reed.Browser
		v    reed.Visit
		want error
	}{
		{reed.Browser{Podman: podman}, reed.Visit{URL: "https://a.example/"}, reed.ErrSetup},
		{reed.Browser{Podman: podman, Image: "reed-browser-agent:latest"}, reed.Visit{URL: "https://a.example/"}, reed.ErrInvalid},
		{reed.Browser{Podman: podman, Image: img}, reed.Visit{URL: "file:///etc/passwd"}, reed.ErrInvalid},
		{reed.Browser{Podman: podman, Image: img}, reed.Visit{URL: "http://127.0.0.1/"}, reed.ErrInvalid},
		{reed.Browser{Podman: podman, Image: img}, reed.Visit{URL: "https://a.example/", Allow: []string{"*.example"}}, reed.ErrInvalid},
		{reed.Browser{Podman: podman, Image: img, Resolve: map[string]string{"a.example": "169.254.169.254"}}, reed.Visit{URL: "https://a.example/"}, reed.ErrInvalid},
	} {
		if _, err := c.b.Start(context.Background(), c.v); !errors.Is(err, c.want) {
			t.Errorf("%+v %+v: %v, want %v", c.b, c.v, err, c.want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "log")); len(b) != 0 {
		t.Errorf("podman ran: %s", b)
	}
}

// TestInv_FramesAreSmall: a screenshot becomes a frame like browse's
// live frames (at most FrameWidth wide, JPEG at FrameQuality), whether
// Playwright sent a PNG or a JPEG; anything else, or anything too large
// to decode cheaply, is refused. (Roadmap 9.5.)
func TestInv_FramesAreSmall(t *testing.T) {
	shot := image.NewRGBA(image.Rect(0, 0, reed.Width, reed.Height))
	var p, j bytes.Buffer
	png.Encode(&p, shot)
	jpeg.Encode(&j, shot, nil)
	for _, in := range [][]byte{p.Bytes(), j.Bytes()} {
		f, err := reed.FrameOf("https://forms.example.org/"+strings.Repeat("x", reed.MaxURL), in)
		if err != nil {
			t.Fatal(err)
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(f.JPEG))
		if err != nil || format != "jpeg" || cfg.Width != reed.FrameWidth || cfg.Height != reed.Height/2 || len(f.URL) != reed.MaxURL {
			t.Errorf("frame: %v %s %dx%d url %d", err, format, cfg.Width, cfg.Height, len(f.URL))
		}
	}
	var huge bytes.Buffer
	png.Encode(&huge, image.NewGray(image.Rect(0, 0, 5000, 5000)))
	for name, bad := range map[string][]byte{"text": []byte("<html>"), "too many pixels": huge.Bytes(), "too many bytes": make([]byte, reed.MaxScreenshot+1)} {
		if _, err := reed.FrameOf("u", bad); !errors.Is(err, reed.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestInv_BuildImageAgentMode: with BUILD_IMAGE_NODE and BUILD_IMAGE_MCP,
// build-image.sh puts Node and Playwright MCP beside the browser, with
// Node's libraries, labels both versions, starts Node, and names the
// browser /browser/chromium; it refuses a Node or MCP folder that isn't
// one. (Roadmap 9.5.)
func TestInv_BuildImageAgentMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		return // build-image.sh copies a Linux browser and its libraries
	}
	dir := t.TempDir()
	browser, bin, node, mcp := filepath.Join(dir, "browser"), filepath.Join(dir, "bin"), filepath.Join(dir, "node"), filepath.Join(dir, "mcp")
	for _, d := range []string{browser, bin, filepath.Join(node, "bin"), filepath.Join(mcp, "node_modules", "@playwright", "mcp")} {
		os.MkdirAll(d, 0o755)
	}
	src := filepath.Join(dir, "fake.go")
	os.WriteFile(src, []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(`v22.0.0`) }\n"), 0o644)
	build := exec.Command("go", "build", "-o", filepath.Join(browser, "chrome"), src)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	exe, _ := os.ReadFile(filepath.Join(browser, "chrome"))
	os.WriteFile(filepath.Join(browser, "icudtl.dat"), nil, 0o644)
	os.WriteFile(filepath.Join(browser, "resources.pak"), nil, 0o644)
	os.WriteFile(filepath.Join(bin, "socat"), exe, 0o755)
	os.WriteFile(filepath.Join(node, "bin", "node"), exe, 0o755)
	os.WriteFile(filepath.Join(mcp, "node_modules", "@playwright", "mcp", "cli.js"), nil, 0o644)
	os.WriteFile(filepath.Join(mcp, "node_modules", "@playwright", "mcp", "package.json"), []byte("{\n  \"name\": \"@playwright/mcp\",\n  \"version\": \"0.0.84\"\n}\n"), 0o644)
	env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx := filepath.Join(dir, "context")
	os.Mkdir(ctx, 0o755)
	cmd := exec.Command("bash", "scripts/build-image.sh", filepath.Join(browser, "chrome"))
	cmd.Env = append(env, "BUILD_IMAGE_CONTEXT="+ctx, "BUILD_IMAGE_NODE="+node, "BUILD_IMAGE_MCP="+mcp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent mode: %v\n%s", err, out)
	}
	cf, _ := os.ReadFile(filepath.Join(ctx, "Containerfile"))
	for _, want := range []string{`LABEL reed.node="v22.0.0"`, `LABEL reed.mcp="0.0.84"`, `ENTRYPOINT ["/node/bin/node"]`} {
		if !strings.Contains(string(cf), want) {
			t.Errorf("Containerfile lacks %s:\n%s", want, cf)
		}
	}
	for _, f := range []string{"node/bin/node", "mcp/node_modules/@playwright/mcp/cli.js", "browser/chromium"} {
		if _, err := os.Stat(filepath.Join(ctx, "rootfs", f)); err != nil {
			t.Errorf("rootfs lacks %s: %v", f, err)
		}
	}
	for _, bad := range [][2]string{{dir, mcp}, {node, dir}} {
		c2 := filepath.Join(dir, "c2")
		os.RemoveAll(c2)
		os.Mkdir(c2, 0o755)
		cmd := exec.Command("bash", "scripts/build-image.sh", filepath.Join(browser, "chrome"))
		cmd.Env = append(env, "BUILD_IMAGE_CONTEXT="+c2, "BUILD_IMAGE_NODE="+bad[0], "BUILD_IMAGE_MCP="+bad[1])
		if b, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(b), "has no") {
			t.Errorf("node %s mcp %s: %v\n%s", bad[0], bad[1], err, b)
		}
	}
}
