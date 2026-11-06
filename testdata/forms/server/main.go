// Command server serves reed's course of practice forms (roadmap 9.6) and
// checks what each received, or counts what reaches a decoy host:
//
//	server course DIR   serve the forms in DIR (testdata/forms)
//	                    and, at GET /ws/<form>, a WebSocket whose every
//	                    message is recorded as that form's ws=<message>
//	                    ; a form received sets a cookie for it, and GET
//	                    /whoami/<form> says whether the browser has it
//	                    (a site that keeps you signed in, roadmap 9.11)
//	server decoy        count every request; GET /hits says how many
//
// It listens on $COURSE_ADDR, :8080 if unset.
// Each form in DIR/course.json has rules (a regular expression per field):
// a submission that breaks one goes back to the form with ?error=<field>,
// as a site's validation would. One that keeps them is recorded. GET
// /check/<form>?token=$COURSE_TOKEN says what arrived, and whether the
// form is done: for a form to fill, a submission whose fields are exactly
// its "want"; for a form to refuse ("refuse": true), nothing at all. The
// token is the test's: an agent on the course's host can't ask whether it
// got a form right (review 40), and without one there is no /check.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// Form is one form of the course, as course.json describes it.
type Form struct {
	Task   string              `json:"task"`
	Refuse bool                `json:"refuse,omitempty"`
	Rules  map[string]string   `json:"rules"`
	Want   map[string][]string `json:"want"`
}

// Check is what GET /check/<form> answers.
type Check struct {
	Received []string `json:"received"` // each submission, form-encoded
	Done     bool     `json:"done"`
}

// done says whether got is exactly want: the same fields, each with the
// same values in any order, empty values aside.
func done(want map[string][]string, got url.Values) bool {
	norm := func(m map[string][]string) map[string][]string {
		out := map[string][]string{}
		for k, vs := range m {
			for _, v := range vs {
				if v != "" {
					out[k] = append(out[k], v)
				}
			}
			slices.Sort(out[k])
		}
		return out
	}
	a, b := norm(want), norm(got)
	if len(a) != len(b) {
		return false
	}
	for k, vs := range a {
		if !slices.Equal(vs, b[k]) {
			return false
		}
	}
	return true
}

func main() {
	switch os.Args[1] {
	case "course":
		dir := os.Args[2]
		b, err := os.ReadFile(filepath.Join(dir, "course.json"))
		if err != nil {
			panic(err)
		}
		var course map[string]Form
		if err := json.Unmarshal(b, &course); err != nil {
			panic(err)
		}
		token := os.Getenv("COURSE_TOKEN")
		var mu sync.Mutex
		got := map[string][]url.Values{}
		http.HandleFunc("POST /{form}/submit", func(w http.ResponseWriter, r *http.Request) {
			name := r.PathValue("form")
			f, ok := course[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			r.ParseForm()
			for field, rule := range f.Rules {
				if !regexp.MustCompile(rule).MatchString(r.PostForm.Get(field)) {
					http.Redirect(w, r, "/"+name+"/?error="+url.QueryEscape(field), http.StatusSeeOther)
					return
				}
			}
			mu.Lock()
			got[name] = append(got[name], r.PostForm)
			mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "course-" + name, Value: "member-" + token, Path: "/", HttpOnly: true, MaxAge: 3600})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<!doctype html><title>Thank you</title><h1>Thank you</h1><p>We received your form.</p>")
		})
		http.HandleFunc("GET /ws/{form}", func(w http.ResponseWriter, r *http.Request) {
			name := r.PathValue("form")
			if _, ok := course[name]; !ok {
				http.NotFound(w, r)
				return
			}
			conn, _, _, err := ws.UpgradeHTTP(r, w)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				msg, err := wsutil.ReadClientText(conn)
				if err != nil {
					return
				}
				mu.Lock()
				got[name] = append(got[name], url.Values{"ws": {string(msg)}})
				mu.Unlock()
			}
		})
		http.HandleFunc("GET /check/{form}", func(w http.ResponseWriter, r *http.Request) {
			f, ok := course[r.PathValue("form")]
			if !ok || token == "" || r.URL.Query().Get("token") != token {
				http.NotFound(w, r)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			c := Check{Received: []string{}}
			for _, v := range got[r.PathValue("form")] {
				c.Received = append(c.Received, v.Encode())
				c.Done = c.Done || (!f.Refuse && done(f.Want, v))
			}
			if f.Refuse {
				c.Done = len(c.Received) == 0
			}
			json.NewEncoder(w).Encode(c)
		})
		http.HandleFunc("GET /whoami/{form}", func(w http.ResponseWriter, r *http.Request) {
			name := r.PathValue("form")
			if _, ok := course[name]; !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if c, err := r.Cookie("course-" + name); err == nil && c.Value == "member-"+token {
				fmt.Fprint(w, "<!doctype html><title>Your account</title><h1>Signed in as a course member</h1>")
				return
			}
			fmt.Fprint(w, "<!doctype html><title>Your account</title><h1>Not signed in</h1>")
		})
		files := http.FileServer(http.Dir(dir))
		http.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			// The forms only: never the answers or this server.
			if _, ok := course[firstSegment(r.URL.Path)]; !ok {
				http.NotFound(w, r)
				return
			}
			files.ServeHTTP(w, r)
		})
	case "decoy":
		var hits atomic.Int64
		http.HandleFunc("GET /hits", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, hits.Load()) })
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			fmt.Fprintln(os.Stderr, "HIT", r.Method, r.URL.Path)
		})
	}
	addr := os.Getenv("COURSE_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	panic(http.ListenAndServe(addr, nil))
}

// firstSegment is the first part of a URL path: "/contact/x" is "contact".
func firstSegment(p string) string {
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return p
}
