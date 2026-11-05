package reed_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// courseForm is one form of the practice course (testdata/forms/course.json).
type courseForm struct {
	Task   string              `json:"task"`
	Refuse bool                `json:"refuse"`
	Rules  map[string]string   `json:"rules"`
	Want   map[string][]string `json:"want"`
}

func readCourse(t *testing.T) map[string]courseForm {
	t.Helper()
	b, err := os.ReadFile("testdata/forms/course.json")
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]courseForm
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// buildCourseServer builds testdata/forms/server into dir.
func buildCourseServer(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "server")
	build := exec.Command("go", "build", "-o", bin, "./testdata/forms/server")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the course server: %v\n%s", err, out)
	}
	return bin
}

// courseCheck is what the course server says a form received.
type courseCheck struct {
	Received []string `json:"received"`
	Done     bool     `json:"done"`
}

// TestInv_CourseChecksWhatArrived: every form of the course (roadmap 9.6)
// has a task, its page and a check of what arrived; its server sends a
// submission that breaks a rule back to the form with the field named; a
// wrong submission (each wanted field changed, one dropped, one added)
// never counts as done, and exactly the wanted one does; a form to be
// refused is done only while nothing has arrived. The server serves the
// forms, never the answers.
func TestInv_CourseChecksWhatArrived(t *testing.T) {
	course := readCourse(t)
	if len(course) != 7 {
		t.Fatalf("the course has %d forms", len(course))
	}
	bin := buildCourseServer(t, t.TempDir())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	srv := exec.Command(bin, "course", "testdata/forms")
	srv.Env = append(os.Environ(), "COURSE_ADDR="+addr)
	srv.Stderr = os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Process.Kill(); srv.Wait() })
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + addr
	for i := 0; ; i++ {
		if _, err := client.Get(base + "/contact/"); err == nil {
			break
		}
		if i > 100 {
			t.Fatal("the course server didn't start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	check := func(name string) courseCheck {
		res, err := client.Get(base + "/check/" + name)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var c courseCheck
		json.NewDecoder(res.Body).Decode(&c)
		return c
	}
	post := func(name string, v url.Values) *http.Response {
		res, err := client.PostForm(base+"/"+name+"/submit", v)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res
	}
	names := make([]string, 0, len(course))
	for n := range course {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		f := course[name]
		if strings.TrimSpace(f.Task) == "" {
			t.Errorf("%s has no task", name)
		}
		if res, err := client.Get(base + "/" + name + "/"); err != nil || res.StatusCode != 200 {
			t.Errorf("%s: its page: %v %v", name, err, res)
		}
		if f.Refuse {
			if !check(name).Done {
				t.Errorf("%s isn't done before anything arrived", name)
			}
			post(name, url.Values{"email": {"ada@example.org"}, "password": {"x"}})
			if c := check(name); c.Done || len(c.Received) != 1 {
				t.Errorf("%s: a submission to a form to refuse counted as done: %+v", name, c)
			}
			continue
		}
		want := url.Values(f.Want)
		// Each rule turns a wrong entry away, naming its field.
		for field := range f.Rules {
			bad := cloneValues(want)
			bad.Set(field, "")
			if res := post(name, bad); res.StatusCode != http.StatusSeeOther || !strings.HasSuffix(res.Header.Get("Location"), "?error="+field) {
				t.Errorf("%s: an empty %s went through: %d %s", name, field, res.StatusCode, res.Header.Get("Location"))
			}
		}
		// Wrong submissions that keep the rules aren't done.
		for field := range want {
			for _, bad := range []url.Values{changed(want, field), dropped(want, field)} {
				post(name, bad)
				if check(name).Done {
					t.Errorf("%s: a wrong %s counted as done: %v", name, field, bad)
				}
			}
		}
		extra := cloneValues(want)
		extra.Add("extras", "newsletter")
		post(name, extra)
		if check(name).Done {
			t.Errorf("%s: an extra field counted as done", name)
		}
		post(name, want)
		if !check(name).Done {
			t.Errorf("%s: exactly what it wants wasn't done", name)
		}
	}
	for _, p := range []string{"/course.json", "/server/main.go", "/check/nothing"} {
		if res, err := client.Get(base + p); err != nil || res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %v %v", p, err, res.Status)
		}
	}
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// changed is v with field's value changed, but still keeping the rules
// where it can (an email stays an email, a choice another choice).
func changed(v url.Values, field string) url.Values {
	out := cloneValues(v)
	alt := map[string]string{"email": "grace@example.org", "parking": "no", "size": "large", "date": "2026-12-13", "terms": "",
		"day": "friday", "lunch": "standard", "frequency": "daily", "rating": "4", "country": "Spain", "city": "Madrid", "extras": "newsletter"}
	if a, ok := alt[field]; ok {
		out.Set(field, a)
	} else {
		out.Set(field, v.Get(field)+" Byron")
	}
	return out
}

func dropped(v url.Values, field string) url.Values {
	out := cloneValues(v)
	out.Del(field)
	return out
}
