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
	"runtime"
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

// TestInv_ImageIsTestedBeforeItIsPublished: the browser image is built
// on both Linux architectures, each passes the container tests
// (ci:image-tested) before it is saved, and only a job that needs both
// builds may push, by digest with a manifest list. (roadmap 9.1)
func TestInv_ImageIsTestedBeforeItIsPublished(t *testing.T) {
	b, err := os.ReadFile(".github/workflows/image.yml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"runner: ubuntu-24.04 ", "runner: ubuntu-24.04-arm ", "\n  publish:\n    needs: build\n", "podman manifest push --all --digestfile"} {
		if !strings.Contains(s, want) {
			t.Errorf("image.yml lacks %q", want)
		}
	}
	publish := strings.Index(s, "\n  publish:\n")
	order := []string{"- name: build\n", "- name: image-tested\n", "- name: save\n", "actions/upload-artifact"}
	last := -1
	for _, step := range order {
		i := strings.Index(s, step)
		if i < 0 || i < last || (publish >= 0 && i > publish) {
			t.Errorf("image.yml: the build job must run %q in this order, before publish: %v", step, order)
		}
		last = i
	}
	for _, w := range []string{"packages: write", "podman login", "manifest push"} {
		if i := strings.Index(s, w); i < 0 || publish < 0 || i < publish {
			t.Errorf("image.yml: %q may appear only in the publish job", w)
		}
	}
	if strings.Contains(s, "podman push") {
		t.Error("image.yml: images are pushed only with their manifest list (podman manifest push --all)")
	}
	// Review 36: nothing may let a step or job be skipped or its failure
	// ignored, which these string checks alone would not notice.
	for _, bad := range []string{"continue-on-error", "|| true", "|| :", "if: ", "${{ always", "${{ failure", "if-no-files-found: ignore", "if-no-files-found: warn"} {
		if strings.Contains(s, bad) {
			t.Errorf("image.yml uses %q, which could publish an untested image or hide a failure", bad)
		}
	}
	if !strings.Contains(s, "fail-fast: true") || !strings.Contains(s, "if-no-files-found: error") {
		t.Error("image.yml: both architectures must build, and both archives must exist, or nothing is published")
	}
	// Review 36, round 2: the image the tests run against must be the one
	// built, and the one saved and published; the tests must require Podman,
	// fail on a skip, and each expected pass must be checked.
	build := s[:max(publish, 0)]
	for _, bad := range []string{"||", "; true", "--from"} {
		if strings.Contains(build, bad) {
			t.Errorf("image.yml's build job uses %q", bad)
		}
	}
	want := map[string][]string{
		"build": {"id=$(podman build --quiet ", `echo "REED_IMAGE=$id" >> "$GITHUB_ENV"` + "\n"},
		"image-tested": {
			`REED_REQUIRE_PODMAN: "1"` + "\n",
			"go test -count=1 -v -run 'TestContainer_' . | tee image-tested.log\n",
			"if grep -q -- '--- SKIP' image-tested.log; then echo 'a test skipped'; exit 1; fi\n",
			"grep -q -- '--- PASS: TestContainer_OnlyAllowedHostsAreReached' image-tested.log\n",
			"grep -q -- '--- PASS: TestContainer_EveryVisitStartsClean' image-tested.log\n",
			"grep -q -- '--- PASS: TestContainer_InAProgram' image-tested.log\n",
			"grep -q -- '--- PASS: TestContainer_DevToolsIsNotExposed' image-tested.log\n",
			"grep -q -- '--- PASS: TestContainer_OutputsAreBounded' image-tested.log\n",
			"grep -q -- '--- PASS: TestContainer_ResolvePinsHosts' image-tested.log\n",
		},
		"save":   {`"$REED_IMAGE"` + "\n"},
		"podman": {"--add-subuids 100000-165535 --add-subgids 100000-165535"},
	}
	for name, lines := range want {
		step := imageStep(build, name)
		for _, l := range lines {
			if !strings.Contains(step, l) {
				t.Errorf("image.yml's %s step lacks %q", name, strings.TrimSpace(l))
			}
		}
	}
}

// TestInv_ImageBaseIsPinned: every image the workflow names is pinned by
// digest, and nothing anywhere uses :latest. (roadmap 9.1)
func TestInv_ImageBaseIsPinned(t *testing.T) {
	b, err := os.ReadFile(".github/workflows/image.yml")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`docker\.io/\S+`).FindAllString(string(b), -1)
	if len(refs) == 0 {
		t.Fatal("image.yml names no base image")
	}
	pinned := regexp.MustCompile(`^docker\.io/[a-z0-9./_-]+@sha256:[0-9a-f]{64}$`)
	for _, r := range refs {
		if !pinned.MatchString(r) {
			t.Errorf("image.yml: %s isn't pinned by digest", r)
		}
	}
	for _, f := range []string{".github/workflows/image.yml", "scripts/build-image.sh"} {
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), ":latest") {
			t.Errorf("%s uses :latest", f)
		}
	}
	// Review 36: the container the browser comes from must be the pinned
	// one, not a name typed beside it.
	s := string(b)
	if strings.Count(s, "podman run ") != 1 || !regexp.MustCompile(`-e BUILD_IMAGE_CONTEXT=/context "\$DEBIAN" bash -c`).MatchString(s) {
		t.Error(`image.yml: the browser must be installed by one podman run of "$DEBIAN", the pinned base`)
	}
	if regexp.MustCompile(`debian:[a-z]`).MatchString(strings.ReplaceAll(s, "debian:bookworm-slim's", "")) {
		t.Error("image.yml names Debian by tag; use $DEBIAN, pinned by digest")
	}
}

// TestInv_BuildImageContextMode: with BUILD_IMAGE_CONTEXT set to an empty
// folder, build-image.sh writes the build context there (the browser's
// folder under rootfs/ and a Containerfile whose label can't expand
// anything) and prints the folder, without running podman; it refuses a
// folder that isn't empty; unset, it still builds with podman. The script
// is for Linux, so this runs on CI's Linux jobs. (Review 36: the mode had
// no guard.)
func TestInv_BuildImageContextMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		return // build-image.sh copies a Linux browser and its libraries
	}
	dir := t.TempDir()
	browser, bin := filepath.Join(dir, "browser"), filepath.Join(dir, "bin")
	os.MkdirAll(browser, 0o755)
	os.MkdirAll(bin, 0o755)
	// A static Linux program stands in for Chromium (it only prints a
	// version) and for socat.
	src := filepath.Join(dir, "fake.go")
	os.WriteFile(src, []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(`Fake \"Chromium\" 1.0 $HOME`) }\n"), 0o644)
	build := exec.Command("go", "build", "-o", filepath.Join(browser, "chromium"), src)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(browser, "icudtl.dat"), nil, 0o644)
	os.WriteFile(filepath.Join(browser, "resources.pak"), nil, 0o644)
	exe, _ := os.ReadFile(filepath.Join(browser, "chromium"))
	os.WriteFile(filepath.Join(bin, "socat"), exe, 0o755)
	calls := filepath.Join(dir, "podman-calls")
	os.WriteFile(filepath.Join(bin, "podman"), []byte("#!/bin/sh\necho \"$*\" >> "+calls+"\necho imageid\n"), 0o755)
	env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx := filepath.Join(dir, "context")
	os.Mkdir(ctx, 0o755)
	cmd := exec.Command("bash", "scripts/build-image.sh", filepath.Join(browser, "chromium"))
	cmd.Env = append(env, "BUILD_IMAGE_CONTEXT="+ctx)
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != ctx {
		t.Fatalf("context mode: %v, printed %q", err, out)
	}
	if _, err := os.Stat(calls); err == nil {
		b, _ := os.ReadFile(calls)
		t.Errorf("context mode ran podman: %s", b)
	}
	cf, err := os.ReadFile(filepath.Join(ctx, "Containerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cf), `LABEL reed.browser="Fake \"Chromium\" 1.0 \$HOME"`) || !strings.Contains(string(cf), `ENTRYPOINT ["/browser/chromium"]`) {
		t.Errorf("Containerfile:\n%s", cf)
	}
	if _, err := os.Stat(filepath.Join(ctx, "rootfs", "browser", "icudtl.dat")); err != nil {
		t.Errorf("the browser's folder isn't in the context: %v", err)
	}

	cmd = exec.Command("bash", "scripts/build-image.sh", filepath.Join(browser, "chromium"))
	cmd.Env = append(env, "BUILD_IMAGE_CONTEXT="+ctx)
	if b, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(b), "must be an empty folder") {
		t.Errorf("a folder that isn't empty: %v\n%s", err, b)
	}

	cmd = exec.Command("bash", "scripts/build-image.sh", filepath.Join(browser, "chromium"))
	cmd.Env = env
	if b, err := cmd.Output(); err != nil || strings.TrimSpace(string(b)) != "imageid" {
		t.Errorf("without BUILD_IMAGE_CONTEXT: %v, printed %q", err, b)
	}
	if b, _ := os.ReadFile(calls); !strings.HasPrefix(string(b), "build --quiet -f ") {
		t.Errorf("without BUILD_IMAGE_CONTEXT, podman ran %q", b)
	}
}

// imageStep is the text of the step called name in a workflow's job, up to
// the next step.
func imageStep(job, name string) string {
	i := strings.Index(job, "- name: "+name+"\n")
	if i < 0 {
		return ""
	}
	rest := job[i+len("- name: "+name+"\n"):]
	if j := strings.Index(rest, "\n      - "); j >= 0 {
		rest = rest[:j+1]
	}
	return rest
}
