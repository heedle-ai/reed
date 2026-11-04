// Command server is the test web server. As "allowed" it serves a page
// that tries every way out it can think of; as "blocked" it counts every
// request it gets, which must stay zero.
package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

const page = `<!doctype html><html><head><title>Allowed page</title></head><body>
<h1>Allowed page</h1><p id="r">pending</p>
<img src="http://blocked-srv:8080/pixel.png">
<iframe src="http://blocked-srv:8080/frame"></iframe>
<script>
const ip = new URLSearchParams(location.search).get("ip");
const ws = (u) => new Promise((r) => { try { const w = new WebSocket(u); w.onopen = () => r("open"); w.onerror = () => r("error"); } catch (e) { r("error"); } });
Promise.allSettled([
  fetch("http://blocked-srv:8080/fetch-by-name"),
  fetch("http://" + ip + ":8080/fetch-by-ip"),
  ws("ws://blocked-srv:8080/ws"),
  fetch("/same-origin").then((r) => r.text()),
]).then((rs) => { document.getElementById("r").textContent = "done " + rs.map((x) => x.status + ":" + (typeof x.value === "string" ? x.value : x.value && x.value.status)).join(" "); });
</script></body></html>`

func main() {
	var hits atomic.Int64
	switch os.Args[1] {
	case "allowed":
		http.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
			// Reports what an earlier visit left behind, then leaves some.
			w.Header().Set("Content-Type", "text/html")
			if c, err := r.Cookie("seen"); err == nil {
				fmt.Fprintf(w, "<title>state</title><p id=c>cookie=%s</p>", c.Value)
			} else {
				fmt.Fprint(w, "<title>state</title><p id=c>cookie=none</p>")
			}
			fmt.Fprint(w, `<p id=l></p><script>document.getElementById("l").textContent = "storage=" + (localStorage.getItem("seen") || "none"); localStorage.setItem("seen", "yes"); document.cookie = "seen=yes; max-age=3600";</script>`)
		})
		http.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://blocked-srv:8080/landed", http.StatusFound)
		})
		http.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
			// A 5 MB title, a 1.5 MB URL and 3000 distinct blocked requests.
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<title>big</title><body><p>big</p><script>
document.title = "T".repeat(5e6);
history.replaceState(null, "", "/big?" + "u".repeat(1.5e6));
for (let i = 0; i < 3000; i++) fetch("http://blocked-srv:8080/" + i + "/" + "x".repeat(1000)).catch(() => {});
</script></body>`)
		})
		http.HandleFunc("/big-title", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `<title>t</title><body><p>t</p><script>document.title = "T".repeat(5e6);</script></body>`)
		})
		http.HandleFunc("/big-url", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `<title>u</title><body><p>u</p><script>history.replaceState(null, "", "/big-url?" + "u".repeat(1.5e6));</script></body>`)
		})
		http.HandleFunc("/many", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `<title>m</title><body><p>m</p><script>for (let i = 0; i < 3000; i++) fetch("http://blocked-srv:8080/" + i + "/" + "x".repeat(1000)).catch(() => {});</script></body>`)
		})
		http.HandleFunc("/never-ends", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		http.HandleFunc("/same-origin", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "same-origin-ok") })
		// A two-page form for act (roadmap 9.5); /got says what arrived.
		var mu sync.Mutex
		var got []string
		form := func(w http.ResponseWriter, title, body string) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, "<!doctype html><title>%s</title><h1>%s</h1>%s", title, title, body)
		}
		keep := func(r *http.Request) {
			r.ParseForm()
			mu.Lock()
			got = append(got, r.PostForm.Encode())
			mu.Unlock()
		}
		http.HandleFunc("/volunteer", func(w http.ResponseWriter, r *http.Request) {
			form(w, "Volunteer sign-up: you", `<form method=post action=/volunteer/2><label>Name <input name=name required></label>
<label>Email <input name=email type=email required></label><button>Next</button></form>
<img src="http://blocked-srv:8080/form-pixel.png"><script>fetch("http://blocked-srv:8080/form-fetch").catch(() => {})</script>`)
		})
		http.HandleFunc("POST /volunteer/2", func(w http.ResponseWriter, r *http.Request) {
			keep(r)
			form(w, "Volunteer sign-up: shift", `<form method=post action=/volunteer/done><label>Shift <select name=shift>
<option>Friday</option><option>Saturday</option><option>Sunday</option></select></label><button>Submit</button></form>`)
		})
		http.HandleFunc("POST /volunteer/done", func(w http.ResponseWriter, r *http.Request) {
			keep(r)
			form(w, "Thank you", "<p>You're signed up for the shift.</p>")
		})
		http.HandleFunc("/got", func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			fmt.Fprint(w, strings.Join(got, "\n"))
		})
		http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, page)
		})
	case "blocked":
		http.HandleFunc("/hits", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, hits.Load()) })
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			fmt.Fprintln(os.Stderr, "HIT", r.URL.Path)
			fmt.Fprint(w, "leaked")
		})
	}
	panic(http.ListenAndServe(":8080", nil))
}
