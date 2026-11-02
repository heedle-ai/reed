package reed_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/heedle-ai/draft"
	"github.com/heedle-ai/reed"
	"github.com/heedle-ai/shuttle/plan"
)

var img = strings.Repeat("ab", 32)

// TestInv_ExactPodmanCommandLines: one hardened container, nothing
// published or mounted, the image after every flag (so nothing a program
// says can become a podman flag); DevTools is reached only through podman
// exec. (Review 5: the published DevTools port could be driven by any
// local user or container.)
func TestInv_ExactPodmanCommandLines(t *testing.T) {
	b := reed.Browser{Image: img, Network: "net1"}
	argv := strings.Join(b.Argv("reed-x", []string{"a.example"}), " ")
	want := "run -d --rm --name reed-x --label reed.visit --pull=never --cap-drop=all --security-opt=no-new-privileges --pids-limit=512 --memory=1g --http-proxy=false --log-driver=none --network=net1 " + img + " --headless --no-sandbox"
	if !strings.HasPrefix(argv, want) {
		t.Errorf("browser:\n got  %s\n want %s…", argv, want)
	}
	if strings.Contains(argv, "--publish") || strings.Contains(argv, "-p ") {
		t.Error("nothing may be published")
	}
	if got := strings.Join(reed.RelayArgv("reed-x"), " "); got != "exec -i reed-x /usr/bin/socat STDIO TCP:127.0.0.1:9222" {
		t.Errorf("relay: %s", got)
	}
	if argv := (reed.Browser{Image: img}).Argv("reed-x", nil); strings.Contains(strings.Join(argv, " "), "--network") {
		t.Errorf("without a network: %v", argv)
	}
}

// TestInv_ChromiumFlagsEnforceTheAllowlist: everything goes to a proxy
// that doesn't exist except the allowed hosts, which bypass it; only
// allowed names resolve; WebRTC can't use UDP outside the proxy. The
// container tests show these flags alone stop every request (review of the
// layers: request interception alone missed a WebSocket).
func TestInv_ChromiumFlagsEnforceTheAllowlist(t *testing.T) {
	f := strings.Join(reed.ChromiumFlags([]string{"a.example", "b.example", "10.0.0.5"}, map[string]string{"a.example": "10.0.0.9"}), "\n")
	for _, want := range []string{
		"--proxy-server=http://127.0.0.1:9",
		"--proxy-bypass-list=<-loopback>;a.example;b.example;10.0.0.5",
		// A pinned name isn't EXCLUDEd: EXCLUDE would cancel its MAP
		// (review 5: Resolve didn't work).
		"--host-resolver-rules=MAP a.example 10.0.0.9, MAP * ~NOTFOUND, EXCLUDE b.example, EXCLUDE 10.0.0.5",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--remote-debugging-port=9222",
	} {
		if !strings.Contains(f, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(f, "remote-debugging-address") {
		t.Error("Chromium must only listen on the pod's loopback")
	}
}

// TestInv_AllowlistIsValidated: a visit's allowlist is its URL's host
// plus exact host names or IP addresses, lower-cased, sorted and unique;
// anything else can't run.
func TestInv_AllowlistIsValidated(t *testing.T) {
	got, err := reed.Visit{URL: "https://Shop.Example/p?q=1", Allow: []string{"cdn.example", "shop.example", "10.1.2.3"}}.Allowlist()
	if err != nil || strings.Join(got, ",") != "10.1.2.3,cdn.example,shop.example" {
		t.Errorf("allowlist = %v %v", got, err)
	}
	for _, v := range []reed.Visit{
		{URL: "file:///etc/passwd"}, {URL: "javascript:alert(1)"}, {URL: "https://"}, {URL: "ftp://x.example/"},
		{URL: "https://user:pw@x.example/"}, {URL: "https://x.example/", Allow: []string{"*.example"}},
		{URL: "https://x.example/", Allow: []string{"a.example;b.example"}}, {URL: "https://x.example/", Allow: []string{"a b"}},
		{URL: "https://x.example/", Allow: []string{""}}, {URL: "https://x.example/", Allow: []string{"a.example,EXCLUDE"}},
		// Loopback, link-local (cloud metadata) and unspecified addresses,
		// and localhost, can't be allowed (review 5).
		{URL: "http://127.0.0.1/"}, {URL: "http://169.254.169.254/latest/meta-data/"}, {URL: "http://[::1]/"},
		{URL: "http://0.0.0.0/"}, {URL: "http://localhost/"}, {URL: "https://x.example/", Allow: []string{"db.localhost"}},
	} {
		if _, err := v.Allowlist(); !errors.Is(err, reed.ErrInvalid) {
			t.Errorf("%+v: err = %v, want ErrInvalid", v, err)
		}
	}
}

// TestInv_ImagesArePinned: only content-pinned images run; bad resolve
// entries are refused; both before podman is called.
func TestInv_ImagesArePinned(t *testing.T) {
	for _, b := range []reed.Browser{
		{Image: "chromium:latest"}, {Image: "abc123"}, {Image: "--privileged"},
		{Image: img, Resolve: map[string]string{"a.example": "not-an-ip"}},
		{Image: img, Resolve: map[string]string{"a example": "10.0.0.1"}},
		{Image: img, Resolve: map[string]string{"a.example": "169.254.169.254"}},
	} {
		b.Podman = "/nonexistent/podman"
		if _, err := b.Open(context.Background(), reed.Visit{URL: "https://a.example/"}); !errors.Is(err, reed.ErrInvalid) {
			t.Errorf("%+v: err = %v, want ErrInvalid", b, err)
		}
	}
}

// TestInv_NoMounts: every flag-like literal in the package is on a fixed
// list, so no code path can mount a host path, add a device, share a host
// namespace or publish anything but the one loopback port.
func TestInv_NoMounts(t *testing.T) {
	allowed := map[string]bool{}
	for _, f := range []string{"-d", "--rm", "--name", "--label", "--pull=never", "--cap-drop=all",
		"--security-opt=no-new-privileges", "--pids-limit=", "--memory=", "--http-proxy=false", "--log-driver=none",
		"--network=", "-i", "-f", "-t", "-a", "--filter", "--format",
		"--headless", "--no-sandbox", "--disable-dev-shm-usage", "--no-first-run", "--no-default-browser-check",
		"--user-data-dir=/tmp/profile", "--crash-dumps-dir=/tmp", "--disable-background-networking",
		"--disable-component-update", "--disable-sync", "--remote-debugging-port=9222",
		"--proxy-server=http://127.0.0.1:9", "--proxy-bypass-list=<-loopback>;", "--host-resolver-rules=",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp", "--window-size="} {
		allowed[f] = true
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					s, _ := strconv.Unquote(lit.Value)
					if strings.HasPrefix(s, "-") && !allowed[s] {
						t.Errorf("%s: flag-like literal %q isn't on the allowed list", fset.Position(lit.Pos()), s)
					}
				}
				return true
			})
		}
	}
	var fields []string
	st := reflect.TypeOf(reed.Browser{})
	for i := 0; i < st.NumField(); i++ {
		fields = append(fields, st.Field(i).Name)
	}
	if got := strings.Join(fields, " "); got != "Podman Image Network Resolve FrameEvery" {
		t.Errorf("Browser fields = %s; adding a field needs a new invariant", got)
	}
}

// TestInv_OnlyPodmanRunsProcesses: reed's imports are an allowlist, and
// only reed.go starts a process.
func TestInv_OnlyPodmanRunsProcesses(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range strings.Fields(string(out)) {
		if !strings.Contains(strings.SplitN(p, "/", 2)[0], ".") || strings.HasPrefix(p, "go.temporal.io/sdk/") ||
			strings.HasPrefix(p, "github.com/chromedp/") || p == "github.com/gobwas/ws" || p == "github.com/heedle-ai/weft" ||
			p == "github.com/heedle-ai/shuttle" || p == "github.com/heedle-ai/shuttle/plan" || p == "github.com/heedle-ai/draft/registry" {
			continue
		}
		t.Errorf("reed imports %s", p)
	}
	files, err := exec.Command("go", "list", "-f", `{{join .GoFiles " "}}`, ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range strings.Fields(string(files)) {
		src, err := exec.Command("grep", "-l", `"os/exec"`, f).Output()
		if err == nil && strings.TrimSpace(string(src)) != "" && f != "reed.go" {
			t.Errorf("%s imports os/exec; only reed.go starts processes", f)
		}
	}
}

type fakeOpener struct {
	got  reed.Visit
	page reed.Page
	err  error
}

func (f *fakeOpener) Open(_ context.Context, v reed.Visit) (reed.Page, error) {
	f.got = v
	if v.OnFrame != nil {
		v.OnFrame(reed.Frame{URL: v.URL, JPEG: []byte{0xff, 0xd8}})
	}
	return f.page, f.err
}

// runBrowse runs (browse …) as a program in Temporal's test environment.
func runBrowse(t *testing.T, o reed.Opener, src string) (plan.Result, error, []reed.Frame) {
	t.Helper()
	reg := draft.DefaultRegistry()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	plan.Register(env)
	if err := reed.Register(reg, env, o); err != nil {
		t.Fatal(err)
	}
	p, err := draft.Compile(context.Background(), reg, draft.Source(src))
	if err != nil {
		t.Fatal(err)
	}
	var frames []reed.Frame
	env.SetOnActivityHeartbeatListener(func(_ *activity.Info, details converter.EncodedValues) {
		var f reed.Frame
		if details.Get(&f) == nil {
			frames = append(frames, f)
		}
	})
	env.ExecuteWorkflow(plan.WorkflowName, p)
	var res plan.Result
	if err := env.GetWorkflowError(); err != nil {
		return res, err, frames
	}
	return res, env.GetWorkflowResult(&res), frames
}

// TestInv_BrowseInAProgram: the program's args are the visit (the URL,
// then the extra allowed hosts), the output is the page as JSON, each live
// frame is heartbeated to Temporal, and errors are classified: invalid
// input and setup never retry; a failed load does, in a fresh browser.
func TestInv_BrowseInAProgram(t *testing.T) {
	o := &fakeOpener{page: reed.Page{Reed: 1, URL: "https://a.example/", Title: "A", Allowed: []string{"a.example", "cdn.example"}}}
	res, err, frames := runBrowse(t, o, `(browse "https://a.example/" "cdn.example")`)
	if err != nil {
		t.Fatal(err)
	}
	if o.got.URL != "https://a.example/" || strings.Join(o.got.Allow, ",") != "cdn.example" {
		t.Errorf("visit = %+v", o.got)
	}
	if p, ok := reed.ParsePage(res.Outputs["n0"]); !ok || p.Title != "A" {
		t.Errorf("output %q", res.Outputs["n0"])
	}
	if len(frames) != 1 || frames[0].URL != "https://a.example/" {
		t.Errorf("heartbeats = %+v", frames)
	}
	for _, c := range []struct {
		err  error
		kind string
	}{{reed.ErrInvalid, "InvalidInput"}, {reed.ErrSetup, "BrowserSetup"}, {reed.ErrBlocked, "Blocked"}} {
		_, err, _ := runBrowse(t, &fakeOpener{err: c.err}, `(browse "https://a.example/")`)
		var ae *temporal.ApplicationError
		if !errors.As(err, &ae) || !strings.Contains(err.Error(), c.kind) || !strings.Contains(err.Error(), "retryable: false") {
			t.Errorf("%v: err = %v, want non-retryable %s", c.err, err, c.kind)
		}
	}
	if _, ok := reed.ParsePage(`{"reed":2}`); ok {
		t.Error("ParsePage accepted another format")
	}
}

// TestInv_StaleBrowsersAreSwept: browser containers older than the cutoff
// (left by a worker that died mid-visit) are removed; newer ones aren't.
// (Review 5: a killed visit's pod was still running 12 minutes later.)
func TestInv_StaleBrowsersAreSwept(t *testing.T) {
	dir := t.TempDir()
	log := dir + "/log"
	old, recent := time.Now().Add(-time.Hour).Unix(), time.Now().Unix()
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\ncase \"$1\" in ps) printf '[{\"Id\":\"old1\",\"Created\":" +
		strconv.FormatInt(old, 10) + "},{\"Id\":\"new1\",\"Created\":" + strconv.FormatInt(recent, 10) + "}]' ;; esac\n"
	if err := os.WriteFile(dir+"/podman", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reed.Sweep(context.Background(), dir+"/podman", reed.StaleAfter); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(log)
	if !strings.Contains(string(got), "ps -a --filter label=reed.visit --format json\nrm -f -t 0 old1\n") || strings.Contains(string(got), "new1") {
		t.Errorf("podman calls:\n%s", got)
	}
}

// TestInv_WorkerOptionsThrottleHeartbeats: workers that run browse pass
// live frames on every 2 seconds. (Review 5: the SDK's default throttle
// is 30 seconds, so the live view wasn't live.)
func TestInv_WorkerOptionsThrottleHeartbeats(t *testing.T) {
	o := reed.WorkerOptions(worker.Options{MaxConcurrentActivityExecutionSize: 7})
	if o.DefaultHeartbeatThrottleInterval != 2*time.Second || o.MaxHeartbeatThrottleInterval != 2*time.Second || o.MaxConcurrentActivityExecutionSize != 7 {
		t.Errorf("options = %+v", o)
	}
}

// TestInv_BuildImageCopiesOnlyABrowserFolder: build-image.sh copies the
// whole folder the browser binary is in, so it refuses a binary whose
// folder isn't a Chromium's. (Review 5: given a binary in /usr/bin it
// would have copied all of /usr/bin, setuid programs included.)
func TestInv_BuildImageCopiesOnlyABrowserFolder(t *testing.T) {
	out, err := exec.Command("bash", "scripts/build-image.sh", "/bin/sh").CombinedOutput()
	// On Linux /bin/sh is an ELF in a folder that isn't Chromium's; on macOS
	// it isn't an ELF at all. Either way it's refused.
	if err == nil || !strings.Contains(string(out), "Chromium") {
		t.Fatalf("build-image.sh /bin/sh: %v\n%s", err, out)
	}
}

// TestInv_BuildImageFollowsChromeWrappers: Google Chrome's launcher is a
// shell script next to its binary ("exec -a "$0" "$HERE/chrome""), with
// no absolute exec line; build-image.sh finds the binary beside it.
// (CI's first run: the script exited silently on GitHub's runner.)
func TestInv_BuildImageFollowsChromeWrappers(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/google-chrome", []byte("#!/bin/bash\nHERE=\"$(dirname \"$(readlink -f \"$0\")\")\"\nexec -a \"$0\" \"$HERE/chrome\" \"$@\"\n"), 0o755)
	os.WriteFile(dir+"/chrome", []byte("not really an ELF"), 0o755)
	out, err := exec.Command("bash", "scripts/build-image.sh", dir+"/google-chrome").CombinedOutput()
	// It must get past the wrapper to the binary beside it, and refuse
	// that for not being a browser.
	if err == nil || !strings.Contains(string(out), dir+"/chrome isn't a Chromium binary") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// TestInv_CIChecksCanFail: every workflow runs steps with bash -eo
// pipefail (shell: bash), so a failing command in a pipe fails its step,
// and no command is negated ("! grep ..."), which errexit ignores.
func TestInv_CIChecksCanFail(t *testing.T) {
	files, _ := filepath.Glob(".github/workflows/*.yml")
	negated := regexp.MustCompile(`(^|&&|\|\||;|\bthen|\bdo)\s*!\s*[A-Za-z]`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "\ndefaults:\n  run:\n") || !strings.Contains(string(b), "\n    shell: bash\n") {
			t.Errorf("%s: no defaults run shell: bash, so pipes hide failures", f)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if l := strings.TrimSpace(line); !strings.HasPrefix(l, "#") && negated.MatchString(l) {
				t.Errorf("%s:%d: a negated command never fails a step: %s", f, i+1, l)
			}
		}
	}
}
