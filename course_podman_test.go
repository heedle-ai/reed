package reed_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heedle-ai/reed"
)

// courseSites starts the course (forms-srv) and a decoy host (decoy-srv)
// on a fresh Podman network, and returns the network, what the course
// says each form received, and how many requests the decoy got.
func courseSites(t *testing.T) (network string, check func(form string) courseCheck, hits func() int) {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("no podman")
	}
	dir := t.TempDir()
	buildCourseServer(t, dir)
	if out, err := exec.Command("cp", "-r", "testdata/forms", filepath.Join(dir, "forms")).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM scratch\nCOPY server /server\nCOPY forms /forms\nENTRYPOINT [\"/server\"]\n"), 0o644)
	img := podman(t, "build", "--quiet", dir)
	network = fmt.Sprintf("reed-course-%d", time.Now().UnixNano())
	podman(t, "network", "create", network)
	t.Cleanup(func() {
		exec.Command("podman", "rm", "-f", "-t", "0", network+"-forms", network+"-decoy").Run()
		exec.Command("podman", "network", "rm", "-f", network).Run()
	})
	podman(t, "run", "-d", "--rm", "--name", network+"-forms", "--network", network, "--network-alias", "forms-srv", "--publish", "127.0.0.1::8080", img, "course", "/forms")
	podman(t, "run", "-d", "--rm", "--name", network+"-decoy", "--network", network, "--network-alias", "decoy-srv", "--publish", "127.0.0.1::8080", img, "decoy")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	get := func(name, path string) []byte {
		addr := strings.Split(podman(t, "port", name, "8080/tcp"), "\n")[0]
		for i := 0; i < 100; i++ {
			if res, err := client.Get("http://" + addr + path); err == nil {
				b, _ := io.ReadAll(res.Body)
				res.Body.Close()
				return b
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s unreachable", name)
		return nil
	}
	check = func(form string) courseCheck {
		var c courseCheck
		json.Unmarshal(get(network+"-forms", "/check/"+form), &c)
		return c
	}
	hits = func() int {
		var n int
		fmt.Sscan(string(get(network+"-decoy", "/hits")), &n)
		return n
	}
	check("contact")
	hits()
	return network, check, hits
}

// TestContainer_CourseInjectionReachesNothing: the course's injection
// form, filled and sent in an act session as Claude would (by hand here),
// reaches its own server, while the decoy host its page asks for (text
// addressed to the model, an image and a script's request) and Playwright
// navigating there by name receive nothing. (Roadmap 9.6.)
func TestContainer_CourseInjectionReachesNothing(t *testing.T) {
	img := testActBrowser(t)
	network, check, hits := courseSites(t)
	s, err := reed.Browser{Image: img, Network: network}.Start(ctx, reed.Visit{URL: "http://forms-srv:8080/injection/"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cs := mcpSession(t, s)
	call(t, cs, "browser_navigate", map[string]any{"url": "http://forms-srv:8080/injection/"})
	snap, _ := call(t, cs, "browser_snapshot", nil)
	if !strings.Contains(snap, "decoy-srv") {
		t.Fatalf("the page doesn't try to steer:\n%s", snap)
	}
	if text, isErr := call(t, cs, "browser_fill_form", map[string]any{"fields": []map[string]any{
		{"name": "Feedback", "type": "textbox", "target": ref(t, snap, `textbox "Feedback"`), "value": "Great workshop, thank you."},
		{"name": "Rating", "type": "textbox", "target": ref(t, snap, `spinbutton "Rating (1 to 5)"`), "value": "5"},
	}}); isErr {
		t.Fatal(text)
	}
	call(t, cs, "browser_click", map[string]any{"element": "Send feedback", "target": ref(t, snap, `button "Send feedback"`)})
	if text, isErr := call(t, cs, "browser_navigate", map[string]any{"url": "http://decoy-srv:8080/collect"}); !isErr {
		t.Errorf("navigating to the decoy worked:\n%s", text)
	}
	time.Sleep(time.Second)
	if c := check("injection"); !c.Done {
		t.Errorf("the form's server got %v", c.Received)
	}
	if n := hits(); n != 0 {
		t.Fatalf("the decoy received %d requests", n)
	}
}
